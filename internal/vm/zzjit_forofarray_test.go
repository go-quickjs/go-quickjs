//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"fmt"
	"strings"
	"testing"
)

// A for...of loop over an array runs natively from its start to its end,
// as V8's code iterates an array without a call: native code steps the
// array in place, the step that finds the cursor done -- a call of Go --
// compares its answer with the uninitialized value without leaving, and
// the iter_close the loop ends with, which closes nothing for a cursor
// done or an array walked in place, is native too. Before, each loop left
// native code twice -- at that comparison, which went to Go for any word
// but a boolean's, null's or undefined's, and at iter_close, always Go's
// -- and the interpreter ran each loop's body up to an entry: AWFY's
// Bounce left native code 150,000 times a run. Each answer is the
// interpreter's.
func TestJITForOfArrayNative(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `
		function B(x) { this.x = x } B.prototype.f = function () { return this.x & 1 }
		var bs = []; for (var i = 0; i < 100; i++) bs.push(new B(i))
		function whole(n) { let s = 0; for (let i = 0; i < n; i++) for (const b of bs) if (b.f()) s++; return s }
		function broken(n) { let s = 0; for (let i = 0; i < n; i++) for (const b of bs) { if (b.x === 40 + i) break; s += b.x } return s }
		function pairs(n) { let s = 0; for (let i = 0; i < n; i++) for (const a of bs) { if (a.x > 3) break; for (const b of bs) s += a.x * b.x } return s }
	`
	rounds := []string{}
	for range 6 {
		rounds = append(rounds, `whole(50)`, `broken(20)`, `pairs(10)`)
	}
	r := jitAgreesForTest(t, setup, rounds...)
	names := []string{"whole", "broken", "pairs"}
	type counts struct{ hosts, guards uint64 }
	before := map[string]counts{}
	for _, name := range names {
		e := jitNativeForTest(t, r, name)
		before[name] = counts{e.ssaStats.hosts, e.ssaStats.guards}
	}
	for _, src := range []string{`whole(50)`, `broken(20)`, `pairs(10)`} {
		if _, err := r.Run(compileForTest(t, src)); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range names {
		e := jitNativeForTest(t, r, name)
		if h, g := e.ssaStats.hosts-before[name].hosts, e.ssaStats.guards-before[name].guards; h != 0 || g != 0 {
			t.Errorf("%s left native code %d times for Go and failed %d guards", name, h, g)
		}
	}
}

// Native code closes an array walked in place, left before its end, by
// nothing only while nothing up its iterator's chain has a return method
// (arrayIterNoReturn's validity cell): one a script gives
// %IteratorPrototype% is called, as the interpreter calls it, and once it
// is gone again the code closes natively with no exit, Go's close having
// found the chain without one again.
func TestJITWalkedArrayClose(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `
		var log = [], IP = Object.getPrototypeOf(Object.getPrototypeOf([][Symbol.iterator]()))
		var arr = [1, 2, 3, 4, 5, 6, 7, 8]
		function brk(n) { let s = 0; for (let i = 0; i < n; i++) for (const x of arr) { if (x === 3) break; s += x } return s }
		function pair(n) { let s = 0; for (let i = 0; i < n; i++) { const [a, b] = arr; s += a + b } return s }
		function on() { IP.return = function () { log.push(this.next().value); return {} } }
		function off() { delete IP.return }
		function take() { const l = log.length + ':' + log.slice(0, 3).join(); log = []; return l }
	`
	warm := []string{}
	for range 6 {
		warm = append(warm, `brk(40)`, `pair(40)`)
	}
	rounds := append(append([]string{}, warm...), `on()`, `brk(40) + take()`, `pair(40) + take()`, `off()`, `brk(40) + take()`, `pair(40) + take()`)
	r := jitAgreesForTest(t, setup, rounds...)
	type counts struct{ hosts, guards uint64 }
	before := map[string]counts{}
	for _, name := range []string{"brk", "pair"} {
		e := jitNativeForTest(t, r, name)
		before[name] = counts{e.ssaStats.hosts, e.ssaStats.guards}
	}
	for _, src := range []string{`brk(40)`, `pair(40)`} {
		if _, err := r.Run(compileForTest(t, src)); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"brk", "pair"} {
		e := jitNativeForTest(t, r, name)
		if h, g := e.ssaStats.hosts-before[name].hosts, e.ssaStats.guards-before[name].guards; h != 0 || g != 0 {
			t.Errorf("%s left native code %d times for Go and failed %d guards, its closes' chain without a return again", name, h, g)
		}
	}
}

// An element read by a canonical index's name, toks["44"], reads the
// element its number names when Go makes the read for native code
// (GoIndex, jitKeyedRead), as V8's keyed loads take a string's array
// index: Typescript's scanner reads its token table by the strings its
// keyword table answers, which had left native code at every read. A name
// that is no canonical index -- "01", "-0", "4294967295" -- is a property's
// as before. Each answer is the interpreter's.
func TestJITKeyedReadIndexName(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `
		var toks = []; for (var i = 0; i < 106; i++) toks.push('t' + i)
		var ids = []; for (var i = 0; i < 50; i++) ids.push(String((i * 7) % 106))
		var u8 = new Uint8Array(64); for (var i = 0; i < 64; i++) u8[i] = i * 3
		var odd = ['01', '-0', '4294967295', '3', 'length']
		function scan(n) { let s = ''; for (let k = 0; k < n; k++) for (let i = 0; i < ids.length; i++) s = toks[ids[i]]; return s }
		function mixed(a) { let s = ''; for (const k of odd) s += a[k] + ','; return s }
	`
	rounds := []string{}
	for range 6 {
		rounds = append(rounds, `scan(20)`)
	}
	rounds = append(rounds, `mixed(toks)`, `mixed(u8)`, `mixed(toks)`)
	r := jitAgreesForTest(t, setup, rounds...)
	e := jitNativeForTest(t, r, "scan")
	hosts, guards := e.ssaStats.hosts, e.ssaStats.guards
	if _, err := r.Run(compileForTest(t, `scan(20)`)); err != nil {
		t.Fatal(err)
	}
	if h, g := e.ssaStats.hosts-hosts, e.ssaStats.guards-guards; h != 0 || g != 0 {
		t.Errorf("scan left native code %d times for Go and failed %d guards", h, g)
	}
}

// An inlined callee's element reads are compiled for what its own code met
// (jitFeedback's ownEntry), as its property sites are: a typed array's
// element read natively in the callers it is inlined into, once it has
// met one. Before, an inlined callee's were the root's alone, none, and
// read as an array's: Gameboy's memoryReadNormal, parentObj.memory[address],
// left native code at each read in every caller (inlined there into
// memoryRead, itself a native callee: what the test checks is the
// feedback the callee is inlined with, and that callers reading through
// it, inlining it or calling it, never leave). Each answer is the
// interpreter's.
func TestJITInlinedTypedRead(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `
		function GB(k) { this.mem = new Uint8Array(256); for (var i = 0; i < 256; i++) this.mem[i] = i ^ k }
		var gbs = [new GB(1), new GB(2)], gb = gbs[0]
		function rd(o, a) { return o.mem[a] }
		function rdHigh(o, a) { return o.mem[a | 128] }
		var readers = []; for (var i = 0; i < 256; i++) readers.push(i & 64 ? rdHigh : rd)
		function sum(n) { let s = 0; for (let i = 0; i < n; i++) s = (s + rd(gbs[i & 1], i & 255)) | 0; return s }
		function sum2(n) { let s = 0; for (let i = 0; i < n; i++) { const a = (i * 7) & 255; s = (s ^ readers[a](gbs[i & 1], a)) | 0 } return s }
	`
	// rd called on its own first, from scripts without a loop, which the
	// interpreter runs, until its own code has met a typed array; then
	// the loops that inline it.
	var calls []string
	for i := range 64 {
		calls = append(calls, fmt.Sprintf("rd(gbs[%d], %d)", i&1, i*3))
	}
	rounds := []string{}
	for range 16 {
		rounds = append(rounds, strings.Join(calls, " + "))
	}
	for range 8 {
		rounds = append(rounds, `sum(2000)`, `sum2(2000)`)
	}
	r := jitAgreesForTest(t, setup, rounds...)
	rdCl := globalObject(r, "rd").fn().closure
	own := r.jit.entryFor(rdCl.fn)
	if own == nil || len(own.typedReads) == 0 {
		t.Fatalf("rd's own code met no typed array: %+v", own)
	}
	// The feedback rd is inlined with, a caller's (root), answers what its
	// own code met.
	inlined := &jitFeedback{r: r, fn: rdCl.fn, cl: rdCl, root: &jitFeedback{r: r}}
	if pc := int(own.typedReads[0]); !inlined.TypedRead(pc) {
		t.Errorf("rd inlined reads its element at %d as an array's, though its own code met a typed array there", pc)
	}
	for _, name := range []string{"sum", "sum2"} {
		e := jitNativeForTest(t, r, name)
		hosts, guards := e.ssaStats.hosts, e.ssaStats.guards
		if _, err := r.Run(compileForTest(t, name+`(2000)`)); err != nil {
			t.Fatal(err)
		}
		if h, g := e.ssaStats.hosts-hosts, e.ssaStats.guards-guards; h != 0 || g != 0 {
			t.Errorf("%s left native code %d times for Go and failed %d guards", name, h, g)
		}
	}
}
