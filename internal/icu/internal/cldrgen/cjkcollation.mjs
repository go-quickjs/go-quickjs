// cjkcollation.mjs writes one locale's primary order for a CJK script.
// The binary stream contains three bytes per character. The low 21 bits are
// the code point and bit 23 marks the start of a new primary-weight group.

import fs from "fs";
import path from "path";
import {fileURLToPath} from "url";

const here = path.dirname(fileURLToPath(import.meta.url));
const [, , locale, script] = process.argv;
if (!locale || !script) throw new Error("usage: cjkcollation.mjs locale han|kana|hangul");

const {icu, weights} = JSON.parse(fs.readFileSync(path.join(here, "collation.json"), "utf8"));
if (icu !== process.versions.icu) {
  throw new Error(`collation data is ICU ${icu}, but node uses ICU ${process.versions.icu}`);
}
const belongs = {
  han: ch => /\p{Script_Extensions=Han}/u.test(ch),
  kana: ch => /\p{Script_Extensions=Hiragana}|\p{Script_Extensions=Katakana}/u.test(ch),
  hangul: ch => /\p{Script_Extensions=Hangul}/u.test(ch),
}[script];
if (!belongs) throw new Error(`unknown script ${script}`);

const pointSet = new Set(weights.map(row => row[0])
  .filter(cp => belongs(String.fromCodePoint(cp))));
// ICU 78.3 knows unified Han through Extension J at U+33479, beyond the
// root table's historical U+2FFFF limit.
for (let cp = 0x30000; cp < 0x33500; cp++) {
  if (belongs(String.fromCodePoint(cp))) pointSet.add(cp);
}
const points = [...pointSet];
const collator = new Intl.Collator(locale, {sensitivity: "base"});
points.sort((a, b) => collator.compare(String.fromCodePoint(a), String.fromCodePoint(b)) || a - b);

const out = Buffer.alloc(points.length * 3);
let previous = "";
for (let i = 0; i < points.length; i++) {
  const ch = String.fromCodePoint(points[i]);
  const startsGroup = i === 0 || collator.compare(previous, ch) !== 0;
  const value = points[i] | (startsGroup ? 0x800000 : 0);
  out[i * 3] = value;
  out[i * 3 + 1] = value >>> 8;
  out[i * 3 + 2] = value >>> 16;
  previous = ch;
}
process.stdout.write(out);
