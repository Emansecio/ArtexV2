package llm

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"time"
)

// ErrStreamIdleTimeout is returned when an open SSE stream receives no data or
// keepalive lines before StreamIdleTimeout expires.
var ErrStreamIdleTimeout = errors.New("llm: sse stream idle timeout")

// sseScanner reads Server-Sent Events from a response body, yielding the
// (event, data) pair of each message. Anthropic uses named events; OpenAI uses
// bare data lines. Both are handled.
type sseScanner struct {
	r            *bufio.Reader
	buf          bytes.Buffer // reused across frames to avoid per-event allocations
	closer       io.Closer
	timeout      time.Duration
	timer        *time.Timer
	lastActivity atomic.Int64 // unix nanos of last received line
	closed       atomic.Bool
	timed        atomic.Bool
}

func newSSEScanner(body io.Reader) *sseScanner {
	return &sseScanner{r: bufio.NewReaderSize(body, 64*1024)}
}

func newSSEScannerWithIdleTimeout(body io.ReadCloser, idleTimeout time.Duration) *sseScanner {
	s := &sseScanner{
		r:       bufio.NewReaderSize(body, 64*1024),
		closer:  body,
		timeout: idleTimeout,
	}
	if idleTimeout > 0 && body != nil {
		s.lastActivity.Store(time.Now().UnixNano())
		var check func()
		check = func() {
			if s.closed.Load() {
				return
			}
			elapsed := time.Duration(time.Now().UnixNano() - s.lastActivity.Load())
			if elapsed >= s.timeout {
				s.timed.Store(true)
				_ = body.Close()
				return
			}
			// Re-arm once for the remaining idle window instead of resetting the
			// runtime timer heap on every single SSE line.
			s.timer.Reset(s.timeout - elapsed)
		}
		s.timer = time.AfterFunc(idleTimeout, check)
	}
	return s
}

func (s *sseScanner) touchWatchdog() {
	if s.timeout > 0 {
		s.lastActivity.Store(time.Now().UnixNano())
	}
}

func (s *sseScanner) Close() {
	s.closed.Store(true)
	if s.timer != nil {
		s.timer.Stop()
	}
}

// readLine reads one '\n'-terminated line using ReadSlice on the fast path
// (zero allocation for lines <= 64KB) and falling back to ReadBytes for huge lines.
func (s *sseScanner) readLine() ([]byte, error) {
	frag, err := s.r.ReadSlice('\n')
	if err != bufio.ErrBufferFull {
		return frag, err
	}
	full := make([]byte, len(frag), len(frag)*2)
	copy(full, frag)
	for err == bufio.ErrBufferFull {
		frag, err = s.r.ReadSlice('\n')
		full = append(full, frag...)
	}
	return full, err
}

// next returns the next event's (type, data). event is "" for bare-data streams.
// Returns io.EOF at end of stream.
func (s *sseScanner) next() (event, data string, err error) {
	s.buf.Reset()
	for {
		s.touchWatchdog()
		rawLine, rerr := s.readLine()
		if rerr != nil {
			if s.timed.Load() {
				s.Close()
				return "", "", ErrStreamIdleTimeout
			}
			if rerr == io.EOF && (s.buf.Len() > 0 || event != "") {
				s.Close()
				return event, strings.TrimRight(s.buf.String(), "\n"), nil
			}
			s.Close()
			return "", "", rerr
		}
		rawLine = bytes.TrimRight(rawLine, "\r\n")
		if len(rawLine) == 0 {
			if s.buf.Len() > 0 || event != "" {
				return event, strings.TrimRight(s.buf.String(), "\n"), nil
			}
			continue
		}
		switch {
		case bytes.HasPrefix(rawLine, []byte("event:")):
			event = string(bytes.TrimSpace(rawLine[len("event:"):]))
		case bytes.HasPrefix(rawLine, []byte("data:")):
			d := rawLine[len("data:"):]
			d = bytes.TrimPrefix(d, []byte(" "))
			s.buf.Write(d)
			s.buf.WriteByte('\n')
		default:
			// comment / unknown field — ignore (still refreshed watchdog above)
		}
	}
}
