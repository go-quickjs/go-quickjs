package regexp

import (
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

// execASCII is exec over the bytes of an ASCII string, with a pooled matcher.
func (re *Regexp) execASCII(dst []int, b []byte, start int, check func() error, sticky bool) ([]int, error) {
	if lit := re.prog.litOnly; lit != nil && !sticky {
		if at := indexUnitsASCII(b, lit, start); at >= 0 {
			return append(dst[:0], at, at+len(lit)), nil
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

// indexUnitsASCII is indexUnits over bytes.
func indexUnitsASCII(s []byte, lit []uint16, from int) int {
	first, rest := lit[0], lit[1:]
	for i := from; i <= len(s)-len(lit); i++ {
		if uint16(s[i]) != first {
			continue
		}
		j := 0
		for j < len(rest) && uint16(s[i+1+j]) == rest[j] {
			j++
		}
		if j == len(rest) {
			return i
		}
	}
	return -1
}
