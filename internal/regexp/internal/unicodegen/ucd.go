package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// The Unicode Character Database files the case tables come from, by version,
// with their SHA-256 sums: the files are fetched from unicode.org, or read
// from a directory that holds them (-ucddata), and either way must be these.
// A test262 that moves to another Unicode version needs that version's sums
// here, which is the point -- the case data and the property data then cannot
// come from two versions.
var ucdSums = map[string]map[string]string{
	"17.0.0": {
		"UnicodeData.txt":   "2e1efc1dcb59c575eedf5ccae60f95229f706ee6d031835247d843c11d96470c",
		"CaseFolding.txt":   "ff8d8fefbf123574205085d6714c36149eb946d717a0c585c27f0f4ef58c4183",
		"SpecialCasing.txt": "efc25faf19de21b92c1194c111c932e03d2a5eaf18194e33f1156e96de4c9588",
	},
}

// ucdProperties are the sets package ucd answers, each by the spelling its
// test262 test is found under, with the name of its table there.
var ucdProperties = []struct{ name, table, doc string }{
	{"ID_Start", "idStart", "ID_Start"},
	{"ID_Continue", "idContinue", "ID_Continue"},
	{"Space_Separator", "spaceSeparator", "General_Category=Space_Separator (Zs)"},
	{"Uppercase_Letter", "uppercaseLetter", "General_Category=Uppercase_Letter (Lu)"},
	{"Cased", "cased", "Cased"},
	{"Case_Ignorable", "caseIgnorable", "Case_Ignorable"},
}

// readUCD returns one of the database's files at version, checked against
// its pinned sum.
func readUCD(dir, version, name string) ([]byte, error) {
	sums, ok := ucdSums[version]
	if !ok {
		return nil, fmt.Errorf("no pinned sums for Unicode %s: add them to ucdSums", version)
	}
	var data []byte
	var err error
	if dir != "" {
		data, err = os.ReadFile(filepath.Join(dir, name))
	} else {
		url := "https://www.unicode.org/Public/" + version + "/ucd/" + name
		var res *http.Response
		if res, err = http.Get(url); err == nil {
			defer res.Body.Close()
			if res.StatusCode != http.StatusOK {
				return nil, fmt.Errorf("%s: %s", url, res.Status)
			}
			data, err = io.ReadAll(res.Body)
		}
	}
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != sums[name] {
		return nil, fmt.Errorf("%s: SHA-256 %s, want %s", name, got, sums[name])
	}
	// Every file but UnicodeData.txt names its version in its first line.
	if name != "UnicodeData.txt" {
		want := "# " + strings.TrimSuffix(name, ".txt") + "-" + version + ".txt"
		if first, _, _ := bytes.Cut(data, []byte("\n")); strings.TrimSpace(string(first)) != want {
			return nil, fmt.Errorf("%s: begins %q, want %q", name, first, want)
		}
	}
	return data, nil
}

// fields splits the database's lines into their semicolon-separated fields,
// comments and blank lines left out.
func fields(data []byte, each func(f []string) error) error {
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line, _, _ := strings.Cut(sc.Text(), "#")
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Split(line, ";")
		for i := range f {
			f[i] = strings.TrimSpace(f[i])
		}
		if err := each(f); err != nil {
			return fmt.Errorf("%q: %v", sc.Text(), err)
		}
	}
	return sc.Err()
}

func hexRune(s string) (rune, error) {
	n, err := strconv.ParseUint(s, 16, 32)
	return rune(n), err
}

func hexRunes(s string) ([]rune, error) {
	var out []rune
	for _, h := range strings.Fields(s) {
		r, err := hexRune(h)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// caseData is what the database says of a version's case.
type caseData struct {
	upper, lower map[rune]rune   // UnicodeData's simple mappings
	fold         map[rune]rune   // CaseFolding's common and simple foldings
	fullUpper    map[rune][]rune // SpecialCasing's unconditional mappings
	fullLower    map[rune][]rune // where they are not the simple one
}

func readCaseData(dir, version string) (*caseData, error) {
	d := &caseData{
		upper: map[rune]rune{}, lower: map[rune]rune{},
		fold: map[rune]rune{}, fullUpper: map[rune][]rune{}, fullLower: map[rune][]rune{},
	}
	ud, err := readUCD(dir, version, "UnicodeData.txt")
	if err != nil {
		return nil, err
	}
	err = fields(ud, func(f []string) error {
		if len(f) != 15 {
			return fmt.Errorf("%d fields", len(f))
		}
		r, err := hexRune(f[0])
		if err != nil {
			return err
		}
		// A range's first and last lines name no mapping, and nothing
		// between them has one.
		for i, m := range map[int]map[rune]rune{12: d.upper, 13: d.lower} {
			if f[i] == "" {
				continue
			}
			c, err := hexRune(f[i])
			if err != nil {
				return err
			}
			m[r] = c
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("UnicodeData.txt: %v", err)
	}

	cf, err := readUCD(dir, version, "CaseFolding.txt")
	if err != nil {
		return nil, err
	}
	err = fields(cf, func(f []string) error {
		if f[1] != "C" && f[1] != "S" {
			return nil
		}
		r, err := hexRune(f[0])
		if err != nil {
			return err
		}
		c, err := hexRune(f[2])
		if err != nil {
			return err
		}
		d.fold[r] = c
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("CaseFolding.txt: %v", err)
	}

	sc, err := readUCD(dir, version, "SpecialCasing.txt")
	if err != nil {
		return nil, err
	}
	err = fields(sc, func(f []string) error {
		// <code>; <lower>; <title>; <upper>; (<condition_list>;)?
		if len(f) > 5 && f[4] != "" {
			// A language's or a context's rule, which is not the
			// language-independent mapping.
			return nil
		}
		r, err := hexRune(f[0])
		if err != nil {
			return err
		}
		lower, err := hexRunes(f[1])
		if err != nil {
			return err
		}
		upper, err := hexRunes(f[3])
		if err != nil {
			return err
		}
		if !isSimple(lower, r, d.lower) {
			d.fullLower[r] = lower
		}
		if !isSimple(upper, r, d.upper) {
			d.fullUpper[r] = upper
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("SpecialCasing.txt: %v", err)
	}
	return d, nil
}

// isSimple reports whether a full mapping of r is what its simple mapping
// gives.
func isSimple(full []rune, r rune, simple map[rune]rune) bool {
	s, ok := simple[r]
	if !ok {
		s = r
	}
	return len(full) == 1 && full[0] == s
}

// simpleFolds is the next member of each code point's case-folding orbit, as
// unicode.SimpleFold answers it: the orbit is every code point with the same
// simple folding, the folding included, in ascending order and round again.
func (d *caseData) simpleFolds() map[rune]rune {
	classes := map[rune][]rune{}
	add := func(r rune) {
		f, ok := d.fold[r]
		if !ok {
			f = r
		}
		classes[f] = append(classes[f], r)
	}
	seen := map[rune]bool{}
	for _, m := range []map[rune]rune{d.fold, d.upper, d.lower} {
		for r, c := range m {
			for _, x := range []rune{r, c} {
				if !seen[x] {
					seen[x] = true
					add(x)
				}
			}
		}
	}
	next := map[rune]rune{}
	for _, class := range classes {
		sort.Slice(class, func(i, j int) bool { return class[i] < class[j] })
		for i, r := range class {
			next[r] = class[(i+1)%len(class)]
		}
	}
	return next
}

// fallbackFold is what SimpleFold answers for a code point its orbit table
// leaves out: its lower case if it has one, else its upper case.
func (d *caseData) fallbackFold(r rune) rune {
	if l, ok := d.lower[r]; ok && l != r {
		return l
	}
	if u, ok := d.upper[r]; ok {
		return u
	}
	return r
}

// check holds the case data against test262's sets of the same version: a
// character with a mapping changes when mapped, and the fold orbits are
// small enough for the matcher's four.
func (d *caseData) check(props map[string]*property) error {
	in := func(name string, r rune) bool {
		p := props[name]
		i := sort.Search(len(p.ranges), func(i int) bool { return p.ranges[i].hi >= r })
		return i < len(p.ranges) && p.ranges[i].lo <= r
	}
	for _, c := range []struct {
		prop   string
		simple map[rune]rune
		full   map[rune][]rune
	}{
		{"Changes_When_Uppercased", d.upper, d.fullUpper},
		{"Changes_When_Lowercased", d.lower, d.fullLower},
	} {
		if props[c.prop] == nil {
			return fmt.Errorf("no %s test", c.prop)
		}
		for r, m := range c.simple {
			if m != r && !in(c.prop, r) {
				return fmt.Errorf("U+%04X maps to U+%04X but is not %s", r, m, c.prop)
			}
		}
		for r := range c.full {
			if !in(c.prop, r) {
				return fmt.Errorf("U+%04X has a full mapping but is not %s", r, c.prop)
			}
		}
	}
	size := map[rune]int{}
	for r := range d.simpleFolds() {
		f, ok := d.fold[r]
		if !ok {
			f = r
		}
		if size[f]++; size[f] > 4 {
			return fmt.Errorf("U+%04X's fold orbit has more than four members", f)
		}
	}
	return nil
}

// caseRange is a run of code points that map by the same deltas, or, with
// upperLower set, a run alternating between an upper-case code point and the
// lower case after it, as unicode.CaseRanges keeps them.
type caseRange struct {
	lo, hi     rune
	up, low    rune
	upperLower bool
}

func (d *caseData) caseRanges() []caseRange {
	var rs []rune
	seen := map[rune]bool{}
	for _, m := range []map[rune]rune{d.upper, d.lower} {
		for r := range m {
			if !seen[r] {
				seen[r] = true
				rs = append(rs, r)
			}
		}
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i] < rs[j] })
	delta := func(m map[rune]rune, r rune) rune {
		if c, ok := m[r]; ok {
			return c - r
		}
		return 0
	}
	var out []caseRange
	for i := 0; i < len(rs); {
		r := rs[i]
		up, low := delta(d.upper, r), delta(d.lower, r)
		// An alternating run: r upper case with r+1 its lower case, and so
		// on, two at a time, at least two pairs of them.
		if up == 0 && low == 1 {
			j := i
			for j+1 < len(rs) && rs[j] == r+rune(j-i) && rs[j+1] == rs[j]+1 &&
				delta(d.upper, rs[j]) == 0 && delta(d.lower, rs[j]) == 1 &&
				delta(d.upper, rs[j+1]) == -1 && delta(d.lower, rs[j+1]) == 0 {
				j += 2
			}
			if j-i >= 4 {
				out = append(out, caseRange{lo: r, hi: rs[j-1], upperLower: true})
				i = j
				continue
			}
		}
		j := i + 1
		for j < len(rs) && rs[j] == rs[j-1]+1 && delta(d.upper, rs[j]) == up && delta(d.lower, rs[j]) == low {
			j++
		}
		out = append(out, caseRange{lo: r, hi: rs[j-1], up: up, low: low})
		i = j
	}
	return out
}

// emitUCD writes package ucd's tables.
func emitUCD(w io.Writer, version string, props map[string]*property, d *caseData) {
	fmt.Fprintf(w, `// Code generated by internal/regexp/internal/unicodegen. DO NOT EDIT.

package ucd

// Version is the Unicode version of every table here, and of the regexp's
// property escapes, which the same run of the generator writes from the
// same test262.
const Version = %q

`, version)

	for _, p := range ucdProperties {
		set := props[p.name]
		fmt.Fprintf(w, "// %s is %s.\nvar %s = table{\n\tr16: []range16{\n", p.table, p.doc, p.table)
		for _, r := range set.ranges {
			if r.lo <= 0xFFFF {
				hi := r.hi
				if hi > 0xFFFF {
					hi = 0xFFFF
				}
				fmt.Fprintf(w, "\t\t{0x%04X, 0x%04X},\n", r.lo, hi)
			}
		}
		fmt.Fprintf(w, "\t},\n\tr32: []range32{\n")
		for _, r := range set.ranges {
			if r.hi > 0xFFFF {
				lo := r.lo
				if lo <= 0xFFFF {
					lo = 0x10000
				}
				fmt.Fprintf(w, "\t\t{0x%04X, 0x%04X},\n", lo, r.hi)
			}
		}
		fmt.Fprintf(w, "\t},\n}\n\n")
	}

	fmt.Fprintf(w, "// caseRanges are UnicodeData's simple upper- and lower-case mappings.\nvar caseRanges = []caseRange{\n")
	for _, c := range d.caseRanges() {
		if c.upperLower {
			fmt.Fprintf(w, "\t{0x%04X, 0x%04X, 0, 0, true},\n", c.lo, c.hi)
		} else {
			fmt.Fprintf(w, "\t{0x%04X, 0x%04X, %d, %d, false},\n", c.lo, c.hi, c.up, c.low)
		}
	}
	fmt.Fprintf(w, "}\n\n")

	next := d.simpleFolds()
	fmt.Fprintf(w, "// asciiFold is SimpleFold of each ASCII code point.\nvar asciiFold = [128]uint16{\n")
	for r := rune(0); r < 128; r++ {
		f, ok := next[r]
		if !ok {
			f = r
		}
		fmt.Fprintf(w, "\t0x%04X,\n", f)
	}
	fmt.Fprintf(w, "}\n\n")

	var orbit []rune
	for r, f := range next {
		if r >= 128 && f != d.fallbackFold(r) {
			orbit = append(orbit, r)
		}
	}
	sort.Slice(orbit, func(i, j int) bool { return orbit[i] < orbit[j] })
	fmt.Fprintf(w, "// caseOrbit is SimpleFold of each code point past ASCII whose orbit is\n"+
		"// not just its lower and upper case.\nvar caseOrbit = []foldPair{\n")
	for _, r := range orbit {
		fmt.Fprintf(w, "\t{0x%04X, 0x%04X},\n", r, next[r])
	}
	fmt.Fprintf(w, "}\n\n")

	for _, m := range []struct {
		name, doc string
		full      map[rune][]rune
	}{
		{"fullUpper", "upper", d.fullUpper},
		{"fullLower", "lower", d.fullLower},
	} {
		var rs []rune
		for r := range m.full {
			rs = append(rs, r)
		}
		sort.Slice(rs, func(i, j int) bool { return rs[i] < rs[j] })
		fmt.Fprintf(w, "// %s is SpecialCasing's unconditional %s-case mappings that are not\n"+
			"// the simple one.\nvar %s = map[rune][]rune{\n", m.name, m.doc, m.name)
		for _, r := range rs {
			var parts []string
			for _, c := range m.full[r] {
				parts = append(parts, fmt.Sprintf("0x%04X", c))
			}
			fmt.Fprintf(w, "\t0x%04X: {%s},\n", r, strings.Join(parts, ", "))
		}
		fmt.Fprintf(w, "}\n\n")
	}
}
