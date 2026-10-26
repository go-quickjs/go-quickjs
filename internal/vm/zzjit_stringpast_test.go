//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"testing"
)

// A string's element read at an index past its end is undefined natively,
// where Go says nothing up String.prototype's chain has that index
// (ssa.GoStringAbsent), as an array's read past its end is: a tokenizer's
// read one past its text's end -- PDF.js's Type1 header's -- had left native
// code at each, the interpreter finishing the rest of each invocation. An
// index String.prototype or Object.prototype has is read there, by Go. Each
// answer is the interpreter's.
func TestJITStringReadPastEnd(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	src := `var text = "ab cd efg h"
		function count(n) {
			let c = 0
			for (let k = 0; k < n; k++) {
				let i = 0
				while (text[i] !== undefined) { c = (c + text.charCodeAt(i)) | 0; i++ }
				if (text[text.length + 3] === undefined) c++
			}
			return c
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
			t.Fatalf("%s: got %s, interpreter %s", call, got, want)
		}
	}
	// Compiled, and its reads past the end learned.
	run(`String(count(3000))`)
	run(`String(count(3000))`)
	hosts, interpreted := r.jit.hosts, r.jit.interpreted
	run(`String(count(3000))`)
	if h, i := r.jit.hosts-hosts, r.jit.interpreted-interpreted; h != 0 || i != 0 {
		t.Fatalf("reads past the end: %d exits to Go, %d finished in the interpreter", h, i)
	}
	if r.jit.ssaEntries == 0 {
		t.Fatal("count never ran natively")
	}
	// An index on the prototypes is no longer absent: read there.
	run(`String.prototype[13] = 1; String(count(50))`)
	run(`delete String.prototype[13]; Object.prototype[11] = 2; String(count(50))`)
	run(`delete Object.prototype[11]; String(count(50))`)
}
