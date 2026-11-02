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
