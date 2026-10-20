//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"testing"
	"unsafe"
)

// lengthSetup's functions write arrays' lengths, as microbench's
// array_length_decr does, tab.length = i: one shorter at a time, to none,
// to the same, of dense arrays and holey ones, of numbers and objects; and
// then what Go writes: a longer length, one that is no length -- a
// fraction, a negative, NaN, 2^32, -0, a string -- one not writable, in
// sloppy code and in strict, a sparse array's, one with a property of its
// own, a receiver that only inherits from an array.
const lengthSetup = `
	function shorten(a) { for (let i = a.length; i-- > 0;) a.length = i; return a }
	function filled(n, holes) { const a = []; for (let i = 0; i < n; i++) if (!holes || i % 3) a[i] = holes === 2 ? { i } : i; return a }
	function decr(n) { let a; for (let j = 0; j < n; j++) a = shorten(filled(20, j % 3)); return a.length + ':' + JSON.stringify(a) }
	function regrow() { const a = shorten(filled(5, 0)); a.length = 3; return a.length + ':' + (0 in a) + ':' + a[1] + ':' + JSON.stringify(a) }
	function set(a, v) { a.length = v; return a.length }
	function values() {
		let out = []
		for (const v of [3, 0, 5, 10, 2.5, -1, NaN, 4294967296, -0, '2', Infinity]) {
			const a = filled(5, 0)
			try { out.push(set(a, v) + '/' + a.join('')) } catch (e) { out.push(e.constructor.name) }
		}
		return out.join(',')
	}
	function frozen() { const a = Object.freeze([1, 2, 3]); return set(a, 1) + ':' + a.join('') }
	function strictFrozen() { 'use strict'; const a = Object.freeze([1, 2, 3]); try { a.length = 1; return 'no' } catch (e) { return e.constructor.name + a.length } }
	function sparse() { const a = [1, 2, 3]; a[1000] = 4; return set(a, 2) + ':' + (1000 in a) + ':' + a.join('') }
	function named() { const a = [1, 2, 3]; a.foo = 'f'; return set(a, 1) + ':' + a.foo + ':' + a.join('') }
	function inherited() { const p = [1, 2, 3], o = Object.create(p); o.length = 1; return p.length + ':' + o.length + ':' + Object.keys(o).join('') }
	function typed() { const t = new Int8Array(4); t.length = 1; return t.length }
`

// lengthRounds are lengthSetup's calls and their answers, Node's
// (v26.10.0).
var lengthRounds = [][2]string{
	{`decr(30)`, `0:[]`},
	{`regrow()`, `3:false:undefined:[null,null,null]`},
	{`values()`, `3/012,0/,5/01234,10/01234,RangeError,RangeError,RangeError,RangeError,0/,2/01,RangeError`},
	{`frozen()`, `3:123`},
	{`strictFrozen()`, `TypeError3`},
	{`sparse()`, `2:false:12`},
	{`named()`, `1:f:1`},
	{`inherited()`, `3:1:length`},
	{`typed()`, `4`},
}

// An array's length written to one no longer than it is is written
// natively (mir's lengthWrite): each answer is Node's and the
// interpreter's, shorten's writes call no Go, and the elements past the
// length are cleared, as setArrayLength clears them, so that they hold
// nothing. While the collector marks, Go writes them.
func TestJITLengthWrites(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		var flag *uint8
		if marking {
			flag = jitMarkingForTest(t)
		}
		var rounds []string
		for range 6 {
			for _, r := range lengthRounds {
				rounds = append(rounds, r[0])
			}
		}
		r := jitAgreesForTest(t, lengthSetup, rounds...)
		for _, round := range lengthRounds {
			v, err := r.Run(compileForTest(t, "''+("+round[0]+")"))
			if err != nil {
				t.Fatalf("%s: %v", round[0], err)
			}
			if got := v.String().Go(); got != round[1] {
				t.Errorf("marking %v: %s = %s, want %s", marking, round[0], got, round[1])
			}
		}
		jitNativeForTest(t, r, "shorten")
		if _, err := r.Run(compileForTest(t, `var arr = filled(50, 2)`)); err != nil {
			t.Fatal(err)
		}
		v, calls := jitCountGoCalls(t, r, `shorten(arr)`)
		if marking {
			*flag = 0
			if calls < 50 {
				t.Errorf("while marking, shorten called Go %d times for 50 writes", calls)
			}
		} else if calls != 0 {
			t.Errorf("shorten called Go %d times", calls)
		}
		a := v.Object()
		if n := len(a.elemsOf()); n != 0 {
			t.Fatalf("shortened to %d elements", n)
		}
		for i, e := range a.elemsOf()[:cap(a.elemsOf())] {
			if e.num != 0 || e.ref != nil {
				t.Errorf("marking %v: element %d past the length holds %#x %v", marking, i, *(*uint64)(unsafe.Pointer(&e.num)), e.ref)
				break
			}
		}
	}
}
