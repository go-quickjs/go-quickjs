package icu

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/go-quickjs/go-quickjs/internal/normalize"
)

// ICU's word dictionaries are compressed independently and decoded only when
// text in the corresponding script is segmented. The decoded form remains one
// string plus offsets, avoiding hundreds of thousands of string allocations.
type breakDictionary struct {
	packed   []byte
	weighted bool
	once     sync.Once
	text     string
	starts   []uint32
}

type dictionaryMatch struct {
	bytes, runes int
	cost         uint8
}

type dictionaryMatches struct {
	words  []dictionaryMatch
	prefix int
}

var (
	cjkBreakDictionary     = breakDictionary{packed: cjkDictionaryPacked, weighted: true}
	thaiBreakDictionary    = breakDictionary{packed: thaiDictionaryPacked}
	laoBreakDictionary     = breakDictionary{packed: laoDictionaryPacked}
	khmerBreakDictionary   = breakDictionary{packed: khmerDictionaryPacked}
	burmeseBreakDictionary = breakDictionary{packed: burmeseDictionaryPacked}
)

func (d *breakDictionary) load() {
	d.once.Do(func() {
		text, err := inflate(d.packed)
		if err != nil || text == "" {
			return
		}
		d.text = text
		d.starts = append(d.starts, 0)
		for i := 0; i < len(text); i++ {
			if text[i] == '\n' && i+1 < len(text) {
				d.starts = append(d.starts, uint32(i+1))
			}
		}
	})
}

func (d *breakDictionary) entry(i int) string {
	start := int(d.starts[i])
	end := len(d.text)
	if i+1 < len(d.starts) {
		end = int(d.starts[i+1]) - 1
	}
	line := d.text[start:end]
	if d.weighted {
		word, _, _ := strings.Cut(line, "\t")
		return word
	}
	return line
}

func (d *breakDictionary) cost(i int) uint8 {
	start := int(d.starts[i])
	end := len(d.text)
	if i+1 < len(d.starts) {
		end = int(d.starts[i+1]) - 1
	}
	_, value, _ := strings.Cut(d.text[start:end], "\t")
	cost, _ := strconv.ParseUint(value, 10, 8)
	return uint8(cost)
}

// matches returns dictionary words at the beginning of text in increasing
// length order, and the longest prefix shared with any dictionary word.
func (d *breakDictionary) matches(text string, maxRunes, maxMatches int) dictionaryMatches {
	d.load()
	if len(d.starts) == 0 || text == "" {
		return dictionaryMatches{}
	}
	var out dictionaryMatches
	end, runes := 0, 0
	for _, r := range text {
		end += utf8.RuneLen(r)
		runes++
		if maxRunes > 0 && runes > maxRunes {
			break
		}
		prefix := text[:end]
		i := sort.Search(len(d.starts), func(i int) bool {
			return d.entry(i) >= prefix
		})
		if i == len(d.starts) {
			break
		}
		word := d.entry(i)
		if !strings.HasPrefix(word, prefix) {
			break
		}
		out.prefix = runes
		if word == prefix && (maxMatches <= 0 || len(out.words) < maxMatches) {
			var cost uint8
			if d.weighted {
				cost = d.cost(i)
			}
			out.words = append(out.words, dictionaryMatch{bytes: end, runes: runes, cost: cost})
		}
	}
	return out
}

type dictionaryKind uint8

const (
	noDictionary dictionaryKind = iota
	cjkDictionary
	thaiDictionary
	laoDictionary
	khmerDictionary
	burmeseDictionary
)

func dictionaryForRune(r rune) dictionaryKind {
	switch {
	case cjkRune(r):
		return cjkDictionary
	case unicode.Is(unicode.Thai, r):
		return thaiDictionary
	case unicode.Is(unicode.Lao, r):
		return laoDictionary
	case unicode.Is(unicode.Khmer, r):
		return khmerDictionary
	case unicode.Is(unicode.Myanmar, r):
		return burmeseDictionary
	default:
		return noDictionary
	}
}

func cjkRune(r rune) bool {
	return unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) || r == 0x30fc || r == 0xff70 ||
		r == 0xff9e || r == 0xff9f
}

// dictionaryWordBreaks refines the script runs left whole by the Unicode word
// rules. The returned positions remain byte offsets, as Breaks promises.
func dictionaryWordBreaks(s string, base []int) []int {
	out := make([]int, 1, len(base)+8)
	for i := 1; i < len(base); i++ {
		start, end := base[i-1], base[i]
		piece := s[start:end]
		kind := noDictionary
		for _, r := range piece {
			if kind = dictionaryForRune(r); kind != noDictionary {
				break
			}
		}
		var internal []int
		switch kind {
		case cjkDictionary:
			internal = cjkWordBreaks(piece)
		case thaiDictionary, laoDictionary, khmerDictionary, burmeseDictionary:
			internal = southeastAsianWordBreaks(piece, kind)
		}
		for _, at := range internal {
			if at > 0 && at < len(piece) {
				out = append(out, start+at)
			}
		}
		out = append(out, end)
	}
	return out
}

func cjkWordBreaks(text string) []int {
	normalized, inputMap := normalize.NFKCWithOffsets(text)
	runes := []rune(normalized)
	if len(runes) < 2 {
		return nil
	}
	offsets := runeByteOffsets(normalized, len(runes))
	best := make([]uint32, len(runes)+1)
	previous := make([]int, len(runes)+1)
	for i := 1; i < len(best); i++ {
		best[i] = math.MaxUint32
		previous[i] = -1
	}

	wasKatakana := false
	for i, r := range runes {
		if best[i] == math.MaxUint32 {
			continue
		}
		matches := cjkBreakDictionary.matches(normalized[offsets[i]:], 20, 0).words
		if len(matches) == 0 || matches[0].runes != 1 {
			matches = append(matches, dictionaryMatch{runes: 1, cost: 255})
		}
		for _, match := range matches {
			end := i + match.runes
			if end <= len(runes) && best[i]+uint32(match.cost) < best[end] {
				best[end] = best[i] + uint32(match.cost)
				previous[end] = i
			}
		}

		katakana := isKatakana(r)
		if katakana && !wasKatakana {
			length := 1
			for i+length < len(runes) && length < 20 && isKatakana(runes[i+length]) {
				length++
			}
			if length < 20 {
				cost := katakanaCost(length)
				if best[i]+cost < best[i+length] {
					best[i+length] = best[i] + cost
					previous[i+length] = i
				}
			}
		}
		wasKatakana = katakana
	}

	var reversed []int
	for at := len(runes); at > 0 && previous[at] >= 0; at = previous[at] {
		if at < len(runes) {
			original := inputMap[at]
			if original > 0 && original < len(text) &&
				(len(reversed) == 0 || reversed[len(reversed)-1] != original) {
				reversed = append(reversed, original)
			}
		}
	}
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	return reversed
}

func runeByteOffsets(text string, count int) []int {
	offsets := make([]int, 0, count+1)
	for i := range text {
		offsets = append(offsets, i)
	}
	return append(offsets, len(text))
}

func isKatakana(r rune) bool {
	return r >= 0x30a1 && r <= 0x30fe && r != 0x30fb || r >= 0xff66 && r <= 0xff9f
}

func katakanaCost(length int) uint32 {
	costs := [...]uint32{8192, 984, 408, 240, 204, 252, 300, 372, 480}
	if length >= len(costs) {
		return 8192
	}
	return costs[length]
}

func southeastAsianWordBreaks(text string, kind dictionaryKind) []int {
	runes := []rune(text)
	if len(runes) < 4 {
		return nil
	}
	offsets := runeByteOffsets(text, len(runes))
	dictionary := southeastAsianDictionary(kind)
	matchesAt := func(at int) dictionaryMatches {
		if at >= len(runes) {
			return dictionaryMatches{}
		}
		return dictionary.matches(text[offsets[at]:], len(runes)-at, 20)
	}

	var breaks []int
	for current := 0; current < len(runes); {
		length := chooseSoutheastAsianWord(current, len(runes), matchesAt)
		rootLength := length
		next := current + length
		if next < len(runes) && rootLength < 3 {
			following := matchesAt(next)
			if len(following.words) == 0 && (length == 0 || following.prefix < 3) {
				for next < len(runes) {
					next++
					length++
					if next < len(runes) && southeastAsianEnd(kind, runes[next-1]) &&
						southeastAsianBegin(kind, runes[next]) && len(matchesAt(next).words) > 0 {
						break
					}
				}
			}
		}
		for current+length < len(runes) && southeastAsianMark(kind, runes[current+length]) {
			length++
		}
		if kind == thaiDictionary && current+length < len(runes) && length > 0 && len(matchesAt(current+length).words) == 0 {
			at := current + length
			if runes[at] == 0x0e2f && (at == 0 || !thaiSuffix(runes[at-1])) {
				length++
				at++
			}
			if at < len(runes) && runes[at] == 0x0e46 && (at == 0 || runes[at-1] != 0x0e46) {
				length++
			}
		}
		if length <= 0 {
			length = 1
		}
		current += length
		if current < len(runes) {
			breaks = append(breaks, offsets[current])
		}
	}
	return breaks
}

func chooseSoutheastAsianWord(start, end int, matchesAt func(int) dictionaryMatches) int {
	first := matchesAt(start).words
	if len(first) == 0 {
		return 0
	}
	if len(first) == 1 {
		return first[0].runes
	}
	marked := first[len(first)-1].runes
	if start+marked >= end {
		return marked
	}
	for i := len(first) - 1; i >= 0; i-- {
		secondStart := start + first[i].runes
		second := matchesAt(secondStart).words
		if len(second) == 0 {
			continue
		}
		marked = first[i].runes
		if secondStart+second[len(second)-1].runes >= end {
			return marked
		}
		for j := len(second) - 1; j >= 0; j-- {
			if len(matchesAt(secondStart+second[j].runes).words) > 0 {
				return marked
			}
		}
	}
	return marked
}

func southeastAsianDictionary(kind dictionaryKind) *breakDictionary {
	switch kind {
	case thaiDictionary:
		return &thaiBreakDictionary
	case laoDictionary:
		return &laoBreakDictionary
	case khmerDictionary:
		return &khmerBreakDictionary
	default:
		return &burmeseBreakDictionary
	}
}

func southeastAsianMark(kind dictionaryKind, r rune) bool {
	return southeastAsianScript(kind, r) && unicode.Is(unicode.M, r) || r == ' '
}

func southeastAsianBegin(kind dictionaryKind, r rune) bool {
	switch kind {
	case thaiDictionary:
		return r >= 0x0e01 && r <= 0x0e2e || r >= 0x0e40 && r <= 0x0e44
	case laoDictionary:
		return r >= 0x0e81 && r <= 0x0eae || r >= 0x0edc && r <= 0x0edd || r >= 0x0ec0 && r <= 0x0ec4
	case khmerDictionary:
		return r >= 0x1780 && r <= 0x17b3
	default:
		return r >= 0x1000 && r <= 0x102a
	}
}

func southeastAsianEnd(kind dictionaryKind, r rune) bool {
	if !southeastAsianScript(kind, r) {
		return false
	}
	switch kind {
	case thaiDictionary:
		return r != 0x0e31 && (r < 0x0e40 || r > 0x0e44)
	case laoDictionary:
		return r < 0x0ec0 || r > 0x0ec4
	case khmerDictionary:
		return r != 0x17d2
	default:
		return true
	}
}

func southeastAsianScript(kind dictionaryKind, r rune) bool {
	switch kind {
	case thaiDictionary:
		return r >= 0x0e00 && r <= 0x0e7f
	case laoDictionary:
		return r >= 0x0e80 && r <= 0x0eff
	case khmerDictionary:
		return r >= 0x1780 && r <= 0x17ff || r >= 0x19e0 && r <= 0x19ff
	default:
		return r >= 0x1000 && r <= 0x109f || r >= 0xa9e0 && r <= 0xa9ff || r >= 0xaa60 && r <= 0xaa7f
	}
}

func thaiSuffix(r rune) bool { return r == 0x0e2f || r == 0x0e46 }
