package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAnthropicThinkingBudgetAdaptiveAndDisplay(t *testing.T) {
	cases := []struct {
		name        string
		cfg         Config
		maxTokens   int
		wantType    string
		wantBudget  int
		wantDisplay string
		wantMaxTok  int
	}{
		{
			name:        "enabled with explicit budget",
			cfg:         Config{Model: "claude-haiku-4-5", ThinkingType: "enabled", ThinkingBudget: 4096},
			maxTokens:   16384,
			wantType:    "enabled",
			wantBudget:  4096,
			wantDisplay: "summarized",
			wantMaxTok:  16384,
		},
		{
			name:        "enabled with zero budget clamps below max_tokens",
			cfg:         Config{Model: "claude-haiku-4-5", ThinkingType: "enabled"},
			maxTokens:   4096,
			wantType:    "enabled",
			wantBudget:  3072,
			wantDisplay: "summarized",
			wantMaxTok:  4096,
		},
		{
			name:        "enabled with positive sub-minimum budget clamps up to 1024 floor",
			cfg:         Config{Model: "claude-haiku-4-5", ThinkingType: "enabled", ThinkingBudget: 512},
			maxTokens:   16384,
			wantType:    "enabled",
			wantBudget:  1024,
			wantDisplay: "summarized",
			wantMaxTok:  16384,
		},
		{
			name:        "enabled with tiny max_tokens raises floor safely",
			cfg:         Config{Model: "claude-haiku-4-5", ThinkingType: "enabled"},
			maxTokens:   128,
			wantType:    "enabled",
			wantBudget:  1024,
			wantDisplay: "summarized",
			wantMaxTok:  2048,
		},
		{
			name:        "adaptive omits budget and defaults display to summarized",
			cfg:         Config{Model: "claude-opus-4-6", ThinkingType: "adaptive", ThinkingBudget: 8192},
			maxTokens:   16384,
			wantType:    "adaptive",
			wantBudget:  0,
			wantDisplay: "summarized",
			wantMaxTok:  16384,
		},
		{
			name:        "adaptive respects explicit omitted display",
			cfg:         Config{Model: "claude-opus-4-6", ThinkingType: "adaptive", ThinkingDisplay: "omitted"},
			maxTokens:   16384,
			wantType:    "adaptive",
			wantBudget:  0,
			wantDisplay: "omitted",
			wantMaxTok:  16384,
		},
		{
			name:        "disabled sends only type",
			cfg:         Config{Model: "claude-opus-4-6", ThinkingType: "disabled", ThinkingBudget: 4096, ThinkingDisplay: "summarized"},
			maxTokens:   4096,
			wantType:    "disabled",
			wantBudget:  0,
			wantDisplay: "",
			wantMaxTok:  4096,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &anthropicProvider{cfg: tc.cfg}
			raw, err := p.buildBody(CompletionRequest{
				Messages:  []Message{UserText("hello")},
				MaxTokens: tc.maxTokens,
			}, true)
			if err != nil {
				t.Fatalf("buildBody: %v", err)
			}
			var got struct {
				MaxTokens int                `json:"max_tokens"`
				Thinking  *anthropicThinking `json:"thinking"`
			}
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got.MaxTokens != tc.wantMaxTok {
				t.Errorf("max_tokens=%d, want %d", got.MaxTokens, tc.wantMaxTok)
			}
			if got.Thinking == nil {
				t.Fatalf("thinking is nil, want type=%q", tc.wantType)
			}
			if got.Thinking.Type != tc.wantType || got.Thinking.BudgetTokens != tc.wantBudget || got.Thinking.Display != tc.wantDisplay {
				t.Errorf("thinking=%+v, want {Type:%q BudgetTokens:%d Display:%q}",
					*got.Thinking, tc.wantType, tc.wantBudget, tc.wantDisplay)
			}
		})
	}
}

func TestAnthropicPerRoleCacheTTL(t *testing.T) {
	req := CompletionRequest{
		System:             []string{"static sys"},
		Tools:              []ToolSchema{{Name: "t1", Description: "d", InputSchema: map[string]any{"type": "object"}}},
		Messages:           []Message{UserText("hi")},
		PromptCacheTTL:     "1h",
		PromptCacheTailTTL: "",
	}
	p := &anthropicProvider{cfg: Config{Model: "claude-opus-4-6"}}
	raw, err := p.buildBody(req, true)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	var body struct {
		Tools    []struct{ CacheControl map[string]any `json:"cache_control"` } `json:"tools"`
		System   []struct{ CacheControl map[string]any `json:"cache_control"` } `json:"system"`
		Messages []struct {
			Content []struct{ CacheControl map[string]any `json:"cache_control"` } `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Tools[0].CacheControl["ttl"] != "1h" {
		t.Errorf("tools ttl=%v, want 1h", body.Tools[0].CacheControl["ttl"])
	}
	if body.System[0].CacheControl["ttl"] != "1h" {
		t.Errorf("system ttl=%v, want 1h", body.System[0].CacheControl["ttl"])
	}
	if _, hasTTL := body.Messages[0].Content[0].CacheControl["ttl"]; hasTTL {
		t.Errorf("worker tail should use default 5m (no ttl key), got %v", body.Messages[0].Content[0].CacheControl)
	}

	// Tail 1h without stable 1h must promote stable to 1h (Anthropic ordering rule).
	req.PromptCacheTTL = ""
	req.PromptCacheTailTTL = "1h"
	raw2, err := p.buildBody(req, true)
	if err != nil {
		t.Fatalf("buildBody promote: %v", err)
	}
	if err := json.Unmarshal(raw2, &body); err != nil {
		t.Fatalf("unmarshal promote: %v", err)
	}
	if body.Tools[0].CacheControl["ttl"] != "1h" || body.System[0].CacheControl["ttl"] != "1h" || body.Messages[0].Content[0].CacheControl["ttl"] != "1h" {
		t.Errorf("expected promoted 1h across all breakpoints, got tools=%v sys=%v msg=%v",
			body.Tools[0].CacheControl, body.System[0].CacheControl, body.Messages[0].Content[0].CacheControl)
	}

	// Config.PromptCacheTTL="5m" must override per-request 1h defaults and omit the ttl key.
	p5m := &anthropicProvider{cfg: Config{Model: "claude-opus-4-6", PromptCacheTTL: "5m"}}
	req.PromptCacheTTL = "1h"
	req.PromptCacheTailTTL = "1h"
	raw5m, err := p5m.buildBody(req, true)
	if err != nil {
		t.Fatalf("buildBody 5m override: %v", err)
	}
	body = struct {
		Tools    []struct{ CacheControl map[string]any `json:"cache_control"` } `json:"tools"`
		System   []struct{ CacheControl map[string]any `json:"cache_control"` } `json:"system"`
		Messages []struct {
			Content []struct{ CacheControl map[string]any `json:"cache_control"` } `json:"content"`
		} `json:"messages"`
	}{}
	if err := json.Unmarshal(raw5m, &body); err != nil {
		t.Fatalf("unmarshal 5m override: %v", err)
	}
	for name, cc := range map[string]map[string]any{
		"tools":   body.Tools[0].CacheControl,
		"system":  body.System[0].CacheControl,
		"message": body.Messages[0].Content[0].CacheControl,
	} {
		if cc["type"] != "ephemeral" {
			t.Errorf("%s cache_control=%v, want type=ephemeral", name, cc)
		}
		if _, hasTTL := cc["ttl"]; hasTTL {
			t.Errorf("%s cache_control=%v, want ttl omitted for 5m override", name, cc)
		}
	}

	// OpenAI cache_control path with PromptCacheTTL="5m" must also omit ttl across tools, system, and tail.
	pOA5m := &openaiProvider{cfg: Config{Model: "anthropic/claude-opus-4-6", OpenAICacheControl: true, PromptCacheTTL: "5m"}}
	rawOA5m, err := pOA5m.buildBody(req, true)
	if err != nil {
		t.Fatalf("openai buildBody 5m: %v", err)
	}
	var oaBody struct {
		Tools []struct {
			CacheControl map[string]any `json:"cache_control"`
		} `json:"tools"`
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Text         string         `json:"text"`
				CacheControl map[string]any `json:"cache_control"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(rawOA5m, &oaBody); err != nil {
		t.Fatalf("unmarshal openai 5m: %v", err)
	}
	if len(oaBody.Tools) == 0 || oaBody.Tools[0].CacheControl["type"] != "ephemeral" {
		t.Fatalf("openai tools cache_control=%+v, want type=ephemeral", oaBody.Tools)
	}
	if _, hasTTL := oaBody.Tools[0].CacheControl["ttl"]; hasTTL {
		t.Errorf("openai tools cache_control=%v, want ttl omitted for 5m", oaBody.Tools[0].CacheControl)
	}
	if len(oaBody.Messages) < 2 || len(oaBody.Messages[0].Content) == 0 || len(oaBody.Messages[1].Content) == 0 {
		t.Fatalf("unexpected openai messages: %+v", oaBody.Messages)
	}
	for name, cc := range map[string]map[string]any{
		"system": oaBody.Messages[0].Content[0].CacheControl,
		"tail":   oaBody.Messages[1].Content[0].CacheControl,
	} {
		if cc["type"] != "ephemeral" {
			t.Errorf("openai %s cache_control=%v, want type=ephemeral", name, cc)
		}
		if _, hasTTL := cc["ttl"]; hasTTL {
			t.Errorf("openai %s cache_control=%v, want ttl omitted for 5m", name, cc)
		}
	}
}

func TestOpenAICacheControlPerRoleTTL(t *testing.T) {
	p := &openaiProvider{cfg: Config{Model: "anthropic/claude-sonnet-4-6", OpenAICacheControl: true}}
	type oaWire struct {
		Tools []struct {
			CacheControl map[string]any `json:"cache_control"`
		} `json:"tools"`
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Text         string         `json:"text"`
				CacheControl map[string]any `json:"cache_control"`
			} `json:"content"`
		} `json:"messages"`
	}
	decode := func(t *testing.T, req CompletionRequest) oaWire {
		t.Helper()
		raw, err := p.buildBody(req, true)
		if err != nil {
			t.Fatalf("buildBody: %v", err)
		}
		var out oaWire
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(out.Tools) == 0 || len(out.Messages) < 2 || len(out.Messages[0].Content) == 0 || len(out.Messages[1].Content) == 0 {
			t.Fatalf("unexpected wire payload: %+v", out)
		}
		return out
	}

	// Worker split (1h stable prefix, 5m rolling tail) with DynamicBoundary split.
	workerReq := CompletionRequest{
		System:          []string{"static prompt", "dynamic tail"},
		DynamicBoundary: 1,
		Tools:           []ToolSchema{{Name: "bash", InputSchema: map[string]any{"type": "object"}}},
		Messages:        []Message{UserText("hello")},
		PromptCacheTTL:  "1h",
	}
	wBody := decode(t, workerReq)
	if wBody.Tools[0].CacheControl["ttl"] != "1h" {
		t.Errorf("worker tools ttl=%v, want 1h", wBody.Tools[0].CacheControl)
	}
	if wBody.Messages[0].Content[0].CacheControl["ttl"] != "1h" {
		t.Errorf("worker static system ttl=%v, want 1h", wBody.Messages[0].Content[0].CacheControl)
	}
	if len(wBody.Messages[0].Content) < 2 || wBody.Messages[0].Content[1].CacheControl != nil {
		t.Errorf("worker dynamic system part should have no cache_control, got %+v", wBody.Messages[0].Content)
	}
	if wBody.Messages[1].Content[0].CacheControl["type"] != "ephemeral" {
		t.Errorf("worker tail cache_control=%v, want ephemeral", wBody.Messages[1].Content[0].CacheControl)
	}
	if _, hasTTL := wBody.Messages[1].Content[0].CacheControl["ttl"]; hasTTL {
		t.Errorf("worker tail should use 5m (no ttl key), got %v", wBody.Messages[1].Content[0].CacheControl)
	}

	// Worker split without DynamicBoundary (single unsplit system message still gets ccStable).
	workerUnsplit := workerReq
	workerUnsplit.System = []string{"single static prompt"}
	workerUnsplit.DynamicBoundary = 0
	wuBody := decode(t, workerUnsplit)
	if wuBody.Messages[0].Content[0].CacheControl["ttl"] != "1h" {
		t.Errorf("unsplit system ttl=%v, want 1h", wuBody.Messages[0].Content[0].CacheControl)
	}
	if _, hasTTL := wuBody.Messages[1].Content[0].CacheControl["ttl"]; hasTTL {
		t.Errorf("unsplit worker tail should use 5m (no ttl key), got %v", wuBody.Messages[1].Content[0].CacheControl)
	}

	// Chat / Planner (1h stable + 1h tail).
	chatReq := workerReq
	chatReq.PromptCacheTTL = "1h"
	chatReq.PromptCacheTailTTL = "1h"
	cBody := decode(t, chatReq)
	if cBody.Tools[0].CacheControl["ttl"] != "1h" ||
		cBody.Messages[0].Content[0].CacheControl["ttl"] != "1h" ||
		cBody.Messages[1].Content[0].CacheControl["ttl"] != "1h" {
		t.Errorf("chat/planner expected 1h on tools/system/tail, got tools=%v sys=%v tail=%v",
			cBody.Tools[0].CacheControl, cBody.Messages[0].Content[0].CacheControl, cBody.Messages[1].Content[0].CacheControl)
	}

	// Tail 1h without stable 1h must promote stable breakpoints to 1h (Anthropic ordering rule).
	promoteReq := workerReq
	promoteReq.PromptCacheTTL = ""
	promoteReq.PromptCacheTailTTL = "1h"
	pBody := decode(t, promoteReq)
	if pBody.Tools[0].CacheControl["ttl"] != "1h" ||
		pBody.Messages[0].Content[0].CacheControl["ttl"] != "1h" ||
		pBody.Messages[1].Content[0].CacheControl["ttl"] != "1h" {
		t.Errorf("tail-1h promotion expected 1h across all breakpoints, got tools=%v sys=%v tail=%v",
			pBody.Tools[0].CacheControl, pBody.Messages[0].Content[0].CacheControl, pBody.Messages[1].Content[0].CacheControl)
	}
}

func TestOpenAIResponsesSummaryAndCacheWriteUsage(t *testing.T) {
	p := &openaiResponsesProvider{cfg: Config{Model: "o3", ReasoningEffort: "high"}}
	raw, err := p.buildBody(CompletionRequest{Messages: []Message{UserText("hi")}}, true)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	var got struct {
		Reasoning *orReasoning `json:"reasoning"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Reasoning == nil || got.Reasoning.Summary != "auto" {
		t.Fatalf("expected reasoning.summary=auto by default, got %+v", got.Reasoning)
	}

	pOmit := &openaiResponsesProvider{cfg: Config{Model: "o3", ReasoningEffort: "high", ThinkingDisplay: "omitted"}}
	rawOmit, err := pOmit.buildBody(CompletionRequest{Messages: []Message{UserText("hi")}}, true)
	if err != nil {
		t.Fatalf("buildBody omitted: %v", err)
	}
	got.Reasoning = nil
	if err := json.Unmarshal(rawOmit, &got); err != nil {
		t.Fatalf("unmarshal omitted: %v", err)
	}
	if got.Reasoning == nil || got.Reasoning.Summary != "" {
		t.Fatalf("expected empty reasoning.summary when omitted, got %+v", got.Reasoning)
	}

	ev, ok, err := parseResponsesFrame("response.completed", `{"response":{"status":"completed","output":[],"usage":{"input_tokens":1000,"output_tokens":50,"input_tokens_details":{"cached_tokens":600,"cache_write_tokens":200}}}}`)
	if err != nil || !ok {
		t.Fatalf("parseResponsesFrame: ok=%v err=%v", ok, err)
	}
	if ev.Usage.CacheReadTokens != 600 || ev.Usage.CacheWriteTokens != 200 {
		t.Errorf("usage=%+v, want read=600 write=200", ev.Usage)
	}
}

func TestOpenAIStreamEmitsToolUseStopAndCacheCounters(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		frames := []string{
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_0","type":"function","function":{"name":"read","arguments":"{\"p\":"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_1","type":"function","function":{"name":"grep","arguments":"{\"q\":\"a\"}"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"f\"}"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":500,"completion_tokens":40,"prompt_tokens_details":{"cached_tokens":0,"cache_write_tokens":120},"prompt_cache_hit_tokens":350}}`,
		}
		for _, f := range frames {
			fmt.Fprintf(w, "data: %s\n\n", f)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	prov, err := NewProvider(Config{Format: FormatOpenAI, BaseURL: srv.URL, APIKey: "k", Model: "deepseek-chat", MaxRetries: -1})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	var stops []int
	var deltaIdx int
	var gotUsage Usage
	pos := 0
	stopPositions := []int{}
	for ev, serr := range prov.Stream(context.Background(), CompletionRequest{Messages: []Message{UserText("hi")}}) {
		if serr != nil {
			t.Fatalf("stream err: %v", serr)
		}
		switch ev.Type {
		case SEToolUseStop:
			stops = append(stops, ev.ToolIndex)
			stopPositions = append(stopPositions, pos)
		case SEMessageDelta:
			deltaIdx = pos
			gotUsage = ev.Usage
		}
		pos++
	}
	if len(stops) != 2 || stops[0] != 0 || stops[1] != 1 {
		t.Fatalf("expected SEToolUseStop for [0, 1], got %v", stops)
	}
	for _, sp := range stopPositions {
		if sp >= deltaIdx {
			t.Fatalf("SEToolUseStop at %d must arrive before SEMessageDelta at %d", sp, deltaIdx)
		}
	}
	if gotUsage.CacheReadTokens != 350 || gotUsage.CacheWriteTokens != 120 {
		t.Errorf("usage=%+v, want cache_read=350 (DeepSeek fallback) cache_write=120", gotUsage)
	}
}

func TestDefaultHTTPClientHTTP2Config(t *testing.T) {
	c, err := defaultHTTPClient("")
	if err != nil {
		t.Fatalf("defaultHTTPClient: %v", err)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", c.Transport)
	}
	if tr.IdleConnTimeout != 5*time.Minute {
		t.Errorf("IdleConnTimeout=%v, want 5m", tr.IdleConnTimeout)
	}
	if tr.HTTP2 == nil || tr.HTTP2.SendPingTimeout != 15*time.Second || tr.HTTP2.PingTimeout != 5*time.Second {
		t.Errorf("HTTP2=%+v, want SendPingTimeout=15s PingTimeout=5s", tr.HTTP2)
	}
}
