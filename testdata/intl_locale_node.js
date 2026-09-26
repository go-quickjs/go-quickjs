// What Intl.Locale answers for a spread of tags and options, one line each,
// "name<TAB>result". Run by Node, it writes testdata/intl_locale_node.txt:
//
//	node testdata/intl_locale_node.js > testdata/intl_locale_node.txt
//
// and TestIntlLocaleMatchesNode runs it here and compares. Left out is what
// Node 26 has not implemented and test262 holds go-quickjs to: the variants
// getter, and the variants option.
const show = v => v === undefined ? "undefined" : JSON.stringify(v);
const describe = l => [
  l.toString(), l.baseName, l.language, l.script, l.region,
  l.calendar, l.collation, l.firstDayOfWeek, l.hourCycle, l.caseFirst,
  l.numeric, l.numberingSystem,
].map(show).join(" ");
const info = l => [
  l.getCalendars(), l.getCollations(), l.getHourCycles(), l.getNumberingSystems(),
  l.getTimeZones(), l.getTextInfo(), l.getWeekInfo(),
].map(show).join(" ");
const cases = [
  ["en"], ["EN-us"], ["en-Latn-US-u-ca-gregory"], ["zh-Hant-TW"], ["sr-Cyrl"], ["und"],
  ["en-u-ca"], ["en-u-kn"], ["en-u-kn-true"], ["en-u-kn-false"], ["en-u-kf"], ["en-u-kf-upper"],
  ["en-u-co-phonebk"], ["de-u-co-phonebk"], ["en-u-ca-islamicc"], ["en-u-ca-ethiopic-amete-alem"],
  ["en-u-fw-mon"], ["en-u-hc-h23"], ["en-u-nu-arab"], ["en-u-rg-gbzzzz"], ["en-u-sd-usca"],
  ["en-US-u-rg-dezzzz"], ["ar"], ["he-IL"], ["fa"], ["ur"], ["ja-JP"], ["ko"], ["th-TH"],
  ["en-GB-oxendict"], ["de-1996-1901"], ["sl-rozaj-biske-1994"], ["iw"], ["in"], ["mo"],
  ["cmn-Hans"], ["sgn-GR"], ["zh-min-nan"], ["art-lojban"], ["en-t-de-h0-hybrid"], ["en-x-private"],
  ["en-a-bbb-u-ca-gregory-x-private"], ["es-419"], ["pt-BR"], ["und-Arab"], ["und-150"],
  ["en", { language: "fr" }], ["en", { script: "Cyrl" }], ["en", { region: "gb" }],
  ["en", { calendar: "japanese" }],
  ["en-u-ca-buddhist", { calendar: "japanese" }], ["en", { collation: "emoji" }],
  ["en", { firstDayOfWeek: "3" }], ["en", { firstDayOfWeek: "sun" }], ["en", { firstDayOfWeek: 0 }],
  ["en", { firstDayOfWeek: "someday" }], ["en", { hourCycle: "h11" }], ["en", { caseFirst: "false" }],
  ["en", { numeric: true }], ["en", { numeric: false }], ["en", { numberingSystem: "thai" }],
  ["en", { calendar: "islamicc" }], ["en", { language: "iw" }], ["en-u-kn", { numeric: false }],
  ["und", { region: "DE" }], ["en-u-fw-thu", { firstDayOfWeek: "fri" }],
];
for (const [tag, options] of cases) {
  const name = tag + (options ? " " + JSON.stringify(options) : "");
  let line;
  try {
    const l = new Intl.Locale(tag, options);
    line = describe(l) + " | " + describe(l.maximize()) + " | " + describe(l.minimize()) + " | " + info(l);
  } catch (e) {
    line = e.name + ": " + e.message;
  }
  console.log(name + "\t" + line);
}
