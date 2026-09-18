package icu

import (
	"strings"
	"testing"
)

// The plural rules are stored as answers rather than as arithmetic, so what
// matters is that the answers are the ones CLDR gives -- including for the
// languages whose rules ask more than what the last two digits are.
func TestPluralRules(t *testing.T) {
	cases := []struct {
		tag    string
		counts []float64
		want   string
	}{
		{"en", []float64{0, 1, 2, 5, 21, 1.5}, "other one other other other other"},
		{"ru", []float64{0, 1, 2, 5, 21, 101, 111, 1.5},
			"many one few many one one many other"},
		{"pl", []float64{1, 2, 5, 22, 101, 102}, "one few many few many few"},
		{"ar", []float64{0, 1, 2, 3, 11, 100, 101}, "zero one two few many other other"},
		{"cy", []float64{0, 1, 2, 3, 6, 5}, "zero one two few many other"},
		{"he", []float64{1, 2, 3, 0.5}, "one two other one"},
		{"ja", []float64{0, 1, 2, 100}, "other other other other"},
		// Cornish counts in scores, and in hundreds of thousands: twenty-one
		// thousand is not what a thousand is, and a hundred thousand is not
		// what a million is.
		{"kw", []float64{0, 1, 2, 3, 21, 1000, 20000, 21000, 40000, 100000},
			"zero one two few many two two other two two"},
		{"kw", []float64{1000000, 1100000, 2000000, 2100000}, "other two other two"},
	}
	for _, tc := range cases {
		l := Resolve(tc.tag)
		got := make([]string, len(tc.counts))
		for i, n := range tc.counts {
			got[i] = l.Cardinal.Category(n)
		}
		if strings.Join(got, " ") != tc.want {
			t.Errorf("%s %v:\n got  %s\n want %s", tc.tag, tc.counts,
				strings.Join(got, " "), tc.want)
		}
	}

	// Ordinals, which are a different rule in the same shape.
	for _, tc := range []struct{ tag, want string }{
		{"en", "one two few other other"},
		{"it", "other other other other many"},
	} {
		l := Resolve(tc.tag)
		var got []string
		for _, n := range []float64{1, 2, 3, 4, 11} {
			got = append(got, l.Ordinal.Category(n))
		}
		if strings.Join(got, " ") != tc.want {
			t.Errorf("%s ordinals:\n got  %s\n want %s", tc.tag,
				strings.Join(got, " "), tc.want)
		}
	}
}

// A tag is answered with the data of the locale it means, whether that is its
// own, one it shares, its language, or English.
func TestResolve(t *testing.T) {
	for _, tc := range []struct{ tag, want string }{
		{"de", "de"},
		{"de-DE", "de"},      // the region says nothing new
		{"de-CH", "de-CH"},   // this one does
		{"DE_ch", "de-CH"},   // however it is written
		{"zh-CN", "zh"},      // Chinese as written in China is Chinese
		{"zh-TW", "zh-Hant"}, // and in Taiwan it is the other script
		{"ar-EG", "ar-BH"},   // Egyptian Arabic is written as Bahraini is
		{"en-US", "en"},
		{"xx-YY", "en"}, // nothing at all falls back to English
		{"", "en"},
		{"en-GB-u-ca-gregory", "en-GB"},
	} {
		if got := ResolveTag(tc.tag); got != tc.want {
			t.Errorf("ResolveTag(%q) = %q, want %q", tc.tag, got, tc.want)
		}
		if got := Resolve(tc.tag).Tag; got != tc.want {
			t.Errorf("Resolve(%q).Tag = %q, want %q", tc.tag, got, tc.want)
		}
	}

	// Has is what supportedLocalesOf asks: is this a locale we know, rather
	// than one we would answer in English.
	for _, tag := range []string{"de", "de-CH", "zh-CN", "ar-EG", "kw", "haw"} {
		if !Has(tag) {
			t.Errorf("Has(%q) = false", tag)
		}
	}
	for _, tag := range []string{"xx", "zz-ZZ"} {
		if Has(tag) {
			t.Errorf("Has(%q) = true", tag)
		}
	}
}

// Each locale is independently compressed and decoded the first time it is
// asked for.
func TestTable(t *testing.T) {
	blobs := unpack()
	if len(blobs) != len(tags) {
		t.Fatalf("%d locales in the table, %d tags", len(blobs), len(tags))
	}
	total := 0
	packedTotal := 0
	for _, b := range blobs {
		total += len(b)
	}
	for _, bounds := range packedLocales {
		packedTotal += int(bounds[1] - bounds[0])
	}
	t.Logf("%d locales, %d KB of data in %d KB of source, from CLDR via ICU %s",
		len(tags), total/1024, packedTotal/1024, cldrVersion)

	// Every locale decodes into something usable.
	for _, tag := range tags {
		l := Resolve(tag)
		if l == nil {
			t.Fatalf("%s did not decode", tag)
		}
		if len(l.Months) != 12 || len(l.Days) != 7 {
			t.Errorf("%s: %d months, %d days", tag, len(l.Months), len(l.Days))
		}
		if l.Decimal == "" || l.Group == "" {
			t.Errorf("%s: no separators", tag)
		}
		if l.DatePatterns[0] == "" || l.TimePatterns[0] == "" {
			t.Errorf("%s: no patterns", tag)
		}
		if len(l.Cardinal.Categories) == 0 {
			t.Errorf("%s: no plural categories", tag)
		}
	}
}
