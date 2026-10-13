//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
)

// An element read with its key updated, a[i++] or a[--i], whose value is
// used as no number -- an object, here -- is read by its cell, as any
// element read so is, whatever it holds, the key updated as the read
// succeeds. Before, only a read without an update was: the update's read
// speculated a number, failed its guard at every call, and, its site
// failed, was compiled again with the same guard -- Octane's Box2D, whose
// minified code walks its arrays so, deoptimized ten thousand times.
func TestJITElementUpdateReadsCells(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitAgreesForTest(t, `
		var objs = []; for (let i = 0; i < 64; i++) objs.push({v: i, s: 'k' + i})
		function walk(a, n) { let s = 0, i = 0, t = ''; while (i < n) { const o = a[i++]; s += o.v; t = o.s } return s + t + i }
		function back(a, n) { let s = 0, i = n; while (i > 0) { const o = a[--i]; s = (s * 3 + o.v) | 0 } return s + ',' + i }
		function past(a, n) { let s = '', i = n - 2; for (let k = 0; k < 4; k++) s += a[i++] === undefined ? 'u' : 'd'; return s + i }`,
		`walk(objs, 64)`, `walk(objs, 64)`, `back(objs, 64)`, `back(objs, 64)`, `past(objs, 64)`, `past(objs, 64)`,
		`walk(objs, 64) + back(objs, 64) + past(objs, 64)`)
	for _, name := range []string{"walk", "back"} {
		e := jitNativeForTest(t, r, name)
		updates := 0
		for _, in := range e.ssaProg.Code {
			if in.Op == ir.ArrayUpdate {
				updates++
			}
		}
		if updates == 0 {
			t.Fatalf("%s reads no element with its key updated", name)
		}
		for _, site := range e.failed {
			if e.ssaProg.Code[site].Op == ir.ArrayUpdate {
				t.Errorf("%s: the update's read failed its guard", name)
			}
		}
		if e.ssaStats.guards != 0 {
			t.Errorf("%s: %d guards failed in its last code", name, e.ssaStats.guards)
		}
	}
}
