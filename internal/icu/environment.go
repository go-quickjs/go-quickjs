package icu

import (
	"os"
	"strings"
	"sync"
)

// Which language the machine is set to.
//
// A program that formats a date without saying which language to format it in
// means the language of whoever is reading, and on a Unix machine that is what
// the environment says: LC_ALL if it is set, then LC_MESSAGES, then LANG. They
// are written in the older style -- de_DE.UTF-8@euro -- which has to be turned
// into a tag before anything here can use it.
//
// This is what ICU does, and so what every other engine does, which is the
// point: a program run twice in the same shell should not be given two
// different answers by two engines.

// Environment is the language the machine is set to, as a tag. It is empty
// when nothing says, which leaves the caller to choose.
func Environment() string {
	environmentOnce.Do(func() {
		for _, name := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
			if tag := TagFromPosix(os.Getenv(name)); tag != "" {
				environmentTag = tag
				return
			}
		}
	})
	return environmentTag
}

var (
	environmentOnce sync.Once
	environmentTag  string
)

// TagFromPosix turns a locale as a Unix environment writes it into a tag:
// "de_DE.UTF-8@euro" is "de-DE", "C" and "POSIX" are nothing at all.
func TagFromPosix(value string) string {
	// The character set and the modifier say nothing about the language.
	if at := strings.IndexAny(value, ".@"); at >= 0 {
		value = value[:at]
	}
	switch value {
	case "", "C", "POSIX", "c", "posix":
		return ""
	}
	parts := strings.Split(strings.ReplaceAll(value, "_", "-"), "-")
	out := make([]string, 0, len(parts))
	for i, part := range parts {
		switch {
		case i == 0:
			if len(part) < 2 || len(part) > 8 || !onlyLetters(part) {
				return ""
			}
			out = append(out, strings.ToLower(part))
		case len(part) == 4 && onlyLetters(part):
			out = append(out, strings.ToUpper(part[:1])+strings.ToLower(part[1:]))
		case len(part) == 2 && onlyLetters(part), len(part) == 3 && onlyDigits(part):
			out = append(out, strings.ToUpper(part))
		default:
			// Anything else -- a Unix locale may carry a name that is not part
			// of a tag at all -- is left off.
		}
	}
	return strings.Join(out, "-")
}

func onlyLetters(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return len(s) > 0
}

func onlyDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return len(s) > 0
}
