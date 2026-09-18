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

// Enough counts to meet every plural form a language has.
const SAMPLES = [0, 1, 2, 3, 5, 11, 21, 100];

function unitsFor(locale) {
  const out = {};
  const plural = new Intl.PluralRules(locale);
  const plain = new Intl.NumberFormat(locale);
  for (const width of ["short", "narrow", "long"]) {
    for (const unit of UNITS) {
      const f = new Intl.NumberFormat(locale, {style: "unit", unit, unitDisplay: width});
      const forms = [];
      const seen = new Set();
      for (const n of SAMPLES) {
        const key = plural.select(n);
        if (seen.has(key)) continue;
        seen.add(key);
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

process.stdout.write(JSON.stringify({icu: process.versions.icu, units, same}) + "\n");
