package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/norma/llm"
)

// modelDetail carries rich auto-detected metadata for a model so the UI can
// populate context window, output cap, reasoning levels, thinking switch,
// max_tokens_field, and per-1M pricing without risky manual guesswork.
type modelDetail struct {
	ID                     string   `json:"id"`
	Label                  string   `json:"label,omitempty"`
	ContextWindow          int      `json:"context_window,omitempty"`
	ContextWindowK         int      `json:"context_window_k,omitempty"`
	MaxOutputTokens        int      `json:"max_output_tokens,omitempty"`
	ReasoningLevels        []string `json:"reasoning_levels,omitempty"`
	DefaultReasoningEffort string   `json:"default_reasoning_effort,omitempty"`
	ThinkingType           string   `json:"thinking_type,omitempty"`
	MaxTokensField         string   `json:"max_tokens_field,omitempty"`
	PriceInputPerM         float64  `json:"price_input_per_m,omitempty"`
	PriceOutputPerM        float64  `json:"price_output_per_m,omitempty"`
	PriceCacheReadPerM     float64  `json:"price_cache_read_per_m,omitempty"`
	PriceCacheWritePerM    float64  `json:"price_cache_write_per_m,omitempty"`
	ReleaseDate            string   `json:"release_date,omitempty"`
	SupportsTools          *bool    `json:"supports_tools,omitempty"`
	SupportsVision         *bool    `json:"supports_vision,omitempty"`
	NonChat                bool     `json:"non_chat,omitempty"`
	Source                 string   `json:"source,omitempty"` // "api" | "ollama" | "models.dev" | "known"
}

type knownModelRule struct {
	match                  string // lowercase substring/prefix matched against normalized model id
	prefix                 bool   // true = HasPrefix, false = Contains
	contextWindow          int
	maxOutputTokens        int
	reasoningLevels        []string
	defaultReasoningEffort string
	supportsThinkingSwitch bool
	usesMaxCompletion      bool // OpenAI Chat Completions requires max_completion_tokens
}

// knownModelRules are ordered from most specific to most general.
var knownModelRules = []knownModelRule{
	// --- Anthropic Claude ---
	{match: "claude-opus-5", prefix: true, contextWindow: 1_000_000, maxOutputTokens: 128_000, reasoningLevels: []string{"low", "medium", "high", "xhigh", "max"}, defaultReasoningEffort: "high", supportsThinkingSwitch: true},
	{match: "claude-sonnet-5", prefix: true, contextWindow: 1_000_000, maxOutputTokens: 64_000, reasoningLevels: []string{"low", "medium", "high"}, defaultReasoningEffort: "high", supportsThinkingSwitch: true},
	{match: "claude-opus-4-8", prefix: true, contextWindow: 1_000_000, maxOutputTokens: 128_000, reasoningLevels: []string{"low", "medium", "high", "xhigh", "max"}, defaultReasoningEffort: "high", supportsThinkingSwitch: true},
	{match: "claude-opus-4.8", prefix: true, contextWindow: 1_000_000, maxOutputTokens: 128_000, reasoningLevels: []string{"low", "medium", "high", "xhigh", "max"}, defaultReasoningEffort: "high", supportsThinkingSwitch: true},
	{match: "claude-opus-4-6", prefix: true, contextWindow: 200_000, maxOutputTokens: 64_000, reasoningLevels: []string{"low", "medium", "high", "max"}, defaultReasoningEffort: "high", supportsThinkingSwitch: true},
	{match: "claude-opus-4.6", prefix: true, contextWindow: 200_000, maxOutputTokens: 64_000, reasoningLevels: []string{"low", "medium", "high", "max"}, defaultReasoningEffort: "high", supportsThinkingSwitch: true},
	{match: "claude-opus-4-5", prefix: true, contextWindow: 200_000, maxOutputTokens: 64_000, reasoningLevels: []string{"low", "medium", "high"}, defaultReasoningEffort: "high", supportsThinkingSwitch: true},
	{match: "claude-opus-4.5", prefix: true, contextWindow: 200_000, maxOutputTokens: 64_000, reasoningLevels: []string{"low", "medium", "high"}, defaultReasoningEffort: "high", supportsThinkingSwitch: true},
	{match: "claude-opus-4", prefix: true, contextWindow: 200_000, maxOutputTokens: 32_000, reasoningLevels: []string{"low", "medium", "high"}, defaultReasoningEffort: "high", supportsThinkingSwitch: true},
	{match: "claude-sonnet-4-6", prefix: true, contextWindow: 200_000, maxOutputTokens: 64_000, reasoningLevels: []string{"low", "medium", "high"}, defaultReasoningEffort: "high", supportsThinkingSwitch: true},
	{match: "claude-sonnet-4.6", prefix: true, contextWindow: 200_000, maxOutputTokens: 64_000, reasoningLevels: []string{"low", "medium", "high"}, defaultReasoningEffort: "high", supportsThinkingSwitch: true},
	{match: "claude-sonnet-4-5", prefix: true, contextWindow: 200_000, maxOutputTokens: 64_000, reasoningLevels: []string{"low", "medium", "high"}, defaultReasoningEffort: "high", supportsThinkingSwitch: true},
	{match: "claude-sonnet-4.5", prefix: true, contextWindow: 200_000, maxOutputTokens: 64_000, reasoningLevels: []string{"low", "medium", "high"}, defaultReasoningEffort: "high", supportsThinkingSwitch: true},
	{match: "claude-sonnet-4", prefix: true, contextWindow: 200_000, maxOutputTokens: 64_000, reasoningLevels: []string{"low", "medium", "high"}, defaultReasoningEffort: "high", supportsThinkingSwitch: true},
	{match: "claude-haiku-4", prefix: true, contextWindow: 200_000, maxOutputTokens: 64_000, reasoningLevels: []string{"low", "medium", "high"}, supportsThinkingSwitch: true},
	{match: "claude-3-7-sonnet", prefix: true, contextWindow: 200_000, maxOutputTokens: 64_000, reasoningLevels: []string{"low", "medium", "high"}, supportsThinkingSwitch: true},
	{match: "claude-3.7-sonnet", prefix: true, contextWindow: 200_000, maxOutputTokens: 64_000, reasoningLevels: []string{"low", "medium", "high"}, supportsThinkingSwitch: true},
	{match: "claude-3-5-", prefix: true, contextWindow: 200_000, maxOutputTokens: 8_192},
	{match: "claude-3.5-", prefix: true, contextWindow: 200_000, maxOutputTokens: 8_192},
	{match: "claude-3-", prefix: true, contextWindow: 200_000, maxOutputTokens: 4_096},

	// --- OpenAI GPT-6 / GPT-5 / Codex / o-series / GPT-4 ---
	{match: "gpt-6", prefix: true, contextWindow: 1_000_000, maxOutputTokens: 128_000, reasoningLevels: []string{"low", "medium", "high", "xhigh"}, defaultReasoningEffort: "high", usesMaxCompletion: true},
	{match: "gpt-5.4", prefix: true, contextWindow: 1_000_000, maxOutputTokens: 128_000, reasoningLevels: []string{"low", "medium", "high", "xhigh"}, defaultReasoningEffort: "high", usesMaxCompletion: true},
	{match: "gpt-5.3", prefix: true, contextWindow: 400_000, maxOutputTokens: 128_000, reasoningLevels: []string{"low", "medium", "high", "xhigh"}, defaultReasoningEffort: "high", usesMaxCompletion: true},
	{match: "gpt-5.2", prefix: true, contextWindow: 400_000, maxOutputTokens: 128_000, reasoningLevels: []string{"low", "medium", "high", "xhigh"}, defaultReasoningEffort: "high", usesMaxCompletion: true},
	{match: "gpt-5.1", prefix: true, contextWindow: 400_000, maxOutputTokens: 128_000, reasoningLevels: []string{"low", "medium", "high"}, defaultReasoningEffort: "high", usesMaxCompletion: true},
	{match: "gpt-5", prefix: true, contextWindow: 400_000, maxOutputTokens: 128_000, reasoningLevels: []string{"low", "medium", "high"}, defaultReasoningEffort: "high", usesMaxCompletion: true},
	{match: "codex-mini", prefix: true, contextWindow: 200_000, maxOutputTokens: 100_000, reasoningLevels: []string{"low", "medium", "high"}, usesMaxCompletion: true},
	{match: "o4-mini", prefix: true, contextWindow: 200_000, maxOutputTokens: 100_000, reasoningLevels: []string{"low", "medium", "high"}, defaultReasoningEffort: "medium", usesMaxCompletion: true},
	{match: "o3-pro", prefix: true, contextWindow: 200_000, maxOutputTokens: 100_000, reasoningLevels: []string{"low", "medium", "high"}, defaultReasoningEffort: "high", usesMaxCompletion: true},
	{match: "o3-mini", prefix: true, contextWindow: 200_000, maxOutputTokens: 100_000, reasoningLevels: []string{"low", "medium", "high"}, defaultReasoningEffort: "medium", usesMaxCompletion: true},
	{match: "o3", prefix: true, contextWindow: 200_000, maxOutputTokens: 100_000, reasoningLevels: []string{"low", "medium", "high"}, defaultReasoningEffort: "high", usesMaxCompletion: true},
	{match: "o1-mini", prefix: true, contextWindow: 128_000, maxOutputTokens: 65_536, usesMaxCompletion: true},
	{match: "o1", prefix: true, contextWindow: 200_000, maxOutputTokens: 100_000, reasoningLevels: []string{"low", "medium", "high"}, usesMaxCompletion: true},
	{match: "gpt-4.1", prefix: true, contextWindow: 1_000_000, maxOutputTokens: 32_768},
	{match: "gpt-4o", prefix: true, contextWindow: 128_000, maxOutputTokens: 16_384},
	{match: "chatgpt-4o", prefix: true, contextWindow: 128_000, maxOutputTokens: 16_384},
	{match: "gpt-4-turbo", prefix: true, contextWindow: 128_000, maxOutputTokens: 4_096},
	{match: "gpt-4", prefix: true, contextWindow: 8_192, maxOutputTokens: 4_096},
	{match: "gpt-3.5", prefix: true, contextWindow: 16_384, maxOutputTokens: 4_096},
	{match: "gpt-oss-", prefix: true, contextWindow: 128_000, maxOutputTokens: 32_768, reasoningLevels: []string{"low", "medium", "high"}},

	// --- Google Gemini ---
	{match: "gemini-4-carbon", prefix: true, contextWindow: 1_000_000, maxOutputTokens: 65_536, reasoningLevels: []string{"low", "medium", "high", "xhigh"}, defaultReasoningEffort: "high"},
	{match: "gemini-4-neon", prefix: true, contextWindow: 1_000_000, maxOutputTokens: 65_536, reasoningLevels: []string{"low", "medium", "high", "xhigh"}, defaultReasoningEffort: "high"},
	{match: "gemini-4-argon", prefix: true, contextWindow: 200_000, maxOutputTokens: 64_000, reasoningLevels: []string{"low", "medium", "high"}, defaultReasoningEffort: "medium"},
	{match: "gemini-4-hydrogen", prefix: true, contextWindow: 200_000, maxOutputTokens: 64_000, reasoningLevels: []string{"low", "medium", "high"}, defaultReasoningEffort: "medium"},
	{match: "gemini-4", prefix: true, contextWindow: 1_000_000, maxOutputTokens: 65_536, reasoningLevels: []string{"low", "medium", "high", "xhigh"}, defaultReasoningEffort: "high"},
	{match: "gemini-3.8", prefix: true, contextWindow: 1_000_000, maxOutputTokens: 65_536, reasoningLevels: []string{"low", "medium", "high"}, defaultReasoningEffort: "medium"},
	{match: "gemini-3", prefix: true, contextWindow: 1_000_000, maxOutputTokens: 65_536, reasoningLevels: []string{"low", "medium", "high"}, defaultReasoningEffort: "high"},
	{match: "gemini-2.5", prefix: true, contextWindow: 1_000_000, maxOutputTokens: 65_536, reasoningLevels: []string{"low", "medium", "high"}, defaultReasoningEffort: "medium"},
	{match: "gemini-2.0", prefix: true, contextWindow: 1_000_000, maxOutputTokens: 8_192},
	{match: "gemini-1.5-pro", prefix: true, contextWindow: 2_000_000, maxOutputTokens: 8_192},
	{match: "gemini-1.5", prefix: true, contextWindow: 1_000_000, maxOutputTokens: 8_192},

	// --- DeepSeek ---
	{match: "deepseek-v4", prefix: true, contextWindow: 256_000, maxOutputTokens: 64_000, reasoningLevels: []string{"low", "medium", "high"}},
	{match: "deepseek-r1", prefix: true, contextWindow: 128_000, maxOutputTokens: 64_000, reasoningLevels: []string{"medium", "high"}},
	{match: "deepseek-reasoner", prefix: true, contextWindow: 128_000, maxOutputTokens: 64_000, reasoningLevels: []string{"medium", "high"}},
	{match: "deepseek-v3.2", prefix: true, contextWindow: 128_000, maxOutputTokens: 64_000, reasoningLevels: []string{"low", "medium", "high"}},
	{match: "deepseek-v3", prefix: true, contextWindow: 128_000, maxOutputTokens: 8_192},
	{match: "deepseek-chat", prefix: true, contextWindow: 128_000, maxOutputTokens: 8_192},

	// --- xAI Grok ---
	{match: "grok-4-fast", prefix: true, contextWindow: 2_000_000, maxOutputTokens: 64_000, reasoningLevels: []string{"low", "medium", "high"}},
	{match: "grok-4", prefix: true, contextWindow: 256_000, maxOutputTokens: 64_000, reasoningLevels: []string{"low", "medium", "high"}},
	{match: "grok-3", prefix: true, contextWindow: 131_000, maxOutputTokens: 16_384, reasoningLevels: []string{"low", "high"}},

	// --- Qwen ---
	{match: "qwen3-coder", prefix: true, contextWindow: 262_000, maxOutputTokens: 65_536},
	{match: "qwen3", prefix: true, contextWindow: 131_000, maxOutputTokens: 32_768, reasoningLevels: []string{"low", "medium", "high"}},
	{match: "qwq", prefix: true, contextWindow: 131_000, maxOutputTokens: 32_768},
	{match: "qwen2.5", prefix: true, contextWindow: 128_000, maxOutputTokens: 8_192},

	// --- Kimi / GLM / MiniMax / Devin SWE ---
	{match: "kimi-k2", prefix: true, contextWindow: 256_000, maxOutputTokens: 32_768, reasoningLevels: []string{"low", "medium", "high"}},
	{match: "glm-5", prefix: true, contextWindow: 200_000, maxOutputTokens: 32_768, reasoningLevels: []string{"low", "medium", "high"}},
	{match: "glm-4.7", prefix: true, contextWindow: 200_000, maxOutputTokens: 32_768, reasoningLevels: []string{"low", "medium", "high"}},
	{match: "glm-4.6", prefix: true, contextWindow: 200_000, maxOutputTokens: 32_768, reasoningLevels: []string{"low", "medium", "high"}},
	{match: "glm-4.5", prefix: true, contextWindow: 128_000, maxOutputTokens: 16_384},
	{match: "minimax-m2", prefix: true, contextWindow: 200_000, maxOutputTokens: 32_768},
	{match: "minimax-m1", prefix: true, contextWindow: 1_000_000, maxOutputTokens: 32_768},
	{match: "swe-1", prefix: true, contextWindow: 200_000, maxOutputTokens: 64_000},

	// --- Meta Llama / Mistral / Gemma ---
	{match: "llama-4", prefix: true, contextWindow: 1_000_000, maxOutputTokens: 16_384},
	{match: "llama4", prefix: true, contextWindow: 1_000_000, maxOutputTokens: 16_384},
	{match: "llama-3.3", prefix: true, contextWindow: 128_000, maxOutputTokens: 8_192},
	{match: "llama3.3", prefix: true, contextWindow: 128_000, maxOutputTokens: 8_192},
	{match: "llama-3.2", prefix: true, contextWindow: 128_000, maxOutputTokens: 8_192},
	{match: "llama3.2", prefix: true, contextWindow: 128_000, maxOutputTokens: 8_192},
	{match: "llama-3.1", prefix: true, contextWindow: 128_000, maxOutputTokens: 8_192},
	{match: "llama3.1", prefix: true, contextWindow: 128_000, maxOutputTokens: 8_192},
	{match: "codestral", prefix: true, contextWindow: 256_000, maxOutputTokens: 16_384},
	{match: "devstral", prefix: true, contextWindow: 128_000, maxOutputTokens: 16_384},
	{match: "mistral-large", prefix: true, contextWindow: 128_000, maxOutputTokens: 16_384},
	{match: "gemma3", prefix: true, contextWindow: 128_000, maxOutputTokens: 8_192},
	{match: "gemma-3", prefix: true, contextWindow: 128_000, maxOutputTokens: 8_192},
}

func normalizeModelKey(modelID string) string {
	s := strings.ToLower(strings.TrimSpace(modelID))
	if idx := strings.LastIndex(s, "/"); idx >= 0 {
		s = s[idx+1:]
	}
	if idx := strings.Index(s, ":"); idx >= 0 {
		s = s[:idx]
	}
	return strings.TrimSpace(s)
}

func tokensToK(tokens int) int {
	if tokens <= 0 {
		return 0
	}
	k := (tokens + 500) / 1000
	if k < 1 {
		k = 1
	}
	return k
}

// isNonChatModel flags audio, image, video, embedding, moderation, and legacy
// non-chat completion models that clutter OpenAI / gateway model pickers.
func isNonChatModel(modelID string) bool {
	norm := normalizeModelKey(modelID)
	if norm == "" {
		return false
	}
	prefixes := []string{
		"whisper", "tts-", "dall-e", "gpt-image", "chatgpt-image",
		"text-embedding", "embedding-", "omni-moderation", "text-moderation",
		"sora", "babbage", "davinci", "curie", "ada",
		"text-davinci", "text-curie", "text-babbage", "text-ada",
		"code-davinci", "code-cushman", "gpt-audio", "gpt-realtime",
	}
	for _, p := range prefixes {
		if strings.HasPrefix(norm, p) {
			return true
		}
	}
	substrings := []string{
		"-realtime", "-transcribe", "-tts", "-embedding", "moderation",
		"-image-preview", "-image-1", "audio-preview", "computer-use-preview",
	}
	for _, sub := range substrings {
		if strings.Contains(norm, sub) {
			return true
		}
	}
	return false
}

// modelGenerationScore ranks flagship/modern agentic models ahead of older
// snapshots so the most useful choices appear at the top of the model picker.
func modelGenerationScore(modelID string) int {
	norm := normalizeModelKey(modelID)
	switch {
	case strings.HasPrefix(norm, "gpt-6"):
		return 100
	case strings.HasPrefix(norm, "claude-opus-5"):
		return 98
	case strings.HasPrefix(norm, "claude-opus-4-8"), strings.HasPrefix(norm, "claude-opus-4.8"):
		return 96
	case strings.HasPrefix(norm, "gpt-5.4"):
		return 95
	case strings.HasPrefix(norm, "gemini-4-carbon"), strings.HasPrefix(norm, "gemini-4-neon"):
		return 94
	case strings.HasPrefix(norm, "claude-sonnet-5"), strings.HasPrefix(norm, "claude-opus-4-6"), strings.HasPrefix(norm, "claude-opus-4.6"):
		return 92
	case strings.HasPrefix(norm, "gpt-5.3"), strings.HasPrefix(norm, "gpt-5.2"), strings.HasPrefix(norm, "gpt-5.1"), strings.HasPrefix(norm, "gpt-5"):
		return 90
	case strings.HasPrefix(norm, "claude-sonnet-4-6"), strings.HasPrefix(norm, "claude-sonnet-4.6"):
		return 89
	case strings.HasPrefix(norm, "swe-1"):
		return 88
	case strings.HasPrefix(norm, "gemini-4"), strings.HasPrefix(norm, "gemini-3.8"):
		return 87
	case strings.HasPrefix(norm, "o4-"), strings.HasPrefix(norm, "o3"):
		return 85
	case strings.HasPrefix(norm, "claude-opus-4"), strings.HasPrefix(norm, "claude-sonnet-4"):
		return 84
	case strings.HasPrefix(norm, "deepseek-v4"), strings.HasPrefix(norm, "grok-4"):
		return 82
	case strings.HasPrefix(norm, "gpt-4.1"):
		return 80
	case strings.HasPrefix(norm, "claude-haiku-4"), strings.HasPrefix(norm, "gemini-2.5"):
		return 78
	case strings.HasPrefix(norm, "deepseek-r1"), strings.HasPrefix(norm, "deepseek-v3"), strings.HasPrefix(norm, "kimi-k2"), strings.HasPrefix(norm, "glm-5"), strings.HasPrefix(norm, "glm-4.7"), strings.HasPrefix(norm, "qwen3"):
		return 75
	case strings.HasPrefix(norm, "gpt-4o"), strings.HasPrefix(norm, "o1"):
		return 70
	case strings.HasPrefix(norm, "claude-3-7"), strings.HasPrefix(norm, "claude-3.7"), strings.HasPrefix(norm, "claude-3-5"), strings.HasPrefix(norm, "claude-3.5"):
		return 65
	case strings.HasPrefix(norm, "gpt-4"):
		return 55
	case strings.HasPrefix(norm, "gpt-3.5"):
		return 40
	default:
		return 50
	}
}

func looksDatedSnapshot(id string) bool {
	// e.g. gpt-4o-2024-08-06 or claude-sonnet-4-5-20250929
	if len(id) >= 11 && id[len(id)-11] == '-' && id[len(id)-6] == '-' && id[len(id)-3] == '-' {
		return true
	}
	if len(id) >= 9 && id[len(id)-9] == '-' {
		tail := id[len(id)-8:]
		if strings.HasPrefix(tail, "202") {
			allDigits := true
			for _, r := range tail {
				if r < '0' || r > '9' {
					allDigits = false
					break
				}
			}
			if allDigits {
				return true
			}
		}
	}
	return false
}

// sortModelDetails orders chat models before non-chat, higher-generation models
// first, canonical aliases ahead of dated snapshots, and newer release dates first.
func sortModelDetails(details []modelDetail) {
	sort.SliceStable(details, func(i, j int) bool {
		a, b := details[i], details[j]
		if a.NonChat != b.NonChat {
			return !a.NonChat
		}
		sa, sb := modelGenerationScore(a.ID), modelGenerationScore(b.ID)
		if sa != sb {
			return sa > sb
		}
		da, db := looksDatedSnapshot(a.ID), looksDatedSnapshot(b.ID)
		if da != db {
			return !da
		}
		if a.ReleaseDate != b.ReleaseDate {
			return a.ReleaseDate > b.ReleaseDate
		}
		return a.ID < b.ID
	})
}

// lookupKnownModelSpec resolves curated context/output/reasoning defaults for a
// model ID (provider-aware for max_tokens_field and thinking_type).
func lookupKnownModelSpec(provider, modelID string) (modelDetail, bool) {
	norm := normalizeModelKey(modelID)
	if norm == "" {
		return modelDetail{}, false
	}
	for _, r := range knownModelRules {
		matched := false
		if r.prefix {
			matched = strings.HasPrefix(norm, r.match)
		} else {
			matched = strings.Contains(norm, r.match)
		}
		if !matched {
			continue
		}
		d := modelDetail{
			ID:                     modelID,
			ContextWindow:          r.contextWindow,
			ContextWindowK:         tokensToK(r.contextWindow),
			MaxOutputTokens:        r.maxOutputTokens,
			ReasoningLevels:        append([]string(nil), r.reasoningLevels...),
			DefaultReasoningEffort: r.defaultReasoningEffort,
			NonChat:                isNonChatModel(modelID),
			Source:                 "known",
		}
		if r.supportsThinkingSwitch && (provider == "anthropic" || provider == "anthropic-oauth") {
			d.ThinkingType = "enabled"
		}
		if r.usesMaxCompletion && provider == "openai" {
			d.MaxTokensField = llm.MaxTokensFieldCompletion
		}
		return d, true
	}
	return modelDetail{}, false
}

func mergeModelDetailFallback(dst *modelDetail, src modelDetail) {
	if dst.Label == "" && src.Label != "" && src.Label != dst.ID {
		dst.Label = src.Label
	}
	if dst.ContextWindow == 0 && src.ContextWindow > 0 {
		dst.ContextWindow = src.ContextWindow
		dst.ContextWindowK = src.ContextWindowK
		if dst.Source == "" {
			dst.Source = src.Source
		}
	}
	if dst.MaxOutputTokens == 0 && src.MaxOutputTokens > 0 {
		dst.MaxOutputTokens = src.MaxOutputTokens
		if dst.Source == "" {
			dst.Source = src.Source
		}
	}
	if len(dst.ReasoningLevels) == 0 && len(src.ReasoningLevels) > 0 {
		dst.ReasoningLevels = append([]string(nil), src.ReasoningLevels...)
	}
	if dst.DefaultReasoningEffort == "" && src.DefaultReasoningEffort != "" &&
		len(dst.ReasoningLevels) > 0 && containsStr(dst.ReasoningLevels, src.DefaultReasoningEffort) {
		dst.DefaultReasoningEffort = src.DefaultReasoningEffort
	}
	if dst.ThinkingType == "" && src.ThinkingType != "" {
		dst.ThinkingType = src.ThinkingType
	}
	if dst.MaxTokensField == "" && src.MaxTokensField != "" {
		dst.MaxTokensField = src.MaxTokensField
	}
	if dst.PriceInputPerM == 0 && src.PriceInputPerM > 0 {
		dst.PriceInputPerM = src.PriceInputPerM
	}
	if dst.PriceOutputPerM == 0 && src.PriceOutputPerM > 0 {
		dst.PriceOutputPerM = src.PriceOutputPerM
	}
	if dst.PriceCacheReadPerM == 0 && src.PriceCacheReadPerM > 0 {
		dst.PriceCacheReadPerM = src.PriceCacheReadPerM
	}
	if dst.PriceCacheWritePerM == 0 && src.PriceCacheWritePerM > 0 {
		dst.PriceCacheWritePerM = src.PriceCacheWritePerM
	}
	if dst.ReleaseDate == "" && src.ReleaseDate != "" {
		dst.ReleaseDate = src.ReleaseDate
	}
	if dst.SupportsTools == nil && src.SupportsTools != nil {
		dst.SupportsTools = src.SupportsTools
	}
	if dst.SupportsVision == nil && src.SupportsVision != nil {
		dst.SupportsVision = src.SupportsVision
	}
}

// rawModelEntry parses common fields across OpenAI, Anthropic, OpenRouter,
// OpenCode Console, Together, Fireworks, Groq, and vLLM /models responses.
type rawModelEntry struct {
	ID                  string `json:"id"`
	DisplayName         string `json:"display_name"`
	Name                string `json:"name"`
	ContextLength       int    `json:"context_length"`
	MaxContextLength    int    `json:"max_context_length"`
	ContextWindow       int    `json:"context_window"`
	MaxInputTokens      int    `json:"max_input_tokens"`
	MaxModelLen         int    `json:"max_model_len"`
	MaxOutputTokens     int    `json:"max_output_tokens"`
	MaxCompletionTokens int    `json:"max_completion_tokens"`
	MaxTokens           int    `json:"max_tokens"`
	Created             int64  `json:"created"`
	CreatedAt           string `json:"created_at"`
	ReleaseDate         string `json:"release_date"`
	Attachment          *bool  `json:"attachment"`
	ToolCall            *bool  `json:"tool_call"`
	Reasoning           bool   `json:"reasoning"`
	ReasoningOptions    []struct {
		Type   string   `json:"type"`
		Values []string `json:"values"`
	} `json:"reasoning_options"`
	Limit *struct {
		Context int `json:"context"`
		Output  int `json:"output"`
	} `json:"limit"`
	Cost *struct {
		Input      float64 `json:"input"`
		Output     float64 `json:"output"`
		CacheRead  float64 `json:"cache_read"`
		CacheWrite float64 `json:"cache_write"`
	} `json:"cost"`
	TopProvider *struct {
		ContextLength       int `json:"context_length"`
		MaxCompletionTokens int `json:"max_completion_tokens"`
	} `json:"top_provider"`
}

func firstPositive(vals ...int) int {
	for _, v := range vals {
		if v > 0 {
			return v
		}
	}
	return 0
}

func buildModelDetailFromAPI(provider string, raw rawModelEntry, reg *modelRegistry) modelDetail {
	d := modelDetail{
		ID:      raw.ID,
		NonChat: isNonChatModel(raw.ID),
	}
	if lbl := strings.TrimSpace(raw.DisplayName); lbl != "" && lbl != raw.ID {
		d.Label = lbl
	} else if lbl := strings.TrimSpace(raw.Name); lbl != "" && lbl != raw.ID {
		d.Label = lbl
	}
	if raw.ReleaseDate != "" {
		d.ReleaseDate = strings.TrimSpace(raw.ReleaseDate)
	} else if len(raw.CreatedAt) >= 10 {
		d.ReleaseDate = raw.CreatedAt[:10]
	} else if raw.Created > 1_500_000_000 {
		d.ReleaseDate = time.Unix(raw.Created, 0).UTC().Format("2006-01-02")
	}

	var tpCtx, tpOut, limCtx, limOut int
	if raw.TopProvider != nil {
		tpCtx = raw.TopProvider.ContextLength
		tpOut = raw.TopProvider.MaxCompletionTokens
	}
	if raw.Limit != nil {
		limCtx = raw.Limit.Context
		limOut = raw.Limit.Output
	}
	apiCtx := firstPositive(raw.ContextLength, raw.ContextWindow, raw.MaxContextLength, raw.MaxInputTokens, raw.MaxModelLen, limCtx, tpCtx)
	apiOut := firstPositive(raw.MaxOutputTokens, raw.MaxCompletionTokens, limOut, tpOut, raw.MaxTokens)

	if apiCtx > 0 {
		d.ContextWindow = apiCtx
		d.ContextWindowK = tokensToK(apiCtx)
		d.Source = "api"
	}
	if apiOut > 0 {
		d.MaxOutputTokens = apiOut
		if d.Source == "" {
			d.Source = "api"
		}
	}
	if raw.Cost != nil {
		d.PriceInputPerM = raw.Cost.Input
		d.PriceOutputPerM = raw.Cost.Output
		d.PriceCacheReadPerM = raw.Cost.CacheRead
		d.PriceCacheWritePerM = raw.Cost.CacheWrite
	}
	if raw.Attachment != nil {
		d.SupportsVision = raw.Attachment
	}
	if raw.ToolCall != nil {
		d.SupportsTools = raw.ToolCall
	}
	for _, ro := range raw.ReasoningOptions {
		switch strings.ToLower(strings.TrimSpace(ro.Type)) {
		case "toggle":
			if provider == "anthropic" || provider == "anthropic-oauth" {
				d.ThinkingType = "enabled"
			}
		case "effort":
			for _, v := range ro.Values {
				v = strings.ToLower(strings.TrimSpace(v))
				if v != "" && !containsStr(d.ReasoningLevels, v) {
					d.ReasoningLevels = append(d.ReasoningLevels, v)
				}
			}
		}
	}
	if len(d.ReasoningLevels) > 0 && d.DefaultReasoningEffort == "" {
		for _, pref := range []string{"high", "medium", "low"} {
			if containsStr(d.ReasoningLevels, pref) {
				d.DefaultReasoningEffort = pref
				break
			}
		}
	}

	// Enrich missing fields in authority order: live models.dev -> compiled known table.
	if reg != nil {
		if md, ok := reg.Lookup(provider, raw.ID); ok {
			mergeModelDetailFallback(&d, md)
		}
	}
	if known, ok := lookupKnownModelSpec(provider, raw.ID); ok {
		mergeModelDetailFallback(&d, known)
	}
	return d
}

// parseOpenCodeCatalogBody parses either OpenCode's rich Console catalog
// (`/console/api/v1/models` with `{"opencode-go":{"models":{...}}}` or
// `{"opencode":{"models":{...}}}`) or the standard OpenAI-shaped `{"data":[...]}`.
func parseOpenCodeCatalogBody(provider string, body []byte, reg *modelRegistry) ([]modelDetail, bool) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, false
	}
	// 1) Standard OpenAI-shaped {"data": [...]}
	if dataRaw, ok := root["data"]; ok {
		var entries []rawModelEntry
		if err := json.Unmarshal(dataRaw, &entries); err == nil && len(entries) > 0 {
			out := make([]modelDetail, 0, len(entries))
			for _, m := range entries {
				if strings.TrimSpace(m.ID) == "" {
					continue
				}
				out = append(out, buildModelDetailFromAPI(provider, m, reg))
			}
			if len(out) > 0 {
				return out, true
			}
		}
	}
	// 2) OpenCode Console rich format: {"opencode-go": {"models": {...}}} or {"opencode": {"models": {...}}}
	provKey := "opencode-go"
	if provider == "opencode-zen" {
		provKey = "opencode"
	}
	provRaw, ok := root[provKey]
	if !ok {
		return nil, false
	}
	var provDoc struct {
		Models json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(provRaw, &provDoc); err != nil || len(provDoc.Models) == 0 {
		return nil, false
	}
	var details []modelDetail
	var modelMap map[string]rawModelEntry
	if err := json.Unmarshal(provDoc.Models, &modelMap); err == nil && len(modelMap) > 0 {
		for k, entry := range modelMap {
			if strings.TrimSpace(entry.ID) == "" {
				entry.ID = k
			}
			if strings.TrimSpace(entry.ID) == "" {
				continue
			}
			details = append(details, buildModelDetailFromAPI(provider, entry, reg))
		}
		return details, len(details) > 0
	}
	var modelArr []rawModelEntry
	if err := json.Unmarshal(provDoc.Models, &modelArr); err == nil && len(modelArr) > 0 {
		for _, entry := range modelArr {
			if strings.TrimSpace(entry.ID) == "" {
				continue
			}
			details = append(details, buildModelDetailFromAPI(provider, entry, reg))
		}
		return details, len(details) > 0
	}
	return nil, false
}

// probeOllamaModelContext queries Ollama's native POST /api/show endpoint to
// discover the actual context length configured for a local model.
func probeOllamaModelContext(ctx context.Context, client *http.Client, ollamaRootURL, modelName string, hdr http.Header) (int, bool) {
	showURL := strings.TrimRight(ollamaRootURL, "/") + "/api/show"
	payload, err := json.Marshal(map[string]string{"name": modelName})
	if err != nil {
		return 0, false
	}
	pCtx, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(pCtx, http.MethodPost, showURL, bytes.NewReader(payload))
	if err != nil {
		return 0, false
	}
	req.Header = hdr.Clone()
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return 0, false
	}
	var parsed struct {
		Parameters string         `json:"parameters"`
		ModelInfo  map[string]any `json:"model_info"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, false
	}
	// Explicit Modelfile `num_ctx` override takes priority over model architecture default.
	if numCtx := parseOllamaNumCtx(parsed.Parameters); numCtx > 0 {
		return numCtx, true
	}
	for k, v := range parsed.ModelInfo {
		if strings.HasSuffix(strings.ToLower(k), ".context_length") || strings.EqualFold(k, "context_length") {
			if n, ok := jsonNumberToInt(v); ok && n > 0 {
				return n, true
			}
		}
	}
	return 0, false
}

func parseOllamaNumCtx(parameters string) int {
	for _, line := range strings.Split(parameters, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.EqualFold(fields[0], "num_ctx") {
			if n, err := strconv.Atoi(fields[1]); err == nil && n > 0 {
				return n
			}
		}
	}
	return 0
}

func jsonNumberToInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		if n > 0 {
			return int(n), true
		}
	case int:
		if n > 0 {
			return n, true
		}
	case int64:
		if n > 0 {
			return int(n), true
		}
	}
	return 0, false
}

// enrichOllamaDetails concurrently queries Ollama /api/show for up to 24 models.
func enrichOllamaDetails(ctx context.Context, client *http.Client, ollamaRootURL string, hdr http.Header, details []modelDetail) {
	limit := len(details)
	if limit > 24 {
		limit = 24
	}
	if limit == 0 {
		return
	}
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i := 0; i < limit; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if ctxLen, ok := probeOllamaModelContext(ctx, client, ollamaRootURL, details[idx].ID, hdr); ok && ctxLen > 0 {
				details[idx].ContextWindow = ctxLen
				details[idx].ContextWindowK = tokensToK(ctxLen)
				details[idx].Source = "ollama"
			}
		}(i)
	}
	wg.Wait()
}

// lookupBestModelDetail resolves the richest non-blocking metadata available
// from cached provider catalogs -> models.dev -> compiled known table.
func (s *Server) lookupBestModelDetail(provider, modelID string) (modelDetail, bool) {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return modelDetail{}, false
	}
	reg := s.ensureModelRegistry()
	if reg != nil {
		reg.TriggerBackgroundRefresh(s.ctx)
	}

	var result modelDetail
	found := false

	// 1. Live RAM or disk catalog for Antigravity / Devin.
	if provider == "devin" || provider == "antigravity-cli" {
		if _, details, ok := s.getCachedModelCatalog(provider); ok {
			for _, d := range details {
				if strings.EqualFold(d.ID, modelID) {
					result, found = d, true
					break
				}
			}
		}
		if !found {
			if _, details, _, ok := s.readDiskProviderCatalog(provider); ok {
				for _, d := range details {
					if strings.EqualFold(d.ID, modelID) {
						result, found = d, true
						break
					}
				}
			}
		}
		if !found && provider == "devin" {
			for _, e := range llm.DevinFallbackCatalog() {
				if strings.EqualFold(e.ID, modelID) {
					result = modelDetail{
						ID:              e.ID,
						Label:           e.Name,
						ContextWindow:   e.ContextWindow,
						ContextWindowK:  tokensToK(e.ContextWindow),
						MaxOutputTokens: e.MaxOutputTokens,
						ReasoningLevels: append([]string(nil), e.ReasoningLevels...),
						Source:          "known",
					}
					found = true
					break
				}
			}
		}
		if !found && provider == "antigravity-cli" {
			for _, e := range llm.AntigravityFallbackCatalog() {
				if strings.EqualFold(e.ID, modelID) {
					result = modelDetail{
						ID:              e.ID,
						Label:           e.DisplayName,
						ContextWindow:   e.ContextWindow,
						ContextWindowK:  tokensToK(e.ContextWindow),
						MaxOutputTokens: e.MaxOutputTokens,
						ReasoningLevels: append([]string(nil), e.ReasoningLevels...),
						Source:          "known",
					}
					found = true
					break
				}
			}
		}
	}

	// 2. models.dev registry (adds pricing, latest releases, reasoning levels).
	if reg != nil {
		if md, ok := reg.Lookup(provider, modelID); ok {
			if !found {
				result, found = md, true
			} else {
				mergeModelDetailFallback(&result, md)
			}
		}
	}

	// 3. Compiled offline known-model table.
	if known, ok := lookupKnownModelSpec(provider, modelID); ok {
		if !found {
			result, found = known, true
		} else {
			mergeModelDetailFallback(&result, known)
		}
	}
	return result, found
}

// pgLookupModelSpec resolves auto-detected context window, output cap, reasoning
// levels, pricing, and cap field for a single (provider, model) pair. For Ollama
// it also probes POST /api/show (SSRF-guarded) to read the exact local context_length.
func (s *Server) pgLookupModelSpec(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider  string `json:"provider"`
		Model     string `json:"model"`
		BaseURL   string `json:"base_url"`
		Proxy     string `json:"proxy"`
		APIKey    string `json:"api_key"`
		ProfileID *int64 `json:"profile_id"`
	}
	if err := decode(w, r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	provider := strings.TrimSpace(req.Provider)
	modelID := strings.TrimSpace(req.Model)
	if modelID == "" {
		writeJSON(w, 200, map[string]any{"ok": true, "found": false})
		return
	}

	detail, found := s.lookupBestModelDetail(provider, modelID)

	// Live single-model probe for Ollama so manually typed local models still detect real context_length.
	if provider == "ollama" {
		if ctxLen, ok := s.probeSingleOllamaSpec(r.Context(), req.BaseURL, req.Proxy, req.APIKey, req.ProfileID, modelID); ok && ctxLen > 0 {
			if !found {
				detail = modelDetail{ID: modelID}
				found = true
			}
			detail.ContextWindow = ctxLen
			detail.ContextWindowK = tokensToK(ctxLen)
			detail.Source = "ollama"
		}
	}

	if found {
		writeJSON(w, 200, map[string]any{"ok": true, "found": true, "detail": detail})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "found": false})
}

func (s *Server) probeSingleOllamaSpec(ctx context.Context, rawBaseURL, rawProxy, rawAPIKey string, profileID *int64, modelID string) (int, bool) {
	needProxy := isMaskedSecret(rawProxy)
	needKey := strings.TrimSpace(rawAPIKey) == ""
	if needProxy {
		rawProxy = ""
	}
	if (needProxy || needKey) && profileID != nil && s.m != nil && s.m.pg != nil {
		if p, err := s.m.pg.ProfileByID(*profileID); err == nil && p != nil {
			if needProxy {
				rawProxy = p.Proxy
			}
			if needKey && sameLLMEndpoint("ollama", rawBaseURL, p.Format, p.BaseURL) {
				rawAPIKey = p.APIKey
			}
		}
	}
	baseURL := strings.TrimRight(strings.TrimSpace(rawBaseURL), "/")
	defaultOllama := baseURL == ""
	if baseURL == "" {
		baseURL = agent.DefaultOllamaBaseURL
	}
	ollamaRoot := strings.TrimRight(strings.TrimSuffix(strings.TrimSuffix(baseURL, "/chat/completions"), "/v1"), "/")
	showURL := ollamaRoot + "/api/show"

	guard := newModelURLGuard(s)
	var proxyURL *url.URL
	if p := strings.TrimSpace(rawProxy); p != "" {
		var err error
		proxyURL, err = guard.validateURL(ctx, p, "proxy")
		if err != nil {
			return 0, false
		}
	}
	if defaultOllama {
		if du, err := url.Parse(agent.DefaultOllamaBaseURL); err == nil {
			if addr, err := netip.ParseAddr(du.Hostname()); err == nil {
				if port, ok := urlEffectivePort(du); ok {
					guard.allowDialAddr(addr, port)
				}
			}
		}
	} else {
		if _, err := guard.validateURL(ctx, showURL, "endpoint"); err != nil {
			return 0, false
		}
	}

	var transport *http.Transport
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = base.Clone()
	} else {
		transport = &http.Transport{}
	}
	transport.Proxy = nil
	if proxyURL != nil {
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	if s.modelRootCAs != nil {
		if transport.TLSClientConfig == nil {
			transport.TLSClientConfig = &tls.Config{}
		} else {
			transport.TLSClientConfig = transport.TLSClientConfig.Clone()
		}
		transport.TLSClientConfig.RootCAs = s.modelRootCAs
	}
	dialCtx := (&net.Dialer{Timeout: 1500 * time.Millisecond, Control: guard.blockDial}).DialContext
	if s.modelDialContext != nil {
		dialCtx = s.modelDialContext
	}
	transport.DialContext = dialCtx
	defer transport.CloseIdleConnections()

	client := &http.Client{
		Transport: transport,
		Timeout:   2500 * time.Millisecond,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxModelRedirects {
				return &modelURLRefusalError{msg: "too many redirects"}
			}
			if len(via) > 0 {
				prev := via[len(via)-1].URL
				prevScheme := strings.ToLower(prev.Scheme)
				nextScheme := strings.ToLower(req.URL.Scheme)
				if nextScheme != prevScheme {
					if !(prevScheme == "http" && nextScheme == "https" && sameRedirectHost(prev, req.URL)) {
						return &modelURLRefusalError{msg: "redirect scheme change is not allowed"}
					}
				}
			}
			if _, err := guard.validateURL(req.Context(), req.URL.String(), "redirect"); err != nil {
				return err
			}
			return nil
		},
	}
	hdr := http.Header{}
	if k := strings.TrimSpace(rawAPIKey); k != "" {
		hdr.Set("Authorization", "Bearer "+k)
	}
	return probeOllamaModelContext(ctx, client, ollamaRoot, modelID, hdr)
}
