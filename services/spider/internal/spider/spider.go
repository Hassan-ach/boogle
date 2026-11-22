package spider

import (
	"bytes"
	"context"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"
	"uuid"

	"github.com/Hassan-ach/boogle/services/spider/internal/config"
	"github.com/Hassan-ach/boogle/services/spider/internal/entity"
	"github.com/Hassan-ach/boogle/services/spider/internal/messaging"
	"github.com/Hassan-ach/boogle/services/spider/internal/parser"
	"github.com/Hassan-ach/boogle/services/spider/internal/policy"
	"github.com/Hassan-ach/boogle/services/spider/internal/state"
	"github.com/Hassan-ach/boogle/services/spider/internal/store"
	"github.com/Hassan-ach/boogle/services/spider/internal/utils"
)

type pageStore interface {
	Init(startUrls []string) error
	Persist(ctx context.Context, page *entity.Page) (uuid.UUID, error)
	Close()
}

type htmlParser interface {
	ParseHTML(r io.Reader, baseURL string) (*entity.Page, error)
}

type Spider struct {
	config     *config.Config
	httpClient *http.Client
	store      pageStore
	mq         messaging.MessagingQueue
	parser     htmlParser
	policy     *policy.PolicyManager

	wg             sync.WaitGroup
	ctx            context.Context
	cancel         context.CancelFunc
	crawlerTimeout time.Duration
	crawlerDelay   time.Duration

	fetchpool chan struct{}
	logger    *utils.Logger

	// One AMQP channel per crawler goroutine, created on first use. Opening a
	// channel per crawl produced roughly a hundred channel objects per second at
	// the default settings (H-09).
	channels sync.Map
}

func NewSpider(conf *config.Config) *Spider {
	// The crawler dials addresses supplied by arbitrary pages, so the transport
	// refuses non-public destinations (loopback, RFC 1918, link-local, metadata
	// service, container network names) instead of relying on the policy layer,
	// which only sees the URL string (H-07). Proxy support is deliberately off:
	// a proxy would be dialed through the same guard and an operator-configured
	// proxy is not part of this deployment.
	httpClient := &http.Client{
		Timeout: time.Duration(conf.App.HttpTimeout) * time.Second,
		Transport: &http.Transport{
			DialContext:         utils.PublicOnlyDialContext(nil),
			TLSHandshakeTimeout: 10 * time.Second,
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
			ForceAttemptHTTP2:   true,
		},
	}
	logger := utils.NewMultiLogger(conf.App.LogsPath)

	mq, err := messaging.NewRabbitMQ(&conf.RabbitMq, logger)
	if err != nil {
		log.Fatal("Failed to initialize RabbitMQ", "error", err)
	}
	ctx, cancel := context.WithCancel(context.Background())

	cache := store.NewRedisClient(conf.Store.Cache)
	crawlState := state.NewRedisState(
		cache.Conn(),
		conf.Policy.RedisPrefix,
		conf.Policy.URLStateTTL,
	)

	policyManager := policy.New(conf.Policy, crawlState, logger.Logger).
		WithFetchClient(httpClient).
		WithRobotsClient(httpClient).
		WithSiteMapResolver(func(ctx context.Context, base *url.URL, sitemaps []string) []string {
			return parser.FetchSitemaps(ctx, httpClient, sitemaps, base)
		})

	s := &Spider{
		config:         conf,
		httpClient:     httpClient,
		mq:             mq,
		parser:         parser.NewParser(httpClient, logger),
		store:          store.NewStore(conf.Store, logger, cache, crawlState),
		policy:         policyManager,
		wg:             sync.WaitGroup{},
		ctx:            ctx,
		cancel:         cancel,
		crawlerTimeout: time.Duration(conf.App.CrawlerTimeout) * time.Second,
		crawlerDelay:   time.Duration(conf.App.ClawlerDelay) * time.Millisecond,
		fetchpool:      make(chan struct{}, conf.App.MaxConcurrentFetch),
		logger:         logger,
	}

	// config is a *config.Config, whose String redacts, so this cannot print a
	// password even though it dumps the whole struct.
	logger.Info("Spider initialized", "config", conf.Redacted())

	return s
}

func (s *Spider) Start(startUrls []string) {
	if err := s.store.Init(startUrls); err != nil {
		s.logger.Error(
			"Failed to initialize store with start URLs",
			"component",
			"store",
			"error",
			err,
		)
		return
	}

	for i := 0; i < s.config.App.MaxCrawlers; i++ {
		s.wg.Add(1)
		s.logger.Info("Starting worker", "component", "spider", "crawler_id", i)
		go s.crawler(i + 1)
	}
}

func (s *Spider) Stop() {
	s.cancel()
	s.wg.Wait()
}

func (s *Spider) Close() {
	s.channels.Range(func(key, value any) bool {
		value.(messaging.MessagingChannel).Close()
		s.channels.Delete(key)
		return true
	})
	s.store.Close()
	s.mq.Close()
	s.logger.Close()
}

// channelFor returns the crawler's long-lived AMQP channel, opening one on first
// use. A channel is not shared between goroutines, so the id is the key.
func (s *Spider) channelFor(crawlerID int) (messaging.MessagingChannel, error) {
	if existing, ok := s.channels.Load(crawlerID); ok {
		return existing.(messaging.MessagingChannel), nil
	}

	ch, err := s.mq.NewMessagingChannel()
	if err != nil {
		return nil, err
	}

	actual, loaded := s.channels.LoadOrStore(crawlerID, ch)
	if loaded {
		ch.Close()
	}
	return actual.(messaging.MessagingChannel), nil
}

// dropChannel discards a broken channel so the next round reopens one.
func (s *Spider) dropChannel(crawlerID int) {
	if existing, ok := s.channels.LoadAndDelete(crawlerID); ok {
		existing.(messaging.MessagingChannel).Close()
	}
}

func (s *Spider) crawler(crawler_id int) {
	defer s.wg.Done()

	ticker := time.NewTicker(s.crawlerDelay)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.crawl(crawler_id)
		}
	}
}

const requeueDelay = 30 * time.Second

// bookkeepingContext survives the crawl deadline.
//
// The whole crawl runs under one CRAWLER_TIMEOUT context, and the failures that
// most need recording -- a timed-out fetch, a slow persist -- are exactly the
// ones where that context is already done. Without this, the bookkeeping write
// fails with the same deadline and the popped URL is lost (M-25).
func bookkeepingContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}

// deferCrawl parks a URL for a short retry without touching its attempt budget.
// Used for infrastructure failures that say nothing about the URL itself.
func (s *Spider) deferCrawl(ctx context.Context, rawURL string, logger *slog.Logger) {
	rctx, cancel := bookkeepingContext(ctx)
	defer cancel()
	if err := s.policy.Park(rctx, rawURL, s.policy.Now().Add(requeueDelay)); err != nil {
		logger.Error("Could not park a URL after an infrastructure failure",
			"url", rawURL, "error", err)
	}
}

// requeue returns a popped URL to the frontier and spends one attempt from the
// same budget a failed fetch uses.
//
// PopFrontier removes an entry unconditionally. Every failure before the page is
// persisted therefore has to put the URL back, or the frontier silently loses
// work (M-25).
func (s *Spider) requeue(ctx context.Context, rawURL string, logger *slog.Logger) {
	rctx, cancel := bookkeepingContext(ctx)
	defer cancel()
	if err := s.policy.Requeue(rctx, rawURL); err != nil {
		logger.Error("Could not return a popped URL to the frontier",
			"url", rawURL, "error", err)
	}
}

func (s *Spider) crawl(crawler_id int) {
	ctx, cancel := context.WithTimeout(s.ctx, s.crawlerTimeout)
	defer cancel()

	logger := s.logger.With("component", "crawler", "crawler_id", crawler_id)

	queue, err := s.channelFor(crawler_id)
	if err != nil {
		logger.Error("Failed to create messaging channel", "error", err)
		return
	}

	select {
	case s.fetchpool <- struct{}{}:
	case <-ctx.Done():
		logger.Error("Crawler timed out waiting for fetch slot", "crawler_id", crawler_id)
		return
	}
	defer func() { <-s.fetchpool }()

	next, err := s.policy.TakeNext(ctx)
	if err != nil {
		logger.Warn("Could not take the next URL; skipping this round", "error", err)
		return
	}
	if next.Idle {
		return
	}

	rawUrl := next.URL

	// The visited set is keyed by the canonical form, so fetch, persist and
	// retire the same spelling or a rediscovered page is stored twice under two
	// identities.
	if canonical, ok := utils.CanonicalizeUrl(rawUrl, ""); ok {
		rawUrl = canonical
	}

	verdict, err := s.policy.Admit(ctx, rawUrl)
	if err != nil {
		logger.Error("Could not ask the policy manager about a URL", "url", rawUrl, "error", err)
		s.requeue(ctx, rawUrl, logger)
		return
	}
	switch verdict.Kind {
	case policy.Allow:
	case policy.Defer:
		if verdict.Until.IsZero() {
			logger.Error("The policy manager deferred a URL with no due time, so it "+
				"cannot be parked and has been put back on the frontier",
				"url", rawUrl, "reason", verdict.Reason)
			s.requeue(ctx, rawUrl, logger)
		} else {
			pctx, cancel := bookkeepingContext(ctx)
			err := s.policy.Park(pctx, rawUrl, verdict.Until)
			cancel()
			if err != nil {
				logger.Error("Could not park a deferred URL", "url", rawUrl, "error", err)
			}
		}
		logger.Debug("URL deferred", "url", rawUrl, "reason", verdict.Reason, "until", verdict.Until)
		return
	case policy.Skip:
		// PopFrontier is destructive. A Skip that carries a future Until means
		// "not now", not "never": retiring it would blacklist every URL of a host
		// that was transiently cold or over budget for the deployment's lifetime
		// (H-08).
		if !verdict.Until.IsZero() && verdict.Until.After(s.policy.Now()) {
			pctx, cancel := bookkeepingContext(ctx)
			err := s.policy.Park(pctx, rawUrl, verdict.Until)
			cancel()
			if err != nil {
				logger.Error("Could not park a temporarily skipped URL", "url", rawUrl, "error", err)
			}
			logger.Debug("URL skipped until later", "url", rawUrl,
				"reason", verdict.Reason, "until", verdict.Until)
			return
		}
		rctx, cancel := bookkeepingContext(ctx)
		err := s.policy.Retire(rctx, rawUrl)
		cancel()
		if err != nil {
			logger.Error("Could not retire a skipped URL", "url", rawUrl, "error", err)
		}
		logger.Debug("URL skipped", "url", rawUrl, "reason", verdict.Reason)
		return
	default:
		logger.Error("The policy manager returned an unknown verdict", "url", rawUrl, "verdict", verdict.Kind)
		s.requeue(ctx, rawUrl, logger)
		return
	}

	body, out := s.policy.Fetch(ctx, rawUrl)

	action, err := s.policy.Classify(ctx, rawUrl, out)
	if err != nil {
		logger.Error("Could not record a fetch outcome",
			"url", rawUrl, "status", out.StatusCode, "error", err)
		s.requeue(ctx, rawUrl, logger)
		return
	}
	if action.Kind != policy.ActSuccess {
		logger.Info("URL not crawled", "url", rawUrl,
			"action", action.Kind, "reason", action.Reason, "retry_after", action.RetryAfter)
		return
	}

	page, err := s.parser.ParseHTML(bytes.NewReader(body), rawUrl)
	if err != nil {
		logger.Warn("Discarded a page that would not parse", "url", rawUrl, "error", err)
		dctx, cancel := bookkeepingContext(ctx)
		derr := s.policy.Discard(dctx, rawUrl, policy.ReasonBodyUnparseable)
		cancel()
		if derr != nil {
			logger.Error("Could not discard an unparseable page", "url", rawUrl, "error", derr)
		}
		return
	}
	// Storage identity is always the URL that was fetched. The parser may have
	// captured an `og:url` as display metadata, but a document does not get to
	// decide which `urls`/`pages` row it is written to (C-03).
	page.URL = rawUrl
	page.StatusCode = out.StatusCode
	page.HTML = body

	if !policy.IsEnglish(page.Lang) {
		logger.Info("Skipping non-English page", "url", rawUrl, "lang", page.Lang)
		dctx, cancel := bookkeepingContext(ctx)
		err := s.policy.Discard(dctx, rawUrl, policy.ReasonLanguageNotEnglish)
		cancel()
		if err != nil {
			logger.Error("Could not record a discarded page", "url", rawUrl, "error", err)
		}
		return
	}

	logger.Info(
		"Successfully processed page",
		"url",
		page.URL,
		"status_code",
		page.StatusCode,
		"links_found",
		len(page.Links),
		"image_count",
		len(page.Images),
	)

	admitted, refused := s.policy.AdmitLinks(ctx, page.Links)
	byReason := make(map[policy.Reason]int, len(refused))
	for _, reason := range refused {
		byReason[reason]++
	}
	for reason, n := range byReason {
		logger.Info("Page links declined", "url", rawUrl, "reason", reason, "count", n)
	}
	page.Links = admitted

	pageId, persistErr := s.store.Persist(ctx, page)
	if persistErr != nil || pageId == uuid.Nil() {
		logger.Warn("Page was not persisted; returning it to the frontier",
			"url", page.URL, "error", persistErr)
		s.requeue(ctx, rawUrl, logger)
		return
	}

	// Publish before retiring. Retiring first meant a confirmed-failed publish
	// still removed the URL from the crawl path; now the failure leaves it
	// reclaimable and the page is retried end to end (H-09).
	pubctx, pcancel := bookkeepingContext(ctx)
	err = queue.Publish(
		pubctx,
		s.config.App.JobQueue,
		messaging.NewIndexerJobPayload(pageId.String()),
	)
	pcancel()
	if err != nil {
		logger.Error(
			"Failed to publish job to indexer queue; returning the URL to the frontier",
			"url", page.URL, "error", err,
		)
		s.dropChannel(crawler_id)
		// A broker outage is not this URL's fault, so it must not spend the
		// page's fetch-attempt budget (which would retire it permanently).
		s.deferCrawl(ctx, rawUrl, logger)
		return
	}

	rctx, cancel := bookkeepingContext(ctx)
	err = s.policy.Retire(rctx, rawUrl)
	cancel()
	if err != nil {
		logger.Error("Could not mark a stored page visited", "url", rawUrl, "error", err)
	}

	dctx, dcancel := bookkeepingContext(ctx)
	s.policy.Discover(dctx, page.Links)
	dcancel()
}
