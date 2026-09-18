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

	// What is expected to differ, and why. Each is something the data carried
	// here does not say; everything else has to match exactly.
	known := []string{
		// Chinese and Japanese order their characters by sound or by stroke,
		// which is a whole ordering of twenty thousand characters rather than
		// the handful of moves a European alphabet needs.
		`Collator("ja"`, `Collator("ko"`,
		// Chinese also orders accents the other way round, which is a
		// tailoring of the second level rather than the first.
		`Collator("zh"`,
		// Thai passes over punctuation and then orders it its own way.
		`Collator("th"`,
		// Turkish keeps the dotted and dotless i apart, which takes more than
		// moving a letter: it splits one letter into two and moves the
		// capitals across.
		`Collator("tr"`,
	}

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
		t.Errorf("%d of %d cases differ from ICU for reasons that are not known:\n%s",
			len(unexpected), cases, strings.Join(unexpected[:min(len(unexpected), 20)], "\n"))
	}
	// The known ones are few, and are meant to stay few.
	if len(differences) > 40 {
		t.Errorf("%d cases differ from ICU, which is more than the %d expected",
			len(differences), 40)
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

		// Time zones, which are the operating system's to know and this to
		// write: a zone with names of its own, one on the three-quarter hour,
		// and one whose summer is in January.
		{`new Intl.DateTimeFormat("en", {timeZone: "America/New_York", dateStyle: "medium",
		    timeStyle: "short"}).format(Date.UTC(2024, 6, 20, 15, 4))`,
			"Jul 20, 2024, 11:04 AM"},
		{`new Intl.DateTimeFormat("en", {timeZone: "America/New_York", hour: "numeric",
		    timeZoneName: "short"}).format(Date.UTC(2024, 6, 20, 15, 4))`, "11 AM EDT"},
		{`new Intl.DateTimeFormat("en", {timeZone: "America/New_York", hour: "numeric",
		    timeZoneName: "long"}).format(Date.UTC(2024, 0, 20, 15, 4))`,
			"10 AM Eastern Standard Time"},
		{`new Intl.DateTimeFormat("en", {timeZone: "Pacific/Chatham", timeStyle: "short",
		    hour12: false}).format(Date.UTC(2024, 6, 20, 0, 0))`, "12:45"},
		{`new Intl.DateTimeFormat("en", {timeZone: "Australia/Sydney", dateStyle: "short",
		    timeStyle: "short"}).format(Date.UTC(2024, 0, 20, 15, 4))`, "1/21/24, 2:04 AM"},
		{`new Intl.DateTimeFormat("en", {timeZone: "america/new_york"}).resolvedOptions().timeZone`,
			"America/New_York"},
		{`try { new Intl.DateTimeFormat("en", {timeZone: "Mars/Olympus"}) }
		  catch (e) { e.constructor.name }`, "RangeError"},

		// Sorting, which is a language's own business: Swedish puts å, ä and ö
		// after z, Czech treats ch as a letter between h and i, Danish writes
		// å as aa and sorts it accordingly, and German does none of that.
		{`["z", "ä", "a", "ö"].sort(new Intl.Collator("sv").compare).join()`, "a,z,ä,ö"},
		{`["z", "ä", "a", "ö"].sort(new Intl.Collator("de").compare).join()`, "a,ä,ö,z"},
		{`["chleba", "cukr", "hora"].sort(new Intl.Collator("cs").compare).join()`,
			"cukr,hora,chleba"},
		{`["chleba", "cukr", "hora"].sort(new Intl.Collator("en").compare).join()`,
			"chleba,cukr,hora"},
		{`["aardvark", "zebra", "ångström"].sort(new Intl.Collator("da").compare).join()`,
			"zebra,ångström,aardvark"},
		// A ligature sorts as what it is made of, and an accent counts for
		// less than a letter.
		{`["œuvre", "Öl", "ovum"].sort(new Intl.Collator("en").compare).join()`,
			"œuvre,Öl,ovum"},
		{`new Intl.Collator("en").compare("résumé", "resumes")`, "-1"},

		// A run of digits as a number, and a letter without its
		// accent when that is what was asked for.
		{`["file10", "file9", "file1"].sort(new Intl.Collator("en", {numeric: true}).compare).join()`,
			"file1,file9,file10"},
		{`"résumé".localeCompare("resume", "en", {sensitivity: "base"})`, "0"},
		{`"a".localeCompare("b")`, "-1"},

		// What things are called, which the engine knows in English and the
		// intldata package knows in every language.
		{`new Intl.DisplayNames("en", {type: "region"}).of("FR")`, "France"},
		{`new Intl.DisplayNames("en", {type: "language"}).of("de-AT")`, "Austrian German"},
		{`new Intl.DisplayNames("en", {type: "currency"}).of("EUR")`, "Euro"},
		{`new Intl.DisplayNames("fr", {type: "region"}).of("DE")`, "Germany"},
		{`new Intl.DisplayNames("en", {type: "region", fallback: "none"}).of("QQ")`,
			"undefined"},
		{`new Intl.DisplayNames("en", {type: "region"}).of("QQ")`, "QQ"},

		// What it says about itself, which is how a program can tell what it
		// got: a locale it has data for, or English.
		{`new Intl.NumberFormat("de-DE").resolvedOptions().locale`, "de-DE"},
		{`new Intl.NumberFormat("xx-YY").resolvedOptions().locale`, "en"},
		{`Intl.NumberFormat.supportedLocalesOf(["de", "xx"]).join()`, "de"},
		{`Intl.getCanonicalLocales(["EN-us", "de_de"]).join()`, "en-US,de-DE"},

		// Where a text may be broken, which is what a program counting
		// characters or selecting a word has to know: an accent written apart
		// belongs to its letter, a family emoji is one character, an
		// apostrophe stands inside a word, and a full stop is not always the
		// end of a sentence.
		{`[...new Intl.Segmenter("en").segment("e\u0301clair")].map(s => s.segment).join("|")`,
			"e\u0301|c|l|a|i|r"},
		{`[...new Intl.Segmenter("en").segment("\u{1f468}\u200d\u{1f469}\u200d\u{1f467}")]
		    .length`, "1"},
		{`"\u{1f468}\u200d\u{1f469}\u200d\u{1f467}".length`, "8"},
		{`[...new Intl.Segmenter("en", {granularity: "word"}).segment("can't stop, won't stop")]
		    .filter(s => s.isWordLike).map(s => s.segment).join("|")`, "can't|stop|won't|stop"},
		{`[...new Intl.Segmenter("en", {granularity: "word"}).segment("3.14 and 1,000")]
		    .map(s => s.segment).join("|")`, "3.14| |and| |1,000"},
		{`[...new Intl.Segmenter("ja", {granularity: "word"})
		    .segment("日本語のテキストです")].map(s => s.segment).join("|")`,
			"日本語|の|テキスト|です"},
		{`[...new Intl.Segmenter("en", {granularity: "sentence"})
		    .segment("It is 3.14 exactly. Yes!")].map(s => s.segment).join("|")`,
			"It is 3.14 exactly. |Yes!"},
		{`JSON.stringify(new Intl.Segmenter("en", {granularity: "word"})
		    .segment("hello world").containing(7))`,
			`{"segment":"world","index":6,"input":"hello world","isWordLike":true}`},
		{`new Intl.Segmenter("en", {granularity: "word"}).segment("hi").containing(9)`,
			"undefined"},
		{`new Intl.Segmenter("en", {granularity: "sentence"}).resolvedOptions().granularity`,
			"sentence"},

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
