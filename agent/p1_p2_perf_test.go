package agent

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	actool "github.com/Autumn-27/norma/tool"
)

type slowFetchTool struct {
	actool.CoreTool
	calls atomic.Int32
}

func (s *slowFetchTool) Name() string { return "WebFetch" }
func (s *slowFetchTool) Call(_ context.Context, _ json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
	s.calls.Add(1)
	time.Sleep(25 * time.Millisecond)
	return actool.Text("ok"), nil
}

func TestTaskProbeCacheSingleflightAndLRU(t *testing.T) {
	cache := newTaskProbeCache(time.Minute, 2, time.Now)
	stub := &slowFetchTool{}
	wrapped := wrapCachedWebFetch([]actool.CoreTool{stub}, cache)[0]

	// 4 concurrent identical GETs coalesce into 1 upstream call.
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = wrapped.Call(context.Background(), json.RawMessage(`{"url":"https://example.com/a"}`), nil)
		}()
	}
	wg.Wait()
	if got := stub.calls.Load(); got != 1 {
		t.Fatalf("singleflight calls = %d, want 1", got)
	}

	// Fill capacity (2), touch /a so /b is LRU, then insert /c -> /b should be evicted.
	_, _ = wrapped.Call(context.Background(), json.RawMessage(`{"url":"https://example.com/b"}`), nil)
	_, _ = wrapped.Call(context.Background(), json.RawMessage(`{"url":"https://example.com/a"}`), nil) // refresh /a
	_, _ = wrapped.Call(context.Background(), json.RawMessage(`{"url":"https://example.com/c"}`), nil) // evict /b
	if _, ok := cache.get("https://example.com/a|extract=false|"); !ok {
		t.Fatal("expected /a to remain in LRU cache")
	}
	if _, ok := cache.get("https://example.com/b|extract=false|"); ok {
		t.Fatal("expected /b to be evicted as LRU")
	}
}

func TestPlannerPruneHookBlocksProbingTools(t *testing.T) {
	h := &plannerPruneHook{}
	if block, _, _ := h.PreToolUse(context.Background(), "Bash", nil); !block {
		t.Fatal("expected Bash to be blocked by plannerPruneHook")
	}
	if block, _, _ := h.PreToolUse(context.Background(), "WebFetch", nil); !block {
		t.Fatal("expected WebFetch to be blocked by plannerPruneHook")
	}
	if block, _, _ := h.PreToolUse(context.Background(), "add_intent", nil); block {
		t.Fatal("expected add_intent to be allowed by plannerPruneHook")
	}
}
