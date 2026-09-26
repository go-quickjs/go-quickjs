package vm

import (
	"testing"

	intl "github.com/go-quickjs/go-intl"
)

// ICU's fallback locale, which a "C" or "POSIX" environment gives, is
// "en-US" to V8 (Isolate::DefaultLocale); every other locale is ICU's own.
// A Linux run with no locale set resolved Intl.DurationFormat("lkt") to
// "en-US-u-va-posix", where Node says "en-US".
func TestDefaultLocale(t *testing.T) {
	for tag, want := range map[string]string{
		"en-US-u-va-posix": "en-US",
		"de-DE":            "de-DE",
		"en-GB":            "en-GB",
		"de-DE-u-va-posix": "de-DE-u-va-posix",
	} {
		loc, err := intl.ParseLocale(tag)
		if err != nil {
			t.Fatal(err)
		}
		if got := defaultLocale(loc); got != want {
			t.Errorf("%s: %q, want %q", tag, got, want)
		}
	}
}
