//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"fmt"
	"runtime/debug"
	"testing"
)

// A call entry raises the VM stack's high water past its frame only where
// its context found it lower (abi.Context's HighSeen), and Go has every
// context forget what it found where it lowers the high water at a turn's
// end (jitForgetHighSeen): each turn's native recursion, as deep as the
// last's, raises it again, and the turn's end clears every slot its frames
// left past the stack's top. A context that remembered the last turn's
// high water would leave them, holding what the frames held. Warmed as
// deep as it runs, so that there are contexts for every level: a level past
// them is Go's, whose frames raise the high water past all of them; and\n// it makes no object, a pool's refill being a call of Go, which does too.\n// Each frame holds the object, which a slot left would keep.
func TestJITCallEntryHighWater(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	gc := debug.SetGCPercent(-1)
	t.Cleanup(func() { debug.SetGCPercent(gc) })
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprint(marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			r := jitRuntimeForTest(t, Config{JIT: true})
			if _, err := r.Run(compileForTest(t, `
				function rec(n, o) { if (n === 0) return o.v; return rec(n - 1, o) + 1; }
				for (var i = 0; i < 300; i++) rec(60, { v: 0 });`)); err != nil {
				t.Fatal(err)
			}
			cl := r.global.getOwn(r.atoms.intern("rec")).value().Object().fn().closure
			if e := r.jit.hint(cl); e == nil || e.ssa == nil || e.link == nil || e.link.in == 0 {
				t.Fatalf("rec does not call itself natively: %+v", e)
			}
			for turn := range 4 {
				v, err := r.Run(compileForTest(t, `rec(60, { v: 7 })`))
				if err != nil || v.Number() != 67 {
					t.Fatalf("turn %d: rec(60, {v: 7}) = %v, %v", turn, v, err)
				}
				// The turn's end lowered the high water: no context remembers it
				// higher.
				for k, c := range r.jit.ssaCtxs {
					if c.HighSeen != 0 {
						t.Fatalf("turn %d: context %d still finds the high water at %d, past %d", turn, k, c.HighSeen, r.stackHigh)
					}
				}
				for i := r.stackTop; i < len(r.stack); i++ {
					if r.stack[i] != (Value{}) {
						t.Fatalf("turn %d: slot %d past the stack's top (%d, high water %d) still holds %v", turn, i, r.stackTop, r.stackHigh, r.stack[i])
					}
				}
			}
		})
	}
}
