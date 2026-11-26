package llm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

const devinMaxModelConfigsBytes = 16 * 1024 * 1024

// DevinEffortRoute maps a reasoning effort level to a wire chat_model_uid.
type DevinEffortRoute struct {
	Effort string `json:"effort"`
	UID    string `json:"uid"`
}

// DevinCatalogEntry represents one normalized model in the Devin account catalog.
type DevinCatalogEntry struct {
	ID                string             `json:"id"`
	Name              string             `json:"name"`
	ContextWindow     int                `json:"context_window"`
	MaxOutputTokens   int                `json:"max_output_tokens"`
	AcceptsImages     bool               `json:"accepts_images"`
	ModelRouter       bool               `json:"model_router"`
	Default           bool               `json:"default"`
	RequestModel      string             `json:"request_model,omitempty"`
	EffortRoutes      []DevinEffortRoute `json:"effort_routes,omitempty"`
	ReasoningLevels   []string           `json:"reasoning_levels,omitempty"`
	ParallelToolCalls bool               `json:"parallel_tool_calls"`
}

// ChatModelUID returns the wire chat_model_uid for this entry at the given effort.
func (e DevinCatalogEntry) ChatModelUID(effort string) string {
	effort = strings.TrimSpace(strings.ToLower(effort))
	if effort != "" {
		for _, r := range e.EffortRoutes {
			if r.Effort == effort {
				return r.UID
			}
		}
	}
	if e.RequestModel != "" {
		return e.RequestModel
	}
	return e.ID
}

// DevinFallbackCatalog returns the bundled seed models used when catalog discovery is unavailable.
func DevinFallbackCatalog() []DevinCatalogEntry {
	return []DevinCatalogEntry{
		{
			ID:              "swe-1-6",
			Name:            "SWE-1.6",
			ContextWindow:   200_000,
			MaxOutputTokens: 128_000,
		},
		{
			ID:              "swe-1-6-fast",
			Name:            "SWE-1.6 Fast",
			ContextWindow:   200_000,
			MaxOutputTokens: 128_000,
		},
	}
}

// IsDevinRouterModel reports whether model is a server-side router UID requiring AssignModel.
func IsDevinRouterModel(model string) bool {
	return model == "adaptive" || model == "auto" || strings.HasPrefix(model, "subagent-")
}

type devinModelFamilyEntry struct {
	key   string
	order uint64
	name  string
}

type devinModelFamily struct {
	label     string
	entries   []devinModelFamilyEntry
	isDefault bool
}

type devinClientModelConfig struct {
	label                string
	disabled             bool
	supportsImages       bool
	modelUID             string
	maxTokens            int64
	infoMaxOutputTokens  int64
	infoMaxTokens        int64
	infoDisplayOption    uint64
	infoIsModelRouter    bool
	infoHasHarnessUIDs   bool
	hasFeatureImages     bool
	featureImages        bool
	featureParallelTools bool
	family               *devinModelFamily
	isDefaultInFamily    bool
}

func decodeDevinModelFeatures(b []byte, cfg *devinClientModelConfig) {
	images := false
	for _, f := range decodeProtoFields(b) {
		switch f.number {
		case 11:
			images = f.varint != 0
		case 21:
			cfg.featureParallelTools = f.varint != 0
		}
	}
	cfg.hasFeatureImages = true
	cfg.featureImages = images
}

func decodeDevinModelFamily(b []byte) *devinModelFamily {
	fam := &devinModelFamily{}
	for _, f := range decodeProtoFields(b) {
		switch f.number {
		case 1:
			fam.label = f.str()
		case 2:
			var key string
			var order uint64
			var name string
			hasVal := false
			for _, entry := range decodeProtoFields(f.bytes) {
				switch entry.number {
				case 1:
					key = entry.str()
				case 2:
					hasVal = true
					for _, part := range decodeProtoFields(entry.bytes) {
						switch part.number {
						case 1:
							order = part.varint
						case 2:
							name = part.str()
						}
					}
				}
			}
			if hasVal {
				fam.entries = append(fam.entries, devinModelFamilyEntry{
					key:   key,
					order: order,
					name:  name,
				})
			}
		case 3:
			fam.isDefault = f.varint != 0
		}
	}
	return fam
}

func decodeDevinClientModelConfig(b []byte) devinClientModelConfig {
	var cfg devinClientModelConfig
	for _, f := range decodeProtoFields(b) {
		switch f.number {
		case 1:
			cfg.label = f.str()
		case 4:
			cfg.disabled = f.varint != 0
		case 5:
			cfg.supportsImages = f.varint != 0
		case 18:
			cfg.maxTokens = int64(f.varint)
		case 22:
			cfg.modelUID = f.str()
		case 23:
			for _, info := range decodeProtoFields(f.bytes) {
				switch info.number {
				case 4:
					cfg.infoMaxTokens = int64(info.varint)
				case 6:
					decodeDevinModelFeatures(info.bytes, &cfg)
				case 13:
					cfg.infoMaxOutputTokens = int64(info.varint)
				case 20:
					cfg.infoHasHarnessUIDs = true
				case 22:
					cfg.infoDisplayOption = info.varint
				case 25:
					cfg.infoIsModelRouter = info.varint != 0
				}
			}
		case 30:
			cfg.family = decodeDevinModelFamily(f.bytes)
		case 31:
			cfg.isDefaultInFamily = f.varint != 0
		}
	}
	return cfg
}

var devinFamilyEfforts = []string{"minimal", "low", "medium", "high", "xhigh", "max"}

func devinFamilyEffort(name string) string {
	var sb strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			if r >= 'A' && r <= 'Z' {
				r += 'a' - 'A'
			}
			sb.WriteRune(r)
		}
	}
	switch sb.String() {
	case "none", "nothinking":
		return "off"
	case "minimal":
		return "minimal"
	case "low":
		return "low"
	case "medium":
		return "medium"
	case "high":
		return "high"
	case "xhigh":
		return "xhigh"
	case "max":
		return "max"
	default:
		return ""
	}
}

func devinSlug(input string, sep byte) string {
	var out strings.Builder
	out.Grow(len(input))
	lastSep := false
	for i := 0; i < len(input); i++ {
		c := input[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			out.WriteByte(c)
			lastSep = false
		} else if c >= 'A' && c <= 'Z' {
			out.WriteByte(c + ('a' - 'A'))
			lastSep = false
		} else if out.Len() > 0 && !lastSep {
			out.WriteByte(sep)
			lastSep = true
		}
	}
	return strings.TrimRight(out.String(), string(sep))
}

type devinFamilyLane struct {
	id            string
	name          string
	members       []string
	memberSet     map[string]bool
	defaultMember string
	routing       map[string]string
}

func collectDevinFamilyLane(lanes *[]*devinFamilyLane, cfg *devinClientModelConfig, uid string) {
	if cfg.family == nil {
		return
	}
	label := strings.TrimSpace(cfg.family.label)
	if label == "" {
		return
	}
	var effort string
	var thinkingSet, thinkingVal bool
	fast := false
	oneMillion := false
	for _, e := range cfg.family.entries {
		switch devinSlug(e.key, ' ') {
		case "fast mode":
			fast = e.order == 1
		case "thinking":
			thinkingSet = true
			thinkingVal = e.order == 1
		case "1m context":
			oneMillion = e.order == 1
		case "effort", "reasoning effort":
			effort = devinFamilyEffort(e.name)
		}
	}
	if thinkingSet && !thinkingVal {
		effort = "off"
	}
	base := devinSlug(label, '-')
	if base == "" {
		return
	}
	parts := []string{base}
	if oneMillion {
		parts = append(parts, "1m")
	}
	if fast {
		parts = append(parts, "fast")
	}
	id := strings.Join(parts, "-")

	var lane *devinFamilyLane
	for _, existing := range *lanes {
		if existing.id == id {
			lane = existing
			break
		}
	}
	if lane == nil {
		name := label
		if oneMillion {
			name += " 1M"
		}
		if fast {
			name += " Fast"
		}
		lane = &devinFamilyLane{
			id:        id,
			name:      name,
			memberSet: map[string]bool{},
			routing:   map[string]string{},
		}
		*lanes = append(*lanes, lane)
	}
	lane.members = append(lane.members, uid)
	lane.memberSet[uid] = true
	if lane.defaultMember == "" && (cfg.isDefaultInFamily || cfg.family.isDefault) {
		lane.defaultMember = uid
	}
	if effort != "" {
		if _, exists := lane.routing[effort]; !exists {
			lane.routing[effort] = uid
		}
	}
}

func collapseDevinFamilyLanes(entries []DevinCatalogEntry, lanes []*devinFamilyLane, accountDefault string) []DevinCatalogEntry {
	for _, lane := range lanes {
		var routes []DevinEffortRoute
		if offUID, ok := lane.routing["off"]; ok {
			routes = append(routes, DevinEffortRoute{Effort: "off", UID: offUID})
		}
		hasNonOff := false
		for _, eff := range devinFamilyEfforts {
			if uid, ok := lane.routing[eff]; ok {
				routes = append(routes, DevinEffortRoute{Effort: eff, UID: uid})
				hasNonOff = true
			}
		}
		if !hasNonOff {
			continue
		}
		head := lane.defaultMember
		if head == "" && len(lane.members) > 0 {
			head = lane.members[0]
		}
		var familyEntry *DevinCatalogEntry
		for i := range entries {
			if entries[i].ID == head {
				cp := entries[i]
				familyEntry = &cp
				break
			}
		}
		if familyEntry == nil {
			continue
		}
		isDef := false
		for _, e := range entries {
			if e.Default && lane.memberSet[e.ID] {
				isDef = true
				break
			}
		}
		familyEntry.Default = isDef
		accountRouted := false
		for _, r := range routes {
			if r.Effort != "off" && r.UID == accountDefault {
				accountRouted = true
				break
			}
		}
		if accountRouted {
			familyEntry.RequestModel = accountDefault
		} else {
			familyEntry.RequestModel = head
		}
		familyEntry.ID = lane.id
		familyEntry.Name = lane.name
		familyEntry.EffortRoutes = routes
		levels := make([]string, 0, len(routes))
		for _, r := range routes {
			if r.Effort != "off" {
				levels = append(levels, r.Effort)
			}
		}
		familyEntry.ReasoningLevels = levels

		filtered := entries[:0]
		for _, e := range entries {
			if e.ID != familyEntry.ID && !lane.memberSet[e.ID] {
				filtered = append(filtered, e)
			}
		}
		entries = append(filtered, *familyEntry)
	}
	return entries
}

func devinFusionLead(uid string, live map[string]*devinClientModelConfig) (lead string, isFusion bool, ok bool) {
	if !strings.HasPrefix(uid, "fusion-") {
		return "", false, false
	}
	rest := strings.TrimPrefix(uid, "fusion-")
	cut := strings.Index(rest, "-sidekick-")
	if cut <= 0 {
		return "", false, false
	}
	cand := rest[:cut]
	if _, exists := live[cand]; exists {
		return cand, true, true
	}
	if strings.HasSuffix(cand, "-fast") {
		base := strings.TrimSuffix(cand, "-fast")
		if _, exists := live[base+"-priority"]; exists {
			return base + "-priority", true, true
		}
		if _, exists := live[base]; exists {
			return base, true, true
		}
	}
	return "", true, false
}

func devinConfigContextWindow(cfg *devinClientModelConfig) int {
	if cfg.maxTokens > 0 {
		return int(cfg.maxTokens)
	}
	if cfg.infoMaxTokens > 0 {
		return int(cfg.infoMaxTokens)
	}
	return 200_000
}

func devinConfigMaxOutputTokens(cfg *devinClientModelConfig) int {
	if cfg.infoMaxOutputTokens > 0 {
		return int(cfg.infoMaxOutputTokens)
	}
	return 64_000
}

func devinConfigAcceptsImages(cfg *devinClientModelConfig, uid string) bool {
	if uid == "swe-1-6" || uid == "swe-1-6-fast" {
		return false
	}
	if cfg.hasFeatureImages {
		return cfg.featureImages
	}
	return cfg.supportsImages
}

// NormalizeDevinModelConfigs decodes a GetCliModelConfigsResponse protobuf payload
// into normalized logical models, collapsing family effort lanes while preserving
// 1M and Fast variants.
func NormalizeDevinModelConfigs(payload []byte) []DevinCatalogEntry {
	var defaultUID string
	var configs []devinClientModelConfig
	for _, f := range decodeProtoFields(payload) {
		switch f.number {
		case 1:
			configs = append(configs, decodeDevinClientModelConfig(f.bytes))
		case 3:
			for _, nested := range decodeProtoFields(f.bytes) {
				if nested.number == 3 {
					defaultUID = strings.TrimSpace(nested.str())
				}
			}
		}
	}

	live := make(map[string]*devinClientModelConfig, len(configs))
	for i := range configs {
		if configs[i].disabled {
			continue
		}
		uid := strings.TrimSpace(configs[i].modelUID)
		if uid != "" {
			if _, exists := live[uid]; !exists {
				live[uid] = &configs[i]
			}
		}
	}

	seen := map[string]bool{}
	entries := make([]DevinCatalogEntry, 0, len(configs))
	var lanes []*devinFamilyLane
	for i := range configs {
		cfg := &configs[i]
		uid := strings.TrimSpace(cfg.modelUID)
		if cfg.disabled || isDevinInternalDisplay(cfg.infoDisplayOption) || uid == "" || seen[uid] {
			continue
		}
		seen[uid] = true

		lead, isFusion, leadOK := devinFusionLead(uid, live)
		if isFusion && !leadOK {
			continue
		}
		limits := cfg
		imgUID := uid
		if lead != "" {
			if src, ok := live[lead]; ok {
				limits = src
			}
			imgUID = lead
		}
		isRouter := cfg.infoDisplayOption == devinDisplayOptionModelRouter || cfg.infoIsModelRouter
		name := strings.TrimSpace(cfg.label)
		if name == "" {
			name = uid
		}
		entries = append(entries, DevinCatalogEntry{
			ID:                uid,
			Name:              name,
			ContextWindow:     devinConfigContextWindow(limits),
			MaxOutputTokens:   devinConfigMaxOutputTokens(limits),
			AcceptsImages:     devinConfigAcceptsImages(limits, imgUID),
			ModelRouter:       isRouter && !cfg.infoHasHarnessUIDs,
			Default:           uid == defaultUID,
			RequestModel:      lead,
			ParallelToolCalls: limits.featureParallelTools,
		})
		if !isRouter {
			collectDevinFamilyLane(&lanes, cfg, uid)
		}
	}

	entries = collapseDevinFamilyLanes(entries, lanes, defaultUID)
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	return entries
}

// FetchDevinModelConfigs calls GetCliModelConfigs against baseURL and returns
// the normalized account model catalog.
func FetchDevinModelConfigs(ctx context.Context, client *http.Client, baseURL, apiKey string) ([]DevinCatalogEntry, error) {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = DevinDefaultBaseURL
	}
	if client == nil {
		client = http.DefaultClient
	}
	endpoint := strings.TrimRight(baseURL, "/") + devinModelConfigsPath
	body := encodeDevinUnaryRequest(encodeDevinDiscoveryMetadata(apiKey))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/proto")
	req.Header.Set("connect-protocol-version", "1")
	req.Header.Set("accept", "*/*")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("devin catalog connection: %w", err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, devinMaxModelConfigsBytes+1))
	if err != nil {
		return nil, fmt.Errorf("devin catalog body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &HTTPStatusError{
			Prefix:     "devin catalog",
			StatusCode: resp.StatusCode,
			Body:       devinHTTPErrorDetail(payload),
		}
	}
	if len(payload) > devinMaxModelConfigsBytes {
		return nil, fmt.Errorf("devin catalog exceeds 16MB limit")
	}
	entries := NormalizeDevinModelConfigs(payload)
	if len(entries) == 0 {
		return nil, fmt.Errorf("devin catalog returned no usable models")
	}
	cacheDevinCatalog(baseURL, apiKey, entries)
	return entries, nil
}

// Small in-memory catalog cache so devinProvider can resolve family effort routes
// (e.g. logical model "claude-sonnet-4-6" + effort "high" -> wire chat_model_uid)
// without repeating GetCliModelConfigs on every turn.
type cachedDevinCatalog struct {
	byID      map[string]DevinCatalogEntry
	expiresAt time.Time
}

var (
	devinCatalogMu    sync.Mutex
	devinCatalogCache = map[[32]byte]cachedDevinCatalog{}
)

func devinCatalogKey(baseURL, apiKey string) [32]byte {
	h := sha256.New()
	h.Write([]byte(strings.TrimRight(baseURL, "/")))
	h.Write([]byte{0})
	h.Write([]byte(strings.TrimSpace(apiKey)))
	var k [32]byte
	copy(k[:], h.Sum(nil))
	return k
}

func cacheDevinCatalog(baseURL, apiKey string, entries []DevinCatalogEntry) {
	byID := make(map[string]DevinCatalogEntry, len(entries))
	for _, e := range entries {
		byID[e.ID] = e
	}
	devinCatalogMu.Lock()
	if len(devinCatalogCache) >= 16 {
		clear(devinCatalogCache)
	}
	devinCatalogCache[devinCatalogKey(baseURL, apiKey)] = cachedDevinCatalog{
		byID:      byID,
		expiresAt: time.Now().Add(10 * time.Minute),
	}
	devinCatalogMu.Unlock()
}

func lookupDevinCatalogEntry(baseURL, apiKey, modelID string) (entry DevinCatalogEntry, found bool, cached bool) {
	devinCatalogMu.Lock()
	c, ok := devinCatalogCache[devinCatalogKey(baseURL, apiKey)]
	devinCatalogMu.Unlock()
	if !ok || time.Now().After(c.expiresAt) {
		return DevinCatalogEntry{}, false, false
	}
	entry, found = c.byID[modelID]
	return entry, found, true
}

func ensureDevinCatalogEntry(ctx context.Context, client *http.Client, baseURL, apiKey, modelID string) (DevinCatalogEntry, bool) {
	if entry, found, cached := lookupDevinCatalogEntry(baseURL, apiKey, modelID); cached {
		return entry, found
	}
	fetchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := FetchDevinModelConfigs(fetchCtx, client, baseURL, apiKey); err != nil {
		// Cache an empty negative entry briefly so turns don't repeatedly hit
		// GetCliModelConfigs when the catalog endpoint is unreachable.
		devinCatalogMu.Lock()
		devinCatalogCache[devinCatalogKey(baseURL, apiKey)] = cachedDevinCatalog{
			byID:      map[string]DevinCatalogEntry{},
			expiresAt: time.Now().Add(30 * time.Second),
		}
		devinCatalogMu.Unlock()
		return DevinCatalogEntry{}, false
	}
	entry, found, _ := lookupDevinCatalogEntry(baseURL, apiKey, modelID)
	return entry, found
}
