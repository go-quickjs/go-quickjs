//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"fmt"
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"runtime"
	"runtime/debug"
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

// A call whose callee is an arrow reading the this it captured is made
// natively as any callee's (ssa.CallSite's ArrowThis), the call giving it
// that this: the builder had left such a site out, so a call the arrow
// was learned at missed it at every call after, leaving native code --
// AWFY CD's Vector.forEach, given (e) => this.state.remove(e) and others,
// 45,000 exits a run. Each answer is the interpreter's.
func TestJITArrowThisCallee(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `
		class Vec { constructor(n) { this.s = []; for (let i = 0; i < n; i++) this.s.push(i) }
			forEach(fn) { for (let i = 0; i < this.s.length; i++) fn(this.s[i]) } }
		class Acc { constructor() { this.sum = 0; this.v = new Vec(64) }
			add() { this.v.forEach((e) => { this.sum += e }) }
			sub() { this.v.forEach((e) => { this.sum -= e >> 1 }) } }
		var acc = new Acc(), plain = 0
		function run(n) { for (let k = 0; k < n; k++) { acc.add(); acc.sub(); acc.v.forEach((e) => { plain += e }) } return acc.sum + ':' + plain }
	`
	rounds := []string{}
	for range 8 {
		rounds = append(rounds, `run(20)`)
	}
	r := jitAgreesForTest(t, setup, rounds...)
	var e *jitEntry
	for k, x := range r.jit.cache {
		if fn := k.Value(); fn != nil && fn.Name == "forEach" {
			e = x
		}
	}
	if e == nil || e.ssa == nil {
		t.Fatal("forEach was not compiled")
	}
	hosts, guards := e.ssaStats.hosts, e.ssaStats.guards
	if _, err := r.Run(compileForTest(t, `run(20)`)); err != nil {
		t.Fatal(err)
	}
	if h, g := e.ssaStats.hosts-hosts, e.ssaStats.guards-guards; h != 0 || g != 0 {
		t.Errorf("forEach left native code %d times for Go and failed %d guards", h, g)
	}
}

// An arithmetic operator's operand that is no number nor an oddball, once
// the site is generic, is converted by Go from native code (ssa.GoNumber),
// as V8's code calls its NonNumberToNumber builtin: '2.5' * 2 runs
// natively, where it had left native code at each such operation. Go
// refuses what could run code or throws -- an object's valueOf, a
// symbol's, a BigInt's -- which leave for Go to make the operation, as
// before. A relational comparison of strings is not converted (Go
// compares it, GoCompare). Each answer is the interpreter's.
func TestJITGoNumber(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `
		var mixed = ['1', '2.5', 3, ' 4 ', true, null, '0x10', '', 'x', undefined, 7]
		function arith(a) { let s = 0, p = 1; for (const x of a) { s += x * 2 - (x % 3 | 0); p = (p ^ (x >> 1)) | 0; s += -x || 0; let y = x; y++; s += y || 0 } return s + ':' + p }
		function rel(a) { let n = 0; for (const x of a) for (const y of a) if (x < y) n++; return n }
		var odd = [{ valueOf() { return 5 } }, 3n]
		function oddArith(a) { let s = ''; for (const x of a) s += (x * 2) + ','; return s }
		function sym() { try { return Symbol() * 2 } catch (e) { return e.constructor.name } }
	`
	rounds := []string{}
	for range 6 {
		rounds = append(rounds, `arith(mixed)`, `rel(['10', '9', '1', '2'])`)
	}
	rounds = append(rounds, `oddArith(odd)`, `oddArith(odd)`, `sym()`, `sym()`)
	r := jitAgreesForTest(t, setup, rounds...)
	e := jitNativeForTest(t, r, "arith")
	hosts, guards := e.ssaStats.hosts, e.ssaStats.guards
	if _, err := r.Run(compileForTest(t, `arith(mixed)`)); err != nil {
		t.Fatal(err)
	}
	if h, g := e.ssaStats.hosts-hosts, e.ssaStats.guards-guards; h != 0 || g != 0 {
		t.Errorf("arith left native code %d times for Go and failed %d guards", h, g)
	}
}

// == and === of what native code does not compare inline (OpEqTagged) --
// a number and a string, a BigInt and a string, strings longer than it
// compares, ropes -- are Go's, called from native code (ssa.GoEqual), as
// V8's code calls its Equal builtin, rather than left at an exit each
// time. An object compared loosely with a primitive, whose conversion may
// run its valueOf, leaves for Go as before. Each answer is the
// interpreter's.
func TestJITGoEqual(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `
		var long = 'abcdefghij'.repeat(20), long2 = 'abcdefghij'.repeat(19) + 'abcdefghij'
		var vals = ['1', 1, 1n, '1.0', long, long2, long + 'x', true, 'true', null, undefined, 0, '', -0, NaN, 'NaN']
		// Native work around each, as code has, which the profit check
		// weighs Go's calls against: a function of Go's calls alone is
		// judged slow on some machines, and left to the tree tier.
		function eqs(a) { let n = 0, m = 0; for (const x of a) for (const y of a) { if (x == y) n++; if (x === y) m++; if (x != y) n += 2; for (let k = 0; k < 40; k++) n = (n * 7 + k) & 0xffff } return n + ':' + m }
		var o = { valueOf() { return 1 } }
		function objEq(a) { let n = 0; for (const x of a) if (x == o) n++; return n }
	`
	rounds := []string{}
	for range 6 {
		rounds = append(rounds, `eqs(vals)`)
	}
	rounds = append(rounds, `objEq(vals)`, `objEq(vals)`)
	r := jitAgreesForTest(t, setup, rounds...)
	e := jitNativeForTest(t, r, "eqs")
	hosts, guards, entries := e.ssaStats.hosts, e.ssaStats.guards, e.ssaStats.entries
	if _, err := r.Run(compileForTest(t, `eqs(vals)`)); err != nil {
		t.Fatal(err)
	}
	if e.ssaStats.entries == entries {
		t.Fatalf("eqs's code was not entered: slow %v", e.entrySlow)
	}
	if h, g := e.ssaStats.hosts-hosts, e.ssaStats.guards-guards; h != 0 || g != 0 {
		t.Errorf("eqs left native code %d times for Go and failed %d guards", h, g)
	}
}

// A typeof not compared with a name, and an instanceof whose constructor
// native code does not know, are Go's from native code (ssa.CallSite's
// TypeOf and InstanceOf), as V8's code calls its Typeof and InstanceOf
// builtins, rather than left at an exit each: an instanceof's
// Symbol.hasInstance runs there, and its throw is the operation's. Each
// answer is the interpreter's.
func TestJITGoTypeOfInstanceOf(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `
		class A {} class B extends A {}
		var vals = [1, 'x', true, null, undefined, {}, [], () => 0, 1n, Symbol(), new A(), new B()]
		var ctors = [A, B, Object, Array, Function]
		// Native work around each, as code has, which the profit check
		// weighs Go's calls against.
		function kinds(a) { let h = 0; for (const x of a) { const t = typeof x; h = (h * 31 + t.length) | 0; for (let k = 0; k < 40; k++) h = (h * 7 + k) | 0 } return h }
		function inst(a) { let n = 0; for (const x of a) for (const c of ctors) { if (x instanceof c) n++; for (let k = 0; k < 40; k++) n = (n * 7 + k) & 0xffff } return n }
		var Even = { [Symbol.hasInstance](v) { return v % 2 === 0 } }
		function odd(a) { let n = 0; for (const x of a) if (x instanceof Even) n++; return n }
		function bad() { try { return 1 instanceof 2 } catch (e) { return e.constructor.name } }
	`
	rounds := []string{}
	for range 6 {
		rounds = append(rounds, `kinds(vals)`, `inst(vals)`)
	}
	rounds = append(rounds, `odd([1, 2, 3, 4])`, `odd([1, 2, 3, 4])`, `bad()`, `bad()`)
	r := jitAgreesForTest(t, setup, rounds...)
	for _, name := range []string{"kinds", "inst"} {
		e := jitNativeForTest(t, r, name)
		hosts, guards, entries := e.ssaStats.hosts, e.ssaStats.guards, e.ssaStats.entries
		if _, err := r.Run(compileForTest(t, name+`(vals)`)); err != nil {
			t.Fatal(err)
		}
		if e.ssaStats.entries == entries {
			t.Fatalf("%s's code was not entered: slow %v", name, e.entrySlow)
		}
		if h, g := e.ssaStats.hosts-hosts, e.ssaStats.guards-guards; h != 0 || g != 0 {
			t.Errorf("%s left native code %d times for Go and failed %d guards", name, h, g)
		}
	}
}

// A derived class's constructor is compiled, its super(...) Go's from
// native code on the code's own frame (ssa.CallSite's Super), which binds
// its this, and its this read after (push_this's check) natively; a this
// read before super() and a second super() throw as the interpreter's, a
// constructor returning an object or a primitive answers as the
// interpreter's, and class fields are initialized by super() as there.
// Before, a derived class's constructor was never compiled: each `new` of
// one ran it in the tree tier -- AWFY DeltaBlue's constraints, a quarter
// of its time with the JIT -- a quarter that is the construction's
// machinery, not the constructors' own: compiled, those with only a store
// after their super() saved nothing measured, and are left to the tree
// tier as any function of host operations alone is (hostOnly). Each
// answer is the interpreter's.
func TestJITDerivedConstructor(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `
		// Each constructor does work of its own after its super(), a loop,
		// as one compiled for itself does (jitcompile's hostOnly).
		class A { constructor(x) { this.x = x } }
		class B extends A { constructor(x, y) { super(x); let h = 0; for (let k = 0; k < 8; k++) h = (h * 31 + k) | 0; this.y = y + this.x + (h & 1) } }
		class C extends B { constructor(x) { super(x, 2); let h = 0; for (let k = 0; k < 8; k++) h = (h + this.y) | 0; this.z = this.x * this.y + h } }
		class F { a = 1 } class G extends F { b = 2; constructor(c) { super(); let h = 0; for (let k = 0; k < 8; k++) h ^= k; this.c = c + this.a + this.b + h } }
		class Imp extends A {}
		function make(n) { let s = 0; for (let i = 0; i < n; i++) { const b = new B(i, 1), c = new C(i), g = new G(i), m = new Imp(i); s = (s + b.y + c.z + g.c + m.x) | 0 } return s }
		class Early extends A { constructor() { this.q = 1; super(1) } }
		class Twice extends A { constructor() { super(1); super(2) } }
		class RetObj extends A { constructor() { super(1); return { other: true } } }
		class RetPrim extends A { constructor() { super(1); return 5 } }
		function err(K) { try { const o = new K(); return JSON.stringify(o) } catch (e) { return e.constructor.name } }
	`
	// No collection meanwhile: which receiver shapes A's code met first
	// depended on the collector, which had Go make native calls while it
	// marked, and A, B's native callee by its super(), leaving at one it
	// was not compiled for is counted as B's leaving.
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	rounds := []string{}
	for range 10 {
		rounds = append(rounds, `make(500)`)
	}
	for range 3 {
		rounds = append(rounds, `err(Early)`, `err(Twice)`, `err(RetObj)`, `err(RetPrim)`)
	}
	r := jitAgreesForTest(t, setup, rounds...)
	for _, name := range []string{"B", "C", "G"} {
		// A class is a lexical binding, not the global object's: its
		// constructor's entry, by its name.
		var e *jitEntry
		for k, x := range r.jit.cache {
			if fn := k.Value(); fn != nil && fn.Name == name && fn.Kind == bytecode.KindDerivedConstructor {
				e = x
			}
		}
		if e == nil || e.ssa == nil || e.ssaStats.entries == 0 {
			t.Fatalf("%s never ran natively: %+v", name, e)
		}
		if e.ssaStats.hosts+e.ssaStats.guards != 0 {
			t.Errorf("%s left native code %d times for Go and failed %d guards", name, e.ssaStats.hosts, e.ssaStats.guards)
		}
	}
	// An implicit constructor, super(...arguments) alone, which native
	// code would only leave for at its super(), is left to the tree tier
	// as any function of host operations alone is (jitcompile's hostOnly).
	for k, x := range r.jit.cache {
		if fn := k.Value(); fn != nil && fn.Name == "Imp" && x.ssa != nil {
			t.Errorf("Imp, an implicit constructor, was compiled: it left native code %d times", x.ssaStats.hosts)
		}
	}
}

// A keyed read of a name the object and its chain do not have -- a name
// some other object has, or one no atom has -- is undefined when Go reads
// it for native code (jitAbsentKey): each object up the chain ordinary,
// none answering the name itself. Typescript's StringHashTable, read by
// keys it has not got, this.table[key], had left native code at each.
// A proxy up the chain, or a getter there, is read as before. Each answer
// is the interpreter's.
func TestJITKeyedReadAbsent(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `
		function Table() { this.table = Object.create(null); this.n = 0 }
		Table.prototype.add = function (k, v) { if (this.table[k] != undefined) return false; this.table[k] = v; this.n++; return true }
		Table.prototype.lookup = function (k) { const d = this.table[k]; return d != undefined ? d : null }
		var t = new Table(); for (var i = 0; i < 100; i++) t.add('k' + i, i)
		var other = { present: 1, toString: 2 }
		var keys = []; for (var i = 0; i < 200; i += 3) keys.push('k' + i); keys.push('present', 'toString', 'zz' + 'q' + 7)
		function hits(n) { let s = 0; for (let j = 0; j < n; j++) for (const k of keys) { const d = t.lookup(k); if (d !== null) s = (s + d) | 0; else s = (s + 1) | 0 } return s }
		var trap = 0, viaProxy = Object.create(new Proxy({}, { get(o, k) { trap++; return 7 } }))
		var getterProto = Object.create({ get missing() { return 9 } })
		function odd(o) { let s = 0; for (const k of ['missing', 'gone']) s += o[k] === undefined ? 1 : o[k]; return s + ':' + trap }
	`
	rounds := []string{}
	for range 6 {
		rounds = append(rounds, `hits(20)`)
	}
	rounds = append(rounds, `odd(viaProxy)`, `odd(getterProto)`, `odd(viaProxy)`, `odd(getterProto)`)
	r := jitAgreesForTest(t, setup, rounds...)
	var e *jitEntry
	for k, x := range r.jit.cache {
		if fn := k.Value(); fn != nil && fn.Name == "lookup" {
			e = x
		}
	}
	h := jitNativeForTest(t, r, "hits")
	hosts, entries := h.ssaStats.hosts, h.ssaStats.entries
	var lookupHosts uint64
	if e != nil {
		lookupHosts = e.ssaStats.hosts
	}
	if _, err := r.Run(compileForTest(t, `hits(20)`)); err != nil {
		t.Fatal(err)
	}
	if h.ssaStats.entries == entries {
		t.Fatalf("hits's code was not entered: slow %v", h.entrySlow)
	}
	if n := h.ssaStats.hosts - hosts; n != 0 {
		t.Errorf("hits left native code %d times for Go", n)
	}
	if e != nil && e.ssaStats.hosts != lookupHosts {
		t.Errorf("lookup left native code %d times for Go", e.ssaStats.hosts-lookupHosts)
	}
}

// A function whose locals closures capture is compiled for native callers
// (jitcompile's callee lowering): its frame is made in the VM's stack, and
// Go makes its closures and closes what they captured there
// (jitCalleeFrame), as on a frame Go entered. It is not inlined (ssa's
// InlineCalls). AWFY DeltaBlue's inputsKnown(mark), which passes an arrow
// reading mark, had been refused, each call of it Go's. Writes around the
// capture, closures that outlive the call, an exit after a closure is
// made, and a loop's let each closure gets its own, answer as the
// interpreter's.
func TestJITCapturedLocalCallees(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `
		class Con { constructor(vs) { this.vs = vs }
			has(fn) { for (const v of this.vs) if (fn(v)) return true; return false }
			inputsKnown(mark) { return !this.has((v) => !(v.mark === mark || v.stay)) } }
		var cons = []; for (var i = 0; i < 8; i++) { const vs = []; for (var j = 0; j < 4; j++) vs.push({ mark: (i + j) % 3, stay: j === 3 }); cons.push(new Con(vs)) }
		function known(n) { let s = 0; for (let k = 0; k < n; k++) for (const c of cons) if (c.inputsKnown(k % 3)) s++; return s }
		function counter(k) { let c = k; const inc = () => { c++; return c }; c += 10; inc(); return c * 2 + inc() }
		function counters(n) { let s = 0; for (let k = 0; k < n; k++) s = (s + counter(k)) | 0; return s }
		function maker(i) { const x = i * 2; return () => x + 1 }
		var made = []
		function makes(n) { for (let k = 0; k < n; k++) made.push(maker(k)); let s = 0; for (const f of made) s = (s + f()) | 0; made.length = 0; return s }
		var weird = { get g() { return { valueOf() { return 3 } } } }
		function exits(i) { let x = i; const get = () => x; const y = weird.g + 1; x += y; return get() }
		function exiting(n) { let s = 0; for (let k = 0; k < n; k++) s = (s + exits(k)) | 0; return s }
		function lets(n) { const fs = []; for (let i = 0; i < n; i++) fs.push(() => i); let s = 0; for (const f of fs) s += f(); return s }
		function letting(n) { let s = 0; for (let k = 0; k < n; k++) s = (s + lets(5)) | 0; return s }
		function blocks(k) { const o = {}; { let x = k; o.a = () => x; x += 5 } { let y = k * 2; o.b = () => y++; y += 1 } return o.a() + o.b() + o.b() }
		function blocking(n) { let s = 0; for (let k = 0; k < n; k++) s = (s + blocks(k)) | 0; return s }
		function addr(a, b) { return () => a * 3 + b }
		function adding(n) { let s = 0; for (let k = 0; k < n; k++) s = (s + addr(k, 7)()) | 0; return s }
	`
	rounds := []string{}
	for range 6 {
		rounds = append(rounds, `known(30)`, `counters(100)`, `makes(100)`, `exiting(100)`, `letting(50)`, `blocking(100)`, `adding(100)`)
	}
	r := jitAgreesForTest(t, setup, rounds...)
	for _, name := range []string{"inputsKnown", "counter", "maker", "exits", "blocks", "addr"} {
		var e *jitEntry
		for k, x := range r.jit.cache {
			if fn := k.Value(); fn != nil && fn.Name == name {
				e = x
			}
		}
		if e == nil || e.ssa == nil || e.calleeRefused {
			t.Errorf("%s has no code for native callers: %+v", name, e)
			continue
		}
		// Compiled for native callers alone, its captured locals in cells
		// of their own (ir.Program's Cells), as V8 allocates a context.
		if !e.ssaCallee || e.ssaProg == nil || !e.ssaProg.Cells {
			t.Errorf("%s's code has its captured locals in its frame: callee %v", name, e.ssaCallee)
		}
	}
}

// A native callee whose captured locals are in cells of its own, whose own
// native callee leaves native code: Go finishes that, and the callee's code
// goes on natively after its call (jitUnwindNative), with the cells now
// open on the frame Go made for it.
func TestJITCapturedLocalCellsResumed(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `
		var weird = { get g() { return { valueOf() { return 3 } } } }
		function deep(i) { return weird.g + i }
		function mid(i) { let x = i; const get = () => x; x += deep(i); x++; return get() * 2 + x }
		function midding(n) { let s = 0; for (let k = 0; k < n; k++) s = (s + mid(k)) | 0; return s }
	`
	rounds := []string{}
	for range 6 {
		rounds = append(rounds, `midding(100)`)
	}
	// With the collector off, which the count is not about: CI's checkptr
	// run, under a collector marking often, once resumed no level.
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	r := jitAgreesForTest(t, setup, rounds...)
	var e *jitEntry
	for k, x := range r.jit.cache {
		if fn := k.Value(); fn != nil && fn.Name == "mid" {
			e = x
		}
	}
	if e == nil || e.ssa == nil || !e.ssaCallee || e.ssaProg == nil || !e.ssaProg.Cells {
		t.Fatalf("mid has no code for native callers with cells of its own: %+v", e)
	}
	if r.jit.resumedLevels == 0 {
		t.Errorf("no native callee went on natively after its callee left native code")
	}
}

// Closures native callees make from their pools (mir's closureTake), as
// V8's code allocates them: each capturing the cells of its maker's own
// (a local, a parameter, a block's let made again), a binding its maker
// captured, and an arrow's this -- the pool run out and filled again, with
// more, which only native code's taking makes; a loop's per-iteration let
// in a native callee; every answer the interpreter's.
func TestJITClosuresFromPools(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `
		function maker(i) { const x = i * 2; return () => x + 1 }
		var made = []
		function makes(n) { for (let k = 0; k < n; k++) made.push(maker(k)); let s = 0; for (const f of made) s = (s + f()) | 0; made.length = 0; return s }
		function addr(a, b) { return function () { return a * 3 + b } }
		function adding(n) { let s = 0; for (let k = 0; k < n; k++) s = (s + addr(k, 7)()) | 0; return s }
		function outer() {
			let z = 5
			function mk(k) { let a = k; const f = () => a + z; a += 2; z++; return f }
			return function (n) { let s = 0; for (let k = 0; k < n; k++) s = (s + mk(k)()) | 0; return s + z }
		}
		var outers = outer()
		class P { constructor(v) { this.v = v } arrow(k) { const a = k; return () => this.v * 10 + a } }
		var ps = [new P(1), new P(2), new P(3)]
		function thises(n) { let s = 0; for (let k = 0; k < n; k++) s = (s + ps[k % 3].arrow(k)()) | 0; return s }
		function blocks(k) { const o = {}; { let x = k; o.a = () => x; x += 5 } { let y = k * 2; o.b = () => y++; y += 1 } return o.a() + o.b() + o.b() }
		function blocking(n) { let s = 0; for (let k = 0; k < n; k++) s = (s + blocks(k)) | 0; return s }
		function lets(n) { const fs = []; for (let i = 0; i < n; i++) fs.push(() => i); let s = 0; for (const f of fs) s = s * 10 + f(); return s }
		function letting(n) { let s = 0; for (let k = 0; k < n; k++) s = (s + lets(5)) | 0; return s }
	`
	rounds := []string{}
	for range 6 {
		rounds = append(rounds, `makes(100)`, `adding(100)`, `outers(100)`, `thises(100)`, `blocking(100)`, `letting(50)`)
	}
	r := jitAgreesForTest(t, setup, rounds...)
	// lets, a native callee, gives each iteration's i a cell of its own
	// at the iteration's end (GoClose), which no frame has open.
	for _, name := range []string{"maker", "addr", "mk", "arrow", "blocks", "lets"} {
		var e *jitEntry
		for k, x := range r.jit.cache {
			if fn := k.Value(); fn != nil && fn.Name == name {
				e = x
			}
		}
		if e == nil || e.ssa == nil || e.ssaProg == nil || !e.ssaProg.Cells {
			t.Errorf("%s has no code with cells of its own: %+v", name, e)
			continue
		}
		pools := 0
		for _, l := range e.literals {
			if l.tmpl.template() == nil {
				continue
			}
			pools++
			if l.pool.Size <= abi.PoolSize {
				t.Errorf("%s's closures at %d were not taken from their pool: its size is %d", name, l.pc, l.pool.Size)
			}
		}
		if pools == 0 {
			t.Errorf("%s makes no closure from a pool", name)
		}
	}
}

// What TestJITCapturedLocalCallees's callees do, the collector marking
// meanwhile: the cells Go makes, and what native code assigns them, live
// as long as the closures that captured them.
func TestJITCapturedLocalCellsMarking(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				runtime.GC()
			}
		}
	}()
	defer func() { close(stop); <-done }()
	setup := `
		function maker(i) { const x = { v: i * 2 }; let y = 'y' + i; const f = () => x.v + y.length; y += '!'; return f }
		var made = []
		function makes(n) { for (let k = 0; k < n; k++) made.push(maker(k)); let s = 0; for (const f of made) s = (s + f()) | 0; made.length = 0; return s }
	`
	rounds := []string{}
	for range 20 {
		rounds = append(rounds, `makes(300)`)
	}
	jitAgreesForTest(t, setup, rounds...)
}
