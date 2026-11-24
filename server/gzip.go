package server

import (
	"bufio"
	"compress/gzip"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
)

const gzipMinBytes = 1024

var gzipWriterPool = sync.Pool{
	New: func() any {
		w, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
		return w
	},
}

func gzipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !acceptsGzip(r) || shouldSkipGzipPath(r) {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipResponseWriter{w: w, status: http.StatusOK}
		defer gw.close()
		next.ServeHTTP(gw, r)
	})
}

func acceptsGzip(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")
}

func shouldSkipGzipPath(r *http.Request) bool {
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return true
	}
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		return true
	}
	p := r.URL.Path
	return p == "/api/ws" || strings.HasSuffix(p, "/stream") || strings.HasSuffix(p, "/events")
}

type gzipResponseWriter struct {
	w       http.ResponseWriter
	gz      *gzip.Writer
	buf     []byte
	status  int
	wroteH  bool
	decided bool
	useGzip bool
}

func (g *gzipResponseWriter) Header() http.Header { return g.w.Header() }

func (g *gzipResponseWriter) Unwrap() http.ResponseWriter { return g.w }

func (g *gzipResponseWriter) WriteHeader(code int) {
	if g.wroteH {
		return
	}
	g.status = code
	g.wroteH = true
	if code < 200 || code == http.StatusNoContent || code == http.StatusNotModified || g.shouldBypassHeaders() {
		g.commitRaw()
	}
}

func (g *gzipResponseWriter) shouldBypassHeaders() bool {
	if g.w.Header().Get("Content-Encoding") != "" {
		return true
	}
	ct := strings.ToLower(g.w.Header().Get("Content-Type"))
	if strings.HasPrefix(ct, "text/event-stream") ||
		strings.HasPrefix(ct, "image/") ||
		strings.HasPrefix(ct, "video/") ||
		strings.HasPrefix(ct, "audio/") ||
		strings.HasPrefix(ct, "font/") ||
		strings.HasPrefix(ct, "application/font-") ||
		strings.HasPrefix(ct, "application/zip") ||
		strings.HasPrefix(ct, "application/gzip") {
		return true
	}
	return false
}

func (g *gzipResponseWriter) commitRaw() {
	if g.decided {
		return
	}
	g.decided = true
	g.useGzip = false
	if g.wroteH {
		g.w.WriteHeader(g.status)
	}
	if len(g.buf) > 0 {
		_, _ = g.w.Write(g.buf)
		g.buf = nil
	}
}

func (g *gzipResponseWriter) commitGzip() {
	if g.decided {
		return
	}
	g.decided = true
	if g.shouldBypassHeaders() {
		g.useGzip = false
		if g.wroteH {
			g.w.WriteHeader(g.status)
		}
		if len(g.buf) > 0 {
			_, _ = g.w.Write(g.buf)
			g.buf = nil
		}
		return
	}
	g.useGzip = true
	h := g.w.Header()
	h.Del("Content-Length")
	h.Set("Content-Encoding", "gzip")
	if v := h.Get("Vary"); v == "" {
		h.Set("Vary", "Accept-Encoding")
	} else if !strings.Contains(v, "Accept-Encoding") {
		h.Set("Vary", v+", Accept-Encoding")
	}
	if g.wroteH {
		g.w.WriteHeader(g.status)
	} else {
		g.w.WriteHeader(http.StatusOK)
	}
	gw := gzipWriterPool.Get().(*gzip.Writer)
	gw.Reset(g.w)
	g.gz = gw
	if len(g.buf) > 0 {
		_, _ = g.gz.Write(g.buf)
		g.buf = nil
	}
}

func (g *gzipResponseWriter) Write(p []byte) (int, error) {
	if g.decided {
		if g.useGzip {
			return g.gz.Write(p)
		}
		return g.w.Write(p)
	}
	if g.shouldBypassHeaders() {
		g.commitRaw()
		return g.w.Write(p)
	}
	g.buf = append(g.buf, p...)
	if len(g.buf) < gzipMinBytes {
		return len(p), nil
	}
	g.commitGzip()
	return len(p), nil
}

func (g *gzipResponseWriter) Flush() {
	if !g.decided {
		g.commitRaw()
	} else if g.useGzip && g.gz != nil {
		_ = g.gz.Flush()
	}
	if f, ok := g.w.(http.Flusher); ok {
		f.Flush()
	}
}

func (g *gzipResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if !g.decided {
		g.decided = true
	}
	h, ok := g.w.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("hijack unsupported")
	}
	return h.Hijack()
}

func (g *gzipResponseWriter) close() {
	if !g.decided {
		g.commitRaw()
		return
	}
	if g.useGzip && g.gz != nil {
		_ = g.gz.Close()
		g.gz.Reset(io.Discard)
		gzipWriterPool.Put(g.gz)
		g.gz = nil
	}
}
