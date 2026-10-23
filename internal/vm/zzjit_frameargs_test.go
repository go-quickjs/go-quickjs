//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime/debug"
	"testing"
)

// frameArgsSetup's functions read their arguments past their parameters'
// slots, from the frame Go entered their code with: sum's rest parameter;
// rest's after a parameter; count's arguments object, read by index and
// counted; strictArgs' unmapped object, made and read as an object;
// mapped's, a sloppy function's of simple parameters it never assigns,
// read by index; noParams', a sloppy function's of none, made and read as
// an object, as EarleyBoyer's sc_list makes it. Each has a loop, and Go
// makes the arguments' operations at exits, native code going on after
// them.
const frameArgsSetup = `
	function sum(...xs) { let s = 0; for (let i = 0; i < xs.length; i++) s += xs[i]; return s }
	function rest(k, ...xs) { let s = 0; for (const x of xs) s += x * k; return s + xs.length }
	function count() { let s = 0; for (let i = 0; i < arguments.length; i++) s += arguments[i]; return s }
	function strictArgs(a, b) { 'use strict'; const o = arguments; let s = 0; for (let i = 0; i < o.length; i++) s += o[i]; a = 10; return s + a + o[0] }
	function mapped(a, b) { let s = 0; for (let i = 0; i < arguments.length; i++) s += arguments[i] * (i + 1); return s + a + b }
	function written(a) { 'use strict'; const o = arguments; o[0] = 9; let s = 0; for (let i = 0; i < 3; i++) s += arguments[0] + arguments.length; return s + a }
	function lengthSet() { 'use strict'; let s = arguments.length; arguments.length = 1; for (let i = 0; i < 3; i++) s += arguments.length * 10 + (arguments[1] === undefined ? 1 : 2); return s }
	function indexSet() { 'use strict'; let s = arguments.length; arguments[0] = 7; for (let i = 0; i < 3; i++) s += arguments[0] * 10 + arguments[1]; return s }
	function noParams() { const a = arguments; let res = null; for (let i = a.length - 1; i >= 0; i--) res = { car: a[i], cdr: res }; let n = 0; for (; res; res = res.cdr) n += res.car; return n }
	function run(n) {
		let s = 0
		for (let i = 0; i < n; i++) {
			s += sum() + sum(i) + sum(i, 1, 2) + rest(2) + rest(2, i, 3) + count() + count(i, 1, 2, 3)
			s += strictArgs(i) + strictArgs(i, 2, 3) + mapped(i) + mapped(i, 2) + mapped(i, 2, 3, 4) + noParams(i, 1, 2) + written(i, 1) + lengthSet(i, 1, 2)
		}
		return s
	}`

// A function's own code reads a rest parameter's array, and its arguments
// object's -- unmapped, or mapped where it aliases nothing native code
// holds -- from the frame of the VM's it entered the code with: the array
// and the object Go makes at host exits (compile's argumentsOwn), and
// arguments[k] and arguments.length are read by Go from native code
// (ssa.GoArgument) while the object is not made -- written's, lengthSet's
// and indexSet's, made and changed, are read from the object. Such
// functions -- 146 and 95 of npm's own code, 186 of Octane's -- had been
// refused whole. A native call of one writes all its arguments to its
// callee's context (ssa.CallSite's FrameArgs), from which Go makes the
// array and the object, and reads arguments[k] and arguments.length, and,
// for a mapped object, which names it, its function object (Function):
// each is called natively, at its call entry. Each answer is the
// interpreter's.
func TestJITFrameArguments(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	both := jitBothForTest(t, frameArgsSetup)
	for range 3 {
		both.agree(t, `run(300)`)
	}
	r := both.jit
	for _, name := range []string{"sum", "rest", "count", "strictArgs", "mapped", "noParams"} {
		e := jitEntryForTest(t, r, name)
		if e.ssa.CallEntry() == 0 {
			t.Errorf("%s has no call entry", name)
		}
		if e.link == nil || e.link.in == 0 {
			t.Errorf("%s was never called natively", name)
		}
	}
	// lengthSet's, made and changed, are the object's.
	for range 3 {
		both.agree(t, `(() => { let s = 0; for (let i = 0; i < 300; i++) s += lengthSet(i, 1, 2) + written(i, 1) + indexSet(i, 2); return s })()`)
	}
	// count's reads of its arguments leave native code nowhere.
	e := jitEntryForTest(t, r, "count")
	hosts := e.ssaStats.hosts
	both.agree(t, `(() => { let s = 0; for (let i = 0; i < 200; i++) s += count(i, 1, 2, 3, 4, 5); return s })()`)
	if e := jitEntryForTest(t, r, "count"); e.ssaStats.hosts-hosts > 2 {
		t.Errorf("count left native code %d times", e.ssaStats.hosts-hosts)
	}
	jitAgreesForTest(t, frameArgsSetup, `run(20)`, `run(40)`)
}

// A sloppy function whose arguments object aliases a parameter it assigns,
// or lets the object out, stays the interpreter's: native code holds the
// parameters as values, which the object would not see.
func TestJITFrameArgumentsAliased(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	both := jitBothForTest(t, `
		function assigns(a) { let s = 0; for (let i = 0; i < 3; i++) { a = a + 1; s += arguments[0] } return s }
		function escapes(a) { let s = 0; const o = arguments; for (let i = 0; i < 3; i++) { o[0] = i; s += a } return s }
		function run(n) { let s = 0; for (let i = 0; i < n; i++) s += assigns(i) + escapes(i); return s }`)
	for range 3 {
		both.agree(t, `run(300)`)
	}
	for _, name := range []string{"assigns", "escapes"} {
		if e := both.jit.jit.entryFor(both.jit.global.getOwn(both.jit.atoms.intern(name)).value().Object().fn().closure.fn); e != nil && e.ssa != nil {
			t.Errorf("%s was compiled", name)
		}
	}
}

// A call in tail position deep in the stack, of the function itself,
// where the function reads its arguments from its frame -- here a
// prologue's, which binds m from them -- is Go's (ir.Program's FrameArgs),
// not a jump to the code's start, which would leave the frame of the VM's
// the first call's arguments: k's default, where n is a string, leaves
// native code inside the prologue, and the interpreter binds m from the
// frame's arguments, the last call's.
func TestJITTailCallKeepsFrameArguments(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	jitAgreesForTest(t, `'use strict'
		function rec(n, k = n * 2, m = 0) { if (n <= 0 || n === '3') return m * 1000 + k; return rec(n === 5 ? '3' : n - 1, undefined, m + 1) }
		function deep(d, n) { return d === 0 ? rec(n, undefined, 0) : deep(d - 1, n) + 0 }`,
		`deep(40, 8)`, `deep(40, 20)`, `deep(40, 4)`, `deep(40, 20)`, `deep(5, 20)`, `deep(40, 30)`, `deep(40, 30)`)
}

// Generators and async functions whose parameter lists have defaults,
// patterns or a rest parameter, or which read their arguments object, are
// native code's: their parameters are bound as the generator is made, and
// a resumption enters native code past them (end_params), on the
// generator's frame, which keeps the call's arguments. 188 of npm's own
// functions had been refused for such a list.
func TestJITGeneratorParameters(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitAgreesForTest(t, `
		function* range(from = 0, { to, step = 1 } = { to: 5 }) { for (let i = from; i < to; i += step) yield i }
		function* rest(k, ...xs) { for (const x of xs) yield x * k; yield arguments.length }
		async function sum(a, { b = 2 } = {}, ...more) { let s = a + b; for (const m of more) s += await m; return s + arguments.length }
		var out = []
		function run(n) {
			let s = 0
			for (let i = 0; i < n; i++) {
				for (const v of range()) s += v
				for (const v of range(i % 3, { to: 7, step: 2 })) s += v
				for (const v of rest(i, 1, 2, 3)) s += v
				sum(i, undefined, 1, 2).then(v => out.push(v))
				sum(i, { b: 5 }).then(v => out.push(v))
			}
			return s
		}`, `run(20)`, `out.join()`, `run(60)`, `out.length + ':' + out.slice(-6).join()`)
	for _, name := range []string{"range", "rest", "sum"} {
		if e := jitEntryForTest(t, r, name); e.ssaStats.entries == 0 {
			t.Errorf("%s was never entered", name)
		}
	}
}
