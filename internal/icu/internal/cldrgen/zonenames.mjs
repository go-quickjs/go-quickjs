// zonenames.mjs writes what the time zones are called, in every language.
//
// Usage:
//
//	node internal/icu/internal/cldrgen/zonenames.mjs > .../zonenames.json
//
// A zone has up to six names in a language: a long one and a short one for
// standard time, for summer time, and for neither -- Eastern Standard Time,
// Eastern Daylight Time, Eastern Time; EST, EDT, ET. Most zones have none of
// them and are called an offset from Greenwich instead, and the way that
// offset is written is itself part of the language: GMT-05:00 in English,
// UTC−05:00 in French, ‎−۰۵:۰۰ گرینویچ in Persian.
//
// Which of the two seasonal names applies to an instant is something the zone
// files answer rather than the language, so the two probes below are recorded
// as winter and summer and sorted out by the packer, which has Go's zone
// files to hand.
//
// The zones that are called the same thing everywhere share an entry: Paris,
// Berlin and Madrid are all on Central European Time, in every language. That
// is what keeps this to a megabyte rather than eight.

import fs from "fs";

const root = new URL(".", import.meta.url).pathname;
const {locales} = JSON.parse(fs.readFileSync(root + "locales.json", "utf8"));
// Greenwich is not among the zones a place is named after, but every one of
// its other names points at it, so it is named here with the rest.
const zones = [...Intl.supportedValuesOf("timeZone"), "UTC"];

const WINTER = Date.UTC(2024, 0, 15, 12);
const SUMMER = Date.UTC(2024, 6, 15, 12);
const WHEN = [WINTER, SUMMER];

const formatters = new Map();
const nameAt = (locale, zone, style, when) => {
  const key = locale + "|" + zone + "|" + style;
  let f = formatters.get(key);
  if (f === undefined) {
    f = new Intl.DateTimeFormat(locale, {timeZone: zone, timeZoneName: style});
    formatters.set(key, f);
  }
  const found = f.formatToParts(when).find(p => p.type === "timeZoneName");
  return found ? found.value : "";
};

// What each zone's offset is at each of the two instants, which is the same
// number whatever language is asking.
const offsets = {};
for (const zone of zones) {
  offsets[zone] = WHEN.map(when => {
    const found = /([+-−])(\d{1,2})(?::(\d{2}))?$/.exec(
      nameAt("en", zone, "shortOffset", when));
    if (!found) return 0;
    const sign = found[1] === "+" ? 1 : -1;
    return sign * (Number(found[2]) * 60 + Number(found[3] || 0));
  });
}

// An offset written out is the same for every zone that is on it, so it is
// worked out once per language and offset and then remembered.
const offsetText = new Map();
const offsetAt = (locale, style, offset, zone, when) => {
  const key = locale + "|" + style + "|" + offset;
  let text = offsetText.get(key);
  if (text === undefined) {
    text = nameAt(locale, zone, style, when);
    offsetText.set(key, text);
  }
  return text;
};

// namesIn reads every zone's six names in one language. A name that is only
// the offset written out says nothing the clock cannot work out, and is left
// empty so that the engine writes the offset itself -- which it must do
// anyway for the offsets a zone had in the past.
const namesIn = (locale) => {
  const out = {};
  for (const zone of zones) {
    const slots = ["", "", "", "", "", ""];
    for (let i = 0; i < WHEN.length; i++) {
      const when = WHEN[i], offset = offsets[zone][i];
      const long = nameAt(locale, zone, "long", when);
      const short = nameAt(locale, zone, "short", when);
      if (long !== offsetAt(locale, "longOffset", offset, zone, when)) {
        slots[i] = long;
      }
      if (short !== offsetAt(locale, "shortOffset", offset, zone, when)) {
        slots[3 + i] = short;
      }
    }
    // The name that does not depend on the time of year, which a zone may
    // have even where it has no seasonal name at all: Morocco Time.
    const longGeneric = nameAt(locale, zone, "longGeneric", WINTER);
    const shortGeneric = nameAt(locale, zone, "shortGeneric", WINTER);
    if (longGeneric !== offsetAt(locale, "longOffset", offsets[zone][0], zone, WINTER)) {
      slots[2] = longGeneric;
    }
    if (shortGeneric !== offsetAt(locale, "shortOffset", offsets[zone][0], zone, WINTER)) {
      slots[5] = shortGeneric;
    }
    out[zone] = slots.join("|");
  }
  return out;
};

// Which numbering system a digit belongs to, so that the digits an offset is
// written with say which system wrote them.
const systemOf = new Map();
for (const system of Intl.supportedValuesOf("numberingSystem")) {
  const shown = new Intl.NumberFormat("en-u-nu-" + system, {useGrouping: false})
    .format(1234567890);
  const digits = [...shown];
  // A system that writes a number as something other than ten digits --
  // roman numerals, or the counting rods -- cannot say which digit is which.
  if (digits.length !== 10) continue;
  for (const digit of digits) {
    if (!systemOf.has(digit)) systemOf.set(digit, system);
  }
}

// How a language writes an offset, as the text with the numbers taken out:
// "GMT+{0}:{1}". The long form pads the hour to two digits and the short form
// does not, which is the one thing about these that every language agrees on.
//
// A few languages write the two numbers with nothing between them -- Amharic
// says GMT-0500 -- so a single run of digits is cut two from the end, which
// is where the minutes start.
const template = (text, minutes) => {
  const runs = [...text.matchAll(/\p{Nd}+/gu)];
  if (runs.length === 0) return text;
  if (!minutes) {
    return text.replace(/\p{Nd}+/gu, "{0}");
  }
  if (runs.length >= 2) {
    let at = 0;
    return text.replace(/\p{Nd}+/gu, () => "{" + at++ + "}");
  }
  const run = runs[0][0];
  const digits = [...run];
  if (digits.length < 3) return text.replace(/\p{Nd}+/gu, "{0}");
  const hour = digits.slice(0, digits.length - 2).join("");
  return text.replace(run, "{0}" + run.slice(hour.length).replace(/\p{Nd}+/gu, "{1}"));
};

const gmtFormsIn = (locale) => {
  const probes = [
    ["longOffset", "Asia/Kolkata"],    // +05:30
    ["longOffset", "America/St_Johns"], // -03:30
    ["shortOffset", "Asia/Kolkata"],   // +5:30
    ["shortOffset", "America/St_Johns"],
    ["shortOffset", "Etc/GMT-5"],      // +5
    ["shortOffset", "Etc/GMT+5"],      // -5
  ];
  const shown = probes.map(([style, zone]) => nameAt(locale, zone, style, WINTER));
  const digits = /\p{Nd}/u.exec(shown[0]);
  const system = digits ? systemOf.get(digits[0]) || "latn" : "latn";
  // The last two probes are a whole number of hours, which is written
  // without any minutes at all.
  return [system, ...shown.map((text, at) => template(text, at < 4))].join(";");
};

// Read every language, then keep only what is different: the zones that are
// named alike everywhere share a group, and the languages that name every
// group alike share a block.
const perLocale = {};
const started = Date.now();
for (const locale of locales) {
  perLocale[locale] = namesIn(locale);
  // Nothing here needs a formatter from the language just read again.
  formatters.clear();
}
process.stderr.write("read " + locales.length + " languages in " +
  ((Date.now() - started) / 1000).toFixed(0) + "s\n");

const groupOf = {};
const groups = new Map();
for (const zone of zones) {
  const key = locales.map(locale => perLocale[locale][zone]).join("");
  if (!groups.has(key)) groups.set(key, groups.size);
  groupOf[zone] = groups.get(key);
}

// One zone stands for each group, since the others are named the same.
const standIn = [];
for (const zone of zones) {
  if (standIn[groupOf[zone]] === undefined) standIn[groupOf[zone]] = zone;
}

const blocks = new Map();
const blockOf = {};
for (const locale of locales) {
  const row = standIn.map(zone => perLocale[locale][zone]).join("");
  if (!blocks.has(row)) blocks.set(row, blocks.size);
  blockOf[locale] = blocks.get(row);
}

const gmtForms = new Map();
const gmtOf = {};
for (const locale of locales) {
  const form = gmtFormsIn(locale);
  if (!gmtForms.has(form)) gmtForms.set(form, gmtForms.size);
  gmtOf[locale] = gmtForms.get(form);
}

process.stderr.write("zones " + zones.length + " in " + groups.size +
  " groups; " + locales.length + " languages in " + blocks.size + " blocks, " +
  gmtForms.size + " ways of writing an offset\n");

process.stdout.write(JSON.stringify({
  icu: process.versions.icu,
  // The zone standing for each group, so that the packer can ask Go's zone
  // files which of the two probes was summer time.
  standIn,
  groups: groupOf,
  english: [...blocks.keys()][blockOf["en"]].split(""),
  blocks: blockOf,
  names: [...blocks.keys()].map(row => row.split("")),
  gmt: gmtOf,
  gmtForms: [...gmtForms.keys()],
}) + "\n");
