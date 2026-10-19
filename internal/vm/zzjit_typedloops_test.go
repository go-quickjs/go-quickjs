//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "testing"

// typedLoopsSetup's functions each make a typed array's forEach, reduce,
// some, every, find or findIndex that native code makes a loop of, as it
// makes an array's -- and then make it meet what the loop leaves for Go: a
// view the callback shrinks, whose elements past its end are undefined; a
// Float16Array, whose elements Go reads; a BigInt64Array, Go's; a
// subclass's instance; a site that meets two types; an array whose
// prototype is a typed array's, which the method throws at.
const typedLoopsSetup = `
	var i32 = new Int32Array([5, -3, 8, 13, 21, -1, 7, 2])
	var f64 = new Float64Array([0.5, 1.25, -2, 3.75, 10])
	var u8 = new Uint8Array([200, 1, 2, 255, 3])
	var thisObj = { k: 3 }
	function eachSum(t) { let s = 0; t.forEach(function eachCb(x, i, a) { s += x * (i + 1) + (a === t ? 1 : 0) }); return s }
	function eachThis(t) { let s = 0; t.forEach(function (x) { s += this.k * x }, thisObj); return s }
	function reduceInit(t) { return t.reduce(function reduceCb(a, x, i) { return a + x * i }, 100) }
	function reduceFirst(t) { return t.reduce((a, x) => a * 2 + x) }
	function someAt(t, n) { let calls = 0; const r = t.some(function someCb(x) { calls++; return x === n }); return r + ':' + calls }
	function everyBelow(t, n) { let calls = 0; const r = t.every((x) => { calls++; return x < n }); return r + ':' + calls }
	function findAt(t, n) { const r = t.find((x, i) => i === n); return String(r) }
	function findIndexOf(t, n) { return t.findIndex(function findCb(x) { return x === n }) }
	function shrink(n) {
		const buf = new ArrayBuffer(32, { maxByteLength: 64 })
		const t = new Int32Array(buf, 0, 8)
		for (let i = 0; i < t.length; i++) t[i] = i + 1
		let s = '', c = 0
		t.forEach((x, i) => { if (i === n) buf.resize(8); s += x + ','; c++ })
		return s + c
	}
	var f16 = typeof Float16Array === 'function' ? new Float16Array([1.5, 2.5, -4]) : new Float32Array([1.5, 2.5, -4])
	var big = new BigInt64Array([1n, 2n, 3n])
	function bigSum() { let s = 0n; big.forEach((x) => { s += x }); return String(s) }
	class Mine extends Int32Array {}
	var mine = new Mine([1, 2, 3])
	function eachMixed(t) { let s = 0; t.forEach((x) => { s += x }); return s }
	function either(t) { let s = 0; t.forEach((x) => { s += x }); return s }
	var fake = Object.setPrototypeOf([1, 2, 3], Int32Array.prototype)
	function onFake() { try { let s = 0; fake.forEach((x) => { s += x }); return s } catch (e) { return e.constructor.name + ': ' + e.message } }
	function eachOrFake(t) { let s = 0; t.forEach((x) => { s += x }); return s }
	function empty() { try { return new Int32Array(0).reduce((a, x) => a + x) } catch (e) { return e.constructor.name + ': ' + e.message } }
	function throwsAt(t, n) { let s = 0; t.forEach((x, i) => { if (i === n) throw new Error('at ' + i); s += x }); return s }
	function undefinedAt(t, n) { let s = 0; t.forEach((x, i) => { if (i === n) notDefined(); s += x }); return s }
`

// A typed array's forEach, reduce, some, every, find or findIndex is a
// loop of the caller's native code, as an array's is (ssa's forEachCall,
// abi.LoopTyped): its typed array's type checked (OpSameObject's
// prototype), each element read as a number (OpElemRead); what the loop
// does not read -- an element past a view shrunk meanwhile, a Float16's --
// Go goes on with from that index, as %TypedArray%.prototype's method
// does (jitLoopFinish). Each answer is the interpreter's, stack traces
// included ("at Int32Array.forEach"), and eachSum's, reduceInit's,
// someAt's and findIndexOf's loops neither leave their code nor call Go.
func TestJITTypedArrayLoops(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	names := []string{"eachSum", "eachThis", "reduceInit", "reduceFirst", "someAt", "everyBelow", "findAt", "findIndexOf",
		"shrink", "bigSum", "eachMixed", "either", "onFake", "eachOrFake", "empty", "throwsAt", "undefinedAt"}
	drivers := ""
	for _, name := range names {
		drivers += "function drive_" + name + "(n, a, b) { let r; for (let i = 0; i < n; i++) r = " + name + "(a, b); return r };"
	}
	rounds := []string{}
	for i := range 8 {
		late := []string{"99", "2"}[i/6]
		rounds = append(rounds, `drive_eachSum(30, i32)`, `drive_eachMixed(30, f64)`, `drive_eachMixed(30, i32)`, `drive_eachThis(30, u8)`,
			`drive_reduceInit(30, i32)`, `drive_reduceFirst(30, f64)`, `drive_someAt(30, i32, 21)`, `drive_someAt(30, i32, 99)`,
			`drive_everyBelow(30, u8, 250)`, `drive_everyBelow(30, u8, 100)`, `drive_findAt(30, f64, 3)`, `drive_findAt(30, f64, 9)`,
			`drive_findIndexOf(30, i32, 7)`, `drive_findIndexOf(30, i32, 99)`, `drive_shrink(30, `+late+`)`,
			`drive_eachMixed(30, f16)`, `drive_bigSum(30)`, `drive_either(30, mine)`, `drive_either(30, i32)`,
			`drive_onFake(30)`, `drive_empty(30)`, `drive_throwsAt(30, i32, 99)`, `drive_undefinedAt(30, u8, 99)`)
	}
	for range 3 {
		rounds = append(rounds,
			`(() => { try { drive_throwsAt(3, i32, 2) } catch (e) { return e.stack } })()`,
			`(() => { try { drive_undefinedAt(3, u8, 2) } catch (e) { return e.stack } })()`)
	}
	rounds = append(rounds, `drive_either(30, f64)`, `drive_either(30, u8)`,
		`drive_eachOrFake(30, i32)`, `drive_eachOrFake(30, i32)`, `(() => { try { return drive_eachOrFake(30, fake) } catch (e) { return e.constructor.name + ': ' + e.message } })()`, `drive_eachOrFake(30, i32)`)
	// forEach replaced on Int32Array.prototype: the loop's check of its
	// function fails, and the call is Go's.
	jitAgreesForTest(t, typedLoopsSetup+drivers, append(rounds, `(Int32Array.prototype.forEach = function (f) { f(100) }, drive_eachSum(30, i32))`)...)
	r := jitAgreesForTest(t, typedLoopsSetup+drivers, rounds...)
	entry := func(name string) *jitEntry {
		for k, x := range r.jit.cache {
			if fn := k.Value(); fn != nil && fn.Name == name {
				return x
			}
		}
		return nil
	}
	if r.jit.loopsFinished == 0 {
		t.Error("Go went on with no loop")
	}
	// Each one's loop native, its callback called by it natively: the
	// call costs Go only its method's read, t.forEach, which native code
	// does not read from a typed array.
	for _, c := range []struct{ fn, cb, run string }{
		{"eachSum", "eachCb", "drive_eachSum(10, i32)"}, {"reduceInit", "reduceCb", "drive_reduceInit(10, i32)"},
		{"someAt", "someCb", "drive_someAt(10, i32, 99)"}, {"findIndexOf", "findCb", "drive_findIndexOf(10, i32, 99)"},
	} {
		e, cb := entry(c.fn), entry(c.cb)
		if e == nil || e.ssa == nil || !e.loops || cb == nil || cb.link == nil {
			t.Errorf("%s makes no loop of its call: %+v", c.fn, e)
			continue
		}
		hosts, in := e.ssaStats.hosts, cb.link.in
		if _, err := r.Run(compileForTest(t, c.run)); err != nil {
			t.Fatal(err)
		}
		if n := cb.link.in - in; n < 80 {
			t.Errorf("%s's callback called natively %d times of 80", c.fn, n)
		}
		if n := e.ssaStats.hosts - hosts; n != 0 {
			t.Errorf("%s left its code %d times", c.fn, n)
		}
	}
	for _, name := range []string{"throwsAt", "undefinedAt", "shrink", "eachOrFake"} {
		if e := entry(name); e == nil || e.ssa == nil || !e.loops {
			t.Errorf("%s makes no loop of its call: %+v", name, e)
		}
	}
}
