package db

import (
	"fmt"
)

const (
	secragMCPName        = "secrag"
	secragMCPCommand     = "secrag-mcp"
	secragMCPSeedSetting = "secrag_mcp_seed_v1"
)

// secragDefaultAgents are the built-in agent roles that receive direct visibility
// to the local cybersecurity knowledge base (secrag MCP) on first seed.
var secragDefaultAgents = []string{
	"mainagent",
	"planner",
	"worker",
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
	if v, _, _ := d.GetSetting(secragMCPSeedSetting); v == "done" {
		return nil
	}

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
	return d.SetSetting(secragMCPSeedSetting, "done")
}
