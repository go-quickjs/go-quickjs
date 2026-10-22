package regexp

import (
	"math/rand"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"
)

// byteSearchPatterns are patterns whose searches over bytes look for a
// literal or a sole first unit with bytes.IndexByte: one that can begin with
// one unit alone, one that is nothing but units, one that requires a literal,
// one whose literal or first unit is past ASCII, and some that can begin with
// more than one unit, which are passed over a byte at a time.
var byteSearchPatterns = []struct{ src, flags string }{
	{"x", ""}, {"x\\d+", ""}, {"[x]y", ""}, {"(?:x|x)y", ""}, {"k+y", ""}, {"needle", ""},
	{"ab", ""}, {"\\w+@ex", ""}, {"(a|b)needle", ""}, {"é", ""}, {"éx", ""}, {"xé", ""}, {"[xé]", ""}, {"[xé]y", ""},
	{"x", "i"}, {"needle", "i"}, {"x\\d", "m"}, {"x", "u"}, {"x.", "u"}, {"x", "y"},
	{"[^a]", ""}, {"\\d", ""}, {"x|y", ""}, {"(?<=a)x", ""}, {"\\bx", ""},
}

// longSubjects is subjects of up to some hundreds of bytes, mostly of a
// filler byte, with the units the patterns look for far between: what a
// search over many bytes at a time passes over in a stretch. extra is pieces
// added to the alphabet.
func longSubjects(seed int64, extra ...string) []string {
	rare := append([]string{"x", "y", "k", "1", "@ex", "needle", "X", "NEEDLE", "\n"}, extra...)
	r := rand.New(rand.NewSource(seed))
	var subjects []string
	for i := 0; i < 120; i++ {
		var b strings.Builder
		for n := r.Intn(300); n > 0; n-- {
			if r.Intn(25) == 0 {
				b.WriteString(rare[r.Intn(len(rare))])
			} else {
				b.WriteByte("aab "[r.Intn(4)])
			}
		}
		subjects = append(subjects, b.String())
	}
	// A unit sought at the last byte, and just before it.
	return append(subjects, strings.Repeat("a", 64)+"x", strings.Repeat("a", 63)+"x1", strings.Repeat("a", 47)+"needle", "")
}

// byteSearchStarts is starts to match subj from: every one near either end,
// and some between.
func byteSearchStarts(n int) []int {
	var starts []int
	for s := 0; s <= n+1; s++ {
		if s < 20 || s > n-20 || s%7 == 0 {
			starts = append(starts, s)
		}
	}
	return starts
}

// TestByteSearchesAgree pins that a search over bytes that looks for a
// literal or a sole first unit with bytes.IndexByte finds what one over
// code units does, from each start, over subjects long enough for it to pass
// many bytes over at once.
func TestByteSearchesAgree(t *testing.T) {
	subjects := longSubjects(5)
	for _, p := range byteSearchPatterns {
		re, err := Compile(p.src, p.flags)
		if err != nil {
			t.Fatalf("/%s/%s: %v", p.src, p.flags, err)
		}
		for _, s := range subjects {
			units := make([]uint16, len(s))
			for i := range s {
				units[i] = uint16(s[i])
			}
			for _, start := range byteSearchStarts(len(s)) {
				want, _ := re.MatchCheckedInto(nil, units, start, nil)
				got, err := re.MatchASCIIInto(nil, s, start, nil)
				if err != nil || !slices.Equal(got, want) {
					t.Fatalf("/%s/%s on %q from %d: bytes %v, %v; code units %v", p.src, p.flags, s, start, got, err, want)
				}
				got, err = re.MatchASCIICheckedInto(nil, s, start, nil)
				if err != nil || !slices.Equal(got, want) {
					t.Fatalf("/%s/%s on %q from %d: bytes with the lent matcher %v, %v; code units %v", p.src, p.flags, s, start, got, err, want)
				}
				want, _ = re.SearchCheckedInto(nil, units, start, nil)
				got, err = re.SearchASCIICheckedInto(nil, s, start, nil)
				if err != nil || !slices.Equal(got, want) {
					t.Fatalf("/%s/%s on %q from %d: a search of bytes %v, %v; of code units %v", p.src, p.flags, s, start, got, err, want)
				}
			}
		}
	}
}

// TestUTF8SoleFirstAgrees pins that a search over UTF-8 that looks for a
// sole first unit with bytes.IndexByte finds what one passing bytes over one
// at a time does, over subjects with characters of two, three and four bytes.
func TestUTF8SoleFirstAgrees(t *testing.T) {
	subjects := longSubjects(7, "é", "日本", "😀", "x😀")
	for _, p := range byteSearchPatterns {
		fast, err := CompileUTF8(p.src, p.flags)
		slow, err2 := CompileUTF8(p.src, p.flags)
		if err != nil || err2 != nil {
			t.Fatalf("/%s/%s: %v %v", p.src, p.flags, err, err2)
		}
		if slow.prog.first != nil {
			slow.prog.first.sole = false
		}
		for _, s := range subjects {
			for _, start := range byteSearchStarts(len(s)) {
				a, err := fast.MatchUTF8Into(nil, s, start, nil)
				b, err2 := slow.MatchUTF8Into(nil, s, start, nil)
				if err != nil || err2 != nil || !slices.Equal(a, b) {
					t.Fatalf("/%s/%s on %q from %d: %v, %v; a byte at a time %v, %v", p.src, p.flags, s, start, a, err, b, err2)
				}
			}
		}
	}
}

// TestFirstUnitsSole pins which patterns can begin with one unit alone, which
// a search over bytes looks for with bytes.IndexByte.
func TestFirstUnitsSole(t *testing.T) {
	for _, c := range []struct {
		src, flags string
		sole       bool
		single     byte
	}{
		{"x\\d", "", true, 'x'}, {"[x]y", "", true, 'x'}, {"(?:x|x)", "", true, 'x'}, {"\\x7f", "", true, 0x7f},
		{"\\0", "", true, 0}, {"@", "", true, '@'}, {"x", "i", false, 0}, {"x|y", "", false, 0},
		{"é", "", false, 0}, {"[xé]", "", true, 'x'}, {"\\d", "", false, 0},
	} {
		re, err := Compile(c.src, c.flags)
		if err != nil {
			t.Fatal(err)
		}
		f := re.prog.first
		if f == nil || f.sole != c.sole || f.single != c.single {
			t.Errorf("/%s/%s: first %+v, want sole %v single %q", c.src, c.flags, f, c.sole, c.single)
		}
	}
	// A unit past ASCII is never a sole one to look for in bytes.
	units := utf16.Encode([]rune("é"))
	if re, _ := Compile("é", ""); re.prog.first == nil || !re.prog.first.admits(units[0]) {
		t.Error("/é/ does not begin with é")
	}
}
