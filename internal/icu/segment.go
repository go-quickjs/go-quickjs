package icu

import (
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Where text may be broken: between characters, between words, and between
// sentences.
//
// A character is not a code point. An e with an accent written as two code
// points is one character; so is a flag, which is two; so is a family emoji,
// which may be seven. What a person calls a character the Unicode algorithm
// calls a grapheme cluster, and finding one is a matter of sorting every code
// point into a class and knowing which pairs of classes may be broken between.
//
// The classes are generated in internal/cldrgen, recovered from a full ICU by
// asking it where it breaks. The pair table that comes with them decides most
// of it; what is written out here are the rules that look further than one
// character -- a full stop joins the letters on either side of it, an emoji
// joined to an emoji is one character, two flag halves are one flag but three
// are two, and a sentence does not end at the full stop of an abbreviation.

// Granularity is what a text is broken into.
type Granularity int

const (
	// Graphemes are what a reader would call characters.
	Graphemes Granularity = iota
	// Words are what double-clicking a word would select.
	Words
	// Sentences end at a full stop that is not an abbreviation.
	Sentences
)

// Breaks reports the byte offsets a text may be broken at, from 0 to its
// length: the pieces between them are its graphemes, words or sentences.
func Breaks(s string, g Granularity) []int {
	if s == "" {
		return nil
	}
	loadSegments()
	var out []int
	switch g {
	case Words:
		out = wordBreaks(s)
	case Sentences:
		out = sentenceBreaks(s)
	default:
		out = graphemeBreaks(s)
	}
	return out
}

// WordLike reports whether a word-sized piece of text is a word rather than
// punctuation or space, which is what a program counting words wants to know.
func WordLike(s string) bool {
	loadSegments()
	t := &segments[Words]
	for _, r := range s {
		if c := int(t.classOf(r)); c < len(wordLike) && wordLike[c] == '1' {
			return true
		}
	}
	return false
}

// segmentTable is one granularity's classes and the pairs that break.
type segmentTable struct {
	runs  []classRun
	pairs []string

	// The classes the rules below are written in terms of, found by looking up
	// characters whose class is not in doubt. A class that no character was
	// offered for stays -1, which nothing is equal to.
	other, letter, hebrew, number, space, katakana         int16
	midLetter, midNum, midNumLet, extendNumLet, quote      int16
	extend, zwj, regional, control, cr, lf                 int16
	lower, upper, aTerm, sTerm, closer, continuation, para int16
}

type classRun struct {
	start rune
	// length is how many code points the run covers, and class what they are.
	length int32
	class  int16
}

var (
	segmentOnce  sync.Once
	segments     [3]segmentTable
	pictographic []classRun
	// wordLike is one character per word class, '1' where a piece of text made
	// of that class is a word rather than a space or a mark.
	wordLike string
)

func (t *segmentTable) classOf(r rune) int16 {
	i := sort.Search(len(t.runs), func(i int) bool { return t.runs[i].start > r }) - 1
	if i < 0 {
		return 0
	}
	run := t.runs[i]
	if r >= run.start+rune(run.length) {
		return 0
	}
	return run.class
}

// breaksBetween is what the table says about a pair of classes, before the
// rules that look further have their say.
func (t *segmentTable) breaksBetween(a, b int16) bool {
	if int(a) >= len(t.pairs) || int(b) >= len(t.pairs[a]) {
		return true
	}
	return t.pairs[a][b] == '1'
}

// classify is a text as its characters, where each one starts, and what class
// each belongs to.
func (t *segmentTable) classify(s string) (runes []rune, offsets []int, classes []int16) {
	n := len(s)
	runes = make([]rune, 0, n)
	offsets = make([]int, 0, n+1)
	classes = make([]int16, 0, n)
	for i, r := range s {
		runes = append(runes, r)
		offsets = append(offsets, i)
		classes = append(classes, t.classOf(r))
	}
	offsets = append(offsets, n)
	return runes, offsets, classes
}

// --- graphemes --------------------------------------------------------------

func graphemeBreaks(s string) []int {
	t := &segments[Graphemes]
	const zwj = rune(0x200D)

	out := make([]int, 1, len(s)/2+2)
	var previous rune
	var previousClass int16
	// How many flag halves have run together, since two make one flag and four
	// make two.
	flags := 0
	// Whether an emoji came before, marks aside: a joiner after one binds it to
	// the emoji that follows.
	picture := false

	for i, r := range s {
		class := t.classOf(r)
		if i == 0 {
			previous, previousClass = r, class
			picture = isPictographic(r)
			if class == t.regional {
				flags = 1
			}
			continue
		}

		split := t.breaksBetween(previousClass, class)
		switch {
		case previous == zwj && picture && isPictographic(r):
			// An emoji joined to an emoji is one character.
			split = false
		case class == t.regional && previousClass == t.regional:
			// Flags go in pairs: a third half starts a second flag.
			split = flags%2 == 0
		}

		switch {
		case split:
			out = append(out, i)
			flags = 0
			picture = isPictographic(r)
		default:
			picture = picture && (class == t.extend || r == zwj) || isPictographic(r)
		}
		if class == t.regional {
			if split {
				flags = 1
			} else {
				flags++
			}
		} else {
			flags = 0
		}
		previous, previousClass = r, class
	}
	return append(out, len(s))
}

func isPictographic(r rune) bool {
	i := sort.Search(len(pictographic), func(i int) bool {
		return pictographic[i].start > r
	}) - 1
	if i < 0 {
		return false
	}
	run := pictographic[i]
	return r < run.start+rune(run.length)
}

// --- words ------------------------------------------------------------------

func wordBreaks(s string) []int {
	t := &segments[Words]
	runes, offsets, classes := t.classify(s)

	// The marks and joiners that hang off a letter belong to it, and the rules
	// read through them as though they were not there.
	skippable := func(c int16) bool {
		return c == t.extend || c == t.zwj
	}
	// The character before and the character after, marks aside, as positions
	// so that the rules can step back twice.
	prev := func(i int) int {
		for i--; i >= 0 && skippable(classes[i]); i-- {
		}
		return i
	}
	next := func(i int) int {
		for i++; i < len(classes); i++ {
			if !skippable(classes[i]) {
				return i
			}
		}
		return -1
	}
	class := func(i int) int16 {
		if i < 0 {
			return -1
		}
		return classes[i]
	}
	letterish := func(c int16) bool { return c == t.letter || c == t.hebrew }
	// The marks that may stand in the middle of a word: between letters, and
	// between digits. A full stop and an apostrophe may do either.
	inWord := func(c int16) bool {
		return c == t.midLetter || c == t.midNumLet || c == t.quote
	}
	inNumber := func(c int16) bool {
		return c == t.midNum || c == t.midNumLet || c == t.quote
	}
	// A line end is a word on its own, marks or no marks.
	alone := func(c int16) bool { return c == t.cr || c == t.lf || c == t.para }

	out := make([]int, 1, len(runes)/4+2)
	flags := 0
	if len(classes) > 0 && classes[0] == t.regional {
		flags = 1
	}
	for i := 1; i < len(runes); i++ {
		right := classes[i]
		if classes[i-1] == t.cr && right == t.lf {
			continue
		}
		if alone(classes[i-1]) || alone(right) {
			out = append(out, offsets[i])
			flags = 0
			continue
		}
		if skippable(right) {
			continue
		}
		p := prev(i)
		if p < 0 {
			// Marks at the very start of a text have nothing to belong to,
			// and so stand apart from what follows.
			out = append(out, offsets[i])
			continue
		}
		left := classes[p]
		split := t.breaksBetween(left, right)
		if cjkRune(runes[p]) && cjkRune(runes[i]) {
			// ICU sends a whole mixed Han/Hiragana/Katakana run through one
			// dictionary engine before deciding its internal boundaries.
			split = false
		}
		if classes[i-1] == t.zwj && isPictographic(runes[i]) {
			// An emoji joined to an emoji is one word, as it is one character.
			split = false
		}

		// A punctuation mark in the middle of a word joins what is on either
		// side of it, when both sides are of a kind: can't, 3.14, 1,000.
		switch {
		case letterish(right) && inWord(left) && letterish(class(prev(p))):
			split = false
		case right == t.number && inNumber(left) && class(prev(p)) == t.number:
			split = false
		case inWord(right) && letterish(left) && letterish(class(next(i))):
			split = false
		case inNumber(right) && left == t.number && class(next(i)) == t.number:
			split = false
		}

		if right == t.regional && left == t.regional {
			split = flags%2 == 0
		}
		if right == t.regional {
			if split {
				flags = 1
			} else {
				flags++
			}
		} else {
			flags = 0
		}

		if split {
			out = append(out, offsets[i])
		}
	}
	return dictionaryWordBreaks(s, append(out, len(s)))
}

// --- sentences --------------------------------------------------------------

func sentenceBreaks(s string) []int {
	t := &segments[Sentences]
	runes, offsets, classes := t.classify(s)

	out := make([]int, 1, 4)
	for i := 1; i < len(runes); i++ {
		left, right := classes[i-1], classes[i]

		// A line end finishes a sentence, and the two halves of a Windows one
		// are not to be parted.
		if left == t.cr && right == t.lf {
			continue
		}
		if left == t.cr || left == t.lf || left == t.para {
			out = append(out, offsets[i])
			continue
		}

		// Whether what came before ends a sentence: a full stop or a question
		// mark, and after it the brackets, spaces and marks that belong to the
		// sentence it ended rather than to the one that follows.
		end, spaced := -1, false
		j := i - 1
		for ; j >= 0 && (classes[j] == t.space || classes[j] == t.extend); j-- {
			spaced = spaced || classes[j] == t.space
		}
		// The marks hang off whatever they follow, brackets and full stop
		// alike, and the rules read straight through them.
		for ; j >= 0 && (classes[j] == t.closer || classes[j] == t.extend); j-- {
		}
		if j >= 0 && (classes[j] == t.aTerm || classes[j] == t.sTerm) {
			end = j
		}
		if end < 0 {
			continue
		}

		// Everything that carries the ended sentence on: its own punctuation,
		// a comma or a colon that goes on with it, and a line end, which is
		// broken after rather than before. A bracket keeps the sentence only
		// while it is still up against the full stop.
		switch right {
		case t.space, t.extend, t.aTerm, t.sTerm, t.continuation, t.cr, t.lf, t.para:
			continue
		case t.closer:
			if !spaced {
				continue
			}
		}
		if classes[end] == t.aTerm && !spaced {
			// A full stop between digits is a decimal point, and one between
			// capitals is an initial: 3.14, U.S.A.
			if right == t.number {
				continue
			}
			if right == t.upper && end > 0 &&
				(classes[end-1] == t.upper || classes[end-1] == t.lower) {
				continue
			}
		}
		if classes[end] == t.aTerm && continuesLower(t, classes, i) {
			// A full stop followed by a small letter was an abbreviation.
			continue
		}
		out = append(out, offsets[i])
	}
	return append(out, len(s))
}

// continuesLower reports whether a small letter follows, with nothing that
// could begin a sentence in the way -- which is what tells "etc. and so on"
// from the end of a sentence.
func continuesLower(t *segmentTable, classes []int16, i int) bool {
	for ; i < len(classes); i++ {
		switch classes[i] {
		case t.lower:
			return true
		case t.upper, t.other, t.cr, t.lf, t.para, t.sTerm, t.aTerm:
			return false
		}
	}
	return false
}

// --- the tables -------------------------------------------------------------

func loadSegments() {
	segmentOnce.Do(func() {
		text, err := inflate(segmentPacked)
		if err != nil {
			return
		}
		sections := strings.Split(text, "\n")
		for g := range segments {
			if g*2+1 >= len(sections) {
				break
			}
			t := &segments[g]
			t.runs = decodeClassRuns(sections[g*2])
			t.pairs = strings.Split(strings.TrimSuffix(sections[g*2+1], ";"), ";")
			t.name()
		}
		if len(sections) > 6 {
			pictographic = decodeClassRuns(sections[6])
		}
		if len(sections) > 7 {
			wordLike = sections[7]
		}
	})
}

// name finds each class the rules speak of by a character that can only belong
// to it. A class no character was offered for is left unequal to everything.
func (t *segmentTable) name() {
	*t = segmentTable{runs: t.runs, pairs: t.pairs}
	for _, field := range []*int16{
		&t.other, &t.letter, &t.hebrew, &t.number, &t.space, &t.katakana,
		&t.midLetter, &t.midNum, &t.midNumLet, &t.extendNumLet, &t.quote,
		&t.extend, &t.zwj, &t.regional, &t.control, &t.cr, &t.lf,
		&t.lower, &t.upper, &t.aTerm, &t.sTerm, &t.closer, &t.continuation,
		&t.para,
	} {
		*field = -1
	}
	find := func(field *int16, r rune) { *field = t.classOf(r) }
	switch {
	case t.classOf('a') == t.classOf('A'):
		// Graphemes and words: a letter is a letter, whatever its case.
		find(&t.letter, 'a')
		find(&t.hebrew, 0x05D0)
		find(&t.number, '0')
		find(&t.space, ' ')
		find(&t.katakana, 0x30A1)
		find(&t.midLetter, ':')
		find(&t.midNum, ',')
		find(&t.midNumLet, '.')
		find(&t.quote, '\'')
		find(&t.extendNumLet, '_')
		find(&t.extend, 0x0300)
		find(&t.zwj, 0x200D)
		find(&t.regional, 0x1F1E6)
		find(&t.control, 0x0001)
		find(&t.para, 0x2029)
		find(&t.cr, '\r')
		find(&t.lf, '\n')
	default:
		// Sentences, where the case of a letter is the whole question.
		find(&t.lower, 'a')
		find(&t.upper, 'A')
		find(&t.other, 0x4E00)
		find(&t.number, '0')
		find(&t.space, ' ')
		find(&t.aTerm, '.')
		find(&t.sTerm, '!')
		find(&t.closer, ')')
		find(&t.continuation, ',')
		find(&t.extend, 0x0300)
		find(&t.para, 0x2029)
		find(&t.cr, '\r')
		find(&t.lf, '\n')
	}
}

func decodeClassRuns(field string) []classRun {
	items := strings.Split(strings.TrimSuffix(field, ";"), ";")
	out := make([]classRun, 0, len(items))
	for _, item := range items {
		if item == "" {
			continue
		}
		parts := strings.Split(item, ",")
		if len(parts) < 2 {
			continue
		}
		start, err1 := strconv.ParseInt(parts[0], 16, 32)
		length, err2 := strconv.ParseInt(parts[1], 16, 32)
		if err1 != nil || err2 != nil {
			continue
		}
		var class int64
		if len(parts) > 2 {
			class, _ = strconv.ParseInt(parts[2], 16, 16)
		}
		out = append(out, classRun{
			start: rune(start), length: int32(length), class: int16(class),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].start < out[j].start })
	return out
}
