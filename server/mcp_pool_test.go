package server

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

type fakeMCPClient struct {
	closed  atomic.Int32
	dead    atomic.Bool
	onTools func(context.Context) ([]actool.CoreTool, error)
}

func (f *fakeMCPClient) Alive() bool {
	return !f.dead.Load() && f.closed.Load() == 0
}

func (f *fakeMCPClient) Tools(ctx context.Context) ([]actool.CoreTool, error) {
	if f.onTools != nil {
		return f.onTools(ctx)
	}
	return nil, nil
}
func (f *fakeMCPClient) Close() error {
	f.closed.Add(1)
	return nil
}

func TestMCPPoolIdleReaper(t *testing.T) {
	t.Setenv("ARTEX_MCP_IDLE_TTL_MIN", "0")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := newMCPPool(ctx)
	staleClient := &fakeMCPClient{}
	freshClient := &fakeMCPClient{}

	stale := &pooledMCPEntry{id: 1, client: staleClient}
	stale.setLastUsedForTest(time.Now().Add(-5 * time.Minute).UnixNano())
	fresh := &pooledMCPEntry{id: 2, client: freshClient}
	fresh.touch()

	p.mu.Lock()
	p.entries[1] = stale
	p.entries[2] = fresh
	p.mu.Unlock()

	p.reapIdle(1 * time.Minute)

	if staleClient.closed.Load() != 1 {
		t.Fatalf("expected stale client closed once, got %d", staleClient.closed.Load())
	}
	if freshClient.closed.Load() != 0 {
		t.Fatalf("expected fresh client untouched, got %d", freshClient.closed.Load())
	}
	p.mu.Lock()
	_, hasStale := p.entries[1]
	_, hasFresh := p.entries[2]
	p.mu.Unlock()
	if hasStale || !hasFresh {
		t.Fatalf("unexpected pool state after reap: hasStale=%v hasFresh=%v", hasStale, hasFresh)
	}
}

func TestMCPPoolAcquireRefreshProtectsAgainstReapIdle(t *testing.T) {
	t.Setenv("ARTEX_MCP_IDLE_TTL_MIN", "0")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := newMCPPool(ctx)
	m := &db.MCPServer{ID: 7, Transport: "stdio", Command: "echo"}
	priv := ExecPrivilege{}
	fp := mcpFingerprint(m, priv)

	reapedDuringRefresh := false
	cl := &fakeMCPClient{}
	cl.onTools = func(context.Context) ([]actool.CoreTool, error) {
		p.reapIdle(1 * time.Minute)
		reapedDuringRefresh = cl.closed.Load() > 0
		return nil, nil
	}

	ent := &pooledMCPEntry{
		id:          m.ID,
		fingerprint: fp,
		client:      cl,
		toolsAt:     time.Now().Add(-2 * mcpToolsCacheTTL),
	}
	ent.setLastUsedForTest(time.Now().Add(-5 * time.Minute).UnixNano())

	p.mu.Lock()
	p.entries[m.ID] = ent
	p.mu.Unlock()

	if _, err := p.Acquire(ctx, m, priv); err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}
	if reapedDuringRefresh || cl.closed.Load() != 0 {
		t.Fatalf("entry was reaped/closed during tools refresh (closed=%d)", cl.closed.Load())
	}
	if ent.inFlightCount() != 0 {
		t.Fatalf("expected inFlight=0 after Acquire, got %d", ent.inFlightCount())
	}
	p.mu.Lock()
	cur := p.entries[m.ID]
	p.mu.Unlock()
	if cur != ent {
		t.Fatalf("expected original entry to remain pooled")
	}
}

func TestMCPPoolAutoReconnectAfterIdleReapAndReadOnlyRetry(t *testing.T) {
	t.Setenv("ARTEX_MCP_IDLE_TTL_MIN", "0")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var dials atomic.Int32
	var calls atomic.Int32

	p := newMCPPool(ctx)
	p.connectFn = func(_ context.Context, _ *db.MCPServer, _ ExecPrivilege) (mcpClient, error) {
		gen := dials.Add(1)
		cl := &fakeMCPClient{}
		cl.onTools = func(context.Context) ([]actool.CoreTool, error) {
			return []actool.CoreTool{
				actool.Build(actool.Spec{
					Name:        "mcp__secrag__kb_search",
					Description: "search",
					Schema:      map[string]any{"type": "object"},
					ReadOnly:    func(json.RawMessage) bool { return true },
					Concurrent:  func(json.RawMessage) bool { return true },
					Run: func(_ context.Context, _ json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
						n := calls.Add(1)
						// Simulate a dead pipe on gen 2's first call to verify transparent read-only retry.
						if gen == 2 && n == 2 {
							return actool.Errorf("Error: mcp: connection closed"), nil
						}
						return actool.Text("ok-gen-" + string(rune('0'+gen))), nil
					},
				}),
			}, nil
		}
		return cl, nil
	}

	m := &db.MCPServer{ID: 8, Name: "secrag", Transport: "stdio", Command: "secrag-mcp"}
	tools, err := p.Acquire(ctx, m, ExecPrivilege{})
	if err != nil || len(tools) != 1 {
		t.Fatalf("initial Acquire failed: len=%d err=%v", len(tools), err)
	}
	kbSearch := tools[0]

	// 1. Normal call on gen 1.
	res, err := kbSearch.Call(ctx, []byte(`{"query":"ssrf"}`), nil)
	if err != nil || res.IsError || !strings.Contains(res.Flatten(), "ok-gen-1") {
		t.Fatalf("gen1 call failed: res=%+v err=%v", res, err)
	}

	// 2. Idle reap kills gen 1 while the long-running agent keeps holding kbSearch.
	p.mu.Lock()
	p.entries[m.ID].setLastUsedForTest(time.Now().Add(-20 * time.Minute).UnixNano())
	p.mu.Unlock()
	p.reapIdle(10 * time.Minute)

	// 3. Next call transparently reconnects (gen 2), hits a simulated transport drop on gen 2,
	// evicts gen 2, retries once on gen 3 (because kb_search is ReadOnly), and succeeds!
	res, err = kbSearch.Call(ctx, []byte(`{"query":"ssti"}`), nil)
	if err != nil || res.IsError || !strings.Contains(res.Flatten(), "ok-gen-3") {
		t.Fatalf("expected transparent reconnect + retry to gen 3, got res=%+v err=%v", res, err)
	}
	if dials.Load() != 3 {
		t.Fatalf("expected 3 dials, got %d", dials.Load())
	}
}

func TestMCPPoolDomainErrorDoesNotEvictOrKillWarmServer(t *testing.T) {
	t.Setenv("ARTEX_MCP_IDLE_TTL_MIN", "0")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var dials atomic.Int32
	var calls atomic.Int32
	cl := &fakeMCPClient{}
	cl.onTools = func(context.Context) ([]actool.CoreTool, error) {
		return []actool.CoreTool{
			actool.Build(actool.Spec{
				Name:        "mcp__secrag__kb_search",
				Description: "search",
				Schema:      map[string]any{"type": "object"},
				ReadOnly:    func(json.RawMessage) bool { return true },
				Run: func(_ context.Context, _ json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
					calls.Add(1)
					// Domain errors mentioning "disclosed" or "port closed" must NOT trigger eviction/SIGKILL.
					return actool.Errorf("Target port 443 is closed; CVE not yet disclosed"), nil
				},
			}),
		}, nil
	}

	p := newMCPPool(ctx)
	p.connectFn = func(_ context.Context, _ *db.MCPServer, _ ExecPrivilege) (mcpClient, error) {
		dials.Add(1)
		return cl, nil
	}

	m := &db.MCPServer{ID: 9, Name: "secrag", Transport: "stdio", Command: "secrag-mcp", Enabled: true}
	tools, err := p.Acquire(ctx, m, ExecPrivilege{})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	res, err := tools[0].Call(ctx, []byte(`{"query":"test"}`), nil)
	if err != nil || !res.IsError {
		t.Fatalf("expected domain error result, got res=%+v err=%v", res, err)
	}
	if cl.closed.Load() != 0 || dials.Load() != 1 || calls.Load() != 1 {
		t.Fatalf("domain error must not kill/evict/retry warm client: closed=%d dials=%d calls=%d",
			cl.closed.Load(), dials.Load(), calls.Load())
	}

	// Disabling the server and invalidating prevents reconnect from respawning it.
	disabled := *m
	disabled.Enabled = false
	p.lookupFn = func(id int64) (*db.MCPServer, bool) {
		if id == m.ID {
			return &disabled, true
		}
		return nil, false
	}
	p.Invalidate(m.ID)
	res, _ = tools[0].Call(ctx, []byte(`{"query":"test"}`), nil)
	if !res.IsError || !strings.Contains(res.Flatten(), "disabled or removed") {
		t.Fatalf("expected disabled-server refusal on reconnect, got %+v", res)
	}
	if dials.Load() != 1 {
		t.Fatalf("expected no respawn after disable, got dials=%d", dials.Load())
	}
}

func TestMCPPool404SessionExpiredEvictsAndRetries(t *testing.T) {
	t.Setenv("ARTEX_MCP_IDLE_TTL_MIN", "0")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var dials atomic.Int32
	p := newMCPPool(ctx)
	p.connectFn = func(_ context.Context, _ *db.MCPServer, _ ExecPrivilege) (mcpClient, error) {
		gen := dials.Add(1)
		cl := &fakeMCPClient{}
		cl.onTools = func(context.Context) ([]actool.CoreTool, error) {
			return []actool.CoreTool{
				actool.Build(actool.Spec{
					Name:        "mcp__remote__ping",
					Description: "ping",
					Schema:      map[string]any{"type": "object"},
					ReadOnly:    func(json.RawMessage) bool { return true },
					Run: func(_ context.Context, _ json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
						if gen == 1 {
							return actool.Errorf("Error: mcp http 404: session expired"), nil
						}
						return actool.Text("pong-gen-2"), nil
					},
				}),
			}, nil
		}
		return cl, nil
	}

	m := &db.MCPServer{ID: 10, Name: "remote", Transport: "http", URL: "http://127.0.0.1:9999/mcp", Enabled: true}
	tools, err := p.Acquire(ctx, m, ExecPrivilege{})
	if err != nil || len(tools) != 1 {
		t.Fatalf("Acquire: len=%d err=%v", len(tools), err)
	}
	res, err := tools[0].Call(ctx, []byte(`{}`), nil)
	if err != nil || res.IsError || res.Flatten() != "pong-gen-2" {
		t.Fatalf("expected 404 eviction + retry to gen 2, got res=%+v err=%v", res, err)
	}
	if dials.Load() != 2 {
		t.Fatalf("expected 2 dials, got %d", dials.Load())
	}
}

func TestMCPPoolStaleDialInvalidationAndNameFingerprint(t *testing.T) {
	t.Setenv("ARTEX_MCP_IDLE_TTL_MIN", "0")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m1 := &db.MCPServer{ID: 11, Name: "oldname", Transport: "stdio", Command: "echo", Enabled: true}
	m2 := &db.MCPServer{ID: 11, Name: "newname", Transport: "stdio", Command: "echo", Enabled: true}
	if mcpFingerprint(m1, ExecPrivilege{}) == mcpFingerprint(m2, ExecPrivilege{}) {
		t.Fatal("mcpFingerprint must include server Name")
	}

	dialStarted := make(chan struct{})
	releaseDial := make(chan struct{})
	staleClient := &fakeMCPClient{}
	p := newMCPPool(ctx)
	p.connectFn = func(_ context.Context, _ *db.MCPServer, _ ExecPrivilege) (mcpClient, error) {
		close(dialStarted)
		<-releaseDial
		return staleClient, nil
	}

	errCh := make(chan error, 1)
	go func() {
		_, err := p.Acquire(ctx, m1, ExecPrivilege{})
		errCh <- err
	}()
	<-dialStarted
	p.Invalidate(m1.ID)
	close(releaseDial)

	if err := <-errCh; err == nil {
		t.Fatal("expected invalidated dial to return error")
	}
	if staleClient.closed.Load() != 1 {
		t.Fatalf("expected invalidated dial client closed once, got %d", staleClient.closed.Load())
	}
	p.mu.Lock()
	_, present := p.entries[m1.ID]
	p.mu.Unlock()
	if present {
		t.Fatal("invalidated dial must not be installed in pool")
	}
}

func TestMCPPoolRefreshDoesNotDialAfterInvalidation(t *testing.T) {
	t.Setenv("ARTEX_MCP_IDLE_TTL_MIN", "0")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var dials atomic.Int32
	p := newMCPPool(ctx)
	p.connectFn = func(_ context.Context, _ *db.MCPServer, _ ExecPrivilege) (mcpClient, error) {
		dials.Add(1)
		return &fakeMCPClient{}, nil
	}

	m := &db.MCPServer{ID: 12, Name: "warm", Transport: "stdio", Command: "echo", Enabled: true}
	priv := ExecPrivilege{}
	fp := mcpFingerprint(m, priv)

	// Case 1: Tools() succeeds after Invalidate() — must NOT fall through to dial.
	cl1 := &fakeMCPClient{}
	cl1.onTools = func(context.Context) ([]actool.CoreTool, error) {
		p.Invalidate(m.ID)
		return nil, nil
	}
	p.mu.Lock()
	p.entries[m.ID] = &pooledMCPEntry{
		id:          m.ID,
		fingerprint: fp,
		client:      cl1,
		toolsAt:     time.Now().Add(-2 * mcpToolsCacheTTL),
	}
	p.mu.Unlock()

	if _, err := p.Acquire(ctx, m, priv); err == nil || !strings.Contains(err.Error(), "invalidated") {
		t.Fatalf("expected invalidation error after refresh success, got %v", err)
	}
	if dials.Load() != 0 {
		t.Fatalf("expected 0 dials after refresh invalidation, got %d", dials.Load())
	}

	// Case 2: Tools() fails because Invalidate() closed cur — must NOT fall through to dial.
	cl2 := &fakeMCPClient{}
	cl2.onTools = func(context.Context) ([]actool.CoreTool, error) {
		p.Invalidate(m.ID)
		return nil, context.Canceled
	}
	p.mu.Lock()
	p.entries[m.ID] = &pooledMCPEntry{
		id:          m.ID,
		fingerprint: fp,
		client:      cl2,
		toolsAt:     time.Now().Add(-2 * mcpToolsCacheTTL),
	}
	p.mu.Unlock()

	if _, err := p.Acquire(ctx, m, priv); err == nil || !strings.Contains(err.Error(), "invalidated") {
		t.Fatalf("expected invalidation error after refresh failure, got %v", err)
	}
	if dials.Load() != 0 {
		t.Fatalf("expected 0 dials after invalidated refresh failure, got %d", dials.Load())
	}
}

func TestMCPPoolNonReadOnly404SessionExpiredRetries(t *testing.T) {
	t.Setenv("ARTEX_MCP_IDLE_TTL_MIN", "0")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var dials atomic.Int32
	p := newMCPPool(ctx)
	p.connectFn = func(_ context.Context, _ *db.MCPServer, _ ExecPrivilege) (mcpClient, error) {
		gen := dials.Add(1)
		cl := &fakeMCPClient{}
		cl.onTools = func(context.Context) ([]actool.CoreTool, error) {
			return []actool.CoreTool{
				actool.Build(actool.Spec{
					Name:        "mcp__browser__click",
					Description: "click",
					Schema:      map[string]any{"type": "object"},
					ReadOnly:    func(json.RawMessage) bool { return false },
					Run: func(_ context.Context, _ json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
						if gen == 1 {
							return actool.Errorf("Error: mcp http 404: session expired: unknown session"), nil
						}
						if gen == 2 {
							return actool.Errorf("Error: mcp sse http 404: session gone"), nil
						}
						return actool.Text("clicked-gen-3"), nil
					},
				}),
			}, nil
		}
		return cl, nil
	}

	m := &db.MCPServer{ID: 13, Name: "browser", Transport: "http", URL: "http://127.0.0.1:9999/mcp", Enabled: true}
	tools, err := p.Acquire(ctx, m, ExecPrivilege{})
	if err != nil || len(tools) != 1 {
		t.Fatalf("Acquire: len=%d err=%v", len(tools), err)
	}
	// First call hits Streamable HTTP 404 on gen 1 -> retries once on gen 2 (which hits SSE 404).
	res, err := tools[0].Call(ctx, []byte(`{}`), nil)
	if err != nil || !res.IsError || !strings.Contains(res.Flatten(), "mcp sse http 404") {
		t.Fatalf("expected retry from gen 1 to gen 2, got res=%+v err=%v", res, err)
	}
	if dials.Load() != 2 {
		t.Fatalf("expected 2 dials after single retry, got %d", dials.Load())
	}
	// Next call reconnects to gen 3 and succeeds!
	res, err = tools[0].Call(ctx, []byte(`{}`), nil)
	if err != nil || res.IsError || res.Flatten() != "clicked-gen-3" {
		t.Fatalf("expected gen 3 success, got res=%+v err=%v", res, err)
	}
}

func TestMCPPoolEvictsDeadClientBeforeNonReadOnlyCallAndAcquire(t *testing.T) {
	t.Setenv("ARTEX_MCP_IDLE_TTL_MIN", "0")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var dials atomic.Int32
	var firstClient *fakeMCPClient
	p := newMCPPool(ctx)
	p.connectFn = func(_ context.Context, _ *db.MCPServer, _ ExecPrivilege) (mcpClient, error) {
		gen := dials.Add(1)
		cl := &fakeMCPClient{}
		if gen == 1 {
			firstClient = cl
		}
		cl.onTools = func(context.Context) ([]actool.CoreTool, error) {
			return []actool.CoreTool{
				actool.Build(actool.Spec{
					Name:        "mcp__browser__navigate",
					Description: "navigate",
					Schema:      map[string]any{"type": "object"},
					ReadOnly:    func(json.RawMessage) bool { return false },
					Run: func(_ context.Context, _ json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
						if gen == 1 {
							return actool.Errorf("Error: mcp: connection closed"), nil
						}
						return actool.Text("navigated-gen-2"), nil
					},
				}),
			}, nil
		}
		return cl, nil
	}

	m := &db.MCPServer{ID: 21, Name: "browser", Transport: "stdio", Command: "patchright-mcp", Enabled: true}
	tools1, err := p.Acquire(ctx, m, ExecPrivilege{})
	if err != nil || len(tools1) != 1 {
		t.Fatalf("Acquire 1: len=%d err=%v", len(tools1), err)
	}
	if firstClient == nil {
		t.Fatal("expected firstClient")
	}

	// Simulate underlying subprocess crashing while idle.
	firstClient.dead.Store(true)

	// Calling a non-readOnly tool on tools1 must detect !Alive() before execution
	// and transparently reconnect to gen 2 without failing on gen 1!
	res, err := tools1[0].Call(ctx, []byte(`{}`), nil)
	if err != nil || res.IsError || res.Flatten() != "navigated-gen-2" {
		t.Fatalf("expected transparent reconnect for dead client, got res=%+v err=%v", res, err)
	}
	if dials.Load() != 2 {
		t.Fatalf("expected 2 dials, got %d", dials.Load())
	}

	// Also verify Acquire() fast-path evicts a dead pooled client immediately.
	p.mu.Lock()
	ent2 := p.entries[m.ID]
	p.mu.Unlock()
	ent2.client.(*fakeMCPClient).dead.Store(true)
	if _, err := p.Acquire(ctx, m, ExecPrivilege{}); err != nil {
		t.Fatalf("Acquire after dead entry failed: %v", err)
	}
	if dials.Load() != 3 {
		t.Fatalf("expected Acquire to reconnect after dead client, dials=%d", dials.Load())
	}
}

func TestMCPConfigChangedDetectsEnvAndInsecure(t *testing.T) {
	base := &db.MCPServer{
		ID:        1,
		Name:      "remote",
		Transport: "http",
		URL:       "https://mcp.example.com",
		Args:      json.RawMessage(`[]`),
		Env:       json.RawMessage(`{"Authorization":"Bearer old"}`),
		Insecure:  false,
	}
	same := &db.MCPServer{
		ID:        1,
		Name:      "remote",
		Transport: "http",
		URL:       "https://mcp.example.com",
		Args:      json.RawMessage(`null`),
		Env:       json.RawMessage(`{ "Authorization" : "Bearer old" }`),
		Insecure:  false,
	}
	if mcpConfigChanged(base, same) {
		t.Fatal("expected canonical-equivalent configs to report unchanged")
	}

	envChanged := *base
	envChanged.Env = json.RawMessage(`{"Authorization":"Bearer new"}`)
	if !mcpConfigChanged(base, &envChanged) {
		t.Fatal("expected Env change to be detected")
	}

	insecureChanged := *base
	insecureChanged.Insecure = true
	if !mcpConfigChanged(base, &insecureChanged) {
		t.Fatal("expected Insecure toggle to be detected")
	}
}
