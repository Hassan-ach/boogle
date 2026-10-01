package utils

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
)

const MaxResponseBytes = 10 << 20

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
	if opts.MaxRedirects > 0 {
		sender.CheckRedirect = func(_ *http.Request, via []*http.Request) error {
			followed = len(via)
			if followed > opts.MaxRedirects {
				return http.ErrUseLastResponse
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
