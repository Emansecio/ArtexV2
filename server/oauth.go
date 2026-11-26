package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/oauth"
	"github.com/Autumn-27/norma/llm"
)

type ollamaHealthCache struct {
	mu     sync.Mutex
	at     time.Time
	state  string
	detail string
}

var ollamaProbe ollamaHealthCache

func probeOllamaStatus(ctx context.Context, baseURL string, force bool) map[string]any {
	const ttl = 30 * time.Second
	ollamaProbe.mu.Lock()
	if !force && !ollamaProbe.at.IsZero() && time.Since(ollamaProbe.at) < ttl {
		st, det := ollamaProbe.state, ollamaProbe.detail
		ollamaProbe.mu.Unlock()
		return map[string]any{"state": st, "detail": det}
	}
	ollamaProbe.mu.Unlock()

	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = "http://127.0.0.1:11434"
	}
	base = strings.TrimSuffix(base, "/v1")
	pctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, base+"/api/tags", nil)
	state, detail := "disconnected", "Ollama offline"
	if err == nil {
		resp, derr := (&http.Client{Timeout: 1500 * time.Millisecond}).Do(req)
		if derr == nil {
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				state, detail = "connected", "Ollama online"
			} else {
				state, detail = "error", fmt.Sprintf("HTTP %d", resp.StatusCode)
			}
		} else {
			state, detail = "disconnected", "Ollama offline"
		}
	}
	ollamaProbe.mu.Lock()
	ollamaProbe.at = time.Now()
	ollamaProbe.state = state
	ollamaProbe.detail = detail
	ollamaProbe.mu.Unlock()
	return map[string]any{"state": state, "detail": detail}
}

func (s *Server) ensureOAuth() *oauth.Manager {
	if s.oauth != nil {
		return s.oauth
	}
	var st oauth.Store
	if s.m != nil && s.m.pg != nil {
		st = s.m.pg
	}
	s.oauth = oauth.NewManager(st)
	return s.oauth
}

// attachOAuth binds the server's OAuth manager to cfg when cfg uses an OAuth format.
// Returns ok=false if the format requires OAuth and the account is not signed in,
// or if a non-canonical custom base_url is configured.
func (s *Server) attachOAuth(cfg *agent.Config) bool {
	prov := cfg.OAuthProvider()
	if prov == "" {
		return true
	}
	if strings.TrimSpace(cfg.BaseURL) != "" && !sameLLMEndpoint(cfg.Provider(), cfg.BaseURL, cfg.Provider(), "") {
		return false
	}
	mgr := s.ensureOAuth()
	if !mgr.IsConnected(prov) {
		return false
	}
	cfg.OAuthSource = mgr
	return true
}

func (s *Server) oauthStatus(w http.ResponseWriter, r *http.Request) {
	mgr := s.ensureOAuth()
	verify := r.URL.Query().Get("verify") == "1" || r.URL.Query().Get("verify") == "true"
	force := r.URL.Query().Get("force") == "1" || r.URL.Query().Get("force") == "true"
	var providers []oauth.Status
	if verify || force {
		providers = mgr.VerifyAll(r.Context(), force)
	} else {
		providers = mgr.Statuses()
	}
	resp := map[string]any{
		"providers": providers,
	}
	if verify || force {
		resp["ollama"] = probeOllamaStatus(r.Context(), "", force)
	}
	writeJSON(w, 200, resp)
}

func (s *Server) oauthStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider string `json:"provider"`
	}
	if err := decode(w, r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	prov := strings.TrimSpace(req.Provider)
	res, err := s.ensureOAuth().StartLogin(prov)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	s.invalidateModelCatalogCache(prov)
	writeJSON(w, 200, res)
}

func (s *Server) oauthComplete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider string `json:"provider"`
		Input    string `json:"input"`
	}
	if err := decode(w, r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	prov := strings.TrimSpace(req.Provider)
	st, err := s.ensureOAuth().CompleteManual(r.Context(), prov, req.Input)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	s.invalidateModelCatalogCache(prov)
	s.invalidateProfileAgents()
	writeJSON(w, 200, st)
}

func (s *Server) oauthLogout(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider string `json:"provider"`
	}
	if err := decode(w, r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	prov := strings.TrimSpace(req.Provider)
	if err := s.ensureOAuth().SignOut(prov); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if prov == oauth.ProviderAntigravity {
		llm.ShutdownAntigravityAccount("")
	}
	s.invalidateModelCatalogCache(prov)
	s.invalidateProfileAgents()
	writeJSON(w, 200, map[string]any{"ok": true})
}
