package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAnthropicPromptCacheBreakpoints(t *testing.T) {
	tools := []ToolSchema{
		{Name: "Read", Description: "read file", InputSchema: map[string]any{"type": "object"}},
		{Name: "Bash", Description: "run cmd", InputSchema: map[string]any{"type": "object"}},
	}
	msgs := []Message{
		UserText("initial task"),
		{Role: RoleAssistant, Content: []ContentBlock{
			{Type: BlockThinking, Thinking: "plan", Signature: "sig1"},
			{Type: BlockToolUse, ID: "t1", Name: "Bash", Input: []byte(`{"command":"ls"}`)},
		}},
		{Role: RoleUser, Content: []ContentBlock{ToolResultText("t1", "file.txt", false)}},
	}

	t.Run("default_enabled_boundary_zero", func(t *testing.T) {
		p := &anthropicProvider{cfg: Config{Format: FormatAnthropic, Model: "claude-sonnet-4", ThinkingType: "enabled"}}
		raw, err := p.buildBody(CompletionRequest{
			System:          []string{"static system"},
			DynamicBoundary: 0,
			Tools:           tools,
			Messages:        msgs,
		}, true)
		if err != nil {
			t.Fatalf("buildBody: %v", err)
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		// 1. Last tool has cache_control, first tool does not.
		gotTools := doc["tools"].([]any)
		if _, ok := gotTools[0].(map[string]any)["cache_control"]; ok {
			t.Fatal("first tool should not have cache_control")
		}
		if cc, ok := gotTools[1].(map[string]any)["cache_control"].(map[string]any); !ok || cc["type"] != "ephemeral" {
			t.Fatalf("last tool cache_control=%v", gotTools[1])
		}
		// 2. System cached even with DynamicBoundary == 0.
		gotSys := doc["system"].([]any)
		if cc, ok := gotSys[0].(map[string]any)["cache_control"].(map[string]any); !ok || cc["type"] != "ephemeral" {
			t.Fatalf("system[0] cache_control=%v", gotSys[0])
		}
		// 3. Last cacheable message block has cache_control.
		gotMsgs := doc["messages"].([]any)
		lastMsg := gotMsgs[len(gotMsgs)-1].(map[string]any)
		lastBlocks := lastMsg["content"].([]any)
		if cc, ok := lastBlocks[0].(map[string]any)["cache_control"].(map[string]any); !ok || cc["type"] != "ephemeral" {
			t.Fatalf("last message block cache_control=%v", lastBlocks[0])
		}
		// Verify original msgs slice was not mutated.
		rawOrig, _ := json.Marshal(msgs)
		if strings.Contains(string(rawOrig), "cache_control") {
			t.Fatal("original req.Messages mutated with cache_control")
		}
	})

	t.Run("split_boundary_and_1h_ttl", func(t *testing.T) {
		p := &anthropicProvider{cfg: Config{Format: FormatAnthropic, Model: "claude-sonnet-4", PromptCacheTTL: "1h"}}
		raw, err := p.buildBody(CompletionRequest{
			System:          []string{"static", "dynamic"},
			DynamicBoundary: 1,
			Tools:           tools,
			Messages:        []Message{UserText("hi")},
		}, false)
		if err != nil {
			t.Fatalf("buildBody: %v", err)
		}
		var doc map[string]any
		_ = json.Unmarshal(raw, &doc)
		gotSys := doc["system"].([]any)
		if len(gotSys) != 2 {
			t.Fatalf("want 2 system blocks, got %d", len(gotSys))
		}
		cc0, _ := gotSys[0].(map[string]any)["cache_control"].(map[string]any)
		if cc0["type"] != "ephemeral" || cc0["ttl"] != "1h" {
			t.Fatalf("system[0] cache_control=%v, want 1h ttl", cc0)
		}
		if _, ok := gotSys[1].(map[string]any)["cache_control"]; ok {
			t.Fatal("dynamic system[1] should not have cache_control")
		}
	})

	t.Run("skips_thinking_blocks_for_message_breakpoint", func(t *testing.T) {
		p := &anthropicProvider{cfg: Config{Format: FormatAnthropic, Model: "claude-sonnet-4", ThinkingType: "enabled"}}
		raw, err := p.buildBody(CompletionRequest{
			System: []string{"sys"},
			Messages: []Message{
				UserText("go"),
				{Role: RoleAssistant, Content: []ContentBlock{
					{Type: BlockThinking, Thinking: "thought", Signature: "sig"},
				}},
			},
		}, false)
		if err != nil {
			t.Fatalf("buildBody: %v", err)
		}
		var doc map[string]any
		_ = json.Unmarshal(raw, &doc)
		gotMsgs := doc["messages"].([]any)
		// Thinking block on assistant must not receive cache_control; user message before it gets it.
		asstBlocks := gotMsgs[1].(map[string]any)["content"].([]any)
		if _, ok := asstBlocks[0].(map[string]any)["cache_control"]; ok {
			t.Fatal("thinking block must never receive cache_control")
		}
		userBlocks := gotMsgs[0].(map[string]any)["content"].([]any)
		if _, ok := userBlocks[0].(map[string]any)["cache_control"]; !ok {
			t.Fatal("preceding cacheable user text block should receive cache_control")
		}
	})

	t.Run("explicit_disable_removes_all_cache_control", func(t *testing.T) {
		off := false
		p := &anthropicProvider{cfg: Config{Format: FormatAnthropic, Model: "claude-sonnet-4", PromptCacheEnabled: &off}}
		raw, err := p.buildBody(CompletionRequest{
			System:          []string{"static", "dynamic"},
			DynamicBoundary: 1,
			Tools:           tools,
			Messages:        msgs,
		}, false)
		if err != nil {
			t.Fatalf("buildBody: %v", err)
		}
		if strings.Contains(string(raw), "cache_control") {
			t.Fatalf("expected zero cache_control when disabled, got: %s", raw)
		}
	})
}
