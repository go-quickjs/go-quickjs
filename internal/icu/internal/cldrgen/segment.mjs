// segment.mjs reads where text may be broken: between characters, between
// words, and between sentences.
//
// Usage:
//
//	node internal/icu/internal/cldrgen/segment.mjs > .../segment.json
//
// The Unicode algorithm sorts every character into a class -- a letter, a
// digit, a mark that joins to what came before, half of a flag -- and then
// says, for each pair of classes, whether a break may fall between them. The
// classes are not published in a form a program can ask for either, so they
// are recovered the same way the collation weights were: by asking the
// segmenter about pairs and grouping the characters that behave alike.
//
// What comes out is a class per character and a table of which pairs break,
// which is the whole of the algorithm apart from a few rules that look further
// than one character. Those are written out in Go, against these classes.

const LIMIT = 0x30000;

// The characters each question is asked with. One from each corner of the
// algorithm: a letter, a digit, a space, the punctuation that joins words, the
// line ends, a mark, a Hangul jamo of each kind, half a flag, an emoji, and the
// joiner that binds emoji together.
const PROBES = ["a", "1", " ", ".", ",", "'", "\r", "\n", "̀", "‍", "א",
                "ः", "؀", "ᄀ", "ᅠ", "ᆨ", "가",
                "각", "\u{1f1e6}", "\u{1f600}", "漢", "ก", "_", "!", "­"];

// The scripts ICU breaks words in with a dictionary rather than by rule,
// because they are written without spaces. A dictionary of Chinese words is
// two megabytes and one of Thai is another hundred kilobytes, which is more
// than a JavaScript engine should carry, so a run of text in one of these
// scripts is left whole and reported as a single word. That is a good deal
// closer to what a reader would call a word than the alternative the rules
// offer, which is to break between every character.
//
// They must be taken out of the measurement as well as out of the rules: the
// dictionary answers differently for one Chinese character than for another,
// and characters that behave alike are what the classes are made of.
const DICTIONARY = [
  ["Han", /\p{Script=Han}/u],
  ["Hiragana", /\p{Script=Hiragana}/u],
  ["Thai", /\p{Script=Thai}/u],
  ["Lao", /\p{Script=Lao}/u],
  ["Khmer", /\p{Script=Khmer}/u],
  ["Myanmar", /\p{Script=Myanmar}/u],
  ["Tai_Tham", /\p{Script=Tai_Tham}/u],
  ["New_Tai_Lue", /\p{Script=New_Tai_Lue}/u],
];

// The classes the dictionary scripts are allowed to join: the marks and the
// joiners, which belong to whatever they follow.
const JOINERS = [0x0300, 0x200d, 0x00ad, 0xfeff];

// A mark is a mark whatever script it is written in: the Thai vowel that sits
// above the letter before it belongs to that letter, not to the dictionary.
const MARK = /\p{Mn}|\p{Mc}|\p{Me}|\p{Cf}/u;

function classesFor(granularity, byDictionary = false) {
  const seg = new Intl.Segmenter("en", {granularity});
  const sentences = granularity === "sentence";
  const breaks = (a, b) => [...seg.segment(a + b)].length > 1;
  // Whether a character joins what is on either side of it, which is what
  // keeps "can't" and "3.14" together and is not a question about a pair: the
  // apostrophe joins letters only when there are letters on both sides.
  const joins = (before, c, after) =>
    [...seg.segment(before + c + after)].length === 1;
  const signature = (c) =>
    PROBES.map(p => (breaks(p, c) ? "1" : "0") + (breaks(c, p) ? "1" : "0")).join("") +
    // What this character does in the middle: an apostrophe joins letters, a
    // comma joins numbers, a full stop joins either.
    (joins("a", c, "a") ? "1" : "0") +
    (joins("1", c, "1") ? "1" : "0") +
    (joins("a", c, "1") ? "1" : "0") +
    (joins("a", c, " ") ? "1" : "0") +
    // And what it does on either side of one, which is what tells a letter
    // from a digit: "a,a" is two words and "1,1" is one.
    (joins(c, ",", c) ? "1" : "0") +
    (joins(c, ".", c) ? "1" : "0") +
    (joins(c, "'", c) ? "1" : "0") +
    // Whether a sentence may end before it, which tells a capital from a
    // small letter: a full stop ends a sentence before Smith and not before
    // smith.
    (breaks("Mr. ", c) ? "1" : "0") +
    // And whether a sentence may still end after it once one has been ended,
    // which tells a space from a comma: a sentence ends after "Stop! " and
    // goes on after "Stop!, ".
    (sentences ? (breaks("!" + c, "a") ? "1" : "0") : "") +
    // And whether a letter of a script that has no capitals stands in the way
    // of the small letter that would have made the full stop an abbreviation.
    (sentences ? (breaks("a." + c, "b") ? "1" : "0") : "") +
    // And whether it is a space or a mark, which read alike everywhere else
    // but differ in whether a bracket after them still ends the sentence.
    (sentences ? (breaks("a!" + c, "'") ? "1" : "0") : "");

  // The characters left to the dictionary, which are measured as one class
  // apiece rather than by how they behave.
  const script = new Map();
  if (byDictionary) {
    for (let cp = 1; cp < LIMIT; cp++) {
      if (cp >= 0xd800 && cp <= 0xdfff) continue;
      const c = String.fromCodePoint(cp);
      if (MARK.test(c)) continue;
      for (const [name, pattern] of DICTIONARY) {
        if (pattern.test(c)) { script.set(cp, name); break; }
      }
    }
  }

  // Which characters behave alike.
  const bySignature = new Map();
  for (let cp = 1; cp < LIMIT; cp++) {
    if (cp >= 0xd800 && cp <= 0xdfff || script.has(cp)) continue;
    const key = signature(String.fromCodePoint(cp));
    if (!bySignature.has(key)) bySignature.set(key, []);
    bySignature.get(key).push(cp);
  }

  // The classes, largest first, so that the one most characters belong to is
  // the one the table can leave out.
  const classes = [...bySignature.values()].sort((a, b) => b.length - a.length);

  // Which pairs may be broken between, asked of one character from each class.
  const table = classes.map(a =>
    classes.map(b => breaks(String.fromCodePoint(a[0]), String.fromCodePoint(b[0])) ? 1 : 0));

  const classOf = new Map();
  classes.forEach((members, index) => {
    for (const cp of members) classOf.set(cp, index);
  });

  // A class for each dictionary script, which runs together with itself and
  // with the marks, and breaks from everything else. These rows are written
  // rather than measured, since what they are here to avoid is the answer the
  // dictionary would give.
  const joining = new Set(JOINERS.map(cp => classOf.get(cp)).filter(i => i !== undefined));
  for (const [name] of DICTIONARY) {
    const members = [...script].filter(([, s]) => s === name).map(([cp]) => cp);
    if (members.length === 0) continue;
    const index = classes.length;
    classes.push(members);
    for (const cp of members) classOf.set(cp, index);
    for (const row of table) row.push(1);
    table.push(classes.map((_, i) => (i === index || joining.has(i) ? 0 : 1)));
    table[index][index] = 0;
  }

  // Where each class begins, as runs of code points.
  const runs = [];
  let start = -1, current = -1, previous = -2;
  for (let cp = 1; cp < LIMIT; cp++) {
    const index = classOf.has(cp) ? classOf.get(cp) : 0;
    if (index !== current || cp !== previous + 1) {
      if (start >= 0 && current !== 0) runs.push([start, previous - start + 1, current]);
      start = cp;
      current = index;
    }
    previous = cp;
  }
  if (start >= 0 && current !== 0) runs.push([start, previous - start + 1, current]);

  // Which classes are made of letters rather than of spaces and punctuation,
  // which is the one thing about a word a script may ask that is not a
  // question about where it begins. ICU is asked outright.
  // A vote rather than a single answer, since a class gathered by script
  // rather than by behaviour holds the odd radical along with its letters.
  const wordLike = classes.map(members => {
    const step = Math.max(1, Math.floor(members.length / 25));
    let yes = 0, asked = 0;
    for (let k = 0; k < members.length; k += step) {
      const [piece] = [...seg.segment(String.fromCodePoint(members[k]))];
      if (piece && piece.isWordLike) yes++;
      asked++;
    }
    return yes * 2 > asked ? 1 : 0;
  });

  return {
    representatives: classes.map(members => members[0]),
    sizes: classes.map(members => members.length),
    table,
    runs,
    wordLike,
  };
}

// The characters that are emoji in their own right, which the rules about
// joined emoji are written in terms of: a joiner between two of them keeps
// them together.
function pictographic() {
  const seg = new Intl.Segmenter("en", {granularity: "grapheme"});
  const joined = (c) => [...seg.segment("\u{1F600}‍" + c)].length === 1;
  const out = [];
  let start = -1, previous = -2;
  for (let cp = 0xa9; cp < LIMIT; cp++) {
    if (cp >= 0xd800 && cp <= 0xdfff) continue;
    if (!joined(String.fromCodePoint(cp))) continue;
    if (cp !== previous + 1) {
      if (start >= 0) out.push([start, previous - start + 1]);
      start = cp;
    }
    previous = cp;
  }
  if (start >= 0) out.push([start, previous - start + 1]);
  return out;
}

process.stdout.write(JSON.stringify({
  icu: process.versions.icu,
  grapheme: classesFor("grapheme"),
  word: classesFor("word", true),
  sentence: classesFor("sentence"),
  pictographic: pictographic(),
}) + "\n");
