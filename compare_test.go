package quickjs_test

import (
	"fmt"
	"testing"
)

// A comparison that a branch immediately tests is compiled into the branch, so
// the two run as one instruction. What it decides must be what the comparison
// on its own decides, for every operator and every pair of operands -- and a
// comparison that throws must still throw.

func TestFusedComparisonBranches(t *testing.T) {
	// Each case is evaluated in an if, a while, a for and a conditional, since
	// those are the four places a condition is compiled into its branch.
	cases := []struct{ cond, want string }{
		// Numbers, which is the case the fused form answers directly.
		{`1 < 2`, "yes"},
		{`2 < 1`, "no"},
		{`1 <= 1`, "yes"},
		{`2 >= 3`, "no"},
		{`3 > 2`, "yes"},
		{`-0 < 0`, "no"},
		{`0 <= -0`, "yes"},
		// NaN compares false whichever way round it is asked.
		{`NaN < 1`, "no"},
		{`NaN > 1`, "no"},
		{`NaN <= NaN`, "no"},
		{`NaN >= NaN`, "no"},
		{`NaN == NaN`, "no"},
		{`NaN != NaN`, "yes"},
		{`Infinity > 1e308`, "yes"},
		{`-Infinity < -1e308`, "yes"},

		// Strings compare by code unit, not by number.
		{`"a" < "b"`, "yes"},
		{`"10" < "9"`, "yes"},
		{`"10" < 9`, "no"},
		{`"abc" >= "abc"`, "yes"},
		{`"\uD800" < "￿"`, "yes"},

		// Mixed kinds, where the comparison coerces.
		{`null <= 0`, "yes"},
		{`undefined < 1`, "no"},
		{`undefined >= undefined`, "no"},
		{`true > 0`, "yes"},
		{`[] < 1`, "yes"},
		{`[2] > [1]`, "yes"},
		{`({}) < 1`, "no"},
		{`1n < 2`, "yes"},
		{`2n > 3n`, "no"},
		{`1n == 1`, "yes"},
		{`1n === 1`, "no"},

		// Equality, loose and strict.
		{`1 == "1"`, "yes"},
		{`1 === "1"`, "no"},
		{`null == undefined`, "yes"},
		{`null === undefined`, "no"},
		{`null != 0`, "yes"},
		{`"" == 0`, "yes"},
		{`"" !== 0`, "yes"},
		{`Symbol.iterator === Symbol.iterator`, "yes"},
		{`Symbol() === Symbol()`, "no"},

		// An operand that coerces through valueOf, which runs once.
		{`({valueOf() { return 5 }}) < 6`, "yes"},
		{`({valueOf() { return 5 }}) == 5`, "yes"},

		// Operators that look like comparisons but are not fused.
		{`"a" in {a: 1}`, "yes"},
		{`[] instanceof Array`, "yes"},
		{`!(1 < 2) === false`, "yes"},
	}
	for _, tc := range cases {
		for _, shape := range []string{
			`if (%s) { "yes" } else { "no" }`,
			`(function () { while (%s) { return "yes" } return "no" })()`,
			`(function () { for (; %s; ) { return "yes" } return "no" })()`,
			`(%s) ? "yes" : "no"`,
			`(function () { do { if (%s) return "yes"; return "no" } while (false) })()`,
		} {
			src := fmt.Sprintf(shape, tc.cond)
			checkEval(t, src, tc.want)
		}
	}
}

// A comparison runs its operands' coercions in order, and reports what they
// throw, whether or not the branch that tests it is fused into it.
func TestFusedComparisonSideEffects(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var log = []
		  var a = {valueOf() { log.push("a"); return 1 }}
		  var b = {valueOf() { log.push("b"); return 2 }}
		  if (a < b) log.push("then")
		  log.join()`, "a,b,then"},
		{`var log = []
		  var a = {valueOf() { log.push("a"); return 2 }}
		  var b = {valueOf() { log.push("b"); return 1 }}
		  if (a <= b) log.push("then"); else log.push("else")
		  log.join()`, "a,b,else"},
		// A relational comparison converts with a number hint, so a Date
		// compares as a number here and as a string when added.
		{`var d = new Date(0); (d < new Date(1))`, "true"},

		{`try { if ({valueOf() { throw new RangeError() }} < 1) ; }
		  catch (e) { e.constructor.name }`, "RangeError"},
		{`try { while ({valueOf() { throw new RangeError() }} > 1) ; }
		  catch (e) { e.constructor.name }`, "RangeError"},
		{`try { for (; 1 < {valueOf() { throw new RangeError() }};) ; }
		  catch (e) { e.constructor.name }`, "RangeError"},
		{`try { var x = ({valueOf() { throw new RangeError() }} == 1) ? 1 : 2 }
		  catch (e) { e.constructor.name }`, "RangeError"},
		// Comparing a BigInt with a symbol is a TypeError from the coercion.
		{`try { if (1n < Symbol()) ; } catch (e) { e.constructor.name }`, "TypeError"},

		// A loop whose test is a comparison runs the right number of times.
		{`var n = 0; for (var i = 0; i < 5; i++) n++; n`, "5"},
		{`var n = 0; for (var i = 5; i > 0; i--) n++; n`, "5"},
		{`var n = 0, i = 0; while (i !== 3) { i++; n += i } n`, "6"},
		{`var s = ""; for (var i = 0; "" + i !== "3"; i++) s += i; s`, "012"},
		{`var n = 0; for (var i = 0n; i < 4n; i++) n++; n`, "4"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}
