//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"math"
	"testing"

	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
)

// regFreqSrc calls one function four times a run and another every other
// run: frequencies 4 and 0.5.
const regFreqSrc = `
function leafy(x) { return x + 1 }
function half(x) { return x * 2 }
function caller(n) {
	let s = 0;
	for (let i = 0; i < 4; i++) s = leafy(s);
	if (n % 2 == 0) s = half(s);
	return s;
}
function drive(m) { let t = 0; for (let k = 0; k < m; k++) t += caller(k); return t }
`

// TestJITRegFrequency has the feedback dispatcher count a function's runs
// and the calls at its call sites (jitRegCounts), its code never
// compiled, and checks the frequencies register code's feedback answers
// (jitRegFeedback.Frequency): 4 calls a run of one site, 0.5 of the
// other.
func TestJITRegFrequency(t *testing.T) {
	t.Parallel()
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			prog := compileForTest(t, regFreqSrc)
			r := newTestRuntime(t, Config{JIT: true})
			t.Cleanup(r.Close)
			r.jitStress = jitStressConfig{}
			r.SetJITWarmUp(1<<16, 1<<16)
			if _, err := r.Run(prog); err != nil {
				t.Fatal(err)
			}
			fv, _ := r.GetProp(Obj(r.Global()), r.Intern("caller"))
			cl := fv.Object().fn().closure
			// Never compiled: the interpreter runs every call of it,
			// and counts.
			cl.jitRegRefused = true
			drive, _ := r.GetProp(Obj(r.Global()), r.Intern("drive"))
			if _, err := r.Call(drive, Undefined, []Value{Float(1000)}); err != nil {
				t.Fatal(err)
			}
			if !regNativeDefault {
				// The Go dispatcher counts nothing (QJS_NONATIVE, a
				// verifying build): no frequency is known.
				for _, n := range cl.jitRegCounts {
					if n != 0 {
						t.Fatalf("counts %v without the native handlers", cl.jitRegCounts)
					}
				}
				return
			}
			if len(cl.jitRegCounts) == 0 || cl.jitRegCounts[0] < 990 {
				t.Fatalf("counts %v, want about 1000 runs", cl.jitRegCounts)
			}
			p, err := jitcompile.LowerReg(cl.reg.rc, cl.fn)
			if err != nil {
				t.Fatal(err)
			}
			rf := r.jitRegFeedbackFor(cl, p)
			got := map[string]float64{}
			for pc, in := range p.Code {
				if in.Op != ir.Host || !in.Window {
					continue
				}
				w := cl.reg.rc.Code[p.Origin[pc]]
				if int(w.C()) >= len(cl.regCalls) || cl.regCalls[w.C()].cl == nil {
					continue
				}
				f, ok := rf.Frequency(pc)
				if !ok {
					t.Fatalf("no frequency at %d", pc)
				}
				got[cl.regCalls[w.C()].cl.fn.Name] = f
			}
			for name, want := range map[string]float64{"leafy": 4, "half": 0.5} {
				if f, ok := got[name]; !ok || math.Abs(f-want) > 0.01*want {
					t.Errorf("%s's frequency %v (%v), want %v; counts %v", name, f, ok, want, cl.jitRegCounts)
				}
			}
		})
	}
}

// A call the native frame stack has no room for is counted once: the
// feedback dispatcher counts it where it dispatches it, the stack grows
// (nativeCtx.grow), and the call dispatched again is counted again, the
// first count taken back (nativeGrowUncount). A 100-deep recursion run
// three times on one runtime counts its 303 runs and 299 calls, where it
// had counted 308 calls, the stack growing from 8 records to 128. The
// runtime runs the native handlers, whose feedback dispatcher counts,
// whatever the build's default (a verifying build's is the Go table,
// which counts nothing).
func TestJITRegFrequencyPastFrameRoom(t *testing.T) {
	r := newTestRuntime(t, Config{JIT: true}.withNative(true))
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	jitDefaultsForTest(r)
	if _, err := r.Run(compileForTest(t, `function rec(n) { return n ? rec(n - 1) + 1 : 0 }`)); err != nil {
		t.Fatal(err)
	}
	cl := jitGlobalClosureForTest(r, "rec")
	for range 3 {
		v, err := r.Run(compileForTest(t, `rec(100)`))
		if err != nil || v != Int(100) {
			t.Fatalf("rec(100) = %v, %v", v, err)
		}
	}
	if got := cl.closureCode.jitRegCounts; len(got) != 2 || got[0] != 303 || got[1] != 299 {
		t.Fatalf("counts %v, want [303 299]", got)
	}
}
