//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"testing"
)

// argSelectSetup's callees, inlined in the loops that call them, read
// their arguments by a key that is no constant: sum and back by a loop's
// index, through `var a = arguments` too, last by the count less one;
// pick by its own argument, an index past the count, a negative one, a
// fraction and a string among them.
const argSelectSetup = `
	function sum() { let s = 0; for (let i = 0; i < arguments.length; i++) s += arguments[i]; return s }
	function back() { var a = arguments; var r = 0; for (var i = a.length - 1; i >= 0; i--) r = r * 10 + a[i]; return r }
	function last() { return arguments[arguments.length - 1] }
	function pick(k) { return arguments[k] }
	function run(n) { let s = 0; for (let i = 0; i < n; i++) s += sum(i, 1, 2) + back(1, i & 7, 3) + last(i, 5) + sum() + back(i); return s }
	function picks(n) { let s = ''; for (let i = 0; i < n; i++) s += pick(i % 4, 'b', 'c') + ':'; return s }
	function odd() { return [pick(1.5, 'x'), pick(-1, 'x'), pick('1', 'x'), pick(NaN), pick(-0, 'x'), pick(5, 'x')].join() }`

// arguments[k] of an inlined call, k no constant, is the call's argument
// of that index, picked natively among them (ssa's argumentSelect): k
// checked to be a number, an integer below the count, as V8 reads an
// inlined frame's arguments, the elements of an object escape analysis
// replaced by the call's values. It had deoptimized at its first read, and
// was Go's at an exit from then on, each call inlined finishing in the
// interpreter: EarleyBoyer's sc_list, `var a = arguments; ... a[i]`. Every
// answer is the interpreter's, a key past the count, a negative one, a
// fraction, a string and NaN among them, and the loop calling the callees
// inlines them all and leaves native code at none of their reads, nor
// calls Go.
func TestJITInlinedArgumentsByKey(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	inlined := jitRegInlinedForTest(t)
	rounds := []string{}
	for range 6 {
		rounds = append(rounds, `run(200)`, `picks(40)`, `odd()`)
	}
	r := jitAgreesForTest(t, argSelectSetup, rounds...)
	defer holdCollectorNow(t)()
	m := jitNativeForTest(t, r, "run")
	for _, callee := range []string{"sum", "back", "last"} {
		if !jitRegInlinesForTest(t, r, inlined, "run", callee) {
			t.Errorf("run does not inline %s", callee)
		}
	}
	hosts, guards, goCalls := m.ssaStats.hosts, m.ssaStats.guards, m.ssaStats.goCalls
	interp := r.jit.interpreted
	if v, err := r.Run(compileForTest(t, `''+run(200)`)); err != nil || v.String().Go() != "108600" {
		t.Fatalf("run(200) = %s, %v; want 108600", v.String().Go(), err)
	}
	if d := m.ssaStats.hosts - hosts; d != 0 {
		t.Errorf("run left for Go %d times in a round, want none", d)
	}
	if d := m.ssaStats.guards - guards; d != 0 {
		t.Errorf("run deoptimized %d times in a round, want none", d)
	}
	if d := m.ssaStats.goCalls - goCalls; d != 0 {
		t.Errorf("run made %d calls of Go in a round, want none", d)
	}
	if d := r.jit.interpreted - interp; d != 0 {
		t.Errorf("the interpreter finished %d runs in a round, want none", d)
	}
	// Node's answers.
	for src, want := range map[string]string{`picks(8)`: "0:b:c:undefined:0:b:c:undefined:", `odd()`: ",,x,,0,"} {
		if v, err := r.Run(compileForTest(t, src)); err != nil || v.String().Go() != want {
			t.Errorf("%s = %s, %v; want %s", src, v.String().Go(), err, want)
		}
	}
}
