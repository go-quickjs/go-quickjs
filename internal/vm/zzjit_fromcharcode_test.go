//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"testing"
)

// String.fromCharCode of a code past Latin-1, which native code has no
// string of its own for (ssa.OpCodeChar), is made by a call of Go from
// native code (ssa.OpCodeCharOr, GoLeaf), as V8's code makes a string of
// any code unit: PDF.js's Font.charToGlyph, whose fonts' characters are past
// 255, had left native code at each call, the interpreter finishing the
// rest of each. Codes ToUint16 wraps, fractions and negatives the same.
// Each answer is the interpreter's.
func TestJITFromCharCodePastLatin1(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	src := `function glyphs(base, n) {
			let s = ""
			for (let i = 0; i < n; i++) {
				const c = String.fromCharCode(base + (i & 15))
				if ((i & 255) === 0) s += c
				else if (c.length !== 1) s += "?"
			}
			return s
		}`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	jitDefaultsForTest(r)
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, src)); err != nil {
			t.Fatal(err)
		}
	}
	run := func(call string) {
		t.Helper()
		wv, err := want.Run(compileForTest(t, call))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, call))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("%s: got %q, interpreter %q", call, got, want)
		}
	}
	run(`glyphs(65, 5000)`)
	run(`glyphs(0xe000, 5000)`)
	hosts, interpreted := r.jit.hosts, r.jit.interpreted
	run(`glyphs(0xe000, 5000)`)
	run(`glyphs(0x3040, 5000)`)
	run(`glyphs(65, 5000)`)
	if h, i := r.jit.hosts-hosts, r.jit.interpreted-interpreted; h != 0 || i != 0 {
		t.Fatalf("codes past 255: %d exits to Go, %d finished in the interpreter", h, i)
	}
	if r.jit.ssaEntries == 0 {
		t.Fatal("glyphs never ran natively")
	}
	run(`glyphs(65536 + 66, 300)`)
	run(`glyphs(-3.5, 300)`)
	run(`glyphs(0.5, 300)`)
	run(`glyphs(NaN, 300)`)
	run(`glyphs("7", 300)`)
}
