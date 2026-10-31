package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// An equality branch of a local, an upvalue or a tree with undefined, null,
// true or false, either way round, strict and loose (eqLitJump), answers
// as the interpreter does for every kind of value: 0, NaN and the empty string,
// which equal none of them, the booleans, which == converts and eqJump
// still answers, an object, and document.all, which == says is null and
// undefined and === is neither. Each function has a local of its own, so
// that it is run on its tree rather than by pureCall.
func TestEqualityWithLiterals(t *testing.T) {
	const src = `
		const vals = [null, undefined, true, false, 0, -0, 1, NaN, '', 'null', 0n, {}, [], dda]
		const o = { p: null }
		function a(x) { let _k = 0; if (x === null) return 'a'; return 'b' }
		function b(x) { let _k = 0; if (undefined !== x) return 'c'; return 'd' }
		function c(x) { let _k = 0; if (x == null) return 'e'; return 'f' }
		function d(x) { let _k = 0; if (x != undefined) return 'g'; return 'h' }
		function e(x) { let _k = 0; if (x === true) return 'i'; if (x !== false) return 'j'; return 'k' }
		function f(x) { let _k = 0; if (x == true) return 'l'; if (false == x) return 'm'; return 'n' }
		function g(x) { let _k = 0; o.p = x; if (o.p === null) return 'o'; if (o.p === undefined) return 'p'; return 'q' }
		function h(x) { let _k = 0; o.p = x; if (o.p == null) return 'r'; if (o.p !== true) return 's'; return 't' }
		function up(x) {
		  let _k = 0
		  const t = () => { let _j = 0; if (x === null) return 'u'; if (x === undefined) return 'v'; if (x == null) return 'w'; if (x !== false) return 'x'; return 'y' }
		  return t()
		}
		function loop(x) {
		  let _k = 0, n = 0, y = x
		  while (y !== null) { n++; y = n < 3 ? y : null }
		  return n
		}
		const s = []
		for (let round = 0; round < 3; round++) {
		  const r = []
		  for (const x of vals) r.push(a(x) + b(x) + c(x) + d(x) + e(x) + f(x) + g(x) + h(x) + up(x) + loop(x))
		  s.push(r.join(','))
		}
		const out = s[0] + ' | ' + (s[0] === s[1] && s[1] === s[2])
		out`
	const want = "acehjnoru0,bdehjnprv3,bcfgilqtx3,bcfgkmqsy3,bcfgjmqsx3,bcfgjmqsx3,bcfgjlqsx3,bcfgjnqsx3,bcfgjmqsx3,bcfgjnqsx3,bcfgjmqsx3,bcfgjnqsx3,bcfgjmqsx3,bcehjnqrw3 | true"
	for _, c := range compileForTest(t, src).Constants {
		if c.Kind != bytecode.ConstFunction || c.Fn == nil {
			continue
		}
		if name := c.Fn.Name; len(name) == 1 || name == "up" || name == "loop" {
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

// A strict equality branch with a string constant, either way round
// (eqLitJump), answers as the interpreter does: for the constant itself,
// stored and read back, a string of the same code units made at run time,
// flat and as a concatenation not yet flattened, a String object, which
// is no string, a number that reads as it, null, undefined, a symbol and
// another string; of a local, of a property read and of a switch's cases.
func TestEqualityWithStrings(t *testing.T) {
	const src = `
		const o = { d: 'forward' }
		const part = 'for'
		const vals = ['forward', part + 'ward', [part, 'ward'].join(''), new String('forward'), 'backward',
		  '', 7, '7', null, undefined, Symbol('forward'), { toString() { return 'forward' } }, 'forwardx', 'été']
		function a(x) { let _k = 0; if (x === 'forward') return 'a'; return 'b' }
		function b(x) { let _k = 0; if ('forward' !== x) return 'c'; return 'd' }
		function c(x) { let _k = 0; o.d = x; if (o.d === 'forward') return 'e'; if ('7' === o.d) return 'f'; return 'g' }
		function d(x) { let _k = 0; switch (x) { case 'forward': return 'h'; case '7': return 'i'; case 'été': return 'j' } return 'k' }
		function e(x) { let _k = 0; if (x == 'forward') return 'l'; return 'm' }
		function f(x) { let _k = 0; if ('a' === 'a' && x !== '') return 'n'; return 'o' }
		const s = []
		for (let round = 0; round < 3; round++) {
		  const r = []
		  for (const x of vals) r.push(a(x) + b(x) + c(x) + d(x) + e(x) + f(x))
		  s.push(r.join(','))
		}
		const out = s[0] + ' | ' + (s[0] === s[1] && s[1] === s[2])
		out`
	const want = "adehln,adehln,adehln,bcgkln,bcgkmn,bcgkmo,bcgkmn,bcfimn,bcgkmn,bcgkmn,bcgkmn,bcgkln,bcgkmn,bcgjmn | true"
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
