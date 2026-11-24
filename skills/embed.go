// Package skills embeds ARTEX's built-in default skills inside the compiled
// binary and seeds missing skill folders into the runtime skill directory on
// startup without ever overwriting user-modified skills.
package skills

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

//go:embed */*
var embeddedSkills embed.FS

// SeedDefaults copies each built-in skill directory into dstDir if a directory
// with that name does not already exist there. Existing skill directories are
// left untouched so user edits always survive restarts and upgrades.
func SeedDefaults(dstDir string) ([]string, error) {
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir skill dir: %w", err)
	}
	entries, err := embeddedSkills.ReadDir(".")
	if err != nil {
		return nil, fmt.Errorf("read embedded skills: %w", err)
	}
	var added []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		target := filepath.Join(dstDir, name)
		if _, err := os.Stat(target); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return added, fmt.Errorf("stat skill %s: %w", name, err)
		}
		if err := extractSkillDir(name, dstDir, target); err != nil {
			return added, fmt.Errorf("seed skill %s: %w", name, err)
		}
		added = append(added, name)
	}
	sort.Strings(added)
	return added, nil
}

func extractSkillDir(name, dstDir, target string) error {
	tmpDir, err := os.MkdirTemp(dstDir, ".seed-"+name+"-*")
	if err != nil {
		return err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(tmpDir)
		}
	}()
	if err := os.Chmod(tmpDir, 0o755); err != nil {
		return err
	}
	err = fs.WalkDir(embeddedSkills, name, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(name, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		dst := filepath.Join(tmpDir, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		data, err := embeddedSkills.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dst, data, 0o644)
	})
	if err != nil {
		return err
	}
	if err := os.Rename(tmpDir, target); err != nil {
		return err
	}
	cleanup = false
	return nil
}
