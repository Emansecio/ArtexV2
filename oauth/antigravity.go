package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Autumn-27/norma/llm"
)

const (
	ProviderAntigravity = "antigravity-cli"

	agyGoogleTokenURL  = "https://oauth2.googleapis.com/token"
	agyClientSecretB64 = "R09DU1BYLUs1OEZXUjQ4NkxkTEoxbUxCOHNYQzR6NnFEQWY="
)

var agyLoginNonce atomic.Uint64

type agyPendingLogin struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	tempDir  string
	waitDone chan struct{}
	waitErr  error

	mu     sync.Mutex
	closed bool
}

func (a *agyPendingLogin) cleanup() {
	if a == nil {
		return
	}
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}
	a.closed = true
	a.mu.Unlock()

	if a.stdin != nil {
		_ = a.stdin.Close()
	}
	if a.cmd != nil && a.cmd.Process != nil {
		select {
		case <-a.waitDone:
		default:
			_ = a.cmd.Process.Kill()
		}
	}
	if a.waitDone != nil {
		<-a.waitDone
	}
	if a.tempDir != "" {
		_ = os.RemoveAll(a.tempDir)
	}
}

func (m *Manager) startAntigravityLogin() (*StartResult, error) {
	startCtx, startCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer startCancel()

	binary, err := llm.ResolveAgyBinary(startCtx, m.httpClient())
	if err != nil {
		return nil, err
	}
	loginsRoot := filepath.Join(llm.AgyGeminiDir(), "logins")
	if err := os.MkdirAll(loginsRoot, 0o700); err != nil {
		return nil, fmt.Errorf("create agy login dir: %w", err)
	}
	nonce := agyLoginNonce.Add(1)
	tempDir := filepath.Join(loginsRoot, fmt.Sprintf("login-%d-%d", os.Getpid(), nonce))
	if err := os.MkdirAll(tempDir, 0o700); err != nil {
		return nil, fmt.Errorf("create temp agy login dir: %w", err)
	}

	cmd := exec.Command(
		binary,
		"acp",
		"--login",
		"--gemini-dir", tempDir,
		"--app-data-dir", "antigravity-cli",
	)
	cmd.Env = append(os.Environ(), llm.AgyCredentialEnv()...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = os.RemoveAll(tempDir)
		return nil, fmt.Errorf("agy login stdin unavailable: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		_ = os.RemoveAll(tempDir)
		return nil, fmt.Errorf("agy login stdout unavailable: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = os.RemoveAll(tempDir)
		return nil, fmt.Errorf("agy login stderr unavailable: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = os.RemoveAll(tempDir)
		return nil, fmt.Errorf("start agy acp --login: %w", err)
	}

	type urlResult struct {
		url string
		err error
	}
	urlCh := make(chan urlResult, 1)
	var sendOnce sync.Once
	scanStream := func(r io.Reader) {
		var parser llm.AgyAuthURLParser
		buf := make([]byte, 4096)
		for {
			n, rerr := r.Read(buf)
			if n > 0 {
				u, perr := parser.Push(string(buf[:n]), false)
				if perr != nil {
					sendOnce.Do(func() { urlCh <- urlResult{err: perr} })
					return
				}
				if u != "" {
					sendOnce.Do(func() { urlCh <- urlResult{url: u} })
				}
			}
			if rerr != nil {
				if u, perr := parser.Push("", true); perr == nil && u != "" {
					sendOnce.Do(func() { urlCh <- urlResult{url: u} })
				}
				return
			}
		}
	}
	go scanStream(stdout)
	go scanStream(stderr)

	var authURL string
	select {
	case res := <-urlCh:
		if res.err != nil {
			_ = stdin.Close()
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			_ = os.RemoveAll(tempDir)
			return nil, res.err
		}
		authURL = res.url
	case <-startCtx.Done():
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = os.RemoveAll(tempDir)
		return nil, errors.New("timed out waiting for Antigravity CLI authorization URL")
	}

	agyPend := &agyPendingLogin{
		cmd:      cmd,
		stdin:    stdin,
		tempDir:  tempDir,
		waitDone: make(chan struct{}),
	}
	go func() {
		agyPend.waitErr = cmd.Wait()
		close(agyPend.waitDone)
	}()

	m.mu.Lock()
	if old := m.pending[ProviderAntigravity]; old != nil {
		old.closeResources()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	pl := &pendingLogin{
		provider: ProviderAntigravity,
		cancel:   cancel,
		agy:      agyPend,
	}
	m.pending[ProviderAntigravity] = pl
	delete(m.lastErr, ProviderAntigravity)
	m.mu.Unlock()

	go func() {
		<-ctx.Done()
		m.finishPending(pl, nil, errors.New("login timed out or was cancelled"))
	}()

	return &StartResult{
		Provider:       ProviderAntigravity,
		AuthURL:        authURL,
		ManualRequired: true,
	}, nil
}

func (m *Manager) completeAntigravityManual(ctx context.Context, pl *pendingLogin, code string) (*Credential, error) {
	if pl.agy == nil {
		return nil, errors.New("no active Antigravity CLI login process")
	}
	if _, err := io.WriteString(pl.agy.stdin, code+"\n"); err != nil {
		return nil, fmt.Errorf("send authorization code to agy: %w", err)
	}
	_ = pl.agy.stdin.Close()

	waitCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	select {
	case <-pl.agy.waitDone:
		if pl.agy.waitErr != nil {
			return nil, errors.New("Antigravity CLI sign-in failed; please try signing in again")
		}
	case <-waitCtx.Done():
		pl.agy.cleanup()
		return nil, errors.New("Antigravity CLI sign-in timed out")
	}

	exported, err := llm.ExportAgyCredentials(pl.agy.tempDir)
	if err != nil {
		return nil, err
	}
	hint := exported.Email
	if hint == "" {
		hint = exported.AccountID
	}
	if hint == "" {
		hint = "Google"
	}
	return &Credential{
		Provider:     ProviderAntigravity,
		ClientID:     llm.AgyOAuthClientID,
		AccessToken:  exported.Access,
		RefreshToken: exported.Refresh,
		IDToken:      exported.AccountID,
		AccountHint:  hint,
		ExpiresAt:    exported.ExpiresMS / 1000,
		UpdatedAt:    time.Now().Unix(),
	}, nil
}

func (m *Manager) refreshAntigravity(ctx context.Context, current *Credential) (*Credential, error) {
	if strings.TrimSpace(current.RefreshToken) == "" {
		return nil, ErrSessionRevoked
	}
	secretBytes, err := base64.StdEncoding.DecodeString(agyClientSecretB64)
	if err != nil {
		return nil, errors.New("internal Antigravity OAuth secret decode error")
	}
	endpoint := m.agyTokenURL
	if endpoint == "" {
		endpoint = agyGoogleTokenURL
	}
	form := url.Values{
		"client_id":     {llm.AgyOAuthClientID},
		"client_secret": {string(secretBytes)},
		"grant_type":    {"refresh_token"},
		"refresh_token": {current.RefreshToken},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := m.httpClient().Do(req)
	if err != nil {
		return nil, &TransientError{Err: fmt.Errorf("Antigravity CLI refresh failed: %w", err)}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, &TransientError{Err: fmt.Errorf("Antigravity CLI refresh body failed: %w", err)}
	}
	var parsed struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Error        string `json:"error"`
	}
	_ = json.Unmarshal(body, &parsed)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if (resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized) &&
			(parsed.Error == "invalid_grant" || parsed.Error == "invalid_client" || parsed.Error == "unauthorized_client") {
			return nil, ErrSessionRevoked
		}
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
			return nil, &TransientError{Err: fmt.Errorf("Antigravity CLI refresh failed: HTTP %d", resp.StatusCode)}
		}
		return nil, fmt.Errorf("Antigravity CLI refresh failed: HTTP %d", resp.StatusCode)
	}
	access := strings.TrimSpace(parsed.AccessToken)
	if access == "" {
		return nil, errors.New("Antigravity CLI refresh returned empty access_token")
	}
	if parsed.ExpiresIn <= 0 || parsed.ExpiresIn > 365*24*3600 {
		return nil, errors.New("Antigravity CLI refresh returned invalid expires_in")
	}
	refresh := strings.TrimSpace(parsed.RefreshToken)
	if refresh == "" {
		refresh = current.RefreshToken
	}
	hint := current.AccountHint
	accountID := current.IDToken
	if parsed.IDToken != "" {
		if em, sub := extractGoogleIDTokenClaims(parsed.IDToken); em != "" || sub != "" {
			if accountID != "" && sub != "" && accountID != sub {
				return nil, errors.New("Antigravity CLI refresh changed account identity")
			}
			if em != "" {
				hint = em
			}
			if sub != "" {
				accountID = sub
			}
		}
	}
	now := time.Now().Unix()
	skew := int64(60)
	if parsed.ExpiresIn/10 < skew {
		skew = parsed.ExpiresIn / 10
	}
	return &Credential{
		Provider:     ProviderAntigravity,
		ClientID:     llm.AgyOAuthClientID,
		AccessToken:  access,
		RefreshToken: refresh,
		IDToken:      accountID,
		AccountHint:  hint,
		ExpiresAt:    now + parsed.ExpiresIn - skew,
		UpdatedAt:    now,
	}, nil
}

func extractGoogleIDTokenClaims(jwt string) (email, sub string) {
	parts := strings.Split(jwt, ".")
	if len(parts) < 2 {
		return "", ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", ""
	}
	var claims struct {
		Email string `json:"email"`
		Sub   string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", ""
	}
	return strings.TrimSpace(claims.Email), strings.TrimSpace(claims.Sub)
}
