//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"fmt"
	"testing"
)

// forwardSetup are forwarders, f.apply(x, arguments) in functions whose
// arguments object is never made (bytecode's ForwardsArguments), of every
// kind of target and receiver, each called with fewer arguments than its
// target's parameters, as many, more, and more than a context holds
// (abi.MaxGoArgs), by Go and by native callers.
const forwardSetup = `
	function t0() { return this === undefined ? 'u' : this === globalThis ? 'g' : typeof this === 'object' ? 'o' + (this.k || '') : typeof this }
	function t2(a, b) { return a + ':' + b }
	function t6(a, b, c, d, e, f) { return [a, b, c, d, e, f].join('/') }
	function tArgs(a) { let s = arguments.length + '|'; for (let i = 0; i < arguments.length; i++) s += arguments[i] + ','; return s + a }
	function tStrictArgs(a) { 'use strict'; a = 'x'; return arguments.length + ':' + arguments[0] + ':' + a }
	function tMapped(a, b) { arguments[0] = 'm'; b = 'n'; return a + b + arguments[1] + arguments.length }
	function tThrow(a) { if (a > 2) throw new Error('thrown ' + a); return a }
	function tStack() { return new Error('s').stack.split('\n').slice(0, 3).map(l => l.replace(/\(.*:(\d+):(\d+)\)/, '$1:$2').trim()).join(';') }
	function tKind(a) { return a.v + 1 }
	function* tGen(a, b) { yield a; yield b; yield arguments.length }
	async function tAsync(a, b) { await null; return a * b }
	const tArrow = (a, b) => a + '+' + b
	const bound = t2.bind(null, 'B')

	function f0() { return t0.apply(this, arguments) }
	function f2() { return t2.apply(this, arguments) }
	function f6() { return t6.apply(this, arguments) }
	function fArgs() { return tArgs.apply(this, arguments) }
	function fStrictArgs() { return tStrictArgs.apply(this, arguments) }
	function fMapped() { return tMapped.apply(this, arguments) }
	function fThrow() { return tThrow.apply(this, arguments) }
	function fStack() { return tStack.apply(this, arguments) }
	function fStrict() { 'use strict'; return t0.apply(this, arguments) }
	function fGen() { return tGen.apply(this, arguments) }
	function fAsync() { return tAsync.apply(this, arguments) }
	function fArrow() { return tArrow.apply(this, arguments) }
	function fBound() { return bound.apply(this, arguments) }
	function fMax() { return Math.max.apply(Math, arguments) }
	var target = t2
	function fVar() { return target.apply(this, arguments) }
	function fKind() { const k = this.k * 2; const r = tKind.apply(this, arguments); return r + k }
	function fTwice() { const a = t2.apply(this, arguments); const b = t6.apply(null, arguments); return a + '&' + b }
	function fOwn() { return t2.apply(this, arguments) }
	// The forwarding function assigns a parameter its mapped object
	// aliases: its forwards are the interpreter's, as before.
	function fAssigns(a) { a = 'A'; return t2.apply(this, arguments) }
	function fact(n) { return n <= 1 ? 1 : n * fFact(n - 1) }
	function fFact() { return fact.apply(this, arguments) }
	const wrap = f => function () { return f.apply(this, arguments) }
	const poly = [wrap((a, b) => a + b), wrap((a, b) => a - b), wrap(function (a, b) { return a * b }), wrap(Math.min), wrap(t2)]
	function Point() { this.init.apply(this, arguments) }
	Point.prototype.init = function (x, y) { this.x = x; this.y = y }
	function Base(x, y) { this.x = x; this.y = y }
	function Derived() { Base.apply(this, arguments); this.z = arguments.length === undefined }
	const obj = { k: 'K', f0, f2, fArgs }

	function calls(i) {
		let s = ''
		s += f0() + f0(1) + obj.f0(1, 2) + fStrict() + fStrict.call(5) + fStrict.call(obj)
		s += f2() + f2(i) + f2(i, 2) + f2(i, 2, 3) + f6(1, 2, 3) + f6(1, 2, 3, 4, 5, 6, 7, 8)
		s += fArgs() + fArgs(i) + obj.fArgs(1, 2, 3) + fArgs(1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16)
		s += fArgs(1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18)
		s += fStrictArgs(i) + fStrictArgs() + fMapped(i, 2, 3) + fMapped(i)
		s += fArrow(i, 3) + fBound(i, 4) + fMax(i, 7, 3) + fMax() + fVar(i, 9) + fTwice(i, 'q') + fAssigns(i, 5)
		s += [...fGen(i, 'g')].join() + [...fGen()].join()
		s += fFact(5) + poly[i % poly.length](i, 3) + poly[(i + 1) % poly.length](i)
		const p = new Point(i, 2), d = new Derived(i, 3)
		s += p.x + p.y + d.x + d.y + d.z
		return s
	}
	function loop(n) {
		let s = ''
		for (let i = 0; i < n; i++) s = calls(i)
		return s
	}
	function throws(n) {
		let s = ''
		for (let i = 0; i < n; i++) { try { s += fThrow(i) } catch (e) { s += e.message } }
		return s
	}
	function stacks(n) {
		let s = ''
		for (let i = 0; i < n; i++) s = fStack(i)
		return s
	}
	function kinds(n) {
		let s = ''
		const o = { k: 1, fKind }
		for (let i = 0; i < n; i++) { if (i === n - 3) o.k = '3'; s += o.fKind({ v: i === n - 2 ? 's' : i }) + ',' }
		return s
	}
	var log = []
	function asyncs(n) {
		for (let i = 0; i < n; i++) fAsync(i, 2).then(v => log.push(v))
		return n
	}
`

// A forwarding function's apply_arguments is a native call of what it
// forwards to, with the arguments its frame was given (ssa.CallSite's
// Forward), or Go's from native code: each answer is the interpreter's --
// for every count of arguments, below, at and past the target's
// parameters and past a context's room for them; a target that reads its
// arguments, mapped and not, that throws, whose stack runs through the
// forwarder, a generator, an async function, an arrow, a bound function, a
// built-in, one replaced, a recursion through the forwarder, polymorphic
// forwarders, constructions; a receiver of every kind; a deoptimization in
// the forwarder around the call -- the forwarder called by Go and natively,
// with the collector idle and marking, and under budgets and
// deoptimizations.
func TestJITForwardingCalls(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	rounds := []string{"loop(40)", "loop(3)", "calls(7)", "f2(1, 2) + f0() + fArgs(1, 2) + fStrict()",
		"throws(6)", "throws(2)", "stacks(30)", "fStack()", "kinds(40)", "kinds(5)",
		"(target = t6, fVar(1, 2, 3))", "loop(5)", "(target = (a, b) => a * b, fVar(3, 4))", "(target = t2, loop(5))",
		"asyncs(30)", "log.join()",
		"(t2.apply = function (x, a) { return 'own ' + a.length }, f2(1, 2) + loop(3))",
		"(delete t2.apply, loop(5))"}
	for _, stress := range []jitStressConfig{{}, {threshold: true, budget: 3, deopt: 7}} {
		for _, marking := range []bool{false, true} {
			t.Run(fmt.Sprintf("stress=%v/marking=%v", stress != jitStressConfig{}, marking), func(t *testing.T) {
				if marking {
					jitMarkingForTest(t)
				}
				r := jitAgreesForwardForTest(t, stress, forwardSetup, rounds...)
				if stress == (jitStressConfig{}) {
					for _, name := range []string{"f2", "fArgs", "f6", "fThrow", "fStack", "fVar", "fTwice"} {
						jitNativeOrCalleeForTest(t, r, name)
					}
				}
			})
		}
	}
}

// jitAgreesForwardForTest is jitAgreesStressForTest with the jobs each
// round queued run after it, on both runtimes.
func jitAgreesForwardForTest(t *testing.T, stress jitStressConfig, setup string, rounds ...string) *Runtime {
	t.Helper()
	want := New(Config{})
	t.Cleanup(func() { want.Close(); want.ReleaseClosed() })
	r := jitRuntimeForTest(t, Config{JIT: true})
	r.jitStress = stress
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	run := func(rt *Runtime, src string) (string, error) {
		v, err := rt.Run(compileForTest(t, src))
		if err == nil {
			err = rt.runMicrotasks()
		}
		if err != nil {
			return "", err
		}
		return v.String().Go(), nil
	}
	for i, src := range rounds {
		src = "''+(" + src + ")"
		wv, werr := run(want, src)
		gv, gerr := run(r, src)
		if (werr == nil) != (gerr == nil) {
			t.Fatalf("round %d (%s): error %v, interpreter's %v", i, src, gerr, werr)
		}
		if werr != nil {
			if got, want := gerr.Error(), werr.Error(); got != want {
				t.Fatalf("round %d (%s): error %q, interpreter's %q", i, src, got, want)
			}
			continue
		}
		if gv != wv {
			t.Fatalf("round %d (%s): got %s, interpreter %s", i, src, gv, wv)
		}
	}
	return r
}

// A forwarder's call of a function native code calls is native: once
// warm, a loop of its calls by a native caller, and its own loop of
// forwards, make no call of Go; and a forwarder given more arguments than
// a context holds has Go make its forwards, from the frame's.
func TestJITForwardingCallsNative(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitRuntimeForTest(t, Config{JIT: true})
	jitDefaultsForTest(r)
	src := `
		function t2(a, b) { return a + b }
		function f2() { return t2.apply(this, arguments) }
		function own() { let s = 0; for (let i = 0; i < 8; i++) s += t2.apply(this, arguments); return s }
		function loop(n) { let s = 0; for (let i = 0; i < n; i++) s += f2(i, 1) + own(i, 2); return s }
		function many(n) { let s = 0; for (let i = 0; i < n; i++) s += f2(i, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17); return s }
	`
	if _, err := r.Run(compileForTest(t, src)); err != nil {
		t.Fatal(err)
	}
	for range 40 {
		if _, err := r.Run(compileForTest(t, "loop(200)")); err != nil {
			t.Fatal(err)
		}
	}
	names := []string{"f2", "own", "loop"}
	before := make([]uint64, len(names))
	for i, name := range names {
		before[i] = uint64(jitNativeOrCalleeForTest(t, r, name).ssaStats.goCalls)
	}
	v, err := r.Run(compileForTest(t, "''+loop(1000)"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := v.String().Go(), fmt.Sprint(1000*999/2+1000+8*(1000*999/2+2000)); got != want {
		t.Fatalf("loop(1000) = %s, want %s", got, want)
	}
	for i, name := range names {
		if n := uint64(jitNativeOrCalleeForTest(t, r, name).ssaStats.goCalls) - before[i]; n != 0 {
			t.Errorf("%s: %d calls of Go for loop(1000)'s forwards, want 0", name, n)
		}
	}
	if v, err := r.Run(compileForTest(t, "''+many(50)")); err != nil || v.String().Go() != fmt.Sprint(50*49/2+50) {
		t.Fatalf("many(50) = %v, %v", v, err)
	}
}

// forwardInlineSetup are forwarders a loop calls, plainly and as methods,
// sloppy and strict, which its code inlines with the functions their own
// code learned they forward to: their forwards are calls with the loop's
// arguments, as V8's are where it knows the argument count.
const forwardInlineSetup = `
	function t2(a, b) { return a * 10 + (b === undefined ? 1 : b) + (this === undefined ? 1000 : this.k === 1 ? 2000 : 0) }
	function tStrict(a, b) { 'use strict'; return a * 100 + (b === undefined ? 3 : b) + (this === undefined ? 5000 : 0) }
	function tThrow(a) { if (a === 7) throw new RangeError('at ' + a); return a * 2 }
	function tKind(a) { return a.v + 1 }
	function f2() { return t2.apply(this, arguments) }
	function fStrict() { return tStrict.apply(this, arguments) }
	// Strict, the forward kept out of tail position, where the compiler
	// makes a call of the arguments object.
	function sStrict() { 'use strict'; const r = tStrict.apply(this, arguments); return r }
	function sSloppy() { 'use strict'; const r = t2.apply(this, arguments); return r }
	function fThrow() { return tThrow.apply(this, arguments) }
	function fKind() { return tKind.apply(this, arguments) }
	var target = t2
	function fVar() { return target.apply(this, arguments) }
	const o = { k: 1, f2, sStrict }
	function plain(n) {
		let s = 0
		for (let i = 0; i < n; i++) s += fVar(i, 5) + f2(i) + f2(i, 2) + f2(i, 2, 3) + fStrict(i, 1) + sStrict(i) + o.f2(i, 4) + o.sStrict(i) + sSloppy(i, 3)
		return s
	}
	function throws(n) {
		let s = ''
		for (let i = 0; i < n; i++) { try { s += fThrow(i) } catch (e) { s += e.message + e.stack.split('\n').slice(1, 3).map(l => l.trim().split(' ')[1]).join() } }
		return s
	}
	function kinds(n) {
		let s = ''
		for (let i = 0; i < n; i++) s += fKind({ v: i === n - 2 ? 'x' : i }) + ','
		return s
	}
`

// A forwarder a loop calls is inlined in it, with the function its own
// code learned it forwards to inlined in turn, called with the loop's
// arguments (ssa.InlineSite's Forward): each answer the interpreter's --
// fewer arguments than parameters, as many and more, a plain call's
// receiver, sloppy and strict, a method call's, a throw and its stack, a
// deoptimization inside, the function forwarded to replaced -- with the
// collector idle and marking.
func TestJITForwardersInlined(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	inlined := jitInlinedForTest(t)
	for _, stress := range []jitStressConfig{{}, {threshold: true, budget: 3, deopt: 7}} {
		for _, marking := range []bool{false, true} {
			t.Run(fmt.Sprintf("stress=%v/marking=%v", stress != jitStressConfig{}, marking), func(t *testing.T) {
				if marking {
					jitMarkingForTest(t)
				}
				both := jitBothForTest(t, forwardInlineSetup)
				r := both.jit
				r.jitStress = stress
				for range 8 {
					both.agree(t, "plain(200)", "throws(10)", "kinds(30)")
				}
				if stress == (jitStressConfig{}) {
					for _, callee := range []string{"f2", "t2", "fStrict", "tStrict", "sStrict", "fVar"} {
						if !jitInlinedFnForTest(t, r, inlined, "plain", callee) {
							t.Errorf("plain's code does not inline %s", callee)
						}
					}
					hosts := r.jit.hosts
					both.agree(t, "plain(300)")
					if n := r.jit.hosts - hosts; n != 0 {
						t.Errorf("plain left native code %d times", n)
					}
				}
				both.agree(t, "(target = tStrict, plain(5))", "plain(300)", "(target = t2, plain(300))", "throws(10)", "kinds(30)")
			})
		}
	}
}
