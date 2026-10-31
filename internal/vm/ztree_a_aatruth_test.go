package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// A forward jump on whether a local or an upvalue is truthy (truthJump)
// goes where the interpreter goes for every kind of value: objects, an
// array and a function, document.all, which is falsy, null, undefined,
// the numbers 0, -0, NaN and 1, the empty string and another, the
// booleans, the BigInts 0n and 1n, and a symbol; for if (x), if (!x) and a
// loop's test. Each function has a local of its own, so that it is run on
// its tree rather than by pureCall.
func TestTruthJump(t *testing.T) {
	const src = `
		const vals = [{}, [], function () {}, dda, null, undefined, 0, -0, NaN, 1, '', 'a', true, false, 0n, 1n, Symbol('s')]
		function a(x) { let _k = 0; if (x) return 'a'; return 'b' }
		function b(x) { let _k = 0; if (!x) return 'c'; return 'd' }
		function c(x) { let _k = 0; const f = () => { let _j = 0; if (x) return 'e'; if (!x) return 'f'; return 'g' }; return f() }
		function d(x) { let _k = 0, n = 0, y = x; while (y) { n++; y = n < 2 ? y : null } return String(n) }
		function e(x) { let _k = 0; return x && x !== 1 ? 'h' : 'i' }
		const s = []
		for (let round = 0; round < 3; round++) {
		  const r = []
		  for (const x of vals) r.push(a(x) + b(x) + c(x) + d(x) + e(x))
		  s.push(r.join(','))
		}
		const out = s[0] + ' | ' + (s[0] === s[1] && s[1] === s[2])
		out`
	const want = "ade2h,ade2h,ade2h,bcf0i,bcf0i,bcf0i,bcf0i,bcf0i,bcf0i,ade2i,bcf0i,ade2h,ade2h,bcf0i,bcf0i,ade2h,ade2h | true"
	for _, c := range compileForTest(t, src).Constants {
		if c.Kind != bytecode.ConstFunction || c.Fn == nil {
			continue
		}
		if name := c.Fn.Name; len(name) == 1 {
			if buildTree(c.Fn) == nil {
				t.Errorf("%s is not built as a tree", name)
			}
		}
	}
	for _, tree := range []bool{true, false} {
		SetTreeTier(tree)
		r := New(Config{})
		dda := r.NewObject()
		dda.MarkHTMLDDA()
		r.global.setOwnRaw(r.atoms.intern("dda"), Obj(dda), propDefault)
		v, err := r.Run(compileForTest(t, src))
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != want {
			t.Errorf("tree tier %v: got\n%s\nwant\n%s", tree, got, want)
		}
	}
	SetTreeTier(true)
}
