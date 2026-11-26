package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestDevinCredentialAndToolCallID(t *testing.T) {
	if got := encodeDevinCredential("a.b.c"); got != "devin-session-token$a.b.c" {
		t.Fatalf("expected prefixed JWT, got %q", got)
	}
	if got := encodeDevinCredential("sk-ws-abc"); got != "sk-ws-abc" {
		t.Fatalf("expected raw key, got %q", got)
	}
	if got := encodeDevinCredential("devin-session-token$a.b.c"); got != "devin-session-token$a.b.c" {
		t.Fatalf("expected unchanged prefixed JWT, got %q", got)
	}

	short := devinWireToolCallID("call.1/2:3")
	if short != "call_1_2_3" {
		t.Fatalf("unexpected short sanitized id: %q", short)
	}
	long1 := devinWireToolCallID(strings.Repeat("a", 50) + "1")
	long2 := devinWireToolCallID(strings.Repeat("a", 50) + "2")
	if len(long1) != 40 || len(long2) != 40 || long1 == long2 {
		t.Fatalf("expected distinct 40-char hashed ids, got %q and %q", long1, long2)
	}
}

func TestDevinSchemaNormalization(t *testing.T) {
	raw := map[string]any{
		"type":  "object",
		"anyOf": []any{map[string]any{"required": []any{"a"}}},
		"properties": map[string]any{
			"count": map[string]any{
				"type": []any{"integer", "null"},
			},
		},
	}
	scaled := scalarizeSchemaTypes(raw).(map[string]any)
	props := scaled["properties"].(map[string]any)
	count := props["count"].(map[string]any)
	if count["type"] != "integer" || count["nullable"] != true {
		t.Fatalf("expected scalarized nullable integer, got %#v", count)
	}
	stripped := stripTopLevelCombinators(raw)
	if _, exists := stripped["anyOf"]; exists {
		t.Fatalf("expected top-level anyOf to be stripped")
	}
}

func TestDevinCatalogCollapseAndVariants(t *testing.T) {
	encodeFamily := func(label string, entries []struct {
		k string
		o uint64
		n string
	}, isDef bool) []byte {
		var b []byte
		b = protoAppendStringField(b, 1, label)
		for _, e := range entries {
			var val []byte
			val = protoAppendVarintField(val, 1, e.o)
			val = protoAppendStringField(val, 2, e.n)
			var pair []byte
			pair = protoAppendStringField(pair, 1, e.k)
			pair = protoAppendLenField(pair, 2, val)
			b = protoAppendLenField(b, 2, pair)
		}
		b = protoAppendBoolField(b, 3, isDef)
		return b
	}
	encodeCfg := func(uid, label string, ctxWin, maxOut uint64, fam []byte, defInFam bool) []byte {
		var info []byte
		info = protoAppendVarintField(info, 4, ctxWin)
		info = protoAppendVarintField(info, 13, maxOut)
		var b []byte
		b = protoAppendStringField(b, 1, label)
		b = protoAppendVarintField(b, 18, ctxWin)
		b = protoAppendStringField(b, 22, uid)
		b = protoAppendLenField(b, 23, info)
		b = protoAppendLenField(b, 30, fam)
		b = protoAppendBoolField(b, 31, defInFam)
		return b
	}

	var resp []byte
	// Standard Claude family (low + high)
	resp = protoAppendLenField(resp, 1, encodeCfg(
		"claude-sonnet-4-6-low", "Claude Sonnet 4.6 Low", 200_000, 64_000,
		encodeFamily("Claude Sonnet 4.6", []struct {
			k string
			o uint64
			n string
		}{{"Effort", 1, "Low"}}, false), false,
	))
	resp = protoAppendLenField(resp, 1, encodeCfg(
		"claude-sonnet-4-6-high", "Claude Sonnet 4.6 High", 200_000, 64_000,
		encodeFamily("Claude Sonnet 4.6", []struct {
			k string
			o uint64
			n string
		}{{"Effort", 2, "High"}}, false), true,
	))
	// 1M Fast variant must remain a distinct logical model
	resp = protoAppendLenField(resp, 1, encodeCfg(
		"claude-sonnet-4-6-1m-fast-high", "Claude Sonnet 4.6 1M Fast", 1_000_000, 64_000,
		encodeFamily("Claude Sonnet 4.6", []struct {
			k string
			o uint64
			n string
		}{
			{"1M Context", 1, "1M"},
			{"Fast Mode", 1, "Fast"},
			{"Effort", 2, "High"},
		}, false), true,
	))

	entries := NormalizeDevinModelConfigs(resp)
	if len(entries) != 2 {
		t.Fatalf("expected 2 collapsed entries, got %d: %#v", len(entries), entries)
	}
	byID := map[string]DevinCatalogEntry{}
	for _, e := range entries {
		byID[e.ID] = e
	}
	std, ok := byID["claude-sonnet-4-6"]
	if !ok {
		t.Fatalf("missing claude-sonnet-4-6 in %#v", byID)
	}
	if std.ChatModelUID("low") != "claude-sonnet-4-6-low" || std.ChatModelUID("high") != "claude-sonnet-4-6-high" {
		t.Fatalf("unexpected effort routing: %#v", std.EffortRoutes)
	}
	if len(std.ReasoningLevels) != 2 || std.ReasoningLevels[0] != "low" || std.ReasoningLevels[1] != "high" {
		t.Fatalf("unexpected reasoning levels: %#v", std.ReasoningLevels)
	}
	fast1m, ok := byID["claude-sonnet-4-6-1m-fast"]
	if !ok || fast1m.ContextWindow != 1_000_000 {
		t.Fatalf("expected 1M Fast variant with 1M context, got %#v", fast1m)
	}
}

func TestDevinStreamCachingUsageAndErrors(t *testing.T) {
	var seenCascadeIDs []string
	var seenPromptCache bool
	var jwtCalls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case devinAuthPath:
			jwtCalls.Add(1)
			var out []byte
			out = protoAppendStringField(out, 1, "header.eyJleHAiOjQxMDI0NDQ4MDB9.sig")
			w.Header().Set("content-type", "application/proto")
			_, _ = w.Write(out)
		case devinChatMessagePath:
			// Unwrap 5-byte Connect envelope
			if len(body) < 5 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			fields := decodeProtoFields(body[5:])
			for _, f := range fields {
				if f.number == 13 && len(f.bytes) > 0 {
					seenPromptCache = true
				}
				if f.number == 16 {
					seenCascadeIDs = append(seenCascadeIDs, f.str())
				}
			}

			// Emit thinking + text + tool call + usage + clean trailer
			var frame1 []byte
			frame1 = protoAppendStringField(frame1, 9, "thinking step")
			frame1 = protoAppendStringField(frame1, 10, "sig_abc")
			frame1 = protoAppendStringField(frame1, 3, "hello ")

			var usageProto []byte
			usageProto = protoAppendVarintField(usageProto, 2, 120)
			usageProto = protoAppendVarintField(usageProto, 3, 45)
			usageProto = protoAppendVarintField(usageProto, 4, 300)
			usageProto = protoAppendVarintField(usageProto, 5, 900)

			var frame2 []byte
			frame2 = protoAppendStringField(frame2, 3, "world")
			frame2 = protoAppendLenField(frame2, 7, usageProto)

			var buf bytes.Buffer
			buf.Write(frameConnectMessage(frame1))
			buf.Write(frameConnectMessage(frame2))
			// EndStream trailer (flags=0x02) with empty JSON {}
			trailer := frameConnectMessage([]byte(`{}`))
			trailer[0] = connectFlagEndStream
			buf.Write(trailer)

			w.Header().Set("content-type", "application/connect+proto")
			_, _ = w.Write(buf.Bytes())
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	p, err := NewProvider(Config{
		Format:  FormatDevin,
		BaseURL: srv.URL,
		APIKey:  "test-devin-key",
		Model:   "swe-1-6",
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	req := CompletionRequest{
		System:         []string{"You are helpful."},
		Messages:       []Message{UserText("hi")},
		PromptCacheKey: "session-42",
	}
	msg, stop, usage, err := p.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("Complete 1: %v", err)
	}
	if msg.Text() != "hello world" || stop != "end_turn" {
		t.Fatalf("unexpected reply: text=%q stop=%q", msg.Text(), stop)
	}
	if usage.InputTokens != 120 || usage.OutputTokens != 45 || usage.CacheWriteTokens != 300 || usage.CacheReadTokens != 900 {
		t.Fatalf("unexpected usage: %+v", usage)
	}
	if !seenPromptCache {
		t.Fatalf("expected prompt_cache_options field 13 to be sent")
	}

	// Second call with same PromptCacheKey should reuse cached JWT and emit identical cascade_id.
	_, _, _, err = p.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("Complete 2: %v", err)
	}
	if jwtCalls.Load() != 1 {
		t.Fatalf("expected JWT cache hit (1 call), got %d", jwtCalls.Load())
	}
	if len(seenCascadeIDs) != 2 || seenCascadeIDs[0] == "" || seenCascadeIDs[0] != seenCascadeIDs[1] {
		t.Fatalf("expected deterministic matching cascade_id, got %#v", seenCascadeIDs)
	}
}

func TestDevinTrailerErrorMapsToHTTPStatusError(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"code":    "resource_exhausted",
			"message": "rate limit reached",
		},
	})
	err := parseDevinTrailerError(payload, false, 0)
	var hse *HTTPStatusError
	if !errors.As(err, &hse) || hse.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429 HTTPStatusError, got %v", err)
	}
}

func TestDevinParallelToolCallsAndColdStartCatalogResolution(t *testing.T) {
	var seenModelUID string
	var catalogCalls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case devinAuthPath:
			resp := protoAppendStringField(nil, 1, "jwt.coldstart.sig")
			_, _ = w.Write(resp)
		case devinModelConfigsPath:
			catalogCalls.Add(1)
			encodeCfg := func(uid, label, famLabel, effortName string, defInFam bool) []byte {
				var val []byte
				val = protoAppendVarintField(val, 1, 1)
				val = protoAppendStringField(val, 2, effortName)
				var pair []byte
				pair = protoAppendStringField(pair, 1, "effort")
				pair = protoAppendLenField(pair, 2, val)
				var fam []byte
				fam = protoAppendStringField(fam, 1, famLabel)
				fam = protoAppendLenField(fam, 2, pair)
				var info []byte
				info = protoAppendVarintField(info, 4, 200_000)
				info = protoAppendVarintField(info, 13, 64_000)
				var b []byte
				b = protoAppendStringField(b, 1, label)
				b = protoAppendVarintField(b, 18, 200_000)
				b = protoAppendStringField(b, 22, uid)
				b = protoAppendLenField(b, 23, info)
				b = protoAppendLenField(b, 30, fam)
				b = protoAppendBoolField(b, 31, defInFam)
				return b
			}
			var payload []byte
			payload = protoAppendLenField(payload, 1, encodeCfg("claude-sonnet-4-6-low", "Claude Sonnet 4.6 Low", "Claude Sonnet 4.6", "Low", true))
			payload = protoAppendLenField(payload, 1, encodeCfg("claude-sonnet-4-6-high", "Claude Sonnet 4.6 High", "Claude Sonnet 4.6", "High", false))
			_, _ = w.Write(payload)
		case devinChatMessagePath:
			body, _ := io.ReadAll(r.Body)
			if len(body) >= 5 {
				for _, f := range decodeProtoFields(body[5:]) {
					if f.number == 21 {
						seenModelUID = f.str()
					}
				}
			}
			var buf bytes.Buffer
			// Tool 0 starts with empty args (backfilled at end of stream)
			var f1 []byte
			f1 = protoAppendLenField(f1, 6, encodeDevinToolCall("call_empty", "ping", ""))
			buf.Write(frameConnectMessage(f1))
			// Tool 1 starts with partial args
			var f2 []byte
			f2 = protoAppendLenField(f2, 6, encodeDevinToolCall("call_args", "echo", `{"msg":`))
			buf.Write(frameConnectMessage(f2))
			// Tool 2 interleaved delta
			var f3 []byte
			f3 = protoAppendLenField(f3, 6, encodeDevinToolCall("call_two", "sum", `{"x":1}`))
			buf.Write(frameConnectMessage(f3))
			// Tool 1 receives remaining args AFTER tool 2 started
			var f4 []byte
			f4 = protoAppendLenField(f4, 6, encodeDevinToolCall("call_args", "", `"hello"}`))
			buf.Write(frameConnectMessage(f4))

			trailer := frameConnectMessage([]byte(`{}`))
			trailer[0] = connectFlagEndStream
			buf.Write(trailer)
			w.Header().Set("content-type", "application/connect+proto")
			_, _ = w.Write(buf.Bytes())
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	// Ensure catalog cache is cold (simulates server restart).
	devinCatalogMu.Lock()
	clear(devinCatalogCache)
	devinCatalogMu.Unlock()

	p, err := NewProvider(Config{
		Format:          FormatDevin,
		BaseURL:         srv.URL,
		APIKey:          "cold-start-key",
		Model:           "claude-sonnet-4-6",
		ReasoningEffort: "high",
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	msg, stop, _, err := p.Complete(context.Background(), CompletionRequest{
		Messages: []Message{UserText("run tools")},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if catalogCalls.Load() != 1 {
		t.Fatalf("expected lazy catalog fetch (1 call), got %d", catalogCalls.Load())
	}
	if seenModelUID != "claude-sonnet-4-6-high" {
		t.Fatalf("expected resolved wire chat_model_uid claude-sonnet-4-6-high, got %q", seenModelUID)
	}
	if stop != "tool_use" {
		t.Fatalf("expected tool_use stop reason, got %q", stop)
	}
	uses := msg.ToolUses()
	if len(uses) != 3 {
		t.Fatalf("expected 3 tool uses, got %d: %+v", len(uses), uses)
	}
	if uses[0].ID != "call_empty" || string(uses[0].Input) != "{}" {
		t.Fatalf("tool 0 corrupted: %+v input=%s", uses[0], string(uses[0].Input))
	}
	if uses[1].ID != "call_args" || string(uses[1].Input) != `{"msg":"hello"}` {
		t.Fatalf("tool 1 corrupted: %+v input=%s", uses[1], string(uses[1].Input))
	}
	if uses[2].ID != "call_two" || string(uses[2].Input) != `{"x":1}` {
		t.Fatalf("tool 2 corrupted: %+v input=%s", uses[2], string(uses[2].Input))
	}
}
