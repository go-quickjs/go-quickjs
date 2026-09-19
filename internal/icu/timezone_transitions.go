package icu

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// TimeZone retains the transition-related information that time.Location
// deliberately keeps opaque. It is immutable and safe for concurrent use.
type TimeZone struct {
	name        string
	location    *time.Location
	offsets     []int
	transitions []int64
}

// ZoneOffset describes the rule in effect at an instant.
type ZoneOffset struct {
	Abbreviation  string
	OffsetSeconds int
	IsDST         bool
}

var loadedTimeZones sync.Map

// LoadTimeZone loads a named zone and the TZif metadata needed by Temporal.
func LoadTimeZone(name string) (*TimeZone, error) {
	if cached, ok := loadedTimeZones.Load(name); ok {
		return cached.(*TimeZone), nil
	}
	data, err := embeddedTZData(name)
	if err != nil {
		return nil, err
	}
	parsed, err := parseTZif(data)
	if err != nil {
		return nil, fmt.Errorf("parse embedded tzdata for %s: %w", name, err)
	}
	location, err := time.LoadLocationFromTZData(name, data)
	if err != nil {
		return nil, err
	}
	parsed.transitions = offsetTransitions(location, parsed.transitions)
	zone := &TimeZone{
		name:        name,
		location:    location,
		offsets:     parsed.offsets,
		transitions: parsed.transitions,
	}
	actual, _ := loadedTimeZones.LoadOrStore(name, zone)
	return actual.(*TimeZone), nil
}

// Name returns the IANA time-zone identifier used to load the zone.
func (z *TimeZone) Name() string {
	return z.name
}

// Location returns the matching Go location. Callers that need Temporal's
// inverse local-time semantics should use PossibleInstants instead.
func (z *TimeZone) Location() *time.Location {
	return z.location
}

// OffsetAt returns the time-zone rule in effect at epochSeconds.
func (z *TimeZone) OffsetAt(epochSeconds int64) ZoneOffset {
	instant := time.Unix(epochSeconds, 0).In(z.location)
	name, offset := instant.Zone()
	return ZoneOffset{
		Abbreviation:  name,
		OffsetSeconds: offset,
		IsDST:         instant.IsDST(),
	}
}

// PossibleInstants returns the UTC epoch seconds whose local representation is
// localEpochSeconds. The input encodes local date and time fields as though
// they were UTC. Results are ordered from earlier to later. A skipped local
// time has no results and a repeated local time has two.
func (z *TimeZone) PossibleInstants(localEpochSeconds int64) []int64 {
	offsets := make([]int, 0, len(z.offsets)+3)
	offsets = append(offsets, z.offsets...)

	// POSIX rules after the final explicit TZif transition can theoretically
	// introduce a type not present in the transition table. Probe both seasons
	// so those offsets are candidates as well.
	const seasonalProbe = int64(183 * 24 * 60 * 60)
	for _, probe := range []int64{
		localEpochSeconds,
		saturatingAdd(localEpochSeconds, -seasonalProbe),
		saturatingAdd(localEpochSeconds, seasonalProbe),
	} {
		offsets = appendUniqueInt(offsets, z.OffsetAt(probe).OffsetSeconds)
	}

	instants := make([]int64, 0, 2)
	for _, offset := range offsets {
		candidate, ok := subtractOffset(localEpochSeconds, offset)
		if !ok || z.OffsetAt(candidate).OffsetSeconds != offset {
			continue
		}
		if got, ok := addOffset(candidate, offset); !ok || got != localEpochSeconds {
			continue
		}
		instants = appendUniqueInt64(instants, candidate)
	}
	sort.Slice(instants, func(i, j int) bool { return instants[i] < instants[j] })
	return instants
}

// NextTransition returns the first UTC transition strictly after epochSeconds.
func (z *TimeZone) NextTransition(epochSeconds int64) (int64, bool) {
	index := sort.Search(len(z.transitions), func(i int) bool {
		return z.transitions[i] > epochSeconds
	})
	if index < len(z.transitions) {
		return z.transitions[index], true
	}
	if epochSeconds == int64(^uint64(0)>>1) {
		return 0, false
	}
	probe := epochSeconds
	currentOffset := z.OffsetAt(probe).OffsetSeconds
	for {
		_, end := time.Unix(probe, 0).In(z.location).ZoneBounds()
		if end.IsZero() {
			return 0, false
		}
		next := end.Unix()
		if next <= probe {
			return 0, false
		}
		if z.OffsetAt(next).OffsetSeconds != currentOffset {
			return next, true
		}
		probe = next
	}
}

// PreviousTransition returns the last UTC transition strictly before
// epochSeconds.
func (z *TimeZone) PreviousTransition(epochSeconds int64) (int64, bool) {
	const minInt64 = -int64(^uint64(0)>>1) - 1
	if epochSeconds == minInt64 {
		return 0, false
	}
	if len(z.transitions) > 0 && epochSeconds <= z.transitions[len(z.transitions)-1] {
		index := sort.Search(len(z.transitions), func(i int) bool {
			return z.transitions[i] >= epochSeconds
		})
		if index == 0 {
			return 0, false
		}
		return z.transitions[index-1], true
	}
	probe := epochSeconds
	start, _ := time.Unix(probe, 0).In(z.location).ZoneBounds()
	if !start.IsZero() && start.Unix() == epochSeconds {
		probe--
	}
	currentOffset := z.OffsetAt(probe).OffsetSeconds
	for {
		start, _ = time.Unix(probe, 0).In(z.location).ZoneBounds()
		if start.IsZero() {
			return 0, false
		}
		previous := start.Unix()
		if previous >= epochSeconds || previous == minInt64 {
			return 0, false
		}
		if z.OffsetAt(previous-1).OffsetSeconds != currentOffset {
			return previous, true
		}
		probe = previous - 1
	}
}

func offsetTransitions(location *time.Location, transitions []int64) []int64 {
	const minInt64 = -int64(^uint64(0)>>1) - 1
	out := transitions[:0]
	for _, transition := range transitions {
		if transition == minInt64 {
			continue
		}
		_, before := time.Unix(transition-1, 0).In(location).Zone()
		_, after := time.Unix(transition, 0).In(location).Zone()
		if before != after {
			out = append(out, transition)
		}
	}
	return out
}

type tzifData struct {
	offsets     []int
	transitions []int64
}

type tzifCounts struct {
	utcLocal int
	stdWall  int
	leap     int
	times    int
	zones    int
	chars    int
}

var errMalformedTZif = errors.New("malformed TZif data")

func parseTZif(data []byte) (tzifData, error) {
	version, counts, body, err := parseTZifHeader(data)
	if err != nil {
		return tzifData{}, err
	}
	timeSize := 4
	if version > 1 {
		blockSize, ok := tzifBlockSize(counts, 4)
		if !ok || blockSize > len(body) {
			return tzifData{}, errMalformedTZif
		}
		version, counts, body, err = parseTZifHeader(body[blockSize:])
		if err != nil || version < 2 {
			return tzifData{}, errMalformedTZif
		}
		timeSize = 8
	}

	blockSize, ok := tzifBlockSize(counts, timeSize)
	if !ok || blockSize > len(body) || counts.zones == 0 {
		return tzifData{}, errMalformedTZif
	}
	body = body[:blockSize]
	timesSize := counts.times * timeSize
	indicesAt := timesSize
	typesAt := indicesAt + counts.times

	result := tzifData{
		offsets:     make([]int, 0, counts.zones),
		transitions: make([]int64, counts.times),
	}
	for i := range counts.times {
		at := i * timeSize
		if timeSize == 8 {
			result.transitions[i] = int64(binary.BigEndian.Uint64(body[at : at+8]))
		} else {
			result.transitions[i] = int64(int32(binary.BigEndian.Uint32(body[at : at+4])))
		}
		if int(body[indicesAt+i]) >= counts.zones {
			return tzifData{}, errMalformedTZif
		}
	}
	for i := range counts.zones {
		at := typesAt + i*6
		offset := int(int32(binary.BigEndian.Uint32(body[at : at+4])))
		result.offsets = appendUniqueInt(result.offsets, offset)
	}
	return result, nil
}

func parseTZifHeader(data []byte) (version int, counts tzifCounts, body []byte, err error) {
	const headerSize = 44
	if len(data) < headerSize || string(data[:4]) != "TZif" {
		return 0, tzifCounts{}, nil, errMalformedTZif
	}
	switch data[4] {
	case 0:
		version = 1
	case '2':
		version = 2
	case '3':
		version = 3
	default:
		return 0, tzifCounts{}, nil, errMalformedTZif
	}
	values := []*int{
		&counts.utcLocal,
		&counts.stdWall,
		&counts.leap,
		&counts.times,
		&counts.zones,
		&counts.chars,
	}
	for i, target := range values {
		value := binary.BigEndian.Uint32(data[20+i*4 : 24+i*4])
		converted := int(value)
		if uint32(converted) != value {
			return 0, tzifCounts{}, nil, errMalformedTZif
		}
		*target = converted
	}
	return version, counts, data[headerSize:], nil
}

func tzifBlockSize(c tzifCounts, timeSize int) (int, bool) {
	parts := [][2]int{
		{c.times, timeSize},
		{c.times, 1},
		{c.zones, 6},
		{c.chars, 1},
		{c.leap, timeSize + 4},
		{c.stdWall, 1},
		{c.utcLocal, 1},
	}
	total := 0
	for _, part := range parts {
		if part[0] < 0 || part[1] < 0 || (part[0] != 0 && part[1] > int(^uint(0)>>1)/part[0]) {
			return 0, false
		}
		size := part[0] * part[1]
		if size > int(^uint(0)>>1)-total {
			return 0, false
		}
		total += size
	}
	return total, true
}

func appendUniqueInt(values []int, value int) []int {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func appendUniqueInt64(values []int64, value int64) []int64 {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func subtractOffset(value int64, offset int) (int64, bool) {
	return addDelta(value, -int64(offset))
}

func addOffset(value int64, offset int) (int64, bool) {
	return addDelta(value, int64(offset))
}

func addDelta(value, delta int64) (int64, bool) {
	const (
		maxInt64 = int64(^uint64(0) >> 1)
		minInt64 = -maxInt64 - 1
	)
	if delta > 0 && value > maxInt64-delta || delta < 0 && value < minInt64-delta {
		return 0, false
	}
	return value + delta, true
}

func saturatingAdd(value, delta int64) int64 {
	const (
		maxInt64 = int64(^uint64(0) >> 1)
		minInt64 = -maxInt64 - 1
	)
	if delta > 0 && value > maxInt64-delta {
		return maxInt64
	}
	if delta < 0 && value < minInt64-delta {
		return minInt64
	}
	return value + delta
}
