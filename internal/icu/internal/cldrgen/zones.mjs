// zones.mjs writes which time zones there are, and the other names each of
// them goes by.
//
// Usage:
//
//	node internal/icu/internal/cldrgen/zones.mjs > .../zones.json
//
// What they are called is read separately, in every language, by
// zonenames.mjs.

const zones = Intl.supportedValuesOf("timeZone");

// The same zone goes by more than one name -- Asia/Kolkata and Asia/Calcutta
// are one place -- and which of them the data is filed under is ICU's choice,
// not the caller's. The other spellings are read from the zone files this
// machine has, and recorded so that either name finds the zone.
import fs from "fs";
import path from "path";

const roots = ["/usr/share/zoneinfo", "/usr/lib/zoneinfo", "/etc/zoneinfo"];
const aliases = {};
const walk = (root, dir) => {
  let entries;
  try {
    entries = fs.readdirSync(path.join(root, dir), {withFileTypes: true});
  } catch (e) { return; }
  for (const entry of entries) {
    const name = dir ? dir + "/" + entry.name : entry.name;
    if (entry.isDirectory()) { walk(root, name); continue; }
    // The files that are not zones: the database's own bookkeeping.
    if (!/^[A-Za-z][A-Za-z0-9_+-]*(\/[A-Za-z0-9_+-]+)*$/.test(name)) continue;
    if (/^(posix|right|posixrules|leapseconds|tzdata\.zi|leap-seconds\.list|iso3166\.tab|zone\.tab|zone1970\.tab)/.test(name)) {
      continue;
    }
    let canonical;
    try {
      canonical = new Intl.DateTimeFormat("en", {timeZone: name}).resolvedOptions().timeZone;
    } catch (e) { continue; }
    if (canonical !== name) aliases[name] = canonical;
  }
};
for (const root of roots) {
  if (fs.existsSync(root)) { walk(root, ""); break; }
}

process.stdout.write(JSON.stringify({
  icu: process.versions.icu,
  zones,
  aliases,
}, null, 1) + "\n");
