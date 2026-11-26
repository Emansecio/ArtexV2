package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Autumn-27/norma/llm"
)

const (
	defaultModelsDevURL      = "https://models.dev/api.json"
	modelsDevCacheTTL        = 24 * time.Hour
	modelsDevRetryBackoff    = 5 * time.Minute
	modelsDevMaxBytes        = 16 << 20 // 16 MiB hard cap
	providerCatalogMaxBytes  = 4 << 20  // 4 MiB hard cap
	diskCatalogStaleAfter    = 10 * time.Minute
)

var catalogTmpNonce atomic.Uint64

// modelsDevEntry is our compact normalized projection of one models.dev entry.
type modelsDevEntry struct {
	ID                     string   `json:"id"`
	Name                   string   `json:"name,omitempty"`
	ContextWindow          int      `json:"context_window,omitempty"`
	MaxOutputTokens        int      `json:"max_output_tokens,omitempty"`
	Reasoning              bool     `json:"reasoning,omitempty"`
	ReasoningLevels        []string `json:"reasoning_levels,omitempty"`
	SupportsThinkingToggle bool     `json:"supports_thinking_toggle,omitempty"`
	SupportsTools          *bool    `json:"supports_tools,omitempty"`
	SupportsVision         *bool    `json:"supports_vision,omitempty"`
	ReleaseDate            string   `json:"release_date,omitempty"`
	PriceInputPerM         float64  `json:"price_input_per_m,omitempty"`
	PriceOutputPerM        float64  `json:"price_output_per_m,omitempty"`
	PriceCacheReadPerM     float64  `json:"price_cache_read_per_m,omitempty"`
	PriceCacheWritePerM    float64  `json:"price_cache_write_per_m,omitempty"`
}

type modelsDevSnapshot struct {
	Version    int                                  `json:"version"`
	FetchedAt  int64                                `json:"fetched_at"`
	ByProvider map[string]map[string]modelsDevEntry `json:"by_provider"`
	Global     map[string]modelsDevEntry            `json:"global"`
}

// modelRegistry maintains a RAM + disk-cached projection of models.dev so
// ArtexV2 automatically knows specs and pricing for newly released models
// without requiring a binary rebuild, while staying 100% functional offline.
type modelRegistry struct {
	cacheDir   string
	url        string
	httpClient *http.Client

	mu          sync.RWMutex
	snap        *modelsDevSnapshot
	diskLoaded  bool
	refreshing  bool
	lastAttempt time.Time
	doneCh      chan struct{}
}

func newModelRegistry(cacheDir, overrideURL string) *modelRegistry {
	u := strings.TrimSpace(overrideURL)
	if u == "" {
		u = defaultModelsDevURL
	}
	return &modelRegistry{
		cacheDir:   cacheDir,
		url:        u,
		httpClient: &http.Client{Timeout: 12 * time.Second},
	}
}

func (r *modelRegistry) disabled() bool {
	if r == nil {
		return true
	}
	// Never hit the public internet from unit tests unless an explicit test URL was injected.
	if r.url == defaultModelsDevURL && strings.EqualFold(strings.TrimSpace(os.Getenv("ARTEX_DISABLE_MODELS_DEV")), "1") {
		return true
	}
	return false
}

func (r *modelRegistry) cachePath() string {
	if strings.TrimSpace(r.cacheDir) == "" {
		return ""
	}
	return filepath.Join(r.cacheDir, "models-dev.json")
}

func (r *modelRegistry) ensureDiskLoadedLocked() {
	if r.diskLoaded {
		return
	}
	r.diskLoaded = true
	p := r.cachePath()
	if p == "" {
		return
	}
	info, err := os.Stat(p)
	if err != nil || info.IsDir() || info.Size() > modelsDevMaxBytes {
		return
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return
	}
	var snap modelsDevSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil || snap.Version != 1 || len(snap.Global) == 0 {
		return
	}
	r.snap = &snap
}

func (r *modelRegistry) isStaleLocked() bool {
	if r.snap == nil {
		return true
	}
	return time.Since(time.Unix(r.snap.FetchedAt, 0)) > modelsDevCacheTTL
}

// Refresh fetches models.dev with singleflight deduplication. When force is false,
// a fresh cached snapshot (within 24h) is returned immediately without network I/O.
func (r *modelRegistry) Refresh(ctx context.Context, force bool) bool {
	if r.disabled() {
		return false
	}
	r.mu.Lock()
	r.ensureDiskLoadedLocked()
	if !force && !r.isStaleLocked() {
		r.mu.Unlock()
		return true
	}
	if r.refreshing {
		ch := r.doneCh
		r.mu.Unlock()
		select {
		case <-ch:
			r.mu.RLock()
			ok := r.snap != nil
			r.mu.RUnlock()
			return ok
		case <-ctx.Done():
			return false
		}
	}
	if !force && !r.lastAttempt.IsZero() && time.Since(r.lastAttempt) < modelsDevRetryBackoff {
		ok := r.snap != nil
		r.mu.Unlock()
		return ok
	}
	r.refreshing = true
	r.lastAttempt = time.Now()
	ch := make(chan struct{})
	r.doneCh = ch
	url := r.url
	client := r.httpClient
	r.mu.Unlock()

	snap, err := fetchModelsDevSnapshot(ctx, client, url)

	r.mu.Lock()
	if err == nil && snap != nil && len(snap.Global) > 0 {
		r.snap = snap
		_ = writeJSONAtomic(r.cachePath(), snap)
	}
	r.refreshing = false
	close(ch)
	ok := r.snap != nil
	r.mu.Unlock()
	return ok
}

// TriggerBackgroundRefresh starts a non-blocking refresh when the cache is
// missing or older than 24h.
func (r *modelRegistry) TriggerBackgroundRefresh(parent context.Context) {
	if r.disabled() {
		return
	}
	r.mu.Lock()
	r.ensureDiskLoadedLocked()
	needs := r.isStaleLocked() && (r.lastAttempt.IsZero() || time.Since(r.lastAttempt) >= modelsDevRetryBackoff) && !r.refreshing
	r.mu.Unlock()
	if !needs {
		return
	}
	go func() {
		bgCtx := parent
		if bgCtx == nil {
			bgCtx = context.Background()
		}
		ctx, cancel := context.WithTimeout(bgCtx, 12*time.Second)
		defer cancel()
		r.Refresh(ctx, false)
	}()
}

// Lookup resolves a model from the RAM/disk models.dev snapshot without blocking
// on network I/O. Provider-specific matches win over cross-provider fallbacks.
func (r *modelRegistry) Lookup(provider, modelID string) (modelDetail, bool) {
	if r == nil {
		return modelDetail{}, false
	}
	r.mu.RLock()
	loaded := r.diskLoaded
	snap := r.snap
	r.mu.RUnlock()

	if !loaded {
		r.mu.Lock()
		r.ensureDiskLoadedLocked()
		snap = r.snap
		r.mu.Unlock()
	}
	if snap == nil {
		return modelDetail{}, false
	}

	rawLower := strings.ToLower(strings.TrimSpace(modelID))
	norm := normalizeModelKey(modelID)
	if norm == "" {
		return modelDetail{}, false
	}

	var hit modelsDevEntry
	found := false
	for _, provKey := range modelsDevProviderKeys(provider, rawLower) {
		pm := snap.ByProvider[provKey]
		if pm == nil {
			continue
		}
		if e, ok := pm[rawLower]; ok {
			hit, found = e, true
			break
		}
		if e, ok := pm[norm]; ok {
			hit, found = e, true
			break
		}
	}
	if !found {
		if e, ok := snap.Global[rawLower]; ok {
			hit, found = e, true
		} else if e, ok := snap.Global[norm]; ok {
			hit, found = e, true
		}
	}
	if !found {
		return modelDetail{}, false
	}

	d := modelDetail{
		ID:              modelID,
		Label:           hit.Name,
		ContextWindow:   hit.ContextWindow,
		ContextWindowK:  tokensToK(hit.ContextWindow),
		MaxOutputTokens: hit.MaxOutputTokens,
		ReasoningLevels: append([]string(nil), hit.ReasoningLevels...),
		ReleaseDate:     hit.ReleaseDate,
		SupportsTools:   hit.SupportsTools,
		SupportsVision:  hit.SupportsVision,
		Source:          "models.dev",
	}
	if provider != "ollama" {
		d.PriceInputPerM = hit.PriceInputPerM
		d.PriceOutputPerM = hit.PriceOutputPerM
		d.PriceCacheReadPerM = hit.PriceCacheReadPerM
		d.PriceCacheWritePerM = hit.PriceCacheWritePerM
	}
	if len(d.ReasoningLevels) > 0 {
		for _, pref := range []string{"high", "medium", "low"} {
			if containsStr(d.ReasoningLevels, pref) {
				d.DefaultReasoningEffort = pref
				break
			}
		}
	}
	if hit.SupportsThinkingToggle && (provider == "anthropic" || provider == "anthropic-oauth") {
		d.ThinkingType = "enabled"
	}
	if provider == "openai" && openAIUsesMaxCompletionTokens(norm) {
		d.MaxTokensField = llm.MaxTokensFieldCompletion
	}
	return d, true
}

func modelsDevProviderKeys(provider, rawLower string) []string {
	// If the model ID has an explicit vendor prefix (e.g. "anthropic/claude-..."), prefer that vendor first.
	var out []string
	if slash := strings.Index(rawLower, "/"); slash > 0 {
		prefix := rawLower[:slash]
		switch prefix {
		case "x-ai":
			out = append(out, "xai")
		case "moonshotai":
			out = append(out, "moonshotai", "moonshot")
		case "z-ai":
			out = append(out, "zai", "zhipuai")
		default:
			out = append(out, prefix)
		}
	}
	switch provider {
	case "anthropic", "anthropic-oauth":
		out = append(out, "anthropic")
	case "openai", "openai-responses", "chatgpt":
		out = append(out, "openai")
	case "opencode-zen":
		out = append(out, "opencode", "anthropic", "openai", "google", "deepseek", "xai")
	case "opencode-go":
		out = append(out, "opencode-go", "opencode", "deepseek", "moonshotai", "zai", "minimax")
	case "antigravity-cli":
		out = append(out, "google", "anthropic", "openai")
	case "devin":
		out = append(out, "anthropic", "openai", "google")
	default:
		out = append(out, "openai", "anthropic", "google", "deepseek", "xai")
	}
	return out
}

func openAIUsesMaxCompletionTokens(norm string) bool {
	return strings.HasPrefix(norm, "gpt-5") ||
		strings.HasPrefix(norm, "gpt-6") ||
		strings.HasPrefix(norm, "o1") ||
		strings.HasPrefix(norm, "o3") ||
		strings.HasPrefix(norm, "o4") ||
		strings.HasPrefix(norm, "codex-mini")
}

func containsStr(list []string, target string) bool {
	for _, s := range list {
		if s == target {
			return true
		}
	}
	return false
}

// canonicalModelsDevProviders restricts the compact cached projection to
// high-signal providers so the disk cache stays small (<300 KB instead of 5 MB).
var canonicalModelsDevProviders = map[string]int{
	"anthropic":   100,
	"openai":      100,
	"google":      100,
	"deepseek":    95,
	"xai":         95,
	"opencode":    90,
	"opencode-go": 90,
	"mistral":     85,
	"moonshotai":  85,
	"alibaba":     80,
	"qwen":        80,
	"minimax":     80,
	"zai":         80,
	"zhipuai":     75,
	"groq":        70,
	"openrouter":  60,
}

func fetchModelsDevSnapshot(ctx context.Context, client *http.Client, endpoint string) (*modelsDevSnapshot, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ArtexV2/model-catalog")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models.dev HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, modelsDevMaxBytes))
	if err != nil {
		return nil, err
	}
	return parseModelsDevDocument(body)
}

// modelsDevVariantScore ranks candidate entries that normalize to the same key
// so unsuffixed/paid canonical entries deterministically beat `:free` or other
// `:variant` entries regardless of Go map iteration order.
func modelsDevVariantScore(lowID, normID string, e modelsDevEntry) int {
	score := 0
	if lowID == normID {
		score += 100
	}
	if !strings.Contains(lowID, ":") {
		score += 40
	}
	if !strings.HasSuffix(lowID, ":free") && !strings.Contains(strings.ToLower(e.Name), "(free)") {
		score += 20
	}
	if e.PriceInputPerM > 0 || e.PriceOutputPerM > 0 {
		score += 10
	}
	return score
}

func parseModelsDevDocument(body []byte) (*modelsDevSnapshot, error) {
	// Parse top-level provider map lazily so we skip struct reflection on ~310 non-canonical providers.
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, err
	}

	type providerDoc struct {
		Models map[string]struct {
			ID               string `json:"id"`
			Name             string `json:"name"`
			Attachment       *bool  `json:"attachment"`
			Reasoning        bool   `json:"reasoning"`
			ReasoningOptions []struct {
				Type   string   `json:"type"`
				Values []string `json:"values"`
			} `json:"reasoning_options"`
			ToolCall    *bool  `json:"tool_call"`
			ReleaseDate string `json:"release_date"`
			Limit       *struct {
				Context int `json:"context"`
				Output  int `json:"output"`
			} `json:"limit"`
			Cost *struct {
				Input      float64 `json:"input"`
				Output     float64 `json:"output"`
				CacheRead  float64 `json:"cache_read"`
				CacheWrite float64 `json:"cache_write"`
			} `json:"cost"`
		} `json:"models"`
	}

	snap := &modelsDevSnapshot{
		Version:    1,
		FetchedAt:  time.Now().Unix(),
		ByProvider: make(map[string]map[string]modelsDevEntry, len(canonicalModelsDevProviders)),
		Global:     make(map[string]modelsDevEntry, 512),
	}
	globalRank := make(map[string]int, 512)
	globalScore := make(map[string]int, 512)
	globalID := make(map[string]string, 512)

	for provKey, rawProv := range top {
		pk := strings.ToLower(strings.TrimSpace(provKey))
		rank, keep := canonicalModelsDevProviders[pk]
		if !keep || len(rawProv) == 0 {
			continue
		}
		var prov providerDoc
		if err := json.Unmarshal(rawProv, &prov); err != nil || len(prov.Models) == 0 {
			continue
		}
		pm := make(map[string]modelsDevEntry, len(prov.Models))
		pmNormScore := make(map[string]int, len(prov.Models))
		pmNormID := make(map[string]string, len(prov.Models))
		for mapID, m := range prov.Models {
			id := strings.TrimSpace(m.ID)
			if id == "" {
				id = strings.TrimSpace(mapID)
			}
			if id == "" {
				continue
			}
			var ctxWin, outCap int
			if m.Limit != nil {
				ctxWin = m.Limit.Context
				outCap = m.Limit.Output
			}
			if ctxWin <= 0 && outCap <= 0 {
				continue
			}
			var levels []string
			toggle := false
			for _, ro := range m.ReasoningOptions {
				switch strings.ToLower(strings.TrimSpace(ro.Type)) {
				case "toggle":
					toggle = true
				case "effort":
					for _, v := range ro.Values {
						v = strings.ToLower(strings.TrimSpace(v))
						if v != "" && !containsStr(levels, v) {
							levels = append(levels, v)
						}
					}
				}
			}
			entry := modelsDevEntry{
				ID:                     id,
				Name:                   strings.TrimSpace(m.Name),
				ContextWindow:          ctxWin,
				MaxOutputTokens:        outCap,
				Reasoning:              m.Reasoning,
				ReasoningLevels:        levels,
				SupportsThinkingToggle: toggle,
				SupportsTools:          m.ToolCall,
				SupportsVision:         m.Attachment,
				ReleaseDate:            strings.TrimSpace(m.ReleaseDate),
			}
			if m.Cost != nil {
				entry.PriceInputPerM = m.Cost.Input
				entry.PriceOutputPerM = m.Cost.Output
				entry.PriceCacheReadPerM = m.Cost.CacheRead
				entry.PriceCacheWritePerM = m.Cost.CacheWrite
			}
			lowID := strings.ToLower(id)
			normID := normalizeModelKey(id)
			vScore := modelsDevVariantScore(lowID, normID, entry)
			pm[lowID] = entry
			if normID != "" {
				if lowID == normID {
					pmNormScore[normID] = vScore
					pmNormID[normID] = lowID
				} else {
					prevScore, exists := pmNormScore[normID]
					if !exists || vScore > prevScore || (vScore == prevScore && lowID < pmNormID[normID]) {
						pm[normID] = entry
						pmNormScore[normID] = vScore
						pmNormID[normID] = lowID
					}
				}
			}
			putGlobal := func(key string) {
				prevRank, exists := globalRank[key]
				if !exists ||
					rank > prevRank ||
					(rank == prevRank && (vScore > globalScore[key] || (vScore == globalScore[key] && lowID < globalID[key]))) {
					globalRank[key] = rank
					globalScore[key] = vScore
					globalID[key] = lowID
					snap.Global[key] = entry
				}
			}
			putGlobal(lowID)
			if normID != "" && normID != lowID {
				putGlobal(normID)
			}
		}
		if len(pm) > 0 {
			snap.ByProvider[pk] = pm
		}
	}
	return snap, nil
}

// ---------- Provider Disk Catalog Cache (Antigravity / Devin) ----------

type diskProviderCatalog struct {
	Version   int           `json:"version"`
	UpdatedAt int64         `json:"updated_at"`
	Models    []string      `json:"models"`
	Details   []modelDetail `json:"details"`
}

func (s *Server) modelCatalogCacheDir() string {
	if s != nil && strings.TrimSpace(s.dataDir) != "" {
		return filepath.Join(s.dataDir, "model-catalogs")
	}
	return ""
}

func (s *Server) ensureModelRegistry() *modelRegistry {
	if s == nil {
		return nil
	}
	s.modelCatalogMu.Lock()
	defer s.modelCatalogMu.Unlock()
	if s.modelReg == nil {
		url := s.modelsDevURL
		// Bare &Server{} in unit tests has empty dataDir and empty modelsDevURL:
		// avoid hitting the live internet during isolated tests.
		if url == "" && strings.TrimSpace(s.dataDir) == "" {
			return nil
		}
		s.modelReg = newModelRegistry(s.modelCatalogCacheDir(), url)
	}
	return s.modelReg
}

func safeCatalogFileKey(provider string) string {
	var sb strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(provider)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

func (s *Server) readDiskProviderCatalog(provider string) ([]string, []modelDetail, time.Time, bool) {
	dir := s.modelCatalogCacheDir()
	key := safeCatalogFileKey(provider)
	if dir == "" || key == "" {
		return nil, nil, time.Time{}, false
	}
	p := filepath.Join(dir, key+".json")
	info, err := os.Stat(p)
	if err != nil || info.IsDir() || info.Size() > providerCatalogMaxBytes {
		return nil, nil, time.Time{}, false
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, nil, time.Time{}, false
	}
	var doc diskProviderCatalog
	if err := json.Unmarshal(raw, &doc); err != nil || doc.Version != 1 || len(doc.Models) == 0 {
		return nil, nil, time.Time{}, false
	}
	return doc.Models, doc.Details, time.Unix(doc.UpdatedAt, 0), true
}

func (s *Server) writeDiskProviderCatalog(provider string, models []string, details []modelDetail) {
	dir := s.modelCatalogCacheDir()
	key := safeCatalogFileKey(provider)
	if dir == "" || key == "" || len(models) == 0 {
		return
	}
	doc := diskProviderCatalog{
		Version:   1,
		UpdatedAt: time.Now().Unix(),
		Models:    models,
		Details:   details,
	}
	_ = writeJSONAtomic(filepath.Join(dir, key+".json"), doc)
}

func (s *Server) deleteDiskProviderCatalog(provider string) {
	dir := s.modelCatalogCacheDir()
	key := safeCatalogFileKey(provider)
	if dir == "" || key == "" {
		return
	}
	_ = os.Remove(filepath.Join(dir, key+".json"))
}

func writeJSONAtomic(dest string, v any) error {
	if strings.TrimSpace(dest) == "" {
		return nil
	}
	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	nonce := catalogTmpNonce.Add(1)
	tmp := filepath.Join(dir, fmt.Sprintf(".%s-%d-%d.tmp", filepath.Base(dest), os.Getpid(), nonce))
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
