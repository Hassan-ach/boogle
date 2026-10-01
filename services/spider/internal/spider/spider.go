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

// pageStore is the part of the store the crawl loop uses.
//
// It is an interface rather than *store.Store for one reason: the interesting
// behaviour of the loop is *which* page it decides to persist and which it
// decides not to, and a test that has to stand up Postgres to observe that cannot
// be written. The concrete store satisfies it unchanged.
type pageStore interface {
	Init(startUrls []string) error
	Persist(ctx context.Context, page *entity.Page, host *entity.Host) uuid.UUID
	Close()
}

// htmlParser turns a fetched body into a page.
//
// Also an interface, for the same reason, and for a second one: a parser that
// cannot fail is not a parser, so the "fetched fine, would not parse" path --
// which has to retire the URL rather than re-fetch it for ever -- needs a parser
// that fails on demand to be reachable at all.
type htmlParser interface {
	ParseHTML(r io.Reader, baseURL string) (*entity.Page, error)
}

type Spider struct {
	config     *config.Config
	httpClient *http.Client
	store      pageStore
	mq         messaging.MessagingQueue
	parser     htmlParser
	// policy owns every decision about what to crawl. The loop below asks it
	// what to fetch and what a fetch outcome means, and forms no opinion of its
	// own.
	policy *policy.PolicyManager

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

	// One cache connection, shared. The store needs it for host metadata and the
	// policy manager needs it for the frontier and the visited set, and two
	// connections would be two pools for one workload plus an argument about
	// which of them owns Close.
	cache := store.NewRedisClient(conf.Store.Cache)
	policyState := policy.NewRedisState(
		cache.Conn(),
		conf.Policy.RedisPrefix,
		conf.Policy.URLStateTTL,
	)

	policyManager := policy.New(conf.Policy, policyState, logger.Logger).
		// The crawler already has a connection pool for every URL it fetches;
		// giving the policy manager a second one would be a second set of sockets
		// to the same hosts. robots.txt goes through the same pool for the same
		// reason: it is fetched from the same hosts, at the same rate, under the
		// same timeout policy, and a caller reading either cannot see the other.
		WithFetchClient(httpClient).
		WithRobotsClient(httpClient).
		// Reading and parsing a sitemap is the parser's job and deciding what to do
		// with the result is the policy manager's. The seam exists so the manager
		// never has to know the XML, and so a sitemap is expanded once per host per
		// robots.txt reading -- rather than once per URL from a host we had never
		// seen, which is what the old code did, and why a dead domain cost a
		// request per URL for ever.
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

	// TakeNext promotes due retries and then pops the highest-priority unvisited
	// URL. Idle is a definitive "the frontier is empty", not a failure and not a
	// reason to try again this round.
	next, err := s.policy.TakeNext(ctx)
	if err != nil {
		logger.Warn("Could not take the next URL; skipping this round", "error", err)
		return
	}
	if next.Idle {
		return
	}

	rawUrl := next.URL

	// The gate. Everything this function used to decide for itself -- is this host
	// known, does it have robots rules, is the path refused, is there budget left
	// -- is answered here, from Redis, in one place. The switch is the only
	// branching on whether to fetch that exists in the tree.
	verdict, err := s.policy.Admit(ctx, rawUrl)
	if err != nil {
		logger.Error("Could not ask the policy manager about a URL", "url", rawUrl, "error", err)
		return
	}
	switch verdict.Kind {
	case policy.Allow:
		// Carry on below.
	case policy.Defer:
		// Parked, and that verb is doing the work. The URL is already off the
		// frontier: it was popped before the gate was asked. So a deferred URL is
		// not "still queued", it is gone unless it is put back here, and parking is
		// the only thing that puts it back.
		//
		// Every Defer the policy manager produces carries a due time -- the two
		// cases where it cannot know when the *host* may be crawled still know
		// when to ask again -- and TestEveryDeferComesWithADueTime is what holds
		// them to that. The check below is therefore a backstop against a verdict
		// kind added later without one, and it shouts when it fires for that
		// reason: silently doing nothing is exactly the failure this loop is
		// guarding against.
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
		// Terminal, and recorded as such: a Skip is the difference between "not now"
		// and "not ever", and forgetting that is how a disallowed path comes back
		// every time a page links to it.
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

	// One fetch, one decision about it. The retry loop that used to be here slept
	// inside a worker slot with a fixed count; the retry is now the delayed set,
	// which is promoted by a later crawl and knows this host's failure history.
	body, out := s.policy.Fetch(ctx, rawUrl)

	action, err := s.policy.Classify(ctx, rawUrl, out)
	if err != nil {
		// The outcome could not be recorded, so the URL's state and the host's
		// counters have not moved. Dropping the round is the only answer that
		// cannot produce work nobody authorised; the alternative -- treating an
		// unrecorded outcome as success -- is how a page gets indexed and then
		// crawled again.
		logger.Error("Could not record a fetch outcome",
			"url", rawUrl, "status", out.StatusCode, "error", err)
		return
	}
	if action.Kind != policy.ActSuccess {
		// Classify has already parked or retired the URL, counted the reason and
		// set whatever marker the failure implies. Nothing is left for this loop
		// to do but not fetch it again.
		logger.Info("URL not crawled", "url", rawUrl,
			"action", action.Kind, "reason", action.Reason, "retry_after", action.RetryAfter)
		return
	}

	page, err := s.parser.ParseHTML(bytes.NewReader(body), rawUrl)
	if err != nil {
		// Fetched, and then found not to be a page. Retiring it matters: the
		// alternative is re-fetching the same unparseable body every time anything
		// links to it, which is a loop with an unbounded budget.
		logger.Warn("Discarded a page that would not parse", "url", rawUrl, "error", err)
		if derr := s.policy.Discard(ctx, rawUrl, policy.ReasonBodyUnparseable); derr != nil {
			logger.Error("Could not discard an unparseable page", "url", rawUrl, "error", derr)
		}
		return
	}
	if page.URL == "" {
		// The parser was given the URL as its base and did not use it.
		page.URL = rawUrl
	}
	page.StatusCode = out.StatusCode
	page.HTML = body

	// The one check the policy manager cannot make. It reads a document, so it
	// needs the document, and a lang attribute is only knowable after the fetch.
	// The parser no longer decides this -- it reports what the document claims and
	// the policy manager judges and counts it, where before the parser returned an
	// error and every non-English page in the corpus logged as a failed fetch with
	// nothing counted anywhere.
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

	// The page's links go through the same gate as everything else, so a link this
	// crawler declines is declined with a reason rather than silently dropped.
	//
	// It used to be utils.ValidateLinks, which returned one list and no
	// explanation: the links it removed left no trace, so a page with forty links
	// and eleven PDFs produced eleven entries that a later function removed with
	// nothing recorded, and the crawl log could not say how much of a site was
	// being declined. AdmitLinks counts each refusal against the host as it goes.
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
		// Not indexed, so not visited: the page is retired only once the insert
		// has committed, and a page that failed to be stored is a page worth
		// fetching again. The host's page count has already moved, which is the
		// safe direction to be wrong in -- it spends budget rather than granting it.
		logger.Warn("Page was not persisted; leaving it crawlable", "url", page.URL)
		return
	}

	// In the index, so no longer a crawl candidate. This used to be the last two
	// lines of store.persistPage, which is the wrong place for it: a store that
	// writes to the database is not entitled to decide when a URL has been seen,
	// and doing it there meant the decision could not be counted, retried or
	// reasoned about from anywhere except the function that happened to insert the
	// row.
	if err := s.policy.Retire(ctx, rawUrl); err != nil {
		logger.Error("Could not mark a stored page visited", "url", rawUrl, "error", err)
	}

	// And the links it found are the crawl's next round of work. Enqueue does not
	// consult the visited set, so a page that links to itself comes back round
	// once; it is not crawled twice because the pop asks Admit first, which is the
	// same check done once per pop instead of once per link on every page.
	s.policy.Discover(ctx, page.Links)

	if err := queue.Publish(
		"indexer.jobs",
		messaging.NewIndexerJobPayload(pageId.String()),
	); err != nil {
		s.logger.Error(
			"Failed to publish job to indexer queue",
			"url", page.URL, "error", err,
		)
	}
}
