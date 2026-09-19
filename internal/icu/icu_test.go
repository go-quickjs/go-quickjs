package icu

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHotLocaleDataStartsWithoutDecompression(t *testing.T) {
	if os.Getenv("QUICKJS_ICU_LAZY_HELPER") == "" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHotLocaleDataStartsWithoutDecompression$")
		cmd.Env = append(os.Environ(), "QUICKJS_ICU_LAZY_HELPER=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("lazy-data helper: %v\n%s", err, out)
		}
		return
	}
	if packedDecoder != nil || collationOrder.runs != nil {
		t.Fatal("compressed Intl data was initialized during package startup")
	}
	TagAliases()
	l := Resolve("de")
	_ = l.CardinalRule()
	l.PrepareDate()
	if packedDecoder != nil {
		t.Fatal("hot locale data initialized the decompressor")
	}
	if collationOrder.runs != nil {
		t.Fatal("hot locale data initialized root collation")
	}
}

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
			got[i] = l.CardinalRule().Category(n)
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
			got = append(got, l.OrdinalRule().Category(n))
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
		{"sc", "sc"}, // supported languages must not disappear during extraction
		{"ksh", "ksh"},
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
	for _, tag := range []string{"de", "de-CH", "zh-CN", "ar-EG", "kw", "haw", "sc", "ksh"} {
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

func TestLocaleInfo(t *testing.T) {
	for _, tc := range []struct {
		language, script, region string
		want                     string
	}{
		{"en", "", "", "en-Latn-US"},
		{"en", "Shaw", "", "en-Shaw-GB"},
		{"und", "Thai", "", "th-Thai-TH"},
		{"und", "Cyrl", "RO", "bg-Cyrl-RO"},
		{"zz", "", "", "zz--"},
	} {
		language, script, region := AddLikelySubtags(tc.language, tc.script, tc.region)
		got := language + "-" + script + "-" + region
		if got != tc.want {
			t.Errorf("AddLikelySubtags(%q, %q, %q) = %q, want %q",
				tc.language, tc.script, tc.region, got, tc.want)
		}
	}
	us := TerritoryInfoFor("US")
	if us.FirstDay != 7 || len(us.Weekend) != 2 || len(us.TimeZones) == 0 {
		t.Fatalf("US territory info is incomplete: %+v", us)
	}
	if got := LocaleHourCycles("fr", "CA"); len(got) != 1 || got[0] != "h23" {
		t.Fatalf("French Canadian hour cycles = %v, want [h23]", got)
	}
	if got := ScriptDirection("Arab"); got != "rtl" {
		t.Fatalf("Arabic direction = %q, want rtl", got)
	}
	if got := strings.Join(LocaleCollations("zh", "Hans"), ","); got != "emoji,eor,pinyin,stroke,unihan,zhuyin" {
		t.Fatalf("Simplified Chinese collations = %q", got)
	}
	if got := strings.Join(LocaleCollations("zh", "Latn"), ","); got != "emoji,eor" {
		t.Fatalf("Latin Chinese collations = %q", got)
	}
	for _, tc := range []struct{ language, script, region, want string }{
		{"ar", "", "EG", "arab"},
		{"ar", "Latn", "EG", "latn"},
		{"pa", "Arab", "IN", "arabext"},
		{"sd", "", "IN", "latn"},
		{"sd", "Arab", "IN", "arab"},
	} {
		if got := LocaleNumberingSystem(tc.language, tc.script, tc.region); got != tc.want {
			t.Errorf("numbering for %s-%s-%s = %q, want %q", tc.language, tc.script, tc.region, got, tc.want)
		}
	}
	if _, ok := TerritoryInfoExact("QQ"); ok {
		t.Fatal("unknown territory QQ unexpectedly has locale information")
	}
	if got := tagFromWindows("hu-HU_technl"); got != "hu-HU" {
		t.Fatalf("Windows alternate-sort locale = %q, want hu-HU", got)
	}
	if got := tagFromWindows("zh-Hans-CN"); got != "zh-Hans-CN" {
		t.Fatalf("Windows script locale = %q, want zh-Hans-CN", got)
	}
}

// Each locale is directly indexed and parsed the first time it is asked for.
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
		l.PrepareDate()
		if len(l.Months) != 12 || len(l.Days) != 7 {
			t.Errorf("%s: %d months, %d days", tag, len(l.Months), len(l.Days))
		}
		if l.Decimal == "" || l.Group == "" {
			t.Errorf("%s: no separators", tag)
		}
		if l.DatePatterns[0] == "" || l.TimePatterns[0] == "" {
			t.Errorf("%s: no patterns", tag)
		}
		if len(l.CardinalRule().Categories) == 0 {
			t.Errorf("%s: no plural categories", tag)
		}
	}
}

func TestLocaleSectionsLoadOnDemand(t *testing.T) {
	record, ok := localeRecord(indexOf("en"))
	if !ok {
		t.Fatal("English locale record is missing")
	}
	l := decode("en", record)
	if l.Decimal == "" || l.Numbering == "" {
		t.Fatal("the directly indexed locale header was not parsed")
	}
	if l.DatePatterns[0] != "" || l.Currencies != nil || l.Cardinal.Categories != nil ||
		l.Ordinal.Categories != nil || l.Lists != nil || l.Relative != nil || l.Short != nil {
		t.Fatal("a cold locale section was parsed eagerly")
	}

	l.PrepareDate()
	if len(l.Months) != 12 || l.DatePatterns[0] == "" {
		t.Fatal("date section did not load")
	}
	if symbol, ok := l.CurrencySymbol("USD"); !ok || symbol == "" {
		t.Fatal("currency section did not load")
	}
	if len(l.CardinalRule().Categories) == 0 || len(l.OrdinalRule().Categories) == 0 {
		t.Fatal("plural sections did not load")
	}
	if _, ok := l.ListPatternFor("conjunction-long"); !ok {
		t.Fatal("list section did not load")
	}
	if _, ok := l.RelativeUnitFor("day"); !ok {
		t.Fatal("relative-time section did not load")
	}
	if _, divisor, _ := l.Compact(1000, false); divisor == 1 {
		t.Fatal("compact-number section did not load")
	}
	fr := Resolve("fr")
	if _, divisor, _ := fr.Compact(1.5e6, false); divisor != 1e6 {
		t.Fatalf("French compact divisor = %v, want 1000000", divisor)
	}
	if got := fr.CardinalRule().CategoryOf("1500000", "", 1.5e6, 6); got != "many" {
		t.Fatalf("French compact plural category = %q, want many", got)
	}
	if _, ok := localeRecord(-1); ok {
		t.Fatal("an invalid locale index unexpectedly resolved")
	}
}

func TestPackedBlockTableBoundsAndConcurrentLoad(t *testing.T) {
	if _, ok := (*packedBlockTable)(nil).record([3]uint32{}); ok {
		t.Fatal("a nil block table returned a record")
	}
	bad := newPackedBlockTable(nil, [][2]uint32{{0, 1}})
	if _, ok := bad.record([3]uint32{1, 0, 0}); ok {
		t.Error("an invalid block number returned a record")
	}
	if _, ok := bad.record([3]uint32{0, 1, 0}); ok {
		t.Error("inverted record bounds returned a record")
	}
	if _, ok := bad.record([3]uint32{0, 0, 0}); ok {
		t.Error("out-of-range packed bounds returned a record")
	}
	inverted := newPackedBlockTable(nil, [][2]uint32{{1, 0}})
	if _, ok := inverted.record([3]uint32{0, 0, 0}); ok {
		t.Error("inverted packed bounds returned a record")
	}

	var first, second [3]uint32
	found := false
	for i := 1; i < len(calendarNameEntriesPacked); i++ {
		if calendarNameEntriesPacked[i-1][0] == calendarNameEntriesPacked[i][0] {
			first, second = calendarNameEntriesPacked[i-1], calendarNameEntriesPacked[i]
			found = true
			break
		}
	}
	if !found {
		t.Fatal("generated calendar records do not share a block")
	}
	table := newPackedBlockTable(packedTables, calendarNameBlocksPacked[:])
	var wg sync.WaitGroup
	errors := make(chan string, 32)
	for i := range 32 {
		ref := first
		if i%2 != 0 {
			ref = second
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			text, ok := table.record(ref)
			if !ok || len(text) != int(ref[2]-ref[1]) {
				errors <- text
			}
		}()
	}
	wg.Wait()
	close(errors)
	for text := range errors {
		t.Errorf("concurrent block lookup returned %d bytes", len(text))
	}
	if _, ok := table.record([3]uint32{first[0], 0, ^uint32(0)}); ok {
		t.Fatal("an out-of-range record end was accepted")
	}
}

func TestWarmupDateTimeData(t *testing.T) {
	WarmupDateTimeData()
	for _, tag := range tags {
		if _, ok := decoded[tag]; !ok {
			t.Errorf("locale %s was not warmed", tag)
		}
	}
	for name, table := range map[string]*zoneTable{
		"seasonal": &seasonNames,
		"generic":  &genericNames,
		"legacy":   &legacyNames.names,
	} {
		if got, want := len(table.decoded), len(table.rows); got != want {
			t.Errorf("warmed %s rows = %d, want %d", name, got, want)
		}
	}
	if got, want := len(historicalNames.decoded), len(historicalNames.rows); got != want {
		t.Errorf("warmed historical rows = %d, want %d", got, want)
	}
	periodReader := zoneTableReader{data: historicalNames.encodedPeriods}
	periodCount, ok := periodReader.uvarint()
	if !ok || len(historicalNames.periods) != int(periodCount) {
		t.Errorf("warmed historical timelines = %d, want %d",
			len(historicalNames.periods), periodCount)
	}
	changeReader := zoneTableReader{data: legacyNames.encodedChanges}
	changeCount, ok := changeReader.uvarint()
	if !ok || len(legacyNames.changes) != int(changeCount) {
		t.Errorf("warmed legacy timelines = %d, want %d",
			len(legacyNames.changes), changeCount)
	}
	if len(calendarEntries) == 0 || len(calendarIndex) == 0 ||
		len(calendarFormatEntries) == 0 || len(calendarFormatIndex) == 0 ||
		len(monthTables) == 0 {
		t.Error("calendar data was not warmed")
	}
	for i, entry := range calendarEntries {
		if entry == nil {
			t.Errorf("calendar name entry %d was not warmed", i)
		}
	}
	for i, entry := range calendarFormatEntries {
		if entry == nil {
			t.Errorf("calendar format entry %d was not warmed", i)
		}
	}
}

func TestBundledTimeZoneMatchesICURelease(t *testing.T) {
	for _, name := range Zones() {
		target := name
		if alias, ok := ZoneTarget(name); ok {
			target = alias
		}
		if _, err := LoadLocation(target); err != nil {
			t.Errorf("load %s: %v", name, err)
		}
	}

	zone, err := LoadLocation("America/Vancouver")
	if err != nil {
		t.Fatal(err)
	}
	instant := time.Date(2030, time.January, 15, 12, 0, 0, 0, time.UTC).In(zone)
	name, offset := instant.Zone()
	if got, want := instant.Format("2006-01-02 15:04:05 -0700"), "2030-01-15 05:00:00 -0700"; got != want {
		t.Fatalf("tzdata 2026c Vancouver rule: got %q, want %q (%s, %d)", got, want, name, offset)
	}
}

func TestZoneTableConcurrentFirstLoad(t *testing.T) {
	table := zoneTable{packed: zoneSeasonPacked}
	var wg sync.WaitGroup
	errors := make(chan string, 32)
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if name, ok := table.entry("de", "Europe/Berlin"); !ok || name == "" {
				errors <- name
			}
		}()
	}
	wg.Wait()
	close(errors)
	for name := range errors {
		t.Errorf("concurrent lookup returned %q", name)
	}
}

func TestCJKCollationOrders(t *testing.T) {
	for tag, want := range map[string]string{
		"zh": "pinyin", "zh-Hans-CN": "pinyin", "zh-Hant": "stroke",
		"zh-Hans-TW": "pinyin", "zh-Hant-CN": "stroke", "zh-Latn-TW": "pinyin",
		"zh_TW": "stroke", "zh-HK": "stroke", "zh-MO": "stroke", "ja": "default",
	} {
		if got := DefaultCollation(tag); got != want {
			t.Errorf("DefaultCollation(%q) = %q, want %q", tag, got, want)
		}
	}
	cases := []struct {
		tag, collation, want string
	}{
		{"zh", "default", "阿 八 丁 一 中 𠀀 A α 가 あ ア"},
		{"zh-Hant", "default", "一 丁 八 中 阿 𠀀 A α 가 あ ア"},
		{"zh", "unihan", "一 丁 𠀀 中 八 阿 A α 가 あ ア"},
		{"zh", "zhuyin", "八 丁 中 阿 一 𠀀 A α 가 あ ア"},
		{"ja", "default", "A あ ア 阿 一 中 丁 八 𠀀 α 가"},
		{"ja", "unihan", "A あ ア 一 丁 𠀀 中 八 阿 α 가"},
		{"ko", "default", "가 阿 一 丁 中 八 𠀀 A α あ ア"},
		{"ko", "unihan", "가 一 丁 𠀀 中 八 阿 A α あ ア"},
		{"ko", "searchjl", "A α 가 あ ア 一 丁 𠀀 中 八 阿"},
	}
	const source = "一 丁 阿 八 中 𠀀 A α 가 あ ア"
	for _, tc := range cases {
		locale := Resolve(tc.tag)
		if locale == nil {
			t.Fatalf("Resolve(%q) returned nil", tc.tag)
		}
		words := strings.Fields(source)
		sort.SliceStable(words, func(i, j int) bool {
			return locale.CompareCollation(words[i], words[j], Primary, false, false,
				false, tc.collation) < 0
		})
		if got := strings.Join(words, " "); got != tc.want {
			t.Errorf("%s/%s order = %q, want %q", tc.tag, tc.collation, got, tc.want)
		}
	}
	plane3 := []rune{'一', '丁', '龘', 0x20000, 0x2ee5d, 0x30000, 0x31350, 0x323af, 0x33479}
	for _, tc := range []struct {
		tag, collation, want string
	}{
		{"zh", "default", "9f98,4e01,4e00,20000,30000,31350,33479,2ee5d,323af"},
		{"zh", "unihan", "4e00,4e01,20000,30000,31350,33479,2ee5d,9f98,323af"},
		{"ja", "default", "4e00,4e01,20000,30000,31350,33479,2ee5d,9f98,323af"},
	} {
		locale := Resolve(tc.tag)
		points := append([]rune(nil), plane3...)
		sort.SliceStable(points, func(i, j int) bool {
			return locale.CompareCollation(string(points[i]), string(points[j]), Primary,
				false, false, false, tc.collation) < 0
		})
		got := make([]string, len(points))
		for i, point := range points {
			got[i] = fmt.Sprintf("%x", point)
		}
		if joined := strings.Join(got, ","); joined != tc.want {
			t.Errorf("%s/%s plane-3 order = %s, want %s", tc.tag, tc.collation,
				joined, tc.want)
		}
	}

	ko := Resolve("ko")
	for _, other := range []string{"까", "각", "간", "개", "갸"} {
		if got := ko.CompareCollation("가", other, Primary, false, false, false,
			"searchjl"); got != 0 {
			t.Errorf("ko/searchjl compare(가, %s) = %d, want 0", other, got)
		}
	}
	ja := Resolve("ja")
	for _, pair := range [][2]string{{"あ", "ア"}, {"ア", "ｱ"}} {
		if got := ja.CompareCollation(pair[0], pair[1], Tertiary, false, false, false,
			"default"); got != 0 {
			t.Errorf("ja compare(%s, %s) = %d, want 0", pair[0], pair[1], got)
		}
	}
	if got := ja.CompareCollation("や", "ゃ", Tertiary, false, false, false,
		"default"); got <= 0 {
		t.Errorf("ja compare(や, ゃ) = %d, want positive", got)
	}
	if got := ja.CompareCollation("は", "ば", Secondary, false, false, false,
		"default"); got >= 0 {
		t.Errorf("ja compare(は, ば) = %d, want negative", got)
	}
}
