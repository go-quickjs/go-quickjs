//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"math"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
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
// other; and that a runtime without the JIT counts nothing.
func TestJITRegFrequency(t *testing.T) {
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			compiler.SetRegisterForm(true)
			prog := compileForTest(t, regFreqSrc)
			compiler.SetRegisterForm(false)
			SetRegTier(true)
			was := SetJITRegTier(true)
			t.Cleanup(func() { SetRegTier(bytecode.RegDefault()); SetJITRegTier(was) })
			for _, on := range []bool{true, false} {
				r := New(Config{JIT: on})
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
				if !on {
					if cl.jitRegCounts != nil {
						t.Fatalf("a runtime without the JIT counted %v", cl.jitRegCounts)
					}
					continue
				}
				if !regNativeOn {
					// The Go dispatcher counts nothing (QJS_NONATIVE, a
					// verifying build): no frequency is known.
					for _, n := range cl.jitRegCounts {
						if n != 0 {
							t.Fatalf("counts %v without the native handlers", cl.jitRegCounts)
						}
					}
					continue
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
			}
		})
	}
}
