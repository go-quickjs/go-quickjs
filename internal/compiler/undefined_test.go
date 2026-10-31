package compiler

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

// TestUndefinedPushed pins a read of undefined that nothing can shadow
// compiled as push_undef, and one that something may as the read of the
// global it is: a parameter or a local of that name, a with body, a sloppy
// function with a direct eval and a function inside one.
func TestUndefinedPushed(t *testing.T) {
	const pushed, local, global = 0, 1, 2
	for _, c := range []struct {
		src  string
		want int
	}{
		{`function f(x) { return x === undefined; }`, pushed},
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
		globals, pushes, drops := 0, 0, 0
		var walk func(fn *bytecode.Function)
		walk = func(fn *bytecode.Function) {
			for _, in := range fn.Code {
				switch {
				case in.Op == bytecode.OpGetGlobal && fn.Names[in.A] == "undefined":
					globals++
				case in.Op == bytecode.OpPushUndef:
					pushes++
				case in.Op == bytecode.OpDrop:
					drops++
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
		case c.want == pushed && (globals != 0 || pushes == 0 || drops != 0):
			t.Errorf("%s: %d reads of the global undefined, %d push_undef and %d drops, want it pushed", c.src, globals, pushes, drops)
		case c.want == local && globals != 0:
			t.Errorf("%s: %d reads of the global undefined, want the binding's", c.src, globals)
		case c.want == global && globals == 0:
			t.Errorf("%s: undefined not read from the global", c.src)
		}
	}
}
