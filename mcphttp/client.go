// Package mcphttp contains remote Streamable HTTP and legacy SSE MCP clients.
//
// The core mcp package only speaks stdio; this adapts an HTTP/JSON-RPC MCP server
// to the same tool.CoreTool shape (mcp__server__tool) WITHOUT touching the SDK.
// It implements the MCP "Streamable HTTP" transport: JSON-RPC over HTTP POST, with
// the server free to answer either as application/json or a text/event-stream (SSE)
// carrying the response. Custom headers (e.g. Authorization) come from the MCP
// row's env map, so a bearer token is configured as {"Authorization":"Bearer …"}.
package mcphttp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

const protocolVersion = "2025-11-25"
const legacySSEProtocolVersion = "2024-11-05"

// mcpMaxRPCBytes is the largest JSON-RPC response (JSON body or SSE data
// payload) the client will buffer. A remote MCP server is untrusted.
const mcpMaxRPCBytes = 8 << 20

// ToolName mirrors mcp.ToolName so remote tools share the mcp__server__tool scheme.
func ToolName(server, name string) string { return "mcp__" + server + "__" + name }

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Method  string          `json:"method,omitempty"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("mcp rpc error %d: %s", e.Code, e.Message) }

// Client is a connection to one remote MCP server. New uses Streamable HTTP;
// NewSSE uses the legacy GET /sse + POST /message transport.
type Client struct {
	server  string
	url     string
	headers map[string]string
	http    *http.Client

	mu        sync.Mutex
	nextID    int
	sessionID string
	// initialized is set once initialize succeeds. Until then the
	// MCP-Protocol-Version header is omitted: the spec requires it on requests
	// after initialization, and the offered version already travels in the
	// initialize body.
	initialized bool
	// sessionExpired is set when the server returns HTTP 404 for an active
	// session so concurrent callers fail fast with a session-expired error
	// instead of issuing headerless POSTs that servers reject with HTTP 400.
	sessionExpired bool
	closed         bool

	// legacySSE is the pre-2025 MCP SSE transport used by servers such as GSL5.
	legacySSE    bool
	messageURL   string
	streamBody   io.ReadCloser
	streamCancel context.CancelFunc
	streamDone   chan struct{}
	pendingMu    sync.Mutex
	pending      map[int]chan *rpcResponse
	streamErr    error
	protocol     string
}

func normalizeHeaders(headers map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(headers))
	for key, value := range headers {
		clean := strings.TrimSpace(key)
		if clean == "" || strings.ContainsAny(clean, "\r\n") {
			return nil, fmt.Errorf("invalid HTTP header name %q", key)
		}
		out[clean] = value
	}
	return out, nil
}

// originKey returns the scheme, host and effective port of u, in a form two
// URLs can be compared with. Empty means u is not an absolute http(s) URL with
// a host.
func originKey(u *url.URL) string {
	if u == nil {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	var port string
	switch scheme {
	case "http":
		port = "80"
	case "https":
		port = "443"
	default:
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return ""
	}
	if p := u.Port(); p != "" {
		port = p
	}
	return scheme + "://" + net.JoinHostPort(host, port)
}

// sameOrigin reports whether u has the same scheme, host and effective port as
// base.
func sameOrigin(base, u *url.URL) bool {
	key := originKey(base)
	return key != "" && key == originKey(u)
}

// redirectPolicy refuses a redirect that leaves the origin of the configured
// endpoint, so a remote MCP server cannot retarget authenticated requests.
func redirectPolicy(base *url.URL) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("mcp: stopped after 10 redirects")
		}
		if !sameOrigin(base, req.URL) {
			return fmt.Errorf("mcp: refusing a redirect to another origin: %s", req.URL.Redacted())
		}
		return nil
	}
}

// parseMCPURL parses and validates a configured MCP endpoint URL.
func parseMCPURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("mcp url: %w", err)
	}
	if originKey(u) == "" {
		return nil, fmt.Errorf("mcp url must be an absolute http or https URL with a host: %q", raw)
	}
	return u, nil
}

// insecureTransport returns an *http.Transport with TLS verification disabled
// while preserving DefaultTransport's proxy, dial timeout/keepalive, TLS
// handshake timeout, HTTP/2, and idle-connection settings.
func insecureTransport() *http.Transport {
	var tr *http.Transport
	if def, ok := http.DefaultTransport.(*http.Transport); ok && def != nil {
		tr = def.Clone()
	} else {
		tr = &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		}
	}
	if tr.TLSClientConfig == nil {
		tr.TLSClientConfig = &tls.Config{}
	} else {
		tr.TLSClientConfig = tr.TLSClientConfig.Clone()
	}
	tr.TLSClientConfig.InsecureSkipVerify = true
	return tr
}

// New connects to a remote MCP endpoint and performs the initialize handshake.
// headers are sent on every request (Authorization, custom API keys, …).
// When insecure is true, TLS certificate verification is skipped so servers that
// present a self-signed certificate can still be reached (issue #108).
// timeoutSeconds bounds each request (handshake, tools/list, one tools/call); 0
// falls back to the 120s default. Analysis servers with minutes-long tool calls
// raise it per server.
func New(ctx context.Context, server, url string, headers map[string]string, insecure bool, timeoutSeconds int) (*Client, error) {
	cleanHeaders, err := normalizeHeaders(headers)
	if err != nil {
		return nil, err
	}
	timeout := 120 * time.Second
	if timeoutSeconds > 0 {
		timeout = time.Duration(timeoutSeconds) * time.Second
	}
	base, err := parseMCPURL(url)
	if err != nil {
		return nil, err
	}
	hc := &http.Client{Timeout: timeout, CheckRedirect: redirectPolicy(base)}
	if insecure {
		hc.Transport = insecureTransport()
	}
	c := &Client{
		server:   server,
		url:      url,
		headers:  cleanHeaders,
		http:     hc,
		protocol: protocolVersion,
	}
	if err := c.initialize(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// NewSSE connects to the legacy MCP SSE transport: a long-lived GET /sse
// announces a per-session POST /message endpoint, while JSON-RPC responses are
// delivered asynchronously as SSE message events.
func NewSSE(ctx context.Context, server, sseURL string, headers map[string]string, insecure bool) (*Client, error) {
	cleanHeaders, err := normalizeHeaders(headers)
	if err != nil {
		return nil, err
	}
	base, err := parseMCPURL(sseURL)
	if err != nil {
		return nil, err
	}
	hc := &http.Client{CheckRedirect: redirectPolicy(base)} // the SSE stream is intentionally long-lived.
	if insecure {
		hc.Transport = insecureTransport()
	}
	streamCtx, cancel := context.WithCancel(context.Background())
	// Respect caller ctx during initial connect + endpoint discovery + initialize;
	// detach once connected so the long-lived SSE stream survives after handshake.
	stopHandshakeCancel := context.AfterFunc(ctx, cancel)
	req, err := http.NewRequestWithContext(streamCtx, http.MethodGet, sseURL, nil)
	if err != nil {
		stopHandshakeCancel()
		cancel()
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range cleanHeaders {
		req.Header.Set(k, v)
	}
	resp, err := hc.Do(req)
	if err != nil {
		stopHandshakeCancel()
		cancel()
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		stopHandshakeCancel()
		cancel()
		return nil, fmt.Errorf("mcp sse http %d: %s", resp.StatusCode, readSnippet(resp.Body))
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), mcpMaxRPCBytes)
	endpoint, err := readSSEEndpoint(sc, base)
	if err != nil {
		resp.Body.Close()
		stopHandshakeCancel()
		cancel()
		return nil, err
	}
	c := &Client{
		server:       server,
		url:          sseURL,
		headers:      cleanHeaders,
		http:         hc,
		legacySSE:    true,
		messageURL:   endpoint,
		streamBody:   resp.Body,
		streamCancel: cancel,
		streamDone:   make(chan struct{}),
		pending:      make(map[int]chan *rpcResponse),
		protocol:     legacySSEProtocolVersion,
	}
	go c.readLegacySSE(sc)
	if err := c.initialize(ctx); err != nil {
		stopHandshakeCancel()
		_ = c.Close()
		return nil, err
	}
	if !stopHandshakeCancel() {
		// Handshake ctx expired right as initialize finished; streamCtx was cancelled.
		_ = c.Close()
		return nil, ctx.Err()
	}
	return c, nil
}

// readSSEEndpoint reads the message endpoint announced by the SSE stream. The
// endpoint must stay on the origin of the configured SSE URL: the POSTs that
// follow carry the operator's headers, and a remote server must not be able to
// retarget them.
func readSSEEndpoint(sc *bufio.Scanner, base *url.URL) (string, error) {
	eventType := ""
	var dataBuf strings.Builder
	resolveCandidate := func(candidate string) (string, error) {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			return "", nil
		}
		u, err := url.Parse(candidate)
		if err != nil {
			return "", fmt.Errorf("mcp sse endpoint URL: %w", err)
		}
		if u.User != nil {
			return "", errors.New("mcp sse endpoint must not carry userinfo")
		}
		abs := u
		if !u.IsAbs() {
			abs = base.ResolveReference(u)
		}
		if !sameOrigin(base, abs) {
			return "", fmt.Errorf("mcp sse endpoint %q is not on the configured origin", abs.Redacted())
		}
		return abs.String(), nil
	}
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" {
			if dataBuf.Len() > 0 && (eventType == "" || eventType == "endpoint") {
				if ep, err := resolveCandidate(dataBuf.String()); err != nil || ep != "" {
					return ep, err
				}
			}
			eventType = ""
			dataBuf.Reset()
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "event:") {
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			if eventType != "" && eventType != "endpoint" {
				continue
			}
			val := strings.TrimPrefix(line, "data:")
			val = strings.TrimPrefix(val, " ")
			if dataBuf.Len() > 0 {
				dataBuf.WriteByte('\n')
			}
			dataBuf.WriteString(val)
			if dataBuf.Len() > mcpMaxRPCBytes {
				return "", fmt.Errorf("mcp sse endpoint payload exceeds %d bytes", mcpMaxRPCBytes)
			}
		}
	}
	if dataBuf.Len() > 0 && (eventType == "" || eventType == "endpoint") {
		if ep, err := resolveCandidate(dataBuf.String()); err != nil || ep != "" {
			return ep, err
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("mcp sse endpoint: %w", err)
	}
	return "", fmt.Errorf("mcp sse endpoint: %w", io.EOF)
}

func (c *Client) readLegacySSE(sc *bufio.Scanner) {
	var err error
	defer func() {
		if err == nil {
			err = io.EOF
		} else if errors.Is(err, context.Canceled) {
			err = errors.New("connection closed")
		}
		c.pendingMu.Lock()
		c.streamErr = err
		c.pendingMu.Unlock()
		close(c.streamDone)
	}()
	eventType := ""
	var dataBuf strings.Builder
	flush := func() {
		defer func() {
			eventType = ""
			dataBuf.Reset()
		}()
		if dataBuf.Len() == 0 {
			return
		}
		if eventType != "" && eventType != "message" {
			return
		}
		var response rpcResponse
		if uerr := json.Unmarshal([]byte(strings.TrimSpace(dataBuf.String())), &response); uerr != nil || response.ID == 0 || response.Method != "" {
			return
		}
		c.pendingMu.Lock()
		ch := c.pending[response.ID]
		delete(c.pending, response.ID)
		c.pendingMu.Unlock()
		if ch != nil {
			ch <- &response
		}
	}
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "event:") {
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			val := strings.TrimPrefix(line, "data:")
			val = strings.TrimPrefix(val, " ")
			if dataBuf.Len() > 0 {
				dataBuf.WriteByte('\n')
			}
			dataBuf.WriteString(val)
			if dataBuf.Len() > mcpMaxRPCBytes {
				err = fmt.Errorf("sse payload exceeds %d bytes", mcpMaxRPCBytes)
				return
			}
		}
	}
	flush()
	err = sc.Err()
}

func (c *Client) getStreamErr() error {
	c.pendingMu.Lock()
	err := c.streamErr
	c.pendingMu.Unlock()
	if err == nil {
		return io.EOF
	}
	return err
}

func (c *Client) initialize(ctx context.Context) error {
	versions := []string{protocolVersion, "2025-06-18"}
	if c.legacySSE {
		versions = []string{legacySSEProtocolVersion}
	}
	for i, version := range versions {
		c.mu.Lock()
		c.protocol = version
		c.mu.Unlock()
		result, err := c.call(ctx, "initialize", map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "artex", "version": "0.2"},
		})
		if err != nil {
			if i+1 < len(versions) && isUnsupportedVersion(err) {
				continue
			}
			return err
		}
		var negotiated struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		c.mu.Lock()
		if json.Unmarshal(result, &negotiated) == nil && negotiated.ProtocolVersion != "" {
			c.protocol = negotiated.ProtocolVersion
		}
		c.initialized = true
		c.sessionExpired = false
		c.mu.Unlock()
		return c.notify(ctx, "notifications/initialized", map[string]any{})
	}
	return fmt.Errorf("mcp: no supported protocol version")
}

// isUnsupportedVersion reports whether err is a server rejecting the offered
// protocol version: the spec's "Unsupported protocol version" (JSON-RPC -32602,
// or an HTTP 400 on the MCP-Protocol-Version header) or the
// UNSUPPORTED_PROTOCOL_VERSION code some servers return instead.
func isUnsupportedVersion(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unsupported protocol version") ||
		strings.Contains(msg, "unsupported_protocol_version")
}

// Tools lists the server's tools (following MCP nextCursor pagination when
// present) and adapts them to CoreTools.
func (c *Client) Tools(ctx context.Context) ([]actool.CoreTool, error) {
	const maxPages = 20
	var out []actool.CoreTool
	cursor := ""
	seenCursors := map[string]bool{}
	for page := 0; page < maxPages; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := c.call(ctx, "tools/list", params)
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

type remoteTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations *struct {
		ReadOnlyHint bool `json:"readOnlyHint"`
	} `json:"annotations,omitempty"`
}

func (c *Client) wrap(rt remoteTool) actool.CoreTool {
	schema := rt.InputSchema
	if schema == nil {
		schema = map[string]any{"type": "object"}
	}
	full := ToolName(c.server, rt.Name)
	readOnly := rt.Annotations != nil && rt.Annotations.ReadOnlyHint
	concurrent := readOnly || os.Getenv("ARTEX_MCP_CONCURRENT") == "1"
	spec := actool.Spec{
		Name:        full,
		Description: rt.Description,
		Schema:      schema,
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.AskUser("call MCP tool " + full + "?")
		},
		Run: func(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
			var args any
			if len(in) > 0 {
				_ = json.Unmarshal(in, &args)
			}
			if args == nil {
				args = map[string]any{}
			}
			raw, err := c.call(ctx, "tools/call", map[string]any{"name": rt.Name, "arguments": args})
			if err != nil {
				return actool.Errorf("Error: " + err.Error()), nil
			}
			text, isErr, err := formatMCPToolResult(raw, mcpMaxRPCBytes)
			if err != nil {
				return actool.Errorf("Error: bad MCP response: " + err.Error()), nil
			}
			// Consistent with built-in/custom tools: oversized output goes through Capture —
			// truncated at the session's MaxOutputChars (default 30000), and when
			// ToolOutputDir is configured the full output spills to disk leaving only the
			// head + pointer, so a large MCP result does not flood the context whole.
			return actool.Result{Content: []llm.ContentBlock{llm.TextBlock(actool.Capture(tc, text))}, IsError: isErr}, nil
		},
	}
	if readOnly {
		spec.ReadOnly = func(json.RawMessage) bool { return true }
	}
	if concurrent {
		spec.Concurrent = func(json.RawMessage) bool { return true }
	}
	return actool.Build(spec)
}

func formatMCPToolResult(raw json.RawMessage, maxBytes int) (string, bool, error) {
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
	var b strings.Builder
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
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(part)
		if maxBytes > 0 && b.Len() > maxBytes {
			break
		}
	}
	if b.Len() == 0 {
		trimmed := bytes.TrimSpace(res.StructuredContent)
		if len(trimmed) > 0 && string(trimmed) != "null" {
			if maxBytes > 0 && len(trimmed) > maxBytes {
				trimmed = trimmed[:maxBytes]
			}
			b.Write(trimmed)
		}
	}
	return b.String(), res.IsError, nil
}

// Call invokes one tool and returns the concatenated text of its content blocks
// (ScopeSentry answers with a single JSON text block). Unlike the CoreTool wrapper
// from Tools(), this bypasses the agent permission (AskUser) layer — it is meant
// for backend batch jobs (e.g. asset sync) that call MCP tools programmatically.
func (c *Client) Call(ctx context.Context, tool string, args any) (string, error) {
	raw, err := c.call(ctx, "tools/call", map[string]any{"name": tool, "arguments": args})
	if err != nil {
		return "", err
	}
	text, isErr, err := formatMCPToolResult(raw, mcpMaxRPCBytes)
	if err != nil {
		return "", fmt.Errorf("bad MCP response: %w", err)
	}
	if isErr {
		return text, fmt.Errorf("mcp tool %q error: %s", tool, text)
	}
	return text, nil
}

// Alive reports whether the underlying transport/session is still usable.
func (c *Client) Alive() bool {
	if c == nil {
		return false
	}
	if c.legacySSE {
		if c.streamDone == nil {
			return false
		}
		select {
		case <-c.streamDone:
			return false
		default:
			return true
		}
	}
	c.mu.Lock()
	alive := !c.closed && !c.sessionExpired
	c.mu.Unlock()
	return alive
}

// Close terminates the MCP session best-effort (DELETE with the session id, per the
// Streamable HTTP spec). Servers that don't track sessions simply ignore it.
func (c *Client) Close() error {
	c.mu.Lock()
	c.closed = true
	sid := c.sessionID
	c.mu.Unlock()
	defer func() {
		if c.http != nil && c.http.Transport != nil {
			c.http.CloseIdleConnections()
		}
	}()
	if c.legacySSE {
		if c.streamCancel != nil {
			c.streamCancel()
		}
		if c.streamBody != nil {
			_ = c.streamBody.Close()
		}
		select {
		case <-c.streamDone:
		case <-time.After(time.Second):
		}
		return nil
	}
	if sid == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.url, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Mcp-Session-Id", sid)
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	if resp, err := c.http.Do(req); err == nil {
		resp.Body.Close()
	}
	return nil
}

// --- JSON-RPC over Streamable HTTP ---

func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.mu.Unlock()
	resp, err := c.roundTrip(ctx, rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}, true)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, fmt.Errorf("mcp: empty response for %s", method)
	}
	if resp.Error != nil {
		return nil, resp.Error
	}
	return resp.Result, nil
}

func (c *Client) notify(ctx context.Context, method string, params any) error {
	_, err := c.roundTrip(ctx, rpcRequest{JSONRPC: "2.0", Method: method, Params: params}, false)
	return err
}

// roundTrip POSTs one JSON-RPC frame. When expectResp is false (a notification) the
// server replies 202 Accepted with no body. Otherwise the reply is parsed from JSON
// or from an SSE stream, whichever the server chose.
func (c *Client) roundTrip(ctx context.Context, body rpcRequest, expectResp bool) (*rpcResponse, error) {
	if c.legacySSE {
		return c.legacyRoundTrip(ctx, body, expectResp)
	}
	c.mu.Lock()
	expired := c.sessionExpired
	sid := c.sessionID
	initialized, version := c.initialized, c.protocol
	c.mu.Unlock()
	if expired && body.Method != "initialize" {
		return nil, fmt.Errorf("mcp http 404: session expired: session previously invalidated")
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if initialized && version != "" {
		req.Header.Set("MCP-Protocol-Version", version)
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	if sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Capture the session id the server assigns on initialize.
	if sid == "" {
		if got := resp.Header.Get("Mcp-Session-Id"); got != "" {
			c.mu.Lock()
			c.sessionID = got
			c.sessionExpired = false
			c.mu.Unlock()
		}
	}

	if resp.StatusCode == http.StatusNotFound && sid != "" {
		c.mu.Lock()
		if c.sessionID == sid {
			c.sessionID = ""
			c.initialized = false
			c.sessionExpired = true
		}
		c.mu.Unlock()
		return nil, fmt.Errorf("mcp http 404: session expired: %s", readSnippet(resp.Body))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("mcp http %d: %s", resp.StatusCode, readSnippet(resp.Body))
	}
	if !expectResp {
		// Drain the 202 ack so the connection keeps keep-alive reuse.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, nil // notification — no JSON-RPC body to parse
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return parseSSE(resp.Body, body.ID)
	}
	return decodeRPC(resp.Body)
}

// decodeRPC reads one JSON-RPC response, refusing a body larger than
// mcpMaxRPCBytes so a remote server cannot force an unbounded allocation.
func decodeRPC(r io.Reader) (*rpcResponse, error) {
	data, err := io.ReadAll(io.LimitReader(r, mcpMaxRPCBytes+1))
	if err != nil {
		return nil, fmt.Errorf("mcp: read json response: %w", err)
	}
	if len(data) > mcpMaxRPCBytes {
		return nil, fmt.Errorf("mcp: json response exceeds %d bytes", mcpMaxRPCBytes)
	}
	var out rpcResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("mcp: decode json response: %w", err)
	}
	return &out, nil
}

// legacyRoundTrip posts to the endpoint announced by GET /sse. The HTTP POST
// only acknowledges receipt; the JSON-RPC response arrives on the SSE stream.
// Calls run concurrently: the shared reader demultiplexes responses to callers
// by JSON-RPC ID through c.pending, so serializing the POSTs is unnecessary.
func (c *Client) legacyRoundTrip(ctx context.Context, body rpcRequest, expectResp bool) (*rpcResponse, error) {
	select {
	case <-c.streamDone:
		return nil, fmt.Errorf("mcp sse stream: %w", c.getStreamErr())
	default:
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	var responseCh chan *rpcResponse
	if expectResp {
		responseCh = make(chan *rpcResponse, 1)
		c.pendingMu.Lock()
		c.pending[body.ID] = responseCh
		c.pendingMu.Unlock()
		defer func() {
			c.pendingMu.Lock()
			delete(c.pending, body.ID)
			c.pendingMu.Unlock()
		}()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.messageURL, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := readSnippet(resp.Body)
		_ = resp.Body.Close()
		return nil, fmt.Errorf("mcp sse http %d: %s", resp.StatusCode, snippet)
	}
	// Drain and close immediately so the HTTP/1.1 connection returns to the
	// keep-alive pool instead of remaining pinned open while waiting for the
	// asynchronous JSON-RPC response on the SSE stream.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	if !expectResp {
		return nil, nil
	}
	select {
	case response := <-responseCh:
		return response, nil
	case <-c.streamDone:
		select {
		case response := <-responseCh:
			return response, nil
		default:
		}
		return nil, fmt.Errorf("mcp sse stream: %w", c.getStreamErr())
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// parseSSE reads an SSE stream and returns the first data frame that is a JSON-RPC
// response matching wantID (server-to-client requests/notifications are skipped).
func parseSSE(r io.Reader, wantID int) (*rpcResponse, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var dataBuf strings.Builder
	flush := func() (*rpcResponse, bool) {
		if dataBuf.Len() == 0 {
			return nil, false
		}
		payload := dataBuf.String()
		dataBuf.Reset()
		var out rpcResponse
		if err := json.Unmarshal([]byte(payload), &out); err != nil {
			return nil, false
		}
		if out.ID != wantID || out.Method != "" {
			return nil, false
		}
		return &out, true
	}
	for sc.Scan() {
		line := sc.Text()
		if line == "" { // event boundary
			if resp, ok := flush(); ok {
				return resp, nil
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataBuf.WriteString(strings.TrimSpace(line[len("data:"):]))
			if dataBuf.Len() > mcpMaxRPCBytes {
				return nil, fmt.Errorf("mcp: sse payload exceeds %d bytes", mcpMaxRPCBytes)
			}
		}
	}
	if resp, ok := flush(); ok {
		return resp, nil
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("mcp: no matching response in event stream")
}

func readSnippet(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 2048))
	return strings.TrimSpace(string(b))
}
