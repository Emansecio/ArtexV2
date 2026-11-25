package db

import (
	"database/sql"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Small write-invalidated caches for rarely-mutated config tables. Agent run
// assembly repeats ~15-20 config queries per turn (settings, tools, prompts,
// MCP list, visibility, agent lookup); most answers are stable for minutes.
//
// Entries carry a 60s TTL (up from 2s) because all write paths — both typed
// setters and raw Exec/Query/QueryRow DML — actively invalidate affected keys.
const configCacheTTL = 60 * time.Second

type configCache struct {
	mu sync.RWMutex
	m  map[string]configEntry
}

type configEntry struct {
	v  any
	at time.Time
}

func (c *configCache) get(key string) (any, bool) {
	c.mu.RLock()
	e, ok := c.m[key]
	c.mu.RUnlock()
	if !ok || time.Since(e.at) > configCacheTTL {
		return nil, false
	}
	return e.v, true
}

func (c *configCache) put(key string, v any) {
	c.mu.Lock()
	if c.m == nil {
		c.m = map[string]configEntry{}
	}
	c.m[key] = configEntry{v: v, at: time.Now()}
	c.mu.Unlock()
}

func (c *configCache) drop(keys ...string) {
	c.mu.Lock()
	for _, k := range keys {
		delete(c.m, k)
	}
	c.mu.Unlock()
}

// dropPrefix invalidates a whole key family (e.g. "tools:" on any tools write).
func (c *configCache) dropPrefix(prefix string) {
	c.mu.Lock()
	for k := range c.m {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			delete(c.m, k)
		}
	}
	c.mu.Unlock()
}

// cfgRef returns the DB's config cache, allocating it on first use so
// zero-value &DB{} receivers (tests) keep working.
func (d *DB) cfgRef() *configCache {
	d.cfgOnce.Do(func() { d.cfg = &configCache{} })
	return d.cfg
}

// InvalidateSettingCache drops a cached settings row — for callers that write
// the settings table through a raw transaction instead of SetSetting.
func (d *DB) InvalidateSettingCache(key string) {
	d.cfgRef().drop("set:" + key)
}

// InvalidateToolCache drops the cached tool catalog — for callers that write
// the tools table through raw Exec/Tx (schema-migration repair paths).
func (d *DB) InvalidateToolCache() {
	d.cfgRef().drop("tools:list", "tools:custom")
}

// InvalidateMCPCache drops the cached MCP server list — for callers that write
// mcp_servers through raw Exec/Tx.
func (d *DB) InvalidateMCPCache() {
	d.cfgRef().drop("mcp:list")
}

// configWriteTable matches DML that targets a configCache-backed table. Raw-SQL
// writes (migrations, repair Execs, tests that seed via d.Exec) bypass the
// cache-aware write methods, so the Exec/Query/QueryRow wrappers below drop the
// affected entries instead of serving stale config for up to configCacheTTL.
var configWriteTable = regexp.MustCompile(`(?i)\b(?:insert\s+into|update|delete\s+from|truncate(?:\s+table)?)\s+(settings|tools|mcp_servers|agent_prompts|agent_visibility|agent_skill_visibility|agents)\b`)
var graphWriteTable = regexp.MustCompile(`(?i)\b(?:insert\s+into|update|delete\s+from|truncate(?:\s+table)?)\s+(exploration_nodes|exploration_edges|assets|task_credentials|task_scope|coverage_ledger)\b`)

// isPureSelect reports whether query begins with SELECT (ignoring leading
// whitespace/comments), allowing read queries to bypass the DML invalidation
// regex completely. CTEs starting with WITH still fall through to the regex so
// write CTEs (WITH ... DELETE/UPDATE/INSERT) continue to invalidate.
func isPureSelect(query string) bool {
	s := strings.TrimSpace(query)
	if len(s) < 6 {
		return false
	}
	return strings.EqualFold(s[:6], "SELECT") && (len(s) == 6 || s[6] == ' ' || s[6] == '\t' || s[6] == '\n' || s[6] == '\r')
}

func (d *DB) invalidateConfigFor(query string) {
	if isPureSelect(query) {
		return
	}
	if graphWriteTable.MatchString(query) {
		d.BumpGraphVersion()
	}
	m := configWriteTable.FindStringSubmatch(query)
	if m == nil {
		return
	}
	switch strings.ToLower(m[1]) {
	case "settings":
		d.cfgRef().dropPrefix("set:")
	case "tools":
		d.cfgRef().dropPrefix("tools:")
	case "mcp_servers":
		d.cfgRef().drop("mcp:list")
	case "agent_prompts", "agents":
		d.cfgRef().dropPrefix("prompt:")
	case "agent_visibility":
		d.cfgRef().dropPrefix("vis:")
	case "agent_skill_visibility":
		d.cfgRef().dropPrefix("skills:")
	}
}

// Exec shadows the embedded *sql.DB.Exec so raw DML on cached config tables
// invalidates the matching entries.
func (d *DB) Exec(query string, args ...any) (sql.Result, error) {
	res, err := d.DB.Exec(query, args...)
	if err == nil {
		d.invalidateConfigFor(query)
	}
	return res, err
}

// Query shadows the embedded *sql.DB.Query for the same reason — write CTEs
// (WITH ... DELETE) reach the server through Query.
func (d *DB) Query(query string, args ...any) (*sql.Rows, error) {
	rows, err := d.DB.Query(query, args...)
	if err == nil {
		d.invalidateConfigFor(query)
	}
	return rows, err
}

// QueryRow shadows the embedded *sql.DB.QueryRow for the same reason — writes
// with RETURNING (INSERT ... RETURNING id) reach the server through QueryRow.
// The result error is only visible at Scan, so a failed write may drop an
// entry; that costs one re-read, never staleness.
func (d *DB) QueryRow(query string, args ...any) *sql.Row {
	row := d.DB.QueryRow(query, args...)
	d.invalidateConfigFor(query)
	return row
}
