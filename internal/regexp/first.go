package regexp

import "math/bits"

// The first unit of a match.
//
// A search tries each position of the subject in turn, and trying one costs a
// reset of the matcher and a run of the program, however soon it fails. Most
// patterns can only begin with a few characters -- a literal, a class -- and
// at every other position no match can begin, which one look at the unit
// there says. firstUnits is what the pattern can begin with; it is
// conservative, and a pattern it cannot be sure of has none, so that a
// position it refuses is one the matcher would have refused too.

// firstUnits is the code units a match can begin with: the ASCII ones, one
// bit each, and whether any unit past ASCII can begin one.
type firstUnits struct {
	ascii    [2]uint64
	nonASCII bool
	// single is the one ASCII unit a match can begin with when sole is
	// set, as it is when there is just one: a search over bytes looks for it
	// with bytes.IndexByte (see seal). Where there are two, pair is set and
	// they are single and second -- a letter under the i flag, [ab] -- each
	// of which a search over bytes looks for so.
	single, second byte
	sole, pair     bool
}

// seal sets single and sole once the units are all added.
func (f *firstUnits) seal() {
	f.single, f.second, f.sole, f.pair = 0, 0, false, false
	var units [2]byte
	n := 0
	for w := range f.ascii {
		for m := f.ascii[w]; m != 0 && n < 2; m &= m - 1 {
			units[n] = byte(64*w + bits.TrailingZeros64(m))
			n++
		}
	}
	switch bits.OnesCount64(f.ascii[0]) + bits.OnesCount64(f.ascii[1]) {
	case 1:
		f.single, f.sole = units[0], true
	case 2:
		f.single, f.second, f.pair = units[0], units[1], true
	}
}

// skipUnits is the first position from pos on that a match can begin at by
// first, or len(units) when there is none. next is the search's (matcher's
// skipNext), which the search over bytes keeps where it found each of a
// pair of units last.
func skipUnits(units []uint16, pos int, first *firstUnits, next *[2]int) int {
	for pos < len(units) && !first.admits(units[pos]) {
		pos++
	}
	return pos
}

// admits reports whether a match can begin with the unit u.
func (f *firstUnits) admits(u uint16) bool {
	if u < 128 {
		return f.ascii[u>>6]&(1<<(u&63)) != 0
	}
	return f.nonASCII
}

func (f *firstUnits) add(u rune) { f.ascii[u>>6] |= 1 << (u & 63) }

func (f *firstUnits) union(g firstUnits) {
	f.ascii[0] |= g.ascii[0]
	f.ascii[1] |= g.ascii[1]
	f.nonASCII = f.nonASCII || g.nonASCII
}

// firstOf is the first units of a pattern whose case is folded when fold is
// set -- as unicode mode folds it where unicodeFold is -- or nil when it can
// match the empty string, which it can do anywhere, or when it is beyond
// what is worked out here.
func firstOf(n node, fold, unicodeFold bool) *firstUnits {
	f, empty, ok := first(n, fold, unicodeFold)
	if !ok || empty {
		return nil
	}
	f.seal()
	return &f
}

// first is what n can begin with, whether it can match nothing at all --
// when what follows it decides -- and whether it is understood at all.
func first(n node, fold, unicodeFold bool) (f firstUnits, empty, ok bool) {
	switch n := n.(type) {
	case nodeEmpty, nodeAssert, nodeLook:
		// They consume nothing, and what follows them begins the match at
		// the same position: a lookaround narrows what can begin there, but
		// never widens it.
		return f, true, true
	case nodeChar:
		if !fold {
			if n.r < 128 {
				f.add(n.r)
			} else {
				f.nonASCII = true
			}
			return f, false, true
		}
		// A folded character is any of those it is taken for: k is K and,
		// in unicode mode, the Kelvin sign.
		set, ok := foldSetOf(n.r, unicodeFold)
		if !ok {
			return f, false, false
		}
		for _, r := range set {
			if r < 128 {
				f.add(r)
			} else {
				// Its first unit is past ASCII, a surrogate's included.
				f.nonASCII = true
			}
		}
		return f, false, true
	case nodeClass:
		s := n.set
		if fold || s.foldCase || s.negated || s.wordComplement {
			return f, false, false
		}
		for _, rg := range s.ranges {
			for u := rg.lo; u <= rg.hi && u < 128; u++ {
				f.add(u)
			}
			if rg.hi >= 128 {
				f.nonASCII = true
			}
		}
		return f, false, true
	case nodeSeq:
		for _, item := range n.items {
			g, e, ok := first(item, fold, unicodeFold)
			if !ok {
				return f, false, false
			}
			f.union(g)
			if !e {
				return f, false, true
			}
		}
		return f, true, true
	case nodeAlt:
		for _, alt := range n.alts {
			g, e, ok := first(alt, fold, unicodeFold)
			if !ok {
				return f, false, false
			}
			f.union(g)
			empty = empty || e
		}
		return f, empty, true
	case nodeRepeat:
		if n.max == 0 {
			return f, true, true
		}
		f, empty, ok = first(n.item, fold, unicodeFold)
		return f, empty || n.min == 0, ok
	case nodeGroup:
		return first(n.item, fold, unicodeFold)
	case nodeModifier:
		return first(n.item, n.flags&FlagIgnoreCase != 0, unicodeFold)
	}
	// Anything else -- `.`, a backreference -- can begin with nearly
	// anything, or with what only the match knows.
	return f, false, false
}

// anchorStarts turns anchoredStart on; a test turns it off, to compare.
var anchorStarts = true

// anchoredStart reports whether every match of n begins at the start of the
// subject: whether every way through it meets a ^ without the m flag before
// it consumes anything. A search then tries the start alone, where it would
// otherwise try every position of a subject the pattern fails on at once.
func anchoredStart(n node) bool {
	switch n := n.(type) {
	case nodeAssert:
		return n.kind == assertStart && !n.multiline
	case nodeSeq:
		for _, item := range n.items {
			if anchoredStart(item) {
				return true
			}
			switch item.(type) {
			case nodeEmpty, nodeAssert, nodeLook:
				// Consuming nothing, it leaves the ^ still to come at the
				// position the match began.
			default:
				return false
			}
		}
		return false
	case nodeAlt:
		for _, alt := range n.alts {
			if !anchoredStart(alt) {
				return false
			}
		}
		return len(n.alts) > 0
	case nodeGroup:
		return anchoredStart(n.item)
	case nodeModifier:
		return anchoredStart(n.item)
	case nodeRepeat:
		// The first iteration of one that must happen begins the match.
		return n.min > 0 && anchoredStart(n.item)
	}
	return false
}
