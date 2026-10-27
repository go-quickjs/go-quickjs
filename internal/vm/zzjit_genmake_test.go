//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "testing"

// jitGenMakeCases are calls of generator functions in loops, each a setup,
// expressions and their answers, Node v26.10.0's (vm.runInContext, as a
// classic script), and the generator function whose generators a native
// call makes, whose resumptions the steps then make natively, if one is
// global -- "once" runs the expressions once, in order, for a case that
// changes what it calls: a short generator made and run by for-of; fewer
// arguments than parameters and more; a method's, its this; the
// function's prototype read, replaced and made a primitive; a strict
// generator's undefined this, a sloppy one's global; one reading its
// arguments; locals past the parameters; next with a value sent; a
// generator made inside another; a break closing one with a finally; one
// that throws as it starts.
var jitGenMakeCases = []struct {
	name, setup string
	exprs, want []string
	gen         string
}{
	{"makePair", `function* pair(a, b) { yield a; yield b }
		function f(n) { let s = 0; for (let j = 0; j < n; j++) for (const v of pair(j, 1)) s += v; return s }`,
		[]string{`f(10)`, `f(100)`, `f(0)`}, []string{`55`, `5050`, `0`}, "pair"},
	{"makeArgs", `function* g(a, b) { var c = a; yield c; yield b }
		function f(n) { const out = []; for (let j = 0; j < n; j++) { out.push([...g(j)].join('/')); out.push([...g(j, 7, 8)].join('/')) } return out.join() }`,
		[]string{`f(3)`, `f(1)`}, []string{`0/,0/7,1/,1/7,2/,2/7`, `0/,0/7`}, ""},
	{"makeMethod", `var o = { k: 10, *each(n) { for (let i = 0; i < n; i++) yield this.k + i } }
		function f(n) { let s = 0; for (let j = 0; j < n; j++) for (const v of o.each(3)) s += v; return s }`,
		[]string{`f(4)`, `f(10)`}, []string{`132`, `330`}, ""},
	{"makeProto", `function* G(n) { yield n }
		var GP = Object.getPrototypeOf(function* () {}).prototype
		function f(n) { const out = []; for (let j = 0; j < n; j++) { const g = G(j); out.push(Object.getPrototypeOf(g) === G.prototype, g instanceof G, GP.next.call(g).value) } return out.join() }
		function swap() { G.prototype = { tag: 'new' }; return f(2) + ':' + G(1).tag }
		function prim() { G.prototype = 5; const g = G(3); return Object.getPrototypeOf(g) === GP ? 'gp:' + g.next().value : 'other' }`,
		[]string{`f(3)`, `f(3)`, `f(3)`, `f(3)`, `swap()`, `f(2)`, `f(2)`, `prim()`}, []string{`true,true,0,true,true,1,true,true,2`, `true,true,0,true,true,1,true,true,2`, `true,true,0,true,true,1,true,true,2`, `true,true,0,true,true,1,true,true,2`, `true,true,0,true,true,1:new`, `true,true,0,true,true,1`, `true,true,0,true,true,1`, `gp:3`}, "once"},
	{"makeThisStrict", `function* g() { 'use strict'; yield typeof this; yield this }
		function f(n) { const out = []; for (let j = 0; j < n; j++) out.push([...g()].join('/')); return out.join() }`,
		[]string{`f(3)`}, []string{`undefined/,undefined/,undefined/`}, ""},
	{"makeThisSloppy", `function* g() { yield this === globalThis }
		function f(n) { const out = []; for (let j = 0; j < n; j++) out.push([...g()].join('/')); return out.join() }`,
		[]string{`f(3)`}, []string{`true,true,true`}, ""},
	{"makeArguments", `function* g(a) { yield arguments.length; yield a }
		function f(n) { const out = []; for (let j = 0; j < n; j++) out.push([...g(j, j)].join('/')); return out.join() }`,
		[]string{`f(3)`}, []string{`2/0,2/1,2/2`}, ""},
	{"makeLocals", `function* g(a, b) { let s = 0; for (let i = a; i < b; i++) { s += i; yield s } return s }
		function f(n) { let t = 0; for (let j = 0; j < n; j++) { const it = g(j, j + 3); let r; while (!(r = it.next()).done) t += r.value; t += r.value } return t }`,
		[]string{`f(5)`, `f(50)`}, []string{`125`, `11375`}, "g"},
	{"makeNext", `function* g(a) { const x = yield a; yield x * 2 }
		function f(n) { let s = 0; for (let j = 0; j < n; j++) { const it = g(j); s += it.next().value; s += it.next(j).value; s += it.next().done ? 1 : 0 } return s }`,
		[]string{`f(4)`, `f(30)`}, []string{`22`, `1335`}, "g"},
	{"makeNested", `function* inner(a) { yield a; yield a + 1 }
		function* outer(n) { for (let i = 0; i < n; i++) for (const v of inner(i)) yield v }
		function f(n) { let s = 0; for (let j = 0; j < n; j++) for (const v of outer(3)) s += v + j; return s }`,
		[]string{`f(3)`, `f(20)`}, []string{`45`, `1320`}, "inner"},
	{"makeReturn", `function* g(a) { try { yield a; yield a + 1 } finally { log.push('f' + a) } }
		var log = []
		function f(n) { log = []; let s = 0; for (let j = 0; j < n; j++) for (const v of g(j)) { s += v; break } return s + ':' + log.join() }`,
		[]string{`f(3)`, `f(5)`}, []string{`3:f0,f1,f2`, `10:f0,f1,f2,f3,f4`}, ""},
	{"makeThrow", `function* g(a) { if (a == 2) throw new RangeError('r' + a); yield a }
		function f(n) { const out = []; for (let j = 0; j < n; j++) { try { for (const v of g(j)) out.push(v) } catch (e) { out.push(e.message) } } return out.join() }`,
		[]string{`f(4)`}, []string{`0,1,r2,3`}, "g"},
}

// TestJITGenMakeAgreesWithNode runs jitGenMakeCases in the interpreter, in
// the tree tier, and with the JIT compiling at the first call, the
// collector marking, and under QJS_JIT_STRESS's budgets and
// deoptimizations: each answer is Node's, and, compiled, the steps resume
// each generator the cases name natively.
func TestJITGenMakeAgreesWithNode(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	rounds := func(t *testing.T, mk func() *Runtime, n int, native func(r *Runtime, name, gen string)) {
		t.Helper()
		for _, tc := range jitGenMakeCases {
			r := mk()
			if _, err := r.Run(compileForTest(t, tc.setup)); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			k := n
			if tc.gen == "once" {
				k = 1
			}
			for round := 0; round < k; round++ {
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
			if native != nil && tc.gen != "once" {
				native(r, tc.name, tc.gen)
			}
			r.Close()
			r.ReleaseClosed()
		}
	}
	defer SetTreeTier(true)
	for _, on := range []bool{false, true} {
		SetTreeTier(on)
		rounds(t, func() *Runtime { return New(Config{}) }, 2, nil)
	}
	SetTreeTier(true)
	jit := func() *Runtime {
		r := New(Config{JIT: true})
		jitDefaultsForTest(r)
		r.jitCallThreshold, r.jitICThreshold = 1, 1
		return r
	}
	rounds(t, jit, 8, func(r *Runtime, name, gen string) {
		if gen != "" && jitGenInForTest(r, gen) == 0 {
			t.Errorf("%s: no step resumed %s natively", name, gen)
		}
	})
	t.Run("marking", func(t *testing.T) {
		jitMarkingForTest(t)
		rounds(t, jit, 6, nil)
	})
	for _, c := range []jitStressConfig{{threshold: true, budget: 3, deopt: 7}, {threshold: true, budget: 1}} {
		rounds(t, func() *Runtime {
			r := jit()
			r.jitStress = c
			return r
		}, 4, nil)
	}
}

// A loop making a short generator at each turn and running it, compiled,
// makes the generator natively, from its call's pool, where it had been a
// call of Go: the calls of Go a turn makes are fewer than two -- the
// loop's cursor's making, the pool's fillings, the polls -- and every step
// resumes its generator natively.
func TestJITGenMakeCallsLessGo(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitRuntimeForTest(t, Config{JIT: true})
	if _, err := r.Run(compileForTest(t, `function* pair(a, b) { yield a; yield b }
		function f(n) { let s = 0; for (let j = 0; j < n; j++) for (const v of pair(j, 1)) s += v; return s }`)); err != nil {
		t.Fatal(err)
	}
	for range 8 {
		if v, err := r.Run(compileForTest(t, "''+f(100)")); err != nil || v.String().Go() != "5050" {
			t.Fatalf("f(100) = %v, %v", v, err)
		}
	}
	in := jitGenInForTest(r, "pair")
	v, calls := jitCountGoCalls(t, r, "''+f(10000)")
	if v.String().Go() != "50005000" {
		t.Fatalf("f(10000) = %v", v)
	}
	if calls > 10000*12/10 {
		t.Errorf("a loop making 10000 generators made %d calls of Go", calls)
	}
	if n := jitGenInForTest(r, "pair") - in; n < 30000 {
		t.Errorf("30000 steps resumed the generators natively %d times", n)
	}
}
