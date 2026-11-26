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
	agyIdleRetire           = 5 * time.Minute
	agyEventIdleTimeout     = 300 * time.Second
	agyDefaultMaxProcs      = 6
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
	mu   sync.Mutex
	proc *agyProcess
}

var (
	agyPoolMu    sync.Mutex
	agyPoolSlots = map[string]*agyPoolSlot{}
	agySessionSeq atomic.Uint64
)

func getAgyPoolSlot(slotKey string) *agyPoolSlot {
	var toKill []*agyProcess
	maxProcs := agyMaxProcs()

	agyPoolMu.Lock()
	if slot, ok := agyPoolSlots[slotKey]; ok {
		agyPoolMu.Unlock()
		return slot
	}

	// Evict idle processes first, then LRU non-busy process if at cap.
	now := time.Now()
	for k, s := range agyPoolSlots {
		if s.mu.TryLock() {
			if s.proc != nil && now.Sub(s.proc.lastUsed) > agyIdleRetire {
				toKill = append(toKill, s.proc)
				s.proc = nil
				delete(agyPoolSlots, k)
			}
			s.mu.Unlock()
		}
	}
	for len(agyPoolSlots) >= maxProcs {
		var oldestKey string
		var oldestTime time.Time
		var oldestSlot *agyPoolSlot
		for k, s := range agyPoolSlots {
			if s.mu.TryLock() {
				t := time.Time{}
				if s.proc != nil {
					t = s.proc.lastUsed
				}
				if oldestSlot == nil || t.Before(oldestTime) {
					oldestKey = k
					oldestTime = t
					oldestSlot = s
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
			delete(agyPoolSlots, oldestKey)
			oldestSlot.mu.Unlock()
		} else {
			break
		}
	}

	slot := &agyPoolSlot{}
	agyPoolSlots[slotKey] = slot
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
	agyPoolMu.Unlock()

	for _, s := range slots {
		s.mu.Lock()
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
		s.mu.Lock()
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
		sessionKey := strings.TrimSpace(req.PromptCacheKey)
		if sessionKey == "" {
			sessionKey = p.defaultSessionKey
		}
		slotKey := accountKey + ":" + sessionKey
		slot := getAgyPoolSlot(slotKey)
		slot.mu.Lock()
		defer slot.mu.Unlock()

		needsRestart := slot.proc == nil ||
			slot.proc.toolsKey != toolsKey ||
			slot.proc.systemHash != systemHash ||
			slot.proc.credentialVersion != credVersion ||
			(slot.proc.activePromptID != 0 && !agyHasMatchingParkedResults(slot.proc, req.Messages)) ||
			(slot.proc.activePromptID == 0 && slot.proc.deliveredMessages > 0 && len(req.Messages) <= slot.proc.deliveredMessages)

		if needsRestart {
			if slot.proc != nil {
				slot.proc.Kill()
				slot.proc = nil
			}
			binary, berr := ResolveAgyBinary(ctx, p.httpClient())
			if berr != nil {
				yield(StreamEvent{}, berr)
				return
			}
			cwd, _ := os.Getwd()
			if cwd == "" {
				cwd = geminiDir
			}
			proc, serr := spawnAgyProcess(ctx, binary, geminiDir, cwd, systemPrompt, req.Tools, toolsKey, systemHash, credVersion)
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
			if cerr := configureAgySession(ctx, proc, modelID, effort); cerr != nil {
				proc.Kill()
				slot.proc = nil
				yield(StreamEvent{}, cerr)
				return
			}
		}

		if proc.activePromptID != 0 {
			agyDeliverParkedToolResults(proc, req.Messages)
			proc.deliveredMessages = len(req.Messages)
		} else {
			var promptText string
			if proc.deliveredMessages == 0 {
				promptText = agyFormatTranscript(req.Messages)
			} else {
				start := min(proc.deliveredMessages, len(req.Messages))
				promptText = agyFormatTranscript(req.Messages[start:])
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
		}

		if aborted, err := agyDriveTurn(ctx, proc, yield); err != nil {
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
	go func() {
		defer close(msgCh)
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
					msgCh <- agyACPMessage{kind: agyMsgServerRequest, rawID: raw.ID, method: raw.Method}
				} else {
					msgCh <- agyACPMessage{kind: agyMsgNotification, method: raw.Method, params: raw.Params}
				}
			} else if hasID {
				var idNum uint64
				if err := json.Unmarshal(raw.ID, &idNum); err == nil {
					msgCh <- agyACPMessage{kind: agyMsgResponse, id: idNum, result: raw.Result, errObj: raw.Error}
				}
			}
		}
		msgCh <- agyACPMessage{kind: agyMsgEOF}
	}()

	proc := &agyProcess{
		cmd:               cmd,
		stdin:             stdin,
		msgCh:             msgCh,
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
		_, _ = proc.rpc(ctx, "session/set_config_option", map[string]any{
			"sessionId": proc.sessionID,
			"configId":  "thought_level",
			"value":     effort,
		})
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

	emit := func(ev StreamEvent) bool {
		if !started {
			started = true
			if !yield(StreamEvent{Type: SEMessageStart}, nil) {
				return false
			}
		}
		return yield(ev, nil)
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
					if qerr := agyQuotaNoticeError(text); qerr != nil {
						return false, qerr
					}
					textTail.WriteString(text)
					if textTail.Len() > 2048 {
						s := textTail.String()
						textTail.Reset()
						textTail.WriteString(s[len(s)-1024:])
					}
					if !emit(StreamEvent{Type: SETextDelta, Text: text}) {
						return true, nil
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
				if qerr := agyQuotaNoticeError(textTail.String()); qerr != nil {
					return false, qerr
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

func agyHasMatchingParkedResults(proc *agyProcess, messages []Message) bool {
	if len(proc.parked) == 0 || len(messages) == 0 {
		return false
	}
	last := messages[len(messages)-1]
	if last.Role != RoleUser {
		return false
	}
	for _, b := range last.Content {
		if b.Type == BlockToolResult {
			if _, ok := proc.parked[b.ToolUseID]; ok {
				return true
			}
		}
	}
	return false
}

func agyDeliverParkedToolResults(proc *agyProcess, messages []Message) {
	if len(messages) > 0 {
		last := messages[len(messages)-1]
		if last.Role == RoleUser {
			for _, b := range last.Content {
				if b.Type != BlockToolResult {
					continue
				}
				if ch, ok := proc.parked[b.ToolUseID]; ok {
					delete(proc.parked, b.ToolUseID)
					var sb strings.Builder
					for _, tb := range b.Content {
						if tb.Type == BlockText {
							sb.WriteString(tb.Text)
						}
					}
					ch <- agyMCPToolOutput{Text: sb.String(), IsError: b.IsError}
				}
			}
		}
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
			if txt := strings.TrimSpace(m.Text()); txt != "" {
				parts = append(parts, "[previous assistant]\n"+txt)
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
	if err := os.WriteFile(p, []byte(agyBridgeMD), 0o644); err != nil {
		return fmt.Errorf("write bridge agent definition: %w", err)
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
