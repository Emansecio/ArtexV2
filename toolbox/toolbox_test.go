package toolbox

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func buildTestTarZst(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(zw)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name:     name,
			Mode:     0o755,
			Size:     int64(len(body)),
			Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestMaterializeArchiveIdempotentAndUpdatesPath(t *testing.T) {
	origPath := os.Getenv("PATH")
	t.Cleanup(func() {
		_ = os.Setenv("PATH", origPath)
		stateMu.Lock()
		activeDir = ""
		activeTools = nil
		stateMu.Unlock()
	})

	dataDir := t.TempDir()
	staleDir := filepath.Join(dataDir, "toolbox", "deadbeef00000000")
	if err := os.MkdirAll(staleDir, 0o755); err != nil {
		t.Fatal(err)
	}

	raw := buildTestTarZst(t, map[string]string{
		"nuclei": "#!/bin/sh\necho nuclei\n",
		"httpx":  "#!/bin/sh\necho httpx\n",
	})

	binDir, names, err := materializeArchive(dataDir, raw)
	if err != nil {
		t.Fatalf("materializeArchive failed: %v", err)
	}
	if !slices.Equal(names, []string{"httpx", "nuclei"}) {
		t.Fatalf("names=%v, want [httpx nuclei]", names)
	}
	if !strings.HasPrefix(os.Getenv("PATH"), binDir+string(os.PathListSeparator)) {
		t.Fatalf("PATH did not prepend %s: %s", binDir, os.Getenv("PATH"))
	}
	if _, err := os.Stat(staleDir); !os.IsNotExist(err) {
		t.Fatalf("expected stale version dir %s to be pruned", staleDir)
	}
	info, err := os.Stat(filepath.Join(binDir, "nuclei"))
	if err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("nuclei should be executable: info=%v err=%v", info, err)
	}

	// Second call hits fast path without duplicating PATH entry.
	binDir2, names2, err := materializeArchive(dataDir, raw)
	if err != nil || binDir2 != binDir || !slices.Equal(names2, names) {
		t.Fatalf("second materialize mismatch: dir=%s names=%v err=%v", binDir2, names2, err)
	}
	if strings.Count(os.Getenv("PATH"), binDir) != 1 {
		t.Fatalf("PATH duplicated binDir: %s", os.Getenv("PATH"))
	}
	note := BashPromptNote()
	if !strings.Contains(note, "nuclei:") || !strings.Contains(note, "httpx:") {
		t.Fatalf("unexpected BashPromptNote: %q", note)
	}
}

func TestMaterializeWordlists(t *testing.T) {
	t.Cleanup(func() {
		stateMu.Lock()
		activeWordlistsDir = ""
		activeWordlists = nil
		stateMu.Unlock()
	})

	dataDir := t.TempDir()
	wlDir, names, err := materializeWordlists(dataDir)
	if err != nil {
		t.Fatalf("materializeWordlists: %v", err)
	}
	if !slices.Equal(names, []string{"api-params.txt", "common-paths.txt"}) {
		t.Fatalf("wordlists=%v, want [api-params.txt common-paths.txt]", names)
	}
	if WordlistsDir() != wlDir {
		t.Fatalf("WordlistsDir()=%q, want %q", WordlistsDir(), wlDir)
	}
	for _, f := range names {
		data, err := os.ReadFile(filepath.Join(wlDir, f))
		if err != nil || len(data) == 0 {
			t.Fatalf("wordlist %s not materialized: err=%v len=%d", f, err, len(data))
		}
	}
	// Prune must preserve wordlists/.
	pruneOldVersions(filepath.Join(dataDir, "toolbox"), "keep1234")
	if _, err := os.Stat(filepath.Join(wlDir, "common-paths.txt")); err != nil {
		t.Fatalf("wordlists dir was pruned: %v", err)
	}
	note := BashPromptNote()
	if !strings.Contains(note, filepath.Join(wlDir, "common-paths.txt")) || !strings.Contains(note, "ffuf") {
		t.Fatalf("BashPromptNote missing wordlists info: %q", note)
	}
}
