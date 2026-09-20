package quickjs_test

import (
	"testing"

	"github.com/go-quickjs/go-quickjs"
)

func TestTemporalInstantConstruction(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	tests := []struct {
		source string
		want   string
	}{
		{`typeof Temporal + "," + typeof Temporal.Instant + "," + typeof Instant`, "object,function,undefined"},
		{`Object.prototype.toString.call(Temporal)`, "[object Temporal]"},
		{`Object.prototype.toString.call(Temporal.Instant.prototype)`, "[object Temporal.Instant]"},
		{`new Temporal.Instant(0n).toString()`, "1970-01-01T00:00:00Z"},
		{`new Temporal.Instant(-1n).toString()`, "1969-12-31T23:59:59.999999999Z"},
		{`new Temporal.Instant("1").toString()`, "1970-01-01T00:00:00.000000001Z"},
		{`Temporal.Instant.fromEpochMilliseconds(-1).toString()`, "1969-12-31T23:59:59.999Z"},
		{`Temporal.Instant.fromEpochNanoseconds(1234567890n).toString()`, "1970-01-01T00:00:01.23456789Z"},
		{`Temporal.Instant.from("2000-02-29T12:34:56.123400000+01:30").toString()`, "2000-02-29T11:04:56.1234Z"},
		{`Temporal.Instant.from({toString() { return "2000-01-01T00:00Z" }}).toJSON()`, "2000-01-01T00:00:00Z"},
		{`new Temporal.Instant(-1n).epochMilliseconds`, "-1"},
		{`new Temporal.Instant(-1n).epochNanoseconds.toString()`, "-1"},
		{`Temporal.Instant.compare("1970-01-01T00:00Z", "1970-01-01T00:00:00.000000001Z")`, "-1"},
		{`Temporal.Instant.from("1970-01-01T00:00Z").equals(new Temporal.Instant(0n))`, "true"},
		{`new Temporal.Instant(30_123_400_000n).toString({fractionalSecondDigits: 6})`, "1970-01-01T00:00:30.123400Z"},
		{`new Temporal.Instant(56_789_999_999n).toString({smallestUnit: "milliseconds"})`, "1970-01-01T00:00:56.789Z"},
		{`new Temporal.Instant(0n).toString({timeZone: "America/New_York"})`, "1969-12-31T19:00:00-05:00"},
		{`new Temporal.Instant(0n).toZonedDateTimeISO("uTc").toString()`, "1970-01-01T00:00:00+00:00[UTC]"},
		{`new Temporal.Instant(0n).toZonedDateTimeISO("2021-08-19T17:30-07:00[UTC]").timeZoneId`, "UTC"},
		{`Temporal.Instant.from("2000-02-29T12:34:56.123456789Z").toLocaleString("en-US", {timeZone: "UTC"})`, "2/29/2000, 12:34:56 PM"},
		{`Temporal.Instant.from("2000-02-29T12:34:56.123456789Z").toLocaleString("en-US", {timeZone: "America/New_York", timeZoneName: "long"})`, "2/29/2000, 7:34:56 AM Eastern Standard Time"},
		{`new Temporal.Instant(0n).toString({timeZone: "Africa/Monrovia"})`, "1969-12-31T23:15:30-00:45"},
		{`new Intl.DateTimeFormat("en-US", {timeZone: "UTC"}).format(Temporal.Instant.from("2000-02-29T12:34:56Z"))`, "2/29/2000, 12:34:56 PM"},
		{`class I extends Temporal.Instant {}; Object.getPrototypeOf(new I(0n)) === I.prototype`, "true"},
	}
	for _, test := range tests {
		if got := evalString(t, rt, test.source); got != test.want {
			t.Errorf("%s\n got: %s\nwant: %s", test.source, got, test.want)
		}
	}
}

func TestTemporalInstantErrors(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	tests := []struct {
		source string
		want   string
	}{
		{`try { Temporal.Instant(0n) } catch (e) { e.name }`, "TypeError"},
		{`try { new Temporal.Instant(0) } catch (e) { e.name }`, "TypeError"},
		{`try { Temporal.Instant.from(0n) } catch (e) { e.name }`, "TypeError"},
		{`try { Temporal.Instant.from("not an instant") } catch (e) { e.name }`, "RangeError"},
		{`try { Temporal.Instant.fromEpochMilliseconds(1.5) } catch (e) { e.name }`, "RangeError"},
		{`try { Temporal.Instant.fromEpochNanoseconds(8640000000000000000001n) } catch (e) { e.name }`, "RangeError"},
		{`try { Temporal.Instant.prototype.toString.call({}) } catch (e) { e.name }`, "TypeError"},
		{`try { new Temporal.Instant(0n).toString({smallestUnit: "day"}) } catch (e) { e.name }`, "RangeError"},
		{`try { new Temporal.Instant(0n).toString({timeZone: "Mars/Olympus_Mons"}) } catch (e) { e.name }`, "RangeError"},
		{`try { new Temporal.Instant(0n).toZonedDateTimeISO() } catch (e) { e.name }`, "TypeError"},
		{`try { new Temporal.Instant(0n).valueOf() } catch (e) { e.name }`, "TypeError"},
	}
	for _, test := range tests {
		if got := evalString(t, rt, test.source); got != test.want {
			t.Errorf("%s\n got: %s\nwant: %s", test.source, got, test.want)
		}
	}
}

func TestTemporalIntlDateTimeFormat(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	tests := []struct{ source, want string }{
		{`new Intl.DateTimeFormat("en-US", { era: "narrow", timeZone: "UTC" }).format(new Temporal.Instant(0n))`, "1/1/1970 AD, 12:00:00 AM"},
		{`new Intl.DateTimeFormat("en-US", { era: "narrow", timeZone: "UTC" }).format(new Temporal.PlainDate(2025, 11, 4))`, "11/4/2025 AD"},
		{`new Intl.DateTimeFormat("en-US", { year: "numeric", month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit", timeZone: "America/New_York" }).format(new Temporal.PlainDate(2000, 2, 29))`, "02/29/2000"},
		{`new Intl.DateTimeFormat("en-US", { year: "numeric", month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit", timeZone: "America/New_York" }).format(new Temporal.PlainTime(12, 34))`, "12:34 PM"},
		{`new Intl.DateTimeFormat("en-US", { timeZoneName: "long", timeZone: "America/New_York" }).formatToParts(new Temporal.PlainTime(12, 34)).some(part => part.type === "timeZoneName")`, "false"},
		{`new Intl.DateTimeFormat("en", { calendar: "chinese", timeZone: "UTC",
			year: "numeric", month: "numeric", day: "numeric" }).formatToParts(
			Date.UTC(2047, 5, 30)).find(part => part.type === "month").value`, "5bis"},
		{`new Temporal.PlainDate(5808, 2, 29, "hebrew").monthCode`, "M05L"},
		{`new Temporal.PlainDate(2011, 12, 30).toLocaleString("en-US", {timeZone: "Pacific/Apia"})`, "12/30/2011"},
		{`try {
			new Temporal.PlainDate(2000, 5, 2, "hebrew").toLocaleString("en-US-u-ca-gregory");
		} catch (error) { error.name }`, "RangeError"},
		{`new Intl.DateTimeFormat("en-US", { timeZone: "Pacific/Apia" }).formatRange(
			new Temporal.PlainDateTime(2021, 8, 4, 0, 30, 45),
			new Temporal.PlainDateTime(2021, 8, 4, 23, 30, 45))`,
			"8/4/2021, 12:30:45 AM\u2009–\u200911:30:45 PM"},
		{`(() => {
			let calls = 0;
			const invalid = { valueOf() { calls++; return NaN; } };
			try {
				new Intl.DateTimeFormat().formatRange(invalid, new Temporal.PlainDate(1970, 1, 1));
			} catch (error) {
				return error.name + "," + calls;
			}
		})()`, "TypeError,1"},
	}
	for _, test := range tests {
		if got := evalString(t, rt, test.source); got != test.want {
			t.Errorf("%s\n got: %s\nwant: %s", test.source, got, test.want)
		}
	}
	if got := evalString(t, rt, `try {
		new Intl.DateTimeFormat("en-US").format(new Temporal.ZonedDateTime(0n, "UTC"));
	} catch (e) { e.name }`); got != "TypeError" {
		t.Fatalf("formatting a ZonedDateTime threw %s", got)
	}
}

func TestTemporalZonedDateTimeFoundation(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	tests := []struct {
		source string
		want   string
	}{
		{`new Temporal.ZonedDateTime(0n, "UTC").toString()`, "1970-01-01T00:00:00+00:00[UTC]"},
		{`new Temporal.ZonedDateTime(0n, "+01:30").toString()`, "1970-01-01T01:30:00+01:30[+01:30]"},
		{`new Temporal.ZonedDateTime(0n, "America/New_York").toString()`, "1969-12-31T19:00:00-05:00[America/New_York]"},
		{`new Temporal.ZonedDateTime(1n, "UTC", "gregory").toString()`, "1970-01-01T00:00:00.000000001+00:00[UTC][u-ca=gregory]"},
		{`new Temporal.ZonedDateTime(-1n, "UTC").epochMilliseconds`, "-1"},
		{`new Temporal.ZonedDateTime(-1n, "UTC").epochNanoseconds.toString()`, "-1"},
		{`Temporal.Instant.from(new Temporal.ZonedDateTime(123n, "UTC")).epochNanoseconds.toString()`, "123"},
		{`Temporal.Instant.compare(new Temporal.ZonedDateTime(123n, "UTC"), new Temporal.Instant(124n))`, "-1"},
		{`new Temporal.Instant(123n).equals(new Temporal.ZonedDateTime(123n, "UTC"))`, "true"},
		{`(() => { let z = Temporal.ZonedDateTime.from("2000-02-29T12:34:56.123456789Z[UTC]"); return [z.year, z.month, z.monthCode, z.day, z.hour, z.minute, z.second, z.millisecond, z.microsecond, z.nanosecond].join(",") })()`, "2000,2,M02,29,12,34,56,123,456,789"},
		{`(() => { let z = Temporal.ZonedDateTime.from("2000-02-29T12:34:56Z[UTC]"); return [z.dayOfWeek, z.dayOfYear, z.weekOfYear, z.yearOfWeek, z.daysInWeek, z.daysInMonth, z.daysInYear, z.monthsInYear, z.inLeapYear].join(",") })()`, "2,60,9,2000,7,29,366,12,true"},
		{`(() => { let z = Temporal.ZonedDateTime.from("2000-02-29T12:34:56.123456789Z[UTC]"); return [z.offset, z.offsetNanoseconds, z.toInstant(), z.toPlainDate(), z.toPlainTime(), z.toPlainDateTime()].join("|") })()`, "+00:00|0|2000-02-29T12:34:56.123456789Z|2000-02-29|12:34:56.123456789|2000-02-29T12:34:56.123456789"},
		{`Temporal.ZonedDateTime.from("2024-03-10T12:00-04:00[America/New_York]").hoursInDay`, "23"},
		{`Temporal.ZonedDateTime.from("2024-11-03T12:00-05:00[America/New_York]").hoursInDay`, "25"},
		{`Temporal.ZonedDateTime.from("2024-03-10T12:00-04:00[America/New_York]").startOfDay().toString()`, "2024-03-10T00:00:00-05:00[America/New_York]"},
		{`Temporal.ZonedDateTime.from("2000-02-29T12:00Z[UTC]").withTimeZone("-08:00").toString()`, "2000-02-29T04:00:00-08:00[-08:00]"},
		{`(() => { let z = Temporal.ZonedDateTime.from("2000-02-29T12:00Z[UTC]").withCalendar("gregory"); return [z.calendarId, z.year, z.era, z.eraYear].join(",") })()`, "gregory,2000,ce,2000"},
		{`Temporal.ZonedDateTime.from({year: 2021, month: 2, day: 31, hour: 25, minute: 70, timeZone: "UTC"}).toString()`, "2021-02-28T23:59:00+00:00[UTC]"},
		{`Temporal.ZonedDateTime.from({year: 2021, monthCode: "M11", day: 7, hour: 1, minute: 30, offset: "-05:00", timeZone: "America/New_York"}).toString()`, "2021-11-07T01:30:00-05:00[America/New_York]"},
		{`Temporal.ZonedDateTime.from({year: 2021, month: 3, day: 14, hour: 2, minute: 30, timeZone: "America/New_York"}, {disambiguation: "earlier"}).toString()`, "2021-03-14T01:30:00-05:00[America/New_York]"},
		{`Temporal.ZonedDateTime.from("2020-03-08T01:00-04:00[UTC]", {offset: "use"}).toInstant().toString()`, "2020-03-08T05:00:00Z"},
		{`Temporal.ZonedDateTime.from({year: 2000, month: 5, day: 2, timeZone: new Temporal.ZonedDateTime(0n, "UTC")}).timeZoneId`, "UTC"},
		{`Temporal.ZonedDateTime.compare("1970-01-01T00:00Z[UTC]", "1969-12-31T19:00-05:00[America/New_York]")`, "0"},
		{`Temporal.ZonedDateTime.from("2024-03-09T12:00-05:00[America/New_York]").add({ days: 1 }).toString()`, "2024-03-10T12:00:00-04:00[America/New_York]"},
		{`Temporal.ZonedDateTime.from("2024-03-09T12:00-05:00[America/New_York]").add({ hours: 24 }).toString()`, "2024-03-10T13:00:00-04:00[America/New_York]"},
		{`Temporal.ZonedDateTime.from("2021-11-07T01:30-05:00[America/New_York]").add({ nanoseconds: 1 }).toString()`, "2021-11-07T01:30:00.000000001-05:00[America/New_York]"},
		{`Temporal.ZonedDateTime.from("2024-03-10T12:00-04:00[America/New_York]").subtract({ days: 1 }).toString()`, "2024-03-09T12:00:00-05:00[America/New_York]"},
		{`Temporal.ZonedDateTime.from("2021-11-07T12:00-05:00[America/New_York]").withPlainTime("01:30").toString()`, "2021-11-07T01:30:00-04:00[America/New_York]"},
		{`Temporal.ZonedDateTime.from("2024-03-10T12:00-04:00[America/New_York]").withPlainTime().toString()`, "2024-03-10T00:00:00-05:00[America/New_York]"},
		{`Temporal.ZonedDateTime.from("2021-01-31T12:34:56Z[UTC]").with({ month: 2 }).toString()`, "2021-02-28T12:34:56+00:00[UTC]"},
		{`(() => { const z = Temporal.ZonedDateTime.from("2021-01-31T12:34:56Z[UTC]"); z.with({ month: 2 }); return z.toString(); })()`, "2021-01-31T12:34:56+00:00[UTC]"},
		{`new Temporal.ZonedDateTime(0n, "America/New_York").getTimeZoneTransition("next").toString()`, "1970-04-26T03:00:00-04:00[America/New_York]"},
		{`new Temporal.ZonedDateTime(0n, "America/New_York").getTimeZoneTransition({ direction: "previous" }).toString()`, "1969-10-26T01:00:00-05:00[America/New_York]"},
		{`new Temporal.ZonedDateTime(9961200000000001n, "America/New_York").getTimeZoneTransition("previous").toString()`, "1970-04-26T03:00:00-04:00[America/New_York]"},
		{`new Temporal.ZonedDateTime(0n, "+01:30").getTimeZoneTransition("next")`, "null"},
		{`Temporal.ZonedDateTime.from("2024-03-10T12:00-04:00[America/New_York]").round({smallestUnit: "day"}).toString()`, "2024-03-10T00:00:00-05:00[America/New_York]"},
		{`Temporal.ZonedDateTime.from("2024-11-03T12:00-05:00[America/New_York]").round({smallestUnit: "day"}).toString()`, "2024-11-04T00:00:00-05:00[America/New_York]"},
		{`Temporal.ZonedDateTime.from("2021-11-07T01:29:45-05:00[America/New_York]").round({smallestUnit: "hour"}).toString()`, "2021-11-07T01:00:00-05:00[America/New_York]"},
		{`new Temporal.ZonedDateTime(3661987654321n, "UTC").toString({calendarName: "critical", offset: "never", timeZoneName: "critical", smallestUnit: "millisecond"})`, "1970-01-01T01:01:01.987[!UTC][!u-ca=iso8601]"},
		{`Temporal.ZonedDateTime.from("2024-03-09T12:00-05:00[America/New_York]").until("2024-03-10T12:00-04:00[America/New_York]").toString()`, "PT23H"},
		{`Temporal.ZonedDateTime.from("2024-03-09T12:00-05:00[America/New_York]").until("2024-03-10T12:00-04:00[America/New_York]", {largestUnit: "day"}).toString()`, "P1D"},
		{`Temporal.ZonedDateTime.from("2024-03-10T12:00-04:00[America/New_York]").since("2024-03-09T12:00-05:00[America/New_York]", {largestUnit: "day"}).toString()`, "P1D"},
	}
	for _, test := range tests {
		if got := evalString(t, rt, test.source); got != test.want {
			t.Errorf("%s\n got: %s\nwant: %s", test.source, got, test.want)
		}
	}
	for _, source := range []string{
		`Temporal.ZonedDateTime.from({year: 2021, monthCode: "M12", month: 11, day: 7, timeZone: "UTC"})`,
		`Temporal.ZonedDateTime.from({year: 2021, month: 3, day: 8, hour: 1, offset: "-04:00", timeZone: "UTC"})`,
		`Temporal.ZonedDateTime.from({year: 2021, month: 2, day: 29, timeZone: "UTC"}, {overflow: "reject"})`,
		`Temporal.ZonedDateTime.from("2021-01-31T12:00Z[UTC]").add({months: 1}, {overflow: "reject"})`,
		`new Temporal.ZonedDateTime(0n, "UTC", "İSO8601")`,
		`new Temporal.ZonedDateTime(0n, "UTC", "1997-12-04[u-ca=iso8601]")`,
	} {
		if got := evalString(t, rt, `try { `+source+` } catch (e) { e.name }`); got != "RangeError" {
			t.Errorf("%s threw %s", source, got)
		}
	}
	if got := evalString(t, rt, `try {
		new Temporal.ZonedDateTime(0n, "UTC").with({calendar: "iso8601"});
	} catch (e) { e.name }`); got != "TypeError" {
		t.Errorf("ZonedDateTime.with accepted a calendar field: %s", got)
	}
}

func TestTemporalDurationFoundation(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	tests := []struct{ source, want string }{
		{`new Temporal.Duration().toString()`, "PT0S"},
		{`new Temporal.Duration(1, 2, 3, 4, 5, 6, 7, 8, 9, 10).toString()`, "P1Y2M3W4DT5H6M7.00800901S"},
		{`Temporal.Duration.from({ hours: 1, minutes: 30 }).toJSON()`, "PT1H30M"},
		{`Temporal.Duration.from("-PT1H30M").abs().toString()`, "PT1H30M"},
		{`Temporal.Duration.from("PT1H30M").negated().toString()`, "-PT1H30M"},
		{`Temporal.Duration.from("PT1.03125H").toString()`, "PT1H1M52.5S"},
		{`Temporal.Duration.from("-PT1.5M").toString()`, "-PT1M30S"},
		{`Temporal.Duration.from("p1y1m1dt1h1m1.123456789s").toString()`, "P1Y1M1DT1H1M1.123456789S"},
		{`Temporal.Duration.from({years: 9, hours: 5}).with({years: 2, minutes: 3}).toString()`, "P2YT5H3M"},
		{`Temporal.Duration.from({days: 1, hours: 12}).total("hours")`, "36"},
		{`Object.is(new Temporal.Duration(-0).years, 0)`, "true"},
		{`new Temporal.Duration().blank`, "true"},
		{`new Temporal.Duration(0, 0, 0, 0, -1).sign`, "-1"},
		{`new Temporal.Instant(1n).add({ seconds: 1, nanoseconds: 2 }).epochNanoseconds.toString()`, "1000000003"},
		{`new Temporal.Instant(1n).subtract(Temporal.Duration.from("PT1S")).epochNanoseconds.toString()`, "-999999999"},
		{`new Temporal.Instant(0n).add("PT1.03125H").epochNanoseconds.toString()`, "3712500000000"},
		{`new Temporal.Instant(0n).add({nanoseconds: 9007199254740990976}).epochNanoseconds.toString()`, "9007199254740990976"},
	}
	for _, test := range tests {
		if got := evalString(t, rt, test.source); got != test.want {
			t.Errorf("%s\n got: %s\nwant: %s", test.source, got, test.want)
		}
	}
	for _, source := range []string{
		`new Temporal.Duration(0, 0, 0, 0, -1, 1)`,
		`Temporal.Duration.from({})`,
		`new Temporal.Instant(0n).add({ days: 1 })`,
	} {
		if got := evalString(t, rt, `try { `+source+` } catch (e) { e.name }`); got != "RangeError" && source != `Temporal.Duration.from({})` {
			t.Errorf("%s threw %s", source, got)
		} else if source == `Temporal.Duration.from({})` && got != "TypeError" {
			t.Errorf("%s threw %s", source, got)
		}
	}
}

func TestTemporalDurationIntlFormatting(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	tests := []struct {
		source string
		want   string
	}{
		{`(() => {
			const formatter = new Intl.DurationFormat("en");
			return formatter.format("P1Y2M3W4DT5H6M7.00800901S") ===
				formatter.format({ years: 1, months: 2, weeks: 3, days: 4,
					hours: 5, minutes: 6, seconds: 7, milliseconds: 8,
					microseconds: 9, nanoseconds: 10 });
		})()`, "true"},
		{`(() => {
			const value = new Temporal.Duration(1, 2, 3, 4, 5, 6, 7, 8, 9, 10);
			const options = { style: "long" };
			return value.toLocaleString("de", options) ===
				new Intl.DurationFormat("de", options).format(value);
		})()`, "true"},
		{`(() => {
			const value = new Temporal.Duration(1, 2, 3, 4, 5, 6, 7, 8, 9, 10);
			const formatter = new Intl.DurationFormat("en");
			const expected = formatter.format(value);
			for (const field of ["years", "months", "weeks", "days", "hours",
				"minutes", "seconds", "milliseconds", "microseconds", "nanoseconds"]) {
				Object.defineProperty(Temporal.Duration.prototype, field, {
					get() { throw new Error("getter observed"); }, configurable: true
				});
			}
			return formatter.format(value) === expected;
		})()`, "true"},
	}
	for _, test := range tests {
		if got := evalString(t, rt, test.source); got != test.want {
			t.Errorf("%s\n got: %s\nwant: %s", test.source, got, test.want)
		}
	}
}

func TestTemporalInstantDifferenceAndRounding(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	tests := []struct{ source, want string }{
		{`new Temporal.Instant(0n).until(new Temporal.Instant(3723500600700n)).toString()`, "PT3723.5006007S"},
		{`new Temporal.Instant(0n).until(new Temporal.Instant(3723500600700n), {largestUnit: "hour"}).toString()`, "PT1H2M3.5006007S"},
		{`new Temporal.Instant(0n).until(new Temporal.Instant(3723500600700n), {smallestUnit: "second", roundingMode: "halfExpand"}).toString()`, "PT3724S"},
		{`new Temporal.Instant(0n).since(new Temporal.Instant(3723500600700n), {smallestUnit: "second", roundingMode: "ceil"}).toString()`, "-PT3723S"},
		{`new Temporal.Instant(3723500600700n).round("second").toString()`, "1970-01-01T01:02:04Z"},
		{`new Temporal.Instant(3723500600700n).round({smallestUnit: "minute", roundingIncrement: 15, roundingMode: "floor"}).toString()`, "1970-01-01T01:00:00Z"},
	}
	for _, test := range tests {
		if got := evalString(t, rt, test.source); got != test.want {
			t.Errorf("%s\n got: %s\nwant: %s", test.source, got, test.want)
		}
	}
}

func TestTemporalDurationAddSubtract(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	tests := []struct{ source, want string }{
		{`Temporal.Duration.from({ days: 1, minutes: 5 }).add("P2DT5M").toString()`, "P3DT10M"},
		{`new Temporal.Duration(0, 0, 0, 0, -60).add({ days: -1 }).toString()`, "-P3DT12H"},
		{`Temporal.Duration.from("P3DT1H10M").subtract({ minutes: 15 }).toString()`, "P3DT55M"},
		{`Temporal.Duration.from({ nanoseconds: Number.MAX_SAFE_INTEGER }).add({ nanoseconds: 2, days: 1 }).nanoseconds`, "993"},
	}
	for _, test := range tests {
		if got := evalString(t, rt, test.source); got != test.want {
			t.Errorf("%s\n got: %s\nwant: %s", test.source, got, test.want)
		}
	}
	for _, source := range []string{
		`new Temporal.Duration(1).add(new Temporal.Duration())`,
		`Temporal.Duration.from({ seconds: Number.MAX_SAFE_INTEGER }).add(Temporal.Duration.from({ seconds: Number.MAX_SAFE_INTEGER }))`,
	} {
		if got := evalString(t, rt, `try { `+source+` } catch (e) { e.name }`); got != "RangeError" {
			t.Errorf("%s threw %s", source, got)
		}
	}
}

func TestTemporalDurationRound(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	tests := []struct{ source, want string }{
		{`new Temporal.Duration(0, 0, 0, 5, 5, 5, 5, 5, 5, 5).round({ largestUnit: "hours", smallestUnit: "minutes" }).toString()`, "PT125H5M"},
		{`new Temporal.Duration(0, 0, 0, 3, 12).round({ smallestUnit: "hours", roundingIncrement: 8, roundingMode: "halfEven", relativeTo: new Temporal.PlainDate(1970, 1, 1) }).toString()`, "P3DT8H"},
		{`new Temporal.Duration(0, 1, 0, 1).round({ largestUnit: "weeks", smallestUnit: "weeks", roundingIncrement: 6, roundingMode: "ceil", relativeTo: new Temporal.PlainDate(2024, 1, 1) }).toString()`, "P6W"},
		{`new Temporal.Duration(1, 0, 0, 0, 24).round({ largestUnit: "years", relativeTo: { year: 2021, month: 10, day: 28, timeZone: "UTC" } }).toString()`, "P1Y1D"},
		{`new Temporal.Duration(0, 0, 0, 1).round({ largestUnit: "seconds",
			relativeTo: "1952-10-15T23:59:59-11:20[Pacific/Niue]" }).seconds`, "86420"},
		{`new Temporal.Duration(0, 0, 0, 0, 13).round({ largestUnit: "years",
			smallestUnit: "hours", roundingIncrement: 12, roundingMode: "ceil",
			relativeTo: "2024-03-10T00:00:00[America/New_York]" }).toString()`, "P1DT12H"},
		{`new Temporal.Duration(0, 0, 0, 0, -12, -30).round({ largestUnit: "days",
			smallestUnit: "days", roundingMode: "halfExpand",
			relativeTo: "2025-11-02T01:00:00-08:00[America/Vancouver]" }).toString()`, "-P1D"},
		{`new Temporal.Duration(1).round({smallestUnit: "months",
			relativeTo: new Temporal.PlainDate(2020, 2, 29)}).toString()`, "P1Y"},
	}
	for _, test := range tests {
		if got := evalString(t, rt, test.source); got != test.want {
			t.Errorf("%s\n got: %s\nwant: %s", test.source, got, test.want)
		}
	}
	if got := evalString(t, rt, `try {
		new Temporal.Duration(0, 0, 0, 0, 0, 5).round({
			smallestUnit: "minutes",
			relativeTo: new Temporal.ZonedDateTime(8640000000000000000000n, "UTC")
		});
	} catch (e) { e.name }`); got != "RangeError" {
		t.Fatalf("rounding past the Temporal instant limit threw %s", got)
	}
	if got := evalString(t, rt, `try {
		new Temporal.Duration().round({
			largestUnit: "days",
			smallestUnit: "minutes",
			relativeTo: new Temporal.ZonedDateTime(8640000000000000000000n, "UTC")
		});
	} catch (e) { e.name }`); got != "RangeError" {
		t.Fatalf("rounding at the final zoned day threw %s", got)
	}
	if got := evalString(t, rt, `try {
		new Temporal.Duration(1).round({
			smallestUnit: "years",
			relativeTo: { era: "ad", eraYear: Infinity, month: 5, day: 2,
				calendar: "gregory" }
		});
	} catch (e) { e.name }`); got != "RangeError" {
		t.Fatalf("rounding with an infinite Gregorian era year threw %s", got)
	}
}

func TestTemporalDurationCompare(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	tests := []struct{ source, want string }{
		{`Temporal.Duration.compare({ months: 1 }, { days: 30 }, { relativeTo: new Temporal.PlainDate(2018, 4, 1) })`, "0"},
		{`Temporal.Duration.compare({ months: 1 }, { days: 30 }, { relativeTo: new Temporal.PlainDate(2018, 3, 1) })`, "1"},
		{`Temporal.Duration.compare({ days: 1 }, { hours: 24 }, { relativeTo: new Temporal.ZonedDateTime(1541302200000000000n, "America/Los_Angeles") })`, "1"},
		{`Temporal.Duration.compare(new Temporal.Duration(5, 5, 5), new Temporal.Duration(5, 5, 5))`, "0"},
	}
	for _, test := range tests {
		if got := evalString(t, rt, test.source); got != test.want {
			t.Errorf("%s\n got: %s\nwant: %s", test.source, got, test.want)
		}
	}
	if got := evalString(t, rt, `try {
		Temporal.Duration.compare({ months: 1 }, { days: 30 });
	} catch (e) { e.name }`); got != "RangeError" {
		t.Fatalf("comparing a calendar duration without relativeTo threw %s", got)
	}
}

func TestTemporalDurationTotal(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	tests := []struct{ source, want string }{
		{`new Temporal.Duration(1).total({ unit: "days", relativeTo: new Temporal.PlainDate(2021, 12, 15) })`, "365"},
		{`new Temporal.Duration(1, 0, 0, 0, 1).total({ unit: "years", relativeTo: new Temporal.PlainDate(2020, 2, 29) })`, "1.0001141552511414"},
		{`new Temporal.Duration(0, 1, 0, 0, 10).total({ unit: "months", relativeTo: new Temporal.PlainDate(2020, 1, 31) })`, "1.0134408602150538"},
		{`new Temporal.Duration(0, 0, 1, 0, 1).total({ unit: "days", relativeTo: new Temporal.ZonedDateTime(0n, "UTC") })`, "7.041666666666667"},
		{`new Temporal.Duration(0, 0, 0, 1).total({ unit: "seconds",
			relativeTo: "1952-10-15T23:59:59-11:20[Pacific/Niue]" })`, "86420"},
	}
	for _, test := range tests {
		if got := evalString(t, rt, test.source); got != test.want {
			t.Errorf("%s\n got: %s\nwant: %s", test.source, got, test.want)
		}
	}
	if got := evalString(t, rt, `try {
		new Temporal.Duration(0, 0, 0, 0, 0, 0, 0, 0, 0, 1).total({
			unit: "nanoseconds",
			relativeTo: new Temporal.ZonedDateTime(8640000000000000000000n, "UTC")
		});
	} catch (e) { e.name }`); got != "RangeError" {
		t.Fatalf("totaling past the Temporal instant limit threw %s", got)
	}
}

func TestTemporalPlainDateFoundation(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	tests := []struct{ source, want string }{
		{`new Temporal.PlainDate(2020.9, 2.8, 29.4).toString()`, "2020-02-29"},
		{`Temporal.PlainDate.from("2019-03-15").toJSON()`, "2019-03-15"},
		{`Temporal.PlainDate.from("1976-11-18T15:23:30.1+00:00").toString()`, "1976-11-18"},
		{`Temporal.PlainDate.from("2000-05-02T15:23[UTC][u-ca=iso8601]").toString()`, "2000-05-02"},
		{`Temporal.PlainDate.from("2000-05-02T15:23[u-ca=iso8601][u-ca=discord]").calendarId`, "iso8601"},
		{`Temporal.PlainDate.from({year: 2021, month: 2, day: 31}).toString()`, "2021-02-28"},
		{`Temporal.PlainDate.from({year: 1976, monthCode: "M11", day: 18, calendar: "2020-01-01"}).calendarId`, "iso8601"},
		{`Temporal.PlainDate.compare("2020-01-01", "2019-12-31")`, "1"},
		{`new Temporal.PlainDate(2020, 12, 31).weekOfYear`, "53"},
		{`new Temporal.PlainDate(2021, 1, 1).yearOfWeek`, "2020"},
		{`new Temporal.PlainDate(2024, 2, 29).dayOfYear`, "60"},
		{`new Temporal.PlainDate(2000, 5, 2).toString({calendarName: "critical"})`, "2000-05-02[!u-ca=iso8601]"},
		{`new Temporal.PlainDate(1970, 1, 1).dayOfWeek`, "4"},
		{`new Temporal.PlainDate(2000, 5, 2, "GREGORY").calendarId`, "gregory"},
		{`new Temporal.PlainDate(2020, 2, 29).with({year: 2021}).toString()`, "2021-02-28"},
		{`new Temporal.PlainDate(2020, 1, 31).add({months: 1}).toString()`, "2020-02-29"},
		{`new Temporal.PlainDate(2020, 3, 1).subtract({days: 1}).toString()`, "2020-02-29"},
		{`new Temporal.PlainDate(2020, 1, 1).add({hours: 36}).toString()`, "2020-01-02"},
		{`new Temporal.PlainDate(2019, 1, 31).until(new Temporal.PlainDate(2019, 3, 30), {largestUnit: "months"}).toString()`, "P1M30D"},
		{`new Temporal.PlainDate(2019, 1, 29).until(new Temporal.PlainDate(2019, 2, 28), {largestUnit: "months"}).toString()`, "P30D"},
		{`Temporal.PlainDate.from({year: 1987, month: 7, day: 1,
			calendar: "chinese"}).monthCode`, "M06L"},
		{`Temporal.PlainDate.from({year: 2026, month: 1, day: 1,
			calendar: "chinese"}).daysInYear`, "354"},
		{`Temporal.PlainDate.from({year: 1770, monthCode: "M13", day: 5,
			calendar: "coptic"}).until(Temporal.PlainDate.from({year: 1771,
			monthCode: "M01", day: 5, calendar: "coptic"}),
			{largestUnit: "years"}).toString()`, "P1M"},
		{`new Temporal.PlainDate(2021, 9, 7).since(new Temporal.PlainDate(2019, 1, 8), {smallestUnit: "years", roundingMode: "halfExpand"}).toString()`, "P3Y"},
		{`new Temporal.PlainDate(2019, 1, 1).until(new Temporal.PlainDate(2020, 7, 2), {smallestUnit: "years", roundingMode: "halfEven"}).toString()`, "P2Y"},
		{`new Temporal.PlainDate(2020, 1, 1).withCalendar("15:23").calendarId`, "iso8601"},
		{`new Temporal.PlainDate(2020, 1, 1).toZonedDateTime({timeZone: "UTC", plainTime: "12:34:56.123456789"}).toString()`, "2020-01-01T12:34:56.123456789+00:00[UTC]"},
		{`new Temporal.PlainDate(2021, 3, 14).toZonedDateTime({timeZone: "America/New_York", plainTime: "02:30"}).toString()`, "2021-03-14T03:30:00-04:00[America/New_York]"},
		{`new Temporal.PlainDate(2021, 11, 7).toZonedDateTime({timeZone: "America/New_York", plainTime: "01:30"}).toString()`, "2021-11-07T01:30:00-04:00[America/New_York]"},
		{`new Temporal.PlainDate(2015, 10, 18).toZonedDateTime("America/Sao_Paulo").toString()`, "2015-10-18T01:00:00-02:00[America/Sao_Paulo]"},
		{`new Temporal.PlainDate(2020, 1, 1).toZonedDateTime("2021-08-19T17:30-12:12[+01:46]").timeZoneId`, "+01:46"},
		{`new Temporal.PlainDate(-271821, 4, 19).toString()`, "-271821-04-19"},
		{`class D extends Temporal.PlainDate {}; Object.getPrototypeOf(new D(2000, 1, 1)) === D.prototype`, "true"},
	}
	for _, test := range tests {
		if got := evalString(t, rt, test.source); got != test.want {
			t.Errorf("%s\n got: %s\nwant: %s", test.source, got, test.want)
		}
	}
	for _, source := range []string{
		`new Temporal.PlainDate(2021, 2, 29)`,
		`new Temporal.PlainDate(-271821, 4, 18)`,
		`Temporal.PlainDate.from({year: 2021, month: 2, day: 31}, {overflow: "reject"})`,
		`new Temporal.PlainDate(2000, 1, 1, "not-a-calendar")`,
		`new Temporal.PlainDate(2020, 1, 1).toZonedDateTime("2021-08-19T17:30-07:00:00")`,
	} {
		if got := evalString(t, rt, `try { `+source+` } catch (e) { e.name }`); got != "RangeError" {
			t.Errorf("%s threw %s", source, got)
		}
	}
}

func TestTemporalPlainYearMonthAndMonthDayFoundation(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	tests := []struct{ source, want string }{
		{`new Temporal.PlainYearMonth(2019, 10).toString()`, "2019-10"},
		{`new Temporal.PlainYearMonth(2019, 10, "iso8601", 31).toString({calendarName: "always"})`, "2019-10-31[u-ca=iso8601]"},
		{`Temporal.PlainYearMonth.from("2019-10-31").toString({calendarName: "always"})`, "2019-10-01[u-ca=iso8601]"},
		{`Temporal.PlainYearMonth.compare(new Temporal.PlainYearMonth(2019, 10, "iso8601", 1), new Temporal.PlainYearMonth(2019, 10, "iso8601", 2))`, "-1"},
		{`new Temporal.PlainYearMonth(2024, 2).daysInMonth`, "29"},
		{`new Temporal.PlainYearMonth(2019, 10).with({month: 9}).toString()`, "2019-09"},
		{`new Temporal.PlainYearMonth(2019, 10).add({years: 1, months: 4}).toString()`, "2021-02"},
		{`new Temporal.PlainYearMonth(2019, 10).subtract("P1Y4M").toString()`, "2018-06"},
		{`new Temporal.PlainYearMonth(2019, 1).until(new Temporal.PlainYearMonth(2021, 9), {smallestUnit: "years", roundingMode: "halfExpand"}).toString()`, "P3Y"},
		{`new Temporal.PlainYearMonth(2021, 9).since(new Temporal.PlainYearMonth(2019, 1), {largestUnit: "months"}).toString()`, "P32M"},
		{`new Temporal.PlainYearMonth(2024, 2).toPlainDate({day: 29}).toString()`, "2024-02-29"},
		{`(() => {
			const value = Temporal.PlainYearMonth.from({ calendar: "hebrew",
				year: 5784, monthCode: "M05L" });
			return [value.toString(), value.year, value.month, value.monthCode,
				value.daysInMonth, value.daysInYear, value.monthsInYear,
				value.inLeapYear].join("|");
		})()`, "2024-02-10[u-ca=hebrew]|5784|6|M05L|30|383|13|true"},
		{`Temporal.PlainYearMonth.from({ calendar: "hebrew", year: 5784,
			monthCode: "M05L" }).add({ years: 1 }).toString()`,
			"2025-03-01[u-ca=hebrew]"},
		{`Temporal.PlainYearMonth.from({ calendar: "coptic", year: 1739,
			monthCode: "M13" }).add({ months: 1 }).toString()`,
			"2023-09-12[u-ca=coptic]"},
		{`(() => {
			const start = Temporal.PlainYearMonth.from({ calendar: "hebrew",
				year: 5783, monthCode: "M07" });
			const end = Temporal.PlainYearMonth.from({ calendar: "hebrew",
				year: 5793, monthCode: "M07" });
			return [start.until(end, { largestUnit: "months" }),
				end.since(start, { largestUnit: "years" })].join("|");
		})()`, "P124M|P10Y"},
		{`Temporal.PlainYearMonth.from({ calendar: "hebrew", year: 5784,
			monthCode: "M05L" }).toPlainDate({ day: 40 }).toString()`,
			"2024-03-10[u-ca=hebrew]"},
		{`new Temporal.PlainMonthDay(2, 29).toString()`, "02-29"},
		{`new Temporal.PlainMonthDay(10, 31, "iso8601", 2019).toString({calendarName: "always"})`, "2019-10-31[u-ca=iso8601]"},
		{`Temporal.PlainMonthDay.from("2019-10-31").toString({calendarName: "always"})`, "1972-10-31[u-ca=iso8601]"},
		{`new Temporal.PlainMonthDay(1, 31).with({month: 2}).toString()`, "02-29"},
		{`new Temporal.PlainMonthDay(2, 29).toPlainDate({year: 2024}).toString()`, "2024-02-29"},
		{`new Temporal.PlainDate(2024, 2, 29).toPlainYearMonth().toString()`, "2024-02"},
		{`new Temporal.PlainDate(2024, 2, 29).toPlainMonthDay().toString()`, "02-29"},
		{`Temporal.PlainDate.from({year: 2024, month: 2, day: 29, calendar: new Temporal.PlainYearMonth(2000, 1)}).calendarId`, "iso8601"},
		{`Temporal.PlainDate.from({year: 2024, month: 2, day: 29, calendar: new Temporal.PlainMonthDay(1, 1)}).calendarId`, "iso8601"},
		{`Temporal.ZonedDateTime.from("2019-05-17T12:34Z[UTC]").toString()`, "2019-05-17T12:34:00+00:00[UTC]"},
	}
	for _, test := range tests {
		if got := evalString(t, rt, test.source); got != test.want {
			t.Errorf("%s\n got: %s\nwant: %s", test.source, got, test.want)
		}
	}
	for _, source := range []string{
		`new Temporal.PlainYearMonth(-271821, 3)`,
		`new Temporal.PlainYearMonth(2019, 2, "iso8601", 29)`,
		`new Temporal.PlainMonthDay(9, 14, "iso8601", 275760)`,
		`Temporal.PlainMonthDay.from({month: 2, day: 29, year: 2023}, {overflow: "reject"})`,
	} {
		if got := evalString(t, rt, `try { `+source+` } catch (e) { e.name }`); got != "RangeError" {
			t.Errorf("%s threw %s", source, got)
		}
	}
}

func TestTemporalPlainMonthDayNonISOCalendars(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	tests := []struct{ source, want string }{
		{`Temporal.PlainMonthDay.from({ calendar: "hebrew",
			monthCode: "M05L", day: 1 }).toString()`,
			"1970-02-07[u-ca=hebrew]"},
		{`Temporal.PlainMonthDay.from("2023-01-01[u-ca=hebrew]").toString()`,
			"1972-12-13[u-ca=hebrew]"},
		{`Temporal.PlainMonthDay.from({ calendar: "chinese",
			monthCode: "M02L", day: 30 }).toString()`,
			"1972-04-13[u-ca=chinese]"},
		{`Temporal.PlainMonthDay.from({ calendar: "hebrew",
			monthCode: "M11", day: 4 }).with({ monthCode: "M10" }).monthCode`,
			"M10"},
		{`Temporal.PlainMonthDay.from({ calendar: "hebrew",
			monthCode: "M11", day: 4 }).toPlainDate({
				era: "am", eraYear: 5784 }).toString()`,
			"2024-08-08[u-ca=hebrew]"},
		{`Temporal.PlainDate.from({ calendar: "hebrew", year: 5784,
			monthCode: "M05L", day: 1 }).toPlainMonthDay().monthCode`,
			"M05L"},
	}
	for _, test := range tests {
		if got := evalString(t, rt, test.source); got != test.want {
			t.Errorf("%s\n got: %s\nwant: %s", test.source, got, test.want)
		}
	}

	errors := []struct{ source, want string }{
		{`Temporal.PlainMonthDay.from({ calendar: "hebrew",
			month: 8, day: 1 })`, "TypeError"},
		{`Temporal.PlainMonthDay.from({ calendar: "gregory", era: "ce",
			eraYear: 2024, year: 2023, monthCode: "M01", day: 1 })`,
			"RangeError"},
		{`Temporal.PlainMonthDay.from({ calendar: "chinese",
			monthCode: "M01L", day: 29 }, { overflow: "reject" })`,
			"RangeError"},
	}
	for _, test := range errors {
		got := evalString(t, rt,
			`try { `+test.source+` } catch (e) { e.name }`)
		if got != test.want {
			t.Errorf("%s threw %s, want %s", test.source, got, test.want)
		}
	}
}

func TestTemporalPlainDateTimeFoundation(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	tests := []struct{ source, want string }{
		{`new Temporal.PlainDateTime(2000, 5, 2, 15, 23, 30, 987, 654, 321).toString()`, "2000-05-02T15:23:30.987654321"},
		{`Temporal.PlainDateTime.from("2000-05-02T15:23:30.1234").toJSON()`, "2000-05-02T15:23:30.1234"},
		{`Temporal.PlainDateTime.compare("2000-05-02T15:23", "2000-05-02T15:24")`, "-1"},
		{`new Temporal.PlainDateTime(2000, 5, 2, 15).toPlainDate().toString()`, "2000-05-02"},
		{`new Temporal.PlainDate(2000, 5, 2).toPlainDateTime({hour: 15, minute: 23}).toString()`, "2000-05-02T15:23:00"},
		{`new Temporal.PlainDateTime(2000, 5, 2, 15, 23).toPlainTime().toString()`, "15:23:00"},
		{`new Temporal.PlainDateTime(2000, 5, 2, 15, 23).withPlainTime().toString()`, "2000-05-02T00:00:00"},
		{`Temporal.PlainDate.from(new Temporal.PlainDateTime(2000, 5, 2, 23)).toString()`, "2000-05-02"},
		{`new Temporal.PlainDateTime(2000, 5, 2, 15, 23, 30).hour`, "15"},
		{`new Temporal.PlainDateTime(1976, 11, 18, 15).dayOfWeek`, "4"},
		{`new Temporal.PlainDateTime(1976, 11, 18, 15).weekOfYear`, "47"},
		{`new Temporal.PlainDateTime(2000, 5, 2, 15, 23).with({month: 6, hour: 4}).toString()`, "2000-06-02T04:23:00"},
		{`new Temporal.PlainDateTime(2000, 5, 2, 15).withCalendar("GREGORY").calendarId`, "gregory"},
		{`new Temporal.PlainDateTime(2021, 11, 7, 1, 30).toZonedDateTime("America/New_York", {disambiguation: "earlier"}).epochNanoseconds.toString()`, "1636263000000000000"},
		{`new Temporal.PlainDateTime(2021, 11, 7, 1, 30).toZonedDateTime("America/New_York", {disambiguation: "later"}).epochNanoseconds.toString()`, "1636266600000000000"},
		{`Temporal.ZonedDateTime.from("2022-04-12T15:19:45[UTC]").toString()`, "2022-04-12T15:19:45+00:00[UTC]"},
		{`Temporal.PlainDateTime.from("-271821-04-19T00:00:00.000000001").nanosecond`, "1"},
		{`Temporal.PlainDateTime.from(new Temporal.ZonedDateTime(-13849764999999999n, "UTC")).toString()`, "1969-07-24T16:50:35.000000001"},
		{`Temporal.PlainDateTime.from({year: 2016, month: 12, day: 31, second: 60}).second`, "59"},
		{`(() => {
			const value = Temporal.PlainDateTime.from({ calendar: "hebrew",
				year: 5782, monthCode: "M05L", day: 15, hour: 12 });
			return [value.toString(), value.year, value.month, value.monthCode].join("|");
		})()`, "2022-02-16T12:00:00[u-ca=hebrew]|5782|6|M05L"},
		{`(() => {
			const value = Temporal.PlainDateTime.from({ calendar: "hebrew",
				year: 5782, monthCode: "M05L", day: 15, hour: 12 });
			const changed = value.with({ year: 5783, month: 6 });
			return [changed.toString(), changed.year, changed.month,
				changed.monthCode].join("|");
		})()`, "2023-03-08T12:00:00[u-ca=hebrew]|5783|6|M06"},
		{`try {
			new Temporal.PlainDateTime(2000, 5, 2, 12, 34, 56, 0, 0, 0,
				"hebrew").toLocaleString("en-US-u-ca-gregory");
		} catch (error) { error.name }`, "RangeError"},
		{`new Temporal.PlainDateTime(2021, 8, 4, 23, 30, 45).toLocaleString(
			"en-US", { timeZone: "Pacific/Apia" })`, "8/4/2021, 11:30:45 PM"},
		{`new Temporal.PlainDateTime(1999, 12, 31, 23, 59, 59, 999, 999, 999).toString({fractionalSecondDigits: 8, roundingMode: "ceil"})`, "2000-01-01T00:00:00.00000000"},
		{`new Temporal.PlainDateTime(2000, 5, 2, 12, 34, 56, 123, 456, 789).toString({smallestUnit: "minute"})`, "2000-05-02T12:34"},
		{`new Temporal.PlainDateTime(1999, 12, 31, 23, 59, 59, 999, 999, 999).round("microsecond").toString()`, "2000-01-01T00:00:00"},
		{`new Temporal.PlainDateTime(2020, 5, 31, 23, 12, 38).add({hours: 2}).toString()`, "2020-06-01T01:12:38"},
		{`new Temporal.PlainDateTime(2020, 1, 31, 15).subtract({months: -1}).toString()`, "2020-02-29T15:00:00"},
		{`new Temporal.PlainDateTime(1997, 12, 1, 12, 34).until(new Temporal.PlainDateTime(2001, 6, 18, 12, 34), {largestUnit: "year"}).toString()`, "P3Y6M17D"},
		{`new Temporal.PlainDateTime(2000, 5, 2).until(new Temporal.PlainDateTime(2000, 5, 2, 1, 59, 59), {largestUnit: "hour", smallestUnit: "minute", roundingMode: "expand"}).toString()`, "PT2H"},
		{`new Temporal.PlainDateTime(2012, 1, 1, 12).until(new Temporal.PlainDateTime(2012, 2, 1, 12), {largestUnit: "month", smallestUnit: "day", roundingIncrement: 2, roundingMode: "halfExpand"}).toString()`, "P1M"},
		{`new Temporal.PlainDateTime(2012, 1, 1, 12).until(new Temporal.PlainDateTime(2012, 2, 2, 12), {largestUnit: "month", smallestUnit: "day", roundingIncrement: 2, roundingMode: "halfExpand"}).toString()`, "P1M2D"},
		{`new Temporal.PlainDateTime(2012, 1, 1, 12).until(new Temporal.PlainDateTime(2012, 2, 5), {largestUnit: "month", smallestUnit: "week", roundingMode: "halfExpand"}).toString()`, "P1M1W"},
		{`new Temporal.PlainDateTime(2012, 1, 1, 12).until(new Temporal.PlainDateTime(2012, 2, 5), {largestUnit: "month", smallestUnit: "week", roundingMode: "halfTrunc"}).toString()`, "P1M"},
		{`try { Temporal.PlainDateTime.from("2000-01-01T0000:00") } catch (e) { e.name }`, "RangeError"},
		{`class D extends Temporal.PlainDateTime {}; Object.getPrototypeOf(new D(2000, 1, 1)) === D.prototype`, "true"},
	}
	for _, test := range tests {
		if got := evalString(t, rt, test.source); got != test.want {
			t.Errorf("%s\n got: %s\nwant: %s", test.source, got, test.want)
		}
	}
}

func TestTemporalPlainTimeFoundation(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	tests := []struct{ source, want string }{
		{`new Temporal.PlainTime(15, 23, 30, 123, 456, 789).toString()`, "15:23:30.123456789"},
		{`Temporal.PlainTime.from("15:23:30.1234").toJSON()`, "15:23:30.1234"},
		{`Temporal.PlainTime.from({hour: 27, minute: 70}).toString()`, "23:59:00"},
		{`Temporal.PlainTime.compare("15:23", "15:24")`, "-1"},
		{`Temporal.PlainTime.from(new Temporal.PlainDateTime(2000, 1, 1, 12, 34)).minute`, "34"},
		{`new Temporal.PlainTime(12, 34, 56).with({hour: 1e100, minute: undefined}).toString()`, "23:34:56"},
		{`new Temporal.PlainTime(15, 23, 30, 123, 456, 789).add({hours: 16}).toString()`, "07:23:30.123456789"},
		{`new Temporal.PlainTime(1, 1, 1, 1, 1, 1).subtract({nanoseconds: 2}).toString()`, "01:01:01.001000999"},
		{`new Temporal.PlainTime(12, 34, 56).add({years: 1, months: 1, weeks: 1, days: 1}).toString()`, "12:34:56"},
		{`Temporal.PlainTime.from({microsecond: 1}).add(Temporal.Duration.from({microseconds: Number.MAX_SAFE_INTEGER, nanoseconds: 1000})).toString()`, "23:47:34.740993"},
		{`new Temporal.PlainTime().add("PT9007199254740991.999999999S").toString()`, "07:36:31.999999999"},
		{`try { new Temporal.PlainTime().add({seconds: 9007199254740992}) } catch (e) { e.name }`, "RangeError"},
		{`let read = false; new Temporal.PlainTime().add({hours: 1}, {get overflow() { read = true }}); read`, "false"},
		{`new Temporal.PlainTime(3, 34, 56, 987, 654, 321).round({smallestUnit: "hour", roundingIncrement: 6}).toString()`, "06:00:00"},
		{`new Temporal.PlainTime(23, 59, 59, 999, 999, 999).round("microsecond").toString()`, "00:00:00"},
		{`new Temporal.PlainTime(12, 34, 56, 123, 400).toString({fractionalSecondDigits: 6})`, "12:34:56.123400"},
		{`new Temporal.PlainTime(12, 34, 56, 123, 987, 500).toString({smallestUnit: "minute", roundingMode: "halfEven"})`, "12:35"},
		{`new Temporal.PlainTime(23, 59, 59, 999, 999, 999).toString({fractionalSecondDigits: 8, roundingMode: "ceil"})`, "00:00:00.00000000"},
		{`new Temporal.PlainTime(15, 23, 30).until(new Temporal.PlainTime(17, 0, 30)).toString()`, "PT1H37M"},
		{`new Temporal.PlainTime(15, 23, 30).since(new Temporal.PlainTime(17, 0, 30)).toString()`, "-PT1H37M"},
		{`new Temporal.PlainTime().until("01:59:59", {smallestUnit: "minute", roundingMode: "expand"}).toString()`, "PT2H"},
		{`typeof new Temporal.PlainTime(12, 34, 56).toLocaleString("en", {timeStyle: "short"})`, "string"},
		{`class T extends Temporal.PlainTime {}; Object.getPrototypeOf(new T(12)) === T.prototype`, "true"},
	}
	for _, test := range tests {
		if got := evalString(t, rt, test.source); got != test.want {
			t.Errorf("%s\n got: %s\nwant: %s", test.source, got, test.want)
		}
	}
}

func TestTemporalIsLazy(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	got := evalString(t, rt, `{
        const before = Object.getOwnPropertyDescriptor(globalThis, "Temporal");
        const first = Temporal;
        const after = Object.getOwnPropertyDescriptor(globalThis, "Temporal");
        [typeof before.get, "value" in before, first === Temporal,
         typeof after.get, after.value === first].join(",")
    }`)
	if got != "function,false,true,undefined,true" {
		t.Fatalf("lazy Temporal descriptor = %q", got)
	}
}

func TestTemporalNowFoundation(t *testing.T) {
	checkEval(t, `[
        Object.prototype.toString.call(Temporal.Now),
        Temporal.Now.instant() instanceof Temporal.Instant,
        Temporal.Now.plainDateISO() instanceof Temporal.PlainDate,
        Temporal.Now.plainDateTimeISO() instanceof Temporal.PlainDateTime,
        Temporal.Now.plainTimeISO() instanceof Temporal.PlainTime,
        Temporal.Now.zonedDateTimeISO("UTC") instanceof Temporal.ZonedDateTime,
        typeof Temporal.Now.timeZoneId()
    ].join(",")`, "[object Temporal.Now],true,true,true,true,true,string")
}
