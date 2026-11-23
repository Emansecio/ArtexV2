package compaction

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/norma/llm"
)

func TestSpeculativePreCompactSplicesWithoutBlocking(t *testing.T) {
	var calls atomic.Int32
	var wg sync.WaitGroup
	wg.Add(1)
	sumFn := func(_ context.Context, msgs []llm.Message) (string, error) {
		calls.Add(1)
		defer wg.Done()
		return "SPECULATIVE_SUMMARY", nil
	}

	// ContextWindow=100000, MaxOutputTokens=4000 -> autoThreshold = 83000.
	// 80% threshold = 66400; microThreshold = 57600.
	c := New(Config{
		ContextWindow:         100000,
		MaxOutputTokens:       4000,
		KeepRecent:            2,
		SpeculativePreCompact: true,
	}, sumFn)

	msgs := []llm.Message{
		llm.UserText(strings.Repeat("a", 12000)),
		llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("ack 1")}},
		llm.UserText(strings.Repeat("b", 12000)),
		llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("ack 2")}},
		llm.UserText("recent q"),
		llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("recent a")}},
	}

	// Step 1: pass >= 80% of autoThreshold (67000) -> kicks speculative summary in background.
	out1 := c.Pre(context.Background(), msgs, 67000)
	if len(out1) != len(msgs) {
		t.Fatalf("did not expect boundary yet at 80%%, got len=%d", len(out1))
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("speculative summarizer did not finish")
	}
	if calls.Load() != 1 {
		t.Fatalf("expected 1 speculative call, got %d", calls.Load())
	}

	// Step 2: cross autoThreshold -> should splice ready summary with 0 extra summarizer calls.
	out2 := c.Pre(context.Background(), append(msgs, llm.UserText("new turn"), llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("new ans")}}), 90000)
	if calls.Load() != 1 {
		t.Fatalf("expected speculative summary reuse (total calls=1), got %d", calls.Load())
	}
	bidx := llm.LastBoundaryIndex(out2)
	if bidx < 0 || !strings.Contains(out2[bidx+1].Text(), "SPECULATIVE_SUMMARY") {
		t.Fatalf("expected speculative summary spliced at boundary, out=%+v", out2)
	}
}

func TestSpeculativeCancelAbortsInFlightSummarizer(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	sumFn := func(ctx context.Context, _ []llm.Message) (string, error) {
		close(started)
		<-ctx.Done()
		close(canceled)
		return "", ctx.Err()
	}

	c := New(Config{
		ContextWindow:         100000,
		MaxOutputTokens:       4000,
		KeepRecent:            2,
		SpeculativePreCompact: true,
	}, sumFn)

	msgs := []llm.Message{
		llm.UserText(strings.Repeat("a", 12000)),
		llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("ack 1")}},
		llm.UserText(strings.Repeat("b", 12000)),
		llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("ack 2")}},
		llm.UserText("recent q"),
		llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("recent a")}},
	}

	c.Pre(context.Background(), msgs, 67000)

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("speculative summarizer did not start")
	}

	c.CancelSpeculative()

	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("CancelSpeculative did not cancel in-flight summarizer")
	}
}
