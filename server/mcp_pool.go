package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/skill"
	actool "github.com/Autumn-27/norma/tool"
	"golang.org/x/sync/singleflight"
)

const (
	mcpToolsCacheTTL   = 5 * time.Minute
	defaultMCPIdleTTL  = 10 * time.Minute
	mcpReapMaxInterval = 2 * time.Minute
)

type pooledMCPEntry struct {
	id          int64
	fingerprint string
	client      mcpClient
	tools       []actool.CoreTool
	toolsAt     time.Time

	mu       sync.Mutex
	lastUsed int64
	inFlight int64
	closed   bool

	closeOnce sync.Once
}

func (e *pooledMCPEntry) touch() {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.lastUsed = time.Now().UnixNano()
	e.mu.Unlock()
}

func (e *pooledMCPEntry) setLastUsedForTest(ts int64) {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.lastUsed = ts
	e.mu.Unlock()
}

func (e *pooledMCPEntry) isClosed() bool {
	if e == nil {
		return true
	}
	e.mu.Lock()
	c := e.closed
	e.mu.Unlock()
	return c
}

func (e *pooledMCPEntry) markClosed() {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.closed = true
	e.mu.Unlock()
}

// beginCall atomically checks that the entry is still open and increments
// inFlight, eliminating any race window with reapIdle.
func (e *pooledMCPEntry) beginCall() bool {
	if e == nil {
		return false
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return false
	}
	e.inFlight++
	e.lastUsed = time.Now().UnixNano()
	e.mu.Unlock()
	return true
}

func (e *pooledMCPEntry) endCall() {
	e.endCallTouch(true)
}

func (e *pooledMCPEntry) endCallTouch(updateLastUsed bool) {
	if e == nil {
		return
	}
	e.mu.Lock()
	if e.inFlight > 0 {
		e.inFlight--
	}
	if updateLastUsed && !e.closed {
		e.lastUsed = time.Now().UnixNano()
	}
	e.mu.Unlock()
}

func (e *pooledMCPEntry) inFlightCount() int64 {
	if e == nil {
		return 0
	}
	e.mu.Lock()
	n := e.inFlight
	e.mu.Unlock()
	return n
}

func (e *pooledMCPEntry) clientAlive() bool {
	if e == nil {
		return false
	}
	if e.client == nil {
		return true
	}
	if ac, ok := e.client.(interface{ Alive() bool }); ok {
		return ac.Alive()
	}
	return true
}

// tryEvictDead atomically marks a dead entry closed if it has no in-flight calls.
func (e *pooledMCPEntry) tryEvictDead() bool {
	if e == nil {
		return false
	}
	e.mu.Lock()
	if e.closed || e.inFlight > 0 {
		e.mu.Unlock()
		return false
	}
	e.closed = true
	e.mu.Unlock()
	return true
}

// tryEvictIdle atomically marks the entry closed if it has no in-flight calls
// and has been idle since before cutoff.
func (e *pooledMCPEntry) tryEvictIdle(cutoff int64) bool {
	if e == nil {
		return false
	}
	e.mu.Lock()
	if e.closed || e.inFlight > 0 || e.lastUsed == 0 || e.lastUsed >= cutoff {
		e.mu.Unlock()
		return false
	}
	e.closed = true
	e.mu.Unlock()
	return true
}

func (e *pooledMCPEntry) closeClient() error {
	if e == nil {
		return nil
	}
	e.markClosed()
	var err error
	e.closeOnce.Do(func() {
		if e.client != nil {
			err = e.client.Close()
		}
	})
	return err
}

func mcpIdleTTL() time.Duration {
	raw := strings.TrimSpace(os.Getenv("ARTEX_MCP_IDLE_TTL_MIN"))
	if raw == "" {
		return defaultMCPIdleTTL
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return defaultMCPIdleTTL
	}
	if n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Minute
}

// mcpPool keeps connected MCP clients (stdio subprocesses and HTTP/SSE clients)
// warm across agent runs so planner rounds, workers, and chat turns do not pay
// process-spawn + initialize + tools/list + teardown on every single turn.
type mcpPool struct {
	ctx       context.Context
	mu        sync.Mutex
	entries   map[int64]*pooledMCPEntry
	gen       map[int64]uint64
	closed    bool
	sf        singleflight.Group
	connectFn func(context.Context, *db.MCPServer, ExecPrivilege) (mcpClient, error)
	lookupFn  func(id int64) (*db.MCPServer, bool)
}

func newMCPPool(ctx context.Context) *mcpPool {
	if ctx == nil {
		ctx = context.Background()
	}
	p := &mcpPool{
		ctx:       ctx,
		entries:   map[int64]*pooledMCPEntry{},
		gen:       map[int64]uint64{},
		connectFn: connectMCP,
	}
	if ttl := mcpIdleTTL(); ttl > 0 {
		interval := ttl
		if interval > mcpReapMaxInterval {
			interval = mcpReapMaxInterval
		}
		go p.reapLoop(interval, ttl)
	}
	return p
}

func (p *mcpPool) reapLoop(interval, ttl time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			p.reapIdle(ttl)
		}
	}
}

func (p *mcpPool) reapIdle(ttl time.Duration) {
	cutoff := time.Now().Add(-ttl).UnixNano()
	var stale []*pooledMCPEntry
	p.mu.Lock()
	for id, ent := range p.entries {
		if ent == nil {
			delete(p.entries, id)
			continue
		}
		if ent.tryEvictIdle(cutoff) || (!ent.clientAlive() && ent.tryEvictDead()) {
			stale = append(stale, ent)
			delete(p.entries, id)
		}
	}
	p.mu.Unlock()
	for _, ent := range stale {
		_ = ent.closeClient()
	}
}

func cloneMCPServer(m *db.MCPServer) *db.MCPServer {
	if m == nil {
		return nil
	}
	cp := *m
	if len(m.Args) > 0 {
		cp.Args = append(json.RawMessage(nil), m.Args...)
	}
	if len(m.Env) > 0 {
		cp.Env = append(json.RawMessage(nil), m.Env...)
	}
	return &cp
}

func mcpFingerprint(m *db.MCPServer, priv ExecPrivilege) string {
	h := sha256.New()
	fmt.Fprintf(h, "%d|%s|%s|%s|%s|%s|%s|%v|%d|%v|%d|%d",
		m.ID, m.Name, m.Transport, m.Command, m.Args, m.URL, m.Env, m.Insecure, m.TimeoutSeconds,
		priv.Enabled, priv.UID, priv.GID)
	return hex.EncodeToString(h.Sum(nil))
}

// Acquire returns wrapped CoreTools for an enabled MCP server, reusing the warm
// client and cached tools/list whenever the server configuration matches.
func (p *mcpPool) Acquire(ctx context.Context, m *db.MCPServer, priv ExecPrivilege) ([]actool.CoreTool, error) {
	if p == nil {
		cl, err := connectMCP(ctx, m, priv)
		if err != nil {
			return nil, fmt.Errorf("connect: %w", err)
		}
		ts, err := cl.Tools(ctx)
		if err != nil {
			_ = cl.Close()
			return nil, fmt.Errorf("tools/list: %w", err)
		}
		// Without a pool, close would leak if not managed; caller shouldn't hit nil here.
		return ts, nil
	}
	srvCopy := cloneMCPServer(m)
	ent, tools, err := p.acquireEntry(ctx, srvCopy, priv)
	if err != nil {
		return nil, err
	}
	return wrapPooledTools(p, srvCopy, priv, ent, tools), nil
}

func (p *mcpPool) acquireEntry(ctx context.Context, m *db.MCPServer, priv ExecPrivilege) (*pooledMCPEntry, []actool.CoreTool, error) {
	fp := mcpFingerprint(m, priv)

	var deadFast *pooledMCPEntry
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, nil, fmt.Errorf("mcp pool closed")
	}
	ent := p.entries[m.ID]
	if ent != nil && !ent.isClosed() && !ent.clientAlive() {
		ent.markClosed()
		delete(p.entries, m.ID)
		deadFast = ent
		ent = nil
	}
	if ent != nil && !ent.isClosed() && ent.fingerprint == fp && time.Since(ent.toolsAt) < mcpToolsCacheTTL {
		ent.touch()
		tools := ent.tools
		p.mu.Unlock()
		return ent, tools, nil
	}
	startGen := p.gen[m.ID]
	p.mu.Unlock()
	if deadFast != nil {
		go func(e *pooledMCPEntry) { _ = e.closeClient() }(deadFast)
	}

	type acquiredMCP struct {
		ent   *pooledMCPEntry
		tools []actool.CoreTool
	}
	key := fmt.Sprintf("%d:%d:%s", m.ID, startGen, fp)
	ch := p.sf.DoChan(key, func() (any, error) {
		var deadSF *pooledMCPEntry
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, fmt.Errorf("mcp pool closed")
		}
		if p.gen[m.ID] != startGen {
			p.mu.Unlock()
			return nil, fmt.Errorf("mcp server invalidated during connect")
		}
		cur := p.entries[m.ID]
		if cur != nil && !cur.isClosed() && !cur.clientAlive() {
			cur.markClosed()
			delete(p.entries, m.ID)
			deadSF = cur
			cur = nil
		}
		if cur != nil && !cur.isClosed() && cur.fingerprint == fp {
			if time.Since(cur.toolsAt) < mcpToolsCacheTTL {
				cur.touch()
				res := acquiredMCP{ent: cur, tools: cur.tools}
				p.mu.Unlock()
				return res, nil
			}
			// Same client, refresh tools list only. Hold inFlight across the
			// Tools() RPC so reapIdle cannot evict and close cur mid-refresh.
			if cur.beginCall() {
				p.mu.Unlock()
				tctx, cancel := context.WithTimeout(p.ctx, 30*time.Second)
				ts, terr := cur.client.Tools(tctx)
				cancel()
				if terr == nil {
					p.mu.Lock()
					cur.endCall()
					if !p.closed && p.gen[m.ID] == startGen && p.entries[m.ID] == cur && !cur.isClosed() {
						cur.tools = ts
						cur.toolsAt = time.Now()
						p.mu.Unlock()
						return acquiredMCP{ent: cur, tools: ts}, nil
					}
					if p.closed {
						p.mu.Unlock()
						return nil, fmt.Errorf("mcp pool closed")
					}
					if p.gen[m.ID] != startGen {
						p.mu.Unlock()
						return nil, fmt.Errorf("mcp server invalidated during connect")
					}
					p.mu.Unlock()
				} else {
					cur.endCallTouch(false)
					// Broken connection: evict and reconnect below unless invalidated/closed.
					p.evictIfMatch(m.ID, cur)
					p.mu.Lock()
					if p.closed {
						p.mu.Unlock()
						return nil, fmt.Errorf("mcp pool closed")
					}
					if p.gen[m.ID] != startGen {
						p.mu.Unlock()
						return nil, fmt.Errorf("mcp server invalidated during connect")
					}
					p.mu.Unlock()
				}
			} else {
				if p.closed {
					p.mu.Unlock()
					return nil, fmt.Errorf("mcp pool closed")
				}
				if p.gen[m.ID] != startGen {
					p.mu.Unlock()
					return nil, fmt.Errorf("mcp server invalidated during connect")
				}
				p.mu.Unlock()
			}
		} else {
			p.mu.Unlock()
		}
		if deadSF != nil {
			go func(e *pooledMCPEntry) { _ = e.closeClient() }(deadSF)
		}

		// Dial using pool-lifetime context so cancelling one agent run does not
		// terminate shared MCP connections or stdio subprocesses.
		dialCtx, cancel := context.WithTimeout(p.ctx, 45*time.Second)
		defer cancel()
		dial := p.connectFn
		if dial == nil {
			dial = connectMCP
		}
		cl, cerr := dial(dialCtx, m, priv)
		if cerr != nil {
			return nil, fmt.Errorf("connect: %w", cerr)
		}
		ts, terr := cl.Tools(dialCtx)
		if terr != nil {
			_ = cl.Close()
			return nil, fmt.Errorf("tools/list: %w", terr)
		}

		next := &pooledMCPEntry{
			id:          m.ID,
			fingerprint: fp,
			client:      cl,
			tools:       ts,
			toolsAt:     time.Now(),
		}
		next.touch()
		p.mu.Lock()
		if p.closed || p.gen[m.ID] != startGen {
			wasClosed := p.closed
			p.mu.Unlock()
			_ = cl.Close()
			if wasClosed {
				return nil, fmt.Errorf("mcp pool closed")
			}
			return nil, fmt.Errorf("mcp server invalidated during connect")
		}
		old := p.entries[m.ID]
		p.entries[m.ID] = next
		p.mu.Unlock()
		if old != nil {
			old.markClosed()
			go func(e *pooledMCPEntry) { _ = e.closeClient() }(old)
		}
		return acquiredMCP{ent: next, tools: ts}, nil
	})
	select {
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	case sfRes := <-ch:
		if sfRes.Err != nil {
			return nil, nil, sfRes.Err
		}
		res := sfRes.Val.(acquiredMCP)
		return res.ent, res.tools, nil
	}
}

func (p *mcpPool) evictIfMatch(id int64, ent *pooledMCPEntry) {
	if ent != nil {
		ent.markClosed()
	}
	p.mu.Lock()
	cur := p.entries[id]
	if cur != ent {
		p.mu.Unlock()
		return
	}
	delete(p.entries, id)
	p.mu.Unlock()
	if cur != nil {
		go func(e *pooledMCPEntry) { _ = e.closeClient() }(cur)
	}
}

func (p *mcpPool) Invalidate(id int64) {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.gen == nil {
		p.gen = map[int64]uint64{}
	}
	p.gen[id]++
	cur := p.entries[id]
	delete(p.entries, id)
	p.mu.Unlock()
	if cur != nil {
		cur.markClosed()
		go func(e *pooledMCPEntry) { _ = e.closeClient() }(cur)
	}
}

func (p *mcpPool) CloseAll() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.closed = true
	all := make([]*pooledMCPEntry, 0, len(p.entries))
	for id, ent := range p.entries {
		if ent != nil {
			ent.markClosed()
		}
		all = append(all, ent)
		delete(p.entries, id)
	}
	p.mu.Unlock()
	for _, ent := range all {
		if ent != nil {
			if err := ent.closeClient(); err != nil {
				log.Printf("[mcp-pool] close id=%d: %v", ent.id, err)
			}
		}
	}
}

type pooledMCPTool struct {
	actool.CoreTool
	pool   *mcpPool
	server *db.MCPServer
	priv   ExecPrivilege
	id     int64
	ent    *pooledMCPEntry
}

// isMCPTransportError identifies genuine transport-level failures returned by
// stdio/HTTP/SSE MCP clients (as opposed to domain-level tool errors whose text
// may incidentally contain words like "disclosed", "port closed", or "eof").
func isMCPTransportError(res actool.Result) bool {
	if !res.IsError {
		return false
	}
	for _, b := range res.Content {
		low := strings.ToLower(strings.TrimSpace(b.Text))
		if !strings.HasPrefix(low, "error:") {
			continue
		}
		if strings.Contains(low, "mcp: connection closed") ||
			strings.Contains(low, "broken pipe") ||
			strings.Contains(low, "connection refused") ||
			strings.Contains(low, "connection reset") ||
			strings.Contains(low, "use of closed network connection") ||
			strings.Contains(low, "file already closed") ||
			strings.Contains(low, "mcp sse stream:") ||
			strings.Contains(low, "mcp http 404") ||
			strings.Contains(low, "mcp sse http 404") ||
			strings.Contains(low, "process already finished") ||
			strings.HasSuffix(low, ": eof") ||
			strings.HasSuffix(low, " eof") ||
			strings.Contains(low, "unexpected eof") {
			return true
		}
	}
	return false
}

// isSafePreExecMCPError identifies transport errors where the remote MCP server
// rejected the request before executing the tool (expired Streamable HTTP or
// legacy SSE session returning HTTP 404), making a single reconnect-and-retry
// safe even for non-readOnly tools per the MCP Streamable HTTP specification.
func isSafePreExecMCPError(res actool.Result) bool {
	if !res.IsError {
		return false
	}
	for _, b := range res.Content {
		low := strings.ToLower(strings.TrimSpace(b.Text))
		if !strings.HasPrefix(low, "error:") {
			continue
		}
		if strings.Contains(low, "mcp http 404: session expired") ||
			strings.Contains(low, "mcp sse http 404") {
			return true
		}
	}
	return false
}

func findToolByName(tools []actool.CoreTool, name string) actool.CoreTool {
	for _, candidate := range tools {
		if candidate.Name() == name {
			return candidate
		}
	}
	return nil
}

// activeServerConfig resolves the latest config when lookupFn is wired, ensuring
// disabled or deleted MCP servers are never respawned by an older session.
func (t pooledMCPTool) activeServerConfig() (*db.MCPServer, error) {
	if t.pool != nil && t.pool.lookupFn != nil {
		fresh, ok := t.pool.lookupFn(t.id)
		if !ok || fresh == nil || !fresh.Enabled {
			name := ""
			if t.server != nil {
				name = t.server.Name
			}
			return nil, fmt.Errorf("mcp server %q is disabled or removed", name)
		}
		return cloneMCPServer(fresh), nil
	}
	return t.server, nil
}

// resolveAndBegin returns an active pooled entry (with inFlight already
// incremented via beginCall) and CoreTool for this tool name. If the originally
// captured entry was reaped for idleness, invalidated, or crashed mid-session,
// it transparently re-acquires a fresh client from the pool.
func (t pooledMCPTool) resolveAndBegin(ctx context.Context, forceReacquire bool) (*pooledMCPEntry, actool.CoreTool, *db.MCPServer, error) {
	if !forceReacquire && t.ent != nil {
		if !t.ent.clientAlive() {
			if t.pool != nil {
				t.pool.evictIfMatch(t.id, t.ent)
			} else {
				t.ent.markClosed()
			}
		} else if t.ent.beginCall() {
			return t.ent, t.CoreTool, t.server, nil
		}
	}
	if t.pool == nil || t.server == nil {
		if t.ent != nil {
			t.ent.beginCall()
		}
		return t.ent, t.CoreTool, t.server, nil
	}
	srv, err := t.activeServerConfig()
	if err != nil {
		return nil, nil, nil, err
	}
	for retry := 0; retry < 2; retry++ {
		ent, tools, aerr := t.pool.acquireEntry(ctx, srv, t.priv)
		if aerr != nil {
			return nil, nil, nil, aerr
		}
		live := findToolByName(tools, t.Name())
		if live == nil {
			return nil, nil, nil, fmt.Errorf("mcp tool %s not advertised after reconnect", t.Name())
		}
		if !ent.clientAlive() {
			t.pool.evictIfMatch(t.id, ent)
			continue
		}
		if ent.beginCall() {
			return ent, live, srv, nil
		}
	}
	return nil, nil, nil, fmt.Errorf("mcp entry closed during reconnect")
}

func (t pooledMCPTool) Call(ctx context.Context, input json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	readOnly := t.IsReadOnly(input)
	for attempt := 0; attempt < 2; attempt++ {
		ent, live, srv, err := t.resolveAndBegin(ctx, attempt > 0)
		if err != nil {
			return actool.Errorf("Error: mcp reconnect failed: " + err.Error()), nil
		}

		callCtx := ctx
		var cancel context.CancelFunc
		if srv != nil && srv.TimeoutSeconds > 0 {
			callCtx, cancel = context.WithTimeout(ctx, time.Duration(srv.TimeoutSeconds)*time.Second)
		}
		res, callErr := live.Call(callCtx, input, tc)
		if cancel != nil {
			cancel()
		}
		evict := callErr != nil || isMCPTransportError(res)
		if ent != nil {
			ent.endCallTouch(!evict)
		}

		if callErr != nil {
			t.pool.evictIfMatch(t.id, ent)
			if attempt == 0 && readOnly && ctx.Err() == nil && t.pool != nil && t.server != nil {
				continue
			}
			return res, callErr
		}
		if evict {
			t.pool.evictIfMatch(t.id, ent)
			if attempt == 0 && (readOnly || isSafePreExecMCPError(res)) && ctx.Err() == nil && t.pool != nil && t.server != nil {
				continue
			}
		}
		return res, nil
	}
	return actool.Errorf("Error: mcp call failed after reconnect"), nil
}

func wrapPooledTools(p *mcpPool, server *db.MCPServer, priv ExecPrivilege, ent *pooledMCPEntry, tools []actool.CoreTool) []actool.CoreTool {
	out := make([]actool.CoreTool, len(tools))
	for i, t := range tools {
		out[i] = pooledMCPTool{CoreTool: t, pool: p, server: server, priv: priv, id: server.ID, ent: ent}
	}
	return out
}

// skillsCache caches loadSkillsConfined(skillDir) with a short TTL + directory
// mtime check so parallel workers do not walk the filesystem on every launch.
type cachedSkillsEntry struct {
	reg       *skill.Registry
	dirMtime  int64
	fetchedAt time.Time
}

var skillsCache struct {
	mu sync.Mutex
	m  map[string]cachedSkillsEntry
}

func loadSkillsCached(dir string) (*skill.Registry, error) {
	if dir == "" {
		return nil, nil
	}
	var mtime int64
	if st, err := os.Stat(dir); err == nil {
		mtime = st.ModTime().UnixNano()
		if entries, rerr := os.ReadDir(dir); rerr == nil {
			sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
			for _, e := range entries {
				if info, ierr := e.Info(); ierr == nil {
					mtime ^= info.ModTime().UnixNano()
				} else if sst, serr := os.Stat(filepath.Join(dir, e.Name())); serr == nil {
					mtime ^= sst.ModTime().UnixNano()
				}
			}
		}
	}
	skillsCache.mu.Lock()
	if skillsCache.m != nil {
		if ent, ok := skillsCache.m[dir]; ok && ent.dirMtime == mtime && time.Since(ent.fetchedAt) < 5*time.Second {
			reg := ent.reg
			skillsCache.mu.Unlock()
			return reg, nil
		}
	}
	skillsCache.mu.Unlock()

	reg, err := loadSkillsConfined(dir)
	if err != nil {
		return nil, err
	}
	skillsCache.mu.Lock()
	if skillsCache.m == nil {
		skillsCache.m = map[string]cachedSkillsEntry{}
	}
	skillsCache.m[dir] = cachedSkillsEntry{reg: reg, dirMtime: mtime, fetchedAt: time.Now()}
	skillsCache.mu.Unlock()
	return reg, nil
}
