package icu

import (
	_ "embed"
	"errors"
	"fmt"
	"time"
)

// zoneinfoData is IANA tzdata 2026c, the same version used by the Node/ICU
// release that generated the accompanying CLDR tables.
//
//go:embed zoneinfo.zip
var zoneinfoData []byte

// LoadLocation loads a named zone from the database paired with the generated
// ICU data. Keeping the arithmetic and names on the same tzdata release avoids
// host-specific results when an operating system's zone files lag behind ICU.
func LoadLocation(name string) (*time.Location, error) {
	if name == "UTC" {
		return time.UTC, nil
	}
	data, err := embeddedTZData(name)
	if err != nil {
		return nil, err
	}
	return time.LoadLocationFromTZData(name, data)
}

var errUnknownTimeZone = errors.New("unknown time zone in embedded tzdata")

// embeddedTZData reads one stored file from Go's zoneinfo.zip layout without
// materializing the archive directory. The standard archive is deliberately
// uncompressed because each TZif record is small and already compact.
func embeddedTZData(name string) ([]byte, error) {
	const (
		endHeader     = 0x06054b50
		centralHeader = 0x02014b50
		localHeader   = 0x04034b50
		endSize       = 22
	)
	data := zoneinfoData
	if len(data) < endSize {
		return nil, errors.New("corrupt embedded tzdata")
	}
	end := len(data) - endSize
	if getUint32(data, end) != endHeader {
		return nil, errors.New("corrupt embedded tzdata directory")
	}
	entries := int(getUint16(data, end+10))
	at := int(getUint32(data, end+16))
	for range entries {
		if at < 0 || at+46 > len(data) || getUint32(data, at) != centralHeader {
			return nil, errors.New("corrupt embedded tzdata directory entry")
		}
		method := getUint16(data, at+10)
		size := int(getUint32(data, at+24))
		nameLen := int(getUint16(data, at+28))
		extraLen := int(getUint16(data, at+30))
		commentLen := int(getUint16(data, at+32))
		offset := int(getUint32(data, at+42))
		next := at + 46 + nameLen + extraLen + commentLen
		if nameLen < 0 || next < at || next > len(data) {
			return nil, errors.New("corrupt embedded tzdata entry name")
		}
		if string(data[at+46:at+46+nameLen]) == name {
			if method != 0 {
				return nil, fmt.Errorf("unsupported compression in embedded tzdata for %s", name)
			}
			if offset < 0 || offset+30 > len(data) || getUint32(data, offset) != localHeader {
				return nil, errors.New("corrupt embedded tzdata local header")
			}
			localMethod := getUint16(data, offset+8)
			localNameLen := int(getUint16(data, offset+26))
			localExtraLen := int(getUint16(data, offset+28))
			start := offset + 30 + localNameLen + localExtraLen
			finish := start + size
			if localMethod != method || localNameLen != nameLen || start < offset ||
				finish < start || finish > len(data) ||
				string(data[offset+30:offset+30+localNameLen]) != name {
				return nil, errors.New("corrupt embedded tzdata record")
			}
			return data[start:finish], nil
		}
		at = next
	}
	return nil, fmt.Errorf("%w: %s", errUnknownTimeZone, name)
}

func getUint16(data []byte, at int) uint16 {
	if at < 0 || at+2 > len(data) {
		return 0
	}
	return uint16(data[at]) | uint16(data[at+1])<<8
}

func getUint32(data []byte, at int) uint32 {
	if at < 0 || at+4 > len(data) {
		return 0
	}
	return uint32(data[at]) | uint32(data[at+1])<<8 |
		uint32(data[at+2])<<16 | uint32(data[at+3])<<24
}
