//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"testing"
	"weak"

	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// A recursive function's calls of itself are decided at its compile --
// called natively, through its own link, which holds the code that
// compile makes -- as its exits would decide them: it is not compiled
// again for them. Left to Go, decided at their exits, they had cost a
// recursive function a second compile: RayTrace's rayTrace, EarleyBoyer's
// list walkers, as this method, too long to inline, of a tree's walk.
// Each answer is the interpreter's.
func TestJITRecursiveCallsSeeded(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	src := `function Walker(k) { this.k = k; this.n = 0 }
		Walker.prototype.walk = function (t, d) {
			if (t === null) { this.n = (this.n + 1) | 0; return (d * this.k) | 0 }
			let s = d, m = 0
			for (let i = 0; i < t.kids.length; i++) {
				const c = t.kids[i]
				if (c !== null && c.kids.length > m) m = c.kids.length
				s = (s + this.walk(c, d + 1)) | 0
				s = (s ^ (m << 3) ^ (this.n & 255)) | 0
			}
			return (s + m * this.k) | 0
		}
		var w = new Walker(3)
		function tree(n) { const kids = []; for (let i = 0; i < n; i++) kids.push(n > 1 ? tree(n - 1) : null); return { kids } }
		var big = tree(7)`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	jitDefaultsForTest(r)
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, src)); err != nil {
			t.Fatal(err)
		}
	}
	trace := r.global.getOwn(r.atoms.intern("w")).value().Object().proto.getOwn(r.atoms.intern("walk")).value().Object().fn().closure
	if len(trace.fn.Code) <= ssa.MaxInline {
		t.Fatalf("walk is %d instructions, short enough to inline", len(trace.fn.Code))
	}
	rounds := []string{`w.walk(big, 0)`, `w.walk(big, 0)`, `w.walk(big, 0)`, `w.walk(big, 0)`}
	var compiles []uint64
	for i, call := range rounds {
		wv, err := want.Run(compileForTest(t, call))
		if err != nil {
			t.Fatal(err)
		}
		var c uint64
		if r.jit != nil {
			c = r.jit.reoptimized
		}
		gv, err := r.Run(compileForTest(t, call))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.Number(), wv.Number(); got != want {
			t.Fatalf("round %d, %s: got %v, interpreter %v", i, call, got, want)
		}
		if r.jit != nil {
			c = r.jit.reoptimized - c
		}
		compiles = append(compiles, c)
	}
	e := r.jit.cache[weak.Make(trace.fn)]
	if e == nil || e.ssa == nil || e.entrySlow || e.invalid {
		t.Fatalf("walk did not run natively: %+v", e)
	}
	if r.jit.reoptimized != 0 {
		t.Fatalf("compiled again %v times by round", compiles)
	}
	if e.link == nil || e.link.in == 0 {
		t.Fatal("walk's calls of itself were not made natively")
	}
}
