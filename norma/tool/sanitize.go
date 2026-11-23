package tool

import (
	"strconv"
	"strings"
)

// SanitizeToolOutput deterministically strips terminal ANSI escape sequences,
// bare carriage-return progress overwrites, and heavyweight HTML blocks
// (<style>, <svg>, <script>) in linear time without regular expressions.
func SanitizeToolOutput(s string) string {
	if !strings.ContainsAny(s, "\x1b\r<") {
		return s
	}
	s = stripEscapeAndWebBlocks(s)
	return collapseCarriageReturns(s)
}

// CollapseRepeatedLines collapses runs of 4 or more consecutive non-empty lines
// whose trimmed text is identical into the first line plus an omission marker.
func CollapseRepeatedLines(s string) string {
	if s == "" || !strings.ContainsRune(s, '\n') {
		return s
	}

	var b strings.Builder
	began := false
	copiedTo := 0

	var runKey string
	var runFirstLine string
	runStart := 0
	runCount := 0
	runTrailingNL := false

	flushRun := func(endOffset int) {
		if runCount >= 4 {
			if !began {
				b.Grow(len(s))
				b.WriteString(s[:runStart])
				began = true
			}
			b.WriteString(runFirstLine)
			b.WriteString("\n... [")
			b.WriteString(strconv.Itoa(runCount - 1))
			b.WriteString(" identical lines omitted]")
			if runTrailingNL {
				b.WriteByte('\n')
			}
			copiedTo = endOffset
		} else if began && endOffset > copiedTo {
			b.WriteString(s[copiedTo:endOffset])
			copiedTo = endOffset
		}
		runKey = ""
		runFirstLine = ""
		runCount = 0
		runTrailingNL = false
	}

	pos := 0
	for pos < len(s) {
		lineStart := pos
		nl := strings.IndexByte(s[pos:], '\n')
		var line string
		var hasNL bool
		var nextPos int
		if nl >= 0 {
			line = s[pos : pos+nl]
			hasNL = true
			nextPos = pos + nl + 1
		} else {
			line = s[pos:]
			hasNL = false
			nextPos = len(s)
		}

		trimmed := strings.TrimSpace(line)
		if trimmed != "" && trimmed == runKey {
			runCount++
			runTrailingNL = hasNL
		} else {
			if runCount > 0 {
				flushRun(lineStart)
			}
			if trimmed != "" {
				runKey = trimmed
				runFirstLine = line
				runStart = lineStart
				runCount = 1
				runTrailingNL = hasNL
			} else if began && nextPos > copiedTo {
				b.WriteString(s[copiedTo:nextPos])
				copiedTo = nextPos
			}
		}
		pos = nextPos
	}
	if runCount > 0 {
		flushRun(len(s))
	}
	if !began {
		return s
	}
	if copiedTo < len(s) {
		b.WriteString(s[copiedTo:])
	}
	return b.String()
}

// CompressToolOutput applies both SanitizeToolOutput and CollapseRepeatedLines.
func CompressToolOutput(s string) string {
	return CollapseRepeatedLines(SanitizeToolOutput(s))
}

func stripEscapeAndWebBlocks(s string) string {
	if !strings.ContainsAny(s, "\x1b<") {
		return s
	}
	var b strings.Builder
	began := false
	copiedTo := 0
	emitSkip := func(start, end int) {
		if !began {
			b.Grow(len(s))
			began = true
		}
		if start > copiedTo {
			b.WriteString(s[copiedTo:start])
		}
		copiedTo = end
	}

	i := 0
	n := len(s)
	for i < n {
		switch s[i] {
		case '\x1b':
			if i+1 < n && s[i+1] == '[' {
				if end := findCSIEnd(s, i+2); end > 0 {
					emitSkip(i, end)
					i = end
					continue
				}
			} else if i+1 < n && s[i+1] == ']' {
				if end := findOSCEnd(s, i+2); end > 0 {
					emitSkip(i, end)
					i = end
					continue
				}
			}
			i++
		case '<':
			if end := findIgnoredHTMLBlockEnd(s, i); end > 0 {
				emitSkip(i, end)
				i = end
				continue
			}
			i++
		default:
			i++
		}
	}

	if !began {
		return s
	}
	if copiedTo < n {
		b.WriteString(s[copiedTo:])
	}
	return b.String()
}

func findCSIEnd(s string, start int) int {
	j := start
	for j < len(s) && s[j] >= 0x20 && s[j] <= 0x3F {
		j++
	}
	if j < len(s) && s[j] >= 0x40 && s[j] <= 0x7E {
		return j + 1
	}
	return 0
}

func findOSCEnd(s string, start int) int {
	limit := len(s)
	if start+4096 < limit {
		limit = start + 4096
	}
	for j := start; j < limit; j++ {
		switch s[j] {
		case '\n', '\r':
			return 0
		case 0x07:
			return j + 1
		case '\x1b':
			if j+1 < len(s) && s[j+1] == '\\' {
				return j + 2
			}
			return 0
		}
	}
	return 0
}

var ignoredHTMLTags = []string{"style", "svg", "script"}

func findIgnoredHTMLBlockEnd(s string, start int) int {
	if start+1 >= len(s) {
		return 0
	}
	for _, tag := range ignoredHTMLTags {
		tagEnd := start + 1 + len(tag)
		if tagEnd >= len(s) || !asciiEqualFold(s[start+1:tagEnd], tag) {
			continue
		}
		sep := s[tagEnd]
		if sep != '>' && sep != '/' && !isASCIIWhitespace(sep) {
			continue
		}
		openClose := strings.IndexByte(s[tagEnd:], '>')
		if openClose < 0 {
			return 0
		}
		openEnd := tagEnd + openClose
		if openEnd > tagEnd && s[openEnd-1] == '/' {
			return openEnd + 1
		}
		if closeEnd := findClosingHTMLTag(s, openEnd+1, tag); closeEnd > 0 {
			return closeEnd
		}
		return 0
	}
	return 0
}

func findClosingHTMLTag(s string, from int, tag string) int {
	needLen := 2 + len(tag) + 1 // "</" + tag + ">"
	for i := from; i+needLen <= len(s); i++ {
		if s[i] != '<' || s[i+1] != '/' {
			continue
		}
		tagEnd := i + 2 + len(tag)
		if !asciiEqualFold(s[i+2:tagEnd], tag) {
			continue
		}
		j := tagEnd
		for j < len(s) && isASCIIWhitespace(s[j]) {
			j++
		}
		if j < len(s) && s[j] == '>' {
			return j + 1
		}
	}
	return 0
}

func asciiEqualFold(a, lowerASCII string) bool {
	if len(a) != len(lowerASCII) {
		return false
	}
	for i := 0; i < len(a); i++ {
		c := a[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != lowerASCII[i] {
			return false
		}
	}
	return true
}

func isASCIIWhitespace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

func collapseCarriageReturns(s string) string {
	if !strings.ContainsRune(s, '\r') {
		return s
	}
	var b strings.Builder
	began := false
	copiedTo := 0

	pos := 0
	for pos < len(s) {
		lineStart := pos
		nl := strings.IndexByte(s[pos:], '\n')
		var lineEnd int
		var suffix string
		var nextPos int
		if nl >= 0 {
			lineEnd = pos + nl
			if lineEnd > lineStart && s[lineEnd-1] == '\r' {
				lineEnd--
				suffix = "\r\n"
			} else {
				suffix = "\n"
			}
			nextPos = pos + nl + 1
		} else {
			lineEnd = len(s)
			nextPos = len(s)
		}

		core := s[lineStart:lineEnd]
		if cr := strings.LastIndexByte(core, '\r'); cr >= 0 {
			if !began {
				b.Grow(len(s))
				began = true
			}
			if lineStart > copiedTo {
				b.WriteString(s[copiedTo:lineStart])
			}
			b.WriteString(core[cr+1:])
			b.WriteString(suffix)
			copiedTo = nextPos
		}
		pos = nextPos
	}

	if !began {
		return s
	}
	if copiedTo < len(s) {
		b.WriteString(s[copiedTo:])
	}
	return b.String()
}
