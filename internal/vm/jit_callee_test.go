//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"strings"
	"testing"
)

// regCalleeSrc's callers call natively functions that do not read their
// function object, whose calls leave it out of their windows
// (ssa.CallSite's ReadsFunction), and functions that do, whose calls
// write it -- one after another at the same window, whose register 0 then
// holds another function. probe, a built-in, says what function each frame
// below it has, which the frames Go makes of the native levels must have
// as the interpreter's do (jitRegLevelCallee): of one site, of several, of
// a closure checked by its code, a method's, past a call in tail position
// that gave its frame to another.
const regCalleeSrc = `
function work(k) { var s = 0; for (var i = 0; i < 3; i++) s += k; return s }
function plain(k) { return probe() + work(k) }
var named = function named(k) { return typeof named + probe() + work(k) };
var o = { m: function (k) { return probe() + work(k) } };
o.alias = o.m;
function f1(k) { return "1" + probe() + work(k) }
function f2(k) { return "2" + probe() + work(k) }
function f3(k) { return "3" + probe() + work(k) }
function mk(j) { return function (k) { return j + probe() + work(k) } }
function ac() { return (arguments.callee === ac) + probe() + work(arguments[0]) }
function t1(n) { "use strict"; if (n == 0) return probe(); return t2(n - 1) + work(n) }
function t2(n) { "use strict"; return t1(n) }
function run(n) {
	var s = "", fs = [f1, f2, f3];
	for (var i = 0; i < n; i++) {
		s += named(i) + plain(i) + named(i) + o.alias(i) + named(i) + fs[i % 3](i) + named(i);
		s += mk(i % 3)(i) + named(i) + ac(i) + plain(i) + t1(i % 3) + o.m(i);
	}
	return s;
}
`

// TestJITRegCalleeLeftOut runs regCalleeSrc's run natively and with no JIT
// and compares what probe saw, the collector idle and marking.
func TestJITRegCalleeLeftOut(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			regNoInline(t)
			rts := regNativeRuntimes(t, regCalleeSrc)
			rts[0].SetJITWarmUp(1, 1)
			for _, r := range rts {
				// The functions of the frames below it, the calls' first.
				probe := r.newNativeFunc("probe", 0, func(r *Runtime, _ Value, _ []Value) (Value, error) {
					var b strings.Builder
					for i := r.frameDepth - 2; i >= 0; i-- {
						f := r.frameAt(i)
						if f.cl == nil || f.cl.fn.TopLevel {
							continue
						}
						name := "nil"
						if f.callee != nil {
							name = "other"
							if fd := f.callee.fn(); fd != nil && fd.closure == f.cl {
								name = f.cl.fn.Name
							}
						}
						fmt.Fprintf(&b, "<%s", name)
						if f.cl.fn.Name == "run" {
							break
						}
					}
					return Str(NewString(b.String())), nil
				})
				if err := r.SetProp(Obj(r.Global()), r.Intern("probe"), Obj(probe)); err != nil {
					t.Fatal(err)
				}
			}
			var got [2]string
			for i, r := range rts {
				fn, _ := r.GetProp(Obj(r.Global()), r.Intern("run"))
				for range 12 {
					v, err := r.Call(fn, Undefined, []Value{Float(60)})
					got[i] = regNativeShow(r, v, err)
				}
			}
			if got[0] != got[1] {
				t.Fatalf("with the JIT %q, without %q", got[0], got[1])
			}
			r := rts[0]
			native := map[string]uint64{}
			for key, l := range r.jit.regLinks {
				if fn := key.Value(); fn != nil {
					native[fn.Name] += l.in
				}
			}
			for _, name := range []string{"plain", "named", "m", "f1", "f2", "f3", "ac", "t1"} {
				if native[name] == 0 {
					t.Errorf("%s: never called natively (%v)", name, native)
				}
			}
		})
	}
}
