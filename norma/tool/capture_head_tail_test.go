package tool

import (
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCaptureSpillPreservesHeadAndTailWithinCap(t *testing.T) {
	dir := t.TempDir()
	const max = 1000
	tc := &ToolContext{WorkingDir: dir, OutputDir: dir, MaxOutputChars: max}

	headMarker := "HEAD_START_LINE\n"
	middle := strings.Repeat("middle-line-data\n", 500)
	tailMarker := "\nTAIL_END_SUMMARY_PORT_443_OPEN"
	full := headMarker + middle + tailMarker

	out := Capture(tc, full)
	if !strings.Contains(out, "<persisted-output>") {
		t.Fatalf("expected spill notice: %q", out)
	}
	idx := strings.Index(out, "\n\n... <persisted-output>")
	if idx < 0 {
		t.Fatalf("missing persisted-output suffix: %q", out)
	}
	preview := out[:idx]
	if len(preview) > max {
		t.Fatalf("preview len=%d exceeds MaxOutputChars=%d", len(preview), max)
	}
	if !strings.HasPrefix(preview, headMarker) {
		t.Fatalf("preview missing head marker: %q", preview[:80])
	}
	if !strings.HasSuffix(preview, tailMarker) {
		t.Fatalf("preview missing tail marker: %q", preview[len(preview)-80:])
	}
	if !strings.Contains(preview, spillMiddleSep) {
		t.Fatalf("preview missing middle separator: %q", preview)
	}
	ref := extractPointer(t, out)
	disk, err := os.ReadFile(ref)
	if err != nil || string(disk) != full {
		t.Fatalf("disk spill mismatch: err=%v len=%d want=%d", err, len(disk), len(full))
	}
}

func TestCaptureHeadTailUTF8Safe(t *testing.T) {
	dir := t.TempDir()
	tc := &ToolContext{WorkingDir: dir, OutputDir: dir, MaxOutputChars: 777}
	full := strings.Repeat("界", 2000) + "TAIL_端"
	out := Capture(tc, full)
	if !utf8.ValidString(out) {
		t.Fatal("captured output is not valid UTF-8")
	}
	if !strings.Contains(out, "TAIL_端") {
		t.Fatal("multibyte tail lost")
	}
	if got := CaptureOnce(tc, out); got != out {
		t.Fatal("CaptureOnce not idempotent")
	}
}
