package agent

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
)

func TestMaxToolOutputCharsEnv(t *testing.T) {
	t.Setenv("ARTEX_MAX_TOOL_OUTPUT_CHARS", "")
	if got := maxToolOutputChars(); got != defaultMaxToolOutputChars {
		t.Fatalf("default=%d, want %d", got, defaultMaxToolOutputChars)
	}
	t.Setenv("ARTEX_MAX_TOOL_OUTPUT_CHARS", "12000")
	if got := maxToolOutputChars(); got != 12000 {
		t.Fatalf("custom=%d, want 12000", got)
	}
	t.Setenv("ARTEX_MAX_TOOL_OUTPUT_CHARS", "0")
	if got := maxToolOutputChars(); got != 0 {
		t.Fatalf("zero=%d, want 0 (fallback to SDK default)", got)
	}
}

func TestCompactionConfigOptimizations(t *testing.T) {
	t.Setenv("ARTEX_COMPACT_WINDOW_CAP_K", "80")
	t.Setenv("ARTEX_MICRO_COMPACT_RATIO", "0.30")
	t.Setenv("ARTEX_MICRO_COMPACT_BATCH", "5")

	cfg := compactionConfig(200000)
	if cfg.ContextWindow != 80000 {
		t.Fatalf("ContextWindow=%d, want capped 80000", cfg.ContextWindow)
	}
	if cfg.MicroThresholdRatio != 0.30 {
		t.Fatalf("MicroThresholdRatio=%v, want 0.30", cfg.MicroThresholdRatio)
	}
	if cfg.MicroBatchInterval != 5 {
		t.Fatalf("MicroBatchInterval=%d, want 5", cfg.MicroBatchInterval)
	}
	if cfg.SummaryMaxTokens != defaultSummaryMaxTokens {
		t.Fatalf("SummaryMaxTokens=%d, want %d", cfg.SummaryMaxTokens, defaultSummaryMaxTokens)
	}
	if !strings.Contains(cfg.CustomSummaryPrompt, "penetration-testing") {
		t.Fatalf("expected pentest summary prompt")
	}
	for _, tool := range []string{
		"traffic_get", "traffic_search", "traffic_blob", "traffic_messages",
		"list_assets", "graph_overview", "node_detail",
		"list_worker_traces", "search_all_worker_traces", "TaskOutput", "shell_read",
	} {
		if !slices.Contains(cfg.ExtraCompactableTools, tool) {
			t.Errorf("ExtraCompactableTools missing %q", tool)
		}
	}
	for _, bogus := range []string{"traffic_query", "search_worker_traces"} {
		if slices.Contains(cfg.ExtraCompactableTools, bogus) {
			t.Errorf("ExtraCompactableTools should not contain stale tool name %q", bogus)
		}
	}
}

func TestWorkerCompactOverviewToggle(t *testing.T) {
	mk := func() map[string]any {
		open := make([]map[string]any, 25)
		done := make([]map[string]any, 12)
		facts := make([]map[string]any, 20)
		for i := range open {
			open[i] = map[string]any{"id": i + 1, "summary": fmt.Sprintf("open-%d", i+1)}
		}
		for i := range done {
			done[i] = map[string]any{"id": i + 100, "summary": fmt.Sprintf("done-%d", i+1)}
		}
		for i := range facts {
			facts[i] = map[string]any{"id": i + 200, "summary": fmt.Sprintf("fact-%d", i+1)}
		}
		return map[string]any{
			"task":                map[string]any{"goal": "test"},
			"goals":               []map[string]any{},
			"hints":               []map[string]any{},
			"open_intents":        open,
			"recent_done_intents": done,
			"recent_facts":        facts,
			"related_tasks": []map[string]any{{
				"source_task_id":        9,
				"task":                  map[string]any{"goal": "prev"},
				"recent_intent_results": []map[string]any{{"id": 1, "result_summary": "HUGE_INTENT_RESULT"}},
			}},
		}
	}

	t.Setenv("ARTEX_WORKER_COMPACT_OVERVIEW", "1")
	compactOut := renderWorkerGraphOverview(mk(), newDataEnvelope())
	if strings.Contains(compactOut, "open-25") || strings.Contains(compactOut, "fact-20") || strings.Contains(compactOut, "HUGE_INTENT_RESULT") {
		t.Fatalf("compact worker overview did not trim excess items")
	}
	if !strings.Contains(compactOut, "open-1") || !strings.Contains(compactOut, "fact-1") {
		t.Fatalf("compact worker overview dropped top items")
	}

	t.Setenv("ARTEX_WORKER_COMPACT_OVERVIEW", "0")
	fullOut := renderWorkerGraphOverview(mk(), newDataEnvelope())
	if !strings.Contains(fullOut, "open-25") || !strings.Contains(fullOut, "fact-20") || !strings.Contains(fullOut, "HUGE_INTENT_RESULT") {
		t.Fatalf("full worker overview missing expected items when ARTEX_WORKER_COMPACT_OVERVIEW=0")
	}
}

func TestSummaryProviderFromEnv(t *testing.T) {
	t.Setenv("ARTEX_SUMMARY_LLM_MODEL", "")
	if p, _, ok := SummaryProviderFromEnv(); ok || p != nil {
		t.Fatalf("expected disabled when model unset")
	}

	t.Setenv("ARTEX_SUMMARY_LLM_PROVIDER", "anthropic")
	t.Setenv("ARTEX_SUMMARY_LLM_MODEL", "claude-3-5-haiku-latest")
	t.Setenv("ARTEX_SUMMARY_LLM_API_KEY", "sk-test-summary")
	p, model, ok := SummaryProviderFromEnv()
	if !ok || p == nil || model != "claude-3-5-haiku-latest" {
		t.Fatalf("expected summary provider, got ok=%v model=%q prov=%v", ok, model, p)
	}
}

func TestColdDigestThresholdsEnv(t *testing.T) {
	t.Setenv("ARTEX_COLD_DIGEST_MIN_NODES", "")
	t.Setenv("ARTEX_COLD_DIGEST_MAJOR_DIGESTS", "")
	c := NewCompactor(nil, "")
	if c.n != 12 || c.m != 6 {
		t.Fatalf("defaults n=%d m=%d, want 12/6", c.n, c.m)
	}

	t.Setenv("ARTEX_COLD_DIGEST_MIN_NODES", "8")
	t.Setenv("ARTEX_COLD_DIGEST_MAJOR_DIGESTS", "4")
	c2 := NewCompactor(nil, "")
	if c2.n != 8 || c2.m != 4 {
		t.Fatalf("overrides n=%d m=%d, want 8/4", c2.n, c2.m)
	}
}

func TestCachePrefixDefaultOnAndOptOut(t *testing.T) {
	t.Setenv(CachePrefixEnv, "")
	if !cachePrefixEnabled() {
		t.Fatal("expected ARTEX_CACHE_PREFIX enabled by default in V2")
	}
	t.Setenv(CachePrefixEnv, "0")
	if cachePrefixEnabled() {
		t.Fatal("expected ARTEX_CACHE_PREFIX=0 to disable split")
	}
}

func TestDeferBuiltinToolsAndPlannerOverviewFacts(t *testing.T) {
	tools := append(defaultTools(t.TempDir()),
		(&ToolSet{}).ingestBbot(),
		(&ToolSet{}).saveTaskCredential(),
	)
	def := DeferredInfo{Unlock: actool.NewUnlockSet("mcp__demo__scan")}
	got := deferBuiltinTools(tools, def, workerDeferredBuiltins)
	for _, want := range []string{"ingest_bbot", "save_task_credential", "sleep"} {
		if !slices.Contains(got.Deferred, want) || !slices.Contains(got.GlobalNames, want) {
			t.Fatalf("expected %q to be deferred and listed in GlobalNames: %+v", want, got)
		}
		if !got.Unlock.Has(want) {
			t.Fatalf("expected %q unlocked in UnlockSet", want)
		}
	}
	if slices.Contains(got.Deferred, "Bash") {
		t.Fatalf("core tool Bash should not be deferred: %v", got.Deferred)
	}

	t.Setenv("ARTEX_DEFER_BUILTIN_TOOLS", "0")
	off := deferBuiltinTools(tools, DeferredInfo{}, workerDeferredBuiltins)
	if len(off.Deferred) != 0 || len(off.GlobalNames) != 0 {
		t.Fatalf("expected ARTEX_DEFER_BUILTIN_TOOLS=0 to be no-op, got %+v", off)
	}

	if overviewHasFacts(nil) || overviewHasFacts(map[string]any{"facts": 0}) {
		t.Fatal("expected empty overview to report false for overviewHasFacts")
	}
	if !overviewHasFacts(map[string]any{"facts": 2}) || !overviewHasFacts(map[string]any{"recent_facts": []map[string]any{{"id": 1}}}) {
		t.Fatal("expected non-empty facts to report true for overviewHasFacts")
	}
}

func TestOnda1CompactionAndReasoningDefaults(t *testing.T) {
	t.Setenv("ARTEX_COMPACT_WINDOW_CAP_K", "")
	cfg := compactionConfig(200000)
	if cfg.ContextWindow != 160000 {
		t.Fatalf("default ContextWindow=%d, want 160000", cfg.ContextWindow)
	}
	t.Setenv("ARTEX_COMPACT_WINDOW_CAP_K", "0")
	cfgOff := compactionConfig(200000)
	if cfgOff.ContextWindow != 200000 {
		t.Fatalf("opt-out ContextWindow=%d, want 200000", cfgOff.ContextWindow)
	}

	t.Setenv("ARTEX_STRIP_OLD_REASONING", "")
	if !stripOldReasoningEnabled() {
		t.Fatal("expected ARTEX_STRIP_OLD_REASONING enabled by default")
	}
	t.Setenv("ARTEX_STRIP_OLD_REASONING", "0")
	if stripOldReasoningEnabled() {
		t.Fatal("expected ARTEX_STRIP_OLD_REASONING=0 to disable")
	}
}

func TestRelatedTasksBudgetCapAndOverride(t *testing.T) {
	t.Setenv("ARTEX_RELATED_TASKS_BUDGET_RUNES", "")
	if got := relatedOverviewTotalRunes(); got != 12_000 {
		t.Fatalf("default total runes=%d, want 12000", got)
	}
	if got := relatedOverviewBudgetForSources(3); got != 4_000 {
		t.Fatalf("3-source budget=%d, want 4000", got)
	}
	t.Setenv("ARTEX_RELATED_TASKS_BUDGET_RUNES", "24000")
	if got := relatedOverviewBudgetForSources(3); got != 8_000 {
		t.Fatalf("overridden 3-source budget=%d, want 8000", got)
	}
}
