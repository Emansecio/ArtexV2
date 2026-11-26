package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestAgyAuthURLParserAndCodeExtraction(t *testing.T) {
	var p AgyAuthURLParser
	url := "https://accounts.google.com/o/oauth2/v2/auth?client_id=" + AgyOAuthClientID + "&scope=openid"
	if got, err := p.Push("Open this URL:\n", false); err != nil || got != "" {
		t.Fatalf("unexpected initial push: got=%q err=%v", got, err)
	}
	if got, err := p.Push(url+"\n", false); err != nil || got != url {
		t.Fatalf("expected URL %q, got %q err=%v", url, got, err)
	}

	var bad AgyAuthURLParser
	if _, err := bad.Push("https://accounts.google.com/o/oauth2/v2/auth?client_id=wrong\n", false); err == nil {
		t.Fatalf("expected error for wrong client_id")
	}

	if code, err := AgyAuthorizationCode("http://localhost:8080/cb?code=abc123&state=s"); err != nil || code != "abc123" {
		t.Fatalf("expected abc123, got %q err=%v", code, err)
	}
	if code, err := AgyAuthorizationCode("  raw_code_42  "); err != nil || code != "raw_code_42" {
		t.Fatalf("expected raw_code_42, got %q err=%v", code, err)
	}
	if _, err := AgyAuthorizationCode("   "); err == nil {
		t.Fatalf("expected error on blank input")
	}
}

func TestAgyCredentialsStageExportRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	cred := AgyCredential{
		Access:    "ya29.access",
		Refresh:   "1//refresh",
		ExpiresMS: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC).UnixMilli(),
		Email:     "dev@example.com",
	}
	if err := StageAgyCredentials(cred, tmp); err != nil {
		t.Fatalf("StageAgyCredentials: %v", err)
	}
	exported, err := ExportAgyCredentials(tmp)
	if err != nil {
		t.Fatalf("ExportAgyCredentials: %v", err)
	}
	if exported.Access != cred.Access || exported.Refresh != cred.Refresh || exported.Email != cred.Email {
		t.Fatalf("roundtrip mismatch: got %+v want %+v", exported, cred)
	}
}

func TestAgyModelCollapseAndQuotaError(t *testing.T) {
	collapsed := CollapseAdvertisedAgyModels([]string{
		"gemini-4-carbon",
		"gemini-4-hydrogen-low",
		"gemini-4-hydrogen-high",
		"gemini-4-disabled",
		"internal-orchestrator",
	})
	if len(collapsed) != 2 {
		t.Fatalf("expected 2 models, got %#v", collapsed)
	}
	if collapsed[1].ID != "gemini-4-hydrogen" || len(collapsed[1].ReasoningLevels) != 2 {
		t.Fatalf("unexpected collapsed effort model: %+v", collapsed[1])
	}
	if got := AgyModelValue("gemini-4-hydrogen", "high", []string{"gemini-4-hydrogen-high"}); got != "gemini-4-hydrogen-high" {
		t.Fatalf("expected suffixed model, got %q", got)
	}
	err := agyQuotaNoticeError("API error: RESOURCE_EXHAUSTED (code 429)")
	var hse *HTTPStatusError
	if !errors.As(err, &hse) || hse.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429 HTTPStatusError, got %v", err)
	}
}

func TestAntigravityFakeAgyProbeToolParkingAndPoolReuse(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake agy test is unix-only")
	}
	if !HasNCOnPath() {
		t.Skip("nc not available on PATH")
	}

	tmp := t.TempDir()
	t.Setenv("ARTEX_AGY_HOME", filepath.Join(tmp, "agy-home"))
	pidFile := filepath.Join(tmp, "pids.log")

	// Fake `agy` binary speaking ACP NDJSON on stdin/stdout and talking to the
	// loopback MCP bridge via `nc` when prompted with "use-tool".
	script := `#!/bin/sh
set -eu
MCP_PORT=""
while [ $# -gt 0 ]; do
  if [ "$1" = "--ide-mcp-port" ]; then
    MCP_PORT="$2"
    shift 2
    continue
  fi
  shift
done
echo "$$" >> "` + pidFile + `"
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*)
      id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1}}\n' "$id"
      ;;
    *'"method":"session/new"'*)
      id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
      printf '{"jsonrpc":"2.0","id":%s,"result":{"sessionId":"sess-1","configOptions":[{"id":"model","options":[{"value":"gemini-4-carbon"},{"value":"gemini-4-neon-high"},{"value":"gemini-4-neon-low"}]}]}}\n' "$id"
      ;;
    *'"method":"session/set_config_option"'*)
      id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
      printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
      ;;
    *'"method":"session/prompt"'*)
      id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
      if printf '%s' "$line" | grep -q "use-tool"; then
        printf '{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_thought_chunk","content":{"text":"planning tool"}}}}\n'
        tool_resp=$( (printf '{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"echo","arguments":{"msg":"ping"}}}\n'; sleep 3) | nc 127.0.0.1 "$MCP_PORT" | head -n 1 )
        printf '{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"text":"tool-done:%s"}}}}\n' "$(printf '%s' "$tool_resp" | grep -o 'pong' || echo none)"
        printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
      else
        printf '{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"text":"hello-agy"}}}}\n'
        printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
      fi
      ;;
  esac
done
`
	fakeBin := filepath.Join(tmp, "fake-agy")
	if err := os.WriteFile(fakeBin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake agy: %v", err)
	}
	t.Setenv("ARTEX_AGY_PATH", fakeBin)
	defer ShutdownAntigravityPool()

	cred := AgyCredential{
		Access:    "ya29.fake",
		Refresh:   "1//fake",
		ExpiresMS: time.Now().Add(time.Hour).UnixMilli(),
		Email:     "tester@example.com",
	}

	// 1. Probe models
	models, err := ProbeAntigravityModels(context.Background(), nil, cred)
	if err != nil {
		t.Fatalf("ProbeAntigravityModels: %v", err)
	}
	if len(models) != 2 || models[0].ID != "gemini-4-carbon" || models[1].ID != "gemini-4-neon" {
		t.Fatalf("unexpected probed models: %#v", models)
	}

	// 2. Run a tool-parking round-trip and confirm process reuse across turns
	_ = os.Remove(pidFile)
	p, err := NewProvider(Config{
		Format: FormatAntigravity,
		Model:  "gemini-4-carbon",
		AgyCredFunc: func(ctx context.Context, forceRefresh bool) (AgyCredential, error) {
			return cred, nil
		},
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	tools := []ToolSchema{{
		Name:        "echo",
		Description: "Echo message",
		InputSchema: map[string]any{"type": "object"},
	}}
	req1 := CompletionRequest{
		System:         []string{"sys"},
		Messages:       []Message{UserText("please use-tool now")},
		Tools:          tools,
		PromptCacheKey: "conv-agy-1",
	}
	msg1, stop1, _, err := p.Complete(context.Background(), req1)
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if stop1 != "tool_use" {
		t.Fatalf("expected stop=tool_use, got %q (msg=%+v)", stop1, msg1)
	}
	uses := msg1.ToolUses()
	if len(uses) != 1 || uses[0].Name != "echo" {
		t.Fatalf("expected 1 echo tool_use, got %+v", uses)
	}
	var args map[string]string
	_ = json.Unmarshal(uses[0].Input, &args)
	if args["msg"] != "ping" {
		t.Fatalf("unexpected tool args: %+v", args)
	}

	// Turn 2 delivers the parked tool result on the SAME pooled process.
	req2 := req1
	req2.Messages = append(req1.Messages, msg1, Message{
		Role:    RoleUser,
		Content: []ContentBlock{ToolResultText(uses[0].ID, "pong", false)},
	})
	msg2, stop2, _, err := p.Complete(context.Background(), req2)
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if stop2 != "end_turn" || !strings.Contains(msg2.Text(), "tool-done:pong") {
		t.Fatalf("expected tool-done:pong end_turn, got stop=%q text=%q", stop2, msg2.Text())
	}

	// Verify only 1 process was spawned for both turns!
	pidData, _ := os.ReadFile(pidFile)
	pids := strings.Fields(strings.TrimSpace(string(pidData)))
	if len(pids) != 1 {
		t.Fatalf("expected pooled process reuse (1 spawn), got %d spawns: %v", len(pids), pids)
	}

	ShutdownAntigravityPool()
}

func TestAntigravityParallelToolCallsAndCompactionRestart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake agy test is unix-only")
	}
	if !HasNCOnPath() {
		t.Skip("nc not available on PATH")
	}

	tmp := t.TempDir()
	t.Setenv("ARTEX_AGY_HOME", filepath.Join(tmp, "agy-home"))
	pidFile := filepath.Join(tmp, "pids-parallel.log")
	promptLog := filepath.Join(tmp, "prompts.log")

	script := `#!/bin/sh
set -eu
MCP_PORT=""
while [ $# -gt 0 ]; do
  if [ "$1" = "--ide-mcp-port" ]; then
    MCP_PORT="$2"
    shift 2
    continue
  fi
  shift
done
echo "$$" >> "` + pidFile + `"
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*)
      id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1}}\n' "$id"
      ;;
    *'"method":"session/new"'*)
      id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
      printf '{"jsonrpc":"2.0","id":%s,"result":{"sessionId":"sess-p","configOptions":[{"id":"model","options":[{"value":"gemini-4-carbon"}]}]}}\n' "$id"
      ;;
    *'"method":"session/set_config_option"'*)
      id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
      printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
      ;;
    *'"method":"session/prompt"'*)
      id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
      printf '%s\n' "$line" >> "` + promptLog + `"
      if printf '%s' "$line" | grep -q "parallel-tools"; then
        # Send TWO parallel tools/call requests on the same MCP connection.
        (printf '{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"echo","arguments":{"msg":"first"}}}\n{"jsonrpc":"2.0","id":12,"method":"tools/call","params":{"name":"echo","arguments":{"msg":"second"}}}\n'; sleep 3) | nc 127.0.0.1 "$MCP_PORT" | head -n 2 >/dev/null
        printf '{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"text":"both-done"}}}}\n'
        printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
      else
        printf '{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"text":"compacted-ok"}}}}\n'
        printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
      fi
      ;;
  esac
done
`
	fakeBin := filepath.Join(tmp, "fake-agy-parallel")
	if err := os.WriteFile(fakeBin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake agy: %v", err)
	}
	t.Setenv("ARTEX_AGY_PATH", fakeBin)
	defer ShutdownAntigravityPool()

	cred := AgyCredential{
		Access:    "ya29.parallel",
		Refresh:   "1//parallel",
		ExpiresMS: time.Now().Add(time.Hour).UnixMilli(),
		Email:     "parallel@example.com",
		AccountID: "acct-parallel",
	}
	p, err := NewProvider(Config{
		Format: FormatAntigravity,
		Model:  "gemini-4-carbon",
		AgyCredFunc: func(context.Context, bool) (AgyCredential, error) {
			return cred, nil
		},
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	tools := []ToolSchema{{
		Name:        "echo",
		Description: "Echo message",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"msg": map[string]any{"type": "string"}}},
	}}

	// Turn 1: model issues 2 parallel tool calls. Both must retain distinct valid JSON args!
	req1 := CompletionRequest{
		Messages:       []Message{UserText("parallel-tools")},
		Tools:          tools,
		PromptCacheKey: "sess-parallel",
	}
	msg1, stop1, _, err := p.Complete(context.Background(), req1)
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if stop1 != "tool_use" {
		t.Fatalf("expected tool_use, got %q", stop1)
	}
	uses := msg1.ToolUses()
	if len(uses) != 2 {
		t.Fatalf("expected 2 parallel tool calls, got %d: %+v", len(uses), uses)
	}
	var args0, args1 map[string]string
	_ = json.Unmarshal(uses[0].Input, &args0)
	_ = json.Unmarshal(uses[1].Input, &args1)
	if args0["msg"] != "first" || args1["msg"] != "second" {
		t.Fatalf("parallel tool args corrupted: tool0=%s tool1=%s", string(uses[0].Input), string(uses[1].Input))
	}

	// Turn 2: deliver both tool results on the same process.
	req2 := req1
	req2.Messages = append(req1.Messages, msg1, Message{
		Role: RoleUser,
		Content: []ContentBlock{
			ToolResultText(uses[0].ID, "r1", false),
			ToolResultText(uses[1].ID, "r2", false),
		},
	})
	msg2, stop2, _, err := p.Complete(context.Background(), req2)
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if stop2 != "end_turn" || msg2.Text() != "both-done" {
		t.Fatalf("unexpected turn 2: stop=%q text=%q", stop2, msg2.Text())
	}

	// Turn 3: simulate history compaction (len(Messages) drops from 3 to 1).
	// Must restart the pooled process and send the compacted prompt, not "".
	req3 := req1
	req3.Messages = []Message{UserText("compacted-summary")}
	msg3, stop3, _, err := p.Complete(context.Background(), req3)
	if err != nil {
		t.Fatalf("turn 3 (compacted): %v", err)
	}
	if stop3 != "end_turn" || msg3.Text() != "compacted-ok" {
		t.Fatalf("unexpected turn 3: stop=%q text=%q", stop3, msg3.Text())
	}
	pidData, _ := os.ReadFile(pidFile)
	if pids := strings.Fields(strings.TrimSpace(string(pidData))); len(pids) != 2 {
		t.Fatalf("expected 2 spawns (initial + compaction restart), got %d: %v", len(pids), pids)
	}
	promptsRaw, _ := os.ReadFile(promptLog)
	if !strings.Contains(string(promptsRaw), "compacted-summary") {
		t.Fatalf("expected compacted-summary to be sent to restarted process, got: %s", string(promptsRaw))
	}
}
