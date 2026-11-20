//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"testing"
)

// regDerivedArrowSrc's derived constructors bind their this in an arrow's
// super(), which native code's this slot does not see: its derived_ret's
// copy of the slot leaves for Go to run the instruction, whose operand,
// undefined, the exit must hand on. The loops construct natively, with a
// call between, which kept the constructor's code native.
const regDerivedArrowSrc = `
class A { constructor(x) { this.x = x } }
class D extends A { constructor() { const f = () => super(4); f() } }
class E extends A { constructor(k) { const f = () => { super(k) }; f(); this.y = k + 1 } }
class F extends A { constructor() { const f = () => super(2); f(); return undefined } }
function plain() { var r = 0; for (var i = 0; i < 300; i++) { var d = new D(); Math.abs(1); r += d.x } return r }
function fields() { var r = 0; for (var i = 0; i < 300; i++) { var e = new E(i & 7); r += e.x * 10 + e.y } return r }
function both() { var r = 0; for (var i = 0; i < 300; i++) { var d = new D(), f = new F(); r += d.x + f.x + (d instanceof D ? 1 : 0) } return r }
`

// TestJITRegDerivedArrowSuper constructs derived classes whose this an
// arrow's super() binds, in loops native code runs, and checks every
// result against the interpreter's: before, derived_ret's exit left its
// operand out of the exit's state -- dead to the copy it was lowered to --
// and Go's derived_ret returned the window's stale register, an object of
// an earlier frame, or threw that a derived constructor may return only
// an object.
func TestJITRegDerivedArrowSuper(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	regBothMarking(t, func(t *testing.T) {
		rts := regNativeRuntimes(t, regDerivedArrowSrc)
		fields := 0
		for i := range 300 {
			fields += 11*(i&7) + 1
		}
		for _, c := range []struct {
			name, want string
		}{{"plain", "1200"}, {"fields", fmt.Sprint(fields)}, {"both", "2100"}} {
			for i, r := range rts {
				fn, _ := r.GetProp(Obj(r.Global()), r.Intern(c.name))
				for k := range 40 {
					v, err := r.Call(fn, Undefined, nil)
					if got := regNativeShow(r, v, err); got != c.want {
						t.Fatalf("runtime %d, %s call %d: %s, want %s", i, c.name, k, got, c.want)
					}
				}
			}
		}
	})
}
