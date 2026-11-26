// Package mcp integrates Model Context Protocol tools (FR-10). It speaks
// JSON-RPC 2.0 over a newline-delimited stdio transport to external MCP servers
// and also supports in-process servers (Go functions). Every MCP tool is
// adapted to a tool.CoreTool named mcp__<server>__<tool> (FR-10.2/10.3).
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
)

// ToolName builds the three-segment MCP tool name.
func ToolName(server, name string) string { return "mcp__" + server + "__" + name }

// --- in-process server ---

// InProcessTool is a Go-native tool exposed under the MCP naming convention.
type InProcessTool struct {
	Name        string
	Description string
	Schema      map[string]any
	Handler     func(ctx context.Context, args json.RawMessage) (string, error)
}

// InProcessServer adapts a set of in-process tools to CoreTools (FR-10.3).
func InProcessServer(server string, tools []InProcessTool) []tool.CoreTool {
	out := make([]tool.CoreTool, 0, len(tools))
	for _, it := range tools {
		it := it
		out = append(out, tool.Build(tool.Spec{
			Name:        ToolName(server, it.Name),
			Description: it.Description,
			Schema:      it.Schema,
			Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
				return permission.AskUser("call MCP tool " + ToolName(server, it.Name) + "?")
			},
			Run: func(ctx context.Context, in json.RawMessage, tc *tool.ToolContext) (tool.Result, error) {
				text, err := it.Handler(ctx, in)
				if err != nil {
					return tool.Errorf("Error: " + err.Error()), nil
				}
				return tool.Text(tool.Capture(tc, text)), nil
			},
		}))
	}
	return out
}

// --- JSON-RPC transport ---

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method,omitempty"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

func hasRPCID(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && string(trimmed) != "null"
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("mcp rpc error %d: %s", e.Code, e.Message) }

// conn is a JSON-RPC 2.0 connection over a newline-delimited byte stream. It is
// transport-agnostic (stdio, pipe, socket) for testability.
type conn struct {
	w          io.Writer
	mu         sync.Mutex
	nextID     int
	pending    map[int]chan rpcResponse
	closeCh    chan struct{}
	once       sync.Once
	stderrTail func() string
}

func newConn(r io.Reader, w io.Writer) *conn {
	c := &conn{w: w, pending: map[int]chan rpcResponse{}, closeCh: make(chan struct{})}
	go c.readLoop(r)
	return c
}

func (c *conn) readLoop(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var resp rpcResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			continue // notifications / non-response frames are ignored
		}
		if !hasRPCID(resp.ID) {
			continue
		}
		if resp.Method != "" {
			// Server-to-client JSON-RPC request (e.g. ping): never let it collide
			// with a client-initiated pending RPC ID. Reply asynchronously so
			// readLoop never blocks on c.w and deadlocks bidirectional pipes.
			if resp.Method == "ping" {
				pingID := append(json.RawMessage(nil), bytes.TrimSpace(resp.ID)...)
				go func() {
					c.mu.Lock()
					_ = c.writeLocked(map[string]any{"jsonrpc": "2.0", "id": pingID, "result": map[string]any{}})
					c.mu.Unlock()
				}()
			}
			continue
		}
		var id int
		if err := json.Unmarshal(resp.ID, &id); err != nil || id <= 0 {
			continue
		}
		c.mu.Lock()
		ch := c.pending[id]
		delete(c.pending, id)
		c.mu.Unlock()
		if ch != nil {
			ch <- resp
		}
	}
	c.once.Do(func() { close(c.closeCh) })
}

func (c *conn) notify(method string, params any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writeLocked(rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
}

func (c *conn) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan rpcResponse, 1)
	c.pending[id] = ch
	err := c.writeLocked(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}
	c.mu.Unlock()
	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	case <-c.closeCh:
		select {
		case resp := <-ch:
			if resp.Error != nil {
				return nil, resp.Error
			}
			return resp.Result, nil
		default:
		}
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		if c.stderrTail != nil {
			if tail := c.stderrTail(); tail != "" {
				return nil, fmt.Errorf("mcp: connection closed (stderr: %s)", tail)
			}
		}
		return nil, fmt.Errorf("mcp: connection closed")
	case resp := <-ch:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	}
}

func (c *conn) writeLocked(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = c.w.Write(data)
	return err
}

// --- client ---

// Client is a connection to one MCP server.
type Client struct {
	server string
	conn   *conn
	closer io.Closer
}

type remoteTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations *struct {
		ReadOnlyHint bool `json:"readOnlyHint"`
	} `json:"annotations,omitempty"`
}

// initialize performs the MCP handshake.
func (c *Client) initialize(ctx context.Context) error {
	_, err := c.conn.call(ctx, "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "norma", "version": "0.2"},
	})
	if err != nil {
		return err
	}
	return c.conn.notify("notifications/initialized", map[string]any{})
}

// Tools lists the server's tools (following MCP nextCursor pagination when
// present) and adapts them to CoreTools.
func (c *Client) Tools(ctx context.Context) ([]tool.CoreTool, error) {
	const maxPages = 20
	var out []tool.CoreTool
	cursor := ""
	seenCursors := map[string]bool{}
	for page := 0; page < maxPages; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := c.conn.call(ctx, "tools/list", params)
		if err != nil {
			return nil, err
		}
		var res struct {
			Tools      []remoteTool `json:"tools"`
			NextCursor string       `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			return nil, err
		}
		for _, rt := range res.Tools {
			out = append(out, c.wrap(rt))
		}
		if res.NextCursor == "" || seenCursors[res.NextCursor] {
			break
		}
		seenCursors[res.NextCursor] = true
		cursor = res.NextCursor
	}
	return out, nil
}

func (c *Client) wrap(rt remoteTool) tool.CoreTool {
	schema := rt.InputSchema
	if schema == nil {
		schema = map[string]any{"type": "object"}
	}
	full := ToolName(c.server, rt.Name)
	// The stdio transport multiplexes concurrent JSON-RPC IDs, so a tool
	// declaring readOnlyHint may run in parallel like the HTTP/SSE clients do.
	readOnly := rt.Annotations != nil && rt.Annotations.ReadOnlyHint
	spec := tool.Spec{
		Name:        full,
		Description: rt.Description,
		Schema:      schema,
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.AskUser("call MCP tool " + full + "?")
		},
		Run: func(ctx context.Context, in json.RawMessage, tc *tool.ToolContext) (tool.Result, error) {
			var args any
			if len(in) > 0 {
				_ = json.Unmarshal(in, &args)
			}
			if args == nil {
				args = map[string]any{}
			}
			raw, err := c.conn.call(ctx, "tools/call", map[string]any{"name": rt.Name, "arguments": args})
			if err != nil {
				return tool.Errorf("Error: " + err.Error()), nil
			}
			text, isErr, err := formatMCPToolResult(raw)
			if err != nil {
				return tool.Errorf("Error: bad MCP response: " + err.Error()), nil
			}
			return tool.Result{Content: []llm.ContentBlock{llm.TextBlock(tool.Capture(tc, text))}, IsError: isErr}, nil
		},
	}
	if readOnly {
		spec.ReadOnly = func(json.RawMessage) bool { return true }
		spec.Concurrent = func(json.RawMessage) bool { return true }
	}
	return tool.Build(spec)
}

func formatMCPToolResult(raw json.RawMessage) (string, bool, error) {
	var res struct {
		Content []struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			MimeType    string `json:"mimeType"`
			URI         string `json:"uri"`
			Name        string `json:"name"`
			Description string `json:"description"`
			Resource    *struct {
				URI      string `json:"uri"`
				MimeType string `json:"mimeType"`
				Text     string `json:"text"`
				Blob     string `json:"blob"`
			} `json:"resource"`
		} `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		IsError           bool            `json:"isError"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", false, err
	}
	var sb strings.Builder
	for _, blk := range res.Content {
		var part string
		switch blk.Type {
		case "", "text":
			part = blk.Text
		case "image":
			if blk.MimeType != "" {
				part = fmt.Sprintf("[image: %s]", blk.MimeType)
			} else {
				part = "[image]"
			}
		case "audio":
			if blk.MimeType != "" {
				part = fmt.Sprintf("[audio: %s]", blk.MimeType)
			} else {
				part = "[audio]"
			}
		case "resource":
			if blk.Resource != nil {
				if blk.Resource.Text != "" {
					part = blk.Resource.Text
				} else if blk.Resource.URI != "" {
					if blk.Resource.MimeType != "" {
						part = fmt.Sprintf("[resource: %s (%s)]", blk.Resource.URI, blk.Resource.MimeType)
					} else {
						part = fmt.Sprintf("[resource: %s]", blk.Resource.URI)
					}
				} else if blk.Resource.Blob != "" {
					if blk.Resource.MimeType != "" {
						part = fmt.Sprintf("[resource: %s]", blk.Resource.MimeType)
					} else {
						part = "[resource: binary]"
					}
				}
			}
		case "resource_link":
			if blk.URI != "" {
				label := blk.URI
				if blk.Name != "" {
					label = fmt.Sprintf("%s (%s)", blk.Name, blk.URI)
				}
				if blk.Description != "" {
					label += ": " + blk.Description
				}
				part = "[resource_link: " + label + "]"
			}
		default:
			part = blk.Text
		}
		if part == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(part)
	}
	if sb.Len() == 0 {
		trimmed := bytes.TrimSpace(res.StructuredContent)
		if len(trimmed) > 0 && string(trimmed) != "null" {
			sb.Write(trimmed)
		}
	}
	return sb.String(), res.IsError, nil
}

// Alive reports whether the underlying transport connection is still open.
func (c *Client) Alive() bool {
	if c == nil || c.conn == nil {
		return false
	}
	select {
	case <-c.conn.closeCh:
		return false
	default:
		return true
	}
}

// Close shuts down the client's transport.
func (c *Client) Close() error {
	if c.closer != nil {
		return c.closer.Close()
	}
	return nil
}
