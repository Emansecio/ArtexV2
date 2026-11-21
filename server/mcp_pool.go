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
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/skill"
	actool "github.com/Autumn-27/norma/tool"
	"golang.org/x/sync/singleflight"
)

const mcpToolsCacheTTL = 5 * time.Minute

type pooledMCPEntry struct {
	id          int64
	fingerprint string
	client      mcpClient
	tools       []actool.CoreTool
	toolsAt     time.Time
}

// mcpPool keeps connected MCP clients (stdio subprocesses and HTTP/SSE clients)
// warm across agent runs so planner rounds, workers, and chat turns do not pay
// process-spawn + initialize + tools/list + teardown on every single turn.
type mcpPool struct {
	ctx     context.Context
	mu      sync.Mutex
	entries map[int64]*pooledMCPEntry
	sf      singleflight.Group
}

func newMCPPool(ctx context.Context) *mcpPool {
	if ctx == nil {
		ctx = context.Background()
	}
	return &mcpPool{
		ctx:     ctx,
		entries: map[int64]*pooledMCPEntry{},
	}
}

func mcpFingerprint(m *db.MCPServer, priv ExecPrivilege) string {
	h := sha256.New()
	fmt.Fprintf(h, "%d|%s|%s|%s|%s|%s|%v|%d|%v|%d|%d",
		m.ID, m.Transport, m.Command, m.Args, m.URL, m.Env, m.Insecure, m.TimeoutSeconds,
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
	fp := mcpFingerprint(m, priv)

	p.mu.Lock()
	ent := p.entries[m.ID]
	if ent != nil && ent.fingerprint == fp && time.Since(ent.toolsAt) < mcpToolsCacheTTL {
		wrapped := wrapPooledTools(p, m.ID, ent, ent.tools)
		p.mu.Unlock()
		return wrapped, nil
	}
	p.mu.Unlock()

	key := fmt.Sprintf("%d:%s", m.ID, fp)
	v, err, _ := p.sf.Do(key, func() (any, error) {
		p.mu.Lock()
		cur := p.entries[m.ID]
		if cur != nil && cur.fingerprint == fp {
			if time.Since(cur.toolsAt) < mcpToolsCacheTTL {
				res := cur
				p.mu.Unlock()
				return res, nil
			}
			// Same client, refresh tools list only.
			p.mu.Unlock()
			tctx, cancel := context.WithTimeout(p.ctx, 30*time.Second)
			ts, terr := cur.client.Tools(tctx)
			cancel()
			if terr == nil {
				p.mu.Lock()
				cur.tools = ts
				cur.toolsAt = time.Now()
				p.mu.Unlock()
				return cur, nil
			}
			// Broken connection: evict and reconnect below.
			p.evictIfMatch(m.ID, cur)
		} else {
			p.mu.Unlock()
		}

		// Dial using pool-lifetime context so cancelling one agent run does not
		// terminate shared MCP connections or stdio subprocesses.
		dialCtx, cancel := context.WithTimeout(p.ctx, 45*time.Second)
		defer cancel()
		cl, cerr := connectMCP(dialCtx, m, priv)
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
		p.mu.Lock()
		old := p.entries[m.ID]
		p.entries[m.ID] = next
		p.mu.Unlock()
		if old != nil && old.client != nil {
			go func(c mcpClient) { _ = c.Close() }(old.client)
		}
		return next, nil
	})
	if err != nil {
		return nil, err
	}
	res := v.(*pooledMCPEntry)
	return wrapPooledTools(p, m.ID, res, res.tools), nil
}

func (p *mcpPool) evictIfMatch(id int64, ent *pooledMCPEntry) {
	p.mu.Lock()
	cur := p.entries[id]
	if cur != ent {
		p.mu.Unlock()
		return
	}
	delete(p.entries, id)
	p.mu.Unlock()
	if cur != nil && cur.client != nil {
		go func(c mcpClient) { _ = c.Close() }(cur.client)
	}
}

func (p *mcpPool) Invalidate(id int64) {
	if p == nil {
		return
	}
	p.mu.Lock()
	cur := p.entries[id]
	delete(p.entries, id)
	p.mu.Unlock()
	if cur != nil && cur.client != nil {
		go func(c mcpClient) { _ = c.Close() }(cur.client)
	}
}

func (p *mcpPool) CloseAll() {
	if p == nil {
		return
	}
	p.mu.Lock()
	all := make([]*pooledMCPEntry, 0, len(p.entries))
	for id, ent := range p.entries {
		all = append(all, ent)
		delete(p.entries, id)
	}
	p.mu.Unlock()
	for _, ent := range all {
		if ent != nil && ent.client != nil {
			if err := ent.client.Close(); err != nil {
				log.Printf("[mcp-pool] close id=%d: %v", ent.id, err)
			}
		}
	}
}

type pooledMCPTool struct {
	actool.CoreTool
	pool *mcpPool
	id   int64
	ent  *pooledMCPEntry
}

func (t pooledMCPTool) Call(ctx context.Context, input json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	res, err := t.CoreTool.Call(ctx, input, tc)
	if err != nil {
		t.pool.evictIfMatch(t.id, t.ent)
		return res, err
	}
	if res.IsError {
		for _, b := range res.Content {
			low := strings.ToLower(b.Text)
			if strings.Contains(low, "broken pipe") ||
				strings.Contains(low, "connection refused") ||
				strings.Contains(low, "connection reset") ||
				strings.Contains(low, "closed") ||
				strings.Contains(low, "eof") {
				t.pool.evictIfMatch(t.id, t.ent)
				break
			}
		}
	}
	return res, nil
}

func wrapPooledTools(p *mcpPool, id int64, ent *pooledMCPEntry, tools []actool.CoreTool) []actool.CoreTool {
	out := make([]actool.CoreTool, len(tools))
	for i, t := range tools {
		out[i] = pooledMCPTool{CoreTool: t, pool: p, id: id, ent: ent}
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
