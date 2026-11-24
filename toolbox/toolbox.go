// Package toolbox embeds pinned static Go security binaries (nuclei, httpx,
// subfinder, katana, ffuf, dalfox) compressed as tar.zst inside the compiled
// artex binary, materializes them once into <dataDir>/toolbox/<sha>/bin, and
// prepends that directory to PATH so every agent Bash call invokes them
// directly without external /usr/local/bin installation.
package toolbox

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
)

//go:embed embedded/*
var embeddedFS embed.FS

//go:embed wordlists/*.txt
var wordlistsFS embed.FS

const (
	archiveName     = "embedded/toolbox.tar.zst"
	archiveHashName = "embedded/toolbox.sha256"
)

var toolDescriptions = map[string]string{
	"nuclei":    "template-based vulnerability scanner (-jsonl -o for ingest_scan_output)",
	"httpx":     "fast HTTP prober and tech/WAF fingerprinter (-json -o for ingest_scan_output)",
	"subfinder": "passive subdomain discovery",
	"katana":    "web crawler and endpoint/JS route extractor",
	"ffuf":      "fast web/parameter fuzzer (-of json -o for ingest_scan_output)",
	"dalfox":    "XSS parameter scanner and verifier",
	"naabu":     "fast TCP port scanner (-s c -json; use in place of nmap when nmap is unavailable or for rapid port sweeps before httpx)",
	"dnsx":      "fast DNS resolver and wildcard detector (use with subfinder + tlsx + gau in place of bbot for multi-stage recon)",
	"tlsx":      "TLS certificate/SAN/CN extractor (discovers hidden hostnames without active fuzzing; part of the embedded bbot replacement)",
	"gau":       "passive archived URL harvester from Wayback/CommonCrawl/OTX (use in place of bbot URL harvesting)",
	"gojq":      "pure-Go JSON processor (drop-in replacement for jq with standard jq filter syntax)",
}

var (
	stateMu            sync.RWMutex
	activeDir          string
	activeTools        []string
	activeWordlistsDir string
	activeWordlists    []string
)

// ActiveTools returns the sorted names of embedded binaries materialized into PATH.
func ActiveTools() []string {
	stateMu.RLock()
	defer stateMu.RUnlock()
	if len(activeTools) == 0 {
		return nil
	}
	out := make([]string, len(activeTools))
	copy(out, activeTools)
	return out
}

// BinDir returns the materialized bin directory, or "" if no embedded bundle is active.
func BinDir() string {
	stateMu.RLock()
	defer stateMu.RUnlock()
	return activeDir
}

// WordlistsDir returns the materialized embedded wordlists directory, or "" if
// Materialize has not run yet.
func WordlistsDir() string {
	stateMu.RLock()
	defer stateMu.RUnlock()
	return activeWordlistsDir
}

// BashPromptNote returns a short description addendum telling agents which
// embedded security binaries and wordlists are ready via Bash, or "" when none are active.
func BashPromptNote() string {
	tools := ActiveTools()
	stateMu.RLock()
	wlDir := activeWordlistsDir
	wlFiles := append([]string(nil), activeWordlists...)
	stateMu.RUnlock()
	if len(tools) == 0 && len(wlFiles) == 0 {
		return ""
	}
	var b strings.Builder
	have := make(map[string]bool, len(tools))
	if len(tools) > 0 {
		b.WriteString("\n\nEmbedded security binaries pre-bundled inside ARTEX and ready in PATH via Bash:\n")
		for _, name := range tools {
			base := strings.TrimSuffix(name, ".exe")
			have[base] = true
			if desc, ok := toolDescriptions[base]; ok {
				fmt.Fprintf(&b, "- %s: %s\n", base, desc)
			} else {
				fmt.Fprintf(&b, "- %s\n", base)
			}
		}
		var pipes []string
		if have["naabu"] {
			pipes = append(pipes, "- Port discovery (replaces nmap): naabu -host <target> -s c -silent | httpx -json -o httpx.jsonl")
		}
		if have["dnsx"] || have["tlsx"] || have["gau"] {
			pipes = append(pipes, "- Multi-stage recon (replaces bbot): subfinder -d <domain> -silent | dnsx -silent | tlsx -san -cn -silent | httpx -json; combine with gau <domain> and katana")
		}
		if have["gojq"] {
			pipes = append(pipes, "- JSON filtering (replaces jq): use gojq with standard jq filter syntax")
		}
		if len(pipes) > 0 {
			b.WriteString("Preferred embedded pipelines (always available even without Docker/nmap/bbot/jq):\n")
			for _, line := range pipes {
				b.WriteString(line + "\n")
			}
		}
	}
	if wlDir != "" && len(wlFiles) > 0 {
		paths := make([]string, len(wlFiles))
		for i, f := range wlFiles {
			paths[i] = filepath.Join(wlDir, f)
		}
		fmt.Fprintf(&b, "\nEmbedded wordlists for ffuf (use instead of missing /usr/share/wordlists): %s\n", strings.Join(paths, ", "))
		fmt.Fprintf(&b, "- Example: ffuf -u https://target/FUZZ -w %s -of json -o ffuf.json\n", filepath.Join(wlDir, "common-paths.txt"))
	}
	return strings.TrimRight(b.String(), "\n")
}

// Materialize extracts the embedded wordlists into <dataDir>/toolbox/wordlists
// and, if an embedded binary bundle is present, extracts it into
// <dataDir>/toolbox/<sha16>/bin, prepends it to PATH, and removes stale bundle
// directories. When built without an embedded binary bundle (e.g. go run / go test),
// wordlists are still materialized and it returns "", nil, nil.
func Materialize(dataDir string) (string, []string, error) {
	if _, _, err := materializeWordlists(dataDir); err != nil {
		return "", nil, err
	}
	// Fast-path: read the 64-byte sidecar hash first so warm starts avoid
	// faulting or copying the ~136 MB embedded archive into the Go heap.
	var tag string
	if hraw, err := embeddedFS.ReadFile(archiveHashName); err == nil {
		h := strings.TrimSpace(string(hraw))
		if len(h) >= 16 {
			tag = h[:16]
		}
	}
	if tag != "" {
		return materializeArchiveReader(dataDir, tag, func() (io.ReadCloser, error) {
			return embeddedFS.Open(archiveName)
		})
	}
	// Fallback when built without toolbox.sha256: stream-hash the archive
	// without loading the entire blob into memory.
	f, err := embeddedFS.Open(archiveName)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil, nil
		}
		return "", nil, fmt.Errorf("open embedded toolbox: %w", err)
	}
	h := sha256.New()
	n, copyErr := io.Copy(h, f)
	_ = f.Close()
	if copyErr != nil {
		return "", nil, fmt.Errorf("hash embedded toolbox: %w", copyErr)
	}
	if n == 0 {
		return "", nil, nil
	}
	tag = hex.EncodeToString(h.Sum(nil)[:8])
	return materializeArchiveReader(dataDir, tag, func() (io.ReadCloser, error) {
		return embeddedFS.Open(archiveName)
	})
}

func materializeWordlists(dataDir string) (string, []string, error) {
	absData, err := filepath.Abs(dataDir)
	if err != nil {
		return "", nil, fmt.Errorf("resolve data dir: %w", err)
	}
	wlDir := filepath.Join(absData, "toolbox", "wordlists")
	if err := os.MkdirAll(wlDir, 0o755); err != nil {
		return "", nil, fmt.Errorf("mkdir toolbox wordlists: %w", err)
	}
	_ = os.Chmod(filepath.Join(absData, "toolbox"), 0o755)
	_ = os.Chmod(wlDir, 0o755)

	entries, err := wordlistsFS.ReadDir("wordlists")
	if err != nil {
		return "", nil, fmt.Errorf("read embedded wordlists: %w", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := wordlistsFS.ReadFile("wordlists/" + e.Name())
		if err != nil {
			return "", nil, fmt.Errorf("read wordlist %s: %w", e.Name(), err)
		}
		dst := filepath.Join(wlDir, e.Name())
		if cur, err := os.ReadFile(dst); err != nil || !bytes.Equal(cur, data) {
			if err := os.WriteFile(dst, data, 0o644); err != nil {
				return "", nil, fmt.Errorf("write wordlist %s: %w", e.Name(), err)
			}
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	stateMu.Lock()
	activeWordlistsDir = wlDir
	activeWordlists = append([]string(nil), names...)
	stateMu.Unlock()
	return wlDir, names, nil
}

func materializeArchive(dataDir string, raw []byte) (string, []string, error) {
	sum := sha256.Sum256(raw)
	tag := hex.EncodeToString(sum[:8])
	return materializeArchiveReader(dataDir, tag, func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(raw)), nil
	})
}

func materializeArchiveReader(dataDir, tag string, openArchive func() (io.ReadCloser, error)) (string, []string, error) {
	absData, err := filepath.Abs(dataDir)
	if err != nil {
		return "", nil, fmt.Errorf("resolve data dir: %w", err)
	}

	rootDir := filepath.Join(absData, "toolbox")
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return "", nil, fmt.Errorf("mkdir toolbox root: %w", err)
	}
	_ = os.Chmod(rootDir, 0o755)

	targetDir := filepath.Join(rootDir, tag)
	binDir := filepath.Join(targetDir, "bin")
	readyPath := filepath.Join(targetDir, ".ready")

	var names []string
	if _, err := os.Stat(readyPath); err == nil {
		names, err = listBinaries(binDir)
		if err != nil {
			return "", nil, err
		}
	} else {
		rc, err := openArchive()
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return "", nil, nil
			}
			return "", nil, fmt.Errorf("open embedded toolbox: %w", err)
		}
		defer rc.Close()

		tmpDir, err := os.MkdirTemp(rootDir, ".extract-*")
		if err != nil {
			return "", nil, fmt.Errorf("mkdir extract temp: %w", err)
		}
		cleanup := true
		defer func() {
			if cleanup {
				_ = os.RemoveAll(tmpDir)
			}
		}()

		tmpBin := filepath.Join(tmpDir, "bin")
		if err := os.MkdirAll(tmpBin, 0o755); err != nil {
			return "", nil, fmt.Errorf("mkdir extract bin: %w", err)
		}
		names, err = extractTarZstReader(rc, tmpBin)
		if err != nil {
			return "", nil, err
		}
		if err := os.WriteFile(filepath.Join(tmpDir, ".ready"), []byte(tag+"\n"), 0o644); err != nil {
			return "", nil, fmt.Errorf("write ready marker: %w", err)
		}
		_ = os.Chmod(tmpDir, 0o755)
		_ = os.Chmod(tmpBin, 0o755)

		if err := os.Rename(tmpDir, targetDir); err != nil {
			if _, statErr := os.Stat(readyPath); statErr != nil {
				return "", nil, fmt.Errorf("commit toolbox dir: %w", err)
			}
			names, err = listBinaries(binDir)
			if err != nil {
				return "", nil, err
			}
		} else {
			cleanup = false
		}
		pruneOldVersions(rootDir, tag)
		debug.FreeOSMemory()
	}

	prependPath(binDir)
	stateMu.Lock()
	activeDir = binDir
	activeTools = append([]string(nil), names...)
	stateMu.Unlock()
	return binDir, names, nil
}

// Check materializes the embedded toolbox and verifies every binary executes
// its version flag cleanly. Used by `artex -check-toolbox`.
func Check(dataDir string) ([]string, error) {
	cleanupDir := ""
	if strings.TrimSpace(dataDir) == "" {
		tmp, err := os.MkdirTemp("", "artex-toolbox-check-*")
		if err != nil {
			return nil, err
		}
		cleanupDir = tmp
		dataDir = tmp
	}
	if cleanupDir != "" {
		defer os.RemoveAll(cleanupDir)
	}

	binDir, names, err := Materialize(dataDir)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, errors.New("no embedded toolbox bundle found in this binary")
	}
	for _, name := range names {
		flag := "-version"
		switch strings.TrimSuffix(name, ".exe") {
		case "ffuf":
			flag = "-V"
		case "dalfox", "gau", "gojq":
			flag = "--version"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		cmd := exec.CommandContext(ctx, filepath.Join(binDir, name), flag)
		out, runErr := cmd.CombinedOutput()
		cancel()
		if runErr != nil {
			return nil, fmt.Errorf("embedded %s %s failed: %w (%s)", name, flag, runErr, strings.TrimSpace(string(out)))
		}
	}
	return names, nil
}

func extractTarZstReader(r io.Reader, destBin string) ([]string, error) {
	zr, err := zstd.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("open zstd toolbox: %w", err)
	}
	defer zr.Close()

	tr := tar.NewReader(zr)
	var names []string
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read toolbox tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		base := filepath.Base(hdr.Name)
		if base == "." || base == ".." || base == "" || strings.ContainsAny(base, `/\`) {
			return nil, fmt.Errorf("invalid toolbox entry name %q", hdr.Name)
		}
		outPath := filepath.Join(destBin, base)
		f, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return nil, fmt.Errorf("create %s: %w", base, err)
		}
		if _, err := io.Copy(f, tr); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("extract %s: %w", base, err)
		}
		if err := f.Close(); err != nil {
			return nil, fmt.Errorf("close %s: %w", base, err)
		}
		_ = os.Chmod(outPath, 0o755)
		names = append(names, base)
	}
	sort.Strings(names)
	return names, nil
}

func listBinaries(binDir string) ([]string, error) {
	entries, err := os.ReadDir(binDir)
	if err != nil {
		return nil, fmt.Errorf("read toolbox bin dir: %w", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func pruneOldVersions(rootDir, keepTag string) {
	entries, err := os.ReadDir(rootDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if name == keepTag || name == "wordlists" || strings.HasPrefix(name, ".extract-") {
			continue
		}
		_ = os.RemoveAll(filepath.Join(rootDir, name))
	}
}

func prependPath(binDir string) {
	sep := string(os.PathListSeparator)
	cur := os.Getenv("PATH")
	for _, p := range strings.Split(cur, sep) {
		if p == binDir {
			return
		}
	}
	if cur == "" {
		_ = os.Setenv("PATH", binDir)
		return
	}
	_ = os.Setenv("PATH", binDir+sep+cur)
}
