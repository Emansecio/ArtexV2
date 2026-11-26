package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"iter"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
)

const (
	agyAgentName = "bridge"
	agyBridgeMD  = "---\n" +
		"name: bridge\n" +
		"description: Host-provided system prompt and tools.\n" +
		"mainAgent: true\n" +
		"subagent: false\n" +
		"tools: []\n" +
		"inheritCustomizations: false\n" +
		"excludeDefaultComponents: true\n" +
		"---\n"

	agyStartupTimeout       = 60 * time.Second
	agyParallelToolDebounce = 25 * time.Millisecond
	agyEventIdleTimeout     = 300 * time.Second
	agyDefaultMaxProcs      = 6
)

var (
	agyIdleRetire   = 5 * time.Minute
	agyReapInterval = time.Minute
)

func agyMaxProcs() int {
	if raw := strings.TrimSpace(os.Getenv("ARTEX_AGY_MAX_PROCS")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return n
		}
	}
	return agyDefaultMaxProcs
}

type agyACPMessageKind int

const (
	agyMsgResponse agyACPMessageKind = iota
	agyMsgNotification
	agyMsgServerRequest
	agyMsgEOF
)

type agyACPMessage struct {
	kind   agyACPMessageKind
	id     uint64
	rawID  json.RawMessage
	method string
	params json.RawMessage
	result json.RawMessage
	errObj json.RawMessage
}

type agyProcess struct {
	cmd               *exec.Cmd
	stdin             io.WriteCloser
	msgCh             chan agyACPMessage
	done              chan struct{}
	bridge            *agyMCPBridge
	nextID            uint64
	toolsKey          string
	systemHash        uint64
	credentialVersion string
	sessionID         string
	advertisedModels  []string
	configuredModel   string
	configuredEffort  string
	// pendingSystem is injected once as the first <system-reminder> block of the
	// session's opening session/prompt (sending it via session/new
	// _meta.antigravity.systemPrompt.append triggers HTTP 429 on agy 1.5.17).
	pendingSystem     string
	activePromptID    uint64
	parked            map[string]chan agyMCPToolOutput
	deliveredMessages int
	deliveredHashes   []uint64
	lastUsed          time.Time

	killOnce sync.Once
}

func (p *agyProcess) sendRaw(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if _, err := p.stdin.Write(raw); err != nil {
		return fmt.Errorf("write to agy acp stdin: %w", err)
	}
	return nil
}

func (p *agyProcess) replyServerRequest(rawID json.RawMessage, method string) {
	var resp map[string]any
	if method == "session/request_permission" {
		resp = map[string]any{
			"jsonrpc": "2.0",
			"id":      rawID,
			"result":  map[string]any{"outcome": map[string]any{"outcome": "cancelled"}},
		}
	} else {
		resp = map[string]any{
			"jsonrpc": "2.0",
			"id":      rawID,
			"error":   map[string]any{"code": -32601, "message": "Unsupported method: " + method},
		}
	}
	_ = p.sendRaw(resp)
}

func (p *agyProcess) rpc(ctx context.Context, method string, params any) (json.RawMessage, error) {
	p.nextID++
	id := p.nextID
	if err := p.sendRaw(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}); err != nil {
		return nil, err
	}
	timer := time.NewTimer(agyStartupTimeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return nil, fmt.Errorf("agy acp `%s` timed out", method)
		case msg, ok := <-p.msgCh:
			if !ok || msg.kind == agyMsgEOF {
				return nil, fmt.Errorf("agy acp exited during `%s`", method)
			}
			switch msg.kind {
			case agyMsgResponse:
				if msg.id == id {
					if len(msg.errObj) > 0 && string(msg.errObj) != "null" {
						var e struct {
							Message string `json:"message"`
						}
						_ = json.Unmarshal(msg.errObj, &e)
						if e.Message == "" {
							e.Message = "ACP error"
						}
						return nil, fmt.Errorf("agy acp `%s` failed: %s", method, e.Message)
					}
					return msg.result, nil
				}
			case agyMsgServerRequest:
				p.replyServerRequest(msg.rawID, msg.method)
			}
		}
	}
}

func (p *agyProcess) Kill() {
	p.killOnce.Do(func() {
		if p.done != nil {
			close(p.done)
		}
		for k, ch := range p.parked {
			close(ch)
			delete(p.parked, k)
		}
		if p.bridge != nil {
			p.bridge.Close()
		}
		if p.stdin != nil {
			_ = p.stdin.Close()
		}
		if p.cmd != nil && p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
			_ = p.cmd.Wait()
		}
	})
}

// ---------- Global process pool ----------

type agyPoolSlot struct {
	mu        sync.Mutex
	proc      *agyProcess
	lastUsed  time.Time
	evicted   bool
	abandoned bool

	cancelMu     sync.Mutex
	cancel       context.CancelFunc
	shuttingDown bool
}

func (s *agyPoolSlot) setCancel(fn context.CancelFunc) bool {
	s.cancelMu.Lock()
	if s.shuttingDown {
		s.cancelMu.Unlock()
		fn()
		return false
	}
	s.cancel = fn
	s.cancelMu.Unlock()
	return true
}

func (s *agyPoolSlot) clearCancel() {
	s.cancelMu.Lock()
	s.cancel = nil
	s.cancelMu.Unlock()
}

func (s *agyPoolSlot) beginShutdown() {
	s.cancelMu.Lock()
	s.shuttingDown = true
	fn := s.cancel
	s.cancelMu.Unlock()
	if fn != nil {
		fn()
	}
}

var (
	agyPoolMu        sync.Mutex
	agyPoolSlots     = map[string]*agyPoolSlot{}
	agySessionSeq    atomic.Uint64
	agyReaperRunning bool
	agyReaperStop    chan struct{}
)

func reapIdleAgyLocked(now time.Time) []*agyProcess {
	var toKill []*agyProcess
	retire := agyIdleRetire
	for k, s := range agyPoolSlots {
		if s.mu.TryLock() {
			if s.proc != nil && now.Sub(s.proc.lastUsed) > retire {
				toKill = append(toKill, s.proc)
				s.proc = nil
				s.evicted = true
				delete(agyPoolSlots, k)
			} else if s.proc == nil && (s.abandoned || (!s.lastUsed.IsZero() && now.Sub(s.lastUsed) > retire)) {
				s.evicted = true
				delete(agyPoolSlots, k)
			}
			s.mu.Unlock()
		}
	}
	return toKill
}

func ensureAgyReaperLocked() {
	if agyReaperRunning || len(agyPoolSlots) == 0 {
		return
	}
	interval := agyReapInterval
	if agyIdleRetire > 0 && agyIdleRetire < interval {
		interval = agyIdleRetire
	}
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	stop := make(chan struct{})
	agyReaperRunning = true
	agyReaperStop = stop
	go agyIdleReaperLoop(stop, interval)
}

func agyIdleReaperLoop(stop chan struct{}, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			agyPoolMu.Lock()
			if agyReaperStop != stop {
				agyPoolMu.Unlock()
				return
			}
			toKill := reapIdleAgyLocked(time.Now())
			empty := len(agyPoolSlots) == 0
			if empty {
				agyReaperRunning = false
				agyReaperStop = nil
			}
			agyPoolMu.Unlock()
			for _, proc := range toKill {
				proc.Kill()
			}
			if empty {
				return
			}
		}
	}
}

func getAgyPoolSlot(slotKey string) *agyPoolSlot {
	var toKill []*agyProcess
	maxProcs := agyMaxProcs()

	agyPoolMu.Lock()
	if slot, ok := agyPoolSlots[slotKey]; ok {
		agyPoolMu.Unlock()
		return slot
	}

	// Evict idle processes and abandoned empty slots first, then LRU non-busy process if at cap.
	now := time.Now()
	toKill = append(toKill, reapIdleAgyLocked(now)...)
	for len(agyPoolSlots) >= maxProcs {
		var oldestKey string
		var oldestTime time.Time
		var oldestSlot *agyPoolSlot
		for k, s := range agyPoolSlots {
			if s.mu.TryLock() {
				if s.proc != nil {
					t := s.proc.lastUsed
					if t.IsZero() {
						t = s.lastUsed
					}
					if oldestSlot == nil || t.Before(oldestTime) {
						oldestKey = k
						oldestTime = t
						oldestSlot = s
					}
				} else if s.abandoned || (!s.lastUsed.IsZero() && now.Sub(s.lastUsed) > agyIdleRetire) {
					// Never treat a brand-new unlocked slot (proc==nil, abandoned==false) as an
					// immediate LRU eviction target; only reap abandoned or expired empty slots.
					if oldestSlot == nil || s.lastUsed.Before(oldestTime) {
						oldestKey = k
						oldestTime = s.lastUsed
						oldestSlot = s
					}
				}
				s.mu.Unlock()
			}
		}
		if oldestSlot == nil {
			break
		}
		if oldestSlot.mu.TryLock() {
			if oldestSlot.proc != nil {
				toKill = append(toKill, oldestSlot.proc)
				oldestSlot.proc = nil
			}
			oldestSlot.evicted = true
			delete(agyPoolSlots, oldestKey)
			oldestSlot.mu.Unlock()
		} else {
			break
		}
	}

	slot := &agyPoolSlot{lastUsed: now}
	agyPoolSlots[slotKey] = slot
	ensureAgyReaperLocked()
	agyPoolMu.Unlock()

	for _, proc := range toKill {
		proc.Kill()
	}
	return slot
}

// ShutdownAntigravityPool terminates all pooled `agy acp` subprocesses cleanly.
func ShutdownAntigravityPool() {
	agyPoolMu.Lock()
	slots := make([]*agyPoolSlot, 0, len(agyPoolSlots))
	for k, s := range agyPoolSlots {
		slots = append(slots, s)
		delete(agyPoolSlots, k)
	}
	if agyReaperRunning && agyReaperStop != nil {
		close(agyReaperStop)
		agyReaperStop = nil
		agyReaperRunning = false
	}
	agyPoolMu.Unlock()

	for _, s := range slots {
		s.beginShutdown()
	}
	for _, s := range slots {
		s.mu.Lock()
		s.evicted = true
		if s.proc != nil {
			s.proc.Kill()
			s.proc = nil
		}
		s.mu.Unlock()
	}
}

// ShutdownAntigravitySession terminates any pooled `agy acp` subprocess bound
// to sessionID (used by one-shot callers such as TestConnection so ephemeral
// sessions do not linger in the pool).
func ShutdownAntigravitySession(sessionID string) {
	sid := strings.TrimSpace(sessionID)
	if sid == "" {
		return
	}
	suffix := ":" + sid
	agyPoolMu.Lock()
	var slots []*agyPoolSlot
	for k, s := range agyPoolSlots {
		if strings.HasSuffix(k, suffix) {
			slots = append(slots, s)
			delete(agyPoolSlots, k)
		}
	}
	if len(agyPoolSlots) == 0 && agyReaperRunning && agyReaperStop != nil {
		close(agyReaperStop)
		agyReaperStop = nil
		agyReaperRunning = false
	}
	agyPoolMu.Unlock()

	for _, s := range slots {
		s.beginShutdown()
	}
	for _, s := range slots {
		s.mu.Lock()
		s.evicted = true
		if s.proc != nil {
			s.proc.Kill()
			s.proc = nil
		}
		s.mu.Unlock()
	}
}

// ShutdownAntigravityAccount terminates all pooled `agy acp` subprocesses for accountKey.
// If accountKey is empty, terminates all pooled subprocesses.
func ShutdownAntigravityAccount(accountKey string) {
	if strings.TrimSpace(accountKey) == "" {
		ShutdownAntigravityPool()
		return
	}
	prefix := accountKey + ":"
	agyPoolMu.Lock()
	var slots []*agyPoolSlot
	for k, s := range agyPoolSlots {
		if strings.HasPrefix(k, prefix) {
			slots = append(slots, s)
			delete(agyPoolSlots, k)
		}
	}
	agyPoolMu.Unlock()

	for _, s := range slots {
		s.beginShutdown()
	}
	for _, s := range slots {
		s.mu.Lock()
		s.evicted = true
		if s.proc != nil {
			s.proc.Kill()
			s.proc = nil
		}
		s.mu.Unlock()
	}
}

// ---------- Provider implementation ----------

type antigravityProvider struct {
	cfg               Config
	defaultSessionKey string
}

func newAntigravityProvider(cfg Config) *antigravityProvider {
	id := agySessionSeq.Add(1)
	return &antigravityProvider{
		cfg:               cfg,
		defaultSessionKey: fmt.Sprintf("artex-%d-%d", os.Getpid(), id),
	}
}

func (p *antigravityProvider) httpClient() *http.Client {
	if p.cfg.HTTPClient != nil {
		return p.cfg.HTTPClient
	}
	return http.DefaultClient
}

func (p *antigravityProvider) resolveCredential(ctx context.Context, forceRefresh bool) (AgyCredential, error) {
	if p.cfg.AgyCredFunc != nil {
		return p.cfg.AgyCredFunc(ctx, forceRefresh)
	}
	if p.cfg.TokenFunc != nil {
		tok, err := p.cfg.TokenFunc(ctx, forceRefresh)
		if err != nil {
			return AgyCredential{}, err
		}
		return AgyCredential{Access: tok, ExpiresMS: time.Now().Add(time.Hour).UnixMilli()}, nil
	}
	if strings.TrimSpace(p.cfg.APIKey) != "" {
		return AgyCredential{Access: strings.TrimSpace(p.cfg.APIKey), ExpiresMS: time.Now().Add(time.Hour).UnixMilli()}, nil
	}
	return AgyCredential{}, &HTTPStatusError{
		Prefix:     "antigravity-cli",
		StatusCode: http.StatusUnauthorized,
		Body:       "Antigravity CLI account not connected; sign in in Models",
	}
}

func (p *antigravityProvider) Complete(ctx context.Context, req CompletionRequest) (Message, string, Usage, error) {
	return accumulateStream(p.Stream(ctx, req))
}

func (p *antigravityProvider) Stream(ctx context.Context, req CompletionRequest) iter.Seq2[StreamEvent, error] {
	return func(yield func(StreamEvent, error) bool) {
		if err := p.cfg.limiter.Wait(ctx); err != nil {
			yield(StreamEvent{}, err)
			return
		}
		cred, err := p.resolveCredential(ctx, false)
		if err != nil {
			yield(StreamEvent{}, err)
			return
		}
		accountKey := AgyCredentialKey(cred)
		credVersion := AgyCredentialVersion(cred)
		geminiDir := filepath.Join(AgyGeminiDir(), accountKey)
		if err := StageAgyCredentials(cred, geminiDir); err != nil {
			yield(StreamEvent{}, err)
			return
		}
		if err := writeAgyBridgeAgent(geminiDir); err != nil {
			yield(StreamEvent{}, err)
			return
		}

		systemPrompt := joinSystem(req.System)
		toolsKey := agyToolsFingerprint(req.Tools)
		systemHash := agyFNV1a([]byte(systemPrompt))
		sessionKey := strings.TrimSpace(req.SessionID)
		if sessionKey == "" {
			if pk := strings.TrimSpace(req.PromptCacheKey); pk != "" {
				// PromptCacheKey is often shared across concurrent workers as a
				// prefix-cache hint; incorporate systemHash so distinct intents
				// never collide on the same stateful agy acp process slot.
				sessionKey = fmt.Sprintf("%s:%x", pk, systemHash)
			} else {
				sessionKey = fmt.Sprintf("%s:%x", p.defaultSessionKey, systemHash)
			}
		}
		slotKey := accountKey + ":" + sessionKey
		var slot *agyPoolSlot
		for {
			slot = getAgyPoolSlot(slotKey)
			slot.mu.Lock()
			if !slot.evicted {
				break
			}
			slot.mu.Unlock()
		}
		slot.lastUsed = time.Now()
		slot.abandoned = false
		turnCtx, cancelTurn := context.WithCancel(ctx)
		if !slot.setCancel(cancelTurn) {
			if slot.proc != nil {
				slot.proc.Kill()
				slot.proc = nil
			}
			slot.abandoned = true
			slot.mu.Unlock()
			yield(StreamEvent{}, turnCtx.Err())
			return
		}
		defer func() {
			slot.clearCancel()
			cancelTurn()
			now := time.Now()
			slot.lastUsed = now
			if slot.proc != nil {
				slot.proc.lastUsed = now
			}
			slot.abandoned = slot.proc == nil
			slot.mu.Unlock()
		}()

		var followUpMsgs []Message
		canContinue := false
		if slot.proc != nil && slot.proc.activePromptID == 0 && slot.proc.deliveredMessages > 0 {
			followUpMsgs, canContinue = agyFollowUpDelta(req.Messages, slot.proc.deliveredHashes)
		}

		needsRestart := slot.proc == nil ||
			slot.proc.toolsKey != toolsKey ||
			slot.proc.systemHash != systemHash ||
			slot.proc.credentialVersion != credVersion ||
			(slot.proc.activePromptID != 0 && !agyHasMatchingParkedResults(slot.proc, req.Messages)) ||
			(slot.proc.activePromptID == 0 && slot.proc.deliveredMessages > 0 && !canContinue)

		if needsRestart {
			if slot.proc != nil {
				slot.proc.Kill()
				slot.proc = nil
			}
			binary, berr := ResolveAgyBinary(turnCtx, p.httpClient())
			if berr != nil {
				yield(StreamEvent{}, berr)
				return
			}
			cwd, _ := os.Getwd()
			if cwd == "" {
				cwd = geminiDir
			}
			proc, serr := spawnAgyProcess(turnCtx, binary, geminiDir, cwd, systemPrompt, req.Tools, toolsKey, systemHash, credVersion)
			if serr != nil {
				yield(StreamEvent{}, serr)
				return
			}
			slot.proc = proc
		}

		proc := slot.proc
		proc.lastUsed = time.Now()

		modelID := strings.TrimSpace(p.cfg.Model)
		if modelID == "" {
			modelID = AgyDefaultModel
		}
		effort := req.ReasoningEffort
		if effort == "" {
			effort = p.cfg.ReasoningEffort
		}
		thinking := req.Thinking
		if thinking == "" {
			thinking = p.cfg.ThinkingType
		}
		if thinking == "disabled" {
			effort = ""
		}

		if proc.activePromptID == 0 {
			if cerr := configureAgySession(turnCtx, proc, modelID, effort); cerr != nil {
				proc.Kill()
				slot.proc = nil
				yield(StreamEvent{}, cerr)
				return
			}
		}

		if proc.activePromptID != 0 {
			agyDeliverParkedToolResults(proc, req.Messages)
			proc.deliveredMessages = len(req.Messages)
			proc.deliveredHashes = agyHashMessages(req.Messages)
		} else {
			var promptText string
			if proc.deliveredMessages == 0 {
				promptText = agyFormatTranscript(req.Messages)
			} else {
				promptText = agyFormatTranscript(followUpMsgs)
			}
			proc.nextID++
			promptID := proc.nextID
			promptBlocks := agyBuildPromptBlocks(proc.pendingSystem, promptText)
			proc.pendingSystem = ""
			if err := proc.sendRaw(map[string]any{
				"jsonrpc": "2.0",
				"id":      promptID,
				"method":  "session/prompt",
				"params": map[string]any{
					"sessionId": proc.sessionID,
					"prompt":    promptBlocks,
				},
			}); err != nil {
				proc.Kill()
				slot.proc = nil
				yield(StreamEvent{}, err)
				return
			}
			proc.activePromptID = promptID
			proc.deliveredMessages = len(req.Messages)
			proc.deliveredHashes = agyHashMessages(req.Messages)
		}

		if aborted, err := agyDriveTurn(turnCtx, proc, yield); err != nil {
			proc.Kill()
			slot.proc = nil
			yield(StreamEvent{}, err)
		} else if aborted {
			proc.Kill()
			slot.proc = nil
		}
	}
}

func spawnAgyProcess(
	ctx context.Context,
	binary, geminiDir, cwd, systemPrompt string,
	tools []ToolSchema,
	toolsKey string,
	systemHash uint64,
	credVersion string,
) (*agyProcess, error) {
	if !HasNCOnPath() {
		return nil, errors.New("Antigravity CLI requires `nc` (netcat) on PATH for the MCP tool bridge")
	}
	bridge, err := startAgyMCPBridge(tools)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(
		binary,
		"acp",
		"--agent", agyAgentName,
		"--gemini-dir", geminiDir,
		"--ide-mcp-port", strconv.Itoa(bridge.Port()),
		"--dangerously-skip-permissions",
	)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), AgyCredentialEnv()...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		bridge.Close()
		return nil, fmt.Errorf("agy acp stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		bridge.Close()
		return nil, fmt.Errorf("agy acp stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		bridge.Close()
		return nil, fmt.Errorf("agy acp stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		bridge.Close()
		return nil, fmt.Errorf("spawn agy acp (%s): %w", binary, err)
	}

	// Drain stderr so the child never blocks on a full pipe.
	go func() { _, _ = io.Copy(io.Discard, stderr) }()

	msgCh := make(chan agyACPMessage, 128)
	done := make(chan struct{})
	go func() {
		defer close(msgCh)
		send := func(m agyACPMessage) bool {
			select {
			case msgCh <- m:
				return true
			case <-done:
				return false
			}
		}
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			var raw struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if err := json.Unmarshal([]byte(line), &raw); err != nil {
				continue
			}
			hasID := len(raw.ID) > 0 && string(raw.ID) != "null"
			if raw.Method != "" {
				if hasID {
					if !send(agyACPMessage{kind: agyMsgServerRequest, rawID: raw.ID, method: raw.Method}) {
						return
					}
				} else {
					if !send(agyACPMessage{kind: agyMsgNotification, method: raw.Method, params: raw.Params}) {
						return
					}
				}
			} else if hasID {
				var idNum uint64
				if err := json.Unmarshal(raw.ID, &idNum); err == nil {
					if !send(agyACPMessage{kind: agyMsgResponse, id: idNum, result: raw.Result, errObj: raw.Error}) {
						return
					}
				}
			}
		}
		_ = send(agyACPMessage{kind: agyMsgEOF})
	}()

	proc := &agyProcess{
		cmd:               cmd,
		stdin:             stdin,
		msgCh:             msgCh,
		done:              done,
		bridge:            bridge,
		toolsKey:          toolsKey,
		systemHash:        systemHash,
		credentialVersion: credVersion,
		pendingSystem:     strings.TrimSpace(systemPrompt),
		parked:            map[string]chan agyMCPToolOutput{},
		lastUsed:          time.Now(),
	}

	if _, err := proc.rpc(ctx, "initialize", map[string]any{
		"protocolVersion":    1,
		"clientCapabilities": map[string]any{},
		"clientInfo":         map[string]any{"name": "artex", "version": "2.0"},
	}); err != nil {
		proc.Kill()
		return nil, err
	}

	createdRaw, err := proc.rpc(ctx, "session/new", map[string]any{
		"cwd":        cwd,
		"mcpServers": []any{},
	})
	if err != nil {
		proc.Kill()
		return nil, err
	}
	var created struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(createdRaw, &created)
	proc.sessionID = created.SessionID
	proc.advertisedModels = agyExtractConfigOptionValues(createdRaw, "model")
	return proc, nil
}

func configureAgySession(ctx context.Context, proc *agyProcess, modelID, effort string) error {
	if proc.sessionID == "" {
		return nil
	}
	agyModel := AgyModelValue(modelID, effort, proc.advertisedModels)
	if proc.configuredModel != agyModel {
		respRaw, err := proc.rpc(ctx, "session/set_config_option", map[string]any{
			"sessionId": proc.sessionID,
			"configId":  "model",
			"value":     agyModel,
		})
		if err != nil {
			return err
		}
		if updated := agyExtractConfigOptionValues(respRaw, "model"); len(updated) > 0 {
			proc.advertisedModels = updated
		}
		proc.configuredModel = agyModel
	}
	if agyModel == modelID && effort != "" && proc.configuredEffort != effort {
		if _, err := proc.rpc(ctx, "session/set_config_option", map[string]any{
			"sessionId": proc.sessionID,
			"configId":  "thought_level",
			"value":     effort,
		}); err != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		proc.configuredEffort = effort
	}
	return nil
}

func agyDriveTurn(ctx context.Context, proc *agyProcess, yield func(StreamEvent, error) bool) (bool, error) {
	promptID := proc.activePromptID
	started := false
	sawToolCall := false
	var toolIndex int
	var textTail strings.Builder
	totalTextLen := 0
	bufferingBanner := true
	var pendingBanner strings.Builder

	emit := func(ev StreamEvent) bool {
		if !started {
			started = true
			if !yield(StreamEvent{Type: SEMessageStart}, nil) {
				return false
			}
		}
		return yield(ev, nil)
	}

	flushBanner := func() bool {
		bufferingBanner = false
		if pendingBanner.Len() == 0 {
			return true
		}
		txt := pendingBanner.String()
		pendingBanner.Reset()
		return emit(StreamEvent{Type: SETextDelta, Text: txt})
	}

	idleTimer := time.NewTimer(agyEventIdleTimeout)
	defer idleTimer.Stop()
	var debounceTimer *time.Timer
	var debounceCh <-chan time.Time

	stopDebounce := func() {
		if debounceTimer != nil {
			debounceTimer.Stop()
			debounceTimer = nil
			debounceCh = nil
		}
	}
	defer stopDebounce()

	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()

		case <-debounceCh:
			stopDebounce()
			if !emit(StreamEvent{Type: SEMessageDelta, StopReason: "tool_use"}) {
				return true, nil
			}
			yield(StreamEvent{Type: SEMessageStop}, nil)
			return false, nil

		case parked := <-proc.bridge.callCh:
			idleTimer.Reset(agyEventIdleTimeout)
			if !flushBanner() {
				return true, nil
			}
			args := strings.TrimSpace(string(parked.Arguments))
			if args == "" {
				args = "{}"
			}
			if !emit(StreamEvent{
				Type:      SEToolUseStart,
				ToolID:    parked.CallID,
				ToolName:  parked.Name,
				ToolIndex: toolIndex,
			}) {
				return true, nil
			}
			if !emit(StreamEvent{Type: SEToolInputJSON, Text: args, ToolIndex: toolIndex}) {
				return true, nil
			}
			if !emit(StreamEvent{Type: SEToolUseStop, ToolIndex: toolIndex}) {
				return true, nil
			}
			toolIndex++
			proc.parked[parked.CallID] = parked.Reply
			sawToolCall = true
			if debounceTimer == nil {
				debounceTimer = time.NewTimer(agyParallelToolDebounce)
				debounceCh = debounceTimer.C
			} else {
				debounceTimer.Reset(agyParallelToolDebounce)
			}

		case msg, ok := <-proc.msgCh:
			idleTimer.Reset(agyEventIdleTimeout)
			if !ok || msg.kind == agyMsgEOF {
				proc.activePromptID = 0
				return false, errors.New("Antigravity CLI subprocess exited unexpectedly")
			}
			switch msg.kind {
			case agyMsgServerRequest:
				proc.replyServerRequest(msg.rawID, msg.method)
			case agyMsgNotification:
				if msg.method != "session/update" {
					continue
				}
				kind, text := agyExtractSessionUpdate(msg.params)
				if text == "" {
					continue
				}
				switch kind {
				case "agent_thought_chunk":
					if !emit(StreamEvent{Type: SEThinkingDelta, Text: text}) {
						return true, nil
					}
				case "agent_message_chunk":
					totalTextLen += len(text)
					if textTail.Len() < 2048 {
						textTail.WriteString(text)
					}
					if bufferingBanner && !sawToolCall {
						pendingBanner.WriteString(text)
						if totalTextLen > 600 || !agyCouldBeQuotaBannerPrefix(pendingBanner.String()) {
							if !flushBanner() {
								return true, nil
							}
						}
					} else {
						if !emit(StreamEvent{Type: SETextDelta, Text: text}) {
							return true, nil
						}
					}
				}
			case agyMsgResponse:
				if msg.id != promptID {
					continue
				}
				proc.activePromptID = 0
				stopDebounce()
				if len(msg.errObj) > 0 && string(msg.errObj) != "null" {
					var e struct {
						Message string `json:"message"`
					}
					_ = json.Unmarshal(msg.errObj, &e)
					if e.Message == "" {
						e.Message = "Antigravity CLI prompt failed"
					}
					if qerr := agyQuotaNoticeError(e.Message); qerr != nil {
						return false, qerr
					}
					return false, &HTTPStatusError{
						Prefix:     "antigravity-cli",
						StatusCode: http.StatusBadGateway,
						Body:       e.Message,
					}
				}
				if qerr := agyStandaloneQuotaNoticeError(textTail.String(), totalTextLen, sawToolCall); qerr != nil {
					return false, qerr
				}
				if !flushBanner() {
					return true, nil
				}
				stopReason := "end_turn"
				if sawToolCall {
					stopReason = "tool_use"
				}
				if !emit(StreamEvent{Type: SEMessageDelta, StopReason: stopReason}) {
					return true, nil
				}
				yield(StreamEvent{Type: SEMessageStop}, nil)
				return false, nil
			}

		case <-idleTimer.C:
			return false, &HTTPStatusError{
				Prefix:     "antigravity-cli",
				StatusCode: http.StatusGatewayTimeout,
				Body:       fmt.Sprintf("Antigravity CLI timed out waiting for ACP event after %s", agyEventIdleTimeout),
			}
		}
	}
}

func agyQuotaNoticeError(text string) error {
	if strings.Contains(text, "RESOURCE_EXHAUSTED") ||
		strings.Contains(text, "code 429") ||
		strings.Contains(text, "Quota reached") ||
		strings.Contains(text, "quota reached") {
		return &HTTPStatusError{
			Prefix:     "antigravity-cli",
			StatusCode: http.StatusTooManyRequests,
			Body:       "Antigravity CLI quota exhausted: " + strings.TrimSpace(text),
		}
	}
	return nil
}

var agyQuotaBannerPrefixes = []string{
	"api error:",
	"error:",
	"resource_exhausted",
	"quota reached",
	"you have reached",
}

// agyCouldBeQuotaBannerPrefix returns true while buffered leading text could
// still become one of agyStandaloneQuotaNoticeError's standalone quota banners.
func agyCouldBeQuotaBannerPrefix(text string) bool {
	trimmed := strings.TrimLeftFunc(text, unicode.IsSpace)
	if trimmed == "" {
		return true
	}
	low := strings.ToLower(trimmed)
	for _, prefix := range agyQuotaBannerPrefixes {
		if strings.HasPrefix(prefix, low) || strings.HasPrefix(low, prefix) {
			return true
		}
	}
	return false
}

// agyStandaloneQuotaNoticeError classifies assistant text as an `agy` quota
// banner only when no tool calls occurred and the complete response is a short
// standalone error notice — preventing legitimate prose discussing HTTP 429 or
// RESOURCE_EXHAUSTED from aborting turns.
func agyStandaloneQuotaNoticeError(text string, totalLen int, sawToolCall bool) error {
	if sawToolCall || totalLen == 0 || totalLen > 600 {
		return nil
	}
	trimmed := strings.TrimSpace(text)
	low := strings.ToLower(trimmed)
	isBanner := false
	for _, prefix := range agyQuotaBannerPrefixes {
		if strings.HasPrefix(low, prefix) {
			isBanner = true
			break
		}
	}
	if !isBanner {
		return nil
	}
	return agyQuotaNoticeError(trimmed)
}

func agyExtractSessionUpdate(params json.RawMessage) (kind, text string) {
	var env struct {
		Update *struct {
			SessionUpdate string `json:"sessionUpdate"`
			Type          string `json:"type"`
			Text          string `json:"text"`
			Content       *struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"update"`
		SessionUpdate string `json:"sessionUpdate"`
		Type          string `json:"type"`
		Text          string `json:"text"`
		Content       *struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(params, &env); err != nil {
		return "", ""
	}
	if env.Update != nil {
		kind = env.Update.SessionUpdate
		if kind == "" {
			kind = env.Update.Type
		}
		if env.Update.Content != nil && env.Update.Content.Text != "" {
			text = env.Update.Content.Text
		} else {
			text = env.Update.Text
		}
		return kind, text
	}
	kind = env.SessionUpdate
	if kind == "" {
		kind = env.Type
	}
	if env.Content != nil && env.Content.Text != "" {
		text = env.Content.Text
	} else {
		text = env.Text
	}
	return kind, text
}

func agyExtractConfigOptionValues(payload json.RawMessage, configID string) []string {
	var doc struct {
		ConfigOptions []struct {
			ID      string `json:"id"`
			Options []struct {
				Value   string `json:"value"`
				Options []struct {
					Value string `json:"value"`
				} `json:"options"`
			} `json:"options"`
		} `json:"configOptions"`
	}
	if err := json.Unmarshal(payload, &doc); err != nil {
		return nil
	}
	for _, opt := range doc.ConfigOptions {
		if opt.ID != configID {
			continue
		}
		var vals []string
		for _, item := range opt.Options {
			if item.Value != "" {
				vals = append(vals, item.Value)
			}
			for _, sub := range item.Options {
				if sub.Value != "" {
					vals = append(vals, sub.Value)
				}
			}
		}
		return vals
	}
	return nil
}

func agyMessageHash(m Message) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(m.Role))
	_, _ = h.Write([]byte{0})
	for _, b := range m.Content {
		_, _ = h.Write([]byte(b.Type))
		_, _ = h.Write([]byte{1})
		_, _ = h.Write([]byte(b.Text))
		_, _ = h.Write([]byte{1})
		_, _ = h.Write([]byte(b.ID))
		_, _ = h.Write([]byte{1})
		_, _ = h.Write([]byte(b.Name))
		_, _ = h.Write([]byte{1})
		_, _ = h.Write(b.Input)
		_, _ = h.Write([]byte{1})
		_, _ = h.Write([]byte(b.ToolUseID))
		if b.IsError {
			_, _ = h.Write([]byte{2})
		} else {
			_, _ = h.Write([]byte{3})
		}
		for _, sub := range b.Content {
			_, _ = h.Write([]byte(sub.Type))
			_, _ = h.Write([]byte{4})
			_, _ = h.Write([]byte(sub.Text))
			_, _ = h.Write([]byte{4})
		}
	}
	return h.Sum64()
}

func agyHashMessages(msgs []Message) []uint64 {
	out := make([]uint64, len(msgs))
	for i, m := range msgs {
		out[i] = agyMessageHash(m)
	}
	return out
}

// agyFollowUpDelta verifies msgs is a strict continuation of deliveredHashes
// (catching compaction or a prepended/changed SystemReminder at index 0) and
// returns the unsent follow-up slice while skipping leading assistant message(s)
// already generated inside the live ACP session.
func agyFollowUpDelta(msgs []Message, deliveredHashes []uint64) ([]Message, bool) {
	if len(deliveredHashes) == 0 || len(msgs) <= len(deliveredHashes) {
		return nil, false
	}
	for i, want := range deliveredHashes {
		if agyMessageHash(msgs[i]) != want {
			return nil, false
		}
	}
	start := len(deliveredHashes)
	for start < len(msgs) && msgs[start].Role == RoleAssistant {
		start++
	}
	if start >= len(msgs) {
		return nil, false
	}
	return msgs[start:], true
}

func agyTrailingUserMessages(messages []Message) []Message {
	start := len(messages)
	for start > 0 && messages[start-1].Role == RoleUser {
		start--
	}
	return messages[start:]
}

func agyHasMatchingParkedResults(proc *agyProcess, messages []Message) bool {
	if len(proc.parked) == 0 || len(messages) == 0 {
		return false
	}
	for _, msg := range agyTrailingUserMessages(messages) {
		for _, b := range msg.Content {
			if b.Type == BlockToolResult {
				if _, ok := proc.parked[b.ToolUseID]; ok {
					return true
				}
			}
		}
	}
	return false
}

func agyDeliverParkedToolResults(proc *agyProcess, messages []Message) {
	type parkedDelivery struct {
		ch  chan agyMCPToolOutput
		out agyMCPToolOutput
	}
	var deliveries []parkedDelivery
	var extraUserText []string
	for _, msg := range agyTrailingUserMessages(messages) {
		for _, b := range msg.Content {
			switch b.Type {
			case BlockToolResult:
				if ch, ok := proc.parked[b.ToolUseID]; ok {
					delete(proc.parked, b.ToolUseID)
					var sb strings.Builder
					for _, tb := range b.Content {
						if tb.Type == BlockText {
							sb.WriteString(tb.Text)
						}
					}
					deliveries = append(deliveries, parkedDelivery{
						ch:  ch,
						out: agyMCPToolOutput{Text: sb.String(), IsError: b.IsError},
					})
				}
			case BlockText:
				if t := strings.TrimSpace(b.Text); t != "" {
					extraUserText = append(extraUserText, t)
				}
			}
		}
	}
	if len(deliveries) > 0 && len(extraUserText) > 0 {
		last := &deliveries[len(deliveries)-1]
		suffix := strings.Join(extraUserText, "\n\n")
		if strings.TrimSpace(last.out.Text) != "" {
			last.out.Text = last.out.Text + "\n\n" + suffix
		} else {
			last.out.Text = suffix
		}
	}
	for _, d := range deliveries {
		d.ch <- d.out
	}
	for k, ch := range proc.parked {
		delete(proc.parked, k)
		ch <- agyMCPToolOutput{Text: "Tool call skipped by host.", IsError: true}
	}
}

func agyBuildPromptBlocks(pendingSystem, promptText string) []map[string]any {
	blocks := make([]map[string]any, 0, 2)
	if strings.TrimSpace(pendingSystem) != "" {
		blocks = append(blocks, map[string]any{
			"type": "text",
			"text": "<system-reminder>\n" + pendingSystem + "\n</system-reminder>",
		})
	}
	blocks = append(blocks, map[string]any{
		"type": "text",
		"text": promptText,
	})
	return blocks
}

func agyFormatTranscript(messages []Message) string {
	var parts []string
	for _, m := range messages {
		switch m.Role {
		case RoleUser:
			for _, b := range m.Content {
				switch b.Type {
				case BlockText:
					if strings.TrimSpace(b.Text) != "" {
						parts = append(parts, b.Text)
					}
				case BlockToolResult:
					var sb strings.Builder
					for _, tb := range b.Content {
						if tb.Type == BlockText {
							sb.WriteString(tb.Text)
						}
					}
					if strings.TrimSpace(sb.String()) != "" {
						parts = append(parts, "[tool result]\n"+sb.String())
					}
				}
			}
		case RoleAssistant:
			var asstParts []string
			if txt := strings.TrimSpace(m.Text()); txt != "" {
				asstParts = append(asstParts, txt)
			}
			for _, b := range m.Content {
				if b.Type == BlockToolUse {
					args := strings.TrimSpace(string(b.Input))
					if args == "" {
						args = "{}"
					}
					asstParts = append(asstParts, fmt.Sprintf("[tool call: %s] %s", b.Name, args))
				}
			}
			if len(asstParts) > 0 {
				parts = append(parts, "[previous assistant]\n"+strings.Join(asstParts, "\n"))
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

func writeAgyBridgeAgent(geminiDir string) error {
	p := filepath.Join(geminiDir, "config", "agents", agyAgentName+".md")
	if existing, err := os.ReadFile(p); err == nil && string(existing) == agyBridgeMD {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("create bridge agent dir: %w", err)
	}
	tmp := fmt.Sprintf("%s.tmp-%d-%d-%d", p, os.Getpid(), time.Now().UnixNano(), agyStageNonce.Add(1))
	if err := os.WriteFile(tmp, []byte(agyBridgeMD), 0o644); err != nil {
		return fmt.Errorf("write bridge agent definition: %w", err)
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("stage bridge agent definition: %w", err)
	}
	return nil
}

func agyToolsFingerprint(tools []ToolSchema) string {
	var sb strings.Builder
	for _, t := range tools {
		sb.WriteString(t.Name)
		sb.WriteByte(':')
		if raw, err := json.Marshal(t.InputSchema); err == nil {
			sb.Write(raw)
		}
		sb.WriteByte('\n')
	}
	return fmt.Sprintf("%016x", agyFNV1a([]byte(sb.String())))
}

func agyFNV1a(b []byte) uint64 {
	h := fnv.New64a()
	_, _ = h.Write(b)
	return h.Sum64()
}
