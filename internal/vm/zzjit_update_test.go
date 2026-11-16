//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
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
	t.Parallel()
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
		// Each read with its key updated: the stack form's program's
		// ArrayUpdate, by its PC; register code's get_index_post or
		// get_index_pre (x[i++], x[--i]), by its register pc, the sites
		// its code's failed guards name.
		var update func(site int32) bool
		var sites int
		if jitRegCodeForTest(r, name) {
			rc := jitGlobalClosureForTest(r, name).fn.RegCode()
			update = func(rpc int32) bool {
				op := rc.Code[rpc].Op()
				return op == bytecode.RGetIndexPost || op == bytecode.RGetIndexPre
			}
			sites = len(rc.Code)
		} else {
			update = func(pc int32) bool { return e.ssaProg.Code[pc].Op == ir.ArrayUpdate }
			sites = len(e.ssaProg.Code)
		}
		updates := 0
		for site := range sites {
			if update(int32(site)) {
				updates++
			}
		}
		if updates == 0 {
			t.Fatalf("%s reads no element with its key updated", name)
		}
		for _, site := range e.failed {
			if update(site) {
				t.Errorf("%s: the update's read failed its guard", name)
			}
		}
		if jitRegCodeForTest(r, name) {
			// Register code's counts outlive its compiles: its last code's
			// are those of a run of it now, in which it fails no guard and
			// is not replaced.
			code, guards := jitRegEntryForTest(r, name), e.ssaStats.guards
			v, err := r.Run(compileForTest(t, name+"(objs, 64)"))
			want := map[string]string{"walk": "2016k6364", "back": "-726101408,0"}[name]
			if err != nil || v.String().Go() != want {
				t.Fatalf("%s(objs, 64) = %v, %v; want %s", name, v, err, want)
			}
			if n := e.ssaStats.guards - guards; n != 0 || jitRegEntryForTest(r, name) != code {
				t.Errorf("%s: %d guards failed in its last code", name, n)
			}
			continue
		}
		if e.ssaStats.guards != 0 {
			t.Errorf("%s: %d guards failed in its last code", name, e.ssaStats.guards)
		}
	}
}
