//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// regLongSrc's callers call long functions natively, once a call: past
// what a callee inlined may have (ssa.MaxInline), wide, once in twenty-five of its
// caller's calls; past it, narrow, at each; and past what a function
// inlines in all (ssa.MaxInlineTotal), huge, at each.
func regLongSrc() string {
	body := func(n int) string {
		var b strings.Builder
		for i := range n {
			fmt.Fprintf(&b, "s = (s * %d + a) | 0; ", i%7+2)
		}
		return b.String()
	}
	return fmt.Sprintf(`
function wide(a) { var s = 1; %s return s }
function narrow(a) { var s = 2; %s return s }
function huge(a) { var s = 3; %s return s }
function seldom(n) { var t = 0; for (var i = 0; i < 50; i++) t += i; return n %% 25 == 0 ? t + wide(n) : t }
function often(n) { var t = 0; for (var i = 0; i < 50; i++) t += i; return t + narrow(n) }
function always(n) { var t = 0; for (var i = 0; i < 50; i++) t += i; return t + huge(n) }
`, body(20), body(20), body(120))
}

// TestJITRegLongCalleeWarms has native callers call functions too long to
// inline (ssa.MaxInline) that have no code: one its caller calls seldom,
// and one past what a function inlines in all, are compiled once their
// budgets are spent, not at their callers' first call; one its caller calls
// at each call, at once. Before, every such callee was compiled at its
// native callers' first call: Mandreel's set-up, __mandreel_internal_init
// and BenchmarkDemo::initPhysics among it, each run once an iteration, was
// compiled at every iteration after the first, 4.8 MB of code for
// initPhysics alone.
func TestJITRegLongCalleeWarms(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	regBothMarking(t, func(t *testing.T) {
		rts := regNativeRuntimes(t, regLongSrc())
		r := rts[0]
		const budget = 40
		r.SetJITWarmUp(budget, budget)
		closure := func(name string) *closure {
			fn, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
			return fn.Object().fn().closure
		}
		for _, c := range []struct {
			name    string
			min     int
			inlines bool
		}{{"wide", ssa.MaxInline, true}, {"narrow", ssa.MaxInline, true}, {"huge", ssa.MaxInlineTotal, false}} {
			cl := closure(c.name)
			rc := cl.reg
			if rc == nil {
				rc = r.regOf(cl)
			}
			if n := jitRegInlineSize(rc); n <= c.min || c.inlines && n > ssa.MaxInlineTotal {
				t.Fatalf("%s is %d instructions long, want past %d", c.name, n, c.min)
			}
		}
		want := func(name string, n int) string {
			// The interpreter's answer.
			fn, _ := rts[1].GetProp(Obj(rts[1].Global()), rts[1].Intern(name))
			v, err := rts[1].Call(fn, Undefined, []Value{Float(float64(n))})
			return regNativeShow(rts[1], v, err)
		}
		run := func(name string, from, to int) {
			t.Helper()
			fn, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
			for n := from; n < to; n++ {
				v, err := r.Call(fn, Undefined, []Value{Float(float64(n))})
				if got, w := regNativeShow(r, v, err), want(name, n); got != w {
					t.Fatalf("%s(%d) = %q, want %q", name, n, got, w)
				}
			}
		}
		code := func(name string) bool { return closure(name).closureCode.jitReg != nil }
		// The callers compiled, each calling its callee natively, through
		// its link.
		for _, name := range []string{"seldom", "often", "always"} {
			run(name, 0, 2*budget)
			if !code(name) {
				t.Fatalf("%s not compiled", name)
			}
		}
		if !code("narrow") {
			t.Errorf("narrow, called at each of often's calls, not compiled for it")
		}
		// seldom has called wide 4 times, always huge 80 times: each
		// natively once compiled, its budget spent.
		if code("wide") {
			t.Errorf("wide, called once in twenty-five of seldom's calls, compiled before its budget was spent")
		}
		run("seldom", 2*budget, 60*budget)
		run("always", 2*budget, 4*budget)
		for _, name := range []string{"wide", "huge"} {
			if !code(name) {
				t.Errorf("%s not compiled once its budget was spent", name)
			}
		}
		if e := closure("huge").closureCode.jitReg; e != nil && e.life.starts+r.jit.regLinks[e.key].in == 0 {
			t.Errorf("huge's code was never called")
		}
	})
	// With no native caller known, the call is not one made often: a callee
	// past ssa.MaxInline, within ssa.MaxInlineTotal or past it, warms up.
	t.Run("no caller", func(t *testing.T) {
		var b strings.Builder
		// long: past ssa.MaxInlineTotal; mid: past ssa.MaxInline, within it.
		b.WriteString("function long(x) { var s = x;")
		for i := 0; i < 120; i++ {
			fmt.Fprintf(&b, " s = (s ^ (s << %d)) + %d | 0;", i%5+1, i)
		}
		b.WriteString(" return s }\nfunction mid(x) { var s = x;")
		for i := 0; i < 16; i++ {
			fmt.Fprintf(&b, " s = (s ^ (s << %d)) + %d | 0;", i%5+1, i)
		}
		b.WriteString(" return s }\n")
		rts := regNativeRuntimes(t, b.String())
		r := rts[0]
		for _, c := range []struct {
			name string
			long bool
		}{{"long", true}, {"mid", false}} {
			// Run once, by the interpreter, which gives it its register code.
			fn, _ := r.GetProp(Obj(r.Global()), r.Intern(c.name))
			if _, err := r.Call(fn, Undefined, []Value{Float(3)}); err != nil {
				t.Fatal(err)
			}
			cl := jitGlobalClosure(r, c.name)
			if cl.reg == nil {
				t.Fatalf("%s: no register code", c.name)
			}
			if n := jitRegInlineSize(cl.reg); n <= ssa.MaxInline || c.long != (n > ssa.MaxInlineTotal) {
				t.Fatalf("%s: %d instructions, not the length the test is about", c.name, n)
			}
			r.jitRegNativeCalleeN(cl, cl.reg, 1, nil)
			if cl.closureCode.jitReg != nil && cl.closureCode.jitReg.code != nil {
				t.Errorf("%s: compiled for a call of no known caller, before its budget was spent", c.name)
			}
		}
	})
}

// TestJITRegLongCalleeWarmsFirst checks the other half of
// TestJITRegLongCalleeWarms's huge: its caller's first calls of it, before
// its budget is spent, leave it with no code.
func TestJITRegLongCalleeWarmsFirst(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	rts := regNativeRuntimes(t, regLongSrc())
	r := rts[0]
	r.SetJITWarmUp(1000, 1000)
	fn, _ := r.GetProp(Obj(r.Global()), r.Intern("always"))
	caller := fn.Object().fn().closure
	for n := range 20 {
		if _, err := r.Call(fn, Undefined, []Value{Float(float64(n))}); err != nil {
			t.Fatal(err)
		}
	}
	// always compiled: its loop, sampled -- and huge, called by it at each
	// call, not yet.
	e := r.jitRegFor(caller, caller.reg)
	if e == nil || e.code == nil {
		t.Fatalf("always not compiled: %+v", e)
	}
	caller.closureCode.setJITReg(e)
	for n := 20; n < 40; n++ {
		if _, err := r.Call(fn, Undefined, []Value{Float(float64(n))}); err != nil {
			t.Fatal(err)
		}
	}
	hv, _ := r.GetProp(Obj(r.Global()), r.Intern("huge"))
	if hc := hv.Object().fn().closure; hc.closureCode.jitReg != nil {
		t.Errorf("huge compiled at its native caller's call, before its budget was spent")
	}
}
