// legacyzones.mjs is the subprocess half of legacy Date-name extraction.
// A process has one ICU default locale, so zonenames.mjs starts this helper
// once per locale rather than confusing Intl's explicit locale with the
// locale used by Date.prototype.toString.

import fs from "fs";

const input = JSON.parse(fs.readFileSync(0, "utf8"));

const labelAt = when => {
  const text = new Date(when).toString();
  const at = text.indexOf(" (");
  return at < 0 ? "" : text.slice(at + 2, -1);
};

if (process.argv[2] === "timeline") {
  const FIRST = 0;
  const LAST = 2147483647 * 1000;
  const DAY = 24 * 60 * 60 * 1000;
  const out = {};

  for (const zone of input.zones) {
    process.env.TZ = zone;
    let previous = labelAt(FIRST);
    const names = [previous];
    const changes = [];

    const observe = cursor => {
      const current = labelAt(cursor);
      if (current === previous) return;
      let low = Math.max(FIRST, cursor - DAY), high = cursor;
      while (high-low > 1) {
        const middle = low + Math.floor((high-low) / 2);
        if (labelAt(middle) === previous) low = middle;
        else high = middle;
      }
      if (high % 1000 !== 0) {
        throw new Error(zone + " changes its legacy Date name off a whole second");
      }
      let slot = names.indexOf(current);
      if (slot < 0) {
        slot = names.length;
        names.push(current);
      }
      changes.push(Math.floor(high / 1000));
      previous = current;
    };

    for (let cursor = FIRST + DAY; cursor <= LAST; cursor += DAY) {
      observe(cursor);
    }
    // The signed-32-bit window ends three hours into its last UTC day.
    observe(LAST);
    if (names.length > 2) {
      throw new Error(zone + " has more than two legacy Date names");
    }
    out[zone] = {names, changes};
  }
  process.stdout.write(JSON.stringify(out));
} else if (process.argv[2] === "names") {
  const out = {};
  for (const [zone, samples] of Object.entries(input.samples)) {
    process.env.TZ = zone;
    out[zone] = samples.map(labelAt).join("|");
  }
  process.stdout.write(JSON.stringify(out));
} else {
  throw new Error("usage: legacyzones.mjs timeline|names");
}
