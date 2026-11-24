package quickjs_test

import "testing"

// TestLongChains pins that a chain is bounded by the tree's height, which
// every pass over it recurses to, not by the nesting a parenthesis or a
// block counts: 5,000 property reads, 2,000 calls and 1,200 parentheses
// run, as in Node (KI-92). Past either bound the program is a SyntaxError
// a catch sees, where unbounded input overflowed the goroutine's stack.
func TestLongChains(t *testing.T) {
	t.Parallel()
	evalCases(t, []struct{ src, want string }{
		{`var a = {}; a.b = a; new Function("a", "return a" + ".b".repeat(5000))(a) === a`, "true"},
		{`function f() { return f } new Function("f", "return f" + "()".repeat(2000))(f) === f`, "true"},
		{`var o = [[0]]; o[0] = o; new Function("o", "return o" + "[0]".repeat(5000))(o) === o`, "true"},
		{`function t() { return t } new Function("t", "return t" + "\x60\x60".repeat(3000))(t) === t`, "true"},
		{`var a = {}; a.b = a; new Function("a", "return a" + "?.b".repeat(5000) + "?.c?.d")(a)`, "undefined"},
		{`eval("(".repeat(1200) + "7" + ")".repeat(1200))`, "7"},
		// Additions as long, with and without a string to join: the chain
		// left of the string is added, the rest joined.
		{`var a = 1; new Function("a", "return a" + "+a".repeat(8999))(a)`, "9000"},
		{`var a = 1; new Function("a", "return a" + "+a".repeat(4999) + "+'x'" + "+a".repeat(3))(a)`, "5000x111"},
		{`var a = 1; new Function("a", "return a" + "+a".repeat(4999) + "+'x'")(a)`, "5000x"},
		{`var a = "s"; new Function("a", "return a" + "+a".repeat(2) + "+''" + "+a".repeat(2))(a)`, "sssss"},
		{`var a = 1; new Function("a", "return a" + " && a".repeat(8999))(a)`, "1"},
		{`var a = {}; a.b = a; var f = new Function("a", "return [a" + ".b".repeat(6000) + ", a" + ".b".repeat(6000) + "]");
		  var r = f(a); r[0] === a && r[1] === a`, "true"},
		{`var errs = [];
		  for (var src of ["a" + ".b".repeat(10001), "f" + "()".repeat(10001), "a" + "+a".repeat(10001),
		      "(".repeat(2001) + "a" + ")".repeat(2001), "!".repeat(3000000) + "1", "a" + "?.b".repeat(3000000),
		      "a" + "**a".repeat(1000000), "new ".repeat(1000000) + "f"]) {
		    try { new Function(src); errs.push("ok") } catch (e) { errs.push(e.name + ": " + e.message) }
		  }
		  errs.every(e => e.startsWith("SyntaxError: the expression nests too deeply (")) ? "all" : errs.join("; ")`, "all"},
	})
}
