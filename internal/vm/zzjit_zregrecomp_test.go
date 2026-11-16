//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"testing"
)

// TestJITRegReoptimizeResumes runs a loop whose call, never made when the
// loop was compiled, learns its function at a call of Go from native code
// -- one native code calls, not inlines -- and leaves after the call once
// its compile again is due; and checks the function is compiled again
// once for it, with the entry after the call where the frame goes on, as
// the stack form's jitReoptimize compiles its runs' (resumed) and as V8's
// compile for a frame is asked with its bytecode offset. Compiled without
// it -- the call native in the new code, with no entry after it -- the
// frame found none there and had the function compiled again at once
// with it (jitRegResumeSeen): two compiles for one, zlib's loop of ten
// thousand instructions twice in a row. The function is read from an
// array's element, which no compile seeds a call from (regCallTargets'
// binding read before the call): one read from a global the first compile
// calls natively already.
func TestJITRegReoptimizeResumes(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const src = `
function dec(x) { if (x > 5) return dec(x - 5) + 1; return x }
var fns = [dec]
function loop(n, m) { var s = 0; for (var i = 0; i < n; i++) { s = s + 1; if (i > m) s = fns[0](s) } return s }
`
	rts := regNativeRuntimes(t, src)
	r := rts[0]
	r.jitStress = jitStressConfig{}
	call := func(m float64, want string) {
		t.Helper()
		for i, rt := range rts {
			fv, _ := rt.GetProp(Obj(rt.Global()), rt.Intern("loop"))
			v, err := rt.Call(fv, Undefined, []Value{Float(50), Float(m)})
			if got := regNativeShow(rt, v, err); got != want {
				t.Fatalf("loop(50, %v), JIT %v: %q, want %q", m, i == 0, got, want)
			}
		}
	}
	for range 400 {
		call(100, "50")
	}
	fv, _ := r.GetProp(Obj(r.Global()), r.Intern("loop"))
	cl := fv.Object().fn().closure
	e := cl.closureCode.jitReg
	if e == nil || e.code == nil {
		t.Fatal("loop not compiled")
	}
	life := e.life
	before := r.JITStats()
	for range 400 {
		call(10, "2")
	}
	after := r.JITStats()
	ne := cl.closureCode.jitReg
	if ne == nil || ne == e || ne.code == nil {
		t.Fatal("loop not compiled again for dec")
	}
	if n := after.Reoptimized - before.Reoptimized; n != 1 {
		t.Errorf("loop compiled again %d times for dec, want once (resumed %v)", n, life.resumed)
	}
	if len(life.resumed) == 0 {
		t.Errorf("no entry learned where the frame goes on")
	}
	if after.Interpreted != before.Interpreted {
		t.Errorf("%d runs ended in the interpreter", after.Interpreted-before.Interpreted)
	}
}

// TestJITRegReoptimizeRoom compiles a function again, its code invalid,
// in a budget with room for the new code only in place of the old, and
// checks the new code is placed: the room a compile again asks for counts
// what the code it replaces holds as given back, and that code is not let
// go of meanwhile, as the stack form's roomFor makes room for an entry's
// code compiled again. Asked for on top of the old code's, the room for
// zlib's loop of ten thousand instructions, as large as a third of the
// budget, was short at the placing, the finished compile thrown away, and
// asked again at every turn of the loop until other code was let go.
func TestJITRegReoptimizeRoom(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	var body string
	for k := range 60 {
		body += fmt.Sprintf(" s = (s * %d + a[i & 7] + %d) %% 1009;", k+3, k)
	}
	src := "function work(n, a) { var s = 0; for (var i = 0; i < n; i++) {" + body + " } return s }\nvar data = [1, 2, 3, 4, 5, 6, 7, 8];"
	rts := regNativeRuntimes(t, src)
	r := rts[0]
	r.jitStress = jitStressConfig{}
	var want string
	run := func(rt *Runtime) string {
		fv, _ := rt.GetProp(Obj(rt.Global()), rt.Intern("work"))
		data, _ := rt.GetProp(Obj(rt.Global()), rt.Intern("data"))
		v, err := rt.Call(fv, Undefined, []Value{Float(3000), data})
		return regNativeShow(rt, v, err)
	}
	want = run(rts[1])
	for range 3 {
		if got := run(r); got != want {
			t.Fatalf("with the JIT %q, without %q", got, want)
		}
	}
	fv, _ := r.GetProp(Obj(r.Global()), r.Intern("work"))
	cl := fv.Object().fn().closure
	key := r.jitRegKeyOf(cl)
	s := r.jit
	old := s.regCache[key]
	if old == nil || old.code == nil || old.invalid {
		t.Fatal("work not compiled")
	}
	// Room for the new code in place of the old, not beside it.
	r.jitMemoryLimit = int64(s.held() + old.bytes/2)
	r.jitRegInvalidate(cl, old)
	refused, life := s.refusedRoom, old.life
	ne := r.jitRegCompileFor(cl, cl.reg, key, false)
	if ne == nil || ne.code == nil {
		t.Fatalf("not placed: deferred %v, reoptRoom %d, %d refusals for room", life.deferred, life.reoptRoom, s.refusedRoom-refused)
	}
	if s.regCache[key] != ne || !old.dropped || life.deferred || life.reoptRoom != 0 {
		t.Errorf("cache holds the new code %v, old dropped %v, deferred %v, reoptRoom %d", s.regCache[key] == ne, old.dropped, life.deferred, life.reoptRoom)
	}
	if held, budget := s.held(), r.jitBudget(); held > budget {
		t.Errorf("holds %d bytes of a budget of %d", held, budget)
	}
	if got := run(r); got != want {
		t.Fatalf("compiled again: with the JIT %q, without %q", got, want)
	}
}
