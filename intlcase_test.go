package quickjs_test

import "testing"

// TestIntlOptionsCaseIsASCII pins that Intl's options, tags and codes are
// case-insensitive in ASCII alone, as V8's are. strings.ToLower had taken
// U+0130 LATIN CAPITAL LETTER I WITH DOT ABOVE to "i", so that firstDayOfWeek
// "fr\u0130" passed for "fri" and the variant "pos\u0130x" for "posix"; a character
// outside ASCII is now what it is, and the value it is in is rejected with
// V8's error, while ASCII in any case is still taken.
func TestIntlOptionsCaseIsASCII(t *testing.T) {
	t.Parallel()
	const try = `function t(f) { try { return String(f()) } catch (e) { return e.name + ": " + e.message } }
`
	evalCases(t, []struct{ src, want string }{
		{try + `t(() => new Intl.Locale("en", {firstDayOfWeek: "fr\u0130"}).firstDayOfWeek)`,
			"RangeError: Incorrect locale information provided"},
		{try + `t(() => new Intl.Locale("en", {firstDayOfWeek: "\u212Aabc"}).firstDayOfWeek)`,
			"RangeError: Incorrect locale information provided"},
		{try + `t(() => new Intl.Locale("en", {firstDayOfWeek: "\u017Fun"}).firstDayOfWeek)`,
			"RangeError: Incorrect locale information provided"},
		{try + `[t(() => new Intl.Locale("en", {firstDayOfWeek: "FrI"}).firstDayOfWeek),
		   t(() => new Intl.Locale("en", {firstDayOfWeek: "MON"}).firstDayOfWeek)].join()`, "fri,mon"},
		{try + `t(() => new Intl.Locale("en", {variants: "fon\u0130pa"}).toString())`,
			"RangeError: Incorrect locale information provided"},
		{try + `t(() => new Intl.Locale("en", {variants: "FONIPA"}).toString())`, "en-fonipa"},
		{try + `t(() => new Intl.Locale("en", {calendar: "buddh\u0130st"}).calendar)`,
			"RangeError: Incorrect locale information provided"},
		{try + `t(() => new Intl.Locale("en", {calendar: "BUDDHIST"}).calendar)`, "buddhist"},
		{try + `t(() => new Intl.NumberFormat("en", {style: "currency", currency: "u\u017Fd"}))`,
			"RangeError: Invalid currency code : u\u017Fd"},
		{try + `t(() => new Intl.NumberFormat("en", {style: "currency", currency: "uSd"}).resolvedOptions().currency)`,
			"USD"},
		{try + `t(() => new Intl.DisplayNames("en", {type: "region"}).of("u\u017F"))`, "RangeError: invalid_argument"},
		{try + `t(() => new Intl.DisplayNames("en", {type: "script"}).of("lat\u0130"))`, "RangeError: invalid_argument"},
		{try + `t(() => new Intl.DisplayNames("en", {type: "calendar"}).of("gregor\u0131"))`, "RangeError: invalid_argument"},
		{try + `[new Intl.DisplayNames("en", {type: "region"}).of("uS"),
		   new Intl.DisplayNames("en", {type: "script"}).of("LATN"),
		   new Intl.DisplayNames("en", {type: "currency"}).of("eUr")].join()`, "United States,Latin,Euro"},
	})
}
