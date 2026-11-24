package skills

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSeedDefaults(t *testing.T) {
	dst := t.TempDir()

	added, err := SeedDefaults(dst)
	if err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}
	if len(added) < 10 {
		t.Fatalf("expected at least 10 built-in skills, got %d (%v)", len(added), added)
	}
	for _, name := range added {
		if _, err := os.Stat(filepath.Join(dst, name, "SKILL.md")); err != nil {
			t.Fatalf("missing SKILL.md for seeded skill %s: %v", name, err)
		}
	}

	// User edits must survive subsequent restarts.
	editedPath := filepath.Join(dst, added[0], "SKILL.md")
	const customContent = "---\nname: custom\ndescription: user edit\n---\nkeep me\n"
	if err := os.WriteFile(editedPath, []byte(customContent), 0o644); err != nil {
		t.Fatalf("write edit: %v", err)
	}

	added2, err := SeedDefaults(dst)
	if err != nil {
		t.Fatalf("second SeedDefaults: %v", err)
	}
	if len(added2) != 0 {
		t.Fatalf("expected 0 newly seeded skills on second run, got %v", added2)
	}
	got, err := os.ReadFile(editedPath)
	if err != nil || string(got) != customContent {
		t.Fatalf("user edit was overwritten: err=%v content=%q", err, got)
	}
}
