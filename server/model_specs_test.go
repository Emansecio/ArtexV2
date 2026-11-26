package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

func TestLookupKnownModelSpec(t *testing.T) {
	cases := []struct {
		provider string
		model    string
		wantCtx  int
		wantOut  int
		wantFld  string
		wantThk  string
	}{
		{"anthropic", "claude-sonnet-4-6", 200_000, 64_000, "", "enabled"},
		{"anthropic-oauth", "claude-opus-4-6", 200_000, 64_000, "", "enabled"},
		{"openai", "gpt-5.4", 1_000_000, 128_000, llm.MaxTokensFieldCompletion, ""},
		{"openai", "o3", 200_000, 100_000, llm.MaxTokensFieldCompletion, ""},
		{"openai-responses", "o3", 200_000, 100_000, "", ""},
		{"openai", "gpt-4o", 128_000, 16_384, "", ""},
		{"openai", "anthropic/claude-sonnet-4-6:beta", 200_000, 64_000, "", ""},
		{"antigravity-cli", "gemini-4-carbon", 1_000_000, 65_536, "", ""},
		{"opencode-go", "deepseek-v3.2", 128_000, 64_000, "", ""},
	}
	for _, tc := range cases {
		d, ok := lookupKnownModelSpec(tc.provider, tc.model)
		if !ok {
			t.Fatalf("expected known spec for %s/%s", tc.provider, tc.model)
		}
		if d.ContextWindow != tc.wantCtx || d.MaxOutputTokens != tc.wantOut {
			t.Errorf("%s/%s: got ctx=%d out=%d, want ctx=%d out=%d", tc.provider, tc.model, d.ContextWindow, d.MaxOutputTokens, tc.wantCtx, tc.wantOut)
		}
		if d.MaxTokensField != tc.wantFld {
			t.Errorf("%s/%s: got max_tokens_field=%q, want %q", tc.provider, tc.model, d.MaxTokensField, tc.wantFld)
		}
		if d.ThinkingType != tc.wantThk {
			t.Errorf("%s/%s: got thinking_type=%q, want %q", tc.provider, tc.model, d.ThinkingType, tc.wantThk)
		}
	}
}

func TestBuildModelDetailFromAPIPrefersLiveMetadata(t *testing.T) {
	raw := rawModelEntry{
		ID:            "custom/model-x",
		Name:          "Custom Model X",
		ContextLength: 262144,
		TopProvider: &struct {
			ContextLength       int `json:"context_length"`
			MaxCompletionTokens int `json:"max_completion_tokens"`
		}{
			ContextLength:       262144,
			MaxCompletionTokens: 32768,
		},
	}
	d := buildModelDetailFromAPI("openai", raw, nil)
	if d.ContextWindow != 262144 || d.ContextWindowK != 262 || d.MaxOutputTokens != 32768 || d.Source != "api" || d.Label != "Custom Model X" {
		t.Fatalf("unexpected API detail: %+v", d)
	}
}

func TestProbeOllamaModelContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/show" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model_info":{"qwen2.context_length":131072},"parameters":"temperature 0.7\n"}`))
	}))
	defer srv.Close()

	got, ok := probeOllamaModelContext(context.Background(), srv.Client(), srv.URL, "qwen2.5:14b", http.Header{})
	if !ok || got != 131072 {
		t.Fatalf("expected 131072 from /api/show, got %d (ok=%v)", got, ok)
	}

	if n := parseOllamaNumCtx("temperature 0.2\nnum_ctx 65536\n"); n != 65536 {
		t.Fatalf("expected num_ctx=65536, got %d", n)
	}
}

func TestMergeModelDetailFallbackValidatesDefaultReasoningEffort(t *testing.T) {
	// Live discovery only supports ["low", "medium"]; fallback has default "high".
	// Must NOT assign unsupported "high" to DefaultReasoningEffort.
	dst := modelDetail{
		ID:              "demo-model",
		ReasoningLevels: []string{"low", "medium"},
	}
	src := modelDetail{
		ID:                     "demo-model",
		ReasoningLevels:        []string{"low", "medium", "high"},
		DefaultReasoningEffort: "high",
	}
	mergeModelDetailFallback(&dst, src)
	if dst.DefaultReasoningEffort != "" {
		t.Fatalf("expected unsupported default reasoning effort to be ignored, got %q", dst.DefaultReasoningEffort)
	}

	// When fallback default IS in live ReasoningLevels, it should be copied.
	srcSupported := modelDetail{
		ID:                     "demo-model",
		DefaultReasoningEffort: "medium",
	}
	mergeModelDetailFallback(&dst, srcSupported)
	if dst.DefaultReasoningEffort != "medium" {
		t.Fatalf("expected supported default reasoning effort 'medium', got %q", dst.DefaultReasoningEffort)
	}

	// When both dst and src have empty ReasoningLevels, DefaultReasoningEffort must stay empty.
	emptyDst := modelDetail{ID: "non-reasoning"}
	mergeModelDetailFallback(&emptyDst, modelDetail{DefaultReasoningEffort: "high"})
	if emptyDst.DefaultReasoningEffort != "" {
		t.Fatalf("expected empty DefaultReasoningEffort when ReasoningLevels is empty, got %q", emptyDst.DefaultReasoningEffort)
	}

	// When dst has empty ReasoningLevels and src supplies both levels and a valid default, both are copied.
	inheritDst := modelDetail{ID: "inherit-model"}
	mergeModelDetailFallback(&inheritDst, src)
	if len(inheritDst.ReasoningLevels) != 3 || inheritDst.DefaultReasoningEffort != "high" {
		t.Fatalf("expected inherited levels and valid default 'high', got %+v", inheritDst)
	}
}
