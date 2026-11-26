package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/norma/llm"
)

type memStore struct {
	mu       sync.Mutex
	creds    map[string]*Credential
	settings map[string]string
}

func newMemStore() *memStore {
	return &memStore{creds: map[string]*Credential{}, settings: map[string]string{}}
}

func (s *memStore) GetOAuthCredential(provider string) (*Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.creds[provider]; ok {
		cp := *c
		return &cp, nil
	}
	return nil, nil
}

func (s *memStore) UpsertOAuthCredential(c *Credential) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *c
	s.creds[c.Provider] = &cp
	return nil
}

func (s *memStore) DeleteOAuthCredential(provider string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.creds, provider)
	return nil
}

func (s *memStore) GetSetting(key string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.settings[key]
	return v, ok, nil
}

func (s *memStore) SetSetting(key, val string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settings[key] = val
	return nil
}

func TestPKCEAndManualParse(t *testing.T) {
	v := RandomToken()
	if len(v) < 32 {
		t.Fatalf("verifier too short: %d", len(v))
	}
	c1 := PKCEChallenge(v)
	c2 := PKCEChallenge(v)
	if c1 == "" || c1 != c2 {
		t.Fatalf("PKCE challenge not deterministic")
	}
	code, state := ParseManualInput("http://localhost:53692/callback?code=abc123&state=xyz")
	if code != "abc123" || state != "xyz" {
		t.Fatalf("got code=%q state=%q", code, state)
	}
	code, state = ParseManualInput("tok123#st456")
	if code != "tok123" || state != "st456" {
		t.Fatalf("got code=%q state=%q", code, state)
	}
}

func TestLoopbackStateValidation(t *testing.T) {
	cb, err := OpenLoopback("/auth/callback", "expected-state", nil)
	if err != nil {
		t.Fatalf("OpenLoopback: %v", err)
	}
	defer cb.Close()

	base := fmt.Sprintf("http://127.0.0.1:%d", cb.Port)
	resp, err := http.Get(base + "/auth/callback?code=bad&state=wrong")
	if err != nil {
		t.Fatalf("wrong state GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for wrong state, got %d", resp.StatusCode)
	}

	resp, err = http.Get(base + "/auth/callback?code=good-code&state=expected-state&client_id=oaiapp_123")
	if err != nil {
		t.Fatalf("valid GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	q, err := cb.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if q.Get("code") != "good-code" || q.Get("client_id") != "oaiapp_123" {
		t.Fatalf("unexpected query: %v", q)
	}
}

func TestSingleFlightRefresh(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		time.Sleep(50 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "fresh-token",
			"refresh_token": "fresh-refresh",
			"scope":         chatGPTScope,
			"expires_in":    3600,
		})
	}))
	defer srv.Close()

	st := newMemStore()
	_ = st.UpsertOAuthCredential(&Credential{
		Provider:     ProviderChatGPT,
		ClientID:     "oaiapp_test",
		AccessToken:  "old-token",
		RefreshToken: "old-refresh",
		Scopes:       chatGPTScope,
		ExpiresAt:    time.Now().Unix() + 10, // expiring (<180s)
	})
	mgr := NewManager(st)
	mgr.chatGPTTokenURL = srv.URL

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := mgr.AccessToken(context.Background(), ProviderChatGPT, false)
			if err != nil {
				errs <- err
				return
			}
			if tok != "fresh-token" {
				errs <- fmt.Errorf("got token %q", tok)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("expected single-flight refresh (1 hit), got %d", got)
	}
}

func TestDevinPKCEAndExpiryWithoutRefreshToken(t *testing.T) {
	exp := time.Now().Add(2 * time.Hour).Unix()
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d,"email":"dev@cognition.ai"}`, exp)))
	jwtTok := "hdr." + payload + ".sig"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["code"] != "devin-code" || body["code_verifier"] == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"token": jwtTok})
	}))
	defer srv.Close()

	st := newMemStore()
	mgr := NewManager(st)
	mgr.devinTokenURL = srv.URL

	res, err := mgr.StartLogin(ProviderDevin)
	if err != nil {
		t.Fatalf("StartLogin devin: %v", err)
	}
	if res.ManualRequired {
		t.Fatalf("devin should use loopback, not manual_required")
	}
	u, err := url.Parse(res.AuthURL)
	if err != nil || u.Path != "/auth/cli/continue" || u.Query().Get("code_challenge_method") != "S256" {
		t.Fatalf("unexpected devin auth url: %q", res.AuthURL)
	}

	// Hit loopback callback
	cbResp, err := http.Get(fmt.Sprintf("%s?code=devin-code&state=%s", res.RedirectURI, res.State))
	if err != nil {
		t.Fatalf("callback GET: %v", err)
	}
	cbResp.Body.Close()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !mgr.IsConnected(ProviderDevin) {
		time.Sleep(20 * time.Millisecond)
	}
	if !mgr.IsConnected(ProviderDevin) {
		t.Fatalf("devin should be connected after callback")
	}
	cred, err := mgr.Credential(context.Background(), ProviderDevin, false)
	if err != nil || cred.AccessToken != jwtTok || cred.AccountHint != "dev@cognition.ai" {
		t.Fatalf("unexpected devin credential: cred=%+v err=%v", cred, err)
	}

	// Force refresh on token without RefreshToken (after 10s grace or simulated expired) must revoke session.
	mgr.mu.Lock()
	mgr.lastRefresh[ProviderDevin] = time.Now().Add(-30 * time.Second)
	mgr.mu.Unlock()
	if _, err := mgr.AccessToken(context.Background(), ProviderDevin, true); !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("expected ErrSessionRevoked on forceRefresh without refresh token, got %v", err)
	}
	if mgr.IsConnected(ProviderDevin) {
		t.Fatalf("expected devin session to be cleared after revocation")
	}
}

func TestAntigravityManualHandoffNilLoopbackAndRefresh(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake agy script is unix-only")
	}
	tmp := t.TempDir()
	t.Setenv("ARTEX_AGY_HOME", filepath.Join(tmp, "agy-home"))

	authURL := "https://accounts.google.com/o/oauth2/v2/auth?client_id=" + llm.AgyOAuthClientID + "&scope=openid"
	script := `#!/bin/sh
set -eu
GEMINI_DIR=""
while [ $# -gt 0 ]; do
  if [ "$1" = "--gemini-dir" ]; then
    GEMINI_DIR="$2"
    shift 2
    continue
  fi
  shift
done
echo "Visit: ` + authURL + `"
IFS= read -r code
if [ "$code" != "google-auth-code" ]; then
  exit 1
fi
mkdir -p "$GEMINI_DIR/antigravity-cli"
cat > "$GEMINI_DIR/antigravity-cli/antigravity-oauth-token" <<EOF
{"auth_method":"consumer","id_token_email":"agy@example.com","token":{"access_token":"ya29.initial","refresh_token":"1//refresh","expiry":"2030-01-02T03:04:05Z"}}
EOF
`
	fakeBin := filepath.Join(tmp, "fake-agy")
	if err := os.WriteFile(fakeBin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake agy: %v", err)
	}
	t.Setenv("ARTEX_AGY_PATH", fakeBin)

	st := newMemStore()
	mgr := NewManager(st)

	// Verify SignOut with pending antigravity login (pl.cb == nil) never panics.
	res, err := mgr.StartLogin(ProviderAntigravity)
	if err != nil {
		t.Fatalf("StartLogin antigravity: %v", err)
	}
	if !res.ManualRequired || !strings.Contains(res.AuthURL, llm.AgyOAuthClientID) {
		t.Fatalf("unexpected start result: %+v", res)
	}
	if err := mgr.SignOut(ProviderAntigravity); err != nil {
		t.Fatalf("SignOut with nil cb: %v", err)
	}

	// Start again; pasting a malformed URL without code= must return an error
	// WITHOUT tearing down the pending login process, so a subsequent valid paste succeeds.
	res, err = mgr.StartLogin(ProviderAntigravity)
	if err != nil {
		t.Fatalf("StartLogin antigravity 2: %v", err)
	}
	if _, err := mgr.CompleteManual(context.Background(), ProviderAntigravity, "http://localhost:8080/cb?state=only"); err == nil {
		t.Fatalf("expected validation error for callback URL missing code")
	}
	if !mgr.StatusOf(ProviderAntigravity).Pending {
		t.Fatalf("expected pending antigravity login to remain active after malformed input")
	}
	status, err := mgr.CompleteManual(context.Background(), ProviderAntigravity, "http://localhost:8080/cb?code=google-auth-code")
	if err != nil {
		t.Fatalf("CompleteManual antigravity: %v", err)
	}
	if !status.Connected || status.AccountHint != "agy@example.com" {
		t.Fatalf("unexpected status: %+v", status)
	}

	// Verify Google OAuth2 refresh flow.
	refreshSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "ya29.refreshed",
			"expires_in":   3600,
		})
	}))
	defer refreshSrv.Close()
	mgr.agyTokenURL = refreshSrv.URL
	mgr.mu.Lock()
	mgr.lastRefresh[ProviderAntigravity] = time.Now().Add(-30 * time.Second)
	mgr.mu.Unlock()

	cred, err := mgr.Credential(context.Background(), ProviderAntigravity, true)
	if err != nil || cred.AccessToken != "ya29.refreshed" || cred.RefreshToken != "1//refresh" {
		t.Fatalf("unexpected refreshed agy credential: cred=%+v err=%v", cred, err)
	}
}
