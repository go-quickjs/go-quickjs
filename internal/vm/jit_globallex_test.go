//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"testing"
)

// globalLexSetup has read call h, which reads the global gv, rs, which
// reads sh, a configurable property of the global object, and rc, which
// reads C, another, and count the calls in gw, another, each compiled
// while no script-level lexical binding exists.
const globalLexSetup = `
	var gv = 3
	globalThis.sh = 10
	globalThis.C = { k: 0 }
	globalThis.gw = 0
	function h(x) { return (x + gv) | 0 }
	function rs() { return sh }
	function rc() { return C.k }
	function read(n) { let s = 0; for (let i = 0; i < n; i++) s = (h(s) + rs() + rc()) | 0; gw = gw + 1; return s }
	for (let k = 0; k < 60; k++) read(10)
`

// globalLexCounts is what a run of the JIT did: its exits to Go, its
// guards that failed, and the programs it compiled.
type globalLexCounts struct{ hosts, guards, compiled uint64 }

// globalLexCountsOf is r's counts so far.
func globalLexCountsOf(r *Runtime) globalLexCounts {
	st := r.JITStats()
	return globalLexCounts{st.Hosts, st.Guards, st.Compiled}
}

// since is c's counts past before's.
func (c globalLexCounts) since(before globalLexCounts) globalLexCounts {
	return globalLexCounts{c.hosts - before.hosts, c.guards - before.guards, c.compiled - before.compiled}
}

// globalLexRuntime is a runtime with the JIT, compiling in the background
// if background says, each compile installed once the worker is done with
// it, that has run setup and compiled the function compiled.
func globalLexRuntime(t *testing.T, setup, compiled string, background bool) *Runtime {
	t.Helper()
	if background {
		jitTestSwitch(t, func(jc *jitTestConfig) { jc.bgDone = true })
	}
	r := jitRuntimeForTest(t, Config{JIT: true})
	r.jitCallThreshold, r.jitICThreshold = 30, 30
	r.SetJITBackground(background)
	if _, err := r.Run(compileForTest(t, setup)); err != nil {
		t.Fatal(err)
	}
	if e := jitRegEntryForTest(r, compiled); e == nil || e.code == nil {
		t.Fatalf("%s is not compiled", compiled)
	}
	return r
}

// globalLexRun runs globalLexSetup in a runtime with the JIT, then later,
// a script that declares a script-level lexical binding, then calls read
// 200 times from a function, and returns what the calls answered, with gw
// and globalThis.gw then, and what the JIT did over the calls.
func globalLexRun(t *testing.T, later string, background bool) (string, globalLexCounts) {
	t.Helper()
	r := globalLexRuntime(t, globalLexSetup, "read", background)
	before, err := r.Run(compileForTest(t, `'' + read(1)`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(compileForTest(t, later)); err != nil {
		t.Fatal(err)
	}
	c := globalLexCountsOf(r)
	v, err := r.Run(compileForTest(t, `(() => { let t = 0; for (let k = 0; k < 200; k++) t = (t + read(10)) | 0; return '' + t + ' ' + read(1) })()`))
	if err != nil {
		t.Fatal(err)
	}
	c = globalLexCountsOf(r).since(c)
	w, err := r.Run(compileForTest(t, `gw + ' ' + globalThis.gw`))
	if err != nil {
		t.Fatal(err)
	}
	return before.String().Go() + " " + v.String().Go() + " " + w.String().Go(), c
}

// A script-level lexical binding a later script declares is no reason for
// code compiled before it to leave native code at its reads of the global
// object's bindings, which it does not shadow: as V8 invalidates, at a
// script's lexical declaration, only the global object's property cell of
// the name it declares (JSGlobalObject::InvalidatePropertyCell), the reads
// of every other name go on natively, in the code they were compiled in.
// One that shadows a binding the code reads or writes is read and written
// where it now is -- the lexical binding, its exact value -- and the code
// is deoptimized once for it and compiled again, as V8's code that
// depends on the cell is, rather than leaving native code at each read.
func TestJITGlobalReadAfterLexicalDeclaration(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	cases := []struct {
		name, later, want string
		// shadows says the declaration shadows a binding the code reads.
		shadows bool
	}{
		// h(s) adds 3, rs() 10: read(10) = 130, read(1) = 13; 200 calls,
		// and read had been called 61 times before them.
		{"let", `let later = 1`, "13 26000 13 262 262", false},
		{"const", `const later = 1`, "13 26000 13 262 262", false},
		{"class", `class Later {}`, "13 26000 13 262 262", false},
		// rs() reads the lexical sh, 20, from then on: read(10) = 230.
		{"shadowing let", `let sh = 20`, "13 46000 23 262 262", true},
		{"shadowing const", `const sh = 20`, "13 46000 23 262 262", true},
		// rc() reads the lexical C's k, 10.
		{"shadowing class", `class C { static k = 10 }`, "13 46000 23 262 262", true},
		// gw counts the calls in the lexical gw from then on, the global
		// object's left at what it was.
		{"shadowing a write", `let gw = 0`, "13 26000 13 201 61", true},
	}
	for _, native := range []bool{true, false} {
		for _, marking := range jitMarkingModes() {
			for _, background := range []bool{false, true} {
				for _, c := range cases {
					t.Run(fmt.Sprintf("native=%v/marking=%v/background=%v/%s", native, marking, background, c.name), func(t *testing.T) {
						nativeForTest(t, native)
						if marking {
							jitMarkingForTest(t)
						}
						got, n := globalLexRun(t, c.later, background)
						if got != c.want {
							t.Errorf("before, after, gw and globalThis.gw: %s, want %s", got, c.want)
						}
						switch {
						case !c.shadows && n != (globalLexCounts{}):
							t.Errorf("over 200 calls: %+v, want no exits, failed guards or compiles", n)
						case c.shadows && (n.guards != 1 || n.hosts > 4 || n.compiled > 1):
							t.Errorf("over 200 calls: %+v, want one failed guard, at most 4 exits and a compile", n)
						}
					})
				}
			}
		}
	}
}

// churnSetup has drive call churn, which reads cv, a configurable property
// of the global object, beside junk, another.
const churnSetup = `
	globalThis.cv = 3
	globalThis.junk = 0
	globalThis.round = 0
	function churn(n) { let s = 0; for (let i = 0; i < n; i++) s = (s + cv) | 0; return s }
	function drive() { let t = 0; for (let k = 0; k < 100; k++) t = (t + churn(10)) | 0; return t }
	for (let k = 0; k < 60; k++) drive()
`

// A change to the global object's bindings but by an append deoptimizes
// code that reads a global the change has not moved once, after which the
// read checks its own binding, which no change of another binding fails;
// a binding that goes on moving deoptimizes the code at its read once,
// after which the read is generic, Go's where it fails, and the code is
// compiled no more for it; and a global that is an accessor is Go's to
// read from the first compile, which deoptimizes at no read of it. None
// leaves native code at every read where native code can read the
// binding.
func TestJITGlobalReadAfterGlobalChanges(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	cases := []struct {
		name string
		// prelude is run before churnSetup, and change before each round.
		prelude, change string
		// most are the exits to Go a round may take once its first has
		// deoptimized the code.
		most uint64
	}{
		{"another binding", ``, `delete globalThis.junk; globalThis.junk = 0`, 0},
		// cv deleted and added again past a new binding, at a new
		// position each round: Go reads it, at most at each of its 1000
		// reads a round.
		{"its binding", ``, `delete globalThis.cv; globalThis['p' + ++globalThis.round] = 0; globalThis.cv = 3`, 1000},
		{"an accessor", `Object.defineProperty(globalThis, 'cv', { get() { return 3 }, configurable: true })`, `globalThis.junk = 0`, 1000},
	}
	const rounds = 24
	for _, native := range []bool{true, false} {
		for _, marking := range jitMarkingModes() {
			for _, background := range []bool{false, true} {
				for _, c := range cases {
					t.Run(fmt.Sprintf("native=%v/marking=%v/background=%v/%s", native, marking, background, c.name), func(t *testing.T) {
						nativeForTest(t, native)
						if marking {
							jitMarkingForTest(t)
						}
						r := globalLexRuntime(t, c.prelude+";"+churnSetup, "drive", background)
						if g := r.JITStats().Guards; g != 0 {
							t.Errorf("%d guards failed before the first change", g)
						}
						for k := range rounds {
							if _, err := r.Run(compileForTest(t, c.change)); err != nil {
								t.Fatal(err)
							}
							before := globalLexCountsOf(r)
							v, err := r.Run(compileForTest(t, `'' + drive()`))
							if err != nil {
								t.Fatal(err)
							}
							n := globalLexCountsOf(r).since(before)
							if got := v.String().Go(); got != "3000" {
								t.Fatalf("round %d: %s, want 3000", k, got)
							}
							if k > 0 && (n.guards != 0 || n.compiled != 0 || n.hosts > c.most) {
								t.Errorf("round %d: %+v, want no failed guards or compiles and at most %d exits", k, n, c.most)
							}
						}
						if r.shapes.globalEpoch >= jitGlobalEpochs {
							t.Fatalf("global epoch %d: the reads settled past jitGlobalEpochs", r.shapes.globalEpoch)
						}
					})
				}
			}
		}
	}
}
