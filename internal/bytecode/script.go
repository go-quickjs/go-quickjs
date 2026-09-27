package bytecode

import (
	"sort"
	"unicode/utf8"
)

// Script is the source text a set of functions was compiled from: a script, a
// module, or the code one eval or Function call compiled. Every function in it
// shares it, and a stack trace turns an instruction's position in it into a
// line and a column.
type Script struct {
	// Name is the file or origin name a stack trace shows.
	Name string
	// EvalOrigin is set for the code of an eval or a Function call, and says
	// where it was compiled, as a stack trace shows it: "eval at f
	// (main.js:3:9)".
	EvalOrigin string

	text string
	// lineStarts holds the byte offset at which each line begins.
	lineStarts []int32
}

// NewScript returns the script for a source text.
func NewScript(name, text string) *Script {
	s := &Script{Name: name, text: text, lineStarts: make([]int32, 1, 64)}
	for i := 0; i < len(text); i++ {
		if text[i] == '\n' {
			s.lineStarts = append(s.lineStarts, int32(i+1))
		}
	}
	return s
}

// HasText reports whether the script's text is known, without which no
// position in it is.
func (s *Script) HasText() bool { return s != nil && s.text != "" }

// Offset returns a byte offset as a count of UTF-16 code units, which is how
// JavaScript measures a position in source text.
func (s *Script) Offset(pos int32) int32 {
	if !s.HasText() || pos < 0 || int(pos) > len(s.text) {
		return 0
	}
	n := int32(0)
	for p := 0; p < int(pos); {
		r, size := utf8.DecodeRuneInString(s.text[p:])
		n++
		if r > 0xFFFF {
			n++
		}
		p += size
	}
	return n
}

// Position returns the 1-based line and column of a byte offset, or zeros when
// the text is not known. The column counts UTF-16 code units, as a JavaScript
// position does, so a character outside the Basic Multilingual Plane counts
// twice.
func (s *Script) Position(pos int32) (line, col int32) {
	if !s.HasText() || pos < 0 || int(pos) > len(s.text) {
		return 0, 0
	}
	i := sort.Search(len(s.lineStarts), func(i int) bool { return s.lineStarts[i] > pos }) - 1
	col = 1
	for p := int(s.lineStarts[i]); p < int(pos); {
		r, size := utf8.DecodeRuneInString(s.text[p:])
		if r > 0xFFFF {
			col += 2
		} else {
			col++
		}
		p += size
	}
	return int32(i + 1), col
}
