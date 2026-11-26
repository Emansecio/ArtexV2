package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/oauth"
	"github.com/Autumn-27/norma/llm"
)

func TestSameLLMEndpoint(t *testing.T) {
	for _, tc := range []struct {
		provA, baseA, provB, baseB string
		want                       bool
	}{
		{"anthropic", "https://gw.example", "anthropic", "https://gw.example/", true},
		{"anthropic", "https://gw.example/v1/messages", "anthropic", "https://gw.example", true},
		{"", "", "anthropic", "", true}, // empty provider defaults to anthropic
		{"openai", " https://gw.example/v1/chat/completions ", "openai", "https://gw.example/v1", true},
		{"anthropic", "https://evil.example", "anthropic", "https://gw.example", false},
		{"anthropic", "", "anthropic", "https://gw.example", false}, // canonical host != saved host
		{"openai", "", "anthropic", "", false},                      // different canonical hosts
		{"openai", "https://gw.example/v1", "openai-responses", "https://gw.example/v1", false},
		{"opencode-go", agent.DefaultOpenCodeGoBaseURL, "opencode-go", "", true},
		{"opencode-go", "https://custom.example/v1", "opencode-go", "", false},
	} {
		if got := sameLLMEndpoint(tc.provA, tc.baseA, tc.provB, tc.baseB); got != tc.want {
			t.Errorf("sameLLMEndpoint(%q,%q,%q,%q)=%v want %v", tc.provA, tc.baseA, tc.provB, tc.baseB, got, tc.want)
		}
	}
}

// keyRecorder is a fake Anthropic endpoint that records the x-api-key it receives.
func keyRecorder(t *testing.T) (*httptest.Server, *atomic.Int32, *atomic.Value) {
	var hits atomic.Int32
	var key atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		key.Store(r.Header.Get("x-api-key"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1},"data":[]}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits, &key
}

func postJSON(t *testing.T, h http.HandlerFunc, path string, body map[string]any) map[string]any {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h(rec, req)
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return resp
}

// The global key must not follow a base_url typed into the test form.
func TestLLMTestGlobalKeyOnlyForGlobalEndpoint(t *testing.T) {
	saved, savedHits, savedKey := keyRecorder(t)
	other, otherHits, _ := keyRecorder(t)
	s := &Server{}
	s.llmCfg = agent.ConfigFrom("anthropic", "m", saved.URL, "global-secret", "")

	resp := postJSON(t, s.testLLM, "/api/llm/test", map[string]any{
		"provider": "anthropic", "model": "m", "base_url": other.URL, "api_key": "", "streaming": false,
	})
	if otherHits.Load() != 0 || resp["ok"] != false {
		t.Fatalf("global key went to a form-supplied host: hits=%d resp=%v", otherHits.Load(), resp)
	}

	postJSON(t, s.testLLM, "/api/llm/test", map[string]any{
		"provider": "anthropic", "model": "m", "base_url": saved.URL + "/", "api_key": "", "streaming": false,
	})
	if k, _ := savedKey.Load().(string); savedHits.Load() != 1 || k != "global-secret" {
		t.Fatalf("global key not used for its own endpoint: hits=%d key=%q", savedHits.Load(), k)
	}
}

// The connection test applies the same User-Agent rule as saving a profile: a
// value with a control character is a 400 before any request leaves, and a
// printable value reaches the wire exactly as given.
func TestLLMTestValidatesUserAgent(t *testing.T) {
	var hits atomic.Int32
	var gotUA atomic.Value
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		gotUA.Store(r.Header.Get("User-Agent"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	t.Cleanup(fake.Close)
	s := &Server{}
	post := func(ua string) *httptest.ResponseRecorder {
		b, _ := json.Marshal(map[string]any{
			"provider": "anthropic", "model": "m", "base_url": fake.URL, "api_key": "k", "streaming": false, "user_agent": ua,
		})
		req := httptest.NewRequest(http.MethodPost, "/api/llm/test", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.testLLM(rec, req)
		return rec
	}

	if rec := post("good/1.0\r\nX-Injected: 1"); rec.Code != http.StatusBadRequest || hits.Load() != 0 {
		t.Fatalf("control character in user_agent: status %d hits=%d body=%s", rec.Code, hits.Load(), rec.Body.String())
	}

	const want = "MyTool/1.0 (contact-free)"
	if rec := post("  " + want + "  "); rec.Code != http.StatusOK {
		t.Fatalf("printable user_agent: status %d body=%s", rec.Code, rec.Body.String())
	}
	if ua, _ := gotUA.Load().(string); hits.Load() != 1 || ua != want {
		t.Fatalf("test request User-Agent = %q (hits=%d), want %q", ua, hits.Load(), want)
	}
}

// A stored profile's key goes only to that profile's base URL, for both the
// connection test and the model listing.
func TestStoredProfileKeyOnlyForStoredEndpoint(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		db.SkipPGUnavailable(t, err)
	}
	t.Cleanup(func() { m.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := New(ctx, m, t.TempDir(), t.TempDir(), t.TempDir())
	s.allowLoopbackModels = true
	s.cfgMu.Lock()
	s.llmCfg = agent.Config{} // no global key to fall back on
	s.cfgMu.Unlock()

	saved, savedHits, savedKey := keyRecorder(t)
	other, otherHits, _ := keyRecorder(t)
	pid, err := m.pg.SaveProfile(&db.LLMProfile{
		Name: "key-endpoint", Format: "anthropic", Model: "m", BaseURL: saved.URL, APIKey: "profile-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.pg.Exec("DELETE FROM llm_profiles WHERE id=$1", pid) })

	for _, ep := range []struct {
		path string
		h    http.HandlerFunc
	}{{"/api/llm/test", s.testLLM}, {"/api/llm/models", s.pgListModels}} {
		resp := postJSON(t, ep.h, ep.path, map[string]any{
			"provider": "anthropic", "model": "m", "base_url": other.URL, "api_key": "", "profile_id": pid, "streaming": false,
		})
		if otherHits.Load() != 0 || resp["ok"] != false {
			t.Fatalf("%s: stored key went to a form-supplied host: hits=%d resp=%v", ep.path, otherHits.Load(), resp)
		}
		before := savedHits.Load()
		postJSON(t, ep.h, ep.path, map[string]any{
			"provider": "anthropic", "model": "m", "base_url": saved.URL, "api_key": "", "profile_id": pid, "streaming": false,
		})
		if k, _ := savedKey.Load().(string); savedHits.Load() == before || k != "profile-secret" {
			t.Fatalf("%s: stored key not used for its own endpoint: key=%q", ep.path, k)
		}
	}
}

func TestAttachOAuthRejectsCustomBaseURL(t *testing.T) {
	s := &Server{}
	for _, prov := range []string{"chatgpt", "anthropic-oauth"} {
		cfg := agent.ConfigFrom(prov, "m", "https://evil.example/v1", "", "")
		if s.attachOAuth(&cfg) {
			t.Fatalf("attachOAuth(%q) with custom base_url returned true", prov)
		}
		if cfg.OAuthSource != nil {
			t.Fatalf("attachOAuth(%q) with custom base_url attached OAuthSource", prov)
		}
	}
}

func TestSaveProfileRejectsOAuthCustomBaseURL(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		db.SkipPGUnavailable(t, err)
	}
	t.Cleanup(func() { m.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := New(ctx, m, t.TempDir(), t.TempDir(), t.TempDir())

	for _, format := range []string{"chatgpt", "anthropic-oauth"} {
		b, _ := json.Marshal(map[string]any{
			"name":     "oauth-evil",
			"format":   format,
			"model":    "m",
			"base_url": "https://evil.example/v1",
		})
		req := httptest.NewRequest(http.MethodPost, "/api/llm/profiles", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.pgSaveProfile(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("pgSaveProfile(%q) with custom base_url: status %d body=%s, want 400", format, rec.Code, rec.Body.String())
		}
	}
}

func TestSetLLMRejectsRebindingKeyToNewEndpoint(t *testing.T) {
	s := &Server{}
	s.llmCfg = agent.ConfigFrom("anthropic", "m", "https://saved.example", "global-secret", "")

	b, _ := json.Marshal(map[string]any{
		"provider": "anthropic",
		"model":    "m",
		"base_url": "https://evil.example",
		"api_key":  "",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/llm", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.setLLM(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("setLLM with changed base_url and blank api_key: status %d body=%s, want 400", rec.Code, rec.Body.String())
	}
	s.cfgMu.Lock()
	gotBase := s.llmCfg.BaseURL
	gotKey := s.llmCfg.APIKey
	s.cfgMu.Unlock()
	if gotBase != "https://saved.example" || gotKey != "global-secret" {
		t.Fatalf("setLLM mutated global config on rejected request: base=%q key=%q", gotBase, gotKey)
	}
}

func TestSaveProfileRejectsRebindingKeyToNewEndpoint(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		db.SkipPGUnavailable(t, err)
	}
	t.Cleanup(func() { m.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := New(ctx, m, t.TempDir(), t.TempDir(), t.TempDir())

	pid, err := m.pg.SaveProfile(&db.LLMProfile{
		Name: "pin-test", Format: "anthropic", Model: "m", BaseURL: "https://saved.example", APIKey: "profile-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.pg.Exec("DELETE FROM llm_profiles WHERE id=$1", pid) })

	// Updating base_url with blank api_key must be rejected with 400.
	b, _ := json.Marshal(map[string]any{
		"id":       pid,
		"name":     "pin-test",
		"format":   "anthropic",
		"model":    "m",
		"base_url": "https://evil.example",
		"api_key":  "",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/llm/profiles", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.pgSaveProfile(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("pgSaveProfile with changed base_url and blank api_key: status %d body=%s, want 400", rec.Code, rec.Body.String())
	}

	// Updating model on the same endpoint with blank api_key must still succeed.
	bSame, _ := json.Marshal(map[string]any{
		"id":       pid,
		"name":     "pin-test",
		"format":   "anthropic",
		"model":    "m2",
		"base_url": "https://saved.example/",
		"api_key":  "",
	})
	reqSame := httptest.NewRequest(http.MethodPost, "/api/llm/profiles", bytes.NewReader(bSame))
	reqSame.Header.Set("Content-Type", "application/json")
	recSame := httptest.NewRecorder()
	s.pgSaveProfile(recSame, reqSame)
	if recSame.Code != http.StatusOK {
		t.Fatalf("pgSaveProfile on same endpoint with blank api_key: status %d body=%s, want 200", recSame.Code, recSame.Body.String())
	}
}

func TestSaveProfileSwitchToOAuthOrOllamaClearsKey(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		db.SkipPGUnavailable(t, err)
	}
	t.Cleanup(func() { m.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := New(ctx, m, t.TempDir(), t.TempDir(), t.TempDir())

	findHint := func(pid int64) string {
		t.Helper()
		list, err := m.pg.ListProfiles()
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range list {
			if p.ID == pid {
				return p.APIKeyHint
			}
		}
		t.Fatalf("profile %d not found in ListProfiles", pid)
		return ""
	}

	// Switching an existing API-key profile to any OAuth format or keyless Ollama
	// must succeed without 400 and clear the stored api_key and api_key_hint.
	for _, targetFormat := range []string{"chatgpt", "anthropic-oauth", "antigravity-cli", "devin", "ollama"} {
		t.Run("switch-to-"+targetFormat, func(t *testing.T) {
			pid, err := m.pg.SaveProfile(&db.LLMProfile{
				Name: "switch-" + targetFormat, Format: "anthropic", Model: "claude-opus-4-8",
				BaseURL: "https://saved.example", APIKey: "sk-old-secret-9999",
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _, _ = m.pg.Exec("DELETE FROM llm_profiles WHERE id=$1", pid) })

			b, _ := json.Marshal(map[string]any{
				"id":       pid,
				"name":     "switch-" + targetFormat,
				"format":   targetFormat,
				"model":    "m",
				"base_url": "",
				"api_key":  "",
			})
			req := httptest.NewRequest(http.MethodPost, "/api/llm/profiles", bytes.NewReader(b))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			s.pgSaveProfile(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("pgSaveProfile switch to %q: status %d body=%s, want 200", targetFormat, rec.Code, rec.Body.String())
			}

			got, err := m.pg.ProfileByID(pid)
			if err != nil || got == nil {
				t.Fatalf("ProfileByID(%d): got=%v err=%v", pid, got, err)
			}
			if got.Format != targetFormat {
				t.Fatalf("format=%q, want %q", got.Format, targetFormat)
			}
			if got.APIKey != "" {
				t.Fatalf("switching to %q retained old api_key %q", targetFormat, got.APIKey)
			}
			if hint := findHint(pid); hint != "" {
				t.Fatalf("switching to %q retained old api_key_hint %q", targetFormat, hint)
			}
		})
	}

	// OAuth profiles must never store an unrelated api_key or api_key_hint even if sent.
	t.Run("oauth-drops-stray-api-key", func(t *testing.T) {
		b, _ := json.Marshal(map[string]any{
			"name":     "oauth-stray-key",
			"format":   "chatgpt",
			"model":    "gpt-5.4",
			"base_url": "",
			"api_key":  "sk-unrelated-secret-4321",
		})
		req := httptest.NewRequest(http.MethodPost, "/api/llm/profiles", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.pgSaveProfile(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("pgSaveProfile oauth with stray key: status %d body=%s, want 200", rec.Code, rec.Body.String())
		}
		var resp struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.ID == 0 {
			t.Fatalf("invalid response: %s", rec.Body.String())
		}
		t.Cleanup(func() { _, _ = m.pg.Exec("DELETE FROM llm_profiles WHERE id=$1", resp.ID) })

		got, err := m.pg.ProfileByID(resp.ID)
		if err != nil || got == nil {
			t.Fatalf("ProfileByID(%d): got=%v err=%v", resp.ID, got, err)
		}
		if got.APIKey != "" || findHint(resp.ID) != "" {
			t.Fatalf("oauth profile stored api_key=%q hint=%q, want both empty", got.APIKey, findHint(resp.ID))
		}
	})

	// An authenticated Ollama profile keeps its optional key on same-endpoint edits,
	// and clears it without 400 when moved to a different Ollama endpoint with blank key.
	t.Run("ollama-optional-key-endpoint-behavior", func(t *testing.T) {
		pid, err := m.pg.SaveProfile(&db.LLMProfile{
			Name: "ollama-auth", Format: "ollama", Model: "llama3.2",
			BaseURL: "https://ollama-gw.example/v1", APIKey: "ol-secret-7777",
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = m.pg.Exec("DELETE FROM llm_profiles WHERE id=$1", pid) })

		bSame, _ := json.Marshal(map[string]any{
			"id":       pid,
			"name":     "ollama-auth",
			"format":   "ollama",
			"model":    "qwen2.5",
			"base_url": "https://ollama-gw.example/v1",
			"api_key":  "",
		})
		reqSame := httptest.NewRequest(http.MethodPost, "/api/llm/profiles", bytes.NewReader(bSame))
		reqSame.Header.Set("Content-Type", "application/json")
		recSame := httptest.NewRecorder()
		s.pgSaveProfile(recSame, reqSame)
		if recSame.Code != http.StatusOK {
			t.Fatalf("same-endpoint ollama update: status %d body=%s", recSame.Code, recSame.Body.String())
		}
		got, err := m.pg.ProfileByID(pid)
		if err != nil || got.APIKey != "ol-secret-7777" {
			t.Fatalf("same-endpoint ollama update lost key: got=%+v err=%v", got, err)
		}

		bLocal, _ := json.Marshal(map[string]any{
			"id":       pid,
			"name":     "ollama-auth",
			"format":   "ollama",
			"model":    "qwen2.5",
			"base_url": "",
			"api_key":  "",
		})
		reqLocal := httptest.NewRequest(http.MethodPost, "/api/llm/profiles", bytes.NewReader(bLocal))
		reqLocal.Header.Set("Content-Type", "application/json")
		recLocal := httptest.NewRecorder()
		s.pgSaveProfile(recLocal, reqLocal)
		if recLocal.Code != http.StatusOK {
			t.Fatalf("ollama endpoint switch: status %d body=%s, want 200", recLocal.Code, recLocal.Body.String())
		}
		got, err = m.pg.ProfileByID(pid)
		if err != nil || got.APIKey != "" || findHint(pid) != "" {
			t.Fatalf("ollama endpoint switch retained old key: key=%q hint=%q err=%v", got.APIKey, findHint(pid), err)
		}
	})
}

type memOAuthStore struct {
	creds    map[string]*oauth.Credential
	settings map[string]string
}

func (m *memOAuthStore) GetOAuthCredential(provider string) (*oauth.Credential, error) {
	if c, ok := m.creds[provider]; ok {
		cp := *c
		return &cp, nil
	}
	return nil, nil
}
func (m *memOAuthStore) UpsertOAuthCredential(c *oauth.Credential) error {
	cp := *c
	m.creds[c.Provider] = &cp
	return nil
}
func (m *memOAuthStore) DeleteOAuthCredential(provider string) error {
	delete(m.creds, provider)
	return nil
}
func (m *memOAuthStore) GetSetting(key string) (string, bool, error) {
	v, ok := m.settings[key]
	return v, ok, nil
}
func (m *memOAuthStore) SetSetting(key, val string) error {
	m.settings[key] = val
	return nil
}

func TestSetLLMOAuthValidationAndHydration(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		db.SkipPGUnavailable(t, err)
	}
	t.Cleanup(func() { m.Close() })

	// Snapshot existing "default" profile and active profile ID so we restore PG cleanly.
	var origDefault *db.LLMProfile
	if profs, err := m.pg.ListProfiles(); err == nil {
		for _, p := range profs {
			if p.Name == "default" {
				if full, ferr := m.pg.ProfileByID(p.ID); ferr == nil && full != nil {
					origDefault = full
				}
				break
			}
		}
	}
	origActive, _ := m.pg.ActiveProfile()
	t.Cleanup(func() {
		if origDefault != nil {
			origDefault.ClearAPIKey = origDefault.APIKey == ""
			_, _ = m.pg.SaveProfile(origDefault)
		} else {
			_, _ = m.pg.Exec("DELETE FROM llm_profiles WHERE name = 'default'")
		}
		if origActive != nil {
			_ = m.pg.SetActiveProfile(origActive.ID)
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := New(ctx, m, t.TempDir(), t.TempDir(), t.TempDir())
	store := &memOAuthStore{
		creds:    map[string]*oauth.Credential{},
		settings: map[string]string{},
	}
	s.oauth = oauth.NewManager(store)

	// Seed a baseline active "default" profile with preserved non-default settings.
	var defaultID int64
	if origDefault != nil {
		defaultID = origDefault.ID
	}
	defaultID, err = m.pg.SaveProfile(&db.LLMProfile{
		ID:               defaultID,
		Name:             "default",
		Format:           "openai",
		Model:            "gpt-4o",
		BaseURL:          "https://baseline.example/v1",
		APIKey:           "sk-baseline-1234",
		IsDefault:        true,
		Streaming:        false,
		MaxTokens:        4096,
		MaxTokensField:   llm.MaxTokensFieldCompletion,
		SessionHeaderKey: "X-Artex-Session",
		UserAgent:        "ArtexTestUA/1.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.pg.SetActiveProfile(defaultID); err != nil {
		t.Fatal(err)
	}
	s.cfgMu.Lock()
	s.llmCfg = agent.ConfigFrom("openai", "gpt-4o", "https://baseline.example/v1", "sk-baseline-1234", "")
	s.cfgMu.Unlock()

	postSetLLM := func(payload map[string]any) *httptest.ResponseRecorder {
		b, _ := json.Marshal(payload)
		req := httptest.NewRequest(http.MethodPost, "/api/llm", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.setLLM(rec, req)
		return rec
	}

	// 1. Custom base_url on OAuth provider must fail with 400 without touching DB.
	for _, prov := range []string{"chatgpt", "antigravity-cli", "devin"} {
		rec := postSetLLM(map[string]any{
			"provider": prov,
			"model":    "m",
			"base_url": "https://evil.example/v1",
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("setLLM(%q) custom base_url: status %d body=%s, want 400", prov, rec.Code, rec.Body.String())
		}
	}

	// 2. Signed-out OAuth provider must fail with 400 and leave the active DB profile intact.
	rec := postSetLLM(map[string]any{
		"provider": "chatgpt",
		"model":    "gpt-5.4",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("setLLM signed-out chatgpt: status %d body=%s, want 400", rec.Code, rec.Body.String())
	}
	act, err := m.pg.ActiveProfile()
	if err != nil || act == nil || act.Format != "openai" || act.Model != "gpt-4o" {
		t.Fatalf("signed-out setLLM mutated active DB profile: act=%+v err=%v", act, err)
	}

	// 3. Updating non-OAuth default profile via setLLM must preserve DB streaming/max_tokens/header/ua on live cfg & reuse cached provider.
	rec = postSetLLM(map[string]any{
		"provider": "openai",
		"model":    "gpt-4o-mini",
		"base_url": "https://baseline.example/v1",
		"api_key":  "",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("setLLM non-oauth update: status %d body=%s, want 200", rec.Code, rec.Body.String())
	}
	s.cfgMu.Lock()
	liveCfg := s.llmCfg
	liveHealthID := s.llmHealthProfileID
	s.cfgMu.Unlock()
	if liveCfg.Stream != false || liveCfg.MaxTokens != 4096 || liveCfg.MaxTokensField != llm.MaxTokensFieldCompletion ||
		liveCfg.SessionHeaderKey != "X-Artex-Session" || liveCfg.UserAgent != "ArtexTestUA/1.0" {
		t.Fatalf("setLLM dropped preserved default-profile fields on live cfg: %+v", liveCfg)
	}
	if liveHealthID != defaultID {
		t.Fatalf("setLLM bypassed cached profile provider: healthID=%d want %d", liveHealthID, defaultID)
	}

	// 4. Signed-in OAuth provider must hydrate OAuthSource on live cfg and reuse cached profile provider.
	store.creds[oauth.ProviderChatGPT] = &oauth.Credential{
		Provider:     oauth.ProviderChatGPT,
		AccessToken:  "access-token-test",
		RefreshToken: "refresh-token-test",
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
	}
	s.oauth = oauth.NewManager(store)

	rec = postSetLLM(map[string]any{
		"provider": "chatgpt",
		"model":    "gpt-5.4",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("setLLM signed-in chatgpt: status %d body=%s, want 200", rec.Code, rec.Body.String())
	}
	s.cfgMu.Lock()
	oauthLiveCfg := s.llmCfg
	oauthHealthID := s.llmHealthProfileID
	s.cfgMu.Unlock()
	if oauthLiveCfg.OAuthSource == nil {
		t.Fatal("setLLM installed OAuth provider with nil OAuthSource")
	}
	if oauthHealthID != defaultID {
		t.Fatalf("setLLM OAuth bypassed cached profile provider: healthID=%d want %d", oauthHealthID, defaultID)
	}
}
