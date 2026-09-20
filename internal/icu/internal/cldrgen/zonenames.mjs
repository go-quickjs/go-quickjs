// zonenames.mjs writes what the time zones are called, in every language.
//
// Usage:
//
//	node internal/icu/internal/cldrgen/zonenames.mjs | gzip -9n > .../zonenames.json.gz
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
import {spawnSync} from "child_process";

const root = new URL(".", import.meta.url).pathname;
const {locales} = JSON.parse(fs.readFileSync(root + "locales.json", "utf8"));
// UTC and Greenwich are not among the zones a place is named after. Keep both:
// Date's legacy string distinguishes Coordinated Universal Time from the
// Greenwich Mean Time IDs even though Intl canonicalizes both to UTC.
const zones = [...Intl.supportedValuesOf("timeZone"), "UTC", "Greenwich"];

const legacyHelper = root + "legacyzones.mjs";
const runLegacyHelper = (mode, input, locale) => {
  const run = spawnSync(process.execPath, [legacyHelper, mode], {
    input: JSON.stringify(input),
    encoding: "utf8",
    env: {...process.env, LC_ALL: locale, LANG: locale},
    maxBuffer: 64 * 1024 * 1024,
  });
  if (run.status !== 0) {
    throw new Error("legacy Date names for " + locale + ": " +
      (run.stderr || "node exited " + run.status));
  }
  return JSON.parse(run.stdout);
};

// V8 asks its platform time-zone cache for legacy Date labels only between
// the Unix epoch and signed-32-bit time_t's last second. It maps every other
// instant to an equivalent year in that window. Extract this small exact
// timeline rather than trying to reproduce ICU's daylight classification
// from tzdb: the two disagree for wartime and permanent-offset periods.
const legacyTimeline = runLegacyHelper("timeline", {zones}, "en-US");
const legacySamples = {};
for (const zone of zones) {
  const data = legacyTimeline[zone];
  legacySamples[zone] = [0];
  if (data.names.length > 1) legacySamples[zone].push(data.changes[0] * 1000);
}

const WINTER = Date.UTC(2025, 0, 15, 12);
const SUMMER = Date.UTC(2025, 6, 15, 12);
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

// CLDR records the exact UTC intervals in which a zone belongs to a
// metazone. Keep this separate from tzdb's offset transitions: both kinds of
// boundary can change what Intl calls an instant. A local copy is convenient
// while updating the tables; otherwise fetch the supplemental file matching
// the CLDR built into the Node used for extraction.
const metazoneSource = process.env.CLDR_METAZONES || root + "metaZones.json";
let metazoneRaw;
if (fs.existsSync(metazoneSource)) {
  metazoneRaw = fs.readFileSync(metazoneSource, "utf8");
} else {
  const version = process.versions.cldr.split(".")[0] + ".0.0";
  const url = "https://raw.githubusercontent.com/unicode-org/cldr-json/" +
    version + "/cldr-json/cldr-core/supplemental/metaZones.json";
  const response = await fetch(url);
  if (!response.ok) throw new Error("reading " + url + ": " + response.status);
  metazoneRaw = await response.text();
}
const metazoneDocument = JSON.parse(metazoneRaw);
const metazoneTree = metazoneDocument.supplemental.metaZones.metazoneInfo.timezone;
const metazoneMappings = metazoneDocument.supplemental.metaZones.metazones;

const metazones = {};
const flattenMetazones = (value, path = []) => {
  for (const [name, child] of Object.entries(value)) {
    const next = [...path, name];
    if (Array.isArray(child)) {
      metazones[next.join("/")] = child.map(item => item.usesMetazone);
    } else {
      flattenMetazones(child, next);
    }
  }
};
flattenMetazones(metazoneTree);

// Generic names use the reference zone for the locale's likely region. A
// target zone can therefore need a location fallback in one language but not
// another when those zones follow different offset transitions.
const localeRegions = new Set(locales.map(locale =>
  new Intl.Locale(locale).maximize().region || "001"));
localeRegions.add("001");
const referenceZones = new Map();
for (const item of metazoneMappings) {
  const mapping = item.mapZone;
  if (!localeRegions.has(mapping._territory)) continue;
  let names = referenceZones.get(mapping._other);
  if (names === undefined) {
    names = new Set();
    referenceZones.set(mapping._other, names);
  }
  names.add(mapping._type);
}
for (const [metazone, names] of referenceZones) {
  referenceZones.set(metazone, [...names].sort());
}

const boundaryTime = text => text === undefined ? undefined :
  Date.parse(text.replace(" ", "T") + "Z");

// Keep enough future history to cover the long-range compatibility corpus.
// ICU can change metazone fallback at future transitions (Morocco is one
// example), which cannot always be reconstructed from the modern name pair.
const HISTORY_FIRST = Date.UTC(1800, 0, 1);
// The modern name table is sampled from 2025 rules and is also the right
// source for the compatibility corpus's 2024 instants. Localized generic
// names can change even when their English name and metazone do not (Akan's
// name for Asia/Bishkek changed in 2007), so do not deduplicate the current
// period through an older English-identical historical record.
const MODERN_START = Date.UTC(2024, 0, 1);
const HISTORY_END = Date.UTC(2301, 0, 1);

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

// Unlike the modern table, a historical entry describes one exact interval
// between offset/metazone transitions. Both seasonal slots deliberately hold
// the observed name. Runtime lookup therefore follows ICU's historical
// classification instead of trying to reproduce it with time.Time.IsDST.
const historicalNamesAt = (locale, zone, when) => {
  const longOffset = nameAt(locale, zone, "longOffset", when);
  const shortOffset = nameAt(locale, zone, "shortOffset", when);
  const long = nameAt(locale, zone, "long", when);
  const short = nameAt(locale, zone, "short", when);
  const longGeneric = nameAt(locale, zone, "longGeneric", when);
  const shortGeneric = nameAt(locale, zone, "shortGeneric", when);
  return [
    long === longOffset ? "" : long,
    long === longOffset ? "" : long,
    short === shortOffset ? "" : short,
    short === shortOffset ? "" : short,
    longGeneric === longOffset ? "" : longGeneric,
    shortGeneric === shortOffset ? "" : shortGeneric,
  ].join("|");
};

const metazoneAt = (zone, when) => {
  for (const period of metazones[zone] || []) {
    const from = boundaryTime(period._from);
    const to = boundaryTime(period._to);
    if ((from === undefined || when >= from) &&
        (to === undefined || when < to)) return period._mzone;
  }
  return "";
};

const transitionCache = new Map();
const transitionTimes = zone => {
  const cached = transitionCache.get(zone);
  if (cached !== undefined) return cached;
  const out = [];
  let cursor = Temporal.Instant.fromEpochMilliseconds(HISTORY_FIRST)
    .toZonedDateTimeISO(zone);
  for (;;) {
    const next = cursor.getTimeZoneTransition("next");
    if (next === null) break;
    const when = Number(next.epochMilliseconds);
    if (when >= HISTORY_END) break;
    if (when > HISTORY_FIRST) out.push(when);
    cursor = next;
  }
  transitionCache.set(zone, out);
  return out;
};

const offsetAtInstant = (zone, when) => Number(
  Temporal.Instant.fromEpochMilliseconds(when)
    .toZonedDateTimeISO(zone).offsetNanoseconds);

// This is the part of ICU's generic-name decision that English alone cannot
// reveal. Offset deltas for every relevant regional reference zone separate
// intervals such as Paris-before-Berlin-DST and Kyiv-without-Cairo-DST.
const referenceSignature = (zone, when) => {
  const metazone = metazoneAt(zone, when);
  const references = referenceZones.get(metazone) || [];
  const offset = offsetAtInstant(zone, when);
  return references.map(reference =>
    reference + ":" + Number(offsetAtInstant(reference, when) === offset)).join(";");
};

// Find a name change inside an interval whose offset and metazone are fixed.
// ICU's generic-name fallback can switch shortly before a future transition,
// so the transition itself is not always the first instant with a new name.
const firstNameChange = (zone, from, to, before, after) => {
  if (before === after) return undefined;
  let low = from, high = to;
  while (high-low > 1) {
    const middle = low + Math.floor((high-low) / 2);
    if (historicalNamesAt("en", zone, middle) === before) low = middle;
    else high = middle;
  }
  return high;
};

// Build a compact historical timeline. It contains one representative for
// each distinct (metazone, English result) pair rather than one localized row
// per transition; the representatives are localized once below.
const historyRecords = [];
const historyRecordOf = new Map();
const historyPeriods = {};
const modernSeasonsByRecord = new Map();
const historyKey = (zone, when, english) => zone + "\x02" +
  metazoneAt(zone, when) + "\x02" + referenceSignature(zone, when) +
  "\x02" + english;
const historyRecord = (zone, when, english, family = "") => {
  // The same English fallback can be localized differently for two places,
  // so sharing is safe within one zone only. The later all-locale grouping
  // still combines records proven identical in every language.
  const key = historyKey(zone, when, english) + family;
  let at = historyRecordOf.get(key);
  if (at === undefined) {
    at = historyRecords.length;
    historyRecordOf.set(key, at);
    historyRecords.push({zone, when});
  }
  return at;
};

for (const zone of zones) {
  if (metazones[zone] === undefined) continue;
  const boundaries = new Set([HISTORY_FIRST, 0, MODERN_START, HISTORY_END]);
  // Ireland models winter as negative daylight saving time. Go's TZif
  // IsDST classification can change in projected years even when ICU's name
  // state matches a modern probe, so its future transitions stay explicit.
  const keepFutureTimeline = zone === "Europe/Dublin";
  const modernKeys = new Map();
  for (let season = 0; season < WHEN.length; season++) {
    const when = WHEN[season];
    const key = historyKey(zone, when, historicalNamesAt("en", zone, when));
    const seasons = modernKeys.get(key) || [];
    seasons.push(season);
    modernKeys.set(key, seasons);
  }
  for (const period of metazones[zone]) {
    const from = boundaryTime(period._from);
    const to = boundaryTime(period._to);
    if (from !== undefined && from > HISTORY_FIRST && from < HISTORY_END) boundaries.add(from);
    if (to !== undefined && to > HISTORY_FIRST && to < HISTORY_END) boundaries.add(to);
    const first = Math.max(from === undefined ? HISTORY_FIRST : from, HISTORY_FIRST);
    const last = Math.min(to === undefined ? HISTORY_END : to, HISTORY_END);
    for (const reference of referenceZones.get(period._mzone) || []) {
      for (const when of transitionTimes(reference)) {
        if (when > first && when < last) boundaries.add(when);
      }
    }
  }
  for (const when of transitionTimes(zone)) boundaries.add(when);
  const ordered = [...boundaries].sort((a, b) => a-b);
  const timeline = [];

  // Dates before the first tzdb transition use its earliest offset/metazone
  // rules. File them under an unbounded first interval.
  const early = HISTORY_FIRST;
  const earlyNames = historicalNamesAt("en", zone, early);
  timeline.push([null, historyRecord(zone, early, earlyNames)]);

  for (let i = 0; i + 1 < ordered.length; i++) {
    const from = ordered[i], to = ordered[i + 1];
    if (to <= HISTORY_FIRST || from >= HISTORY_END) continue;
    const first = Math.max(from, HISTORY_FIRST);
    const last = to - 1;
    let cursor = first;
    let names = historicalNamesAt("en", zone, cursor);
    const starts = [[cursor, names]];
    // A generic fallback can change and change back while the actual offset
    // remains fixed (Iran did so in the 1980s). Monthly samples find those
    // interior runs; binary search then recovers their exact millisecond.
    const step = 30 * 24 * 60 * 60 * 1000;
    for (let sample = Math.min(cursor + step, last);;) {
      const sampled = historicalNamesAt("en", zone, sample);
      if (sampled !== names) {
        const change = firstNameChange(zone, cursor, sample, names, sampled);
        starts.push([change, historicalNamesAt("en", zone, change)]);
        names = sampled;
      }
      cursor = sample;
      if (sample === last) break;
      sample = Math.min(sample + step, last);
    }
    for (let atStart = 0; atStart < starts.length; atStart++) {
      const start = starts[atStart][0];
      const until = atStart + 1 < starts.length ? starts[atStart + 1][0] : to;
      // Right on an offset transition ICU can briefly choose a location
      // fallback in only some locales. The stable name for the interval is
      // observed in its middle; the timeline still begins at the exact
      // transition millisecond.
      const probe = start + Math.floor((until-start) / 2);
      const entry = historicalNamesAt("en", zone, probe);
      // The ordinary six-name table already handles recurring future states.
      // Retain only future states whose metazone or regional fallback differs
      // from both modern probes.
      const key = historyKey(zone, probe, entry);
      const modernCandidate = !keepFutureTimeline && start >= MODERN_START &&
        modernKeys.has(key);
      // Do not reuse an English-identical historical representative here:
      // its localization can be stale even though every English discriminator
      // agrees, as with Akan's name for Bishkek before and after 2007.
      const at = historyRecord(zone, probe, entry,
        modernCandidate ? "\x02modern" : "");
      if (modernCandidate) {
        modernSeasonsByRecord.set(at, modernKeys.get(key));
      }
      const previous = timeline[timeline.length - 1];
      if (previous === undefined || previous[1] !== at) timeline.push([start, at]);
    }
  }
  // Empty record means the runtime returns to the modern seasonal table.
  if (timeline[timeline.length - 1][1] !== -1) {
    timeline.push([HISTORY_END, -1]);
  }
  historyPeriods[zone] = timeline;
}

process.stderr.write("historical zone names: " + historyRecords.length +
  " distinct intervals\n");

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
const historyPerLocale = {};
const legacyPerLocale = {};
const started = Date.now();
for (const locale of locales) {
  perLocale[locale] = namesIn(locale);
  historyPerLocale[locale] = historyRecords.map(record =>
    historicalNamesAt(locale, record.zone, record.when));
  if (legacyPerLocale[locale] === undefined) {
    legacyPerLocale[locale] = runLegacyHelper("names",
      {samples: legacySamples}, locale);
  }
  // Nothing here needs a formatter from the language just read again.
  formatters.clear();
}
process.stderr.write("read " + locales.length + " languages in " +
  ((Date.now() - started) / 1000).toFixed(0) + "s\n");

// Replace a current/future interval with the compact modern table only when
// every locale proves that table can reproduce it. English alone cannot show
// localized seasonal generic fallbacks such as Bosnian "CET (Algiers)".
const modernCanReproduce = (zone, record, season) => locales.every(locale => {
  const historical = historyPerLocale[locale][record].split("|");
  const modern = perLocale[locale][zone].split("|");
  return historical[0] === modern[season] &&
    historical[2] === modern[3 + season] &&
    historical[4] === modern[2] && historical[5] === modern[5];
});
for (const [zone, timeline] of Object.entries(historyPeriods)) {
  const compact = [];
  for (const [start, record] of timeline) {
    let kept = record;
    const seasons = modernSeasonsByRecord.get(record) || [];
    if (start !== null && start >= MODERN_START &&
        seasons.some(season => modernCanReproduce(zone, record, season))) {
      kept = -1;
    }
    if (compact.length === 0 || compact[compact.length - 1][1] !== kept) {
      compact.push([start, kept]);
    }
  }
  historyPeriods[zone] = compact;
}

// Candidate records proven identical to the modern table are no longer
// referenced. Remove them before transposing the multilingual matrices; even
// an unreachable column would otherwise cost one cell per locale and name.
const usedHistoryRecords = new Set();
for (const timeline of Object.values(historyPeriods)) {
  for (const [, record] of timeline) {
    if (record >= 0) usedHistoryRecords.add(record);
  }
}
const keptHistoryRecords = [...usedHistoryRecords].sort((a, b) => a-b);
const historyRecordRemap = new Map(keptHistoryRecords.map((record, at) => [record, at]));
for (const timeline of Object.values(historyPeriods)) {
  for (const period of timeline) {
    if (period[1] >= 0) period[1] = historyRecordRemap.get(period[1]);
  }
}
for (const locale of locales) {
  historyPerLocale[locale] = keptHistoryRecords.map(record =>
    historyPerLocale[locale][record]);
}
const compactHistoryRecords = keptHistoryRecords.map(record => historyRecords[record]);
historyRecords.splice(0, historyRecords.length, ...compactHistoryRecords);

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

const historyGroupOf = [];
const historyGroups = new Map();
for (let record = 0; record < historyRecords.length; record++) {
  const key = locales.map(locale => historyPerLocale[locale][record]).join("");
  if (!historyGroups.has(key)) historyGroups.set(key, historyGroups.size);
  historyGroupOf[record] = historyGroups.get(key);
}

const historyStandIn = [];
for (let record = 0; record < historyRecords.length; record++) {
  if (historyStandIn[historyGroupOf[record]] === undefined) {
    historyStandIn[historyGroupOf[record]] = record;
  }
}

const historyBlocks = new Map();
const historyBlockOf = {};
for (const locale of locales) {
  const row = historyStandIn.map(record => historyPerLocale[locale][record]).join("");
  if (!historyBlocks.has(row)) historyBlocks.set(row, historyBlocks.size);
  historyBlockOf[locale] = historyBlocks.get(row);
}

const legacyGroups = new Map();
const legacyGroupOf = {};
for (const zone of zones) {
  const key = locales.map(locale => legacyPerLocale[locale][zone]).join("\x02");
  if (!legacyGroups.has(key)) legacyGroups.set(key, legacyGroups.size);
  legacyGroupOf[zone] = legacyGroups.get(key);
}

const legacyStandIn = [];
for (const zone of zones) {
  if (legacyStandIn[legacyGroupOf[zone]] === undefined) {
    legacyStandIn[legacyGroupOf[zone]] = zone;
  }
}

const legacyBlocks = new Map();
const legacyBlockOf = {};
for (const locale of locales) {
  const row = legacyStandIn.map(zone => legacyPerLocale[locale][zone]).join("\x01");
  if (!legacyBlocks.has(row)) legacyBlocks.set(row, legacyBlocks.size);
  legacyBlockOf[locale] = legacyBlocks.get(row);
}

const gmtForms = new Map();
const gmtOf = {};
for (const locale of locales) {
  const form = gmtFormsIn(locale);
  if (!gmtForms.has(form)) gmtForms.set(form, gmtForms.size);
  gmtOf[locale] = gmtForms.get(form);
}

// Historical rows are by far the largest generator artifact. Each cell has
// duplicate seasonal slots, so store four fields as uint24 indexes into one
// string dictionary. The four transposed matrices also compress much better
// when the Go table generator consumes them.
const historyRows = [...historyBlocks.keys()].map(row => row.split("\x01"));
const historyDictionary = [""];
const historyDictionaryIndex = new Map([["", 0]]);
const historyColumns = historyRows[0]?.length || 0;
const historyMatrices = Array.from({length: 4}, () =>
  Buffer.alloc(historyRows.length * historyColumns * 3));
for (let row = 0; row < historyRows.length; row++) {
  for (let column = 0; column < historyColumns; column++) {
    const fields = historyRows[row][column].split("|");
    [fields[0] || "", fields[2] || "", fields[4] || "", fields[5] || ""]
      .forEach((field, matrix) => {
        let at = historyDictionaryIndex.get(field);
        if (at === undefined) {
          at = historyDictionary.length;
          if (at >= 2 ** 24) throw new Error("historical name dictionary exceeds uint24");
          historyDictionaryIndex.set(field, at);
          historyDictionary.push(field);
        }
        historyMatrices[matrix].writeUIntLE(
          at, (row * historyColumns + column) * 3, 3);
      });
  }
}

process.stderr.write("zones " + zones.length + " in " + groups.size +
  " groups; " + locales.length + " languages in " + blocks.size + " blocks, " +
  gmtForms.size + " ways of writing an offset; history in " +
  historyGroups.size + " groups and " + historyBlocks.size + " blocks; " +
  "legacy Date names in " + legacyGroups.size + " groups and " +
  legacyBlocks.size + " blocks\n");

process.stdout.write(JSON.stringify({
  icu: process.versions.icu,
  // The zone standing for each group, so that the packer can ask Go's zone
  // files which of the two probes was summer time.
  standIn,
  groups: groupOf,
  english: [...blocks.keys()][blockOf["en"]].split(""),
  blocks: blockOf,
  names: [...blocks.keys()].map(row => row.split("")),
  history: {
    periods: Object.fromEntries(Object.entries(historyPeriods).map(([zone, periods]) =>
      [zone, periods.map(([from, record]) =>
        [from, record === null ? null : historyGroupOf[record]])])),
    blocks: historyBlockOf,
    dictionary: historyDictionary,
    rows: historyRows.length,
    columns: historyColumns,
    matrices: historyMatrices.map(matrix => matrix.toString("base64")),
  },
  legacy: {
    periods: Object.fromEntries(zones.map(zone =>
      [zone, legacyTimeline[zone].changes])),
    groups: legacyGroupOf,
    standIn: legacyStandIn,
    blocks: legacyBlockOf,
    names: [...legacyBlocks.keys()].map(row => row.split("")),
  },
  gmt: gmtOf,
  gmtForms: [...gmtForms.keys()],
}) + "\n");
