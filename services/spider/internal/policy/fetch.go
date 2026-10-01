package policy

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/Hassan-ach/boogle/services/spider/internal/utils"
)

// errNoFetcher is a manager with no transport at all.
//
// Unreachable through New, which installs one. It exists so the failure is a
// classified backoff on the crawl's hot path rather than a nil dereference, which
// would take the process down over one worker's copy.
var errNoFetcher = errors.New("no fetcher configured")

// The transport, as the policy manager sees it.
//
// A fetch is the one thing in the crawl that is neither a decision nor a fact
// about stored state: it is an observation, and it is deliberately kept behind a
// function type for the same reason RobotsFetcher is. Classify's rules are the
// interesting part of this package, and they cannot be exercised against a real
// host -- the interesting cases are "a 404 is never fetched again" and "a 503 is
// parked for thirty seconds", both of which are assertions about what was
// written afterwards rather than about the network.
//
// # Single-shot, deliberately
//
// The retry loop that used to live in utils.GetReq is gone, and its absence is
// the design rather than a simplification of it. Retrying is a policy question --
// whether *this* URL deserves another attempt *now* -- and the only thing that
// can answer it is a manager holding the host's consecutive-failure count, this
// URL's own attempt count, and the delay robots.txt asked for. A helper holding
// none of those could only answer "yes, always, three times", inside a sleep,
// holding a worker slot the whole time.
//
// So a failed fetch is one call that returns, and the retry happens when the
// crawl comes back around and promotes the URL out of the delayed set.

// Fetcher performs one GET and reports everything observed about it.
//
// It never returns an error: every failure is reported through Outcome.Err, and
// a caller that treated "no error" as "something to do about it" would have no
// way to tell a transport failure from a 404. The body is returned separately
// because only the success path wants it, and handing back an error alongside it
// invites the caller to ask whether the body is valid when the error is nil.
type Fetcher func(ctx context.Context, url string) ([]byte, Outcome)

// defaultFetchTimeout bounds a page fetch for a manager built without a client of
// its own.
//
// It exists because http.DefaultClient has no timeout at all, and a host that
// accepts the connection and then never sends a body would hold a crawl worker
// until the OS gave up -- thirty seconds or more, twenty workers over, which is
// the stall this package was written to remove arriving by a different door.
// Production installs the spider's shared client, which has its own configured
// timeout; this is the floor for tests and short-lived tools.
const defaultFetchTimeout = 30 * time.Second

// NewHTTPFetcher builds a Fetcher over a shared client.
//
// The client is shared rather than created here wherever possible: the crawl
// already has one connection pool for every URL it fetches, and a second pool
// would be a second set of sockets to the same hosts.
//
// The user agent comes from the configuration rather than from a constant, for
// the reason documented on defaultUserAgent: the token a robots.txt addresses us
// by and the token on the wire have to be the same string.
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
			// No response at all: a dial, TLS, DNS or cancellation failure. The
			// error is what Classify reads, and the type is what tells a timeout
			// from a dead host.
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
			// A body that failed part way through. The status stays, because a
			// server that answered 200 and then dropped the connection has told us
			// something a bare transport error would not: it exists and it is
			// reachable.
			out.Err = err
		}
		return res.Body, out
	}
}

// WithFetcher returns a copy of the manager fetching through f.
//
// The manager is given a fetcher rather than an *http.Client so that the
// transport can be replaced wholesale in a test without a server, and so that
// nothing outside this package decides what a request looks like. A nil f leaves
// the existing one in place, which keeps WithFetcher safe to chain.
//
// Installing a fetcher also disowns the built-in one: from here on the fetcher is
// the caller's, and WithConfig will not replace it.
func (m *PolicyManager) WithFetcher(f Fetcher) *PolicyManager {
	cp := *m
	if f != nil {
		cp.fetch = f
		cp.fetchClient = nil
	}
	return &cp
}

// WithFetchClient installs the client the built-in fetcher uses.
//
// This is what production wants: the crawler has one connection pool for every
// URL it fetches, and a second pool inside the policy manager would be a second
// set of sockets to the same hosts. The caps still come from the configuration,
// unlike WithFetcher, which replaces the fetcher wholesale.
func (m *PolicyManager) WithFetchClient(client *http.Client) *PolicyManager {
	cp := *m
	if client == nil {
		return &cp
	}
	cp.fetchClient = client
	cp.fetch = NewHTTPFetcher(client, m.cfg)
	return &cp
}

// Fetch performs one fetch and reports what happened.
//
// It exists so the crawl loop never holds an HTTP client of its own. The loop's
// only job around a fetch is to hand what came back to Classify, and a loop that
// could reach the network directly is a loop that can decide what a fetch means
// without asking.
//
// The built-in fetcher is built in New and rebuilt by WithConfig, both of which
// happen before the crawl starts, so the steady-state path here allocates
// nothing. The lazy fallback exists for a manager assembled by hand rather than
// through New, and it deliberately does not cache: twenty workers share one
// manager, and writing to it from Fetch would be a data race on the crawl's
// hottest path.
func (m *PolicyManager) Fetch(ctx context.Context, url string) ([]byte, Outcome) {
	f := m.fetch
	if f == nil {
		if m.fetchClient == nil {
			// Unreachable through New and through every exported installer.
			// Handled rather than panicked because a nil here would take the
			// process down rather than one worker.
			return nil, Outcome{Err: errNoFetcher}
		}
		f = NewHTTPFetcher(m.fetchClient, m.cfg)
	}
	return f(ctx, url)
}
