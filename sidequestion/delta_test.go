package sidequestion

import (
	"context"
	"iter"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

// Streamed answers deliver only the new chunk per update (Answer.Delta) — the
// accumulated text is materialized once at the end, not per token (O(N²)).
func TestAnswerStreamDelta(t *testing.T) {
	chunks := []string{"Hello", ", ", "world", "!"}
	p := fakeProvider{stream: func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		return func(yield func(llm.StreamEvent, error) bool) {
			for _, c := range chunks {
				if !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: c}, nil) {
					return
				}
			}
			yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
		}
	}}

	var deltas []string
	var texts []string
	out, err := (SideQuestionService{Provider: p}).Answer(t.Context(), fixture(), true, func(a Answer) {
		deltas = append(deltas, a.Delta)
		texts = append(texts, a.Text)
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Text != "Hello, world!" {
		t.Fatalf("final text = %q", out.Text)
	}
	if strings.Join(deltas, "") != "Hello, world!" {
		t.Fatalf("deltas = %q", deltas)
	}
	// Mid-stream updates must NOT materialize the full text (that was the O(N²)).
	for i, txt := range texts {
		if txt != "" {
			t.Fatalf("update %d carried full text %q — should carry Delta only", i, txt)
		}
	}
	if out.Delta != "" {
		t.Fatalf("returned answer leaks Delta %q", out.Delta)
	}
}

// Non-streaming answers carry no Delta and the full Text — unchanged contract.
func TestAnswerNonStreamingNoDelta(t *testing.T) {
	p := fakeProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		return assistant("done"), "end_turn", llm.Usage{}, nil
	}}
	out, err := (SideQuestionService{Provider: p}).Answer(t.Context(), fixture(), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Text != "done" || out.Delta != "" {
		t.Fatalf("answer = %+v", out)
	}
}
