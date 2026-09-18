// calendars.mjs reads what the other calendars call their months and eras.
//
// Usage:
//
//	node internal/icu/internal/cldrgen/calendars.mjs > .../calendars.json
//
// Not every year starts in January. The Islamic year is eleven days shorter
// than ours and its months are named in Arabic; the Hebrew year has a
// thirteenth month in seven years out of nineteen; the Japanese year is
// counted from the start of an emperor's reign and is named after it. Which
// day falls in which month is arithmetic, and is written in Go; what those
// months and years are called is data, and is read here.
//
// The names are read by asking for the month twice -- once as a number and
// once as a name -- so that the two line up. A calendar's own numbering is
// what the Go side has to produce, and this is where it is learned from.

import fs from "fs";
import path from "path";
import {fileURLToPath} from "url";

const here = path.dirname(fileURLToPath(import.meta.url));
const {locales} = JSON.parse(fs.readFileSync(path.join(here, "locales.json"), "utf8"));

const CALENDARS = ["buddhist", "coptic", "ethiopic", "ethioaa", "hebrew",
  "indian", "islamic", "islamic-civil", "islamic-rgsa", "islamic-tbla",
  "islamic-umalqura", "japanese", "persian", "roc", "chinese", "dangi"];

// Enough days to meet every month of every calendar, including the ones that
// have a thirteenth in some years: a day every week for twenty years.
const DAYS = [];
for (let n = 0; n < 20 * 52; n++) {
  DAYS.push(Date.UTC(2010, 0, 5) + n * 7 * 86400000);
}
// And far enough back to meet the eras.
const ERAS = [Date.UTC(-2000, 0, 1), Date.UTC(-500, 0, 1), Date.UTC(1000, 0, 1),
  Date.UTC(1700, 0, 1), Date.UTC(1900, 0, 1), Date.UTC(1950, 0, 1),
  Date.UTC(2000, 0, 1), Date.UTC(2023, 0, 1)];

// The Japanese year is counted from the start of a reign, and there have been
// two hundred and thirty-seven of them: a day from each, so that every one is
// named. Which days those are is found once, from the calendar itself.
const REIGNS = (() => {
  const f = new Intl.DateTimeFormat("en-u-ca-japanese",
    {era: "short", timeZone: "UTC"});
  const eraOf = (ms) => f.formatToParts(ms)
    .filter(p => p.type === "era").map(p => p.value).join("");
  const out = [];
  let last = null;
  for (let ms = Date.UTC(600, 0, 1); ms < Date.UTC(2035, 0, 1); ms += 86400000) {
    const era = eraOf(ms);
    if (era !== last) {
      out.push(ms);
      last = era;
    }
  }
  return out;
})();

// Which month of which year each sample day falls in, asked of a calendar
// rather than of a language: the number a calendar gives a month is the same
// whoever is reading, and some languages write even a numeric month as a name.
const keysFor = (calendar) => {
  const tag = "en-u-ca-" + calendar;
  const numbers = new Intl.DateTimeFormat(tag,
    {month: "numeric", timeZone: "UTC", numberingSystem: "latn"});
  const years = new Intl.DateTimeFormat(tag,
    {year: "numeric", era: "short", timeZone: "UTC", numberingSystem: "latn"});

  // Which years have a thirteenth month, since a calendar that has one names
  // its months differently in those years: the Hebrew sixth month is Adar in
  // an ordinary year and Adar I in a long one.
  const byYear = new Map();
  for (const day of DAYS) {
    const year = years.format(day);
    if (!byYear.has(year)) byYear.set(year, new Set());
    byYear.get(year).add(numbers.format(day));
  }
  const long = new Set();
  for (const [year, months] of byYear) {
    if (months.size > 12) long.add(year);
  }
  // Only a calendar that names the months of a long year differently needs
  // them written down twice: the lunisolar ones mark the month said twice in
  // the number itself.
  const marksLongYears = calendar === "hebrew";
  return DAYS.map(day =>
    numbers.format(day) +
    (marksLongYears && long.has(years.format(day)) ? "L" : ""));
};

const KEYS = {};
for (const calendar of CALENDARS) {
  try { KEYS[calendar] = keysFor(calendar); } catch (e) {}
}

function namesFor(locale, calendar) {
  const tag = locale + "-u-ca-" + calendar;
  const keys = KEYS[calendar];
  if (!keys) throw new Error("no such calendar");

  const out = {};
  for (const width of ["long", "short", "narrow"]) {
    const names = new Intl.DateTimeFormat(tag, {month: width, timeZone: "UTC"});
    const table = new Map();
    DAYS.forEach((day, i) => {
      if (!table.has(keys[i])) table.set(keys[i], names.format(day));
    });
    out["m" + width] = [...table].map(([n, name]) => n + "=" + name).join("|");
  }
  // The names a lunisolar year goes by, which run in a cycle of sixty rather
  // than counting upwards.
  if (calendar === "chinese" || calendar === "dangi") {
    const f = new Intl.DateTimeFormat(tag, {year: "numeric", timeZone: "UTC"});
    const names = [];
    for (let year = 0; year < 60; year++) {
      const parts = f.formatToParts(Date.UTC(1984 + year, 5, 1));
      const name = parts.filter(p => p.type === "yearName").map(p => p.value).join("");
      names.push(name);
    }
    if (names.some(name => name !== "")) out.cycle = names.join("|");
  }

  const eraDays = calendar === "japanese" ? REIGNS : ERAS;
  for (const width of ["long", "short", "narrow"]) {
    const f = new Intl.DateTimeFormat(tag, {era: width, year: "numeric", timeZone: "UTC"});
    const seen = new Map();
    for (const day of eraDays) {
      const parts = f.formatToParts(day);
      const era = parts.filter(p => p.type === "era").map(p => p.value).join("");
      const year = parts.filter(p => p.type === "year").map(p => p.value).join("");
      if (era !== "" && !seen.has(era)) seen.set(era, year);
    }
    out["e" + width] = [...seen.keys()].join("|");
  }
  return out;
}

// A calendar at a time rather than a locale at a time: the languages that name
// the Islamic months alike are not the same set as the ones that name the
// Japanese reigns alike, and a locale that differs in one should not have to
// carry its own copy of the other.
const entries = new Map();
const index = {};
for (const locale of locales) {
  const at = {};
  for (const calendar of CALENDARS) {
    let names;
    try { names = namesFor(locale, calendar); } catch (e) { continue; }
    const key = JSON.stringify(names);
    if (!entries.has(key)) entries.set(key, entries.size);
    at[calendar] = entries.get(key);
  }
  index[locale] = at;
}

process.stdout.write(JSON.stringify({
  icu: process.versions.icu,
  entries: [...entries.keys()].map(k => JSON.parse(k)),
  index,
}) + "\n");
