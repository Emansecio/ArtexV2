package llmpool

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/Autumn-27/norma/llm"
)

// Member is one LLM profile in the chain, already built into a provider (wrapped
// with the recorder by the caller, so a failed attempt is still recorded under
// its own profile name).
type Member struct {
	ID       int64  // llm_profiles.id
	Name     string // profile name, for logs / UI
	Model    string
	Format   string // "anthropic" | "openai"
	Priority int    // the profile's configured priority (display only)
	Active   bool   // is_default (display only)
	// Rank is the ordering key the caller assigned: higher goes first, and members
	// sharing a Rank take turns leading (load-spreading across duplicate keys).
	// The caller encodes "active profile heads the chain" as a Rank above every
	// user-settable priority, so this type needs no policy of its own.
	Rank int
	// WindowTokens is the profile's context window in tokens. A member whose
	// window can't hold the request is skipped rather than made to fail on it.
	WindowTokens int
	// Thinking reports whether this profile defaults to thinking/reasoning mode
	// (so speculative TTFT hedging avoids double-billing reasoning latency).
	Thinking bool
	Prov     llm.Provider
}

// RankActive is the Rank the caller gives the chain head (the active profile, or
// an explicitly bound one) so it always outranks any configured priority.
const RankActive = int(^uint(0)>>1) - 1

// ErrExhausted is returned when every member of the chain failed.
var ErrExhausted = errors.New("LLM failover: all profiles unavailable")

// Pool is an llm.Provider that fails over across an ordered chain of members.
// It is safe for concurrent use: members are immutable after construction and
// all mutable state lives in the shared Registry.
type Pool struct {
	members    []*Member // in chain order (active first, then priority DESC)
	health     *Registry
	rr         atomic.Uint64 // rotates the starting point within an equal-priority group
	hedgeDelay time.Duration // optional speculative hedge threshold (ARTEX_LLM_HEDGE_MS)
}

// New builds a Pool over members (already in chain order). Returns nil when the
// chain is empty. A single-member chain is still a valid Pool — it just behaves
// exactly like the bare provider.
func New(members []*Member, health *Registry) *Pool {
	if len(members) == 0 {
		return nil
	}
	if health == nil {
		health = NewRegistry(nil, nil)
	}
	return &Pool{
		members:    members,
		health:     health,
		hedgeDelay: hedgeDelayFromEnv(),
	}
}

// SetHedgeDelay configures speculative hedging between the top two members in
// Pool.Stream (0 disables). Only starts member 1 if member 0 yields no event
// within d, and cancels the slower stream before emitting to the caller.
func (p *Pool) SetHedgeDelay(d time.Duration) { p.hedgeDelay = d }

func hedgeDelayFromEnv() time.Duration {
	raw := strings.TrimSpace(os.Getenv("ARTEX_LLM_HEDGE_MS"))
	if raw == "" {
		return 0
	}
	ms, err := strconv.Atoi(raw)
	if err != nil || ms <= 0 {
		return 0
	}
	return time.Duration(ms) * time.Millisecond
}

// Members returns the chain in order (read-only).
func (p *Pool) Members() []*Member { return p.members }

// Head returns the first member of the chain.
func (p *Pool) Head() *Member { return p.members[0] }

// Stream implements llm.Provider with failover.
//
// The one hard rule: a member may only be abandoned BEFORE it has yielded any
// event. Once text or a tool_use has reached the caller, re-sending the same
// request to another model would duplicate output and corrupt the conversation
// history — so a mid-stream failure is surfaced as-is and left to the agent
// harness's resume logic. Fortunately the failures this exists for (402 no
// credit, 401 bad key, 429, 5xx) all surface during request establishment,
// before the body is read, so they always land in the safe window.
func (p *Pool) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	order, releaseProbes := p.order(req)
	return func(yield func(llm.StreamEvent, error) bool) {
		defer releaseProbes()
		var lastErr error
		startIdx := 0
		thinking := req.Thinking == "enabled" || (req.Thinking == "" && len(order) > 0 && (order[0].Thinking || (req.ReasoningEffort != "" && req.ReasoningEffort != "none")))
		if p.hedgeDelay > 0 && !thinking && len(order) >= 2 {
			done, err := p.streamHedgedFirstTwo(ctx, req, order[0], order[1], len(order) > 2, yield)
			if done {
				return
			}
			if err != nil {
				lastErr = err
			}
			startIdx = 2
		}
		for i := startIdx; i < len(order); i++ {
			m := order[i]
			hasNext := i+1 < len(order)
			attemptCtx := ctx
			if hasNext {
				attemptCtx = llm.WithMaxRetries(ctx, 0)
			}
			startAt := time.Now()
			firstTTFT := false
			emitted := false
			var failed error
			for ev, err := range m.Prov.Stream(attemptCtx, req) {
				if err != nil && !emitted && shouldFailover(ctx, err) {
					failed = err
					break // safe window: nothing reached the caller yet
				}
				if !firstTTFT && err == nil {
					firstTTFT = true
					p.health.RecordTTFT(m.ID, time.Since(startAt))
				}
				emitted = true
				if !yield(ev, err) {
					return // caller stopped consuming (cancel / early exit)
				}
				if err != nil {
					return // terminal error already handed to the caller
				}
			}
			if failed == nil {
				p.health.RecordCall(ctx, m.ID, true, false, nil) // completed: Pass
				return
			}
			lastErr = failed
			if tripped, msg := p.health.RecordCall(ctx, m.ID, true, false, failed); tripped {
				log.Printf("[llmpool] profile %q(%s) breaker tripped: %s", m.Name, m.Model, msg)
			}
			if hasNext {
				n := order[i+1]
				log.Printf("[llmpool] LLM failover: %q(%s) → %q(%s), reason: %s",
					m.Name, m.Model, n.Name, n.Model, trimErr(failed))
			}
		}
		if lastErr == nil {
			lastErr = ErrExhausted
		}
		log.Printf("[llmpool] failover chain exhausted (all %d profiles failed), last error: %s", len(order), trimErr(lastErr))
		yield(llm.StreamEvent{}, fmt.Errorf("%w: %v", ErrExhausted, lastErr))
	}
}

type hedgeResult struct {
	ev  llm.StreamEvent
	err error
}

// streamHedgedFirstTwo races m0 against a delayed speculative m1 when m0 has
// yielded no event within p.hedgeDelay. Whichever yields its first non-error
// event wins; the other is cancelled before anything reaches yield.
// Returns (true, nil) if the stream finished or handed off to a winner;
// returns (false, lastErr) only if both failed inside the safe pre-emit window.
func (p *Pool) streamHedgedFirstTwo(
	ctx context.Context,
	req llm.CompletionRequest,
	m0, m1 *Member,
	hasMore bool,
	yield func(llm.StreamEvent, error) bool,
) (bool, error) {
	runMember := func(mctx context.Context, m *Member, out chan<- hedgeResult) {
		defer close(out)
		for ev, err := range m.Prov.Stream(mctx, req) {
			select {
			case out <- hedgeResult{ev: ev, err: err}:
			case <-mctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}

	ctx0, cancel0 := context.WithCancel(llm.WithMaxRetries(ctx, 0))
	defer cancel0()
	ch0 := make(chan hedgeResult, 16)
	start0 := time.Now()
	go runMember(ctx0, m0, ch0)

	var ctx1 context.Context
	var cancel1 context.CancelFunc
	var ch1 chan hedgeResult
	var start1 time.Time
	var m1Started, m0Dead, m1Dead bool
	var lastErr error

	drainWinner := func(m *Member, start time.Time, first hedgeResult, ch <-chan hedgeResult, cancelLoser context.CancelFunc) (bool, error) {
		if cancelLoser != nil {
			cancelLoser()
		}
		p.health.RecordTTFT(m.ID, time.Since(start))
		if !yield(first.ev, nil) {
			return true, nil
		}
		for res := range ch {
			if !yield(res.ev, res.err) {
				return true, nil
			}
			if res.err != nil {
				return true, nil
			}
		}
		p.health.RecordCall(ctx, m.ID, true, false, nil)
		return true, nil
	}

	failMember := func(m *Member, err error) {
		lastErr = err
		if tripped, msg := p.health.RecordCall(ctx, m.ID, true, false, err); tripped {
			log.Printf("[llmpool] profile %q(%s) breaker tripped: %s", m.Name, m.Model, msg)
		}
	}

	// Fast path: wait up to p.hedgeDelay for m0 before hedging m1.
	timer := time.NewTimer(p.hedgeDelay)
	select {
	case <-ctx.Done():
		timer.Stop()
		return true, nil
	case res, ok := <-ch0:
		timer.Stop()
		if !ok {
			res.err = ErrExhausted
		}
		if res.err == nil {
			return drainWinner(m0, start0, res, ch0, nil)
		}
		if !shouldFailover(ctx, res.err) {
			yield(llm.StreamEvent{}, res.err)
			return true, nil
		}
		failMember(m0, res.err)
		m0Dead = true
	case <-timer.C:
	}

	// Launch speculative backup m1 immediately (either m0 timed out on TTFT or m0 failed fast).
	if m1 != nil {
		bctx := ctx
		if hasMore || !m0Dead {
			bctx = llm.WithMaxRetries(ctx, 0)
		}
		ctx1, cancel1 = context.WithCancel(bctx)
		defer cancel1()
		ch1 = make(chan hedgeResult, 16)
		start1 = time.Now()
		m1Started = true
		go runMember(ctx1, m1, ch1)
		if !m0Dead {
			log.Printf("[llmpool] speculative hedge: racing %q(%s) alongside slow %q(%s) after %v",
				m1.Name, m1.Model, m0.Name, m0.Model, p.hedgeDelay)
		} else {
			log.Printf("[llmpool] LLM failover: %q(%s) → %q(%s), reason: %s",
				m0.Name, m0.Model, m1.Name, m1.Model, trimErr(lastErr))
		}
	}

	for !(m0Dead && (!m1Started || m1Dead)) {
		var c0 <-chan hedgeResult
		var c1 <-chan hedgeResult
		if !m0Dead {
			c0 = ch0
		}
		if m1Started && !m1Dead {
			c1 = ch1
		}
		select {
		case <-ctx.Done():
			return true, nil
		case res, ok := <-c0:
			if !ok {
				res.err = ErrExhausted
			}
			if res.err == nil {
				return drainWinner(m0, start0, res, ch0, cancel1)
			}
			if !shouldFailover(ctx, res.err) {
				if cancel1 != nil {
					cancel1()
				}
				yield(llm.StreamEvent{}, res.err)
				return true, nil
			}
			failMember(m0, res.err)
			m0Dead = true
		case res, ok := <-c1:
			if !ok {
				res.err = ErrExhausted
			}
			if res.err == nil {
				return drainWinner(m1, start1, res, ch1, cancel0)
			}
			if !shouldFailover(ctx, res.err) {
				cancel0()
				yield(llm.StreamEvent{}, res.err)
				return true, nil
			}
			failMember(m1, res.err)
			m1Dead = true
		}
	}
	return false, lastErr
}

// Complete implements llm.Provider with failover for non-streaming calls. A
// non-streaming request is atomic — it never delivers partial output — so every
// failover-eligible failure lands in the safe window and the next member can be
// tried without risk of duplicated output. Mirrors Stream's health-tripping and
// chain-exhaustion behavior.
func (p *Pool) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	order, releaseProbes := p.order(req)
	defer releaseProbes()
	var lastErr error
	for i, m := range order {
		hasNext := i+1 < len(order)
		attemptCtx := ctx
		if hasNext {
			attemptCtx = llm.WithMaxRetries(ctx, 0)
		}
		startAt := time.Now()
		msg, sr, usage, err := m.Prov.Complete(attemptCtx, req)
		if err == nil {
			p.health.RecordTTFT(m.ID, time.Since(startAt))
			p.health.RecordCall(ctx, m.ID, false, false, nil) // Pass
			return msg, sr, usage, nil
		}
		if !shouldFailover(ctx, err) {
			// Non-failover error (e.g. ctx cancel, deterministic 4xx): surface as-is
			// without tripping health. RecordCall clears the state on an atomic
			// call, matching the historical Pass here.
			p.health.RecordCall(ctx, m.ID, false, false, err)
			return llm.Message{}, "", llm.Usage{}, err
		}
		lastErr = err
		if tripped, msg := p.health.RecordCall(ctx, m.ID, false, false, err); tripped {
			log.Printf("[llmpool] profile %q(%s) breaker tripped: %s", m.Name, m.Model, msg)
		}
		if hasNext {
			n := order[i+1]
			log.Printf("[llmpool] LLM failover: %q(%s) → %q(%s), reason: %s",
				m.Name, m.Model, n.Name, n.Model, trimErr(err))
		}
	}
	if lastErr == nil {
		lastErr = ErrExhausted
	}
	log.Printf("[llmpool] failover chain exhausted (all %d profiles failed), last error: %s", len(order), trimErr(lastErr))
	return llm.Message{}, "", llm.Usage{}, fmt.Errorf("%w: %v", ErrExhausted, lastErr)
}

// order picks the members to try, in order: skip those in a cooling-off window
// (or already undergoing a single-flight half-open probe) and those whose context
// window can't hold this request, then rotate within each equal-priority group
// while preferring dramatically faster (>500ms TTFT) profiles.
func (p *Pool) order(req llm.CompletionRequest) ([]*Member, func()) {
	est := estimateTokens(req)
	var open []*Member
	var probes []int64
	for _, m := range p.members {
		usable, isProbe := p.health.AcquireUsable(m.ID)
		if !usable {
			continue
		}
		if m.WindowTokens > 0 && est > m.WindowTokens {
			if isProbe {
				p.health.ReleaseProbe(m.ID)
			}
			continue // would 400 on length — not a useful failover target
		}
		if isProbe {
			probes = append(probes, m.ID)
		}
		open = append(open, m)
	}
	release := func() {
		for _, id := range probes {
			p.health.ReleaseProbe(id)
		}
	}
	if len(open) == 0 {
		return p.members[:1], release // last resort: probe the head rather than stall
	}
	if req.PromptCacheKey != "" {
		return orderGroups(open, req.PromptCacheKey, 0, p.health), release
	}
	return orderGroups(open, "", p.rr.Add(1)-1, p.health), release
}

// orderGroups orders each run of equal-ranked members. When cacheKey is set it
// pins the head deterministically via rendezvous hashing so repeated turns hit
// provider prompt caches instead of ping-ponging. Otherwise it rotates round-robin
// by n and promotes clearly faster (>500ms lower EWMA TTFT) members.
func orderGroups(in []*Member, cacheKey string, n uint64, health *Registry) []*Member {
	sticky := cacheKey != ""
	var keyHash uint64
	if sticky {
		keyHash = fnv1aString(cacheKey)
	}
	out := make([]*Member, 0, len(in))
	for i := 0; i < len(in); {
		j := i + 1
		for j < len(in) && in[j].Rank == in[i].Rank {
			j++
		}
		g := in[i:j]
		if len(g) > 1 {
			rotated := make([]*Member, len(g))
			var off int
			if sticky {
				off = stickyStartOffset(g, keyHash)
			} else {
				off = int(n % uint64(len(g)))
			}
			for k := range g {
				rotated[k] = g[(k+off)%len(g)]
			}
			if !sticky && health != nil {
				bestIdx := -1
				var bestTTFT time.Duration
				allMeasured := true
				for k, m := range rotated {
					d := health.TTFT(m.ID)
					if d <= 0 {
						allMeasured = false
						break
					}
					if bestIdx == -1 || d < bestTTFT {
						bestTTFT = d
						bestIdx = k
					}
				}
				if allMeasured && bestIdx > 0 {
					headTTFT := health.TTFT(rotated[0].ID)
					if headTTFT-bestTTFT > 500*time.Millisecond {
						rotated[0], rotated[bestIdx] = rotated[bestIdx], rotated[0]
					}
				}
			}
			out = append(out, rotated...)
		} else {
			out = append(out, g...)
		}
		i = j
	}
	return out
}

func fnv1aString(s string) uint64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	var h uint64 = offset64
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime64
	}
	return h
}

func stickyStartOffset(g []*Member, keyHash uint64) int {
	bestIdx := 0
	var bestScore uint64
	for k, m := range g {
		score := keyHash ^ (uint64(m.ID)*0x9e3779b97f4a7c15 + 0x517cc1b727220a95)
		score ^= score >> 30
		score *= 0xbf58476d1ce4e5b9
		score ^= score >> 27
		if k == 0 || score > bestScore {
			bestScore = score
			bestIdx = k
		}
	}
	return bestIdx
}

// statusRe pulls the HTTP status out of the SDK's error text, which is formatted
// as "<prefix>: status <code>: <body>" (norma/llm/retry.go). The SDK exposes no
// typed error, so the string is what we have.
var statusRe = regexp.MustCompile(`status (\d{3})`)

// statusOf returns the HTTP status carried by err, or 0 if it isn't one.
func statusOf(err error) int {
	m := statusRe.FindStringSubmatch(err.Error())
	if m == nil {
		return 0
	}
	code, _ := strconv.Atoi(m[1])
	return code
}

// shouldFailover reports whether err justifies trying the next profile.
//
// Never fails over on context cancellation — that's the user stopping a task or a
// task-level timeout, and burning a backup key on it would both waste credit and
// pollute the run's termination diagnosis. Never on 400 either: a malformed or
// over-long request fails identically everywhere.
func shouldFailover(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	switch code := statusOf(err); {
	case code == 0:
		return true // no status → transport-level failure (reset / DNS / timeout)
	case code == 400:
		return false // bad or over-long request: identical everywhere
	case code == 401, code == 402, code == 403, code == 404, code == 408, code == 429:
		return true
	case code >= 500:
		return true
	}
	return false
}

// isHardFailure reports whether the failure is deterministic (the profile will
// keep failing until a human fixes it) rather than transient. Hard failures open
// the breaker on the first occurrence.
func isHardFailure(err error) bool {
	switch statusOf(err) {
	case 401, 402, 403, 404:
		return true
	}
	return false
}

// trimErr shortens an error for logs/UI — provider bodies can be long.
func trimErr(err error) string {
	s := strings.TrimSpace(strings.ReplaceAll(err.Error(), "\n", " "))
	if len(s) > 300 {
		cut := 300
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "…"
	}
	return s
}

// estimateTokens roughly sizes a request so members whose context window clearly
// can't hold it are skipped. Deliberately crude (~3.5 chars/token) and biased to
// over-estimate slightly; it only needs to tell "fits" from "nowhere near".
func estimateTokens(req llm.CompletionRequest) int {
	n := 0
	for _, s := range req.System {
		n += len(s)
	}
	for _, m := range req.Messages {
		n += blocksLen(m.Content)
	}
	for _, t := range req.Tools {
		// InputSchema is a decoded map, so its serialized size isn't available
		// cheaply — charge a flat ~120 chars per top-level property instead.
		n += len(t.Name) + len(t.Description) + len(t.InputSchema)*120
	}
	return n * 2 / 7 // ≈ len/3.5
}

func blocksLen(bs []llm.ContentBlock) int {
	n := 0
	for _, b := range bs {
		n += len(b.Text) + len(b.Thinking) + len(b.Input) + len(b.Name)
		if len(b.Content) > 0 {
			n += blocksLen(b.Content)
		}
	}
	return n
}
