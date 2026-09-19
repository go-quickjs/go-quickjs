// calendarformats.mjs extracts calendar-specific date patterns from a CLDR
// JSON release. Unlike month and era names, patterns are directly available
// in CLDR and do not need to be recovered by formatting sample dates.
//
// Usage:
//
//   node calendarformats.mjs /path/to/cldr-json > calendarformats.json

// The path contains the cldr-cal-*-full directories from the matching CLDR
// release. ICU 78 uses CLDR 48.

import fs from "fs";
import path from "path";
import {spawnSync} from "child_process";
import {fileURLToPath} from "url";

const here = path.dirname(fileURLToPath(import.meta.url));
const localeList = JSON.parse(fs.readFileSync(path.join(here, "locales.json"), "utf8"));
const {locales, aliases} = localeList;
const CALENDARS = {
  buddhist: ["buddhist", "buddhist"],
  chinese: ["chinese", "chinese"],
  coptic: ["coptic", "coptic"],
  dangi: ["dangi", "dangi"],
  ethioaa: ["ethiopic", "ethiopic-amete-alem"],
  ethiopic: ["ethiopic", "ethiopic"],
  hebrew: ["hebrew", "hebrew"],
  indian: ["indian", "indian"],
  islamic: ["islamic", "islamic"],
  "islamic-civil": ["islamic", "islamic-civil"],
  "islamic-rgsa": ["islamic", "islamic-rgsa"],
  "islamic-tbla": ["islamic", "islamic-tbla"],
  "islamic-umalqura": ["islamic", "islamic-umalqura"],
  japanese: ["japanese", "japanese"],
  persian: ["persian", "persian"],
  roc: ["roc", "roc"],
};
const SKELETONS = ["yMd", "yMMMd", "yMMMMd", "yMMMMMd", "MMMd", "MMMMd",
  "MMMMMd", "Md", "yM", "yMMMM", "yMMMMM", "d", "M", "MMM", "MMMM",
  "MMMMM", "y", "E"];
const WIDTHS = ["full", "long", "medium", "short"];
const SAMPLE = Date.UTC(2024, 0, 5, 9, 4, 5);

const skeletonOptions = (skeleton) => {
  const options = {};
  if (skeleton.includes("y")) options.year = "numeric";
  const months = skeleton.match(/M+/);
  if (months) {
    options.month = ["", "numeric", "2-digit", "short", "long", "narrow"][months[0].length];
  }
  if (skeleton.includes("d")) options.day = "numeric";
  if (skeleton.includes("E")) options.weekday = "long";
  return options;
};

const quoteLiteral = value => "'" + value.replaceAll("'", "''") + "'";

const eraWidths = new Map();
function eraPattern(locale, calendar, value) {
  const key = locale + "\n" + calendar;
  let widths = eraWidths.get(key);
  if (!widths) {
    widths = {};
    for (const width of ["long", "short", "narrow"]) {
      const parts = new Intl.DateTimeFormat(locale, {
        calendar, era: width, year: "numeric", numberingSystem: "latn", timeZone: "UTC",
      }).formatToParts(SAMPLE);
      widths[width] = parts.find(part => part.type === "era")?.value || "";
    }
    eraWidths.set(key, widths);
  }
  if (value === widths.narrow && value !== widths.short) return "GGGGG";
  if (value === widths.long && value !== widths.short) return "GGGG";
  return "G";
}

const monthForms = new Map();
function monthPattern(locale, calendar, value, resolved, options, standalone) {
  const digits = /^\p{Decimal_Number}+$/u.test(value);
  if (digits) {
    const padded = resolved.month === "2-digit" ||
      (options.month === undefined && options.dateStyle && [...value].length === 2);
    return "M".repeat(padded ? 2 : 1);
  }

  const cacheKey = locale + "\n" + calendar;
  let forms = monthForms.get(cacheKey);
  if (!forms) {
    forms = {};
    for (const width of ["long", "short", "narrow"]) {
      forms[width] = {};
      for (const [context, fields] of Object.entries({
        L: {}, M: {day: "numeric"}, N: {year: "numeric", day: "numeric"},
      })) {
        const parts = new Intl.DateTimeFormat(locale, {
          calendar, month: width, ...fields, numberingSystem: "latn", timeZone: "UTC",
        }).formatToParts(SAMPLE);
        forms[width][context] = parts.find(part => part.type === "month")?.value || "";
      }
    }
    monthForms.set(cacheKey, forms);
  }

  const preferred = resolved.month ||
    ({full: "long", long: "long", medium: "short", short: "narrow"}[options.dateStyle]);
  const widths = [...new Set([preferred, "long", "short", "narrow"].filter(Boolean))];
  const contexts = standalone ? ["L", "M", "N"] :
    (options.year !== undefined || options.dateStyle !== undefined ? ["N", "M", "L"] : ["M", "L", "N"]);
  const repetitions = {long: 4, short: 3, narrow: 5};
  for (const width of widths) {
    for (const context of contexts) {
      if (forms[width][context] === value) return context.repeat(repetitions[width]);
    }
  }
  for (const width of widths) {
    for (const context of contexts) {
      if (forms[width][context].toLocaleLowerCase(locale) === value) {
        return "P".repeat(repetitions[width]);
      }
    }
  }

  // Hawaiian short date styles use lower-case Roman month numbers, a form
  // not exposed by any individual month-width request.
  if (/^[ivxlcdm]+$/.test(value)) return "j";
  if (/^[IVXLCDM]+$/.test(value)) return "J";

  const letter = standalone ? "L" : "M";
  if (resolved.month === "narrow") return letter.repeat(5);
  if (resolved.month === "short" || options.dateStyle === "medium") return letter.repeat(3);
  return letter.repeat(4);
}

const weekdayForms = new Map();
function weekdayPattern(locale, calendar, value, standalone) {
  const key = locale + "\n" + calendar;
  let forms = weekdayForms.get(key);
  if (!forms) {
    forms = {};
    for (const width of ["long", "short", "narrow"]) {
      const alone = new Intl.DateTimeFormat(locale, {
        calendar, weekday: width, timeZone: "UTC",
      }).formatToParts(SAMPLE);
      const format = new Intl.DateTimeFormat(locale, {
        calendar, weekday: width, year: "numeric", month: "long", day: "numeric", timeZone: "UTC",
      }).formatToParts(SAMPLE);
      forms[width] = {
        c: alone.find(part => part.type === "weekday")?.value || "",
        E: format.find(part => part.type === "weekday")?.value || "",
      };
    }
    weekdayForms.set(key, forms);
  }
  const contexts = standalone ? ["c", "E"] : ["E", "c"];
  for (const [width, count] of [["long", 4], ["short", 3], ["narrow", 5]]) {
    for (const context of contexts) {
      if (forms[width][context] === value) return context.repeat(count);
    }
  }
  return standalone ? "cccc" : "EEEE";
}

// Read a pattern back from Node in a subprocess. ICU occasionally aborts the
// process for a malformed locale/calendar combination, so the parent runs one
// locale at a time and retries any unfinished calendars separately.
function nodePattern(locale, calendar, options) {
  const format = new Intl.DateTimeFormat(locale, {
    ...options, calendar, numberingSystem: "latn", timeZone: "UTC",
  });
  const resolved = format.resolvedOptions();
  let out = "";
  const standaloneMonth = options.month !== undefined && options.year === undefined &&
    options.day === undefined && options.weekday === undefined && options.dateStyle === undefined;
  const standaloneWeekday = options.weekday !== undefined && options.year === undefined &&
    options.month === undefined && options.day === undefined && options.dateStyle === undefined;
  for (const part of format.formatToParts(SAMPLE)) {
    switch (part.type) {
      case "era": out += eraPattern(locale, calendar, part.value); break;
      case "year":
        if (/^[\u0590-\u05ff]+$/.test(part.value)) out += "A";
        else out += resolved.year === "2-digit" ||
          (options.year === undefined && options.dateStyle && /^\d{2}$/.test(part.value)) ? "yy" : "y";
        break;
      case "relatedYear": out += "r"; break;
      case "yearName": out += "U"; break;
      case "month":
        out += monthPattern(locale, calendar, part.value, resolved, options, standaloneMonth);
        break;
      case "day":
        if (/^[\u0590-\u05ff]+$/.test(part.value)) out += "I";
        else out += resolved.day === "2-digit" ||
          (options.day === undefined && options.dateStyle && /^\d{2}$/.test(part.value)) ? "dd" : "d";
        break;
      case "weekday":
        out += weekdayPattern(locale, calendar, part.value, standaloneWeekday);
        break;
      default: out += quoteLiteral(part.value); break;
    }
  }
  return out;
}

function nodeCalendarData(locale, calendar) {
  const dates = WIDTHS.map(dateStyle => nodePattern(locale, calendar, {dateStyle}));
  const skeletons = {};
  for (const skeleton of SKELETONS) {
    skeletons[skeleton] = nodePattern(locale, calendar, skeletonOptions(skeleton));
  }
  return {dates, skeletons};
}

const workerTasks = [
  // Leave the patterns which have triggered ICU field-iterator crashes until
  // last. A worker writes every completed pattern immediately, so one broken
  // pattern does not discard the rest of the calendar's usable data.
  ...SKELETONS.filter(skeleton => skeleton !== "yM" && skeleton !== "yMd")
    .map(skeleton => ["skeleton", skeleton]),
  ...[3, 2, 1].map(index => ["date", index]),
  ["skeleton", "yM"],
  ["skeleton", "yMd"],
  ["date", 0],
];

function writeWorkerPattern(locale, calendar, kind, key) {
  const options = kind === "date" ? {dateStyle: WIDTHS[key]} : skeletonOptions(key);
  const pattern = nodePattern(locale, calendar, options);
  process.stdout.write(JSON.stringify({calendar, kind, key, pattern}) + "\n");
}

function runWorker() {
  const locale = process.argv[3];
  const calendars = process.argv.length > 4 ? [process.argv[4]] : Object.keys(CALENDARS);
  const onlyKind = process.argv[5];
  const onlyKey = process.argv[6];
  for (const calendar of calendars) {
    if (onlyKind) {
      writeWorkerPattern(locale, calendar, onlyKind,
        onlyKind === "date" ? Number(onlyKey) : onlyKey);
      continue;
    }
    for (const [kind, key] of workerTasks) {
      writeWorkerPattern(locale, calendar, kind, key);
    }
  }
}

function workerResults(locale, calendar, kind, key) {
  const args = [fileURLToPath(import.meta.url), "--worker", locale];
  if (calendar) args.push(calendar);
  if (kind) args.push(kind, String(key));
  const child = spawnSync(process.execPath, args, {encoding: "utf8", maxBuffer: 8 << 20});
  const found = new Map();
  for (const line of (child.stdout || "").split("\n")) {
    if (!line) continue;
    try {
      const item = JSON.parse(line);
      let data = found.get(item.calendar);
      if (!data) {
        data = {dates: [], skeletons: {}};
        found.set(item.calendar, data);
      }
      if (item.kind === "date") data.dates[item.key] = item.pattern;
      else if (item.kind === "skeleton") data.skeletons[item.key] = item.pattern;
    } catch {}
  }
  return found;
}

function mergeCalendarData(into, from) {
  if (!from) return into;
  if (!into) into = {dates: [], skeletons: {}};
  for (let i = 0; i < WIDTHS.length; i++) {
    if (from.dates[i]) into.dates[i] = from.dates[i];
  }
  Object.assign(into.skeletons, from.skeletons);
  return into;
}

function refineWithNode(filename) {
  const original = JSON.parse(fs.readFileSync(filename, "utf8"));
  const expanded = {};
  for (const locale of locales) {
    expanded[locale] = {};
    for (const [calendar, which] of Object.entries(original.index[locale] || {})) {
      expanded[locale][calendar] = structuredClone(original.entries[which]);
    }
    const found = workerResults(locale);
    for (const calendar of Object.keys(CALENDARS)) {
      let data = found.get(calendar);
      if (!data || data.dates.filter(Boolean).length !== WIDTHS.length ||
          Object.keys(data.skeletons).length !== SKELETONS.length) {
        data = mergeCalendarData(data, workerResults(locale, calendar).get(calendar));
      }
      // Retry only unfinished patterns in their own process. If ICU aborts for
      // one of them, its CLDR pattern remains as the intentional fallback.
      for (let i = 0; i < WIDTHS.length; i++) {
        if (!data?.dates[i]) {
          data = mergeCalendarData(data,
            workerResults(locale, calendar, "date", i).get(calendar));
        }
      }
      for (const skeleton of SKELETONS) {
        if (!data?.skeletons[skeleton]) {
          data = mergeCalendarData(data,
            workerResults(locale, calendar, "skeleton", skeleton).get(calendar));
        }
      }
      const target = expanded[locale][calendar];
      if (!data || !target) continue;
      for (let i = 0; i < WIDTHS.length; i++) {
        if (data.dates[i]) target.dates[i] = data.dates[i];
      }
      Object.assign(target.skeletons, data.skeletons);
    }
  }

  const distinct = new Map();
  const index = {};
  for (const locale of locales) {
    index[locale] = {};
    for (const [calendar, data] of Object.entries(expanded[locale])) {
      const key = JSON.stringify(data);
      if (!distinct.has(key)) distinct.set(key, distinct.size);
      index[locale][calendar] = distinct.get(key);
    }
  }
  const output = {cldr: original.cldr, node: process.versions.icu,
    entries: [...distinct.keys()].map(key => JSON.parse(key)), index};
  fs.writeFileSync(filename, JSON.stringify(output) + "\n");
}

if (process.argv[2] === "--worker") {
  runWorker();
  process.exit(0);
}
if (process.argv[2] === "--node") {
  const filename = process.argv[3];
  if (!filename) throw new Error("the calendar format file is required");
  refineWithNode(filename);
  process.exit(0);
}

const root = process.argv[2];
if (!root) throw new Error("the CLDR JSON directory is required");

const valueOf = value => typeof value === "string" ? value : value && value._value;
const runtimeGlue = value => valueOf(value)
  .replace("{0}", "{time}").replace("{1}", "{0}").replace("{time}", "{1}");

function calendarData(locale, calendar, directory, file) {
  const main = path.join(root, `cldr-cal-${directory}-full`, "main");
  const candidates = [locale, aliases[locale]];
  try {
    const maximized = new Intl.Locale(locale).maximize();
    if (maximized.script) candidates.push(`${maximized.language}-${maximized.script}`);
    candidates.push(maximized.language);
  } catch {}
  candidates.push(locale.split("-")[0]);
  if (locale === "ars") candidates.unshift("ar");
  const used = candidates.find(candidate => candidate &&
    fs.existsSync(path.join(main, candidate, `ca-${file}.json`)));
  if (!used) return null;
  const filename = path.join(main, used, `ca-${file}.json`);
  const json = JSON.parse(fs.readFileSync(filename, "utf8"));
  const calendars = json.main[used].dates.calendars;
  const data = calendars[file];
  if (!data) return null;
  const available = data.dateTimeFormats.availableFormats;
  const skeletons = {};
  for (const requested of SKELETONS) {
    const alternatives = [requested];
    if (requested.startsWith("y")) {
      alternatives.push("yyyy" + requested.slice(1), "G" + requested);
    }
    const shorter = requested.replace(/M{4,5}/, "MMM");
    if (shorter !== requested) {
      alternatives.push(shorter);
      if (shorter.startsWith("y")) {
        alternatives.push("yyyy" + shorter.slice(1), "G" + shorter);
      }
    }
    const key = alternatives.find(name => available[name] !== undefined);
    if (key) skeletons[requested] = valueOf(available[key]);
  }
  return {
    dates: WIDTHS.map(width => valueOf(data.dateFormats[width])),
    glue: WIDTHS.map(width => runtimeGlue(data.dateTimeFormats[width])),
    skeletons,
  };
}

const entries = new Map();
const index = {};
for (const locale of locales) {
  const byCalendar = {};
  for (const [calendar, [directory, file]] of Object.entries(CALENDARS)) {
    const data = calendarData(locale, calendar, directory, file);
    if (!data) continue;
    const key = JSON.stringify(data);
    if (!entries.has(key)) entries.set(key, entries.size);
    byCalendar[calendar] = entries.get(key);
  }
  index[locale] = byCalendar;
}

process.stdout.write(JSON.stringify({
  cldr: "48",
  entries: [...entries.keys()].map(key => JSON.parse(key)),
  index,
}) + "\n");
