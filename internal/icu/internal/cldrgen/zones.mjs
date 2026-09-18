// zones.mjs writes what the time zones are called, in English.
//
// Usage:
//
//	node internal/icu/internal/cldrgen/zones.mjs > .../zones.json
//
// A zone has two names in each length, one for standard time and one for
// summer time -- Eastern Standard Time and Eastern Daylight Time -- and the
// short one is a set of letters in the Americas and an offset from Greenwich
// almost everywhere else. Which of the two applies to an instant is something
// the tz database answers, so only the names are read here.
//
// English only: the names of four hundred zones in four hundred languages is
// several megabytes, and a name in the wrong language is worse than an offset
// in the right one.

const WINTER = new Date(Date.UTC(2024, 0, 15, 12, 0, 0));
const SUMMER = new Date(Date.UTC(2024, 6, 15, 12, 0, 0));

const zones = Intl.supportedValuesOf("timeZone");

const nameAt = (zone, when, style) => {
  const parts = new Intl.DateTimeFormat("en", {
    timeZone: zone, timeZoneName: style, hour: "numeric",
  }).formatToParts(when);
  const found = parts.find(p => p.type === "timeZoneName");
  return found ? found.value : "";
};

// Which name applies is decided by the offset the zone is on at the time, not
// by the season: south of the equator the summer is in January, and a zone may
// have changed its mind about daylight saving altogether.
const offsetAt = (zone, when) => {
  const shown = nameAt(zone, when, "shortOffset");
  const found = /^GMT([+-])(\d{1,2})(?::(\d{2}))?$/.exec(shown);
  if (!found) return 0;
  const sign = found[1] === "-" ? -1 : 1;
  return sign * (Number(found[2]) * 60 + Number(found[3] || 0));
};

const out = {};
for (const zone of zones) {
  const names = {};
  for (const when of [WINTER, SUMMER]) {
    const offset = offsetAt(zone, when);
    if (offset in names) continue;
    const short = nameAt(zone, when, "short");
    const long = nameAt(zone, when, "long");
    // A name that is only the offset says nothing a clock cannot work out.
    if (/^GMT([+-]|$)/.test(short) && /^GMT([+-]|$)/.test(long)) continue;
    names[offset] = {short, long};
  }
  if (Object.keys(names).length > 0) out[zone] = names;
}

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
  names: out,
  aliases,
}, null, 1) + "\n");
