package icu

import "sync"

// What the machine's time zone is called.
//
// ECMA-402 answers with the name the IANA database uses -- America/New_York --
// and a Unix machine already says it that way, through TZ or the file that
// /etc/localtime points at. Windows names its zones its own way, "Eastern
// Standard Time", so that name has to be translated before it can be given
// out. CLDR publishes the mapping and ICU uses it, which is the point: Node
// and this engine on one machine should call the zone the same thing.

// SystemZone is the database name of the zone the machine is set to. It is
// empty when the platform does not say, or names a zone the database does not
// have, which leaves the caller to fall back to the offset.
func SystemZone() string {
	systemZoneOnce.Do(func() { systemZoneName = systemZone() })
	return systemZoneName
}

var (
	systemZoneOnce sync.Once
	systemZoneName string
)

// WindowsZone is the database name for a Windows time-zone identifier as it is
// used in a territory. One identifier can stand for several zones -- "Central
// Europe Standard Time" is Europe/Budapest but Europe/Prague in Czechia -- so
// the territory decides between them. It is a two-letter region code, and may
// be empty; a territory that names no zone of its own takes the default.
func WindowsZone(identifier, territory string) (string, bool) {
	if territory != "" {
		if zone, ok := windowsZoneTerritories[identifier+"\x00"+territory]; ok {
			return zone, true
		}
	}
	zone, ok := windowsZoneDefaults[identifier]
	return zone, ok
}
