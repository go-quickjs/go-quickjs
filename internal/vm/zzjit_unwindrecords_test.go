//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"fmt"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// A local a function assigned keeps its old value in a value that goes on
// reading the word out of the local's slot, where native code left it: its
// frame is written only at an exit. A native call's level Go finishes
// (jitUnwindNative) has the records of its call applied, for what reads its
// frame meanwhile, and its code goes on after the call. Written over by
// the record, the slot had the value read another's pointer: Box2D's
// CollidePolygons, in the background, took a transform for a polygon.
func TestJITLevelResumesWithItsSlots(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	jitNoInline = func(fn *bytecode.Function) bool { return fn.Name == "inner" || fn.Name == "swap" }
	t.Cleanup(func() { jitNoInline = nil })
	setup := `
		var obj = {a: 1}, A = {v: 1}, B = {v: 2}
		function inner(x, k) {
			const o = x === k ? 1 : obj
			o.a = 1
			return 0
		}
		function swap(a, b, i, k) {
			var t = a
			a = b
			b = t
			inner(i, k)
			return t.v * 10 + a.v
		}
		function run(n, k) {
			let s = 0
			for (let i = 0; i < n; i++) s += swap(A, B, i, k)
			return s
		}
	`
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			jitAgreesForTestWith(t, jitStressConfig{}, setup, `run(20000, -1)`, `run(20000, 500)`, `run(20000, 19000)`, `run(20000, 7)`)
		})
	}
}

// So too for a generator a loop's step resumed natively, whose frame is its
// own; and what the callee assigned of a local its closures captured stays
// assigned, in the slot the frame holds its cell open on.
func TestJITLevelKeepsWhatItsCalleeAssigned(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	jitNoInline = func(fn *bytecode.Function) bool {
		return fn.Name == "inner" || fn.Name == "swap" || fn.Name == "set" || fn.Name == "gen"
	}
	t.Cleanup(func() { jitNoInline = nil })
	setup := `
		var obj = {a: 1}, A = {v: 1}, B = {v: 2}
		function inner(x, k) {
			const o = x === k ? 1 : obj
			o.a = 1
			return 0
		}
		function* gen(a, b, i, k) {
			var t = a
			a = b
			b = t
			inner(i, k)
			yield t.v * 10 + a.v
		}
		function steps(n, k) {
			let s = 0
			for (let i = 0; i < n; i++)
				for (const x of gen(A, B, i, k)) s += x
			return s
		}
		function swap(a, b, i, k) {
			var t = a, c = a
			const set = () => { inner(i, k); c = b }
			a = b
			set()
			return t.v * 100 + a.v * 10 + c.v
		}
		function run(n, k) {
			let s = 0
			for (let i = 0; i < n; i++) s += swap(A, B, i, k)
			return s
		}
	`
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			jitAgreesForTestWith(t, jitStressConfig{}, setup, `steps(20000, -1)`, `steps(20000, 500)`, `steps(20000, 19000)`,
				`run(20000, -1)`, `run(20000, 500)`, `run(20000, 19000)`)
		})
	}
}
