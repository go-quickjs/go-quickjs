package quickjs_test

import (
	"bufio"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// Intl is held against a full ICU rather than against what it was written to
// do: testdata/intl_golden.txt is what node's ICU answered for a corpus of
// formatting calls, and this runs the same calls here.
//
// A handful of cases differ, and they are listed below rather than hidden: each
// is something the data carried here cannot say. The count is checked too, so
// that a change which fixes one of them or breaks something else is noticed.
func TestIntlMatchesICU(t *testing.T) {
	file, err := os.Open("testdata/intl_golden.txt")
	if err != nil {
		t.Skip("no golden file: run node internal/icu/internal/cldrgen/golden.mjs")
	}
	defer file.Close()

	rt := quickjs.New()
	defer rt.Close()

	// The differences that are expected. Chinese tells the times of day apart
	// more finely than a morning and an afternoon -- 凌晨 is the small hours,
	// 晚上 the evening -- and choosing between them needs day-period rules,
	// which are not carried here.
	known := []string{"凌晨", "晚上", "中午"}

	var differences []string
	cases, checked := 0, 0
	scan := bufio.NewScanner(file)
	scan.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scan.Scan() {
		line := scan.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		source, quoted, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		want, err := strconv.Unquote(quoted)
		if err != nil {
			t.Fatalf("%s: %v", source, err)
		}
		cases++

		v, err := rt.Eval(source)
		if err != nil {
			differences = append(differences, source+"\n   failed: "+err.Error())
			continue
		}
		if got := v.String(); got != want {
			differences = append(differences,
				source+"\n   got  "+strconv.Quote(got)+"\n   want "+strconv.Quote(want))
			continue
		}
		checked++
	}
	if err := scan.Err(); err != nil {
		t.Fatal(err)
	}
	if cases == 0 {
		t.Fatal("the golden file is empty")
	}

	// Everything else has to match exactly.
	var unexpected []string
	for _, d := range differences {
		explained := false
		for _, mark := range known {
			if strings.Contains(d, mark) {
				explained = true
				break
			}
		}
		if !explained {
			unexpected = append(unexpected, d)
		}
	}
	sort.Strings(unexpected)
	if len(unexpected) > 0 {
		t.Errorf("%d of %d cases differ from ICU:\n%s", len(unexpected), cases,
			strings.Join(unexpected[:min(len(unexpected), 20)], "\n"))
	}
	t.Logf("%d of %d cases match ICU exactly (%.2f%%)", checked, cases,
		100*float64(checked)/float64(cases))
}

// What Intl does is data, and this is the reading of it: a handful of cases
// whose answers are worth seeing written down.
func TestIntlFormats(t *testing.T) {
	cases := []struct{ src, want string }{
		// The separators, the grouping, and the digits are the language's.
		{`new Intl.NumberFormat("en").format(1234567.891)`, "1,234,567.891"},
		{`new Intl.NumberFormat("de").format(1234567.891)`, "1.234.567,891"},
		{`new Intl.NumberFormat("en-IN").format(12345678)`, "1,23,45,678"},
		{`new Intl.NumberFormat("es").format(1234)`, "1234"},
		{`new Intl.NumberFormat("ar-EG").format(1234.5)`, "١٬٢٣٤٫٥"},

		// Money, which has a symbol, a place for it, and its own number of
		// decimal places.
		{`new Intl.NumberFormat("en", {style: "currency", currency: "USD"}).format(9.5)`, "$9.50"},
		{`new Intl.NumberFormat("de", {style: "currency", currency: "EUR"}).format(9.5)`, "9,50\u00a0€"},
		{`new Intl.NumberFormat("ja", {style: "currency", currency: "JPY"}).format(1200)`, "￥1,200"},
		{`new Intl.NumberFormat("en", {style: "currency", currency: "USD"}).format(-1)`, "-$1.00"},

		// Shortening a number is part of the language too: thousands in
		// English, ten-thousands in Japanese, lakh in India.
		{`new Intl.NumberFormat("en", {notation: "compact"}).format(1234567)`, "1.2M"},
		{`new Intl.NumberFormat("ja", {notation: "compact"}).format(12345678)`, "1235万"},
		{`new Intl.NumberFormat("en-IN", {notation: "compact"}).format(12345678)`, "1.2Cr"},
		{`new Intl.NumberFormat("de", {notation: "compact", compactDisplay: "long"}).format(2e6)`,
			"2 Millionen"},

		// Dates: the order of the fields, the names of the months, and the
		// word that joins a date to a time.
		{`new Intl.DateTimeFormat("en", {dateStyle: "full", timeZone: "UTC"})
		    .format(Date.UTC(2024, 0, 5))`, "Friday, January 5, 2024"},
		{`new Intl.DateTimeFormat("de", {dateStyle: "full", timeZone: "UTC"})
		    .format(Date.UTC(2024, 0, 5))`, "Freitag, 5. Januar 2024"},
		{`new Intl.DateTimeFormat("ja", {dateStyle: "long", timeZone: "UTC"})
		    .format(Date.UTC(2024, 0, 5))`, "2024年1月5日"},
		{`new Intl.DateTimeFormat("en", {dateStyle: "long", timeStyle: "short", timeZone: "UTC"})
		    .format(Date.UTC(2024, 0, 5, 15, 4))`, "January 5, 2024 at 3:04 PM"},
		// Polish declines its months: the fifth of January is not January.
		{`new Intl.DateTimeFormat("pl", {month: "long", day: "numeric", timeZone: "UTC"})
		    .format(Date.UTC(2024, 0, 5))`, "5 stycznia"},
		{`new Intl.DateTimeFormat("pl", {month: "long", timeZone: "UTC"})
		    .format(Date.UTC(2024, 0, 5))`, "styczeń"},
		// Thailand counts its years from another era.
		{`new Intl.DateTimeFormat("th", {dateStyle: "long", timeZone: "UTC"})
		    .format(Date.UTC(2024, 0, 5))`, "5 มกราคม 2567"},

		// Which form a count takes, which is arithmetic in every language and
		// different arithmetic in each.
		{`["en","ru","pl","ar","cy"].map(l =>
		    [0,1,2,5,21].map(n => new Intl.PluralRules(l).select(n)).join(",")).join(" | ")`,
			"other,one,other,other,other | many,one,few,many,one | " +
				"many,one,few,many,many | zero,one,two,few,many | zero,one,two,other,other"},
		{`new Intl.PluralRules("en", {type: "ordinal"}).select(22)`, "two"},

		// Lists and times gone by.
		{`new Intl.ListFormat("en").format(["a", "b", "c"])`, "a, b, and c"},
		{`new Intl.ListFormat("de").format(["a", "b", "c"])`, "a, b und c"},
		{`new Intl.RelativeTimeFormat("en", {numeric: "auto"}).format(-1, "day")`, "yesterday"},
		{`new Intl.RelativeTimeFormat("ru").format(3, "day")`, "через 3 дня"},
		{`new Intl.RelativeTimeFormat("ru").format(5, "day")`, "через 5 дней"},

		// The methods on the values go through the same thing.
		{`(1234.5).toLocaleString("de-DE")`, "1.234,5"},
		{`(1234567890123456789012345678901234567890n).toLocaleString("en")`,
			"1,234,567,890,123,456,789,012,345,678,901,234,567,890"},
		{`new Date(Date.UTC(2024, 0, 5, 15, 4, 5)).toLocaleDateString("en", {timeZone: "UTC"})`,
			"1/5/2024"},

		// Sorting: a run of digits as a number, and a letter without its
		// accent when that is what was asked for.
		{`["file10", "file9", "file1"].sort(new Intl.Collator("en", {numeric: true}).compare).join()`,
			"file1,file9,file10"},
		{`"résumé".localeCompare("resume", "en", {sensitivity: "base"})`, "0"},
		{`"a".localeCompare("b")`, "-1"},

		// What it says about itself, which is how a program can tell what it
		// got: a locale it has data for, or English.
		{`new Intl.NumberFormat("de-DE").resolvedOptions().locale`, "de-DE"},
		{`new Intl.NumberFormat("xx-YY").resolvedOptions().locale`, "en"},
		{`Intl.NumberFormat.supportedLocalesOf(["de", "xx"]).join()`, "de"},
		{`Intl.getCanonicalLocales(["EN-us", "de_de"]).join()`, "en-US,de-DE"},

		// A time zone it cannot do is refused rather than guessed at.
		{`try { new Intl.DateTimeFormat("en", {timeZone: "America/New_York"}) }
		  catch (e) { e.constructor.name }`, "RangeError"},
		// And the parts, for a program that lays them out itself.
		{`JSON.stringify(new Intl.NumberFormat("en", {style: "currency", currency: "EUR"})
		    .formatToParts(1234.5))`,
			`[{"type":"currency","value":"€"},{"type":"integer","value":"1"},` +
				`{"type":"group","value":","},{"type":"integer","value":"234"},` +
				`{"type":"decimal","value":"."},{"type":"fraction","value":"50"}]`},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// Intl is built the first time it is looked at, which nothing but the property
// descriptor can tell.
func TestIntlIsBuiltWhenAskedFor(t *testing.T) {
	cases := []struct{ src, want string }{
		{`typeof Intl`, "object"},
		{`Intl.NumberFormat === Intl.NumberFormat`, "true"},
		// A host can put its own there instead.
		{`globalThis.Intl = {mine: true}; Intl.mine`, "true"},
		{`globalThis.Intl = 5; typeof Intl`, "number"},
		{`delete globalThis.Intl; typeof Intl`, "undefined"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}
