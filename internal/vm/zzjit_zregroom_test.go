//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"strings"
	"testing"
)

// regRoomSrc's big has 50 registers and an array of them all, whose
// elements its code copies to scratch slots past its window: recursion of
// it reaches the VM's stack's end long before the frames' limit. tailA
// and tailB call each other in tail position.
func regRoomSrc() string {
	var b strings.Builder
	b.WriteString("function big(n) {\n")
	var parts []string
	for i := range 48 {
		fmt.Fprintf(&b, "\tvar v%d = n + %d\n", i, i)
		parts = append(parts, fmt.Sprintf("v%d", i))
	}
	b.WriteString("\tvar a = [v0, v1, v2, v3], s = `${v4}:${v5}:${v6}:${v7}`\n")
	b.WriteString("\tif (n === 0) return a.length + s.length + v47\n")
	b.WriteString("\treturn (big(n - 1) + a[3] - a[2] + s.length) | 0\n}\n")
	b.WriteString(`
function deep(n) { try { return big(n) } catch (e) { return e.name + ": " + e.message } }
function tailA(n, s) { "use strict"; return n === 0 ? s : tailB(n - 1, (s + n) | 0) }
function tailB(n, s) { "use strict"; return n === 0 ? -s : tailA(n - 1, (s ^ n) | 0) }
function tails(n) { var s = 0; for (var i = 0; i < 20; i++) s = (s + tailA(n + (i & 1), i)) | 0; return s }
`)
	return b.String()
}

// TestJITRegCallRoom: register code's native call checks the room for its
// callee's window alone, a constant, as the stack form's call checks its
// callee's locals and operands, and a call in tail position of another
// function's code the room for its window, the callee's tail entry making
// what its frame takes past the window: the contexts' StackEnd is the
// VM's stack's length less the most any register code compiled takes past
// its window (jitState's regRoom), so that a callee's frame fits past any
// window a call found room for. The checks had read the room each
// callee's link keeps, and a call in tail position its code's frame and
// operands' address too, at every call (call_fib and tail_mutual 1.06 and
// 1.25 times the stack form's code). A recursion to the stack's end of a
// function whose scratch slots its window is followed by answers as the
// interpreter does, its last call a RangeError, and calls in tail position
// of two functions as many as they like, with the collector idle and
// marking.
func TestJITRegCallRoom(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			rts := regNativeRuntimes(t, regRoomSrc())
			rts[0].SetJITReport(true)
			var got [2]string
			for k, r := range rts {
				for _, c := range []struct {
					fn string
					n  float64
				}{{"deep", 50}, {"deep", 1e6}, {"tails", 3000}} {
					fn, _ := r.GetProp(Obj(r.Global()), r.Intern(c.fn))
					for range 40 {
						v, err := r.Call(fn, Undefined, []Value{Float(c.n)})
						got[k] += regNativeShow(r, v, err) + ";"
					}
				}
			}
			// Node's answers; its message is capitalized.
			want := strings.Repeat("644;", 40) + strings.Repeat("RangeError: maximum call stack size exceeded;", 40) +
				strings.Repeat("-1074700;", 40)
			if got[0] != got[1] || got[0] != want {
				t.Errorf("with the JIT\n%s\nwithout\n%s\nwant\n%s", got[0], got[1], want)
			}
			r := rts[0]
			if r.RegNativeStats().Entries == 0 {
				t.Fatal("no native code entered")
			}
			s := r.jit
			if s.regRoom == 0 {
				t.Errorf("no register code's frame past its window counted: %+v", r.RegNativeStats())
				for _, f := range r.JITReport() {
					t.Logf("%+v", f)
				}
			}
			end := uint64(len(r.stack)) - s.regRoom
			for i, c := range s.ssaCtxs {
				if c.StackEnd != end && c.StackBase != nil {
					t.Errorf("context %d: StackEnd %d, want %d, the stack's %d less %d", i, c.StackEnd, end, len(r.stack), s.regRoom)
				}
			}
		})
	}
}
