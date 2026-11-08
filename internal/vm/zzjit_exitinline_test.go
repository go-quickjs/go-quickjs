//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"slices"
	"testing"
)

const exitInlineSetup = `
	function inner(o, k) {
		if (k > 1e9) throw new RangeError('big ' + k)
		return o.v * 2 + k
	}
	function mid(o, k) { return inner(o, k) + 1 }
	function outer(os, n) {
		let s = 0
		for (let i = 0; i < n; i++) {
			const o = os[i % os.length]
			s += mid(o, i) + mid(o, i + 1)
		}
		return s
	}
	function outerTry(os, n, big) {
		let s = ''
		for (let i = 0; i < n; i++) {
			try { s += mid(os[i % os.length], i === big ? 2e9 : i) + ',' } catch (e) { s += e.name + ':' + e.message + '|' + e.stack + ';' }
		}
		return s
	}
	var nums = [{v: 1}, {v: 2}, {v: 3}]
`

// An exit inside a callee inlined two levels deep makes both inlined
// frames (abi.ExitDescriptor's Inlines, kept in the code's exit frame):
// a deoptimization there, by a value of another type and an object of
// another shape, goes on in the interpreter with the frames' locals; a
// throw there is caught by the root's try, its stack trace the
// interpreter's, with both callees' frames. The same holds while the
// collector marks (jitMarkingForTest) and while it runs in a loop. Each
// answer is the interpreter's.
func TestJITExitTwoInlinedFrames(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, mode := range []string{"idle", "marking", "collecting"} {
		t.Run(mode, func(t *testing.T) {
			switch mode {
			case "marking":
				jitMarkingForTest(t)
			case "collecting":
				t.Cleanup(jitCollectingForTest(t))
			}
			inlined := jitInlinedForTest(t)
			both := jitBothForTest(t, exitInlineSetup)
			var warm []string
			for range 5 {
				warm = append(warm, `outer(nums, 200)`, `outerTry(nums, 50, -1)`)
			}
			both.agree(t, warm...)
			r := both.jit
			midC, innerC := jitClosureForTest(t, r, `mid`), jitClosureForTest(t, r, `inner`)
			for _, fn := range []string{"outer", "outerTry"} {
				if !slices.Contains(inlined[fn], midC) || !slices.Contains(inlined[fn], innerC) {
					t.Fatalf("%s inlines %x, not mid %x and inner %x", fn, inlined[fn], midC, innerC)
				}
			}
			exits := r.jit.hosts + r.jit.guards
			both.agree(t,
				// A string's v, and then an object of another shape, two
				// frames down.
				`outer([{v: 1}, {v: 'a'}, {v: 2}], 30)`,
				`outer([{w: 0, v: 5}, {v: 1}], 30)`,
				// A throw two frames down, caught by the root's try.
				`outerTry(nums, 6, 3)`,
				`outerTry([{v: 'x'}, {v: 1}], 6, 4)`,
				// Uncaught, out through both inlined frames and the root's.
				`outer([{v: 1}, {get v() { throw new TypeError('getter') }}], 4)`,
				`outer(nums, 300)`, `outerTry(nums, 20, 10)`)
			if r.jit.hosts+r.jit.guards == exits {
				t.Errorf("no exit left native code")
			}
		})
	}
}
