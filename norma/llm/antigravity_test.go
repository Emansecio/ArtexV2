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

func TestAgyKeychainCredentialsAndPromptBlocks(t *testing.T) {
	rawJSON := `{"auth_method":"consumer","id_token_email":"dev@example.com","token":{"access_token":"ya29.access","refresh_token":"1//refresh","expiry":"2030-01-02T00:04:05.500-03:00"}}`
	encoded := "eyJhdXRoX21ldGhvZCI6ImNvbnN1bWVyIiwiaWRfdG9rZW5fZW1haWwiOiJkZXZAZXhhbXBsZS5jb20iLCJ0b2tlbiI6eyJhY2Nlc3NfdG9rZW4iOiJ5YTI5LmFjY2VzcyIsInJlZnJlc2hfdG9rZW4iOiIxLy9yZWZyZXNoIiwiZXhwaXJ5IjoiMjAzMC0wMS0wMlQwMDowNDowNS41MDAtMDM6MDAifX0="
	_ = rawJSON

	prefixed, err := ParseAgyKeychainCredentials("go-keyring-base64:" + encoded + "\n")
	if err != nil {
		t.Fatalf("ParseAgyKeychainCredentials prefixed: %v", err)
	}
	unprefixed, err := ParseAgyKeychainCredentials(encoded)
	if err != nil {
		t.Fatalf("ParseAgyKeychainCredentials unprefixed: %v", err)
	}
	if prefixed != unprefixed {
		t.Fatalf("mismatch prefixed vs unprefixed: %+v vs %+v", prefixed, unprefixed)
	}
	wantExp := time.Date(2030, 1, 2, 3, 4, 5, 500_000_000, time.UTC).UnixMilli()
	if prefixed.ExpiresMS != wantExp || prefixed.Access != "ya29.access" || prefixed.Refresh != "1//refresh" || prefixed.Email != "dev@example.com" {
		t.Fatalf("unexpected keychain credential: %+v", prefixed)
	}
	if _, err := ParseAgyKeychainCredentials("not a keyring payload"); err == nil {
		t.Fatalf("expected error on invalid payload")
	}
	if _, err := ParseAgyKeychainCredentials("   "); err == nil {
		t.Fatalf("expected error on blank payload")
	}

	first := agyBuildPromptBlocks("You are Artex.", "hello")
	if len(first) != 2 || first[0]["text"] != "<system-reminder>\nYou are Artex.\n</system-reminder>" || first[1]["text"] != "hello" {
		t.Fatalf("unexpected opening prompt blocks: %#v", first)
	}
	second := agyBuildPromptBlocks("", "again")
	if len(second) != 1 || second[0]["text"] != "again" {
		t.Fatalf("unexpected follow-up prompt blocks: %#v", second)
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

func TestAgyQuotaNoticeIgnoresProseAndMatchesStandaloneBanner(t *testing.T) {
	// Normal security/pentest prose mentioning "code 429" or "RESOURCE_EXHAUSTED" must NOT abort the turn.
	prose := "The API rate-limiter returned HTTP status code 429 (RESOURCE_EXHAUSTED) after 50 requests."
	if err := agyStandaloneQuotaNoticeError(prose, len(prose), false); err != nil {
		t.Fatalf("legitimate prose must not trigger quota error, got: %v", err)
	}
	// Standalone agy error banner with no tool calls MUST trigger 429.
	banner := "API error: RESOURCE_EXHAUSTED (code 429)"
	var hse *HTTPStatusError
	if err := agyStandaloneQuotaNoticeError(banner, len(banner), false); !errors.As(err, &hse) || hse.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429 for standalone banner, got: %v", err)
	}
	// When tool calls occurred in the turn, never misclassify as quota banner.
	if err := agyStandaloneQuotaNoticeError(banner, len(banner), true); err != nil {
		t.Fatalf("turn with tool calls must not be aborted as quota banner: %v", err)
	}
}

func TestAgyTrailingUserMessagesParkedDeliveryAndTranscriptToolUses(t *testing.T) {
	replyCh := make(chan agyMCPToolOutput, 1)
	proc := &agyProcess{
		parked: map[string]chan agyMCPToolOutput{"call-1": replyCh},
	}
	msgs := []Message{
		UserText("initial"),
		{
			Role: RoleAssistant,
			Content: []ContentBlock{{
				Type:  BlockToolUse,
				ID:    "call-1",
				Name:  "Skill",
				Input: json.RawMessage(`{"name":"scopesentry"}`),
			}},
		},
		{
			Role:    RoleUser,
			Content: []ContentBlock{ToolResultText("call-1", "skill loaded", false)},
		},
		// Trailing RoleUser message (e.g. SkillInvocationMessage / todo reminder / wrap-up prompt)
		UserText("<system-reminder>follow scopesentry rules</system-reminder>"),
	}
	if !agyHasMatchingParkedResults(proc, msgs) {
		t.Fatal("expected agyHasMatchingParkedResults=true when trailing RoleUser message follows tool_result")
	}
	agyDeliverParkedToolResults(proc, msgs)
	out := <-replyCh
	if !strings.Contains(out.Text, "skill loaded") || !strings.Contains(out.Text, "follow scopesentry rules") {
		t.Fatalf("expected parked delivery to include tool result + trailing user text, got %q", out.Text)
	}

	transcript := agyFormatTranscript(msgs)
	if !strings.Contains(transcript, `[tool call: Skill] {"name":"scopesentry"}`) {
		t.Fatalf("expected agyFormatTranscript to include BlockToolUse, got:\n%s", transcript)
	}
}

func TestCollapseAdvertisedAgyModelsUnknownZeroLimits(t *testing.T) {
	collapsed := CollapseAdvertisedAgyModels([]string{"claude-opus-4-8-high", "gemini-4-carbon"})
	if len(collapsed) != 2 {
		t.Fatalf("expected 2 models, got %+v", collapsed)
	}
	if collapsed[0].ID != "claude-opus-4-8" || collapsed[0].ContextWindow != 0 || collapsed[0].MaxOutputTokens != 0 {
		t.Fatalf("unknown model should leave limits 0 for models.dev/known lookup, got %+v", collapsed[0])
	}
	if collapsed[1].ID != "gemini-4-carbon" || collapsed[1].ContextWindow != 1_000_000 {
		t.Fatalf("builtin model should keep known limits, got %+v", collapsed[1])
	}
}

func TestAgyPoolSlotEvictionFlagAndNoFreshNilLRUEviction(t *testing.T) {
	ShutdownAntigravityPool()
	defer ShutdownAntigravityPool()
	t.Setenv("ARTEX_AGY_MAX_PROCS", "2")

	s1 := getAgyPoolSlot("acct:s1")
	s2 := getAgyPoolSlot("acct:s2")
	// Both s1 and s2 are freshly created with proc==nil; acquiring s3 must not evict them as time.Time{}.
	s3 := getAgyPoolSlot("acct:s3")
	if s1.evicted || s2.evicted || s3.evicted {
		t.Fatalf("fresh proc==nil slots must not be LRU-evicted: s1=%v s2=%v s3=%v", s1.evicted, s2.evicted, s3.evicted)
	}

	ShutdownAntigravityAccount("acct")
	if !s1.evicted || !s2.evicted || !s3.evicted {
		t.Fatalf("expected ShutdownAntigravityAccount to mark slots evicted: s1=%v s2=%v s3=%v", s1.evicted, s2.evicted, s3.evicted)
	}
}

func TestAgyPoolReapsAbandonedEmptySlotsBeforeEvictingLiveProcs(t *testing.T) {
	ShutdownAntigravityPool()
	defer ShutdownAntigravityPool()
	t.Setenv("ARTEX_AGY_MAX_PROCS", "2")

	// Slot 1 has a live process; Slot 2 is an abandoned empty slot (simulating a failed spawn/aborted turn).
	live := getAgyPoolSlot("acct:live")
	live.mu.Lock()
	live.proc = &agyProcess{lastUsed: time.Now()}
	live.abandoned = false
	live.mu.Unlock()

	abandoned := getAgyPoolSlot("acct:failed")
	abandoned.mu.Lock()
	abandoned.proc = nil
	abandoned.lastUsed = time.Now()
	abandoned.abandoned = true
	abandoned.mu.Unlock()

	// Acquiring a new slot at maxProcs=2 must reap the abandoned empty slot
	// instead of evicting the healthy live process.
	fresh := getAgyPoolSlot("acct:new")
	if !abandoned.evicted {
		t.Fatal("expected abandoned empty slot to be reaped immediately")
	}
	if live.evicted || live.proc == nil {
		t.Fatalf("expected live slot to survive when abandoned slot could be reaped: evicted=%v proc=%v", live.evicted, live.proc)
	}
	if fresh.evicted {
		t.Fatal("expected new slot not to be evicted")
	}
}

func TestAgyDriveTurnBuffersQuotaBannerWithZeroEmittedEvents(t *testing.T) {
	makeProc := func(chunks []string) *agyProcess {
		msgCh := make(chan agyACPMessage, len(chunks)+2)
		for _, c := range chunks {
			params, _ := json.Marshal(map[string]any{
				"update": map[string]any{
					"sessionUpdate": "agent_message_chunk",
					"content":       map[string]any{"text": c},
				},
			})
			msgCh <- agyACPMessage{kind: agyMsgNotification, method: "session/update", params: params}
		}
		msgCh <- agyACPMessage{kind: agyMsgResponse, id: 1}
		return &agyProcess{
			activePromptID: 1,
			msgCh:          msgCh,
			bridge:         &agyMCPBridge{callCh: make(chan agyParkedToolCall)},
			parked:         map[string]chan agyMCPToolOutput{},
		}
	}

	// 1. Standalone quota banner split across chunks emits ZERO stream events and returns 429.
	procQuota := makeProc([]string{"API ", "error: ", "RESOURCE_EXHAUSTED (code 429)"})
	var emittedQuota []StreamEvent
	_, err := agyDriveTurn(context.Background(), procQuota, func(ev StreamEvent, _ error) bool {
		emittedQuota = append(emittedQuota, ev)
		return true
	})
	var hse *HTTPStatusError
	if !errors.As(err, &hse) || hse.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429 HTTPStatusError, got %v", err)
	}
	if len(emittedQuota) != 0 {
		t.Fatalf("expected zero stream events before 429 failover, got %+v", emittedQuota)
	}

	// 2. Non-quota text starting with "Error:" still flushes completely on turn end.
	procNormal := makeProc([]string{"Error: ", "file not found in workspace"})
	var textBuf strings.Builder
	_, err = agyDriveTurn(context.Background(), procNormal, func(ev StreamEvent, _ error) bool {
		if ev.Type == SETextDelta {
			textBuf.WriteString(ev.Text)
		}
		return true
	})
	if err != nil {
		t.Fatalf("expected nil error for non-quota Error: text, got %v", err)
	}
	if textBuf.String() != "Error: file not found in workspace" {
		t.Fatalf("expected full text flushed, got %q", textBuf.String())
	}
}

func TestStageAgyCredentialsAndBridgeAgentConcurrent(t *testing.T) {
	dir := t.TempDir()
	cred := AgyCredential{
		Access:    "ya29.concurrent",
		Refresh:   "1//refresh",
		ExpiresMS: time.Now().Add(time.Hour).UnixMilli(),
		Email:     "dev@example.com",
	}
	const workers = 16
	errCh := make(chan error, workers*2)
	for i := 0; i < workers; i++ {
		go func() {
			errCh <- StageAgyCredentials(cred, dir)
		}()
		go func() {
			errCh <- writeAgyBridgeAgent(dir)
		}()
	}
	for i := 0; i < workers*2; i++ {
		if err := <-errCh; err != nil {
			t.Fatalf("concurrent stage/bridge failed: %v", err)
		}
	}
}

type notifyWriteCloser struct {
	ch chan struct{}
}

func (w *notifyWriteCloser) Write(p []byte) (int, error) {
	select {
	case w.ch <- struct{}{}:
	default:
	}
	return len(p), nil
}

func (w *notifyWriteCloser) Close() error { return nil }

func TestShutdownAntigravityCancelsActiveStreamImmediately(t *testing.T) {
	t.Setenv("ARTEX_AGY_GEMINI_DIR", t.TempDir())

	runCase := func(t *testing.T, shutdownFn func(accountKey string)) {
		ShutdownAntigravityPool()
		defer ShutdownAntigravityPool()

		cred := AgyCredential{
			Access:    "ya29.shutdown-test",
			Email:     "shutdown@example.com",
			ExpiresMS: time.Now().Add(time.Hour).UnixMilli(),
		}
		accountKey := AgyCredentialKey(cred)
		credVersion := AgyCredentialVersion(cred)
		sessionKey := "sess-inflight"
		slotKey := accountKey + ":" + sessionKey

		bridge, err := startAgyMCPBridge(nil)
		if err != nil {
			t.Fatalf("startAgyMCPBridge: %v", err)
		}
		promptSent := make(chan struct{}, 1)
		procDone := make(chan struct{})
		proc := &agyProcess{
			stdin:             &notifyWriteCloser{ch: promptSent},
			msgCh:             make(chan agyACPMessage), // never receives -> blocks in agyDriveTurn
			done:              procDone,
			bridge:            bridge,
			toolsKey:          agyToolsFingerprint(nil),
			systemHash:        agyFNV1a([]byte("")),
			credentialVersion: credVersion,
			parked:            map[string]chan agyMCPToolOutput{},
			lastUsed:          time.Now(),
		}
		slot := getAgyPoolSlot(slotKey)
		slot.mu.Lock()
		slot.proc = proc
		slot.mu.Unlock()

		prov := newAntigravityProvider(Config{
			AgyCredFunc: func(context.Context, bool) (AgyCredential, error) { return cred, nil },
		})

		streamErrCh := make(chan error, 1)
		go func() {
			var gotErr error
			for _, err := range prov.Stream(context.Background(), CompletionRequest{
				SessionID: sessionKey,
				Messages:  []Message{{Role: RoleUser, Content: []ContentBlock{{Type: BlockText, Text: "hello"}}}},
			}) {
				if err != nil {
					gotErr = err
					break
				}
			}
			streamErrCh <- gotErr
		}()

		select {
		case <-promptSent:
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for Stream to enter agyDriveTurn")
		}

		shutdownDone := make(chan struct{})
		go func() {
			shutdownFn(accountKey)
			close(shutdownDone)
		}()

		select {
		case <-shutdownDone:
		case <-time.After(3 * time.Second):
			t.Fatal("shutdown blocked on active Stream instead of cancelling it immediately")
		}

		select {
		case err := <-streamErrCh:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("expected context.Canceled from aborted Stream, got %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for aborted Stream to exit")
		}

		select {
		case <-procDone:
		default:
			t.Fatal("expected proc.Kill() to close proc.done")
		}
		slot.mu.Lock()
		evicted, remainingProc := slot.evicted, slot.proc
		slot.mu.Unlock()
		if !evicted || remainingProc != nil {
			t.Fatalf("expected slot evicted with nil proc, got evicted=%v proc=%v", evicted, remainingProc)
		}
	}

	t.Run("ShutdownAntigravityAccount", func(t *testing.T) {
		runCase(t, func(accountKey string) { ShutdownAntigravityAccount(accountKey) })
	})
	t.Run("ShutdownAntigravityPool", func(t *testing.T) {
		runCase(t, func(string) { ShutdownAntigravityPool() })
	})
	t.Run("ShutdownBeforeSetCancelRace", func(t *testing.T) {
		slot := &agyPoolSlot{}
		slot.beginShutdown()
		ctx, cancel := context.WithCancel(context.Background())
		if slot.setCancel(cancel) {
			t.Fatal("expected setCancel to return false after beginShutdown")
		}
		if ctx.Err() == nil {
			t.Fatal("expected cancel to be invoked immediately when slot is shutting down")
		}
	})
}

func TestAgyFollowUpDeltaSkipsAssistantAndDetectsPrefixShift(t *testing.T) {
	u1 := UserText("first question")
	a1 := Message{Role: RoleAssistant, Content: []ContentBlock{{Type: BlockText, Text: "first answer"}}}
	u2 := UserText("second question")
	rem := UserText("<system-reminder>updated</system-reminder>")

	delivered := agyHashMessages([]Message{u1})

	// Normal turn 2: [u1, a1, u2] -> must skip a1 and send ONLY [u2].
	delta, ok := agyFollowUpDelta([]Message{u1, a1, u2}, delivered)
	if !ok || len(delta) != 1 || delta[0].Text() != "second question" {
		t.Fatalf("expected only [u2], got ok=%v delta=%+v", ok, delta)
	}
	if formatted := agyFormatTranscript(delta); strings.Contains(formatted, "[previous assistant]") || formatted != "second question" {
		t.Fatalf("unexpected follow-up transcript: %q", formatted)
	}

	// Prepended SystemReminder at index 0 shifts indices -> must return ok=false to trigger restart.
	if _, ok := agyFollowUpDelta([]Message{rem, u1, a1, u2}, delivered); ok {
		t.Fatal("expected prefix shift from prepended SystemReminder to trigger restart")
	}

	// Compaction (shorter history) -> must return ok=false.
	if _, ok := agyFollowUpDelta([]Message{UserText("summary")}, delivered); ok {
		t.Fatal("expected shorter history to trigger restart")
	}

	// Same prefix with SystemReminder present on both turns -> continues cleanly without duplicating a1.
	deliveredWithRem := agyHashMessages([]Message{rem, u1})
	delta2, ok := agyFollowUpDelta([]Message{rem, u1, a1, u2}, deliveredWithRem)
	if !ok || len(delta2) != 1 || delta2[0].Text() != "second question" {
		t.Fatalf("expected continuation with stable SystemReminder, got ok=%v delta=%+v", ok, delta2)
	}
}

func TestAgyPoolLazyIdleReaperAndSessionShutdown(t *testing.T) {
	ShutdownAntigravityPool()
	defer ShutdownAntigravityPool()

	// 1. ShutdownAntigravitySession terminates only the targeted session slot.
	doneA := make(chan struct{})
	doneB := make(chan struct{})
	slotA := getAgyPoolSlot("acct1:conntest-123")
	slotA.mu.Lock()
	slotA.proc = &agyProcess{done: doneA, parked: map[string]chan agyMCPToolOutput{}, lastUsed: time.Now()}
	slotA.mu.Unlock()

	slotB := getAgyPoolSlot("acct1:live-chat-1")
	slotB.mu.Lock()
	slotB.proc = &agyProcess{done: doneB, parked: map[string]chan agyMCPToolOutput{}, lastUsed: time.Now()}
	slotB.mu.Unlock()

	ShutdownAntigravitySession("conntest-123")

	select {
	case <-doneA:
	default:
		t.Fatal("expected conntest-123 proc to be killed by ShutdownAntigravitySession")
	}
	select {
	case <-doneB:
		t.Fatal("live-chat-1 proc must not be killed by ShutdownAntigravitySession(conntest-123)")
	default:
	}

	// 2. Lazy background reaper retires idle slots without any new getAgyPoolSlot calls.
	ShutdownAntigravityPool()
	agyPoolMu.Lock()
	origRetire, origInterval := agyIdleRetire, agyReapInterval
	agyIdleRetire = 40 * time.Millisecond
	agyReapInterval = 20 * time.Millisecond
	agyPoolMu.Unlock()
	defer func() {
		ShutdownAntigravityPool()
		agyPoolMu.Lock()
		agyIdleRetire = origRetire
		agyReapInterval = origInterval
		agyPoolMu.Unlock()
	}()

	idleDone := make(chan struct{})
	idleSlot := getAgyPoolSlot("acct1:idle-sess")
	idleSlot.mu.Lock()
	idleSlot.proc = &agyProcess{
		done:     idleDone,
		parked:   map[string]chan agyMCPToolOutput{},
		lastUsed: time.Now().Add(-time.Second),
	}
	idleSlot.mu.Unlock()

	select {
	case <-idleDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for lazy idle reaper to retire idle agy process")
	}

	// Verify reaper stopped cleanly once pool became empty.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		agyPoolMu.Lock()
		running := agyReaperRunning
		count := len(agyPoolSlots)
		agyPoolMu.Unlock()
		if !running && count == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("expected idle reaper to stop when pool became empty")
}

func TestAntigravityStreamRefreshesLastUsedOnTurnEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake agy test is unix-only")
	}
	ShutdownAntigravityPool()
	defer ShutdownAntigravityPool()

	tmp := t.TempDir()
	t.Setenv("ARTEX_AGY_HOME", filepath.Join(tmp, "agy-home"))

	// Fake `agy` binary that takes 140ms to complete a turn (exceeding agyIdleRetire=80ms).
	script := `#!/bin/sh
set -eu
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*)
      id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1}}\n' "$id"
      ;;
    *'"method":"session/new"'*)
      id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
      printf '{"jsonrpc":"2.0","id":%s,"result":{"sessionId":"sess-slow","configOptions":[{"id":"model","options":[{"value":"gemini-4-carbon"}]}]}}\n' "$id"
      ;;
    *'"method":"session/set_config_option"'*)
      id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
      printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
      ;;
    *'"method":"session/prompt"'*)
      id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
      sleep 0.14
      printf '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess-slow","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"done"}}}}\n'
      printf '{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"end_turn"}}\n' "$id"
      ;;
  esac
done
`
	fakeBin := filepath.Join(tmp, "fake-agy")
	if err := os.WriteFile(fakeBin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake-agy: %v", err)
	}
	t.Setenv("ARTEX_AGY_PATH", fakeBin)

	agyPoolMu.Lock()
	origRetire, origInterval := agyIdleRetire, agyReapInterval
	agyIdleRetire = 80 * time.Millisecond
	agyReapInterval = time.Hour
	agyPoolMu.Unlock()
	defer func() {
		ShutdownAntigravityPool()
		agyPoolMu.Lock()
		agyIdleRetire = origRetire
		agyReapInterval = origInterval
		agyPoolMu.Unlock()
	}()

	cred := AgyCredential{
		Access:    "ya29.slow-turn",
		Refresh:   "1//refresh",
		ExpiresMS: time.Now().Add(time.Hour).UnixMilli(),
		Email:     "slow@example.com",
	}
	prov := newAntigravityProvider(Config{
		Model:       "gemini-4-carbon",
		AgyCredFunc: func(context.Context, bool) (AgyCredential, error) { return cred, nil },
	})
	turnStart := time.Now()
	msg, _, _, err := prov.Complete(context.Background(), CompletionRequest{
		SessionID: "slow-turn-session",
		Messages:  []Message{UserText("take a moment")},
	})
	if err != nil {
		t.Fatalf("Complete failed: %v", err)
	}
	if strings.TrimSpace(msg.Text()) != "done" {
		t.Fatalf("unexpected reply: %q", msg.Text())
	}

	// Immediately after the 140ms turn ends, lastUsed must reflect turn completion
	// (not turn start), so reapIdleAgyLocked must NOT kill the live process.
	agyPoolMu.Lock()
	killed := reapIdleAgyLocked(time.Now())
	slot := agyPoolSlots[AgyCredentialKey(cred)+":slow-turn-session"]
	agyPoolMu.Unlock()

	if len(killed) != 0 {
		for _, p := range killed {
			p.Kill()
		}
		t.Fatalf("reapIdleAgyLocked prematurely killed live process right after long turn")
	}
	if slot == nil {
		t.Fatal("expected pool slot to remain alive after long turn")
	}
	slot.mu.Lock()
	defer slot.mu.Unlock()
	if slot.proc == nil {
		t.Fatal("expected slot.proc to remain alive after long turn")
	}
	if slot.proc.lastUsed.Sub(turnStart) < 100*time.Millisecond {
		t.Fatalf("expected proc.lastUsed to be refreshed on turn end; delta from turnStart=%v", slot.proc.lastUsed.Sub(turnStart))
	}
	if slot.lastUsed.Sub(turnStart) < 100*time.Millisecond {
		t.Fatalf("expected slot.lastUsed to be refreshed on turn end; delta from turnStart=%v", slot.lastUsed.Sub(turnStart))
	}
}
