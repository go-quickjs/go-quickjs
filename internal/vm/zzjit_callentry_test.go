//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime/debug"
	"testing"
	"weak"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
)

// A callee's locals past its parameters are undefined when a native call
// enters it, though its code's start, a loop's target, is no place to make
// them so (compile.StartOnly): its call entry makes them undefined (mir's
// callEntry), and Go, which runs a callee with no call entry from its
// start (abi.ExitEnter), does (jitStartLocals) -- no call writes them
// now. Each callee here reads such a local before it assigns it, in a
// frame where the call before wrote its third argument, the same slot,
// past the VM's stack's top: left as it was, it reads 777.
func TestJITCallEntryClearsLocals(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	// Native calls are Go's while the collector marks.
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	const setup = `
		function g(a, b, c) { return a + b + c }
		function f(a, b) { for (;;) { if (b) return y; var y = a; b = 1 } }
		function run(n) {
			let s = 0
			for (let i = 0; i < n; i++) {
				s = (s + g(i, 0, 777)) | 0
				const v = f(i, 1)
				if (v !== undefined) return "stale " + v
			}
			return s
		}
		function g16(a,b,c,d,e,f,g,h,i,j,k,l,m,n,o,p,q) { return a + q }
		function f16(a,b,c,d,e,f,g,h,i,j,k,l,m,n,o,p) { for (;;) { if (b) return y; var y = a; b = 1 } }
		function run16(n) {
			let s = 0
			for (let i = 0; i < n; i++) {
				s = (s + g16(i,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,777)) | 0
				const v = f16(i,1,0,0,0,0,0,0,0,0,0,0,0,0,0,0)
				if (v !== undefined) return "stale " + v
			}
			return s
		}`
	want := New(Config{})
	t.Cleanup(func() { want.Close(); want.ReleaseClosed() })
	if _, err := want.Run(compileForTest(t, setup)); err != nil {
		t.Fatal(err)
	}
	r := jitRuntimeForTest(t, Config{JIT: true})
	if _, err := r.Run(compileForTest(t, setup+`; run(300); run(300); run16(300); run16(300)`)); err != nil {
		t.Fatal(err)
	}
	fnOf := func(name string) *bytecode.Function {
		return r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure.fn
	}
	for _, tc := range []struct {
		run, callee string
		// enter has the callee have no code native callers may call, which
		// Go runs from its start (abi.ExitEnter): its operands are more than
		// a call of Go takes (abi.MaxGoArgs).
		enter bool
	}{
		{"String(run(300))", "f", false},
		{"String(run16(300))", "f16", false},
		{"String(run16(300))", "f16", true},
	} {
		fn := fnOf(tc.callee)
		e := r.jit.cache[weak.Make(fn)]
		if e == nil || e.ssa == nil || e.link.in == 0 {
			t.Fatalf("%s is not called natively: %+v", tc.callee, e)
		}
		if e.ssa.CallEntry() == 0 || jitcompile.StartOnly(fn) {
			t.Fatalf("%s: call entry %#x, start only %v", tc.callee, e.ssa.CallEntry(), jitcompile.StartOnly(fn))
		}
		if tc.enter {
			e.notNative, e.link.entry, e.link.body, e.nativeRetry = true, 0, 0, 0
		}
		in, entered := e.link.in, r.jit.entered
		wv, err := want.Run(compileForTest(t, tc.run))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, tc.run))
		if err != nil {
			t.Fatal(err)
		}
		if gv.String().Go() != wv.String().Go() {
			t.Fatalf("%s = %s; interpreter %s", tc.run, gv.String().Go(), wv.String().Go())
		}
		switch {
		case tc.enter && r.jit.entered == entered:
			t.Fatalf("%s: Go never ran %s from its start", tc.run, tc.callee)
		case !tc.enter && e.link.in-in < 300:
			t.Fatalf("%s: %s was called natively %d times of 300", tc.run, tc.callee, e.link.in-in)
		}
	}
}

// A native call's state is described statically (abi.CallState): Go
// expands it into the records it makes the caller's frame from when the
// callee leaves (jit.ExpandCall), each value from where the caller's code
// keeps it -- a spill slot, a constant, a slot that held it on entry -- or
// from where the call wrote it: a register dead past the call, a pointer
// word read from a cell the callee may change. Here the callee throws, so
// the caller goes on from its frame, not natively after the call, and
// every kind of value is live across it: numbers, doubles, booleans,
// strings, an object from a parameter, one read from a property the callee
// replaces, a constant, an object made in the loop.
func TestJITCallStateRebuildsTheFrame(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	const setup = `
		var holder = {p: {v: 1}, tag: "h"}, other = {tag: "m"}
		function thrower(x, h) {
			let w = 0
			for (let j = 0; j < 3; j++) w = (w + j) | 0
			h.p = {v: x + w - 3}
			if (x % 50 === 49) throw new Error("e" + x)
			return x
		}
		function caller(o, n, m) {
			let out = []
			for (let i = 0; i < n; i++) {
				const a = o.p, b = i * 0.5 + 0.25, c = i > 10, d = o, e = "s" + (i & 3), f = {q: i}, k = 7, g = i & 1 ? m : o
				let r
				try { r = thrower(i, o) } catch (err) { r = err.message }
				out.push([a.v, b, c, d === o, d.tag, e, f.q, k, r, o.p.v, g.tag].join())
			}
			return out.join(";")
		}`
	want := New(Config{})
	t.Cleanup(func() { want.Close(); want.ReleaseClosed() })
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup+`; caller(holder, 200, other); caller(holder, 200, other)`)); err != nil {
			t.Fatal(err)
		}
	}
	fnOf := func(name string) *bytecode.Function {
		return r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure.fn
	}
	ce, te := r.jit.cache[weak.Make(fnOf("caller"))], r.jit.cache[weak.Make(fnOf("thrower"))]
	if ce == nil || ce.ssa == nil || te == nil || te.ssa == nil {
		t.Fatalf("not compiled: caller %+v, thrower %+v", ce, te)
	}
	in, records := te.link.in, r.jit.ssaRecords
	for range 3 {
		wv, err := want.Run(compileForTest(t, `caller(holder, 400, other)`))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, `caller(holder, 400, other)`))
		if err != nil {
			t.Fatal(err)
		}
		if gv.String().Go() != wv.String().Go() {
			t.Fatalf("caller = %.300s...; interpreter %.300s...", gv.String().Go(), wv.String().Go())
		}
	}
	if te.link.in == in || r.jit.ssaRecords == records {
		t.Fatalf("thrower called natively %d times, %d records applied", te.link.in-in, r.jit.ssaRecords-records)
	}
}
