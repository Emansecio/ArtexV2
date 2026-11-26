package mcphttp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A Streamable HTTP server whose slow_call tool takes 3s, so the client's
// per-request timeout can be observed: 1s cuts the call off, 10s lets it finish.
func TestStreamableClientRequestTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-06-18","capabilities":{},"serverInfo":{"name":"t","version":"0"}}}`, req.ID)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"tools":[{"name":"slow_call","description":"slow","inputSchema":{"type":"object"}}]}}`, req.ID)
		case "tools/call":
			time.Sleep(3 * time.Second)
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":"done"}]}}`, req.ID)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// timeout_seconds=1: the slow call is cut off well before the server answers.
	c, err := New(ctx, "t", srv.URL, nil, false, 1)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := c.Tools(ctx); err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	start := time.Now()
	if _, err := c.Call(ctx, "slow_call", nil); err == nil {
		t.Fatal("expected the 1s-timeout call to fail")
	} else if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("call not cut off by the timeout: %v", elapsed)
	}
	_ = c.Close()

	// timeout_seconds=10: the same slow call completes.
	c2, err := New(ctx, "t", srv.URL, nil, false, 10)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	got, err := c2.Call(ctx, "slow_call", nil)
	if err != nil || got != "done" {
		t.Fatalf("Call() = %q, %v", got, err)
	}
	_ = c2.Close()

	// timeout_seconds=0: falls back to the 120s default (call completes).
	c3, err := New(ctx, "t", srv.URL, nil, false, 0)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if got, err := c3.Call(ctx, "slow_call", nil); err != nil || got != "done" {
		t.Fatalf("default-timeout Call() = %q, %v", got, err)
	}
	_ = c3.Close()
}

// initRequest captures what the client sent for one MCP request.
type initRequest struct {
	method, header, bodyVersion string
}

func recordRequest(r *http.Request) (rpcRequest, initRequest) {
	var req rpcRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	rec := initRequest{method: req.Method, header: r.Header.Get("MCP-Protocol-Version")}
	if p, ok := req.Params.(map[string]any); ok {
		rec.bodyVersion, _ = p["protocolVersion"].(string)
	}
	return req, rec
}

// TestStreamableClientProtocolVersions uses spec-shaped servers that each support
// one version: they answer initialize with their own version, accept a request
// without MCP-Protocol-Version, and reject any other header value. The client
// must send no version header on initialize and the negotiated one afterwards.
func TestStreamableClientProtocolVersions(t *testing.T) {
	for _, supported := range []string{"2025-11-25", "2025-06-18", "2025-03-26"} {
		t.Run(supported, func(t *testing.T) {
			var seen []initRequest
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				req, rec := recordRequest(r)
				seen = append(seen, rec)
				if rec.header != "" && rec.header != supported {
					w.WriteHeader(http.StatusBadRequest)
					fmt.Fprint(w, `{"code":"UNSUPPORTED_PROTOCOL_VERSION"}`)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				switch req.Method {
				case "initialize":
					fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":%q,"capabilities":{},"serverInfo":{"name":"t","version":"0"}}}`, req.ID, supported)
				case "notifications/initialized":
					w.WriteHeader(http.StatusAccepted)
				case "tools/list":
					fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"tools":[]}}`, req.ID)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()

			c, err := New(context.Background(), "t", srv.URL, nil, false, 0)
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			defer c.Close()
			if _, err := c.Tools(context.Background()); err != nil {
				t.Fatalf("tools/list: %v", err)
			}
			if c.protocol != supported {
				t.Fatalf("negotiated protocol = %q", c.protocol)
			}
			var inits int
			for _, rec := range seen {
				if rec.method == "initialize" {
					inits++
					if rec.header != "" {
						t.Fatalf("initialize carried MCP-Protocol-Version %q", rec.header)
					}
					if rec.bodyVersion != protocolVersion {
						t.Fatalf("initialize offered %q, want %q", rec.bodyVersion, protocolVersion)
					}
					continue
				}
				if rec.header != supported {
					t.Fatalf("%s carried MCP-Protocol-Version %q, want %q", rec.method, rec.header, supported)
				}
			}
			if inits != 1 {
				t.Fatalf("initialize attempts = %d, want 1", inits)
			}
		})
	}
}

// TestStreamableClientVersionRejectionForms keeps the fallback loop honest for
// servers that reject the initialize body version instead of negotiating: an
// HTTP 400 "Unsupported protocol version", the JSON-RPC -32602 error from the
// spec's example, and an unrelated failure that must not trigger a retry.
func TestStreamableClientVersionRejectionForms(t *testing.T) {
	cases := []struct {
		name     string
		reject   func(w http.ResponseWriter, id int)
		fallback bool
	}{
		{"http 400", func(w http.ResponseWriter, _ int) {
			http.Error(w, "Bad Request: Unsupported protocol version", http.StatusBadRequest)
		}, true},
		{"json-rpc -32602", func(w http.ResponseWriter, id int) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"error":{"code":-32602,"message":"Unsupported protocol version","data":{"supported":["2025-06-18"],"requested":"2025-11-25"}}}`, id)
		}, true},
		{"unrelated 401", func(w http.ResponseWriter, _ int) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var inits []initRequest
			var after []initRequest
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				req, rec := recordRequest(r)
				if req.Method == "initialize" {
					inits = append(inits, rec)
					if rec.bodyVersion != "2025-06-18" {
						tc.reject(w, req.ID)
						return
					}
				} else {
					after = append(after, rec)
				}
				w.Header().Set("Content-Type", "application/json")
				switch req.Method {
				case "initialize":
					fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-06-18","capabilities":{},"serverInfo":{"name":"t","version":"0"}}}`, req.ID)
				case "notifications/initialized":
					w.WriteHeader(http.StatusAccepted)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()

			c, err := New(context.Background(), "t", srv.URL, nil, false, 0)
			for _, rec := range inits {
				if rec.header != "" {
					t.Fatalf("initialize carried MCP-Protocol-Version %q", rec.header)
				}
			}
			if !tc.fallback {
				if err == nil {
					_ = c.Close()
					t.Fatal("expected connect error")
				}
				if len(inits) != 1 {
					t.Fatalf("unrelated error retried: %d initialize attempts", len(inits))
				}
				return
			}
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			defer c.Close()
			if len(inits) != 2 || inits[0].bodyVersion != protocolVersion || inits[1].bodyVersion != "2025-06-18" {
				t.Fatalf("fallback attempts = %+v", inits)
			}
			if c.protocol != "2025-06-18" {
				t.Fatalf("negotiated protocol = %q", c.protocol)
			}
			if len(after) == 0 || after[0].method != "notifications/initialized" || after[0].header != "2025-06-18" {
				t.Fatalf("requests after initialize = %+v", after)
			}
		})
	}
}

func TestDecodeRPCCapsBody(t *testing.T) {
	ok := `{"jsonrpc":"2.0","id":1,"result":{}}`
	got, err := decodeRPC(strings.NewReader(ok))
	if err != nil || got == nil || got.ID != 1 {
		t.Fatalf("small body: got=%+v err=%v", got, err)
	}
	if _, err := decodeRPC(strings.NewReader(strings.Repeat("x", mcpMaxRPCBytes+1))); err == nil {
		t.Fatal("oversize body must fail")
	}
}

func TestParseSSECapsPayload(t *testing.T) {
	chunk := strings.Repeat("a", 64*1024)
	var b strings.Builder
	for b.Len() <= mcpMaxRPCBytes {
		b.WriteString("data: ")
		b.WriteString(chunk)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	if _, err := parseSSE(strings.NewReader(b.String()), 1); err == nil {
		t.Fatal("oversize SSE payload must fail")
	}
}

func TestLegacySSEDisconnectWakesAllConcurrentWaitersAndClose(t *testing.T) {
	dropSSE := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sse":
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			fmt.Fprint(w, "event: endpoint\ndata: /message\n\n")
			flusher.Flush()
			// Answer initialize on the SSE stream.
			time.Sleep(20 * time.Millisecond)
			fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"protocolVersion\":\"2024-11-05\"}}\n\n")
			flusher.Flush()
			<-dropSSE
		case "/message":
			w.WriteHeader(http.StatusAccepted)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := NewSSE(ctx, "legacy", srv.URL+"/sse", nil, false)
	if err != nil {
		t.Fatalf("NewSSE: %v", err)
	}
	defer c.Close()

	// Start 3 concurrent in-flight calls and drop the SSE stream.
	errs := make(chan error, 3)
	for i := 0; i < 3; i++ {
		go func() {
			_, cerr := c.Call(ctx, "slow", nil)
			errs <- cerr
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(dropSSE)

	for i := 0; i < 3; i++ {
		select {
		case cerr := <-errs:
			if cerr == nil || !strings.Contains(cerr.Error(), "mcp sse stream:") {
				t.Fatalf("waiter %d expected mcp sse stream error, got %v", i, cerr)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("waiter %d hung after SSE disconnect", i)
		}
	}

	// Subsequent call after disconnect must also fail fast.
	start := time.Now()
	if _, err := c.Call(ctx, "after_drop", nil); err == nil || time.Since(start) > time.Second {
		t.Fatalf("post-disconnect call should fail fast: err=%v elapsed=%v", err, time.Since(start))
	}
}

func TestStreamableHTTP404ClearsSessionAndCloseBounded(t *testing.T) {
	blockDelete := make(chan struct{})
	defer close(blockDelete)
	var expired bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			select {
			case <-blockDelete:
			case <-r.Context().Done():
			}
			return
		}
		var req rpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "sess-1")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-11-25"}}`, req.ID)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			if expired {
				http.Error(w, "session not found", http.StatusNotFound)
				return
			}
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}}`, req.ID)
		}
	}))
	defer srv.Close()

	c, err := New(context.Background(), "s", srv.URL, nil, false, 60)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := c.Call(context.Background(), "t", nil)
	if err != nil || got != "a\nb" {
		t.Fatalf("Call() = %q, %v; want %q", got, err, "a\nb")
	}

	// Simulate expired session returning 404.
	expired = true
	if _, err := c.Call(context.Background(), "t", nil); err == nil || !strings.Contains(err.Error(), "mcp http 404") {
		t.Fatalf("expected mcp http 404 error, got %v", err)
	}
	c.mu.Lock()
	sid, initd, exp := c.sessionID, c.initialized, c.sessionExpired
	c.mu.Unlock()
	if sid != "" || initd || !exp {
		t.Fatalf("expected session cleared & sessionExpired set on 404, got sid=%q init=%v exp=%v", sid, initd, exp)
	}

	// Verify Close() with an active session and hung DELETE returns within bounded timeout (~3s), not 60s.
	c2, err := New(context.Background(), "s", srv.URL, nil, false, 60)
	if err != nil {
		t.Fatalf("New c2: %v", err)
	}
	start := time.Now()
	_ = c2.Close()
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Close() took %v; expected bounded <=3s timeout", elapsed)
	}
}

func TestStreamableHTTP404FailsFastForSubsequentCallsWithoutHeaderlessPOST(t *testing.T) {
	var expired bool
	var headerlessCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "sess-1")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-11-25"}}`, req.ID)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			if r.Header.Get("Mcp-Session-Id") == "" {
				headerlessCalls++
				http.Error(w, "missing Mcp-Session-Id", http.StatusBadRequest)
				return
			}
			if expired {
				http.Error(w, "session not found", http.StatusNotFound)
				return
			}
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":"ok"}]}}`, req.ID)
		}
	}))
	defer srv.Close()

	c, err := New(context.Background(), "s", srv.URL, nil, false, 10)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	expired = true
	if _, err := c.Call(context.Background(), "t", nil); err == nil || !strings.Contains(err.Error(), "session expired") {
		t.Fatalf("first post-expiry call err = %v, want session expired", err)
	}
	// A sibling call arriving before pool eviction must fail fast with session expired
	// and never send a headerless POST that would produce an unretried HTTP 400.
	if _, err := c.Call(context.Background(), "t", nil); err == nil || !strings.Contains(err.Error(), "mcp http 404: session expired") {
		t.Fatalf("second post-expiry call err = %v, want mcp http 404: session expired", err)
	}
	if headerlessCalls != 0 {
		t.Fatalf("saw %d headerless POSTs after session expiry, want 0", headerlessCalls)
	}
}

func TestInsecureTransportClonesDefaultTransportSettings(t *testing.T) {
	tr := insecureTransport()
	if tr.TLSClientConfig == nil || !tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatalf("expected InsecureSkipVerify=true, got %+v", tr.TLSClientConfig)
	}
	if tr.Proxy == nil {
		t.Fatal("expected Proxy function preserved from DefaultTransport")
	}
	if tr.TLSHandshakeTimeout <= 0 {
		t.Fatalf("expected positive TLSHandshakeTimeout, got %v", tr.TLSHandshakeTimeout)
	}
	if tr.IdleConnTimeout <= 0 {
		t.Fatalf("expected positive IdleConnTimeout, got %v", tr.IdleConnTimeout)
	}
	if !tr.ForceAttemptHTTP2 {
		t.Fatal("expected ForceAttemptHTTP2=true")
	}
	if def, ok := http.DefaultTransport.(*http.Transport); ok && def.TLSClientConfig != nil && def.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("insecureTransport must not mutate http.DefaultTransport")
	}
}

func TestLegacySSERoundTripDrainsAndClosesPostBodyBeforeWaitingOnSSE(t *testing.T) {
	sendReply := make(chan int, 2)
	var postConns []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sse":
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			fmt.Fprint(w, "event: endpoint\ndata: /message\n\n")
			flusher.Flush()
			for id := range sendReply {
				if id == 1 {
					fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"protocolVersion\":\"2024-11-05\"}}\n\n")
				} else {
					fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"ok-%d\"}]}}\n\n", id, id)
				}
				flusher.Flush()
			}
		case "/message":
			var req rpcRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			postConns = append(postConns, r.RemoteAddr)
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte("accepted"))
			if req.Method == "initialize" {
				sendReply <- req.ID
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer func() {
		close(sendReply)
		srv.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := NewSSE(ctx, "legacy", srv.URL+"/sse", nil, false)
	if err != nil {
		t.Fatalf("NewSSE: %v", err)
	}
	defer c.Close()

	// Issue call 1 and wait briefly while it is blocked waiting for its SSE result.
	// Because the POST response body was drained & closed immediately, a second
	// concurrent POST can reuse the same keep-alive TCP connection instead of
	// opening a brand-new connection while call 1 is in flight.
	res1 := make(chan string, 1)
	go func() {
		got, _ := c.Call(ctx, "slow1", nil)
		res1 <- got
	}()
	time.Sleep(40 * time.Millisecond)

	res2 := make(chan string, 1)
	go func() {
		got, _ := c.Call(ctx, "slow2", nil)
		res2 <- got
	}()
	time.Sleep(40 * time.Millisecond)

	// Unblock both SSE replies (IDs 2 and 3 after initialize ID=1).
	sendReply <- 2
	sendReply <- 3
	if got := <-res1; got != "ok-2" {
		t.Fatalf("call 1 = %q, want ok-2", got)
	}
	if got := <-res2; got != "ok-3" {
		t.Fatalf("call 2 = %q, want ok-3", got)
	}
	// All 4 POSTs (initialize, notifications/initialized, slow1, slow2) should have
	// reused the exact same keep-alive client RemoteAddr.
	if len(postConns) < 4 {
		t.Fatalf("expected >=4 POSTs, got %d", len(postConns))
	}
	if postConns[len(postConns)-1] != postConns[len(postConns)-2] {
		t.Fatalf("expected concurrent legacy SSE POSTs to reuse keep-alive connection, got remote addrs %v", postConns)
	}
}
