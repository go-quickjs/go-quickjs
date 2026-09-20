// extract.mjs reads locale data out of an ICU that already has it and writes
// it as JSON on standard output. It is run by cldrgen, which turns that JSON
// into the Go tables the icu package carries.
//
// The data is taken through Intl rather than from the CLDR files because Intl
// answers the question the tables are for -- what does this locale actually
// produce -- and answers it consistently: a month name and the pattern it
// appears in come from the same version of the same data.

// The date every pattern is read from. A Friday, the fifth of January, in the
// morning: every field has a value that says how wide it is written, and the
// hour is a single digit so that H can be told from HH -- which it cannot be
// at three in the afternoon, where both are two digits.
const SAMPLE = new Date(Date.UTC(2024, 0, 5, 9, 4, 5));
const SAMPLE_PM = new Date(Date.UTC(2024, 0, 5, 15, 4, 5));

// The locales to read, worked out by probe.mjs: every language this ICU has
// data for, and every region or script variant of one that says something its
// language does not.
import fs from "fs";
import path from "path";
import {fileURLToPath} from "url";

const here = path.dirname(fileURLToPath(import.meta.url));
const {locales: LOCALES} = JSON.parse(
  fs.readFileSync(path.join(here, "locales.json"), "utf8"));

// The currencies worth carrying a symbol for in every locale. The rest fall
// back to their code, which is what a currency without a symbol is written as.
const CURRENCIES = [
  "USD", "EUR", "GBP", "JPY", "CNY", "KRW", "INR", "RUB", "BRL", "CAD", "AUD",
  "NZD", "MXN", "CHF", "SEK", "NOK", "DKK", "PLN", "TRY", "ILS", "THB", "VND",
  "NGN", "ZAR", "PHP", "UAH", "IDR", "MYR", "SGD", "HKD", "TWD", "CZK", "HUF",
  "RON", "BGN", "HRK", "ISK", "EGP", "SAR", "AED", "PKR", "BDT", "LKR", "NPR",
  "KES", "GHS", "TZS", "UGX", "MAD", "DZD", "TND", "ARS", "CLP", "COP", "PEN",
  "UYU", "VES", "BOB", "PYG", "CRC", "DOP", "GTQ", "HNL", "NIO", "PAB", "JMD",
  "TTD", "BBD", "BSD", "BMD", "KYD", "XCD", "AWG", "ANG", "SRD", "GYD", "BZD",
  "KWD", "BHD", "OMR", "QAR", "JOD", "LBP", "IQD", "IRR", "AFN", "AMD", "AZN",
  "GEL", "KZT", "KGS", "TJS", "TMT", "UZS", "MNT", "MMK", "KHR", "LAK", "BND",
  "FJD", "PGK", "SBD", "TOP", "VUV", "WST", "XPF", "MOP", "MVR", "BTN", "SCR",
  "MUR", "MGA", "MWK", "ZMW", "BWP", "NAD", "SZL", "LSL", "MZN", "AOA", "XOF",
  "XAF", "CDF", "RWF", "BIF", "DJF", "ETB", "ERN", "SOS", "SDG", "SSP", "LYD",
  "GMD", "GNF", "LRD", "SLE", "CVE", "STN", "KMF", "BYN", "MDL", "RSD", "MKD",
  "ALL", "BAM", "TND", "PLN",
];

const RELATIVE_UNITS = ["second", "minute", "hour", "day", "week", "month",
                        "quarter", "year"];

// --- date patterns ----------------------------------------------------------

// pattern turns what a format produced into the CLDR-ish pattern that produced
// it: the fields as letters, and everything else as a literal.
function pattern(parts, hour12, resolved = {}) {
  let out = "";
  for (const part of parts) {
    switch (part.type) {
      case "literal": out += quote(part.value); break;
      case "era": out += "G"; break;
      case "year": out += resolved.year === "2-digit" ||
        (resolved.year === undefined && [...part.value].length <= 2) ? "yy" : "y"; break;
      case "relatedYear": out += "y"; break;
      case "yearName": out += "y"; break;
      case "month": out += monthLetters(part.value, resolved.month); break;
      case "day": out += resolved.day === "2-digit" ||
        (resolved.day === undefined && [...part.value].length === 2) ? "dd" : "d"; break;
      case "weekday": out += weekdayLetters(part.value); break;
      // B is the flexible day period -- the small hours, the evening -- and a
      // is the plain one, morning or afternoon. Which a locale writes is a
      // fact about the locale, not about the hour being written.
      case "dayPeriod": out += currentNames.flexible ? "B" : "a"; break;
      case "hour": out += (hour12 ? "h" : "H").repeat(
        resolved.hour === "2-digit" ||
        (resolved.hour === undefined && [...part.value].length === 2) ? 2 : 1); break;
      case "minute": out += resolved.minute === "2-digit" ||
        (resolved.minute === undefined && [...part.value].length === 2) ? "mm" : "m"; break;
      case "second": out += resolved.second === "2-digit" ||
        (resolved.second === undefined && [...part.value].length === 2) ? "ss" : "s"; break;
      case "fractionalSecond": out += "S"; break;
      // A zone named in full is four letters, and one named in short is one:
      // "Coordinated Universal Time" against "UTC".
      case "timeZoneName": out += part.value.length > 6 ? "zzzz" : "z"; break;
      // Anything else is carried as a literal, which is what it looks like.
      default: out += quote(part.value); break;
    }
  }
  return out;
}

// Some ICU builds expose a broken field iterator for a numeric year and month:
// format() succeeds, but formatToParts() aborts the process. Read that simple
// two-field pattern from the digit runs so one bad iterator cannot make the
// generator drop an otherwise supported locale.
function numericYearMonthPattern(locale) {
  const formatter = new Intl.DateTimeFormat(locale, {
    year: "numeric", month: "numeric", timeZone: "UTC",
  });
  const resolved = formatter.resolvedOptions();
  if (resolved.year !== undefined && resolved.month !== undefined) return null;
  const text = formatter.format(SAMPLE);
  const fields = [...text.matchAll(/\p{Nd}+/gu)];
  if (fields.length !== 2) return null;

  let out = "";
  let at = 0;
  let sawYear = false;
  let sawMonth = false;
  for (const field of fields) {
    out += quote(text.slice(at, field.index));
    const width = [...field[0]].length;
    if (width > 2 && !sawYear) {
      out += "y";
      sawYear = true;
    } else if (!sawMonth) {
      out += width === 2 ? "MM" : "M";
      sawMonth = true;
    } else {
      return null;
    }
    at = field.index + field[0].length;
  }
  out += quote(text.slice(at));
  return sawYear && sawMonth ? out : null;
}

// A month or a weekday is a number or a name, and which it is shows in what
// was produced: names are matched against the lists read for this locale.
//
// The match is loose on purpose. A pattern may carry the full stop after an
// abbreviation as a literal, so "Jan" has to be recognised as the same width
// as "Jan.", and a locale's digits are not always the ASCII ones.
let currentNames = null;
const isNumber = (value) => /^[\p{Nd}]+$/u.test(value);
const bare = (text) => text.replace(/[.\u2024\uFF0E\u0589\u06D4]+$/u, "").toLowerCase();

function widthOf(value, long, short, narrow, letters) {
  const want = bare(value);
  if (long.some(n => bare(n) === want)) return letters[0];
  if (short.some(n => bare(n) === want)) return letters[1];
  if (narrow.some(n => bare(n) === want)) return letters[2];
  // Nothing matched, so the width is judged by what it looks like: one
  // character is a narrow form, anything longer is an abbreviation.
  return [...want].length <= 1 ? letters[2] : letters[1];
}

function monthLetters(value, requested) {
  if (isNumber(value)) {
    if (requested === "numeric") return "M";
    if (requested === "2-digit") return "MM";
    return [...value].length === 2 ? "MM" : "M";
  }
  // A name that is the stand-alone form and not the one used in a date is
  // written with L, which is what that letter is for.
  const standalone = currentNames.monthsAlone.length > 0 &&
    currentNames.monthsAlone.includes(value) && !currentNames.months.includes(value);
  const letters = standalone ? ["LLLL", "LLL", "LLLLL"] : ["MMMM", "MMM", "MMMMM"];
  if (standalone) {
    return widthOf(value, currentNames.monthsAlone, currentNames.monthsAloneShort,
                   currentNames.monthsNarrow, letters);
  }
  return widthOf(value, currentNames.months, currentNames.monthsShort,
                 currentNames.monthsNarrow, letters);
}

function weekdayLetters(value) {
  // E is the form used inside a date, while c is the stand-alone form. They
  // are different words in a handful of locales (and differ only in case in
  // several more), so retain the context in the recovered pattern.
  const format = [currentNames.daysFormat, currentNames.daysFormatShort,
                  currentNames.daysFormatNarrow];
  const alone = [currentNames.days, currentNames.daysShort, currentNames.daysNarrow];
  const inLists = (lists) => lists.some(list => list.includes(value));
  const standalone = currentNames.daysFormat.length > 0 &&
    inLists(alone) && !inLists(format);
  const lists = standalone ? alone : format;
  return widthOf(value,
    lists[0].length > 0 ? lists[0] : alone[0],
    lists[1].length > 0 ? lists[1] : alone[1],
    lists[2].length > 0 ? lists[2] : alone[2],
    standalone ? ["cccc", "ccc", "ccccc"] : ["EEEE", "EEE", "EEEEE"]);
}

// quote escapes a literal so that the letters in it are not read as fields.
//
// The narrow space that ICU puts before a day period is written as an ordinary
// space, because that is what format() produces -- formatToParts keeps the
// narrow one, and the two disagree in the engine this is read from. What a
// program compares against is what format() gave it.
function quote(text) {
  text = text.replace(/\u202f/g, " ");
  if (text === "") return "";
  if (!/[A-Za-z']/.test(text)) return text;
  return "'" + text.replace(/'/g, "''") + "'";
}

function dateNames(locale) {
  // A month has two forms in some languages: the one it is called, and the one
  // it takes in a date. Polish has styczeń and stycznia, and which is written
  // depends on whether a day is written with it.
  const monthInDate = (month, width) => {
    const when = new Date(Date.UTC(2024, month, 15));
    const parts = new Intl.DateTimeFormat(locale, {
      month: width, day: "numeric", timeZone: "UTC",
    }).formatToParts(when);
    const found = parts.find(p => p.type === "month");
    return found ? found.value : monthAlone(month, width, locale);
  };
  const monthAt = (month, width) => new Intl.DateTimeFormat(locale, {
    month: width, timeZone: "UTC",
  }).format(new Date(Date.UTC(2024, month, 15)));
  const dayAt = (day, width) => new Intl.DateTimeFormat(locale, {
    weekday: width, timeZone: "UTC",
  }).format(new Date(Date.UTC(2024, 0, 7 + day)));  // 7 January 2024 was a Sunday
  const dayInDate = (day, width) => {
    const parts = new Intl.DateTimeFormat(locale, {
      weekday: width, year: "numeric", month: "long", day: "numeric", timeZone: "UTC",
    }).formatToParts(new Date(Date.UTC(2024, 0, 7 + day)));
    return parts.find(part => part.type === "weekday")?.value || dayAt(day, width);
  };

  const twelve = (width) => Array.from({length: 12}, (_, i) => monthInDate(i, width));
  const alone = (width) => Array.from({length: 12}, (_, i) => monthAt(i, width));
  const seven = (width) => Array.from({length: 7}, (_, i) => dayAt(i, width));
  const sevenInDate = (width) => Array.from({length: 7}, (_, i) => dayInDate(i, width));

  const months = twelve("long");
  const monthsAlone = alone("long");
  const same = months.every((m, i) => m === monthsAlone[i]);
  const days = seven("long");
  const daysShort = seven("short");
  const daysNarrow = seven("narrow");
  const daysFormat = sevenInDate("long");
  const daysFormatShort = sevenInDate("short");
  const daysFormatNarrow = sevenInDate("narrow");
  const different = (a, b) => a.some((value, i) => value !== b[i]);
  return {
    months,
    monthsShort: twelve("short"),
    monthsNarrow: twelve("narrow"),
    // Only carried where the two forms differ, which is a minority of
    // languages.
    monthsAlone: same ? [] : monthsAlone,
    monthsAloneShort: same ? [] : alone("short"),
    days,
    daysShort,
    daysNarrow,
    daysFormat: different(days, daysFormat) ? daysFormat : [],
    daysFormatShort: different(daysShort, daysFormatShort) ? daysFormatShort : [],
    daysFormatNarrow: different(daysNarrow, daysFormatNarrow) ? daysFormatNarrow : [],
  };
}

// monthAlone is the name of a month by itself, for the fallback above.
function monthAlone(month, width, locale) {
  return new Intl.DateTimeFormat(locale, {month: width, timeZone: "UTC"})
    .format(new Date(Date.UTC(2024, month, 15)));
}

// --- numbers ----------------------------------------------------------------

function numberData(locale) {
  const parts = new Intl.NumberFormat(locale).formatToParts(-1234567.891);
  const find = (type) => {
    const part = parts.find(p => p.type === type);
    return part ? part.value : undefined;
  };
  const digits = Array.from(new Intl.NumberFormat(locale, {useGrouping: false})
    .format(1234567890));
  // The digits come out in the order 1234567890, so zero is last.
  const table = digits[9] + digits.slice(0, 9).join("");

  // Every style's pattern, positive and negative, read the same way: the
  // number becomes {0}, a currency symbol becomes the mark that stands for
  // one, and everything else -- the minus, the percent sign, the marks a
  // right-to-left language puts around it -- is written as it is.
  const pattern = (options, value) => {
    let out = "";
    let seenNumber = false;
    for (const part of new Intl.NumberFormat(locale, options).formatToParts(value)) {
      switch (part.type) {
        case "currency": out += "\u00a4"; break;
        case "integer": case "fraction": case "decimal": case "group":
          if (!seenNumber) { out += "{0}"; seenNumber = true; }
          break;
        default: out += part.value; break;
      }
    }
    return out;
  };

  const grouped = new Intl.NumberFormat(locale).format(1234567);
  return {
    decimal: find("decimal") || ".",
    group: find("group") || ",",
    minus: find("minusSign") || "-",
    percentSign: new Intl.NumberFormat(locale, {style: "percent"})
      .formatToParts(0.25).find(p => p.type === "percentSign").value,
    nan: new Intl.NumberFormat(locale).format(NaN),
    infinity: new Intl.NumberFormat(locale).format(Infinity),
    digits: table === "0123456789" ? "" : table,
    // Where the separators fall: most languages group in threes, some put the
    // first one after three and the rest after two. How many digits are in the
    // group before the last is what tells them apart.
    indian: (() => {
      const groups = groupsOf(grouped, find("group") || ",");
      return groups.length >= 2 && groups[groups.length - 2] === 2;
    })(),
    percentPattern: pattern({style: "percent"}, 0.25),
    currencyPattern: pattern({style: "currency", currency: "USD"}, 1234.5),
    decimalNegative: pattern({}, -1234.5),
    percentNegative: pattern({style: "percent"}, -0.25),
    currencyNegative: pattern({style: "currency", currency: "USD"}, -1234.5),
    // Accounting writes a loss in brackets rather than with a minus, which is
    // a pattern of its own and not a rule that can be applied to the other.
    accounting: pattern(
      {style: "currency", currency: "USD", currencySign: "accounting"}, -1234.5),
    // What stands between the two ends of a range, and the mark that says a
    // number is only approximate: both are the language's own.
    // Which clock the language keeps: the one it uses by default, and the
    // ones it uses when a twelve-hour or a twenty-four-hour clock is asked
    // for.
    hourCycles: (() => {
      const at = (options) => new Intl.DateTimeFormat(locale,
        {hour: "numeric", ...options}).resolvedOptions().hourCycle || "";
      return [at({}), at({hour12: true}), at({hour12: false})].join(",");
    })(),
    range: (() => {
      const parts = new Intl.NumberFormat(locale).formatRangeToParts(1, 5);
      const between = parts.filter(p => p.source === "shared" && p.type === "literal");
      return between.length > 0 ? between[0].value : "\u2013";
    })(),
    approximately: (() => {
      const parts = new Intl.NumberFormat(locale).formatRangeToParts(3, 3);
      const sign = parts.find(p => p.type === "approximatelySign");
      return sign ? sign.value : "~";
    })(),
    // The mark that stands between a number and its exponent: E nearly
    // everywhere, and a word of its own in a few languages.
    exponential: (() => {
      const part = new Intl.NumberFormat(locale, {notation: "scientific"})
        .formatToParts(12345).find(p => p.type === "exponentSeparator");
      return part ? part.value : "E";
    })(),
    // How many digits the first group must have for there to be one at all:
    // Spanish and Italian write 1234 without a separator and 12.345 with one.
    minGrouping: new Intl.NumberFormat(locale).format(1234)
      .includes(find("group") || ",") ? 1 : 2,
  };
}

const escapeRe = (s) => s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");

// groupsOf reports how wide the separated groups of a number are. Which end
// they are counted from matters: the last group is three digits everywhere,
// and what comes before it is three digits in most languages and two in the
// Indian convention -- 1,234,567 against 12,34,567.
function groupsOf(text, separator) {
  return text.split(separator).map(piece =>
    [...piece].filter(c => /[\p{Nd}]/u.test(c)).length);
}

// compactForms reads how a language shortens a large number. Which power it
// shortens at is part of the language: English counts in thousands, Japanese
// in ten-thousands, so what is stored for each power of ten is what it is
// divided by and what is written after it.
function compactForms(locale) {
  const plurals = new Intl.PluralRules(locale);
  const out = {};
  for (const style of ["short", "long"]) {
    const entries = {};
    for (let power = 3; power <= 14; power++) {
      const value = Math.pow(10, power);
      const read = (n) => {
        let parts;
        try {
          parts = new Intl.NumberFormat(locale, {
            notation: "compact", compactDisplay: style,
          }).formatToParts(n);
        } catch (e) { return null; }
        const shown = parts.filter(p => ["integer", "fraction", "decimal"].includes(p.type))
          .map(p => p.type === "decimal" ? "." : p.value).join("");
        const suffix = parts.filter(p => p.type === "compact" || p.type === "literal")
          .map(p => p.value).join("");
        const digits = Number(latinDigits(shown, locale));
        if (!isFinite(digits) || digits <= 0) return null;
        return {digits, suffix};
      };

      // A language may have a word for exactly this many -- French says mille
      // rather than one thousand -- and then there are no digits to read the
      // divisor from, so a larger count of the same size is asked instead.
      let first = null;
      let sampled = value;
      for (const factor of [1, 1.2, 2, 3]) {
        sampled = value * factor;
        first = read(sampled);
        if (first !== null) break;
      }
      if (first === null) continue;

      // What this power is divided by, which says what range the shortened
      // number falls in: 10^11 divided by 10^9 is written as hundreds. It is
      // read from whichever sample answered, against that sample's own value.
      const divisor = Math.round(Math.log10(sampled / first.digits));
      const step = Math.pow(10, divisor);

      // A word can take a different form for each count, and which counts
      // those are depends on the language: Polish says miliardy for 123 and
      // miliardów for 125. So the forms are read across the range this power
      // is written in, far enough to reach every one of them.
      const suffixes = {};
      const base = Math.round(value / step);
      for (const offset of [0, 1, 2, 3, 4, 5, 6, 10, 11, 12, 13, 14, 21, 22, 25, 100]) {
        const scaled = base + offset;
        if (scaled * step >= value * 10) break;
        const got = read(scaled * step);
        if (got === null) continue;
        const category = plurals.select(got.digits);
        if (!(category in suffixes)) suffixes[category] = got.suffix;
      }
      // And the form a count with a fraction takes, which is its own case.
      const half = read(value * 1.5);
      if (half !== null) {
        const category = plurals.select(half.digits);
        if (!(category in suffixes)) suffixes[category] = half.suffix;
      }
      entries[power] = {divisor, suffixes};
    }
    out[style] = entries;
  }
  return out;
}

// latinDigits rewrites a number written in another script's digits as one this
// can parse.
function latinDigits(text, locale) {
  const digits = Array.from(new Intl.NumberFormat(locale, {useGrouping: false})
    .format(1234567890));
  const table = digits[9] + digits.slice(0, 9).join("");
  if (table === "0123456789") return text;
  let out = "";
  for (const ch of text) {
    const at = table.indexOf(ch);
    out += at < 0 ? ch : String(at);
  }
  return out;
}

function currencySymbols(locale) {
  const out = {};
  for (const code of new Set(CURRENCIES)) {
    try {
      const parts = new Intl.NumberFormat(locale, {
        style: "currency", currency: code,
      }).formatToParts(1);
      const symbol = parts.filter(p => p.type === "currency").map(p => p.value).join("");
      if (symbol && symbol !== code) out[code] = symbol;
    } catch (e) { /* a currency this ICU does not know */ }
  }
  return out;
}

// --- plurals ----------------------------------------------------------------

// The category a count falls into, as the answers rather than as the question.
//
// A plural rule in CLDR is arithmetic on a count: whether it is exactly one of
// a few small values, what its last two digits are, and -- in the languages
// that count in scores or in hundreds of thousands -- what it is modulo a
// larger power of ten. Rather than carry the arithmetic, this reads the
// answers: a hundred for the small counts, a hundred for the last two digits,
// and, where those are not enough, the residues that disagree at each of the
// moduli a rule may ask about.
//
// What comes out is checked against the engine it was read from, for every
// count up to a hundred thousand -- or up to three million for a language
// whose rules reach that far.
function pluralTable(locale, type, compact) {
  const rules = new Intl.PluralRules(locale, {type});

  const small = Array.from({length: 100}, (_, n) => rules.select(n));
  // What the last two digits say, read at several places so that a rule about
  // thousands is not mistaken for a rule about tens: Cornish says something
  // particular about a thousand exactly.
  const mod = Array.from({length: 100}, (_, r) => {
    const votes = {};
    // None of these is a round thousand, which is the shape a rule about
    // thousands takes: reading the tens from 1000 would learn the wrong thing.
    for (const base of [100, 300, 700, 1100, 12300, 54300]) {
      const category = rules.select(base + r);
      votes[category] = (votes[category] || 0) + 1;
    }
    return Object.keys(votes).reduce((a, b) => votes[a] >= votes[b] ? a : b);
  });

  const plain = (n) => n < 100 ? small[n] : mod[n % 100];

  // The moduli a rule may ask about. A count is attributed to the widest
  // class that covers it, so that one entry stands for a thousand counts
  // rather than a thousand entries standing for one each.
  const MODULI = [1000, 100000, 1000000];

  const derive = (limit) => {
    const categories = new Array(limit);
    for (let n = 0; n < limit; n++) categories[n] = rules.select(n);

    const disagree = [];
    for (let n = 100; n < limit; n++) {
      if (categories[n] !== plain(n)) disagree.push(n);
    }
    if (disagree.length === 0) return {classes: {}, exact: {}, wrong: []};

    // A residue is a class only if every count with that residue agrees, so
    // that recording it cannot make some other count wrong.
    const classes = {};
    for (const m of MODULI) {
      const byResidue = new Map();
      // Counts below a hundred are answered exactly and never reach a class,
      // so what they say has no bearing on whether one is consistent.
      for (let n = 100; n < limit; n++) {
        const r = n % m;
        const already = byResidue.get(r);
        if (already === undefined) byResidue.set(r, categories[n]);
        else if (already !== categories[n]) byResidue.set(r, null);
      }
      for (const n of disagree) {
        const key = m + ":" + (n % m);
        if (key in classes) continue;
        const agreed = byResidue.get(n % m);
        if (agreed !== null && agreed !== undefined) classes[key] = agreed;
      }
    }

    // Whatever no class covers is recorded as the count it is.
    const exact = {};
    const model = (n) => {
      if (n < 100) return small[n];
      if (n in exact) return exact[n];
      for (const m of MODULI) {
        const key = m + ":" + (n % m);
        if (key in classes) return classes[key];
      }
      return mod[n % 100];
    };
    for (const n of disagree) {
      if (model(n) !== categories[n]) exact[n] = categories[n];
    }

    const wrong = [];
    for (let n = 0; n < limit && wrong.length < 8; n++) {
      if (model(n) !== categories[n]) wrong.push(n);
    }
    return {classes, exact, wrong};
  };

  // Nearly every language is settled by a hundred thousand counts; a few ask
  // about millions, and are looked at further. Whether a language is one of
  // them is found by asking it about a handful of large round numbers, since
  // a rule about millions shows itself at a million and not before.
  const LARGE = [1000000, 1000001, 1500000, 2000000, 3000000, 10000000,
                 100000000, 1000000000];
  let found = derive(100000);
  const answers = (f) => (n) => {
    if (n < 100) return small[n];
    if (n in f.exact) return f.exact[n];
    for (const m of MODULI) {
      const key = m + ":" + (n % m);
      if (key in f.classes) return f.classes[key];
    }
    return mod[n % 100];
  };
  const missed = LARGE.some(n => answers(found)(n) !== rules.select(n));
  if (missed || Object.keys(found.classes).length > 0 ||
      Object.keys(found.exact).length > 0 || found.wrong.length > 0) {
    found = derive(3000000);
  }

  // Compact notation supplies CLDR's `e` operand: the power of ten factored
  // out of the displayed number. Derive the cases where that operand alone
  // decides the category. The exponents come from this locale's compact
  // patterns rather than from an assumed thousands/millions sequence; Japanese,
  // for example, counts in powers of four.
  const compactExponents = {};
  const compactRules = new Intl.PluralRules(locale, {
    type, notation: "compact", maximumFractionDigits: 20,
  });
  const plainRules = new Intl.PluralRules(locale, {
    type, notation: "standard", maximumFractionDigits: 20,
  });
  const supportsNotation = compactRules.resolvedOptions().notation === "compact";
  const byExponent = new Map();
  for (const forms of Object.values(compact)) {
    for (const [powerText, form] of Object.entries(forms)) {
      if (form.divisor <= 0) continue;
      const power = Number(powerText);
      let samples = byExponent.get(form.divisor);
      if (!samples) byExponent.set(form.divisor, samples = []);
      for (const factor of [1, 1.1, 1.5, 2, 3, 5, 7, 9.9]) {
        samples.push(factor * Math.pow(10, power));
      }
    }
  }
  for (const [exponent, samples] of byExponent) {
    const categories = new Set(samples.map(n => compactRules.select(n)));
    const differs = samples.some(n => compactRules.select(n) !== plainRules.select(n));
    if (categories.size === 1 && differs) {
      compactExponents[exponent] = categories.values().next().value;
    } else if (!supportsNotation) {
      // Node releases that predate PluralRules notation silently ignore the
      // option. CLDR's only exponent-dependent rule is the Romance "many"
      // rule: a compact million/billion is many regardless of its mantissa.
      const atPower = plainRules.select(Math.pow(10, exponent));
      if (atPower === "many" && samples.some(n => plainRules.select(n) !== atPower)) {
        compactExponents[exponent] = atPower;
      }
    }
  }

  return {
    categories: rules.resolvedOptions().pluralCategories,
    small, mod,
    classes: found.classes,
    exact: found.exact,
    compactExponents,
    fractionZero: rules.select(0.5),
    fractionOther: rules.select(1.5),
    disagrees: found.wrong,
  };
}

// --- lists and relative times -----------------------------------------------

// listData reads the pieces a list is built from, by formatting marks that
// cannot appear in the patterns themselves and reading what was put between
// them. Four items are needed: what joins the first pair, what joins the ones
// in the middle, and what joins the last.
function listData(locale) {
  const marks = ["\u0001", "\u0002", "\u0003", "\u0004"];
  const out = {};
  for (const type of ["conjunction", "disjunction", "unit"]) {
    for (const style of ["long", "short", "narrow"]) {
      let two, four;
      try {
        const fmt = new Intl.ListFormat(locale, {type, style});
        two = fmt.format(marks.slice(0, 2));
        four = fmt.format(marks);
      } catch (e) { continue; }
      const at = marks.map(m => four.indexOf(m));
      if (at.some(i => i < 0)) continue;
      out[type + "-" + style] = {
        pair: two.replace(marks[0], "{0}").replace(marks[1], "{1}"),
        start: four.slice(at[0] + 1, at[1]),
        middle: four.slice(at[1] + 1, at[2]),
        end: four.slice(at[2] + 1, at[3]),
      };
    }
  }
  return out;
}

function relativeData(locale) {
  const out = {};
  for (const style of ["long", "short", "narrow"]) {
    const forms = relativeStyle(locale, style);
    for (const [unit, entry] of Object.entries(forms)) {
      if (style === "long") {
        out[unit] = entry;
        continue;
      }
      // A style that says the same as the long one is left to it.
      if (JSON.stringify(entry) !== JSON.stringify(out[unit])) {
        out[style + "/" + unit] = entry;
      }
    }
  }
  return out;
}

function relativeStyle(locale, style) {
  const out = {};
  const always = new Intl.RelativeTimeFormat(locale, {numeric: "always", style});
  const auto = new Intl.RelativeTimeFormat(locale, {numeric: "auto", style});
  const numbers = new Intl.NumberFormat(locale);
  for (const unit of RELATIVE_UNITS) {
    const forms = {};
    // A count with a fraction takes a form of its own in some languages:
    // Polish says "dnia" for a day and a half and "dni" for five days.
    for (const n of [1, 2, 3, 5, 11, 21, 100, 1.5]) {
      for (const sign of [1, -1]) {
        const value = n * sign;
        const text = always.format(value, unit);
        const shown = numbers.format(Math.abs(value));
        const key = (sign < 0 ? "past:" : "future:") +
          new Intl.PluralRules(locale).select(n);
        if (key in forms) continue;
        const at = text.indexOf(shown);
        // A language may have a word for two of something rather than a way
        // of counting to two, and then there is nowhere to put the count.
        forms[key] = at < 0 ? text
          : text.slice(0, at) + "{0}" + text.slice(at + shown.length);
      }
    }
    // The words a language has instead of a number: yesterday, today.
    const named = {};
    for (const n of [-2, -1, 0, 1, 2]) {
      const text = auto.format(n, unit);
      if (text !== always.format(n, unit)) named[n] = text;
    }
    out[unit] = {forms, named};
  }
  return out;
}

// --- the whole of one locale ------------------------------------------------

function extract(locale) {
  const names = dateNames(locale);
  currentNames = names;

  const resolved = new Intl.DateTimeFormat(locale).resolvedOptions();
  const hour12 = new Intl.DateTimeFormat(locale, {timeStyle: "short"})
    .formatToParts(SAMPLE).some(p => p.type === "dayPeriod");

  // Reading one pattern: what it produces for the sample, with the day period
  // written as the letter this pattern actually uses. A locale that names the
  // parts of the day uses the plain morning-or-afternoon form in some of its
  // patterns and the finer one in others, and what tells them apart is whether
  // the name changes between an hour in the small hours and one in the
  // morning.
  const styled = (options) => {
    const formatter = new Intl.DateTimeFormat(locale, {timeZone: "UTC", ...options});
    const at = (hour) => formatter
      .formatToParts(new Date(Date.UTC(2024, 0, 5, hour, 4, 5)));
    const period = (parts) => {
      const found = parts.find(p => p.type === "dayPeriod");
      return found ? found.value : undefined;
    };
    const early = period(at(1));
    const morning = period(at(9));
    currentNames.flexible = early !== undefined && early !== morning;
    return pattern(at(9), hour12, formatter.resolvedOptions());
  };

  const styledTemporal = (options) => {
    const formatter = new Intl.DateTimeFormat(locale, {timeZone: "UTC", ...options});
    const at = (hour) => formatter.formatToParts(Temporal.Instant.from(
        `2024-01-05T${String(hour).padStart(2, "0")}:04:05Z`));
    const period = (parts) => {
      const found = parts.find(p => p.type === "dayPeriod");
      return found ? found.value : undefined;
    };
    const early = period(at(1));
    const morning = period(at(9));
    currentNames.flexible = early !== undefined && early !== morning;
    return pattern(at(9), hour12, formatter.resolvedOptions());
  };

  const dates = {}, times = {}, both = {}, glue = {};
  for (const style of ["full", "long", "medium", "short"]) {  // eslint-disable-line
    dates[style] = styled({dateStyle: style});
    times[style] = styled({timeStyle: style});
    both[style] = styled({dateStyle: style, timeStyle: style});
    // What a language puts between a date and a time -- " at ", " um ", a
    // space -- so that any two widths can be joined, not only matching ones.
    const joined = both[style];
    let at = joined.indexOf(dates[style]);
    let timeAt = joined.indexOf(times[style]);
    if (at >= 0 && timeAt >= 0 && timeAt < at) {
      // Vietnamese writes the time first, so the two are found the other way
      // round and the pattern says so.
      glue[style] = joined.slice(0, timeAt) + "{1}" +
        joined.slice(timeAt + times[style].length, at) + "{0}" +
        joined.slice(at + dates[style].length);
      continue;
    }
    timeAt = at < 0 ? -1 : joined.indexOf(times[style], at + dates[style].length);
    if (at >= 0 && timeAt > 0) {
      // A language may put a word on both sides of the time, as Hindi does:
      // the date, को, the time, बजे.
      glue[style] = joined.slice(0, at) + "{0}" +
        joined.slice(at + dates[style].length, timeAt) + "{1}" +
        joined.slice(timeAt + times[style].length);
    } else {
      glue[style] = "{0}, {1}";
    }
  }

  const temporalGlue = {};
  const temporalText = options => new Intl.DateTimeFormat(locale,
    {timeZone: "UTC", ...options}).format(
      Temporal.Instant.from("2024-01-05T09:04:05Z"));
  for (const style of ["full", "long", "medium", "short"]) {
    const date = temporalText({dateStyle: style});
    const time = temporalText({timeStyle: style});
    const joined = temporalText({dateStyle: style, timeStyle: style});
    const dateAt = joined.indexOf(date);
    let timeAt = joined.indexOf(time);
    if (dateAt >= 0 && timeAt >= 0 && timeAt < dateAt) {
      temporalGlue[style] = quote(joined.slice(0, timeAt)) + "{1}" +
        quote(joined.slice(timeAt + time.length, dateAt)) + "{0}" +
        quote(joined.slice(dateAt + date.length));
      continue;
    }
    timeAt = dateAt < 0 ? -1 : joined.indexOf(time, dateAt + date.length);
    temporalGlue[style] = dateAt >= 0 && timeAt > 0 ?
      quote(joined.slice(0, dateAt)) + "{0}" +
        quote(joined.slice(dateAt + date.length, timeAt)) + "{1}" +
        quote(joined.slice(timeAt + time.length)) : "{0}, {1}";
  }

  // What a program asks for by field rather than by style: the order the
  // locale puts them in, which is all that changes.
  const skeletons = {};
  for (const [name, options] of Object.entries({
    "yMd": {year: "numeric", month: "numeric", day: "numeric"},
    "yMMMd": {year: "numeric", month: "short", day: "numeric"},
    "yMMMMd": {year: "numeric", month: "long", day: "numeric"},
    "MMMd": {month: "short", day: "numeric"},
    "MMMMd": {month: "long", day: "numeric"},
    "Md": {month: "numeric", day: "numeric"},
    "yM": {year: "numeric", month: "numeric"},
    "yMMMM": {year: "numeric", month: "long"},
    "hms": {hour: "numeric", minute: "numeric", second: "numeric"},
    "hm": {hour: "numeric", minute: "numeric"},
    "ms": {minute: "numeric", second: "numeric"},
    "d": {day: "numeric"},
    "M": {month: "numeric"},
    "MMMM": {month: "long"},
    "MMM": {month: "short"},
    "y": {year: "numeric"},
    "E": {weekday: "long"},
    // The same times on each clock, since which one is asked for changes more
    // than the hour: en writes 9:30 AM on one and 09:30 on the other.
    "Hm": {hour: "numeric", minute: "numeric", hour12: false},
    "Hms": {hour: "numeric", minute: "numeric", second: "numeric", hour12: false},
    "H": {hour: "numeric", hour12: false},
    "hm12": {hour: "numeric", minute: "numeric", hour12: true},
    "hms12": {hour: "numeric", minute: "numeric", second: "numeric", hour12: true},
    "h12": {hour: "numeric", hour12: true},
    "h": {hour: "numeric"},
  })) {
    // Node 26/ICU 78 can abort inside formatToParts() when resolvedOptions()
    // reports an incomplete numeric year-month skeleton. format() remains valid.
    const fallback = name === "yM" ? numericYearMonthPattern(locale) : null;
    skeletons[name] = fallback || styled(options);
  }

  // Time-zone fields affect more than the suffix in some languages. Thai,
  // for example, writes a short named zone with a worded clock rather than
  // the punctuation used by its plain hms pattern. Temporal also has its own
  // hour-width selection, so keep these patterns separate from Date patterns.
  const temporalSkeletons = {};
  for (const [name, options] of Object.entries({
    yMd: {year: "numeric", month: "numeric", day: "numeric"},
    yMMMd: {year: "numeric", month: "short", day: "numeric"},
    yMMMMd: {year: "numeric", month: "long", day: "numeric"},
    MMMd: {month: "short", day: "numeric"},
    MMMMd: {month: "long", day: "numeric"},
    Md: {month: "numeric", day: "numeric"},
    yM: {year: "numeric", month: "numeric"},
    yMMMM: {year: "numeric", month: "long"},
    d: {day: "numeric"},
    M: {month: "numeric"},
    MMM: {month: "short"},
    MMMM: {month: "long"},
    y: {year: "numeric"},
    E: {weekday: "long"},
    hms: {hour: "numeric", minute: "numeric", second: "numeric"},
    hm: {hour: "numeric", minute: "numeric"},
    ms: {minute: "numeric", second: "numeric"},
    h: {hour: "numeric"},
    Hm: {hour: "numeric", minute: "numeric", hour12: false},
    Hms: {hour: "numeric", minute: "numeric", second: "numeric", hour12: false},
    H: {hour: "numeric", hour12: false},
    hm12: {hour: "numeric", minute: "numeric", hour12: true},
    hms12: {hour: "numeric", minute: "numeric", second: "numeric", hour12: true},
    h12: {hour: "numeric", hour12: true},
  })) {
    temporalSkeletons[name] = styledTemporal(options);
  }
  // Temporal supplies date-and-time defaults as one pattern. Some locales
  // place a day period differently here than when the time fields were
  // explicitly requested, so this cannot always be rebuilt from two halves.
  temporalSkeletons.yMdhms = styledTemporal({});
  const plainPattern = (value) => {
    const formatter = new Intl.DateTimeFormat(locale);
    return pattern(formatter.formatToParts(value), hour12,
      formatter.resolvedOptions());
  };
  temporalSkeletons["plain-time/hms"] = plainPattern(
    new Temporal.PlainTime(9, 4, 5));
  temporalSkeletons["plain-date/yMd"] = plainPattern(
    new Temporal.PlainDate(2024, 1, 5));
  temporalSkeletons["plain-date-time/yMdhms"] = plainPattern(
    new Temporal.PlainDateTime(2024, 1, 5, 9, 4, 5));
  for (const [name, fields] of Object.entries({
    h: {hour: "numeric"},
    hm: {hour: "numeric", minute: "numeric"},
    hms: {hour: "numeric", minute: "numeric", second: "numeric"},
  })) {
    for (const [style, letters] of Object.entries({
      short: "z",
      long: "zzzz",
      shortOffset: "O",
      longOffset: "OOOO",
      shortGeneric: "v",
      longGeneric: "vvvv",
    })) {
      temporalSkeletons[name + letters] =
        styledTemporal({...fields, timeZoneName: style});
      if (name === "hms") {
        temporalSkeletons["yMdhms" + letters] =
          styledTemporal({timeZoneName: style});
      }
    }
  }

  const dayPeriod = (when) =>
    new Intl.DateTimeFormat(locale, {hour: "numeric", hour12: true, timeZone: "UTC"})
      .formatToParts(when).filter(p => p.type === "dayPeriod").map(p => p.value)[0];
  const plain = (text) => (text || "").replace(/\u202f/g, " ");
  const dayPeriods = [plain(dayPeriod(SAMPLE)) || "AM",
                      plain(dayPeriod(SAMPLE_PM)) || "PM"];

  // What this locale calls each hour of the day, where it calls them anything
  // beyond morning and afternoon: Chinese has six names for the parts of a
  // day, and writes the one the hour falls in.
  const byHour = Array.from({length: 24}, (_, hour) => {
    const parts = new Intl.DateTimeFormat(locale, {timeStyle: "short", timeZone: "UTC"})
      .formatToParts(new Date(Date.UTC(2024, 0, 5, hour, 4)));
    const found = parts.find(p => p.type === "dayPeriod");
    return found ? plain(found.value) : "";
  });
  const distinct = new Set(byHour.filter(name => name !== ""));
  // Asked for outright, rather than read off a time: the languages that name
  // the parts of the day name them whether or not their clock shows one.
  const periodsAt = (width) => {
    const f = new Intl.DateTimeFormat(locale, {dayPeriod: width, timeZone: "UTC"});
    return Array.from({length: 24}, (_, hour) =>
      plain(f.format(new Date(Date.UTC(2024, 0, 5, hour, 4)))));
  };
  let hourPeriods = periodsAt("long");
  if (new Set(hourPeriods).size <= 2) {
    // A language that says no more than morning and afternoon is left to the
    // two names it already has.
    hourPeriods = distinct.size > 2 ? byHour : [];
  }
  let hourPeriodsNarrow = periodsAt("narrow");
  if (hourPeriodsNarrow.join("\u0000") === hourPeriods.join("\u0000")) {
    hourPeriodsNarrow = [];
  }
  const era = (date, width) => new Intl.DateTimeFormat(locale, {
    era: width, year: "numeric", timeZone: "UTC",
  })
    .formatToParts(date).filter(p => p.type === "era").map(p => p.value)[0] || "";
  const compact = compactForms(locale);

  return {
    tag: locale,
    numbering: resolved.numberingSystem,
    calendar: resolved.calendar,
    hour12,
    names,
    dayPeriods, hourPeriods, hourPeriodsNarrow,
    eras: [era(new Date(Date.UTC(-500, 0, 1)), "short"), era(SAMPLE, "short")],
    erasLong: [era(new Date(Date.UTC(-500, 0, 1)), "long"), era(SAMPLE, "long")],
    erasNarrow: [era(new Date(Date.UTC(-500, 0, 1)), "narrow"), era(SAMPLE, "narrow")],
    dates, times, both, glue, temporalGlue, skeletons, temporalSkeletons,
    // What stands between two dates of a range, which is not always what
    // stands between two numbers.
    // Whether a range of dates written in numbers is written out twice or
    // once with what the two have in common said once: English says
    // "1/1/2024 - 1/5/2024" and German says "01.-05.01.2024".
    dateRangeRepeat: (() => {
      const f = new Intl.DateTimeFormat(locale,
        {year: "numeric", month: "numeric", day: "numeric", timeZone: "UTC"});
      const parts = f.formatRangeToParts(
        new Date(Date.UTC(2024, 0, 1)), new Date(Date.UTC(2024, 0, 5)));
      return !parts.some(p => p.source === "shared" && p.type !== "literal");
    })(),
    dateRange: (() => {
      const parts = new Intl.DateTimeFormat(locale, {dateStyle: "medium", timeZone: "UTC"})
        .formatRangeToParts(new Date(Date.UTC(2024, 0, 1)), new Date(Date.UTC(2024, 0, 5)));
      const at = parts.findIndex(p => p.source === "endRange");
      for (let i = at - 1; i >= 0; i--) {
        if (parts[i].type === "literal") return plain(parts[i].value);
      }
      return "\u2009\u2013\u2009";
    })(),
    numbers: numberData(locale),
    compact,
    currencies: currencySymbols(locale),
    plurals: {cardinal: pluralTable(locale, "cardinal", compact),
              ordinal: pluralTable(locale, "ordinal", compact)},
    lists: listData(locale),
    relative: relativeData(locale),
  };
}

// A locale named on the command line is extracted instead of the whole list,
// which is how the generator works through them in batches and how one that
// misbehaves is found.
const wanted = process.argv.slice(2);
if (wanted[0] === "--list") {
  process.stdout.write(LOCALES.join("\n") + "\n");
  process.exit(0);
}
const out = {
  icu: process.versions.icu,
  locales: (wanted.length > 0 ? wanted : LOCALES).map(extract),
};
process.stdout.write(JSON.stringify(out));
