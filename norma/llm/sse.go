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
	r       *bufio.Reader
	closer  io.Closer
	timeout time.Duration
	timer   *time.Timer
	timed   atomic.Bool
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
		s.timer = time.AfterFunc(idleTimeout, func() {
			s.timed.Store(true)
			_ = body.Close()
		})
	}
	return s
}

func (s *sseScanner) resetWatchdog() {
	if s.timer != nil && s.timeout > 0 {
		s.timer.Reset(s.timeout)
	}
}

func (s *sseScanner) Close() {
	if s.timer != nil {
		s.timer.Stop()
	}
}

// next returns the next event's (type, data). event is "" for bare-data streams.
// Returns io.EOF at end of stream.
func (s *sseScanner) next() (event, data string, err error) {
	var buf bytes.Buffer
	for {
		s.resetWatchdog()
		line, rerr := s.r.ReadString('\n')
		if rerr != nil {
			if s.timed.Load() {
				s.Close()
				return "", "", ErrStreamIdleTimeout
			}
			if rerr == io.EOF && (buf.Len() > 0 || event != "") {
				s.Close()
				return event, strings.TrimRight(buf.String(), "\n"), nil
			}
			s.Close()
			return "", "", rerr
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if buf.Len() > 0 || event != "" {
				return event, strings.TrimRight(buf.String(), "\n"), nil
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(line[len("event:"):])
		case strings.HasPrefix(line, "data:"):
			d := line[len("data:"):]
			d = strings.TrimPrefix(d, " ")
			buf.WriteString(d)
			buf.WriteByte('\n')
		default:
			// comment / unknown field — ignore (still resets watchdog above)
		}
	}
}
