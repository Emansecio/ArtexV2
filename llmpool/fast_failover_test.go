package llmpool

import (
	"context"
	"fmt"
	"iter"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/norma/llm"
)

func TestHalfOpenSingleFlightProbe(t *testing.T) {
	reg := NewRegistry(nil, nil)
	reg.Restore(1, State{Trips: 1, OpenUntil: time.Now().Add(-time.Second)})

	usable1, probe1 := reg.AcquireUsable(1)
	if !usable1 || !probe1 {
		t.Fatalf("first expired caller should acquire probe: usable=%v probe=%v", usable1, probe1)
	}
	usable2, probe2 := reg.AcquireUsable(1)
	if usable2 || probe2 {
		t.Fatalf("concurrent second caller should wait while probe active: usable=%v probe=%v", usable2, probe2)
	}
	reg.ReleaseProbe(1)
	usable3, probe3 := reg.AcquireUsable(1)
	if !usable3 || !probe3 {
		t.Fatalf("after ReleaseProbe should allow new probe: usable=%v probe=%v", usable3, probe3)
	}
}

func TestTripWithRetryAfterCooldown(t *testing.T) {
	reg := NewRegistry(nil, nil)
	httpErr := &llm.HTTPStatusError{
		Prefix:     "anthropic",
		StatusCode: 429,
		Body:       "rate limit",
		RetryAfter: 5 * time.Second,
	}
	tripped, _ := reg.RecordCall(context.Background(), 1, true, false, httpErr)
	if !tripped || !reg.IsOpen(1) {
		t.Fatal("429 with RetryAfter should open breaker immediately for cooldown window")
	}
	d := time.Until(reg.Get(1).OpenUntil)
	if d < 3*time.Second || d > 10*time.Second {
		t.Fatalf("cooldown=%v, want ~5s", d)
	}
}

func TestEqualRankPrefersClearlyFasterTTFT(t *testing.T) {
	reg := NewRegistry(nil, nil)
	reg.RecordTTFT(2, 2*time.Second)
	reg.RecordTTFT(3, 100*time.Millisecond)

	p := New([]*Member{
		member(2, "slow", 5, okProv("slow", "S")),
		member(3, "fast", 5, okProv("fast", "F")),
	}, reg)

	for range 4 {
		got, err := drain(p.Stream(context.Background(), llm.CompletionRequest{}))
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		if got != "F" {
			t.Fatalf("expected faster TTFT member F, got %q", got)
		}
	}
}

type slowProv struct {
	text      string
	delay     time.Duration
	calls     atomic.Int32
	cancelled atomic.Int32
}

func (s *slowProv) Stream(ctx context.Context, _ llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	s.calls.Add(1)
	return func(yield func(llm.StreamEvent, error) bool) {
		if s.delay > 0 {
			timer := time.NewTimer(s.delay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				s.cancelled.Add(1)
				yield(llm.StreamEvent{}, ctx.Err())
				return
			case <-timer.C:
			}
		}
		if !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: s.text}, nil) {
			return
		}
		yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
	}
}

func (s *slowProv) Complete(ctx context.Context, _ llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	return llm.UserText(s.text), "end_turn", llm.Usage{}, nil
}

func TestSpeculativeHedgingPicksFasterBackupAndCancelsSlowPrimary(t *testing.T) {
	slowPrimary := &slowProv{text: "PRIMARY", delay: 500 * time.Millisecond}
	fastBackup := &slowProv{text: "BACKUP", delay: 5 * time.Millisecond}

	p := New([]*Member{
		member(1, "primary", 10, slowPrimary),
		member(2, "backup", 5, fastBackup),
	}, NewRegistry(nil, nil))
	p.SetHedgeDelay(25 * time.Millisecond)

	var sb strings.Builder
	for ev, err := range p.Stream(context.Background(), llm.CompletionRequest{}) {
		if err != nil {
			t.Fatalf("hedged stream failed: %v", err)
		}
		if ev.Type == llm.SETextDelta {
			sb.WriteString(ev.Text)
		}
	}
	if sb.String() != "BACKUP" {
		t.Fatalf("got %q, want BACKUP", sb.String())
	}
	if fastBackup.calls.Load() != 1 || slowPrimary.calls.Load() != 1 {
		t.Fatalf("calls primary=%d backup=%d, want 1/1", slowPrimary.calls.Load(), fastBackup.calls.Load())
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for slowPrimary.cancelled.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if slowPrimary.cancelled.Load() == 0 {
		t.Fatal(fmt.Sprintf("expected slow primary context to be cancelled, count=%d", slowPrimary.cancelled.Load()))
	}
}
