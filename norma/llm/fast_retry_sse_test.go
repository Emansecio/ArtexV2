package llm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestIsRetryableStatusIncludes529(t *testing.T) {
	if !isRetryableStatus(529) {
		t.Fatal("529 overloaded_error should be retryable")
	}
	if !isRetryableStatus(429) || isRetryableStatus(400) || isRetryableStatus(401) {
		t.Fatal("unexpected retry classification")
	}
}

func TestDoStreamWithMaxRetriesZeroAndRetryAfterError(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "4")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, "slow down")
	}))
	defer srv.Close()

	ctx := WithMaxRetries(context.Background(), 0)
	_, err := doStream(ctx, Config{Format: FormatAnthropic, HTTPClient: srv.Client()}, srv.URL, []byte(`{}`), func(*http.Request) {}, "anthropic")
	if err == nil {
		t.Fatal("expected HTTP error")
	}
	if calls.Load() != 1 {
		t.Fatalf("expected 1 attempt with WithMaxRetries(ctx, 0), got %d", calls.Load())
	}
	var httpErr *HTTPStatusError
	if !errors.As(err, &httpErr) {
		t.Fatalf("expected *HTTPStatusError, got %T (%v)", err, err)
	}
	if httpErr.StatusCode != 429 || httpErr.RetryAfter != 4*time.Second {
		t.Fatalf("httpErr = %+v, want 429 / 4s", httpErr)
	}
}

func TestSSEScannerIdleTimeout(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()

	scan := newSSEScannerWithIdleTimeout(pr, 40*time.Millisecond)
	defer scan.Close()

	_, _, err := scan.next()
	if !errors.Is(err, ErrStreamIdleTimeout) {
		t.Fatalf("err = %v, want ErrStreamIdleTimeout", err)
	}
}
