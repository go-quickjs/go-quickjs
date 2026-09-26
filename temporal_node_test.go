package quickjs_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	intl "github.com/go-quickjs/go-intl"
	quickjs "github.com/go-quickjs/go-quickjs"
)

// TestTemporalMatchesNodeAcrossLocalesAndTimeZones compares every locale
// DateTimeFormat is available in with every zone Node lists. It is opt-in
// because the complete matrix is deliberately large and requires a recent
// Node with Temporal.
//
// The locales are cut into shards, and each shard is run in its own Node
// process and its own runtime, as many at once as there are CPUs, or as
// QUICKJS_NODE_TEMPORAL_PARALLEL says. The records keep their places in the
// whole matrix, so the outputs joined in order are the matrix's.
//
// Run it with:
//
//	QUICKJS_COMPARE_NODE_TEMPORAL=1 go test . \
//	  -run '^TestTemporalMatchesNodeAcrossLocalesAndTimeZones$' \
//	  -count=1 -timeout=60m -v
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
	workers := runtime.NumCPU()
	if raw := os.Getenv("QUICKJS_NODE_TEMPORAL_PARALLEL"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			t.Fatalf("QUICKJS_NODE_TEMPORAL_PARALLEL=%q is not a positive number", raw)
		}
		workers = n
	}
	node := os.Getenv("NODE_BINARY")
	if node == "" {
		node = "node"
	}

	// Shards of a few locales each, small enough to spread over the
	// workers evenly, and a last one for the records of the zones alone.
	const perShard = 16
	var shards []string
	for start := 0; start < len(locales); start += perShard {
		end := min(start+perShard, len(locales))
		shards = append(shards, temporalNodeMatrixExpression(t, locales[start:end], start, zones, false))
	}
	shards = append(shards, temporalNodeMatrixExpression(t, []string{}, len(locales), zones, true))

	// Job 2i runs shard i in Node and job 2i+1 in a runtime of its own.
	wants := make([][]byte, len(shards))
	gots := make([][]byte, len(shards))
	errs := make([]error, 2*len(shards))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				i := j / 2
				if j%2 == 0 {
					wants[i], errs[j] = runTemporalNodeShard(node, shards[i])
				} else {
					gots[i], errs[j] = evalTemporalNodeShard(shards[i])
				}
			}
		}()
	}
	for j := range errs {
		jobs <- j
	}
	close(jobs)
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	want, got := bytes.Join(wants, nil), bytes.Join(gots, nil)
	if bytes.Equal(got, want) {
		t.Logf("matched Node for %d locales x %d time zones (%d pairs)",
			len(locales), len(zones), len(locales)*len(zones))
		return
	}

	wantRecord, gotRecord := firstDifferentTemporalNodeRecord(want, got)
	t.Fatalf("Temporal differs from Node in the first matrix record:\nnode    %s\nquickjs %s",
		wantRecord, gotRecord)
}

// runTemporalNodeShard is what Node writes for one shard of the matrix.
func runTemporalNodeShard(node, expression string) ([]byte, error) {
	command := exec.Command(node, "-")
	command.Stdin = strings.NewReader("process.stdout.write(" + expression + ");\n")
	out, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("run %s: %v\n%s", node, err, out)
	}
	return out, nil
}

// evalTemporalNodeShard is what a runtime with Node's quirks answers for one
// shard of the matrix. Each shard has a runtime of its own, since a runtime
// is not safe to share.
func evalTemporalNodeShard(expression string) ([]byte, error) {
	rt := quickjs.New(quickjs.WithNodeQuirks())
	defer rt.Close()
	value, err := rt.Eval(expression)
	if err != nil {
		return nil, fmt.Errorf("evaluate Temporal matrix: %v", err)
	}
	return []byte(value.String()), nil
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

// temporalNodeMatrixExpression is one shard of the matrix: its locales, the
// first of which is locale number offset of the whole, by every zone, and
// the records of the zones alone where withZones says.
func temporalNodeMatrixExpression(t *testing.T, locales []string, offset int, zones []string, withZones bool) string {
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
  const offset = %d;
  const withZones = %t;
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
    out.push("L," + (offset + localeIndex) + "," + digest([
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
      out.push("P," + (offset + localeIndex) + "," + zoneIndex + "," +
        digest(values) + "\n");
    }
  }

  for (let zoneIndex = 0; withZones && zoneIndex < zoneData.length; zoneIndex++) {
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
})()`, localeJSON, zoneJSON, offset, withZones)
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
