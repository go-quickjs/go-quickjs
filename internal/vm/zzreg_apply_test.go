package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/compiler"
)

// TestRegApplyArgs pins apply_args, f.apply(x, arguments) in a function
// whose arguments object is never made: RayTrace's Class.create
// constructor, called and constructed, with fewer and more arguments than
// it names, a mapped parameter assigned before the call, an apply that is
// not the built-in, and nested forwarding -- the stack interpreter's
// answers, run as register code, the arguments passed on without an
// object.
func TestRegApplyArgs(t *testing.T) {
	const src = `
		var Class = { create: function () { return function () { this.initialize.apply(this, arguments) } } };
		var P = Class.create();
		P.prototype = { initialize: function (x, y, z) { this.s = [x, y, z, arguments.length].join() } };
		function mapped(a, b) { a = 7; return show.apply(null, arguments) + ";" + sum.apply(null, arguments) }
		function show() { return Array.prototype.join.call(arguments) }
		function sum() { var s = 0; for (var i = 0; i < arguments.length; i++) s += arguments[i]; return s }
		function strict(a) { "use strict"; a = 9; return sum.apply(null, arguments) }
		function fwd() { return sum.apply(this, arguments) }
		function fwd2() { return fwd.apply(null, arguments) * 2 }
		var odd = { apply: function (t, a) { return typeof a + ":" + a.length + ":" + a[1] } };
		function other() { return odd.apply(null, arguments) }
		function many() { var r = 0; for (var i = 0; i < 50; i++) r += fwd(i, 1, 2, 3, 4, 5, 6, 7, 8); return r }
		[new P(1, 2, 3).s, new P(1).s, new P(1, 2, 3, 4, 5).s, mapped(1, 2, 3), strict(1, 2), fwd(), fwd(1, 2, 3, 4),
		 fwd2(5, 6), other(4, 5, 6), many()].join(" | ")`
	const want = "1,2,3,3 | 1,,,1 | 1,2,3,5 | 7,2,3;12 | 3 | 0 | 10 | 22 | object:3:5 | 3025"
	for _, reg := range []bool{false, true} {
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
		if built := RegsBuilt() - before; reg && built < 8 {
			t.Errorf("only %d functions ran as register code", built)
		}
		if reg {
			// The forwarding functions' argument stack is given back.
			if n := len(r.argStack); n != 0 {
				t.Errorf("argument stack left at %d", n)
			}
		}
		r.Close()
	}
}
