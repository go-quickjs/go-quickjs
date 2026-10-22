package regexp

import (
	"bytes"
	"strings"
	"sync"
	"unsafe"
)

// Matching an ASCII Go string where it is.
//
// A string every byte of which is ASCII has its bytes for its code units, so
// it can be matched without being converted to UTF-16 first. The matcher that
// does it, matcherASCII, is the one in exec.go compiled over bytes: it is
// generated from that file into exec_ascii.go, so that the matcher over code
// units -- the engine's -- stays exactly what it was.

// byteMatcherPool holds the matchers of finished ASCII matches, for the next.
var byteMatcherPool sync.Pool

// MatchASCIIInto is MatchCheckedInto for a subject every byte of which is
// ASCII, matched where it is: positions are byte offsets, which in ASCII are
// code units. A subject with a byte outside ASCII must be converted and
// matched with MatchCheckedInto. It is safe for goroutines to call at once.
func (re *Regexp) MatchASCIIInto(dst []int, s string, start int, check func() error) ([]int, error) {
	if start < 0 {
		start = 0
	}
	if start > len(s) {
		return nil, nil
	}
	return re.execASCII(dst, unsafe.Slice(unsafe.StringData(s), len(s)), start, check, re.flags&FlagSticky != 0)
}

// MatchASCIICheckedInto is MatchCheckedInto for a subject every byte of which
// is ASCII, matched where it is, as MatchASCIIInto matches one -- but with the
// state MatchCheckedInto borrows from the pattern, which, as for it, is for
// one match at a time, rather than with a pooled matcher.
func (re *Regexp) MatchASCIICheckedInto(dst []int, s string, start int, check func() error) ([]int, error) {
	if start < 0 {
		start = 0
	}
	if start > len(s) {
		return nil, nil
	}
	return re.execASCIILent(dst, unsafe.Slice(unsafe.StringData(s), len(s)), start, check, re.flags&FlagSticky != 0)
}

// SearchASCIICheckedInto is SearchCheckedInto for an ASCII subject, as
// MatchASCIICheckedInto is MatchCheckedInto for one.
func (re *Regexp) SearchASCIICheckedInto(dst []int, s string, start int, check func() error) ([]int, error) {
	if start < 0 {
		start = 0
	}
	if start > len(s) {
		return nil, nil
	}
	return re.execASCIILent(dst, unsafe.Slice(unsafe.StringData(s), len(s)), start, check, false)
}

// execASCIILent is execASCII with the matcher exec would borrow: its
// matcher over bytes, which keeps a backtracking stack and capture trail of
// its own beside the matcher's, so that a match over bytes neither copies the
// matcher's storage in nor gives it back.
func (re *Regexp) execASCIILent(dst []int, b []byte, start int, check func() error, sticky bool) ([]int, error) {
	if lit := re.prog.litOnly; lit != nil && !sticky {
		if at := indexUnitsASCII(b, lit, start); at >= 0 {
			return append(dst[:0], at, at+len(lit.units)), nil
		}
		return nil, nil
	}
	m, lent := re.borrow()
	a := m.bytes
	if a == nil {
		a = &matcherASCII{}
		m.bytes = a
	}
	// A pooled matcher may have matched another pattern last.
	a.caps = fitInts(a.caps, len(m.caps))
	a.counters = fitInts(a.counters, len(m.counters))
	a.emptyMarks = fitInts(a.emptyMarks, len(m.emptyMarks))
	a.input = inputASCII{units: b, unicode: re.flags&FlagUnicode != 0}
	a.prog, a.in, a.check = re.prog, &a.input, check
	a.steps, a.checkAt = 0, checkInterval
	defer re.giveBackASCII(m, a, lent)
	return a.search(re, dst, start, sticky)
}

// giveBackASCII is giveBack for a match made with m's matcher over bytes,
// which does not hold on to the subject either.
func (re *Regexp) giveBackASCII(m *matcher, a *matcherASCII, lent bool) {
	a.input, a.in, a.check = inputASCII{}, nil, nil
	re.giveBack(m, lent)
}

// execASCII is exec over the bytes of an ASCII string, with a pooled matcher.
func (re *Regexp) execASCII(dst []int, b []byte, start int, check func() error, sticky bool) ([]int, error) {
	if lit := re.prog.litOnly; lit != nil && !sticky {
		if at := indexUnitsASCII(b, lit, start); at >= 0 {
			return append(dst[:0], at, at+len(lit.units)), nil
		}
		return nil, nil
	}
	m, _ := byteMatcherPool.Get().(*matcherASCII)
	if m == nil {
		m = &matcherASCII{}
	}
	m.caps = fitInts(m.caps, 2*(re.groupCount+1))
	m.counters = fitInts(m.counters, re.prog.counters)
	m.emptyMarks = fitInts(m.emptyMarks, re.prog.emptyChecks)
	m.input = inputASCII{units: b, unicode: re.flags&FlagUnicode != 0}
	m.prog, m.in, m.check = re.prog, &m.input, check
	m.steps = 0
	defer func() {
		// The input is not held on to: it would keep the subject alive.
		m.input, m.in, m.check = inputASCII{}, nil, nil
		byteMatcherPool.Put(m)
	}()
	return m.search(re, dst, start, sticky)
}

// hasUnitsASCII is hasUnits over bytes: a run with a unit past ASCII is in
// no ASCII subject.
func hasUnitsASCII(s []byte, lit *literal, pos int) bool {
	n := len(lit.ascii)
	if !lit.isASCII || uint(pos) > uint(len(s)) || n > len(s)-pos {
		return false
	}
	return unsafe.String(unsafe.SliceData(s[pos:]), n) == lit.ascii
}

// indexUnitsASCII is indexUnits over bytes: the literal's bytes are looked
// for with strings.Index, which passes many bytes over at once. A literal
// with a unit past ASCII is in no ASCII subject.
func indexUnitsASCII(s []byte, lit *literal, from int) int {
	n := len(lit.ascii)
	if !lit.isASCII || from > len(s)-n {
		return -1
	}
	str := unsafe.String(unsafe.SliceData(s), len(s))
	// Where the search begins is often where the literal is: it is looked
	// at before strings.Index is called.
	if str[from:from+n] == lit.ascii {
		return from
	}
	if k := strings.Index(str[from+1:], lit.ascii); k >= 0 {
		return from + 1 + k
	}
	return -1
}

// skipUnitsASCII is skipUnits over bytes. A pattern that can begin with one
// unit alone has it looked for with bytes.IndexByte; an ASCII subject has no
// byte past ASCII for first's nonASCII to admit.
func skipUnitsASCII(units []byte, pos int, first *firstUnits, next *[2]int) int {
	if first.sole {
		return indexByteFrom(units, pos, first.single)
	}
	if first.pair {
		return indexPairFrom(units, pos, first, next)
	}
	for pos < len(units) && !first.admits(uint16(units[pos])) {
		pos++
	}
	return pos
}

// indexPairFrom is the first position from pos on of either of first's
// pair of units in s, or len(s): each looked for with bytes.IndexByte, where
// it is found kept in next for the search's later positions, which are
// further on, so that a search reads past each no more than once however
// often the other is found -- the many E a search for /e.../i passes, where
// there is no e.
func indexPairFrom(s []byte, pos int, first *firstUnits, next *[2]int) int {
	if next[0] < pos {
		next[0] = indexByteFrom(s, pos, first.single)
	}
	if next[1] < pos {
		next[1] = indexByteFrom(s, pos, first.second)
	}
	return min(next[0], next[1])
}

// indexByteFrom is the first position of c in s from pos on, or len(s). A
// position that is c is taken without a call of bytes.IndexByte.
func indexByteFrom(s []byte, pos int, c byte) int {
	if pos < len(s) && s[pos] == c {
		return pos
	}
	if k := bytes.IndexByte(s[pos:], c); k >= 0 {
		return pos + k
	}
	return len(s)
}
