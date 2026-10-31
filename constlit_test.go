package quickjs_test

import (
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/internal/vm"
)

// A read of a const initialized with a literal, which the compiler makes
// the literal where the binding is out of its dead zone (constLiteral),
// gives what Node and C QuickJS give: a ReferenceError from an arrow made
// before the declaration and from a hoisted function, called before it,
// and the value after; a switch's case block's binding read past its
// declaration; a direct eval's var and a with object answering for the
// name; a loop's const; an assignment, which throws a TypeError; each kind
// of literal; and tests of a const false and true; on the tree tier and
// in the interpreter.
func TestConstLiteralReads(t *testing.T) {
	const src = `(function () {
  const r = [];
  function tdzArrow() { const g = () => X; let a; try { a = g() } catch (e) { a = e.constructor.name } const X = 1; return a + '/' + g() }
  function tdzHoisted() { let a; try { a = h() } catch (e) { a = e.constructor.name } const X = 2; return a + '/' + h(); function h() { return X } }
  function sw(k) { switch (k) { case 0: const X = 3; case 1: try { return (() => X)() } catch (e) { return e.constructor.name } } }
  const E = 1;
  function evalShadow() { eval('var E = 2'); return (() => E)() + ',' + E }
  const W = 1;
  function withShadow() { with ({ W: 5 }) { return W + ',' + (() => W)() } }
  function loop() { const fs = []; for (let i = 0; i < 3; i++) { const L = 'a'; fs.push(() => L + i) } return fs.map((f) => f()).join('') }
  const C = 7;
  function assign() { try { (() => { C = 2 })() } catch (e) { return e.constructor.name + C } }
  const NUL = null, T = true, F = false, Z = 0, H = 1.5, S = 'str', ES = '', BIG = 1e21, NEG = -0;
  function kinds() { return [NUL === null, T, F, Z, H, S, ES === '', BIG, Object.is(NEG, -0), typeof NUL, typeof S, NUL == undefined].join(',') }
  function inner() { return () => () => [NUL, S, Z + H].join('|') }
  function ifs() { const DBG = false; let n = 0; for (let i = 0; i < 3; i++) { if (DBG) n += 100; if (!DBG) n++; if (T) n += 10 } return n }
  r.push(tdzArrow(), tdzHoisted(), sw(0), sw(1), evalShadow(), E, withShadow(), loop(), assign(), kinds(), inner()()(), ifs());
  return r.join(' ; ');
})()`
	const want = "ReferenceError/1 ; ReferenceError/2 ; 3 ; ReferenceError ; 2,2 ; 1 ; 5,5 ; a0a1a2 ; TypeError7 ; " +
		"true,true,false,0,1.5,str,true,1e+21,true,object,string,true ; |str|1.5 ; 33"
	defer vm.SetTreeTier(true)
	for _, tree := range []bool{true, false} {
		vm.SetTreeTier(tree)
		rt := quickjs.New()
		v, err := rt.Eval(src)
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String(); got != want {
			t.Errorf("tree tier %v: got\n%s\nwant\n%s", tree, got, want)
		}
		rt.Close()
	}
}
