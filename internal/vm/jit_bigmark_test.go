//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// regBigMarkSrc's loop adds, subtracts, multiplies and compares BigInts of
// the int64 range, which the register interpreter's native handlers make
// themselves, beside a loop counter's compare of numbers.
const regBigMarkSrc = `
function big(n) {
	let a = 1n, b = 3n, s = 0n, c = 0
	for (let i = 0; i < n; i++) {
		s = s + a * b - a
		if (s < b) c++
		if (a >= s) c += 2
		a = a + 1n
	}
	return String(s) + ":" + c
}
`

// TestJITRegNativeBigIntMarks runs regBigMarkSrc's big in the register
// interpreter's native handlers, in a runtime with the JIT, kept from
// compiling it, and in one without, the collector idle and marking: the
// answers are exact and alike, and the feedback dispatcher's handlers
// (nativegen's feedbackBigOps) left each operator of two BigInts to Go
// once, which marked it as one that met BigInts of the int64 range
// (regBigOf, regBigSeen), the number compare unmarked -- where plain's,
// which a runtime without the JIT runs, mark nothing. Compiled then, the
// code makes them natively from its first compile, no guard failing.
func TestJITRegNativeBigIntMarks(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	if !regNativeOn {
		t.Skip("no native handlers in this build")
	}
	const want = "2550:1"
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			rts := regNativeRuntimes(t, regBigMarkSrc)
			r := rts[0]
			// The interpreter's alone first, at the JIT's own thresholds,
			// which one call does not reach.
			r.jitStress = jitStressConfig{}
			run := func() {
				t.Helper()
				var got [2]string
				for i, r := range rts {
					fn, _ := r.GetProp(Obj(r.Global()), r.Intern("big"))
					v, err := r.Call(fn, Undefined, []Value{Float(50)})
					got[i] = regNativeShow(r, v, err)
				}
				if got[0] != got[1] || got[0] != want {
					t.Fatalf("big: with the JIT %q, without %q, want %q", got[0], got[1], want)
				}
			}
			run()
			// A runtime without the JIT, of register code of its own: the
			// marks are the code's, which runtimes running it share.
			plainRT := newTestRuntime(t, Config{})
			t.Cleanup(plainRT.Close)
			prog := compileForTest(t, regBigMarkSrc)
			if _, err := plainRT.Run(prog); err != nil {
				t.Fatal(err)
			}
			fn, _ := plainRT.GetProp(Obj(plainRT.Global()), plainRT.Intern("big"))
			if v, err := plainRT.Call(fn, Undefined, []Value{Float(50)}); regNativeShow(plainRT, v, err) != want {
				t.Fatalf("big without the JIT: %s", regNativeShow(plainRT, v, err))
			}
			for k, r := range []*Runtime{r, plainRT} {
				fn, _ := r.GetProp(Obj(r.Global()), r.Intern("big"))
				cl := fn.Object().fn().closure
				if cl.reg == nil {
					t.Fatal("big has no register code")
				}
				if k == 0 && cl.closureCode.jitReg != nil {
					t.Fatal("big compiled before its marks were looked at")
				}
				// Of each operator, the sites marked and not: every add,
				// subtract and multiply, and the compares of BigInts, but
				// not the loop's i < n, of numbers.
				marked, unmarked := map[string]int{}, map[string]int{}
				for pc, w := range cl.reg.rc.Code {
					kind := ""
					switch w.Op() {
					case bytecode.RAdd, bytecode.RSub, bytecode.RMul:
						kind = w.Op().String()
					case bytecode.RJLt, bytecode.RJNLt, bytecode.RJNGt, bytecode.RJGe, bytecode.RJNGe,
						bytecode.RJLe, bytecode.RJNLe, bytecode.RJGt:
						kind = "compare"
					default:
						continue
					}
					bigs, wide, heap := regBigSeen(cl, cl.reg.rc, pc)
					switch {
					case wide || heap:
						t.Errorf("%s at %d marked wide %v, on the heap %v", w.Op(), pc, wide, heap)
					case bigs:
						marked[kind]++
					default:
						unmarked[kind]++
					}
				}
				if k == 1 {
					if len(marked) != 0 {
						t.Errorf("without the JIT, sites marked: %v", marked)
					}
					continue
				}
				if marked["add"] < 2 || marked["sub"] < 1 || marked["mul"] < 1 || unmarked["add"]+unmarked["sub"]+unmarked["mul"] != 0 ||
					marked["compare"] < 2 || unmarked["compare"] < 1 {
					t.Errorf("sites marked %v, unmarked %v", marked, unmarked)
				}
			}
			// Compiled now, from the marks.
			r.SetJITWarmUp(1, 1)
			big, _ := r.GetProp(Obj(r.Global()), r.Intern("big"))
			for i := 0; i < 4 || big.Object().fn().closure.closureCode.jitReg == nil && i < 1000; i++ {
				run()
			}
			guards, entries := r.RegNativeStats().Guards, r.RegNativeStats().Entries
			run()
			if n := r.RegNativeStats().Guards - guards; n != 0 {
				t.Errorf("%d guards failed once compiled", n)
			}
			if r.RegNativeStats().Entries == entries {
				t.Error("big did not run natively")
			}
		})
	}
}
