//go:build quickjs_jit

package regexp

import (
	"slices"
	"sync"
	"unicode"
)

// The characters an instruction matches, for native code: runs of code
// points, worked out when the code is made from what the matcher asks --
// unitMatches, runeMatches, a class's contains -- so that the code tests a
// character with comparisons or a table and never calls Go.

// span is a run of code points, lo to hi.
type span struct{ lo, hi int }

// normSpans sorts runs and merges those that touch.
func normSpans(sp []span) []span {
	slices.SortFunc(sp, func(a, b span) int { return a.lo - b.lo })
	out := sp[:0]
	for _, s := range sp {
		if n := len(out); n > 0 && s.lo <= out[n-1].hi+1 {
			out[n-1].hi = max(out[n-1].hi, s.hi)
			continue
		}
		out = append(out, s)
	}
	return out
}

// clipSpans is the part of normalized runs within lo to hi.
func clipSpans(sp []span, lo, hi int) []span {
	var out []span
	for _, s := range sp {
		if s.hi < lo || s.lo > hi {
			continue
		}
		out = append(out, span{max(s.lo, lo), min(s.hi, hi)})
	}
	return out
}

// complementSpans is what normalized runs leave out of lo to hi.
func complementSpans(sp []span, lo, hi int) []span {
	var out []span
	next := lo
	for _, s := range clipSpans(sp, lo, hi) {
		if s.lo > next {
			out = append(out, span{next, s.lo - 1})
		}
		next = s.hi + 1
	}
	if next <= hi {
		out = append(out, span{next, hi})
	}
	return out
}

// spansHave reports whether normalized runs hold c.
func spansHave(sp []span, c int) bool {
	i, _ := slices.BinarySearchFunc(sp, c, func(s span, c int) int {
		switch {
		case s.hi < c:
			return -1
		case s.lo > c:
			return 1
		}
		return 0
	})
	return i < len(sp) && sp[i].lo <= c && c <= sp[i].hi
}

// spansOf128 is the runs of bytes a set of them has.
func spansOf128(set *[128]bool) []span {
	var out []span
	for b := 0; b < 128; b++ {
		if !set[b] {
			continue
		}
		lo := b
		for b+1 < 128 && set[b+1] {
			b++
		}
		out = append(out, span{lo, b})
	}
	return out
}

// bools128 is which bytes of ASCII normalized runs hold.
func bools128(sp []span) (set [128]bool) {
	for _, s := range clipSpans(sp, 0, 127) {
		for c := s.lo; c <= s.hi; c++ {
			set[c] = true
		}
	}
	return set
}

// foldables is every code point with another case, which is what a class
// under the i flag may take in that its ranges do not have. Every one is
// in the first two planes -- the case mappings' code points, and the ones
// with none, in an orbit of SimpleFold's own, such as the sharp s and
// U+0390 -- so those are asked once, the first time a pattern needs it
// (TestNativeFoldables asks every code point).
var foldables = sync.OnceValue(func() []rune {
	var out []rune
	for c := rune(0); c < 0x20000; c++ {
		if unicode.SimpleFold(c) != c {
			out = append(out, c)
		}
	}
	return out
})

// classSpans is the code points up to top that s contains.
func classSpans(s *charSet, unicodeFold bool, top int) []span {
	sp := make([]span, 0, len(s.ranges))
	for _, r := range s.ranges {
		sp = append(sp, span{int(r.lo), int(r.hi)})
	}
	sp = normSpans(sp)
	if s.foldCase {
		// What folds into the ranges: contains, asked of every character
		// with another case that the ranges do not have.
		var extra []span
		for _, c := range foldables() {
			if int(c) > top {
				break
			}
			if !s.rawContains(c) && s.contains(c, unicodeFold) != s.negated {
				extra = append(extra, span{int(c), int(c)})
			}
		}
		sp = normSpans(append(sp, extra...))
	}
	if s.negated {
		return complementSpans(sp, 0, top)
	}
	return clipSpans(sp, 0, top)
}

// lineTerminators is the code points isLineTerminator holds.
var lineTerminators = []span{{'\n', '\n'}, {'\r', '\r'}, {0x2028, 0x2029}}

// wordSpans is the code points isWordChar holds.
func wordSpans(fold bool) []span {
	sp := []span{{'0', '9'}, {'A', 'Z'}, {'_', '_'}, {'a', 'z'}}
	if fold {
		sp = append(sp, span{0x017F, 0x017F}, span{0x212A, 0x212A})
	}
	return sp
}

// itemSpans is the code points up to top a one-character instruction
// matches, as unitMatches and runeMatches have it, or false where they are
// not worked out.
func itemSpans(prog *program, in instr, top int) ([]span, bool) {
	switch in.op {
	case opChar:
		if int(in.r) > top || in.r < 0 {
			return nil, true
		}
		return []span{{int(in.r), int(in.r)}}, true
	case opCharFold:
		if in.arg >= 0 {
			var sp []span
			for _, r := range prog.folds[in.arg] {
				if int(r) <= top {
					sp = append(sp, span{int(r), int(r)})
				}
			}
			return normSpans(sp), true
		}
		if top > 0xFFFF {
			return nil, false
		}
		var sp []span
		for c := 0; c <= top; c++ {
			if canonical(rune(c), prog.unicodeFold) == in.r {
				sp = append(sp, span{c, c})
			}
		}
		return normSpans(sp), true
	case opClass:
		return classSpans(prog.classes[in.arg], prog.unicodeFold, top), true
	case opAny:
		return []span{{0, top}}, true
	case opAnyNotNL:
		return complementSpans(lineTerminators, 0, top), true
	}
	return nil, false
}
