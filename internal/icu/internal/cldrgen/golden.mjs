// golden.mjs writes what a full ICU produces for a corpus of formatting calls,
// so that the engine's own Intl can be held against it without needing node.
//
// Usage:
//
//	node internal/icu/internal/cldrgen/golden.mjs > testdata/intl_golden.json
//
// Each case is the source of an expression and what ICU answered. A case whose
// answer this engine cannot reach -- a locale it carries no data for, a time
// zone it does not know -- is left out rather than recorded as a failure that
// is not one.

// A locale from each of the shapes the data takes: the separators, the
// grouping, the digits, the plural rules, the calendar, and the order the
// fields go in.
const LOCALES = ["en", "en-GB", "en-IN", "de", "de-CH", "fr", "es", "pl", "ru",
                 "ar-EG", "hi", "th", "ja", "zh", "zh-Hant"];

const NUMBERS = [0, 1, -1, 0.5, 42, 1234.5, -1234.5, 1234567.891, 12345678,
                 0.256, 123456789012];

const cases = [];
const add = (source, value) => cases.push({source, value});

const q = JSON.stringify;

// --- numbers ---------------------------------------------------------------

const NUMBER_OPTIONS = [
  {},
  {minimumFractionDigits: 2},
  {maximumFractionDigits: 0},
  {minimumFractionDigits: 1, maximumFractionDigits: 3},
  {useGrouping: false},
  {minimumIntegerDigits: 3},
  {style: "percent"},
  {style: "percent", maximumFractionDigits: 1},
  {style: "currency", currency: "USD"},
  {style: "currency", currency: "EUR"},
  {style: "currency", currency: "JPY"},
  {style: "currency", currency: "INR"},
  {style: "currency", currency: "USD", currencyDisplay: "code"},
  {notation: "compact"},
  {notation: "compact", compactDisplay: "long"},
  {signDisplay: "always"},
  {signDisplay: "exceptZero"},
  {signDisplay: "never"},
];

for (const locale of LOCALES) {
  for (const options of NUMBER_OPTIONS) {
    for (const n of NUMBERS) {
      const source = `new Intl.NumberFormat(${q(locale)}, ${q(options)}).format(${n})`;
      add(source, new Intl.NumberFormat(locale, options).format(n));
    }
  }
}

// --- dates -----------------------------------------------------------------

const MOMENTS = [
  Date.UTC(2024, 0, 5, 15, 4, 5),
  Date.UTC(2024, 6, 20, 9, 30, 0),
  Date.UTC(2000, 1, 29, 0, 0, 0),
];

const DATE_OPTIONS = [
  {dateStyle: "full"}, {dateStyle: "long"}, {dateStyle: "medium"}, {dateStyle: "short"},
  {timeStyle: "medium"}, {timeStyle: "short"},
  {dateStyle: "medium", timeStyle: "short"},
  {dateStyle: "full", timeStyle: "medium"},
  {dateStyle: "long", timeStyle: "short"},
  {year: "numeric", month: "numeric", day: "numeric"},
  {year: "numeric", month: "2-digit", day: "2-digit"},
  {year: "numeric", month: "long", day: "numeric"},
  {year: "numeric", month: "short", day: "numeric"},
  {month: "long", day: "numeric"},
  {month: "short", day: "numeric"},
  {year: "numeric", month: "long"},
  {weekday: "long", year: "numeric", month: "long", day: "numeric"},
  {hour: "numeric", minute: "2-digit"},
  {hour: "2-digit", minute: "2-digit", second: "2-digit"},
  {hour: "numeric", minute: "2-digit", hour12: false},
  {hour: "numeric", minute: "2-digit", hour12: true},
  {year: "numeric"},
  {month: "long"},
  {day: "numeric"},
];

for (const locale of LOCALES) {
  for (const options of DATE_OPTIONS) {
    for (const when of MOMENTS) {
      const withZone = {timeZone: "UTC", ...options};
      const source =
        `new Intl.DateTimeFormat(${q(locale)}, ${q(withZone)}).format(${when})`;
      add(source, new Intl.DateTimeFormat(locale, withZone).format(when));
    }
  }
}

// --- plurals, lists and relative times -------------------------------------

for (const locale of LOCALES) {
  for (const type of ["cardinal", "ordinal"]) {
    for (const n of [0, 1, 2, 5, 11, 21, 101, 111, 1000, 1.5]) {
      const source =
        `new Intl.PluralRules(${q(locale)}, {"type":${q(type)}}).select(${n})`;
      add(source, new Intl.PluralRules(locale, {type}).select(n));
    }
  }

  for (const type of ["conjunction", "disjunction"]) {
    for (const items of [["a"], ["a", "b"], ["a", "b", "c"], ["a", "b", "c", "d"]]) {
      const source =
        `new Intl.ListFormat(${q(locale)}, {"type":${q(type)}}).format(${q(items)})`;
      add(source, new Intl.ListFormat(locale, {type}).format(items));
    }
  }

  for (const numeric of ["always", "auto"]) {
    for (const unit of ["second", "minute", "hour", "day", "week", "month", "year"]) {
      for (const n of [-2, -1, 0, 1, 5, 21]) {
        const source = `new Intl.RelativeTimeFormat(${q(locale)}, ` +
          `{"numeric":${q(numeric)}}).format(${n}, ${q(unit)})`;
        add(source, new Intl.RelativeTimeFormat(locale, {numeric}).format(n, unit));
      }
    }
  }
}

// --- the methods on the values themselves ----------------------------------

for (const locale of LOCALES) {
  for (const n of [1234.5, 0.5, 1e9]) {
    add(`(${n}).toLocaleString(${q(locale)})`, (n).toLocaleString(locale));
  }
  const when = MOMENTS[0];
  add(`new Date(${when}).toLocaleDateString(${q(locale)}, {"timeZone":"UTC"})`,
      new Date(when).toLocaleDateString(locale, {timeZone: "UTC"}));
  add(`new Date(${when}).toLocaleTimeString(${q(locale)}, {"timeZone":"UTC"})`,
      new Date(when).toLocaleTimeString(locale, {timeZone: "UTC"}));
  add(`new Date(${when}).toLocaleString(${q(locale)}, {"timeZone":"UTC"})`,
      new Date(when).toLocaleString(locale, {timeZone: "UTC"}));
}

// One case per line: the expression, a tab, and what ICU answered. Shorter
// than JSON, and a diff of it reads as what changed rather than as noise.
let out = "# ICU " + process.versions.icu + "\n";
for (const c of cases) {
  out += c.source + "\t" + JSON.stringify(c.value) + "\n";
}
process.stdout.write(out);
