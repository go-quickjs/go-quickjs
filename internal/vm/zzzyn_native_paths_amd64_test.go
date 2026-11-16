package vm

import (
	"fmt"
	"runtime"
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
// goroutine's stack: what a wrong g (R14) at a Go handler would crash.
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
// zeroes it by X15, which its internal convention keeps zero.
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
// trampoline write by hand: the handler's four arguments in AX, BX, CX
// and DI, the func value in DX (the handler here is a closure, which
// reads its captures through it), its result in AX, the goroutine in R14
// and X15 zero. A Go handler called through it gets the arguments the Go
// table's loop would give it, runs on the test's goroutine, grows the
// stack, zeroes memory, and what it returns is where the code goes on. A
// toolchain
// whose convention differs fails it, and the native handlers then need
// their trampoline changed.
func TestRegNativeCallHConvention(t *testing.T) {
	op := bytecode.RNewObjectLit
	if nativePlain.has[op] {
		t.Fatalf("%s has a native handler: the test needs an operation that exits", op)
	}
	want := nativeGoid()
	orig := regTable[op]
	defer func() { regTable[op] = orig }()
	var calls, viaCallH int
	var bad []string
	regTable[op] = func(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
		calls++
		pcs := make([]uintptr, 16)
		frames := runtime.CallersFrames(pcs[:runtime.Callers(1, pcs)])
		for {
			f, more := frames.Next()
			if strings.HasSuffix(f.Function, ".nativeCallH") {
				viaCallH++
			}
			if !more {
				break
			}
		}
		switch {
		case st == nil || st.c == nil:
			bad = append(bad, "no state (DI)")
		case regs != st.regs:
			bad = append(bad, "the registers (AX) are not the state's")
		case pc < 0 || pc >= len(st.c.src):
			bad = append(bad, fmt.Sprintf("pc (CX) %d out of the code", pc))
		case regAt(st.c.code, pc) != w || w.Op() != op:
			bad = append(bad, fmt.Sprintf("the instruction (BX) %#x is not the code's at pc %d", uint64(w), pc))
		}
		if g := nativeGoid(); g != want {
			bad = append(bad, fmt.Sprintf("goroutine (R14) %q, want %q", g, want))
		}
		if nativeDeep(2000) != 2000*2001/2 {
			bad = append(bad, "the deep recursion's sum")
		}
		if !nativeZeroed(16) {
			bad = append(bad, "a fresh array not zero (X15)")
		}
		return orig(regs, w, pc, st)
	}
	src := `function f(n) { var s = 0; for (var i = 0; i < n; i++) { var o = {a: i, b: 1}; s += o.a + o.b } return s }
		String(f(200)) + " " + String((function () { return {x: 7} })().x)`
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
