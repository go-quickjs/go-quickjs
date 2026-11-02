//go:build quickjs_jit && !android && !ios && (((linux || windows) && amd64) || (darwin && arm64))

package regexp

import (
	"errors"
	"fmt"
	"math/rand"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf16"

	"github.com/go-quickjs/go-quickjs/internal/jit"
)

// testHost is a host with room for any code.
func testHost() *NativeHost {
	arena := jit.NewArena()
	return &NativeHost{
		Place: func(code []byte) (NativeCode, error) { return jit.PlaceRaw(arena, code) },
		Room:  func(int) bool { return true },
		After: 1,
	}
}

// nativeOutcome is a match's result and the steps it took.
type nativeOutcome struct {
	caps  []int
	steps int
	err   error
}

// searchASCII is execASCIILent's search over s's bytes, with a matcher of
// its own, reporting the steps the matcher counted as well.
func searchASCII(re *Regexp, s string, start int, check func() error) nativeOutcome {
	if re.native != nil {
		re.nativeTick(false)
	}
	a := &matcherASCII{
		caps:       make([]int, 2*(re.groupCount+1)),
		counters:   make([]int, re.prog.counters),
		emptyMarks: make([]int, re.prog.emptyChecks),
	}
	a.input = inputASCII{units: []byte(s), unicode: re.flags&FlagUnicode != 0}
	a.ready(re.prog, check)
	a.steps, a.checkAt = 0, checkInterval
	caps, err := a.search(re, nil, start, re.flags&FlagSticky != 0)
	return nativeOutcome{caps, a.steps, err}
}

// searchUTF16 is exec's search over units, with a matcher of its own,
// reporting the steps the matcher counted as well.
func searchUTF16(re *Regexp, units []uint16, start int, check func() error) nativeOutcome {
	if re.native != nil {
		re.nativeTick(true)
	}
	m := re.newMatcher()
	m.input = input{units: units, unicode: re.flags&FlagUnicode != 0}
	m.ready(re.prog, check)
	m.steps, m.checkAt = 0, checkInterval
	caps, err := m.search(re, nil, start, re.flags&FlagSticky != 0)
	return nativeOutcome{caps, m.steps, err}
}

// compileBoth is the pattern compiled twice: matched by the matcher, and by
// native code from its first match.
func compileBoth(t testing.TB, pattern, flags string) (plain, native *Regexp, ok bool) {
	t.Helper()
	plain, err := Compile(pattern, flags)
	if err != nil {
		return nil, nil, false
	}
	native, err = Compile(pattern, flags)
	if err != nil {
		t.Fatalf("%q: compiled once, not twice: %v", pattern, err)
	}
	native.UseNative(testHost())
	return plain, native, true
}

// sameOutcome fails t unless native code's match of s is the matcher's,
// over its bytes where it is ASCII and over its UTF-16 code units: the same
// captures, the same error, the same steps.
func sameOutcome(t testing.TB, pattern, flags, s string, start int, plain, native *Regexp) {
	t.Helper()
	if isASCII(s) {
		want := searchASCII(plain, s, start, nil)
		got := searchASCII(native, s, start, nil)
		sameResult(t, "bytes", pattern, flags, fmt.Sprintf("%q", s), start, got, want)
	}
	sameOutcomeUnits(t, pattern, flags, wtf8ToUnits(s), min(start, len(wtf8ToUnits(s))), plain, native)
}

// sameOutcomeUnits is sameOutcome over UTF-16 code units alone.
func sameOutcomeUnits(t testing.TB, pattern, flags string, units []uint16, start int, plain, native *Regexp) {
	t.Helper()
	want := searchUTF16(plain, units, start, nil)
	got := searchUTF16(native, units, start, nil)
	sameResult(t, "UTF-16", pattern, flags, fmt.Sprintf("%x", units), start, got, want)
}

func sameResult(t testing.TB, kind, pattern, flags, s string, start int, got, want nativeOutcome) {
	t.Helper()
	if !slices.Equal(got.caps, want.caps) || !errors.Is(got.err, want.err) || !errors.Is(want.err, got.err) || got.steps != want.steps {
		t.Fatalf("/%s/%s on %s (%s) from %d: native %v %v in %d steps, matcher %v %v in %d steps",
			pattern, flags, s, kind, start, got.caps, got.err, got.steps, want.caps, want.err, want.steps)
	}
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func wtf8ToUnits(s string) []uint16 { return utf16.Encode([]rune(s)) }

// Native code matches what the matcher matches, in the same steps, across
// every instruction it compiles.
func TestNativeMatchesMatcher(t *testing.T) {
	patterns := []struct{ pattern, flags string }{
		{`a`, ""}, {`abc`, ""}, {`abcdefghijklmnop`, ""}, {`a.c`, ""}, {`a.c`, "s"},
		{`[a-c]+`, ""}, {`[^a-c]+`, ""}, {`[aeiou]x`, ""}, {`[a-zA-Z0-9_]+@`, ""}, {`[!-/:-@\[-` + "`" + `{-~]`, ""},
		{`\d+\.\d*`, ""}, {`\w+\s+\w+`, ""}, {`\S+`, ""}, {`\D\W`, ""},
		{`a|b|c`, ""}, {`ab|ac|ad`, ""}, {`(a|ab)(c|bcd)(d*)`, ""},
		{`a*`, ""}, {`a+`, ""}, {`a?b`, ""}, {`a*?b`, ""}, {`a+?b`, ""}, {`.*b`, ""}, {`.*?b`, ""}, {`.+`, "s"},
		{`a{2}`, ""}, {`a{2,4}`, ""}, {`a{2,}`, ""}, {`(ab){1,3}c`, ""}, {`(?:a|b){3,5}?c`, ""}, {`(a{0,2}b){2,3}`, ""},
		{`(a)(b)?(c)`, ""}, {`(a)|(b)`, ""}, {`((a)|(b))+`, ""}, {`(a*)*b`, ""}, {`(a*)+b`, ""}, {`(a|)*b`, ""}, {`(?:a?)*?b`, ""},
		{`(a+)\1`, ""}, {`(a|b)\1+`, ""}, {`(ab)\1`, "i"}, {`(?<x>a)|(?<x>b)`, ""}, {`(?:(?<x>a)|(?<x>b))\k<x>`, ""}, {`\1(a)`, ""},
		{`^a`, ""}, {`a$`, ""}, {`^a$`, "m"}, {`^$`, "m"}, {`^\w+$`, "m"}, {`\bab\b`, ""}, {`\Ba\B`, ""}, {`\b`, ""},
		{`AbC`, "i"}, {`[a-c]+`, "i"}, {`[^b]+`, "i"}, {`\w+`, "iu"}, {`\bk`, "iu"}, {`ſ`, "iu"}, {`K`, "iu"},
		{`a.b`, "u"}, {`[^x]+`, "u"}, {`.+`, "u"}, {`\w*?x`, "u"}, {`b`, "y"}, {`a+`, "y"},
		{`(?i:a)b`, ""}, {`x(?m:^)y`, ""}, {`"[^"]*"`, ""}, {`<[^>]+>`, ""}, {`\s*,\s*`, ""},
		{`(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})`, ""}, {`([a-z]+)://([^/]+)(/.*)?`, ""},
	}
	subjects := []string{
		"", "a", "b", "ab", "abc", "aab", "aaab", "abab", "aabb", "abcd", "abcbcd", "abcdefghijklmnop", "xabcdefghijklmnopx",
		"aaaaaaaaaaaaaaaaaaaa", "aaaaaaaaaaaaaaaaaaaab", "a\nb", "a\nb\n", "\nab\n", "AbC", "aBc", "Kk", "s S",
		"ab ab", "a b c", "12.5 7. x", "foo  bar baz", "a,b , c", "1.22.333.4", "http://example.com/x/y",
		`say "hi" and "bye"`, "<a href><b>", "!@#[`{~", "abba abab", "a1_b2", "xy", "x\ny",
	}
	native := 0
	for _, p := range patterns {
		plain, nat, ok := compileBoth(t, p.pattern, p.flags)
		if !ok {
			t.Fatalf("/%s/%s does not compile", p.pattern, p.flags)
		}
		for _, s := range subjects {
			for start := 0; start <= len(s); start++ {
				sameOutcome(t, p.pattern, p.flags, s, start, plain, nat)
			}
		}
		if nat.HasNative() {
			native++
		} else {
			t.Errorf("/%s/%s: no native code", p.pattern, p.flags)
		}
	}
	t.Logf("%d of %d patterns ran natively", native, len(patterns))
}

// A pattern native code does not compile -- a lookaround -- is matched by
// the matcher.
func TestNativeRefusesLookarounds(t *testing.T) {
	for _, p := range []string{`a(?=b)`, `(?!a)b`, `(?<=a)b`, `(?<!a)b`} {
		plain, nat, _ := compileBoth(t, p, "")
		sameOutcome(t, p, "", "abab", 0, plain, nat)
		if nat.HasNative() {
			t.Errorf("/%s/ has native code", p)
		}
	}
}

// A match whose choice points outgrow the stack has it grown, and goes on.
func TestNativeStackGrows(t *testing.T) {
	for _, p := range []string{`(a|b)*c`, `((a)|(b))*$`, `(?:a|b)*?c`, `(a?){1,9000}x`} {
		plain, nat, _ := compileBoth(t, p, "")
		s := strings.Repeat("ab", 5000)
		sameOutcome(t, p, "", s, 0, plain, nat)
		sameOutcome(t, p, "", s+"c", 0, plain, nat)
		if r := nat.lent; r != nil && r.bytes != nil && r.bytes.native != nil && len(r.bytes.native.stack) <= 4*maxLentFrames {
			t.Errorf("/%s/: the stack did not grow", p)
		}
	}
}

// The step budget runs out at the same step as the matcher's, and a match
// longer than the poll interval goes back to Go and on.
func TestNativeBudget(t *testing.T) {
	for _, tc := range []struct{ pattern, s string }{
		{`(a+)+b`, strings.Repeat("a", 30)},
		{`(a|aa)+c`, strings.Repeat("a", 40)},
		{`[a-z]*x`, strings.Repeat("abcdefgh", 50000)},
		{`(?:a+){2,}?b`, strings.Repeat("a", 28)},
	} {
		plain, nat, _ := compileBoth(t, tc.pattern, "")
		sameOutcome(t, tc.pattern, "", tc.s, 0, plain, nat)
	}
	_, nat, _ := compileBoth(t, `(a+)+b`, "")
	if got := searchASCII(nat, strings.Repeat("a", 40), 0, nil); !errors.Is(got.err, ErrComplexity) {
		t.Fatalf("native: %v, want ErrComplexity", got.err)
	}
}

// A check is asked at the same steps as the matcher asks it, and what it
// returns stops the match.
func TestNativeCheck(t *testing.T) {
	stop := errors.New("stop")
	for _, after := range []int{1, 3, 50} {
		plain, nat, _ := compileBoth(t, `(a+)+b`, "")
		s := strings.Repeat("a", 26)
		checker := func() func() error {
			n := 0
			return func() error {
				if n++; n == after {
					return stop
				}
				return nil
			}
		}
		want := searchASCII(plain, s, 0, checker())
		got := searchASCII(nat, s, 0, checker())
		if !errors.Is(want.err, stop) || !errors.Is(got.err, stop) || got.steps != want.steps {
			t.Fatalf("stopping at check %d: native %v in %d steps, matcher %v in %d steps", after, got.err, got.steps, want.err, want.steps)
		}
	}
}

// randomPattern is a pattern of the instructions native code compiles.
func randomPattern(r *rand.Rand, depth int) string {
	atoms := []string{"a", "b", "c", ".", "[ab]", "[^a]", "\\d", "\\w", "\\s", "\\b", "\\B", "^", "$", "A", "ab", "abc", "[a-cx-z]"}
	if depth <= 0 {
		return atoms[r.Intn(len(atoms))]
	}
	switch r.Intn(10) {
	case 0:
		return "(" + randomPattern(r, depth-1) + ")"
	case 1:
		return "(?:" + randomPattern(r, depth-1) + ")"
	case 2:
		return randomPattern(r, depth-1) + "|" + randomPattern(r, depth-1)
	case 3:
		q := []string{"*", "+", "?", "*?", "+?", "??", "{2}", "{1,3}", "{0,2}?", "{2,}"}
		return "(?:" + randomPattern(r, depth-1) + ")" + q[r.Intn(len(q))]
	case 4:
		return randomPattern(r, depth-1) + "\\1"
	case 5:
		return atoms[r.Intn(len(atoms))] + []string{"*", "+", "*?", "+?"}[r.Intn(4)]
	default:
		return randomPattern(r, depth-1) + randomPattern(r, depth-1)
	}
}

// Random patterns over random subjects match alike.
func TestNativeRandom(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	flagSets := []string{"", "i", "m", "s", "u", "y", "im", "iu"}
	native, n := 0, 2000
	if testing.Short() {
		n = 200
	}
	for i := 0; i < n; i++ {
		p := randomPattern(r, 1+r.Intn(4))
		flags := flagSets[r.Intn(len(flagSets))]
		plain, nat, ok := compileBoth(t, p, flags)
		if !ok {
			continue
		}
		for j := 0; j < 8; j++ {
			b := make([]byte, r.Intn(24))
			for k := range b {
				b[k] = "abcAB 1_\n"[r.Intn(9)]
			}
			sameOutcome(t, p, flags, string(b), r.Intn(len(b)+1), plain, nat)
		}
		if nat.HasNative() {
			native++
		}
	}
	t.Logf("%d of %d random patterns ran natively", native, n)
}

// FuzzNative compares native code with the matcher on any pattern and any
// ASCII subject.
func FuzzNative(f *testing.F) {
	f.Add(`(a|b)*c`, "", "ababc")
	f.Add(`(\w+)\s\1`, "i", "Hey hey")
	f.Add(`^(?:a{2,3}|b)+?$`, "m", "aab\naaa")
	f.Fuzz(func(t *testing.T, pattern, flags, s string) {
		for i := 0; i < len(s); i++ {
			if s[i] >= 0x80 {
				return
			}
		}
		plain, nat, ok := compileBoth(t, pattern, flags)
		if !ok {
			return
		}
		sameOutcome(t, pattern, flags, s, 0, plain, nat)
	})
}

// Code the budget has no room for is not made again at each retry, but once
// there is room: a pattern of a dozen \p{L}, 96 KB of code over UTF-16, in a
// runtime whose JIT had 64 KiB, had its code made and thrown away every
// nativeRetry matches, 3 ms each, which made its matches 75 times slower
// than the matcher's.
func TestNativeNoRoomNotMadeAgain(t *testing.T) {
	arena := jit.NewArena()
	room, asked := false, 0
	h := &NativeHost{
		Place: func(code []byte) (NativeCode, error) { return jit.PlaceRaw(arena, code) },
		Room:  func(int) bool { asked++; return room },
		After: 1,
	}
	pattern := strings.Repeat(`(?:\p{L}\d)`, 12)
	plain, _ := Compile(pattern, "u")
	re, _ := Compile(pattern, "u")
	re.UseNative(h)
	units := wtf8ToUnits("é1é2é3é4é5é6é7é8é9é0é1é2 x")
	sameOutcomeUnits(t, pattern, "u", units, 0, plain, re)
	if re.HasNativeUTF16() || asked != 1 {
		t.Fatalf("native %v, room asked %d times; want no code, asked once", re.HasNativeUTF16(), asked)
	}
	if allocs := testing.AllocsPerRun(1, func() {
		for range 4 * nativeRetry {
			re.nativeTick(true)
		}
	}); allocs != 0 {
		t.Errorf("%v allocations in %d matches with no room: the code made again", allocs, 4*nativeRetry)
	}
	if asked < 4 {
		t.Errorf("room asked %d times, want once a retry", asked)
	}
	room = true
	for range nativeRetry {
		sameOutcomeUnits(t, pattern, "u", units, 0, plain, re)
	}
	if !re.HasNativeUTF16() {
		t.Fatal("no code once there is room")
	}
	sameOutcomeUnits(t, pattern, "u", units, 0, plain, re)
}

// The code is given back when its pattern is collected.
func TestNativeReleased(t *testing.T) {
	h := testHost()
	for i := 0; i < 50; i++ {
		re, _ := Compile(fmt.Sprintf("a%db", i), "")
		re.UseNative(h)
		searchASCII(re, "xa1b", 0, nil)
		if !re.HasNative() {
			t.Fatal("no native code")
		}
	}
	if h.Held() == 0 {
		t.Fatal("the host holds nothing")
	}
	for i := 0; i < 20 && h.Held() != 0; i++ {
		runtimeGC()
	}
	if h.Held() != 0 {
		t.Fatalf("the host still holds %d bytes", h.Held())
	}
}

// runtimeGC collects, and gives the cleanups time to run.
func runtimeGC() {
	runtime.GC()
	time.Sleep(10 * time.Millisecond)
}

// Over UTF-16 -- characters past ASCII, pairs of surrogates and lone ones,
// classes of every size, case folding past ASCII -- native code matches
// what the matcher matches, in the same steps.
func TestNativeUTF16MatchesMatcher(t *testing.T) {
	patterns := []struct{ pattern, flags string }{
		{`é`, ""}, {`café`, ""}, {`[à-ÿ]+`, ""}, {`[^à-ÿ]+`, ""}, {`\w+`, ""}, {`\W+`, ""}, {`\s+`, ""}, {`\S+`, ""},
		{`.`, ""}, {`.`, "u"}, {`.+`, "u"}, {`.+?x`, "u"}, {`^.$`, "u"}, {`^.$`, ""}, {`[^x]`, "u"}, {`[^x]+$`, "u"},
		{`😀`, ""}, {`😀`, "u"}, {`😀+`, "u"}, {`[😀-😂]+`, "u"}, {`[^😀]`, "u"}, {`\uD83D`, ""}, {`\uD83D`, "u"},
		{`\uDE00`, "u"}, {`[\uD800-\uDBFF]`, "u"}, {`[\uDC00-\uDFFF]`, ""}, {`😀`, ""},
		{`\p{L}+`, "u"}, {`\P{L}+`, "u"}, {`\p{Lu}`, "u"}, {`\p{Script=Greek}+`, "u"}, {`[\p{L}\p{N}]+`, "u"}, {`\p{Emoji}`, "u"},
		{`[α-ω]+`, "i"}, {`σ`, "i"}, {`ΣΑΣ`, "i"}, {`[а-я]+`, "i"}, {`k`, "iu"}, {`K`, "i"}, {`K`, "iu"}, {`ſ`, "i"}, {`[^k]`, "iu"},
		{`\w`, "iu"}, {`\W`, "iu"}, {`[\w-]+`, "iu"}, {`\bcafé\b`, ""}, {`\B`, "u"}, {`^`, "m"}, {`$`, "m"}, {`^\S+$`, "mu"},
		{`(é)\1`, ""}, {`(.)\1`, "u"}, {`(\p{L})(?:\1)+`, "u"}, {`(?<x>é)|(?<x>è)`, ""},
		{`(?:é|è){2,3}`, ""}, {`(.{2})+`, "u"}, {`(a|é)*?ü`, ""}, {`[éè]+?ç`, "u"}, {`x*`, "u"},
		{`"[^"]*"`, ""}, {`«[^»]*»`, ""}, {`\d+(?:[.,]\d+)?\s*€`, ""},
		{`[Ā-ſƀ-ɏḀ-ỿ]+`, ""}, {`[一-鿿]+`, ""}, {`[^\u0000-\u007F]+`, ""},
	}
	subjects := []string{
		"", "é", "café", "Café CAFÉ", "naïve façade", "über straße", "ΣΑΣ σας", "Привет мир", "日本語テキスト",
		"a😀b", "😀😁😂", "x😀x😀", "k K K", "ſs S", "«quoted» text", `"a" "é"`, "12,5 €", "é\ne\r x",
		"αβγ ΑΒΓ", "mixé ascii 😀 and more", "éé èè", "ééé", "abcü", "àáâãäåæçèéêë", "Ḁỿƀ",
	}
	units := [][]uint16{
		{0xD83D}, {0xDE00}, {0xD83D, 0x61}, {0x61, 0xDE00}, {0xDE00, 0xD83D}, {0xD83D, 0xDE00, 0xDE00},
		{0xD83D, 0xD83D, 0xDE00}, {0x41, 0xD800, 0xDC00, 0x42}, {0xDBFF, 0xDFFF}, {0xE9, 0xD83D, 0xE9},
	}
	refused := 0
	for _, p := range patterns {
		plain, nat, ok := compileBoth(t, p.pattern, p.flags)
		if !ok {
			t.Fatalf("/%s/%s does not compile", p.pattern, p.flags)
		}
		for _, s := range subjects {
			n := len(wtf8ToUnits(s))
			for start := 0; start <= n; start++ {
				sameOutcomeUnits(t, p.pattern, p.flags, wtf8ToUnits(s), start, plain, nat)
			}
		}
		for _, u := range units {
			for start := 0; start <= len(u); start++ {
				sameOutcomeUnits(t, p.pattern, p.flags, u, start, plain, nat)
			}
		}
		if !nat.HasNativeUTF16() {
			refused++
			t.Errorf("/%s/%s: no native code over UTF-16", p.pattern, p.flags)
		}
	}
	t.Logf("%d of %d patterns ran natively over UTF-16", len(patterns)-refused, len(patterns))
}

// A backreference under the i flag is refused over UTF-16, and matched by
// the matcher; over bytes it runs natively.
func TestNativeUTF16RefusesFoldBackref(t *testing.T) {
	plain, nat, _ := compileBoth(t, `(é)\1`, "i")
	sameOutcomeUnits(t, `(é)\1`, "i", wtf8ToUnits("éÉ"), 0, plain, nat)
	sameOutcome(t, `(é)\1`, "i", "xx", 0, plain, nat)
	if nat.HasNativeUTF16() || !nat.HasNative() {
		t.Fatalf("UTF-16 %v, bytes %v", nat.HasNativeUTF16(), nat.HasNative())
	}
}

// What native code takes each instruction to match is what the matcher's
// unitMatches and runeMatches say, for every unit of the BMP and samples
// past it.
func TestNativeItemSpans(t *testing.T) {
	for _, tc := range []struct{ pattern, flags string }{
		{`[a-z]`, ""}, {`[^a-z]`, "i"}, {`\w`, "iu"}, {`\W`, "iu"}, {`\W`, "i"}, {`[à-ÿ]`, "i"}, {`[^à-ÿ]`, "iu"},
		{`\p{L}`, "u"}, {`\P{Lu}`, "iu"}, {`\p{Ll}`, "i"}, {`[\p{Script=Greek}\d]`, "iu"}, {`σ`, "i"}, {`k`, "iu"}, {`ß`, "iu"},
		{`.`, ""}, {`.`, "s"}, {`.`, "u"}, {`[^😀]`, "u"}, {`[😀-😂]`, "iu"}, {`\s`, ""}, {`[\s\S]`, ""}, {`\D`, "u"},
	} {
		re, err := Compile(tc.pattern, tc.flags)
		if err != nil {
			t.Fatal(err)
		}
		m := &matcher{prog: re.prog}
		for _, in := range re.prog.code {
			if !isUnit(in.op) {
				continue
			}
			sp, ok := itemSpans(re.prog, in, 0x10FFFF)
			if !ok {
				t.Fatalf("/%s/%s: no spans for %v", tc.pattern, tc.flags, in.op)
			}
			for c := 0; c <= 0x10FFFF; c++ {
				if c > 0xFFFF && c%97 != 0 {
					continue
				}
				want := m.runeMatches(in, rune(c))
				if c <= 0xFFFF && m.unitMatches(in, uint16(c)) != want {
					t.Fatalf("/%s/%s: unitMatches and runeMatches differ at %U", tc.pattern, tc.flags, c)
				}
				if spansHave(sp, c) != want {
					t.Fatalf("/%s/%s: %U is %v in the spans, %v to the matcher", tc.pattern, tc.flags, c, !want, want)
				}
			}
		}
	}
}

// Every code point with another case is one foldables lists.
func TestNativeFoldables(t *testing.T) {
	f := foldables()
	for c := rune(0); c <= unicode.MaxRune; c++ {
		if unicode.SimpleFold(c) != c {
			if _, ok := slices.BinarySearch(f, c); !ok {
				t.Fatalf("%U has another case but is not listed", c)
			}
		}
	}
}

// Random patterns over random subjects of UTF-16 units -- past ASCII,
// surrogates paired and alone -- match alike.
func TestNativeUTF16Random(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	flagSets := []string{"", "i", "u", "iu", "m", "su"}
	alphabet := []uint16{'a', 'b', 'A', ' ', '1', 0xE9, 0xC9, 0x3C3, 0x3A3, 0x212A, 'k', 0xD83D, 0xDE00, 0xDE01, 0x4E00, '\n'}
	native, n := 0, 2000
	if testing.Short() {
		n = 200
	}
	atoms := []string{"é", "[éa]", "[^é]", `\w`, `\p{L}`, "😀", `\uD83D`, "σ", ".", `\b`}
	for i := 0; i < n; i++ {
		p := randomPattern(r, 1+r.Intn(3))
		if r.Intn(2) == 0 {
			p += atoms[r.Intn(len(atoms))]
		}
		flags := flagSets[r.Intn(len(flagSets))]
		plain, nat, ok := compileBoth(t, p, flags)
		if !ok {
			continue
		}
		for j := 0; j < 8; j++ {
			u := make([]uint16, r.Intn(16))
			for k := range u {
				u[k] = alphabet[r.Intn(len(alphabet))]
			}
			sameOutcomeUnits(t, p, flags, u, r.Intn(len(u)+1), plain, nat)
		}
		if nat.HasNativeUTF16() {
			native++
		}
	}
	t.Logf("%d of %d random patterns ran natively over UTF-16", native, n)
}

// Lone surrogates: native code reads a pair of surrogates as one character
// under the unicode flag and a lone one as a character of its own, and
// without the flag every unit alone, as the matcher does -- over every
// subject of up to four units of a, high and low surrogates, from every
// start, the middle of a pair included.
func TestNativeLoneSurrogates(t *testing.T) {
	patterns := []struct{ pattern, flags string }{
		{`.`, ""}, {`.`, "u"}, {`^.$`, "u"}, {`^..$`, "u"}, {`^.+$`, "u"}, {`.+?$`, "u"}, {`(.)\1`, "u"}, {`(.)\1`, ""},
		{`[^a]`, "u"}, {`[^a]+`, "u"}, {`[^a]*a`, "u"}, {`[^\uDE00]`, "u"}, {`[^\uD83D]+`, "u"},
		{`\uD83D`, ""}, {`\uD83D`, "u"}, {`\uDE00`, ""}, {`\uDE00`, "u"}, {`😀`, ""}, {`😀`, "u"},
		{`[\uD800-\uDBFF]`, ""}, {`[\uD800-\uDBFF]`, "u"}, {`[\uDC00-\uDFFF]+`, "u"}, {`[\uD800-\uDFFF]`, "u"},
		{`\p{Cs}`, "u"}, {`\P{Cs}+`, "u"}, {`[\u{10000}-\u{10FFFF}]`, "u"}, {`[\u{1F600}]`, "u"}, {`😀`, "u"}, {`😀`, ""},
		{`(?:\uD83D|\uDE00)+`, "u"}, {`\uD83D*?\uDE00`, "u"}, {`[^a]*?a`, "u"}, {`a.`, "u"}, {`.a`, "u"},
		{`\b.`, "u"}, {`.$`, "mu"}, {`^.`, "mu"}, {`[^a]`, "iu"}, {`.`, "iu"}, {`\W`, "iu"}, {`.`, "y"}, {`.`, "uy"},
	}
	alphabet := []uint16{'a', 0xD83D, 0xDE00, 0xDBFF, 0xDC00}
	var subjects [][]uint16
	var grow func(u []uint16)
	grow = func(u []uint16) {
		subjects = append(subjects, slices.Clone(u))
		if len(u) == 4 {
			return
		}
		for _, c := range alphabet {
			grow(append(u, c))
		}
	}
	grow(nil)
	for _, p := range patterns {
		plain, nat, ok := compileBoth(t, p.pattern, p.flags)
		if !ok {
			t.Fatalf("/%s/%s does not compile", p.pattern, p.flags)
		}
		for _, u := range subjects {
			for start := 0; start <= len(u); start++ {
				sameOutcomeUnits(t, p.pattern, p.flags, u, start, plain, nat)
			}
		}
		if !nat.HasNativeUTF16() {
			t.Errorf("/%s/%s: no native code over UTF-16", p.pattern, p.flags)
		}
	}

	// What the standard says, as V8 and C QuickJS answer -- but for the last,
	// where Node 26.12 misses the match (/[^a]$/u.exec("😀") is null there,
	// though /^[^a]$/u and /[^a]$/v match), which C QuickJS finds.
	for _, tc := range []struct {
		pattern, flags string
		subject        []uint16
		want           []int
	}{
		{`^.$`, "u", []uint16{0xD83D}, []int{0, 1}},
		{`^.$`, "u", []uint16{0xD83D, 0xDE00}, []int{0, 2}},
		{`^.$`, "", []uint16{0xD83D, 0xDE00}, nil},
		{`\uD83D`, "u", []uint16{0xD83D, 0xDE00}, nil},
		{`\uD83D`, "", []uint16{0xD83D, 0xDE00}, []int{0, 1}},
		{`\uDE00`, "u", []uint16{0xD83D, 0xDE00}, nil},
		{`\uDE00`, "u", []uint16{0xDE00, 0xD83D}, []int{0, 1}},
		{`.`, "u", []uint16{0xDE00, 0xD83D}, []int{0, 1}},
		{`.+`, "u", []uint16{0xD83D, 0xD83D, 0xDE00}, []int{0, 3}},
		{`[\uD800-\uDBFF]`, "u", []uint16{0xD83D, 0xDE00}, nil},
		{`[\uD800-\uDBFF]`, "u", []uint16{0xD83D, 'a'}, []int{0, 1}},
		{`\p{Cs}`, "u", []uint16{'a', 0xDE00}, []int{1, 2}},
		{`(.)\1`, "u", []uint16{0xD83D, 0xD83D}, []int{0, 2, 0, 1}},
		{`[^a]$`, "u", []uint16{0xD83D, 0xDE00}, []int{0, 2}},
	} {
		_, nat, _ := compileBoth(t, tc.pattern, tc.flags)
		searchUTF16(nat, tc.subject, 0, nil)
		got := searchUTF16(nat, tc.subject, 0, nil)
		if !nat.HasNativeUTF16() || !slices.Equal(got.caps, tc.want) || got.err != nil {
			t.Errorf("/%s/%s on %x: %v %v (native %v), want %v", tc.pattern, tc.flags, tc.subject, got.caps, got.err, nat.HasNativeUTF16(), tc.want)
		}
	}
}
