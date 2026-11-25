package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Anthropic OAuth flow for Claude Pro / Max / Claude Code subscriptions.
const (
	ProviderAnthropic = "anthropic"

	anthropicClientID     = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	anthropicAuthorizeURL = "https://claude.ai/oauth/authorize"
	anthropicTokenURL     = "https://platform.claude.com/v1/oauth/token"
	anthropicCallbackPath = "/callback"
	anthropicScopes       = "org:create_api_key user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"
)

// AnthropicOAuthBetaHeader is the beta header required when authenticating
// Anthropic Messages/Models requests with an OAuth Bearer access token.
const AnthropicOAuthBetaHeader = "oauth-2025-04-20"

func (m *Manager) buildAnthropicAuthURL(redirectURI, verifier string) string {
	q := url.Values{
		"code":                  {"true"},
		"client_id":             {anthropicClientID},
		"response_type":         {"code"},
		"redirect_uri":          {redirectURI},
		"scope":                 {anthropicScopes},
		"code_challenge":        {PKCEChallenge(verifier)},
		"code_challenge_method": {"S256"},
		"state":                 {verifier},
	}
	return anthropicAuthorizeURL + "?" + q.Encode()
}

func (m *Manager) exchangeAnthropic(ctx context.Context, payload map[string]any, prev *Credential) (*Credential, error) {
	tokenURL := m.anthropicTokenURL
	if tokenURL == "" {
		tokenURL = anthropicTokenURL
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
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
		ExpiresIn        int64  `json:"expires_in"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
		Account          *struct {
			EmailAddress string `json:"email_address"`
		} `json:"account"`
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
		return nil, fmt.Errorf("anthropic oauth rejected: %s", msg)
	}

	refresh := body.RefreshToken
	if refresh == "" && prev != nil {
		refresh = prev.RefreshToken
	}
	hint := ""
	if body.Account != nil && strings.TrimSpace(body.Account.EmailAddress) != "" {
		hint = strings.TrimSpace(body.Account.EmailAddress)
	} else if prev != nil {
		hint = prev.AccountHint
	}
	expIn := body.ExpiresIn
	if expIn <= 0 {
		expIn = 3600
	}
	now := time.Now().Unix()
	return &Credential{
		Provider:     ProviderAnthropic,
		ClientID:     anthropicClientID,
		AccessToken:  body.AccessToken,
		RefreshToken: refresh,
		AccountHint:  hint,
		Scopes:       anthropicScopes,
		ExpiresAt:    now + expIn,
		UpdatedAt:    now,
	}, nil
}

// ParseManualInput extracts code and optional state from a pasted redirect URL,
// `code#state` token, query string, or bare code.
func ParseManualInput(input string) (code, state string) {
	v := strings.TrimSpace(input)
	if v == "" {
		return "", ""
	}
	if u, err := url.Parse(v); err == nil && u.Scheme != "" && u.Host != "" {
		return u.Query().Get("code"), u.Query().Get("state")
	}
	if c, s, ok := strings.Cut(v, "#"); ok {
		return strings.TrimSpace(c), strings.TrimSpace(s)
	}
	if strings.Contains(v, "code=") {
		if q, err := url.ParseQuery(strings.TrimPrefix(v, "?")); err == nil {
			return q.Get("code"), q.Get("state")
		}
	}
	return v, ""
}
