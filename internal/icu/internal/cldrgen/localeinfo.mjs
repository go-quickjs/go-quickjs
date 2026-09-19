// localeinfo.mjs reads the supplemental data exposed through Intl.Locale.
// The generator packs its output alongside the other ICU-derived tables.

import fs from "fs";
import path from "path";
import {fileURLToPath} from "url";

const here = path.dirname(fileURLToPath(import.meta.url));
const display = JSON.parse(fs.readFileSync(path.join(here, "display.json"), "utf8"));
const localeList = JSON.parse(fs.readFileSync(path.join(here, "locales.json"), "utf8"));
const likelySource = JSON.parse(
  fs.readFileSync(path.join(here, "likelysubtags.json"), "utf8"));
if (!process.versions.cldr.startsWith(likelySource.supplemental.version._cldrVersion)) {
  throw new Error(`likely-subtag data is CLDR ${likelySource.supplemental.version._cldrVersion}, ` +
    `but Node uses CLDR ${process.versions.cldr}`);
}

const languages = new Set();
const scripts = new Set();
const regions = new Set(["001"]);

// Script names are not all present in DisplayNames data (Shavian is one
// example), even though likely-subtag data uses them. This is the ISO 15924
// set known by the ICU version the tables are generated from.
for (const script of (`Adlm Aghb Ahom Arab Armi Armn Avst Bali Bamu Bass Batk Beng Berf Bhks
  Bopo Brah Brai Bugi Buhd Cakm Cans Cari Cham Cher Chrs Copt Cpmn Cprt Cyrl
  Deva Diak Dogr Dupl Egyp Elba Elym Ethi Gara Geor Glag Gong Gonm Goth Gran
  Grek Gujr Gukh Guru Hanb Hang Hani Hano Hans Hant Hatr Hebr Hira Hluw Hmng
  Hmnp Hung Ital Jamo Java Jpan Kali Kana Kawi Khar Khmr Khoj Kits Knda Kore
  Krai Kthi Lana Laoo Latf Latg Latn Lepc Limb Lina Linb Lisu Lyci Lydi Mahj
  Maka Mand Mani Marc Medf Mend Merc Mero Mlym Modi Mong Mroo Mtei Mult Mymr
  Nagm Nand Narb Nbat Newa Nkoo Ogam Olck Onao Orkh Orya Osge Osma Ougr Palm
  Pauc Perm Phag Phli Phlp Phnx Plrd Prti Rjng Rohg Runr Samr Sarb Saur Sgnw
  Shaw Shrd Sidd Sidt Sind Sinh Sogd Sogo Sora Soyo Sund Sunu Sylo Syrc Tagb
  Takr Tale Talu Taml Tang Tavt Tayo Telu Tfng Tglg Thaa Thai Tibt Tirh Tnsa
  Todr Tols Toto Tutg Ugar Vaii Vith Wara Wcho Xpeo Xsux Yezi Yiii Zanb`).split(/\s+/)) {
  scripts.add(script);
}

for (const entries of Object.values(display.names)) {
  for (const entry of entries) {
    const key = entry.slice(0, entry.indexOf("="));
    if (key[0] === "l") {
      const language = key.slice(1).split("-")[0];
      if (/^(?:[a-z]{2,3}|[a-z]{5,8})$/i.test(language)) languages.add(language.toLowerCase());
    } else if (/^s[A-Za-z]{4}$/.test(key)) {
      scripts.add(key.slice(1, 2).toUpperCase() + key.slice(2).toLowerCase());
    } else if (/^r(?:[A-Za-z]{2}|[0-9]{3})$/.test(key)) {
      regions.add(key.slice(1).toUpperCase());
    }
  }
}
for (const tag of localeList.locales) languages.add(tag.split("-")[0].toLowerCase());

const sorted = set => [...set].sort();
const same = (a, b) => a.length === b.length && a.every((v, i) => v === b[i]);
const minimumDaysFour = new Set(`AD AN AT AX BE BG CH CZ DE DK EE ES FI FJ FO FR GB GF GG GI
  GP GR IE IM IS IT JE LI LT LU MC MQ NL NO PL PT RE RU SE SJ SK SM VA`.split(/\s+/));
const likely = {};
for (const [from, to] of Object.entries(likelySource.supplemental.likelySubtags)) {
  likely[from.replaceAll("_", "-")] = to.replaceAll("_", "-");
}

const territory = {};
for (const region of sorted(regions)) {
  const locale = new Intl.Locale(`und-${region}`);
  const zones = locale.getTimeZones();
  const week = locale.getWeekInfo();
  territory[region] = {
    calendars: locale.getCalendars(),
    hourCycles: locale.getHourCycles(),
    firstDay: week.firstDay,
    minimalDays: minimumDaysFour.has(region) ? 4 : 1,
    weekend: week.weekend,
    timeZones: zones === undefined ? [] : zones,
  };
}

// CLDR has a small number of language-and-territory clock preferences, such
// as French in Canada. Record only the rows that differ from the territory.
const hourCycles = {};
for (const language of sorted(languages)) {
  for (const region of sorted(regions)) {
    const values = new Intl.Locale(`${language}-${region}`).getHourCycles();
    if (!same(values, territory[region].hourCycles)) {
      hourCycles[`${language}-${region}`] = values;
    }
  }
}

const direction = {};
for (const script of sorted(scripts)) {
  direction[script] = new Intl.Locale(`und-${script}`).getTextInfo().direction;
}

// Collation choices depend on language and, when explicitly present, script;
// regions do not affect them. Keep only deviations from the root choices.
const rootCollations = new Intl.Locale("und").getCollations();
const collations = {};
for (const language of sorted(languages)) {
  const values = new Intl.Locale(language).getCollations();
  if (!same(values, rootCollations)) collations[language] = values;
  for (const script of sorted(scripts)) {
    const scriptValues = new Intl.Locale(`${language}-${script}`).getCollations();
    if (!same(scriptValues, rootCollations)) {
      collations[`${language}-${script}`] = scriptValues;
    }
  }
}

// Numbering systems are normally latn. Record bare-language and native-script
// exceptions, then the few explicit regions which override a language. A
// stored latn value is meaningful: it can override a non-latn bare language.
const numberingSystems = {};
for (const language of sorted(languages)) {
  const base = new Intl.Locale(language).getNumberingSystems()[0];
  if (base !== "latn") numberingSystems[language] = base;
  for (const script of sorted(scripts)) {
    const value = new Intl.Locale(`${language}-${script}`).getNumberingSystems()[0];
    if (value !== "latn") numberingSystems[`${language}-${script}`] = value;
  }
  for (const region of sorted(regions)) {
    const value = new Intl.Locale(`${language}-${region}`).getNumberingSystems()[0];
    if (value !== base) numberingSystems[`${language}-${region}`] = value;
  }
}

process.stdout.write(JSON.stringify({
  icu: process.versions.icu,
  likely,
  territory,
  hourCycles,
  direction,
  collations,
  numberingSystems,
}));
