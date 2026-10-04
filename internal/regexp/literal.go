package regexp

// A pattern's required literal is a run of characters every match contains,
// at a distance from where the match begins that has bounds: the "Qngr(" in
// (^|[^\\])"\/Qngr\(, which is 0 or 1 characters in. A search looks for the
// literal first and tries only the positions it could have been reached from,
// rather than every position in turn: a subject without it has no match, found
// by one scan, and one with it is tried only where it is. The first units say
// which positions a match can begin at; this says where it can, which is what
// a pattern that can begin with nearly anything needs.
//
// It is worked out for a pattern without the unicode flag, where a character
// is a code unit and the distance a count of them.

// literalOnly is the code units of a pattern that is nothing but characters
// of the Basic Multilingual Plane, none of them a surrogate, or nil: what
// such a pattern matches is that run of units, where it is first found.
func literalOnly(n node) []uint16 {
	var units []uint16
	add := func(n node) bool {
		c, ok := n.(nodeChar)
		if !ok || c.r >= 0x10000 || isSurrogateRune(c.r) {
			return false
		}
		units = append(units, uint16(c.r))
		return true
	}
	switch n := n.(type) {
	case nodeChar:
		if !add(n) {
			return nil
		}
	case nodeSeq:
		for _, item := range n.items {
			if !add(item) {
				return nil
			}
		}
	default:
		return nil
	}
	return units
}

// requiredLit is a pattern's required literal: its code units, and the least
// and the most units between the start of a match and the literal.
type requiredLit struct {
	units          []uint16
	minOff, maxOff int
}

// maxLitOffset bounds the distance a literal is looked for at: past it, the
// positions to try before each occurrence are too many for finding it first
// to save anything.
const maxLitOffset = 64

// requiredOf is the required literal of a pattern whose case is folded when
// fold is set, or nil when it has none worth searching for: none of two
// characters or more at a bounded distance from the start.
func requiredOf(n node, fold bool) *requiredLit {
	var best, run requiredLit
	lo, hi := 0, 0
	end := func() {
		if len(run.units) > len(best.units) {
			best = run
		}
		run = requiredLit{}
	}
	var walk func(n node) bool
	walk = func(n node) bool {
		switch n := n.(type) {
		case nodeSeq:
			for _, item := range n.items {
				if !walk(item) {
					return false
				}
			}
			return true
		case nodeGroup:
			return walk(n.item)
		case nodeChar:
			if n.r < 0x10000 && !isSurrogateRune(n.r) && (!fold || !hasCase(n.r)) {
				if len(run.units) == 0 {
					run.minOff, run.maxOff = lo, hi
				}
				run.units = append(run.units, uint16(n.r))
				lo++
				hi++
				return true
			}
		}
		// Anything else ends the run, and moves the bounds by its width.
		end()
		wlo, whi := width(n)
		lo += wlo
		if whi < 0 || hi+whi > maxLitOffset {
			return false
		}
		hi += whi
		return true
	}
	walk(n)
	end()
	if len(best.units) < 2 || best.maxOff > maxLitOffset {
		return nil
	}
	return &best
}

// width is the least and the most code units a node matches without the
// unicode flag, the most -1 when it has no bound.
func width(n node) (lo, hi int) {
	switch n := n.(type) {
	case nodeEmpty, nodeAssert, nodeLook:
		return 0, 0
	case nodeChar:
		if n.r >= 0x10000 {
			return 2, 2
		}
		return 1, 1
	case nodeAny, nodeClass:
		return 1, 1
	case nodeSeq:
		for _, item := range n.items {
			l, h := width(item)
			lo += l
			if hi >= 0 {
				if h < 0 {
					hi = -1
				} else {
					hi += h
				}
			}
		}
		return lo, hi
	case nodeAlt:
		for i, alt := range n.alts {
			l, h := width(alt)
			if i == 0 || l < lo {
				lo = l
			}
			if i == 0 || hi >= 0 && (h < 0 || h > hi) {
				hi = h
			}
		}
		return lo, hi
	case nodeRepeat:
		l, h := width(n.item)
		lo = l * n.min
		switch {
		case n.max < 0 || h < 0:
			if h == 0 {
				return lo, 0
			}
			return lo, -1
		case h > 0 && n.max > maxLitOffset/h:
			return lo, -1
		}
		return lo, h * n.max
	case nodeGroup:
		return width(n.item)
	case nodeModifier:
		return width(n.item)
	}
	// A backreference matches what its group did, of any length.
	return 0, -1
}

// hasCase reports whether a character folds to another, which a
// case-insensitive literal would then have to match too.
func hasCase(r rune) bool {
	if r < 128 {
		return r|0x20 >= 'a' && r|0x20 <= 'z'
	}
	// Past ASCII, a character may fold to an ASCII one -- the Kelvin sign to k
	// -- or be folded to, so none is taken as caseless.
	return true
}

func isSurrogateRune(r rune) bool { return r >= 0xD800 && r <= 0xDFFF }

// indexUnits is the first position at or after from where lit occurs in s, or
// -1.
func indexUnits(s []uint16, lit []uint16, from int) int {
	first, rest := lit[0], lit[1:]
	for i := from; i <= len(s)-len(lit); i++ {
		if s[i] != first {
			continue
		}
		j := 0
		for j < len(rest) && s[i+1+j] == rest[j] {
			j++
		}
		if j == len(rest) {
			return i
		}
	}
	return -1
}
