package spider

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Hassan-ach/boogle/services/spider/internal/config"
	"github.com/Hassan-ach/boogle/services/spider/internal/entity"
	"github.com/Hassan-ach/boogle/services/spider/internal/parser"
	"github.com/Hassan-ach/boogle/services/spider/internal/store"
	"github.com/Hassan-ach/boogle/services/spider/internal/utils"
)

type Spider struct {
	config     *config.Config
	httpClient *http.Client
	store      *store.Store
	parser     *parser.Parser

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
	ctx, cancel := context.WithCancel(context.Background())

	s := &Spider{
		config:         conf,
		httpClient:     httpClient,
		parser:         parser.NewParser(httpClient, logger),
		store:          store.NewStore(conf.Store, logger),
		wg:             sync.WaitGroup{},
		ctx:            ctx,
		cancel:         cancel,
		crawlerTimeout: time.Duration(conf.App.CrawlerTimeout) * time.Second,
		crawlerDelay:   time.Duration(conf.App.ClawlerDelay) * time.Microsecond,
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

	select {
	case s.fetchpool <- struct{}{}:
	case <-ctx.Done():
		fmt.Println("Crawler timed out waiting for fetch slot")
		return
	}
	defer func() { <-s.fetchpool }()

	rawUrl, ok, err := s.store.GetNextUrl(ctx)
	if err != nil || !ok {
		// logger.Warn("Failed to fetch next URL from store", "error", err)
		return
	}

	logger.Info("Fetched URL from store",
		"url", rawUrl)

	u, err := url.Parse(rawUrl)
	if err != nil {
		logger.Error("Failed to parse URL",
			"url", rawUrl, "error", err)
		return
	}

	host, ok, err := s.store.GetHostMetaData(ctx, u.Host)
	if err != nil {
		s.logger.Warn("Failed to retrieve host metadata from store, will attempt to generate",
			"host", u.Host, "error", err)
	}
	if !ok {
		logger.Info("Host metadata not found in store, generating new metadata",
			"host", u.Host)
		// Host metadata missing; generate using parser
		host, err = s.newHostMetaData(ctx, u.Host)
		if err != nil {
			logger.Error("generate host metadata",
				"host", u.Host, "error", err)

			// use a safe default
			host = &entity.Host{
				MaxRetry:        5,
				MaxPages:        10,
				PagesCrawled:    0,
				Delay:           5,
				Name:            u.Host,
				AllowedUrls:     []string{},
				NotAllowedPaths: []string{},
			}
		} else {
			logger.Info(
				"Host metadata retrieved",
				"host",
				host.Name,
				"delay",
				host.Delay,
				"max_retry",
				host.MaxRetry,
				"not_allowed_paths",
				len(host.NotAllowedPaths),
				"allowed_urls",
				len(host.AllowedUrls),
			)
		}
	}

	page, err := s.fetchAndParse(rawUrl, host.MaxRetry, host.Delay)
	if err != nil {
		logger.Error("Failed to fetch and parse page",
			"url", rawUrl, "error", err)
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

	normUrls := utils.ValidateLinks(page.Links, host.NotAllowedPaths)
	page.Links = normUrls

	host.PagesCrawled++
	s.store.Persist(ctx, page, host)
}

func (s *Spider) fetchAndParse(
	u string,
	maxRetry, delay int,
) (*entity.Page, error) {
	body, statusCode, err := utils.GetReq(s.httpClient, u, maxRetry, delay)
	if err != nil {
		// Failed to fetch page after retries
		// Suggest logging the URL and retry parameters
		return nil, fmt.Errorf("GET request failed: %w", err)
	}

	page, err := s.parser.ParseHTML(bytes.NewReader(body), u)
	if err != nil {
		// Failed to parse HTML
		return nil, fmt.Errorf("HTML parsing: %w", err)
	}

	if page.URL == "" {
		// Ensure Page.Url is always set
		page.URL = u
	}
	page.StatusCode = statusCode // Store HTTP status code
	page.HTML = body             // Store raw HTML

	return page, nil
}

func (s *Spider) newHostMetaData(ctx context.Context, raw string) (host *entity.Host, err error) {
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		raw = "https://" + raw
	}

	u, err := url.Parse(strings.TrimPrefix(raw, "www."))
	if err != nil {
		return
	}

	r, err := s.newRobots(u)
	if err != nil {
		return nil, err
	}

	sitemaps := parser.FetchSitemaps(s.httpClient, r.SiteMaps, u)

	// create Host object
	host = &entity.Host{
		MaxRetry:        5,
		Delay:           r.CrawlDelay,
		MaxPages:        10,
		PagesCrawled:    0,
		Name:            u.Host,
		AllowedUrls:     r.Allow,
		NotAllowedPaths: r.Disallow,
	}

	s.logger.Info(
		"Generated host metadata",
		"component",
		"spider",
		"host",
		host.Name,
		"delay",
		host.Delay,
	)

	// persist in store
	cache := s.store.GetCache()
	err = cache.AddHostMetaData(ctx, host.Name, host)
	if err != nil {
		s.logger.Error("Failed to store host metadata in cache", "error", err)
	}
	err = cache.AddUrls(ctx, sitemaps)
	if err != nil {
		s.logger.Error("Failed to add sitemap URLs to cache", "error", err)
	}

	return host, nil
}

func (s *Spider) newRobots(u *url.URL) (*entity.Robots, error) {
	h := strings.TrimPrefix(u.Host, "www.")

	u.Scheme = "https"
	u.Host = h
	u.RawQuery = ""
	u.Fragment = ""
	u.Path = "/robots.txt"

	robotsURL := u.String()

	// fetch robots.txt
	var body []byte
	body, _, err := utils.GetReq(s.httpClient, robotsURL, 3, 5)
	if err != nil {
		return nil, fmt.Errorf("get robots.txt: %w", err)
	}

	// parse robots.txt for rules and sitemaps
	r := s.parser.ParseRobots(string(body), "*")
	return r, nil
}
