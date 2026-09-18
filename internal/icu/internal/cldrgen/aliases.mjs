// aliases.mjs reads the names that have been replaced since.
//
// Usage:
//
//	node internal/icu/internal/cldrgen/aliases.mjs > .../aliases.json
//
// A language tag may be written with a name that is no longer the name: "iw"
// was Hebrew before it was "he", "cmn" is Mandarin and is written "zh", "BU"
// was Burma before it was "MM", and "sh" was Serbo-Croatian before it became
// Serbian written in Latin letters. Two tags that mean the same thing have to
// be written the same way before they can be compared, so every one of these
// has to be known.
//
// They are not published in a form a program can ask for, so they are found
// the way everything else here is found: by asking a full ICU to canonicalize
// every name there could be and keeping the ones it changes. The languages,
// the regions and the scripts are enumerated outright; the variants and the
// settings a tag may carry are asked about from a list, since there are too
// many strings of their length to try them all.

const canon = (tag) => {
  try {
    const out = Intl.getCanonicalLocales(tag);
    return out.length > 0 ? out[0] : null;
  } catch (e) {
    return null;
  }
};

const letters = "abcdefghijklmnopqrstuvwxyz";
const digits = "0123456789";

// Every string of a length, from an alphabet.
function* every(alphabet, length, prefix = "") {
  if (length === 0) {
    yield prefix;
    return;
  }
  for (const c of alphabet) yield* every(alphabet, length - 1, prefix + c);
}

const isVariant = (s) => /^[a-z0-9]{5,8}$/.test(s) || /^[0-9][a-z0-9]{3}$/.test(s);
const isRegion = (s) => /^[A-Z]{2}$/.test(s) || /^[0-9]{3}$/.test(s);

const languages = {};
for (const length of [2, 3]) {
  for (const tag of every(letters, length)) {
    const out = canon(tag);
    if (out !== null && out !== tag) languages[tag] = out;
  }
}

const regions = {};
for (const alphabet of [letters, digits]) {
  for (const code of every(alphabet, alphabet === letters ? 2 : 3)) {
    const region = code.toUpperCase();
    const out = canon("und-" + region);
    if (out !== null && out !== "und-" + region) regions[region] = out;
  }
}

// A country that was dissolved becomes several, and which of them a tag means
// depends on the language it is written in, or on the letters it is written
// with: Soviet Armenian is Armenian, and so is anything Soviet written in the
// Armenian script. Those are recorded against the pair.
const regionsByLanguage = {};
for (const region of Object.keys(regions)) {
  const fallback = regions[region].slice("und-".length);
  const at = (prefix, suffix) => {
    const out = canon(prefix + "-" + region + suffix);
    if (out === null) return;
    const parts = out.split("-");
    const got = parts.length > 1 ? parts[parts.length - 1] : "";
    if (got !== "" && got !== fallback && isRegion(got)) {
      regionsByLanguage[prefix + "-" + region] = got;
    }
  };
  for (const length of [2, 3]) {
    for (const language of every(letters, length)) at(language, "");
  }
  for (const code of every(letters, 4)) {
    const script = code[0].toUpperCase() + code.slice(1);
    at("und-" + script, "");
  }
}

const scripts = {};
for (const code of every(letters, 4)) {
  const script = code[0].toUpperCase() + code.slice(1);
  const out = canon("und-" + script);
  if (out !== null && out !== "und-" + script) scripts[script] = out;
}

// The tags that were registered before the rules were what they are, which are
// a fixed list rather than a pattern.
const GRANDFATHERED = [
  "art-lojban", "cel-gaulish", "en-gb-oed", "i-ami", "i-bnn", "i-default",
  "i-enochian", "i-hak", "i-klingon", "i-lux", "i-mingo", "i-navajo", "i-pwn",
  "i-tao", "i-tay", "i-tsu", "no-bok", "no-nyn", "sgn-be-fr", "sgn-be-nl",
  "sgn-ch-de", "zh-guoyu", "zh-hakka", "zh-min", "zh-min-nan", "zh-xiang",
  "cmn-hans", "sgn-br", "sgn-co", "sgn-de", "sgn-dk", "sgn-es", "sgn-fr",
  "sgn-gb", "sgn-gr", "sgn-ie", "sgn-it", "sgn-jp", "sgn-mx", "sgn-ni",
  "sgn-nl", "sgn-no", "sgn-pt", "sgn-se", "sgn-us", "sgn-za",
];
const grandfathered = {};
for (const tag of GRANDFATHERED) {
  const out = canon(tag);
  if (out !== null && out.toLowerCase() !== tag) grandfathered[tag] = out;
}
// A few replacements are whole language-plus-variant tags rather than a
// replacement for the variant wherever it appears. Western Armenian is the
// remaining registered case: arevmda disappears in other languages, but
// hy-arevmda became the language hyw.
for (const tag of ["hy-arevmda"]) {
  const out = canon(tag);
  if (out !== null && out.toLowerCase() !== tag) grandfathered[tag] = out;
}

// The variants a language may be written in. There are too many strings of
// five to eight characters to try them all, so this is the registered list.
const VARIANTS = [
  "1606nict", "1694acad", "1901", "1959acad", "1994", "1996", "aaland",
  "abl1943", "akuapem", "alalc97", "aluku", "ao1990", "aranes", "arevela",
  "arevmda", "arkaika", "asante", "auvern", "baku1926", "balanka", "barla",
  "basiceng", "bauddha", "bciav", "bcizbl", "biscayan", "biske", "bohoric",
  "boont", "bornholm", "cisaup", "colb1945", "cornu", "creiss", "dajnko",
  "ekavsk", "emodeng", "fascia", "fonipa", "fonkirsh", "fonnapa", "fonupa",
  "fonxsamp", "gallo", "gascon", "grclass", "grital", "grmistr", "hepburn",
  "heploc", "hognorsk", "hsistemo", "ijekavsk", "itihasa", "ivanchov",
  "jauer", "jyutping", "kkcor", "kociewie", "kscor", "laukika", "lemosin",
  "lengadoc", "lipaw", "ltg1929", "ltg2007", "luna1918", "metelko", "monoton",
  "ndyuka", "nedis", "newfound", "nicard", "njiva", "nulik", "osojs",
  "oxendict", "pahawh2", "pahawh3", "pahawh4", "pamaka", "peano", "petr1708",
  "pinyin", "polyton", "polytoni", "provenc", "puter", "rigik", "rozaj",
  "rumgr", "scotland", "scouse", "simple", "solba", "sotav", "spanglis",
  "surmiran", "sursilv", "sutsilv", "synnejyl", "tarask", "tongyong",
  "tunumiit", "uccor", "ucrcor", "ulster", "unifon", "vaidika", "valencia",
  "vallader", "vecdruka", "vivaraup", "wadegile", "xsistemo",
];
const variants = {};
for (const variant of VARIANTS) {
  const out = canon("en-" + variant);
  if (out === null || out === "en-" + variant) continue;
  const replacement = out.slice(out.startsWith("en-") ? 3 : 2);
  // A variant may become another variant, or a region, or nothing at all.
  // Anything else is this ICU misreading a variant that ends in a year, and
  // is left alone.
  if (replacement === "" || isVariant(replacement) || isRegion(replacement)) {
    variants[variant] = replacement;
  }
}
// The pairs that become one: Japanese written the way the Library of Congress
// writes it was two variants and is now one.
for (const pair of ["hepburn-heploc"]) {
  const out = canon("en-" + pair);
  if (out !== null && out !== "en-" + pair) variants[pair] = out.slice(3);
}

// The settings a tag may carry, and the values that have been renamed. The
// keys are the ones a formatter reads; the values are asked about from what
// this ICU says it supports along with the older names for them.
const SETTINGS = {
  ca: [...supported("calendar"), "ethiopic-amete-alem", "islamicc",
       "gregorian", "islamic-civil", "islamic-tbla", "islamic-umalqura",
       "islamic-rgsa", "japanese", "roc", "buddhist"],
  co: [...supported("collation"), "phonebook", "traditional", "gb2312han",
       "big5han", "dictionary", "direct", "phonetic", "pinyin", "reformed",
       "searchjl", "stroke", "unihan", "zhuyin", "standard", "search"],
  nu: [...supported("numberingSystem"), "traditional", "native", "finance",
       "defaul", "default"],
  ks: ["primary", "secondary", "tertiary", "quaternary", "quartenary",
       "identical", "level1", "level2", "level3", "level4", "identic"],
  ms: ["imperial", "uksystem", "metric", "ussystem"],
  // The keys whose value is yes or no, where yes is the word for true and
  // true is written by naming the key and nothing more. Every other key keeps
  // "yes" as the ordinary word it is.
  kb: ["yes", "true"],
  kc: ["yes", "true"],
  kh: ["yes", "true"],
  kk: ["yes", "true"],
  kn: ["yes", "true"],
  kv: ["space", "punct", "symbol", "currency"],
  kf: ["upper", "lower", "false"],
  hc: ["h11", "h12", "h23", "h24"],
  // Transform fields use the same key-value shape as Unicode settings. Keep
  // their deprecated values in the same alias table so both extensions can
  // share the canonicalisation code.
  m0: ["names"],
  tz: timeZoneCodes(),
  rg: subdivisions(),
  sd: subdivisions(),
};

function supported(key) {
  try {
    return Intl.supportedValuesOf(key);
  } catch (e) {
    return [];
  }
}

// The codes a time zone goes by in a tag are a region and a city run together
// -- Asia/Shanghai is "cnsha" -- and cannot be derived from the IANA name.
// Canonical codes already stay as written, so only the finite set of retired
// codes and aliases that themselves fit the Unicode value grammar need to be
// probed. This list comes from CLDR's common/bcp47/timezone.xml.
function timeZoneCodes() {
  return [
    "aqams", "aukns", "caffs", "camtr", "canpg", "capnt", "cathu", "cayzf",
    "cet", "cnckg", "cnhrb", "cnkhg", "cst6cdt", "cuba", "eet", "egypt",
    "eire", "est", "est5edt", "factory", "gaza", "gmt", "gmt0", "hongkong",
    "hst", "iceland", "iran", "israel", "jamaica", "japan", "libya", "met",
    "mncoq", "mst", "mst7mdt", "mxstis", "navajo", "poland", "portugal",
    "prc", "pst8pdt", "roc", "rok", "turkey", "uaozh", "uauzh", "uct",
    "umjon", "usnavajo", "utc", "wet", "zulu",
  ];
}

// The subdivisions a tag may name, which are a region and one to three
// characters after it: "no23" is a county of Norway, "cz10a" a district of
// Prague, and "fra" a region of France.
function* subdivisions() {
  const alphanumeric = letters + digits;
  for (const region of every(letters, 2)) {
    for (const one of alphanumeric) {
      yield region + one;
      for (const two of alphanumeric) {
        yield region + one + two;
        for (const three of alphanumeric) yield region + one + two + three;
      }
    }
  }
}

const settings = {};
for (const [key, values] of Object.entries(SETTINGS)) {
  const seen = new Set();
  for (const value of values) {
    if (seen.size < 1000) {
      if (seen.has(value)) continue;
      seen.add(value);
    }
    const tag = "und-u-" + key + "-" + value;
    const out = canon(tag);
    if (out === null || out === tag) continue;
    const written = out.slice("und-u-".length);
    settings[key + "-" + value] = written;
  }
}

process.stdout.write(JSON.stringify({
  icu: process.versions.icu,
  languages, regions, regionsByLanguage, scripts, grandfathered, variants, settings,
}) + "\n");
