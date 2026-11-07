package vm

import (
	"errors"
	"fmt"
	"math/bits"
	"runtime"
	"strings"
	"testing"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// heapNow is the live heap after full collections.
func heapNow() int64 {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return int64(ms.HeapAlloc)
}

// strCutCase is a built-in that returns a cut string: each of twelve parents,
// mk(i), is held by nothing but its turn of the loop, and keep gets the cut.
// A short cut is copied, and leaves almost nothing live; a long one, at least
// half of its parent, shares, is counted at its size, and goes over the limit.
type strCutCase struct {
	name string
	// mk makes parent p, 4 MB or so, from the turn i.
	mk string
	// short is the expression of a short cut of p, and long, if any, of one
	// past half of it.
	short, long string
}

const (
	strCutPlain = `"  ab,cd " + i + "x".repeat(4e6) + i + ",ef  "`
	strCutWide  = `"  é漢,cd " + i + "é".repeat(2e6) + i + ",ef  "`
	strCutSpace = `" ".repeat(2e6) + "ab" + i + " ".repeat(2e6)`
	strCutHan   = `"漢".repeat(14e5) + i`
	// strCutSource is the comment of a script, nothing of which is in what
	// the script comes to.
	strCutSource = `"x".repeat(4e6) + i`
)

var strCutCases = []strCutCase{
	{"slice", strCutPlain, `p.slice(2, 6)`, `p.slice(1000)`},
	{"slice, not ASCII", strCutWide, `p.slice(2, 8)`, `p.slice(0, 15e5)`},
	{"substring", strCutPlain, `p.substring(2, 6)`, `p.substring(1000)`},
	{"substring, swapped", strCutPlain, `p.substring(6, 2)`, ``},
	{"substr", strCutPlain, `p.substr(2, 4)`, `p.substr(1000)`},
	{"at, not Latin-1", strCutHan, `p.at(3)`, ``},
	{"charAt, not Latin-1", strCutHan, `p.charAt(3)`, ``},
	{"index, not Latin-1", strCutHan, `p[3]`, ``},
	{"substring of one, not Latin-1", strCutHan, `p.substring(3, 4)`, ``},
	{"for-of, not Latin-1", strCutHan, `(() => { for (const c of p) return c })()`, ``},
	{"spread, not Latin-1", strCutHan, `[...p.slice(0, 8)][1]`, ``},
	{"split", strCutPlain, `p.split(",")[0]`, `p.split(",")[1]`},
	{"split, limit", strCutPlain, `p.split(",", 1)[0]`, ``},
	{"split, regexp", strCutPlain, `p.split(/,/)[0]`, `p.split(/,/)[1]`},
	{"split, regexp group", strCutPlain, `p.split(/(,)/)[1]`, ``},
	{"trim", strCutSpace, `p.trim()`, ``},
	{"trimStart", strCutSpace, `p.trimStart().trimEnd()`, ``},
	{"trimEnd", strCutPlain, `p.slice(0, 4).trimEnd()`, `(" " + p).trimEnd()`},
	{"match", strCutPlain, `p.match(/ab/)[0]`, `p.match(/x+/)[0]`},
	{"match, group", strCutPlain, `p.match(/(a)(b)/)[2]`, ``},
	{"match, g", strCutPlain, `p.match(/ab/g)[0]`, ``},
	{"matchAll", strCutPlain, `[...p.matchAll(/(a)b/g)][0][1]`, ``},
	{"exec", strCutPlain, `/ab/.exec(p)[0]`, `/x+/.exec(p)[0]`},
	{"exec, group", strCutPlain, `/(a)(b)/.exec(p)[1]`, `/(x+)/.exec(p)[1]`},
	{"exec, named group", strCutPlain, `/(?<a>a)b/.exec(p).groups.a`, ``},
	{"exec, indices", strCutPlain, `/(a)b/d.exec(p)[1]`, ``},
	{"exec, sticky", strCutPlain, `(() => { const r = /ab/y; r.lastIndex = 2; return r.exec(p)[0] })()`, ``},
	{"Symbol.replace's argument", strCutPlain, `(() => { let m; p.replace(/ab/, x => (m = x, "")); return m })()`, ``},
	{"Symbol.replace's group", strCutPlain, `(() => { let m; p.replace(/(a)b/, (x, a) => (m = a, "")); return m })()`, ``},
	{"RegExp.$1", strCutPlain, `(/(a)b/.test(p), RegExp.$1)`, ``},
	{"RegExp.lastMatch", strCutPlain, `(/ab/.test(p), RegExp.lastMatch)`, ``},
	{"RegExp.leftContext", strCutPlain, `(/ab/.test(p), RegExp.leftContext)`, ``},
	{"RegExp.rightContext", strCutPlain, `(/,ef/.test(p), RegExp.rightContext)`, ``},
	{"padEnd's filler", `"é".repeat(2e6) + i`, `"x".padEnd(40, p)`, ``},
	{"Segmenter", strCutPlain, `new Intl.Segmenter("en").segment(p.slice(0, 2e6)).containing(4).segment`, ``},
	{"JSON.parse", `'["ab",' + JSON.stringify("x".repeat(4e6) + i) + ']'`, `JSON.parse(p)[0]`, `JSON.parse(p)[1]`},
	{"JSON.parse, not ASCII", `'["éb",' + JSON.stringify("é".repeat(2e6) + i) + ']'`, `JSON.parse(p)[0]`, `JSON.parse(p)[1]`},
	{"JSON.parse, escaped", `'["a\\nb",' + JSON.stringify("x".repeat(4e6) + i) + ']'`, `JSON.parse(p)[0]`, ``},
	{"JSON.parse, key", `'{"ab":' + JSON.stringify("x".repeat(4e6) + i) + '}'`, `Object.keys(JSON.parse(p))[0]`, ``},
	{"JSON.parse, a number's source", `'[1, ' + JSON.stringify("x".repeat(4e6) + i) + ']'`,
		`(() => { let m; JSON.parse(p, (k, v, c) => (k === "0" && (m = c.source), v)); return m })()`, ``},
	{"ShadowRealm", strCutPlain, `new ShadowRealm().evaluate("(s) => s.slice(2, 6)")(p)`, ``},
	{"eval's constant", strCutSource, "eval(`'ab' /*${p}*/`)", ``},
	{"eval's tagged template", strCutSource, "eval(`((s) => s[0])\\`ab\\` /*${p}*/`)", ``},
	{"eval's template's raw", strCutSource, "eval(`((s) => s.raw[0])\\`ab\\` /*${p}*/`)", ``},
	{"eval's function's text", strCutSource, "eval(`(function () {}) /*${p}*/`).toString()", ``},
	{"eval's function's name", strCutSource, "eval(`(function ab() {}) /*${p}*/`).name", ``},
	{"eval's variable", strCutSource, "eval(`var v${i} /*${p}*/`)", ``},
	{"eval's property name", strCutSource, "eval(`({}).p${i} /*${p}*/`)", ``},
	{"eval's regexp", strCutSource, "eval(`/ab/ /*${p}*/`)", ``},
	{"eval's regexp's source", strCutSource, "eval(`/ab/g /*${p}*/`).source", ``},
	{"Function's result", strCutSource, "new Function(`return 'ab' /*${p}*/`)()", ``},
}

// TestStringCutsUnderALimit runs every built-in that returns a cut string,
// under a limit, with a parent nothing else holds: a short cut leaves nothing
// of its parent, with no error, and a long one is counted, and ends in
// ErrMemoryLimit. KI-77: twelve parents of 4 MB each held by their short cuts
// are 48 MB past a limit of 16 that nothing sees.
func TestStringCutsUnderALimit(t *testing.T) {
	if testing.Short() {
		t.Skip("makes gigabytes of strings")
	}
	run := func(t *testing.T, r *Runtime, mk, cut string) error {
		t.Helper()
		_, err := r.Run(compileForTest(t, fmt.Sprintf(
			`for (let i = 0; i < 12; i++) { let p = %s; keep.push(%s) }`, mk, cut)))
		return err
	}
	for _, c := range strCutCases {
		t.Run(c.name, func(t *testing.T) {
			r := New(Config{MemoryLimit: 16 << 20})
			defer r.Close()
			r.SetEvaluator(func(source string, _ EvalRequest) (*bytecode.Function, error) {
				return compileForTest(t, source), nil
			})
			if _, err := r.Run(compileForTest(t, `var keep = []`)); err != nil {
				t.Fatal(err)
			}
			before, at := heapNow(), r.meter.walk(r)
			if err := run(t, r, c.mk, c.short); err != nil {
				t.Fatalf("short cut: %v", err)
			}
			if held, counted := heapNow()-before, r.meter.walk(r)-at; held-counted > 1<<20 {
				t.Errorf("twelve short cuts hold %.1f MB, counted at %.1f", float64(held)/(1<<20), float64(counted)/(1<<20))
			}
			if c.long == "" {
				return
			}
			r2 := New(Config{MemoryLimit: 16 << 20})
			defer r2.Close()
			r2.SetEvaluator(r.evaluator)
			if _, err := r2.Run(compileForTest(t, `var keep = []`)); err != nil {
				t.Fatal(err)
			}
			if err := run(t, r2, c.mk, c.long); !errors.Is(err, ErrMemoryLimit) {
				t.Errorf("long cut: %v, want ErrMemoryLimit", err)
			}
		})
	}
}

// A string a chain of halvings has cut only ever shares with its root while it
// is at least half of it, whatever each step's parent was, and on every way a
// string can pass through Go text and come back with no mark.
func TestStringCutChains(t *testing.T) {
	if testing.Short() {
		t.Skip("makes gigabytes of strings")
	}
	for _, c := range []struct{ name, route string }{
		{"slice", `s`},
		{"normalize", `s.normalize()`},
		{"Symbol's description", `Symbol(s).description`},
		{"Symbol.for's key", `Symbol.keyFor(Symbol.for(s))`},
		{"RegExp's source", `new RegExp(s).source`},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := New(Config{MemoryLimit: 1 << 30})
			defer r.Close()
			if _, err := r.Run(compileForTest(t, `var keep = []`)); err != nil {
				t.Fatal(err)
			}
			before, at := heapNow(), r.meter.walk(r)
			if _, err := r.Run(compileForTest(t, fmt.Sprintf(`
				for (let i = 0; i < 12; i++) {
					let s = "x".repeat(4e6) + i
					while (s.length > 2) { s = s.slice(s.length >> 1); s = %s }
					keep.push(s)
				}`, c.route))); err != nil {
				t.Fatal(err)
			}
			if held, counted := heapNow()-before, r.meter.walk(r)-at; held-counted > 1<<20 {
				t.Errorf("twelve chains hold %.1f MB, counted at %.1f", float64(held)/(1<<20), float64(counted)/(1<<20))
			}
		})
	}
}

// strCutChain cuts s by n bytes from its start until little is left, and says
// what it copied, checking the bound on the way: a cut shares with root, the
// string whose bytes it holds, only at half of it or more.
func strCutChain(t *testing.T, r *Runtime, s *String, step func(*String) int) (copied, copies int) {
	t.Helper()
	root := s
	for s.length > 1 {
		n := step(s)
		v := r.kept(s, s.Substring(n, s.length))
		if strWithin(v.s, root.s) {
			if 2*len(v.s) < len(root.s) {
				t.Fatalf("a cut of %d bytes shares a root of %d", len(v.s), len(root.s))
			}
			if v.ropes&strRooted == 0 || int64(1)<<(v.ropes&^strRooted) < int64(len(root.s)) {
				t.Fatalf("a shared cut records %#x, of a root of %d", v.ropes, len(root.s))
			}
		} else {
			if v.ropes != 0 {
				t.Fatalf("a copy records %#x", v.ropes)
			}
			root, copied, copies = v, copied+len(v.s), copies+1
		}
		if !strings.HasSuffix(s.Go(), v.Go()) {
			t.Fatal("the cut is not the string's end")
		}
		s = v
	}
	return copied, copies
}

// A shared cut never holds more than twice its size, and s = s.slice(1) stays
// linear: it shares until it is below half, copies once, and shares again, so
// that what it copies is no more than twice the string, however the rounding
// of the root's size falls. On each start that rounding can make a difference
// for: a power of two, one less, one more.
func TestStringCutBound(t *testing.T) {
	r := New(Config{MemoryLimit: 1 << 30})
	defer r.Close()
	for _, n := range []int{2, 3, 4, 5, 7, 8, 9, 100, 1023, 1024, 1025, 4095, 4096, 4097, 65536, 65537, 1<<20 - 1, 1 << 20, 1<<20 + 1} {
		s := NewString(strings.Repeat("ab", n)[:n])
		copied, copies := strCutChain(t, r, s, func(*String) int { return 1 })
		if copied > 2*n {
			t.Errorf("%d bytes: slice(1) copied %d", n, copied)
		}
		if limit := bits.Len(uint(n)) + 1; copies > limit {
			t.Errorf("%d bytes: slice(1) made %d copies, want at most %d", n, copies, limit)
		}
	}
	// By halves, the step that creeps past a rule on the parent alone.
	for _, n := range []int{1024, 1025, 1 << 20, 1<<20 + 7} {
		s := NewString(strings.Repeat("x", n))
		strCutChain(t, r, s, func(s *String) int { return s.length - s.length/2 })
	}
	// Not ASCII: the rule is in bytes.
	s := NewString(strings.Repeat("é", 1000))
	strCutChain(t, r, s, func(*String) int { return 1 })
}

// s = s.slice(1) in a script: the same, end to end, on the string's last
// character, in linear time. Quadratic, it would not end in a test's life.
func TestStringCutLoopStaysLinear(t *testing.T) {
	r := New(Config{MemoryLimit: 256 << 20})
	defer r.Close()
	v, err := r.Run(compileForTest(t, `
		let s = "x".repeat(4e6) + "y", n = 0
		while (s.length > 1) { s = s.slice(1); n++ }
		s + n`))
	if err != nil || v.String().Go() != "y4000000" {
		t.Fatalf("got %v, %v", v, err)
	}
}

// KI-77's case. Sixty strings of 11 MB, each cut down to two characters under
// a limit of 32 MB, had left 715 MB live and no error. The cuts are copied now:
// nothing is left of the parents, so there is nothing for the limit to stop
// and no error. Cuts past half of each share, are counted, and stop it.
func TestStringCutsOfKI77(t *testing.T) {
	if testing.Short() {
		t.Skip("makes gigabytes of strings")
	}
	const loop = `var keep = []; for (let i = 0; i < 60; i++) { let p = "x".repeat(11e6) + i; keep.push(p.slice(%s)) }`
	r := New(Config{MemoryLimit: 32 << 20})
	defer r.Close()
	before := heapNow()
	if _, err := r.Run(compileForTest(t, fmt.Sprintf(loop, `0, 2`))); err != nil {
		t.Fatalf("short cuts: %v", err)
	}
	if held := heapNow() - before; held > 1<<20 {
		t.Errorf("sixty short cuts hold %.1f MB", float64(held)/(1<<20))
	}
	r2 := New(Config{MemoryLimit: 32 << 20})
	defer r2.Close()
	if _, err := r2.Run(compileForTest(t, fmt.Sprintf(loop, `0, 6e6`))); !errors.Is(err, ErrMemoryLimit) {
		t.Fatalf("long cuts: %v, want ErrMemoryLimit", err)
	}
}

// A Program run again keeps to the rule, on strings of its constants too: its
// constants are the Program's, and a runtime never writes to them.
func TestStringCutsOfAProgramRunAgain(t *testing.T) {
	r := New(Config{MemoryLimit: 16 << 20})
	defer r.Close()
	big := strings.Repeat("x", 1<<20)
	prog := compileForTest(t, `var big = "`+big+`"; globalThis.keep ??= []; keep.push(big.slice(2, 6), big.slice(1000)); keep.length`)
	for i := 1; i <= 3; i++ {
		v, err := r.Run(prog)
		if err != nil || v.Number() != float64(2*i) {
			t.Fatalf("run %d: %v, %v", i, v, err)
		}
		keep := r.global.getOwn(r.atoms.intern("keep")).value().Object().elemsOf()
		parent := r.global.getOwn(r.atoms.intern("big")).value().String()
		short, long := keep[2*i-2].String(), keep[2*i-1].String()
		if strWithin(short.s, parent.s) || !strWithin(long.s, parent.s) || long.ropes&strRooted == 0 {
			t.Fatalf("run %d: short shares %v, long %v, %#x", i, strWithin(short.s, parent.s), strWithin(long.s, parent.s), long.ropes)
		}
		if parent.ropes != 0 {
			t.Fatalf("run %d: the constant has %#x", i, parent.ropes)
		}
	}
}

// What a cut keeps in String.ropes is no count of strAppend's, in either
// direction: an append to a shared cut is an ordinary one, in a runtime with
// no limit, where the cut may have gone, and the count a string of strAppend's
// has left is no size in one with a limit.
func TestStringCutMarkIsNoAppendCount(t *testing.T) {
	metered, plain := New(Config{MemoryLimit: 1 << 30}), New(Config{})
	defer metered.Close()
	defer plain.Close()
	p := NewString(strings.Repeat("x", 1000))
	cut := metered.kept(p, p.Substring(1, 1000))
	if cut.ropes&strRooted == 0 || cut.building || cut.left != nil {
		t.Fatalf("a cut: ropes %#x, building %v, rope %v", cut.ropes, cut.building, cut.left != nil)
	}
	a, err := strAppend[struct{}](plain, cut, NewString(strings.Repeat("y", 100)))
	if err != nil || !a.building || a.ropes != 0 || a.Go() != cut.Go()+strings.Repeat("y", 100) {
		t.Fatalf("an append to a cut: ropes %d, %v", a.ropes, err)
	}
	a, err = strAppend[struct{}](plain, cut, NewString(strings.Repeat("y", 100)))
	if err != nil {
		t.Fatal(err)
	}
	for k := 1; k < strAppendRopes; k++ {
		if a, err = strAppend[struct{}](plain, a, NewString(strings.Repeat("z", 100))); err != nil || int(a.ropes) != k {
			t.Fatalf("append %d: ropes %d, %v", k, a.ropes, err)
		}
	}
	flat := a.Go()
	if a.ropes == 0 || a.left != nil {
		t.Fatalf("flat: ropes %d, rope %v", a.ropes, a.left != nil)
	}
	if root, mark := strRootOf(a); root != int64(len(flat)) || mark&strRooted == 0 {
		t.Fatalf("a count taken for a size: root %d of %d, %#x", root, len(flat), mark)
	}
	if v := metered.kept(a, a.Substring(1, 4)); strWithin(v.s, a.s) {
		t.Fatal("a short cut of an appended string shares it")
	}
}

// A cut in a runtime with a limit leaves the room it was cut into and leaves it
// empty, and in a runtime with none it stays.
func TestStringCutLeavesItsRoom(t *testing.T) {
	metered, plain := New(Config{MemoryLimit: 1 << 30}), New(Config{})
	defer metered.Close()
	defer plain.Close()
	p := NewString(strings.Repeat("x", 1000))
	var room String
	if v := plain.kept(p, p.substringInto(&room, 1, 3)); v != &room {
		t.Fatal("no limit: the cut left its room")
	}
	if v := metered.kept(p, p.substringInto(&room, 1, 3)); v == &room || room.s != "" || strWithin(v.s, p.s) {
		t.Fatalf("short: in the room %v, room %d bytes, shares %v", v == &room, len(room.s), strWithin(v.s, p.s))
	}
	if v := metered.kept(p, p.substringInto(&room, 1, 900)); v == &room || room.s != "" || !strWithin(v.s, p.s) || v.ropes&strRooted == 0 {
		t.Fatalf("long: in the room %v, room %d bytes, shares %v, %#x", v == &room, len(room.s), strWithin(v.s, p.s), v.ropes)
	}
}

// The field stays where it was: native code reads a String's others at fixed
// offsets, and the struct's size is its size class's.
func TestStringCutMarkHasNoFieldOfItsOwn(t *testing.T) {
	if is64Bit != 1 {
		return
	}
	if n := unsafe.Sizeof(String{}); n != 48 {
		t.Errorf("a String is %d bytes", n)
	}
	if n := unsafe.Offsetof(String{}.ropes); n != 47 {
		t.Errorf("ropes is at %d", n)
	}
}

// What the walk last saw is not kept alive by the meter, which would hold it
// past the script's letting go, at no count.
func TestMemoryWalkKeepsNothing(t *testing.T) {
	r := New(Config{MemoryLimit: 1 << 30})
	defer r.Close()
	if _, err := r.Run(compileForTest(t, `var keep = [{a: new Map([[1, "x".repeat(99)]])}]`)); err != nil {
		t.Fatal(err)
	}
	m := r.meter
	m.walk(r)
	if len(m.seen) != 0 || cap(m.objects) == 0 || cap(m.strings) == 0 {
		t.Fatalf("seen %d, lists %d, %d", len(m.seen), cap(m.objects), cap(m.strings))
	}
	for _, o := range m.objects[:cap(m.objects)] {
		if o != nil {
			t.Fatal("the objects' list holds an object")
		}
	}
	for _, s := range m.strings[:cap(m.strings)] {
		if s != nil {
			t.Fatal("the strings' list holds a string")
		}
	}
	for _, v := range m.values[:cap(m.values)] {
		if v.IsValid() {
			t.Fatal("the values' list holds a value")
		}
	}
}
