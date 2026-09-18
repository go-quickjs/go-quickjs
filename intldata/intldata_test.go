package intldata_test

import (
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
	_ "github.com/go-quickjs/go-quickjs/intldata"
)

// With this package imported, Intl.DisplayNames answers in the language it was
// asked in. Without it, the engine has the English names only.
func TestDisplayNamesInEveryLanguage(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()

	for _, tc := range []struct{ src, want string }{
		{`new Intl.DisplayNames("fr", {type: "region"}).of("DE")`, "Allemagne"},
		{`new Intl.DisplayNames("de", {type: "region"}).of("FR")`, "Frankreich"},
		{`new Intl.DisplayNames("ja", {type: "region"}).of("US")`, "アメリカ合衆国"},
		{`new Intl.DisplayNames("es", {type: "language"}).of("de")`, "alemán"},
		{`new Intl.DisplayNames("ru", {type: "language"}).of("en")`, "английский"},
		{`new Intl.DisplayNames("zh", {type: "script"}).of("Latn")`, "拉丁文"},
		{`new Intl.DisplayNames("fr", {type: "currency"}).of("USD")`, "dollar des États-Unis"},
		// English still works, and so does the fallback to it for a language
		// that names nothing.
		{`new Intl.DisplayNames("en", {type: "region"}).of("FR")`, "France"},
		{`typeof new Intl.DisplayNames("en", {type: "region"}).of("FR")`, "string"},
	} {
		v, err := rt.Eval(tc.src)
		if err != nil {
			t.Errorf("%s: %v", tc.src, err)
			continue
		}
		if got := v.String(); got != tc.want {
			t.Errorf("%s\n got  %q\n want %q", tc.src, got, tc.want)
		}
	}
}
