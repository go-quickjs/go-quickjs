package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

// TestBuildTreesUncalled builds the tree of every function of a script
// without calling any, as nothing else in the tests does: a tree is built
// when its function is first called, so a function no test calls had its
// tree built by none -- Crypto's RSAGenerate, whose property swap panicked
// the update matcher, was found that way.
func TestBuildTreesUncalled(t *testing.T) {
	const src = `
		function swap() { var t = this.p; this.p = this.q; this.q = t; }
		function swapIf() { if (this.p <= 9) { var t = this.p; this.p = this.q; this.q = t; } }
		function copy() { this.a = this.b; this.b = this.a + 1; }
		function nested(x) { return [x, (this.p = this.q, this.q = x)]; }
		function destr() { [this.p, this.q] = [this.q, this.p]; }
		var arrow = function () { var g = () => { var t = this.p; this.p = this.q; this.q = t; }; };
		function upd(o, k) { this.p += this.q; o.x = o.y; o.y = 1; o[k] = o[k + 1]; o[k + 1] = 2; G = H; H = 3; }
		function loops(a) { for (var i = 0; i < a.length; i++) { a[i] = a[i + 1]; a[i + 1] = i; this.s = this.t; this.t = i; } }
	`
	prog, err := parser.Parse(src, parser.Options{})
	if err != nil {
		t.Fatal(err)
	}
	top, err := compiler.Compile(prog, compiler.Options{Text: src})
	if err != nil {
		t.Fatal(err)
	}
	var walk func(f *bytecode.Function)
	walk = func(f *bytecode.Function) {
		buildTree(f)
		for _, k := range f.Constants {
			if k.Fn != nil {
				walk(k.Fn)
			}
		}
	}
	walk(top)
}
