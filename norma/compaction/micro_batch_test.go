package compaction

import (
	"context"
	"iter"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

func TestMicroCompactExtraToolsBatchAndSpillPreservation(t *testing.T) {
	cfg := Config{
		ContextWindow:         100000,
		MaxOutputTokens:       4096,
		MicroThresholdRatio:   0.35,
		MicroKeepRecent:       2,
		MicroBatchInterval:    3,
		ExtraCompactableTools: []string{"traffic_query", "list_assets"},
		SummaryMaxTokens:      4096,
	}
	c := New(cfg, nil)

	// Verify SummaryMaxTokens overrides MaxOutputTokens reservation and custom MicroThresholdRatio applies.
	if got, want := c.reservedForSummary(), 4096; got != want {
		t.Fatalf("reservedForSummary=%d, want %d", got, want)
	}
	eff := float64(100000 - 4096)
	wantMicro := int(eff * 0.35)
	if got := c.microThreshold(); got != wantMicro {
		t.Fatalf("microThreshold=%d, want %d", got, wantMicro)
	}

	spillMsg := "head output\n\n... <persisted-output>[Output too large: full 50000 bytes / 400 lines.Full output saved to  /data/spill/output-1-1.txt </persisted-output>"
	var msgs []llm.Message
	msgs = append(msgs, toolPair("traffic_query", "tq0", spillMsg)...)
	msgs = append(msgs, toolPair("list_assets", "la1", strings.Repeat("a", 500))...)
	msgs = append(msgs, toolPair("Bash", "b2", strings.Repeat("b", 500))...)
	msgs = append(msgs, toolPair("Bash", "b3", strings.Repeat("c", 500))...) // 4 total: 2 clearable, 2 kept

	// Below batch interval (only 2 clearable < 3): must NOT mutate (preserves cache).
	out := c.Pre(context.Background(), msgs, wantMicro+100)
	if countCleared(out) != 0 {
		t.Fatalf("expected 0 cleared while batch < 3, got %d", countCleared(out))
	}

	// Add 1 more result (5 total -> 3 clearable == MicroBatchInterval): batch fires!
	msgs = append(msgs, toolPair("traffic_query", "tq4", strings.Repeat("d", 500))...)
	out = c.Pre(context.Background(), msgs, wantMicro+100)
	if countCleared(out) != 3 {
		t.Fatalf("expected 3 cleared on batch trigger, got %d", countCleared(out))
	}

	// Spill path must survive clearing on tq0.
	firstCleared := resultText(out[1].Content[0])
	if !strings.Contains(firstCleared, "/data/spill/output-1-1.txt") {
		t.Fatalf("spill path lost on microCompact: %q", firstCleared)
	}
	// Running Pre again immediately must be a no-op (idempotent even with spill prefix).
	out2 := c.Pre(context.Background(), out, wantMicro+100)
	if countCleared(out2) != 3 {
		t.Fatalf("idempotence failed: %d", countCleared(out2))
	}
}

type captureSummaryProvider struct {
	lastReq llm.CompletionRequest
}

func (p *captureSummaryProvider) Stream(_ context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return nil
}

func (p *captureSummaryProvider) Complete(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	p.lastReq = req
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("<summary>pentest summary</summary>")}}, "end_turn", llm.Usage{}, nil
}

func TestProviderSummarizerWithCustomPromptAndCap(t *testing.T) {
	prov := &captureSummaryProvider{}
	sum := ProviderSummarizerWithOptions(prov, SummaryOptions{
		MaxOutputTokens: 4096,
		NonStreaming:    true,
		Prompt:          "CUSTOM PENTEST SUMMARY PROMPT",
	})
	got, err := sum(context.Background(), []llm.Message{llm.UserText("scan target")})
	if err != nil {
		t.Fatalf("sum err: %v", err)
	}
	if got != "Summary:\npentest summary" {
		t.Fatalf("summary=%q", got)
	}
	if prov.lastReq.MaxTokens != 4096 {
		t.Fatalf("MaxTokens=%d, want 4096", prov.lastReq.MaxTokens)
	}
	lastMsg := prov.lastReq.Messages[len(prov.lastReq.Messages)-1]
	if lastMsg.Text() != "CUSTOM PENTEST SUMMARY PROMPT" {
		t.Fatalf("prompt=%q", lastMsg.Text())
	}
}
