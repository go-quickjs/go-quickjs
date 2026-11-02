package regexp

import (
	"strings"
	"unsafe"
)

// MatchAllASCIIInto is generic, instantiated by its caller with struct{}
// alone, so that its code is the caller's package's, laid out after its
// functions, and the package's own code is where it was: the engine's
// package vm, laid out after this one, is sensitive to where its code is.

// MatchAllASCIIInto appends to spans the matches of the pattern, global,
// over an ASCII subject s, one after another from its start, as a global
// replace or match collects them: each from where the last ended, past an
// empty one by one unit, until a match finds nothing or would begin past the
// end. Each is MatchASCIICheckedInto's at its start, its step budget its
// own, but the matcher is borrowed once for them all, not for each, and a
// pattern of nothing but characters, or of one class, is searched for where
// it is. tick is called before each match after the first, as the caller's
// loop counts its turns. A match's error or tick's ends the matches: next
// is then where the match it ended at would have begun, and -1 where the
// matches ran out.
func MatchAllASCIIInto[_ any](re *Regexp, spans []int, s string, check, tick func() error) (out []int, next int, err error) {
	b := unsafe.Slice(unsafe.StringData(s), len(s))
	sticky := re.flags&FlagSticky != 0
	prog := re.prog
	var (
		m    *matcher
		a    *matcherASCII
		lent bool
	)
	first := len(spans)
	if lit := prog.litOnly; lit != nil && lit.isASCII && !sticky && !prog.classOnly {
		// Where the literal is next, from where the last ended.
		t := lit.ascii
		for pos := 0; pos <= len(s); {
			if len(spans) > first {
				if err := tick(); err != nil {
					return spans, pos, err
				}
			}
			at := -1
			if pos <= len(s)-len(t) {
				if s[pos:pos+len(t)] == t {
					at = pos
				} else if k := strings.Index(s[pos+1:], t); k >= 0 {
					at = pos + 1 + k
				}
			}
			if at < 0 {
				break
			}
			pos = at + len(t)
			spans = append(spans, at, pos)
		}
		return spans, -1, nil
	}
	for pos := 0; pos <= len(b); {
		if len(spans) > first {
			if err := tick(); err != nil {
				if a != nil {
					re.giveBackASCII(m, a, lent)
				}
				return spans, pos, err
			}
		}
		// Each match is written where it is appended.
		buf := spans[len(spans):]
		var caps []int
		switch {
		case prog.classOnly:
			caps = re.classMatchASCII(buf, b, pos, sticky)
		case prog.litOnly != nil && !sticky:
			if at := indexUnitsASCII(b, prog.litOnly, pos); at >= 0 {
				caps = append(buf[:0], at, at+len(prog.litOnly.units))
			}
		case nativeBuilt && re.native != nil:
			if caps, err = re.execASCIILent(buf, b, pos, check, sticky); err != nil {
				if a != nil {
					re.giveBackASCII(m, a, lent)
				}
				return spans, pos, err
			}
		default:
			if a == nil {
				// As execASCIILent makes its matcher ready.
				m, lent = re.borrow()
				if a = m.bytes; a == nil {
					a = &matcherASCII{}
					m.bytes = a
				}
				if len(a.caps) != len(m.caps) {
					a.caps = fitInts(a.caps, len(m.caps))
				}
				if len(a.counters) != len(m.counters) {
					a.counters = fitInts(a.counters, len(m.counters))
				}
				if len(a.emptyMarks) != len(m.emptyMarks) {
					a.emptyMarks = fitInts(a.emptyMarks, len(m.emptyMarks))
				}
				a.input = inputASCII{units: b, unicode: re.flags&FlagUnicode != 0}
				a.ready(prog, check)
			}
			a.steps, a.checkAt = 0, checkInterval
			if caps, err = a.search(re, buf, pos, sticky); err != nil {
				if a != nil {
					re.giveBackASCII(m, a, lent)
				}
				return spans, pos, err
			}
		}
		if caps == nil {
			break
		}
		spans = append(spans, caps...)
		if pos = caps[1]; caps[0] == caps[1] {
			pos++
		}
	}
	if a != nil {
		re.giveBackASCII(m, a, lent)
	}
	return spans, -1, nil
}

// MissesASCII reports whether a match of the pattern, neither global nor
// sticky, of the ASCII subject s from start is sure to find nothing, which
// a look at the subject says before any matcher is borrowed: a pattern
// anchored at the start, without the m flag, whose matches each begin with
// one of its first units, tried at the start alone, where the unit there
// is not one of them, or there is none; any other whose required literal
// is not in the subject where a match from start could reach it, or whose
// one first unit, or either of two, is not in it from start -- what the
// search would find first, before it tried any position. A match that
// finds nothing then costs no borrowed matcher, no reset of its captures
// and no attempt that fails at its first instruction: /^ba/ of a subject
// of other units had been a third of exec's work. Patterns of one literal
// or of one class, searched for where they are without a matcher, are
// left to their match. It is generic for the reason MatchAllASCIIInto is.
func MissesASCII[_ any](re *Regexp, s string, start int) bool {
	p := re.prog
	if p.classOnly || p.litOnly != nil || start > len(s) {
		return false
	}
	if !p.anchored {
		if l := p.lit; l != nil && l.isASCII {
			if from := start + l.minOff; from > len(s)-len(l.ascii) || !strings.Contains(s[from:], l.ascii) {
				return true
			}
		}
		if f := p.first; f != nil && (f.sole || f.pair) {
			t := s[start:]
			return strings.IndexByte(t, f.single) < 0 && (!f.pair || strings.IndexByte(t, f.second) < 0)
		}
		return false
	}
	if p.first == nil {
		return false
	}
	if start != 0 || len(s) == 0 {
		return true
	}
	u := s[0]
	return u < 128 && p.first.ascii[u>>6]&(1<<(u&63)) == 0
}
