//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"testing"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
)

// regexpLitSetup's loops call a RegExp literal's exec and test, and a
// string's replace, split and match of one, as the script semantics tests
// of the tree tier's give-back have them (TestRegExpLiteralReuse,
// TestRegExpLiteralArgReuse): what native code must still tell apart.
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

function tests2(n) { var j, k, c = 0, w = 0, a = ["aab", "x"]; for (j = 0; j < n; j++) { if (/a+b/.test(a[j & 1])) c++; for (k = 0; k < 40; k++) w = (w * 7 + k) & 0xffff } return c + w }
function tests(n) { var c = 0, w = 0, k; for (var i = 0; i < n; i++) { if (/a+b/.test(i & 1 ? "aab" : "x")) c++; for (k = 0; k < 40; k++) w = (w * 7 + k) & 0xffff } return c + w }
function g1(s) { var r = []; for (var i = 0; i < 4; i++) r.push(s.replace(/a(b)?/g, "[$1]"), s.split(/b/).length, s.match(/a/g).length, s.match(/(a)(b)/).index, s.replace(/a/y, "Y")); return r.join() }
function g2() { var leaked = [], orig = RegExp.prototype[Symbol.replace];
  RegExp.prototype[Symbol.replace] = function (s, t) { leaked.push(this); return orig.call(this, s, t) };
  for (var i = 0; i < 3; i++) "abc".replace(/b/g, "x");
  RegExp.prototype[Symbol.replace] = orig;
  return [leaked.length, new Set(leaked).size, leaked.every(function (r) { return r.lastIndex === 0 })].join() }
function g3() { var leaked = [], orig = RegExp.prototype.exec;
  RegExp.prototype.exec = function (s) { leaked.push(this); return orig.call(this, s) };
  for (var i = 0; i < 3; i++) { "abc".match(/b/); "abc".match(/b/g); "abc".replace(/b/, "x"); "a,b".split(/,/) }
  RegExp.prototype.exec = orig;
  return [leaked.length, new Set(leaked).size].join() }
function g4() { var leaked = [], desc = Object.getOwnPropertyDescriptor(RegExp, Symbol.species);
  Object.defineProperty(RegExp, Symbol.species, { get() { var C = function (re, f) { leaked.push(re); return new RegExp(re, f) }; return C }, configurable: true });
  for (var i = 0; i < 3; i++) "a,b".split(/,/);
  Object.defineProperty(RegExp, Symbol.species, desc);
  return [leaked.length, new Set(leaked).size].join() }
function g5() { var leaked = [], orig = RegExp.prototype[Symbol.split];
  RegExp.prototype[Symbol.split] = function (s, l) { leaked.push(this); return orig.call(this, s, l) };
  for (var i = 0; i < 3; i++) "a,b".split(/,/);
  RegExp.prototype[Symbol.split] = orig;
  var origM = RegExp.prototype[Symbol.match];
  RegExp.prototype[Symbol.match] = function (s) { leaked.push(this); return origM.call(this, s) };
  for (var i = 0; i < 3; i++) "ab".match(/b/);
  RegExp.prototype[Symbol.match] = origM;
  return [leaked.length, new Set(leaked).size].join() }
function g6() { var r = [], seen = [];
  for (var i = 0; i < 3; i++) r.push("aXbXc".replace(/x/gi, function (m, off, s) { seen.push(typeof m); return "" + off }));
  var so = new String("a-b"); for (var i = 0; i < 2; i++) r.push(so.split(/-/).join("+"), so.replace(/-/, "="), so.match(/-/).index);
  var lim = { valueOf() { return 1 } }; for (var i = 0; i < 2; i++) r.push("a-b-c".split(/-/, lim).join(), "a-b-c".split(/-/, 2).join(), "".split(/-/).length);
  for (var i = 0; i < 2; i++) { "q1q2".replace(/q(\d)/g, "$1"); r.push(RegExp.$1, RegExp.lastMatch) }
  var t = { toString() { return "T" } }; for (var i = 0; i < 2; i++) r.push("aTb".replace(/T/, t), "aTb".replace(/t/i, "$&$&"));
  return r.join() + "|" + seen.length }
function g7() { var leaked = [], orig = Object.getOwnPropertyDescriptor(RegExp.prototype, "flags");
  Object.defineProperty(RegExp.prototype, "flags", { get() { leaked.push(this); return orig.get.call(this) }, configurable: true });
  for (var i = 0; i < 3; i++) { "a-b".replace(/-/g, "+"); "a-b".split(/-/); "a-b".match(/-/g) }
  Object.defineProperty(RegExp.prototype, "flags", orig);
  return [leaked.length, new Set(leaked).size].join() }
function h1() { var keep = [], keep2 = [], keep3 = [], o; for (var i = 0; i < 4; i++) { "ab".replace((keep[i] = /b/g), "x"); (keep2[i] = /c/y).test("c"); "a,b".split(o = /,/); keep3.push(o); (o = /d/).test("d"); keep3.push(o) } return [new Set(keep).size, new Set(keep2).size, new Set(keep3).size, keep2.map(function (r) { return r.lastIndex }).join("")].join() }
function replaces(n) { var c = 0, w = 0, k; for (var i = 0; i < n; i++) { c += "a-b-c".replace(/-/g, "+").length + "a,b".split(/,/).length + "x1y2".match(/\d/g).length; for (k = 0; k < 40; k++) w = (w * 7 + k) & 0xffff } return c + w }
function execs(n) { var c = 0, w = 0, k; for (var i = 0; i < n; i++) { c += /(a+)(b)?/g.exec("xaab").index + /c/y.exec("c").index; for (k = 0; k < 40; k++) w = (w * 7 + k) & 0xffff } return c + w }
`

// A RegExp literal's RegExp is given back to its pool by the call of Go
// that native code makes of the built-in exec or test, or of a string's
// replace, split or match, where nothing can tell (jitRegExpLitPool): native code's loop of them takes the one RegExp
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
	for _, src := range []string{`f1("aaa")`, `f2()`, `f3()`, `f4()`, `f5()`, `f6()`, `f7()`, `f8("abc")`, `f9()`,
		`g1("abab")`, `g2()`, `g3()`, `g4()`, `g5()`, `g6()`, `g7()`, `h1()`} {
		rounds := []string{}
		for range 6 {
			rounds = append(rounds, `tests(50)`, src)
		}
		jitAgreesForTest(t, regexpLitSetup, rounds...)
	}
	// What the script's own exec, getter and conversions did to
	// RegExp.prototype leaves the realm's RegExps given back no more from
	// native code; in a runtime of its own, native code's loops are.
	r := jitAgreesForTest(t, regexpLitSetup, `tests(300)`, `tests2(300)`, `execs(300)`, `replaces(300)`, `tests(300)`, `tests2(300)`, `execs(300)`, `replaces(300)`)
	sizes := map[*abi.ObjectPool]uint64{}
	entries := map[string]*jitEntry{}
	for _, name := range []string{"tests", "tests2", "execs", "replaces"} {
		e := jitNativeForTest(t, r, name)
		entries[name] = e
		for _, l := range e.literals {
			if l.lit != nil {
				sizes[l.pool] = l.pool.Size
			}
		}
	}
	if len(sizes) != 7 {
		t.Fatalf("%d literals take their RegExps from pools, not 7", len(sizes))
	}
	// Native code's loops, taking the literals' RegExps from their pools
	// thousands of times, never empty them, which would have had the
	// pools filled anew at twice their sizes (jitRefillPool). Native, the
	// work beside each call what the profit check weighs Go's calls
	// against: a loop of the calls alone is left to the tree tier, which
	// gives a literal its RegExp back its own way.
	work := map[string]uint64{}
	for name, e := range entries {
		work[name] = e.ssaStats.work
	}
	for range 10 {
		for _, src := range []string{"tests(500)", "tests2(500)", "execs(500)", "replaces(500)"} {
			if _, err := r.Run(compileForTest(t, src)); err != nil {
				t.Fatal(err)
			}
		}
	}
	for name, e := range entries {
		if e.ssaStats.work-work[name] < 10*500 {
			t.Errorf("%s's loops did not run natively: %+v", name, e.ssaStats)
		}
	}
	for pool, size := range sizes {
		if pool.Size != size || pool.Count == 0 {
			t.Errorf("a literal's pool: its size %d, was %d, %d in it", pool.Size, size, pool.Count)
		}
	}
	// A pool's size stops growing at its capacity: what tells a RegExp
	// given back from one made anew is the pool's objects, the ones it
	// had before thousands of turns, not one filled in anew. (Not what
	// the loops allocate: under checkptr each call of Go allocates.)
	for _, src := range []string{"tests(4000)", "tests2(4000)"} {
		had := map[unsafe.Pointer]bool{}
		for pool := range sizes {
			for _, o := range pool.Objects[:pool.Count] {
				had[o] = true
			}
		}
		if _, err := r.Run(compileForTest(t, src)); err != nil {
			t.Fatal(err)
		}
		for pool := range sizes {
			for _, o := range pool.Objects[:pool.Count] {
				if !had[o] {
					t.Fatalf("%s filled a literal's pool anew", src)
				}
			}
		}
	}
}
