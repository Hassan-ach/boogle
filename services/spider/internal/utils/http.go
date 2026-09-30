package utils

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// MaxResponseBytes caps how much of a response is read. A crawler pointed at
// the wrong URL will happily start streaming a multi-gigabyte file into memory.
const MaxResponseBytes = 10 << 20 // 10 MB

// ErrNoBody is returned when every attempt produced an error with no HTTP
// response to report -- typically a dial or TLS failure.
var ErrNoBody = errors.New("no response received")

// GetReq performs a GET with a browser-ish User-Agent and returns the body and
// status code.
//
// maxAttempts is the *total* number of attempts, not the number of retries after
// the first, and delay is the pause in seconds between attempts. Both used to be
// misread: the loop was `for attempt := range maxRetry`, so a caller asking for
// "3 retries" got 3 attempts, and a caller passing 0 got no attempts at all and
// an error wrapping a nil error ("all 0 retries failed: %!w(<nil>)").
//
// maxAttempts is a number stored per host in the database, so 0 is reachable
// from data, not just from a bad literal in code. It is clamped to 1 here so a
// host row can never make its entire subtree uncrawlable.
func GetReq(
	client *http.Client,
	url string,
	maxAttempts, delay int,
) ([]byte, int, error) {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	if delay < 0 {
		delay = 0
	}
	if client == nil {
		return nil, 0, errors.New("no http client configured")
	}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("request initialization failed: %w", err)
	}
	req.Header.Set(
		"User-Agent",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/118.0.5993.118 Safari/537.36",
	)
	req.Header.Set("Accept", "text/html")
	req.Header.Set("Accept-Language", "en-US")

	var (
		lastErr  error
		lastCode int
	)

	for attempt := range maxAttempts {
		if attempt > 0 && delay > 0 {
			time.Sleep(time.Second * time.Duration(delay))
		}

		res, err := client.Do(req)
		if err != nil {
			lastErr = err
			lastCode = 0
			continue
		}

		statusCode := res.StatusCode

		// Server-side and rate-limit failures are worth another attempt.
		// Everything else is the server's final answer.
		if statusCode >= 500 || statusCode == http.StatusTooManyRequests {
			lastCode = statusCode
			lastErr = fmt.Errorf("server returned %d", statusCode)
			drainAndClose(res.Body)
			continue
		}

		// 4xx is a client error: retrying the same request cannot help, and
		// hammering a 404 or 403 is what gets a crawler banned.
		if statusCode >= 400 {
			drainAndClose(res.Body)
			return nil, statusCode, fmt.Errorf("client error: %d", statusCode)
		}

		body, err := io.ReadAll(io.LimitReader(res.Body, MaxResponseBytes))
		_ = res.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("failed to read response: %w", err)
			lastCode = statusCode
			continue
		}

		return body, statusCode, nil
	}

	if lastErr == nil {
		// Unreachable with maxAttempts >= 1, but a caller must never receive
		// an error that wraps nil.
		lastErr = ErrNoBody
	}
	return nil, lastCode, fmt.Errorf("gave up after %d attempts: %w", maxAttempts, lastErr)
}

// drainAndClose empties a discarded body so the connection can be reused for
// the next attempt. Closing without reading forces the transport to tear the
// TCP connection down, which turns a 5-attempt retry into 5 fresh handshakes.
func drainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 64<<10))
	_ = body.Close()
}
