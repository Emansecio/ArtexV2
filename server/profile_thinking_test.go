package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

func TestValidateProfileThinking(t *testing.T) {
	cases := []struct {
		name    string
		profile db.LLMProfile
		wantErr bool
	}{
		{
			name:    "opus 4.6 adaptive high valid",
			profile: db.LLMProfile{Format: "anthropic-oauth", Model: "claude-opus-4-6", ThinkingType: "adaptive", ReasoningEffort: "high", MaxTokens: 16384},
		},
		{
			name:    "opus 4.6 rejects xhigh",
			profile: db.LLMProfile{Format: "anthropic-oauth", Model: "claude-opus-4-6", ThinkingType: "adaptive", ReasoningEffort: "xhigh"},
			wantErr: true,
		},
		{
			name:    "opus 4.7 accepts xhigh",
			profile: db.LLMProfile{Format: "anthropic", Model: "claude-opus-4-7", ThinkingType: "adaptive", ReasoningEffort: "xhigh", MaxTokens: 16384},
		},
		{
			name:    "opus 4.7 dotted accepts xhigh",
			profile: db.LLMProfile{Format: "anthropic-oauth", Model: "claude-opus-4.7", ThinkingType: "adaptive", ReasoningEffort: "xhigh", MaxTokens: 16384},
		},
		{
			name:    "opus 4.6 rejects tiny max_tokens with adaptive",
			profile: db.LLMProfile{Format: "anthropic-oauth", Model: "claude-opus-4-6", ThinkingType: "adaptive", ReasoningEffort: "high", MaxTokens: 128},
			wantErr: true,
		},
		{
			name:    "haiku 4.5 rejects adaptive",
			profile: db.LLMProfile{Format: "anthropic-oauth", Model: "claude-haiku-4-5", ThinkingType: "adaptive"},
			wantErr: true,
		},
		{
			name:    "sonnet 4.0 rejects adaptive",
			profile: db.LLMProfile{Format: "anthropic", Model: "claude-sonnet-4-20250514", ThinkingType: "adaptive"},
			wantErr: true,
		},
		{
			name:    "opus 4.1 rejects adaptive",
			profile: db.LLMProfile{Format: "anthropic", Model: "claude-opus-4-1-20250805", ThinkingType: "adaptive"},
			wantErr: true,
		},
		{
			name:    "sonnet 4.0 enabled valid",
			profile: db.LLMProfile{Format: "anthropic", Model: "claude-sonnet-4-20250514", ThinkingType: "enabled", ThinkingBudget: 4096, MaxTokens: 8192},
		},
		{
			name:    "claude 3.5 rejects adaptive",
			profile: db.LLMProfile{Format: "anthropic", Model: "claude-3-5-sonnet-20241022", ThinkingType: "adaptive"},
			wantErr: true,
		},
		{
			name:    "claude 3.5 rejects enabled",
			profile: db.LLMProfile{Format: "anthropic", Model: "claude-3-5-sonnet-20241022", ThinkingType: "enabled", ThinkingBudget: 2048},
			wantErr: true,
		},
		{
			name:    "claude 3.5 allows empty or disabled",
			profile: db.LLMProfile{Format: "anthropic", Model: "claude-3-5-sonnet-20241022", ThinkingType: "disabled"},
		},
		{
			name:    "claude 3.5 rejects reasoning_effort",
			profile: db.LLMProfile{Format: "anthropic", Model: "claude-3-5-sonnet-20241022", ThinkingType: "", ReasoningEffort: "high"},
			wantErr: true,
		},
		{
			name:    "claude 3 haiku rejects reasoning_effort",
			profile: db.LLMProfile{Format: "anthropic", Model: "claude-3-haiku-20240307", ThinkingType: "", ReasoningEffort: "medium"},
			wantErr: true,
		},
		{
			name:    "claude 3.5 allows reasoning_effort none",
			profile: db.LLMProfile{Format: "anthropic", Model: "claude-3-5-sonnet-20241022", ThinkingType: "", ReasoningEffort: "none"},
		},
		{
			name:    "unknown custom anthropic model allows adaptive",
			profile: db.LLMProfile{Format: "anthropic", Model: "my-custom-claude", ThinkingType: "adaptive"},
		},
		{
			name:    "third-party minimax on anthropic gateway allows enabled thinking and effort",
			profile: db.LLMProfile{Format: "anthropic", BaseURL: "https://api.minimax.io/anthropic", Model: "MiniMax-M2.5", ThinkingType: "enabled", ThinkingBudget: 4096, MaxTokens: 8192, ReasoningEffort: "high"},
		},
		{
			name:    "third-party glm-4.5 on anthropic gateway allows enabled thinking",
			profile: db.LLMProfile{Format: "anthropic", Model: "glm-4.5", ThinkingType: "enabled", ThinkingBudget: 4096, MaxTokens: 8192},
		},
		{
			name:    "third-party deepseek-v3.1 on anthropic gateway allows enabled thinking and effort",
			profile: db.LLMProfile{Format: "anthropic", Model: "deepseek-v3.1", ThinkingType: "enabled", ThinkingBudget: 4096, MaxTokens: 8192, ReasoningEffort: "high"},
		},
		{
			name:    "third-party anthropic gateway model still enforces thinking_budget >= 1024",
			profile: db.LLMProfile{Format: "anthropic", Model: "MiniMax-M2.5", ThinkingType: "enabled", ThinkingBudget: 512, MaxTokens: 8192},
			wantErr: true,
		},
		{
			name:    "third-party anthropic gateway model still enforces max_tokens > thinking_budget",
			profile: db.LLMProfile{Format: "anthropic", Model: "MiniMax-M2.5", ThinkingType: "enabled", ThinkingBudget: 4096, MaxTokens: 4096},
			wantErr: true,
		},
		{
			name:    "haiku 4.5 enabled with valid budget",
			profile: db.LLMProfile{Format: "anthropic-oauth", Model: "claude-haiku-4-5", ThinkingType: "enabled", ThinkingBudget: 4096, MaxTokens: 8192},
		},
		{
			name:    "haiku 4.5 enabled rejects budget below 1024",
			profile: db.LLMProfile{Format: "anthropic-oauth", Model: "claude-haiku-4-5", ThinkingType: "enabled", ThinkingBudget: 512, MaxTokens: 4096},
			wantErr: true,
		},
		{
			name:    "haiku 4.5 enabled rejects max_tokens <= budget",
			profile: db.LLMProfile{Format: "anthropic-oauth", Model: "claude-haiku-4-5", ThinkingType: "enabled", ThinkingBudget: 4096, MaxTokens: 4096},
			wantErr: true,
		},
		{
			name:    "opus 5 rejects enabled",
			profile: db.LLMProfile{Format: "anthropic", Model: "claude-opus-5", ThinkingType: "enabled", ThinkingBudget: 4096},
			wantErr: true,
		},
		{
			name:    "rejects unknown thinking_display",
			profile: db.LLMProfile{Format: "anthropic", Model: "claude-opus-4-6", ThinkingType: "adaptive", ThinkingDisplay: "verbose"},
			wantErr: true,
		},
		{
			name:    "openai-responses accepts omitted display",
			profile: db.LLMProfile{Format: "openai-responses", Model: "o3", ReasoningEffort: "high", ThinkingDisplay: "omitted"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.profile
			err := validateProfileThinking(&p)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestSaveProfileRoundTripThinkingFields(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		db.SkipPGUnavailable(t, err)
	}
	t.Cleanup(func() { m.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := New(ctx, m, t.TempDir(), t.TempDir(), t.TempDir())

	b, _ := json.Marshal(map[string]any{
		"name":             "thinking-roundtrip-test",
		"format":           "anthropic",
		"model":            "claude-haiku-4-5",
		"api_key":          "sk-test-roundtrip",
		"thinking_type":    "enabled",
		"thinking_budget":  8192,
		"thinking_display": "omitted",
		"max_tokens":       16384,
	})
	saveReq := httptest.NewRequest(http.MethodPost, "/api/llm/profiles", bytes.NewReader(b))
	saveReq.Header.Set("Content-Type", "application/json")
	saveRec := httptest.NewRecorder()
	s.pgSaveProfile(saveRec, saveReq)
	if saveRec.Code != http.StatusOK {
		t.Fatalf("pgSaveProfile: status %d body=%s", saveRec.Code, saveRec.Body.String())
	}
	var saveResp struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(saveRec.Body.Bytes(), &saveResp); err != nil || saveResp.ID == 0 {
		t.Fatalf("decode save response: err=%v body=%s", err, saveRec.Body.String())
	}
	t.Cleanup(func() { _, _ = m.pg.Exec("DELETE FROM llm_profiles WHERE id=$1", saveResp.ID) })

	listReq := httptest.NewRequest(http.MethodGet, "/api/llm/profiles", nil)
	listRec := httptest.NewRecorder()
	s.pgListProfiles(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("pgListProfiles: status %d body=%s", listRec.Code, listRec.Body.String())
	}
	var listResp struct {
		Profiles []LLMProfileDTO `json:"profiles"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	wantID := strconv.FormatInt(saveResp.ID, 10)
	for _, p := range listResp.Profiles {
		if p.ID == wantID {
			if p.ThinkingBudget != 8192 || p.ThinkingDisplay != "omitted" {
				t.Fatalf("GET /api/llm/profiles returned thinking_budget=%d thinking_display=%q, want 8192 / omitted",
					p.ThinkingBudget, p.ThinkingDisplay)
			}
			return
		}
	}
	t.Fatalf("saved profile id=%s not found in GET /api/llm/profiles", wantID)
}

// snapshotProfileByName returns a full copy (including decrypted APIKey and APIKeyHint)
// of any existing profile with the given name, plus a restore closure that puts the row
// back to its exact prior state (or deletes it if it did not exist).
func snapshotProfileByName(t *testing.T, pg *db.DB, name string) (*db.LLMProfile, func()) {
	t.Helper()
	profs, err := pg.ListProfiles()
	if err != nil {
		t.Fatalf("list profiles for snapshot %q: %v", name, err)
	}
	var snap *db.LLMProfile
	for _, p := range profs {
		if p.Name == name {
			full, err := pg.ProfileByID(p.ID)
			if err != nil {
				t.Fatalf("load profile id=%d (%q) for snapshot: %v", p.ID, name, err)
			}
			if full == nil {
				t.Fatalf("load profile id=%d (%q) for snapshot: not found", p.ID, name)
			}
			full.APIKeyHint = p.APIKeyHint
			snap = full
			break
		}
	}
	restore := func() {
		if snap != nil {
			cp := *snap
			cp.ClearAPIKey = cp.APIKey == ""
			if _, err := pg.SaveProfile(&cp); err != nil {
				t.Errorf("restore profile %q: %v", name, err)
			}
			return
		}
		if _, err := pg.Exec("DELETE FROM llm_profiles WHERE name=$1", name); err != nil {
			t.Errorf("delete temporary profile %q: %v", name, err)
		}
	}
	return snap, restore
}

func TestSaveProfileSnapshotRestoresAPIKey(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		db.SkipPGUnavailable(t, err)
	}
	t.Cleanup(func() { m.Close() })

	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	cases := []struct {
		name     string
		origKey  string
		wantHint string
	}{
		{name: "snapshot-restore-keyed-" + suffix, origKey: "sk-original-secret-9876", wantHint: "…9876"},
		{name: "snapshot-restore-keyless-" + suffix, origKey: "", wantHint: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, err := m.pg.SaveProfile(&db.LLMProfile{
				Name:      tc.name,
				Format:    "anthropic",
				Model:     "claude-haiku-4-5",
				APIKey:    tc.origKey,
				Streaming: true,
			})
			if err != nil {
				t.Fatalf("seed profile %q: %v", tc.name, err)
			}
			t.Cleanup(func() { _, _ = m.pg.Exec("DELETE FROM llm_profiles WHERE id=$1", id) })

			snap, restore := snapshotProfileByName(t, m.pg, tc.name)
			if snap == nil || snap.ID != id {
				t.Fatalf("snapshotProfileByName(%q) = %+v, want id=%d", tc.name, snap, id)
			}

			// Overwrite the profile with a test API key, mirroring TestSaveProfileLegacySaveLLMConfigPreservesThinking.
			if _, err := m.pg.SaveProfile(&db.LLMProfile{
				ID:        id,
				Name:      tc.name,
				Format:    "anthropic",
				Model:     "claude-opus-4-6",
				APIKey:    "sk-default-test",
				Streaming: true,
			}); err != nil {
				t.Fatalf("overwrite profile %q: %v", tc.name, err)
			}

			restore()

			got, err := m.pg.ProfileByID(id)
			if err != nil || got == nil {
				t.Fatalf("ProfileByID(%d) after restore: %v", id, err)
			}
			if got.APIKey != tc.origKey {
				t.Fatalf("restored APIKey = %q, want %q", got.APIKey, tc.origKey)
			}
			if got.Model != "claude-haiku-4-5" {
				t.Fatalf("restored Model = %q, want claude-haiku-4-5", got.Model)
			}

			profs, err := m.pg.ListProfiles()
			if err != nil {
				t.Fatalf("ListProfiles after restore: %v", err)
			}
			var found bool
			for _, p := range profs {
				if p.ID == id {
					found = true
					if p.APIKeyHint != tc.wantHint {
						t.Fatalf("restored APIKeyHint = %q, want %q", p.APIKeyHint, tc.wantHint)
					}
					break
				}
			}
			if !found {
				t.Fatalf("restored profile id=%d missing from ListProfiles", id)
			}
		})
	}
}

func TestSaveProfileLegacySaveLLMConfigPreservesThinking(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		db.SkipPGUnavailable(t, err)
	}
	t.Cleanup(func() { m.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := New(ctx, m, t.TempDir(), t.TempDir(), t.TempDir())

	// Snapshot any pre-existing "default" and active profile so the local DB is restored untouched.
	prevActive, err := m.pg.ActiveProfile()
	if err != nil {
		t.Fatalf("snapshot active profile: %v", err)
	}
	prevDefault, restoreDefault := snapshotProfileByName(t, m.pg, "default")
	t.Cleanup(func() {
		restoreDefault()
		if prevActive != nil {
			if err := m.pg.SetActiveProfile(prevActive.ID); err != nil {
				t.Errorf("restore active profile: %v", err)
			}
		} else if prevDefault != nil {
			if _, err := m.pg.Exec("UPDATE llm_profiles SET is_default=FALSE WHERE id=$1", prevDefault.ID); err != nil {
				t.Errorf("restore inactive default profile: %v", err)
			}
		}
	})

	defID := int64(0)
	if prevDefault != nil {
		defID = prevDefault.ID
	}
	_, err = m.pg.SaveProfile(&db.LLMProfile{
		ID:              defID,
		Name:            "default",
		Format:          "anthropic",
		Model:           "claude-haiku-4-5",
		APIKey:          "sk-default-test",
		ThinkingType:    "enabled",
		ThinkingBudget:  6144,
		ThinkingDisplay: "omitted",
		Streaming:       true,
	})
	if err != nil {
		t.Fatalf("seed default profile: %v", err)
	}

	// Legacy saveLLMConfig with ThinkingType="enabled" and zero ThinkingBudget/ThinkingDisplay
	// must preserve both stored fields.
	cfg := agent.ConfigFrom("anthropic", "claude-haiku-4-5", "", "sk-default-test", "")
	cfg.ThinkingType = "enabled"
	if err := s.saveLLMConfig(cfg); err != nil {
		t.Fatalf("saveLLMConfig enabled: %v", err)
	}
	got, err := m.pg.ActiveProfile()
	if err != nil || got == nil {
		t.Fatalf("ActiveProfile after enabled save: %v", err)
	}
	if got.ThinkingBudget != 6144 || got.ThinkingDisplay != "omitted" {
		t.Fatalf("saveLLMConfig wiped thinking fields: budget=%d display=%q, want 6144 / omitted",
			got.ThinkingBudget, got.ThinkingDisplay)
	}

	// Switching ThinkingType away from "enabled" must clear stale ThinkingBudget while keeping ThinkingDisplay.
	cfg.Model = "claude-opus-4-6"
	cfg.ThinkingType = "adaptive"
	if err := s.saveLLMConfig(cfg); err != nil {
		t.Fatalf("saveLLMConfig adaptive: %v", err)
	}
	got2, err := m.pg.ActiveProfile()
	if err != nil || got2 == nil {
		t.Fatalf("ActiveProfile after adaptive save: %v", err)
	}
	if got2.ThinkingBudget != 0 || got2.ThinkingDisplay != "omitted" {
		t.Fatalf("saveLLMConfig adaptive: budget=%d display=%q, want 0 / omitted",
			got2.ThinkingBudget, got2.ThinkingDisplay)
	}
}
