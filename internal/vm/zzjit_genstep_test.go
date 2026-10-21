//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"testing"
	"weak"
)

// jitGenInForTest is how many times loops' steps have resumed generators of
// the global generator function name natively since its code was last
// compiled: its link's count of native calls in, which only its generator
// entry counts (mir's genEntry).
func jitGenInForTest(r *Runtime, name string) uint64 {
	cl := r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure
	if r.jit == nil {
		return 0
	}
	if l := r.jit.links[weak.Make(cl.fn)]; l != nil {
		return l.in
	}
	return 0
}

// jitGenCases are loops' steps over generators, each a setup, expressions
// and their answers, Node v26.10.0's (vm.runInContext, as a classic
// script), and the generator function whose resumptions a native step
// makes, if one is global: a loop over a generator's values, its yields
// with operands beneath them -- a loop's cursor, a value that changes --
// and none, and with a value sent in; a break
// that calls return, which runs a finally clause, and a finally clause
// that yields; a throw from the generator, out of the loop, which calls no
// return method, one the generator catches, from a call it makes, and one
// thrown into it at a yield in a try, operands beneath it; a generator
// delegating with yield*, which no step resumes natively; a generator
// resumed while it runs; a destructuring's steps, done and not, of a
// generator that finishes natively and of one whose return Go makes; a
// generator method's this;
// a step over generators of three functions, and of a closure made anew;
// a generator stepping another; a speculation that fails in the middle of
// a resumption; many yields and a return; and next() mixed with a loop's
// steps.
var jitGenCases = []struct {
	name, setup string
	exprs, want []string
	gen         string
}{
	{"count", `function* count(n) { for (var i = 0; i < n; i++) yield i }
		function sum(n) { var s = 0; for (var v of count(n)) s += v; return s }`,
		[]string{"sum(100)", "sum(1000)", "sum(0)", "sum(1)"}, []string{"4950", "499500", "0", "0"}, "count"},
	{"saved", `function* doubled(a) { for (const x of a) yield x * 2 }
		function total(a) { let s = 0; for (const v of doubled(a)) s = s * 3 + v; return s }`,
		[]string{"total([1,2,3])", "total([])", "total([5,6,7,8,9])"}, []string{"36", "0", "1326"}, "doubled"},
	{"expr", `function* sent() { let t = 0; for (let i = 0; i < 4; i++) t += (yield i) | 0; return t }
		function walk() { const out = []; for (const v of sent()) out.push(v); return out.join() }`,
		[]string{"walk()", "walk()"}, []string{"0,1,2,3", "0,1,2,3"}, "sent"},
	{"beneath", `function* g(n) { let last = 0; for (let i = 0; i < n; i++) last = i * 10 + ((yield last) | 0) }
		function f(n) { const out = []; for (const v of g(n)) out.push(v); return out.join() }`,
		[]string{"f(5)", "f(1)"}, []string{"0,0,10,20,30", "0"}, "g"},
	{"breakFinally", `var log = []
		function* g(n) { try { for (let i = 0; i < n; i++) yield i } finally { log.push('fin' + n) } }
		function f(n, k) { log = []; let s = 0; for (const v of g(n)) { if (v == k) break; s += v } return s + ':' + log.join() }`,
		[]string{"f(10, 3)", "f(100, 30)", "f(100, 200)", "f(3, 0)"}, []string{"3:fin10", "435:fin100", "4950:fin100", "0:fin3"}, "g"},
	{"finallyYields", `function* g() { try { yield 1; yield 2 } finally { yield 'f'; yield 'f2' } }
		function f(k) { const out = []; for (const v of g()) { out.push(v); if (v === k) break } return out.join() }`,
		[]string{"f(1)", "f(2)", "f('f')", "f(0)"}, []string{"1", "1,2", "1,2,f", "1,2,f,f2"}, ""},
	{"throwOut", `function* g(n) { for (let i = 0; i < n; i++) { if (i == 3) throw new RangeError('at ' + i); yield i } }
		function f(n) { let s = 0; try { for (const v of g(n)) s += v } catch (e) { return e.name + ':' + e.message + ':' + s } return s }`,
		[]string{"f(2)", "f(10)", "f(4)"}, []string{"1", "RangeError:at 3:3", "RangeError:at 3:3"}, "g"},
	{"throwNoReturn", `var log = []
		function* g(n) { for (let i = 0; i < n; i++) { if (i == 50) throw new Error('t' + i); yield i } }
		function f(n) { log = []; const it = g(n); it.return = function () { log.push('ret'); return {} }; let s = 0; try { for (const v of it) s += v } catch (e) { log.push(e.message) } return s + ':' + log.join() }`,
		[]string{"f(10)", "f(100)", "f(51)"}, []string{"45:", "1225:t50", "1225:t50"}, "g"},
	{"throwInto", `function* g(a) { for (const x of a) { try { yield x } catch (e) { yield 'c' + e } } }
		function f(n) { const a = []; for (let i = 0; i < n; i++) a.push(i); const it = g(a); const out = []; let k = 0; for (const v of it) { out.push(v); if (++k == 3) { out.push(it.throw(7).value) } } return out.join() }`,
		[]string{"f(5)", "f(2)", "f(8)"}, []string{"0,1,2,c7,3,4", "0,1", "0,1,2,c7,3,4,5,6,7"}, "g"},
	{"delegate", `function* inner(n) { for (let i = 0; i < n; i++) yield i }
		function* outer(n) { yield 'a'; yield* inner(n); yield* [7, 8]; yield* { [Symbol.iterator]() { let k = 0; return { next() { return { value: 'o' + k, done: k++ >= 2 } } } } }; yield 'z' }
		function f(n) { const out = []; for (const v of outer(n)) out.push(v); return out.join() }`,
		[]string{"f(3)", "f(0)"}, []string{"a,0,1,2,7,8,o0,o1,z", "a,7,8,o0,o1,z"}, ""},
	{"throwCaughtInside", `function bad(i) { if (i % 3 == 1) throw new Error('b' + i); return i }
		function* g(n) { for (let i = 0; i < n; i++) { try { yield bad(i) } catch (e) { yield e.message } } }
		function f(n) { const out = []; for (const v of g(n)) out.push(v); return out.join() }`,
		[]string{"f(7)", "f(1)"}, []string{"0,b1,2,3,b4,5,6", "0"}, "g"},
	{"reentrant", `var it
		function* g() { for (let i = 0; i < 3; i++) { try { it.next() } catch (e) { yield e.constructor.name + i } } }
		function f() { it = g(); const out = []; for (const v of it) out.push(v); return out.join() }`,
		[]string{"f()", "f()"}, []string{"TypeError0,TypeError1,TypeError2", "TypeError0,TypeError1,TypeError2"}, "g"},
	{"destructure", `function* g(n) { for (let i = 0; i < n; i++) yield i + 1 }
		var calls = 0
		function f(n) { let out = ''; for (let k = 0; k < 2; k++) { const [a, b, c] = g(n); out += [a, b, c].join() + (c === undefined) + ';' } return out }
		function h(n) { calls = 0; const it = g(n); it.return = function () { calls++; return {} }; const [a, b] = it; return a + ',' + b + ',' + calls }`,
		[]string{"f(5)", "f(2)", "f(0)", "h(1)", "h(2)", "h(5)"}, []string{"1,2,3false;1,2,3false;", "1,2,true;1,2,true;", ",,true;,,true;", "1,undefined,0", "1,2,1", "1,2,1"}, "g"},
	{"destructureFinally", `var cnt = 0
		function* gf(n) { try { for (let i = 0; i < n; i++) yield i + 1; return 9 } finally { cnt++ } }
		function d(n) { cnt = 0; let out = ''; for (let k = 0; k < 2; k++) { const [a, b, c] = gf(n); out += [a, b, c].join() + (c === undefined) + ';' } return out + cnt }`,
		[]string{"d(2)", "d(5)", "d(0)"}, []string{"1,2,true;1,2,true;2", "1,2,3false;1,2,3false;2", ",,true;,,true;2"}, "gf"},
	{"thisGen", `var o = { k: 10, *each(n) { for (let i = 0; i < n; i++) yield this.k + i } }
		function f(n) { let s = 0; for (const v of o.each(n)) s += v; return s }`,
		[]string{"f(4)", "f(10)"}, []string{"46", "145"}, ""},
	{"poly", `function* a(n) { for (let i = 0; i < n; i++) yield i }
		function* b(n) { for (let i = 0; i < n; i++) yield -i }
		function* c(n) { for (let i = 0; i < n; i++) yield i * 100 }
		function f(g, n) { let s = 0; for (const v of g(n)) s += v; return s }`,
		[]string{"f(a, 5)", "f(b, 5)", "f(a, 6)", "f(c, 3)", "f(b, 2)"}, []string{"10", "-10", "15", "300", "-1"}, "b"},
	{"closures", `function mk(k) { return function* (n) { for (let i = 0; i < n; i++) yield i * k } }
		var g2 = mk(2), g3 = mk(3)
		function f(g, n) { let s = 0; for (const v of g(n)) s += v; return s }`,
		[]string{"f(g2, 5)", "f(g3, 5)", "f(mk(7), 3)"}, []string{"20", "30", "21"}, ""},
	{"nested", `function* inner(n) { for (let i = 0; i < n; i++) yield i }
		function* outer(n) { for (const x of inner(n)) yield x * x }
		function f(n) { let s = 0; for (const v of outer(n)) s += v; return s }`,
		[]string{"f(5)", "f(20)"}, []string{"30", "2470"}, "inner"},
	{"deopt", `function* g(a) { for (let i = 0; i < a.length; i++) yield a[i] * 2 }
		function f(a) { const out = []; for (const v of g(a)) out.push(v); return out.join() }`,
		[]string{"f([1,2,3])", "f([4,5,6])", "f([1,'x',3])", "f([1.5,{valueOf(){return 7}},3])", "f([1,2,3])"},
		[]string{"2,4,6", "8,10,12", "2,NaN,6", "3,14,6", "2,4,6"}, "g"},
	{"manyYields", `function* g(n) { yield n; yield n + 1; for (let i = 0; i < 3; i++) yield i; yield 'end' }
		function f(n) { const out = []; for (const v of g(n)) out.push(v); return out.join() }`,
		[]string{"f(5)", "f(6)"}, []string{"5,6,0,1,2,end", "6,7,0,1,2,end"}, "g"},
	{"returnValue", `function* g(n) { for (let i = 0; i < n; i++) yield i; return 'r' }
		function f(n) { const it = g(n); let s = 0; for (const v of it) s += v; return s + ':' + JSON.stringify(it.next()) }`,
		[]string{"f(3)", "f(0)"}, []string{`3:{"done":true}`, `0:{"done":true}`}, "g"},
	{"manualAfter", `function* g(n) { for (let i = 0; i < n; i++) yield i }
		function f(n) { const it = g(n); const out = []; for (const v of it) { out.push(v); if (v == 1) break } return out.join() + ':' + JSON.stringify(it.next()) }`,
		[]string{"f(5)", "f(1)"}, []string{`0,1:{"done":true}`, `0:{"done":true}`}, "g"},
	{"resumeMixed", `function* g(n) { for (let i = 0; i < n; i++) yield i }
		function f(n) { const it = g(n); const a = it.next().value; let s = a * 1000; for (const v of it) s += v; return s }`,
		[]string{"f(5)", "f(1)"}, []string{"10", "0"}, "g"},
}

// jitGenRounds runs the cases in runtimes mk makes, rounds times each, and
// fails at the first answer that is not Node's; native, if not nil, is
// told, for each case, the runtime it ran in.
func jitGenRounds(t *testing.T, mk func() *Runtime, rounds int, native func(r *Runtime, name, gen string)) {
	t.Helper()
	for _, tc := range jitGenCases {
		r := mk()
		if _, err := r.Run(compileForTest(t, tc.setup)); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		for round := 0; round < rounds; round++ {
			for i, src := range tc.exprs {
				v, err := r.Run(compileForTest(t, "''+("+src+")"))
				if err != nil {
					t.Fatalf("%s round %d: %s: %v", tc.name, round, src, err)
				}
				if got := v.String().Go(); got != tc.want[i] {
					t.Fatalf("%s round %d: %s = %q, want %q", tc.name, round, src, got, tc.want[i])
				}
			}
		}
		if native != nil {
			native(r, tc.name, tc.gen)
		}
		r.Close()
		r.ReleaseClosed()
	}
}

// A loop's step over a generator is a native call of the generator's code
// (ssa.CallSite's Gens, mir's genEntry), which resumes it where it was
// suspended, and its yield returns the value to the step: each answer is
// Node's -- in the interpreter, in the tree tier, and with the JIT
// compiling at the first call, the collector marking (its yields then
// leaving pointers to Go), and under QJS_JIT_STRESS's budgets and
// deoptimizations, which leave the generator's code for Go to finish its
// resumption (jitGenFrame, jitGenDone) -- and, compiled, the steps resume
// each generator the cases name natively.
func TestJITGenStepAgreesWithNode(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	defer SetTreeTier(true)
	for _, on := range []bool{false, true} {
		SetTreeTier(on)
		jitGenRounds(t, func() *Runtime { return New(Config{}) }, 2, nil)
	}
	SetTreeTier(true)
	jit := func() *Runtime {
		r := New(Config{JIT: true})
		r.jitCallThreshold, r.jitICThreshold = 1, 1
		return r
	}
	jitGenRounds(t, jit, 8, func(r *Runtime, name, gen string) {
		if gen != "" && jitGenInForTest(r, gen) == 0 {
			t.Errorf("%s: no step resumed %s natively", name, gen)
		}
	})
	t.Run("marking", func(t *testing.T) {
		jitMarkingForTest(t)
		jitGenRounds(t, jit, 6, func(r *Runtime, name, gen string) {
			if gen != "" && jitGenInForTest(r, gen) == 0 {
				t.Errorf("%s: no step resumed %s natively while the collector marked", name, gen)
			}
		})
	})
	for _, c := range []jitStressConfig{{threshold: true, budget: 3, deopt: 7}, {threshold: true, budget: 1}} {
		jitGenRounds(t, func() *Runtime {
			r := jit()
			r.jitStress = c
			return r
		}, 4, nil)
	}
}

// A loop over a generator, compiled, makes no call of Go at its steps:
// each resumes the generator natively and its yields return natively, a
// few calls of Go made in all -- the generator's making, the loop's
// cursor, the polls at the back edges' budgets -- where each step had been
// one.
func TestJITGenStepCallsNoGo(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitRuntimeForTest(t, Config{JIT: true})
	if _, err := r.Run(compileForTest(t, `function* count(n) { for (var i = 0; i < n; i++) yield i }
		function sum(n) { var s = 0; for (var v of count(n)) s += v; return s }`)); err != nil {
		t.Fatal(err)
	}
	for range 8 {
		if v, err := r.Run(compileForTest(t, "''+sum(100)")); err != nil || v.String().Go() != "4950" {
			t.Fatalf("sum(100) = %v, %v", v, err)
		}
	}
	in := jitGenInForTest(r, "count")
	v, calls := jitCountGoCalls(t, r, "''+sum(10000)")
	if v.String().Go() != "49995000" {
		t.Fatalf("sum(10000) = %v", v)
	}
	if calls > 10000/200 {
		t.Errorf("a loop of 10000 steps over a generator made %d calls of Go", calls)
	}
	if n := jitGenInForTest(r, "count") - in; n < 10000 {
		t.Errorf("10001 steps resumed the generator natively %d times", n)
	}
}

// A generator suspended in native code keeps what its frame holds --
// objects its locals and the operands beneath its yield refer to, written
// natively -- across collections, made while it is suspended and while a
// step's resumption is under way: each answer is Node's.
func TestJITGenStepCollects(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	r := jitRuntimeForTest(t, Config{JIT: true})
	r.global.setOwnRaw(r.atoms.intern("collect"), Obj(r.newNativeFunc("collect", 0,
		func(rt *Runtime, this Value, args []Value) (Value, error) {
			runtime.GC()
			return Undefined, nil
		})), propDefault)
	if _, err := r.Run(compileForTest(t, `
		function* objs(a) { for (const x of a) { const o = { v: x, s: 'k' + x }; yield o; collect() } }
		function f(n) {
			const a = []
			for (let i = 0; i < n; i++) a.push(i)
			const held = [], it = objs(a)
			let s = 0, k = 0
			for (const o of it) { s += o.v; if (++k % 7 == 0) { collect(); held.push(o) } }
			return s + ':' + held.map(o => o.s).join('')
		}`)); err != nil {
		t.Fatal(err)
	}
	// Node v26.10.0's.
	want := "1225:k6k13k20k27k34k41k48"
	for range 6 {
		v, err := r.Run(compileForTest(t, "''+f(50)"))
		if err != nil || v.String().Go() != want {
			t.Fatalf("f(50) = %v, %v; want %s", v, err, want)
		}
	}
	if jitGenInForTest(r, "objs") == 0 {
		t.Error("no step resumed objs natively")
	}
}

// An exception a generator a step resumed natively throws has the stack
// the interpreter's has: the generator's frame, its next's, the loop's --
// made in a call of Go from the generator's code (jitGoCallNested), and in
// the interpreter, which a failed speculation leaves the resumption to
// (jitGenFrame).
func TestJITGenStepStack(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	var rounds []string
	for range 8 {
		rounds = append(rounds, "f([1,2,3,4])")
	}
	rounds = append(rounds, "f([1,2,'x',4])", "f([1,2,{},4])")
	r := jitAgreesForTest(t, `
		function* g(a) {
			for (let i = 0; i < a.length; i++) {
				const x = a[i] * 2
				if (i == 2) yield (x + new Error('s').stack); else yield x
			}
		}
		function f(a) { const out = []; for (const v of g(a)) out.push(v); return out.join('|') }`, rounds...)
	if jitGenInForTest(r, "g") == 0 {
		t.Error("no step resumed g natively")
	}
}
