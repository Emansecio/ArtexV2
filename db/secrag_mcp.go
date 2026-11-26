package db

import (
	"fmt"
)

const (
	secragMCPName            = "secrag"
	secragMCPCommand         = "secrag-mcp"
	secragMCPSeedSetting     = "secrag_mcp_seed_v1"
	secragMCPAutoVisSeedV2   = "secrag_mcp_auto_vis_v1"
	secragMCPRetesterVisSeed = "secrag_mcp_retester_vis_v1"
)

// secragDefaultAgents are the built-in agent roles that receive direct visibility
// to the local cybersecurity knowledge base (secrag MCP) on first seed.
var secragDefaultAgents = []string{
	"mainagent",
	"planner",
	"worker",
	"auto",
	"pentest",
	"retester",
}

// seedSecragMCP registers the built-in local cybersecurity RAG MCP server once
// (disabled by default — matching browser MCP so test runs and hosts without a
// populated KB do not spawn Python processes unexpectedly) and grants initial
// visibility to the core reasoning/execution agents.
// Gated by secrag_mcp_seed_v1 so later operator edits (enable/disable, args,
// visibility) in /system/mcp or /system/agents are never overwritten on restart.
func (d *DB) seedSecragMCP() error {
	if v, _, _ := d.GetSetting(secragMCPSeedSetting); v != "done" {
		if _, err := d.Exec(`
INSERT INTO mcp_servers(name, transport, command, args, env, enabled)
VALUES ($1, 'stdio', $2, '[]', '{}', false)
ON CONFLICT (name) DO NOTHING`,
			secragMCPName, secragMCPCommand); err != nil {
			return fmt.Errorf("seed secrag mcp: %w", err)
		}

		var mcpID int64
		if err := d.QueryRow(`SELECT id FROM mcp_servers WHERE name=$1`, secragMCPName).Scan(&mcpID); err != nil {
			return fmt.Errorf("lookup secrag mcp id: %w", err)
		}

		for _, key := range secragDefaultAgents {
			if _, err := d.Exec(`
INSERT INTO agent_visibility(agent_id, resource_kind, resource_id, mcp_tool_name, enabled)
SELECT id, 'mcp', $2, '', true FROM agents WHERE key=$1
ON CONFLICT (agent_id, resource_kind, resource_id, mcp_tool_name) DO NOTHING`,
				key, mcpID); err != nil {
				return fmt.Errorf("secrag visibility agent %s: %w", key, err)
			}
		}

		d.InvalidateMCPCache()
		if err := d.SetSetting(secragMCPSeedSetting, "done"); err != nil {
			return err
		}
		if err := d.SetSetting(secragMCPAutoVisSeedV2, "done"); err != nil {
			return err
		}
		return d.SeedRetesterDefaultVisibility()
	}

	// One-shot additive visibility grant for the 'auto' agent on existing DBs
	// seeded before 'auto' was added to secragDefaultAgents. ON CONFLICT DO NOTHING
	// preserves any explicit operator toggle-off.
	if v, _, _ := d.GetSetting(secragMCPAutoVisSeedV2); v != "done" {
		var mcpID int64
		if err := d.QueryRow(`SELECT id FROM mcp_servers WHERE name=$1`, secragMCPName).Scan(&mcpID); err == nil {
			if _, err := d.Exec(`
INSERT INTO agent_visibility(agent_id, resource_kind, resource_id, mcp_tool_name, enabled)
SELECT id, 'mcp', $1, '', true FROM agents WHERE key='auto'
ON CONFLICT (agent_id, resource_kind, resource_id, mcp_tool_name) DO NOTHING`,
				mcpID); err != nil {
				return fmt.Errorf("secrag visibility agent auto: %w", err)
			}
			d.InvalidateMCPCache()
		}
		if err := d.SetSetting(secragMCPAutoVisSeedV2, "done"); err != nil {
			return err
		}
	}
	return d.SeedRetesterDefaultVisibility()
}

// SeedRetesterDefaultVisibility grants default secrag MCP and cybersec-kb skill
// visibility to the 'retester' agent once it exists. Because 'retester' is
// created during server.New() (after db.Open() runs seedBuiltins), calling this
// both on retester creation and during startup ensures fresh and existing DBs
// alike receive the defaults while preserving any explicit operator opt-outs.
func (d *DB) SeedRetesterDefaultVisibility() error {
	if v, _, _ := d.GetSetting(secragMCPRetesterVisSeed); v == "done" {
		return nil
	}
	var retesterID int64
	if err := d.QueryRow(`SELECT id FROM agents WHERE key=$1`, FindingRetestAgentKey).Scan(&retesterID); err != nil {
		// Retester has not been seeded yet; keep the flag unset so we run once it exists.
		return nil
	}
	var mcpID int64
	if err := d.QueryRow(`SELECT id FROM mcp_servers WHERE name=$1`, secragMCPName).Scan(&mcpID); err == nil {
		if _, err := d.Exec(`
INSERT INTO agent_visibility(agent_id, resource_kind, resource_id, mcp_tool_name, enabled)
VALUES ($1, 'mcp', $2, '', true)
ON CONFLICT (agent_id, resource_kind, resource_id, mcp_tool_name) DO NOTHING`,
			retesterID, mcpID); err != nil {
			return fmt.Errorf("secrag visibility agent retester: %w", err)
		}
		d.InvalidateMCPCache()
	}
	if _, err := d.Exec(`
INSERT INTO agent_skill_visibility(agent_id, skill_name, enabled)
VALUES ($1, 'cybersec-kb', true)
ON CONFLICT (agent_id, skill_name) DO NOTHING`, retesterID); err != nil {
		return fmt.Errorf("cybersec-kb skill visibility agent retester: %w", err)
	}
	return d.SetSetting(secragMCPRetesterVisSeed, "done")
}
