package tool

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type stubSearchProvider struct {
	name  string
	calls atomic.Int32
	delay time.Duration
}

func (s *stubSearchProvider) Name() string { return s.name }

func (s *stubSearchProvider) Search(_ context.Context, query string, limit int) ([]SearchResult, error) {
	s.calls.Add(1)
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	return []SearchResult{{Title: "hit", URL: "https://example.com", Description: query, Position: 1}}, nil
}

func TestWebSearchCacheAndSingleflight(t *testing.T) {
	prov := &stubSearchProvider{name: "stub-cache-test", delay: 30 * time.Millisecond}
	run := webSearchRunner(prov)
	in := json.RawMessage(`{"query":"ArtexV2 Cache Test","limit":5}`)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := run(context.Background(), in, nil)
			if err != nil || res.IsError {
				t.Errorf("unexpected error: res=%+v err=%v", res, err)
			}
		}()
	}
	wg.Wait()

	// Subsequent call with different casing / whitespace should also hit cache.
	res, err := run(context.Background(), json.RawMessage(`{"query":"  artexv2 cache test  ","limit":5}`), nil)
	if err != nil || res.IsError {
		t.Fatalf("cached call failed: res=%+v err=%v", res, err)
	}
	if got := prov.calls.Load(); got != 1 {
		t.Fatalf("provider Search called %d times, want 1 (singleflight + LRU cache)", got)
	}
}
