package regexp

import (
	"slices"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/go-quickjs/go-quickjs/internal/wtf8"
)

// TestClassOnly pins that a pattern of one character class is found by
// its search (classonly.go) exactly where the matcher finds it -- folded,
// negated, \W under i and u, sticky, read as code points or as code units --
// over ASCII subjects and others, a lone surrogate's among them, from
// every start, by every entry that matches or searches.
func TestClassOnly(t *testing.T) {
	patterns := []struct{ src, flags string }{
		{`["'&<>]`, ""}, {`["'&<>]`, "g"}, {`["'&<>]`, "y"}, {`\s`, ""}, {`\S`, "u"}, {`[^\w-]`, ""},
		{`\W`, "iu"}, {`\w`, "iu"}, {`[a-z]`, "i"}, {`[k]`, "iu"}, {`[s]`, "i"}, {`\d`, "y"},
		{`[^a]`, "u"}, {`[^a]`, ""}, {`[é-ÿ]`, ""}, {`[é-ÿ]`, "i"}, {`[😀]`, "u"},
		{`[\ud800-\udfff]`, ""}, {`[^x]`, "v"}, {`[\q{ab}c]`, "v"}, {`\p{L}`, "u"}, {`[^]`, ""},
	}
	subjects := []string{
		"", "a", "plain text", "Tom & Jerry", `<a title="x">'sale'</a>`, "x\ty\nz", "ABC-def_9",
		"café <ok>", "Kelvin K and long ſ", "😀 & 😀", strings.Repeat("abc", 30) + "<",
	}
	var all [][]uint16
	for _, s := range subjects {
		all = append(all, wtf8.ToUTF16(s))
	}
	// Lone surrogates, which a pattern of code units can match.
	all = append(all, []uint16{'l', 0xd800, ' ', 'x'}, []uint16{0xdc00, 'x'}, []uint16{0xd83d, 0xde00, 0xd800})
	compiled := 0
	for _, p := range patterns {
		fast, err := Compile(p.src, p.flags)
		if err != nil {
			t.Fatalf("/%s/%s: %v", p.src, p.flags, err)
		}
		if fast.prog.classOnly {
			compiled++
		}
		cp := *fast.prog
		cp.classOnly = false
		slow := &Regexp{core: &core{source: fast.source, flags: fast.flags, prog: &cp, groupCount: fast.groupCount, groupNames: fast.groupNames}}
		for _, units := range all {
			s := string(utf16.Decode(units))
			for start := 0; start <= len(units)+1; start++ {
				want, err1 := slow.MatchCheckedInto(nil, units, start, nil)
				got, err2 := fast.MatchCheckedInto(nil, units, start, nil)
				if err1 != nil || err2 != nil || !slices.Equal(got, want) {
					t.Fatalf("/%s/%s on %q from %d: %v, %v; matcher %v, %v", p.src, p.flags, s, start, got, err2, want, err1)
				}
				want, _ = slow.SearchCheckedInto(nil, units, start, nil)
				got, _ = fast.SearchCheckedInto(nil, units, start, nil)
				if !slices.Equal(got, want) {
					t.Fatalf("/%s/%s on %q from %d: a search %v; matcher %v", p.src, p.flags, s, start, got, want)
				}
				if !asciiUnits(units) {
					continue
				}
				want, _ = slow.MatchASCIICheckedInto(nil, s, start, nil)
				got, _ = fast.MatchASCIICheckedInto(nil, s, start, nil)
				if !slices.Equal(got, want) {
					t.Fatalf("/%s/%s on %q from %d: over bytes %v; matcher %v", p.src, p.flags, s, start, got, want)
				}
				want, _ = slow.MatchASCIIInto(nil, s, start, nil)
				got, _ = fast.MatchASCIIInto(nil, s, start, nil)
				if !slices.Equal(got, want) {
					t.Fatalf("/%s/%s on %q from %d: pooled over bytes %v; matcher %v", p.src, p.flags, s, start, got, want)
				}
				want, _ = slow.SearchASCIICheckedInto(nil, s, start, nil)
				got, _ = fast.SearchASCIICheckedInto(nil, s, start, nil)
				if !slices.Equal(got, want) {
					t.Fatalf("/%s/%s on %q from %d: a search over bytes %v; matcher %v", p.src, p.flags, s, start, got, want)
				}
			}
		}
	}
	// Most of them are one class; the check above is idle for the rest.
	if compiled < 15 {
		t.Fatalf("%d of %d patterns are one class", compiled, len(patterns))
	}
	if re, _ := Compile(`[ab]c`, ""); re.prog.classOnly {
		t.Fatal("/[ab]c/ taken for one class")
	}
	if re, _ := Compile(`([ab])`, ""); re.prog.classOnly {
		t.Fatal("/([ab])/ taken for one class")
	}
}

func asciiUnits(units []uint16) bool {
	for _, u := range units {
		if u >= 0x80 {
			return false
		}
	}
	return true
}
