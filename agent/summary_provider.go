package agent

import (
	"log"
	"os"
	"strings"
	"sync"

	"github.com/Autumn-27/norma/llm"
)

// summaryProv memoizes the resolved provider (keyed by the env tuple that
// determines it) so every side-question / task-agent build / goal
// decomposition shares one warm HTTP/2 connection pool instead of paying cold
// TCP+TLS+HTTP2 handshakes per call — while an env change still rebuilds it.
var summaryProvMu sync.Mutex
var summaryProv struct {
	key   string
	p     llm.Provider
	model string
	ok    bool
}

func summaryEnvKey() string {
	parts := make([]string, 0, 9)
	for _, k := range []string{
		"ARTEX_SUMMARY_LLM_MODEL", "ARTEX_SUMMARY_LLM_PROVIDER", "ARTEX_LLM_PROVIDER",
		"ARTEX_SUMMARY_LLM_API_KEY", "ANTHROPIC_API_KEY", "OPENAI_API_KEY",
		"ARTEX_SUMMARY_LLM_BASE_URL", "ARTEX_SUMMARY_LLM_PROXY", "ARTEX_LLM_PROXY",
	} {
		parts = append(parts, os.Getenv(k))
	}
	return strings.Join(parts, "\x00")
}

// SummaryProviderFromEnv resolves an optional dedicated (typically cheaper/faster)
// LLM provider used exclusively for AutoCompact summarization and cold-digest
// graph compaction.
//
// Environment variables:
//
//	ARTEX_SUMMARY_LLM_MODEL    (required to enable; empty = use task's normal model)
//	ARTEX_SUMMARY_LLM_PROVIDER (anthropic|openai|openai-responses|ollama; default: ARTEX_LLM_PROVIDER or inferred)
//	ARTEX_SUMMARY_LLM_BASE_URL (optional; defaults to provider endpoint)
//	ARTEX_SUMMARY_LLM_API_KEY  (optional; falls back to ANTHROPIC_API_KEY / OPENAI_API_KEY)
//	ARTEX_SUMMARY_LLM_PROXY    (optional; falls back to ARTEX_LLM_PROXY)
func SummaryProviderFromEnv() (llm.Provider, string, bool) {
	key := summaryEnvKey()
	summaryProvMu.Lock()
	defer summaryProvMu.Unlock()
	if summaryProv.key == key {
		return summaryProv.p, summaryProv.model, summaryProv.ok
	}
	p, model, ok := resolveSummaryProvider()
	summaryProv = struct {
		key   string
		p     llm.Provider
		model string
		ok    bool
	}{key, p, model, ok}
	return p, model, ok
}

func resolveSummaryProvider() (llm.Provider, string, bool) {
	model := strings.TrimSpace(os.Getenv("ARTEX_SUMMARY_LLM_MODEL"))
	if model == "" {
		return nil, "", false
	}
	prov := strings.ToLower(strings.TrimSpace(os.Getenv("ARTEX_SUMMARY_LLM_PROVIDER")))
	if prov == "" {
		prov = strings.ToLower(strings.TrimSpace(os.Getenv("ARTEX_LLM_PROVIDER")))
	}
	apiKey := strings.TrimSpace(os.Getenv("ARTEX_SUMMARY_LLM_API_KEY"))
	if prov == "" {
		switch {
		case apiKey != "" && os.Getenv("OPENAI_API_KEY") != "" && os.Getenv("ANTHROPIC_API_KEY") == "":
			prov = "openai"
		case os.Getenv("ANTHROPIC_API_KEY") != "":
			prov = "anthropic"
		case os.Getenv("OPENAI_API_KEY") != "":
			prov = "openai"
		default:
			prov = "anthropic"
		}
	}
	if apiKey == "" {
		switch prov {
		case "openai", "openai-responses":
			apiKey = strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
		case "ollama":
			// keyless
		default:
			apiKey = strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY"))
		}
	}
	baseURL := strings.TrimSpace(os.Getenv("ARTEX_SUMMARY_LLM_BASE_URL"))
	proxy := strings.TrimSpace(os.Getenv("ARTEX_SUMMARY_LLM_PROXY"))
	if proxy == "" {
		proxy = strings.TrimSpace(os.Getenv("ARTEX_LLM_PROXY"))
	}
	cfg := ConfigFrom(prov, model, baseURL, apiKey, proxy)
	cfg.ThinkingType = "disabled"
	cfg.Stream = false
	if cfg.APIKey == "" && cfg.NeedsAPIKey() {
		log.Printf("[summary-llm] ARTEX_SUMMARY_LLM_MODEL=%q configured without API key for provider %q; falling back to task provider", model, cfg.Provider())
		return nil, "", false
	}
	p, err := cfg.NewProvider()
	if err != nil {
		log.Printf("[summary-llm] failed to build summary provider (%s/%s): %v; falling back to task provider", cfg.Provider(), cfg.Model, err)
		return nil, "", false
	}
	return p, cfg.Model, true
}
