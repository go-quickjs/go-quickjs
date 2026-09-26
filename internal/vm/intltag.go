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
