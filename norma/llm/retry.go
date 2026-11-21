package llm

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// HTTPStatusError preserves HTTP status and parsed Retry-After duration while
// formatting identically to the historical "<prefix>: status <code>: <body>".
type HTTPStatusError struct {
	Prefix     string
	StatusCode int
	Body       string
	RetryAfter time.Duration
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("%s: status %d: %s", e.Prefix, e.StatusCode, e.Body)
}

// doStream sends the streaming request with retries on transient failures and
// returns a 200 response (caller reads + closes the body). Only the request
// establishment phase is retried — connection reset / timeout / 429 / 5xx before
// any stream data is consumed — so retrying is safe (it can't duplicate
// partially-streamed assistant output). A mid-stream drop is the caller's to
// surface. Context cancellation is never retried and interrupts the backoff.
// Count comes from cfg.retries() (or WithMaxRetries on ctx), the wait between
// attempts from Retry-After or cfg.retryDelay().
func doStream(ctx context.Context, cfg Config, url string, body []byte, setHeaders func(*http.Request), errPrefix string) (*http.Response, error) {
	// Rate limit once per logical request (outside the retry loop, so retries do
	// not consume additional tokens). Shared across all callers of this provider.
	if err := cfg.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	retries := cfg.retries()
	if n, ok := maxRetriesFromContext(ctx); ok {
		retries = max(n, 0)
	}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		setHeaders(req)

		client := cfg.HTTPClient
		if client == nil {
			client = http.DefaultClient
		}
		resp, err := client.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			return resp, nil
		}

		retryable := false
		var retryAfter time.Duration
		switch {
		case err != nil:
			retryable = ctx.Err() == nil // transient network error (not cancellation)
		case isRetryableStatus(resp.StatusCode):
			retryable = true
			retryAfter = parseRetryAfter(resp.Header)
		}
		if attempt >= retries || !retryable {
			if err != nil {
				return nil, err
			}
			data, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if retryAfter == 0 {
				retryAfter = parseRetryAfter(resp.Header)
			}
			return nil, &HTTPStatusError{
				Prefix:     errPrefix,
				StatusCode: resp.StatusCode,
				Body:       strings.TrimSpace(string(data)),
				RetryAfter: retryAfter,
			}
		}
		if resp != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
		}
		wait := cfg.retryDelay(attempt)
		if cfg.RetryInterval == 0 {
			wait = jitterDuration(wait)
		}
		if retryAfter > 0 && retryAfter <= 15*time.Second && retryAfter > wait {
			wait = retryAfter
		}
		if !backoffSleep(ctx, wait) {
			return nil, ctx.Err() // context cancelled during backoff
		}
	}
}

func isRetryableStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout, // 408
		http.StatusTooManyRequests,     // 429
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout,      // 504
		529:                            // Anthropic overloaded_error
		return true
	}
	return false
}

func parseRetryAfter(h http.Header) time.Duration {
	if h == nil {
		return 0
	}
	raw := strings.TrimSpace(h.Get("Retry-After"))
	if raw == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(raw, 64); err == nil && secs > 0 {
		return time.Duration(secs * float64(time.Second))
	}
	if t, err := http.ParseTime(raw); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// expBackoff is the default retry ladder: 0.5s, 1s, 2s, … capped at 8s. Used
// whenever the caller did not pin a fixed interval on the Config.
func expBackoff(attempt int) time.Duration {
	shift := min(attempt, 10)
	d := 500 * time.Millisecond * (1 << shift)
	if d > 8*time.Second {
		d = 8 * time.Second
	}
	return d
}

// jitterDuration applies half-jitter [0.5*d, d] to desynchronize concurrent
// retries across workers without delaying past d.
func jitterDuration(d time.Duration) time.Duration {
	half := d / 2
	if half <= 0 {
		return d
	}
	return half + time.Duration(rand.Int64N(int64(half)+1))
}

// backoffSleep waits d, interruptible by ctx. Returns false if the context was
// cancelled (or d is non-positive, which never happens for a real wait).
func backoffSleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
