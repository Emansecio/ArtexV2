//go:build embedui

package server

import (
	"compress/gzip"
	"embed"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"
)

// webuiDist holds the statically-exported frontend (built with
// `cd web && npm run build:static`, then copied into server/webui/dist). Compiled
// into the binary only when building with `-tags embedui`. The `all:` prefix is
// required so Next's `_next/` asset dir (leading underscore) is included.
//
//go:embed all:webui/dist
var webuiDist embed.FS

// webuiHandler serves the embedded SPA. Public (no JWT) — auth is enforced
// client-side and on /api. Serves the exported per-route index.html files and
// falls back to index.html so client-side routing still resolves unknown paths.
func (s *Server) webuiHandler() http.Handler {
	root, err := fs.Sub(webuiDist, "webui/dist")
	if err != nil {
		panic(err)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p == "" {
			p = "index.html"
		}
		// 1) exact file (assets: _next/*, favicon.ico, ...)
		// 2) route dir → <p>/index.html (trailingSlash export)
		// 3) <p>.html
		// 4) SPA fallback → index.html (hand it to client-side routing)
		for _, cand := range []string{p, p + "/index.html", p + ".html"} {
			if serveFileIfExists(w, r, root, cand) {
				return
			}
		}
		serveFileIfExists(w, r, root, "index.html")
	})
}

func acceptsEncoding(r *http.Request, enc string) bool {
	return strings.Contains(r.Header.Get("Accept-Encoding"), enc)
}

func openRegular(fsys fs.FS, name string) (fs.File, bool) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, false
	}
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		_ = f.Close()
		return nil, false
	}
	return f, true
}

// serveFileIfExists serves name from fsys when it exists as a regular file (or
// as pre-compressed .br/.gz variants produced by build.sh), with appropriate
// HTTP cache headers and zero-CPU pre-compressed delivery.
func serveFileIfExists(w http.ResponseWriter, r *http.Request, fsys fs.FS, name string) bool {
	var (
		f        fs.File
		encoding string
		gunzip   bool
		ok       bool
	)
	if acceptsEncoding(r, "br") {
		if f, ok = openRegular(fsys, name+".br"); ok {
			encoding = "br"
		}
	}
	if !ok && acceptsEncoding(r, "gzip") {
		if f, ok = openRegular(fsys, name+".gz"); ok {
			encoding = "gzip"
		}
	}
	if !ok {
		f, ok = openRegular(fsys, name)
	}
	if !ok {
		if f, ok = openRegular(fsys, name+".gz"); ok {
			gunzip = true
		}
	}
	if !ok {
		return false
	}
	defer f.Close()

	if strings.HasPrefix(name, "_next/static/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		ver := BuildVersion
		if ver == "" {
			ver = "dev"
		}
		etag := `W/"` + ver + `"`
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("ETag", etag)
		if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, etag) {
			w.WriteHeader(http.StatusNotModified)
			return true
		}
	}

	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	if encoding != "" {
		w.Header().Add("Vary", "Accept-Encoding")
		w.Header().Set("Content-Encoding", encoding)
	} else if gunzip {
		w.Header().Add("Vary", "Accept-Encoding")
		zr, err := gzip.NewReader(f)
		if err != nil {
			http.Error(w, "corrupt embedded asset", http.StatusInternalServerError)
			return true
		}
		defer zr.Close()
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, zr)
		return true
	}

	if rs, ok := f.(io.ReadSeeker); ok {
		http.ServeContent(w, r, path.Base(name), time.Time{}, rs)
		return true
	}
	http.ServeFileFS(w, r, fsys, name)
	return true
}
