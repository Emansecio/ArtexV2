package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
)

type anthropicProvider struct{ cfg Config }

type anthropicSystemBlock struct {
	Type         string         `json:"type"`
	Text         string         `json:"text"`
	CacheControl map[string]any `json:"cache_control,omitempty"`
}

type anthropicWireMessage struct {
	Role    Role                 `json:"role"`
	Content []anthropicWireBlock `json:"content"`
}

type anthropicWireBlock struct {
	Type         BlockType       `json:"type"`
	Text         string          `json:"text,omitempty"`
	Thinking     string          `json:"thinking,omitempty"`
	Signature    string          `json:"signature,omitempty"`
	Data         string          `json:"data,omitempty"`
	ID           string          `json:"id,omitempty"`
	Name         string          `json:"name,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
	ToolUseID    string          `json:"tool_use_id,omitempty"`
	Content      []ContentBlock  `json:"content,omitempty"`
	IsError      bool            `json:"is_error,omitempty"`
	CacheControl map[string]any  `json:"cache_control,omitempty"`
}

func (b anthropicWireBlock) MarshalJSON() ([]byte, error) {
	if b.Type == BlockThinking {
		return json.Marshal(struct {
			Type      BlockType `json:"type"`
			Thinking  string    `json:"thinking"`
			Signature string    `json:"signature,omitempty"`
		}{b.Type, b.Thinking, b.Signature})
	}
	type plain anthropicWireBlock
	return json.Marshal(plain(b))
}

type anthropicReq struct {
	Model         string                 `json:"model"`
	System        []anthropicSystemBlock `json:"system,omitempty"`
	Messages      any                    `json:"messages"`
	Tools         []anthropicTool        `json:"tools,omitempty"`
	MaxTokens     int                    `json:"max_tokens"`
	Temperature   *float64               `json:"temperature,omitempty"`
	StopSequences []string               `json:"stop_sequences,omitempty"`
	Thinking      *anthropicThinking     `json:"thinking,omitempty"`
	OutputConfig  *anthropicOutputConfig `json:"output_config,omitempty"`
	Stream        bool                   `json:"stream"`
}

// anthropicThinking configures Anthropic's extended/adaptive thinking block.
type anthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
	Display      string `json:"display,omitempty"`
}

const (
	minAnthropicThinkingBudget     = 1024
	defaultAnthropicThinkingBudget = 10000
)

// anthropicOutputConfig carries the reasoning strength via
// "output_config":{"effort":"low"|"medium"|"high"|"max"}.
type anthropicOutputConfig struct {
	Effort string `json:"effort"`
}

type anthropicTool struct {
	Name         string         `json:"name"`
	Description  string         `json:"description"`
	InputSchema  map[string]any `json:"input_schema"`
	CacheControl map[string]any `json:"cache_control,omitempty"`
}

func anthropicCacheControl(ttl string) map[string]any {
	cc := map[string]any{"type": "ephemeral"}
	if ttl != "" && ttl != "5m" {
		cc["ttl"] = ttl
	}
	return cc
}

// resolveAnthropicCacheControls computes the cache_control objects for stable
// prefix breakpoints ([tools, static system]) and the rolling conversation tail.
// A non-empty cfgTTL overrides per-request TTLs uniformly; otherwise Anthropic's
// ordering rule (1h entries must precede 5m entries) promotes stableTTL to 1h
// whenever tailTTL is 1h.
func resolveAnthropicCacheControls(cfgTTL, reqStableTTL, reqTailTTL string) (ccStable, ccTail map[string]any) {
	stableTTL := reqStableTTL
	tailTTL := reqTailTTL
	if cfgTTL != "" {
		stableTTL = cfgTTL
		tailTTL = cfgTTL
	}
	// Anthropic requires 1h cache entries to precede 5m entries in the prefix.
	if tailTTL == "1h" && stableTTL != "1h" {
		stableTTL = "1h"
	}
	return anthropicCacheControl(stableTTL), anthropicCacheControl(tailTTL)
}

func (p *anthropicProvider) buildBody(req CompletionRequest, stream bool) ([]byte, error) {
	maxTok := req.MaxTokens
	if maxTok == 0 {
		maxTok = 8192
	}
	// Effective thinking mode: a non-empty per-request override (req.Thinking)
	// wins over Config.ThinkingType. Thinking-type content blocks may be replayed
	// only when thinking is actively enabled/adaptive; any other mode (incl. "disabled")
	// strips them so Anthropic does not reject the request.
	thinkingType := p.cfg.ThinkingType
	if req.Thinking != "" {
		thinkingType = req.Thinking
	}
	thinkingEnabled := thinkingType != "" && thinkingType != "disabled"
	var thinkingBlock *anthropicThinking
	if thinkingEnabled {
		display := p.cfg.ThinkingDisplay
		if display == "" {
			display = "summarized"
		}
		thinkingBlock = &anthropicThinking{
			Type:    thinkingType,
			Display: display,
		}
		if thinkingType == "enabled" {
			budget := p.cfg.ThinkingBudget
			if budget <= 0 {
				budget = defaultAnthropicThinkingBudget
			} else if budget < minAnthropicThinkingBudget {
				budget = minAnthropicThinkingBudget
			}
			// Anthropic requires 1024 <= budget_tokens < max_tokens.
			if maxTok <= minAnthropicThinkingBudget {
				maxTok = minAnthropicThinkingBudget + 1024
			}
			if budget >= maxTok {
				budget = maxTok - 1024
				if budget < minAnthropicThinkingBudget {
					budget = minAnthropicThinkingBudget
					maxTok = budget + 1024
				}
			}
			thinkingBlock.BudgetTokens = budget
		}
	} else if thinkingType != "" {
		thinkingBlock = &anthropicThinking{Type: thinkingType}
	}
	var ccStable, ccTail map[string]any
	if p.cfg.promptCacheEnabled() {
		ccStable, ccTail = resolveAnthropicCacheControls(p.cfg.PromptCacheTTL, req.PromptCacheTTL, req.PromptCacheTailTTL)
	}
	filtered := mergeAdjacentSameRole(PairToolBlocks(filterThinkingBlocks(req.Messages, thinkingEnabled)))
	body := anthropicReq{
		Model:         p.cfg.Model,
		Messages:      toAnthropicWireMessages(filtered, ccTail),
		MaxTokens:     maxTok,
		Temperature:   req.Temperature,
		StopSequences: req.Stop,
		Stream:        stream,
		System:        buildAnthropicSystem(req, ccStable),
		Thinking:      thinkingBlock,
	}
	for _, t := range req.Tools {
		body.Tools = append(body.Tools, anthropicTool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
	}
	if ccStable != nil && len(body.Tools) > 0 {
		body.Tools[len(body.Tools)-1].CacheControl = ccStable
	}
	effort := p.cfg.ReasoningEffort
	reasoningActive := (p.cfg.ReasoningEffort != "" && p.cfg.ReasoningEffort != "none") || thinkingEnabled
	if req.ReasoningEffort != "" && reasoningActive {
		effort = req.ReasoningEffort
	}
	if thinkingType != "disabled" && effort != "" && effort != "none" {
		body.OutputConfig = &anthropicOutputConfig{Effort: effort}
	}
	return json.Marshal(body)
}

func isAnthropicCacheableBlock(b anthropicWireBlock) bool {
	switch b.Type {
	case BlockToolResult, BlockToolUse:
		return true
	case BlockText:
		return b.Text != ""
	default:
		return false
	}
}

func toAnthropicWireMessages(msgs []Message, cc map[string]any) []anthropicWireMessage {
	wire := make([]anthropicWireMessage, len(msgs))
	for i, m := range msgs {
		blocks := make([]anthropicWireBlock, len(m.Content))
		for j, b := range m.Content {
			input := b.Input
			if b.Type == BlockToolUse && (len(input) == 0 || !json.Valid(input)) {
				input = json.RawMessage("{}")
			}
			nested := b.Content
			if b.Type == BlockToolResult && len(nested) > 0 {
				hasEmptyText := false
				for _, nb := range nested {
					if nb.Type == BlockText && nb.Text == "" {
						hasEmptyText = true
						break
					}
				}
				if hasEmptyText {
					filtered := make([]ContentBlock, 0, len(nested))
					for _, nb := range nested {
						if nb.Type == BlockText && nb.Text == "" {
							continue
						}
						filtered = append(filtered, nb)
					}
					if len(filtered) == 0 {
						filtered = []ContentBlock{TextBlock("(no output)")}
					}
					nested = filtered
				}
			}
			blocks[j] = anthropicWireBlock{
				Type:      b.Type,
				Text:      b.Text,
				Thinking:  b.Thinking,
				Signature: b.Signature,
				Data:      b.Data,
				ID:        b.ID,
				Name:      b.Name,
				Input:     input,
				ToolUseID: b.ToolUseID,
				Content:   nested,
				IsError:   b.IsError,
			}
		}
		wire[i] = anthropicWireMessage{Role: m.Role, Content: blocks}
	}
	if cc != nil && len(wire) > 0 {
		// Place up to 2 sliding breakpoints on messages (Anthropic allows 4 total:
		// tools [1] + static system [2] + penultimate stable turn [3] + latest turn [4]).
		// Having a second breakpoint on an earlier turn preserves cache hits across
		// retries, micro-compaction tail edits, and sidequestion forks.
		placedLatest := -1
		for i := len(wire) - 1; i >= 0; i-- {
			for j := len(wire[i].Content) - 1; j >= 0; j-- {
				if isAnthropicCacheableBlock(wire[i].Content[j]) {
					wire[i].Content[j].CacheControl = cc
					placedLatest = i
					break
				}
			}
			if placedLatest >= 0 {
				break
			}
		}
		if placedLatest >= 2 {
			for i := placedLatest - 2; i >= 0; i-- {
				done := false
				for j := len(wire[i].Content) - 1; j >= 0; j-- {
					if isAnthropicCacheableBlock(wire[i].Content[j]) {
						wire[i].Content[j].CacheControl = cc
						done = true
						break
					}
				}
				if done {
					break
				}
			}
		}
	}
	return wire
}

// buildAnthropicSystem splits the system prompt at DynamicBoundary, placing a
// cache breakpoint on the static prefix (FR-02.2). When cc is non-nil and
// DynamicBoundary <= 0, the whole system prompt is cached in a single block.
func buildAnthropicSystem(req CompletionRequest, cc map[string]any) []anthropicSystemBlock {
	if len(req.System) == 0 {
		return nil
	}
	b := req.DynamicBoundary
	if b <= 0 || b >= len(req.System) {
		text := joinSystem(req.System)
		if text == "" {
			return nil
		}
		return []anthropicSystemBlock{{Type: "text", Text: text, CacheControl: cc}}
	}
	staticText := joinSystem(req.System[:b])
	dynText := joinSystem(req.System[b:])
	switch {
	case staticText != "" && dynText != "":
		return []anthropicSystemBlock{
			{Type: "text", Text: staticText, CacheControl: cc},
			{Type: "text", Text: dynText},
		}
	case staticText != "":
		return []anthropicSystemBlock{{Type: "text", Text: staticText, CacheControl: cc}}
	case dynText != "":
		return []anthropicSystemBlock{{Type: "text", Text: dynText}}
	default:
		return nil
	}
}

func (p *anthropicProvider) Stream(ctx context.Context, req CompletionRequest) iter.Seq2[StreamEvent, error] {
	return func(yield func(StreamEvent, error) bool) {
		body, err := p.buildBody(req, true)
		if err != nil {
			yield(StreamEvent{}, err)
			return
		}
		resp, err := doStream(ctx, p.cfg, p.cfg.BaseURL+"/v1/messages", body, func(r *http.Request) {
			r.Header.Set("content-type", "application/json")
			if p.cfg.UseBearerAuth {
				r.Header.Set("authorization", "Bearer "+p.cfg.APIKey)
			} else {
				r.Header.Set("x-api-key", p.cfg.APIKey)
			}
			r.Header.Set("anthropic-version", p.cfg.APIVersion)
			r.Header.Set("accept", "text/event-stream")
		}, "anthropic")
		if err != nil {
			yield(StreamEvent{}, err)
			return
		}
		defer resp.Body.Close()
		scan := newSSEScannerWithIdleTimeout(resp.Body, p.cfg.streamIdleTimeout())
		defer scan.Close()
		for {
			_, data, serr := scan.next()
			if serr == io.EOF {
				return
			}
			if serr != nil {
				yield(StreamEvent{}, serr)
				return
			}
			if data == "" {
				continue
			}
			ev, ok, perr := parseAnthropicFrame(data)
			if perr != nil {
				yield(StreamEvent{}, perr)
				return
			}
			if !ok {
				continue
			}
			if !yield(ev, nil) {
				return
			}
		}
	}
}

// Complete performs a real non-streaming completion: it sends stream:false and
// parses the single JSON response from /v1/messages into a full assistant
// Message, returning it with the stop reason and normalized usage.
func (p *anthropicProvider) Complete(ctx context.Context, req CompletionRequest) (Message, string, Usage, error) {
	body, err := p.buildBody(req, false)
	if err != nil {
		return Message{}, "", Usage{}, err
	}
	resp, err := doStream(ctx, p.cfg, p.cfg.BaseURL+"/v1/messages", body, func(r *http.Request) {
		r.Header.Set("content-type", "application/json")
		if p.cfg.UseBearerAuth {
			r.Header.Set("authorization", "Bearer "+p.cfg.APIKey)
		} else {
			r.Header.Set("x-api-key", p.cfg.APIKey)
		}
		r.Header.Set("anthropic-version", p.cfg.APIVersion)
		r.Header.Set("accept", "application/json")
	}, "anthropic")
	if err != nil {
		return Message{}, "", Usage{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return Message{}, "", Usage{}, err
	}
	return parseAnthropicResponse(raw)
}

// parseAnthropicResponse decodes a non-streaming Messages API response into the
// neutral Message model. Anthropic already speaks the content-block shape, so
// text/thinking/tool_use blocks map across directly.
func parseAnthropicResponse(raw []byte) (Message, string, Usage, error) {
	var r struct {
		Type    string `json:"type"`
		Content []struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			Thinking  string          `json:"thinking"`
			Signature string          `json:"signature"`
			Data      string          `json:"data"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string          `json:"stop_reason"`
		Usage      *anthropicUsage `json:"usage"`
		Error      *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return Message{}, "", Usage{}, fmt.Errorf("anthropic: decode response: %w (body: %s)", err, truncate(string(raw), 500))
	}
	if r.Error != nil || r.Type == "error" {
		msg := "error response"
		if r.Error != nil {
			msg = r.Error.Message
		}
		return Message{}, "", Usage{}, fmt.Errorf("anthropic: %s", msg)
	}
	var usage Usage
	if r.Usage != nil {
		usage = r.Usage.norm()
	}
	var blocks []ContentBlock
	for _, b := range r.Content {
		switch b.Type {
		case "text":
			if b.Text != "" {
				blocks = append(blocks, TextBlock(b.Text))
			}
		case "thinking":
			blocks = append(blocks, ContentBlock{Type: BlockThinking, Thinking: b.Thinking, Signature: b.Signature})
		case "redacted_thinking":
			blocks = append(blocks, ContentBlock{Type: BlockRedactedThinking, Data: b.Data})
		case "tool_use":
			in := b.Input
			if len(in) == 0 {
				in = json.RawMessage("{}")
			}
			blocks = append(blocks, ContentBlock{Type: BlockToolUse, ID: b.ID, Name: b.Name, Input: in})
		}
	}
	return Message{Role: RoleAssistant, Content: blocks}, r.StopReason, usage, nil
}

func parseAnthropicFrame(data string) (StreamEvent, bool, error) {
	var f struct {
		Type         string `json:"type"`
		Index        int    `json:"index"`
		ContentBlock struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
			Data string `json:"data"`
		} `json:"content_block"`
		Delta struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			PartialJSON string `json:"partial_json"`
			Thinking    string `json:"thinking"`
			Signature   string `json:"signature"`
			StopReason  string `json:"stop_reason"`
		} `json:"delta"`
		Usage   *anthropicUsage `json:"usage"`
		Message struct {
			Usage *anthropicUsage `json:"usage"`
		} `json:"message"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(data), &f); err != nil {
		return StreamEvent{}, false, nil // tolerate non-JSON keepalives
	}
	switch f.Type {
	case "message_start":
		if f.Message.Usage != nil {
			// Anthropic reports the full input side (input + cache read/creation) in
			// BOTH message_start and message_delta — the two ECHO it rather than
			// partition it. Both events therefore carry the input side, and
			// Usage.MergeEcho (not Add) is what keeps it from doubling: it takes the
			// max per echoed field and accumulates only output. Do NOT "fix" this by
			// making message_delta output-only again — Anthropic-compatible
			// emulations report zeros here and the real input counts only in
			// message_delta, and dropping the delta's input side meters them at 0.
			//
			// The initial output_tokens here is a placeholder (≈1), so drop it: it is
			// not an echo of anything and MergeEcho would add it to the real count.
			u := f.Message.Usage.norm()
			u.OutputTokens = 0
			return StreamEvent{Type: SEMessageStart, Usage: u}, true, nil
		}
	case "content_block_start":
		if f.ContentBlock.Type == "tool_use" {
			return StreamEvent{Type: SEToolUseStart, ToolID: f.ContentBlock.ID, ToolName: f.ContentBlock.Name, ToolIndex: f.Index}, true, nil
		}
		if f.ContentBlock.Type == "redacted_thinking" {
			return StreamEvent{Type: SERedactedThinking, Text: f.ContentBlock.Data}, true, nil
		}
	case "content_block_delta":
		switch f.Delta.Type {
		case "text_delta":
			return StreamEvent{Type: SETextDelta, Text: f.Delta.Text}, true, nil
		case "thinking_delta":
			return StreamEvent{Type: SEThinkingDelta, Text: f.Delta.Thinking}, true, nil
		case "signature_delta":
			return StreamEvent{Type: SEThinkingSignature, Text: f.Delta.Signature}, true, nil
		case "input_json_delta":
			return StreamEvent{Type: SEToolInputJSON, Text: f.Delta.PartialJSON, ToolIndex: f.Index}, true, nil
		}
	case "content_block_stop":
		return StreamEvent{Type: SEToolUseStop, ToolIndex: f.Index}, true, nil
	case "message_delta":
		ev := StreamEvent{Type: SEMessageDelta, StopReason: f.Delta.StopReason}
		if f.Usage != nil {
			// Output plus the input side. Real Anthropic re-echoes the input counts
			// from message_start (the accumulator deduplicates echoed fields with
			// max, so nothing doubles); Anthropic-compatible emulations that report
			// zeros in message_start get their real input counts recovered here.
			ev.Usage = f.Usage.norm()
		}
		return ev, true, nil
	case "message_stop":
		return StreamEvent{Type: SEMessageStop}, true, nil
	case "error":
		msg := "stream error"
		if f.Error != nil {
			msg = f.Error.Message
		}
		return StreamEvent{}, false, fmt.Errorf("anthropic: %s", msg)
	}
	return StreamEvent{}, false, nil
}

type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// norm maps Anthropic usage to the canonical Usage. InputTokens is the TOTAL input
// INCLUDING cached tokens (OpenAI-style: prompt_tokens semantics) — Anthropic's raw
// input_tokens counts only the non-cached portion, so cache read+creation are folded
// in. CacheReadTokens/CacheWriteTokens are the cached SUBSET (⊆ InputTokens).
func (u anthropicUsage) norm() Usage {
	return Usage{
		InputTokens:      u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadInputTokens,
		CacheWriteTokens: u.CacheCreationInputTokens,
	}
}

// filterThinkingBlocks normalizes message history before it is sent to the
// Anthropic Messages API. It strips thinking-type content blocks when thinking
// is disabled (Anthropic rejects replayed thinking blocks without the thinking
// parameter enabled), and drops any message left with no content blocks —
// Anthropic rejects an empty content array. An empty message arrives here two
// ways: a thinking-only turn that collapses to empty once thinking is stripped,
// or an empty/truncated assistant turn (only reasoning, or nothing) that was
// recorded with no text or tool_use. (A thinking-only turn WITH thinking
// enabled keeps its thinking block — that is valid content and must be
// replayed.)
//
// It also clears ContentBlock.ThoughtSignature, which belongs to Gemini's
// OpenAI-compatible endpoint. anthropicReq.Messages is []Message, so
// ContentBlock is marshaled straight onto the Anthropic wire and any populated
// field becomes a key inside a tool_use block — Anthropic answers an unknown key
// with "Extra inputs are not permitted". That history is reachable: transcripts
// persist llm.Message verbatim, so a session recorded against Gemini and then
// resumed (or summarized, or handed to a subagent) with an Anthropic provider
// would fail on every turn.
func filterThinkingBlocks(msgs []Message, thinkingEnabled bool) []Message {
	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		content := m.Content
		if !thinkingEnabled {
			filtered := make([]ContentBlock, 0, len(content))
			for _, b := range content {
				if b.Type != BlockThinking && b.Type != BlockRedactedThinking {
					filtered = append(filtered, b)
				}
			}
			content = filtered
		}
		if len(content) == 0 {
			continue // empty content array is rejected by Anthropic — drop it
		}
		content = stripThoughtSignatures(content)
		if len(content) == len(m.Content) && sameBacking(content, m.Content) {
			out = append(out, m)
		} else {
			out = append(out, Message{Role: m.Role, Content: content})
		}
	}
	return out
}

// stripThoughtSignatures clears ThoughtSignature on every block, copying only
// when at least one is set so untouched history keeps sharing its backing array.
// Callers own the returned slice; the input is never mutated — it belongs to the
// caller's conversation state and must survive the request unchanged.
func stripThoughtSignatures(content []ContentBlock) []ContentBlock {
	idx := -1
	for i, b := range content {
		if b.ThoughtSignature != "" {
			idx = i
			break
		}
	}
	if idx < 0 {
		return content
	}
	out := make([]ContentBlock, len(content))
	copy(out, content)
	for i := idx; i < len(out); i++ {
		out[i].ThoughtSignature = ""
	}
	return out
}

// sameBacking reports whether two slices share their first element's address,
// i.e. whether a normalization step above returned the input untouched.
func sameBacking(a, b []ContentBlock) bool {
	return len(a) == 0 || len(b) == 0 || &a[0] == &b[0]
}

// mergeAdjacentSameRole coalesces consecutive same-role messages into one by
// concatenating their content blocks. Anthropic requires messages to alternate
// user/assistant; dropping an empty turn in filterThinkingBlocks can leave two
// adjacent user messages (e.g. a tool result followed by a resume nudge, once
// the empty assistant between them is gone), which the API would reject with
// "roles must alternate". Merging preserves alternation without losing content.
// A fresh content slice is built on merge so the caller's message backing
// arrays are never mutated.
func mergeAdjacentSameRole(msgs []Message) []Message {
	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		if n := len(out); n > 0 && out[n-1].Role == m.Role {
			merged := make([]ContentBlock, 0, len(out[n-1].Content)+len(m.Content))
			merged = append(merged, out[n-1].Content...)
			merged = append(merged, m.Content...)
			out[n-1].Content = merged
			continue
		}
		out = append(out, m)
	}
	return out
}
