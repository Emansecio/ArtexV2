package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOpenAIChatCacheKeyRetentionAndModes(t *testing.T) {
	msgs := []Message{
		UserText("turn 1"),
		{Role: RoleAssistant, Content: []ContentBlock{
			{Type: BlockThinking, Thinking: "old thinking"},
			{Type: BlockToolUse, ID: "c1", Name: "Bash", Input: []byte(`{"command":"id"}`)},
		}},
		{Role: RoleUser, Content: []ContentBlock{ToolResultText("c1", "uid=0", false)}},
		{Role: RoleAssistant, Content: []ContentBlock{TextBlock("done 1")}},
		UserText("turn 2"),
		{Role: RoleAssistant, Content: []ContentBlock{
			{Type: BlockThinking, Thinking: "active thinking"},
			{Type: BlockToolUse, ID: "c2", Name: "Bash", Input: []byte(`{"command":"ls"}`)},
		}},
		{Role: RoleUser, Content: []ContentBlock{ToolResultText("c2", "ok", false)}},
	}

	t.Run("prompt_cache_key_and_retention_and_strip_old_reasoning", func(t *testing.T) {
		p := &openaiProvider{cfg: Config{
			Format:               FormatOpenAI,
			Model:                "gpt-5",
			ThinkingType:         "enabled",
			PromptCacheRetention: "24h",
			StripOldReasoning:    true,
		}}
		raw, err := p.buildBody(CompletionRequest{
			System:         []string{"sys prompt"},
			Messages:       msgs,
			PromptCacheKey: "artex-task-42-worker",
		}, false)
		if err != nil {
			t.Fatalf("buildBody: %v", err)
		}
		var doc map[string]any
		_ = json.Unmarshal(raw, &doc)
		if doc["prompt_cache_key"] != "artex-task-42-worker" {
			t.Fatalf("prompt_cache_key=%v", doc["prompt_cache_key"])
		}
		if doc["prompt_cache_retention"] != "24h" {
			t.Fatalf("prompt_cache_retention=%v", doc["prompt_cache_retention"])
		}
		rawStr := string(raw)
		if strings.Contains(rawStr, "old thinking") {
			t.Fatal("old thinking before latest user turn should be stripped")
		}
		if !strings.Contains(rawStr, "active thinking") {
			t.Fatal("active turn thinking after latest user turn should be preserved")
		}
	})

	t.Run("openai_cache_control_multipart_opt_in", func(t *testing.T) {
		p := &openaiProvider{cfg: Config{
			Format:             FormatOpenAI,
			Model:              "anthropic/claude-sonnet-4",
			OpenAICacheControl: true,
			PromptCacheTTL:     "1h",
		}}
		raw, err := p.buildBody(CompletionRequest{
			System:   []string{"sys"},
			Messages: []Message{UserText("first"), UserText("last")},
		}, false)
		if err != nil {
			t.Fatalf("buildBody: %v", err)
		}
		var doc struct {
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(raw, &doc)
		// system and last user are multipart arrays with cache_control; middle user stays string.
		sysParts, ok := doc.Messages[0].Content.([]any)
		if !ok || len(sysParts) != 1 {
			t.Fatalf("system content=%T %v", doc.Messages[0].Content, doc.Messages[0].Content)
		}
		if cc, _ := sysParts[0].(map[string]any)["cache_control"].(map[string]any); cc["ttl"] != "1h" {
			t.Fatalf("system cache_control=%v", cc)
		}
		if _, ok := doc.Messages[1].Content.(string); !ok {
			t.Fatalf("middle user content should stay string, got %T", doc.Messages[1].Content)
		}
		lastParts, ok := doc.Messages[2].Content.([]any)
		if !ok || len(lastParts) != 1 {
			t.Fatalf("last user content=%T %v", doc.Messages[2].Content, doc.Messages[2].Content)
		}
		if cc, _ := lastParts[0].(map[string]any)["cache_control"].(map[string]any); cc["type"] != "ephemeral" {
			t.Fatalf("last user cache_control=%v", cc)
		}
	})

	t.Run("cache_disabled_omits_keys", func(t *testing.T) {
		off := false
		p := &openaiProvider{cfg: Config{
			Format:               FormatOpenAI,
			Model:                "llama3.2",
			PromptCacheEnabled:   &off,
			PromptCacheRetention: "24h",
			OpenAICacheControl:   true,
		}}
		raw, err := p.buildBody(CompletionRequest{
			System:         []string{"sys"},
			Messages:       []Message{UserText("hi")},
			PromptCacheKey: "ignored",
		}, false)
		if err != nil {
			t.Fatalf("buildBody: %v", err)
		}
		s := string(raw)
		for _, bad := range []string{"prompt_cache_key", "prompt_cache_retention", "cache_control"} {
			if strings.Contains(s, bad) {
				t.Fatalf("unexpected %s when cache disabled: %s", bad, s)
			}
		}
	})
}

func TestOpenAIResponsesCacheKeyAndRetention(t *testing.T) {
	p := &openaiResponsesProvider{cfg: Config{
		Format:               FormatOpenAIResponses,
		Model:                "gpt-5",
		PromptCacheRetention: "24h",
	}}
	raw, err := p.buildBody(CompletionRequest{
		System:         []string{"sys"},
		Messages:       []Message{UserText("hi")},
		PromptCacheKey: "artex-task-7-planner",
	}, false)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	if doc["prompt_cache_key"] != "artex-task-7-planner" {
		t.Fatalf("prompt_cache_key=%v", doc["prompt_cache_key"])
	}
	if doc["prompt_cache_retention"] != "24h" {
		t.Fatalf("prompt_cache_retention=%v", doc["prompt_cache_retention"])
	}

	off := false
	pOff := &openaiResponsesProvider{cfg: Config{Format: FormatOpenAIResponses, Model: "gpt-5", PromptCacheEnabled: &off, PromptCacheRetention: "24h"}}
	rawOff, _ := pOff.buildBody(CompletionRequest{Messages: []Message{UserText("hi")}, PromptCacheKey: "k"}, false)
	if strings.Contains(string(rawOff), "prompt_cache_") {
		t.Fatalf("cache fields should be omitted when disabled: %s", rawOff)
	}
}
