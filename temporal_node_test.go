package quickjs_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	intl "github.com/go-quickjs/go-intl"
	quickjs "github.com/go-quickjs/go-quickjs"
)

// TestTemporalMatchesNodeAcrossLocalesAndTimeZones compares every locale
// DateTimeFormat is available in with every zone Node lists. It is opt-in because the complete
// matrix is deliberately large and requires a recent Node with Temporal.
//
// Run it with:
//
//	QUICKJS_COMPARE_NODE_TEMPORAL=1 go test . \
//	  -run '^TestTemporalMatchesNodeAcrossLocalesAndTimeZones$' \
//	  -count=1 -timeout=120m -v
func TestTemporalMatchesNodeAcrossLocalesAndTimeZones(t *testing.T) {
	if os.Getenv("QUICKJS_COMPARE_NODE_TEMPORAL") == "" {
		t.Skip("set QUICKJS_COMPARE_NODE_TEMPORAL=1 to compare the complete matrix")
	}

	matcher, err := intl.NewLocaleMatcher(intl.Embedded, intl.ServiceDateTimeFormat)
	if err != nil {
		t.Fatal(err)
	}
	allZones, err := intl.TimeZones(intl.NodeICU)
	if err != nil {
		t.Fatal(err)
	}
	locales := filterTemporalNodeValues(t, matcher.Locales(),
		"QUICKJS_NODE_TEMPORAL_LOCALES")
	zones := filterTemporalNodeValues(t, allZones,
		"QUICKJS_NODE_TEMPORAL_ZONES")
	expression := temporalNodeMatrixExpression(t, locales, zones)

	node := os.Getenv("NODE_BINARY")
	if node == "" {
		node = "node"
	}
	command := exec.Command(node, "-")
	command.Stdin = strings.NewReader("process.stdout.write(" + expression + ");\n")
	want, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run %s: %v\n%s", node, err, want)
	}

	rt := quickjs.New(quickjs.WithNodeQuirks())
	defer rt.Close()
	value, err := rt.Eval(expression)
	if err != nil {
		t.Fatalf("evaluate Temporal matrix: %v", err)
	}
	got := []byte(value.String())
	if bytes.Equal(got, want) {
		t.Logf("matched Node for %d locales x %d time zones (%d pairs)",
			len(locales), len(zones), len(locales)*len(zones))
		return
	}

	wantRecord, gotRecord := firstDifferentTemporalNodeRecord(want, got)
	t.Fatalf("Temporal differs from Node in the first matrix record:\nnode    %s\nquickjs %s",
		wantRecord, gotRecord)
}

func filterTemporalNodeValues(t *testing.T, all []string, environment string) []string {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(environment))
	if raw == "" {
		return all
	}
	available := make(map[string]bool, len(all))
	for _, value := range all {
		available[value] = true
	}
	var selected []string
	for _, value := range strings.Split(raw, ",") {
		value = strings.TrimSpace(value)
		if !available[value] {
			t.Fatalf("%s contains unavailable value %q", environment, value)
		}
		selected = append(selected, value)
	}
	if len(selected) == 0 {
		t.Fatalf("%s did not select any values", environment)
	}
	return selected
}

func temporalNodeMatrixExpression(t *testing.T, locales, zones []string) string {
	t.Helper()
	localeJSON, err := json.Marshal(locales)
	if err != nil {
		t.Fatal(err)
	}
	zoneJSON, err := json.Marshal(zones)
	if err != nil {
		t.Fatal(err)
	}

	return fmt.Sprintf(`(() => {
  const locales = %s;
  const zones = %s;
  const instants = [
    Temporal.Instant.from("1900-01-01T00:00:00Z"),
    Temporal.Instant.from("2024-01-15T12:34:56.123456789Z"),
    Temporal.Instant.from("2024-07-15T12:34:56.123456789Z"),
    Temporal.Instant.from("2200-01-15T12:34:56Z")
  ];
  const plainDateTime = new Temporal.PlainDateTime(2024, 2, 29, 23, 45, 6, 123, 456, 789);
  const plainDate = new Temporal.PlainDate(2024, 2, 29);
  const plainTime = new Temporal.PlainTime(23, 45, 6, 123, 456, 789);
  const plainYearMonth = new Temporal.PlainYearMonth(2024, 2);
  const plainMonthDay = new Temporal.PlainMonthDay(2, 29);
  const duration = new Temporal.Duration(1, 2, 0, 4, 5, 6, 7, 8, 9, 10);
  const zoneData = zones.map(zone => ({
    zone,
    zoned: instants.map(instant => instant.toZonedDateTimeISO(zone))
  }));
  const out = [];

  function digest(values) {
    let first = 0x811c9dc5;
    let second = 0x9e3779b9;
    let length = 0;
    for (let value of values) {
      try {
        value = "V" + String(value());
      } catch (error) {
        value = "E" + String(error && error.name);
      }
      length += value.length;
      for (let index = 0; index < value.length; index++) {
        const code = value.charCodeAt(index);
        first = Math.imul(first ^ code, 0x01000193);
        second = Math.imul(second ^ code, 0x5bd1e995);
        second ^= second >>> 13;
      }
      first = Math.imul(first ^ 0xffff, 0x01000193);
      second = Math.imul(second ^ 0xffff, 0x5bd1e995);
      second ^= second >>> 13;
    }
    return (first >>> 0).toString(16) + "," +
      (second >>> 0).toString(16) + "," + values.length + "," + length;
  }

  for (let localeIndex = 0; localeIndex < locales.length; localeIndex++) {
    const locale = locales[localeIndex];
    out.push("L," + localeIndex + "," + digest([
      () => duration.toLocaleString(locale, { style: "long" }),
      () => plainDate.toLocaleString(locale, { era: "narrow" }),
      () => plainDateTime.toLocaleString(locale, { era: "narrow" }),
      () => plainYearMonth.toLocaleString(locale, { era: "narrow" }),
      () => plainTime.toLocaleString(locale, { hour12: false }),
      () => plainTime.toLocaleString(locale, { hourCycle: "h23" }),
      () => plainTime.toLocaleString(locale, { hourCycle: "h24" }),
      () => plainTime.toLocaleString(locale, { hourCycle: "h11" }),
      () => plainTime.toLocaleString(locale, { hourCycle: "h12" })
    ]) + "\n");
    for (let zoneIndex = 0; zoneIndex < zoneData.length; zoneIndex++) {
      const data = zoneData[zoneIndex];
      const zone = data.zone;
      const nameFormatters = ["short", "long", "shortOffset", "longOffset",
        "shortGeneric", "longGeneric"].map(timeZoneName =>
          new Intl.DateTimeFormat(locale, { timeZone: zone, timeZoneName }));
      const nameAt = (formatter, instant) => formatter.formatToParts(instant)
        .find(part => part.type === "timeZoneName").value;
      const values = [
        () => instants[0].toLocaleString(locale, { timeZone: zone }),
        () => instants[1].toLocaleString(locale, { timeZone: zone }),
        () => instants[2].toLocaleString(locale, { timeZone: zone }),
        () => instants[3].toLocaleString(locale, { timeZone: zone }),
        () => data.zoned[1].toLocaleString(locale),
        () => data.zoned[2].toLocaleString(locale),
        () => plainDateTime.toLocaleString(locale, { timeZone: zone }),
        () => plainDate.toLocaleString(locale, { timeZone: zone }),
        () => plainTime.toLocaleString(locale, { timeZone: zone }),
        () => plainYearMonth.toLocaleString(locale, { timeZone: zone }),
        () => plainMonthDay.toLocaleString(locale, { timeZone: zone }),
        () => instants[1].toLocaleString(locale,
          { timeZone: zone, era: "narrow" }),
        () => instants[1].toLocaleString(locale,
          { timeZone: zone, hour12: false }),
        () => data.zoned[1].toLocaleString(locale, { era: "narrow" }),
        () => data.zoned[1].toLocaleString(locale, { hourCycle: "h23" })
      ];
      for (const formatter of nameFormatters) {
        for (const instant of instants) {
          values.push(() => nameAt(formatter, instant));
        }
      }
      out.push("P," + localeIndex + "," + zoneIndex + "," +
        digest(values) + "\n");
    }
  }

  for (let zoneIndex = 0; zoneIndex < zoneData.length; zoneIndex++) {
    const data = zoneData[zoneIndex];
    const values = [];
    for (let instantIndex = 0; instantIndex < instants.length; instantIndex++) {
      const instant = instants[instantIndex];
      const zoned = data.zoned[instantIndex];
      values.push(
        () => instant.toString({ timeZone: data.zone }),
        () => zoned.toString(),
        () => zoned.offset,
        () => zoned.startOfDay().toString(),
        () => zoned.add({ days: 1 }).toString()
      );
    }
    out.push("Z," + zoneIndex + "," + digest(values) + "\n");
  }
  return out.join("");
})()`, localeJSON, zoneJSON)
}

func firstDifferentTemporalNodeRecord(want, got []byte) (string, string) {
	wantScanner := bufio.NewScanner(bytes.NewReader(want))
	gotScanner := bufio.NewScanner(bytes.NewReader(got))
	for {
		wantOK := wantScanner.Scan()
		gotOK := gotScanner.Scan()
		if !wantOK || !gotOK {
			if wantOK {
				return wantScanner.Text(), "<missing>"
			}
			if gotOK {
				return "<missing>", gotScanner.Text()
			}
			return "<end>", "<end>"
		}
		if wantScanner.Text() != gotScanner.Text() {
			return strconv.Quote(wantScanner.Text()), strconv.Quote(gotScanner.Text())
		}
	}
}
