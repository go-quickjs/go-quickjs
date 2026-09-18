// display.mjs reads what things are called: languages, regions, scripts,
// currencies, and the parts of a date.
//
// Usage:
//
//	node internal/icu/internal/cldrgen/display.mjs > .../display.json
//
// The names of four hundred things in four hundred languages is two and a half
// megabytes, which is more than a program that never opens a language picker
// should carry. So this writes two sets: the English names, which the engine
// keeps, and all of them, which go in a package a host imports when it wants
// them -- the same bargain the Go standard library offers for the time zone
// database.

import fs from "fs";
import path from "path";
import {fileURLToPath} from "url";

const here = path.dirname(fileURLToPath(import.meta.url));
const {locales} = JSON.parse(fs.readFileSync(path.join(here, "locales.json"), "utf8"));

// The languages, and the ones with a region or a script that are named in
// their own right: de-AT is Austrian German rather than German in Austria.
const languages = [...new Set([
  ...locales.map(tag => tag.split("-")[0]),
  ...locales,
  "en-US", "en-GB", "en-AU", "en-CA", "es-419", "es-MX", "es-ES", "fr-CA",
  "pt-BR", "pt-PT", "zh-Hans", "zh-Hant", "sr-Latn", "nl-BE", "de-CH", "de-AT",
])].sort();

const regions = [];
for (let a = 65; a <= 90; a++) {
  for (let b = 65; b <= 90; b++) regions.push(String.fromCharCode(a, b));
}
// The groupings CLDR names as regions too: the continents and the like.
for (const code of ["001", "002", "003", "005", "009", "011", "013", "014", "015",
                    "017", "018", "019", "021", "029", "030", "034", "035", "039",
                    "053", "054", "057", "061", "142", "143", "145", "150", "151",
                    "154", "155", "202", "419"]) {
  regions.push(code);
}

const scripts = ["Latn", "Cyrl", "Arab", "Hans", "Hant", "Hani", "Deva", "Jpan",
                 "Kore", "Grek", "Hebr", "Thai", "Beng", "Guru", "Gujr", "Orya",
                 "Taml", "Telu", "Knda", "Mlym", "Sinh", "Mymr", "Khmr", "Laoo",
                 "Ethi", "Armn", "Geor", "Tibt", "Mong", "Adlm", "Vaii", "Nkoo",
                 "Olck", "Cans", "Tfng", "Cher", "Hira", "Kana", "Hang", "Thaa",
                 "Syrc", "Samr", "Bugi", "Java", "Bali", "Sund", "Batk", "Lepc"];

const currencies = ["USD", "EUR", "GBP", "JPY", "CNY", "KRW", "INR", "RUB", "BRL",
                    "CAD", "AUD", "NZD", "MXN", "CHF", "SEK", "NOK", "DKK", "PLN",
                    "TRY", "ILS", "THB", "VND", "NGN", "ZAR", "PHP", "UAH", "IDR",
                    "MYR", "SGD", "HKD", "TWD", "CZK", "HUF", "RON", "BGN", "ISK",
                    "EGP", "SAR", "AED", "PKR", "BDT", "LKR", "NPR", "KES", "GHS",
                    "MAD", "ARS", "CLP", "COP", "PEN", "UYU", "BOB", "CRC", "DOP",
                    "GTQ", "JMD", "TTD", "KWD", "BHD", "OMR", "QAR", "JOD", "LBP",
                    "IQD", "IRR", "AFN", "AMD", "AZN", "GEL", "KZT", "UZS", "MNT",
                    "MMK", "KHR", "LAK", "BND", "FJD", "PGK", "XOF", "XAF", "XPF",
                    "ETB", "TZS", "UGX", "RWF", "MZN", "AOA", "BWP", "NAD", "ZMW"];

const calendars = ["gregory", "buddhist", "chinese", "coptic", "dangi", "ethiopic",
                   "hebrew", "indian", "islamic", "iso8601", "japanese", "persian",
                   "roc"];

const fields = ["era", "year", "quarter", "month", "weekOfYear", "weekday", "day",
                "dayPeriod", "hour", "minute", "second", "timeZoneName"];

// What one locale calls everything, as name=value items.
function namesFor(tag) {
  const parts = [];
  const add = (kind, type, items, options = {}) => {
    let names;
    try {
      names = new Intl.DisplayNames(tag, {type, fallback: "none", ...options});
    } catch (e) { return; }
    for (const code of items) {
      let name;
      try { name = names.of(code); } catch (e) { continue; }
      if (!name || name === code) continue;
      parts.push(kind + code + "=" + name);
    }
  };
  add("l", "language", languages);
  add("r", "region", regions);
  add("s", "script", scripts);
  add("c", "currency", currencies);
  add("a", "calendar", calendars);
  add("f", "dateTimeField", fields);
  return parts;
}

const all = {};
for (const tag of locales) {
  const names = namesFor(tag);
  if (names.length > 0) all[tag] = names;
}

process.stdout.write(JSON.stringify({icu: process.versions.icu, names: all}) + "\n");
