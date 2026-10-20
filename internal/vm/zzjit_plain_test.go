//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "testing"

// plainSetup's functions make the calls of Go a call from native code makes
// without a frame where they run nothing (jitGoCallPlain): a Map's and a
// Set's get, has, set, add and delete, String(x) of every kind of
// primitive, an array's length written, a write a site's cache answers --
// and, beside each, what still needs the frame: a receiver of the wrong
// kind, an object whose toString runs, a length that is no length or not
// writable, a setter, a read-only property.
const plainSetup = `
	function mapLoop(m, n) { let s = 0; for (let i = 0; i < n; i++) { if (m.set(i % 7, i) !== m) s -= 1000; if (m.has(i % 9)) s += m.get(i % 9); if (m.delete(i % 11)) s += 100 } return s }
	function maps(n) {
		const m = new Map(); const r = mapLoop(m, n);
		m.set(NaN, 1); m.set(-0, 2);
		return r + ':' + m.size + ':' + m.get(NaN) + ':' + m.get(0) + ':' + m.has(+0) + ':' + m.delete(99) + ':' + m.delete(NaN) + ':' + Object.is(m.keys().next().value, -0) + ':' + (m.set(5, 5) === m)
	}
	function setLoop(st, n) { let c = 0; for (let i = 0; i < n; i++) { if (st.add(i % 5) !== st) c -= 1000; if (st.has(i % 8)) c++; if (i % 13 === 0 && st.delete(i % 5)) c += 100 } return c }
	function sets(n) { const st = new Set(); const r = setLoop(st, n); return r + ':' + st.size + ':' + st.delete(1) + ':' + st.delete(1) + ':' + st.size + ':' + (st.add(-0) === st) + ':' + Object.is([...st].pop(), 0) }
	function wrongReceiver() { try { Map.prototype.get.call(new Set(), 1); return 'no' } catch (e) { return e.constructor.name } }
	function wrongDelete() { try { Map.prototype.delete.call(new Set([1]), 1); return 'no' } catch (e) { return e.constructor.name + ':' + Set.prototype.delete.call(new Set([1]), 1) } }
	var xs = [1, 2.5, -0, NaN, 'a', true, undefined, null, 10n, Symbol('d'), 1e21, -1e-7]
	function stringLoop(n) { let out = ''; for (let i = 0; i < n; i++) out = String(xs[i % xs.length]) + String() + String(i, 'x'); return out }
	function strings(n) { return stringLoop(n) + '|' + xs.map(x => String(x)).join(',') }
	var called = 0
	var withToString = { toString() { called++; return 'T' } }
	function stringObj(n) { called = 0; let s; for (let i = 0; i < n; i++) s = String(withToString); return s + called }
	function newString() { const s = new String(5); return typeof s + ':' + s.length }
	function lengthLoop(a, n) { for (let i = n; i-- > 0;) a.length = i; return a }
	function lengths(n) {
		const a = []; for (let i = 0; i < n; i++) a[i] = i;
		const r = lengthLoop(a, n).length; a.length = 3;
		const b = [1, 2, 3]; let e = ''; try { b.length = 1.5 } catch (x) { e = x.constructor.name }
		return r + ':' + a.length + ':' + (1 in a) + ':' + a[2] + ':' + e + ':' + b.length
	}
	function frozenLength() { 'use strict'; const a = Object.freeze([1, 2]); try { a.length = 0; return 'no' } catch (e) { return e.constructor.name + a.length } }
	function sloppyFrozenLength() { const a = Object.freeze([1, 2]); a.length = 0; return a.length }
	function sparseLength() { const a = []; a[100000] = 1; a.length = 5; return a.length + ':' + (100000 in a) }
	function writeLoop(n) { let o; for (let i = 0; i < n; i++) { o = {}; o.a = 1; o.b = 2; o.c = 3; o.d = 4; o.e = 5; o.f = 6; o.g = 7; o.h = 8; o.i = 9; o.j = 10; o.k = 11; o.a = 12 } return o }
	function writes(n) { return JSON.stringify(writeLoop(n)) }
	class WithSetter { set v(x) { this._v = x * 2 } }
	function setter(n) { const o = new WithSetter(); for (let i = 0; i < n; i++) o.v = i; return o._v }
	function readOnly(n) { const o = Object.defineProperty({}, 'x', { value: 1, writable: false }); for (let i = 0; i < n; i++) o.x = i; return o.x }
`

// plainRounds are plainSetup's calls and their answers, Node's
// (v26.10.0).
var plainRounds = [][2]string{
	{`maps(30)`, `2310:3:1:2:true:false:true:false:true`},
	{`sets(30)`, `320:4:false:false:4:true:false`},
	{`wrongReceiver()`, `TypeError`},
	{`wrongDelete()`, `TypeError:true`},
	{`strings(30)`, `true29|1,2.5,0,NaN,a,true,undefined,null,10,Symbol(d),1e+21,-1e-7`},
	{`stringObj(30)`, `T30`},
	{`newString()`, `object:1`},
	{`lengths(30)`, `0:3:false:undefined:RangeError:3`},
	{`frozenLength()`, `TypeError2`},
	{`sloppyFrozenLength()`, `2`},
	{`sparseLength()`, `5:false`},
	{`writes(30)`, `{"a":12,"b":2,"c":3,"d":4,"e":5,"f":6,"g":7,"h":8,"i":9,"j":10,"k":11}`},
	{`setter(30)`, `58`},
	{`readOnly(30)`, `1`},
}

// A call of Go from native code that runs nothing -- a collection's
// lookup, addition or deletion, String of a primitive, an array's length
// written, a write its site's cache answers -- is made without the frame
// and the pins of one that may run script (jitGoCallPlain): each answer is
// Node's and the interpreter's, and the loops that make only such calls
// make none of Go's that count (ssaStats' goCalls) and never leave their
// code.
func TestJITPlainGoCalls(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const repeats = 6
	var rounds []string
	for range repeats {
		for _, r := range plainRounds {
			rounds = append(rounds, r[0])
		}
	}
	r := jitAgreesForTest(t, plainSetup, rounds...)
	for _, round := range plainRounds {
		v, err := r.Run(compileForTest(t, "''+("+round[0]+")"))
		if err != nil {
			t.Fatalf("%s: %v", round[0], err)
		}
		if got := v.String().Go(); got != round[1] {
			t.Errorf("%s = %s, want %s", round[0], got, round[1])
		}
	}
	// A Map's and a Set's methods are read once in each invocation, by Go:
	// a call of Go with its frame at an entry at most, against the thirty
	// turns of each loop's.
	for _, name := range []string{"mapLoop", "setLoop", "stringLoop", "lengthLoop", "writeLoop"} {
		e := jitNativeForTest(t, r, name)
		if e.ssaStats.goCalls > e.ssaStats.entries || e.ssaStats.hosts != 0 {
			t.Errorf("%s made %d calls of Go with frames in %d entries and left its code %d times", name, e.ssaStats.goCalls, e.ssaStats.entries, e.ssaStats.hosts)
		}
	}
}

// The calls jitGoCallPlain makes are the realm's built-ins' alone: one
// replaced, or a receiver of the wrong kind, goes through the frame, which
// calls the replacement or throws as the built-in does.
func TestJITPlainGoCallsReplaced(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const setup = `
		function loop(m, n) { let s = ''; for (let i = 0; i < n; i++) { m.set(i, i); s = m.delete(i) + ':' + String(i) } return s }
		function drive(m) { let s; for (let k = 0; k < 20; k++) s = loop(m, 30); return s }
	`
	for _, round := range [][2]string{
		// Node's (v26.10.0) answers.
		{`drive(new Map())`, `true:29`},
		{`(drive(new Map()), Map.prototype.delete = function (k) { return 'gone' + k }, drive(new Map()))`, `gone29:29`},
		{`(drive(new Map()), String = function (x) { return 'S' + x }, drive(new Map()))`, `true:S29`},
		{`(() => { drive(new Map()); try { return drive({ set() {}, delete: Map.prototype.delete }) } catch (e) { return e.constructor.name } })()`, `TypeError`},
	} {
		r := jitAgreesForTest(t, setup, round[0])
		v, err := r.Run(compileForTest(t, "''+("+round[0]+")"))
		if err != nil {
			t.Fatalf("%s: %v", round[0], err)
		}
		if got := v.String().Go(); got != round[1] {
			t.Errorf("%s = %s, want %s", round[0], got, round[1])
		}
	}
}
