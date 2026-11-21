// Package transcript persists a conversation to an append-only JSONL log on
// disk and reconstructs it for resume. The main session is written to
// <dir>/<sessionID>.jsonl; each
// subagent is written to its own sidechain file under
// <dir>/<sessionID>/subagents/agent-<agentID>.jsonl, so subagent tokens are
// counted and stored separately (aggregate them offline if needed).
//
// The log is the full-fidelity history: every user/assistant/tool message is
// recorded as it is born. When compaction inserts a boundary, the post-boundary
// working set — the boundary marker, the summary, and the kept recent tail — is
// ALSO persisted as messages, so a cold-start resume restores the COMPACTED view
// (via llm.MessagesForAPI, which slices from the last boundary) instead of
// replaying raw history and re-summarizing. The kept tail
// is re-recorded after the boundary (it also exists earlier as births); only the
// post-boundary copy reaches the model. Lightweight working-set edits that do not
// add a boundary (MicroCompact tool-result clearing) stay ephemeral and
// are not persisted. A separate Type=="boundary" record is written for
// observability and is skipped during reconstruction.
package transcript

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Autumn-27/norma/llm"
)

// Record is one line of a JSONL transcript.
type Record struct {
	UUID        string            `json:"uuid"`
	ParentUUID  string            `json:"parent_uuid,omitempty"`
	SessionID   string            `json:"session_id"`
	AgentID     string            `json:"agent_id,omitempty"`
	IsSidechain bool              `json:"is_sidechain,omitempty"`
	Timestamp   string            `json:"timestamp"`
	Type        string            `json:"type"` // "message" | "boundary"
	Message     *llm.Message      `json:"message,omitempty"`
	Usage       *llm.Usage        `json:"usage,omitempty"`    // assistant turns only
	Boundary    *llm.BoundaryMeta `json:"boundary,omitempty"` // Type=="boundary"
}

type txWriteReq struct {
	path     string
	data     []byte
	flushCh  chan error
	closeAll bool
}

type openTranscript struct {
	f        *os.File
	w        *bufio.Writer
	lastUsed time.Time
	dirty    bool
}

// Store is a directory of session transcripts. Appends are batched through an
// async buffered writer keeping open file handles per session path, flushed on
// Load, boundary markers, Flush, Close, or periodic ticks.
type Store struct {
	dir string

	initOnce sync.Once
	started  atomic.Bool
	closed   atomic.Bool
	reqCh    chan txWriteReq
	stopCh   chan struct{}
	doneCh   chan struct{}
	stopOnce sync.Once

	errMu   sync.Mutex
	lastErr error
}

// NewStore returns a Store rooted at dir (created lazily on first write).
func NewStore(dir string) *Store {
	return &Store{
		dir:    dir,
		reqCh:  make(chan txWriteReq, 256),
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
}

func (s *Store) ensureWriter() {
	s.initOnce.Do(func() {
		s.started.Store(true)
		go s.runWriter()
	})
}

func (s *Store) recordErr(err error) {
	if err == nil {
		return
	}
	s.errMu.Lock()
	if s.lastErr == nil {
		s.lastErr = err
	}
	s.errMu.Unlock()
}

func (s *Store) popErr() error {
	s.errMu.Lock()
	err := s.lastErr
	s.lastErr = nil
	s.errMu.Unlock()
	return err
}

func (s *Store) runWriter() {
	defer close(s.doneCh)
	handles := map[string]*openTranscript{}

	flushHandle := func(h *openTranscript) error {
		if h == nil || !h.dirty {
			return nil
		}
		if err := h.w.Flush(); err != nil {
			return err
		}
		h.dirty = false
		return nil
	}

	flushAll := func() error {
		var firstErr error
		for _, h := range handles {
			if err := flushHandle(h); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return firstErr
	}

	closeAll := func() error {
		err := flushAll()
		for path, h := range handles {
			if cerr := h.f.Close(); cerr != nil && err == nil {
				err = cerr
			}
			delete(handles, path)
		}
		return err
	}

	writePath := func(path string, data []byte) error {
		h := handles[path]
		if h == nil {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				return err
			}
			h = &openTranscript{
				f: f,
				w: bufio.NewWriterSize(f, 32*1024),
			}
			handles[path] = h
		}
		if _, err := h.w.Write(data); err != nil {
			_ = h.f.Close()
			delete(handles, path)
			return err
		}
		h.dirty = true
		h.lastUsed = time.Now()
		return nil
	}

	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	idleCheck := time.NewTicker(5 * time.Second)
	defer idleCheck.Stop()

	handleReq := func(req txWriteReq) bool {
		if len(req.data) > 0 {
			if err := writePath(req.path, req.data); err != nil {
				s.recordErr(err)
			}
		}
		if req.flushCh != nil {
			var err error
			if req.closeAll {
				err = closeAll()
			} else {
				err = flushAll()
			}
			if prev := s.popErr(); err == nil && prev != nil {
				err = prev
			}
			req.flushCh <- err
			return req.closeAll
		}
		return false
	}

	for {
		select {
		case req := <-s.reqCh:
			if handleReq(req) {
				return
			}
		case <-tick.C:
			if err := flushAll(); err != nil {
				s.recordErr(err)
			}
		case now := <-idleCheck.C:
			for path, h := range handles {
				if now.Sub(h.lastUsed) >= 15*time.Second {
					if err := flushHandle(h); err != nil {
						s.recordErr(err)
					}
					if err := h.f.Close(); err != nil {
						s.recordErr(err)
					}
					delete(handles, path)
				}
			}
		case <-s.stopCh:
			// Drain queued requests before shutting down.
			for {
				select {
				case req := <-s.reqCh:
					_ = handleReq(req)
				default:
					if err := closeAll(); err != nil {
						s.recordErr(err)
					}
					return
				}
			}
		}
	}
}

// MainPath is the JSONL file for a top-level session.
func (s *Store) MainPath(sessionID string) string {
	return filepath.Join(s.dir, sessionID+".jsonl")
}

// AgentPath is the sidechain JSONL file for a subagent of a session.
func (s *Store) AgentPath(sessionID, agentID string) string {
	return filepath.Join(s.dir, sessionID, "subagents", "agent-"+agentID+".jsonl")
}

// Append writes records to path as JSONL, creating parent directories. It is
// safe for concurrent use within the process.
func (s *Store) Append(path string, recs ...Record) error {
	if len(recs) == 0 {
		return nil
	}
	var buf bytes.Buffer
	buf.Grow(len(recs) * 256)
	for _, r := range recs {
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	if s.closed.Load() {
		return errors.New("transcript: store closed")
	}
	s.ensureWriter()
	if err := s.popErr(); err != nil {
		return err
	}
	select {
	case s.reqCh <- txWriteReq{path: path, data: buf.Bytes()}:
		return nil
	case <-s.stopCh:
		return errors.New("transcript: store closed")
	}
}

// Flush waits until all queued transcript appends are flushed to OS files.
func (s *Store) Flush() error {
	if !s.started.Load() || s.closed.Load() {
		return s.popErr()
	}
	ch := make(chan error, 1)
	select {
	case s.reqCh <- txWriteReq{flushCh: ch}:
	case <-s.stopCh:
		<-s.doneCh
		return s.popErr()
	}
	select {
	case err := <-ch:
		return err
	case <-s.doneCh:
		return s.popErr()
	}
}

// Close flushes pending writes, closes all open file handles, and stops the writer goroutine.
func (s *Store) Close() error {
	s.closed.Store(true)
	if !s.started.Load() {
		return s.popErr()
	}
	s.stopOnce.Do(func() { close(s.stopCh) })
	<-s.doneCh
	return s.popErr()
}

// Load reads all records from a transcript file. A missing file yields no
// records and no error (a fresh session).
func (s *Store) Load(path string) ([]Record, error) {
	if err := s.Flush(); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var recs []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024) // tolerate large tool results
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var r Record
		if err := json.Unmarshal(line, &r); err != nil {
			return nil, fmt.Errorf("transcript: corrupt line: %w", err)
		}
		recs = append(recs, r)
	}
	return recs, sc.Err()
}

// Messages reconstructs the raw conversation and cumulative usage from records.
// Boundary records are skipped — the working set is rebuilt by compaction on the
// next turn, not replayed from disk.
func Messages(recs []Record) ([]llm.Message, llm.Usage) {
	var msgs []llm.Message
	var total llm.Usage
	for _, r := range recs {
		if r.Type == "boundary" || r.Message == nil {
			continue
		}
		msgs = append(msgs, *r.Message)
		if r.Usage != nil {
			total.Add(*r.Usage)
		}
	}
	return msgs, total
}

// LastUUID returns the UUID of the last record, for continuing the parent chain.
func LastUUID(recs []Record) string {
	if len(recs) == 0 {
		return ""
	}
	return recs[len(recs)-1].UUID
}

// Writer binds a Store, a file path, and a session/agent identity, assigning a
// UUID and parent link to each record. It satisfies harness.Recorder. Append
// errors are retained (see Err) rather than panicking, so a failing disk
// degrades to in-memory operation instead of crashing the agent loop.
type Writer struct {
	store       *Store
	path        string
	sessionID   string
	agentID     string
	isSidechain bool

	mu   sync.Mutex
	last string
	now  func() time.Time
	err  error
}

// NewWriter returns a Writer for a fresh transcript file. agentID == "" writes
// the main session file; a non-empty agentID writes a subagent sidechain.
func (s *Store) NewWriter(sessionID, agentID string) *Writer {
	w := &Writer{store: s, sessionID: sessionID, agentID: agentID, now: time.Now}
	if agentID == "" {
		w.path = s.MainPath(sessionID)
	} else {
		w.path = s.AgentPath(sessionID, agentID)
		w.isSidechain = true
	}
	return w
}

// ResumeWriter returns a Writer that continues an existing main-session file,
// chaining new records after lastUUID.
func (s *Store) ResumeWriter(sessionID, lastUUID string) *Writer {
	w := s.NewWriter(sessionID, "")
	w.last = lastUUID
	return w
}

// Err returns the first append error encountered, if any.
func (w *Writer) Err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

func (w *Writer) write(r Record) {
	w.mu.Lock()
	defer w.mu.Unlock()
	r.SessionID = w.sessionID
	r.AgentID = w.agentID
	r.IsSidechain = w.isSidechain
	r.UUID = newID()
	r.ParentUUID = w.last
	r.Timestamp = w.now().UTC().Format(time.RFC3339Nano)
	if err := w.store.Append(w.path, r); err != nil {
		if w.err == nil {
			w.err = err
		}
		return
	}
	w.last = r.UUID
}

// RecordMessage persists a conversation message. usage is attributed to this
// turn (zero for non-assistant messages).
func (w *Writer) RecordMessage(m llm.Message, usage llm.Usage) {
	rec := Record{Type: "message", Message: &m}
	if usage != (llm.Usage{}) {
		u := usage
		rec.Usage = &u
	}
	w.write(rec)
}

// Flush waits for queued records written through this Writer's Store to be flushed.
func (w *Writer) Flush() error {
	if w == nil || w.store == nil {
		return nil
	}
	err := w.store.Flush()
	if err != nil {
		w.mu.Lock()
		if w.err == nil {
			w.err = err
		}
		w.mu.Unlock()
	}
	return err
}

// RecordBoundary persists a compaction boundary for observability.
func (w *Writer) RecordBoundary(meta llm.BoundaryMeta) {
	m := meta
	w.write(Record{Type: "boundary", Boundary: &m})
	_ = w.Flush()
}

// newID returns a random 128-bit hex identifier.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// rand should never fail; fall back to a time-based id to stay unique-ish.
		return hex.EncodeToString([]byte(time.Now().UTC().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(b[:])
}

// NewSessionID returns a fresh session identifier.
func NewSessionID() string { return newID() }

// NewAgentID returns a fresh subagent identifier.
func NewAgentID() string { return newID() }
