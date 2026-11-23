package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

type stubWebFetchTool struct {
	calls atomic.Int32
	fn    func(json.RawMessage) (actool.Result, error)
}

func (s *stubWebFetchTool) Name() string                      { return "WebFetch" }
func (s *stubWebFetchTool) Description() string               { return "stub" }
func (s *stubWebFetchTool) Prompt() string                    { return "" }
func (s *stubWebFetchTool) InputSchema() map[string]any       { return nil }
func (s *stubWebFetchTool) IsReadOnly(json.RawMessage) bool   { return true }
func (s *stubWebFetchTool) IsConcurrencySafe(json.RawMessage) bool {
	return true
}
func (s *stubWebFetchTool) CheckPermissions(context.Context, json.RawMessage, permission.Context) permission.Decision {
	return permission.Decision{Behavior: permission.Allow}
}
func (s *stubWebFetchTool) Call(_ context.Context, input json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
	s.calls.Add(1)
	if s.fn != nil {
		return s.fn(input)
	}
	return actool.Text("ok"), nil
}

func TestTaskProbeCacheHitExpiryEvictionAndErrors(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	cache := newTaskProbeCache(10*time.Second, 2, func() time.Time { return now })

	stub := &stubWebFetchTool{fn: func(in json.RawMessage) (actool.Result, error) {
		var p struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(in, &p)
		if p.URL == "https://target.test/err" {
			return actool.Errorf("boom"), nil
		}
		if p.URL == "https://target.test/fail" {
			return actool.Result{}, errors.New("network")
		}
		return actool.Text("body:" + p.URL), nil
	}}
	wrapped := wrapCachedWebFetch([]actool.CoreTool{stub}, cache)[0]

	call := func(raw string) (actool.Result, error) {
		return wrapped.Call(context.Background(), json.RawMessage(raw), nil)
	}

	// Same URL (normalized host/scheme/fragment) hits cache.
	r1, err := call(`{"url":"HTTPS://TARGET.TEST/a#sec"}`)
	if err != nil || r1.Flatten() != "body:HTTPS://TARGET.TEST/a#sec" {
		t.Fatalf("unexpected r1: %+v err=%v", r1, err)
	}
	r2, err := call(`{"url":"https://target.test/a"}`)
	if err != nil || r2.Flatten() != r1.Flatten() || stub.calls.Load() != 1 {
		t.Fatalf("expected cache hit; calls=%d r2=%q err=%v", stub.calls.Load(), r2.Flatten(), err)
	}

	// Different extract flag must miss.
	if _, err := call(`{"url":"https://target.test/a","extract":true}`); err != nil || stub.calls.Load() != 2 {
		t.Fatalf("expected extract=true miss; calls=%d err=%v", stub.calls.Load(), err)
	}

	// Errors and IsError results are never cached.
	if _, _ = call(`{"url":"https://target.test/err"}`); stub.calls.Load() != 3 {
		t.Fatalf("calls=%d", stub.calls.Load())
	}
	if _, _ = call(`{"url":"https://target.test/err"}`); stub.calls.Load() != 4 {
		t.Fatalf("IsError result should not be cached; calls=%d", stub.calls.Load())
	}
	if _, _ = call(`{"url":"https://target.test/fail"}`); stub.calls.Load() != 5 {
		t.Fatalf("calls=%d", stub.calls.Load())
	}
	if _, _ = call(`{"url":"https://target.test/fail"}`); stub.calls.Load() != 6 {
		t.Fatalf("returned error should not be cached; calls=%d", stub.calls.Load())
	}

	// Non-GET method bypasses cache.
	if _, _ = call(`{"url":"https://target.test/a","method":"POST"}`); stub.calls.Load() != 7 {
		t.Fatalf("non-GET should bypass cache; calls=%d", stub.calls.Load())
	}

	// Eviction cap=2: adding a 3rd key evicts oldest (/a), leaving extract=true and /b.
	if _, _ = call(`{"url":"https://target.test/b"}`); stub.calls.Load() != 8 {
		t.Fatalf("calls=%d", stub.calls.Load())
	}
	if _, _ = call(`{"url":"https://target.test/a"}`); stub.calls.Load() != 9 {
		t.Fatalf("expected evicted /a to refetch; calls=%d", stub.calls.Load())
	}

	// TTL expiration clears stale entry.
	now = now.Add(11 * time.Second)
	if _, _ = call(`{"url":"https://target.test/a"}`); stub.calls.Load() != 10 {
		t.Fatalf("expected expired entry to refetch; calls=%d", stub.calls.Load())
	}
}

func TestTaskProbeCacheConcurrentRace(t *testing.T) {
	cache := NewTaskProbeCache()
	stub := &stubWebFetchTool{}
	wrapped := wrapCachedWebFetch([]actool.CoreTool{stub}, cache)[0]

	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				payload := fmt.Sprintf(`{"url":"https://example.com/item-%d"}`, (id+i)%5)
				if _, err := wrapped.Call(context.Background(), json.RawMessage(payload), nil); err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
		}(g)
	}
	wg.Wait()
}
