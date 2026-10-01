package utils

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// MaxResponseBytes caps how much of a response is read when the caller states no
// cap of its own. A crawler pointed at the wrong URL will happily start streaming
// a multi-gigabyte file into memory.
const MaxResponseBytes = 10 << 20 // 10 MB

// defaultUserAgent is what we claim to be when a caller states none. It is a
// browser string because a large share of sites serve a degraded page to
// anything obviously automated, and a crawler that is served the interstitial
// indexes the interstitial.
//
// The spider does not use it. It states the user agent the policy manager
// matched its robots.txt groups against, so the token a site addresses us by and
// the token on the wire are the same string -- a mismatch there is a site whose
// "Disallow: /" for our name never applied to us.
const defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/118.0.5993.118 Safari/537.36"

// Response is one HTTP exchange, reported whole.
//
// Every field is an observation rather than an interpretation. Whether a body is
// worth indexing, whether a status means "try again" and how long to wait are
// policy answers, and this type deliberately holds no opinion about any of them:
// it is what the transport saw, handed to something that decides.
type Response struct {
	// StatusCode is the server's answer, or 0 when no response ever arrived.
	StatusCode int
	// ContentType is the raw header, parameters and all.
	ContentType string
	// Body is what was read. Empty on a failure, and possibly truncated -- see
	// BytesRead.
	Body []byte
	// BytesRead is how many bytes of Body were actually read. A value above
	// GetOptions.MaxBytes means the body was cut off at the cap, which is a
	// different thing from a body that happened to end there.
	BytesRead int
	// Redirects counts hops followed. At the limit it is one past the number
	// actually followed, because the refused hop is counted too: the caller has
	// to be able to tell "stopped at the limit" from "finished the chain".
	Redirects int
	// FinalURL is where the exchange ended, which after a refused redirect is the
	// response that refused rather than where it pointed.
	FinalURL string
}

// GetOptions configures one fetch. The zero value is usable: the package's
// default user agent, its default byte cap, and the client library's own
// redirect handling.
type GetOptions struct {
	// UserAgent is the crawler's identity.
	UserAgent string

	// MaxBytes caps the body read. One byte past the cap is read anyway, so that
	// a body truncated at the cap can be told apart from one that ended there;
	// that is what makes Response.BytesRead a fact rather than a guess.
	//
	// Zero or less means MaxResponseBytes. There is deliberately no "unlimited":
	// an unbounded read is a way to make a crawler hold memory until it dies.
	MaxBytes int

	// MaxRedirects is the longest chain worth following. Zero or less leaves the
	// client library's own behaviour alone.
	MaxRedirects int
}

// GetReq performs exactly one GET and reports what happened.
//
// It used to retry. The retry loop is gone, and that is the point of the change
// rather than a simplification of it: a retry is a decision about whether this
// URL is worth another attempt right now, and the only thing that can answer it
// is the policy manager, which knows the host's consecutive-failure count, this
// URL's own attempt count and what the host's robots.txt asked for. A helper
// that knew none of those could only answer "yes, always, three times", which is
// how a dead domain used to cost fifteen seconds per URL in a worker slot.
//
// A failed fetch is reported through Response and the error together: a response
// that arrived with an error status has both, and the caller is expected to prefer
// the status. That is why the two are returned rather than folded into one.
func GetReq(ctx context.Context, client *http.Client, rawURL string, opts GetOptions) (*Response, error) {
	if client == nil {
		return nil, errors.New("no http client configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("request initialization failed: %w", err)
	}
	ua := opts.UserAgent
	if ua == "" {
		ua = defaultUserAgent
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "text/html")
	req.Header.Set("Accept-Language", "en-US")

	limit := opts.MaxBytes
	if limit <= 0 {
		limit = MaxResponseBytes
	}

	// followed is read after Do returns, so it holds the real hop count however
	// the chain ended.
	followed := 0

	// A copy of the client, never the original. The spider's client is shared
	// with the robots fetcher and every other caller, and installing a redirect
	// policy on it would change their behaviour too. The copy shares the
	// Transport, so this costs no extra connection pool.
	sender := *client
	if opts.MaxRedirects > 0 {
		sender.CheckRedirect = func(_ *http.Request, via []*http.Request) error {
			followed = len(via)
			if followed > opts.MaxRedirects {
				// Refuse the hop and hand back the redirect itself rather than
				// failing the call: the 3xx is a real answer from the server, and
				// it is what tells the caller the chain did not resolve.
				return http.ErrUseLastResponse
			}
			return nil
		}
	}

	res, err := sender.Do(req)
	return finish(res, err, followed, limit)
}

// finish reads the body and assembles the Response.
//
// An error here means no response was ever seen -- a dial, TLS or DNS failure,
// or a redirect policy that refused to hand back anything -- so there is nothing
// to report and the caller gets the transport error alone. A *failed* fetch that
// still has a status (a 503, a 404) is not an error here at all: the status is
// the answer, and it arrives on the Response.
func finish(res *http.Response, err error, redirects, limit int) (*Response, error) {
	if err != nil {
		return nil, err
	}
	defer drainAndClose(res.Body)

	out := &Response{
		StatusCode:  res.StatusCode,
		ContentType: res.Header.Get("Content-Type"),
		Redirects:   redirects,
	}
	if res.Request != nil && res.Request.URL != nil {
		out.FinalURL = res.Request.URL.String()
	}

	body, readErr := io.ReadAll(io.LimitReader(res.Body, int64(limit)+1))
	out.Body = body
	out.BytesRead = len(body)
	if readErr != nil {
		// The status is returned alongside the error on purpose. A body that
		// failed half way through still tells us the server answered, and "the
		// server answered 200 and then the connection broke" is a different
		// decision from "we never reached the server".
		return out, fmt.Errorf("failed to read response: %w", readErr)
	}
	return out, nil
}

// drainAndClose empties a discarded body so the connection can be reused for the
// next request. Closing without reading forces the transport to tear the TCP
// connection down, which turns one fetch per worker into one fresh handshake per
// page.
func drainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 64<<10))
	_ = body.Close()
}
