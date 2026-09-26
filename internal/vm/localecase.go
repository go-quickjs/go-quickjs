package vm

import (
	"strings"

	intl "github.com/go-quickjs/go-intl"
	"github.com/go-quickjs/go-quickjs/internal/wtf8"
)

// Changing case is not the same in every language.
//
// Turkish has two letter i's -- one with a dot and one without -- and keeps
// them apart in both cases, so "I" lowercases to "ı" rather than to "i" and
// "i" uppercases to "İ". Lithuanian keeps the dot on a lowercase i under an
// accent, writing "i̇̀" where everyone else writes "ì". These are the
// conditional mappings Unicode records against a language rather than against
// a character, and a text that goes through the ordinary ones comes out wrong
// in those languages.

// localeUpper and localeLower change case the way a language does. A language
// with nothing of its own to say is answered by the ordinary mapping.
func localeUpper(s, language string) string {
	switch language {
	case "tr", "az":
		return turkishCase(s, true)
	case "lt":
		return lithuanianUpper(s)
	}
	return caseConvert(s, true)
}

func localeLower(s, language string) string {
	switch language {
	case "tr", "az":
		return turkishCase(s, false)
	case "lt":
		return lithuanianLower(s)
	}
	return caseConvert(s, false)
}

// turkishCase keeps the dotted and the dotless i apart, which is what Turkish
// and Azerbaijani do and no other language does.
func turkishCase(s string, upper bool) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := wtf8.DecodeRune(s[i:])
		rest := s[i+size:]
		switch {
		case upper && r == 'i':
			b.WriteRune(0x0130)
		case upper && r == 0x0130:
			b.WriteRune(0x0130)
		case !upper && r == 0x0130:
			b.WriteRune('i')
		case !upper && r == 'I':
			// A dot written after the I belongs to it, and the lowercase i
			// brings its own. Whatever else stood between the two is kept.
			if at, end := dotAfter(rest); end > 0 {
				b.WriteRune('i')
				b.WriteString(rest[:at])
				i += size + end
				continue
			}
			b.WriteRune(0x0131)
		default:
			b.WriteString(caseConvert(s[i:i+size], upper))
		}
		i += size
	}
	return b.String()
}

// dotAfter finds a combining dot above that belongs to the letter before it:
// where it begins, and where it ends. Marks that sit elsewhere may stand in
// between, and are not part of it.
func dotAfter(s string) (at, end int) {
	for at < len(s) {
		r, size := wtf8.DecodeRune(s[at:])
		if r == 0x0307 {
			return at, at + size
		}
		if class := combiningClass(r); class == 0 || class == 230 {
			// A letter, or a mark that sits where the dot would have.
			return 0, 0
		}
		at += size
	}
	return 0, 0
}

// lithuanianLower keeps the dot on an i that has an accent above it, which
// Lithuanian writes and other languages leave out.
func lithuanianLower(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := wtf8.DecodeRune(s[i:])
		// The letters that lose their dot when an accent is put on them, and
		// the accented forms that already have one.
		switch r {
		case 'I', 'J', 0x012E: // I, J, I with ogonek
			b.WriteString(caseConvert(s[i:i+size], false))
			if moreAbove(s[i+size:]) {
				b.WriteRune(0x0307)
			}
		case 0x00CC: // I with grave
			b.WriteString("i̇̀")
		case 0x00CD: // I with acute
			b.WriteString("i̇́")
		case 0x0128: // I with tilde
			b.WriteString("i̇̃")
		default:
			b.WriteString(caseConvert(s[i:i+size], false))
		}
		i += size
	}
	return b.String()
}

// lithuanianUpper drops the dot that a lowercase i carried, since the capital
// has one of its own.
func lithuanianUpper(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	dotted := false
	for i := 0; i < len(s); {
		r, size := wtf8.DecodeRune(s[i:])
		if r == 0x0307 && dotted {
			i += size
			continue
		}
		b.WriteString(caseConvert(s[i:i+size], true))
		if class := combiningClass(r); class == 0 || class == 230 {
			dotted = softDotted[r]
		}
		i += size
	}
	return b.String()
}

// moreAbove reports whether an accent that sits above the letter follows,
// possibly with marks that sit elsewhere in between.
func moreAbove(s string) bool {
	for at := 0; at < len(s); {
		r, size := wtf8.DecodeRune(s[at:])
		switch combiningClass(r) {
		case 230:
			return true
		case 0:
			return false
		}
		at += size
	}
	return false
}

// softDotted are the letters written with a dot that an accent above takes
// away, which is the set Lithuanian's rule is written against.
var softDotted = map[rune]bool{
	0x0069: true, 0x006A: true, 0x012F: true, 0x0249: true, 0x0268: true,
	0x029D: true, 0x02B2: true, 0x03F3: true, 0x0456: true, 0x0458: true,
	0x1D62: true, 0x1D96: true, 0x1DA4: true, 0x1DA8: true, 0x1E2D: true,
	0x1ECB: true, 0x2071: true, 0x2148: true, 0x2149: true, 0x2C7C: true,
	0x1D422: true, 0x1D423: true, 0x1D456: true, 0x1D457: true,
	0x1D48A: true, 0x1D48B: true, 0x1D4BE: true, 0x1D4BF: true,
	0x1D4F2: true, 0x1D4F3: true, 0x1D526: true, 0x1D527: true,
	0x1D55A: true, 0x1D55B: true, 0x1D58E: true, 0x1D58F: true,
	0x1D5C2: true, 0x1D5C3: true, 0x1D5F6: true, 0x1D5F7: true,
	0x1D62A: true, 0x1D62B: true, 0x1D65E: true, 0x1D65F: true,
	0x1D692: true, 0x1D693: true,
}

// caseLanguage is which language's rules to change case by: the first locale
// asked for, if it is one of the few that have rules of their own.
func (r *Runtime) caseLanguage(v Value) (string, error) {
	tags, err := r.requestedLocales(v)
	if err != nil {
		return "", err
	}
	if len(tags) == 0 {
		return "", nil
	}
	tag, err := intl.ParseLocale(tags[0])
	if err != nil {
		return "", nil
	}
	switch language := tag.Language.String(); language {
	case "tr", "az", "lt", "el":
		return language, nil
	}
	return "", nil
}
