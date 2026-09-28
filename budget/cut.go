// Package budget bounds the text a tool returns. A command's output is kept
// whole in a Store, the caller gets a part no larger than a cap, and reaches
// the rest in steps that are bounded too: Page by position, Search by pattern.
//
// A cap counts characters (runes), a byte that is not valid UTF-8 counting as
// one. A position is a 1-based line number of the log as it was written; the
// fluff-removal steps work on text already taken out and never move it.
package budget

import (
	"strings"
	"unicode/utf8"
)

// Cut returns the start of text, at most limit characters of it, and how many
// characters it left out, so a caller can say what it is not seeing. It cuts
// after the last whole line that fits; when not even the first line fits it
// cuts inside that line, between characters, so that something is returned.
func Cut(text string, limit int) (kept string, omitted int) {
	total := utf8.RuneCountInString(text)
	if total <= limit {
		return text, 0
	}
	kept = text[:prefix(text, limit)]
	if i := strings.LastIndexByte(kept, '\n'); i >= 0 {
		kept = kept[:i+1]
	}
	return kept, total - utf8.RuneCountInString(kept)
}

// prefix returns the length in bytes of the first n characters of s.
func prefix(s string, n int) int {
	i := 0
	for ; n > 0 && i < len(s); n-- {
		_, width := utf8.DecodeRuneInString(s[i:])
		i += width
	}
	return i
}
