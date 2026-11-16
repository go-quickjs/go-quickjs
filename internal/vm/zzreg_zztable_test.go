package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
)

// regTableRunSrc runs src as register code, by the Go table's loop or,
// where native is set and the architecture has them, by the native
// handlers, and returns its value as a string.
func regTableRunSrc(t *testing.T, src string, native bool) string {
	t.Helper()
	compiler.SetRegisterForm(true)
	fn := compileForTest(t, src)
	compiler.SetRegisterForm(bytecode.RegDefault())
	SetRegTier(true)
	was := SetRegNative(native)
	defer func() { SetRegTier(bytecode.RegDefault()); SetRegNative(was) }()
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(fn)
	if err != nil {
		t.Fatalf("native %v: %v", native, err)
	}
	return v.String().Go()
}

// TestRegTableFastPaths runs what the Go table's fast paths of
// zzreg_zztable.go answer -- bin's division and int32 remainder, typed
// arrays' elements stored at a constant index (numbers, and BigInts held
// in their Values) and at a register's, in by the realm's cache and by an array's elements,
// private members by their sites, a string's and a number's methods by
// the realm's cache, an array's length written -- and what they leave to
// the slow paths, each a few times so that the caches fill, by the Go
// table and natively, against the answers the language gives.
func TestRegTableFastPaths(t *testing.T) {
	for _, c := range []struct{ name, src, want string }{
		{"bin", `
			function show(x) { return Object.is(x, -0) ? "-0" : String(x) }
			function div(a, b) { return a / b }
			function mod(a, b) { return a % b }
			var out = []
			for (var round = 0; round < 3; round++) {
				var s = []
				for (var p of [[7, 2], [-7, 2], [1, 0], [-1, 0], [1, -0], [0, 0], [6, "3"], [null, 4]]) s.push(show(div(p[0], p[1])))
				for (var p of [[7, 3], [-7, 3], [7, -3], [2147483647, 10], [5.5, 2], [-0, 5], [0, 5], [5, 0],
					[2147483648, 7], [13, 13], [1, -2147483648], [7, "4"], [NaN, 2], [Infinity, 3], [3, Infinity]]) s.push(show(mod(p[0], p[1])))
				out.push(s.join())
			}
			out.join("|")`,
			"3.5,-3.5,Infinity,-Infinity,-Infinity,NaN,2,0,1,-1,1,7,1.5,-0,0,NaN,2,0,1,3,NaN,NaN,3|" +
				"3.5,-3.5,Infinity,-Infinity,-Infinity,NaN,2,0,1,-1,1,7,1.5,-0,0,NaN,2,0,1,3,NaN,NaN,3|" +
				"3.5,-3.5,Infinity,-Infinity,-Infinity,NaN,2,0,1,-1,1,7,1.5,-0,0,NaN,2,0,1,3,NaN,NaN,3"},
		{"typed", `
			var out = []
			for (var round = 0; round < 3; round++) {
				var i8 = new Int8Array(4), u8 = new Uint8Array(2), i16 = new Int16Array(1), u32 = new Uint32Array(1)
				var i32 = new Int32Array(1), f32 = new Float32Array(1), f64 = new Float64Array(1), c = new Uint8ClampedArray(4)
				var b = new BigInt64Array(3), u = new BigUint64Array(2)
				i8[0] = 200; i8[1] = -129; i8[2] = 1.9; i8[3] = NaN
				u8[0] = 300; u8[1] = -1
				i16[0] = 70000; u32[0] = -1; i32[0] = 4294967301; f32[0] = 1.1; f64[0] = -0
				c[0] = 1.5; c[1] = 2.5; c[2] = 300; c[3] = -1
				b[0] = 2n ** 63n; b[1] = -5n; b[2] = 2n ** 64n + 7n
				u[0] = -1n; u[1] = 2n ** 63n
				u8[5] = 9
				out.push([...i8, ...u8, i16[0], u32[0], i32[0], f32[0], Object.is(f64[0], -0), ...c, ...b, ...u, u8.length, u8[5]].join())
			}
			out.join("|")`,
			"-56,127,1,0,44,255,4464,4294967295,5,1.100000023841858,true,2,2,255,0,-9223372036854775808,-5,7,18446744073709551615,9223372036854775808,2,|" +
				"-56,127,1,0,44,255,4464,4294967295,5,1.100000023841858,true,2,2,255,0,-9223372036854775808,-5,7,18446744073709551615,9223372036854775808,2,|" +
				"-56,127,1,0,44,255,4464,4294967295,5,1.100000023841858,true,2,2,255,0,-9223372036854775808,-5,7,18446744073709551615,9223372036854775808,2,"},
		{"typed index", `
			var out = []
			for (var round = 0; round < 3; round++) {
				var i8 = new Int8Array(4), f32 = new Float32Array(2), c = new Uint8ClampedArray(2), a = [1, , 3]
				var vs = [200, -129, 1.9, NaN]
				for (var k = 0; k < 4; k++) i8[k] = vs[k]
				for (var k = 0; k < 2; k++) f32[k] = 1.1 * (k + 1)
				for (var k = 0; k < 2; k++) c[k] = 2.5 * k + 1.5
				for (var k = 0; k < 3; k++) a[k] = k * 10
				i8[7] = 5; i8["1"] = 3; i8[1.5] = 9
				out.push([...i8, ...f32, ...c, ...a, i8.length, i8[7], i8[1.5]].join())
			}
			out.join("|")`,
			"-56,3,1,0,1.100000023841858,2.200000047683716,2,4,0,10,20,4,,|" +
				"-56,3,1,0,1.100000023841858,2.200000047683716,2,4,0,10,20,4,,|" +
				"-56,3,1,0,1.100000023841858,2.200000047683716,2,4,0,10,20,4,,"},
		{"in", `
			var proto = { inh: 1 }, o = Object.create(proto), a = [1, 2, , 4], out = []
			o.a = 1
			function has(k, x) { return k in x }
			for (var round = 0; round < 4; round++) {
				if (round == 2) { delete proto.inh; o.z = 1; Array.prototype[2] = "x" }
				var s = []
				for (var k of ["a", "z", "inh", "toString"]) s.push(has(k, o))
				for (var i of [0, 2, 3, 9]) s.push(has(i, a))
				out.push(s.join())
			}
			delete Array.prototype[2]
			try { has("a", 1) } catch (e) { out.push(e.constructor.name) }
			out.join("|")`,
			"true,false,true,true,true,false,true,false|true,false,true,true,true,false,true,false|" +
				"true,true,false,true,true,true,true,false|true,true,false,true,true,true,true,false|TypeError"},
		{"private", `
			class P { #x = 1; static get(o) { return o.#x } static set(o, v) { o.#x = v } }
			class Q { #x = "q"; static get(o) { return o.#x } }
			var p = new P(), q = new Q(), f = Object.freeze(new P()), out = []
			for (var i = 0; i < 4; i++) {
				P.set(p, i); P.set(f, i * 10)
				out.push(P.get(p), P.get(f), Q.get(q))
			}
			try { P.get(q) } catch (e) { out.push(e.constructor.name) }
			try { P.set({}, 1) } catch (e) { out.push(e.constructor.name) }
			out.join()`,
			"0,0,q,1,10,q,2,20,q,3,30,q,TypeError,TypeError"},
		{"length", `
			function lit() { return [1, 2, 3, 4, 5] }
			function setLen(a, n) { a.length = n; return a.length }
			function strictLen(a, n) { "use strict"; a.length = n }
			var out = []
			for (var round = 0; round < 3; round++) {
				var a = lit(), f = Object.freeze([1, 2]), s = []
				s[100] = 1
				out.push(setLen(a, 3), a.join(), setLen(a, 5), a.join(), 3 in a, setLen(f, 0), setLen(s, 50), s[100])
				try { setLen(a, 1.5) } catch (e) { out.push(e.constructor.name) }
				try { strictLen(f, 0) } catch (e) { out.push(e.constructor.name) }
				out.push(lit().join())
			}
			out.join()`,
			"3,1,2,3,5,1,2,3,,,false,2,50,,RangeError,TypeError,1,2,3,4,5," +
				"3,1,2,3,5,1,2,3,,,false,2,50,,RangeError,TypeError,1,2,3,4,5," +
				"3,1,2,3,5,1,2,3,,,false,2,50,,RangeError,TypeError,1,2,3,4,5"},
		{"methods", `
			var s = "hello", out = []
			for (var i = 0; i < 3; i++) out.push(s.indexOf("l"), (255).toString(16), (2.5).toFixed(2))
			String.prototype.indexOf = function () { return 42 }
			Number.prototype.toString = function () { return "n" }
			for (var i = 0; i < 2; i++) out.push(s.indexOf("l"), (255).toString(16))
			out.join()`,
			"2,ff,2.50,2,ff,2.50,2,ff,2.50,42,n,42,n"},
	} {
		for _, native := range []bool{false, true} {
			if got := regTableRunSrc(t, c.src, native); got != c.want {
				t.Errorf("%s (native %v):\ngot  %s\nwant %s", c.name, native, got, c.want)
			}
		}
	}
}

// TestRegStringImmOperand runs a string as the operand of a bitwise or an
// arithmetic operator with an immediate (regBinImm), which converts it to
// its number first -- but an addition's, which is a concatenation -- by
// the Go table and natively.
func TestRegStringImmOperand(t *testing.T) {
	src := `
		var ss = ["12345", "  7  ", "0x1F", "1e3", "", "abc", "-0", "4294967297", "-2147483649"], out = []
		function or0(s) { return s | 0 }
		function xor1(s) { return s ^ 1 }
		function ushr0(s) { return s >>> 0 }
		function sub1(s) { return s - 1 }
		function add1(s) { return s + 1 }
		for (var round = 0; round < 2; round++)
			for (var f of [or0, xor1, ushr0, sub1, add1]) out.push(ss.map(f).join())
		out.join("|")`
	one := "12345,7,31,1000,0,0,0,1,2147483647|12344,6,30,1001,1,1,1,0,2147483646|" +
		"12345,7,31,1000,0,0,0,1,2147483647|12344,6,30,999,-1,NaN,-1,4294967296,-2147483650|" +
		"123451,  7  1,0x1F1,1e31,1,abc1,-01,42949672971,-21474836491"
	want := one + "|" + one
	for _, native := range []bool{false, true} {
		if got := regTableRunSrc(t, src, native); got != want {
			t.Errorf("native %v:\ngot  %s\nwant %s", native, got, want)
		}
	}
}
