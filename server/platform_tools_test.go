package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSkillTools functionally exercises create_skill + update_skill_file against a
// temp skill dir (no DB needed — these tools only touch the filesystem).
func TestSkillTools(t *testing.T) {
	dir := t.TempDir()
	s := &Server{skillDir: dir}

	// create_skill writes a spec-compliant SKILL.md.
	in, _ := json.Marshal(map[string]any{
		"name": "my-skill", "description": "does X", "instructions": "## do\n1. a",
	})
	res, err := s.toolCreateSkill().Call(context.Background(), in, nil)
	if err != nil || res.IsError {
		t.Fatalf("create_skill: err=%v result=%+v", err, res)
	}
	md, err := os.ReadFile(filepath.Join(dir, "my-skill", "SKILL.md"))
	if err != nil {
		t.Fatalf("SKILL.md not written: %v", err)
	}
	if !strings.Contains(string(md), "name: my-skill") || !strings.Contains(string(md), "description: does X") || !strings.Contains(string(md), "1. a") {
		t.Fatalf("SKILL.md content:\n%s", md)
	}

	// creating the same skill again fails.
	if r, _ := s.toolCreateSkill().Call(context.Background(), in, nil); !r.IsError {
		t.Fatal("duplicate create should error")
	}

	// invalid name rejected.
	bad, _ := json.Marshal(map[string]any{"name": "Bad Name", "description": "x"})
	if r, _ := s.toolCreateSkill().Call(context.Background(), bad, nil); !r.IsError {
		t.Fatal("invalid skill name should error")
	}

	// update_skill_file writes a nested script; path traversal is rejected.
	up, _ := json.Marshal(map[string]any{"name": "my-skill", "file": "scripts/run.py", "content": "print(1)"})
	if r, err := s.toolUpdateSkillFile().Call(context.Background(), up, nil); err != nil || r.IsError {
		t.Fatalf("update_skill_file: err=%v result=%+v", err, r)
	}
	got, err := os.ReadFile(filepath.Join(dir, "my-skill", "scripts", "run.py"))
	if err != nil || string(got) != "print(1)" {
		t.Fatalf("nested file not written: %v %q", err, got)
	}
	evil, _ := json.Marshal(map[string]any{"name": "my-skill", "file": "../escape.txt", "content": "x"})
	if r, _ := s.toolUpdateSkillFile().Call(context.Background(), evil, nil); !r.IsError {
		t.Fatal("path traversal should be rejected")
	}

	// create_skill safely quotes YAML special chars (": ") and writes mcps.
	colonIn, _ := json.Marshal(map[string]any{
		"name":         "graphql-audit",
		"description":  "GraphQL pentest: introspection, batching, IDOR",
		"mcps":         []string{"browser"},
		"instructions": "## Steps\n1. Check introspection",
	})
	if r, err := s.toolCreateSkill().Call(context.Background(), colonIn, nil); err != nil || r.IsError {
		t.Fatalf("create_skill with colon/mcps: err=%v result=%+v", err, r)
	}
	if err := verifySkillFile(filepath.Join(dir, "graphql-audit"), "graphql-audit", []string{"browser"}); err != nil {
		t.Fatalf("verifySkillFile failed: %v", err)
	}
}

func TestInstallSkillTool(t *testing.T) {
	const validSkill = "---\nname: remote-skill\ndescription: \"Remote skill: test\"\n---\n## Usage\n1. Run\n"
	const updatedSkill = "---\nname: remote-skill\ndescription: \"Remote skill: updated\"\n---\n## Usage\n1. Run v2\n"
	const invalidSkill = "---\nname: broken\n---\nmissing description\n"

	body := validSkill
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/valid":
			_, _ = w.Write([]byte(body))
		case "/invalid":
			_, _ = w.Write([]byte(invalidSkill))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	s := &Server{skillDir: dir, allowLoopbackSkillFetch: true}
	tool := s.toolInstallSkill()

	// Non-HTTP scheme rejected.
	badScheme, _ := json.Marshal(map[string]any{"url": "file:///etc/passwd"})
	if r, _ := tool.Call(context.Background(), badScheme, nil); !r.IsError {
		t.Fatal("expected non-HTTP URL to be rejected")
	}

	// Invalid SKILL.md (missing description) rejected and not left on disk.
	badSkill, _ := json.Marshal(map[string]any{"url": srv.URL + "/invalid"})
	if r, _ := tool.Call(context.Background(), badSkill, nil); !r.IsError {
		t.Fatal("expected invalid SKILL.md to be rejected")
	}
	if _, err := os.Stat(filepath.Join(dir, "broken")); !os.IsNotExist(err) {
		t.Fatalf("invalid skill should not be left on disk")
	}

	// Valid install succeeds.
	okIn, _ := json.Marshal(map[string]any{"url": srv.URL + "/valid"})
	if r, err := tool.Call(context.Background(), okIn, nil); err != nil || r.IsError {
		t.Fatalf("install_skill failed: err=%v result=%+v", err, r)
	}
	got, err := os.ReadFile(filepath.Join(dir, "remote-skill", "SKILL.md"))
	if err != nil || string(got) != validSkill {
		t.Fatalf("installed SKILL.md mismatch: err=%v got=%q", err, got)
	}

	// Duplicate install without overwrite is rejected.
	if r, _ := tool.Call(context.Background(), okIn, nil); !r.IsError {
		t.Fatal("expected duplicate install without overwrite to fail")
	}

	// Overwrite=true updates SKILL.md.
	body = updatedSkill
	owIn, _ := json.Marshal(map[string]any{"url": srv.URL + "/valid", "overwrite": true})
	if r, err := tool.Call(context.Background(), owIn, nil); err != nil || r.IsError {
		t.Fatalf("install_skill overwrite failed: err=%v result=%+v", err, r)
	}
	got2, err := os.ReadFile(filepath.Join(dir, "remote-skill", "SKILL.md"))
	if err != nil || string(got2) != updatedSkill {
		t.Fatalf("overwritten SKILL.md mismatch: err=%v got=%q", err, got2)
	}
}

func TestInstallSkillToolSSRFGuard(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		switch r.URL.Path {
		case "/redirect-private":
			http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
		case "/redirect-scheme":
			http.Redirect(w, r, "file:///etc/passwd", http.StatusFound)
		default:
			_, _ = w.Write([]byte("---\nname: secret\ndescription: x\n---\nbody\n"))
		}
	}))
	defer srv.Close()

	// Default production config (allowLoopbackSkillFetch=false) blocks loopback,
	// private, and metadata targets when TargetGate is off.
	s := &Server{skillDir: t.TempDir()}
	tool := s.toolInstallSkill()
	for _, target := range []string{
		srv.URL + "/valid",
		"http://localhost:8080/SKILL.md",
		"http://10.0.0.1/SKILL.md",
		"http://169.254.169.254/latest/meta-data/",
	} {
		in, _ := json.Marshal(map[string]any{"url": target})
		res, err := tool.Call(context.Background(), in, nil)
		if err != nil || !res.IsError {
			t.Fatalf("expected SSRF block for %s: err=%v res=%+v", target, err, res)
		}
	}
	if hits != 0 {
		t.Fatalf("loopback server should not have been contacted, got %d hits", hits)
	}

	// Even when loopback is allowed for the initial hop, redirects to private/metadata
	// IPs or non-http(s) schemes must be rejected.
	sLoop := &Server{skillDir: t.TempDir(), allowLoopbackSkillFetch: true}
	toolLoop := sLoop.toolInstallSkill()
	for _, path := range []string{"/redirect-private", "/redirect-scheme"} {
		in, _ := json.Marshal(map[string]any{"url": srv.URL + path})
		res, err := toolLoop.Call(context.Background(), in, nil)
		if err != nil || !res.IsError {
			t.Fatalf("expected redirect block for %s: err=%v res=%+v", path, err, res)
		}
	}
}
