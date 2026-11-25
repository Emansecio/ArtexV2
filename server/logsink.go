package server

import (
	"context"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
)

// LogLine is one captured backend log entry exposed by the /api/logs endpoints.
type LogLine struct {
	Seq   int64  `json:"seq"`
	DBID  int64  `json:"db_id,omitempty"` // server_logs.id; 0 for pre-persistence entries
	TS    string `json:"ts"`
	Level string `json:"level"` // info | warn | error
	Tag   string `json:"tag"`   // the leading [tag] (pg / planner / activity / …), if any
	Text  string `json:"text"`
}

// dbWriteReq carries a log line to the async DB writer.
type dbWriteReq struct {
	seq int64
	ll  LogLine
}

// logSinkT tees the standard logger into an in-memory ring buffer and fans new
// lines out to SSE subscribers, so the UI can show a live backend log stream.
// It still passes everything through to stderr (the terminal keeps working).
type logSinkT struct {
	mu   sync.Mutex
	ring []LogLine
	cap  int
	seq  int64
	subs map[chan LogLine]struct{}
	out  io.Writer // passthrough (stderr)

	dbOnce sync.Once
	dbCh   chan dbWriteReq // buffered async channel; nil until SetDB is called
}

var logSink = &logSinkT{cap: 3000, subs: map[chan LogLine]struct{}{}, out: os.Stderr}

// StartLogCapture redirects the standard log package through the in-memory sink
// (still writing to stderr). Call once at startup, as early as possible.
func StartLogCapture() { log.SetOutput(logSink) }

// SetDB wires a postgres DB into the sink once (idempotent). It:
//  1. Restores the last 100 log rows from DB into the ring so the /logs page
//     shows history immediately after restart.
//  2. Starts an async goroutine that persists every subsequent log line to DB.
func (s *logSinkT) SetDB(ctx context.Context, pg *db.DB) {
	if pg == nil {
		return
	}
	s.dbOnce.Do(func() {
		ch := make(chan dbWriteReq, 2000)

		// Restore last 100 rows from DB → prepend to ring as history context
		// before wiring s.dbCh so any shifted sequences never conflict with
		// already-enqueued dbWriteReq entries.
		logs, err := pg.RecentLogs(100)

		s.mu.Lock()
		if err == nil && len(logs) > 0 {
			shift := int64(len(logs))
			restored := make([]LogLine, len(logs))
			for i, l := range logs {
				restored[i] = LogLine{
					Seq:   int64(i + 1),
					DBID:  l.ID,
					TS:    l.CreatedAt.Format(time.RFC3339),
					Level: l.Level,
					Tag:   l.Tag,
					Text:  l.Text,
				}
			}
			for i := range s.ring {
				s.ring[i].Seq += shift
			}
			s.seq += shift
			// Prepend history before any in-memory startup logs already in ring.
			s.ring = append(restored, s.ring...)
			if len(s.ring) > s.cap {
				s.ring = s.ring[len(s.ring)-s.cap:]
			}
		}
		s.dbCh = ch
		s.mu.Unlock()

		// Async writer: batches pending DB rows (up to 64 per batch or 50ms) and
		// inserts via a single multi-row SQL statement, stamping DBIDs back in O(1).
		go func() {
			flushBatch := func(batch []dbWriteReq) {
				if len(batch) == 0 {
					return
				}
				rows := make([]db.DBLog, len(batch))
				for i, req := range batch {
					rows[i] = db.DBLog{
						Level: req.ll.Level,
						Tag:   req.ll.Tag,
						Text:  req.ll.Text,
					}
				}
				ids, err := pg.InsertLogsBatch(rows)
				if err != nil {
					_, _ = os.Stderr.Write([]byte("[logsink] db batch write: " + err.Error() + "\n"))
					return
				}
				s.mu.Lock()
				n := len(s.ring)
				for i, req := range batch {
					if i >= len(ids) {
						break
					}
					id := ids[i]
					if n > 0 {
						base := s.ring[0].Seq
						if idx := int(req.seq - base); idx >= 0 && idx < n && s.ring[idx].Seq == req.seq {
							s.ring[idx].DBID = id
							continue
						}
					}
					for j := n - 1; j >= 0; j-- {
						if s.ring[j].Seq == req.seq {
							s.ring[j].DBID = id
							break
						}
					}
				}
				s.mu.Unlock()
			}

			const maxBatch = 64
			batch := make([]dbWriteReq, 0, maxBatch)
			timer := time.NewTimer(time.Hour)
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			defer timer.Stop()

			for {
				if len(batch) == 0 {
					select {
					case req, ok := <-ch:
						if !ok {
							return
						}
						batch = append(batch, req)
						timer.Reset(50 * time.Millisecond)
					case <-ctx.Done():
						return
					}
				}
				for len(batch) < maxBatch {
					select {
					case req, ok := <-ch:
						if !ok {
							flushBatch(batch)
							return
						}
						batch = append(batch, req)
						continue
					default:
					}
					break
				}
				if len(batch) >= maxBatch {
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					flushBatch(batch)
					batch = batch[:0]
					continue
				}
				select {
				case req, ok := <-ch:
					if !ok {
						flushBatch(batch)
						return
					}
					batch = append(batch, req)
				case <-timer.C:
					flushBatch(batch)
					batch = batch[:0]
				case <-ctx.Done():
					flushBatch(batch)
					return
				}
			}
		}()
	})
}

// Write implements io.Writer for the log package: one call per log.Printf line.
func (s *logSinkT) Write(p []byte) (int, error) {
	_, _ = s.out.Write(p) // keep the terminal output
	line := strings.TrimRight(string(p), "\n")
	if strings.TrimSpace(line) != "" {
		s.add(parseLog(line))
	}
	return len(p), nil
}

func (s *logSinkT) add(l LogLine) {
	s.mu.Lock()
	s.seq++
	l.Seq = s.seq
	if len(s.ring) < s.cap {
		s.ring = append(s.ring, l)
	} else if s.cap > 0 {
		// Shift in place so the backing array never grows or retains stale strings.
		copy(s.ring, s.ring[1:])
		s.ring[s.cap-1] = l
	}
	ch := s.dbCh
	for sub := range s.subs {
		select {
		case sub <- l:
		default:
		}
	}
	s.mu.Unlock()

	// Async DB write (non-blocking; drops if channel is full under extreme load).
	if ch != nil {
		select {
		case ch <- dbWriteReq{seq: l.Seq, ll: l}:
		default:
		}
	}
}

// recent returns ring lines with Seq > since (capped to limit), and the latest seq.
func (s *logSinkT) recent(since int64, limit int) (lines []LogLine, cursor int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cursor = s.seq
	if limit <= 0 || limit > s.cap {
		limit = s.cap
	}
	for _, l := range s.ring {
		if l.Seq > since {
			lines = append(lines, l)
		}
	}
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	return lines, cursor
}

func (s *logSinkT) subscribe() (<-chan LogLine, func()) {
	ch := make(chan LogLine, 256)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			s.mu.Lock()
			delete(s.subs, ch)
			s.mu.Unlock()
			close(ch)
		})
	}
}

// parseLog pulls a level + [tag] out of a standard-logger line; the timestamp is
// the capture time (RFC3339).
func parseLog(line string) LogLine {
	msg := line
	// strip the "2006/01/02 15:04:05" LstdFlags prefix if present
	if len(msg) >= 20 && msg[4] == '/' && msg[7] == '/' && msg[10] == ' ' {
		msg = strings.TrimSpace(msg[19:])
	}
	tag := ""
	if strings.HasPrefix(msg, "[") {
		if i := strings.IndexByte(msg, ']'); i > 1 {
			tag = msg[1:i]
		}
	}
	return LogLine{TS: time.Now().Format(time.RFC3339), Level: levelOf(msg), Tag: tag, Text: msg}
}

func levelOf(msg string) string {
	low := strings.ToLower(msg)
	for _, k := range []string{"fatal", "panic", "error", "err:", "failed", "dropped", "refused", "✕", "unreachable"} {
		if strings.Contains(low, k) {
			return "error"
		}
	}
	for _, k := range []string{"warn", "disabled", "disable", "skip", "stopped", "⚠", "retry"} {
		if strings.Contains(low, k) {
			return "warn"
		}
	}
	return "info"
}
