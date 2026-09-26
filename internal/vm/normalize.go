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
