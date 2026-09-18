// collation.mjs reads the order that text sorts in.
//
// Usage:
//
//	node internal/icu/internal/cldrgen/collation.mjs > .../collation.json
//
// Sorting text is not comparing characters: a language puts its letters in an
// order of its own, an accent counts for less than a letter, and case counts
// for less than an accent. The Unicode algorithm says this as three weights
// per character -- the letter, the accent, the case -- and compares two
// strings a whole level at a time.
//
// Those weights are not published in a form a program can ask for, but they
// can be recovered from the order itself: sorting the characters with accents
// and case ignored gives the first level, sorting each group of those with
// only case ignored gives the second, and sorting what is left gives the
// third. What comes out is checked against the collator it came from.

const LIMIT = 0x30000; // through the third plane, which is where text lives

const points = [];
for (let cp = 1; cp < LIMIT; cp++) {
  if (cp >= 0xd800 && cp <= 0xdfff) continue; // half of a pair is not a character
  points.push(cp);
}

const char = (cp) => String.fromCodePoint(cp);

// The three collators the levels are read with.
const byLetter = new Intl.Collator("en", {sensitivity: "base"});
const byAccent = new Intl.Collator("en", {sensitivity: "accent"});
const byAll = new Intl.Collator("en", {sensitivity: "variant", caseFirst: "false"});

// A character the collator pays no attention to at all: a control, a joiner, a
// mark that is not a letter of its own. Comparing it with the empty string
// says so.
const ignorable = new Set();
for (const cp of points) {
  if (byAll.compare(char(cp), "") === 0) ignorable.add(cp);
}

// A character that counts for nothing as a letter but does count as an accent:
// the combining marks. They have to be told apart from the letters, or a
// decomposed é would sort as an e followed by something before every letter,
// rather than as an accented e. Adding one to a letter and asking whether the
// letter changed is what says so.
const markOnly = new Set();
for (const cp of points) {
  if (ignorable.has(cp)) continue;
  const c = char(cp);
  if (byLetter.compare("a" + c, "a") === 0 && byAccent.compare("a" + c, "a") !== 0) {
    markOnly.add(cp);
  }
}

const sortable = points.filter(cp => !ignorable.has(cp) && !markOnly.has(cp));
console.error("characters:", sortable.length, "marks:", markOnly.size,
              "ignored:", ignorable.size);

// weightsIn assigns a weight to each member of a group, counting up each time
// the collator says two of them differ.
function weightsIn(group, collator) {
  const sorted = [...group].sort((a, b) => {
    const c = collator.compare(char(a), char(b));
    return c !== 0 ? c : a - b;
  });
  const weights = new Map();
  const groups = [];
  let weight = 0;
  let current = [];
  for (let i = 0; i < sorted.length; i++) {
    if (i > 0 && collator.compare(char(sorted[i - 1]), char(sorted[i])) !== 0) {
      weight++;
      groups.push(current);
      current = [];
    }
    weights.set(sorted[i], weight);
    current.push(sorted[i]);
  }
  if (current.length > 0) groups.push(current);
  return {weights, groups};
}

// The first level: what letter this is.
const primary = weightsIn(sortable, byLetter);
console.error("letters:", primary.groups.length);

// The second: which accent, within one letter.
const secondary = new Map();
for (const group of primary.groups) {
  const {weights} = weightsIn(group, byAccent);
  for (const [cp, w] of weights) secondary.set(cp, w);
}

// The third: which case, within one accented letter.
const tertiary = new Map();
for (const group of primary.groups) {
  const bySecondary = new Map();
  for (const cp of group) {
    const key = secondary.get(cp);
    if (!bySecondary.has(key)) bySecondary.set(key, []);
    bySecondary.get(key).push(cp);
  }
  for (const members of bySecondary.values()) {
    const {weights} = weightsIn(members, byAll);
    for (const [cp, w] of weights) tertiary.set(cp, w);
  }
}

// A character that sorts as several: œ sorts as oe, ß as ss, ½ as 1⁄2. The
// Unicode algorithm calls these expansions, and without them such a character
// sorts as a letter of its own, in the wrong place.
//
// Most are found by decomposing the character; the ligatures are not
// decomposable and are looked for among the pairs of letters instead.
const expansions = {};
const alphabet = [];
for (let cp = 0x41; cp <= 0x5a; cp++) alphabet.push(String.fromCodePoint(cp));
for (let cp = 0x61; cp <= 0x7a; cp++) alphabet.push(String.fromCodePoint(cp));

const lower = alphabet.filter(ch => ch === ch.toLowerCase());
const upper = alphabet.filter(ch => ch === ch.toUpperCase());

for (const cp of sortable) {
  if (cp < 0xa0) continue;
  const c = char(cp);
  const decomposed = c.normalize("NFKD");
  if (decomposed.length > 1 && decomposed !== c && byLetter.compare(c, decomposed) === 0) {
    expansions[cp] = decomposed;
    continue;
  }
  // A ligature: two letters written as one, which no decomposition says. The
  // pair is looked for in the case the character itself is written in, so
  // that œ is oe rather than OE and the case level stays right.
  if (cp > 0x2fff || !/\p{L}/u.test(c) || decomposed.length > 1) continue;
  const isUpper = c === c.toUpperCase() && c !== c.toLowerCase();
  const first = isUpper ? upper : lower;
  const second = isUpper ? upper : lower;
  let found = null;
  for (const a of first) {
    if (found) break;
    for (const b of second) {
      if (byLetter.compare(c, a + b) === 0) {
        found = a + b;
        break;
      }
    }
  }
  if (found) expansions[cp] = found;
}
console.error("expansions:", Object.keys(expansions).length);

// What each accent counts for, among the accents: they are ordered by what
// they do to a letter.
const marks = [...markOnly].sort((a, b) => {
  const c = byAccent.compare("a" + char(a), "a" + char(b));
  return c !== 0 ? c : a - b;
});
const markWeights = new Map();
let markWeight = 0;
for (let i = 0; i < marks.length; i++) {
  if (i > 0 && byAccent.compare("a" + char(marks[i - 1]), "a" + char(marks[i])) !== 0) {
    markWeight++;
  }
  markWeights.set(marks[i], markWeight + 1); // never zero: zero is no accent
}

// Which of those are canonical: ä is an a with an accent however it is
// written, and no language can say otherwise. The rest -- œ, ﬁ, ½ -- are
// compatibility spellings, which a language may treat as letters of their own.
const canonical = {};
for (const cp of Object.keys(expansions)) {
  const c = char(Number(cp));
  if (c.normalize("NFD") !== c) canonical[cp] = true;
}

process.stdout.write(JSON.stringify({
  icu: process.versions.icu,
  limit: LIMIT,
  canonical,
  ignorable: [...ignorable],
  // The accents, which count for nothing as letters.
  marks: marks.map(cp => [cp, markWeights.get(cp)]),
  // Each character as its three weights, in code point order.
  weights: sortable.map(cp => [cp, primary.weights.get(cp), secondary.get(cp),
                               tertiary.get(cp)]),
  expansions,
}) + "\n");
