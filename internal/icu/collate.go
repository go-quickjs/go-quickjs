package icu

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/go-quickjs/go-quickjs/internal/normalize"
)

// Sorting text.
//
// Comparing two strings is not comparing their characters. A letter counts for
// more than the accent on it, and the accent for more than the case, so two
// words are compared a whole level at a time: the letters first, and the
// accents only if the letters are the same. That is what makes "résumé" sort
// next to "resume" rather than after "z", and what the sensitivity of a
// collator selects between.
//
// Each character carries three weights -- the letter, the accent, the case --
// which are read from the table generated in internal/cldrgen. What is not
// here is what the Unicode algorithm calls contractions and expansions: in
// Czech "ch" is one letter that sorts after "h", and in some languages "æ"
// sorts as though it were "ae". Those are written as several characters and
// compared as several characters.

// weightScale is the room left between two letters of the root order, for the
// letters a language moves in between them.
const weightScale = 4096

// Strength says how much of the difference between two strings counts.
type Strength int

const (
	// Primary compares the letters alone: resume and résumé are the same word.
	Primary Strength = iota + 1
	// Secondary counts the accents as well.
	Secondary
	// Tertiary counts the case as well, which is the whole of the difference.
	Tertiary
)

// weightRun is a stretch of characters whose letter weights run alongside
// them, which is most of a script.
type weightRun struct {
	start  rune
	length int32
	weight int32
}

type collationTable struct {
	runs []weightRun
	// expands holds the characters that sort as several: œ sorts as oe.
	expands map[rune]string
	// marks holds the accents, which count for nothing as letters and for
	// something as accents.
	marks map[rune]int32
	// accents holds the characters that carry an accent or a case weight,
	// which is a few thousand of them.
	accents map[rune][2]uint16
	// ignored holds the characters a collator pays no attention to: the
	// controls, the joiners, the marks that are not letters of their own.
	ignored map[rune]bool
}

var (
	collationOnce  sync.Once
	collationOrder collationTable
)

// order reads the collation table, once, the first time something is sorted.
func order() *collationTable {
	collationOnce.Do(func() {
		text, err := inflate(collationPacked)
		if err != nil {
			return
		}
		sections := strings.Split(text, "\n")
		if len(sections) < 3 {
			return
		}
		collationOrder.accents = map[rune][2]uint16{}
		collationOrder.ignored = map[rune]bool{}
		collationOrder.expands = map[rune]string{}
		collationOrder.marks = map[rune]int32{}

		for _, item := range strings.Split(sections[0], ";") {
			if item == "" {
				continue
			}
			parts := strings.Split(item, ",")
			if len(parts) != 3 {
				continue
			}
			start, err1 := strconv.ParseInt(parts[0], 16, 32)
			length, err2 := strconv.ParseInt(parts[1], 16, 32)
			weight, err3 := strconv.ParseInt(parts[2], 16, 32)
			if err1 != nil || err2 != nil || err3 != nil {
				continue
			}
			collationOrder.runs = append(collationOrder.runs, weightRun{
				start: rune(start), length: int32(length), weight: int32(weight),
			})
		}
		sort.Slice(collationOrder.runs, func(i, j int) bool {
			return collationOrder.runs[i].start < collationOrder.runs[j].start
		})

		for _, item := range strings.Split(sections[1], ";") {
			if item == "" {
				continue
			}
			parts := strings.Split(item, ",")
			if len(parts) != 3 {
				continue
			}
			cp, err1 := strconv.ParseInt(parts[0], 16, 32)
			second, err2 := strconv.ParseInt(parts[1], 16, 32)
			third, err3 := strconv.ParseInt(parts[2], 16, 32)
			if err1 != nil || err2 != nil || err3 != nil {
				continue
			}
			collationOrder.accents[rune(cp)] = [2]uint16{uint16(second), uint16(third)}
		}

		for _, item := range strings.Split(sections[2], ";") {
			if item == "" {
				continue
			}
			cp, err := strconv.ParseInt(item, 16, 32)
			if err != nil {
				continue
			}
			collationOrder.ignored[rune(cp)] = true
		}

		if len(sections) > 3 {
			for _, item := range strings.Split(sections[3], ";") {
				cp, weight, ok := strings.Cut(item, ",")
				if !ok {
					continue
				}
				r, err1 := strconv.ParseInt(cp, 16, 32)
				w, err2 := strconv.ParseInt(weight, 16, 32)
				if err1 != nil || err2 != nil {
					continue
				}
				collationOrder.marks[rune(r)] = int32(w)
			}
		}
		if len(sections) > 4 {
			for _, item := range strings.Split(sections[4], ";") {
				cp, expansion, ok := strings.Cut(item, ",")
				if !ok {
					continue
				}
				r, err := strconv.ParseInt(cp, 16, 32)
				if err != nil {
					continue
				}
				collationOrder.expands[rune(r)] = expansion
			}
		}
	})
	return &collationOrder
}

// weightsOf reports what a character counts for, and whether it counts at all.
func (t *collationTable) weightsOf(r rune) (primary, secondary, tertiary int32, ok bool) {
	if t.ignored[r] {
		return 0, 0, 0, false
	}
	i := sort.Search(len(t.runs), func(i int) bool {
		return t.runs[i].start > r
	}) - 1
	if i < 0 {
		return 0, 0, 0, false
	}
	run := t.runs[i]
	if r >= run.start+rune(run.length) {
		// Beyond every run: a character the table does not know, which sorts
		// after everything it does know, in its own order.
		return int32(r) + 1<<24, 0, 0, true
	}
	primary = run.weight + int32(r-run.start)
	if extra, ok := t.accents[r]; ok {
		return primary, int32(extra[0]), int32(extra[1]), true
	}
	return primary, 0, 0, true
}

// Compare orders two strings as the language does.
//
// strength says how much counts: the letters alone, the accents as well, or
// the case as well. skipAccents compares the letters and the case but not what
// is between them, which is what a collator asking for case sensitivity means.
// upperFirst puts capitals before their small letters, where a language asks
// for that.
// Compare orders two strings as the language does.
//
// strength says how much counts: the letters alone, the accents as well, or
// the case as well. skipAccents compares the letters and the case but not what
// is between them, which is what a collator asking for case sensitivity means.
// upperFirst puts capitals before their small letters, where a language asks
// for that -- and Danish asks for it whether or not the caller does.
func (l *Locale) Compare(a, b string, strength Strength, skipAccents, upperFirst bool) int {
	return l.compare(a, b, strength, skipAccents, upperFirst, false, nil, "")
}

// CompareNumeric is the same, with the runs of digits in the two strings read
// as numbers, so that file9 comes before file10.
func (l *Locale) CompareNumeric(a, b string, strength Strength, skipAccents, upperFirst bool) int {
	return l.compare(a, b, strength, skipAccents, upperFirst, true, nil, "")
}

// ComparePhonebook compares with the German search/phone-book expansions:
// umlauts are written as ae, oe, and ue, and sharp s as ss.
func (l *Locale) ComparePhonebook(a, b string, strength Strength, skipAccents,
	upperFirst, numeric bool) int {
	return l.compare(a, b, strength, skipAccents, upperFirst, numeric, phonebookExpansions, "")
}

// CompareEOR uses the European ordering rules, which are the untailored root
// order rather than the language's ordinary moved letters and contractions.
func (l *Locale) CompareEOR(a, b string, strength Strength, skipAccents,
	upperFirst, numeric bool) int {
	return (*Locale)(nil).compare(a, b, strength, skipAccents, upperFirst, numeric, nil, "")
}

// CompareCollation uses a named CJK ordering, or the locale's ordinary CJK
// ordering when name is "default". The overlay is loaded only when one of
// those collators is first used.
func (l *Locale) CompareCollation(a, b string, strength Strength, skipAccents,
	upperFirst, numeric bool, name string) int {
	return l.compare(a, b, strength, skipAccents, upperFirst, numeric, nil, name)
}

func (l *Locale) compare(a, b string, strength Strength, skipAccents, upperFirst, numeric bool,
	expansions map[rune]string, collation string) int {
	t := order()
	var moved map[rune]int32
	var joined map[string]int32
	shifted := false
	language := ""
	if l != nil {
		language = strings.ToLower(strings.SplitN(l.Tag, "-", 2)[0])
		moved, joined, shifted = l.Tailoring, l.Contractions, l.Shifted
		if l.UpperFirst {
			upperFirst = true
		}
	}
	turkish := language == "tr"
	chinese := language == "zh"
	if turkish {
		// Turkish has two I letters. Its generated alphabet tailoring moves
		// dotless I correctly; dotted capital İ must share lowercase i's
		// primary weight rather than sit after it as an accented root I.
		adjusted := make(map[rune]int32, len(moved)+1)
		for r, weight := range moved {
			adjusted[r] = weight
		}
		if primary, _, _, ok := t.weightsOf('i'); ok {
			adjusted['İ'] = primary * weightScale
		}
		moved = adjusted
	}
	custom := cjkTailoringFor(func() string {
		if l == nil {
			return ""
		}
		return l.Tag
	}(), collation, t)
	ka := t.key(a, moved, joined, shifted, numeric, skipAccents, expansions, custom,
		turkish, chinese)
	kb := t.key(b, moved, joined, shifted, numeric, skipAccents, expansions, custom,
		turkish, chinese)

	if c := compareWeights(ka.primary, kb.primary, false); c != 0 {
		return c
	}
	if strength >= Secondary && !skipAccents {
		if c := compareWeights(ka.secondary, kb.secondary, false); c != 0 {
			return c
		}
	}
	if strength >= Tertiary || skipAccents {
		if c := compareWeights(ka.tertiary, kb.tertiary, upperFirst); c != 0 {
			return c
		}
	}
	// What was passed over decides, where nothing else did.
	return compareWeights(ka.shifted, kb.shifted, false)
}

// sortKey is what a string weighs, level by level.
type sortKey struct {
	primary, secondary, tertiary []int32
	// shifted holds what was passed over, for the language that passes over
	// punctuation: it decides only where everything else was equal.
	shifted []int32
}

// key reads a string into its weights.
//
// A character that sorts as several contributes the weights of all of them --
// œ weighs what oe weighs -- and, where the caller asked for it, a run of
// digits counts as the number it is rather than as its digits.
func (t *collationTable) key(s string, moved map[rune]int32, joined map[string]int32,
	shifted, numeric, skipAccents bool, customExpansions map[rune]string,
	custom *cjkTailoring, turkish, chinese bool) sortKey {
	var out sortKey
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if numeric && r >= '0' && r <= '9' {
			j := i
			for j < len(s) && s[j] >= '0' && s[j] <= '9' {
				j++
			}
			// The run without the zeros in front of it: how long it is says
			// which number is the larger, and then the digits do.
			digits := strings.TrimLeft(s[i:j], "0")
			out.primary = append(out.primary, numberMark, int32(len(digits)))
			for k := 0; k < len(digits); k++ {
				out.primary = append(out.primary, int32(digits[k]-'0'))
			}
			out.secondary = append(out.secondary, 0)
			out.tertiary = append(out.tertiary, 0)
			i = j
			continue
		}
		// A letter this language writes as two or three characters, which is
		// one letter however it is spelled: Czech sorts ch after h.
		if len(joined) > 0 {
			if length, weight, ok := t.contraction(s[i:], joined); ok {
				out.primary = append(out.primary, weight)
				out.secondary = append(out.secondary, 0)
				for _, r := range s[i : i+length] {
					if _, _, third, ok := t.weightsOf(r); ok {
						out.tertiary = append(out.tertiary, third)
					}
				}
				i += length
				continue
			}
		}

		i += size
		// A letter the language moved is a letter of its own there, whatever
		// it is made of: Swedish sorts ä after z rather than as an accented a.
		if _, tailored := moved[r]; tailored {
			t.appendWeights(&out, r, moved, shifted, true, custom, turkish, chinese)
			continue
		}
		// A CJK overlay is already the locale's weight for this character.
		// In particular, Hangul syllables must not be expanded into their
		// root-order jamo before the Korean search order sees them.
		if custom != nil && custom.keepsWhole(r) {
			t.appendWeights(&out, r, moved, shifted, true, custom, turkish, chinese)
			continue
		}
		expansion, customExpansion := customExpansions[r]
		expands := customExpansion
		if !customExpansion {
			expansion, expands = t.expands[r]
		}
		if expands {
			// A character that sorts as several is those several, and then a
			// mark saying it was written as one: ss comes before ß, and 1
			// before ①, though each pair is the same letters.
			for _, e := range expansion {
				t.appendWeights(&out, e, moved, shifted, true, custom, turkish, chinese)
			}
			// A canonical expansion such as a + tilde is merely another
			// spelling of the accented character. When accents are skipped it
			// must not leave the compatibility tie-breaker behind. A phone-book
			// expansion such as ae remains distinct at the tertiary level.
			canonicalAccent := false
			if !customExpansion && skipAccents {
				for _, e := range expansion {
					if _, ok := t.marks[e]; ok {
						canonicalAccent = true
						break
					}
				}
			}
			if _, _, third, ok := t.weightsOf(r); ok && !canonicalAccent {
				out.tertiary = append(out.tertiary, writtenAsOne+third)
			}
			continue
		}
		t.appendWeights(&out, r, moved, shifted, true, custom, turkish, chinese)
	}
	return out
}

var phonebookExpansions = map[rune]string{
	'Ä': "AE", 'ä': "ae",
	'Ö': "OE", 'ö': "oe",
	'Ü': "UE", 'ü': "ue",
	'ẞ': "SS", 'ß': "ss",
}

// HasCollation reports the named orders this compact collation data can
// reproduce for a locale. EOR uses the root order carried here; phone-book
// ordering adds the German expansions above.
func HasCollation(tag, name string) bool {
	language := strings.ToLower(strings.SplitN(tag, "-", 2)[0])
	switch name {
	case "eor":
		return true
	case "phonebk":
		return language == "de"
	case "pinyin", "stroke", "zhuyin":
		return language == "zh"
	case "unihan":
		return language == "zh" || language == "ja" || language == "ko"
	case "searchjl":
		return language == "ko"
	}
	return false
}

// DefaultCollation reports the locale's ordinary named ordering. ICU exposes
// Chinese defaults by name; the other locale defaults are called "default".
func DefaultCollation(tag string) string {
	parts := strings.Split(strings.ToLower(strings.ReplaceAll(tag, "_", "-")), "-")
	if len(parts) == 0 || parts[0] != "zh" {
		return "default"
	}
	// An explicit script wins over the territory. ICU matches zh-Hans-TW as
	// zh-Hans (pinyin), and an uncommon explicit script such as Latn falls
	// back to zh rather than inheriting Taiwan's stroke order.
	for _, part := range parts[1:] {
		if len(part) == 4 {
			if part == "hant" {
				return "stroke"
			}
			return "pinyin"
		}
	}
	for _, part := range parts[1:] {
		switch part {
		case "tw", "hk", "mo":
			return "stroke"
		}
	}
	return "pinyin"
}

// Collations lists the named sort orders this compact collation data can
// reproduce, in the order required by Intl.supportedValuesOf.
func Collations() []string {
	return []string{"eor", "phonebk", "pinyin", "searchjl", "stroke", "unihan", "zhuyin"}
}

// contraction looks for a letter written as two or three characters at the
// front of what is left, whatever case it is written in.
func (t *collationTable) contraction(s string, joined map[string]int32) (int, int32, bool) {
	// The next three characters, and the next two, in lower case.
	var runes [3]rune
	var widths [3]int
	count, at := 0, 0
	for count < 3 && at < len(s) {
		r, size := utf8.DecodeRuneInString(s[at:])
		runes[count] = unicode.ToLower(r)
		widths[count] = size
		at += size
		count++
	}
	for n := count; n >= 2; n-- {
		length := 0
		for i := 0; i < n; i++ {
			length += widths[i]
		}
		if weight, ok := joined[string(runes[:n])]; ok {
			return length, weight, true
		}
	}
	return 0, 0, false
}

// appendWeights adds what one character weighs to a key.
func (t *collationTable) appendWeights(out *sortKey, r rune, moved map[rune]int32,
	shifted, withCase bool, custom *cjkTailoring, turkish, chinese bool) {
	// An accent is not a letter: it counts for nothing at the first level and
	// for itself at the second, which is what makes a decomposed é sort as an
	// accented e rather than as an e and something else.
	if w, ok := t.marks[r]; ok {
		if chinese && len(out.secondary) > 0 {
			// Pinyin's four tone marks precede an unmarked syllable; other
			// accents follow it. Keep the accent in its base character's slot
			// so an accent can sort before the absence of one.
			weight := int32(6) + w
			switch r {
			case '\u0304': // macron, first tone
				weight = 1
			case '\u0301': // acute, second tone
				weight = 2
			case '\u030c': // caron, third tone
				weight = 3
			case '\u0300': // grave, fourth tone
				weight = 4
			}
			last := len(out.secondary) - 1
			if out.secondary[last] == 5 {
				out.secondary[last] = weight
			} else {
				out.secondary = append(out.secondary, weight)
			}
			return
		}
		out.secondary = append(out.secondary, w)
		return
	}
	p, second, third, ok := t.weightsOf(r)
	if !ok {
		return
	}
	if turkish && r == 'İ' {
		second = 0
		_, _, third, _ = t.weightsOf('I')
	}
	if chinese {
		if second == 0 {
			second = 5
		} else {
			second += 6
		}
	}
	weight := scaled(p, r, moved)
	if custom != nil {
		if customWeight, tailored, foldKana := custom.weight(r); tailored {
			weight = customWeight
			if foldKana {
				folded := r
				if expansion, ok := t.expands[r]; ok {
					if expanded, size := utf8.DecodeRuneInString(expansion); size == len(expansion) {
						folded = expanded
					}
				}
				if folded >= 0xff61 && folded <= 0xff9f {
					compat := normalize.String(string(folded), "NFKC")
					if expanded, size := utf8.DecodeRuneInString(compat); size == len(compat) {
						folded = expanded
					}
				}
				if folded >= 0x30a1 && folded <= 0x30f6 {
					folded -= 0x60
				}
				_, _, third, _ = t.weightsOf(folded)
			}
		} else if weight >= custom.anchor {
			weight += custom.span
		}
	}
	if shifted && weight < variableLimit {
		// Punctuation, passed over here and compared at the end, so that it
		// separates two words only when nothing else does.
		out.shifted = append(out.shifted, weight)
		return
	}
	out.primary = append(out.primary, weight)
	out.secondary = append(out.secondary, second)
	if withCase {
		out.tertiary = append(out.tertiary, third)
	}
}

// writtenAsOne is added to the case weight of a character that stands for
// several, so that it comes after the several themselves.
const writtenAsOne = 1 << 20

// variableLimit is where the punctuation ends and the digits begin. A language
// that passes over punctuation passes over everything below this.
var variableLimit = numberMark

// numberMark is where a number sorts among the letters: where the digits
// themselves do, which is what it stands in for.
var numberMark = func() int32 {
	p, _, _, ok := order().weightsOf('0')
	if !ok {
		return 0
	}
	return p * weightScale
}()

func compareWeights(a, b []int32, invert bool) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] == b[i] {
			continue
		}
		less := a[i] < b[i]
		if invert {
			less = !less
		}
		if less {
			return -1
		}
		return 1
	}
	if len(a) != len(b) {
		less := len(a) < len(b)
		if invert {
			less = !less
		}
		if less {
			return -1
		}
		return 1
	}
	return 0
}

// scaled is what a letter weighs in this language: where the root order puts
// it, unless the language moved it.
func scaled(primary int32, r rune, moved map[rune]int32) int32 {
	if w, ok := moved[r]; ok {
		return w
	}
	return primary * weightScale
}

func pick(primary, secondary, tertiary int32, level int) int32 {
	switch level {
	case 0:
		return primary
	case 1:
		return secondary
	default:
		return tertiary
	}
}
