package compiler

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

// TestUndefinedPushed pins a read of undefined that nothing can shadow, and
// void of a literal, compiled as load_undef alone, and one that something may as the read of the
// global it is: a parameter or a local of that name, a with body, a sloppy
// function with a direct eval and a function inside one.
func TestUndefinedPushed(t *testing.T) {
	const pushed, local, global = 0, 1, 2
	for _, c := range []struct {
		src  string
		want int
	}{
		{`function f(x) { return x === undefined; }`, pushed},
		{`function f(x) { return x === void 0 ? void "a" : void 1n; }`, pushed},
		{`function f() { return function () { return undefined; }; }`, pushed},
		{`function f(undefined) { return undefined; }`, local},
		{`function f() { var undefined = 1; return undefined; }`, local},
		{`function f(o) { with (o) { return undefined; } }`, global},
		{`function f(s) { eval(s); return undefined; }`, global},
		{`function f(s) { eval(s); return function () { return undefined; }; }`, global},
	} {
		prog, err := parser.Parse(c.src, parser.Options{})
		if err != nil {
			t.Fatal(err)
		}
		main, err := Compile(prog, Options{})
		if err != nil {
			t.Fatal(err)
		}
		globals, pushes := 0, 0
		var walk func(fn *bytecode.Function)
		walk = func(fn *bytecode.Function) {
			rc := fn.RegCode()
			names := rc.Names
			if names == nil {
				names = fn.Names
			}
			for _, w := range rc.Code {
				switch {
				case w.Op() == bytecode.RGetGlobal && names[w.B()] == "undefined":
					globals++
				case w.Op() == bytecode.RLoadUndef:
					pushes++
				}
			}
			for _, k := range fn.Constants {
				if k.Kind == bytecode.ConstFunction {
					walk(k.Fn)
				}
			}
		}
		for _, k := range main.Constants {
			if k.Kind == bytecode.ConstFunction {
				walk(k.Fn)
			}
		}
		switch {
		case c.want == pushed && (globals != 0 || pushes == 0):
			t.Errorf("%s: %d reads of the global undefined, %d load_undef, want it loaded", c.src, globals, pushes)
		case c.want == local && globals != 0:
			t.Errorf("%s: %d reads of the global undefined, want the binding's", c.src, globals)
		case c.want == global && globals == 0:
			t.Errorf("%s: undefined not read from the global", c.src)
		}
	}
}
