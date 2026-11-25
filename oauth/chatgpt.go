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
	"strings"
	"time"
)

// OpenAI "Sign in with ChatGPT" token-sharing flow (reference: Orb ChatGPTAuth.swift).
// Docs: https://developers.openai.com/siwc/token-sharing-open-source/sign-in
const (
	ProviderChatGPT = "chatgpt"

	chatGPTAuthorizeURL = "https://auth.openai.com/api/accounts/authorize"
	chatGPTTokenURL     = "https://auth.openai.com/api/accounts/oauth/token"
	chatGPTResource     = "https://api.openai.com/v1"
	chatGPTDirectScope  = "chatgpt.tokens.use.direct"
	chatGPTScope        = "openid profile email offline_access resource.invoke " + chatGPTDirectScope
	chatGPTCallbackPath = "/auth/callback"
	chatGPTHostIDKey    = "oauth.chatgpt.host_id"
)

func (m *Manager) chatGPTHostID() string {
	if m.store != nil {
		if id, ok, err := m.store.GetSetting(chatGPTHostIDKey); err == nil && ok && strings.TrimSpace(id) != "" {
			return strings.TrimSpace(id)
		}
	}
	id := RandomToken()
	if m.store != nil {
		_ = m.store.SetSetting(chatGPTHostIDKey, id)
	}
	return id
}

func (m *Manager) buildChatGPTAuthURL(redirectURI, verifier, state, reuseClientID string) string {
	clientID := reuseClientID
	if clientID == "" {
		clientID = "dynamic_agent_client"
	}
	q := url.Values{
		"client_id":             {clientID},
		"ext_agent_host_id":     {m.chatGPTHostID()},
		"response_type":         {"code"},
		"redirect_uri":          {redirectURI},
		"scope":                 {chatGPTScope},
		"resource":              {chatGPTResource},
		"state":                 {state},
		"nonce":                 {RandomToken()},
		"code_challenge":        {PKCEChallenge(verifier)},
		"code_challenge_method": {"S256"},
	}
	if reuseClientID == "" {
		q.Set("agent_name_hint", "Artex")
	}
	return chatGPTAuthorizeURL + "?" + q.Encode()
}

func (m *Manager) exchangeChatGPT(ctx context.Context, form url.Values, clientID string, prev *Credential) (*Credential, error) {
	tokenURL := m.chatGPTTokenURL
	if tokenURL == "" {
		tokenURL = chatGPTTokenURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := m.httpClient().Do(req)
	if err != nil {
		return nil, &TransientError{Err: err}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var body struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		IDToken          string `json:"id_token"`
		Scope            string `json:"scope"`
		ExpiresIn        int64  `json:"expires_in"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	_ = json.Unmarshal(raw, &body)

	if resp.StatusCode != http.StatusOK || body.AccessToken == "" {
		if resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			msg := body.ErrorDescription
			if msg == "" {
				msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
			}
			return nil, &TransientError{Err: errors.New(msg)}
		}
		if body.Error == "invalid_grant" || body.Error == "invalid_client" {
			return nil, ErrSessionRevoked
		}
		msg := body.ErrorDescription
		if msg == "" {
			msg = body.Error
		}
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("chatgpt oauth rejected: %s", msg)
	}

	scopes := body.Scope
	if scopes == "" && prev != nil {
		scopes = prev.Scopes
	}
	if !containsScope(scopes, chatGPTDirectScope) {
		return nil, errors.New("account did not grant direct ChatGPT token access (Plus or Pro plan required)")
	}
	refresh := body.RefreshToken
	if refresh == "" && prev != nil {
		refresh = prev.RefreshToken
	}
	idToken := body.IDToken
	if idToken == "" && prev != nil {
		idToken = prev.IDToken
	}
	expIn := body.ExpiresIn
	if expIn <= 0 {
		expIn = 3600
	}
	now := time.Now().Unix()
	return &Credential{
		Provider:     ProviderChatGPT,
		ClientID:     clientID,
		AccessToken:  body.AccessToken,
		RefreshToken: refresh,
		IDToken:      idToken,
		AccountHint:  jwtEmailHint(idToken),
		Scopes:       scopes,
		ExpiresAt:    now + expIn,
		UpdatedAt:    now,
	}, nil
}

func containsScope(scopeList, target string) bool {
	for _, s := range strings.Fields(scopeList) {
		if s == target {
			return true
		}
	}
	return false
}

// jwtEmailHint extracts the display-only email claim from an unverified JWT ID token.
func jwtEmailHint(idToken string) string {
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return ""
	}
	return strings.TrimSpace(claims.Email)
}
