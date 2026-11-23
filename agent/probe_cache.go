package agent

import (
	"context"
	"encoding/json"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	actool "github.com/Autumn-27/norma/tool"
)

const (
	defaultProbeCacheTTL        = 120 * time.Second
	defaultProbeCacheMaxEntries = 256
)

type probeCacheEntry struct {
	res       actool.Result
	expiresAt time.Time
	seq       uint64
}

// TaskProbeCache deduplicates successful GET-like WebFetch calls across workers
// within the same task during a short TTL window.
type TaskProbeCache struct {
	mu         sync.RWMutex
	entries    map[string]probeCacheEntry
	ttl        time.Duration
	maxEntries int
	seq        uint64
	now        func() time.Time
}

// NewTaskProbeCache constructs a task-scoped probe cache with bounded size and TTL.
func NewTaskProbeCache() *TaskProbeCache {
	return newTaskProbeCache(defaultProbeCacheTTL, defaultProbeCacheMaxEntries, time.Now)
}

func newTaskProbeCache(ttl time.Duration, maxEntries int, now func() time.Time) *TaskProbeCache {
	if ttl <= 0 {
		ttl = defaultProbeCacheTTL
	}
	if maxEntries <= 0 {
		maxEntries = defaultProbeCacheMaxEntries
	}
	if now == nil {
		now = time.Now
	}
	return &TaskProbeCache{
		entries:    make(map[string]probeCacheEntry),
		ttl:        ttl,
		maxEntries: maxEntries,
		now:        now,
	}
}

func (c *TaskProbeCache) get(key string) (actool.Result, bool) {
	if c == nil || key == "" {
		return actool.Result{}, false
	}
	now := c.now()
	c.mu.RLock()
	entry, ok := c.entries[key]
	c.mu.RUnlock()
	if !ok {
		return actool.Result{}, false
	}
	if !now.Before(entry.expiresAt) {
		c.mu.Lock()
		if cur, exists := c.entries[key]; exists && !now.Before(cur.expiresAt) {
			delete(c.entries, key)
		}
		c.mu.Unlock()
		return actool.Result{}, false
	}
	return cloneToolResult(entry.res), true
}

func (c *TaskProbeCache) set(key string, res actool.Result) {
	if c == nil || key == "" || res.IsError {
		return
	}
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.entries[key]; !exists && len(c.entries) >= c.maxEntries {
		for k, e := range c.entries {
			if !now.Before(e.expiresAt) {
				delete(c.entries, k)
			}
		}
		for len(c.entries) >= c.maxEntries {
			var oldestKey string
			var oldestSeq uint64
			first := true
			for k, e := range c.entries {
				if first || e.seq < oldestSeq {
					oldestKey = k
					oldestSeq = e.seq
					first = false
				}
			}
			if oldestKey == "" {
				break
			}
			delete(c.entries, oldestKey)
		}
	}

	c.seq++
	c.entries[key] = probeCacheEntry{
		res:       cloneToolResult(res),
		expiresAt: now.Add(c.ttl),
		seq:       c.seq,
	}
}

func cloneToolResult(r actool.Result) actool.Result {
	r.Content = slices.Clone(r.Content)
	r.Extra = slices.Clone(r.Extra)
	return r
}

func wrapCachedWebFetch(tools []actool.CoreTool, cache *TaskProbeCache) []actool.CoreTool {
	if cache == nil {
		return tools
	}
	out := slices.Clone(tools)
	for i, t := range out {
		if t.Name() == "WebFetch" {
			out[i] = &cachedWebFetchTool{CoreTool: t, cache: cache}
		}
	}
	return out
}

type cachedWebFetchTool struct {
	actool.CoreTool
	cache *TaskProbeCache
}

func (t *cachedWebFetchTool) Call(ctx context.Context, input json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	key, cacheable := probeCacheKey(input)
	if !cacheable {
		return t.CoreTool.Call(ctx, input, tc)
	}
	if cached, ok := t.cache.get(key); ok {
		return cached, nil
	}
	res, err := t.CoreTool.Call(ctx, input, tc)
	if err == nil && !res.IsError {
		t.cache.set(key, res)
	}
	return res, err
}

func probeCacheKey(input json.RawMessage) (string, bool) {
	var in struct {
		URL     string `json:"url"`
		Extract bool   `json:"extract"`
		Prompt  string `json:"prompt"`
		Method  string `json:"method"`
	}
	if json.Unmarshal(input, &in) != nil {
		return "", false
	}
	rawURL := strings.TrimSpace(in.URL)
	if rawURL == "" {
		return "", false
	}
	if method := strings.TrimSpace(in.Method); method != "" && !strings.EqualFold(method, "GET") {
		return "", false
	}
	normURL := rawURL
	if u, err := url.Parse(rawURL); err == nil && u.Host != "" {
		u.Scheme = strings.ToLower(u.Scheme)
		u.Host = strings.ToLower(u.Host)
		u.Fragment = ""
		normURL = u.String()
	}
	return normURL + "|extract=" + strconv.FormatBool(in.Extract) + "|" + strings.TrimSpace(in.Prompt), true
}
