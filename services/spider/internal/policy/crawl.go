package policy

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/Hassan-ach/boogle/services/spider/internal/utils"
)

type Next struct {
	URL string

	Idle bool

	Promoted int
}

func (m *PolicyManager) TakeNext(ctx context.Context) (Next, error) {
	batch := m.cfg.DelayedPromoteBatch
	if batch < 1 {
		batch = defaultPromoteBatch
	}

	promoted, err := m.state.PromoteDelayed(ctx, m.now(), batch)
	if err != nil {
		m.log.Warn("could not promote due retries; the frontier is crawled without them",
			"error", err)
	}

	popBatch := m.cfg.FrontierPopBatch
	if popBatch < 1 {
		popBatch = defaultPopBatch
	}

	got, err := m.state.PopFrontier(ctx, popBatch)
	if err != nil {
		return Next{}, err
	}

	return Next{
		URL:      got.URL,
		Idle:     !got.Found && got.Exhausted,
		Promoted: promoted,
	}, nil
}

func (m *PolicyManager) DrainFrontier(ctx context.Context, limit int) ([]string, error) {
	popBatch := m.cfg.FrontierPopBatch
	if popBatch < 1 {
		popBatch = defaultPopBatch
	}

	var out []string
	for i := 0; limit <= 0 || i < limit; i++ {
		got, err := m.state.PopFrontier(ctx, popBatch)
		if err != nil {
			return out, err
		}
		if !got.Found {
			if got.Exhausted {
				return out, nil
			}
			continue
		}
		out = append(out, got.URL)
	}
	return out, nil
}

func (m *PolicyManager) Discover(ctx context.Context, links []string) {
	if len(links) == 0 {
		return
	}
	if err := m.state.Enqueue(ctx, links...); err != nil {
		m.log.Warn("could not enqueue discovered links",
			"count", len(links), "error", err)
	}
}

func (m *PolicyManager) Park(ctx context.Context, url string, due time.Time) error {
	if err := m.state.EnqueueDelayed(ctx, url, due); err != nil {
		return err
	}
	return nil
}

func (m *PolicyManager) Retire(ctx context.Context, url string) error {
	return m.state.MarkVisited(ctx, url)
}

var errNoFetcher = errors.New("no fetcher configured")

type Fetcher func(ctx context.Context, url string) ([]byte, Outcome)

// A whole-fetch budget, covering connect, TLS, redirects and body read together.
// Past this the origin is treated as a timeout and backed off rather than
// retried promptly.
const defaultFetchTimeout = 30 * time.Second

func NewHTTPFetcher(client *http.Client, cfg Config) Fetcher {
	if client == nil {
		client = &http.Client{Timeout: defaultFetchTimeout}
	}
	opts := utils.GetOptions{
		UserAgent:    defaultUserAgent(cfg),
		MaxBytes:     cfg.MaxBodyBytes,
		MaxRedirects: cfg.MaxRedirects,
	}

	return func(ctx context.Context, rawURL string) ([]byte, Outcome) {
		res, err := utils.GetReq(ctx, client, rawURL, opts)
		if res == nil {
			return nil, Outcome{Err: err}
		}
		out := Outcome{
			StatusCode:  res.StatusCode,
			ContentType: res.ContentType,
			BytesRead:   res.BytesRead,
			Redirects:   res.Redirects,
			FinalURL:    res.FinalURL,
		}
		if err != nil {
			out.Err = err
		}
		return res.Body, out
	}
}

func (m *PolicyManager) WithFetcher(f Fetcher) *PolicyManager {
	cp := *m
	if f != nil {
		cp.fetch = f
		cp.fetchClient = nil
	}
	return &cp
}

func (m *PolicyManager) WithFetchClient(client *http.Client) *PolicyManager {
	cp := *m
	if client == nil {
		return &cp
	}
	cp.fetchClient = client
	cp.fetch = NewHTTPFetcher(client, m.cfg)
	return &cp
}

func (m *PolicyManager) Fetch(ctx context.Context, url string) ([]byte, Outcome) {
	f := m.fetch
	if f == nil {
		if m.fetchClient == nil {
			return nil, Outcome{Err: errNoFetcher}
		}
		f = NewHTTPFetcher(m.fetchClient, m.cfg)
	}
	return f(ctx, url)
}
