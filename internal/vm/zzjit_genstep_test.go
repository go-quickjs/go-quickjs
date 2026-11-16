//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"testing"
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
	// Its register code's link, which native steps resume it by.
	if l := r.jit.regLinks[r.jitRegKeyOf(cl)]; l != nil {
		return l.in
	}
	return 0
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
// made in a call of Go from the generator's code (jitRegGoCallNested), and
// in the interpreter, which a failed speculation leaves the resumption to
// (jitRegGenFill).
func TestJITGenStepStack(t *testing.T) {
	t.Parallel()
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

// A yield with an operand beneath its value -- a number, which the frame
// holds without a record -- saves the operand where the generator keeps it
// natively, while the collector marks as when it does not (its pointer
// words through the write barrier): every step resumes the generator
// natively, a few calls of Go in all. A yield saving operands had been
// Go's suspension while the collector marked, which left the loop judged
// slow, to the interpreter, which made no native step.
func TestJITGenYieldSavesWhileMarking(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "marking"}[marking], func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			r := jitRuntimeForTest(t, Config{JIT: true})
			if _, err := r.Run(compileForTest(t, `function* g(n) { let last = 0; for (let i = 0; i < n; i++) last = i * 10 + ((yield last) | 0) }
				function f(n) { let s = 0; for (const v of g(n)) s += v; return s }`)); err != nil {
				t.Fatal(err)
			}
			for range 8 {
				if v, err := r.Run(compileForTest(t, "''+f(100)")); err != nil || v.String().Go() != "48510" {
					t.Fatalf("f(100) = %v, %v", v, err)
				}
			}
			in := jitGenInForTest(r, "g")
			// Node v26.10.0's.
			v, calls := jitCountGoCalls(t, r, "''+f(10000)")
			if v.String().Go() != "499850010" {
				t.Fatalf("f(10000) = %v", v)
			}
			if calls > 10000/50 {
				t.Errorf("a loop of 10000 steps over a saving yield made %d calls of Go", calls)
			}
			if n := jitGenInForTest(r, "g") - in; n < 10000 {
				t.Errorf("10001 steps resumed the generator natively %d times", n)
			}
		})
	}
}

// A yield whose frame has records -- an object a local holds, yielded or
// an operand beneath the yield, and locals that swap their objects, each
// record reading a slot another writes -- applies them natively while the
// collector marks as when it does not (mir's recordsTail: the pointers
// read shaded first, the pointer words written through the write
// barrier), and suspends the generator natively: every step resumes it
// natively, Go applies none of the records, and the calls of Go are as
// many either way. The records had been Go's while the collector marked,
// which made each yield Go's suspension and left the loop judged slow, to
// the interpreter, which made no native step.
func TestJITGenYieldRecordsWhileMarking(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	// Node v26.10.0's, for f(100) and f(10000).
	cases := []struct{ name, src, small, large string }{
		{"yield an object", `function* g(n) { const o = { v: 0 }; for (let i = 0; i < n; i++) { o.v = i; yield o } }
			function f(n) { let s = 0; for (const v of g(n)) s += v.v; return s }`, "4950", "49995000"},
		{"an object beneath", `function pair(o, x) { return o.v + x }
			function* g(n) { const o = { v: 0 }; let s = 0; for (let i = 0; i < n; i++) { o.v = i; s = pair(o, (yield s) | 0) } }
			function f(n) { let s = 0; for (const v of g(n)) s += v; return s }`, "4851", "49985001"},
		{"objects swapped", `function* g(n) { let a = { v: 1 }, b = { v: 2 }, s = 0; for (let i = 0; i < n; i++) { const t = a; a = b; b = t; a.v += i & 3; s += (yield a.v + b.v) | 0 } }
			function f(n) { let s = 0; for (const v of g(n)) s += v; return s }`, "7750", "75025000"},
	}
	for _, c := range cases {
		calls := map[bool]int{}
		for _, marking := range []bool{false, true} {
			t.Run(c.name+map[bool]string{false: "/idle", true: "/marking"}[marking], func(t *testing.T) {
				if marking {
					jitMarkingForTest(t)
					// What the stores through the barrier keep, kept.
					jitClobberStoresForTest(t)
				}
				r := jitRuntimeForTest(t, Config{JIT: true})
				if _, err := r.Run(compileForTest(t, c.src)); err != nil {
					t.Fatal(err)
				}
				for range 8 {
					if v, err := r.Run(compileForTest(t, "''+f(100)")); err != nil || v.String().Go() != c.small {
						t.Fatalf("f(100) = %v, %v; want %s", v, err, c.small)
					}
				}
				in, records := jitGenInForTest(r, "g"), r.jit.ssaRecords
				v, n := jitCountGoCalls(t, r, "''+f(10000)")
				if v.String().Go() != c.large {
					t.Fatalf("f(10000) = %v; want %s", v, c.large)
				}
				calls[marking] = n
				if n := jitGenInForTest(r, "g") - in; n < 10000 {
					t.Errorf("10001 steps resumed the generator natively %d times", n)
				}
				if n := r.jit.ssaRecords - records; n != 0 {
					t.Errorf("Go applied %d of the yields' records", n)
				}
			})
		}
		if calls[false] != calls[true] {
			t.Errorf("%s: %d calls of Go; while marking %d", c.name, calls[false], calls[true])
		}
	}
}
