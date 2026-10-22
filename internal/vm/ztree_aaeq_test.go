package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// An equality branch of a local, or of any value, with a number constant,
// either way round, and of two locals (eqNumJump) answers as the
// interpreter and Node do for every kind of value: -0 and NaN, strings
// that read as the number, null and undefined, booleans, BigInts, an
// object converted by valueOf, an array, a symbol, and a valueOf that
// throws, whose error the loose comparison passes on.
func TestEqualityWithNumbers(t *testing.T) {
	const src = `
		const vals = [0, -0, 1, NaN, '0', '', '1', null, undefined, true, false, 0n, 1n, { valueOf() { return 1 } }, [0], Symbol.iterator]
		const o = { p: 1 }
		function a(x) { if (x === 0) return 'a'; return 'b' }
		function b(x) { if (0 !== x) return 'c'; return 'd' }
		function c(x) { if (x == 1) return 'e'; return 'f' }
		function d(x) { if (1 != x) return 'g'; return 'h' }
		function e(x) { if ((x | 0) === 0) return 'i'; return 'j' }
		function f(x) { o.p = x; if (o.p == 0) return 'k'; return 'l' }
		function g(x, y) { if (x === y) return 'm'; return 'n' }
		function h(x, y) { if (x !== y) return 'o'; return 'p' }
		function nan(x) { if (x === NaN) return 'q'; if (x == NaN) return 'r'; return 's' }
		function neg(x) { if (x === -0) return 't'; return 'u' }
		const s = []
		for (let round = 0; round < 3; round++) {
		  const r = []
		  for (const x of vals) {
		    let k = ''
		    for (const fn of [a, b, c, d, nan, neg]) { try { k += fn(x) } catch (err) { k += err.constructor.name } }
		    try { k += e(x) } catch (err) { k += err.constructor.name }
		    try { k += f(x) } catch (err) { k += err.constructor.name }
		    for (const y of [0, -0, NaN, '0', x]) k += g(x, y) + h(x, y)
		    r.push(k)
		  }
		  const bad = { valueOf() { throw new RangeError('no') } }
		  try { c(bad) } catch (err) { r.push(err.constructor.name + err.message) }
		  s.push(r.join(','))
		}
		const out = s[0] + ' | ' + (s[0] === s[1] && s[1] === s[2])
		out`
	const want = "adfgstikmpmpnonomp,adfgstikmpmpnonomp,bcehsujlnonononomp,bcfgsuilnonononono,bcfgsuiknononompmp,bcfgsuiknonononomp,bcehsujlnonononomp,bcfgsuilnonononomp,bcfgsuilnonononomp,bcehsujlnonononomp,bcfgsuiknonononomp,bcfgsuTypeErrorknonononomp,bcehsuTypeErrorlnonononomp,bcehsujlnonononomp,bcfgsuiknonononomp,bcfgsuTypeErrorlnonononomp,RangeErrorno | true"
	for _, c := range compileForTest(t, src).Constants {
		if c.Kind != bytecode.ConstFunction || c.Fn == nil || len(c.Fn.Name) != 1 && c.Fn.Name != "nan" && c.Fn.Name != "neg" {
			continue
		}
		if buildTree(c.Fn) == nil {
			t.Errorf("%s is not built as a tree", c.Fn.Name)
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
