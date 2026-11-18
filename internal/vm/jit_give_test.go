//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
)

// regGiveSrc calls RegExp literals' exec and test in loops, a leaf's call
// from native code each: regexp_lit's of a local (lit), a call_method_w
// after a regexp_lit that does not call, of an element (elem), a global
// pattern's, whose lastIndex each fresh literal begins at 0 (glob), one
// whose RegExp the loop keeps (kept), which no pool may have back, and one
// whose test a getter reads once RegExp.prototype has it (seen).
const regGiveSrc = `
function lit(n, s) {
	var r, k = 0;
	for (var i = 0; i < n; i++) { r = /b(c+)/.exec(s); if (r) k += r.index + r[1].length }
	return k + ":" + r[0] + ":" + r.input;
}
var words = ["abc", "ab1", "xyz", "q"];
function elem(n) {
	var k = 0;
	for (var i = 0; i < n; i++) if (/^[a-z]+$/.test(words[i & 3])) k++;
	return k;
}
function glob(n, s) {
	var k = 0;
	for (var i = 0; i < n; i++) { var r = /b/g.exec(s); k += r.index }
	return k;
}
function kept(n, s) {
	var a = [];
	for (var i = 0; i < n; i++) { var re = /b/; re.test(s); a.push(re) }
	return new Set(a).size;
}
var seen = [];
function look(n, s) {
	var k = 0;
	for (var i = 0; i < n; i++) if (/c/.test(s)) k++;
	return k;
}
function getter() {
	var t = RegExp.prototype.test;
	Object.defineProperty(RegExp.prototype, "test", { get() { seen.push(this); return t }, configurable: true });
}
function distinct() { return seen.length + ":" + new Set(seen).size }
`

// TestJITRegExpLiteralGivesBack runs regGiveSrc's loops natively, with the
// collector idle and marking, and checks their results, exact and the
// interpreter's, and that the leaves' calls of exec and test on a
// literal's RegExp gave it back to the literal's pool, which was never
// filled again: the pool keeps its first size (abi.PoolSize), which each
// refill doubles. Before, the leaf's call kept nothing of the RegExp but
// gave it back to nothing, and the pool, run out every PoolSize turns, was
// filled with new RegExps -- a RegExp made at each call, 1.3 GB of the
// micro-benchmarks regexp_ascii and regexp_utf16.
func TestJITRegExpLiteralGivesBack(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	regBothMarking(t, func(t *testing.T) {
		rts := regNativeRuntimes(t, regGiveSrc)
		rts[0].SetJITWarmUp(1, 1)
		call := func(name, want string, args ...Value) {
			t.Helper()
			var got [2]string
			for i, r := range rts {
				fn, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
				v, err := r.Call(fn, Undefined, args)
				got[i] = regNativeShow(r, v, err)
			}
			if got[0] != want || got[1] != want {
				t.Fatalf("%s: with the JIT %q, without %q, want %q", name, got[0], got[1], want)
			}
		}
		s := Str(NewString("abccd"))
		for range 4 {
			call("lit", "12000:bcc:abccd", Float(4000), s)
			call("elem", "3000", Float(4000))
			call("glob", "4000", Float(4000), s)
			call("kept", "300", Float(300), s)
			call("look", "4000", Float(4000), s)
		}
		r := rts[0]
		for _, name := range []string{"lit", "elem", "glob", "look"} {
			e := regCodeNamed(t, r, name)
			if e == nil || e.code == nil || e.life.ssaStats.entries == 0 {
				t.Fatalf("%s did not run natively: %+v", name, e)
			}
			pools := 0
			for _, l := range e.life.literals {
				if l.lit == nil {
					continue
				}
				pools++
				if l.pool.Size > abi.PoolSize {
					t.Errorf("%s: the literal /%s/'s pool was filled again, to %d", name, l.lit.Str, l.pool.Size)
				}
			}
			if pools == 0 {
				t.Errorf("%s: no RegExp literal's pool", name)
			}
		}
		// Reading test through a getter, which sees each RegExp: none
		// given back once the prototype is not the realm's.
		call("getter", "undefined")
		call("look", "500", Float(500), s)
		call("distinct", "500:500")
	})
}
