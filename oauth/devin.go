package oauth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	ProviderDevin = "devin"

	devinAuthorizeURL = "https://app.devin.ai/auth/cli/continue"
	devinTokenURL     = "https://api.devin.ai/auth/cli/token"
	devinCallbackPath = "/callback"
	devinCallbackPort = 59653

	devinFallbackLifetimeSec = 365 * 24 * 3600
	devinExpirySkewSec       = 5 * 60
)

func (m *Manager) buildDevinAuthURL(redirectURI, verifier, state string) string {
	u, _ := url.Parse(devinAuthorizeURL)
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("redirect_uri", redirectURI)
	q.Set("code_challenge", PKCEChallenge(verifier))
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	q.Set("prompt", "select_account")
	u.RawQuery = q.Encode()
	return u.String()
}

func (m *Manager) exchangeDevin(ctx context.Context, code, verifier string) (*Credential, error) {
	endpoint := m.devinTokenURL
	if endpoint == "" {
		endpoint = devinTokenURL
	}
	payload, err := json.Marshal(map[string]string{
		"code":          code,
		"code_verifier": verifier,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := m.httpClient().Do(req)
	if err != nil {
		return nil, &TransientError{Err: fmt.Errorf("devin token exchange failed: %w", err)}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, &TransientError{Err: fmt.Errorf("devin token response read failed: %w", err)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
			return nil, &TransientError{Err: fmt.Errorf("devin oauth failed (%d)", resp.StatusCode)}
		}
		return nil, fmt.Errorf("devin oauth failed (%d)", resp.StatusCode)
	}
	var res struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, errors.New("devin token response is invalid")
	}
	tok := strings.TrimSpace(res.Token)
	if tok == "" {
		return nil, errors.New("devin token response is incomplete")
	}
	now := time.Now().Unix()
	expiresAt := devinTokenExpiry(tok, now)
	return &Credential{
		Provider:     ProviderDevin,
		AccessToken:  tok,
		RefreshToken: "", // Devin session tokens do not refresh; user re-signs in on expiry.
		AccountHint:  devinAccountHint(tok),
		ExpiresAt:    expiresAt,
		UpdatedAt:    now,
	}, nil
}

func devinTokenExpiry(token string, nowSec int64) int64 {
	if exp, ok := devinJWTExp(token); ok {
		if exp > devinExpirySkewSec {
			return exp - devinExpirySkewSec
		}
		return exp
	}
	return nowSec + devinFallbackLifetimeSec
}

func devinJWTExp(token string) (int64, bool) {
	raw := token
	if idx := strings.LastIndexByte(raw, '$'); idx >= 0 {
		raw = raw[idx+1:]
	}
	parts := strings.Split(raw, ".")
	if len(parts) < 2 {
		return 0, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp <= 0 {
		return 0, false
	}
	return claims.Exp, true
}

func devinAccountHint(token string) string {
	raw := token
	if idx := strings.LastIndexByte(raw, '$'); idx >= 0 {
		raw = raw[idx+1:]
	}
	parts := strings.Split(raw, ".")
	if len(parts) < 2 {
		return "Devin"
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "Devin"
	}
	var claims struct {
		Email string `json:"email"`
		Sub   string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err == nil {
		if strings.TrimSpace(claims.Email) != "" {
			return strings.TrimSpace(claims.Email)
		}
		if strings.TrimSpace(claims.Sub) != "" {
			return strings.TrimSpace(claims.Sub)
		}
	}
	return "Devin"
}
