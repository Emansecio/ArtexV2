package server

import (
	"net/http"
	"strings"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/oauth"
)

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
// Returns ok=false if the format requires OAuth and the account is not signed in.
func (s *Server) attachOAuth(cfg *agent.Config) bool {
	prov := cfg.OAuthProvider()
	if prov == "" {
		return true
	}
	mgr := s.ensureOAuth()
	if !mgr.IsConnected(prov) {
		return false
	}
	cfg.OAuthSource = mgr
	return true
}

func (s *Server) oauthStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"providers": s.ensureOAuth().Statuses(),
	})
}

func (s *Server) oauthStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider string `json:"provider"`
	}
	if err := decode(w, r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	res, err := s.ensureOAuth().StartLogin(strings.TrimSpace(req.Provider))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
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
	st, err := s.ensureOAuth().CompleteManual(r.Context(), strings.TrimSpace(req.Provider), req.Input)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
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
	if err := s.ensureOAuth().SignOut(strings.TrimSpace(req.Provider)); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	s.invalidateProfileAgents()
	writeJSON(w, 200, map[string]any{"ok": true})
}
