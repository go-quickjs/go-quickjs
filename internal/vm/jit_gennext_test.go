//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"runtime"
	"testing"
)

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
	defer holdCollectorNow(t)()
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
