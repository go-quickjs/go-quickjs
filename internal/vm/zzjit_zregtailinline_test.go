//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// regTailInlineSrc's callers call register functions whose calls are in
// tail position (tail_call_w, tail_call_method_w), which register code
// inlines, a call in tail position as the ordinary call: a method returning
// one of two methods' results, as Richards' tasks return their scheduler's,
// and an arrow's, which reads the this it captured; a recursion whose calls
// of inc in tail position are inlined, made deep in the stack too, where
// the interpreter gives the frame up to the call; a strict pair calling
// each other in tail position, run deeper than any stack of frames would
// go; and calls whose results go to registers a closure captures, a
// construction's and a call's.
const regTailInlineSrc = `
class Task {
	constructor(n) { this.n = n; this.held = false }
	hold() { this.held = true; return this }
	release() { this.held = false; return this }
	step(i) { this.n = this.n + 1; return (i & 1) ? this.hold() : this.release() }
}
function tails(n) {
	var t = new Task(0), h = 0;
	for (var i = 0; i < n; i++) if (t.step(i).held) h++;
	return t.n * 1000 + h;
}
class Sched {
	constructor() { this.count = 0; this.f = (k) => this.bump(k) }
	bump(k) { this.count += k; return this.count }
}
var sched = new Sched(), arrow = sched.f;
function arrows(n) {
	var s = new Sched(), last = 0;
	for (var i = 0; i < n; i++) last = s.f(i & 3) + arrow(0);
	return last - sched.count;
}
function ping(n) { "use strict"; return n === 0 ? "done" : pong(n - 1) }
function pong(n) { "use strict"; return ping(n) }
function deep(n) { return ping(n) + n }
function inc(v) { "use strict"; return v + 1 }
function rec(n) { "use strict"; if (n === 0) return 0; var v = rec(n - 1); return inc(v) }
function Box(v) { this.v = v }
function sq(a) { return a * a }
function captured(n) {
	var s = 0;
	for (var i = 0; i < n; i++) {
		var b = new Box(i);
		var q = sq(i);
		s += get();
	}
	function get() { return b.v + q }
	return s;
}
`

// TestJITRegTailInline runs regTailInlineSrc's callers natively and in a
// runtime with no JIT, the collector idle and marking: the answers are
// exact and equal, and register code inlines the calls in tail position
// and the calls whose results a closure captures, as the stack form's
// code does -- tails its step and, in it, hold and release; the arrow
// bump; rec inc; captured Box and sq -- the stack deep enough for rec's
// calls of inc, and ping's and pong's of each other, to give their
// frames up all the same.
func TestJITRegTailInline(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			recordJITCompilesForTest(t)
			rts := regNativeRuntimes(t, regTailInlineSrc)
			rts[0].SetJITWarmUp(1, 1)
			for _, c := range []struct {
				name string
				n    float64
				want string
			}{
				{"tails", 2000, "2001000"},
				{"arrows", 2000, "3000"},
				{"deep", 300, "done300"},
				{"rec", 20, "20"},
				{"rec", 1000, "1000"},
				{"captured", 1000, "333333000"},
				// Past the depth at which the interpreter gives a frame
				// up to a call in tail position (tailCallDepth), as deep
				// as no stack of frames would go.
				{"deep", 200000, "done200000"},
			} {
				for i, r := range rts {
					fn, _ := r.GetProp(Obj(r.Global()), r.Intern(c.name))
					for range 8 {
						v, err := r.Call(fn, Undefined, []Value{Float(c.n)})
						if got := regNativeShow(r, v, err); got != c.want {
							t.Fatalf("%s(%v), JIT %v: %q, want %q", c.name, c.n, i == 0, got, c.want)
						}
					}
				}
			}
			r := rts[0]
			for _, c := range []struct {
				name string
				n    int
			}{{"tails", 3}, {"arrow", 1}, {"rec", 1}, {"captured", 2}} {
				fn, _ := r.GetProp(Obj(r.Global()), r.Intern(c.name))
				cl := fn.Object().fn().closure
				e := r.jit.regCache[r.jitKeyOf(cl.fn)]
				if e == nil || e.p == nil {
					t.Errorf("%s: not compiled", c.name)
					continue
				}
				if f, err := ssa.BuildWith(e.p, r.jitRegFeedbackFor(cl, e.p)); err != nil || len(f.Inlined) < c.n {
					t.Errorf("%s: %v, %d callees inlined, want %d", c.name, err, len(f.Inlined), c.n)
				}
			}
			// The same code, arm64's backend's, compiled on any host.
			if fns, _, inlined := regCompileARM64(t, r); inlined == 0 {
				t.Errorf("arm64: %d functions compiled, none inlining a callee", fns)
			}
		})
	}
}
