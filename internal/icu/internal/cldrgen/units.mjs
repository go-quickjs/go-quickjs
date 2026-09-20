// units.mjs reads how a language writes a measurement.
//
// Usage:
//
//	node internal/icu/internal/cldrgen/units.mjs > .../units.json
//
// "987 km/h" in English is "987 km/h" in German and "987公里/小時" in Chinese:
// the name of the unit, whether a space goes before it, which form it takes
// with one and with many, and how a unit written underneath another is joined
// to it. All of that is per language, and none of it can be worked out from
// the name of the unit.
//
// Forty-five units are carried, which are the ones ECMA-402 sanctions, in the
// three widths it offers. Locales that write them all the same way -- and many
// do, since a regional variant rarely renames a kilometre -- share one entry.

import fs from "fs";
import path from "path";
import {fileURLToPath} from "url";

const here = path.dirname(fileURLToPath(import.meta.url));
const {locales} = JSON.parse(fs.readFileSync(path.join(here, "locales.json"), "utf8"));

const UNITS = ["acre", "bit", "byte", "celsius", "centimeter", "day", "degree",
  "fahrenheit", "fluid-ounce", "foot", "gallon", "gigabit", "gigabyte", "gram",
  "hectare", "hour", "inch", "kilobit", "kilobyte", "kilogram", "kilometer",
  "liter", "megabit", "megabyte", "meter", "microsecond", "mile",
  "mile-scandinavian", "milliliter", "millimeter", "millisecond", "minute",
  "month", "nanosecond", "ounce", "percent", "petabyte", "pound", "second",
  "stone", "terabit", "terabyte", "week", "yard", "year"];

// What stands between one form and the next, which cannot be a character a
// unit is written with.
const SEP = "\u0001";
const LIST_SEP = "\u0002";

// Find one count for every plural form rather than relying on a short list of
// common cases. Cebuano, for example, uses "one" for 0-3 but "other" for 4,
// while several Romance languages only expose "many" at a million.
function pluralSamples(plural) {
  const wanted = new Set(plural.resolvedOptions().pluralCategories);
  const found = new Map();
  const consider = n => {
    const category = plural.select(n);
    if (!found.has(category)) found.set(category, n);
  };

  for (let n = 0; n < 200 && found.size < wanted.size; n++) consider(n);
  for (let whole = 0; whole < 20 && found.size < wanted.size; whole++) {
    for (const fraction of [0.1, 0.2, 0.5, 0.01, 0.02, 0.05]) {
      consider(whole + fraction);
    }
  }
  for (let power = 2; power <= 9 && found.size < wanted.size; power++) {
    const base = 10 ** power;
    for (const delta of [-2, -1, 0, 1, 2]) consider(base + delta);
  }

  const missing = [...wanted].filter(category => !found.has(category));
  if (missing.length !== 0) {
    throw new Error("no unit sample for plural categories: " + missing.join(", "));
  }
  return [...found].map(([category, n]) => ({category, n}));
}

function unitsFor(locale) {
  const out = {};
  const plural = new Intl.PluralRules(locale);
  const samples = pluralSamples(plural);
  const plain = new Intl.NumberFormat(locale);
  for (const width of ["short", "narrow", "long"]) {
    for (const unit of UNITS) {
      const f = new Intl.NumberFormat(locale, {style: "unit", unit, unitDisplay: width});
      const forms = [];
      for (const {category: key, n} of samples) {
        const text = f.format(n);
        const shown = plain.format(n);
        const at = text.indexOf(shown);
        forms.push(key + "=" +
          (at < 0 ? text : text.slice(0, at) + "{0}" + text.slice(at + shown.length)));
      }
      out[width + "/" + unit] = forms.join(SEP);
    }
    // The pairs a language writes its own way rather than by joining the two:
    // Japanese says "km/h" where joining would give "km/時間", and says it in
    // front of the number in the long form.
    const numerators = {};
    for (const u of UNITS) {
      numerators[u] = new Intl.NumberFormat(locale,
        {style: "unit", unit: u, unitDisplay: width}).format(1);
    }

    // What a unit looks like written underneath another: "km/h" is the
    // kilometre with the hour's form after it.
    const numerator = new Intl.NumberFormat(locale,
      {style: "unit", unit: "meter", unitDisplay: width}).format(1);
    for (const unit of UNITS) {
      const both = new Intl.NumberFormat(locale,
        {style: "unit", unit: "meter-per-" + unit, unitDisplay: width}).format(1);
      // Where the numerator stood, since a language may put the denominator
      // in front of it rather than after: Chinese writes "per hour, 1 metre".
      const at = both.indexOf(numerator);
      out[width + "!" + unit] = at < 0 ? both
        : both.slice(0, at) + "{0}" + both.slice(at + numerator.length);
    }
    for (const above of UNITS) {
      for (const below of UNITS) {
        if (above === below) continue;
        const actual = new Intl.NumberFormat(locale,
          {style: "unit", unit: above + "-per-" + below, unitDisplay: width}).format(1);
        const joined = out[width + "!" + below].replace("{0}", numerators[above]);
        if (actual === joined) continue;
        const at = actual.indexOf(numerators[above]);
        if (at < 0) continue;
        out[width + "?" + above + "-per-" + below] =
          actual.slice(0, at) + "{0}" + actual.slice(at + numerators[above].length);
      }
    }
  }

  for (const style of ["long", "short", "narrow"]) {
    const formatter = new Intl.DurationFormat(locale, {style});
    const separators = value => formatter.formatToParts(value)
      .filter(part => part.type === "literal" && part.unit === undefined)
      .map(part => part.value);
    const pair = separators({years: 3, months: 4});
    const many = separators({years: 3, months: 4, weeks: 5, days: 6});
    if (pair.length === 1 && many.length === 3) {
      out["duration-list/" + style] = [pair[0], ...many].join(LIST_SEP);
    }
  }
  return out;
}

// The locales that say everything the same way share one entry.
const blocks = new Map();
const same = {};
for (const locale of locales) {
  let block;
  try { block = unitsFor(locale); } catch (e) { continue; }
  const key = JSON.stringify(block);
  if (!blocks.has(key)) blocks.set(key, {at: locale, block});
  same[locale] = blocks.get(key).at;
}
const units = {};
for (const {at, block} of blocks.values()) units[at] = block;

// DurationFormat is newer than NumberFormat's unit support, and ICU does not
// necessarily carry it for every locale supported elsewhere. Keep that
// distinction so an unsupported request falls through to the next locale.
const durationUnsupported = locales.filter(locale =>
  Intl.DurationFormat.supportedLocalesOf(locale).length === 0);

process.stdout.write(JSON.stringify({
  icu: process.versions.icu, units, same, durationUnsupported,
}) + "\n");
