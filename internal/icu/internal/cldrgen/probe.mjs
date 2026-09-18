// probe.mjs works out which locales are worth carrying, by asking an ICU that
// has all of them what each one actually produces.
//
// Usage:
//
//	node internal/icu/internal/cldrgen/probe.mjs > internal/icu/internal/cldrgen/locales.json
//
// A language is carried if ICU has data for it. A region or script variant is
// carried only if it says something its language does not -- Swiss German
// writes its numbers differently, Egyptian Arabic writes its digits
// differently -- and two variants that say the same thing share one entry,
// with the other recorded as an alias. That is what keeps three hundred and
// eighty locales' worth of data down to a size worth carrying.

const letters = "abcdefghijklmnopqrstuvwxyz".split("");

const candidates = [];
for (const a of letters) for (const b of letters) candidates.push(a + b);
for (const a of letters) for (const b of letters) for (const c of letters) {
  candidates.push(a + b + c);
}
const languages = Intl.DateTimeFormat.supportedLocalesOf(candidates);

// What a locale produces, in enough different ways that two locales with the
// same fingerprint really do produce the same thing.
const sample = new Date(Date.UTC(2024, 0, 5, 15, 4, 5));
const fingerprint = (tag) => [
  new Intl.DateTimeFormat(tag, {dateStyle: "full", timeZone: "UTC"}).format(sample),
  new Intl.DateTimeFormat(tag, {dateStyle: "short", timeStyle: "short", timeZone: "UTC"})
    .format(sample),
  new Intl.NumberFormat(tag).format(-1234567.891),
  ...["USD", "EUR", "CNY", "JPY", "GBP", "INR"].map(code =>
    new Intl.NumberFormat(tag, {style: "currency", currency: code}).format(9.5)),
  new Intl.NumberFormat(tag, {notation: "compact", compactDisplay: "long"}).format(1234567),
  new Intl.NumberFormat(tag, {notation: "compact"}).format(1234),
  new Intl.NumberFormat(tag, {notation: "compact"}).format(12345678),
  new Intl.PluralRules(tag).select(2) + new Intl.PluralRules(tag).select(5),
  new Intl.RelativeTimeFormat(tag, {numeric: "auto"}).format(-1, "day"),
  new Intl.ListFormat(tag).format(["a", "b", "c"]),
].join("|");

const REGIONS = [];
for (const a of letters) for (const b of letters) REGIONS.push((a + b).toUpperCase());
const SCRIPTS = ["Latn", "Cyrl", "Arab", "Hans", "Hant", "Deva", "Guru", "Beng",
                 "Ethi", "Adlm", "Tfng", "Vaii", "Nkoo", "Olck", "Mymr", "Sinh",
                 "Khmr", "Laoo", "Thaa", "Hebr", "Grek", "Armn", "Geor", "Jpan",
                 "Kore", "Cans", "Mong", "Orya", "Taml", "Telu", "Knda", "Mlym"];

const locales = [];
const aliases = {};
for (const language of languages) {
  locales.push(language);
  const base = fingerprint(language);
  // Which tag each distinct answer was first seen under, so that a second tag
  // with the same answer can point at it instead of repeating it.
  const seen = new Map();
  for (const extra of [...SCRIPTS, ...REGIONS]) {
    const tag = language + "-" + extra;
    let print;
    try {
      if (Intl.DateTimeFormat.supportedLocalesOf([tag]).length === 0) continue;
      print = fingerprint(tag);
    } catch (e) {
      // A tag this ICU cannot even be asked about.
      continue;
    }
    // Saying what the language says is not a variant at all: falling back to
    // the language gives the right answer.
    if (print === base) continue;
    const already = seen.get(print);
    if (already !== undefined) {
      aliases[tag] = already;
      continue;
    }
    seen.set(print, tag);
    locales.push(tag);
  }
}

process.stdout.write(JSON.stringify({locales, aliases}, null, 1) + "\n");
