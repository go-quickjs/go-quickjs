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
	// Referrer is what an import() in this code is resolved against, as the
	// module loader's referrer: a module's specifier, a script's name, empty
	// for code with no name ("<eval>"), and for the code of an eval or a
	// Function call, the referrer of the code that called it.
	Referrer string

	text string
	// lineStarts holds the byte offset at which each line begins.
	lineStarts []int32
	// lineOffset and columnOffset place the text within a larger file, as
	// node:vm's options of those names do: every line is lineOffset further
	// down, and the first is columnOffset further right.
	lineOffset, columnOffset int32
}

// SetOffset places the script's text within a larger file: its first line is
// line lineOffset+1 and starts at column columnOffset+1.
func (s *Script) SetOffset(lineOffset, columnOffset int32) {
	s.lineOffset, s.columnOffset = lineOffset, columnOffset
}

// NewScript returns the script for a source text.
func NewScript(name, text string) *Script {
	s := &Script{Name: name, Referrer: name, text: text, lineStarts: make([]int32, 1, 64)}
	if name == "<eval>" {
		s.Referrer = ""
	}
	for i := 0; i < len(text); i++ {
		if text[i] == '\n' {
			s.lineStarts = append(s.lineStarts, int32(i+1))
		}
	}
	return s
}

// Offsets are where the script's text is placed within a larger file: how
// many lines down it starts, and how many columns in.
func (s *Script) Offsets() (line, col int32) { return s.lineOffset, s.columnOffset }

// Text is the script's source text, or empty when it is not known.
func (s *Script) Text() string { return s.text }

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
	if i == 0 {
		col += s.columnOffset
	}
	return int32(i+1) + s.lineOffset, col
}
