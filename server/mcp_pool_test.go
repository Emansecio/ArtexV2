package server

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

type fakeMCPClient struct {
	closed  atomic.Int32
	onTools func(context.Context) ([]actool.CoreTool, error)
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
	stale.lastUsed.Store(time.Now().Add(-5 * time.Minute).UnixNano())
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
	ent.lastUsed.Store(time.Now().Add(-5 * time.Minute).UnixNano())

	p.mu.Lock()
	p.entries[m.ID] = ent
	p.mu.Unlock()

	if _, err := p.Acquire(ctx, m, priv); err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}
	if reapedDuringRefresh || cl.closed.Load() != 0 {
		t.Fatalf("entry was reaped/closed during tools refresh (closed=%d)", cl.closed.Load())
	}
	if ent.inFlight.Load() != 0 {
		t.Fatalf("expected inFlight=0 after Acquire, got %d", ent.inFlight.Load())
	}
	p.mu.Lock()
	cur := p.entries[m.ID]
	p.mu.Unlock()
	if cur != ent {
		t.Fatalf("expected original entry to remain pooled")
	}
}
