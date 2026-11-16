package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/compiler"
)

// TestRegBackendRuns is a program compiled by the register backend from
// the AST (regcomp), its functions run as register code: the stack
// interpreter's answers, and register code built for them.
func TestRegBackendRuns(t *testing.T) {
	const src = `
		var g = 0;
		function fib(k) { return k < 2 ? k : fib(k - 1) + fib(k - 2) }
		function locals(n) { var a = 1, b = 2, c = 3, d = 4; for (var j = 0; j < n; j++) { a = b; b = c; c = d; d = a } return a + b * 10 + c * 100 + d * 1000 }
		function props(n) { var o = { a: 1, b: 2 }; for (var j = 0; j < n; j++) { o.a = j; o.b = o.a + 1; o.a += 2 } return o.a + o.b }
		function elems(n) { var t = []; for (var i = 0; i < 10; i++) t[i] = i; for (var j = 0; j < n; j++) { t[0] = j; t[1] = t[0] * 2 } return t.join() }
		function globals(n) { for (var j = 0; j < n; j++) g = g + j; return g }
		function P(x) { this.x = x }
		P.prototype.sum = function (k) { var s = 0; for (var j = 0; j < k; j++) s += this.same(this.x); return s };
		P.prototype.same = function (k) { return k };
		function method(n) { var o = new P(3), s = 0; for (var j = 0; j < n; j++) s += o.sum(2); return s }
		function reduce(n) { var r = [1, 2, 3], calls = 0, s = 0; for (var j = 0; j < n; j++) s += r.reduce((a, x) => { calls++; return a + x }, 0); return s * 1000 + calls }
		function cmp(a, b) { return [a == b, a != null, a === b, a < b, b >= a, a == null].join() }
		function post(n) { var i = 0, a = []; while (i < n) a[i] = i++; return a.join() + i }
		function dead() { let f = () => v; try { f() } catch (e) { return e instanceof ReferenceError } }
		function extra(a) { var x; return [a, x].join() }
		function ops(o) { var r = [o instanceof P, "x" in o, delete o.x, "x" in o, delete o["y"]]; return r.join() }
		function thrower(k) { if (k > 1) throw new RangeError("k " + k); return k }
		function sw(v) { var out = ""; switch (v) { case 1: out += "a"; case 2: out += "b"; break; case "s": out += "s"; break; default: out += "d" } return out }
		function args(a, b) { arguments[0] = 9; return [a, arguments.length, arguments[2], b].join() }
		function fwd() { return args.apply(null, arguments) }
		function C(a) { var t = a * 2; this.a = a; this.b = t }
		C.prototype.m = function () { return this.a + this.b };
		function L(x, y) { this.x = x; this.y = y }
		function O() { this.x = 1; return { y: 2 } }
		function A() { this.n = arguments.length }
		function upd() { var o = { a: 1, s: "5", t: "5" }, j = 2; o.a += j; o.a++; o.s++; o.t += 1; o.u++; return [o.a, o.s, o.t, o.u].join() }
		function NT() { var a = () => new.target; this.t = a() === NT; this.u = a() }
		function lits() { var x = [1, true, null, undefined, -2, "s"], y = [1, true, null, undefined, -2, "s"]; y[0] = 5; return x.join() + "/" + y.join() }
		function build() { var c = new C(3), l = new L(1, 2), o = new O(), a = new A(1, 2, 3);
			var nt = new NT(), plain = {}; NT.call(plain);
			return [c.m(), c instanceof C, Object.getPrototypeOf(c) === C.prototype, l.x + l.y, o.y, o.x, a.n, nt.t, plain.t, plain.u, lits(), upd()].join() }
		[fib(15), locals(7), props(9), elems(5), globals(10), method(4), reduce(5), cmp(1, "1"), cmp(null, undefined), post(3), dead(),
		 extra(1, 2), ops(new P(1)), (function () { try { thrower(5) } catch (e) { return e.message } })(), thrower(1),
		 sw(1) + sw(2) + sw("s") + sw(7), args(1, 2, 3), fwd(4, 5), build()].join(" ")`
	const want = "610 2432 19 4,8,2,3,4,5,6,7,8,9 45 24 30015 true,true,false,false,true,false true,false,false,false,false,true 0,1,23 true " +
		"1, true,true,true,false,true k 5 1 abbsd 9,3,3,2 9,2,,5 9,true,true,3,2,,3,true,false,,1,true,,,-2,s/5,true,,,-2,s,4,6,51,NaN"
	for _, reg := range []bool{false, true} {
		// Each run its own compile: a function keeps the form its first
		// run gave it.
		compiler.SetRegisterForm(true)
		fn := compileForTest(t, src)
		compiler.SetRegisterForm(false)
		SetRegTier(reg)
		before := RegsBuilt()
		r := New(Config{})
		v, err := r.Run(fn)
		SetRegTier(false)
		if err != nil {
			t.Fatalf("reg=%v: %v", reg, err)
		}
		if got := v.String().Go(); got != want {
			t.Errorf("reg=%v: got %s, want %s", reg, got, want)
		}
		if built := RegsBuilt() - before; reg && built < 15 {
			t.Errorf("only %d functions ran as register code", built)
		}
		r.Close()
	}
}

// TestRegFrameErrors pins a register frame's errors: the error a slow
// path throws, its position in a stack trace through register frames, and
// a binding read in its dead zone.
func TestRegFrameErrors(t *testing.T) {
	const src = `
		function inner(o) { return o.x.y }
		function outer(o) { return inner(o) + 1 }
		function dead() { let f = () => v; f(); let v = 1; return v }
		var out = [];
		try { outer({}) } catch (e) { out.push(e instanceof TypeError, /inner/.test(e.stack), /outer/.test(e.stack)) }
		try { dead() } catch (e) { out.push(e instanceof ReferenceError) }
		out.join()`
	SetRegTier(true)
	r := New(Config{})
	v, err := r.Run(compileForTest(t, src))
	SetRegTier(false)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := v.String().Go(), "true,true,true,true"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
	r.Close()
}
