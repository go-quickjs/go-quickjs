//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime/debug"
	"testing"
	"unsafe"
)

// prologueCallSetup's callees have parameter lists with defaults and
// patterns, called natively by loops: wide, too long to inline, binds an
// object pattern with defaults of its own and a default that reads the
// parameters before it; narrow, small enough to inline, a pattern's
// default; hasCall's default calls a function that throws at 49, whose
// throw its caller catches; tdz's default reads the parameter after it,
// in the dead zone. Each is called with every argument missing, passed,
// undefined, and with sources of other shapes -- a getter's object, an
// object with more properties -- after the loops' code was compiled for
// the first: the callee's code leaves inside its prologue, where the
// interpreter goes on binding the parameters from the call's arguments.
const prologueCallSetup = `
	var log = []
	function boom(v) { if (v % 50 === 49) throw new Error('boom ' + v); log.push(v); return v * 3 }
	function wide(a, b = a + 1, { x, y = b * 2 } = { x: a }, c = x + y) {
		let s = a + b + x + y + c
		s = s * 3 + (a & 7) - (b | 1) + (x ^ 3) - (y >> 1) + (c << 1)
		s = s * 2 + (a > b ? a : b) + (x < y ? y : x)
		s = (s + a * b + x * y + c * a) | 0
		return s
	}
	function narrow({ x, y } = { x: 1, y: 2 }) { return x * 10 + y }
	function hasCall(a, b = boom(a)) { return a + b }
	function tdz(a = b, b = 1) { return a + b }
	function wide17(a, { x, y = 2 } = { x: a + 1 }) { return a + x + y }
	function wides17(n, src) {
		let s = 0
		for (let i = 0; i < n; i++) s = (s + wide17(i, undefined, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0) + wide17(i, src, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2)) | 0
		return s
	}
	var getter = { get x() { return 7 }, y: 5 }
	var wider = { w: 0, x: 4, y: 9 }
	function wides(n, src) {
		let s = 0
		for (let i = 0; i < n; i++) {
			s = (s + wide(i)) | 0
			s = (s + wide(i, 2)) | 0
			s = (s + wide(i, undefined, { x: 3 })) | 0
			s = (s + wide(i, 1, src, 8)) | 0
		}
		return s
	}
	function narrows(n, src) {
		let s = 0
		for (let i = 0; i < n; i++) s = (s + narrow() + narrow({ x: i, y: 1 }) + narrow(src)) | 0
		return s
	}
	function calls(n) {
		let s = 0, errs = 0
		for (let i = 0; i < n; i++) {
			try { s = (s + hasCall(i) + hasCall(i, 1)) | 0 } catch (e) { errs++ }
		}
		return s + ':' + errs + ':' + log.length
	}
	function tdzs(n) {
		let s = 0, errs = 0
		for (let i = 0; i < n; i++) {
			s = (s + tdz(i, i)) | 0
			if (i % 10 === 9) try { tdz(undefined, i) } catch (e) { errs += e instanceof ReferenceError ? 1 : 100 }
		}
		return s + ':' + errs
	}`

// A callee whose parameter list has a default or a pattern is called
// natively and inlined: its frame, which a native caller's call or the
// caller's inlining makes, has the call's arguments nowhere but in the
// states inside its prologue, which keep them past their operands
// (ir.Program's PrologueArgs), so that a frame Go makes of it there --
// at a shape the code was not compiled for, a getter a pattern reads, a
// default's call that throws, a parameter read in its dead zone -- is
// given them, and the interpreter goes on binding the parameters from
// them. Such callees -- 404 of the functions of npm's own code native
// callers and inlining could take -- had been called by Go. Each answer
// is the interpreter's, under every exit, and the callees are called
// natively or inlined.
func TestJITPrologueCallees(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	// Native calls are Go's while the collector marks.
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	inlined := jitInlinedForTest(t)
	jitNoInlineForTest(t, "wide")
	both := jitBothForTest(t, prologueCallSetup)
	for range 3 {
		both.agree(t, `wides(300, { x: 2 })`, `narrows(300, { x: 3, y: 4 })`, `calls(120)`, `tdzs(100)`)
	}
	r := both.jit
	for _, name := range []string{"wides", "narrows", "calls", "tdzs"} {
		jitNativeForTest(t, r, name)
	}
	// tdz's every tenth call throws, which has its native callers call it
	// through Go for a while (notNative).
	for _, name := range []string{"wide", "hasCall"} {
		e := jitNativeOrCalleeForTest(t, r, name)
		if e.ssa.CallEntry() == 0 {
			t.Fatalf("%s has no call entry", name)
		}
		in := e.link.in
		both.agree(t, `wides(300, { x: 2 })`, `calls(120)`, `tdzs(100)`)
		if e.link.in-in < 200 {
			t.Errorf("%s was called natively %d times", name, e.link.in-in)
		}
	}
	narrow := uintptr(unsafe.Pointer(jitGlobalClosureForTest(r, "narrow")))
	if !jitInlinedHas(inlined["narrows"], narrow) {
		t.Errorf("narrows inlined %v, not narrow (%#x)", inlined["narrows"], narrow)
	}
	// Sources of other shapes, and a getter's: exits inside the prologues,
	// a native callee's and an inlined one's.
	for _, src := range []string{`getter`, `wider`, `{ y: 1 }`, `{ x: 'a', y: 'b' }`} {
		both.agree(t, `wides(200, `+src+`)`, `narrows(200, `+src+`)`, `wides(200, { x: 2 })`, `narrows(200, { x: 3, y: 4 })`)
	}
	both.agree(t, `calls(300)`, `tdzs(300)`, `log.length`)
}

// A native call of a callee with a prologue whose code is gone -- dropped,
// or no longer called natively -- with more operands than a call of Go
// takes (abi.MaxGoArgs), has Go run it from its start (abi.ExitEnter), its
// frame given the call's arguments from its parameters' slots
// (jitEnterArgs).
func TestJITPrologueEnter(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	jitNoInlineForTest(t, "wide17")
	both := jitBothForTest(t, prologueCallSetup)
	both.agree(t, `wides17(300, { x: 2 })`, `wides17(300, { x: 2 })`)
	r := both.jit
	e := jitNativeOrCalleeForTest(t, r, "wide17")
	if in := e.link.in; in < 300 {
		t.Fatalf("wide17 was called natively %d times", in)
	}
	e.notNative, e.link.entry, e.link.body, e.nativeRetry = true, 0, 0, 0
	entered := r.jit.entered
	both.agree(t, `wides17(300, { x: 2 })`, `wides17(30, getter)`)
	if r.jit.entered == entered {
		t.Fatal("Go never ran wide17 from its start")
	}
}

// jitInlinedHas reports whether the closures a compile inlined include cl.
func jitInlinedHas(inlined []uintptr, cl uintptr) bool {
	for _, c := range inlined {
		if c == cl {
			return true
		}
	}
	return false
}

// Exits at every point of a callee's prologue: compiled at its first call,
// its guards failing as they meet each new shape, under the stress
// settings' polls and deoptimizations too.
func TestJITPrologueCalleesAgree(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	jitAgreesForTest(t, prologueCallSetup,
		`wides(50, { x: 2 })`, `narrows(50, { x: 3, y: 4 })`, `calls(120)`, `tdzs(100)`,
		`wides(50, getter)`, `narrows(50, getter)`, `wides(50, wider)`, `narrows(50, wider)`,
		`wides(50, { x: 2 })`, `narrows(50, { x: 3, y: 4 })`, `calls(120)`, `tdzs(100)`, `log.join()`)
}

// The arguments a frame Go makes inside a callee's prologue is given are
// those the exit's state kept (ssa.FrameState's Args), not whatever the
// VM's stack held past the callee's operands: here a function the
// interpreter alone runs, for its with statement, fills that stack with nines
// before every call, and the callee leaves inside its prologue at each
// source of a shape it had not met, its defaults then bound from the
// arguments the state kept.
func TestJITPrologueArgsFromState(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	const setup = `
		function junk() { with ({}) {} return [9, 9, 9, 9, 9, 9, 9, 9, 9, 9].length }
		function pick(a, b = a * 2, { x, y = b } = { x: a }, c = y + 1) { return a * 1000 + b * 100 + x * 10 + y + c / 100 }
		function picks(n, src, a) {
			let s = 0
			for (let i = 0; i < n; i++) {
				s += junk() + pick(i & 3)
				s += junk() + pick(i % 50 === 49 ? a : i & 3, undefined, src)
			}
			return s
		}`
	jitNoInlineForTest(t, "pick")
	both := jitBothForTest(t, setup)
	both.agree(t, `picks(200, { x: 1 }, 1)`, `picks(200, { x: 1 }, 2)`)
	jitNativeForTest(t, both.jit, "picks")
	if e := jitNativeOrCalleeForTest(t, both.jit, "pick"); e.link.in < 300 {
		t.Errorf("pick was called natively %d times", e.link.in)
	}
	// A string, which b's default, a * 2, was compiled to take only
	// numbers for: an exit at the multiplication, the arguments past the
	// operand it pushed.
	for _, src := range []string{`{ y: 2, x: 3 }`, `{ get x() { return 4 } }`, `{ w: 1, x: 5, y: 6 }`, `{ x: 7, z: 0 }`} {
		both.agree(t, `picks(100, `+src+`, '3')`, `picks(100, `+src+`, 3)`)
	}
}

// A call that has met more functions than it is compiled for calls any
// other through its link (ssa.CallSite's Generic), callees with defaults
// and patterns among them: their frames are their arguments in their
// parameters, which their prologues bind them from, and a frame Go makes
// of one -- one with no code yet (abi.ExitEnter), one that leaves inside
// its prologue -- is given them.
func TestJITPrologueGenericCalls(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	both := jitBothForTest(t, `
		var visitors = [
			function (n, k = 1) { return n * k },
			function (n, { w } = { w: 2 }) { return n + w },
			function (n, k = n + 3) { return k - n },
			function ({ v } = { v: 4 }, n) { return v },
			function (n, k = 5, j = k * 2) { return n + k + j },
			function (n, { w, z = 6 } = {}) { return (w || 0) + z },
			function (n, k = 7) { return k },
			function (n, { w: { q } = { q: 8 } } = {}) { return q + n },
		]
		function visit(m, arg) {
			let s = 0
			for (let i = 0; i < m; i++) s += visitors[i & 7](i, arg)
			return s
		}`)
	for range 3 {
		both.agree(t, `visit(400)`, `visit(400, undefined)`, `visit(400, { w: 1, v: 2, z: 3 })`, `visit(100, 2)`)
	}
	both.agree(t, `visit(200, { get w() { return 9 } })`, `visit(200, { w: { q: 1 } })`, `visit(400)`)
	r := both.jit
	v, err := r.Run(compileForTest(t, `visitors`))
	if err != nil {
		t.Fatal(err)
	}
	native := 0
	for _, f := range v.Object().elemsOf() {
		if e := r.jit.hint(f.Object().fn().closure); e != nil && e.link != nil && e.link.in != 0 {
			native++
		}
	}
	if native < 6 {
		t.Errorf("%d of the visitors were called natively", native)
	}
}
