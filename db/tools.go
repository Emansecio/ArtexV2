package db

import (
	"encoding/json"
)

// Tool is one row of the built-in tool catalog. key + handler live in code; this
// row carries only the page-editable surface: description, parameter schema
// (structure read-only, per-param description/default editable), agent binding,
// and the on/off switch. See schema.sql §H and agent/toolcatalog.go.
type Tool struct {
	Key         string          `json:"key"`
	System      bool            `json:"system"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
	Agents      []string        `json:"agents"`
	Enabled     bool            `json:"enabled"`
	Kind        string          `json:"kind"`     // builtin | command | script | http
	Exec        json.RawMessage `json:"exec"`     // custom tool execution spec (kind!=builtin)
	Deferred    bool            `json:"deferred"` // deferred schema (via SearchExtraTools/ExecuteExtraTool)
	Calls       int             `json:"calls"`    // runtime ledger aggregate; not stored in tools
}

const toolCols = `key, system, description, schema, agents, enabled, kind, exec, deferred`

// dropToolCache invalidates the cached ListTools/ListCustomTools results after
// a successful write to the tools table.
func (d *DB) dropToolCache() {
	d.cfgRef().drop("tools:list", "tools:custom")
}

// SeedTool inserts a built-in tool's code-defined defaults ONCE. ON CONFLICT DO
// NOTHING: an existing row (possibly edited in the UI) is never overwritten on
// startup — that's what keeps page edits from being wiped every restart. Use
// UpsertToolForce for an explicit "reset to code default".
func (d *DB) SeedTool(key, desc string, schema, agents json.RawMessage) error {
	if len(schema) == 0 {
		schema = json.RawMessage("{}")
	}
	if len(agents) == 0 {
		agents = json.RawMessage("[]")
	}
	res, err := d.Exec(`
INSERT INTO tools(key, system, description, schema, agents, enabled)
VALUES ($1, true, $2, $3, $4, true)
ON CONFLICT (key) DO NOTHING`, key, desc, schema, agents)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		d.dropToolCache()
	}
	return nil
}

// AddAgentToToolBinding adds an agent key to the given tools' `agents` arrays if
// not already present (idempotent). Used to give the built-in Auto agent its
// default toolset on existing DBs.
func (d *DB) AddAgentToToolBinding(agentKey string, keys []string) error {
	for _, k := range keys {
		if _, err := d.Exec(`UPDATE tools SET agents = agents || to_jsonb($1::text) WHERE key=$2 AND NOT (agents ? $1)`, agentKey, k); err != nil {
			return err
		}
	}
	d.dropToolCache()
	return nil
}

// RemoveAgentFromToolBindings strips an agent key from every tool's `agents`
// JSONB array — called when a custom agent is deleted so no tool keeps a dangling
// binding. Uses jsonb `-` (remove array element) guarded by `?` (membership).
func (d *DB) RemoveAgentFromToolBindings(agentKey string) error {
	_, err := d.Exec(`UPDATE tools SET agents = agents - $1 WHERE agents ? $1`, agentKey)
	if err == nil {
		d.dropToolCache()
	}
	return err
}

// RemoveAgentFromTool strips one agent key from a SINGLE tool's `agents` array —
// used by one-time migrations that change a tool's default binding on existing DBs
// (SeedTool is first-insert-only, so a changed default never reaches a seeded row).
func (d *DB) RemoveAgentFromTool(agentKey, toolKey string) error {
	_, err := d.Exec(`UPDATE tools SET agents = agents - $1 WHERE key=$2 AND agents ? $1`, agentKey, toolKey)
	if err == nil {
		d.dropToolCache()
	}
	return err
}

// UpsertToolForce overwrites a tool row with the given code-default values (used by
// the per-tool "reset" action). It resets description/schema/agents and re-enables
// the tool, but keeps system=true.
func (d *DB) UpsertToolForce(key, desc string, schema, agents json.RawMessage) error {
	if len(schema) == 0 {
		schema = json.RawMessage("{}")
	}
	if len(agents) == 0 {
		agents = json.RawMessage("[]")
	}
	_, err := d.Exec(`
INSERT INTO tools(key, system, description, schema, agents, enabled)
VALUES ($1, true, $2, $3, $4, true)
ON CONFLICT (key) DO UPDATE
  SET description = EXCLUDED.description,
      schema      = EXCLUDED.schema,
      agents      = EXCLUDED.agents,
      enabled     = true`, key, desc, schema, agents)
	if err == nil {
		d.dropToolCache()
	}
	return err
}

// RefreshToolDefaults updates a system tool's model-facing description + schema to the
// code defaults, PRESERVING the user's agent binding + enabled flag. Used by one-time
// migrations to propagate a code schema change (SeedTool is first-insert-only, so a new
// parameter added in code otherwise never reaches an already-seeded row). No-op for
// custom tools or unknown keys.
func (d *DB) RefreshToolDefaults(key, desc string, schema json.RawMessage) error {
	if len(schema) == 0 {
		schema = json.RawMessage("{}")
	}
	_, err := d.Exec(`UPDATE tools SET description=$2, schema=$3, updated_at=now() WHERE key=$1 AND system`, key, desc, schema)
	if err == nil {
		d.dropToolCache()
	}
	return err
}

func scanTool(rows interface{ Scan(...any) error }) (*Tool, error) {
	var t Tool
	var agents []byte
	if err := rows.Scan(&t.Key, &t.System, &t.Description, &t.Schema, &agents, &t.Enabled, &t.Kind, &t.Exec, &t.Deferred); err != nil {
		return nil, err
	}
	if !t.System { // a custom tool's exec is sealed at rest
		plain, err := openSecret(colToolExec, string(t.Exec))
		if err != nil {
			return nil, err
		}
		t.Exec = json.RawMessage(plain)
	}
	if len(agents) > 0 {
		_ = json.Unmarshal(agents, &t.Agents)
	}
	if t.Agents == nil {
		t.Agents = []string{}
	}
	return &t, nil
}

// cloneTools deep-copies cached tool rows: callers must never mutate the
// shared cache entries (e.g. schema masking writes back over m.Env).
func cloneTools(in []*Tool) []*Tool {
	out := make([]*Tool, len(in))
	for i, t := range in {
		cp := *t
		out[i] = &cp
	}
	return out
}

// ListTools returns the whole tool catalog, ordered by key. The catalog is
// cached briefly (configCacheTTL) — it is re-read for every agent run while
// writes are rare. All db-level tools writes invalidate; raw-SQL repairs rely
// on the TTL.
func (d *DB) ListTools() ([]*Tool, error) {
	v, hit, gen := d.cfgRef().get("tools:list")
	if hit {
		return cloneTools(v.([]*Tool)), nil
	}
	rows, err := d.Query(`SELECT ` + toolCols + ` FROM tools ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Tool
	for rows.Next() {
		t, err := scanTool(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	d.cfgRef().putIfGen("tools:list", out, gen)
	return cloneTools(out), nil
}

// GetTool fetches one tool row (nil if absent). It resolves through the cached
// catalog so per-call lookups (e.g. the target-gate proxy check) don't each hit
// Postgres; writes invalidate via the "tools:" prefix like ListTools.
func (d *DB) GetTool(key string) (*Tool, error) {
	tools, err := d.ListTools()
	if err != nil {
		return nil, err
	}
	for _, t := range tools {
		if t.Key == key {
			return t, nil
		}
	}
	return nil, nil
}

// CreateCustomTool inserts a user-defined tool (system=false) with an execution
// spec. Fails if the key already exists.
func (d *DB) CreateCustomTool(t *Tool) error {
	schema := t.Schema
	if len(schema) == 0 {
		schema = json.RawMessage("{}")
	}
	exec, err := sealToolExec(t.Exec)
	if err != nil {
		return err
	}
	agents, _ := json.Marshal(t.Agents)
	if len(agents) == 0 {
		agents = json.RawMessage("[]")
	}
	_, err = d.Exec(`
INSERT INTO tools(key, system, description, schema, agents, enabled, kind, exec, deferred)
VALUES ($1, false, $2, $3, $4, $5, $6, $7, $8)`,
		t.Key, t.Description, schema, agents, t.Enabled, t.Kind, exec, t.Deferred)
	if err == nil {
		d.dropToolCache()
	}
	return err
}

// UpdateCustomTool updates a custom tool's editable fields (kind/exec/deferred +
// desc/schema/agents/enabled). Only touches system=false rows.
func (d *DB) UpdateCustomTool(t *Tool) error {
	schema := t.Schema
	if len(schema) == 0 {
		schema = json.RawMessage("{}")
	}
	exec, err := sealToolExec(t.Exec)
	if err != nil {
		return err
	}
	agents, _ := json.Marshal(t.Agents)
	if len(agents) == 0 {
		agents = json.RawMessage("[]")
	}
	_, err = d.Exec(`
UPDATE tools SET description=$2, schema=$3, agents=$4, enabled=$5, kind=$6, exec=$7, deferred=$8
WHERE key=$1 AND system=false`,
		t.Key, t.Description, schema, agents, t.Enabled, t.Kind, exec, t.Deferred)
	if err == nil {
		d.dropToolCache()
	}
	return err
}

// sealToolExec encrypts a custom tool's exec spec for storage; empty means {}.
func sealToolExec(exec json.RawMessage) (string, error) {
	if len(exec) == 0 {
		exec = json.RawMessage("{}")
	}
	return sealSecret(colToolExec, string(exec))
}

// DeleteCustomTool removes a custom tool (system=false only; built-ins protected).
func (d *DB) DeleteCustomTool(key string) error {
	_, err := d.Exec(`DELETE FROM tools WHERE key=$1 AND system=false`, key)
	if err == nil {
		d.dropToolCache()
	}
	return err
}

// ListCustomTools returns only the user-defined (system=false) tools. Cached
// like ListTools.
func (d *DB) ListCustomTools() ([]*Tool, error) {
	v, hit, gen := d.cfgRef().get("tools:custom")
	if hit {
		return cloneTools(v.([]*Tool)), nil
	}
	rows, err := d.Query(`SELECT ` + toolCols + ` FROM tools WHERE system=false ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Tool
	for rows.Next() {
		t, err := scanTool(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	d.cfgRef().putIfGen("tools:custom", out, gen)
	return cloneTools(out), nil
}

// UpdateTool saves the page-editable fields. key is never changed (it is welded to
// the Go handler). system tools: the caller must keep the schema structure — only
// per-param description/default and the agent binding are meant to move.
func (d *DB) UpdateTool(key, desc string, schema, agents json.RawMessage, enabled bool) error {
	if len(schema) == 0 {
		schema = json.RawMessage("{}")
	}
	if len(agents) == 0 {
		agents = json.RawMessage("[]")
	}
	_, err := d.Exec(`
UPDATE tools SET description=$2, schema=$3, agents=$4, enabled=$5 WHERE key=$1`,
		key, desc, schema, agents, enabled)
	if err == nil {
		d.dropToolCache()
	}
	return err
}
