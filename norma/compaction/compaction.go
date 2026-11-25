// Package compaction keeps a conversation within a model's context window using
// three mechanisms aligned 1:1 with claude-code (see docs/COMPACTION-SPEC-3.md):
//
//   - MicroCompact — clear stale COMPACTABLE tool-result bodies (non-destructive:
//     the result can be re-read). Keeps the most recent N compactable results and
//     clears the rest.
//   - AutoCompact  — summarize the working head behind a compact boundary at the
//     token threshold (or predictively). Non-destructive: the full history is
//     retained in the message array; only messages after the last boundary are
//     sent to the model (llm.MessagesForAPI).
//   - Reactive     — on a prompt-too-long (413) error, force one AutoCompact and
//     retry. It reuses AutoCompact; it is not a separate algorithm.
//
// Snip and Context-Collapse are intentionally absent: snip's removal decision can
// only come from a model tool / user command (no automatic trigger), and
// context-collapse is disabled by default in claude-code. Thresholds are derived
// from the model's context window (minus a summary reservation and a buffer),
// with a predictive pre-request check. Token counts prefer the previous model
// response's input_tokens and otherwise fall back to a 4/3-scaled per-block
// length estimate.
package compaction

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"

	"github.com/Autumn-27/norma/llm"
)

// Summarizer condenses messages into a digest. Injected so the host controls
// which model summarizes (and so tests can mock).
type Summarizer func(ctx context.Context, msgs []llm.Message) (string, error)

// Config tunes the compactor.
type Config struct {
	// ContextWindow is the model's total context window in tokens (default 200000).
	ContextWindow int
	// MaxOutputTokens is the model's max output capability; the summary reservation
	// is min(MaxOutputTokens, 20000) (default 8000).
	MaxOutputTokens int
	// KeepRecent is the number of recent messages AutoCompact always preserves as
	// the un-summarized tail (default 8). MicroCompact uses its own microKeepRecent.
	KeepRecent int
	// MicroThresholdRatio overrides the 0.60 effective-window ratio for MicroCompact
	// when in (0, 0.95).
	MicroThresholdRatio float64
	// MicroKeepRecent overrides the number of recent compactable tool results kept
	// verbatim by MicroCompact (<=0 uses default 5).
	MicroKeepRecent int
	// MicroBatchInterval, when > 1, defers MicroCompact mutation until at least N
	// newly clearable results accumulate, preserving prompt-cache prefixes between batches.
	MicroBatchInterval int
	// ExtraCompactableTools registers additional tool names eligible for MicroCompact.
	ExtraCompactableTools []string
	// CustomSummaryPrompt optionally overrides the default AutoCompact summary prompt.
	CustomSummaryPrompt string
	// SummaryMaxTokens optionally caps the summary output tokens and summary reservation.
	SummaryMaxTokens int
	// SpeculativePreCompact, when true, starts AutoCompact summarization in the
	// background at 80% of autoThreshold so AutoCompact splices with zero stall.
	SpeculativePreCompact bool
}

func (c *Config) defaults() {
	if c.ContextWindow == 0 {
		c.ContextWindow = 200000
	}
	if c.MaxOutputTokens == 0 {
		c.MaxOutputTokens = 8000
	}
	if c.KeepRecent == 0 {
		c.KeepRecent = 8
	}
}

// compactableTools — tools whose results MicroCompact may clear. Mirrors
// claude-code COMPACTABLE_TOOLS (file read, shell, grep/glob, web, edit/write);
// deliberately excludes LS/MultiEdit/NotebookEdit.
var compactableTools = map[string]bool{
	"Read": true, "Bash": true, "Grep": true, "Glob": true,
	"WebFetch": true, "WebSearch": true, "Edit": true, "Write": true,
	// Actual wire names (the tools above use the built-in registry naming).
	"web_search": true, "SearchExtraTools": true, "shell_send": true,
}

const (
	microClearedMessage     = "[Old tool result content cleared]" // == claude-code TIME_BASED_MC_CLEARED_MESSAGE
	microClearedSpillPrefix = "[Old tool result content cleared; full output still saved at "
	microKeepRecent         = 5     // == claude-code KEEP_RECENT (compactable results kept)
	toolResultGrowthGuess   = 15000 // == claude-code TOOL_RESULT_GROWTH_ESTIMATE
	maxAutoCompactFails     = 3     // == claude-code MAX_CONSECUTIVE_AUTOCOMPACT_FAILURES
)

type speculativeJob struct {
	done     chan struct{}
	cancel   context.CancelFunc
	start    int
	tail     int
	sumCount int
	summary  string
	err      error
}

// Compactor applies the strategy. Construct with New.
type Compactor struct {
	cfg         Config
	summarize   Summarizer
	compactable map[string]bool
	failures    int
	tripped     bool

	specMu sync.Mutex
	spec   *speculativeJob
}

// New builds a Compactor. summarize may be nil to disable AutoCompact.
func New(cfg Config, summarize Summarizer) *Compactor {
	cfg.defaults()
	comp := make(map[string]bool, len(compactableTools)+len(cfg.ExtraCompactableTools))
	for k, v := range compactableTools {
		comp[k] = v
	}
	for _, name := range cfg.ExtraCompactableTools {
		if name != "" {
			comp[name] = true
		}
	}
	return &Compactor{cfg: cfg, summarize: summarize, compactable: comp}
}

func (c *Compactor) isCompactableTool(name string) bool {
	if c.compactable != nil {
		return c.compactable[name]
	}
	return compactableTools[name]
}

// isCompactableUse resolves the effective tool name for a tool_use block:
// deferred built-ins and MCP tools invoked through ExecuteExtraTool report
// their inner tool_name, and MCP results (mcp__server__tool) are always
// re-fetchable read payloads, so they are compactable by construction.
func (c *Compactor) isCompactableUse(b llm.ContentBlock) bool {
	name := b.Name
	if name == "ExecuteExtraTool" {
		var in struct {
			ToolName string `json:"tool_name"`
		}
		if err := json.Unmarshal(b.Input, &in); err != nil || in.ToolName == "" {
			return false
		}
		name = in.ToolName
	}
	return c.isCompactableTool(name) || strings.HasPrefix(name, "mcp__")
}

// --- threshold math ---

// reservedForSummary is min(model max output, 20000). MaxOutputTokens stands in
// for the model's output capability; 0 (unset) reserves the full 20000.
// When SummaryMaxTokens > 0 is explicitly configured, it overrides MaxOutputTokens
// for the summary reservation (still capped at 20000).
func (c *Compactor) reservedForSummary() int {
	r := c.cfg.MaxOutputTokens
	if c.cfg.SummaryMaxTokens > 0 {
		r = c.cfg.SummaryMaxTokens
	}
	if r > 20000 || r == 0 {
		r = 20000
	}
	return r
}
func (c *Compactor) effectiveWindow() int { return c.cfg.ContextWindow - c.reservedForSummary() }

// buffer tiers off the EFFECTIVE window (== claude-code getAutocompactBufferTokens).
func (c *Compactor) buffer() int {
	switch ew := c.effectiveWindow(); {
	case ew >= 800000:
		return 50000
	case ew >= 400000:
		return 30000
	default:
		return 13000
	}
}
func (c *Compactor) autoThreshold() int { return c.effectiveWindow() - c.buffer() }
func (c *Compactor) microThreshold() int {
	if c.cfg.MicroThresholdRatio > 0 && c.cfg.MicroThresholdRatio < 0.95 {
		return int(float64(c.effectiveWindow()) * c.cfg.MicroThresholdRatio)
	}
	return c.effectiveWindow() * 6 / 10
}

// maxTurnGrowth == claude-code estimateMaxTurnGrowth: min(maxOutput,20000)+15000.
func (c *Compactor) maxTurnGrowth() int { return c.reservedForSummary() + toolResultGrowthGuess }
func (c *Compactor) predictiveOver(tok int) bool {
	return tok+c.maxTurnGrowth() > c.effectiveWindow()
}

// count returns the token count of the API view (post-boundary). When
// lastInputTokens > 0 (the total token size the previous model response
// reported), it is preferred over local estimation — mirroring claude-code's
// tokenCountWithEstimation. After a working-set mutation, callers pass 0 to force
// a fresh per-block estimate.
func (c *Compactor) count(msgs []llm.Message, lastInputTokens int) int {
	if lastInputTokens > 0 {
		return lastInputTokens
	}
	return EstimateTokens(llm.MessagesForAPI(msgs)) * 4 / 3
}

// EstimateTokens approximates the token footprint of messages (~4 chars/token),
// counting each block by type (text, thinking text, tool_use name+input, and
// nested tool_result content). Mirrors claude-code roughTokenCountEstimation
// (without the 4/3 scale, which count applies).
func EstimateTokens(msgs []llm.Message) int {
	chars := 0
	for _, m := range msgs {
		for _, b := range m.Content {
			chars += blockChars(b)
		}
	}
	return chars / 4
}

// blockChars is the per-block character footprint. A Norma ContentBlock has no
// image/document type; if one is added, its fixed 2000-token cost (claude-code
// IMAGE_MAX_TOKEN_SIZE) would be added here.
func blockChars(b llm.ContentBlock) int {
	switch b.Type {
	case llm.BlockText:
		return len(b.Text)
	case llm.BlockThinking:
		return len(b.Thinking)
	case llm.BlockToolUse:
		return len(b.Name) + len(b.Input)
	case llm.BlockToolResult:
		n := 0
		for _, cb := range b.Content {
			n += blockChars(cb)
		}
		return n
	default:
		return len(b.Text)
	}
}

// Pre conditions the history before a model request: MicroCompact to clear stale
// tool results, then AutoCompact at the threshold (or predictively). Both are
// non-destructive. lastInputTokens is the previous response's total token size
// (0 if none yet), used as the initial gate count.
func (c *Compactor) Pre(ctx context.Context, msgs []llm.Message, lastInputTokens int) []llm.Message {
	if c.cfg.ContextWindow <= 0 {
		return msgs
	}
	tok := c.count(msgs, lastInputTokens)
	if tok <= c.microThreshold() {
		return msgs
	}
	// MicroCompact: clear old COMPACTABLE tool results (non-destructive). Only
	// re-estimate when it actually changed the array; otherwise the gate count
	// (which prefers the accurate last-response size) still holds.
	if out, changed := c.microCompact(msgs); changed {
		msgs = out
		tok = c.count(msgs, 0)
	}
	// AutoCompact: at the threshold, or predictively if the next turn would overflow.
	if tok >= c.autoThreshold() || c.predictiveOver(tok) {
		// If MicroBatchInterval deferred clearing recent batches, flush them before
		// triggering a full AutoCompact LLM summary call.
		if out, changed := c.microCompactWithFlush(msgs, true); changed {
			msgs = out
			tok = c.count(msgs, 0)
		}
		if tok >= c.autoThreshold() || c.predictiveOver(tok) {
			if out, ok := c.autoCompact(ctx, msgs, tok, "auto"); ok {
				msgs = out
			}
		}
	} else if c.cfg.SpeculativePreCompact && !c.tripped && c.summarize != nil {
		// 0.90 (not 0.80): speculative summarization is a full-context LLM
		// call — starting it at 80% burned a mostly-discarded summary on runs
		// that finished before ever reaching autoThreshold.
		if tok >= int(float64(c.autoThreshold())*0.90) {
			c.maybeStartSpeculative(ctx, msgs)
		}
	}
	return msgs
}

func (c *Compactor) maybeStartSpeculative(ctx context.Context, msgs []llm.Message) {
	start, tailStart := c.workingBounds(msgs)
	if tailStart <= start {
		return
	}
	snap := contentOnly(msgs[start:tailStart])
	if len(snap) == 0 {
		return
	}
	c.specMu.Lock()
	if c.spec != nil && c.spec.start == start {
		// Already running or ready for the current epoch.
		c.specMu.Unlock()
		return
	}
	sctx, cancel := context.WithCancel(ctx)
	job := &speculativeJob{
		done:     make(chan struct{}),
		cancel:   cancel,
		start:    start,
		tail:     tailStart,
		sumCount: len(snap),
	}
	c.spec = job
	c.specMu.Unlock()

	go func() {
		defer close(job.done)
		defer cancel()
		summary, err := c.summarize(sctx, snap)
		job.summary = summary
		job.err = err
	}()
}

// CancelSpeculative aborts any in-flight speculative pre-compaction summary
// and drops any cached speculative job.
func (c *Compactor) CancelSpeculative() {
	c.specMu.Lock()
	job := c.spec
	c.spec = nil
	c.specMu.Unlock()
	if job != nil && job.cancel != nil {
		job.cancel()
	}
}

func (c *Compactor) tryTakeSpeculative(ctx context.Context, start, tailStart int) (summary string, chosenTail, sumCount int, ok bool) {
	c.specMu.Lock()
	job := c.spec
	if job == nil || job.start != start || job.tail <= start || job.tail > tailStart || (tailStart-job.tail) > 12 {
		if job != nil {
			c.spec = nil
		}
		c.specMu.Unlock()
		if job != nil && job.cancel != nil {
			job.cancel()
		}
		return "", 0, 0, false
	}
	// Consume or await this speculative job once.
	c.spec = nil
	c.specMu.Unlock()

	select {
	case <-job.done:
		if job.err != nil || strings.TrimSpace(job.summary) == "" {
			return "", 0, 0, false
		}
		return job.summary, job.tail, job.sumCount, true
	case <-ctx.Done():
		return "", 0, 0, false
	}
}

// Reactive force-compacts after a prompt-too-long error by running one
// AutoCompact (mirrors claude-code tryReactiveCompact, which only summarizes —
// no micro/collapse/snip cascade). Returns whether the API view shrank.
func (c *Compactor) Reactive(ctx context.Context, msgs []llm.Message) ([]llm.Message, bool) {
	before := c.count(msgs, 0)
	if out, ok := c.autoCompact(ctx, msgs, before, "reactive"); ok {
		return out, c.count(out, 0) < before
	}
	return msgs, false
}

// IsOverflow is the method form (satisfies the harness Compactor interface).
func (c *Compactor) IsOverflow(err error) bool { return IsOverflow(err) }

// IsOverflow reports whether err is a context-length / prompt-too-long failure.
func IsOverflow(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "too long") || strings.Contains(s, "context length") ||
		strings.Contains(s, "context_length") || strings.Contains(s, "maximum context") ||
		strings.Contains(s, "status 413")
}

// --- working-set geometry ---

// workingBounds returns [start, tailStart): start is just after the last
// boundary marker (0 if none); tailStart is where the preserved recent tail
// begins. Messages in [start, tailStart) are eligible for summary.
func (c *Compactor) workingBounds(msgs []llm.Message) (start, tailStart int) {
	start = llm.LastBoundaryIndex(msgs) + 1
	tailStart = max(lastNNonMarkerStart(msgs, c.cfg.KeepRecent), start)
	return
}

func lastNNonMarkerStart(msgs []llm.Message, keep int) int {
	count, i := 0, len(msgs)
	for i > 0 && count < keep {
		i--
		if !llm.IsBoundaryMarker(msgs[i]) {
			count++
		}
	}
	return i
}

// --- MicroCompact: keep the most recent N compactable results, clear the rest ---

// microCompact clears the bodies of all COMPACTABLE tool results except the most
// recent keep-window, returning the (possibly new) message slice and whether
// anything changed. It is non-destructive: a cleared result can be re-read.
func (c *Compactor) microCompact(msgs []llm.Message) ([]llm.Message, bool) {
	return c.microCompactWithFlush(msgs, false)
}

func (c *Compactor) microCompactWithFlush(msgs []llm.Message, forceFlush bool) ([]llm.Message, bool) {
	start := llm.LastBoundaryIndex(msgs) + 1
	// Collect compactable tool_use ids in encounter order from the active working set.
	var ids []string
	for _, m := range msgs[start:] {
		for _, b := range m.Content {
			if b.Type == llm.BlockToolUse && c.isCompactableUse(b) {
				ids = append(ids, b.ID)
			}
		}
	}
	keep := c.cfg.MicroKeepRecent
	if keep <= 0 {
		keep = microKeepRecent
	}
	if len(ids) <= keep {
		return msgs, false // nothing old enough to clear
	}
	clearSet := make(map[string]bool, len(ids)-keep)
	for _, id := range ids[:len(ids)-keep] {
		clearSet[id] = true
	}
	// Count newly-clearable results so batching can avoid invalidating the
	// prompt cache prefix every single turn.
	pendingNew := 0
	for i := start; i < len(msgs); i++ {
		for _, b := range msgs[i].Content {
			if b.Type == llm.BlockToolResult && clearSet[b.ToolUseID] && !isCleared(b.Content) {
				pendingNew++
			}
		}
	}
	if pendingNew == 0 {
		return msgs, false
	}
	if !forceFlush && c.cfg.MicroBatchInterval > 1 && pendingNew < c.cfg.MicroBatchInterval {
		return msgs, false
	}
	changed := false
	out := append([]llm.Message(nil), msgs...)
	for i := start; i < len(out); i++ {
		var nc []llm.ContentBlock
		for j, b := range out[i].Content {
			if b.Type == llm.BlockToolResult && clearSet[b.ToolUseID] && !isCleared(b.Content) {
				if nc == nil {
					nc = append([]llm.ContentBlock(nil), out[i].Content...)
				}
				replacement := microClearedMessage
				if path := extractPersistedPath(b.Content); path != "" {
					replacement = microClearedSpillPrefix + path + "]"
				}
				nc[j].Content = []llm.ContentBlock{llm.TextBlock(replacement)}
				changed = true
			}
		}
		if nc != nil {
			out[i].Content = nc
		}
	}
	if !changed {
		return msgs, false
	}
	return out, true
}

func isCleared(blocks []llm.ContentBlock) bool {
	if len(blocks) != 1 || blocks[0].Type != llm.BlockText {
		return false
	}
	t := blocks[0].Text
	return t == microClearedMessage || strings.HasPrefix(t, microClearedSpillPrefix)
}

func extractPersistedPath(blocks []llm.ContentBlock) string {
	const startMarker = "Full output saved to"
	const endMarker = "</persisted-output>"
	for _, blk := range blocks {
		if blk.Type != llm.BlockText {
			continue
		}
		i := strings.Index(blk.Text, startMarker)
		if i < 0 {
			continue
		}
		rest := blk.Text[i+len(startMarker):]
		j := strings.Index(rest, endMarker)
		if j < 0 {
			continue
		}
		if path := strings.TrimSpace(rest[:j]); path != "" {
			return path
		}
	}
	return ""
}

// --- AutoCompact: non-destructive boundary + summary ---

func (c *Compactor) autoCompact(ctx context.Context, msgs []llm.Message, preTok int, trigger string) ([]llm.Message, bool) {
	if c.summarize == nil || c.tripped {
		return msgs, false
	}
	start, tailStart := c.workingBounds(msgs)
	if tailStart <= start {
		return msgs, false
	}
	if toSummarize := contentOnly(msgs[start:tailStart]); len(toSummarize) == 0 {
		return msgs, false
	} else {
		var summary string
		sumCount := len(toSummarize)
		if specSum, specTail, specCount, ok := c.tryTakeSpeculative(ctx, start, tailStart); ok {
			summary = specSum
			tailStart = specTail
			sumCount = specCount
		} else {
			var err error
			summary, err = c.summarize(ctx, toSummarize)
			if err != nil || strings.TrimSpace(summary) == "" {
				c.failures++
				if c.failures >= maxAutoCompactFails {
					c.tripped = true
				}
				return msgs, false
			}
		}
		c.failures = 0

		reinjected := reinjectSkills(msgs, tailStart)
		boundary := llm.BoundaryMessage(llm.BoundaryMeta{
			Trigger:            trigger,
			PreTokens:          preTok,
			MessagesSummarized: sumCount,
			ActiveSkills:       skillNames(reinjected),
		})
		summaryMsg := llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{
			llm.TextBlock(compactContinuationPreamble + summary),
		}}
		out := make([]llm.Message, 0, len(msgs)+2+len(reinjected))
		out = append(out, msgs[:tailStart]...)
		out = append(out, boundary, summaryMsg)
		out = append(out, reinjected...)
		out = append(out, msgs[tailStart:]...)
		return out, true
	}
}

// compactContinuationPreamble prefixes the inserted summary message, mirroring
// claude-code getCompactUserSummaryMessage. formatCompactSummary already prefixes
// the digest with "Summary:\n".
const compactContinuationPreamble = "This session is being continued from a previous conversation that ran out of context. The conversation is summarized below:\n\n"

// Skill re-injection budget: per-skill and total token caps for verbatim skill
// instructions re-surfaced after a compaction boundary (mirrors claude-code's
// 5K/25K policy). Most-recently-invoked skills win when the total is exceeded.
const (
	maxSkillReinjectTokens      = 5000
	maxSkillReinjectTotalTokens = 25000
)

// skillNames returns the invoked-skill names of the given re-injected messages.
func skillNames(msgs []llm.Message) []string {
	var out []string
	for _, m := range msgs {
		if name, ok := llm.SkillInvocationName(m); ok {
			out = append(out, name)
		}
	}
	return out
}

// reinjectSkills finds the latest invocation of each skill in the history behind
// the new boundary (msgs[:tailStart]) and returns copies to place after the
// summary, newest-first under budget, restored to chronological order. A skill
// whose latest invocation is already in the preserved tail (index >= tailStart)
// is skipped — it is still in the model's view.
func reinjectSkills(msgs []llm.Message, tailStart int) []llm.Message {
	type entry struct {
		msg llm.Message
		idx int
	}
	latest := map[string]*entry{}
	var order []string
	for i, m := range msgs {
		name, ok := llm.SkillInvocationName(m)
		if !ok {
			continue
		}
		if e, seen := latest[name]; seen {
			e.msg, e.idx = m, i
		} else {
			latest[name] = &entry{msg: m, idx: i}
			order = append(order, name)
		}
	}
	if len(latest) == 0 {
		return nil
	}
	// Candidates whose latest invocation fell behind the boundary, newest-first.
	var cands []*entry
	for _, name := range order {
		if e := latest[name]; e.idx < tailStart {
			cands = append(cands, e)
		}
	}
	sort.Slice(cands, func(a, b int) bool { return cands[a].idx > cands[b].idx })

	kept := make([]*entry, 0, len(cands))
	total := 0
	for _, e := range cands {
		m := llm.TruncateSkillMessage(e.msg, maxSkillReinjectTokens*4)
		t := EstimateTokens([]llm.Message{m})
		if total+t > maxSkillReinjectTotalTokens {
			continue // drop this one; a smaller, older skill may still fit
		}
		total += t
		kept = append(kept, &entry{msg: m, idx: e.idx})
	}
	if len(kept) == 0 {
		return nil
	}
	sort.Slice(kept, func(a, b int) bool { return kept[a].idx < kept[b].idx })
	out := make([]llm.Message, 0, len(kept))
	for _, e := range kept {
		out = append(out, e.msg)
	}
	return out
}

func contentOnly(msgs []llm.Message) []llm.Message {
	out := make([]llm.Message, 0, len(msgs))
	for _, m := range msgs {
		if !llm.IsBoundaryMarker(m) {
			out = append(out, m)
		}
	}
	return out
}
