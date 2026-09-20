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
