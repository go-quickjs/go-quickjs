//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
)

// regexpLitSetup's loops call a RegExp literal's exec and test, as the
// script semantics test of the tree tier's give-back has them
// (TestRegExpLiteralReuse): what native code must still tell apart.
const regexpLitSetup = `
function f1(s) { var r = []; for (var i = 0; i < 4; i++) r.push(/a/g.exec(s).index + "/" + /a/y.test(s)); return r.join() }
function f2() { var a = []; for (var i = 0; i < 3; i++) a.push(/x/); return [a[0] !== a[1], a[1] !== a[2], a[0].lastIndex].join() }
function f3() { var seen = [], orig = RegExp.prototype.exec;
  RegExp.prototype.exec = function (s) { seen.push(this); return orig.call(this, s) };
  for (var i = 0; i < 3; i++) { /q/.test("q"); /q/.exec("q") }
  RegExp.prototype.exec = orig;
  return [seen.length, new Set(seen).size, seen.every(function (r) { return r.lastIndex === 0 && r.source === "q" })].join() }
function f4() { var leaked = [], orig = RegExp.prototype.exec;
  var leaker = function (s) { leaked.push(this); RegExp.prototype.exec = orig; return orig.call(this, s) };
  var r = [];
  for (var i = 0; i < 3; i++) r.push(/z/g.test((RegExp.prototype.exec = leaker, "zz")));
  var after = []; for (var i = 0; i < 3; i++) { var m = /z/g; after.push(m); m.test("z") }
  return [r.join(""), leaked.length, new Set(leaked).size, leaked.some(function (x) { return after.indexOf(x) >= 0 }), leaked.map(function (x) { return x.lastIndex }).join("")].join() }
function f5() { var leaked = [], orig = Object.getOwnPropertyDescriptor(RegExp.prototype, "test");
  Object.defineProperty(RegExp.prototype, "test", { get() { leaked.push(this); return orig.value }, configurable: true });
  for (var i = 0; i < 3; i++) /w/.test("w");
  Object.defineProperty(RegExp.prototype, "test", orig);
  return [leaked.length, new Set(leaked).size].join() }
function f6() { var r = [];
  for (var i = 0; i < 3; i++) { /(\d)(\d)?/.test("a" + i); r.push(RegExp.$1 + RegExp.lastMatch + RegExp.input) }
  var o = { toString() { return "k7" } };
  for (var i = 0; i < 2; i++) r.push(/k(\d)/.exec(o)[1]);
  try { /x/.exec(Symbol()) } catch (e) { r.push(e.constructor.name) }
  r.push(/x/.test(), /undefined/.test(), /^1$/.exec(1)[0], /null/.exec(null).index);
  return r.join() }
function f7() { var seen = [], orig = RegExp.prototype.exec;
  var o = { toString() { RegExp.prototype.exec = function (s) { seen.push(this); RegExp.prototype.exec = orig; return orig.call(this, s) }; return "t" } };
  for (var i = 0; i < 3; i++) /t/.test(o);
  return [seen.length, new Set(seen).size].join() }
function f8(s) { var c = 0; for (var i = 0; i < 50; i++) { var m = /(b)(c)?/g.exec(s); c += m.index + m.length + m[1].length } return c }
function f9() { function* g() { for (var i = 0; i < 3; i++) yield /y/g.exec("yy").index } return [...g()].join() }

function tests(n) { var c = 0; for (var i = 0; i < n; i++) if (/a+b/.test(i & 1 ? "aab" : "x")) c++; return c }
function execs(n) { var c = 0; for (var i = 0; i < n; i++) c += /(a+)(b)?/g.exec("xaab").index + /c/y.exec("c").index; return c }
`

// A RegExp literal's RegExp is given back to its pool by the call of Go
// that native code makes of the built-in exec or test, where nothing can
// tell (jitRegExpLitPool): native code's loop of them takes the one RegExp
// over and over, its pool never running out, where a script could tell
// each is a RegExp of its own -- an exec or a getter of the script's, an
// argument whose conversion runs it -- and each answer is the
// interpreter's.
func TestJITRegExpLiteralGivenBack(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	// Each in a runtime of its own: a getter put in a method's place
	// (f5) leaves the realm's RegExps given back no more from native code.
	for _, src := range []string{`f1("aaa")`, `f2()`, `f3()`, `f4()`, `f5()`, `f6()`, `f7()`, `f8("abc")`, `f9()`} {
		rounds := []string{}
		for range 6 {
			rounds = append(rounds, `tests(50)`, src)
		}
		jitAgreesForTest(t, regexpLitSetup, rounds...)
	}
	// What the script's own exec, getter and conversions did to
	// RegExp.prototype leaves the realm's RegExps given back no more from
	// native code; in a runtime of its own, native code's loops are.
	r := jitAgreesForTest(t, regexpLitSetup, `tests(300)`, `execs(300)`, `tests(300)`, `execs(300)`)
	sizes := map[*abi.ObjectPool]uint64{}
	for _, name := range []string{"tests", "execs"} {
		e := jitNativeForTest(t, r, name)
		for _, l := range e.literals {
			if l.lit != nil {
				sizes[l.pool] = l.pool.Size
			}
		}
	}
	if len(sizes) != 3 {
		t.Fatalf("%d literals take their RegExps from pools, not 3", len(sizes))
	}
	// Native code's loops, taking the literals' RegExps from their pools
	// thousands of times, never empty them, which would have had the
	// pools filled anew at twice their sizes (jitRefillPool).
	for range 10 {
		for _, src := range []string{"tests(500)", "execs(500)"} {
			if _, err := r.Run(compileForTest(t, src)); err != nil {
				t.Fatal(err)
			}
		}
	}
	for pool, size := range sizes {
		if pool.Size != size || pool.Count == 0 {
			t.Errorf("a literal's pool: its size %d, was %d, %d in it", pool.Size, size, pool.Count)
		}
	}
}
