package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

func TestModelRegistryRefreshDiskCacheAndLookup(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"anthropic": {
				"models": {
					"claude-opus-4-8": {
						"id": "claude-opus-4-8",
						"name": "Claude Opus 4.8",
						"attachment": true,
						"reasoning": true,
						"reasoning_options": [
							{"type": "toggle"},
							{"type": "effort", "values": ["low", "medium", "high", "xhigh", "max"]}
						],
						"tool_call": true,
						"release_date": "2026-05-28",
						"limit": {"context": 1000000, "output": 128000},
						"cost": {"input": 5, "output": 25, "cache_read": 0.5, "cache_write": 6.25}
					}
				}
			},
			"openai": {
				"models": {
					"gpt-6.2-astra": {
						"id": "gpt-6.2-astra",
						"name": "GPT-6.2 Astra",
						"reasoning": true,
						"reasoning_options": [{"type": "effort", "values": ["low", "medium", "high", "xhigh"]}],
						"release_date": "2026-09-01",
						"limit": {"context": 1000000, "output": 128000},
						"cost": {"input": 4, "output": 16}
					}
				}
			}
		}`))
	}))
	defer srv.Close()

	cacheDir := t.TempDir()
	reg := newModelRegistry(cacheDir, srv.URL)
	if !reg.Refresh(context.Background(), false) {
		t.Fatalf("expected initial refresh to succeed")
	}
	if hits.Load() != 1 {
		t.Fatalf("expected 1 fetch, got %d", hits.Load())
	}
	// Second non-forced refresh must reuse fresh RAM cache.
	if !reg.Refresh(context.Background(), false) || hits.Load() != 1 {
		t.Fatalf("expected cached refresh without network hit, hits=%d", hits.Load())
	}

	d, ok := reg.Lookup("anthropic", "claude-opus-4-8")
	if !ok {
		t.Fatalf("expected claude-opus-4-8 in registry")
	}
	if d.ContextWindow != 1_000_000 || d.MaxOutputTokens != 128_000 || d.ThinkingType != "enabled" ||
		d.PriceInputPerM != 5 || d.PriceOutputPerM != 25 || d.PriceCacheReadPerM != 0.5 || d.PriceCacheWritePerM != 6.25 ||
		d.Source != "models.dev" || len(d.ReasoningLevels) != 5 {
		t.Fatalf("unexpected claude-opus-4-8 detail: %+v", d)
	}

	gpt, ok := reg.Lookup("openai", "gpt-6.2-astra")
	if !ok || gpt.MaxTokensField != llm.MaxTokensFieldCompletion || gpt.ContextWindow != 1_000_000 {
		t.Fatalf("unexpected gpt-6.2-astra detail: %+v (ok=%v)", gpt, ok)
	}

	// Brand-new registry pointed at an unreachable URL must load cleanly from disk cache!
	reg2 := newModelRegistry(cacheDir, "http://127.0.0.1:1/unreachable")
	d2, ok := reg2.Lookup("anthropic", "claude-opus-4-8")
	if !ok || d2.ContextWindow != 1_000_000 || d2.PriceInputPerM != 5 {
		t.Fatalf("expected disk cache hit in new registry instance, got ok=%v detail=%+v", ok, d2)
	}
}

func TestProviderDiskCatalogRoundTrip(t *testing.T) {
	s := &Server{dataDir: t.TempDir()}
	wantModels := []string{"gemini-4-carbon", "gemini-4-neon"}
	wantDetails := []modelDetail{
		{ID: "gemini-4-carbon", Label: "Gemini 4 Carbon", ContextWindow: 1_000_000, ContextWindowK: 1000, MaxOutputTokens: 65536},
		{ID: "gemini-4-neon", Label: "Gemini 4 Neon", ContextWindow: 1_000_000, ContextWindowK: 1000, MaxOutputTokens: 65536},
	}
	s.writeDiskProviderCatalog("antigravity-cli", wantModels, wantDetails)

	models, details, updatedAt, ok := s.readDiskProviderCatalog("antigravity-cli")
	if !ok || len(models) != 2 || len(details) != 2 || updatedAt.IsZero() {
		t.Fatalf("unexpected disk catalog read: ok=%v models=%v details=%v updated=%v", ok, models, details, updatedAt)
	}
	if models[0] != "gemini-4-carbon" || details[1].MaxOutputTokens != 65536 {
		t.Fatalf("disk catalog data mismatch: %+v", details)
	}
}

func TestNonChatFilteringAndFlagshipSorting(t *testing.T) {
	for _, nonChat := range []string{"whisper-1", "tts-1-hd", "dall-e-3", "text-embedding-3-large", "omni-moderation-latest", "gpt-4o-realtime-preview", "babbage-002", "sora-2"} {
		if !isNonChatModel(nonChat) {
			t.Errorf("expected %q to be flagged non-chat", nonChat)
		}
	}
	for _, chat := range []string{"gpt-5.4", "o3", "claude-opus-4-8", "claude-sonnet-4-6", "gemini-4-carbon", "deepseek-v3.2"} {
		if isNonChatModel(chat) {
			t.Errorf("expected %q to be kept as chat model", chat)
		}
	}

	items := []modelDetail{
		{ID: "gpt-3.5-turbo"},
		{ID: "gpt-4o-2024-08-06"},
		{ID: "gpt-4o"},
		{ID: "o3"},
		{ID: "gpt-5.4"},
		{ID: "whisper-1", NonChat: true},
	}
	sortModelDetails(items)
	if items[0].ID != "gpt-5.4" || items[1].ID != "o3" || items[2].ID != "gpt-4o" || items[3].ID != "gpt-4o-2024-08-06" || items[len(items)-1].ID != "whisper-1" {
		t.Fatalf("unexpected sort order: %+v", items)
	}
}

func TestParseOpenCodeRichConsoleCatalog(t *testing.T) {
	body := []byte(`{
		"opencode-go": {
			"models": {
				"deepseek-v4.1-flash": {
					"id": "deepseek-v4.1-flash",
					"name": "DeepSeek V4.1 Flash",
					"limit": {"context": 256000, "output": 65536},
					"cost": {"input": 0.25, "output": 1.0}
				}
			}
		}
	}`)
	details, ok := parseOpenCodeCatalogBody("opencode-go", body, nil)
	if !ok || len(details) != 1 {
		t.Fatalf("expected 1 opencode-go model, got ok=%v details=%+v", ok, details)
	}
	if details[0].ID != "deepseek-v4.1-flash" || details[0].ContextWindow != 256000 || details[0].MaxOutputTokens != 65536 || details[0].PriceInputPerM != 0.25 {
		t.Fatalf("unexpected opencode-go detail: %+v", details[0])
	}
}

func TestAnthropicPaginationAndOpenAIFilteringInPGListModels(t *testing.T) {
	// 1. Anthropic multi-page cursor pagination (?limit=100 -> ?after_id=m2)
	var pages atomic.Int32
	anthSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("after_id") == "" {
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-3-5-sonnet-20241022"}],"has_more":true,"last_id":"claude-3-5-sonnet-20241022"}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-opus-4-8","display_name":"Claude Opus 4.8"}],"has_more":false,"last_id":"claude-opus-4-8"}`))
	}))
	defer anthSrv.Close()

	s := &Server{allowLoopbackModels: true}
	resp := postJSON(t, s.pgListModels, "/api/llm/models", map[string]any{
		"provider": "anthropic",
		"base_url": anthSrv.URL,
		"api_key":  "sk-ant-test",
	})
	if resp["ok"] != true || pages.Load() != 2 {
		t.Fatalf("expected 2 paginated anthropic calls, pages=%d resp=%v", pages.Load(), resp)
	}
	models, _ := resp["models"].([]any)
	if len(models) != 2 || models[0] != "claude-opus-4-8" {
		t.Fatalf("expected claude-opus-4-8 ranked first across 2 pages, got %v", models)
	}

	// 2. OpenAI non-chat filtering + include_all toggle
	oaiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"whisper-1"},{"id":"tts-1"},{"id":"gpt-4o"},{"id":"gpt-5.4"}]}`))
	}))
	defer oaiSrv.Close()

	filtered := postJSON(t, s.pgListModels, "/api/llm/models", map[string]any{
		"provider": "openai",
		"base_url": oaiSrv.URL,
		"api_key":  "sk-test",
	})
	fModels, _ := filtered["models"].([]any)
	if len(fModels) != 2 || fModels[0] != "gpt-5.4" || filtered["filtered_count"] != float64(2) {
		t.Fatalf("expected 2 chat models (gpt-5.4 first) and filtered_count=2, got %v", filtered)
	}

	all := postJSON(t, s.pgListModels, "/api/llm/models", map[string]any{
		"provider":    "openai",
		"base_url":    oaiSrv.URL,
		"api_key":     "sk-test",
		"include_all": true,
	})
	allModels, _ := all["models"].([]any)
	if len(allModels) != 4 {
		t.Fatalf("expected 4 models with include_all=true, got %v", allModels)
	}
}

func TestDetectTestLLMSuggestedFix(t *testing.T) {
	fix := detectTestLLMSuggestedFix("openai", "", `openai: status 400: {"error":{"code":"unsupported_parameter","message":"Unsupported parameter: 'max_tokens' is not supported with this model. Use 'max_completion_tokens' instead."}}`)
	if fix == nil || fix["max_tokens_field"] != llm.MaxTokensFieldCompletion {
		t.Fatalf("expected max_completion_tokens suggestion, got %v", fix)
	}
	if nilFix := detectTestLLMSuggestedFix("anthropic", "", "unsupported_parameter max_completion_tokens"); nilFix != nil {
		t.Fatalf("expected no suggestion for non-openai provider, got %v", nilFix)
	}
}
