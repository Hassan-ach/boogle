package utils

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
)

const MaxResponseBytes = 10 << 20

// Redirects followed when the caller does not set a limit. Bounded so a
// redirect chain cannot keep one worker busy indefinitely.
const defaultMaxRedirects = 10

// ErrRedirectScheme marks a redirect the crawler refuses to follow, so the
// policy layer can retire the URL instead of retrying it.
var ErrRedirectScheme = errors.New("redirect to an unsupported scheme")

const defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/118.0.5993.118 Safari/537.36"

type Response struct {
	StatusCode  int
	ContentType string
	Body        []byte
	BytesRead   int
	Redirects   int
	FinalURL    string
}

type GetOptions struct {
	UserAgent string

	MaxBytes int

	MaxRedirects int
}

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

	followed := 0

	sender := *client
	// A caller that installed its own redirect policy without asking for a limit
	// keeps it. Otherwise install a bounded policy: the transport dialer blocks
	// private destinations, and this stops a page from bouncing the crawler
	// through unlimited hops or into a non-HTTP scheme.
	if opts.MaxRedirects > 0 || sender.CheckRedirect == nil {
		maxRedirects := opts.MaxRedirects
		if maxRedirects <= 0 {
			maxRedirects = defaultMaxRedirects
		}
		sender.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			followed = len(via)
			if followed > maxRedirects {
				return http.ErrUseLastResponse
			}
			if req.URL == nil {
				return errors.New("redirect without a target URL")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("%w: %q", ErrRedirectScheme, req.URL.Scheme)
			}
			return nil
		}
	}

	res, err := sender.Do(req)
	return finish(res, err, followed, limit)
}

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
		return out, fmt.Errorf("failed to read response: %w", readErr)
	}
	return out, nil
}

func drainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 64<<10))
	_ = body.Close()
}
