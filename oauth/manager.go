package oauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

var (
	// ErrNotConnected indicates no OAuth credential is stored for the provider.
	ErrNotConnected = errors.New("oauth account not connected")
	// ErrSessionRevoked indicates the refresh token was revoked (`invalid_grant` / `invalid_client`).
	ErrSessionRevoked = errors.New("oauth session expired; sign in again in Models & Fallbacks")
)

// TransientError wraps retryable network or 5xx/429 errors from the token endpoint.
type TransientError struct{ Err error }

func (e *TransientError) Error() string { return e.Err.Error() }
func (e *TransientError) Unwrap() error { return e.Err }

// Credential is the persisted OAuth token bundle for one provider account.
type Credential struct {
	Provider     string
	ClientID     string
	AccessToken  string
	RefreshToken string
	IDToken      string
	AccountHint  string
	Scopes       string
	ExpiresAt    int64
	UpdatedAt    int64
}

// Status is the UI-safe view of a provider's OAuth connection state.
type Status struct {
	Provider    string `json:"provider"`
	Connected   bool   `json:"connected"`
	Pending     bool   `json:"pending"`
	AccountHint string `json:"account_hint,omitempty"`
	ExpiresAt   int64  `json:"expires_at,omitempty"`
	LastError   string `json:"last_error,omitempty"`
}

// StartResult is returned when a browser OAuth login flow begins.
type StartResult struct {
	Provider    string `json:"provider"`
	AuthURL     string `json:"auth_url"`
	RedirectURI string `json:"redirect_uri"`
	State       string `json:"state"`
}

// Store is the persistence contract implemented by db.DB.
type Store interface {
	GetOAuthCredential(provider string) (*Credential, error)
	UpsertOAuthCredential(c *Credential) error
	DeleteOAuthCredential(provider string) error
	GetSetting(key string) (string, bool, error)
	SetSetting(key, val string) error
}

type pendingLogin struct {
	provider      string
	verifier      string
	state         string
	redirectURI   string
	reuseClientID string
	cb            *LoopbackCallback
	cancel        context.CancelFunc
}

type refreshFlight struct {
	done chan struct{}
	cred *Credential
	err  error
}

// Manager coordinates OAuth login, loopback callbacks, and single-flight token
// refresh across all concurrent agents/workers.
type Manager struct {
	store Store
	http  *http.Client

	// Optional test overrides for token endpoints.
	chatGPTTokenURL   string
	anthropicTokenURL string

	mu          sync.Mutex
	cached      map[string]*Credential
	loaded      map[string]bool
	lastRefresh map[string]time.Time
	generation  map[string]int
	flights     map[string]*refreshFlight
	pending     map[string]*pendingLogin
	lastErr     map[string]string
}

// NewManager creates an OAuth manager backed by store.
func NewManager(store Store) *Manager {
	return &Manager{
		store:       store,
		http:        &http.Client{Timeout: 30 * time.Second},
		cached:      map[string]*Credential{},
		loaded:      map[string]bool{},
		lastRefresh: map[string]time.Time{},
		generation:  map[string]int{},
		flights:     map[string]*refreshFlight{},
		pending:     map[string]*pendingLogin{},
		lastErr:     map[string]string{},
	}
}

func (m *Manager) httpClient() *http.Client {
	if m.http != nil {
		return m.http
	}
	return http.DefaultClient
}

// ValidProvider reports whether provider is a supported OAuth provider key.
func ValidProvider(provider string) bool {
	return provider == ProviderChatGPT || provider == ProviderAnthropic
}

// Statuses returns the UI-safe status of all supported OAuth providers.
func (m *Manager) Statuses() []Status {
	return []Status{
		m.StatusOf(ProviderChatGPT),
		m.StatusOf(ProviderAnthropic),
	}
}

// StatusOf returns the UI-safe status for one provider.
func (m *Manager) StatusOf(provider string) Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	cred := m.loadLocked(provider)
	st := Status{
		Provider:  provider,
		Connected: cred != nil && cred.AccessToken != "",
		Pending:   m.pending[provider] != nil,
		LastError: m.lastErr[provider],
	}
	if cred != nil {
		st.AccountHint = cred.AccountHint
		st.ExpiresAt = cred.ExpiresAt
	}
	return st
}

// IsConnected reports whether provider has a stored OAuth token.
func (m *Manager) IsConnected(provider string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.loadLocked(provider)
	return c != nil && c.AccessToken != ""
}

func (m *Manager) loadLocked(provider string) *Credential {
	if m.cached[provider] != nil {
		return m.cached[provider]
	}
	if m.loaded[provider] || m.store == nil {
		return nil
	}
	m.loaded[provider] = true
	c, err := m.store.GetOAuthCredential(provider)
	if err == nil && c != nil && c.AccessToken != "" {
		m.cached[provider] = c
	}
	return m.cached[provider]
}

// StartLogin opens a temporary 127.0.0.1 loopback listener, builds the PKCE
// authorize URL, and waits asynchronously for the browser redirect.
func (m *Manager) StartLogin(provider string) (*StartResult, error) {
	if !ValidProvider(provider) {
		return nil, fmt.Errorf("unsupported oauth provider %q", provider)
	}
	verifier := RandomToken()
	state := RandomToken()
	if provider == ProviderAnthropic {
		state = verifier
	}

	var path string
	var ports []int
	if provider == ProviderChatGPT {
		path = chatGPTCallbackPath
		ports = []int{1455}
	} else {
		path = anthropicCallbackPath
		ports = []int{53692}
	}

	cb, err := OpenLoopback(path, state, ports)
	if err != nil {
		return nil, err
	}

	var redirectURI string
	if provider == ProviderChatGPT {
		redirectURI = fmt.Sprintf("http://127.0.0.1:%d%s", cb.Port, path)
	} else {
		redirectURI = fmt.Sprintf("http://localhost:%d%s", cb.Port, path)
	}

	m.mu.Lock()
	if old := m.pending[provider]; old != nil {
		old.cancel()
		old.cb.Close()
	}
	var reuseClientID string
	if existing := m.loadLocked(provider); existing != nil {
		reuseClientID = existing.ClientID
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	pl := &pendingLogin{
		provider:      provider,
		verifier:      verifier,
		state:         state,
		redirectURI:   redirectURI,
		reuseClientID: reuseClientID,
		cb:            cb,
		cancel:        cancel,
	}
	m.pending[provider] = pl
	delete(m.lastErr, provider)
	m.mu.Unlock()

	var authURL string
	if provider == ProviderChatGPT {
		authURL = m.buildChatGPTAuthURL(redirectURI, verifier, state, reuseClientID)
	} else {
		authURL = m.buildAnthropicAuthURL(redirectURI, verifier)
	}

	go func() {
		defer cb.Close()
		defer cancel()
		q, werr := cb.Wait(ctx)
		if werr != nil {
			m.finishPending(pl, nil, werr)
			return
		}
		if errParam := q.Get("error"); errParam != "" {
			desc := q.Get("error_description")
			if desc == "" {
				desc = errParam
			}
			m.finishPending(pl, nil, fmt.Errorf("oauth denied: %s", desc))
			return
		}
		cred, cerr := m.completeWithCode(ctx, pl, q.Get("code"), q.Get("state"), q.Get("client_id"))
		m.finishPending(pl, cred, cerr)
	}()

	return &StartResult{
		Provider:    provider,
		AuthURL:     authURL,
		RedirectURI: redirectURI,
		State:       state,
	}, nil
}

// CompleteManual completes an active login using a manually pasted redirect URL
// or authorization code (useful over SSH or remote browsers).
func (m *Manager) CompleteManual(ctx context.Context, provider, input string) (*Status, error) {
	m.mu.Lock()
	pl := m.pending[provider]
	m.mu.Unlock()
	if pl == nil {
		return nil, errors.New("no pending login for this provider; click Sign in first")
	}
	code, state := ParseManualInput(input)
	if code == "" {
		return nil, errors.New("could not find an authorization code in the pasted value")
	}
	if state != "" && state != pl.state {
		return nil, errors.New("oauth state mismatch")
	}
	var clientID string
	if u, err := url.Parse(strings.TrimSpace(input)); err == nil && u.Scheme != "" {
		clientID = u.Query().Get("client_id")
	}
	cred, err := m.completeWithCode(ctx, pl, code, pl.state, clientID)
	m.finishPending(pl, cred, err)
	if err != nil {
		return nil, err
	}
	st := m.StatusOf(provider)
	return &st, nil
}

func (m *Manager) completeWithCode(ctx context.Context, pl *pendingLogin, code, state, callbackClientID string) (*Credential, error) {
	if code == "" {
		return nil, errors.New("missing authorization code")
	}
	if pl.provider == ProviderChatGPT {
		clientID := callbackClientID
		if clientID == "" {
			clientID = pl.reuseClientID
		}
		if clientID == "" || clientID == "dynamic_agent_client" {
			return nil, errors.New("chatgpt dynamic client registration did not return a client_id")
		}
		return m.exchangeChatGPT(ctx, url.Values{
			"grant_type":    {"authorization_code"},
			"client_id":     {clientID},
			"code":          {code},
			"code_verifier": {pl.verifier},
			"redirect_uri":  {pl.redirectURI},
			"resource":      {chatGPTResource},
		}, clientID, nil)
	}
	return m.exchangeAnthropic(ctx, map[string]any{
		"grant_type":    "authorization_code",
		"client_id":     anthropicClientID,
		"code":          code,
		"state":         state,
		"redirect_uri":  pl.redirectURI,
		"code_verifier": pl.verifier,
	}, nil)
}

func (m *Manager) finishPending(pl *pendingLogin, cred *Credential, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pending[pl.provider] != pl {
		return
	}
	delete(m.pending, pl.provider)
	pl.cancel()
	pl.cb.Close()
	if err != nil {
		m.lastErr[pl.provider] = err.Error()
		return
	}
	if cred != nil {
		m.cached[pl.provider] = cred
		m.loaded[pl.provider] = true
		m.lastRefresh[pl.provider] = time.Now()
		delete(m.lastErr, pl.provider)
		if m.store != nil {
			if serr := m.store.UpsertOAuthCredential(cred); serr != nil {
				m.lastErr[pl.provider] = fmt.Sprintf("saved in memory only: %v", serr)
			}
		}
	}
}

// SignOut clears the stored credential and cancels any in-flight login.
func (m *Manager) SignOut(provider string) error {
	if !ValidProvider(provider) {
		return fmt.Errorf("unsupported oauth provider %q", provider)
	}
	m.mu.Lock()
	m.generation[provider]++
	if pl := m.pending[provider]; pl != nil {
		delete(m.pending, provider)
		pl.cancel()
		pl.cb.Close()
	}
	delete(m.cached, provider)
	m.loaded[provider] = true
	delete(m.lastErr, provider)
	m.mu.Unlock()
	if m.store != nil {
		return m.store.DeleteOAuthCredential(provider)
	}
	return nil
}

// AccessToken returns a valid access token for provider, refreshing single-flight
// when within 180s of expiry or when forceRefresh is requested after a 401.
func (m *Manager) AccessToken(ctx context.Context, provider string, forceRefresh bool) (string, error) {
	m.mu.Lock()
	current := m.loadLocked(provider)
	if current == nil || current.AccessToken == "" {
		m.mu.Unlock()
		return "", ErrNotConnected
	}
	now := time.Now()
	expiring := current.ExpiresAt > 0 && current.ExpiresAt-now.Unix() < 180
	justRefreshed := !m.lastRefresh[provider].IsZero() && now.Sub(m.lastRefresh[provider]) < 10*time.Second
	stale := expiring || (forceRefresh && !justRefreshed)
	flight := m.flights[provider]
	if !stale && flight == nil {
		tok := current.AccessToken
		m.mu.Unlock()
		return tok, nil
	}
	if flight != nil {
		m.mu.Unlock()
		select {
		case <-flight.done:
			if flight.err != nil {
				return "", flight.err
			}
			return flight.cred.AccessToken, nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if current.RefreshToken == "" {
		tok := current.AccessToken
		m.mu.Unlock()
		return tok, nil
	}
	gen := m.generation[provider]
	flight = &refreshFlight{done: make(chan struct{})}
	m.flights[provider] = flight
	m.mu.Unlock()

	fresh, err := m.doRefresh(ctx, current)

	m.mu.Lock()
	if m.flights[provider] == flight {
		delete(m.flights, provider)
	}
	if err == nil && m.generation[provider] != gen {
		err = ErrNotConnected
		fresh = nil
	}
	if err == nil && fresh != nil {
		m.cached[provider] = fresh
		m.lastRefresh[provider] = time.Now()
		delete(m.lastErr, provider)
		if m.store != nil {
			_ = m.store.UpsertOAuthCredential(fresh)
		}
	} else if errors.Is(err, ErrSessionRevoked) && m.generation[provider] == gen {
		delete(m.cached, provider)
		m.lastErr[provider] = err.Error()
		if m.store != nil {
			_ = m.store.DeleteOAuthCredential(provider)
		}
	}
	flight.cred = fresh
	flight.err = err
	close(flight.done)
	m.mu.Unlock()

	if err != nil {
		var te *TransientError
		if !forceRefresh && current.ExpiresAt > time.Now().Unix() && errors.As(err, &te) {
			return current.AccessToken, nil
		}
		return "", err
	}
	return fresh.AccessToken, nil
}

func (m *Manager) doRefresh(ctx context.Context, current *Credential) (*Credential, error) {
	if current.Provider == ProviderChatGPT {
		return m.exchangeChatGPT(ctx, url.Values{
			"grant_type":    {"refresh_token"},
			"client_id":     {current.ClientID},
			"refresh_token": {current.RefreshToken},
			"resource":      {chatGPTResource},
		}, current.ClientID, current)
	}
	return m.exchangeAnthropic(ctx, map[string]any{
		"grant_type":    "refresh_token",
		"client_id":     anthropicClientID,
		"refresh_token": current.RefreshToken,
	}, current)
}
