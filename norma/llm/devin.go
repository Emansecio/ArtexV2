package llm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strings"
	"sync"
	"time"
)

type cachedDevinJWT struct {
	userJWT   string
	baseURL   string
	expiresAt time.Time
}

var (
	devinJWTMu    sync.Mutex
	devinJWTCache = map[[32]byte]cachedDevinJWT{}
)

func devinJWTKey(endpoint, apiKey string) [32]byte {
	h := sha256.New()
	h.Write([]byte(strings.TrimRight(endpoint, "/")))
	h.Write([]byte{0})
	h.Write([]byte(strings.TrimSpace(apiKey)))
	var k [32]byte
	copy(k[:], h.Sum(nil))
	return k
}

func devinJWTExpTTL(jwt string) (time.Duration, bool) {
	parts := strings.Split(jwt, ".")
	if len(parts) < 2 {
		return 0, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp <= 0 {
		return 0, false
	}
	remaining := time.Until(time.Unix(claims.Exp, 0))
	const safetyMargin = 60 * time.Second
	if remaining <= safetyMargin {
		return 0, true
	}
	ttl := remaining - safetyMargin
	if ttl < 30*time.Second {
		ttl = 30 * time.Second
	}
	if ttl > 15*time.Minute {
		ttl = 15 * time.Minute
	}
	return ttl, true
}

func devinJWTCacheTTL(jwt string) time.Duration {
	if ttl, ok := devinJWTExpTTL(jwt); ok {
		return ttl
	}
	return 5 * time.Minute
}

func getValidCachedDevinJWT(endpoint, apiKey string) (string, string, bool) {
	devinJWTMu.Lock()
	defer devinJWTMu.Unlock()
	c, ok := devinJWTCache[devinJWTKey(endpoint, apiKey)]
	if !ok || time.Now().After(c.expiresAt) {
		return "", "", false
	}
	return c.userJWT, c.baseURL, true
}

func invalidateCachedDevinJWT(endpoint, apiKey string) {
	devinJWTMu.Lock()
	delete(devinJWTCache, devinJWTKey(endpoint, apiKey))
	devinJWTMu.Unlock()
}

type cachedDevinAssignment struct {
	modelUID      string
	assignmentJWT string
	expiresAt     time.Time
}

type devinProvider struct {
	cfg        Config
	endpoint   string
	assignMu   sync.Mutex
	assignMemo map[[32]byte]cachedDevinAssignment
}

func newDevinProvider(cfg Config) *devinProvider {
	ep := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if ep == "" {
		ep = DevinDefaultBaseURL
	}
	return &devinProvider{
		cfg:        cfg,
		endpoint:   ep,
		assignMemo: map[[32]byte]cachedDevinAssignment{},
	}
}

func (p *devinProvider) httpClient() *http.Client {
	if p.cfg.HTTPClient != nil {
		return p.cfg.HTTPClient
	}
	return http.DefaultClient
}

func (p *devinProvider) resolveAPIKey(ctx context.Context, forceRefresh bool) (string, error) {
	if p.cfg.TokenFunc != nil {
		return p.cfg.TokenFunc(ctx, forceRefresh)
	}
	key := strings.TrimSpace(p.cfg.APIKey)
	if key == "" {
		return "", &HTTPStatusError{
			Prefix:     "devin",
			StatusCode: http.StatusUnauthorized,
			Body:       "missing Devin OAuth credential; sign in again in Models",
		}
	}
	return key, nil
}

func (p *devinProvider) unaryRPC(ctx context.Context, baseURL, path string, body []byte, op string) ([]byte, error) {
	url := strings.TrimRight(baseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/proto")
	req.Header.Set("connect-protocol-version", "1")
	req.Header.Set("accept", "*/*")

	resp, err := p.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("devin %s connection: %w", op, err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("devin %s body: %w", op, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &HTTPStatusError{
			Prefix:     "devin " + op,
			StatusCode: resp.StatusCode,
			Body:       devinHTTPErrorDetail(payload),
			RetryAfter: parseRetryAfter(resp.Header),
		}
	}
	return payload, nil
}

func (p *devinProvider) resolveUserJWT(ctx context.Context, apiKey string, forceRefresh bool) (string, string, error) {
	if !forceRefresh {
		if jwt, base, ok := getValidCachedDevinJWT(p.endpoint, apiKey); ok {
			return jwt, base, nil
		}
	}
	meta := encodeDevinCLIMetadata(apiKey, "")
	payload, err := p.unaryRPC(ctx, p.endpoint, devinAuthPath, encodeDevinUnaryRequest(meta), "auth")
	if err != nil {
		return "", "", err
	}
	res, ok := decodeDevinUnary(payload, decodeDevinGetUserJWT)
	if !ok {
		return "", "", fmt.Errorf("devin auth error: GetUserJwt returned empty user JWT")
	}
	baseURL := p.endpoint
	if custom := strings.TrimRight(strings.TrimSpace(res.customAPIServerURL), "/"); custom != "" {
		baseURL = custom
	}
	if ttl := devinJWTCacheTTL(res.userJWT); ttl > 0 {
		devinJWTMu.Lock()
		if len(devinJWTCache) >= 32 {
			now := time.Now()
			for k, v := range devinJWTCache {
				if now.After(v.expiresAt) {
					delete(devinJWTCache, k)
				}
			}
			if len(devinJWTCache) >= 32 {
				clear(devinJWTCache)
			}
		}
		devinJWTCache[devinJWTKey(p.endpoint, apiKey)] = cachedDevinJWT{
			userJWT:   res.userJWT,
			baseURL:   baseURL,
			expiresAt: time.Now().Add(ttl),
		}
		devinJWTMu.Unlock()
	}
	return res.userJWT, baseURL, nil
}

func devinAssignMemoKey(userJWT, cascadeID, routerUID string, routerPrompt []byte) [32]byte {
	h := sha256.New()
	h.Write([]byte("artex-devin-assign-v1\x00"))
	h.Write([]byte(userJWT))
	h.Write([]byte{0})
	h.Write([]byte(cascadeID))
	h.Write([]byte{0})
	h.Write([]byte(routerUID))
	h.Write([]byte{0})
	h.Write(routerPrompt)
	var k [32]byte
	copy(k[:], h.Sum(nil))
	return k
}

func (p *devinProvider) getMemoAssignment(k [32]byte) (string, string, bool) {
	p.assignMu.Lock()
	defer p.assignMu.Unlock()
	a, ok := p.assignMemo[k]
	if !ok {
		return "", "", false
	}
	if time.Now().After(a.expiresAt) {
		delete(p.assignMemo, k)
		return "", "", false
	}
	return a.modelUID, a.assignmentJWT, true
}

func (p *devinProvider) putMemoAssignment(k [32]byte, modelUID, assignmentJWT string) {
	ttl, ok := devinJWTExpTTL(assignmentJWT)
	if !ok || ttl <= 0 {
		return
	}
	p.assignMu.Lock()
	defer p.assignMu.Unlock()
	if len(p.assignMemo) >= 16 {
		now := time.Now()
		for mk, v := range p.assignMemo {
			if now.After(v.expiresAt) {
				delete(p.assignMemo, mk)
			}
		}
		if len(p.assignMemo) >= 16 {
			clear(p.assignMemo)
		}
	}
	p.assignMemo[k] = cachedDevinAssignment{
		modelUID:      modelUID,
		assignmentJWT: assignmentJWT,
		expiresAt:     time.Now().Add(ttl),
	}
}

func (p *devinProvider) clearAssignments() {
	p.assignMu.Lock()
	clear(p.assignMemo)
	p.assignMu.Unlock()
}

func (p *devinProvider) assignModel(ctx context.Context, baseURL, apiKey, userJWT, routerUID, cascadeID string, routerPrompt []byte) (string, string, error) {
	body := make([]byte, 0, 512)
	body = protoAppendLenField(body, 1, encodeDevinCLIMetadata(apiKey, userJWT))
	body = protoAppendStringField(body, 2, routerUID)
	body = protoAppendStringField(body, 3, cascadeID)
	body = protoAppendLenField(body, 5, routerPrompt)

	payload, err := p.unaryRPC(ctx, baseURL, devinAssignModelPath, body, "AssignModel")
	if err != nil {
		return "", "", err
	}
	a, ok := decodeDevinUnary(payload, decodeDevinAssignModel)
	if !ok {
		return "", "", fmt.Errorf("devin AssignModel returned no assignment JWT and model uid")
	}
	return a.modelUID, a.assignmentJWT, nil
}

func accumulateStream(seq iter.Seq2[StreamEvent, error]) (Message, string, Usage, error) {
	acc := NewAccumulator()
	for ev, err := range seq {
		if err != nil {
			return Message{}, "", Usage{}, err
		}
		acc.Add(ev)
	}
	return acc.Message(), acc.StopReason, acc.Usage, nil
}

func (p *devinProvider) Complete(ctx context.Context, req CompletionRequest) (Message, string, Usage, error) {
	return accumulateStream(p.Stream(ctx, req))
}

func (p *devinProvider) Stream(ctx context.Context, req CompletionRequest) iter.Seq2[StreamEvent, error] {
	return func(yield func(StreamEvent, error) bool) {
		if err := p.cfg.limiter.Wait(ctx); err != nil {
			yield(StreamEvent{}, err)
			return
		}
		apiKey, err := p.resolveAPIKey(ctx, false)
		if err != nil {
			yield(StreamEvent{}, err)
			return
		}
		userJWT, baseURL, err := p.resolveUserJWT(ctx, apiKey, false)
		if err != nil {
			var hse *HTTPStatusError
			if errors.As(err, &hse) && (hse.StatusCode == http.StatusUnauthorized || hse.StatusCode == http.StatusForbidden) {
				invalidateCachedDevinJWT(p.endpoint, apiKey)
				if p.cfg.TokenFunc != nil {
					if freshKey, ferr := p.resolveAPIKey(ctx, true); ferr == nil {
						apiKey = freshKey
					} else {
						yield(StreamEvent{}, ferr)
						return
					}
				}
				userJWT, baseURL, err = p.resolveUserJWT(ctx, apiKey, true)
			}
			if err != nil {
				yield(StreamEvent{}, err)
				return
			}
		}

		// Resolve model, effort route, router flag, and parallel tool support.
		modelID := strings.TrimSpace(p.cfg.Model)
		if modelID == "" {
			modelID = DevinDefaultModel
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
			effort = "off"
		}

		chatModelUID := modelID
		modelRouter := IsDevinRouterModel(modelID)
		parallelTools := false
		maxOutput := req.MaxTokens
		if entry, ok := ensureDevinCatalogEntry(ctx, p.httpClient(), p.endpoint, apiKey, modelID); ok {
			chatModelUID = entry.ChatModelUID(effort)
			modelRouter = entry.ModelRouter
			parallelTools = entry.ParallelToolCalls
			if maxOutput <= 0 && entry.MaxOutputTokens > 0 {
				maxOutput = entry.MaxOutputTokens
			}
		}
		if maxOutput <= 0 {
			maxOutput = 64_000
		}

		// Stable cascade_id keyed by PromptCacheKey when present.
		cascadeID := devinRandomUUID()
		if strings.TrimSpace(req.PromptCacheKey) != "" {
			cascadeID = devinDeterministicUUID("cascade\x00" + strings.TrimSpace(req.PromptCacheKey))
		}

		replayThinking := !modelRouter && thinking != "disabled"
		prompts, historyBytes, routerPrompt := buildDevinChatPrompts(cascadeID, req.Messages, replayThinking)

		// Router models resolve the actual chat_model_uid via AssignModel.
		var assignmentJWT string
		if modelRouter {
			memoKey := devinAssignMemoKey(userJWT, cascadeID, chatModelUID, routerPrompt)
			if uid, jwt, ok := p.getMemoAssignment(memoKey); ok {
				chatModelUID, assignmentJWT = uid, jwt
			} else {
				uid, jwt, aerr := p.assignModel(ctx, baseURL, apiKey, userJWT, chatModelUID, cascadeID, routerPrompt)
				if aerr != nil {
					var hse *HTTPStatusError
					if errors.As(aerr, &hse) && (hse.StatusCode == http.StatusUnauthorized || hse.StatusCode == http.StatusForbidden) {
						invalidateCachedDevinJWT(p.endpoint, apiKey)
						p.clearAssignments()
						userJWT, baseURL, aerr = p.resolveUserJWT(ctx, apiKey, true)
						if aerr == nil {
							uid, jwt, aerr = p.assignModel(ctx, baseURL, apiKey, userJWT, chatModelUID, cascadeID, routerPrompt)
						}
					}
					if aerr != nil {
						yield(StreamEvent{}, aerr)
						return
					}
				}
				memoKey = devinAssignMemoKey(userJWT, cascadeID, chatModelUID, routerPrompt)
				p.putMemoAssignment(memoKey, uid, jwt)
				chatModelUID, assignmentJWT = uid, jwt
			}
		}

		// Encode GetChatMessageRequest body wrapped in Connect envelope.
		payload := make([]byte, 0, 4096)
		payload = protoAppendLenField(payload, 1, encodeDevinCLIMetadata(apiKey, userJWT))
		payload = protoAppendStringField(payload, 21, chatModelUID)
		if assignmentJWT != "" {
			payload = protoAppendStringField(payload, 26, assignmentJWT)
		}
		systemPrompt := joinSystem(req.System)
		payload = protoAppendStringField(payload, 2, systemPrompt)
		for _, part := range prompts {
			payload = protoAppendLenField(payload, 3, encodeDevinChatMessagePrompt(part))
		}
		payload = protoAppendVarintField(payload, 7, devinRequestTypeCascade)
		payload = protoAppendLenField(payload, 8, encodeDevinCompletionConfiguration(uint64(maxOutput)))
		payload = protoAppendBoolField(payload, 11, !parallelTools)
		payload = protoAppendLenField(payload, 12, encodeDevinToolChoiceAuto())
		if p.cfg.promptCacheEnabled() {
			payload = protoAppendLenField(payload, 13, encodeDevinPromptCacheOptions())
		}
		payload = protoAppendStringField(payload, 16, cascadeID)
		payload = protoAppendVarintField(payload, 20, devinPlannerModeDefault)
		payload = protoAppendStringField(payload, 22, devinRandomUUID())
		payload = append(payload, encodeDevinTools(req.Tools, modelID, chatModelUID)...)

		framed := frameConnectMessage(payload)

		// Establish streaming request with retries on transient pre-stream errors.
		streamURL := strings.TrimRight(baseURL, "/") + devinChatMessagePath
		resp, err := p.doChatStream(ctx, streamURL, framed, apiKey)
		if err != nil {
			var hse *HTTPStatusError
			if errors.As(err, &hse) {
				p.clearAssignments()
				if hse.StatusCode == http.StatusUnauthorized || hse.StatusCode == http.StatusForbidden {
					invalidateCachedDevinJWT(p.endpoint, apiKey)
				}
			}
			yield(StreamEvent{}, err)
			return
		}
		defer resp.Body.Close()

		p.consumeConnectStream(ctx, resp.Body, historyBytes, yield)
	}
}

func (p *devinProvider) doChatStream(ctx context.Context, url string, body []byte, apiKey string) (*http.Response, error) {
	retries := p.cfg.retries()
	if n, ok := maxRetriesFromContext(ctx); ok {
		retries = max(n, 0)
	}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("content-type", "application/connect+proto")
		req.Header.Set("connect-protocol-version", "1")
		req.Header.Set("accept-encoding", "identity")
		req.Header.Set("user-agent", "connect-go/1.18.1 (go1.26.3)")
		for k, v := range p.cfg.ExtraHeaders {
			if k != "" && v != "" {
				req.Header.Set(k, v)
			}
		}

		resp, err := p.httpClient().Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			return resp, nil
		}
		retryable := false
		var retryAfter time.Duration
		switch {
		case err != nil:
			retryable = ctx.Err() == nil
		case isRetryableStatus(resp.StatusCode):
			retryable = true
			retryAfter = parseRetryAfter(resp.Header)
		}
		if attempt >= retries || !retryable {
			if err != nil {
				return nil, err
			}
			data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			_ = resp.Body.Close()
			if retryAfter == 0 {
				retryAfter = parseRetryAfter(resp.Header)
			}
			return nil, &HTTPStatusError{
				Prefix:     "devin",
				StatusCode: resp.StatusCode,
				Body:       devinHTTPErrorDetail(data),
				RetryAfter: retryAfter,
			}
		}
		if resp != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
			_ = resp.Body.Close()
		}
		wait := p.cfg.retryDelay(attempt)
		if p.cfg.RetryInterval == 0 {
			wait = jitterDuration(wait)
		}
		if retryAfter > 0 && retryAfter <= 15*time.Second && retryAfter > wait {
			wait = retryAfter
		}
		if !backoffSleep(ctx, wait) {
			return nil, ctx.Err()
		}
	}
}

func (p *devinProvider) consumeConnectStream(
	ctx context.Context,
	body io.ReadCloser,
	historyBytes int,
	yield func(StreamEvent, error) bool,
) {
	idle := p.cfg.streamIdleTimeout()
	var (
		timer     *time.Timer
		timedOut  bool
		timeoutMu sync.Mutex
	)
	if idle > 0 {
		timer = time.AfterFunc(idle, func() {
			timeoutMu.Lock()
			timedOut = true
			timeoutMu.Unlock()
			_ = body.Close()
		})
		defer timer.Stop()
	}

	var (
		started        bool
		sawOutput      bool
		stopReason     uint64
		signature      string
		toolOrder      []string
		toolIndices    = map[string]int{}
		toolNames      = map[string]string{}
		toolArgs       = map[string]string{}
		lastToolID     string
		usage          Usage
		hasUsage       bool
	)

	emit := func(ev StreamEvent) bool {
		if !started {
			started = true
			if !yield(StreamEvent{Type: SEMessageStart}, nil) {
				return false
			}
		}
		return yield(ev, nil)
	}

	for {
		flags, payload, err := readConnectFrame(body)
		if timer != nil {
			timer.Reset(idle)
		}
		if err != nil {
			timeoutMu.Lock()
			wasTimeout := timedOut
			timeoutMu.Unlock()
			if wasTimeout {
				yield(StreamEvent{}, &HTTPStatusError{
					Prefix:     "devin",
					StatusCode: http.StatusGatewayTimeout,
					Body:       fmt.Sprintf("stream idle timeout after %s", idle),
				})
				return
			}
			if errors.Is(err, io.EOF) {
				if !sawOutput && !hasUsage {
					yield(StreamEvent{}, fmt.Errorf("devin: stream ended unexpectedly before trailer"))
					return
				}
				break
			}
			if ctx.Err() != nil {
				yield(StreamEvent{}, ctx.Err())
				return
			}
			yield(StreamEvent{}, err)
			return
		}

		// Trailer / EndStream frame.
		if flags&connectFlagEndStream != 0 {
			if terr := parseDevinTrailerError(payload, sawOutput, historyBytes); terr != nil {
				p.clearAssignments()
				yield(StreamEvent{}, terr)
				return
			}
			break
		}

		delta := decodeDevinChatDelta(payload)
		if delta.deltaThinking != "" {
			sawOutput = true
			if !emit(StreamEvent{Type: SEThinkingDelta, Text: delta.deltaThinking}) {
				return
			}
		}
		if delta.deltaSignature != "" {
			var sigDelta string
			if strings.HasPrefix(delta.deltaSignature, signature) {
				sigDelta = delta.deltaSignature[len(signature):]
				signature = delta.deltaSignature
			} else {
				sigDelta = delta.deltaSignature
				signature += delta.deltaSignature
			}
			if sigDelta != "" {
				if !emit(StreamEvent{Type: SEThinkingSignature, Text: sigDelta}) {
					return
				}
			}
		}
		if delta.deltaText != "" {
			sawOutput = true
			if !emit(StreamEvent{Type: SETextDelta, Text: delta.deltaText}) {
				return
			}
		}
		for _, tc := range delta.toolCalls {
			callID := tc.id
			if callID != "" {
				lastToolID = callID
			} else if lastToolID != "" {
				callID = lastToolID
			} else {
				continue
			}
			sawOutput = true
			first := false
			toolIdx, exists := toolIndices[callID]
			if !exists {
				first = true
				toolIdx = len(toolOrder)
				toolOrder = append(toolOrder, callID)
				toolIndices[callID] = toolIdx
				toolNames[callID] = tc.name
			} else if tc.name != "" {
				toolNames[callID] = tc.name
			}
			if first {
				if !emit(StreamEvent{Type: SEToolUseStart, ToolID: callID, ToolName: toolNames[callID], ToolIndex: toolIdx}) {
					return
				}
			}
			prev := toolArgs[callID]
			var argDelta string
			if len(prev) >= 2 && len(tc.args) > len(prev) && strings.HasPrefix(tc.args, prev) {
				argDelta = tc.args[len(prev):]
				toolArgs[callID] = tc.args
			} else if len(prev) >= 2 && tc.args == prev {
				argDelta = ""
			} else {
				toolArgs[callID] = prev + tc.args
				argDelta = tc.args
			}
			if argDelta != "" {
				if !emit(StreamEvent{Type: SEToolInputJSON, Text: argDelta, ToolIndex: toolIdx}) {
					return
				}
			}
		}
		if delta.stopReason != 0 {
			stopReason = delta.stopReason
		}
		if delta.hasUsage {
			hasUsage = true
			usage = Usage{
				InputTokens:      int(delta.inputTokens),
				OutputTokens:     int(delta.outputTokens),
				CacheWriteTokens: int(delta.cacheWriteTokens),
				CacheReadTokens:  int(delta.cacheReadTokens),
			}
		}
	}

	for _, callID := range toolOrder {
		toolIdx := toolIndices[callID]
		if toolArgs[callID] == "" {
			if !emit(StreamEvent{Type: SEToolInputJSON, Text: "{}", ToolIndex: toolIdx}) {
				return
			}
		}
		if !emit(StreamEvent{Type: SEToolUseStop, ToolIndex: toolIdx}) {
			return
		}
	}

	reason := "end_turn"
	if len(toolOrder) > 0 {
		reason = "tool_use"
	} else if stopReason == devinStopReasonMaxTokens {
		reason = "max_tokens"
	}
	if !emit(StreamEvent{Type: SEMessageDelta, StopReason: reason, Usage: usage}) {
		return
	}
	yield(StreamEvent{Type: SEMessageStop}, nil)
}

func buildDevinChatPrompts(cascadeID string, messages []Message, replayThinking bool) ([]devinChatPromptParts, int, []byte) {
	prompts := make([]devinChatPromptParts, 0, len(messages))
	var routerPrompt []byte
	for idx, msg := range messages {
		switch msg.Role {
		case RoleUser:
			var textParts []string
			var toolResults []ContentBlock
			for _, b := range msg.Content {
				switch b.Type {
				case BlockText:
					if b.Text != "" {
						textParts = append(textParts, b.Text)
					}
				case BlockToolResult:
					toolResults = append(toolResults, b)
				}
			}
			for _, tr := range toolResults {
				tid := devinWireToolCallID(tr.ToolUseID)
				var trText strings.Builder
				for _, tb := range tr.Content {
					if tb.Type == BlockText {
						trText.WriteString(tb.Text)
					}
				}
				prompts = append(prompts, devinChatPromptParts{
					messageID:         devinDeterministicUUID(fmt.Sprintf("%s\x00%d\x00tool\x00%s", cascadeID, idx, tid)),
					source:            devinChatSourceTool,
					prompt:            trText.String(),
					toolCallID:        tid,
					toolResultIsError: tr.IsError,
				})
			}
			if len(textParts) > 0 || len(toolResults) == 0 {
				text := strings.Join(textParts, "\n")
				part := devinChatPromptParts{
					messageID: devinDeterministicUUID(fmt.Sprintf("%s\x00%d\x00user", cascadeID, idx)),
					source:    devinChatSourceUser,
					prompt:    text,
				}
				prompts = append(prompts, part)
				routerPrompt = encodeDevinChatMessagePrompt(devinChatPromptParts{
					source: devinChatSourceUser,
					prompt: text,
				})
			}
		case RoleAssistant:
			var text strings.Builder
			var thinking strings.Builder
			var sig string
			var toolCalls [][]byte
			for _, b := range msg.Content {
				switch b.Type {
				case BlockText:
					text.WriteString(b.Text)
				case BlockThinking:
					if replayThinking {
						thinking.WriteString(b.Thinking)
						if b.Signature != "" {
							sig = b.Signature
						}
					}
				case BlockToolUse:
					args := strings.TrimSpace(string(b.Input))
					if args == "" {
						args = "{}"
					}
					toolCalls = append(toolCalls, encodeDevinToolCall(devinWireToolCallID(b.ID), b.Name, args))
				}
			}
			if text.Len() == 0 && thinking.Len() == 0 && sig == "" && len(toolCalls) == 0 {
				continue
			}
			prompts = append(prompts, devinChatPromptParts{
				messageID: devinDeterministicUUID(fmt.Sprintf("%s\x00%d\x00assistant", cascadeID, idx)),
				source:    devinChatSourceSystem,
				prompt:    text.String(),
				thinking:  thinking.String(),
				signature: sig,
				toolCalls: toolCalls,
			})
		}
	}

	// Compute compactable history bytes (everything before the trailing user prompt).
	historyBytes := 0
	limit := len(prompts)
	if limit > 0 && prompts[limit-1].source == devinChatSourceUser {
		limit--
	}
	for i := 0; i < limit; i++ {
		historyBytes += len(encodeDevinChatMessagePrompt(prompts[i]))
	}
	return prompts, historyBytes, routerPrompt
}

func encodeDevinTools(tools []ToolSchema, modelID, chatModelUID string) []byte {
	if len(tools) == 0 {
		return nil
	}
	gemini := isDevinGeminiUID(modelID) || isDevinGeminiUID(chatModelUID)
	claude := isDevinClaudeUID(modelID) || isDevinClaudeUID(chatModelUID)
	out := make([]byte, 0, len(tools)*256)
	for _, t := range tools {
		if t.Name == "" {
			continue
		}
		schema := t.InputSchema
		if schema == nil {
			schema = map[string]any{"type": "object"}
		}
		if gemini {
			if m, ok := scalarizeSchemaTypes(schema).(map[string]any); ok {
				schema = m
			}
		}
		if claude {
			schema = stripTopLevelCombinators(schema)
		}
		out = protoAppendLenField(out, 10, encodeDevinToolDefinition(t.Name, t.Description, schema, false))
	}
	return out
}

func devinHTTPErrorDetail(payload []byte) string {
	const maxChars = 512
	s := strings.TrimSpace(string(payload))
	if s == "" {
		return "empty error body"
	}
	if strings.HasPrefix(s, "<") {
		return "proxy returned an HTML error page"
	}
	var doc map[string]any
	if err := json.Unmarshal(payload, &doc); err == nil {
		if errObj, ok := doc["error"].(map[string]any); ok {
			if msg, ok := errObj["message"].(string); ok && msg != "" {
				return truncateRunes(msg, maxChars)
			}
		}
		if msg, ok := doc["message"].(string); ok && msg != "" {
			return truncateRunes(msg, maxChars)
		}
	}
	return truncateRunes(s, maxChars)
}

func truncateRunes(s string, maxChars int) string {
	r := []rune(s)
	if len(r) <= maxChars {
		return s
	}
	return string(r[:maxChars])
}

func parseDevinTrailerError(payload []byte, sawOutput bool, historyBytes int) error {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return nil
	}
	var doc struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(trimmed, &doc); err != nil || doc.Error == nil {
		return nil
	}
	code := strings.ToLower(strings.TrimSpace(doc.Error.Code))
	msg := strings.TrimSpace(doc.Error.Message)
	if msg == "" {
		msg = "provider rejected the stream"
	}
	if code == "invalid_argument" && strings.Contains(strings.ToLower(msg), "internal error") && !sawOutput && historyBytes >= devinLargeHistoryRecoveryBytes {
		return &HTTPStatusError{
			Prefix:     "devin",
			StatusCode: http.StatusBadRequest,
			Body:       fmt.Sprintf("context_length_exceeded (%s): %s", code, msg),
		}
	}
	status := http.StatusBadGateway
	switch code {
	case "unauthenticated":
		status = http.StatusUnauthorized
	case "permission_denied":
		status = http.StatusForbidden
	case "invalid_argument", "failed_precondition", "out_of_range":
		status = http.StatusBadRequest
	case "not_found":
		status = http.StatusNotFound
	case "resource_exhausted":
		status = http.StatusTooManyRequests
	case "deadline_exceeded":
		status = http.StatusGatewayTimeout
	case "unavailable":
		status = http.StatusServiceUnavailable
	case "internal", "unknown":
		status = http.StatusInternalServerError
	}
	return &HTTPStatusError{
		Prefix:     "devin",
		StatusCode: status,
		Body:       fmt.Sprintf("%s: %s", code, msg),
	}
}
