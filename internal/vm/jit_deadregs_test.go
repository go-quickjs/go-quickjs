//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"runtime"
	"testing"
	"weak"
)

// deadRegsSetup's functions run a loop long enough to be entered natively
// at its header after the interpreter has used registers that are dead
// there: a method call's window, a temporary.
const deadRegsSetup = `
	function f(n) {
		let a = big()
		a.push(1)
		a = null
		let s = 0, alive = 'none'
		for (let i = 0; i < n; i++) {
			s += i
			if (i === n - 1) alive = check()
		}
		return alive
	}
	function mapped(x, n) {
		var s = 0
		for (var i = 0; i < n; i++) s += i
		return arguments[0] + ':' + x + ':' + s
	}
	function* gen(n) {
		consume(big())
		let s = 0
		for (let i = 0; i < n; i++) {
			s += i
			if (i % 4096 === 0) yield s
		}
		return s
	}
	function drive(n) { let k = 0; for (const v of gen(n)) k += v % 7; return k }
	function later(n) {
		consume(big())
		let s = 0, t
		for (let i = 0; i < n; i++) {
			t = i === n - 2 ? { w: 1, v: i } : { v: i }
			s += t.v
		}
		return s + ':' + t.v
	}
	function traced(n) {
		consume(big())
		let s = 0, stack = ''
		for (let i = 0; i < n; i++) {
			s += i
			if (i === n - 1) stack = new Error('x').stack + ':' + typeof traced.caller + ':' + traced.length
		}
		return s + ':' + stack
	}`

// deadRegsRuntime is a runtime that has run deadRegsSetup, its JIT at the
// runtime's own thresholds, so that the loops are entered natively at their
// headers, and big's arrays known by their weak pointers.
func deadRegsRuntime(t *testing.T, jit bool, made *[]weak.Pointer[Object]) *Runtime {
	t.Helper()
	r := newTestRuntime(t, Config{JIT: jit})
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	if jit {
		jitDefaultsForTest(r)
	}
	r.global.setOwnRaw(r.atoms.intern("big"), Obj(r.newNativeFunc("big", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		a := rt.newArrayFrom(make([]Value, 1<<16))
		*made = append(*made, weak.Make(a))
		return Obj(a), nil
	})), propDefault)
	r.global.setOwnRaw(r.atoms.intern("consume"), Obj(r.newNativeFunc("consume", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return Undefined, nil
	})), propDefault)
	r.global.setOwnRaw(r.atoms.intern("check"), Obj(r.newNativeFunc("check", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		runtime.GC()
		runtime.GC()
		last := (*made)[len(*made)-1]
		if last.Value() != nil {
			return Str(NewString("kept")), nil
		}
		return Str(NewString("collected")), nil
	})), propDefault)
	if _, err := r.Run(compileForTest(t, deadRegsSetup)); err != nil {
		t.Fatal(err)
	}
	return r
}

// A register dead where Go enters native code at a loop's header is
// written undefined first (jitRegClearDead): native code keeps the frame
// as it found it until an exit, and the collector scans the VM's stack
// whole, so an array the interpreter had left in a call's window -- dead
// once the call returned -- stayed reachable for as long as the loop ran
// natively. Here big's array, the receiver of a method call before the
// loop, in the call's window, is collected by the end of the loop with the
// JIT as without it. What a
// cleared register can never be is read: a parameter an arguments object
// aliases, a generator resumed past its loop's entry, a temporary dead at
// the entry that a deoptimization later in the loop needs, a stack trace
// and a function's caller inside the loop -- each answer the
// interpreter's, under the CI stress settings too.
func TestJITDeadRegistersClearedAtEntry(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	var made, wantMade []weak.Pointer[Object]
	r := deadRegsRuntime(t, true, &made)
	want := deadRegsRuntime(t, false, &wantMade)
	for _, src := range []string{`f(200000)`, `f(200000)`} {
		for _, rt := range []*Runtime{want, r} {
			v, err := rt.Run(compileForTest(t, "''+("+src+")"))
			if err != nil {
				t.Fatal(err)
			}
			if got := v.String().Go(); got != "collected" {
				t.Errorf("%s (JIT %v): big's array %s by the loop's end, want collected", src, rt == r, got)
			}
		}
	}
	if e := jitRegLifeForTest(r, "f"); e == nil || e.ssaStats.entries == 0 {
		t.Fatalf("f's loop never ran natively: %+v", e)
	}
	for _, src := range []string{`mapped(7, 200000)`, `mapped('a', 300000)`, `drive(200000)`, `drive(300000)`,
		`later(200000)`, `later(300000)`, `traced(200000)`, `traced(300000)`} {
		wv, err := want.Run(compileForTest(t, "''+("+src+")"))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, "''+("+src+")"))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Errorf("%s: got %s, interpreter %s", src, got, want)
		}
	}
	for _, name := range []string{"mapped", "later", "traced"} {
		if e := jitRegLifeForTest(r, name); e == nil || e.ssaStats.entries == 0 {
			t.Errorf("%s never ran natively", name)
		}
	}
}
