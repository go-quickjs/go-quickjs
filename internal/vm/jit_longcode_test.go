//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/go-quickjs/go-quickjs/internal/jit"
	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// longCodeSource is a function of a loop around n statements, whose code
// is some 160 bytes a statement on amd64: a test of a's at the loop's top
// that leaves it for the end of the function, a read of o.v whose guard's
// exit is past all the statements -- a string there deoptimizes at the
// top, a string in o.w at the bottom -- branches over a statement each,
// and the loop's back edge across all of them.
func longCodeSource(name string, n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "function %s(a, o, rounds) {\nvar s = 0, t = 0;\nfor (var r = 0; r < rounds; r++) {\n", name)
	b.WriteString("if (a > 1000) { t = -1; break; }\nvar v = o.v;\n")
	for i := range n {
		switch i % 3 {
		case 0:
			fmt.Fprintf(&b, "s = (s * 31 + v + %d) | 0;\n", i)
		case 1:
			fmt.Fprintf(&b, "if (s & %d) { s = (s + a) | 0; } else { s = (s ^ %d) | 0; }\n", 1<<(i%7), i)
		default:
			fmt.Fprintf(&b, "t = (t + (s >> %d)) | 0;\n", i%5)
		}
	}
	b.WriteString("t = (t + o.w) | 0;\n}\nreturn s + ':' + t;\n}\n")
	return b.String()
}

// longCodeUnchecked has compiles not check their register allocation
// until the test ends, for a function of 10 MB of code: the check
// computes liveness again by block sets over every value
// (mir.CheckAllocations), quadratic in the function's length -- 2.6 GB
// allocated and a heap of gigabytes for one compile, where the compile
// itself is linear, a gigabyte of garbage for 10 MB of code. The 3 MB
// functions are checked.
func longCodeUnchecked(t *testing.T, size int) {
	if size < 10 {
		return
	}
	jitTestSwitch(t, func(jc *jitTestConfig) { jc.checkAllocations = switchOff })
}

// longCodeObject is {v, w}, made in r.
func longCodeObject(r *Runtime, v, w Value) Value {
	o := Obj(r.NewObject())
	for k, x := range map[string]Value{"v": v, "w": w} {
		if err := r.SetProp(o, r.Intern(k), x); err != nil {
			panic(err)
		}
	}
	return o
}

// Functions of some 3 and 10 MB of code compile, are placed and run
// natively from their register code, with the collector idle and
// marking: through the loop's back edge, the test that leaves it for the
// function's end, a deoptimization at the top and one at the bottom of
// the body, each branching across all of it -- on arm64 through the
// assembler's veneers and branches made of others. Each answer is an
// ordinary call's.
//
// Under checkptr (CI's native boundary) the 3 MB function alone: the
// 10 MB one has the same statements, exits and frame, which Go reads
// the same way after its code as after the shorter one's, so checkptr
// checks nothing more there. What its length adds -- the compile, the
// placement, arm64's branches across it -- the ordinary builds check,
// and under checkptr its two compiles took a fifth of the lane.
func TestJITLongCode(t *testing.T) {
	if !jitSSABackend || testing.Short() {
		t.Skip("no SSA backend on this architecture, or short")
	}
	sizes := []int{3, 10}
	if !allocsOrdinary() {
		sizes = sizes[:1]
	}
	for _, size := range sizes {
		n := size << 20 / 160
		name := fmt.Sprintf("long%d", size)
		src := longCodeSource(name, n)
		for _, marking := range []bool{false, true} {
			t.Run(fmt.Sprintf("%dMB/marking=%v", size, marking), func(t *testing.T) {
				if marking {
					jitMarkingForTest(t)
				}
				longCodeUnchecked(t, size)
				rts := regNativeRuntimes(t, src)
				for _, r := range rts {
					r.jitMemoryLimit = 1 << 36
				}
				call := func(i int, a, v, w Value) {
					t.Helper()
					args := []Value{a, Undefined, Float(3)}
					args[1] = longCodeObject(rts[1], v, w)
					fn1, _ := rts[1].GetProp(Obj(rts[1].Global()), rts[1].Intern(name))
					want1, err1 := rts[1].Call(fn1, Undefined, args)
					want := regNativeShow(rts[1], want1, err1)
					fn0, _ := rts[0].GetProp(Obj(rts[0].Global()), rts[0].Intern(name))
					args[1] = longCodeObject(rts[0], v, w)
					st := rts[0].RegNativeStats()
					start := time.Now()
					res := rts[0].RegNativeCall(fn0, args, false)
					if res.Skipped != "" || res.Entered < 0 {
						t.Fatalf("%s(%d): not native: %q", name, i, res.Skipped)
					}
					if got := regNativeShow(rts[0], res.Value, res.Err); got != want {
						t.Fatalf("%s(%d): natively %q, called %q", name, i, got, want)
					}
					after := rts[0].RegNativeStats()
					t.Logf("%s(%d): %q, compiled %v, %v; guards %d, hosts %d, interpreted %d", name, i, want, res.Compiled, time.Since(start).Round(time.Millisecond),
						after.Guards-st.Guards, after.Hosts-st.Hosts, after.Interpreted-st.Interpreted)
				}
				// Feedback first, from the interpreter.
				rts[0].jitEnabled = false
				for i := range 2 {
					fn0, _ := rts[0].GetProp(Obj(rts[0].Global()), rts[0].Intern(name))
					if _, err := rts[0].Call(fn0, Undefined, []Value{Float(float64(i)), longCodeObject(rts[0], Float(5), Float(1)), Float(2)}); err != nil {
						t.Fatal(err)
					}
				}
				rts[0].jitEnabled = true
				call(0, Float(7), Float(5), Float(1))
				fn0, _ := rts[0].GetProp(Obj(rts[0].Global()), rts[0].Intern(name))
				cl := fn0.Object().fn().closure
				e := rts[0].jit.regCache[rts[0].jitRegKeyOf(cl)]
				if e == nil || e.code == nil {
					t.Fatalf("%s: no code", name)
				}
				if got := e.code.Size(); got < size<<20*3/4 {
					t.Errorf("%s: %d bytes of code, want about %d MB", name, got, size)
				} else {
					t.Logf("%s: %d bytes of code", name, got)
				}
				// Counted against the budget, all of it.
				if s := rts[0].jit; e.bytes < e.code.Size() || s.regHeld < e.bytes {
					t.Errorf("%s: %d bytes of code, the entry counts %d, the cache %d", name, e.code.Size(), e.bytes, s.regHeld)
				}
				// The test that leaves the loop, at its top.
				call(1, Float(2000), Float(5), Float(1))
				call(2, Float(9), Float(6), Float(2))
				// A deoptimization at the top, and one at the bottom.
				guards := rts[0].RegNativeStats().Guards
				call(3, Float(9), Str(NewString("x")), Float(2))
				call(4, Float(9), Float(6), Str(NewString("y")))
				if rts[0].RegNativeStats().Guards == guards {
					t.Errorf("%s: no deoptimization", name)
				}
			})
		}
	}
}

// The same functions' code from arm64's backend on any host, assembled
// with the veneers and branches across it that keep each in reach.
func TestJITLongCodeCompilesARM64(t *testing.T) {
	if !jitSSABackend || testing.Short() {
		t.Skip("no SSA backend on this architecture, or short")
	}
	for _, size := range []int{3, 10} {
		name := fmt.Sprintf("long%d", size)
		if size >= 10 {
			jitTestSwitch(t, func(jc *jitTestConfig) { jc.checkAllocations = switchOff })
		}
		rts := regNativeRuntimes(t, longCodeSource(name, size<<20/160))
		r := rts[0]
		r.jitMemoryLimit = 1 << 36
		fn, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
		r.RegNativeCall(fn, []Value{Float(1), longCodeObject(r, Float(5), Float(1)), Float(2)}, false)
		cl := fn.Object().fn().closure
		e := r.jit.regCache[r.jitRegKeyOf(cl)]
		if e == nil || e.p == nil {
			t.Fatalf("%s: not lowered", name)
		}
		for _, arch := range []string{"amd64", "arm64"} {
			f, err := ssa.BuildWith(e.p, r.jitRegFeedbackFor(cl, e.p))
			if err != nil {
				t.Fatal(err)
			}
			jitRegShape(f, e.p, cl, cl.reg)
			ssa.Optimize(f)
			compile := mir.CompileAMD64
			if arch == "arm64" {
				compile = mir.CompileARM64
			}
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			start := time.Now()
			mc, err := compile(f, jitEncoding)
			if err != nil {
				t.Fatalf("%s: %v", arch, err)
			}
			took := time.Since(start)
			runtime.ReadMemStats(&after)
			t.Logf("%s, %s: %d bytes, %d footprint, in %v, %d MB allocated", name, arch, len(mc.Bytes), jit.CodeFootprint(mc), took.Round(time.Millisecond),
				(after.TotalAlloc-before.TotalAlloc)>>20)
		}
	}
}

// Long code counts against the JIT's budget as any code does: where the
// budget has no room for its footprint, its length among it, it is not
// placed, deferred until the cache lets code go -- no refusal, no mapping
// made -- and placed whole once there is room, the entry counting it all.
func TestJITLongCodeBudget(t *testing.T) {
	if !jitSSABackend || testing.Short() {
		t.Skip("no SSA backend on this architecture, or short")
	}
	const name = "long3"
	rts := regNativeRuntimes(t, longCodeSource(name, 3<<20/160))
	r := rts[0]
	r.jitMemoryLimit = 1 << 36
	fn, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
	r.RegNativeCall(fn, []Value{Float(1), longCodeObject(r, Float(5), Float(1)), Float(2)}, false)
	cl := fn.Object().fn().closure
	key := r.jitRegKeyOf(cl)
	old := r.jit.regCache[key]
	if old == nil || old.p == nil || old.code == nil {
		t.Fatalf("%s: not compiled", name)
	}
	p := old.p
	rf := r.jitRegFeedbackFor(cl, p)
	f, err := ssa.BuildWith(p, rf)
	if err != nil {
		t.Fatal(err)
	}
	looped := jitRegShape(f, p, cl, cl.reg)
	ssa.Optimize(f)
	mc, err := mir.Compile(f, jitEncoding)
	if err != nil {
		t.Fatal(err)
	}
	need := jit.CodeFootprint(mc)
	if need < len(mc.Bytes) || len(mc.Bytes) < 2<<20 {
		t.Fatalf("%d bytes of code, footprint %d", len(mc.Bytes), need)
	}
	s := r.jit
	mapped, _ := s.arena.Mapped()
	e := &jitRegEntry{key: key, p: p, life: s.regLearnedOf(key)}
	// Placed as a compile places it, whose end settles (jitRegCompileFor):
	// a look meanwhile leaves the budget to it (keepWithin).
	s.settleAfter++
	defer func() { s.settleAfter-- }()
	// A budget short of the footprint alone, whatever the cache lets go.
	r.jitMemoryLimit = int64(need - 1)
	if r.jitRegPlace(e, cl.reg, rf, f, mc, looped, nil) || e.code != nil || e.refusal != "" || !e.life.deferred || e.life.generation != s.generation {
		t.Fatalf("no room: placed %v, refusal %q, deferred %v", e.code != nil, e.refusal, e.life.deferred)
	}
	if now, _ := s.arena.Mapped(); now != mapped || s.unavailable {
		t.Fatalf("no room: %d bytes mapped, %d before; unavailable %v", now, mapped, s.unavailable)
	}
	r.jitMemoryLimit = int64(s.held() + need)
	if !r.jitRegPlace(e, cl.reg, rf, f, mc, looped, nil) || e.code == nil || e.refusal != "" || e.life.deferred {
		t.Fatalf("room: placed %v, refusal %q, deferred %v", e.code != nil, e.refusal, e.life.deferred)
	}
	defer e.code.Close()
	if e.bytes != need || e.code.Size() < len(mc.Bytes) {
		t.Fatalf("%d bytes of code placed in %d, the entry counts %d of %d", len(mc.Bytes), e.code.Size(), e.bytes, need)
	}
	if now, _ := s.arena.Mapped(); now < mapped+e.code.Size() {
		t.Fatalf("%d bytes mapped, %d before, %d placed", now, mapped, e.code.Size())
	}
}
