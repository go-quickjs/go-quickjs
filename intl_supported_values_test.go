package quickjs_test

import "testing"

func TestIntlSupportedValuesOf(t *testing.T) {
	cases := []struct{ src, want string }{
		{`Intl.supportedValuesOf("calendar").join(",")`,
			"buddhist,chinese,coptic,dangi,ethioaa,ethiopic,gregory,hebrew,indian," +
				"islamic-civil,islamic-tbla,islamic-umalqura,iso8601,japanese,persian,roc"},
		{`Intl.supportedValuesOf("collation").join(",")`,
			"eor,phonebk,pinyin,searchjl,stroke,unihan,zhuyin"},
		{`Intl.supportedValuesOf("unit").length`, "45"},
		{`Intl.supportedValuesOf("numberingSystem").includes("latn")`, "true"},
		{`["Etc/GMT+12", "Etc/GMT-14", "UTC"].every(zone =>
			Intl.supportedValuesOf("timeZone").includes(zone))`, "true"},
		{`["AED", "USD", "XCD"].every(currency =>
			Intl.supportedValuesOf("currency").includes(currency))`, "true"},
		{`(() => {
			for (const key of ["calendar", "collation", "currency", "numberingSystem", "timeZone", "unit"]) {
				const first = Intl.supportedValuesOf(key);
				const second = Intl.supportedValuesOf(key);
				if (first === second || first.join() !== [...first].sort().join() ||
					new Set(first).size !== first.length) return false;
			}
			return true;
		})()`, "true"},
		{`(() => { try { Intl.supportedValuesOf("units") } catch (error) {
			return error instanceof RangeError; } return false })()`, "true"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}
