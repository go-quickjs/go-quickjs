//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"testing"
	"weak"
)

// lazyFramesSetup recurses natively, each level a native call, and at the
// bottom calls through Go what the JIT does not compile -- a function with
// a with statement -- which reads the frames of the levels above it: a
// stack trace's, a thrown error's, a sloppy function's caller chain. It
// also has code compiled again while the levels are in theirs --
// recompile, the host's, compiles helper again, which no level is in:
// its old code is not closed until they have left (closeSSA), as no code
// is while a call has put their frames off. The levels' own code, which
// they return into, is not compiled again while they are in it
// (otherwiseInUse).
const lazyFramesSetup = `
	function trace() { with ({}) {} return new Error("e").stack.split("\n").slice(1, 12).join("|") }
	function thrower() { with ({}) {} throw new Error("t") }
	function callers() { with ({}) {} var n = 0; for (var f = callers.caller; f && n < 30; f = f.caller) n++; return n }
	function P() {} P.prototype.k = 1
	var objs = [new P(), new P()]
	function helper(x) { return x + 1 }
	function change() { with ({}) {} P.prototype.k = P.prototype.k + 1; return recompile() }
	function down(n, what) {
		if (n === 0) {
			if (what === 0) return trace()
			if (what === 1) return callers()
			if (what === 2) return change() + objs[n & 1].k
			return thrower()
		}
		return helper(down(n - 1, what)) - 1 + (what === 2 ? objs[n & 1].k : 0)
	}
	function sloppyDown(n) { return n === 0 ? callers() : sloppyDown(n - 1) }
	function caught(n) { try { return down(n, 3) } catch (e) { return e.stack.split("\n").slice(1, 12).join("|") } }`

func lazyFramesRounds() []string {
	var rounds []string
	for range 30 {
		rounds = append(rounds, `down(20, 0)`, `sloppyDown(20)`, `down(20, 2)`, `caught(20)`)
	}
	return rounds
}

// The native levels below the one that calls Go have frames made only
// when one is read (jitLazyLevels): a stack trace, a thrown error's, a
// function's caller chain see every level as the interpreter does, and
// code invalidated while the levels are in it is returned into, closed
// only once they have left. Each answer is the interpreter's.
func TestJITLazyFrames(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	jitLazyFramesRan(t, jitLazyFramesAgree(t))
}

// jitLazyFramesAgree runs lazyFramesRounds in the interpreter and with the
// JIT, recompile the host's in both, and checks each answer is the
// interpreter's.
func jitLazyFramesAgree(t *testing.T) *Runtime {
	t.Helper()
	install := func(rt *Runtime) {
		rt.global.setOwnRaw(rt.atoms.intern("recompile"), rt.NewFunction("recompile", 0,
			func(rt *Runtime, _ Value, _ []Value) (Value, error) {
				if rt.jit == nil {
					return Int(0), nil
				}
				cl := rt.global.getOwn(rt.atoms.intern("helper")).value().Object().fn().closure
				if e := rt.jit.cache[weak.Make(cl.fn)]; e != nil && e.ssa != nil {
					e.reopt = true
					rt.jitReoptimize(cl, e)
				}
				return Int(0), nil
			}), propDefault)
	}
	want := New(Config{})
	t.Cleanup(func() { want.Close(); want.ReleaseClosed() })
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		install(rt)
		if _, err := rt.Run(compileForTest(t, lazyFramesSetup)); err != nil {
			t.Fatal(err)
		}
	}
	for i, src := range lazyFramesRounds() {
		src = "''+(" + src + ")"
		wv, werr := want.Run(compileForTest(t, src))
		gv, gerr := r.Run(compileForTest(t, src))
		switch {
		case (werr == nil) != (gerr == nil):
			t.Fatalf("round %d (%s): error %v, the interpreter's %v", i, src, gerr, werr)
		case werr != nil:
			if gerr.Error() != werr.Error() {
				t.Fatalf("round %d (%s): error %q, the interpreter's %q", i, src, gerr, werr)
			}
		case gv.String().Go() != wv.String().Go():
			t.Fatalf("round %d (%s): got %s, the interpreter's %s", i, src, gv.String().Go(), wv.String().Go())
		}
	}
	return r
}

// TestJITLazyFramesMarking is TestJITLazyFrames while the collector marks.
func TestJITLazyFramesMarking(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	jitMarkingForTest(t)
	jitClobberStoresForTest(t)
	jitLazyFramesRan(t, jitLazyFramesAgree(t))
}

func jitLazyFramesRan(t *testing.T, r *Runtime) {
	t.Helper()
	if s := r.jit; s.lazyCalls == 0 || s.lazyMade == 0 || s.lazyHeld == 0 || len(s.lazy) != 0 || s.lazyTop != 0 {
		t.Fatalf("frames put off by %d calls, made %d times, %d codes held for them, %d still put off (top %d); want each, none left",
			s.lazyCalls, s.lazyMade, s.lazyHeld, len(s.lazy), s.lazyTop)
	}
	if len(r.jit.goPinned) != 0 || len(r.jit.retired) != 0 {
		t.Fatalf("calls of Go left %d pinned, %d retired", len(r.jit.goPinned), len(r.jit.retired))
	}
}
