package llm

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
)

const agyMCPProtocolVersion = "2025-06-18"

type agyMCPToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type agyMCPToolOutput struct {
	Text    string
	IsError bool
}

type agyParkedToolCall struct {
	CallID    string
	Name      string
	Arguments json.RawMessage
	Reply     chan agyMCPToolOutput
}

// agyMCPBridge is a loopback TCP NDJSON MCP server for `agy acp --ide-mcp-port`.
// It advertises the host's tool definitions and parks each `tools/call` on a
// per-call channel until the next agent turn delivers the tool result.
type agyMCPBridge struct {
	ln     net.Listener
	port   int
	tools  []agyMCPToolDef
	known  map[string]struct{}
	callCh chan agyParkedToolCall

	closeOnce sync.Once
	closed    chan struct{}
	seq       atomic.Uint64
}

func startAgyMCPBridge(tools []ToolSchema) (*agyMCPBridge, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("bind Antigravity MCP bridge: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	defs := make([]agyMCPToolDef, 0, len(tools))
	known := make(map[string]struct{}, len(tools))
	for _, t := range tools {
		if t.Name == "" {
			continue
		}
		schema := t.InputSchema
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		defs = append(defs, agyMCPToolDef{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: schema,
		})
		known[t.Name] = struct{}{}
	}
	b := &agyMCPBridge{
		ln:     ln,
		port:   port,
		tools:  defs,
		known:  known,
		callCh: make(chan agyParkedToolCall, 64),
		closed: make(chan struct{}),
	}
	go b.acceptLoop()
	return b, nil
}

func (b *agyMCPBridge) Port() int { return b.port }

func (b *agyMCPBridge) Close() {
	b.closeOnce.Do(func() {
		close(b.closed)
		_ = b.ln.Close()
	})
}

func (b *agyMCPBridge) acceptLoop() {
	for {
		conn, err := b.ln.Accept()
		if err != nil {
			return
		}
		go b.handleConn(conn)
	}
}

func (b *agyMCPBridge) handleConn(conn net.Conn) {
	defer conn.Close()
	var writeMu sync.Mutex
	sendJSON := func(v any) {
		raw, err := json.Marshal(v)
		if err != nil {
			return
		}
		raw = append(raw, '\n')
		writeMu.Lock()
		_, _ = conn.Write(raw)
		writeMu.Unlock()
	}

	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(line, &msg); err != nil || msg.Method == "" {
			continue
		}
		hasID := len(msg.ID) > 0 && string(msg.ID) != "null"

		switch msg.Method {
		case "initialize":
			if hasID {
				sendJSON(map[string]any{
					"jsonrpc": "2.0",
					"id":      msg.ID,
					"result": map[string]any{
						"protocolVersion": agyMCPProtocolVersion,
						"capabilities":    map[string]any{"tools": map[string]any{}},
						"serverInfo":      map[string]any{"name": "artex-ide-tools", "version": "0.1.0"},
					},
				})
			}
		case "notifications/initialized":
			// notification, no reply
		case "ping":
			if hasID {
				sendJSON(map[string]any{
					"jsonrpc": "2.0",
					"id":      msg.ID,
					"result":  map[string]any{},
				})
			}
		case "tools/list":
			if hasID {
				sendJSON(map[string]any{
					"jsonrpc": "2.0",
					"id":      msg.ID,
					"result":  map[string]any{"tools": b.tools},
				})
			}
		case "tools/call":
			if !hasID {
				continue
			}
			var params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(msg.Params, &params)
			if _, ok := b.known[params.Name]; !ok {
				sendJSON(map[string]any{
					"jsonrpc": "2.0",
					"id":      msg.ID,
					"result": map[string]any{
						"content": []map[string]any{{"type": "text", "text": "Unknown tool: " + params.Name}},
						"isError": true,
					},
				})
				continue
			}
			args := params.Arguments
			if len(args) == 0 {
				args = json.RawMessage(`{}`)
			}
			seq := b.seq.Add(1)
			callID := fmt.Sprintf("agy_call_%d_%s", seq, agyIDFragment(msg.ID))
			replyCh := make(chan agyMCPToolOutput, 1)
			parked := agyParkedToolCall{
				CallID:    callID,
				Name:      params.Name,
				Arguments: args,
				Reply:     replyCh,
			}
			select {
			case b.callCh <- parked:
			case <-b.closed:
				sendJSON(map[string]any{
					"jsonrpc": "2.0",
					"id":      msg.ID,
					"result": map[string]any{
						"content": []map[string]any{{"type": "text", "text": "Host bridge closed."}},
						"isError": true,
					},
				})
				continue
			}
			reqID := append(json.RawMessage(nil), msg.ID...)
			go func() {
				var out agyMCPToolOutput
				select {
				case o, ok := <-replyCh:
					if ok {
						out = o
					} else {
						out = agyMCPToolOutput{Text: "Tool call cancelled.", IsError: true}
					}
				case <-b.closed:
					out = agyMCPToolOutput{Text: "Tool call cancelled.", IsError: true}
				}
				sendJSON(map[string]any{
					"jsonrpc": "2.0",
					"id":      reqID,
					"result": map[string]any{
						"content": []map[string]any{{"type": "text", "text": out.Text}},
						"isError": out.IsError,
					},
				})
			}()
		default:
			if hasID {
				sendJSON(map[string]any{
					"jsonrpc": "2.0",
					"id":      msg.ID,
					"error":   map[string]any{"code": -32601, "message": "Method not found: " + msg.Method},
				})
			}
		}
	}
}

func agyIDFragment(raw json.RawMessage) string {
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		return strconv.FormatInt(n, 10)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if len(s) > 16 {
			s = s[:16]
		}
		return s
	}
	return "0"
}
