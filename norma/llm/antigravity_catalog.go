package llm

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// AntigravityCatalogEntry describes one model available through Antigravity CLI (`agy acp`).
type AntigravityCatalogEntry struct {
	ID              string   `json:"id"`
	DisplayName     string   `json:"display_name"`
	ContextWindow   int      `json:"context_window"`
	MaxOutputTokens int      `json:"max_output_tokens"`
	ReasoningLevels []string `json:"reasoning_levels"`
}

var antigravityBuiltinModels = []AntigravityCatalogEntry{
	{
		ID:              "gemini-4-carbon",
		DisplayName:     "Gemini 4 Carbon",
		ContextWindow:   1_000_000,
		MaxOutputTokens: 65_536,
		ReasoningLevels: []string{"low", "medium", "high", "xhigh"},
	},
	{
		ID:              "gemini-4-neon",
		DisplayName:     "Gemini 4 Neon",
		ContextWindow:   1_000_000,
		MaxOutputTokens: 65_536,
		ReasoningLevels: []string{"low", "medium", "high", "xhigh"},
	},
	{
		ID:              "gemini-3.8-flash",
		DisplayName:     "Gemini 3.8 Flash",
		ContextWindow:   1_000_000,
		MaxOutputTokens: 65_536,
		ReasoningLevels: []string{"low", "medium", "high"},
	},
	{
		ID:              "claude-opus-4-6",
		DisplayName:     "Claude Opus 4.6 (Antigravity)",
		ContextWindow:   200_000,
		MaxOutputTokens: 64_000,
		ReasoningLevels: []string{"low", "medium", "high"},
	},
	{
		ID:              "claude-sonnet-4-6",
		DisplayName:     "Claude Sonnet 4.6 (Antigravity)",
		ContextWindow:   200_000,
		MaxOutputTokens: 64_000,
		ReasoningLevels: []string{"low", "medium", "high"},
	},
}

// AntigravityFallbackCatalog returns the built-in Antigravity CLI model roster.
func AntigravityFallbackCatalog() []AntigravityCatalogEntry {
	out := make([]AntigravityCatalogEntry, len(antigravityBuiltinModels))
	for i, m := range antigravityBuiltinModels {
		m.ReasoningLevels = append([]string(nil), m.ReasoningLevels...)
		out[i] = m
	}
	return out
}

func antigravityBuiltinModel(id string) (AntigravityCatalogEntry, bool) {
	for _, m := range antigravityBuiltinModels {
		if m.ID == id {
			m.ReasoningLevels = append([]string(nil), m.ReasoningLevels...)
			return m, true
		}
	}
	return AntigravityCatalogEntry{}, false
}

// AgyModelValue resolves the model value to pass to `session/set_config_option`:
// keeps the bare ID when advertised, otherwise falls back to `<model>-<effort>`
// for effort-suffixed rosters.
func AgyModelValue(modelID, effort string, advertised []string) string {
	if len(advertised) == 0 {
		return modelID
	}
	has := func(target string) bool {
		for _, m := range advertised {
			if m == target {
				return true
			}
		}
		return false
	}
	if has(modelID) {
		return modelID
	}
	if effort != "" {
		if suffixed := modelID + "-" + effort; has(suffixed) {
			return suffixed
		}
	}
	for _, def := range []string{"high", "medium", "low"} {
		if suffixed := modelID + "-" + def; has(suffixed) {
			return suffixed
		}
	}
	return modelID
}

// CollapseAdvertisedAgyModels collapses effort-suffixed `agy acp` model IDs into
// logical catalog entries with their supported reasoning levels.
func CollapseAdvertisedAgyModels(advertised []string) []AntigravityCatalogEntry {
	out := make([]AntigravityCatalogEntry, 0, len(advertised))
	index := make(map[string]int, len(advertised))
	for _, raw := range advertised {
		if strings.HasSuffix(raw, "-disabled") || strings.Contains(raw, "orchestrator") {
			continue
		}
		base, effort := splitAgyEffortSuffix(raw)
		if pos, exists := index[base]; exists {
			if effort != "" && !containsString(out[pos].ReasoningLevels, effort) {
				out[pos].ReasoningLevels = append(out[pos].ReasoningLevels, effort)
			}
			continue
		}
		known, hasKnown := antigravityBuiltinModel(base)
		var levels []string
		if effort != "" {
			levels = []string{effort}
		} else if hasKnown {
			levels = append([]string(nil), known.ReasoningLevels...)
		} else {
			levels = []string{"low", "medium", "high"}
		}
		if len(levels) == 0 {
			levels = []string{"medium"}
		}
		entry := AntigravityCatalogEntry{
			ID:              base,
			DisplayName:     prettifyAgyModelID(base),
			ContextWindow:   200_000,
			MaxOutputTokens: 64_000,
			ReasoningLevels: levels,
		}
		if hasKnown {
			entry.DisplayName = known.DisplayName
			entry.ContextWindow = known.ContextWindow
			entry.MaxOutputTokens = known.MaxOutputTokens
		}
		index[base] = len(out)
		out = append(out, entry)
	}
	return out
}

func splitAgyEffortSuffix(id string) (base, effort string) {
	for _, suffix := range []string{"-xhigh", "-high", "-medium", "-low"} {
		if strings.HasSuffix(id, suffix) {
			return strings.TrimSuffix(id, suffix), suffix[1:]
		}
	}
	return id, ""
}

func prettifyAgyModelID(id string) string {
	parts := strings.Split(id, "-")
	for i, p := range parts {
		if p != "" {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, " ")
}

func containsString(slice []string, target string) bool {
	for _, s := range slice {
		if s == target {
			return true
		}
	}
	return false
}

// ProbeAntigravityModels spawns a temporary `agy acp` process, reads the
// `configOptions[id="model"]` list from `session/new`, and terminates it cleanly.
func ProbeAntigravityModels(ctx context.Context, client *http.Client, cred AgyCredential) ([]AntigravityCatalogEntry, error) {
	binary, err := ResolveAgyBinary(ctx, client)
	if err != nil {
		return nil, err
	}
	accountKey := AgyCredentialKey(cred)
	geminiDir := filepath.Join(AgyGeminiDir(), accountKey)
	if err := StageAgyCredentials(cred, geminiDir); err != nil {
		return nil, err
	}
	if err := writeAgyBridgeAgent(geminiDir); err != nil {
		return nil, err
	}
	cwd, err := os.Getwd()
	if err != nil || cwd == "" {
		cwd = geminiDir
	}
	proc, err := spawnAgyProcess(ctx, binary, geminiDir, cwd, "", nil, "", 0, AgyCredentialVersion(cred))
	if err != nil {
		return nil, err
	}
	advertised := append([]string(nil), proc.advertisedModels...)
	proc.Kill()
	return CollapseAdvertisedAgyModels(advertised), nil
}
