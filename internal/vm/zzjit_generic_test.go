//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"testing"
)

// A call that has met more functions than it calls natively
// (jitCallTargets) calls the others natively too, through their links
// (ssa.CallSite's Generic): each with its own parameters and locals --
// fewer arguments than parameters, more, none -- a method call's receiver,
// a sloppy function's receiver an object, a function that makes calls in
// tail position, one that throws, one that leaves native code at a getter,
// and those a generic call does not make, which Go does: an arrow that
// reads this, a sloppy function called without a receiver, a generator,
// one that reads arguments, and the functions the JIT does not compile --
// rest parameters, defaults, a destructured parameter -- whose frames
// native code does not make. Each answer is the interpreter's, in every
// tier and under every stress setting.
func TestJITGenericCalls(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const src = `
		const log = []
		function each(a, f) { let s = 0; for (let i = 0; i < a.length; i++) { const r = f(a[i], i); s += typeof r === 'number' ? r : String(r).length } return s }
		function eachOn(os, a) { let s = 0; for (let j = 0; j < os.length; j++) { const o = os[j]; for (let i = 0; i < a.length; i++) s += o.m(a[i]) } return s }
		const fs = [
			x => x + 1,
			(x, i) => x * i,
			() => 7,
			(x, i, a, b, c) => (a === undefined) + (b === undefined) + x,
			function (x) { let a = 1, b = 2, c = 3, d = 4; return x + a + b + c + d },
			function (x) { return x > 3 ? fact(x) : x },
			x => { if (x === 13) throw new RangeError('thirteen'); return x },
			x => ({ get v() { return x * 2 } }).v,
			function* (x) { yield x },
			function (x) { return arguments.length + x },
			(x, ...rest) => rest.length + x,
			(x, y = 5) => x + y,
			function ({ v } = { v: 2 }, x) { return v + (x === undefined) },
		]
		function fact(n, acc = 1) { return n <= 1 ? acc : fact(n - 1, acc * n) }
		const nums = [1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12]
		for (let r = 0; r < 30; r++) for (const f of fs) log.push(each(nums, f))
		try { each([1, 13, 2], fs[6]) } catch (e) { log.push(e.name + ':' + e.message) }
		function sloppy(x) { return this === undefined ? -1 : typeof this === 'object' ? x + (this.k | 0) : -2 }
		function strict(x) { 'use strict'; return this === undefined ? -1 : x }
		const os = [
			{ k: 3, m(x) { return x + this.k } }, { k: 4, m(x) { return x * this.k } }, { k: 5, m(x) { return this.k - x } },
			{ m(x) { return x } }, { k: 1, m(x) { return this.k === 1 ? 1 : 0 } }, { k: 2, m(x) { const t = this; return t.k + x } },
			{ k: 6, m: sloppy }, { m: strict }, { k: 7, m(x, ...r) { return r.length + x + this.k } }, { m: (x, y = 2) => x + y },
		]
		for (let r = 0; r < 30; r++) log.push(eachOn(os, nums), each(nums, sloppy), each(nums, strict))
		const self = { k: 2, run() { return each(nums, x => x * this.k) } }
		for (let r = 0; r < 30; r++) log.push(self.run())
		log.join()`
	jitDifferential(t, src)
}

// Vector.forEach-like code, its callback a closure made at each of many
// call sites, more of them than a call calls natively: the callbacks run
// natively, called by forEach's code through their links, not by Go. They
// had left for Go at each call, and AWFY's DeltaBlue's planner ran its
// constraints' callbacks in the tree tier.
func TestJITGenericCallsNative(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	r := New(Config{JIT: true})
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	jitFewCallsForTest(r)
	const setup = `
		function forEach(a, f) { for (let i = 0; i < a.length; i++) f(a[i]) }
		const a = []
		for (let i = 0; i < 100; i++) a.push({ v: i })
		let s = 0
		function f1() { forEach(a, e => { s += e.v }) }
		function f2() { forEach(a, e => { s -= e.v }) }
		function f3() { forEach(a, e => { s += 2 * e.v }) }
		function f4() { forEach(a, e => { s ^= e.v }) }
		function f5() { forEach(a, e => { s += e.v & 1 }) }
		function f6() { forEach(a, e => { s += e.v >> 1 }) }
		function f7() { forEach(a, e => { s -= e.v & 3 }) }
		function round() { f1(); f2(); f3(); f4(); f5(); f6(); f7(); return s }`
	if _, err := r.Run(compileForTest(t, setup)); err != nil {
		t.Fatal(err)
	}
	for range 200 {
		if _, err := r.Run(compileForTest(t, `round()`)); err != nil {
			t.Fatal(err)
		}
	}
	e := jitNativeForTest(t, r, "forEach")
	entries, goCalls, hosts := e.ssaStats.entries, e.ssaStats.goCalls, e.ssaStats.hosts
	if _, err := r.Run(compileForTest(t, `for (let k = 0; k < 20; k++) round()`)); err != nil {
		t.Fatal(err)
	}
	// 20 rounds of seven forEach calls of 100 elements each, natively.
	if n := e.ssaStats.entries - entries; n < 140 {
		t.Fatalf("forEach ran natively %d times of 140", n)
	}
	if n := e.ssaStats.goCalls - goCalls + e.ssaStats.hosts - hosts; n > 140 {
		t.Errorf("forEach called its callbacks through Go %d times in 14,000 calls", n)
	}
}

// An arrow that reads this, called as a method -- as AWFY's Richards'
// tasks call their handlers, this.fn(work) -- is called natively, given
// the this it captured where it was made, from the closures made anew at
// each call of their maker too; and its calls in tail position are made
// natively. Go had made every such call, entering the arrow's code from
// Go each time, and before that refused the arrow outright.
func TestJITArrowCallsNative(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	r := New(Config{JIT: true})
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	const setup = `
		class Sched {
			constructor() { this.n = 0 }
			bump(w) { this.n += w; return w }
			make(k) { return { k, fn: (w) => w === 0 ? this.n : this.bump(w + k) } }
		}
		const s = new Sched()
		const tasks = [s.make(1), s.make(2), s.make(3)]
		function runTask(t, w) { return t.fn(w) }
		function round(n) { let x = 0; for (let i = 0; i < n; i++) x += runTask(tasks[i % 3], i & 3); return x }`
	if _, err := r.Run(compileForTest(t, setup)); err != nil {
		t.Fatal(err)
	}
	for range 50 {
		if _, err := r.Run(compileForTest(t, `round(300)`)); err != nil {
			t.Fatal(err)
		}
	}
	e := r.jitEntryOf(r.global.getOwn(r.atoms.intern("runTask")).value().Object().fn().closure)
	if e == nil || e.ssa == nil {
		t.Fatalf("runTask has no code: %+v", e)
	}
	goCalls, hosts := e.ssaStats.goCalls, e.ssaStats.hosts
	if _, err := r.Run(compileForTest(t, `round(3000)`)); err != nil {
		t.Fatal(err)
	}
	if n := e.ssaStats.goCalls - goCalls + e.ssaStats.hosts - hosts; n > 10 {
		t.Errorf("runTask called the arrows through Go %d times in 3,000 calls", n)
	}
}

// A call past the functions it calls natively calls arrows natively too,
// through their links, as V8's generic Call does any function: one whose
// body is a call in tail position, e => add(e), as DeltaBlue's planner's
// (c) => c.execute(), and one that reads the this it captured, given it.
// Go had made each such call, the arrows no generic call's, and the
// arrow, entered from Go at every call, was judged slow and left to the
// tree tier. One that shares a derived constructor's this, D's, Go still
// calls, its this the binding.
func TestJITGenericCallsOfArrows(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	r := New(Config{JIT: true})
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	jitFewCallsForTest(r)
	const setup = `'use strict'
		function forEach(a, f) { for (let i = 0; i < a.length; i++) f(a[i]) }
		const a = []
		for (let i = 0; i < 100; i++) a.push({ v: i })
		let s = 0
		function add(e) { s += e.v; return s }
		function sub(e) { s -= e.v & 3; return s }
		const o = { k: 3, f6() { forEach(a, e => { s += this.k + e.v }) } }
		function f1() { forEach(a, e => { s += 2 * e.v }) }
		function f2() { forEach(a, e => { s ^= e.v }) }
		function f3() { forEach(a, e => { s += e.v & 1 }) }
		function f4() { forEach(a, e => { s += e.v >> 1 }) }
		function f5() { forEach(a, e => add(e)) }
		function f7() { forEach(a, e => sub(e)) }
		class B { constructor() { this.k = 2 } }
		class D extends B { constructor() { const f = e => { s += this.k }; super(); forEach(a, f) } }
		function warm() { f1(); f2(); f3(); f4(); return s }
		function round() { f1(); f2(); f3(); f4(); f5(); o.f6(); f7(); new D(); return s }`
	if _, err := r.Run(compileForTest(t, setup)); err != nil {
		t.Fatal(err)
	}
	// The four a call calls natively first, the arrows past them after.
	for range 100 {
		if _, err := r.Run(compileForTest(t, `warm()`)); err != nil {
			t.Fatal(err)
		}
	}
	for range 100 {
		if _, err := r.Run(compileForTest(t, `round()`)); err != nil {
			t.Fatal(err)
		}
	}
	e := jitNativeForTest(t, r, "forEach")
	goCalls, hosts := e.ssaStats.goCalls, e.ssaStats.hosts
	v, err := r.Run(compileForTest(t, `for (let k = 0; k < 20; k++) round(); s`))
	if err != nil {
		t.Fatal(err)
	}
	// 20 rounds of eight forEach calls of 100 elements each; D's arrow's
	// Go's, 2,000.
	if n := e.ssaStats.goCalls - goCalls + e.ssaStats.hosts - hosts; n > 2000+160 {
		t.Errorf("forEach called its callbacks through Go %d times in 16,000 calls", n)
	}
	want := New(Config{})
	t.Cleanup(func() { want.Close(); want.ReleaseClosed() })
	if _, err := want.Run(compileForTest(t, setup)); err != nil {
		t.Fatal(err)
	}
	wv, err := want.Run(compileForTest(t, `for (let k = 0; k < 100; k++) warm(); for (let k = 0; k < 120; k++) round(); s`))
	if err != nil {
		t.Fatal(err)
	}
	if v.Number() != wv.Number() {
		t.Fatalf("s is %v, the interpreter's %v", v.Number(), wv.Number())
	}
}
