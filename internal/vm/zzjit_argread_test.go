//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"testing"
)

const argumentReadsSetup = `
	function three() { return arguments[0] + arguments[1] + arguments[2] }
	function strictTwo(a, b) { 'use strict'; a = 100; return arguments[0] * 10 + arguments[1] }
	function mapped(a) { a = 7; return arguments[0] }
	function short() { return arguments[2] }
	function keyed(k) { 'use strict'; return arguments[k] }
	function sum(n) {
		let s = 0
		for (let i = 0; i < n; i++) s = (s + three(i, 1, 2) + strictTwo(i, 3)) | 0
		return s
	}
	function odd(n) {
		let s = ''
		for (let i = 0; i < n; i++) s = mapped(i) + ':' + short(i, i) + ':' + keyed(i % 2, 'x', 'y') + ':' + three('a', i)
		return s
	}
`

// A call of a function that reads its arguments by index, and does nothing
// else with its arguments object (bytecode's ForwardsArguments, no
// apply_arguments), is inlined, each arguments[k], a constant k below the
// call's count, the call's argument k, as V8 eliminates the arguments
// object. Each answer is the interpreter's: a strict function's
// arguments are not its reassigned parameters; a sloppy one's with
// parameters, which they alias, is not inlined; an index past the
// arguments, a key not a constant, leave for Go; and the loop of the first
// makes no call of Go at all.
func TestJITInlinedArgumentReads(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	var rounds []string
	for range 4 {
		rounds = append(rounds, `sum(300)`, `odd(300)`)
	}
	r := jitAgreesForTest(t, argumentReadsSetup, rounds...)
	calls, hosts, entries := r.jit.goCalls, r.jit.hosts, r.jit.ssaEntries
	if _, err := r.Run(compileForTest(t, `sum(3000)`)); err != nil {
		t.Fatal(err)
	}
	if r.jit.ssaEntries == entries {
		t.Fatal("sum ran no native code")
	}
	if n, h := r.jit.goCalls-calls, r.jit.hosts-hosts; n != 0 || h > 2 {
		t.Errorf("sum's 6,000 calls: %d made by Go, %d exits", n, h)
	}
}
