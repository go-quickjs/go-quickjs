//go:build amd64 || arm64

package vm

import (
	"fmt"
	"reflect"
	"runtime"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// The native paths added after the first set, each run by the native
// handlers against the Go table's loop (QJS_NONATIVE's), with the
// collector idle and as while it marks: the same answers, the same exits
// marking or not, and none of the operations a path is for where the
// program meets only what the path answers.

// nativeWarmExits is how often a fast case's operation may exit while
// the caches it reads fill, in a run of a thousand of it; a test whose
// fast case grows what it writes to raises it for the growth's exits.
var nativeWarmExits int64 = 8

// nativeCheck runs src by the Go table and by the native handlers,
// marking and not, and fails on a different answer or different exits;
// where fast is non-empty, it runs fastSrc natively too and fails where
// any of those operations exited past their caches' filling.
func nativeCheck(t *testing.T, src, fastSrc string, fast ...bytecode.ROp) {
	t.Helper()
	nativeCounting(t)
	want := nativeRunSrc(t, src, false)
	var exits [2][256]int64
	for i, marking := range []bool{false, true} {
		nativeMarking(t, marking)
		before := nativeExitOps()
		if got := nativeRunSrc(t, src, true); got != want {
			t.Errorf("native (marking %v):\n%s\nwant\n%s", marking, got, want)
		}
		after := nativeExitOps()
		for op := range after {
			exits[i][op] = after[op] - before[op]
		}
	}
	nativeMarking(t, false)
	if exits[0] != exits[1] {
		for op := range exits[0] {
			if exits[0][op] != exits[1][op] {
				t.Errorf("%s: %d exits, %d while marking", bytecode.ROp(op), exits[0][op], exits[1][op])
			}
		}
	}
	if len(fast) == 0 {
		return
	}
	want = nativeRunSrc(t, fastSrc, false)
	before := nativeExitOps()
	if got := nativeRunSrc(t, fastSrc, true); got != want {
		t.Errorf("native fast case: %s, want %s", got, want)
	}
	after := nativeExitOps()
	for _, op := range fast {
		// A few while the caches fill, none after.
		if n := after[op] - before[op]; n > nativeWarmExits {
			t.Errorf("%s exited %d times", op, n)
		}
	}
	if atomic.LoadInt64(&nativeEnters) == 0 {
		t.Error("nothing ran natively")
	}
}

// TestRegNativeExitCalls runs instructions the code leaves to Go by its
// call of their Go handlers (nativeCallH): handlers that go on, that give
// back ^pc and finish in their slow function, that throw to a handler of
// the frame's or of a caller's, and that end a frame (a generator's
// yield), in the base frame and in frames the code entered itself.
func TestRegNativeExitCalls(t *testing.T) {
	nativeCheck(t, `
		var out = []
		function strs(n) { var s = "abcabc", t = 0; for (var i = 0; i < n; i++) t += s.indexOf("c", i % 6) + String(i).length; return t }
		function thrower(o) { try { return o.x.y } catch (e) { return e.constructor.name } }
		function deep(n) { return n == 0 ? thrower(null) : deep(n - 1) + "" }
		function getter(n) { var o = { get g() { if (n-- == 2) throw new RangeError("r"); return n } }, r = []; for (var i = 0; i < 4; i++) { try { r.push(o.g) } catch (e) { r.push(e.message) } } return r.join() }
		function* gen(n) { for (var i = 0; i < n; i++) yield i * 2 }
		function gens(n) { var t = 0; for (var v of gen(n)) t += v; var g = gen(3); t += g.next().value + g.next().value; return t }
		function stack() { function f(n) { return n ? f(n - 1) : new Error("e").stack } function g(n) { var s = "x".repeat(n); return n ? g(n - 1) + s.length : h() } function h() { return new Error("h").stack } return f(5) + g(3) }
		out.push(strs(50), thrower({x: {y: 1}}), thrower({}), deep(20), getter(5), gens(10), stack())
		var calls = 0
		function inner(x) { calls++; return [x].concat([1]).length + Math.max(x, 1) }
		function outer(n) { var t = 0; for (var i = 0; i < n; i++) t += inner(i); return t }
		out.push(outer(100), calls)
		out.join("\n")`, "")
}

// TestRegNativePostDec runs x-- of numbers natively and of anything else
// by Go.
func TestRegNativePostDec(t *testing.T) {
	nativeCheck(t, `
		var vals = [3, 0, -0, 1.5, NaN, Infinity, -Infinity, 2 ** 53, -(2 ** 31), "5", "", "x", null, undefined, true, {valueOf() { return 7 }}, 10n, -1n], out = []
		function dec(v) { var a = v, b = a--; return [String(a), String(b), typeof a, typeof b].join() }
		for (var v of vals) out.push(dec(v))
		function loop(n) { var j, k = 0; for (j = n; j-- > 0;) k += j; return k + ":" + j }
		out.push(loop(100), loop(0), loop(2.5))
		var o = {n: 3}
		function prop() { var r = []; while (o.n-- > 0) r.push(o.n); return r.join() }
		out.push(prop())
		out.join("\n")`, `
		function loop(n) { var j, k = 0; for (j = n; j-- > 0;) k += j; return k + ":" + j }
		loop(1000)`, bytecode.RPostDec)
}

// TestRegNativePrimMethods runs get_method_w of strings' and numbers'
// methods natively, from the realm's cache of their prototypes' data
// properties, with the prototype changed under the sites -- a method
// replaced, one made a getter, one added -- and receivers of other kinds,
// which Go reads.
func TestRegNativePrimMethods(t *testing.T) {
	nativeCheck(t, `
		var out = []
		function calls(n) { var s = "hello", t = ""; for (var i = 0; i < n; i++) t += s.charAt(i % 5) + (i).toString(16) + s.indexOf("l") + (1.5).toFixed(1); return t }
		out.push(calls(20))
		String.prototype.charAt = function (i) { return "<" + this + i + ">" }
		out.push(calls(3))
		Object.defineProperty(Number.prototype, "toFixed", { get: function () { var self = this; return function () { return "g" + self } }, configurable: true })
		out.push(calls(3))
		Number.prototype.extra = function () { return this * 2 }
		function ex(n) { var t = 0; for (var i = 0; i < n; i++) t += (i).extra(); return t }
		out.push(ex(10))
		function kinds() { var r = []; for (var v of ["s", 1, true, Symbol("q"), 1n]) { try { r.push(v.toString()) } catch (e) { r.push(e.name) } } return r.join() }
		out.push(kinds())
		function miss() { try { return "x".nope() } catch (e) { return e.constructor.name } }
		out.push(miss(), miss())
		out.join("\n")`, `
		function calls(n) { var s = "hello", t = 0; for (var i = 0; i < n; i++) t += s.indexOf("l", i % 3) + s.charCodeAt(1) + (i).toString().length; return t }
		String(calls(1000))`, bytecode.RGetMethodW)
}

// TestRegNativeArrayForOf runs for-of over dense arrays natively: holes,
// an array grown, shrunk and made sparse under the loop, a break, the
// iterator replaced, and cursors of other kinds, which Go steps.
func TestRegNativeArrayForOf(t *testing.T) {
	nativeCheck(t, `
		var out = []
		function sum(a) { var s = 0; for (var v of a) s += v; return s }
		out.push(sum([1, 2, 3]), sum([]), String(sum([1, , 3])), sum([1.5, -0, NaN].concat([4])))
		function grow() { var a = [1, 2], r = []; for (var v of a) { r.push(v); if (a.length < 6) a.push(v * 10) } return r.join() }
		function shrink() { var a = [1, 2, 3, 4, 5], r = []; for (var v of a) { r.push(v); a.length = 3 } return r.join() }
		function sparse() { var a = [1, 2, 3], r = []; for (var v of a) { r.push(v); a[100] = 9 } return r.length }
		function brk() { var r = []; for (var v of [{x: 1}, "s", null, undefined]) { r.push(String(v && v.x)); if (v === null) break } return r.join() }
		out.push(grow(), shrink(), sparse(), brk())
		var saved = Array.prototype[Symbol.iterator]
		Array.prototype[Symbol.iterator] = function* () { yield "own" }
		out.push(sum([1, 2]))
		Array.prototype[Symbol.iterator] = saved
		function others() { var r = []; for (var v of "ab") r.push(v); for (var v of new Set([3, 4])) r.push(v); for (var [k, v] of new Map([[5, 6]])) r.push(k + v); return r.join() }
		out.push(others(), sum([7, 8]))
		out.join("\n")`, `
		function sum(a, n) { var s = 0; for (var i = 0; i < n; i++) for (var v of a) s += v; return s }
		String(sum([1, 2, 3, 4, 5, 6, 7, 8, 9, 10], 100))`, bytecode.RIterNext)
}

// TestRegNativeCharCodeAt runs s.charCodeAt(i) natively over flat
// strings, ASCII and not, at positions in and out of range and of every
// kind, with ropes, the method replaced and bound, and receivers that are
// no string, which Go answers.
func TestRegNativeCharCodeAt(t *testing.T) {
	nativeCheck(t, `
		var out = []
		function codes(s, ps) { var r = []; for (var p of ps) r.push(s.charCodeAt(p)); return r.join() }
		var ps = [0, 1, 2.9, -0.5, -1, 3, 4, 100, NaN, Infinity, -Infinity, "1", null, undefined, true, 1e300, -0, 2 ** 53]
		out.push(codes("abcd", ps), codes("añ€😀", ps), codes("", ps))
		var rope = "ab"; for (var i = 0; i < 20; i++) rope += String.fromCharCode(65 + i)
		out.push(codes(rope, [0, 5, 21]), codes(rope, [21, 22]))
		var wide = "é"; for (var i = 0; i < 5; i++) wide += "ü"
		out.push(codes(wide, [0, 5, 6]))
		function sum(s) { var t = 0; for (var i = 0; i < s.length; i++) t = (t * 31 + s.charCodeAt(i)) | 0; return t }
		out.push(sum("hello world"), sum("ünïcödé"))
		var saved = String.prototype.charCodeAt
		String.prototype.charCodeAt = function (i) { return -i }
		out.push(sum("abc"))
		String.prototype.charCodeAt = saved.bind("zzz")
		out.push(sum("abc"))
		String.prototype.charCodeAt = saved
		function other(v) { try { return v.charCodeAt(0) } catch (e) { return e.name } }
		out.push(other(5), other(null), other({charCodeAt: function () { return "o" }}), other(new String("q")), sum("abc"))
		out.join("\n")`, `
		function sum(s) { var t = 0; for (var i = 0; i < s.length; i++) t = (t * 31 + s.charCodeAt(i)) | 0; return t }
		var s = "the quick brown fox jumps over the lazy dog", u = "ünïcödé ünïcödé", t = 0
		for (var j = 0; j < 30; j++) t += sum(s) + sum(u)
		String(t)`, bytecode.RCharCodeAt)
}

// TestRegNativeTailCalls runs calls in tail position natively while the
// stack is shallow and, deeper, frames given to their callees in place by
// Go and run on in the same code entry: self and mutual recursion past
// tailCallDepth, captured bindings, a throw from deep in a chain, method
// and arrow callees, sloppy callees that get frames of their own, an
// interrupt's count, and the stack traces of errors made at the bottom.
func TestRegNativeTailCalls(t *testing.T) {
	nativeCheck(t, `
		"use strict"
		var out = []
		function self(n, acc) { if (n === 0) return acc; return self(n - 1, (acc + n) | 0) }
		function even(n) { return n === 0 ? 1 : odd(n - 1) }
		function odd(n) { return n === 0 ? 0 : even(n - 1) }
		out.push(self(10, 0), self(5000, 0), even(3001), odd(3001))
		function caps(n, fs) { if (n === 0) return fs.map(function (f) { return f() }).join(); var k = n; fs.push(function () { return k }); return caps(n - 1, fs) }
		out.push(caps(40, []))
		function thrower(n) { if (n === 0) throw new RangeError("deep " + n); return thrower(n - 1) }
		try { thrower(100) } catch (e) { out.push(e.message) }
		var o = { m(n) { return n === 0 ? "m" : this.m(n - 1) } }
		var arrow = (n) => n === 0 ? "a" : arrow(n - 1)
		out.push(o.m(200), arrow(200))
		function trace(n) { if (n === 0) return new Error("t").stack.split("\n").slice(0, 4).join("|"); return trace(n - 1) }
		out.push(trace(10), trace(100))
		function mixed(n) { if (n === 0) return "x".repeat(3); if (n % 7 === 0) return mixed(n - 1) + ""; return mixed(n - 1) }
		out.push(mixed(300))
		out.join("\n")`, "")
	nativeCheck(t, `
		var out = []
		function sloppy(n, acc) { if (n === 0) return acc + sloppy.length; return sloppy(n - 1, acc + 1) }
		out.push(sloppy(200, 0))
		out.join("\n")`, `
		"use strict"
		function self(n, acc) { if (n === 0) return acc; return self(n - 1, (acc + n) | 0) }
		var t = 0
		for (var i = 0; i < 50; i++) t += self(20, i)
		String(t)`, bytecode.RCallWT)
	// Past tailCallDepth, in frames the code entered itself: the frame
	// given to the callee in place natively (tailTo) -- self and mutual
	// recursion, a method's and an arrow's, fewer and more arguments than
	// parameters, a callee whose bindings are not its parameters -- and
	// what it leaves to Go: a callee that keeps its arguments, captures
	// open on the frame. Beside the caches' filling, a call exits where
	// the native frame stack grows (8 records to 64, three times), and
	// where the interrupt's count is due, every interruptCheckInterval
	// calls -- the chains' and the calls under tailCallDepth that start
	// them, at most as many again -- which Go makes, the frame going on
	// in place from it (nativeRestartDeep).
	was := nativeWarmExits
	nativeWarmExits += 3 + 2*20*(500+300+300+301)/interruptCheckInterval
	defer func() { nativeWarmExits = was }()
	nativeCheck(t, `
		"use strict"
		var out = []
		function fewer(n, a, b) { if (n === 0) return String(a) + String(b); return fewer(n - 1, a) }
		function more(n, a) { if (n === 0) return a; return more(n - 1, a + 1, n, n) }
		function locals(n, a) { var x = a * 2, y; if (n === 0) return x + String(y); return locals(n - 1, a + 1) }
		function args(n) { if (n === 0) return arguments.length; return args(n - 1, 1, 2) }
		function caps(n, f) { if (n === 0) return f(); var k = n; return caps(n - 1, n % 50 ? f : function () { return k }) }
		out.push(fewer(300, 7, 8), more(300, 0), locals(300, 1), args(300), caps(300, null))
		out.join("\n")`, `
		"use strict"
		function self(n, acc) { if (n === 0) return acc; return self(n - 1, (acc + n) | 0) }
		var o = { m(n, a) { return n === 0 ? a : this.m(n - 1, a + 1) } }
		var arrow = (n, a) => n === 0 ? a : arrow(n - 1, a + 2)
		function even(n) { return n === 0 ? 1 : odd(n - 1) }
		function odd(n) { return n === 0 ? 0 : even(n - 1) }
		var t = 0
		for (var i = 0; i < 20; i++) t += self(500, i) + o.m(300, i) + arrow(300, i) + even(301)
		String(t)`, bytecode.RCallWT, bytecode.RCallMWT)
}

// TestRegNativeTypedBigClamped runs the elements of BigInt64Array and
// BigUint64Array read and written natively, and Uint8ClampedArray's
// writes: the int64 range's ends, values past 64 bits, which wrap, and
// heap BigInts, which Go writes; every kind of number clamped.
func TestRegNativeTypedBigClamped(t *testing.T) {
	nativeCheck(t, `
		var out = []
		var bigs = [0n, 1n, -1n, 2n ** 63n - 1n, -(2n ** 63n), 2n ** 63n, 2n ** 64n - 1n, -(2n ** 64n) + 1n, 2n ** 64n, 2n ** 70n + 5n, -(2n ** 65n) - 3n, 12345678901234567n]
		function rw(T) { var a = new T(bigs.length), r = []; for (var i = 0; i < bigs.length; i++) a[i] = bigs[i]; for (var i = 0; i < a.length; i++) r.push(String(a[i])); a[0] = 5n; a[1] = a[0] + 1n; r.push(String(a[1])); try { a[2] = 1 } catch (e) { r.push(e.name) } return r.join() }
		out.push(rw(BigInt64Array), rw(BigUint64Array))
		function sum(a) { var s = 0n; for (var i = 0; i < a.length; i++) s += a[i]; return s }
		out.push(String(sum(BigInt64Array.from(bigs.slice(0, 5)))), String(sum(BigUint64Array.from([1n, 2n, 3n]))))
		var nums = [NaN, -1, -0, 0, 0.4, 0.5, 0.6, 1.5, 2.5, 3.5, 127.5, 254.5, 254.6, 255, 255.5, 300, -Infinity, Infinity, 1e300, -1e300, 2 ** 53, "7", null, true]
		function clamp() { var a = new Uint8ClampedArray(nums.length); for (var i = 0; i < nums.length; i++) a[i] = nums[i]; return Array.from(a).join() }
		out.push(clamp())
		var pairs = [[1n, 2n], [2n, 1n], [-5n, 3n], [3n, -5n], [-(2n ** 63n), 2n ** 63n - 1n], [2n ** 63n, 2n ** 63n - 1n], [-(2n ** 64n) + 1n, -(2n ** 63n)], [7n, 7n], [2n ** 100n, 1n], [1n, 2], [2, 1n], [1n, "2"]]
		function cmps() { var r = []; for (var [x, y] of pairs) { var s = ""; if (x < y) s += "<"; if (x <= y) s += "l"; if (x > y) s += ">"; if (x >= y) s += "g"; if (!(x < y)) s += "!"; r.push(s) } return r.join() }
		out.push(cmps())
		var sorted = BigInt64Array.from([5n, -3n, 2n ** 62n, -(2n ** 63n), 0n, 7n]).sort()
		out.push(String(sorted))
		out.join("\n")`, `
		var a = new BigInt64Array(64), u = new BigUint64Array(64), c = new Uint8ClampedArray(64), t = 0n, n = 0
		for (var k = 0; k < 20; k++) for (var i = 0; i < 64; i++) { a[i] = BigInt(i) - 30n; u[i] = a[i]; c[i] = i * 4.5 - 10; if (a[i] < 0n) t += a[i]; if (a[i] >= u[i]) n++ }
		String(t) + " " + n + " " + c[3]`, bytecode.RSetIndex, bytecode.RGetIndex, bytecode.RJLt, bytecode.RJNLt, bytecode.RJGe, bytecode.RJNGe)
}

// TestRegNativeArrayAppend runs element writes at an array's length
// natively where it has room: arrays grown past their room, frozen,
// sealed, of a length made read-only, sparse, with index keys of their
// own or on their prototype chain, with a prototype changed, subclassed,
// and arguments objects, which Go writes.
func TestRegNativeArrayAppend(t *testing.T) {
	// Each of the eight arrays grows past its room some ten times.
	nativeWarmExits = 100
	defer func() { nativeWarmExits = 8 }()
	nativeCheck(t, `
		var out = []
		function fill(a, n) { for (var i = a.length; i < n; i++) a[i] = i * 2; return a }
		function fill2(a, n) { for (var i = 0; i < n; i++) a[a.length] = i; return a }
		out.push(fill([], 50).join(), fill2([1, 2], 40).length)
		function tryFill(a) { try { fill(a, a.length + 3) } catch (e) { return e.name + a.length } return a.length + ":" + Array.prototype.join.call(a) }
		var fr = Object.freeze([1]), se = Object.seal([1]), ro = [1, 2]
		Object.defineProperty(ro, "length", { writable: false })
		var sp = [1]; sp[100] = 5; sp.length = 2
		var own = [1]; own.x = 1; own["7"] = 9
		var sub = new (class extends Array {})(); sub.push(1)
		var other = [1]; Object.setPrototypeOf(other, { 3: "p" })
		out.push(tryFill(fr), tryFill(se), tryFill(ro), tryFill(sp), tryFill(own), tryFill(sub), tryFill(other))
		function args() { fill(arguments, 4); return [].slice.call(arguments).join() + arguments.length }
		out.push(args(1))
		Array.prototype[5] = "proto"
		out.push(fill([], 8).join(), [].concat(fill2([], 7)).join())
		delete Array.prototype[5]
		Object.defineProperty(Object.prototype, "6", { set: function (v) { out.push("set " + v) }, configurable: true })
		out.push(fill([], 8).join())
		delete Object.prototype[6]
		var tas = fill(new Int8Array(4), 4)
		out.push(String(tas), fill([], 9).length)
		out.join("\n")`, `
		function fill(n) { var a = []; for (var i = 0; i < n; i++) a[i] = i; var b = []; for (var i = 0; i < n; i++) b[b.length] = a[i]; return a.length + b.length }
		var t = 0
		for (var j = 0; j < 4; j++) t += fill(500)
		String(t)`, bytecode.RSetIndex)
}

// TestRegNativeStringEquals runs === and !== of two flat strings of one
// length natively, by their text: ASCII and not, of one length in code
// units and not in bytes, lone surrogates, lengths around eight bytes,
// and ropes, which Go flattens first.
func TestRegNativeStringEquals(t *testing.T) {
	nativeCheck(t, `
		var out = []
		var strs = ["", "a", "b", "é", "ab", "abcdefgh", "abcdefgi", "abcdefghi", "abcdefghj", "bbcdefghi", "ünïcödé", "ünïcödè", "\ud83d", "\ude00", "😀", "\ud83d\ude00", "x".repeat(17), "x".repeat(16) + "y", String.fromCharCode(97, 98), "z".repeat(2000), "z".repeat(1999) + "y", "z".repeat(1024)]
		function eq() { var r = ""; for (var x of strs) for (var y of strs) { r += x === y ? 1 : 0; if (x !== y) r += "!" } return r }
		out.push(eq())
		var rope = "ab"; rope += "cdefgh"
		function eq2(a, b) { return [a === b, a !== b, b === a].join() }
		out.push(eq2(rope, "abcdefgh"), eq2(rope, "abcdefgx"), eq2("ab" + String(1), "ab1"))
		out.join("\n")`, `
		var keys = ["alpha", "beta", "gamma", "delta", "alphabetical", "betamaximum"], copies = keys.map(function (k) { return k.split("").join("") }), n = 0
		for (var j = 0; j < 30; j++) for (var i = 0; i < keys.length; i++) for (var k = 0; k < keys.length; k++) if (keys[i] === copies[k]) n++
		String(n)`, bytecode.RJSEq, bytecode.RJSNe)
}

// nativeGoid is the running goroutine's id, from its stack's header.
func nativeGoid() string {
	b := make([]byte, 64)
	b = b[:runtime.Stack(b, false)]
	f := strings.Fields(string(b))
	if len(f) < 2 {
		return ""
	}
	return f[1]
}

// nativeDeep recurses n deep with a frame of some size, which grows the
// goroutine's stack: what a wrong g (R14, R28) at a Go handler would
// crash.
//
//go:noinline
func nativeDeep(n int) int {
	var pad [32]int
	pad[n%32] = n
	if n == 0 {
		return pad[0]
	}
	return nativeDeep(n-1) + pad[n%32]
}

// nativeZeroed reports whether a fresh array of n words is all zero: Go
// zeroes it on amd64 by X15, which its internal convention keeps zero.
//
//go:noinline
func nativeZeroed(n int) bool {
	var z [16]uint64
	for _, v := range z[:n] {
		if v != 0 {
			return false
		}
	}
	return true
}

// TestRegNativeCallHConvention checks the convention nativeCallH calls Go
// handlers by, Go's internal one (ABIInternal) that the exit stub and the
// trampoline write by hand: on amd64 the handler's four arguments in AX,
// BX, CX and DI, the func value in DX (the handler here is a closure,
// which reads its captures through it), its result in AX, the goroutine
// in R14 and X15 zero; on arm64 the arguments in R0 to R3, the func value
// in R26, the result in R0 and the goroutine in R28 (nativeCallRegs). A
// Go handler called through it gets the arguments the Go table's loop
// would give it, runs on the test's goroutine, grows the stack, zeroes
// memory, and what it returns is where the code goes on; its stack walks
// through nativeCallH to nativeRun, and a collection while it runs scans
// that stack. A toolchain whose convention differs fails it, and the
// native handlers then need their trampoline changed.
func TestRegNativeCallHConvention(t *testing.T) {
	op := bytecode.RNewArray
	if nativePlain.has[op] {
		t.Fatalf("%s has a native handler: the test needs an operation that exits", op)
	}
	want := nativeGoid()
	// The exit stub calls the exit table's function (nativeExitTable),
	// the operation's Go handler.
	orig := nativeExitTable[op]
	defer func() { nativeExitTable[op] = orig }()
	var calls, viaCallH int
	var bad []string
	nativeExitTable[op] = func(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
		calls++
		pcs := make([]uintptr, 16)
		frames := runtime.CallersFrames(pcs[:runtime.Callers(1, pcs)])
		afterCallH := false
		for {
			f, more := frames.Next()
			if afterCallH {
				// nativeCallH's caller, to the unwinder: nativeLoop, which
				// entered the code.
				if !strings.HasSuffix(f.Function, ".nativeLoop") {
					bad = append(bad, "nativeCallH's caller is "+f.Function+", not nativeLoop")
				}
				afterCallH = false
			}
			if strings.HasSuffix(f.Function, ".nativeCallH") {
				viaCallH++
				afterCallH = true
			}
			if !more {
				break
			}
		}
		switch {
		case st == nil || st.c == nil:
			bad = append(bad, "no state ("+nativeCallRegs[3]+")")
		case regs != st.regs:
			bad = append(bad, "the registers ("+nativeCallRegs[0]+") are not the state's")
		case pc < 0 || pc >= len(st.c.src):
			bad = append(bad, fmt.Sprintf("pc (%s) %d out of the code", nativeCallRegs[2], pc))
		case regAt(st.c.code, pc) != w || w.Op() != op:
			bad = append(bad, fmt.Sprintf("the instruction (%s) %#x is not the code's at pc %d", nativeCallRegs[1], uint64(w), pc))
		}
		if g := nativeGoid(); g != want {
			bad = append(bad, fmt.Sprintf("goroutine (%s) %q, want %q", nativeCallRegs[4], g, want))
		}
		if nativeDeep(2000) != 2000*2001/2 {
			bad = append(bad, "the deep recursion's sum")
		}
		if !nativeZeroed(16) {
			bad = append(bad, "a fresh array not zero (X15 on amd64)")
		}
		if calls%50 == 1 {
			// A collection scans the stack through nativeCallH's frame.
			runtime.GC()
		}
		return orig(regs, w, pc, st)
	}
	src := `function f(n) { var s = 0; for (var i = 0; i < n; i++) { var o = [i, 1]; s += o[0] + o[1] } return s }
		String(f(200)) + " " + String((function (k) { return [k] })(7)[0])`
	if got := nativeRunSrc(t, src, true); got != "20100 7" {
		t.Errorf("got %q, want %q", got, "20100 7")
	}
	if calls == 0 || viaCallH == 0 {
		t.Fatalf("%d calls of the handler, %d through nativeCallH: the test reached no exit's call", calls, viaCallH)
	}
	if len(bad) > 0 {
		t.Fatalf("nativeCallH's convention does not hold on this toolchain (%d of %d calls): %s", len(bad), calls, strings.Join(bad, "; "))
	}
}

// nativeCallRegs names the registers nativeCallH gives a Go handler its
// arguments in -- the registers, the instruction, its pc, the state --
// and the goroutine, by the architecture's convention.
var nativeCallRegs = map[string][5]string{
	"amd64": {"AX", "BX", "CX", "DI", "R14"},
	"arm64": {"R0", "R1", "R2", "R3", "R28"},
}[runtime.GOARCH]

// An exit from a native handler that answers all its Go handler's fast
// path does calls the operation's slow function at once
// (nativeExitTable), not the Go handler, which would only give back ^pc:
// every operation of nativeSlowOnMiss has a native handler, and its
// entry is its slow function; the others' are their Go handlers. The
// answers of such misses -- strings, BigInts, objects, NaN and numbers
// past int64 to the arithmetic and the bitwise operators, into registers
// that held pointers -- are the table's, natively, with the collector
// marking and not.
func TestRegNativeExitSlow(t *testing.T) {
	slow := map[bytecode.ROp]bool{}
	for _, op := range nativeSlowOnMiss {
		slow[op] = true
		if !nativePlain.has[op] {
			t.Errorf("%s has no native handler", op)
		}
	}
	for op := range nativeExitTable {
		want, what := regTable[op], "Go handler"
		if slow[bytecode.ROp(op)] {
			want, what = regSlowTable[op], "slow function"
		}
		if reflect.ValueOf(nativeExitTable[op]).Pointer() != reflect.ValueOf(want).Pointer() {
			t.Errorf("%s: the exit table's entry is not its %s", bytecode.ROp(op), what)
		}
	}
	src := `
	var vals = [1, 2.5, -0, NaN, 2 ** 70, -(2 ** 40), "12", "x", "", true, null, undefined, 5n, 2n ** 80n,
		{ valueOf() { return 3 } }, [4], {}];
	var out = [];
	for (var i = 0; i < vals.length; i++) {
		var a = vals[i], o = {};
		for (var j = 0; j < vals.length; j++) {
			var b = vals[j], r;
			try { r = [a + b, a - b, a * b, a & b, a | b, a ^ b, a << b, a >> b, a >>> b].map(String).join(" ") } catch (e) { r = e.name }
			out.push(r);
			try { o = a + 1; o = a - 1; o = a | 0; o = a ^ 5; o = a & 6; o = a << 1; o = a >> 1; o = a >>> 1; r = String(o) } catch (e) { r = e.name }
			out.push(r);
			var k = a;
			try { k++; k--; r = String(k++) + String(k--) + String(k) } catch (e) { r = e.name }
			out.push(r);
		}
	}
	out.join("|")`
	want := nativeRunSrc(t, src, false)
	for _, marking := range []bool{false, true} {
		nativeMarking(t, marking)
		if got := nativeRunSrc(t, src, true); got != want {
			t.Errorf("native (marking %v): %s\nwant %s", marking, got, want)
		}
	}
}

// TestRegNativeGenStep runs for-of loops over generators of register
// code, whose steps and yields the native handlers make (genStep,
// genYield): sums, returns, throws caught in the body, past the loop and
// by the caller, a break that closes the generator, a next of a running
// generator, captures, this, arguments, yield*, generators nested and
// recursing past the native frame stack's first room, stack traces from
// inside a step, and next replaced, which Go steps.
func TestRegNativeGenStep(t *testing.T) {
	nativeCheck(t, `
		var out = []
		function* count(n) { for (var i = 0; i < n; i++) yield i; return "r" + n }
		function sum(it) { var s = 0; for (var v of it) s += v; return s }
		out.push(sum(count(10)), sum(count(0)), sum(count(1)))
		function* thrower(n) { for (var i = 0; ; i++) { if (i == n) throw new Error("at " + i); yield i } }
		function caught(n) { var s = 0; try { for (var v of thrower(n)) s += v } catch (e) { return s + " " + e.message } }
		out.push(caught(0), caught(3))
		function* inner(n) { for (var i = 0; i < n; i++) { try { if (i == 2) throw i; yield i } catch (e) { yield "c" + e } } }
		out.push([...inner(4)].join())
		var closed = []
		function* fin() { try { yield 1; yield 2; yield 3 } finally { closed.push("fin") } }
		function brk() { var r = []; for (var v of fin()) { r.push(v); if (v == 2) break } return r.join() + closed.join() }
		out.push(brk())
		function running() { var g = count(5), r = ""; try { for (var v of g) { if (v == 2) g.next() } } catch (e) { r = e.constructor.name } return r }
		out.push(running())
		function* caps(n) { var fs = []; for (var i = 0; i < n; i++) { let k = i; fs.push(() => k); yield fs.length } yield fs.map(f => f()).join("") }
		out.push([...caps(3)].join())
		var o = { base: 7, *gen(n) { for (var i = 0; i < n; i++) yield this.base + i } }
		out.push(sum(o.gen(3)))
		function* args() { for (var i = 0; i < arguments.length; i++) yield arguments[i] }
		out.push(sum(args(1, 2, 3)))
		function* params(a, b) { yield a; yield b; yield a + b }
		out.push(sum(params(3, 4)))
		function* deleg(n) { yield "a"; yield* count(n); yield* ["x", "y"]; yield "b" }
		out.push([...deleg(3)].join())
		function* nest(n) { for (var v of count(n)) for (var w of count(v)) yield v * 10 + w }
		out.push(sum(nest(5)))
		function* deep(n) { if (n > 0) for (var v of deep(n - 1)) yield v; yield n }
		out.push(sum(deep(300)))
		function* where() { yield new Error("x").stack.split("\n").slice(0, 3).join("|") }
		function trace() { var r = []; for (var i = 0; i < 3; i++) for (var v of where()) r.push(v); return r[2] }
		out.push(trace())
		function* stepped() { for (var i = 0; i < 4; i++) yield i }
		var g2 = stepped(); g2.next = function () { return { done: this.n > 2, value: this.n++ } }; g2.n = 0
		out.push(sum(g2), sum(stepped()))
		function* inWith(o) { yield 0; with (o) { yield x; x += 1; yield x } yield typeof x }
		out.push([...inWith({ x: 5 })].join())
		function* firstWith(o) { with (o) { yield x; yield y } }
		out.push([...firstWith({ x: 1, y: 2 })].join())
		function* rets() { var r = yield 1; yield r }
		out.push([...rets()].map(String).join())
		out.join("\n")`, `
		function* count(n) { for (var i = 0; i < n; i++) yield i }
		function sum(n) { var s = 0; for (var v of count(n)) s += v; return s }
		String(sum(5000))`, bytecode.RIterNext, bytecode.RYield)
}

// nativeClosuresSrc makes closures the native handlers make and close
// (closure_w, close_regs, a return's closing): a binding of each turn
// captured, several at once, captures of captures, an arrow's this,
// closures under a with, a sloppy function's mapped arguments, and
// closures kept past their maker's return and called later.
const nativeClosuresSrc = `
	var out = []
	function turns(n) { var fs = []; for (let i = 0; i < n; i++) { let k = i * 2; fs.push(function () { return k + i }) } return fs.map(f => f()).join() }
	out.push(turns(5))
	function many(a, b) { var c = a + b; let d = c * 2; var f = function () { return a + b + c + d }; a = 10; return f() }
	out.push(many(1, 2))
	function outer(x) { return function (y) { return function (z) { return x + y + z } } }
	out.push(outer(1)(2)(3))
	var o = { v: 4, m() { var fs = []; for (let i = 0; i < 3; i++) fs.push(() => this.v + i); return fs.map(f => f()).join() } }
	out.push(o.m())
	function counter() { var c = 0; return { inc() { return ++c }, get() { return c } } }
	var cs = []; for (var i = 0; i < 3; i++) cs.push(counter())
	cs[0].inc(); cs[2].inc(); cs[2].inc()
	out.push(cs.map(c => c.get()).join())
	function mapped(a, b) { var f = function () { return a + b }; arguments[0] = 7; return f() + arguments.length }
	out.push(mapped(1, 2))
	function withed(w) { var r = []; with (w) { for (let i = 0; i < 2; i++) r.push(() => x + i) } return r.map(f => f()).join() }
	out.push(withed({ x: 5 }))
	function deep(n) { if (n == 0) return () => 0; let k = n; var g = deep(n - 1); return () => k + g() }
	out.push(deep(50)())
	function loopy(n) { var s = 0; for (let j = 0; j < n; j++) { let k = j; var f = function () { return k }; s = (s + f()) | 0 } return s }
	out.push(loopy(1000))
	function appends(a) { var s = a; var add = function (y) { s += y }; add("x"); add("y"); return s }
	out.push(appends("a"), appends("b"))
	function getters() { var fs = []; for (let i = 0; i < 3; i++) fs.push(function () { return i }); var s = 0; for (var j = 0; j < 100; j++) for (var f of fs) s += f(); return s }
	out.push(getters())
	function tdz() { var g = function () { return k }; var r; try { g() } catch (e) { r = e.name } let k = 1; return r + g() }
	out.push(tdz())
	function cb() { var k = 3; var r = [1, 2].map(function () { return k }); k = 4; return r.concat([1].map(function () { return k })).join() }
	out.push(cb())
	function setter() { var v = 1; var get = function () { return v }; var a = get(); v = "s"; return a + get() }
	out.push(setter())
	out.join("\n")`

// TestRegNativeClosures runs nativeClosuresSrc natively and by the Go
// table, marking and not; its fast case makes a closure each turn, with
// no exit for closure_w or close_regs.
func TestRegNativeClosures(t *testing.T) {
	nativeCheck(t, nativeClosuresSrc, `
		function loopy(n) { var s = 0; for (let j = 0; j < n; j++) { let k = j; var f = function () { return k }; s = (s + f()) | 0 } return s }
		String(loopy(5000))`, bytecode.RClosureW, bytecode.RCloseRegs)
}

// TestRegNativeClosuresCollecting runs nativeClosuresSrc natively and by
// the Go table with the collector running all the time (GOGC=1), so that
// it marks while the closures and their upvalues are made and closed.
func TestRegNativeClosuresCollecting(t *testing.T) {
	gc := debug.SetGCPercent(1)
	t.Cleanup(func() { debug.SetGCPercent(gc) })
	want := nativeRunSrc(t, nativeClosuresSrc, false)
	for range 3 {
		if got := nativeRunSrc(t, nativeClosuresSrc, true); got != want {
			t.Fatalf("native:\n%s\nwant\n%s", got, want)
		}
	}
}

// TestRegNativeSwitch runs switches natively: numbers of dense tables in
// and out of range, fractions, -0 and NaN, numbers of few and scattered
// cases, which Go answers; strings by their constants and by their text,
// built, non-ASCII, ropes, longer than the native compare goes, of a
// length no case has; booleans, null, undefined and objects.
func TestRegNativeSwitch(t *testing.T) {
	nativeCheck(t, `
		var out = []
		function dense(x) { switch (x) { case 1: return "one"; case 2: return "two"; case 3: return "three"; case 4: return "four"; default: return "d" } }
		out.push([1, 2, 3, 4, 0, 5, 2.5, -0, NaN, "1", 1e300, -1].map(dense).join())
		function few(x) { switch (x) { case 7: return "s"; case 1000: return "k"; default: return "d" } }
		function scattered(x) { switch (x) { case 1: return 1; case 100: return 2; case 1e6: return 3; case 7.5: return 4; case -3: return 5; case 42: return 6; case 99999: return 7; case 0.25: return 8; case 12345: return 9; default: return 0 } }
		out.push([7, 1000, 8].map(few).join(), [1, 100, 1e6, 7.5, -3, 42, 99999, 0.25, 12345, 5].map(scattered).join())
		function str(x) { switch (x) { case "a": return 1; case "div": return 2; case "span": return 3; case "héllo": return 4; case "": return 5; case "x".repeat(1100): return 6; case "dix": return 7; default: return 0 } }
		var built = ["d" + "iv", "sp".concat("an"), "h\u00e9llo", "", "x".repeat(1100), "x".repeat(1099), "di" + "x", "dvi", "span ", "a", ["sp", "an"].join("")]
		out.push(built.map(str).join(), ["a", "div", "span", "zzz", "dix"].map(str).join())
		var rope = "spa"; for (var i = 0; i < 3; i++) rope += i; out.push(str(rope.slice(0, 3) + "n"))
		function other(x) { switch (x) { case true: return "t"; case false: return "f"; case null: return "n"; case undefined: return "u"; case "1": return "s"; default: return "d" } }
		out.push([true, false, null, undefined, {}, "1", 1].map(other).join())
		function mixed(x) { switch (x) { case 1: return "n1"; case "1": return "s1"; case 2: return "n2"; default: return "d" } }
		out.push([1, "1", 2, "2", true].map(mixed).join())
		out.join("\n")`, `
		function sw(k) { switch (k) { case "a": return 1; case "div": return 2; case "span": return 3; case 1: return 4; case 2: return 5; default: return 0 } }
		var keys = ["d" + "iv", "a", "span", "zz"], s = 0
		for (var i = 0; i < 4000; i++) s += sw(keys[i & 3])
		String(s)`, bytecode.RSwitch)
}

// TestRegNativeBin runs divisions and remainders natively: int32s of
// every sign, zeros of both signs, fractions, NaN, infinities, numbers
// past int32, and operands that are not numbers, which Go answers.
func TestRegNativeBin(t *testing.T) {
	nativeCheck(t, `
		var vals = [7, -7, 3, -3, 0, -0, 1, 2147483647, -2147483648, 2147483648, 4294967296, 0.5, -2.5, 1e300, NaN, Infinity, -Infinity, "6", null, true]
		var out = []
		function div(a, b) { return a / b }
		function mod(a, b) { return a % b }
		for (var x of vals) for (var y of vals) out.push(Object.is(div(x, y), -0) ? "-0" : String(div(x, y)), Object.is(mod(x, y), -0) ? "-0" : String(mod(x, y)))
		out.join()`, `
		function f(n) { var s = 0; for (var j = 0; j < n; j++) s += (j % 10) + (j % 7) / 2; return s }
		String(f(5000))`, bytecode.RBin)
}

// TestRegNativeStrFreeze reads strings appends grow in place, natively and
// by the Go table, marking and not: a closure's appends to the string it
// captures read back each turn and kept, a local's grown and read,
// constants, numbers and objects frozen, and the reads keeping what they
// read while the appends go on; its fast case freezes with no exit.
func TestRegNativeStrFreeze(t *testing.T) {
	nativeCheck(t, `
		var out = []
		function sink() {
			var s = ""
			return { push: function (x) { s += x }, get: function () { return s }, len: function () { return s.length } }
		}
		var k = sink(), kept = []
		for (var i = 0; i < 40; i++) { k.push(i % 3 ? "<x>" : String(i)); if (i % 7 == 0) kept.push(k.get()); out.push(k.len()) }
		out.push(kept.join("|"), k.get())
		function local(n) { var s = "a", r = []; for (var i = 0; i < n; i++) { s += i; r.push(s) } return r.join() }
		out.push(local(12))
		function other(v) { var f = function () { return v }; return f() }
		out.push(other(1), other(null), other("c"), typeof other({}))
		out.join()`, `
		var k = (function () { var s = ""; return { push: function (x) { s += x }, get: function () { return s } } })()
		var c = 0
		for (var i = 0; i < 1000; i++) { k.push("<x>"); c += k.get().length }
		String(c)`, bytecode.RStrFreeze)
}

// TestRegNativePush pushes one argument natively where an array has room,
// against the Go table: arrays of every kind of element, literals whose
// elements are shared, arrays grown past their room, frozen, sealed and
// non-extensible ones, a length made read-only, holes, index keys on the
// array and up its chain (a setter of Array.prototype's among them), an
// array-like object, push bound and borrowed, other counts of arguments,
// and a result kept in the receiver's register; its fast case pushes with
// no exit but the growth's.
func TestRegNativePush(t *testing.T) {
	was := nativeWarmExits
	nativeWarmExits = 16
	defer func() { nativeWarmExits = was }()
	nativeCheck(t, `
		var out = []
		function p1(a, v) { return a.push(v) }
		function lit() { return [1, 2, 3] }
		for (var round = 0; round < 3; round++) {
			var a = [], s = []
			for (var i = 0; i < 40; i++) s.push(p1(a, i % 3 ? {k: i} : i % 2 ? "s" + i : i))
			out.push(s.join(), a.length, typeof a[1], a[39])
			var l = lit(); out.push(p1(l, 4), l.join(), lit().join())
			var f = Object.freeze([1]), z = Object.seal([1]), n = Object.preventExtensions([1])
			for (var x of [f, z, n]) { try { out.push(p1(x, 2)) } catch (e) { out.push(e.constructor.name) } out.push(x.length) }
			var ro = [1]; Object.defineProperty(ro, "length", { writable: false })
			try { p1(ro, 2) } catch (e) { out.push(e.constructor.name, ro.length) }
			var h = [1, , 3]; out.push(p1(h, 4), 1 in h)
			var ik = [1]; ik.x = 1; out.push(p1(ik, 2))
			var like = { length: 2, push: [].push }; out.push(p1(like, "v"), like[2], like.length)
			var bp = [].push.bind(a); out.push(bp(1), a.length)
			var b2 = []; out.push(b2.push(), b2.push(1, 2), b2.join())
			var keep = [7]; keep = keep.push(8); out.push(keep)
			var sub = Object.create([5]); out.push(p1(sub, 6), sub.length)
			if (round == 1) {
				var log = []
				Object.defineProperty(Array.prototype, "3", { set: function (v) { log.push("set" + v) }, configurable: true })
				var q = [0, 1, 2]; out.push(p1(q, 9), q.length, log.join(), q.hasOwnProperty(3))
				delete Array.prototype[3]
			}
		}
		out.join("|")`, `
		var a = [], s = 0
		for (var i = 0; i < 1000; i++) s += a.push(i)
		String(s) + " " + a.length`, bytecode.RCallMW)
}

// nativeLitSrc makes object literals and empty arrays natively: one site
// of fixed keys in a loop, sites of one first key and others of their
// own, literals of many keys, of values of every kind and of nested
// literals, empty arrays grown and kept, all read back.
const nativeLitSrc = `
	var out = [], kept = []
	var makers = [function (j) { return { x: j, a: 1 } }, function (j) { return { x: j, b: "s" + j } },
		function (j) { return { x: j, c: { d: [j] } } }, function (j) { return { x: j, e: null, f: undefined, g: 1.5 } }]
	function many(j) { return { k0: j, k1: 1, k2: 2, k3: 3, k4: 4, k5: 5, k6: 6, k7: 7, k8: 8, k9: 9, k10: "t" } }
	for (var j = 0; j < 200; j++) {
		var o = makers[j & 3](j), m = many(j), a = [], b = []
		a.push(j); b[0] = o; b[1] = m
		if (j % 37 == 0) kept.push(o, a, b)
		out.push(o.x + (o.a || 0) + (o.b ? o.b.length : 0) + (o.c ? o.c.d[0] : 0) + m.k0 + m.k9 + a.length + b.length)
	}
	out.push(kept.map(function (v) { return JSON.stringify(v) }).join(";"))
	out.join()`

// TestRegNativeLiterals makes object literals and empty arrays natively,
// by nativeLit with no exit, against the Go table, marking and not; its
// fast case makes both with no exit.
func TestRegNativeLiterals(t *testing.T) {
	nativeCheck(t, nativeLitSrc, `
		var s = 0
		for (var j = 0; j < 1000; j++) { var o = { x: j, y: 2 }, a = []; s += o.x + o.y + a.length }
		String(s)`, bytecode.RNewObjectLit, bytecode.RNewArrayE)
}

// TestRegNativeLiteralsCollecting makes them while the collector runs
// all along (GOGC=1).
func TestRegNativeLiteralsCollecting(t *testing.T) {
	gc := debug.SetGCPercent(1)
	t.Cleanup(func() { debug.SetGCPercent(gc) })
	want := nativeRunSrc(t, nativeLitSrc, false)
	for range 3 {
		if got := nativeRunSrc(t, nativeLitSrc, true); got != want {
			t.Fatalf("native:\n%s\nwant\n%s", got, want)
		}
	}
}

// TestRegNativeTypeofJumps runs typeof's compares and branches natively,
// against the Go table, marking and not: every type's value against
// every type's name, both ways, functions of each kind, a proxy and a
// revoked one; and a template's parts and ToNumeric, which a string or a
// number passes and anything else converts in Go. Its fast case jumps by
// typeof, makes a template's parts and steps a property with no exit.
func TestRegNativeTypeofJumps(t *testing.T) {
	nativeCheck(t, `
		var vals = [1, -0, NaN, 1.5, "s", "", true, false, null, undefined, {}, [], function () {}, () => 1,
			class {}, async function () {}, function* () {}, Symbol("y"), 10n, new Proxy({}, {}), new Proxy(function () {}, {}),
			Object(1), new Date(0), /x/]
		var out = []
		function kinds(v) {
			var s = ""
			if (typeof v == "undefined") s += "u"; if (typeof v != "undefined") s += "U"
			if (typeof v === "object") s += "o"; if (typeof v !== "object") s += "O"
			if (typeof v == "boolean") s += "b"; if (typeof v == "number") s += "n"
			if (typeof v == "string") s += "s"; if (typeof v == "symbol") s += "y"
			if (typeof v == "bigint") s += "g"; if (typeof v === "function") s += "f"
			if (typeof v != "function") s += "F"
			return s
		}
		for (var round = 0; round < 3; round++) out.push(vals.map(kinds).join(" "))
		var r = Proxy.revocable({}, {}); r.revoke(); out.push(kinds(r.proxy))
		function tpl(a, b) { return a + "<" + b + ">" + `+"`${a}|${b}`"+` }
		var parts = [[1, "x"], ["a", 2.5], [null, undefined], [true, {}], [[1, 2], { toString() { return "t" } }], [10n, Symbol.iterator.description]]
		for (var p of parts) out.push(tpl(p[0], p[1]))
		var o = { i: 0, s: "5", b: 2n, x: null }
		for (var k = 0; k < 3; k++) { o.i++; o.s++; o.b++; o.x++ }
		out.push(o.i, o.s, o.b, o.x)
		out.join("|")`, `
		function f(v) { return (typeof v == "number" ? 1 : 0) + (typeof v === "string" ? 2 : 0) + (typeof v != "object" ? 4 : 0) }
		function g(a, b) { return `+"`${a}-${b}`"+` }
		var o = { n: 0 }, s = 0, vals = [1, "x", null, 2.5]
		for (var i = 0; i < 1000; i++) { s += f(vals[i & 3]); o.n++; s += g(i, "y").length }
		String(s + o.n)`, bytecode.RJTypeof, bytecode.RStrPart, bytecode.RUnary)
}

// TestRegNativeInIndex runs in of number keys natively, against the Go
// table, marking and not: arrays with holes, elements up the chain, an
// array whose prototype is one, ordinary objects of elements and of index
// keys, typed arrays, arguments, strings' wrappers, and keys that are no
// index -- negative, fractional, -0, NaN, past 2^31 and past an int64;
// its fast case asks of an array's indices with no exit.
func TestRegNativeInIndex(t *testing.T) {
	nativeCheck(t, `
		var keys = [0, 1, 2, 3, 4, 7, 12, -1, 1.5, -0, NaN, 2147483647, 2147483648, 4294967295, 1e300, Infinity]
		var holes = [1, 2, , 4], proto = [9, 9, 9, 9, 9, 9, 9, 9], sub = Object.create(proto)
		sub[1] = 1
		var obj = { 0: "a", 3: "b" }, plain = { x: 1 }, chained = Object.create([1, , 3])
		var big = []; big[2147483647] = 1
		var targets = [holes, proto, sub, obj, plain, chained, big, new Int8Array(3), new String("abc"), [], (function () { return arguments })(1, 2)]
		var out = []
		function has(k, o) { return k in o }
		for (var round = 0; round < 3; round++) {
			if (round == 2) { holes[2] = 3; Array.prototype[7] = 1 }
			out.push(targets.map(function (o) { return keys.map(function (k) { return has(k, o) ? 1 : 0 }).join("") }).join(" "))
		}
		delete Array.prototype[7]
		out.join("|")`, `
		var a = [1, 2, 3, , 5, 6, 7, 8], s = 0
		for (var j = 0; j < 1000; j++) { if ((j & 7) in a) s++; if (12 in a) s++ }
		String(s)`, bytecode.RIn)
}

// TestRegNativeForIn runs for-in's steps natively -- an array's index
// keys, walked by index, and an object's snapshot of keys while it keeps
// its shape -- and the element read by a key string the loop handed out
// (keyIndex): holes, elements deleted and added while the loop runs,
// indices past the strings the runtime keeps, keys of the prototype
// chain, an object whose keys were deleted, keys read from another
// array, and the same strings read as keys after the loop.
func TestRegNativeForIn(t *testing.T) {
	// The first loop over an array makes its keys' strings, a step's exit
	// each (keyString), which later loops hand out natively. A key is read
	// by its text (keyIndex), wherever it lies, so that a read long after
	// its step, or of a snapshot's key, exits alike in every run.
	was := nativeWarmExits
	nativeWarmExits += 8
	defer func() { nativeWarmExits = was }()
	nativeCheck(t, `
		var out = []
		function sum(a) { var s = "", k; for (k in a) s += k + ":" + a[k] + ","; return s }
		function keys(a) { var s = "", k; for (k in a) s += k + ","; return s }
		var holes = [1, , 3, , 5], gone = [1, 2, 3]; delete gone[2]
		out.push(sum([1, 2, 3]), keys(holes), keys(gone), sum(holes), sum([]))
		var big = []; for (var i = 0; i < 1100; i++) big[i] = i
		var t = 0; for (var k in big) t += big[k]; out.push(t)
		var del = [1, 2, 3, 4, 5], s = ""; for (var k in del) { s += k; if (k == 1) { del.length = 3; delete del[0] } } out.push(s)
		var add = [1, 2], s2 = ""; for (var k in add) { s2 += k; add.push(9) } out.push(s2, add.length)
		var other = [10, 20, 30], s3 = 0; for (var k in [1, 2, 3]) s3 += other[k]; out.push(s3)
		function P() { this.a = 1; this.b = 2 } P.prototype.c = 3
		var o = new P(), s4 = ""; for (var k in o) s4 += k + o[k]; out.push(s4)
		var d = { x: 1, y: 2, z: 3 }, s5 = ""; for (var k in d) { s5 += k; delete d.z } out.push(s5)
		var mixed = [1, 2]; mixed.foo = 7; var s6 = ""; for (var k in mixed) s6 += k + mixed[k]; out.push(s6)
		var later = []; for (var k in [5, 6, 7]) later.push(k); out.push(later.map(function (k) { return [9, 8, 7][k] }).join())
		var str = "abc", s7 = ""; for (var k in str) s7 += str[k]; out.push(s7)
		var args = (function () { var r = ""; for (var k in arguments) r += arguments[k]; return r })(4, 5, 6); out.push(args)
		out.join("\n")`, `
		var r = []; for (var i = 0; i < 8; i++) r[i] = i
		var objs = []; for (var j = 0; j < 4; j++) objs.push({ a: j, b: 2, c: 3, d: 4 })
		var sum = 0, c = 0
		for (var n = 0; n < 200; n++) {
			for (var k in r) sum += r[k]
			for (var k2 in objs[n & 3]) c++
		}
		String(sum) + " " + String(c)`, bytecode.RIterNext, bytecode.RGetIndex)
}

// TestRegNativeForInKeysAnyHeap runs reads by a for-in's keys natively in
// heaps laid out differently -- strings allocated before each run, a
// collection between runs, as while it marks and not -- and fails where
// get_index's exits differ: which key strings it reads by depends on their
// text alone (keyIndex), never on where they were allocated, which the
// runtime's keyAtoms, by address, would make it.
func TestRegNativeForInKeysAnyHeap(t *testing.T) {
	nativeCounting(t)
	src := `
		var out = [], a = [], o = { x: 1, y: 2 }
		for (var i = 0; i < 300; i++) a[i] = i
		var s = 0
		for (var r = 0; r < 4; r++) {
			for (var k in a) s += a[k]
			for (var k2 in o) s += o[k2]
			var keys = []; for (var k3 in [7, 8, 9]) keys.push(k3)
			for (var k4 of keys) s += [10, 20, 30][k4]
			s += a["12"] + a["0"] + (a["012"] === undefined ? 1 : 0) + (a["1e1"] === undefined ? 1 : 0)
		}
		String(s)`
	want := nativeRunSrc(t, src, false)
	var keep [][]string
	exits := map[int64]int{}
	for i := 0; i < 8; i++ {
		filler := make([]string, 0, i*500)
		for j := 0; j < i*500; j++ {
			filler = append(filler, strings.Repeat("x", 1+j%7))
		}
		keep = append(keep, filler)
		runtime.GC()
		nativeMarking(t, i%2 == 1)
		before := nativeExitOps()
		if got := nativeRunSrc(t, src, true); got != want {
			t.Fatalf("run %d: %s, want %s", i, got, want)
		}
		after := nativeExitOps()
		exits[after[bytecode.RGetIndex]-before[bytecode.RGetIndex]]++
	}
	nativeMarking(t, false)
	runtime.KeepAlive(keep)
	if len(exits) != 1 {
		t.Errorf("get_index's exits differ from heap to heap: %v (count: runs)", exits)
	}
}

// TestRegNativeGenNext runs a generator's next() natively (genNext): the
// call handler steps the generator as for-of's iter_next does, and the
// step's yield and return make next's result: first steps, values sent
// back nowhere, returns with and without a value, a throw out of the
// body and one caught in it, next() of a done generator and of a running
// one, an argument sent (Go's), captures open on the frame (Go's), a
// yield* that hands out its delegate's results, the call's register kept
// where the step throws, and generators stepping generators.
func TestRegNativeGenNext(t *testing.T) {
	nativeCheck(t, `
		var out = []
		function* count(n) { for (var i = 0; i < n; i++) yield i * 3; return "end" }
		function drain(g) { var s = "", r; for (r = g.next(); !r.done; r = g.next()) s += r.value + ","; return s + r.value + r.done }
		out.push(drain(count(5)), drain(count(0)))
		function* bare() { yield 1 }
		var b = bare(); out.push(JSON.stringify([b.next(), b.next(), b.next()]))
		function* thrower() { yield 1; throw new RangeError("t") }
		var r = "kept", th = thrower(); r = th.next(); try { r = th.next() } catch (e) { out.push(e.message, JSON.stringify(r)) }
		function* catcher() { try { yield 1; null.x } catch (e) { yield e.constructor.name } }
		out.push(drain(catcher()))
		function* sent() { var x = yield 1; yield x * 2 }
		var s = sent(); out.push(s.next().value, s.next(21).value)
		function* caps() { for (var i = 0; i < 3; i++) { var f = function () { return i }; yield f } }
		var fs = [], c = caps(); for (var v = c.next(); !v.done; v = c.next()) fs.push(v.value()); out.push(fs.join())
		function* deleg() { yield* count(3); yield* [7, 8] }
		out.push(drain(deleg()))
		var self; function* running() { try { self.next() } catch (e) { yield e.constructor.name } }
		self = running(); out.push(self.next().value)
		function* outer() { var g = count(3), r; while (!(r = g.next()).done) yield r.value + 100 }
		out.push(drain(outer()))
		out.join("\n")`, `
		function* count(n) { for (var i = 0; i < n; i++) yield i }
		var sum = 0
		for (var j = 0; j < 100; j++) {
			var g = count(20)
			for (var r = g.next(); !r.done; r = g.next()) sum += r.value
		}
		String(sum)`, bytecode.RCallMW, bytecode.RYield, bytecode.RRet, bytecode.RRetUndef)
	// A generator Go steps -- a spread's, Array.from's, a destructuring's
	// -- runs as the base frame of a code entry of its own, whose yield
	// the code makes (retDone 2) and whose first run's gen_entry it runs.
	nativeCheck(t, `
		var out = []
		function* count(n) { for (var i = 0; i < n; i++) yield i; return "r" }
		function* caps(n) { for (let i = 0; i < n; i++) yield () => i }
		function* inWith(o) { with (o) { yield a; yield b } }
		out.push([...count(5)].join(), Array.from(count(3)).join(), [...caps(3)].map(f => f()).join())
		var [x, y, ...z] = count(6); out.push(x, y, z.join())
		out.push([...inWith({ a: 1, b: 2 })].join())
		function* thrower() { yield 1; throw new TypeError("t") }
		try { [...thrower()] } catch (e) { out.push(e.message) }
		out.join("\n")`, `
		function* count(n) { for (var i = 0; i < n; i++) yield i }
		var sum = 0
		for (var j = 0; j < 100; j++) sum += [...count(20)].length
		String(sum)`, bytecode.RYield, bytecode.RGenEntry)
}

// TestRegDirectCallbacks runs built-ins' callbacks of pure bodies longer
// than a register call answers without a frame (pureByTree), which Go
// enters with one (regDirect): comparators of BigInts, of strings and of
// mixed values, ones that call an object's valueOf or a getter, and a
// callback that reads an upvalue.
func TestRegDirectCallbacks(t *testing.T) {
	nativeCheck(t, `
		var out = []
		var cmp = (a, b) => (a < b ? -1 : a > b ? 1 : 0)
		var big = new BigInt64Array([5n, -3n, 2n ** 62n, 0n, -(2n ** 63n), 7n])
		for (var i = 0; i < 3; i++) out.push(big.slice().sort(cmp).join())
		out.push(["b", "a", "c", "aa"].sort(cmp).join(), [3, "2", 1, "10"].sort(cmp).join())
		var o = { valueOf() { return 2 } }, objs = [3, o, 1]
		out.push(objs.sort(cmp).map(String).join())
		var lim = 4, pick = (x) => (x < lim ? x * 2 : x > lim * 2 ? -x : 0)
		out.push([1, 5, 9, 12].map(pick).join())
		var g = { get v() { return 1 } }, longp = (x) => (x.v < 0 ? -1 : x.v > 0 ? 1 : 0)
		out.push([g, { v: -2 }, { v: 0 }].map(longp).join())
		out.join("\n")`, "")
}

// TestRegNativePolyCalls runs call sites that call several functions in
// turn: the exit makes the site's cache the new callee's and the call is
// made again natively (nativeCallMiss) where the handler enters it with a
// frame -- plain functions, methods of several layouts, in tail position
// too, arrows with their this, sloppy methods of a primitive receiver --
// and Go makes the rest:
// leaves, functions that keep their arguments, generators, bound
// functions, built-ins and class constructors, which throw.
func TestRegNativePolyCalls(t *testing.T) {
	nativeCheck(t, `
		var out = []
		var fs = [
			function (j) { return { x: j, a: 1 } }, function (j) { var o = { x: j }; o.b = 2; return o },
			(j) => ({ x: this === undefined ? -j : j }), function (j) { return arguments.length + j },
			function* (j) { yield j }, function (j) { return j }, Math.abs,
			function (j) { "use strict"; return { x: typeof this } }.bind(null)
		]
		for (var i = 0; i < 64; i++) {
			var r = fs[i & 7](i)
			out.push(typeof r === "object" && r !== null ? (r.x === undefined ? r.next().value : r.x) : r)
		}
		var ms = [{ m() { return this.k + 1 }, k: 1 }, { k: "s", m() { return this.k + 2 } }, { m: function () { return typeof this } }]
		for (var i = 0; i < 30; i++) out.push(ms[i % 3].m())
		// Methods of several templates called in tail position, whose
		// this the cache made again must keep.
		var ts = [{ k: 1, m(n) { return this.k + n } }, { k: 2, m: function (n) { return this.k * n } }, { k: 3, m(n) { "use strict"; return this.k - n } }]
		function tail(o, n) { "use strict"; return o.m(n) }
		for (var i = 0; i < 30; i++) out.push(tail(ts[i % 3], i))
		class Sched { constructor() { this.n = 0 } bump(x) { this.n += x; return this } hold() { return this.n }
			task() { return (w) => w === 0 ? this.hold() : this.bump(w) } }
		var sc = new Sched(), tk = sc.task(), ss = 0
		for (var i = 0; i < 200; i++) { var rr = tk(i & 3); ss += typeof rr === "number" ? rr : rr.n }
		out.push(ss)
		String.prototype.sm = function () { return typeof this }
		Number.prototype.sm = function () { return this + 1 }
		var prims = ["a", 5]
		for (var i = 0; i < 10; i++) out.push(prims[i & 1].sm())
		class C {}
		var cs = [function () { return 1 }, C]
		for (var i = 0; i < 4; i++) { try { out.push(cs[i & 1]()) } catch (e) { out.push(e.constructor.name) } }
		out.join()`, "")
}

// TestRegCallbackSort runs sorts' comparators kept ready from one call to
// the next (regCallback), natively and by the Go table: comparators of
// BigInts, of mixed values, with a this (sloppy, strict, an arrow's),
// ones that make closures of their bindings each call, that call in tail
// position past tailCallDepth, that sort in turn, that throw, whose result
// converts by a valueOf that throws or reads a stack trace, and ones a
// callback is not kept for: leaves, and bodies that keep their arguments.
func TestRegCallbackSort(t *testing.T) {
	nativeCheck(t, `
		var out = []
		var big = new BigInt64Array([5n, -3n, 2n ** 62n, 0n, -(2n ** 63n), 7n])
		var cmp = (a, b) => (a < b ? -1 : a > b ? 1 : 0)
		out.push(big.slice().sort(cmp).join(), [3, "2", 1, "10", 2].sort(cmp).join())
		function sloppy(a, b) { out.length; return typeof this === "object" ? a - b : 0 }
		function strict(a, b) { "use strict"; return this === undefined ? b - a : 0 }
		var obj = { k: 1, sort(a) { return a.sort((x, y) => (x - y) * this.k) } }
		out.push([3, 1, 2].sort(sloppy).join(), [3, 1, 2].sort(strict).join(), obj.sort([9, 7, 8]).join())
		var made = []
		out.push([4, 2, 3, 1].sort(function (a, b) { let d = a - b; made.push(() => d); return d }).join(), made.map(f => f()).join())
		function down(n, d) { "use strict"; return n === 0 ? d : down(n - 1, d) }
		out.push([5, 3, 4, 1, 2].sort((a, b) => down(100, a - b)).join())
		out.push([[3, 1], [2, 2], [1, 3]].sort((a, b) => a.slice().sort((x, y) => x - y)[0] - b.slice().sort((x, y) => x - y)[0] || a[0] - b[0]).join("|"))
		try { [1, 2, 3].sort((a, b) => { if (a === 2) throw new RangeError("c" + a + b); return a - b }) } catch (e) { out.push(e.message) }
		var where = ""
		try { [1, 2].sort(() => ({ valueOf() { where = new Error("s").stack.split("\n").length > 0 ? "ok" : ""; throw new TypeError("v") } })) } catch (e) { out.push(e.message, where) }
		var st = []
		function traced(a, b) { if (st.length === 0) st.push(new Error("t").stack.split("\n")[1].trim().split(" ")[1]); return a - b }
		out.push([3, 2, 1].sort(traced).join(), st[0])
		out.push([2, 1].sort(function () { return arguments[0] - arguments[1] }).join(), [2, 1, 3].sort((a, b) => b - a).join())
		var u8 = new Uint8Array([9, 4, 6]); out.push(u8.sort((a, b) => { let c = a - b; return c }).join())
		out.join("\n")`, "")
	// The iteration methods' callbacks, kept ready the same way: closures
	// of each call's bindings, a callback that iterates in turn, a throw
	// part way, a this of each kind, typed arrays', and leaves the
	// frameless run has given up on.
	nativeCheck(t, `
		var out = [], fs = []
		;[1, 2, 3].forEach(function (x, i) { let y = x * 10; fs.push(() => y + i) })
		out.push(fs.map(f => f()).join())
		out.push([1, 2, 3].map(function (x) { return [x, x + 1].map(function (y) { return y * this.k }, { k: x }).join("-") }).join())
		try { [1, 2, 3, 4].forEach(function (x) { if (x === 3) throw new Error("at " + x); out.push(x) }) } catch (e) { out.push(e.message) }
		var t = 0; [1, 2, 3].forEach(function (x) { t += x * (this === undefined ? 1 : 2) }); out.push(t)
		out.push([5, 6].filter(function (x) { "use strict"; return this === undefined && x > 5 }).join())
		out.push([1, 2, 3, 4].reduce(function (a, x) { let s = a + x; return s }, 0), [4, 5, 6].find(function (x) { var k = x % 5; return k === 0 }))
		out.push(new Int32Array([3, 1, 2]).map(function (x) { let d = x * 2; return d }).join(), new BigInt64Array([2n, 3n]).reduce(function (a, x) { return a + x * x }, 0n))
		out.push([1, 2, 3].some(function (x) { var r = x > 2; return r }), [1, 2, 3].every(function (x) { var r = x > 0; return r }))
		// Leaves of BigInts, whose frameless run gives up after its misses
		// and which are then kept as frames.
		var bigs = new BigInt64Array(100).map((_, i) => BigInt(i) - 50n), tot = 0n, s = 0n
		for (var r = 0; r < 3; r++) { tot += bigs.reduce((a, x) => a + x, 7n); bigs.forEach((x) => { s += x * x }) }
		out.push(tot, s)
		out.join("\n")`, "")
}
