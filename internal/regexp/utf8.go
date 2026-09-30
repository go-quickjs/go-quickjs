package regexp

import (
	"sync"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/wtf8"
)

// Matching a Go string's UTF-8 where it is.
//
// JavaScript matches over UTF-16 code units, and UTF-8 has no position between
// the halves of a surrogate pair: a subject read from its UTF-8 bytes is read by
// code point, as the u flag reads a subject, whatever the pattern's flags. For a
// pattern with the u or v flag that is exactly JavaScript's matching; for one
// without, it differs only on characters outside the Basic Multilingual Plane,
// each of which is one character rather than two, so that no pattern matches
// half of one. Positions are byte offsets.
//
// The matcher, matcherUTF8, is the one in exec.go over the bytes of UTF-8,
// generated from that file into exec_utf8.go with the few places patched where
// it takes a unit for a character; its input's reading methods are here. A
// pattern for it is compiled without opGreedy, which takes a unit for a
// character too.

// utf8MatcherPool holds the matchers of finished UTF-8 matches, for the next.
var utf8MatcherPool sync.Pool

// CompileUTF8 compiles a pattern for MatchUTF8Into.
func CompileUTF8(source, flags string) (*Regexp, error) {
	f, err := ParseFlags(flags)
	if err != nil {
		return nil, &SyntaxError{Msg: err.Error(), Pattern: source}
	}
	tree, groups, names, err := parse(source, f)
	if err != nil {
		return nil, err
	}
	if f&(FlagUnicode|FlagUnicodeSets) == 0 {
		tree = joinPairs(tree)
	}
	return &Regexp{
		source:     source,
		flags:      f,
		prog:       compileNodeFor(tree, f, true),
		groupCount: groups,
		groupNames: names,
		utf8:       true,
	}, nil
}

// joinPairs makes each surrogate pair that a pattern without the u flag spells
// out in a row -- an emoji written in it, or \uD83D\uDE00 -- the one character
// they encode, which is what a subject read by code point holds there.
func joinPairs(n node) node {
	switch t := n.(type) {
	case nodeSeq:
		items := make([]node, 0, len(t.items))
		for i := 0; i < len(t.items); i++ {
			item := joinPairs(t.items[i])
			if hi, ok := item.(nodeChar); ok && utf16.IsSurrogate(hi.r) && i+1 < len(t.items) {
				if lo, ok := t.items[i+1].(nodeChar); ok {
					if r := utf16.DecodeRune(hi.r, lo.r); r != utf8.RuneError {
						items = append(items, nodeChar{r: r})
						i++
						continue
					}
				}
			}
			items = append(items, item)
		}
		return nodeSeq{items: items}
	case nodeAlt:
		alts := make([]node, len(t.alts))
		for i, a := range t.alts {
			alts[i] = joinPairs(a)
		}
		return nodeAlt{alts: alts}
	case nodeRepeat:
		t.item = joinPairs(t.item)
		return t
	case nodeGroup:
		t.item = joinPairs(t.item)
		return t
	case nodeModifier:
		t.item = joinPairs(t.item)
		return t
	case nodeLook:
		t.item = joinPairs(t.item)
		return t
	}
	return n
}

// MatchUTF8Into is MatchCheckedInto for a subject matched from its UTF-8 bytes
// where it is, read by code point, with positions in bytes. The pattern must
// have been compiled with CompileUTF8. A lone surrogate is read in the three
// bytes WTF-8 gives it, and a byte that begins no character is read as U+FFFD
// one byte wide. It is safe for goroutines to call at once.
func (re *Regexp) MatchUTF8Into(dst []int, s string, start int, check func() error) ([]int, error) {
	if !re.utf8 {
		panic("regexp: MatchUTF8Into with a pattern not compiled by CompileUTF8")
	}
	if start < 0 {
		start = 0
	}
	if start > len(s) {
		return nil, nil
	}
	return re.execUTF8(dst, unsafe.Slice(unsafe.StringData(s), len(s)), start, check, re.flags&FlagSticky != 0)
}

// execUTF8 is exec over a string's UTF-8 bytes, with a pooled matcher.
func (re *Regexp) execUTF8(dst []int, b []byte, start int, check func() error, sticky bool) ([]int, error) {
	m, _ := utf8MatcherPool.Get().(*matcherUTF8)
	if m == nil {
		m = &matcherUTF8{}
	}
	m.caps = fitInts(m.caps, 2*(re.groupCount+1))
	m.counters = fitInts(m.counters, re.prog.counters)
	m.emptyMarks = fitInts(m.emptyMarks, re.prog.emptyChecks)
	// The subject is read by code point, which is what the unicode flag
	// makes the search do: it steps a whole character at a time.
	m.input = inputUTF8{units: b, unicode: true}
	m.prog, m.in, m.check = re.prog, &m.input, check
	m.steps = 0
	defer func() {
		// The input is not held on to: it would keep the subject alive.
		m.input, m.in, m.check = inputUTF8{}, nil, nil
		utf8MatcherPool.Put(m)
	}()
	return m.search(re, dst, start, sticky)
}

// text is the subject from byte i on, as a string, without copying it.
func (in *inputUTF8) text(i int) string {
	if i >= len(in.units) {
		return ""
	}
	return unsafe.String(&in.units[i], len(in.units)-i)
}

// length returns the number of positions in the input: its bytes.
func (in *inputUTF8) length() int { return len(in.units) }

// at returns the code point starting at byte i and its width in bytes.
func (in *inputUTF8) at(i int) (rune, int) {
	if i < 0 || i >= len(in.units) {
		return -1, 0
	}
	if c := in.units[i]; c < utf8.RuneSelf {
		return rune(c), 1
	}
	return wtf8.DecodeRune(in.text(i))
}

// before returns the code point ending at byte i and its width in bytes.
func (in *inputUTF8) before(i int) (rune, int) {
	if i <= 0 || i > len(in.units) {
		return -1, 0
	}
	if c := in.units[i-1]; c < utf8.RuneSelf {
		return rune(c), 1
	}
	// The character begins at most three continuation bytes back; if what
	// begins there does not end at i, the last byte stands alone.
	for j := i - 1; j >= 0 && j >= i-4; j-- {
		if c := in.units[j]; c < 0x80 || c >= 0xC0 {
			if r, w := wtf8.DecodeRune(unsafe.String(&in.units[j], i-j)); j+w == i {
				return r, w
			}
			break
		}
	}
	return utf8.RuneError, 1
}

// backref matches the text a group captured, bytes start to end, at pos --
// leftwards, ending at pos, where rev is set -- reporting where the match
// leaves the cursor. Case variants may differ in length in UTF-8, so a
// case-insensitive reference is compared a character at a time.
//
// So is one whose bytes differ, unless all it captured is ASCII: a byte that
// begins no character is read as U+FFFD, as is U+FFFD itself, and each is the
// same character as the others though its bytes are not.
func (m *matcherUTF8) backref(start, end, pos int, rev, fold bool) (int, bool) {
	units := m.in.units
	if !fold {
		n := end - start
		at := pos
		if rev {
			at = pos - n
		}
		if at >= 0 && at+n <= len(units) && string(units[at:at+n]) == string(units[start:end]) {
			if rev {
				return at, true
			}
			return at + n, true
		}
		ascii := true
		for _, c := range units[start:end] {
			ascii = ascii && c < utf8.RuneSelf
		}
		if ascii {
			return 0, false
		}
	}
	same := func(x, y rune) bool {
		return x == y || fold && canonical(x, m.prog.unicodeFold) == canonical(y, m.prog.unicodeFold)
	}
	if !rev {
		i, p := start, pos
		for i < end {
			x, wx := m.in.at(i)
			y, wy := m.in.at(p)
			if wy == 0 || !same(x, y) {
				return 0, false
			}
			i, p = i+wx, p+wy
		}
		return p, true
	}
	i, p := end, pos
	for i > start {
		x, wx := m.in.before(i)
		y, wy := m.in.before(p)
		if wy == 0 || !same(x, y) {
			return 0, false
		}
		i, p = i-wx, p-wy
	}
	return p, true
}

// indexUnitsUTF8 stands for indexUnits in the generated matcher, where it is
// never asked: the required literal is looked for only without the unicode
// flag, and a UTF-8 input always has it. Were it asked, from -- the first
// place the literal could be -- claims nothing and passes nothing over.
func indexUnitsUTF8(s []byte, lit []uint16, from int) int {
	if from+len(lit) > len(s) {
		return -1
	}
	return from
}
