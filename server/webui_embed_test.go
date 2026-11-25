//go:build embedui

package server

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

func mustGzip(t *testing.T, raw string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		t.Fatalf("gzip.NewWriterLevel: %v", err)
	}
	if _, err := zw.Write([]byte(raw)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func TestServeFileIfExistsPrecompressed(t *testing.T) {
	const rawJS = "console.log('artex-wave1-precompressed-asset');"
	fsys := fstest.MapFS{
		"_next/static/chunks/app.js.br": &fstest.MapFile{Data: []byte("fake-brotli-bytes")},
		"_next/static/chunks/app.js.gz": &fstest.MapFile{Data: mustGzip(t, rawJS)},
	}

	t.Run("prefers brotli when accepted", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/_next/static/chunks/app.js", nil)
		req.Header.Set("Accept-Encoding", "gzip, deflate, br")
		if !serveFileIfExists(rec, req, fsys, "_next/static/chunks/app.js") {
			t.Fatal("expected file to be served")
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if got := rec.Header().Get("Content-Encoding"); got != "br" {
			t.Fatalf("Content-Encoding = %q, want br", got)
		}
		if got := rec.Header().Get("Vary"); got != "Accept-Encoding" {
			t.Fatalf("Vary = %q, want Accept-Encoding", got)
		}
		if got := rec.Body.String(); got != "fake-brotli-bytes" {
			t.Fatalf("body = %q, want fake-brotli-bytes", got)
		}
	})

	t.Run("falls back to gzip when br not accepted", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/_next/static/chunks/app.js", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		if !serveFileIfExists(rec, req, fsys, "_next/static/chunks/app.js") {
			t.Fatal("expected file to be served")
		}
		if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
			t.Fatalf("Content-Encoding = %q, want gzip", got)
		}
	})

	t.Run("gunzips transparently when client accepts no encoding", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/_next/static/chunks/app.js", nil)
		if !serveFileIfExists(rec, req, fsys, "_next/static/chunks/app.js") {
			t.Fatal("expected file to be served")
		}
		if got := rec.Header().Get("Content-Encoding"); got != "" {
			t.Fatalf("Content-Encoding = %q, want empty", got)
		}
		if got := rec.Body.String(); got != rawJS {
			t.Fatalf("body = %q, want %q", got, rawJS)
		}
	})
}
