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
		{`Temporal.Instant.from("2000-02-29T12:34:56.123456789Z").toLocaleString("en-US", {timeZone: "UTC"})`, "2/29/2000, 12:34:56 PM"},
		{`Temporal.Instant.from("2000-02-29T12:34:56.123456789Z").toLocaleString("en-US", {timeZone: "America/New_York", timeZoneName: "long"})`, "2/29/2000, 7:34:56 AM Eastern Standard Time"},
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
		{`try { new Temporal.Instant(0n).valueOf() } catch (e) { e.name }`, "TypeError"},
	}
	for _, test := range tests {
		if got := evalString(t, rt, test.source); got != test.want {
			t.Errorf("%s\n got: %s\nwant: %s", test.source, got, test.want)
		}
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
	}
	for _, test := range tests {
		if got := evalString(t, rt, test.source); got != test.want {
			t.Errorf("%s\n got: %s\nwant: %s", test.source, got, test.want)
		}
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
		{`Temporal.PlainDateTime.from("-271821-04-19T00:00:00.000000001").nanosecond`, "1"},
		{`Temporal.PlainDateTime.from(new Temporal.ZonedDateTime(-13849764999999999n, "UTC")).toString()`, "1969-07-24T16:50:35.000000001"},
		{`Temporal.PlainDateTime.from({year: 2016, month: 12, day: 31, second: 60}).second`, "59"},
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
