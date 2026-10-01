package spider

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
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
	"github.com/Hassan-ach/boogle/services/spider/internal/store"
	"github.com/Hassan-ach/boogle/services/spider/internal/utils"
)

type pageStore interface {
	Init(startUrls []string) error
	Persist(ctx context.Context, page *entity.Page, host *entity.Host) uuid.UUID
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
}

func NewSpider(conf *config.Config) *Spider {
	httpClient := &http.Client{Timeout: time.Duration(conf.App.HttpTimeout) * time.Second}
	logger := utils.NewMultiLogger(conf.App.LogsPath)

	mq, err := messaging.NewRabbitMQ(&conf.RabbitMq, logger)
	if err != nil {
		log.Fatal("Failed to initialize RabbitMQ", "error", err)
	}
	ctx, cancel := context.WithCancel(context.Background())

	cache := store.NewRedisClient(conf.Store.Cache)
	policyState := policy.NewRedisState(
		cache.Conn(),
		conf.Policy.RedisPrefix,
		conf.Policy.URLStateTTL,
	)

	policyManager := policy.New(conf.Policy, policyState, logger.Logger).
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
		store:          store.NewStore(conf.Store, logger, cache, policyState),
		policy:         policyManager,
		wg:             sync.WaitGroup{},
		ctx:            ctx,
		cancel:         cancel,
		crawlerTimeout: time.Duration(conf.App.CrawlerTimeout) * time.Second,
		crawlerDelay:   time.Duration(conf.App.ClawlerDelay) * time.Millisecond,
		fetchpool:      make(chan struct{}, conf.App.MaxConcurrentFetch),
		logger:         logger,
	}

	fmt.Printf("Spider initialized: %+v\n", s)
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
	s.store.Close()
	s.logger.Close()
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

func (s *Spider) crawl(crawler_id int) {
	ctx, cancel := context.WithTimeout(s.ctx, s.crawlerTimeout)
	defer cancel()

	logger := s.logger.With("component", "crawler", "crawler_id", crawler_id)

	queue, err := s.mq.NewMessagingChannel()
	if err != nil {
		logger.Error("Failed to create messaging channel", "error", err)
		return
	}
	defer queue.Close()

	select {
	case s.fetchpool <- struct{}{}:
	case <-ctx.Done():
		fmt.Println("Crawler timed out waiting for fetch slot")
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

	verdict, err := s.policy.Admit(ctx, rawUrl)
	if err != nil {
		logger.Error("Could not ask the policy manager about a URL", "url", rawUrl, "error", err)
		return
	}
	switch verdict.Kind {
	case policy.Allow:
	case policy.Defer:
		if verdict.Until.IsZero() {
			logger.Error("The policy manager deferred a URL with no due time, so it "+
				"cannot be parked and has been dropped from the frontier",
				"url", rawUrl, "reason", verdict.Reason)
		} else if err := s.policy.Park(ctx, rawUrl, verdict.Until); err != nil {
			logger.Error("Could not park a deferred URL", "url", rawUrl, "error", err)
		}
		logger.Debug("URL deferred", "url", rawUrl, "reason", verdict.Reason, "until", verdict.Until)
		return
	case policy.Skip:
		if err := s.policy.Retire(ctx, rawUrl); err != nil {
			logger.Error("Could not retire a skipped URL", "url", rawUrl, "error", err)
		}
		logger.Debug("URL skipped", "url", rawUrl, "reason", verdict.Reason)
		return
	default:
		logger.Error("The policy manager returned an unknown verdict", "url", rawUrl, "verdict", verdict.Kind)
		return
	}

	host := verdict.Host

	body, out := s.policy.Fetch(ctx, rawUrl)

	action, err := s.policy.Classify(ctx, rawUrl, out)
	if err != nil {
		logger.Error("Could not record a fetch outcome",
			"url", rawUrl, "status", out.StatusCode, "error", err)
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
		if derr := s.policy.Discard(ctx, rawUrl, policy.ReasonBodyUnparseable); derr != nil {
			logger.Error("Could not discard an unparseable page", "url", rawUrl, "error", derr)
		}
		return
	}
	if page.URL == "" {
		page.URL = rawUrl
	}
	page.StatusCode = out.StatusCode
	page.HTML = body

	if !policy.IsEnglish(page.Lang) {
		logger.Info("Skipping non-English page", "url", rawUrl, "lang", page.Lang)
		if err := s.policy.Discard(ctx, rawUrl, policy.ReasonLanguageNotEnglish); err != nil {
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

	pageId := s.store.Persist(ctx, page, host)
	if pageId == uuid.Nil() {
		logger.Warn("Page was not persisted; leaving it crawlable", "url", page.URL)
		return
	}

	if err := s.policy.Retire(ctx, rawUrl); err != nil {
		logger.Error("Could not mark a stored page visited", "url", rawUrl, "error", err)
	}

	s.policy.Discover(ctx, page.Links)

	if err := queue.Publish(
		s.config.App.JobQueue,
		messaging.NewIndexerJobPayload(pageId.String()),
	); err != nil {
		s.logger.Error(
			"Failed to publish job to indexer queue",
			"url", page.URL, "error", err,
		)
	}
}
