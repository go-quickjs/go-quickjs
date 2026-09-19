package quickjs_test

import (
	"bufio"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

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

	// What is expected to differ, and why. Everything else has to match
	// exactly; this is deliberately empty when the corpus agrees in full.
	known := []string{}

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
	seenKnown := make([]bool, len(known))
	for _, d := range differences {
		explained := false
		for i, mark := range known {
			if strings.Contains(d, mark) {
				explained = true
				seenKnown[i] = true
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
	for i, seen := range seenKnown {
		if !seen {
			t.Errorf("known ICU difference no longer occurs; remove its allowance: %s", known[i])
		}
	}
	// Every allowance above names one exact golden case.
	if len(differences) > len(known) {
		t.Errorf("%d cases differ from ICU, which is more than the %d named differences",
			len(differences), len(known))
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
		{`new Intl.NumberFormat("ko-KR", {style: "unit", unit: "kilometer-per-hour",
		    unitDisplay: "long"}).formatToParts(-987)
		    .map(p => p.type + "=" + p.value).join("|")`,
			"unit=시속|literal= |minusSign=-|integer=987|unit=킬로미터"},
		{`new Intl.NumberFormat("en-US", {style: "currency", currency: "USD",
		    signDisplay: "always"}).formatRange(2.9, 3.1)`, "+$2.90–3.10"},
		{`new Intl.NumberFormat("pt-PT", {style: "currency", currency: "EUR",
		    maximumFractionDigits: 0}).formatRange(3, 5)`, "3 - 5\u00a0€"},

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
		{`new Intl.DateTimeFormat("en-US", {year: "numeric", month: "short", day: "numeric",
		    timeZone: "UTC"}).formatRange(Date.UTC(2019, 0, 3), Date.UTC(2019, 0, 5))`,
			"Jan 3\u2009–\u20095, 2019"},
		// Thailand counts its years from another era.
		{`new Intl.DateTimeFormat("th", {dateStyle: "long", timeZone: "UTC"})
		    .format(Date.UTC(2024, 0, 5))`, "5 มกราคม 2567"},
		{`new Intl.DateTimeFormat("sc", {calendar: "hebrew", dateStyle: "full",
		    timeZone: "UTC"}).format(Date.UTC(2024, 0, 5))`,
			"chenàbura 24 de tevet de su 5784 a.m."},
		{`new Intl.DateTimeFormat("ksh", {calendar: "buddhist", dateStyle: "full",
		    timeZone: "UTC"}).format(Date.UTC(2024, 0, 5))`,
			"Friidaach, 5. Jannewa 2567 BE"},
		// ICU 78 selects its week-year field for a few locale/calendar
		// combinations whose formatToParts implementation aborts in Node.
		{`new Intl.DateTimeFormat("gl", {calendar: "buddhist", dateStyle: "full",
		    timeZone: "UTC"}).format(Date.UTC(2024, 0, 5))`,
			"venres, 5 de xaneiro de 2024 BE"},
		{`new Intl.DateTimeFormat("my", {calendar: "coptic", year: "numeric",
		    month: "numeric", day: "numeric", timeZone: "UTC"})
		    .format(Date.UTC(2024, 0, 5))`, "AM ၂၆/၀၄/၁၇၄၀"},
		{`new Intl.DateTimeFormat("ksh", {calendar: "buddhist", year: "numeric",
		    month: "numeric", timeZone: "UTC"}).format(Date.UTC(2024, 0, 5))`,
			"2024-01"},
		{`new Intl.DateTimeFormat("gl", {calendar: "buddhist", dateStyle: "full",
		    timeZone: "UTC"}).format(Date.UTC(2016, 0, 1))`,
			"venres, 1 de xaneiro de 2015 BE"},
		{`new Intl.DateTimeFormat("my", {calendar: "buddhist", year: "numeric",
		    month: "numeric", day: "numeric", timeZone: "UTC"})
		    .format(Date.UTC(2015, 11, 27))`, "BE ၂၇/၁၂/၂၀၁၆"},
		{`["sc", "ksh"].map(locale => new Intl.DateTimeFormat(locale)
		    .resolvedOptions().locale).join(",")`, "sc,ksh"},

		// And a calendar may be asked for outright: the Islamic year is
		// eleven days shorter than this one, the Hebrew year has a
		// thirteenth month in seven years out of nineteen, and the Japanese
		// year is counted from the start of a reign and named after it.
		{`new Intl.DateTimeFormat("en-u-ca-islamic-civil", {dateStyle: "long",
		    timeZone: "UTC"}).format(Date.UTC(2024, 0, 5))`, "Jumada II 23, 1445 AH"},
		{`new Intl.DateTimeFormat("en-u-ca-hebrew", {dateStyle: "long",
		    timeZone: "UTC"}).format(Date.UTC(2024, 0, 5))`, "24 Tevet 5784"},
		{`new Intl.DateTimeFormat("en-u-ca-hebrew", {dateStyle: "short",
		    timeZone: "UTC"}).format(Date.UTC(2024, 0, 5))`, "24 Tevet 5784"},
		{`new Intl.DateTimeFormat("be-u-ca-buddhist", {month: "long", day: "numeric",
		    timeZone: "UTC"}).format(Date.UTC(2024, 0, 5))`, "5 студзеня"},
		{`new Intl.DateTimeFormat("bg-u-ca-buddhist", {dateStyle: "full",
		    timeZone: "UTC"}).format(Date.UTC(2024, 0, 5))`, "петък, 5 януари 2567 г. BE"},
		{`new Intl.DateTimeFormat("ccp-u-ca-buddhist", {dateStyle: "short",
		    timeZone: "UTC"}).format(Date.UTC(2024, 0, 5))`, "𑄻/𑄷/𑄸𑄻𑄼𑄽 BE"},
		{`new Intl.DateTimeFormat("ff-Adlm-u-ca-buddhist", {dateStyle: "short",
		    timeZone: "UTC"}).format(Date.UTC(2024, 0, 5))`, "𞥕-𞥑-𞥒𞥕𞥖𞥗 𞤘𞤄"},
		{`new Intl.DateTimeFormat("dz-u-ca-hebrew", {month: "long", timeZone: "UTC"})
		    .formatToParts(Date.UTC(2024, 0, 5)).map(p => p.type + "=" + p.value).join("|")`,
			"literal=སྤྱི་|month=Tevet"},
		{`new Intl.DateTimeFormat("en-u-ca-chinese", {dateStyle: "full", timeZone: "UTC"})
		    .format(Date.UTC(2024, 0, 5))`,
			"Friday, Eleventh Month 24, 2023(gui-mao)"},
		{`new Intl.DateTimeFormat("ja-u-ca-japanese", {dateStyle: "long",
		    timeZone: "UTC"}).format(Date.UTC(2024, 0, 5))`, "令和6年1月5日"},
		{`new Intl.DateTimeFormat("fa-u-ca-persian", {dateStyle: "long",
		    timeZone: "UTC"}).format(Date.UTC(2024, 0, 5))`, "۱۵ دی ۱۴۰۲"},
		{`new Intl.DateTimeFormat("en-u-ca-coptic", {dateStyle: "long",
		    timeZone: "UTC"}).format(Date.UTC(2024, 0, 5))`, "Kiahk 26, 1740 AM"},
		{`new Intl.DateTimeFormat("en-u-ca-indian", {year: "numeric",
		    month: "long", day: "numeric", timeZone: "UTC"})
		    .format(Date.UTC(2024, 0, 5))`, "Pausa 15, 1945 Śaka"},
		{`new Intl.DateTimeFormat("he-u-ca-hebrew", {dateStyle: "full", timeZone: "UTC"})
		    .format(Date.UTC(2024, 0, 5))`, "יום שישי, כ״ד בטבת תשפ״ד"},
		{`new Intl.DateTimeFormat("fi-u-ca-buddhist", {dateStyle: "full", timeZone: "UTC"})
		    .format(Date.UTC(2024, 0, 5))`, "perjantai 5. tammikuuta 2567 BE"},
		{`new Intl.DateTimeFormat("mn-u-ca-buddhist", {dateStyle: "full", timeZone: "UTC"})
		    .format(Date.UTC(2024, 0, 5))`, "BE 2567 оны нэгдүгээр сарын 5. Баасан гараг"},
		{`new Intl.DateTimeFormat("eu-u-ca-coptic", {year: "numeric", month: "short",
		    day: "numeric", timeZone: "UTC"}).format(Date.UTC(2024, 0, 5))`,
			"1740(e)ko Kiahkk 26"},
		{`new Intl.DateTimeFormat("ku-u-ca-indian", {dateStyle: "long", timeZone: "UTC"})
		    .format(Date.UTC(2024, 0, 5))`, "Śaka 15ê Pausaa 1945an"},
		{`new Intl.DateTimeFormat("haw-u-ca-buddhist", {dateStyle: "short", timeZone: "UTC"})
		    .format(Date.UTC(2024, 0, 5))`, "5/i/67 BE"},
		{`new Intl.DateTimeFormat("en-u-ca-hebrew", {timeZone: "UTC"})
		    .resolvedOptions().calendar`, "hebrew"},
		{`new Intl.DateTimeFormat("en", {calendar: "islamic"})
		    .resolvedOptions().calendar`, "islamic-civil"},
		{`Intl.supportedValuesOf("calendar").includes("islamic")`, "false"},
		{`new Intl.DateTimeFormat("ja", {hour: "numeric", hour12: true})
		    .resolvedOptions().hourCycle`, "h12"},
		{`new Intl.DateTimeFormat("ja", {hour: "numeric", minute: "2-digit",
		    hour12: true, timeZone: "UTC"}).format(Date.UTC(2000, 1, 29))`, "午前12:00"},
		{`(() => {
		    const era = (calendar, year) => {
		      const date = new Date(0); date.setUTCFullYear(year, 5, 15);
		      return new Intl.DateTimeFormat("en", {calendar, era: "long",
		        year: "numeric", timeZone: "UTC"}).formatToParts(date)
		        .find(p => p.type === "era").value;
		    };
		    return ["islamic-civil", "islamic-tbla", "islamic-umalqura"]
		      .map(c => [era(c, 600), era(c, 2025)].join(",")).join("|");
		  })()`, "BH,AH|BH,AH|BH,AH"},
		{`(() => {
		    const f = new Intl.DateTimeFormat("en", {calendar: "ethiopic",
		      era: "long", year: "numeric", timeZone: "UTC"});
		    return [-6000, 0, 2025].map(year => {
		      const date = new Date(0); date.setUTCFullYear(year, 5, 15);
		      return f.formatToParts(date).find(p => p.type === "era").value;
		    }).join(",");
		  })()`, "AA,AA,AM"},
		{`["chinese", "dangi"].map(calendar =>
		    new Intl.DateTimeFormat("en", {calendar, era: "long", year: "numeric"})
		      .formatToParts(Date.UTC(2025, 5, 15)).some(p => p.type === "era"))
		    .join()`, "false,false"},
		// A lunisolar year is named rather than numbered, and has a month
		// said twice in the years that need one.
		{`new Intl.DateTimeFormat("zh-u-ca-chinese", {year: "numeric"})
		    .formatToParts(Date.UTC(2024, 5, 1)).map(p => p.type + "=" + p.value).join()`,
			"relatedYear=2024,yearName=甲辰,literal=年"},
		{`new Intl.DateTimeFormat("zh-u-ca-chinese", {month: "long", day: "numeric",
		    timeZone: "UTC"}).format(Date.UTC(2020, 4, 23))`, "闰四月1日"},

		// Which form a count takes, which is arithmetic in every language and
		// different arithmetic in each.
		{`["en","ru","pl","ar","cy"].map(l =>
		    [0,1,2,5,21].map(n => new Intl.PluralRules(l).select(n)).join(",")).join(" | ")`,
			"other,one,other,other,other | many,one,few,many,one | " +
				"many,one,few,many,many | zero,one,two,few,many | zero,one,two,other,other"},
		{`new Intl.PluralRules("en", {type: "ordinal"}).select(22)`, "two"},
		{`["standard", "compact"].map(notation =>
		    [1e6, 1.5e6, 1e-6, 999949].map(value =>
		      new Intl.PluralRules("fr", {notation}).select(value)).join(",")).join(" | ")`,
			"many,other,one,other | many,many,one,many"},

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
		{`["GMT", "Etc/GMT", "Greenwich", "Etc/Greenwich", "Etc/GMT0",
		    "Etc/GMT+0", "Etc/GMT-0"].map(timeZone =>
		    new Intl.DateTimeFormat("en", {timeZone, timeZoneName: "long", hour: "numeric"})
		      .formatToParts(0).find(p => p.type === "timeZoneName").value).join("|")`,
			"Coordinated Universal Time|Coordinated Universal Time|" +
				"Greenwich Mean Time|Greenwich Mean Time|Greenwich Mean Time|" +
				"Greenwich Mean Time|Greenwich Mean Time"},
		// Before standard time, some zones were an offset down to the second.
		// Paris used local mean time, nine minutes and twenty-one seconds east
		// of Greenwich, at the start of 1900.
		{`new Intl.DateTimeFormat("en", {timeZone: "Europe/Paris", timeZoneName: "shortOffset"})
		    .formatToParts(Date.UTC(1900, 0, 1)).find(p => p.type === "timeZoneName").value`,
			"GMT+0:09:21"},
		{`new Intl.DateTimeFormat("en", {timeZone: "Europe/Paris", timeZoneName: "longOffset"})
		    .formatToParts(Date.UTC(1900, 0, 1)).find(p => p.type === "timeZoneName").value`,
			"GMT+00:09:21"},
		{`new Intl.DateTimeFormat("fr", {timeZone: "Europe/Paris", timeZoneName: "longOffset"})
		    .formatToParts(Date.UTC(1900, 0, 1)).find(p => p.type === "timeZoneName").value`,
			"UTC+00:09:21"},
		{`[Date.UTC(1971, 9, 31, 1, 59, 59, 999), Date.UTC(1971, 9, 31, 2)]
		    .map(when => new Intl.DateTimeFormat("en", {timeZone: "Europe/London",
		      timeZoneName: "long"}).formatToParts(when)
		      .find(p => p.type === "timeZoneName").value).join("|")`,
			"GMT+01:00|Greenwich Mean Time"},
		{`[Date.UTC(1976, 8, 25, 23, 59, 59, 999), Date.UTC(1976, 8, 26)]
		    .map(when => new Intl.DateTimeFormat("en", {timeZone: "Europe/Lisbon",
		      timeZoneName: "long"}).formatToParts(when)
		      .find(p => p.type === "timeZoneName").value).join("|")`,
			"Central European Standard Time|Western European Standard Time"},
		{`new Intl.DateTimeFormat("de", {timeZone: "America/New_York",
		    timeZoneName: "longGeneric"}).formatToParts(Date.UTC(2025, 0, 15))
		    .find(p => p.type === "timeZoneName").value`,
			"Nordamerikanische Ostküstenzeit"},
		{`new Intl.DateTimeFormat("en", {timeZone: "Asia/Almaty",
		    timeZoneName: "longGeneric"}).formatToParts(Date.UTC(2025, 0, 15))
		    .find(p => p.type === "timeZoneName").value`,
			"Kazakhstan Time"},
		{`[1980, 1985, 1991].map(year => new Intl.DateTimeFormat("en", {
		    timeZone: "Asia/Tehran", timeZoneName: "longGeneric"})
		    .formatToParts(Date.UTC(year, 0, 15))
		    .find(p => p.type === "timeZoneName").value).join("|")`,
			"Iran Time|Iran Standard Time|Iran Time"},
		{`[1979, 1980].map(year => new Intl.DateTimeFormat("de", {
		    timeZone: "Europe/Paris", timeZoneName: "longGeneric"})
		    .formatToParts(Date.UTC(year, 6, 15))
		    .find(p => p.type === "timeZoneName").value).join("|")`,
			"Mitteleuropäische Zeit (Frankreich)|Mitteleuropäische Zeit"},
		{`[2010, 2015].map(year => new Intl.DateTimeFormat("ar", {
		    timeZone: "Europe/Kyiv", timeZoneName: "longGeneric"})
		    .formatToParts(Date.UTC(year, 6, 15))
		    .find(p => p.type === "timeZoneName").value).join("|")`,
			"توقيت شرق أوروبا|توقيت شرق أوروبا (كييف)"},
		// Manaus also checks that a short form with zero minutes still keeps
		// its seconds rather than choosing the whole-hour template.
		{`new Intl.DateTimeFormat("en", {timeZone: "America/Manaus", timeZoneName: "shortOffset"})
		    .formatToParts(Date.UTC(1900, 0, 1)).find(p => p.type === "timeZoneName").value`,
			"GMT-4:00:04"},
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
		{`new Intl.Collator("de", {usage: "search", sensitivity: "case"})
		    .compare("Aã", "Aa")`, "0"},
		{`["AE", "Ä"].sort(new Intl.Collator("de", {usage: "search"}).compare).join()`,
			"AE,Ä"},
		{`new Intl.Collator("de-u-co-phonebk", {collation: "eor"})
		    .resolvedOptions().collation`, "eor"},
		{`["z", "ä"].sort(new Intl.Collator("sv", {collation: "eor"}).compare).join()`,
			"ä,z"},
		{`["一", "丁", "阿", "八", "中", "𠀀", "A", "α", "가", "あ"]
		    .sort(new Intl.Collator("zh").compare).join(" ")`,
			"阿 八 丁 一 中 𠀀 A α 가 あ"},
		{`["一", "丁", "阿", "八", "中", "𠀀", "A", "α", "가", "あ"]
		    .sort(new Intl.Collator("zh-Hant").compare).join(" ")`,
			"一 丁 八 中 阿 𠀀 A α 가 あ"},
		{`["一", "丁", "阿", "八", "中", "𠀀", "A", "α", "가", "あ"]
		    .sort(new Intl.Collator("ja").compare).join(" ")`,
			"A あ 阿 一 中 丁 八 𠀀 α 가"},
		{`["一", "丁", "阿", "八", "中", "𠀀", "A", "α", "가", "あ"]
		    .sort(new Intl.Collator("ko").compare).join(" ")`,
			"가 阿 一 丁 中 八 𠀀 A α あ"},
		{`[new Intl.Collator("zh").resolvedOptions().collation,
		    new Intl.Collator("zh-Hant").resolvedOptions().collation].join(",")`,
			"pinyin,stroke"},
		{`["a", "ä", "â", "à", "ǎ", "á", "ā"]
		    .sort(new Intl.Collator("zh", {sensitivity: "accent"}).compare).join("")`,
			"āáǎàaâä"},
		{`["ürün", "uzun", "ışık", "iyi", "İstanbul", "izmir", "çay", "civciv", "şeker", "sen"]
		    .sort(new Intl.Collator("tr").compare).join("|")`,
			"civciv|çay|ışık|İstanbul|iyi|izmir|sen|şeker|uzun|ürün"},
		{`["base", "accent", "case", "variant"].map(sensitivity => {
		    const c = new Intl.Collator("tr", {sensitivity});
		    return [c.compare("I", "ı"), c.compare("I", "i"),
		      c.compare("İ", "i"), c.compare("ı", "i"), c.compare("İ", "I")].join("");
		  }).join("|")`, "0-10-11|0-10-11|1-11-11|1-11-11"},
		{`["file1", "file-1", "file 1"].sort(new Intl.Collator("th").compare).join("|")`,
			"file1|file-1|file 1"},
		{`"a".localeCompare("b")`, "-1"},

		// What things are called in the language requested.
		{`new Intl.DisplayNames("en", {type: "region"}).of("FR")`, "France"},
		{`new Intl.DisplayNames("en", {type: "language"}).of("de-AT")`, "Austrian German"},
		{`new Intl.DisplayNames("en", {type: "currency"}).of("EUR")`, "Euro"},
		{`new Intl.DisplayNames("fr", {type: "region"}).of("DE")`, "Allemagne"},
		{`new Intl.DisplayNames("en", {type: "region", fallback: "none"}).of("QQ")`,
			"undefined"},
		{`new Intl.DisplayNames("en", {type: "region"}).of("QQ")`, "QQ"},

		// What it says about itself, which is how a program can tell what it
		// got: a locale it has data for, or the one it formats in when it is
		// not told which.
		{`new Intl.NumberFormat("de-DE").resolvedOptions().locale`, "de-DE"},
		{`new Intl.NumberFormat("xx-YY").resolvedOptions().locale ===
		    new Intl.NumberFormat().resolvedOptions().locale`, "true"},
		{`Intl.NumberFormat.supportedLocalesOf(["de", "xx"]).join()`, "de"},
		{`Intl.getCanonicalLocales(["EN-us", "zh-hant-tw"]).join()`, "en-US,zh-Hant-TW"},
		// Replacements may cover a whole language-and-variant tag, a transform
		// field, or a Unicode setting such as a retired time-zone code.
		{`Intl.getCanonicalLocales("hy-arevmda")[0]`, "hyw"},
		{`Intl.getCanonicalLocales("und-Latn-t-und-hani-m0-names")[0]`,
			"und-Latn-t-und-hani-m0-prprname"},
		{`["cnckg", "eire", "est", "gmt0", "uct", "zulu"].map(tz =>
		    Intl.getCanonicalLocales("und-u-tz-" + tz)[0]).join()`,
			"und-u-tz-cnsha,und-u-tz-iedub,und-u-tz-papty,und-u-tz-gmt,und-u-tz-utc,und-u-tz-utc"},
		// An underscore is not a hyphen, and a tag written with one is not a
		// tag.
		{`try { Intl.getCanonicalLocales("de_de") } catch (e) { e.constructor.name }`,
			"RangeError"},

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

		// How long something took, which is a list of measurements or a
		// reading of a clock.
		{`new Intl.DurationFormat("en").format({hours: 1, minutes: 46, seconds: 40})`,
			"1 hr, 46 min, 40 sec"},
		{`new Intl.DurationFormat("en", {style: "long"})
		    .format({years: 1, months: 2, days: 3})`, "1 year, 2 months, 3 days"},
		{`new Intl.DurationFormat("en", {style: "digital"})
		    .format({hours: 1, minutes: 46, seconds: 40})`, "1:46:40"},
		{`new Intl.DurationFormat("en", {style: "digital"})
		    .format({seconds: 1, milliseconds: 500})`, "0:00:01.5"},
		{`new Intl.DurationFormat("fr", {style: "long"})
		    .format({hours: 1, minutes: 46})`, "1\u00a0heure et 46 minutes"},

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

func TestIntlLocale(t *testing.T) {
	cases := []struct{ src, want string }{
		{`new Intl.Locale("EN-us").toString()`, "en-US"},
		{`new Intl.Locale("de-latn-de-fonipa-1996-u-ca-gregory-co-phonebk-hc-h23-kf-true-kn-false-nu-latn").baseName`,
			"de-Latn-DE-1996-fonipa"},
		{`let l = new Intl.Locale("en", {script: "cyrl", region: "gb", variants: "fonipa-1996", calendar: "BUDDHIST", numeric: true});
		  [l.language, l.script, l.region, l.variants, l.calendar, l.numeric, l.toString()].join("|")`,
			"en|Cyrl|GB|1996-fonipa|buddhist|true|en-Cyrl-GB-1996-fonipa-u-ca-buddhist-kn"},
		{`new Intl.Locale("en").maximize().toString()`, "en-Latn-US"},
		{`new Intl.Locale("en-Latn-GB").minimize().toString()`, "en-GB"},
		{`new Intl.Locale("und-Thai").maximize().toString()`, "th-Thai-TH"},
		{`JSON.stringify(new Intl.Locale("en-US").getWeekInfo())`, `{"firstDay":7,"weekend":[6,7]}`},
		{`new Intl.Locale("ar").getTextInfo().direction`, "rtl"},
		{`new Intl.Locale("zh-Hans").getCollations().join(",")`, "emoji,eor,pinyin,stroke,unihan,zhuyin"},
		{`new Intl.Locale("zh-Latn").getCollations().join(",")`, "emoji,eor"},
		{`new Intl.Locale("ar-EG").getNumberingSystems()[0]`, "arab"},
		{`new Intl.Locale("ar-Latn-EG").getNumberingSystems()[0]`, "latn"},
		{`String(new Intl.Locale("en").getTimeZones())`, "undefined"},
		{`JSON.stringify(new Intl.Locale("en-QQ").getTimeZones())`, `[]`},
		{`new Intl.NumberFormat(new Intl.Locale("en-u-nu-arab")).resolvedOptions().locale`, "en-u-nu-arab"},
		{`class L extends Intl.Locale { toString() { throw new Error("unused") } }
		  Intl.getCanonicalLocales(new L("fr-u-ca-gregory"))[0]`, "fr-u-ca-gregory"},
		{`try { Intl.Locale("en") } catch (e) { e.name }`, "TypeError"},
		{`try { new Intl.Locale("bad_tag", null) } catch (e) { e.name }`, "TypeError"},
		{`try { Intl.Locale.prototype.maximize.call({}) } catch (e) { e.name }`, "TypeError"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// The language a script means when it does not say which, which is the
// machine's unless the host says otherwise.
func TestDefaultLocale(t *testing.T) {
	rt := quickjs.New(quickjs.WithLocale("de-DE"))
	defer rt.Close()

	for _, tc := range []struct{ src, want string }{
		{`new Intl.DateTimeFormat().resolvedOptions().locale`, "de-DE"},
		{`new Intl.NumberFormat().format(1234.5)`, "1.234,5"},
		{`new Intl.DateTimeFormat(undefined, {timeZone: "UTC"})
		    .format(Date.UTC(2024, 0, 5))`, "5.1.2024"},
		{`new Date(Date.UTC(2024, 0, 5)).toLocaleDateString(undefined,
		    {timeZone: "UTC", dateStyle: "full"})`, "Freitag, 5. Januar 2024"},
		// A language nothing is known about is answered with this one, which
		// is what a program that asked for it would be answered with anyway.
		{`new Intl.NumberFormat("xx-YY").resolvedOptions().locale`, "de-DE"},
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

	// And it can be changed while the runtime runs.
	rt.SetLocale("fr-FR")
	if got := rt.Locale(); got != "fr-FR" {
		t.Errorf("Locale() = %q, want fr-FR", got)
	}
	v, err := rt.Eval(`new Intl.NumberFormat().format(1234.5)`)
	if err != nil {
		t.Fatal(err)
	}
	if got := v.String(); got != "1 234,5" {
		t.Errorf("after SetLocale: %q", got)
	}
}

// A date written out ends with the name of the zone it is written in, in the
// language the runtime formats in -- which is what V8 does, and the only part
// of Date.prototype.toString that is not fixed by the specification.
func TestDateStringsNameTheZone(t *testing.T) {
	when := float64(1704412800000) // 2024-01-05T00:00:00Z
	summer := float64(1720137600000)

	for _, tc := range []struct{ locale, zone, want, inSummer string }{
		{"en-US", "America/New_York",
			"Thu Jan 04 2024 19:00:00 GMT-0500 (Eastern Standard Time)",
			"Thu Jul 04 2024 20:00:00 GMT-0400 (Eastern Daylight Time)"},
		{"de-DE", "Europe/Berlin",
			"Fri Jan 05 2024 01:00:00 GMT+0100 (Mitteleuropäische Normalzeit)",
			"Fri Jul 05 2024 02:00:00 GMT+0200 (Mitteleuropäische Sommerzeit)"},
		{"ja-JP", "Asia/Tokyo",
			"Fri Jan 05 2024 09:00:00 GMT+0900 (日本標準時)",
			"Fri Jul 05 2024 09:00:00 GMT+0900 (日本標準時)"},
		// Greenwich itself, and a zone that is nothing but an offset from it,
		// which is written the way the language writes an offset.
		{"en-US", "UTC",
			"Fri Jan 05 2024 00:00:00 GMT+0000 (Coordinated Universal Time)",
			"Fri Jul 05 2024 00:00:00 GMT+0000 (Coordinated Universal Time)"},
		{"en-US", "GMT",
			"Fri Jan 05 2024 00:00:00 GMT+0000 (Greenwich Mean Time)",
			"Fri Jul 05 2024 00:00:00 GMT+0000 (Greenwich Mean Time)"},
		{"de-DE", "Etc/GMT",
			"Fri Jan 05 2024 00:00:00 GMT+0000 (Mittlere Greenwich-Zeit)",
			"Fri Jul 05 2024 00:00:00 GMT+0000 (Mittlere Greenwich-Zeit)"},
		{"fr-FR", "Greenwich",
			"Fri Jan 05 2024 00:00:00 GMT+0000 (heure moyenne de Greenwich)",
			"Fri Jul 05 2024 00:00:00 GMT+0000 (heure moyenne de Greenwich)"},
		{"ja-JP", "Etc/Greenwich",
			"Fri Jan 05 2024 00:00:00 GMT+0000 (グリニッジ標準時)",
			"Fri Jul 05 2024 00:00:00 GMT+0000 (グリニッジ標準時)"},
		{"en-US", "GMT0",
			"Fri Jan 05 2024 00:00:00 GMT+0000 (Greenwich Mean Time)",
			"Fri Jul 05 2024 00:00:00 GMT+0000 (Greenwich Mean Time)"},
		{"en-US", "Etc/GMT+0",
			"Fri Jan 05 2024 00:00:00 GMT+0000 (Greenwich Mean Time)",
			"Fri Jul 05 2024 00:00:00 GMT+0000 (Greenwich Mean Time)"},
		{"en-US", "Etc/UTC",
			"Fri Jan 05 2024 00:00:00 GMT+0000 (Coordinated Universal Time)",
			"Fri Jul 05 2024 00:00:00 GMT+0000 (Coordinated Universal Time)"},
		{"fr-FR", "Etc/GMT+5",
			"Thu Jan 04 2024 19:00:00 GMT-0500 (UTC−05:00)",
			"Thu Jul 04 2024 19:00:00 GMT-0500 (UTC−05:00)"},
		{"fa-IR", "Etc/GMT+5",
			"Thu Jan 04 2024 19:00:00 GMT-0500 (‎−۰۵:۰۰ گرینویچ)",
			"Thu Jul 04 2024 19:00:00 GMT-0500 (‎−۰۵:۰۰ گرینویچ)"},
	} {
		zone, err := time.LoadLocation(tc.zone)
		if err != nil {
			t.Skipf("no zone files: %v", err)
		}
		rt := quickjs.New(quickjs.WithLocale(tc.locale))
		rt.SetTimeZone(zone)

		for _, when := range []struct {
			at   float64
			want string
		}{{when, tc.want}, {summer, tc.inSummer}} {
			v, err := rt.Eval(`new Date(` + strconv.FormatFloat(when.at, 'f', -1, 64) +
				`).toString()`)
			if err != nil {
				t.Fatal(err)
			}
			if got := v.String(); got != when.want {
				t.Errorf("%s in %s\n got  %q\n want %q",
					tc.locale, tc.zone, got, when.want)
			}
			// toTimeString is the same line without the date, and
			// toDateString the date without the zone.
			v, err = rt.Eval(`new Date(` + strconv.FormatFloat(when.at, 'f', -1, 64) +
				`).toTimeString()`)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := v.String(), when.want[len("Thu Jan 04 2024 "):]; got != want {
				t.Errorf("%s in %s toTimeString\n got  %q\n want %q",
					tc.locale, tc.zone, got, want)
			}
		}
		rt.Close()
	}
}

func TestDateStringsFollowNodeLegacyZoneNames(t *testing.T) {
	for _, tc := range []struct {
		locale string
		zone   string
		at     int64
		want   string
	}{
		{"en-US", "Europe/Paris", -5363409600000,
			"Wed Jan 15 1800 12:09:21 GMT+0009 (Central European Standard Time)"},
		{"en-US", "Europe/Paris", -5347771200000,
			"Tue Jul 15 1800 12:09:21 GMT+0009 (Central European Summer Time)"},
		{"fr-FR", "Europe/Paris", -5347771200000,
			"Tue Jul 15 1800 12:09:21 GMT+0009 (heure d’été d’Europe centrale)"},
		{"en-US", "America/New_York", -787665600000,
			"Mon Jan 15 1945 08:00:00 GMT-0400 (Eastern Standard Time)"},
		{"en-US", "Europe/Kyiv", -772027200000,
			"Sun Jul 15 1945 15:00:00 GMT+0300 (Eastern European Summer Time)"},
		{"en-US", "Asia/Shanghai", 648043200000,
			"Sun Jul 15 1990 21:00:00 GMT+0900 (China Daylight Time)"},
		// ICU keeps the permanent post-2026 western-Canada offsets classified
		// as daylight time, while Go's zone files classify them as standard.
		{"en-US", "America/Vancouver", 1894708800000,
			"Tue Jan 15 2030 05:00:00 GMT-0700 (Pacific Daylight Time)"},
		// Outside signed-32-bit Unix time V8 chooses the name in an equivalent
		// 2008-2035 year, while retaining the original instant's offset.
		{"en-US", "America/Vancouver", 7259371200000,
			"Wed Jan 15 2200 05:00:00 GMT-0700 (Pacific Daylight Time)"},
		{"en-US", "America/Coyhaique", 1721044800000,
			"Mon Jul 15 2024 08:00:00 GMT-0400 (GMT-03:00)"},
		{"en-US", "Africa/Casablanca", -5363409600000,
			"Wed Jan 15 1800 11:29:40 GMT-0030 (GMT+00:00)"},
		// Some localized ICU names contain their own opening parenthesis; Node
		// still appends only one final closing parenthesis.
		{"wo", "America/Chicago", 1719792000000,
			"Sun Jun 30 2024 19:00:00 GMT-0500 (CDT (waxtu bëccëgu sàntaraal)"},
	} {
		zone, err := time.LoadLocation(tc.zone)
		if err != nil {
			t.Skipf("no zone files: %v", err)
		}
		rt := quickjs.New(quickjs.WithLocale(tc.locale))
		rt.SetTimeZone(zone)
		v, err := rt.Eval(`new Date(` + strconv.FormatInt(tc.at, 10) + `).toString()`)
		if err != nil {
			rt.Close()
			t.Fatal(err)
		}
		if got := v.String(); got != tc.want {
			t.Errorf("%s in %s at %d\n got  %q\n want %q",
				tc.locale, tc.zone, tc.at, got, tc.want)
		}
		v, err = rt.Eval(`new Date(` + strconv.FormatInt(tc.at, 10) + `).toTimeString()`)
		if err != nil {
			rt.Close()
			t.Fatal(err)
		}
		wantTime := tc.want[len("Wed Jan 15 1800 "):]
		if got := v.String(); got != wantTime {
			t.Errorf("%s in %s at %d toTimeString\n got  %q\n want %q",
				tc.locale, tc.zone, tc.at, got, wantTime)
		}
		rt.Close()
	}
}

func TestWarmupDateTimeDataBeforeRuntime(t *testing.T) {
	quickjs.WarmupDateTimeData()
	quickjs.WarmupDateTimeData() // Idempotent.

	zone, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Skipf("no zone files: %v", err)
	}
	rt := quickjs.New(quickjs.WithLocale("de-DE"))
	defer rt.Close()
	rt.SetTimeZone(zone)
	v, err := rt.Eval(`[
		new Date(0).toTimeString(),
		new Intl.DateTimeFormat("de-DE", {timeZone: "Europe/Paris", timeZoneName: "long"})
			.formatToParts(Date.UTC(2025, 0, 15)).find(p => p.type === "timeZoneName").value,
		new Intl.DateTimeFormat("de-DE", {timeZone: "Europe/Paris", timeZoneName: "longGeneric"})
			.formatToParts(Date.UTC(1979, 6, 15)).find(p => p.type === "timeZoneName").value
	].join("|")`)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := v.String(), "01:00:00 GMT+0100 (Mitteleuropäische Normalzeit)|"+
		"Mitteleuropäische Normalzeit|Mitteleuropäische Zeit (Frankreich)"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
