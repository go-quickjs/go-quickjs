// numbering.mjs reads the digits of every numbering system.
//
// Usage:
//
//	node internal/icu/internal/cldrgen/numbering.mjs > .../numbering.json
//
// Most of the world writes numbers with the ten digits it was taught, and
// which ten those are is part of the language: Arabic has ٠١٢٣٤٥٦٧٨٩, Hindi
// has ०१२३४५६७८९, Thai has ๐๑๒๓๔๕๖๗๘๙. A locale carries the ones it uses, and
// a tag may ask for others -- "ar-u-nu-latn" is Arabic written with the digits
// this sentence is written with. So all of them are wanted, not just the ones
// some locale defaults to.

const names = Intl.supportedValuesOf("numberingSystem");
const systems = {};
for (const name of names) {
  const format = new Intl.NumberFormat("en-u-nu-" + name, {useGrouping: false});
  const shown = [...Array(10).keys()].map(n => format.format(n));
  // A system that writes a number as something other than ten digits -- roman
  // numerals, or the counting rods -- cannot be written as a table of ten, and
  // is left out.
  if (shown.some(d => [...d].length !== 1)) continue;
  const digits = shown.join("");
  // The marks that go with those digits: Arabic writes its decimal point and
  // its thousands mark differently from the way it writes them in Latin
  // letters.
  const marks = new Intl.NumberFormat("en-u-nu-" + name)
    .formatToParts(12345.6);
  const decimal = marks.find(p => p.type === "decimal");
  const group = marks.find(p => p.type === "group");
  const symbols = (decimal ? decimal.value : ".") + (group ? group.value : ",");
  if (digits === "0123456789" && symbols === ".," && name !== "latn") continue;
  systems[name] = digits + "\u0001" + symbols;
}

process.stdout.write(JSON.stringify({icu: process.versions.icu, systems}) + "\n");
