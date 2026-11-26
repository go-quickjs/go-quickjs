//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

func TestJITInterpreterStringAdds(t *testing.T) {
	for _, native := range []bool{false, true} {
		for _, marking := range []bool{false, true} {
			for _, tc := range []struct {
				expr, first, next string
				op                bytecode.ROp
			}{
				{"a+b", `"2","3"`, `2,3`, bytecode.RAdd},
				{"a+1", `"2",0`, `2,0`, bytecode.RAddI},
				{"(a+b)|0", `"2","3"`, `2,3`, bytecode.RAddI32},
				{"(a+1)|0", `"2",0`, `2,0`, bytecode.RAddII32},
			} {
				t.Run(fmt.Sprintf("native=%v/marking=%v/%s", native, marking, tc.expr), func(t *testing.T) {
					nativeForTest(t, native)
					if marking {
						jitMarkingForTest(t)
					}
					r, run := jitTierRuntimeForTest(t, 8, `
function f(a,b,n){let s;for(let i=0;i<n;i++)s=`+tc.expr+`;return s}`)
					run(`f(` + tc.first + `,2)`)
					fn, _ := r.GetProp(Obj(r.Global()), r.Intern("f"))
					cl := fn.Object().fn().closure
					if jitHasCodeOfForTest(r, cl) {
						t.Fatal("compiled before interpreter feedback was inspected")
					}
					marks := cl.reg.stringAdds.Load()
					found := false
					for pc, w := range cl.reg.rc.Code {
						if w.Op() == tc.op {
							found = true
							if marks == nil || (*marks)[pc>>5].Load()&(uint32(1)<<(pc&31)) == 0 {
								t.Fatalf("%s at %d has no string feedback", w.Op(), pc)
							}
						}
					}
					if !found {
						t.Fatalf("no %s in f", tc.op)
					}
					for range 20 {
						run(`f(` + tc.first + `,2)`)
					}
					e := jitNativeForTest(t, r, "f")
					wantDeopts := uint16(0)
					if (tc.op == bytecode.RAddI32 || tc.op == bytecode.RAddII32) && !r.runNative {
						// The fused form learns its string's ToInt32
						// conversion separately from the addition where no
						// operand lattice says the instruction met a string
						// (the Go table's): with one, the conversion is
						// generic from the first compile.
						wantDeopts = 1
					}
					if e.deopts != wantDeopts || len(e.failedEntries) != 0 {
						t.Fatalf("%d deoptimizations, want %d; entry failures %v", e.deopts, wantDeopts, e.failedEntries)
					}
					// A mark chooses general addition; it proves no operand's type.
					run(`f(` + tc.next + `,2)`)
					run(`f({valueOf(){return "4"}},5,2)`)
					run(`(()=>{try{return f(Symbol(),1,2)}catch(e){return e.name}})()`)
				})
			}
		}
	}
}

func TestJITStringAddsSharedCode(t *testing.T) {
	prog := compileForTest(t, `
function f(a,b,n){let s;for(let i=0;i<n;i++)s=a+b;return s}
f`)
	errs := make(chan error, 8)
	for i := range 8 {
		go func() {
			r := newTestRuntime(t, Config{JIT: true})
			defer r.Close()
			r.jitCallThreshold, r.jitICThreshold = 8, 8
			fn, err := r.Run(prog)
			if err != nil {
				errs <- err
				return
			}
			args := []Value{Int32(2), Int32(3), Int32(3)}
			want := "5"
			if i%2 == 0 {
				args[0], want = Str(NewString("2")), "23"
			}
			for range 24 {
				v, err := r.Call(fn, Undefined, args)
				if got := regNativeShow(r, v, err); got != want {
					errs <- fmt.Errorf("runtime %d: got %s, want %s", i, got, want)
					return
				}
			}
			errs <- nil
		}()
	}
	for range 8 {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
}

func TestInterpreterStringAddsWithoutJIT(t *testing.T) {
	r := newTestRuntime(t, Config{})
	t.Cleanup(r.Close)
	fn, err := r.Run(compileForTest(t, `function f(a,b){return a+b} f("a","b"); f`))
	if err != nil {
		t.Fatal(err)
	}
	if fn.Object().fn().closure.reg.stringAdds.Load() != nil {
		t.Fatal("runtime without the JIT allocated string-add feedback")
	}
}

// o.name += x of a string x is marked as add's additions are, so that the
// first compile makes it Go's rather than deoptimize at it.
func TestJITInterpreterStringAddProps(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(fmt.Sprintf("native=%v", native), func(t *testing.T) {
			nativeForTest(t, native)
			r, run := jitTierRuntimeForTest(t, 8, `
function f(o,a,n){for(let i=0;i<n;i++)o.t+=a;return o.t}`)
			run(`f({t:""},"x",2)`)
			fn, _ := r.GetProp(Obj(r.Global()), r.Intern("f"))
			cl := fn.Object().fn().closure
			if jitHasCodeOfForTest(r, cl) {
				t.Fatal("compiled before interpreter feedback was inspected")
			}
			marks := cl.reg.stringAdds.Load()
			found := false
			for pc, w := range cl.reg.rc.Code {
				if w.Op() == bytecode.RAddProp {
					found = true
					if marks == nil || (*marks)[pc>>5].Load()&(uint32(1)<<(pc&31)) == 0 {
						t.Fatalf("add_prop at %d has no string feedback", pc)
					}
				}
			}
			if !found {
				t.Fatal("no add_prop in f")
			}
			for range 20 {
				run(`f({t:""},"x",2)`)
			}
			if e := jitNativeForTest(t, r, "f"); e.deopts != 0 || len(e.failedEntries) != 0 {
				t.Fatalf("%d deoptimizations; entry failures %v", e.deopts, e.failedEntries)
			}
		})
	}
}
