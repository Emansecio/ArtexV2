package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestInProcessServerNamingAndCall(t *testing.T) {
	tools := InProcessServer("calc", []InProcessTool{{
		Name:        "add",
		Description: "add",
		Schema:      map[string]any{"type": "object"},
		Handler: func(_ context.Context, args json.RawMessage) (string, error) {
			var a struct{ A, B float64 }
			json.Unmarshal(args, &a)
			return "sum", nil
		},
	}})
	if len(tools) != 1 || tools[0].Name() != "mcp__calc__add" {
		t.Fatalf("naming wrong: %v", tools[0].Name())
	}
	res, _ := tools[0].Call(context.Background(), []byte(`{"a":1,"b":2}`), nil)
	if res.Flatten() != "sum" {
		t.Fatalf("call result: %q", res.Flatten())
	}
}

// fakeServer speaks just enough MCP JSON-RPC to satisfy initialize, tools/list,
// and tools/call, over the provided pipes.
func fakeServer(in io.Reader, out io.Writer) {
	sc := bufio.NewScanner(in)
	enc := json.NewEncoder(out)
	for sc.Scan() {
		var req map[string]any
		if json.Unmarshal(sc.Bytes(), &req) != nil {
			continue
		}
		id, hasID := req["id"]
		method, _ := req["method"].(string)
		if !hasID {
			continue // notification
		}
		var result any
		switch method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2024-11-05"}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{
				"name": "echo", "description": "echoes", "inputSchema": map[string]any{"type": "object"},
			}}}
		case "tools/call":
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "echoed!"}}}
		}
		enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
}

func TestStdioClientOverPipes(t *testing.T) {
	cReader, sWriter := io.Pipe() // server -> client
	sReader, cWriter := io.Pipe() // client -> server
	go fakeServer(sReader, sWriter)

	ctx := context.Background()
	client, err := newClientWithConn(ctx, "srv", cReader, cWriter, nil)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	tools, err := client.Tools(ctx)
	if err != nil {
		t.Fatalf("tools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name() != "mcp__srv__echo" {
		t.Fatalf("tool naming: %+v", tools)
	}
	res, err := tools[0].Call(ctx, []byte(`{"msg":"hi"}`), nil)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !strings.Contains(res.Flatten(), "echoed!") {
		t.Fatalf("call result: %q", res.Flatten())
	}
}

func TestConnCallCleansPendingOnCancel(t *testing.T) {
	cReader, sWriter := io.Pipe()
	sReader, cWriter := io.Pipe()
	defer sWriter.Close()
	defer cWriter.Close()
	// Drain client writes without replying so ctx cancellation triggers.
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := sReader.Read(buf); err != nil {
				return
			}
		}
	}()

	cn := newConn(cReader, cWriter)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cn.call(ctx, "tools/call", map[string]any{}); err == nil {
		t.Fatalf("expected error on cancelled ctx")
	}
	cn.mu.Lock()
	left := len(cn.pending)
	cn.mu.Unlock()
	if left != 0 {
		t.Fatalf("pending map leaked %d entries after cancel", left)
	}
}

func TestToolsPagination(t *testing.T) {
	cReader, sWriter := io.Pipe()
	sReader, cWriter := io.Pipe()
	go func() {
		sc := bufio.NewScanner(sReader)
		enc := json.NewEncoder(sWriter)
		for sc.Scan() {
			var req map[string]any
			if json.Unmarshal(sc.Bytes(), &req) != nil {
				continue
			}
			id, hasID := req["id"]
			if !hasID {
				continue
			}
			method, _ := req["method"].(string)
			switch method {
			case "initialize":
				_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"protocolVersion": "2024-11-05"}})
			case "tools/list":
				params, _ := req["params"].(map[string]any)
				cursor, _ := params["cursor"].(string)
				if cursor == "" {
					_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
						"tools":      []any{map[string]any{"name": "t1", "description": "first", "inputSchema": map[string]any{"type": "object"}}},
						"nextCursor": "p2",
					}})
				} else {
					_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
						"tools": []any{map[string]any{"name": "t2", "description": "second", "inputSchema": map[string]any{"type": "object"}}},
					}})
				}
			}
		}
	}()
	client, err := newClientWithConn(context.Background(), "srv", cReader, cWriter, nil)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	tools, err := client.Tools(context.Background())
	if err != nil {
		t.Fatalf("tools: %v", err)
	}
	if len(tools) != 2 || tools[0].Name() != "mcp__srv__t1" || tools[1].Name() != "mcp__srv__t2" {
		t.Fatalf("unexpected paginated tools: %d", len(tools))
	}
}

func TestTailBufferCapsAndKeepsLatest(t *testing.T) {
	tb := newTailBuffer(8)
	_, _ = tb.Write([]byte("123456"))
	_, _ = tb.Write([]byte("7890"))
	if got := tb.String(); got != "34567890" {
		t.Fatalf("tail=%q want 34567890", got)
	}
}

func TestServerRequestDoesNotHijackPendingClientRPC(t *testing.T) {
	cReader, sWriter := io.Pipe()
	sReader, cWriter := io.Pipe()
	go func() {
		sc := bufio.NewScanner(sReader)
		enc := json.NewEncoder(sWriter)
		for sc.Scan() {
			var req map[string]any
			if json.Unmarshal(sc.Bytes(), &req) != nil {
				continue
			}
			id, hasID := req["id"]
			method, _ := req["method"].(string)
			if !hasID || method == "" {
				continue
			}
			switch method {
			case "initialize":
				_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"protocolVersion": "2024-11-05"}})
			case "tools/list":
				// Inject a server->client ping with the SAME id before sending the real response.
				_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "method": "ping", "params": map[string]any{}})
				_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
					"tools": []any{map[string]any{"name": "stats", "description": "stats", "inputSchema": map[string]any{"type": "object"}}},
				}})
			case "tools/call":
				params, _ := req["params"].(map[string]any)
				if _, ok := params["arguments"].(map[string]any); !ok {
					_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32602, "message": "arguments must be object"}})
					continue
				}
				_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
					"content": []any{map[string]any{"type": "text", "text": "ok-stats"}},
				}})
			}
		}
	}()
	client, err := newClientWithConn(context.Background(), "srv", cReader, cWriter, nil)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	tools, err := client.Tools(context.Background())
	if err != nil || len(tools) != 1 {
		t.Fatalf("tools failed (server ping may have hijacked RPC): len=%d err=%v", len(tools), err)
	}
	// Empty input must still send arguments: {} instead of null.
	res, err := tools[0].Call(context.Background(), nil, nil)
	if err != nil || res.IsError || res.Flatten() != "ok-stats" {
		t.Fatalf("empty-arg call failed: res=%+v err=%v", res, err)
	}
}

func TestServerPingZeroAndStringIDAnswered(t *testing.T) {
	cReader, sWriter := io.Pipe()
	sReader, cWriter := io.Pipe()
	defer sWriter.Close()
	defer cWriter.Close()

	pongs := make(chan string, 4)
	go func() {
		sc := bufio.NewScanner(sReader)
		enc := json.NewEncoder(sWriter)
		for sc.Scan() {
			var frame map[string]any
			if json.Unmarshal(sc.Bytes(), &frame) != nil {
				continue
			}
			method, _ := frame["method"].(string)
			if method == "" {
				// Response from client (e.g. pong to server ping)
				if rawID, ok := frame["id"]; ok {
					b, _ := json.Marshal(rawID)
					pongs <- string(b)
				}
				continue
			}
			id, hasID := frame["id"]
			if !hasID {
				continue
			}
			if method == "initialize" {
				_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"protocolVersion": "2024-11-05"}})
				// Send server-to-client pings with ID 0 and a string ID.
				_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": 0, "method": "ping", "params": map[string]any{}})
				_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": "srv-ping-1", "method": "ping", "params": map[string]any{}})
			}
		}
	}()

	if _, err := newClientWithConn(context.Background(), "srv", cReader, cWriter, nil); err != nil {
		t.Fatalf("init: %v", err)
	}
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		seen[<-pongs] = true
	}
	if !seen["0"] || !seen[`"srv-ping-1"`] {
		t.Fatalf("expected pongs for 0 and \"srv-ping-1\", got %+v", seen)
	}
}

func TestFormatMCPToolResultBlocksAndStructured(t *testing.T) {
	raw := json.RawMessage(`{
		"content": [
			{"type":"text","text":"line1"},
			{"type":"text","text":"line2"},
			{"type":"image","mimeType":"image/png","data":"abc"},
			{"type":"resource","resource":{"uri":"file:///a.txt","text":"res-body"}},
			{"type":"resource_link","uri":"https://example.com","name":"Example"}
		]
	}`)
	got, isErr, err := formatMCPToolResult(raw)
	if err != nil || isErr {
		t.Fatalf("formatMCPToolResult err=%v isErr=%v", err, isErr)
	}
	want := "line1\nline2\n[image: image/png]\nres-body\n[resource_link: Example (https://example.com)]"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	rawStruct := json.RawMessage(`{"content":[],"structuredContent":{"ok":true,"count":3}}`)
	got, _, err = formatMCPToolResult(rawStruct)
	if err != nil || got != `{"ok":true,"count":3}` {
		t.Fatalf("structured fallback got %q err=%v", got, err)
	}
}

func TestClientAliveAndStdioGracefulTermAndWaitDelay(t *testing.T) {
	// 1. Alive() tracks underlying conn closeCh.
	cReader, sWriter := io.Pipe()
	sReader, cWriter := io.Pipe()
	defer cWriter.Close()
	go fakeServer(sReader, sWriter)
	cl, err := newClientWithConn(context.Background(), "srv", cReader, cWriter, nil)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if !cl.Alive() {
		t.Fatal("expected connected client to be Alive()")
	}
	_ = sWriter.Close()
	deadline := time.Now().Add(2 * time.Second)
	for cl.Alive() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if cl.Alive() {
		t.Fatal("expected Alive() == false after transport EOF")
	}

	if runtime.GOOS == "windows" {
		return
	}

	// 2. stdioCloser.Close() gives SIGTERM grace window so server trap runs cleanup,
	// and cmd.WaitDelay prevents hanging when a detached grandchild inherits stderr.
	tmp := t.TempDir()
	cleanedFile := filepath.Join(tmp, "cleaned")
	grandchildPidFile := filepath.Join(tmp, "grandchild.pid")
	script := `#!/bin/sh
trap 'echo ok > "` + cleanedFile + `"; exit 0' TERM INT
# Spawn a detached background child that keeps stderr (fd 2) open briefly.
(sleep 4 >/dev/null 2>&2) &
echo $! > "` + grandchildPidFile + `"
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*)
      id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2024-11-05","capabilities":{}}}\n' "$id"
      ;;
  esac
done
`
	bin := filepath.Join(tmp, "fake-stdio-mcp")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	origGrace, origDelay := stdioCloseGrace, stdioWaitDelay
	stdioCloseGrace = 1500 * time.Millisecond
	stdioWaitDelay = 400 * time.Millisecond
	defer func() {
		stdioCloseGrace = origGrace
		stdioWaitDelay = origDelay
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	scl, err := NewStdioClient(ctx, "grace", bin, nil)
	if err != nil {
		t.Fatalf("NewStdioClient: %v", err)
	}
	if !scl.Alive() {
		t.Fatal("expected live stdio client")
	}
	start := time.Now()
	if err := scl.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 2500*time.Millisecond {
		t.Fatalf("Close() took too long (%v), expected bounded WaitDelay", elapsed)
	}
	data, err := os.ReadFile(cleanedFile)
	if err != nil || strings.TrimSpace(string(data)) != "ok" {
		t.Fatalf("expected SIGTERM cleanup trap to run before exit; err=%v data=%q", err, string(data))
	}
}

func TestConnCallDrainsResponseOnImmediateClose(t *testing.T) {
	// When a stdio MCP server writes a final JSON-RPC error or result and closes
	// stdout immediately, readLoop buffers the response in ch and closes closeCh
	// on EOF. conn.call must always return the buffered response instead of
	// racing with closeCh and returning "mcp: connection closed".
	for i := 0; i < 100; i++ {
		t.Run("error", func(t *testing.T) {
			cReader, sWriter := io.Pipe()
			sReader, cWriter := io.Pipe()
			defer cWriter.Close()
			go func() {
				defer sWriter.Close()
				sc := bufio.NewScanner(sReader)
				if !sc.Scan() {
					return
				}
				var req map[string]any
				if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
					return
				}
				_ = json.NewEncoder(sWriter).Encode(map[string]any{
					"jsonrpc": "2.0",
					"id":      req["id"],
					"error":   map[string]any{"code": -32001, "message": "fatal init error"},
				})
			}()

			cn := newConn(cReader, cWriter)
			_, err := cn.call(context.Background(), "initialize", map[string]any{})
			if err == nil || !strings.Contains(err.Error(), "fatal init error") {
				t.Fatalf("iter %d: got err=%v, want JSON-RPC error containing 'fatal init error'", i, err)
			}
		})

		t.Run("result", func(t *testing.T) {
			cReader, sWriter := io.Pipe()
			sReader, cWriter := io.Pipe()
			defer cWriter.Close()
			go func() {
				defer sWriter.Close()
				sc := bufio.NewScanner(sReader)
				if !sc.Scan() {
					return
				}
				var req map[string]any
				if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
					return
				}
				_ = json.NewEncoder(sWriter).Encode(map[string]any{
					"jsonrpc": "2.0",
					"id":      req["id"],
					"result":  map[string]any{"ok": true},
				})
			}()

			cn := newConn(cReader, cWriter)
			raw, err := cn.call(context.Background(), "tools/call", map[string]any{})
			if err != nil {
				t.Fatalf("iter %d: unexpected err=%v, want result", i, err)
			}
			if !strings.Contains(string(raw), `"ok":true`) {
				t.Fatalf("iter %d: got raw=%s, want {\"ok\":true}", i, string(raw))
			}
		})
	}
}
