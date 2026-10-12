package vm

import (
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// TestStringChain pins a + b + c joined in one concat once a string literal
// has made the additions concatenations: the conversions it makes, and
// when, are each addition's, on both tiers.
func TestStringChain(t *testing.T) {
	defer SetTreeTier(true)
	const obj = `var log = []; var o = { valueOf() { log.push('v'); return 7 }, toString() { log.push('s'); return 'S' } }; function g() { log.push('g'); return 'G' }; `
	for _, tt := range []struct {
		name, src, want string
		concat          bool
	}{
		{name: "leaf", src: `function f(tag) { return 'String for key ' + tag + ' in leaf node' } f('k')`, want: "String for key k in leaf node", concat: true},
		{name: "order", src: obj + `function f() { return 'a' + o + g() + 'b' + '|' + log } f()`, want: "a7Gb|v,g", concat: true},
		{name: "prefix", src: obj + `function f() { return o + 'a' + g() + o + '|' + log } f()`, want: "7aG7|v,g,v", concat: true},
		{name: "numericPrefix", src: `function f(a, b) { return a + b + 'x' + a + b } f(1, 2)`, want: "3x12", concat: true},
		{name: "numbers", src: `function f(x) { return 'x' + 1 + 2.5 + x + 1e21 + 'y' } f(-0)`, want: "x12.501e+21y", concat: true},
		{name: "template", src: obj + "function f() { return `t${o}` + o + 'z' + '|' + log } f()", want: "tS7z|s,v", concat: true},
		{name: "empty", src: `function f(a, b) { return '' + a + b } f(1, 2)`, want: "12"},
		{name: "two", src: `function f(a) { return 'x' + a } f(1)`, want: "x1"},
		{name: "noLiteral", src: `function f(a, b, c) { return a + b + c } f(1, 2, '3')`, want: "33"},
		{name: "bigint", src: `function f(a) { return 'n' + a + 'm' } f(10n)`, want: "n10m", concat: true},
		{name: "symbol", src: `var log = ''; function f(a) { try { return 'a' + a + (log += 'b') } catch (e) { return e.name + log } } f(Symbol())`, want: "TypeError", concat: true},
		{name: "symbolAfter", src: `var log = ''; function f(a) { try { return 'a' + (log += 'b') + a } catch (e) { return e.name + log } } f(Symbol())`, want: "TypeErrorb", concat: true},
		{name: "long", src: `function f(x) { return 'a' + x + x + x + x + x + x + x + x + x + x + x + x + x + x + x + x + x + x + x + x + x + x + x + x + x + x + x + x + x + x + x + x + x + x + x + x + x + x + x + x + 'z' } f(1)`, want: "a" + strings.Repeat("1", 40) + "z", concat: true},
		{name: "wrap", src: `function f() { var s = 'x'; for (var i = 0; i < 2000; i++) s = '(' + s + ')'; return s.length + s.slice(1998, 2003) } f()`, want: "4001((x))", concat: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, on := range []bool{false, true} {
				SetTreeTier(on)
				fn := compileForTest(t, tt.src)
				if got := hasOp(fn, "f", bytecode.OpConcat); got != tt.concat {
					t.Errorf("f joins with concat: %v, want %v", got, tt.concat)
				}
				r := New(Config{})
				v, err := r.Run(fn)
				if err != nil {
					r.Close()
					t.Fatalf("tree %v: %v", on, err)
				}
				got, err := r.ToString(v)
				if err != nil || got.Go() != tt.want {
					t.Errorf("tree %v: got %v, %v; want %s", on, got, err, tt.want)
				}
				r.Close()
				if on {
					assertTreeIn(t, fn, "f")
				}
			}
		})
	}
}

// TestStringChainAllocs pins what a chain of strings costs: the string it
// makes, and no string between.
func TestStringChainAllocs(t *testing.T) {
	defer SetTreeTier(true)
	for _, on := range []bool{false, true} {
		SetTreeTier(on)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, `function f(a, b) { return 'key ' + a + ' and ' + b + ' end' }; for (let i = 0; i < 10; i++) f('x', 'y'); f`))
		if err != nil {
			t.Fatal(err)
		}
		args := []Value{Str(NewString("x")), Str(NewString("y"))}
		if n := testing.AllocsPerRun(100, func() {
			if _, err := r.Call(v, Undefined, args); err != nil {
				t.Fatal(err)
			}
		}); n != 1 {
			t.Errorf("tree %v: %v allocations a call, want 1", on, n)
		}
		r.Close()
	}
}

// hasOp reports whether the function name, somewhere in fn, has op.
func hasOp(fn *bytecode.Function, name string, op bytecode.Op) bool {
	if fn.Name == name {
		for _, in := range fn.Code {
			if in.Op == op {
				return true
			}
		}
		return false
	}
	for _, k := range fn.Constants {
		if k.Fn != nil && hasOp(k.Fn, name, op) {
			return true
		}
	}
	return false
}
