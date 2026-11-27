package llmrec

import (
	"bytes"
	"context"
	"database/sql"
	"iter"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

type delayedStubProvider struct {
	delay time.Duration
	usage llm.Usage
}

func (p delayedStubProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		if p.delay > 0 {
			time.Sleep(p.delay)
		}
		if !yield(llm.StreamEvent{Type: llm.SEMessageStart, Usage: p.usage}, nil) {
			return
		}
		if !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: "ok"}, nil) {
			return
		}
		if !yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn", Usage: p.usage}, nil) {
			return
		}
		yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
	}
}

func (p delayedStubProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("ok")}}, "end_turn", p.usage, nil
}

func TestRecorderPersistsTTFTMsAndLogsCacheDrop(t *testing.T) {
	dsn := os.Getenv("ARTEX_PG_DSN")
	if dsn == "" {
		t.Skip("ARTEX_PG_DSN not set")
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = pg.Close() })

	taskID := "ttft-test-task"
	t.Cleanup(func() {
		_, _ = pg.Exec(`DELETE FROM llm_usage WHERE task_id=$1`, taskID)
	})

	rec := Wrap(delayedStubProvider{
		delay: 15 * time.Millisecond,
		usage: llm.Usage{InputTokens: 6000, OutputTokens: 20, CacheReadTokens: 5800},
	}, pg, "claude-haiku-4-5", "test-prof", "", "", func() bool { return false })

	ctx := WithTaskID(transcript.WithSessionID(context.Background(), "exp99-worker-i1"), taskID)
	for _, err := range rec.Stream(ctx, llm.CompletionRequest{Messages: []llm.Message{llm.UserText("turn 1")}}) {
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
	}
	if _, _, _, err := rec.Complete(ctx, llm.CompletionRequest{Messages: []llm.Message{llm.UserText("complete")}}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	FlushUsage()

	rows, err := pg.Query(`SELECT ttft_ms FROM llm_usage WHERE task_id=$1 ORDER BY id`, taskID)
	if err != nil {
		t.Fatalf("query llm_usage: %v", err)
	}
	defer rows.Close()
	var vals []sql.NullInt64
	for rows.Next() {
		var v sql.NullInt64
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		vals = append(vals, v)
	}
	if len(vals) != 2 {
		t.Fatalf("expected 2 usage rows, got %d", len(vals))
	}
	if !vals[0].Valid || vals[0].Int64 < 10 {
		t.Errorf("stream ttft_ms=%+v, want valid >= 10ms", vals[0])
	}
	if vals[1].Valid {
		t.Errorf("non-stream Complete should store NULL ttft_ms, got %+v", vals[1])
	}

	// Verify cache-hit drop warning triggers on a subsequent turn with low hit ratio.
	var buf bytes.Buffer
	prevOut := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prevOut)

	rec.checkCacheDrop("exp99-worker-i1", "worker", llm.Usage{InputTokens: 6000, CacheReadTokens: 100, CacheWriteTokens: 5900})
	if !strings.Contains(buf.String(), "prompt cache hit drop") {
		t.Errorf("expected cache drop warning log, got %q", buf.String())
	}
}

func TestCheckCacheDropWarmThresholdAndSingleFire(t *testing.T) {
	var buf bytes.Buffer
	prevOut := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prevOut)

	rec := &Recorder{model: "claude-opus-4-6"}
	sess := "sess-cache-drop"

	// 1. Cold session with small cache hit (10% < 25%) must NOT arm warm state or warn on next cold turn.
	rec.checkCacheDrop(sess, "worker", llm.Usage{InputTokens: 10000, CacheReadTokens: 1000})
	rec.checkCacheDrop(sess, "worker", llm.Usage{InputTokens: 10000, CacheReadTokens: 1000})
	if buf.Len() != 0 {
		t.Fatalf("unexpected cache drop warning on never-warm session: %q", buf.String())
	}

	// 2. Session warms up (>= 25%), then drops (< 25%) -> logs once.
	rec.checkCacheDrop(sess, "worker", llm.Usage{InputTokens: 10000, CacheReadTokens: 5000})
	if buf.Len() != 0 {
		t.Fatalf("unexpected warning on warm hit: %q", buf.String())
	}
	rec.checkCacheDrop(sess, "worker", llm.Usage{InputTokens: 10000, CacheReadTokens: 1000})
	if count := strings.Count(buf.String(), "prompt cache hit drop"); count != 1 {
		t.Fatalf("expected 1 cache drop warning on warm->cold transition, got %d (%q)", count, buf.String())
	}

	// 3. Consecutive cold turn must NOT spam another warning.
	rec.checkCacheDrop(sess, "worker", llm.Usage{InputTokens: 10000, CacheReadTokens: 500})
	if count := strings.Count(buf.String(), "prompt cache hit drop"); count != 1 {
		t.Fatalf("expected still 1 cache drop warning on consecutive cold turn, got %d (%q)", count, buf.String())
	}

	// 4. Re-warming and dropping again should fire a second warning.
	rec.checkCacheDrop(sess, "worker", llm.Usage{InputTokens: 10000, CacheReadTokens: 4000})
	rec.checkCacheDrop(sess, "worker", llm.Usage{InputTokens: 10000, CacheReadTokens: 200})
	if count := strings.Count(buf.String(), "prompt cache hit drop"); count != 2 {
		t.Fatalf("expected 2 cache drop warnings after re-warm and drop, got %d (%q)", count, buf.String())
	}
}
