//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "testing"

// substringSetup's counts reads one character at a time with substring,
// slice and substr, as AWFY Json's read does, and compares it, past the
// string's end too; inRange only inside it; mixed
// takes what Go makes: longer results, substring's arguments swapped,
// negative indices, which slice and substr count from the end, indices
// past the end, code units past 255, of a string of two-byte units, and
// a string made by concatenation.
const substringSetup = `
	var text = 'hello, world: the quick brown fox jumps over the lazy dog'
	var wide = 'héllo 世界, wörld'
	function counts(s, n) {
		let c = 0
		for (let i = 0; i < n; i++) {
			const k = i % (s.length + 3)
			if (s.substring(k, k + 1) === 'o') c += 1
			if (s.slice(k, k + 1) === 'e') c += 10
			if (s.substr(k, 1) === ' ') c += 100
		}
		return c
	}
	function inRange(s, n) {
		let c = 0
		for (let i = 0; i < n; i++) {
			const k = i % s.length
			if (s.substring(k, k + 1) === 'o') c += 1
			if (s.slice(k, k + 1) === 'e') c += 10
			if (s.substr(k, 1) === ' ') c += 100
		}
		return c
	}
	function mixed(s, n) {
		let out = ''
		for (let i = 0; i < n; i++) {
			const k = i % (s.length + 3)
			out += s.substring(k, k + 1) + s.substring(k, k + 3) + s.substring(k + 1, k) + '|'
			out += s.slice(k, k + 1) + s.slice(-2, -1) + s.slice(k - 3, k - 2) + '|'
			out += s.substr(k, 1) + s.substr(-3, 1) + s.substr(k, 2) + '/'
		}
		return out
	}
`

// A one-character substring, slice or substr of a flat string is the
// runtime's string of its code unit, made natively (ssa.OpSubstringChar),
// as V8 makes it from its table of them; any other is Go's, called from
// native code on a branch of its own. Each answer is the interpreter's,
// and inRange's calls make no call of Go: Json's read had made 2.3
// million a run.
func TestJITSubstringChars(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	rounds := []string{}
	for range 6 {
		rounds = append(rounds, `counts(text, 300)`, `counts(wide, 300)`, `inRange(text, 300)`, `mixed(text, 80)`, `mixed(wide, 80)`, `mixed(text + wide, 40)`)
	}
	r := jitAgreesForTest(t, substringSetup, rounds...)
	var e *jitEntry
	for k, x := range r.jit.cache {
		if fn := k.Value(); fn != nil && fn.Name == "inRange" {
			e = x
		}
	}
	if e == nil || e.ssa == nil {
		t.Fatal("inRange has no code")
	}
	// Two more rounds, compiled: 1,800 calls, none of them Go's.
	before := e.ssaStats.goCalls
	for range 2 {
		if _, err := r.Run(compileForTest(t, `inRange(text, 300)`)); err != nil {
			t.Fatal(err)
		}
	}
	if n := e.ssaStats.goCalls - before; n != 0 {
		t.Errorf("inRange called Go %d times in 1,800 one-character calls", n)
	}
	// String.prototype.substring replaced: the call checks its function.
	jitAgreesForTest(t, substringSetup, append(rounds[:12:12],
		`(String.prototype.substring = function (a, b) { return 'o' }, counts(text, 300))`, `mixed(text, 80)`)...)
}
