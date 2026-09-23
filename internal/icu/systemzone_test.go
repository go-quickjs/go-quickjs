package icu

import "testing"

func TestWindowsZone(t *testing.T) {
	cases := []struct {
		identifier string
		territory  string
		want       string
	}{
		// The default is CLDR's 001 row.
		{"Eastern Standard Time", "", "America/New_York"},
		{"Central Europe Standard Time", "", "Europe/Budapest"},
		// A territory that names a zone of its own is answered with it.
		{"Central Europe Standard Time", "CZ", "Europe/Prague"},
		{"Eastern Standard Time", "CA", "America/Toronto"},
		// One that does not falls back to the default rather than failing.
		{"Eastern Standard Time", "US", "America/New_York"},
		{"Eastern Standard Time", "ZZ", "America/New_York"},
		{"Afghanistan Standard Time", "AF", "Asia/Kabul"},
	}
	for _, c := range cases {
		zone, ok := WindowsZone(c.identifier, c.territory)
		if !ok {
			t.Errorf("WindowsZone(%q, %q): not found",
				c.identifier, c.territory)
			continue
		}
		if zone != c.want {
			t.Errorf("WindowsZone(%q, %q) = %q, want %q",
				c.identifier, c.territory, zone, c.want)
		}
	}

	if zone, ok := WindowsZone("Not A Windows Zone", ""); ok {
		t.Errorf("an identifier that is not one was answered with %q", zone)
	}
}

// The table and the zone archive are separate releases, so a name in one that
// the other does not have would be given out and then fail to load.
func TestWindowsZonesAreInTheArchive(t *testing.T) {
	seen := 0
	check := func(identifier, zone string) {
		t.Helper()
		seen++
		if _, ok := CanonicalZone(zone); !ok {
			t.Errorf("%s maps to %q, which the zone data does not have",
				identifier, zone)
			return
		}
		if _, err := LoadLocation(zone); err != nil {
			t.Errorf("%s maps to %q, which does not load: %v",
				identifier, zone, err)
		}
	}
	for identifier, zone := range windowsZoneDefaults {
		check(identifier, zone)
	}
	for key, zone := range windowsZoneTerritories {
		check(key, zone)
	}
	if seen == 0 {
		t.Fatal("the Windows zone table is empty")
	}
	t.Logf("checked %d Windows zone mappings", seen)
}

// SystemZone answers with a name the database has, or with nothing. It must
// never answer with an offset, which is what ECMA-402 does not accept as the
// resolved time zone.
func TestSystemZoneIsANameOrNothing(t *testing.T) {
	zone := SystemZone()
	if zone == "" {
		t.Skip("the system does not say which zone it is set to")
	}
	if _, ok := CanonicalZone(zone); !ok {
		t.Errorf("SystemZone() = %q, which the zone data does not have", zone)
	}
	if zone[0] == '+' || zone[0] == '-' {
		t.Errorf("SystemZone() = %q, which is an offset rather than a name",
			zone)
	}
}
