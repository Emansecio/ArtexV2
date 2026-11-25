package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

// RandomToken returns 32 cryptographically random bytes encoded as unpadded base64url.
func RandomToken() string {
	buf := make([]byte, 32)
	_, _ = rand.Read(buf)
	return base64.RawURLEncoding.EncodeToString(buf)
}

// PKCEChallenge computes the RFC 7636 S256 challenge for verifier.
func PKCEChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// LoopbackCallback is a one-shot HTTP listener on 127.0.0.1 that captures an
// OAuth redirect matching both path and state (mirrors Orb's OAuthLoopback.swift).
type LoopbackCallback struct {
	Port   int
	Path   string
	ln     net.Listener
	srv    *http.Server
	result chan url.Values
}

// OpenLoopback binds 127.0.0.1 on the first available preferred port, falling
// back to an OS-assigned ephemeral port (:0).
func OpenLoopback(path, expectedState string, preferredPorts []int) (*LoopbackCallback, error) {
	var ln net.Listener
	var err error
	for _, p := range preferredPorts {
		ln, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err == nil {
			break
		}
	}
	if ln == nil {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, fmt.Errorf("open loopback listener: %w", err)
		}
	}
	port := ln.Addr().(*net.TCPAddr).Port
	cb := &LoopbackCallback{
		Port:   port,
		Path:   path,
		ln:     ln,
		result: make(chan url.Values, 1),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			writeHTML(w, http.StatusNotFound, "Not found", "This callback path does not exist.")
			return
		}
		q := r.URL.Query()
		if q.Get("state") != expectedState {
			writeHTML(w, http.StatusBadRequest, "Invalid state", "This login callback does not match an active session. You can close this tab.")
			return
		}
		if q.Get("error") != "" {
			writeHTML(w, http.StatusOK, "Sign-in cancelled", "Authorization was not granted. You can return to ARTEX and try again.")
			select {
			case cb.result <- q:
			default:
			}
			return
		}
		if q.Get("code") == "" {
			writeHTML(w, http.StatusBadRequest, "Incomplete response", "No authorization code was returned. You can return to ARTEX and try again.")
			return
		}
		writeHTML(w, http.StatusOK, "Connected!", "Sign-in complete. You can close this tab and return to ARTEX.")
		select {
		case cb.result <- q:
		default:
		}
	})
	cb.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = cb.srv.Serve(ln) }()
	return cb, nil
}

// Wait blocks until the matching callback arrives or ctx is cancelled.
func (cb *LoopbackCallback) Wait(ctx context.Context) (url.Values, error) {
	select {
	case q := <-cb.result:
		return q, nil
	case <-ctx.Done():
		return nil, errors.New("login timed out or was cancelled")
	}
}

// Close shuts down the temporary loopback listener.
func (cb *LoopbackCallback) Close() {
	if cb == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = cb.srv.Shutdown(ctx)
	_ = cb.ln.Close()
}

func writeHTML(w http.ResponseWriter, status int, title, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w,
		`<!doctype html><html><body style="font-family:-apple-system,system-ui,sans-serif;background:#0a0a0a;color:#fafafa;text-align:center;padding-top:80px"><h2>%s</h2><p style="color:#a1a1aa">%s</p></body></html>`,
		title, msg,
	)
}
