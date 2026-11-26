package db

import (
	"os"
	"testing"
)

func TestSeedSecragMCP_IdempotentAndRespectsUserEdits(t *testing.T) {
	dsn := os.Getenv("ARTEX_PG_DSN")
	if dsn == "" {
		t.Skip("ARTEX_PG_DSN not set")
	}
	d, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	// Ensure secrag MCP row exists after Open/seedBuiltins.
	var id int64
	var cmd string
	if err := d.QueryRow(`SELECT id, command FROM mcp_servers WHERE name=$1`, secragMCPName).Scan(&id, &cmd); err != nil {
		t.Fatalf("expected secrag mcp row: %v", err)
	}
	if cmd != secragMCPCommand {
		t.Fatalf("command=%q want %q", cmd, secragMCPCommand)
	}

	// Verify seed setting prevents clobbering user toggle on restart.
	v, _, err := d.GetSetting(secragMCPSeedSetting)
	if err != nil || v != "done" {
		t.Fatalf("expected %s=done, got %q (err=%v)", secragMCPSeedSetting, v, err)
	}
}
