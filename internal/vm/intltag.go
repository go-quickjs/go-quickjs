package vm

import (
	intl "github.com/go-quickjs/go-intl"
)

// canonicalSetting is ECMA-402's CanonicalizeUValue: what a Unicode
// extension keyword's value goes by now, for the values that have been
// renamed: "islamicc" is the civil Islamic calendar, and a key set to yes,
// where its data has that alias, is a key set to true. A value that is no
// Unicode locale type is left as it is.
func canonicalSetting(key, value string) string {
	canon, err := intl.NewCanonicalizer(intl.Embedded, intl.CanonicalizeOptions{})
	if err != nil {
		return value
	}
	l, err := canon.Canonicalize("und-u-" + key + "-" + value)
	if err != nil {
		return value
	}
	got, ok := l.Keyword(key)
	switch {
	case !ok:
		return value
	case got == "":
		return "true"
	}
	return got
}

func isVariant(s string) bool {
	if len(s) >= 5 && len(s) <= 8 && allAlphanumeric(s) {
		return true
	}
	return len(s) == 4 && s[0] >= '0' && s[0] <= '9' && allAlphanumeric(s)
}

// asciiLower and asciiUpper change the case of the ASCII letters of s and
// nothing else, which is how a tag, an option's value or a code is
// case-insensitive: strings.ToLower would take U+0130 to "i" and the Kelvin
// sign to "k", so that "fr" followed by U+0130 passed for "fri", where V8
// rejects it.
func asciiLower(s string) string { return asciiCaseString(s, 'A', 'Z') }

func asciiUpper(s string) string { return asciiCaseString(s, 'a', 'z') }

// asciiCaseString flips bit 5 of each byte of s from lo to hi.
func asciiCaseString(s string, lo, hi byte) string {
	i := 0
	for i < len(s) && (s[i] < lo || s[i] > hi) {
		i++
	}
	if i == len(s) {
		return s
	}
	b := []byte(s)
	for ; i < len(b); i++ {
		if c := b[i]; c >= lo && c <= hi {
			b[i] = c ^ 0x20
		}
	}
	return string(b)
}
