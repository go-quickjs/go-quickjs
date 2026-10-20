//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"slices"
	"testing"
	"weak"
)

// jitFlushForTest has the cache's clocks (jitEpochNow, jitFlushNow) stand
// at what the returned function sets, for the rest of the test.
func jitFlushForTest(t *testing.T) func(uint64) {
	t.Helper()
	now, flushNow := jitEpochNow, jitFlushNow
	epoch := uint64(1000)
	jitEpochNow = func() uint64 { return epoch }
	jitFlushNow = func() uint64 { return epoch }
	t.Cleanup(func() { jitEpochNow, jitFlushNow = now, flushNow })
	return func(e uint64) { epoch = e }
}

// jitNoFlushForTest has the cache flush no code for the rest of the test
// (jitFlushAge), for a test of the room it makes, whose clock runs fast
// enough that code would be flushed before there was none.
func jitNoFlushForTest(t *testing.T) {
	t.Helper()
	was := jitFlushAge
	jitFlushAge = 1 << 62
	t.Cleanup(func() { jitFlushAge = was })
}

// jitGlobalClosure is the closure the global name holds.
func jitGlobalClosure(r *Runtime, name string) *closure {
	return r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure
}

// Code that has gone unused is flushed, whatever room the budget has, and
// what it was compiled for with it: f's code, which called g natively,
// had kept g -- the callee its call was seen to call -- and the array g
// read alive after the program let both go, as long as f's bytecode was,
// which f, a global function, keeps. Octane's zlib, once run, had kept its
// 128 MB heap so. Compiled again once hot again, f's answers are the same.
func TestJITFlushesUnusedCode(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setEpoch := jitFlushForTest(t)
	r := jitRuntimeForTest(t, Config{JIT: true})
	if _, err := r.Run(compileForTest(t, `
		var probe;
		function make(k) {
			var bytes = new Uint8Array(1 << 20);
			for (var i = 0; i < 1024; i++) bytes[i] = (i * k) & 255;
			probe = bytes;
			return function g(i) { return bytes[i & 1023] + i };
		}
		function f(g, n) { var s = 0; for (var i = 0; i < n; i++) s = (s + g(i)) | 0; return s }
		var g = make(7);`)); err != nil {
		t.Fatal(err)
	}
	const want = 16816 // Node: f(make(7), 100)
	for i := 0; i < 300; i++ {
		v, err := r.Run(compileForTest(t, `f(g, 100)`))
		if err != nil || v.Number() != want {
			t.Fatalf("round %d: %v, %v", i, v, err)
		}
	}
	fl := jitGlobalClosure(r, "f")
	s := r.jit
	if e := s.cache[weak.Make(fl.fn)]; e == nil || e.ssa == nil || len(e.nativeCalls) == 0 {
		t.Fatalf("f's code does not call g natively: %+v", e)
	}
	probe := weak.Make(r.global.getOwn(r.atoms.intern("probe")).value().Object())
	if _, err := r.Run(compileForTest(t, `probe = null; g = null;`)); err != nil {
		t.Fatal(err)
	}
	// Two looks, jitFlushAge epochs apart, f unused between them.
	setEpoch(1000 + jitFlushAge)
	s.flush()
	setEpoch(1000 + 2*jitFlushAge)
	s.flush()
	if e := s.cache[weak.Make(fl.fn)]; e != nil {
		t.Fatalf("f's unused code was not flushed: %+v", e)
	}
	if err := jitLinksHeld(s); err != nil {
		t.Fatal(err)
	}
	if err := jitEntriesFound(s); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		runtime.GC()
	}
	if probe.Value() != nil {
		t.Fatal("the array g read is alive, kept by f's flushed code")
	}
	for i := 0; i < 300; i++ {
		v, err := r.Run(compileForTest(t, `f(g || (g = make(7)), 100)`))
		if err != nil || v.Number() != want {
			t.Fatalf("after the flush, round %d: %v, %v", i, v, err)
		}
	}
	if e := s.cache[weak.Make(fl.fn)]; e == nil || e.ssa == nil {
		t.Fatalf("f was not compiled again once hot again: %+v", e)
	}
}

// Code in use is never flushed: a function that ran between the cache's
// looks; nor an entry with no code -- a function the JIT refuses, w, whose
// uses are not counted, and which runs all the same.
func TestJITFlushKeepsCodeInUse(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setEpoch := jitFlushForTest(t)
	r := jitRuntimeForTest(t, Config{JIT: true})
	if _, err := r.Run(compileForTest(t, `
		function f(n) { var s = 0; for (var i = 0; i < n; i++) s = (s + (i * 7) % 13) | 0; return s }
		function w(n) { var o = { x: 2 }, s = 0; for (var i = 0; i < n; i++) { with (o) { s = (s + x) | 0 } } return s }`)); err != nil {
		t.Fatal(err)
	}
	run := func() {
		for i := 0; i < 50; i++ {
			if v, err := r.Run(compileForTest(t, `f(1000) + w(100)`)); err != nil || v.Number() != 6200 {
				t.Fatalf("f(1000) + w(100): %v, %v", v, err)
			}
		}
	}
	run()
	fl := jitGlobalClosure(r, "f")
	s := r.jit
	e := s.cache[weak.Make(fl.fn)]
	if e == nil || e.ssa == nil {
		t.Fatalf("f was not compiled: %+v", e)
	}
	ew := s.cache[weak.Make(jitGlobalClosure(r, "w").fn)]
	if ew == nil || ew.holdsCode() {
		t.Fatalf("w is not an entry with no code: %+v", ew)
	}
	for k := uint64(1); k <= 4; k++ {
		setEpoch(1000 + k*jitFlushAge)
		s.flush()
		run()
	}
	if s.cache[weak.Make(fl.fn)] != e {
		t.Fatal("f's code, run between the looks, was flushed")
	}
	if s.cache[weak.Make(jitGlobalClosure(r, "w").fn)] != ew {
		t.Fatal("w's entry, which has no code and ran between the looks, was flushed")
	}
}

// An entry the cache drops is no longer among its callees' native callers:
// one kept there, its entry gone, kept what it had learned and compiled
// for alive with it, through a callee whose own code stayed in use.
func TestJITDroppedCallerLeavesCallees(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setEpoch := jitFlushForTest(t)
	r := jitRuntimeForTest(t, Config{JIT: true})
	if _, err := r.Run(compileForTest(t, `
		function h(i) { var t = 0; for (var j = 0; j < 4; j++) t = (t + (i * j) % 7) | 0; return t }
		function a(n) { var s = 0; for (var i = 0; i < n; i++) s = (s + h(i)) | 0; return s }
		function b(n) { var s = 0; for (var i = 0; i < n; i++) s = (s + h(i + 1)) | 0; return s }`)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 300; i++ {
		if _, err := r.Run(compileForTest(t, `a(100) + b(100)`)); err != nil {
			t.Fatal(err)
		}
	}
	s := r.jit
	ea := s.cache[weak.Make(jitGlobalClosure(r, "a").fn)]
	eh := s.cache[weak.Make(jitGlobalClosure(r, "h").fn)]
	if ea == nil || ea.ssa == nil || eh == nil || !slices.Contains(eh.nativeCallers, ea) {
		t.Fatalf("a's code does not call h's natively: %+v, %+v", ea, eh)
	}
	for k := uint64(1); k <= 2; k++ {
		setEpoch(1000 + k*jitFlushAge)
		s.flush()
		// b, and h through it, stay in use; a does not.
		for i := 0; i < 20; i++ {
			if _, err := r.Run(compileForTest(t, `b(100)`)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if s.cache[weak.Make(jitGlobalClosure(r, "a").fn)] != nil {
		t.Fatal("a's unused code was not flushed")
	}
	for _, e := range s.cache {
		if slices.Contains(e.nativeCallers, ea) {
			t.Fatal("a callee keeps a's dropped entry among its native callers")
		}
	}
}

// The frames made for a run's native levels at its calls of Go
// (jitLevels), which only that run's calls share, keep nothing once the
// run has ended: their closures and receivers had kept a benchmark's graph
// alive after it ended, through the closure of the callee that called Go.
func TestJITLevelsClearedAtRunEnd(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitRuntimeForTest(t, Config{JIT: true})
	if _, err := r.Run(compileForTest(t, `
		function make() {
			var bytes = new Uint8Array(1 << 10);
			return function g(i) { return bytes[i & 1023] + JSON.stringify(i).length };
		}
		function f(g, n) { var s = 0; for (var i = 0; i < n; i++) s = (s + g(i)) | 0; return s }
		var g = make();`)); err != nil {
		t.Fatal(err)
	}
	made := false
	for i := 0; i < 300; i++ {
		v, err := r.Run(compileForTest(t, `f(g, 100)`))
		if err != nil || v.Number() != 190 {
			t.Fatalf("round %d: %v, %v", i, v, err)
		}
		made = made || cap(r.jit.levels.at) != 0
	}
	if !made {
		t.Fatal("no run made frames for its native levels")
	}
	for i, at := range r.jit.levels.at[:cap(r.jit.levels.at)] {
		if at.cl != nil || at.f != nil || at.e != nil || at.this != (jitLevelFrame{}).this {
			t.Fatalf("level %d keeps what its run had once the run ended: %+v", i, at)
		}
	}
}

// A callee whose code runs only inlined in its caller's, or called
// natively from it, is used while the caller is, though its own uses do
// not count those runs: it is not flushed, nor what it learned, which the
// caller's code compiled again learns from. g is recorded as inlined in
// f's code here, as a compile of f that inlines it records it.
func TestJITFlushKeepsInlinedCallees(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setEpoch := jitFlushForTest(t)
	r := jitRuntimeForTest(t, Config{JIT: true})
	if _, err := r.Run(compileForTest(t, `
		function f(n) { var s = 0; for (var i = 0; i < n; i++) s = (s + (i * 7) % 13) | 0; return s }
		function g(n) { var s = 0; for (var i = 0; i < n; i++) s = (s + (i * 5) % 11) | 0; return s }
		for (var k = 0; k < 50; k++) f(1000) + g(1000);`)); err != nil {
		t.Fatal(err)
	}
	s := r.jit
	ef := s.cache[weak.Make(jitGlobalClosure(r, "f").fn)]
	eg := s.cache[weak.Make(jitGlobalClosure(r, "g").fn)]
	if ef == nil || ef.ssa == nil || eg == nil || eg.ssa == nil {
		t.Fatalf("f or g has no code: %+v, %+v", ef, eg)
	}
	ef.ssaInlined = append(ef.ssaInlined, jitGlobalClosure(r, "g"))
	for k := uint64(1); k <= 4; k++ {
		setEpoch(1000 + k*jitFlushAge)
		s.flush()
		for i := 0; i < 20; i++ {
			if v, err := r.Run(compileForTest(t, `f(1000)`)); err != nil || v.Number() != 6000 {
				t.Fatalf("f(1000): %v, %v", v, err)
			}
		}
	}
	if s.cache[weak.Make(jitGlobalClosure(r, "f").fn)] != ef {
		t.Fatal("f's code, run between the looks, was flushed")
	}
	if s.cache[weak.Make(jitGlobalClosure(r, "g").fn)] != eg {
		t.Fatal("g, inlined in f's code, which ran, was flushed")
	}
}

// The flush's clock is time, not the collector's cycles, which the cache's
// epochs count too: a program that collects all the while has those go by
// within a call. TestJITBigInt64, its collector running in a loop, had
// found squares, which all calls once a call, flushed between its calls.
func TestJITFlushByTimeNotCollections(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setEpoch := jitFlushForTest(t)
	r := jitRuntimeForTest(t, Config{JIT: true})
	if _, err := r.Run(compileForTest(t, `
		function f(n) { var s = 0; for (var i = 0; i < n; i++) s = (s + (i * 7) % 13) | 0; return s }
		for (var k = 0; k < 50; k++) f(1000);`)); err != nil {
		t.Fatal(err)
	}
	s := r.jit
	e := s.cache[weak.Make(jitGlobalClosure(r, "f").fn)]
	if e == nil || e.ssa == nil {
		t.Fatalf("f has no code: %+v", e)
	}
	// The epochs race, as the collector's cycles would; the flush's clock
	// stands.
	jitFlushNow = func() uint64 { return 1000 }
	for k := uint64(1); k <= 4; k++ {
		setEpoch(1000 + k*100*jitFlushAge)
		s.flush()
	}
	if s.cache[weak.Make(jitGlobalClosure(r, "f").fn)] != e {
		t.Fatal("f's code was flushed by the collector's cycles alone")
	}
}
