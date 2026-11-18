//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"testing"
)

// regPushSrc's pushes are of no value, of one, and of two, three and five
// values at once, onto an array that grows past its room, its lengths and
// elements read back; onto a frozen array, which throws a TypeError at
// the first push, of whatever count; and onto an array whose length
// another push of the loop's made, then read.
const regPushSrc = `
function pushes(n) {
	var a = [], t = 0
	for (var i = 0; i < n; i++) {
		t += a.push()
		a.push(i)
		t += a.push(i, "x")
		t += a.push(" ", "disabled", '=""')
		t += a.push(1, 2, 3, 4, i)
	}
	return t + ":" + a.length + ":" + a[1] + a[2] + a[5] + a[11] + ":" + a[a.length - 1]
}
function frozen(n, k) {
	var a = Object.freeze([1, 2]), t = 0
	try {
		for (var i = 0; i < n; i++) {
			if (i === k) t += a.push(7, 8, 9)
			t++
		}
	} catch (e) {
		return e.name + ":" + t + ":" + a.length
	}
	return "no:" + t
}
`

// TestJITRegPushLeaf: register code's push of other than one value is a
// leaf, a call of Go of its own (ssa.GoLeaf), where the register form had
// made it a generic call of Go; a push of one value native code appends
// itself. Each answer is the interpreter's, with the collector idle and
// marking: a frozen array's TypeError, the count before it, the array as it
// was.
func TestJITRegPushLeaf(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			rts := regNativeRuntimes(t, regPushSrc)
			rts[0].jitStress = jitStressConfig{}
			var got [2]string
			for i, r := range rts {
				p, _ := r.GetProp(Obj(r.Global()), r.Intern("pushes"))
				f, _ := r.GetProp(Obj(r.Global()), r.Intern("frozen"))
				got[i] = ""
				for range 4 {
					v, err := r.Call(p, Undefined, []Value{Float(3000)})
					got[i] += regNativeShow(r, v, err) + ";"
					v, err = r.Call(f, Undefined, []Value{Float(3000), Float(2500)})
					got[i] += regNativeShow(r, v, err) + ";"
				}
			}
			const want = `197994000:33000:0x=""1:2999;TypeError:2500:2;`
			if got[0] != got[1] || got[0] != want+want+want+want {
				t.Errorf("with the JIT %q, without %q, want %q four times", got[0], got[1], want)
			}
			r := rts[0]
			for _, c := range []struct {
				name string
				want int
			}{{"pushes", 4}, {"frozen", 1}} {
				v, _ := r.GetProp(Obj(r.Global()), r.Intern(c.name))
				cl := v.Object().fn().closure
				e := r.jitRegFor(cl, cl.reg)
				if e == nil || e.p == nil {
					t.Fatalf("%s: not compiled: %+v", c.name, e)
				}
				fb := r.jitRegFeedbackFor(cl, e.p)
				leaves := 0
				for pc := range e.p.Code {
					if _, ok := fb.Leaf(pc); ok {
						leaves++
					}
				}
				if leaves != c.want {
					t.Errorf("%s: %d calls of leaves, want %d", c.name, leaves, c.want)
				}
			}
		})
	}
}
