package agent

import (
	"container/list"
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
	key       string
	res       actool.Result
	expiresAt time.Time
}

// TaskProbeCache deduplicates successful GET-like WebFetch calls across workers
// within the same task during a short TTL window, using O(1) LRU eviction and
// singleflight in-flight coalescing.
type TaskProbeCache struct {
	mu         sync.Mutex
	entries    map[string]*list.Element
	lru        *list.List
	flight     flightGroup[actool.Result]
	ttl        time.Duration
	maxEntries int
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
		entries:    make(map[string]*list.Element, maxEntries),
		lru:        list.New(),
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
	c.mu.Lock()
	defer c.mu.Unlock()
	elem, ok := c.entries[key]
	if !ok {
		return actool.Result{}, false
	}
	entry := elem.Value.(*probeCacheEntry)
	if !now.Before(entry.expiresAt) {
		c.lru.Remove(elem)
		delete(c.entries, key)
		return actool.Result{}, false
	}
	c.lru.MoveToFront(elem)
	return cloneToolResult(entry.res), true
}

func (c *TaskProbeCache) set(key string, res actool.Result) {
	if c == nil || key == "" || res.IsError {
		return
	}
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()

	if elem, exists := c.entries[key]; exists {
		entry := elem.Value.(*probeCacheEntry)
		entry.res = cloneToolResult(res)
		entry.expiresAt = now.Add(c.ttl)
		c.lru.MoveToFront(elem)
		return
	}
	for c.lru.Len() >= c.maxEntries {
		back := c.lru.Back()
		if back == nil {
			break
		}
		ev := back.Value.(*probeCacheEntry)
		delete(c.entries, ev.key)
		c.lru.Remove(back)
	}
	entry := &probeCacheEntry{
		key:       key,
		res:       cloneToolResult(res),
		expiresAt: now.Add(c.ttl),
	}
	c.entries[key] = c.lru.PushFront(entry)
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
	// Deduplicate concurrent in-flight GET probes for the same URL across workers
	// while isolating per-caller context cancellation.
	res, err := t.cache.flight.DoContext(ctx, key, func(fctx context.Context) (actool.Result, error) {
		if cached, ok := t.cache.get(key); ok {
			return cached, nil
		}
		r, callErr := t.CoreTool.Call(fctx, input, tc)
		if callErr == nil && !r.IsError {
			t.cache.set(key, r)
		}
		return r, callErr
	})
	if err != nil {
		return res, err
	}
	return cloneToolResult(res), nil
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
