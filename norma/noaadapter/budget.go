package noaadapter

import (
	"unicode/utf8"

	"github.com/Autumn-27/norma/noa"
)

const (
	// boundMinRunes is the smallest tool result whose outgoing copy may be
	// trimmed; boundKeepRunes is how much of each end the trimmed copy keeps.
	boundMinRunes  = 512
	boundKeepRunes = 128
	boundMarker    = "\n[noa: tool result shortened for this request only; the full original is retained in the session history]\n"
)

// boundToolCopies trims the largest outgoing tool-result copies in view until
// size(view) fits under target, and returns the new view plus how many copies
// it trimmed.
//
// noa's own valve leaves small results alone, and a model that ignores
// Compress can still push the request over the window with many of them. Only
// the copies in this request's view change: the host history, tool-call
// arguments and the call/result pairing are untouched, and Compress results
// (the archive and navigation panels) are never trimmed. A trimmed copy keeps
// its first and last boundKeepRunes runes around a marker pointing at the
// original kept in history.
func boundToolCopies(view []noa.CoreMessage, target int, size func([]noa.CoreMessage) int) ([]noa.CoreMessage, int) {
	var candidates []int
	for i, m := range view {
		if m.ContentType == noa.CTToolResult && m.ToolName != noa.CompressToolName &&
			utf8.RuneCountInString(m.Text) > boundMinRunes {
			candidates = append(candidates, i)
		}
	}
	if len(candidates) == 0 || size(view) <= target {
		return view, 0
	}
	out := append([]noa.CoreMessage(nil), view...)
	trimmed := 0
	// Oldest-first: an already-trimmed copy re-trims to identical bytes, so a
	// later sweep only mutates the single next-oldest result — the early
	// prefix stays byte-stable across turns (prompt cache). Largest-first
	// would jump around the history and re-trim a different message each turn.
	for _, i := range candidates {
		r := []rune(out[i].Text)
		out[i].Text = string(r[:boundKeepRunes]) + boundMarker + string(r[len(r)-boundKeepRunes:])
		trimmed++
		if size(out) <= target {
			break
		}
	}
	return out, trimmed
}
