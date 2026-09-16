// Command unicodegen writes the Unicode property tables the \p{...} escapes
// need, from test262's own generated property-escape tests.
//
// The tests are the source rather than the Unicode character database because
// they answer both questions at once and answer them consistently: which
// spellings a property escape may use, and exactly which code points each one
// denotes, at one stated Unicode version. Deriving the first from the second by
// hand is where the alias tables this replaces kept going wrong -- the
// abbreviation is sometimes the longer string, Han against Hani -- and pairing
// data from one Unicode version with a name list from another is how a property
// ends up accepted but empty.
//
// Usage:
//
//	go run ./internal/regexp/internal/unicodegen /path/to/test262 > internal/regexp/unicodetables.go
package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// charRange mirrors the one in the regexp package.
type charRange struct{ lo, hi rune }

// property is one set together with every spelling that names it.
type property struct {
	names  []string
	ranges []charRange
}

var (
	// The escapes a generated test exercises, which is the list of spellings.
	reEscape = regexp.MustCompile(`/\^\\[pP]\{([^}]*)\}\+\$/u`)
	reLone   = regexp.MustCompile(`^\s*(0x[0-9A-Fa-f]+),?\s*$`)
	reRange  = regexp.MustCompile(`^\s*\[(0x[0-9A-Fa-f]+),\s*(0x[0-9A-Fa-f]+)\],?\s*$`)
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: unicodegen <test262-dir>")
		os.Exit(2)
	}
	dir := filepath.Join(os.Args[1], "test", "built-ins", "RegExp",
		"property-escapes", "generated")
	files, err := filepath.Glob(filepath.Join(dir, "*.js"))
	if err != nil || len(files) == 0 {
		fmt.Fprintf(os.Stderr, "no generated tests under %s\n", dir)
		os.Exit(1)
	}
	sort.Strings(files)

	version := ""
	var props []property
	for _, f := range files {
		p, v, err := parse(f)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", f, err)
			os.Exit(1)
		}
		if version == "" {
			version = v
		}
		props = append(props, p)
	}
	emit(os.Stdout, version, props)
}

// parse reads one generated test's matched set and the spellings it covers.
func parse(path string) (property, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return property{}, "", err
	}
	defer f.Close()

	var p property
	version := ""
	seen := map[string]bool{}
	section := "" // "lone", "ranges", or empty outside the matched set
	inMatch := false

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(strings.TrimSpace(line), "Unicode v"):
			version = strings.TrimPrefix(strings.TrimSpace(line), "Unicode v")
		case strings.HasPrefix(line, "const matchSymbols"):
			inMatch, section = true, ""
			continue
		case strings.HasPrefix(line, "const nonMatchSymbols"):
			// The complement is not stored: it is the complement.
			inMatch = false
			continue
		}
		if m := reEscape.FindStringSubmatch(line); m != nil && !seen[m[1]] {
			seen[m[1]] = true
			p.names = append(p.names, m[1])
			continue
		}
		if !inMatch {
			continue
		}
		switch {
		case strings.Contains(line, "loneCodePoints:"):
			section = "lone"
			continue
		case strings.Contains(line, "ranges:"):
			section = "ranges"
			continue
		case strings.HasPrefix(line, "});"):
			inMatch, section = false, ""
			continue
		}
		switch section {
		case "lone":
			if m := reLone.FindStringSubmatch(line); m != nil {
				c := parseRune(m[1])
				p.ranges = append(p.ranges, charRange{c, c})
			}
		case "ranges":
			if m := reRange.FindStringSubmatch(line); m != nil {
				p.ranges = append(p.ranges, charRange{parseRune(m[1]), parseRune(m[2])})
			}
		}
	}
	if err := sc.Err(); err != nil {
		return property{}, "", err
	}
	if len(p.names) == 0 {
		return property{}, "", fmt.Errorf("no property escapes found")
	}
	p.ranges = normalize(p.ranges)
	sort.Strings(p.names)
	return p, version, nil
}

func parseRune(s string) rune {
	n, err := strconv.ParseUint(strings.TrimPrefix(s, "0x"), 16, 32)
	if err != nil {
		panic(err)
	}
	return rune(n)
}

// normalize sorts and coalesces, which the matcher's membership test assumes.
func normalize(in []charRange) []charRange {
	if len(in) == 0 {
		return nil
	}
	sort.Slice(in, func(i, j int) bool { return in[i].lo < in[j].lo })
	out := in[:1]
	for _, r := range in[1:] {
		last := &out[len(out)-1]
		if r.lo <= last.hi+1 {
			if r.hi > last.hi {
				last.hi = r.hi
			}
			continue
		}
		out = append(out, r)
	}
	return out
}

// digits is the alphabet the encoded table uses: 64 characters, none of which
// needs escaping inside a Go string literal.
const digits = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz$_"

// appendVarint writes n as base-32 digits, low bits first, with the top bit of
// each digit marking that another follows.
func appendVarint(sb *strings.Builder, n uint32) {
	for {
		d := n & 31
		n >>= 5
		if n != 0 {
			sb.WriteByte(digits[d|32])
			continue
		}
		sb.WriteByte(digits[d])
		return
	}
}

func emit(out *os.File, version string, props []property) {
	// Each set is encoded once; the spellings that name it share the offset.
	var data strings.Builder
	offsets := map[string]int{}
	type entry struct {
		name string
		off  int
	}
	var entries []entry

	for _, p := range props {
		off := data.Len()
		appendVarint(&data, uint32(len(p.ranges)))
		prev := rune(-1)
		for _, r := range p.ranges {
			appendVarint(&data, uint32(r.lo-prev-1))
			appendVarint(&data, uint32(r.hi-r.lo))
			prev = r.hi
		}
		for _, n := range p.names {
			if _, dup := offsets[n]; dup {
				continue
			}
			offsets[n] = off
			entries = append(entries, entry{n, off})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })

	w := bufio.NewWriter(out)
	defer w.Flush()

	fmt.Fprintf(w, `// Code generated by internal/regexp/internal/unicodegen. DO NOT EDIT.

package regexp

// Unicode property data for the \p{...} escapes, at Unicode %s.
//
// Both halves come from test262's generated property-escape tests: the keys of
// unicodeProperties are exactly the spellings a property escape may use, so a
// name outside the table is a syntax error, and the ranges are exactly what
// each one denotes. Keeping the two together is the point -- a name list from
// one Unicode version paired with data from another gives a property that is
// accepted and empty.
//
// unicodePropertyData encodes every set once, as a count of ranges followed by
// that many (gap, span) pairs, each a base-32 varint written low digit first
// with the top bit of a digit marking that another follows. Spellings that name
// the same set share one offset into it.

// UnicodeVersion is the version the tables were generated from.
const UnicodeVersion = %q

`, version, version)

	fmt.Fprintf(w, "var unicodeProperties = map[string]uint32{\n")
	for _, e := range entries {
		fmt.Fprintf(w, "\t%q: %d,\n", e.name, e.off)
	}
	fmt.Fprintf(w, "}\n\nconst unicodePropertyData = \"\" +\n")

	const width = 100
	s := data.String()
	for i := 0; i < len(s); i += width {
		j := i + width
		if j > len(s) {
			j = len(s)
		}
		sep := " +"
		if j == len(s) {
			sep = ""
		}
		fmt.Fprintf(w, "\t%q%s\n", s[i:j], sep)
	}
}
