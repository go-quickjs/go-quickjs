// tailoring.mjs reads where a language puts its letters, when that is not
// where the Unicode root order puts them.
//
// Usage:
//
//	node internal/icu/internal/cldrgen/tailoring.mjs > .../tailoring.json
//
// Swedish sorts å, ä and ö after z rather than beside a and o; Turkish keeps
// the dotted and dotless i apart; Estonian puts z between s and t. The root
// order knows none of that, and it is the smaller half of what a collator
// does, so what is recorded here is the difference: which letters a language
// moves, and what it puts them after.

import fs from "fs";
import path from "path";
import {fileURLToPath} from "url";

const here = path.dirname(fileURLToPath(import.meta.url));
const {locales} = JSON.parse(fs.readFileSync(path.join(here, "locales.json"), "utf8"));

// The characters that sort as several -- œ as oe, ä as a and an accent -- read
// from what collation.mjs found. A language that treats one of them as its own
// letter needs to say so; one that does not needs it left alone, so that it
// goes where what it is made of goes.
const {expansions, canonical} =
  JSON.parse(fs.readFileSync(path.join(here, "collation.json"), "utf8"));
const canonicalOf = new Map();
for (const [cp, into] of Object.entries(expansions)) {
  if (canonical[cp]) canonicalOf.set(String.fromCodePoint(Number(cp)), into);
}
const expansionOf = new Map();
for (const [cp, into] of Object.entries(expansions)) {
  // A canonical spelling is the same letter however it is written, so a
  // language that moves the letter moves it either way; only the compatibility
  // ones are in question here.
  if (canonical[cp]) continue;
  expansionOf.set(String.fromCodePoint(Number(cp)), into);
}

// The letters worth asking about: the Latin ones a European language may use,
// the Greek and Cyrillic alphabets, and whatever letters the language writes
// its own month and day names in.
const alphabet = () => {
  const out = new Set();
  const add = (from, to) => {
    for (let cp = from; cp <= to; cp++) out.add(String.fromCodePoint(cp));
  };
  add(0x41, 0x5a);    // A-Z
  add(0x61, 0x7a);    // a-z
  add(0xc0, 0x24f);   // Latin-1 Supplement through Latin Extended-B
  add(0x370, 0x3ff);  // Greek
  add(0x400, 0x4ff);  // Cyrillic
  return out;
};

const root = new Intl.Collator("en", {sensitivity: "base"});

const out = {};
for (const locale of locales) {
  let collator;
  try {
    collator = new Intl.Collator(locale, {sensitivity: "base"});
  } catch (e) { continue; }

  const letters = alphabet();
  const ownLetters = new Set();
  // The letters this language writes its own names in, which is how a script
  // beyond the ones above gets looked at.
  for (const width of ["long", "short"]) {
    for (let month = 0; month < 12; month++) {
      const name = new Intl.DateTimeFormat(locale, {month: width, timeZone: "UTC"})
        .format(new Date(Date.UTC(2024, month, 15)));
      for (const ch of name) { letters.add(ch); ownLetters.add(ch); }
    }
    for (let day = 0; day < 7; day++) {
      const name = new Intl.DateTimeFormat(locale, {weekday: width, timeZone: "UTC"})
        .format(new Date(Date.UTC(2024, 0, 7 + day)));
      for (const ch of name) { letters.add(ch); ownLetters.add(ch); }
    }
  }

  // The letters an alphabet is written in. Chinese and Japanese order their
  // characters by sound or by stroke rather than by letter, which is a whole
  // ordering rather than a handful of moves, and is read separately.
  const candidates = [...letters].filter(ch => {
    if (root.compare(ch, "") === 0) return false;
    const cp = ch.codePointAt(0);
    if (cp >= 0x2e80 && cp <= 0x9fff) return false;   // Han and its radicals
    if (cp >= 0xf900 && cp <= 0xfaff) return false;   // Han compatibility
    if (cp >= 0x3040 && cp <= 0x30ff) return false;   // kana
    if (cp >= 0xac00 && cp <= 0xd7af) return false;   // Hangul syllables
    if (cp >= 0x20000) return false;                  // the Han planes
    return true;
  });
  // The letters this language has, which are not always the letters the root
  // order has: Azerbaijani writes I as the capital of the dotless ı and İ as
  // the capital of i, splitting in two what the root order treats as one
  // letter. So the characters are gathered into the groups this language says
  // are the same letter, and a group moves as a whole.
  const groupsOf = (comparator) => {
    const sorted = [...candidates].sort((a, b) => {
      const c = comparator.compare(a, b);
      return c !== 0 ? c : a.codePointAt(0) - b.codePointAt(0);
    });
    const groups = [];
    for (const ch of sorted) {
      const last = groups[groups.length - 1];
      if (last && comparator.compare(last[0], ch) === 0) last.push(ch);
      else groups.push([ch]);
    }
    return groups;
  };

  const rootGroups = groupsOf(root);
  const localeGroups = groupsOf(collator);

  // Where the root order puts each letter, by any of its characters.
  const rootPlace = new Map();
  rootGroups.forEach((group, i) => {
    for (const ch of group) rootPlace.set(ch, i);
  });

  if (localeGroups.map(g => g.join("")).join("|") ===
      rootGroups.map(g => g.join("")).join("|")) {
    continue;
  }

  // A letter that the language puts before one the root order puts it after
  // has been moved, and what it was moved to sit after is the last letter that
  // had not been. Every character of a moved letter moves with it, so that the
  // letter's capital and its accents stay with it.
  const moved = [];
  let furthest = -1;
  let anchor = null;
  let since = 0;
  for (const group of localeGroups) {
    const places = group.map(ch => rootPlace.get(ch)).filter(p => p !== undefined);
    const place = places.length > 0 ? Math.min(...places) : -1;
    const inOrder = place > furthest &&
      // A group the language split is not in root order however it looks: its
      // characters belong to different letters there.
      new Set(places).size === 1;
    if (inOrder) {
      furthest = place;
      anchor = group[0];
      since = 0;
      continue;
    }
    if (anchor === null) continue;
    since++;
    for (const ch of group) {
      // A character that sorts as several needs a place of its own only where
      // this language says it is a letter of its own: Swedish ä is a letter,
      // and the œ in a Bulgarian list is still an o and an e.
      const expansion = expansionOf.get(ch);
      if (expansion !== undefined && collator.compare(ch, expansion) === 0) continue;
      moved.push([ch, anchor, since]);
    }
  }
  // A character that is written as a letter and an accent, where the letter
  // moved but it did not: Turkish moves I to sit with the dotless ı, and İ is
  // the capital of i rather than of I, so it needs saying where it goes.
  const movedChars = new Set(moved.map(entry => entry[0]));
  const inLocaleOrder = localeGroups.flat();
  for (let i = 0; i < inLocaleOrder.length; i++) {
    const ch = inLocaleOrder[i];
    if (movedChars.has(ch)) continue;
    const expansion = canonicalOf.get(ch);
    if (expansion === undefined || !movedChars.has(expansion[0])) continue;
    // Where this language puts it: just after whatever comes before it.
    let anchor = null;
    for (let j = i - 1; j >= 0; j--) {
      if (!movedChars.has(inLocaleOrder[j])) {
        anchor = inLocaleOrder[j];
        break;
      }
    }
    if (anchor !== null) moved.push([ch, anchor, 1]);
  }

  // A pair or a triple of letters that the language treats as one letter:
  // Czech sorts "ch" after "h", Hungarian "cs" after "c". It shows in the
  // ordering -- c comes before h, but ch comes after it -- and nothing else
  // in the character weights can say it.
  // The language's own small letters: the ones it writes its names in, and the
  // plain a to z. A digraph is made of the letters a language actually uses,
  // so there is no sense in trying every letter of every script.
  const own = new Set();
  for (const ch of ownLetters) {
    const lower = ch.toLowerCase();
    if (/\p{Ll}/u.test(lower)) own.add(lower);
  }
  for (let cp = 0x61; cp <= 0x7a; cp++) own.add(String.fromCodePoint(cp));
  const small = [...own];
  // The letters a spelling may be anchored to, which is every small letter
  // this language orders rather than only the ones it writes its names in:
  // Danish sorts aa as å, and å is in no Danish month name.
  const anchors = candidates.filter(ch => /\p{Ll}/u.test(ch));
  const contractions = [];
  for (const first of small) {
    for (const second of small) {
      const pair = first + second;
      // The pair sorts somewhere the letters themselves do not put it: after
      // a letter that its first letter comes before.
      if (!small.some(other =>
            collator.compare(first, other) < 0 && collator.compare(pair, other) > 0)) {
        continue;
      }
      // What it sits immediately after: the letter it is written as, where the
      // language has one -- Danish aa is å -- and otherwise the last letter it
      // beats.
      const same = anchors.find(other => collator.compare(pair, other) === 0);
      const after = same || small.filter(other => collator.compare(pair, other) > 0)
        .sort(collator.compare).pop();
      if (after) contractions.push([pair, after]);
    }
  }
  // And the three-letter ones, looked for only after a pair that was found.
  for (const [pair] of [...contractions]) {
    for (const third of small) {
      const triple = pair + third;
      if (!small.some(other =>
            collator.compare(pair, other) < 0 && collator.compare(triple, other) > 0)) {
        continue;
      }
      const after = small.filter(other => collator.compare(triple, other) > 0)
        .sort(collator.compare).pop();
      if (after) contractions.push([triple, after]);
    }
  }

  // Two things a language may say about sorting besides where its letters go.
  const plain = new Intl.Collator(locale);
  // Whether a capital comes before its small letter, which Danish says and
  // most languages do not.
  const upperFirst = plain.compare("a", "A") > 0;
  // Whether punctuation counts as a letter or is passed over until everything
  // else has been compared: "ab" before "a-c" means it is passed over.
  const shifted = plain.compare("ab", "a-c") < 0;
  if (moved.length > 0 || contractions.length > 0 || upperFirst || shifted) {
    out[locale] = {moved, contractions, upperFirst, shifted};
  }
}

process.stdout.write(JSON.stringify({icu: process.versions.icu, tailoring: out}) + "\n");
