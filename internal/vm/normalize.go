package vm

import (
	"sort"
	"strings"
	"sync"

	"github.com/go-quickjs/go-quickjs/internal/wtf8"
)

// Unicode normalization, which String.prototype.normalize exposes.
//
// Two strings can spell the same text in more than one way: an e with an acute
// accent is one character or two, and a sequence of accents can be written in
// more than one order. Normalizing settles both questions, so that text from
// different sources can be compared.
//
// There are four forms. D takes characters apart and C puts them back together
// again; the K forms additionally replace a character by what it stands for --
// the ligature ﬁ by "fi", the superscript ² by "2" -- which loses the
// distinction rather than merely respelling it.
//
// The tables this works from are in normtables.go.

// Hangul syllables decompose and compose by arithmetic rather than by table:
// the block is laid out so that a syllable's index gives its three parts.
const (
	hangulSBase  = 0xAC00
	hangulLBase  = 0x1100
	hangulVBase  = 0x1161
	hangulTBase  = 0x11A7
	hangulLCount = 19
	hangulVCount = 21
	hangulTCount = 28
	hangulNCount = hangulVCount * hangulTCount
	hangulSCount = hangulLCount * hangulNCount
)

// normalizeString returns s in one of the four normal forms.
//
// A string of nothing but ASCII is already in all four, which is the common
// case and is answered without touching the tables.
func normalizeString(s, form string) string {
	if wtf8.IsASCII(s) {
		return s
	}
	compat := form == "NFKC" || form == "NFKD"
	runes := decomposeString(s, compat)
	reorderCombiningMarks(runes)
	if form == "NFC" || form == "NFKC" {
		runes = composeRunes(runes)
	}
	var b strings.Builder
	b.Grow(len(s))
	buf := make([]byte, 0, 4)
	for _, r := range runes {
		buf = wtf8.AppendRune(buf[:0], r)
		b.Write(buf)
	}
	return b.String()
}

// decomposeString takes every character of s apart, as far as it goes: a
// decomposition may itself decompose, and the K forms apply both kinds.
func decomposeString(s string, compat bool) []rune {
	out := make([]rune, 0, len(s))
	for i := 0; i < len(s); {
		r, size := wtf8.DecodeRune(s[i:])
		i += size
		out = appendDecomposed(out, r, compat)
	}
	return out
}

// appendDecomposed appends the decomposition of one character, recursively.
func appendDecomposed(out []rune, r rune, compat bool) []rune {
	if r >= hangulSBase && r < hangulSBase+hangulSCount {
		i := r - hangulSBase
		out = append(out, hangulLBase+i/hangulNCount, hangulVBase+(i%hangulNCount)/hangulTCount)
		if t := i % hangulTCount; t != 0 {
			out = append(out, hangulTBase+t)
		}
		return out
	}
	tables := normTables()
	if d, ok := tables.canonical[r]; ok {
		for _, c := range d {
			out = appendDecomposed(out, c, compat)
		}
		return out
	}
	if compat {
		if d, ok := tables.compat[r]; ok {
			for _, c := range d {
				out = appendDecomposed(out, c, compat)
			}
			return out
		}
	}
	return append(out, r)
}

// reorderCombiningMarks sorts each run of combining marks into canonical order,
// which is by combining class and otherwise stable: two marks of the same class
// interact, so the order they were written in is the order they keep.
func reorderCombiningMarks(rs []rune) {
	for i := 0; i < len(rs); {
		if combiningClass(rs[i]) == 0 {
			i++
			continue
		}
		j := i
		for j < len(rs) && combiningClass(rs[j]) != 0 {
			j++
		}
		run := rs[i:j]
		sort.SliceStable(run, func(a, b int) bool {
			return combiningClass(run[a]) < combiningClass(run[b])
		})
		i = j
	}
}

// composeRunes puts back together what can be put back together.
//
// A character composes with the starter before it unless something between
// them blocks it: a mark of the same or a higher combining class stands in the
// way, because reordering could not have moved this one past it.
func composeRunes(rs []rune) []rune {
	if len(rs) == 0 {
		return rs
	}
	out := make([]rune, 1, len(rs))
	out[0] = rs[0]
	starter := 0
	lastClass := combiningClass(rs[0])
	if lastClass != 0 {
		// The string begins with a mark, which nothing before it can take.
		lastClass = 256
	}
	for _, c := range rs[1:] {
		class := combiningClass(c)
		if composed := composePair(out[starter], c); composed != 0 &&
			(lastClass < class || lastClass == 0) {
			out[starter] = composed
			continue
		}
		if class == 0 {
			starter = len(out)
		}
		lastClass = class
		out = append(out, c)
	}
	return out
}

// composePair is the character two characters compose to, or zero.
func composePair(a, b rune) rune {
	// Hangul again by arithmetic: a leading jamo takes a vowel, and a syllable
	// with no trailing consonant takes one.
	if l := a - hangulLBase; l >= 0 && l < hangulLCount {
		if v := b - hangulVBase; v >= 0 && v < hangulVCount {
			return hangulSBase + (l*hangulVCount+v)*hangulTCount
		}
	}
	if s := a - hangulSBase; s >= 0 && s < hangulSCount && s%hangulTCount == 0 {
		if t := b - hangulTBase; t > 0 && t < hangulTCount {
			return a + t
		}
	}
	return normTables().composition[[2]rune{a, b}]
}

// combiningClass is the canonical combining class of a character: zero for a
// starter, and higher for a mark the further from the base it sits.
func combiningClass(r rune) int {
	return classIn(normTables().combining, r)
}

// classIn is combiningClass over a table the caller already has, which the
// table-building below needs: it runs before the tables are there to be asked.
func classIn(cc []combiningRange, r rune) int {
	lo, hi := 0, len(cc)
	for lo < hi {
		mid := (lo + hi) / 2
		switch {
		case r < cc[mid].lo:
			hi = mid
		case r > cc[mid].hi:
			lo = mid + 1
		default:
			return cc[mid].class
		}
	}
	return 0
}

// normData is the decoded form of the tables in normtables.go.
type normData struct {
	canonical   map[rune]string
	compat      map[rune]string
	combining   []combiningRange
	composition map[[2]rune]rune
}

type combiningRange struct {
	lo, hi rune
	class  int
}

var (
	normTablesOnce sync.Once
	normTablesData *normData
)

// normTables decodes the tables on first use. Most programs never normalize
// anything, and the decoding costs more than the strings it would save.
func normTables() *normData {
	normTablesOnce.Do(func() {
		d := &normData{
			canonical:   decodeDecompositions(normCanonicalData),
			compat:      decodeDecompositions(normCompatData),
			composition: map[[2]rune]rune{},
		}
		for i := 0; i+2 < len(normCombiningData); {
			lo, n := wtf8.DecodeRune(normCombiningData[i:])
			i += n
			hi, n := wtf8.DecodeRune(normCombiningData[i:])
			i += n
			class, n := wtf8.DecodeRune(normCombiningData[i:])
			i += n
			d.combining = append(d.combining, combiningRange{lo, hi, int(class)})
		}
		excluded := map[rune]bool{}
		for i := 0; i < len(normExclusionData); {
			lo, n := wtf8.DecodeRune(normExclusionData[i:])
			i += n
			hi, n := wtf8.DecodeRune(normExclusionData[i:])
			i += n
			for r := lo; r <= hi; r++ {
				excluded[r] = true
			}
		}
		// The pairs that recompose are the canonical decompositions of exactly
		// two characters, less the ones the database excludes. A singleton
		// never recomposes, and neither does a decomposition that begins with
		// a mark: reordering would have moved the mark away from it.
		for r, dec := range d.canonical {
			if excluded[r] {
				continue
			}
			first, n := wtf8.DecodeRune(dec)
			if n == len(dec) {
				continue
			}
			second, n2 := wtf8.DecodeRune(dec[n:])
			if n+n2 != len(dec) {
				continue
			}
			if classIn(d.combining, first) != 0 {
				continue
			}
			d.composition[[2]rune{first, second}] = r
		}
		normTablesData = d
	})
	return normTablesData
}

// decodeDecompositions reads the record format the tables are written in: a
// character, what it decomposes to, and a NUL.
func decodeDecompositions(data string) map[rune]string {
	out := make(map[rune]string, strings.Count(data, "\x00"))
	for len(data) > 0 {
		end := strings.IndexByte(data, 0)
		rec := data[:end]
		data = data[end+1:]
		r, n := wtf8.DecodeRune(rec)
		out[r] = rec[n:]
	}
	return out
}
