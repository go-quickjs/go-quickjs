package regexp

import (
	"math/rand"
	"slices"
	"testing"
	"unicode/utf16"
)

// A literal's search over code units looks for its rarest unit: a
// quotation mark is common, Q, a capital past the commonest, is not, and
// past ASCII a unit is rarer than a lowercase letter.
func TestRareUnit(t *testing.T) {
	for _, c := range []struct {
		s    string
		want int
	}{
		{`"\/Qngr(`, 3}, {"ab", 1}, {"eQ", 1}, {"aé", 1}, {"éQ", 1}, {"xyz", 2}, {"\x00a", 0}, {"a", 0},
	} {
		if got := rareUnit(utf16.Encode([]rune(c.s))); got != c.want {
			t.Errorf("rareUnit(%q) = %d, want %d", c.s, got, c.want)
		}
	}
}

// indexUnits finds what trying every position finds -- from every start,
// for literals of one to six units whose rare unit is anywhere in them,
// over subjects of units near theirs with the literal, its prefixes and
// its rare unit alone strewn in them -- and a regular expression's search
// for its required literal, over a subject past ASCII, finds Node's
// matches.
func TestIndexUnitsRare(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	alphabet := []uint16{'"', '\\', '/', 'Q', 'n', 'g', 'r', '(', 'e', 0xe9, 0x2030, 0xd83d}
	for round := 0; round < 3000; round++ {
		lit := make([]uint16, 1+r.Intn(6))
		for i := range lit {
			lit[i] = alphabet[r.Intn(len(alphabet))]
		}
		l := newLiteral(lit)
		s := make([]uint16, r.Intn(40))
		for i := range s {
			switch r.Intn(4) {
			case 0:
				s[i] = lit[l.rare]
			default:
				s[i] = alphabet[r.Intn(len(alphabet))]
			}
		}
		if len(s) > len(lit) && round%2 == 0 {
			copy(s[r.Intn(len(s)-len(lit)+1):], lit)
		}
		for from := 0; from <= len(s); from++ {
			want := -1
			for i := from; i+len(lit) <= len(s); i++ {
				if slices.Equal(s[i:i+len(lit)], lit) {
					want = i
					break
				}
			}
			if got := indexUnits(s, l, from); got != want {
				t.Fatalf("indexUnits(%x, %x (rare %d), %d) = %d, want %d", s, lit, l.rare, from, got, want)
			}
		}
	}

	// Octane's search, in a subject past ASCII with quotation marks and Q
	// everywhere: Node finds the one match at 23, and none from 24.
	re, err := Compile(`(^|[^\\])"\\\/Qngr\((-?[0-9]+)\)\\\/"`, "g")
	if err != nil {
		t.Fatal(err)
	}
	s := utf16.Encode([]rune(`{"Q":"‰","QQ":"Q\"","x":"\/Qngr(12)\/","Qngr":"\/Qngr"}`))
	got, err := re.SearchCheckedInto(nil, s, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int{23, 38, 23, 24, 32, 34}; !slices.Equal(got, want) {
		t.Errorf("the search: %v, want %v", got, want)
	}
	if got, _ := re.SearchCheckedInto(nil, s, 24, nil); got != nil {
		t.Errorf("the search from 24: %v, want none", got)
	}
}
