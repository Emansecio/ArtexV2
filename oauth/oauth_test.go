package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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
