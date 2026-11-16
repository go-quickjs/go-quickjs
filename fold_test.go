package quickjs

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/vm"
)

// The compiler folds operators on literals, constant tests, and the code
// they and a return leave unreached (internal/fold). These tests
// hold each fold to what the operator gives at run time -- the same
// operator on the same values passed through a function, which nothing
// folds -- and the code left out to what it would have done, in each kind
// of code: a script, strict code, a module, a debugger's, Node's quirks,
// and a direct eval's.

// foldMode runs src, statements that leave their answer in out, and
// returns out.join("|").
type foldMode struct {
	name   string
	strict bool
	run    func(t *testing.T, src string) (string, error)
}

func foldModes() []foldMode {
	eval := func(opts ...Option) func(t *testing.T, src string) (string, error) {
		return func(t *testing.T, src string) (string, error) {
			r := New(opts...)
			defer r.Close()
			v, err := r.Eval(src + "\nout.join('|')")
			return v.String(), err
		}
	}
	return []foldMode{
		{"script", false, eval()},
		{"strict", true, func(t *testing.T, src string) (string, error) {
			return eval()(t, "'use strict';\n"+src)
		}},
		{"module", true, func(t *testing.T, src string) (string, error) {
			r := New()
			defer r.Close()
			if _, err := r.EvalModule("fold.mjs", src+"\nglobalThis.__out = out.join('|');"); err != nil {
				return "", err
			}
			v, err := r.Eval("__out")
			return v.String(), err
		}},
		{"debugger", false, eval(WithDebugger())},
		{"quirks", false, eval(WithNodeQuirks())},
		{"eval", false, func(t *testing.T, src string) (string, error) {
			r := New()
			defer r.Close()
			v, err := r.Eval("(function () { return eval(" + strconv.Quote(src+"\nout.join('|')") + "); })()")
			return v.String(), err
		}},
	}
}

// foldChecks is a program that checks each pair of expressions, a folded
// one and the same unfolded, for the same value of the same type, -0 told
// from 0, at the top level and in a function, and leaves what differs in
// out.
func foldChecks(pairs [][2]string) string {
	var b strings.Builder
	b.WriteString("var out = [];\nfunction id(v) { return v; }\n")
	b.WriteString("function chk(name, a, b) { if (!(Object.is(a, b) && typeof a === typeof b)) out.push(name + ': ' + String(a) + ' / ' + String(b)); }\n")
	body := func() {
		for _, p := range pairs {
			fmt.Fprintf(&b, "chk(%q, %s, %s);\n", p[0], p[0], p[1])
		}
	}
	body()
	b.WriteString("function inner() {\n")
	body()
	b.WriteString("}\ninner();\n")
	return b.String()
}

func runFoldModes(t *testing.T, src, want string, wantStrict ...string) {
	t.Helper()
	for _, m := range foldModes() {
		w := want
		if m.strict && len(wantStrict) > 0 {
			w = wantStrict[0]
		}
		got, err := m.run(t, src)
		if err != nil {
			t.Errorf("%s: %v", m.name, err)
			continue
		}
		if got != w {
			t.Errorf("%s: got\n%s\nwant\n%s", m.name, got, w)
		}
	}
}

// TestUnaryFolds checks !, typeof, void, -, + and ~ of literals and of
// what folds, BigInts among them, against the operators at run time, and
// that what can be rebound is not folded.
func TestUnaryFolds(t *testing.T) {
	t.Parallel()
	pairs := [][2]string{
		{"!0", "!id(0)"}, {"!1", "!id(1)"}, {"!-0", "!id(-0)"}, {"!0.5", "!id(0.5)"},
		{`!""`, `!id("")`}, {`!"a"`, `!id("a")`}, {"!null", "!id(null)"},
		{"!true", "!id(true)"}, {"!false", "!id(false)"},
		{"!0n", "!id(0n)"}, {"!1n", "!id(1n)"}, {"!0x0n", "!id(0x0n)"}, {"!0x10n", "!id(0x10n)"},
		{"!!0", "!!id(0)"}, {`!!"x"`, `!!id("x")`}, {"!!null", "!!id(null)"}, {"!void 0", "!(void id(0))"},
		{"!typeof 1", "!typeof id(1)"}, {"!-1", "!-id(1)"}, {"!(0/0)", "!(id(0)/id(0))"},
		{"typeof 1", "typeof id(1)"}, {`typeof "s"`, `typeof id("s")`}, {"typeof true", "typeof id(true)"},
		{"typeof null", "typeof id(null)"}, {"typeof 1n", "typeof id(1n)"}, {"typeof void 0", "typeof id(undefined)"},
		{"typeof !0", "typeof !id(0)"}, {"typeof -0", "typeof -id(0)"}, {"typeof typeof 1", "typeof typeof id(1)"},
		{"void 0", "void id(0)"}, {`void "a"`, `void id("a")`}, {"void 1n", "void id(1n)"}, {"void !0", "void !id(0)"},
		{"-0", "-id(0)"}, {"-1", "-id(1)"}, {"- -1", "-(-id(1))"}, {"-(-0)", "-(-id(0))"}, {"-(1/0)", "-(id(1)/id(0))"},
		{"-1n", "-id(1n)"}, {"-0n", "-id(0n)"}, {"-0x10n", "-id(0x10n)"}, {"-(-5n)", "-(-id(5n))"},
		{"+1", "+id(1)"}, {"+-0", "+(-id(0))"}, {"+0.5", "+id(0.5)"},
		{"~0", "~id(0)"}, {"~-1", "~-id(1)"}, {"~0.5", "~id(0.5)"}, {"~4294967295", "~id(4294967295)"}, {"~(0/0)", "~(id(0)/id(0))"},
		{"!0 === true", "true"}, {`typeof 1n === "bigint"`, "true"}, {"1 / -0", "-1/id(0)"}, {"-0n === 0n", "true"},
	}
	src := foldChecks(pairs) + `
// What a binding may shadow is not folded.
(function (undefined, NaN, Infinity) {
	chk("shadowed", [!undefined, typeof NaN, -Infinity, String(void undefined)].join(), "false,string,-2,undefined");
})(1, "s", 2);
// + of a BigInt throws, and is not folded.
try { +1n; out.push("+1n folded"); } catch (e) { if (!(e instanceof TypeError)) out.push("+1n: " + e); }
// Constant tests.
var s = "";
if (!0) s += "a"; if ("") s += "b"; if (null) s += "c"; if (0n) s += "d"; if (!0n) s += "e";
if (typeof 1) s += "f"; if (void 0) s += "g"; s += !1 ? "h" : "i"; while (!1) s += "j";
for (; 0;) s += "k"; do s += "l"; while (!1); if (1 - 1) s += "m"; if (2 ** -1) s += "n";
s += (!0 && "o") + (0 || "p") + (null ?? "q") + (!1 ? "r" : "t");
chk("tests", s, "aefilnopqt");
`
	runFoldModes(t, src, "")
}

// TestNumericFolds checks the arithmetic, shift and bitwise operators on
// two numbers, which V8's parser folds and so does the compiler, against
// the runtime's, at the edges: zeros of either sign, NaN, the infinities,
// the int32 and uint32 bounds, ** and % where C and JavaScript differ.
func TestNumericFolds(t *testing.T) {
	t.Parallel()
	values := []string{"0", "-0", "1", "-1", "2", "3", "0.5", "-0.5", "1.5", "2.5", "31", "32", "33", "-33",
		"7", "-7", "0.1", "1e21", "2147483647", "2147483648", "-2147483649", "4294967295", "4294967296",
		"1e308", "5e-324", "1/0", "-1/0", "0/0"}
	ops := []string{"+", "-", "*", "/", "%", "**", "<<", ">>", ">>>", "&", "|", "^"}
	var pairs [][2]string
	for _, op := range ops {
		for _, a := range values {
			for _, b := range values {
				pairs = append(pairs, [2]string{
					fmt.Sprintf("(%s) %s (%s)", a, op, b),
					fmt.Sprintf("id(%s) %s id(%s)", a, op, b),
				})
			}
		}
	}
	pairs = append(pairs, [2]string{"1 + 2 * 3 - 4 / 2", "id(1) + id(2) * id(3) - id(4) / id(2)"},
		[2]string{"-(2 ** 2)", "-(id(2) ** id(2))"}, [2]string{"(1 << 31) >>> 0", "(id(1) << id(31)) >>> id(0)"},
		[2]string{"~(1 << 31)", "~(id(1) << id(31))"}, [2]string{`1 + "2"`, `id(1) + id("2")`},
		[2]string{"1n + 2n", "id(1n) + id(2n)"})
	// Each mode compiles some 9,000 checks twice; the script and strict
	// code alone, as the folds are the same in each.
	src := foldChecks(pairs)
	for _, m := range foldModes()[:2] {
		got, err := m.run(t, src)
		if err != nil || got != "" {
			t.Errorf("%s: %v\n%s", m.name, err, got)
		}
	}
}

// TestDeadCode checks what the code a constant test or a return leaves
// unreached still does: its var and Annex B function declarations, no
// lexical binding outside its block, its early errors, labels and jumps
// around it, finally clauses, and completion values.
func TestDeadCode(t *testing.T) {
	t.Parallel()
	src := `var out = [];
function t1() { if (false) { var v = 1; } return typeof v + ":" + v; }
function t2() { if (false) { function g() {} } try { return typeof g + ":" + String(g); } catch (e) { return e.name; } }
function t2b() { while (false) { function h() {} } try { return String(h); } catch (e) { return e.name; } }
function t3() { if (false) { let x = 1; const y = 2; class C {} } try { x; return "x"; } catch (e) { return e.name; } }
function t4() { return 1; var y = 2; function g() {} }
function t5() { return typeof g + typeof y; var y; function g() {} }
function t6() { var log = []; function f() { try { return 1; log.push("dead"); } finally { log.push("fin"); } } return f() + log.join(); }
function t7() { var r = ""; L: for (;;) { if (false) { continue L; } r += "a"; break; r += "dead"; } return r; }
function t8() { var r = ""; outer: while (true) { while (false) { break outer; } r += "b"; break outer; } return r; }
function t9() { var r = ""; switch (1) { case 1: r += "a"; break; r += "dead"; case 2: r += "b"; } return r; }
function t10() { var r = 0; for (r = 5; false; r++) { r = 99; } return r; }
function t11() { var r = 0; do { r++; } while (false); return r; }
function t12() { var r; try { throw 7; r = 1; } catch (e) { return e; } }
function t13(x) { if (x) { return "a"; } else { return "b"; } return "dead"; }
function t14() { var r = ""; for (var i = 0; i < 3; i++) { if (i == 1) { continue; r += "x"; } r += i; } return r; }
function t15() { var r = []; for (let i = 0; i < 2; i++) { r.push(() => i); if (false) { r = null; } } return r.map(f => f()).join(); }
function t16() { if (true) { var a = 1; } else { var b = 2; } return a + ":" + b; }
function* t17() { yield 1; return 2; yield 3; }
function t18() { var r = 0; L: { r = 1; break L; r = 2; } return r; }
function t19() { if (!0) return "folded"; return "dead"; }
function t20() { for (let i = 0; false; i++) { return "dead"; } return "after"; }
function se(src) { try { new Function(src); return "parsed"; } catch (e) { return e.name; } }
out.push(t1(), t2(), t2b(), t3(), t4(), t5(), t6(), t7(), t8(), t9(), t10(), t11(), t12(), t13(1) + t13(0),
	t14(), t15(), t16(), [...t17()].join(), t18(), t19(), t20());
out.push([eval("1; if (false) 2;"), eval("1; if (true) 2; else 3;"), eval("3; while (false) 4;"),
	eval("5; do { 6 } while (false)"), eval("7; for (8; false;) 9;"), eval("10; if (!0) 11;"),
	eval("12; L: { 13; break L; 14; }"), eval("15; if (false) { 16 } else { }")].map(String).join());
out.push([se("if (false) { break; }"), se("return; break;"), se("if (false) { continue; }"),
	se("while (false) { x = 1 = 2; }"), se("if (false) { ({ __proto__: 1, __proto__: 2 }); }"),
	se("return; class C { m() { this.#x; } }"), se("'use strict'; if (false) { with (a) {} }"),
	se("if (false) { let a; var a; }"), se("return; L: L: ;"), se("if (0) { function* g() { yield = 1; } }"),
	se("throw 1; delete this.#y;")].join());
`
	sloppy := "undefined:undefined|undefined:undefined|undefined|ReferenceError|1|functionundefined|1fin|a|b|a|5|1|7|ab|02|0,1|1:undefined|1|1|folded|after|" +
		"undefined,2,undefined,6,undefined,11,13,undefined|" +
		strings.Repeat("SyntaxError,", 10) + "SyntaxError"
	strict := strings.Replace(sloppy, "undefined:undefined|undefined:undefined|undefined|", "undefined:undefined|ReferenceError|ReferenceError|", 1)
	runFoldModes(t, src, sloppy, strict)
}

// TestDeadCodeDebugger pins a debugger's view of code with folds and code
// left out: a breakpoint on a folded test stops there with the frame as
// it is, one on a line of code that is left out is set without harm and
// resolved where the code goes on, and stepping through the function stops
// at the statements that are left and nowhere else.
func TestDeadCodeDebugger(t *testing.T) {
	t.Parallel()
	const src = `function f(x) {
  if (false) {
    x = x + 1;
  }
  if (!0) x = x + 2;
  return x;
  x = 9;
}
f(1);
`
	r, h := newDebugged(t)
	var seen []string
	h.then = func(p *vm.DebugPause) vm.StepAction {
		x := "-"
		if p.Frames[0].FunctionName() == "f" {
			v, err := p.Frames[0].Evaluate("x")
			if err != nil {
				t.Fatal(err)
			}
			x = h.show(v)
		}
		seen = append(seen, fmt.Sprintf("%s x=%s", h.pauses[len(h.pauses)-1], x))
		return vm.StepOver
	}
	for _, line := range []int{3, 5, 7} {
		r.rt.SetBreakpoint("dead.js", line, 0)
	}
	v, err := r.EvalFile("dead.js", src)
	if err != nil || v.String() != "3" {
		t.Fatalf("result %s, %v", v.String(), err)
	}
	got := strings.Join(h.resolved, "|") + "\n" + strings.Join(seen, "\n")
	// The breakpoints on lines 3 and 7, whose code is left out, resolve
	// where code goes on after them, as V8's would: the folded if on line
	// 5, and the call on line 9.
	const want = "1 dead.js:5:3|2 dead.js:5:3|3 dead.js:9:1\n" +
		"breakpoint :9:1 x=-\n" +
		"breakpoint f:5:3 x=1\n" +
		"step f:5:11 x=1\n" +
		"step f:6:3 x=3"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

// TestStringFolds checks + of string literals, which the parser joins as
// V8's does, against the same + at run time: runs at a chain's start and
// at its end, templates with no substitution, surrogate halves that meet
// as a pair; numbers and strings, which are not joined, are checked too;
// and a joined literal that starts a function is not its prologue's.
func TestStringFolds(t *testing.T) {
	t.Parallel()
	pairs := [][2]string{
		{`"a" + "b"`, `id("a") + "b"`},
		{`"a" + "b" + "c"`, `id("a") + "b" + id("c")`},
		{`"" + ""`, `id("") + ""`},
		{"\"a\" + `b` + 'c'", `id("a") + "b" + "c"`},
		{`id(1) + "a" + "b"`, `id(1) + id("a") + "b"`},
		{`id({ toString() { return "o"; }, valueOf() { return 7; } }) + "a" + "b"`, `"7ab"`},
		{`"\uD800" + "\uDC00"`, `id("\uD800") + "\uDC00"`},
		{`("\uD800" + "\uDC00").length`, `2`},
		{`("\uD800" + "\uDC00").codePointAt(0)`, `0x10000`},
		{`("\uD800" + "\uDC00") === "\u{10000}"`, `true`},
		{`"\uDC00" + "\uD800"`, `id("\uDC00") + "\uD800"`},
		{`("x" + "\uD800").length`, `2`},
		{`"x" + -0`, `"x0"`},
		{`"x" + 0.1`, `"x0.1"`},
		{`"x" + 1e21`, `"x1e+21"`},
		{`1 + "a"`, `"1a"`},
		{`"a" + 1 + "b"`, `"a1b"`},
	}
	src := foldChecks(pairs) + `
// Function's code is sloppy but for its own prologue, in every mode.
chk("prologue", Function('"a" + "b"; "use strict"; return this === undefined;')(), false);
chk("prologue joined", Function('"use" + " strict"; return this === undefined;')(), false);
chk("prologue kept", Function('"a"; "use strict"; return this === undefined;')(), true);
`
	runFoldModes(t, src, "")
}
