package server

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	actool "github.com/Autumn-27/norma/tool"
)

type fakeMCPClient struct {
	closed atomic.Int32
}

func (f *fakeMCPClient) Tools(context.Context) ([]actool.CoreTool, error) { return nil, nil }
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
