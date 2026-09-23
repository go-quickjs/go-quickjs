//go:build windows

package icu

import (
	"strings"
	"syscall"
	"unsafe"
)

const localeNameMaxLength = 85

var getUserDefaultLocaleName = syscall.NewLazyDLL("kernel32.dll").
	NewProc("GetUserDefaultLocaleName")

func systemLocale() string {
	var name [localeNameMaxLength]uint16
	written, _, _ := getUserDefaultLocaleName.Call(
		uintptr(unsafe.Pointer(&name[0])), uintptr(len(name)))
	if written == 0 {
		return ""
	}
	return tagFromWindows(syscall.UTF16ToString(name[:]))
}

const geoNameMaxLength = 10

var getUserDefaultGeoName = syscall.NewLazyDLL("kernel32.dll").
	NewProc("GetUserDefaultGeoName")

// systemZone reads the zone Windows is set to and translates it. The registry
// holds the identifier rather than the database name, and the user's region
// decides between the zones one identifier can stand for.
func systemZone() string {
	identifier := timeZoneKeyName()
	if identifier == "" {
		return ""
	}
	zone, ok := WindowsZone(identifier, systemTerritory())
	if !ok {
		return ""
	}
	// The mapping is CLDR's, and the zone archive this package carries is the
	// matching release, but a zone that has since been renamed is still worth
	// answering under the name the archive knows.
	if canonical, ok := CanonicalZone(zone); ok {
		return canonical
	}
	return zone
}

// timeZoneKeyName is the Windows identifier for the machine's zone, which the
// system keeps in the registry: "Eastern Standard Time".
func timeZoneKeyName() string {
	const path = `SYSTEM\CurrentControlSet\Control\TimeZoneInformation`
	subKey, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return ""
	}
	var key syscall.Handle
	if err := syscall.RegOpenKeyEx(syscall.HKEY_LOCAL_MACHINE, subKey, 0,
		syscall.KEY_READ, &key); err != nil {
		return ""
	}
	defer syscall.RegCloseKey(key)

	name, err := syscall.UTF16PtrFromString("TimeZoneKeyName")
	if err != nil {
		return ""
	}
	// The identifiers are short, and a value that does not fit the buffer is
	// not one of them.
	var buffer [128]uint16
	size := uint32(len(buffer) * 2)
	var kind uint32
	if err := syscall.RegQueryValueEx(key, name, nil, &kind,
		(*byte)(unsafe.Pointer(&buffer[0])), &size); err != nil {
		return ""
	}
	if kind != syscall.REG_SZ || size > uint32(len(buffer)*2) {
		return ""
	}
	return syscall.UTF16ToString(buffer[:size/2])
}

// systemTerritory is the region the user is set to, which is what decides
// between the zones one Windows identifier can stand for. Older Windows has no
// GetUserDefaultGeoName, where the region in the user's locale is the best
// answer there is.
func systemTerritory() string {
	if err := getUserDefaultGeoName.Find(); err == nil {
		var name [geoNameMaxLength]uint16
		written, _, _ := getUserDefaultGeoName.Call(
			uintptr(unsafe.Pointer(&name[0])), uintptr(len(name)))
		if written > 0 {
			// The call answers with a region code for a country and a number
			// for anything else; only the two-letter codes are in the table.
			if region := syscall.UTF16ToString(name[:]); len(region) == 2 {
				return strings.ToUpper(region)
			}
		}
	}
	// The first subtag is the language, which is not a region however much it
	// looks like one, so the search starts after it.
	parts := strings.Split(Environment(), "-")
	if len(parts) > 1 {
		for _, part := range parts[1:] {
			if len(part) == 2 && onlyLetters(part) {
				return strings.ToUpper(part)
			}
		}
	}
	return ""
}
