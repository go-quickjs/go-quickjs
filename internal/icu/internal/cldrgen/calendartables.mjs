// calendartables.mjs reads the calendars that cannot be computed.
//
// Usage:
//
//	node internal/icu/internal/cldrgen/calendartables.mjs > .../calendartables.json
//
// Most calendars are arithmetic: given the day, a few divisions say which year
// and month it falls in. Three are not. The Islamic calendar as ICU reckons it
// follows the moon rather than a table, and the Umm al-Qura calendar follows a
// table kept in Saudi Arabia; the Persian year begins at the spring equinox as
// it falls in Tehran, which is astronomy and not arithmetic. And the Japanese
// year is counted from the start of an emperor's reign, which is history.
//
// So those are read rather than computed: where each month begins, and where
// each reign does. What comes out is a few kilobytes and covers the years a
// program is likely to ask about; outside them the arithmetic takes over.

// The day number of the first of January 1970, counted from the first of
// January in the year 1, which is what the Go side counts in.
const UNIX_EPOCH = 719163;
const DAY = 86400000;

const fixedOf = (ms) => UNIX_EPOCH + Math.round(ms / DAY);

// Where each month of a calendar begins, over the years a program is likely to
// ask about.
function monthStarts(calendar, fromYear, toYear) {
  const f = new Intl.DateTimeFormat("en-u-ca-" + calendar, {
    year: "numeric", month: "numeric", day: "numeric",
    timeZone: "UTC", numberingSystem: "latn",
  });
  const read = (ms) => {
    const out = {};
    for (const part of f.formatToParts(ms)) {
      if (part.type !== "literal") out[part.type] = part.value;
    }
    return out;
  };

  const starts = [];
  let last = null;
  for (let ms = Date.UTC(fromYear, 0, 1); ms < Date.UTC(toYear, 0, 1); ms += DAY) {
    const at = read(ms);
    const key = at.year + "/" + at.month;
    if (key !== last) {
      starts.push({fixed: fixedOf(ms), year: Number(at.year), month: at.month, day: Number(at.day)});
      last = key;
    }
  }
  // The first is partial -- the range began in the middle of a month -- so it
  // is dropped.
  return starts.filter(s => s.day === 1);
}

// The lengths of the months, as one digit each, along with where the first of
// them begins. A month is twenty-nine, thirty or thirty-one days.
function lengthsOf(starts) {
  const lengths = [];
  for (let i = 1; i < starts.length; i++) {
    lengths.push(String(starts[i].fixed - starts[i - 1].fixed - 28));
  }
  return {
    from: starts[0].fixed,
    year: starts[0].year,
    month: starts[0].month,
    lengths: lengths.join(""),
  };
}

// Where each reign begins, which is where the Japanese year count restarts.
function japaneseEras() {
  const f = new Intl.DateTimeFormat("en-u-ca-japanese", {
    era: "short", year: "numeric", timeZone: "UTC", numberingSystem: "latn",
  });
  const eraOf = (ms) => f.formatToParts(ms)
    .filter(p => p.type === "era").map(p => p.value).join("");

  const out = [];
  let last = null;
  for (let ms = Date.UTC(600, 0, 1); ms < Date.UTC(2035, 0, 1); ms += DAY) {
    const era = eraOf(ms);
    if (era !== last) {
      out.push({fixed: fixedOf(ms), era});
      last = era;
    }
  }
  return out;
}

process.stdout.write(JSON.stringify({
  icu: process.versions.icu,
  // The Islamic calendars that follow the moon or a table rather than a rule.
  islamic: lengthsOf(monthStarts("islamic", 1500, 2500)),
  "islamic-umalqura": lengthsOf(monthStarts("islamic-umalqura", 1500, 2500)),
  // The Persian year, which begins at the equinox.
  persian: lengthsOf(monthStarts("persian", 1500, 2500)),
  eras: japaneseEras(),
}) + "\n");
