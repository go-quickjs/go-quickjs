//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"testing"
)

// jitGenNextCases are calls of a generator's next, each a setup,
// expressions and their answers, Node v26.10.0's (vm.runInContext, as a
// classic script), and the generator function whose resumptions a native
// call of next makes, if one is global: a loop of calls; the done result
// and the return value, and next on a finished generator; values sent in,
// a first one ignored; fresh result objects of Object.prototype, keys
// value then done, writable and extensible; an exception out of the body,
// the generator done after; next called on the generator while it runs; a
// speculation that fails in the middle of a resumption; next mixed with a
// loop's steps; one call site calling generators of two functions, an
// object of its own next and an array's iterator; a generator method's
// this; return and throw between native calls, a finally clause that
// yields; a call of two arguments, which Go makes; a generator's own
// next; and yields inside try statements.
var jitGenNextCases = []struct {
	name, setup string
	exprs, want []string
	gen         string
}{
	{"nextLoop", `function* count(n) { for (var i = 0; i < n; i++) yield i }
		function sum(n) { var g = count(n), s = 0; for (var r = g.next(); !r.done; r = g.next()) s += r.value; return s }`,
		[]string{`sum(100)`, `sum(0)`, `sum(1)`}, []string{`4950`, `0`, `0`}, "count"},
	{"nextReturn", `function* g(n) { for (let i = 0; i < n; i++) yield i; return 'r' + n }
		function f(n) { const it = g(n), out = []; let r; do { r = it.next(); out.push(JSON.stringify(r)) } while (!r.done); out.push(JSON.stringify(it.next())); return out.join() }`,
		[]string{`f(2)`, `f(0)`}, []string{`{"value":0,"done":false},{"value":1,"done":false},{"value":"r2","done":true},{"done":true}`, `{"value":"r0","done":true},{"done":true}`}, "g"},
	{"nextSend", `function* g() { let t = 0; for (let i = 0; i < 4; i++) t += (yield t) | 0; return t }
		function f(k) { const it = g(); const out = [it.next(99).value]; for (let i = 1; i < 6; i++) out.push(JSON.stringify(it.next(i * k))); return out.join() }`,
		[]string{`f(1)`, `f(10)`}, []string{`0,{"value":1,"done":false},{"value":3,"done":false},{"value":6,"done":false},{"value":10,"done":true},{"done":true}`, `0,{"value":10,"done":false},{"value":30,"done":false},{"value":60,"done":false},{"value":100,"done":true},{"done":true}`}, "g"},
	{"nextFresh", `function* g() { yield 1; yield 2 }
		function f() { const it = g(); const a = it.next(), b = it.next(), c = it.next(); a.value = 7; return [a !== b, Object.keys(a).join(), Object.getPrototypeOf(a) === Object.prototype, a.value, b.value, c.done, c.value === undefined, JSON.stringify(c), Object.isExtensible(a)].join() }`,
		[]string{`f()`, `f()`}, []string{`true,value,done,true,7,2,true,true,{"done":true},true`, `true,value,done,true,7,2,true,true,{"done":true},true`}, ""},
	{"nextThrows", `function* g(n) { for (let i = 0; i < n; i++) { if (i == 3) throw new RangeError('at ' + i); yield i } }
		function f(n) { const it = g(n); let s = 0; try { for (;;) { const r = it.next(); if (r.done) break; s += r.value } } catch (e) { return e.name + ':' + s + ':' + JSON.stringify(it.next()) } return s }`,
		[]string{`f(2)`, `f(10)`, `f(4)`}, []string{`1`, `RangeError:3:{"done":true}`, `RangeError:3:{"done":true}`}, "g"},
	{"nextReentrant", `var it
		function* g() { for (let i = 0; i < 3; i++) { try { it.next() } catch (e) { yield e.constructor.name + i } } }
		function f() { it = g(); const out = []; for (let r = it.next(); !r.done; r = it.next()) out.push(r.value); return out.join() }`,
		[]string{`f()`, `f()`}, []string{`TypeError0,TypeError1,TypeError2`, `TypeError0,TypeError1,TypeError2`}, "g"},
	{"nextDeopt", `function* g(a) { for (let i = 0; i < a.length; i++) yield a[i] * 2 }
		function f(a) { const it = g(a), out = []; for (let r = it.next(); !r.done; r = it.next()) out.push(r.value); return out.join() }`,
		[]string{`f([1,2,3])`, `f([4,5,6])`, `f([1,'x',3])`, `f([1.5,{valueOf(){return 7}},3])`, `f([1,2,3])`}, []string{`2,4,6`, `8,10,12`, `2,NaN,6`, `3,14,6`, `2,4,6`}, "g"},
	{"nextMixed", `function* g(n) { for (let i = 0; i < n; i++) yield i }
		function f(n) { const it = g(n); let s = it.next().value * 1000; for (const v of it) { s += v; if (v == 2) break } return s + ':' + JSON.stringify(it.next()) }`,
		[]string{`f(5)`, `f(1)`}, []string{`3:{"done":true}`, `0:{"done":true}`}, "g"},
	{"nextPoly", `function* a(n) { for (let i = 0; i < n; i++) yield i }
		function* b(n) { for (let i = 0; i < n; i++) yield -i }
		function f(it) { let s = 0; for (let r = it.next(); !r.done; r = it.next()) s += r.value; return s }`,
		[]string{`f(a(5))`, `f(b(5))`, `f({ i: 0, next() { return this.i < 3 ? { value: this.i++, done: false } : { done: true } } })`, `f(a(6))`, `f([1, 2][Symbol.iterator]())`}, []string{`10`, `-10`, `3`, `15`, `3`}, "a"},
	{"nextThis", `var o = { k: 10, *each(n) { for (let i = 0; i < n; i++) yield this.k + i } }
		function f(n) { const it = o.each(n); let s = 0; for (let r = it.next(); !r.done; r = it.next()) s += r.value; return s }`,
		[]string{`f(4)`, `f(10)`}, []string{`46`, `145`}, ""},
	{"nextReturnThrow", `function* g() { try { yield 1; yield 2 } finally { yield 'f' } }
		function f() { const it = g(), out = []; out.push(it.next().value); out.push(JSON.stringify(it.return(5))); out.push(JSON.stringify(it.next())); out.push(JSON.stringify(it.next())); return out.join() }
		function h() { const it = g(), out = []; out.push(it.next().value); out.push(JSON.stringify(it.throw(new Error('t')))); try { it.next() } catch (e) { out.push('c' + e.message) } out.push(JSON.stringify(it.next())); return out.join() }`,
		[]string{`f()`, `h()`, `f()`}, []string{`1,{"value":"f","done":false},{"value":5,"done":true},{"done":true}`, `1,{"value":"f","done":false},ct,{"done":true}`, `1,{"value":"f","done":false},{"value":5,"done":true},{"done":true}`}, ""},
	{"nextArgs", `function* g() { const a = yield 1; const b = yield a; return a + b }
		function f() { const it = g(); return [it.next(5, 6).value, it.next(7, 8).value, JSON.stringify(it.next(9, 10))].join() }`,
		[]string{`f()`, `f()`}, []string{`1,7,{"value":16,"done":true}`, `1,7,{"value":16,"done":true}`}, ""},
	{"nextOwnNext", `function* g() { yield 1; yield 2 }
		function f() { const it = g(); it.next = function () { return { value: 'own', done: true } }; const r = it.next(); return r.value + ':' + Object.getPrototypeOf(it).next.call(it).value }`,
		[]string{`f()`, `f()`}, []string{`own:1`, `own:1`}, ""},
	{"nextHandlers", `function* g(n) { for (let i = 0; i < n; i++) { try { yield i } finally { if (i == 1) yield 'fin' } } }
		function f(n) { const it = g(n), out = []; for (let r = it.next(); !r.done; r = it.next()) out.push(r.value); return out.join() }`,
		[]string{`f(3)`, `f(1)`}, []string{`0,1,fin,2`, `0`}, "g"},
}

// TestJITGenNextAgreesWithNode runs jitGenNextCases in the interpreter, in
// the tree tier, and with the JIT compiling at the first call, the
// collector marking, and under QJS_JIT_STRESS's budgets and
// deoptimizations: each answer is Node's, and, compiled, the calls of next
// resume each generator the cases name natively.
func TestJITGenNextAgreesWithNode(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	rounds := func(t *testing.T, mk func() *Runtime, n int, native func(r *Runtime, name, gen string)) {
		t.Helper()
		for _, tc := range jitGenNextCases {
			r := mk()
			if _, err := r.Run(compileForTest(t, tc.setup)); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			for round := 0; round < n; round++ {
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
	native := func(r *Runtime, name, gen string) {
		if gen != "" && jitGenInForTest(r, gen) == 0 {
			t.Errorf("%s: no call of next resumed %s natively", name, gen)
		}
	}
	rounds(t, jit, 8, native)
	t.Run("marking", func(t *testing.T) {
		// Native while the collector marks, as when it does not.
		jitMarkingForTest(t)
		rounds(t, jit, 6, native)
	})
	for _, c := range []jitStressConfig{{threshold: true, budget: 3, deopt: 7}, {threshold: true, budget: 1}} {
		rounds(t, func() *Runtime {
			r := jit()
			r.jitStress = c
			return r
		}, 4, nil)
	}
}

// A loop of calls of a generator's next, compiled, makes no call of Go at
// its calls: each resumes the generator natively, its result an object of
// its call's pool, a few calls of Go made in all -- the generator's making,
// the pool's fillings, the polls at the back edges' budgets -- where each
// call had been one. So too while the collector marks (jitMarkingForTest),
// the result's pointer words stored through the write barrier: a result
// Go made while it marked was a call of Go at every call of next.
func TestJITGenNextCallsNoGo(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "marking"}[marking], func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			r := jitRuntimeForTest(t, Config{JIT: true})
			if _, err := r.Run(compileForTest(t, `function* count(n) { for (var i = 0; i < n; i++) yield i }
				function sum(n) { var g = count(n), s = 0; for (var r = g.next(); !r.done; r = g.next()) s += r.value; return s }`)); err != nil {
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
			if calls > 10000/50 {
				t.Errorf("a loop of 10000 calls of next made %d calls of Go", calls)
			}
			if n := jitGenInForTest(r, "count") - in; n < 10000 {
				t.Errorf("10001 calls of next resumed the generator natively %d times", n)
			}
		})
	}
}

// The results a native call of next makes, and what the generator yields
// into them, survive collections made while they are held and while the
// generator is suspended: each answer is Node's.
func TestJITGenNextCollects(t *testing.T) {
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
		function* objs(n) { for (let i = 0; i < n; i++) { yield { v: i, s: 'k' + i }; if (i % 5 == 0) collect() } return 'end' }
		function f(n) {
			const it = objs(n), held = []
			let s = 0, r
			for (r = it.next(); !r.done; r = it.next()) { s += r.value.v; if (r.value.v % 7 == 0) { collect(); held.push(r) } }
			return s + ':' + held.map(x => x.value.s).join('') + ':' + r.value
		}`)); err != nil {
		t.Fatal(err)
	}
	// Node v26.10.0's.
	want := "1225:k0k7k14k21k28k35k42k49:end"
	for range 6 {
		v, err := r.Run(compileForTest(t, "''+f(50)"))
		if err != nil || v.String().Go() != want {
			t.Fatalf("f(50) = %v, %v; want %s", v, err, want)
		}
	}
	if jitGenInForTest(r, "objs") == 0 {
		t.Error("no call of next resumed objs natively")
	}
}
