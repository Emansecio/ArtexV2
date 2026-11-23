package tool

import (
	"strings"
	"testing"
)

func TestSanitizeToolOutput(t *testing.T) {
	t.Run("clean passthrough", func(t *testing.T) {
		in := "HTTP/1.1 200 OK\nContent-Type: application/json\n{\"ok\":true}\n"
		if got := SanitizeToolOutput(in); got != in {
			t.Fatalf("got %q, want %q", got, in)
		}
	})

	t.Run("strips ANSI CSI and OSC sequences", func(t *testing.T) {
		in := "\x1b]0;scan title\x07\x1b[32mOPEN\x1b[0m 443/tcp\x1b]8;;https://example.com\x1b\\ link\x1b]8;;\x1b\\"
		want := "OPEN 443/tcp link"
		if got := SanitizeToolOutput(in); got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("collapses bare CR progress overwrites and preserves CRLF", func(t *testing.T) {
		in := "header\r\n10%\r50%\r100% done\r\nnext\rfinal"
		want := "header\r\n100% done\r\nfinal"
		if got := SanitizeToolOutput(in); got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("strips style svg and script HTML blocks case-insensitively", func(t *testing.T) {
		in := "<html><HEAD><STYLE media=\"screen\">body{color:red}</style ></head>" +
			"<body><h1>Hi</h1><svg viewBox=\"0 0 10 10\"><path d=\"M0 0\"/></SVG>" +
			"<svg id=\"empty\"/><SCRIPT type=\"text/javascript\">alert(1)</script><p>keep</p></body></html>"
		want := "<html><HEAD></head><body><h1>Hi</h1><p>keep</p></body></html>"
		if got := SanitizeToolOutput(in); got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("preserves unclosed style or angle bracket safely", func(t *testing.T) {
		in := "a < b && <style>unterminated"
		if got := SanitizeToolOutput(in); got != in {
			t.Fatalf("got %q, want %q", got, in)
		}
	})
}

func TestCollapseRepeatedLines(t *testing.T) {
	t.Run("leaves fewer than 4 identical lines unchanged", func(t *testing.T) {
		in := "alpha\n  alpha  \nalpha\nbeta\n"
		if got := CollapseRepeatedLines(in); got != in {
			t.Fatalf("got %q, want %q", got, in)
		}
	})

	t.Run("collapses 4 or more identical non-empty lines", func(t *testing.T) {
		in := "start\n  [404] /nope  \n[404] /nope\n[404] /nope\n[404] /nope\n\n\n\n\n[200] /admin\n"
		want := "start\n  [404] /nope  \n... [3 identical lines omitted]\n\n\n\n\n[200] /admin\n"
		if got := CollapseRepeatedLines(in); got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("Capture uses SanitizeToolOutput to avoid spurious spill", func(t *testing.T) {
		huge := "<style>" + strings.Repeat("a{b:c}", 200) + "</style>result: ok"
		got := Capture(&ToolContext{MaxOutputChars: 100, OutputDir: t.TempDir()}, huge)
		if got != "result: ok" {
			t.Fatalf("got %q, want %q", got, "result: ok")
		}
	})

	t.Run("CompressToolOutput combines sanitize and repeat collapsing", func(t *testing.T) {
		in := "\x1b[31m403\x1b[0m\n403\n403\n403\n403\n"
		want := "403\n... [4 identical lines omitted]\n"
		if got := CompressToolOutput(in); got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
}
