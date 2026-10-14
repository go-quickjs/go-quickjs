package sourcemap

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
)

// find writes what a position maps to, or "-" for nothing.
func find(m *Map, line, col int) string {
	e, ok := m.Find(line, col)
	if !ok {
		return "-"
	}
	return fmt.Sprintf("%s:%d:%d %s", e.Source, e.Line, e.Column, e.Name)
}

// TestFind pins Node's findEntry: a position maps through the last mapping
// at or before it, a line before it included; before the first, or through
// a mapping of a place to nothing, it maps to nothing. Values are base64
// VLQ, signed and relative, a name's index too.
func TestFind(t *testing.T) {
	// Line 0: col 0 -> a.ts 0:0; col 9 -> a.ts 0:9 named "add".
	// Line 1: col 4 maps to nothing; col 8 -> b.ts 2:4 (source +1, line +2,
	// col -5) named "sum" (name +1).
	m, err := Parse([]byte(`{"version":3,"sources":["a.ts","b.ts"],"names":["add","sum"],
		"mappings":"AAAA,SAASA;I,ICELC"}`), "file:///p/dist/x.js.map")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		line, col int
		want      string
	}{
		{0, 0, "file:///p/dist/a.ts:0:0 "},
		{0, 8, "file:///p/dist/a.ts:0:0 "},
		{0, 9, "file:///p/dist/a.ts:0:9 add"},
		{0, 500, "file:///p/dist/a.ts:0:9 add"},
		{1, 3, "file:///p/dist/a.ts:0:9 add"},
		{1, 4, "-"},
		{1, 8, "file:///p/dist/b.ts:2:4 sum"},
		{7, 0, "file:///p/dist/b.ts:2:4 sum"},
	} {
		if got := find(m, c.line, c.col); got != c.want {
			t.Errorf("%d:%d: %q, want %q", c.line, c.col, got, c.want)
		}
	}
	empty, _ := Parse([]byte(`{"version":3,"sources":["a.ts"],"names":[],"mappings":";;IAAA"}`), "file:///x.map")
	if got := find(empty, 1, 0); got != "-" {
		t.Errorf("before the first mapping: %q", got)
	}
}

// TestSources pins how a map's sources are made absolute: after its
// sourceRoot, against the map's own URL, and an absolute path as a file URL.
func TestSources(t *testing.T) {
	for _, c := range []struct {
		root, source, base, want string
	}{
		{"", "../src/app.ts", "file:///proj/dist/app.js.map", "file:///proj/src/app.ts"},
		{"src/", "app.ts", "file:///proj/app.js.map", "file:///proj/src/app.ts"},
		{"", "/abs/app.ts", "file:///proj/app.js.map", "file:///abs/app.ts"},
		{"", "C:\\abs\\app.ts", "file:///proj/app.js.map", "file:///C:/abs/app.ts"},
		{"", "webpack://pkg/app.ts", "file:///proj/app.js.map", "webpack://pkg/app.ts"},
		{"", "app.ts", "/proj/dist/app.js", "/proj/dist/app.ts"},
	} {
		data := fmt.Sprintf(`{"version":3,"sourceRoot":%q,"sources":[%q],"names":[],"mappings":"AAAA"}`, c.root, c.source)
		m, err := Parse([]byte(data), c.base)
		if err != nil {
			t.Fatal(err)
		}
		if e, _ := m.Find(0, 0); e.Source != c.want {
			t.Errorf("%q + %q against %q: %q, want %q", c.root, c.source, c.base, e.Source, c.want)
		}
	}
}

// TestSections pins an index map: each section's mappings start at its
// offset, the first of its lines at its column.
func TestSections(t *testing.T) {
	m, err := Parse([]byte(`{"version":3,"sections":[
		{"offset":{"line":0,"column":0},"map":{"version":3,"sources":["a.ts"],"names":[],"mappings":"AAAA"}},
		{"offset":{"line":2,"column":10},"map":{"version":3,"sources":["b.ts"],"names":[],"mappings":"AACA;IAAA"}}]}`),
		"file:///p/x.map")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		line, col int
		want      string
	}{
		{1, 0, "file:///p/a.ts:0:0 "},
		{2, 9, "file:///p/a.ts:0:0 "},
		{2, 10, "file:///p/b.ts:1:0 "},
		{3, 4, "file:///p/b.ts:1:0 "},
	} {
		if got := find(m, c.line, c.col); got != c.want {
			t.Errorf("%d:%d: %q, want %q", c.line, c.col, got, c.want)
		}
	}
}

// TestBadMaps pins maps that cannot be read: not JSON, or mappings that are
// not base64 VLQ.
func TestBadMaps(t *testing.T) {
	for _, data := range []string{`{`, `{"version":3,"sources":[],"mappings":"A!"}`, `{"version":3,"sources":[],"mappings":"g"}`} {
		if _, err := Parse([]byte(data), "file:///x.map"); err == nil {
			t.Errorf("%s: no error", data)
		}
	}
}

// encodeVLQ is v as base64 VLQ, for any v an int64 holds.
func encodeVLQ(v int64) string {
	u := uint64(v) << 1
	if v < 0 {
		u = uint64(-v)<<1 | 1
	}
	var b strings.Builder
	for {
		d := u & 31
		if u >>= 5; u != 0 {
			d |= 32
		}
		b.WriteByte(base64Digits[d])
		if u == 0 {
			return b.String()
		}
	}
}

// TestValuesAre32Bit pins that a mapping's values, and the positions they
// add up to, are 32-bit signed integers, as a source map's are: past that a
// map is refused rather than wrapped. Shifts of up to 60 bits let thirteen
// digits of 31 decode to 1, and an int of 32 bits lost all but seven.
func TestValuesAre32Bit(t *testing.T) {
	parse := func(mappings string) error {
		_, err := Parse([]byte(`{"version":3,"sources":["a.js"],"mappings":"`+mappings+`"}`), "file:///x.map")
		return err
	}
	for _, v := range []int64{1<<31 - 1, -(1<<31 - 1), 12345} {
		if err := parse(encodeVLQ(v) + "AAA"); err != nil {
			t.Errorf("%d: %v", v, err)
		}
	}
	for _, mappings := range []string{
		encodeVLQ(1<<31) + "AAA",
		encodeVLQ(-(1 << 31) - 1),
		strings.Repeat("/", 12) + "f", // thirteen digits of 31
		encodeVLQ(1<<35) + "AAA",
		// In range alone, past it together: the column adds up.
		encodeVLQ(1<<31-1) + "AAA," + encodeVLQ(1) + "AAA",
		// And the source line does.
		"AA" + encodeVLQ(1<<31-1) + "A,AA" + encodeVLQ(1) + "A",
	} {
		if err := parse(mappings); err == nil {
			t.Errorf("%q: no error", mappings)
		}
	}
	m, err := Parse([]byte(`{"version":3,"sources":["a.js"],"mappings":"`+encodeVLQ(1<<31-1)+`AAA"}`), "file:///x.map")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := find(m, 0, 1<<31-1), "file:///a.js:0:0 "; got != want {
		t.Errorf("at column 2**31-1: %s, want %s", got, want)
	}
	// A section's offset is a line or a column, or the map is refused.
	for _, off := range []string{`{"line":-1,"column":0}`, `{"line":0.5,"column":0}`, `{"line":0,"column":4294967296}`, `{"line":"1","column":0}`} {
		data := `{"version":3,"sections":[{"offset":` + off + `,"map":{"version":3,"sources":["a.js"],"mappings":"AAAA"}}]}`
		if _, err := Parse([]byte(data), "file:///x.map"); err == nil {
			t.Errorf("offset %s: no error", off)
		}
	}
}

// TestDataURL pins the data URLs a map is inline in: application/json,
// base64 or percent-encoded, and nothing else.
func TestDataURL(t *testing.T) {
	json := `{"version":3}`
	for _, c := range []struct {
		url string
		ok  bool
	}{
		{"data:application/json;charset=utf-8;base64," + base64.StdEncoding.EncodeToString([]byte(json)), true},
		{"data:application/json;base64," + base64.StdEncoding.EncodeToString([]byte(json)), true},
		{"data:application/json," + strings.ReplaceAll(json, `"`, "%22"), true},
		{"data:text/plain;base64,e30=", false},
		{"app.js.map", false},
	} {
		got, ok := DataURL(c.url)
		if ok != c.ok || ok && string(got) != json {
			t.Errorf("%s: %q %v", c.url, got, ok)
		}
	}
}
