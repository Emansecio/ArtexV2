package mcp

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

var (
	// stdioCloseGrace is the SIGTERM graceful shutdown window before escalating
	// to SIGKILL, giving servers like patchright-mcp time to terminate detached
	// child browsers and clean temporary profile directories.
	stdioCloseGrace = 2 * time.Second
	// stdioWaitDelay bounds cmd.Wait() if a detached grandchild inherits stderr/stdout.
	stdioWaitDelay = 2 * time.Second
)

// tailBuffer keeps the last max bytes written to a subprocess's stderr so
// startup/crash failures report actionable diagnostics instead of bare EOF.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func newTailBuffer(max int) *tailBuffer {
	return &tailBuffer{max: max}
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(p) >= b.max {
		b.buf = append(b.buf[:0], p[len(p)-b.max:]...)
		return len(p), nil
	}
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.max {
		b.buf = append(b.buf[:0], b.buf[len(b.buf)-b.max:]...)
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(string(b.buf))
}

// stdioCloser shuts down a spawned MCP server process and its pipes.
type stdioCloser struct {
	once  sync.Once
	cmd   *exec.Cmd
	stdin io.Closer
}

func (s *stdioCloser) Close() error {
	s.once.Do(func() {
		if s.stdin != nil {
			_ = s.stdin.Close()
		}
		if s.cmd != nil && s.cmd.Process != nil {
			terminateProcessGroup(s.cmd.Process)
			waitDone := make(chan struct{})
			go func() {
				_ = s.cmd.Wait()
				close(waitDone)
			}()
			grace := stdioCloseGrace
			if grace <= 0 {
				grace = 2 * time.Second
			}
			timer := time.NewTimer(grace)
			defer timer.Stop()
			select {
			case <-waitDone:
			case <-timer.C:
				killProcessGroup(s.cmd.Process)
				<-waitDone
			}
		}
	})
	return nil
}

// NewStdioClient spawns an MCP server as a subprocess, performs the handshake,
// and returns a connected Client (FR-10.1). Call Close to terminate it.
//
// env holds extra environment variables (e.g. API tokens an MCP server needs)
// applied on top of the parent process environment; pass nil for none.
func NewStdioClient(ctx context.Context, server, command string, env map[string]string, args ...string) (*Client, error) {
	cmd := exec.Command(command, args...)
	setProcessGroup(cmd)
	cmd.WaitDelay = stdioWaitDelay
	if len(env) > 0 {
		cmd.Env = os.Environ()
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr := newTailBuffer(4096)
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	cn := newConn(stdout, stdin)
	cn.stderrTail = stderr.String
	c := &Client{
		server: server,
		conn:   cn,
		closer: &stdioCloser{cmd: cmd, stdin: stdin},
	}
	if err := c.initialize(ctx); err != nil {
		c.Close()
		if tail := stderr.String(); tail != "" && !strings.Contains(err.Error(), "stderr:") {
			return nil, fmt.Errorf("%w (stderr: %s)", err, tail)
		}
		return nil, err
	}
	return c, nil
}

// newClientWithConn builds a Client over an arbitrary transport (used by tests).
func newClientWithConn(ctx context.Context, server string, r io.Reader, w io.Writer, closer io.Closer) (*Client, error) {
	c := &Client{server: server, conn: newConn(r, w), closer: closer}
	if err := c.initialize(ctx); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}
