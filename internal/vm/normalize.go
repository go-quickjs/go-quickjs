package vm

import (
	"strings"

	intl "github.com/go-quickjs/go-intl"
	"github.com/go-quickjs/go-quickjs/internal/wtf8"
)

// Unicode normalization, which String.prototype.normalize exposes and the
// case rules of a few languages read combining classes from, is go-intl's.

// normalizer is go-intl's normalizer, built the first time a string is
// normalized or a language's case rules ask for a combining class.
func (r *Runtime) normalizer() (*intl.Normalizer, error) {
	if r.normalizerLoaded == nil {
		n, err := intl.NewNormalizer()
		if err != nil {
			return nil, r.intlInternal()
		}
		r.normalizerLoaded = n
	}
	return r.normalizerLoaded, nil
}

// normalizationForms are String.prototype.normalize's forms by name.
var normalizationForms = map[string]intl.NormalizationForm{
	"NFC": intl.NFC, "NFD": intl.NFD, "NFKC": intl.NFKC, "NFKD": intl.NFKD,
}

// normalizeString puts a string into a normal form. A JavaScript string may
// hold lone surrogates, which go-intl's UTF-8 does not, so the runs between
// them are normalized one at a time and the surrogates kept as they are: a
// surrogate is a starter that combines with nothing, so nothing is reordered
// or composed across it.
func normalizeString(n *intl.Normalizer, s string, form intl.NormalizationForm) string {
	if wtf8.IsASCII(s) {
		return s
	}
	var b strings.Builder
	start := 0
	for i := 0; i < len(s); {
		r, size := wtf8.DecodeRune(s[i:])
		if wtf8.IsSurrogate(r) {
			b.WriteString(n.Normalize(s[start:i], form))
			b.WriteString(s[i : i+size])
			start = i + size
		}
		i += size
	}
	if start == 0 {
		return n.Normalize(s, form)
	}
	b.WriteString(n.Normalize(s[start:], form))
	return b.String()
}

// maxDecomposition is the most UTF-16 code units one code unit of a string
// becomes in any normal form: U+FDFA's compatibility decomposition, 18
// characters (TestNormalizeExpansionBound checks every code point).
const maxDecomposition = 18

// normalizedTooLong reports whether s in form would be longer than
// maxStringLength, before anything is built: normalize's result can be
// eighteen times its string, which a string near the limit would make
// gigabytes of. Each character is decomposed alone (canonically for NFC and
// NFD, by compatibility for NFKC and NFKD) and the lengths summed, which is
// the decomposition forms' length exactly -- reordering keeps it -- and
// at least the composition forms', which only compose what decomposing
// made. A string short enough that no form can take it past the limit is
// not walked.
func normalizedTooLong(n *intl.Normalizer, s *String, form intl.NormalizationForm) bool {
	if s.length <= maxStringLength/maxDecomposition {
		return false
	}
	decompose := intl.NFD
	if form == intl.NFKC || form == intl.NFKD {
		decompose = intl.NFKD
	}
	units := make(map[rune]int)
	total := 0
	str := s.Go()
	for i := 0; i < len(str); {
		r, size := wtf8.DecodeRune(str[i:])
		i += size
		switch {
		case r < 0x80, wtf8.IsSurrogate(r):
			// What has no decomposition, a lone surrogate among it, is
			// itself.
			total++
		default:
			u, ok := units[r]
			if !ok {
				u = utf16Length(n.Normalize(string(r), decompose))
				units[r] = u
			}
			total += u
		}
		if total > maxStringLength {
			return true
		}
	}
	return false
}

// utf16Length is how many UTF-16 code units the UTF-8 string s is.
func utf16Length(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n++
		}
		n++
	}
	return n
}
