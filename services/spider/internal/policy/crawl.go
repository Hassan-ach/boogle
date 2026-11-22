package policy

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/Hassan-ach/boogle/services/spider/internal/state"
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
		batch = state.DefaultPromoteBatch
	}

	promoted, err := m.st.PromoteDelayed(ctx, m.now(), batch)
	if err != nil {
		m.log.Warn("could not promote due retries; the frontier is crawled without them",
			"error", err)
	}

	popBatch := m.cfg.FrontierPopBatch
	if popBatch < 1 {
		popBatch = state.DefaultPopBatch
	}

	got, err := m.st.PopFrontier(ctx, popBatch)
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
		popBatch = state.DefaultPopBatch
	}

	var out []string
	for i := 0; limit <= 0 || i < limit; i++ {
		got, err := m.st.PopFrontier(ctx, popBatch)
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
	if err := m.st.Enqueue(ctx, links...); err != nil {
		m.log.Warn("could not enqueue discovered links",
			"count", len(links), "error", err)
	}
}

func (m *PolicyManager) Park(ctx context.Context, url string, due time.Time) error {
	if err := m.st.EnqueueDelayed(ctx, url, due); err != nil {
		return err
	}
	return nil
}

func (m *PolicyManager) Retire(ctx context.Context, url string) error {
	return m.st.MarkVisited(ctx, url)
}

// Requeue returns a URL to the frontier after an internal failure that the
// classification path never saw. It spends the same attempt budget and uses the
// same backoff as a failed fetch, so a URL that keeps breaking the crawler is
// eventually retired instead of being refetched every requeue delay forever.
func (m *PolicyManager) Requeue(ctx context.Context, url string) error {
	attempts, err := m.st.BumpAttempts(ctx, url)
	if err != nil {
		return err
	}
	if attempts >= m.cfg.URLMaxAttempts {
		return m.Retire(ctx, url)
	}
	due := m.now().Add(m.cfg.URLBackoff(attempts))
	return m.Park(ctx, url, due)
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
