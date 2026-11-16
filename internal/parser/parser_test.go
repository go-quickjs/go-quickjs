package parser

import (
	"slices"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/ast"
	"github.com/go-quickjs/go-quickjs/internal/wtf8"
)

// parseOK parses src and fails the test if it does not parse.
func parseOK(t *testing.T, src string) string {
	t.Helper()
	prog, err := Parse(src, Options{})
	if err != nil {
		t.Fatalf("parsing %q: %v", src, err)
	}
	return dump(prog)
}

// checkParse asserts that src parses to the given s-expression.
func checkParse(t *testing.T, src, want string) {
	t.Helper()
	if got := parseOK(t, src); got != want {
		t.Errorf("parsing %q\n got: %s\nwant: %s", src, got, want)
	}
}

// checkError asserts that src fails to parse with a message containing substr.
func checkError(t *testing.T, src, substr string) {
	t.Helper()
	_, err := Parse(src, Options{})
	if err == nil {
		t.Errorf("parsing %q: expected an error mentioning %q, but it parsed", src, substr)
		return
	}
	if !strings.Contains(err.Error(), substr) {
		t.Errorf("parsing %q: error %q does not mention %q", src, err, substr)
	}
}

func TestLiterals(t *testing.T) {
	tests := []struct{ src, want string }{
		{"1", "1"},
		{"1.5", "1.5"},
		{"'abc'", `"abc"`},
		{"true", "true"},
		{"false", "false"},
		{"null", "null"},
		{"this", "this"},
		{"x", "x"},
		{"123n", "123n"},
		{"/ab+c/gi", "/ab+c/gi"},
		{"[]", "(array)"},
		{"[1, 2]", "(array 1 2)"},
		{"[1, , 3]", "(array 1 hole 3)"},
		{"[...a]", "(array (... a))"},
		{"({})", "(object)"},
		{"({a: 1})", "(object (prop a 1))"},
		{"({a})", "(object (prop a a))"},
		{"({[k]: 1})", "(object (prop [k] 1))"},
		{"({...a})", "(object (... a))"},
		{"({1: 2})", "(object (prop 1 2))"},
		{"({'s': 2})", `(object (prop "s" 2))`},
	}
	for _, tt := range tests {
		checkParse(t, tt.src, tt.want)
	}
}

func TestOperatorPrecedence(t *testing.T) {
	tests := []struct{ src, want string }{
		// Names, not numbers, which the parser folds (TestParseFolds).
		{"a + b * c", "(+ a (* b c))"},
		{"a * b + c", "(+ (* a b) c)"},
		{"a + b + c", "(+ (+ a b) c)"},     // left-associative
		{"a ** b ** c", "(** a (** b c))"}, // right-associative
		{"a || b && c", "(|| a (&& b c))"},
		{"a | b ^ c & d", "(| a (^ b (& c d)))"},
		{"a == b < c", "(== a (< b c))"},
		{"a << b + c", "(<< a (+ b c))"},
		{"a in b", "(in a b)"},
		{"a instanceof b", "(instanceof a b)"},
		{"-a * b", "(* (- a) b)"},
		{"!a && b", "(&& (! a) b)"},
		{"typeof a === 'x'", `(=== (typeof a) "x")`},
		{"a ? b : c", "(?: a b c)"},
		{"a ? b : c ? d : e", "(?: a b (?: c d e))"},
		{"a, b, c", "(seq a b c)"},
		{"a ?? b", "(?? a b)"},
		{"(a || b) ?? c", "(?? (|| a b) c)"},
	}
	for _, tt := range tests {
		checkParse(t, tt.src, tt.want)
	}
}

func TestNullishCannotMixWithLogical(t *testing.T) {
	// The grammar forbids these without parentheses, because the precedence
	// would be surprising either way.
	checkError(t, "a ?? b || c", "cannot be mixed")
	checkError(t, "a || b ?? c", "cannot be mixed")
	checkError(t, "a && b ?? c", "cannot be mixed")
}

func TestUnaryBeforeExponentIsAnError(t *testing.T) {
	// `-2 ** 2` is ambiguous to a reader, so the grammar rejects it.
	checkError(t, "-2 ** 2", "requires parentheses")
	checkError(t, "typeof a ** 2", "requires parentheses")
	checkParse(t, "(-a) ** 2", "(** (- a) 2)")
}

// TestParseFolds pins the operators the parser folds as it builds them, as
// V8's does (internal/fold): ! of a literal, -, + and ~ of a number, - of
// a BigInt, and the arithmetic, shift and bitwise operators on two
// numbers, from the bottom up, in the order the grammar binds them; and
// what it leaves: names, which may be rebound, typeof and void, which the
// compiler folds, a string's +, and + of a BigInt, which throws.
func TestParseFolds(t *testing.T) {
	for _, tt := range []struct{ src, want string }{
		{"1 + 2 * 3", "7"},
		{"(1 + 2) * 3", "9"},
		{"2 ** 3 ** 2", "512"},
		{"(-2) ** 2", "4"},
		{"8 - 2 - 1", "5"},
		{"1 << 31", "-2147483648"},
		{"-1 >>> 0", "4294967295"},
		{"~5", "-6"},
		{"- -1", "1"},
		{"!0", "true"},
		{`!""`, "true"},
		{"!null", "true"},
		{"!0n", "true"},
		{"!!1", "true"},
		{"-1n", "-1n"},
		{"-0x10n", "-16n"},
		{"a + 1 + 2", "(+ (+ a 1) 2)"},
		{"1 + 2 + a", "(+ 3 a)"},
		{"!a", "(! a)"},
		{"-NaN", "(- NaN)"},
		{"!undefined", "(! undefined)"},
		{"typeof 1", "(typeof 1)"},
		{"void 0", "(void 0)"},
		{`1 + "2"`, `(+ 1 "2")`},
		{"+1n", "(+ 1n)"},
	} {
		checkParse(t, tt.src, tt.want)
	}
}

// TestParseStringFolds pins + of two string literals, which the parser
// joins as V8's does: a chain's left run, and the run at the end of a chain
// whose left is not a literal (expr + "a" + "b" is expr + "ab"); a
// template with no substitution as a string; and what it leaves: a number
// and a string, which V8 does not join, a run in the middle of a chain, a
// template with a substitution or an invalid escape, and a tagged one.
func TestParseStringFolds(t *testing.T) {
	for _, tt := range []struct{ src, want string }{
		{`"a" + "b"`, `"ab"`},
		{`"a" + "b" + "c"`, `"abc"`},
		{`"a" + 'b' + ` + "`c`", `"abc"`},
		{"`a` + `b`", `"ab"`},
		{`"" + ""`, `""`},
		{`x + "a" + "b"`, `(+ x "ab")`},
		{`x + "a" + "b" + y`, `(+ (+ x "ab") y)`},
		{`x + "a" + "b" + y + "c" + "d"`, `(+ (+ (+ x "ab") y) "cd")`},
		{`"a" + "b" + x + "c"`, `(+ (+ "ab" x) "c")`},
		{`"a" + ("b" + "c")`, `"abc"`},
		{`("a" + x) + "b" + "c"`, `(+ (+ "a" x) "bc")`},
		{`"a" + 1`, `(+ "a" 1)`},
		{`1 + "a"`, `(+ 1 "a")`},
		{`"a" + 1 + "b"`, `(+ (+ "a" 1) "b")`},
		{`"a" - "b"`, `(- "a" "b")`},
		{`"a" + "b" * 2`, `(+ "a" (* "b" 2))`},
		{"`a${x}` + \"b\"", `(+ (tpl "a" x "") "b")`},
		{"\"a\" + t`b`", `(+ "a" (tagged t (tpl "b")))`},
	} {
		checkParse(t, tt.src, tt.want)
	}
	// The halves of a surrogate pair in two literals are the pair, as the
	// runtime's concatenation makes them; a lone half stays a lone half.
	for _, tt := range []struct {
		src  string
		want []uint16
	}{
		{`"\uD800" + "\uDC00"`, []uint16{0xD800, 0xDC00}},
		{`"\uD800" + "x"`, []uint16{0xD800, 'x'}},
		{`"x" + "\uDC00"`, []uint16{'x', 0xDC00}},
		{`"\uDC00" + "\uD800"`, []uint16{0xDC00, 0xD800}},
	} {
		prog, err := Parse(tt.src, Options{})
		if err != nil {
			t.Fatal(err)
		}
		lit, ok := prog.Body[0].(*ast.ExprStmt).X.(*ast.StringLit)
		if !ok {
			t.Fatalf("%s: not folded", tt.src)
		}
		if got := wtf8.ToUTF16(lit.Value); !slices.Equal(got, tt.want) {
			t.Errorf("%s: %x, want %x", tt.src, got, tt.want)
		}
		if tt.want[0] == 0xD800 && tt.want[1] == 0xDC00 && lit.Value != "\U00010000" {
			t.Errorf("%s: the pair is %q, not the code point's one spelling", tt.src, lit.Value)
		}
	}
	// A string literal that starts a statement and is joined is no longer
	// the prologue's directive, so the "use strict" after it is not one.
	checkParse(t, `function f() { "a" + "b"; "use strict"; with (x) {} }`,
		`(decl (function f () ("ab" "use strict" (with x block()))))`)
	// Past the most a string may have, the literals stay apart, for the
	// program to throw as it runs.
	defer func(n int) { maxFoldedString = n }(maxFoldedString)
	maxFoldedString = 4
	checkParse(t, `"ab" + "cd"`, `"abcd"`)
	checkParse(t, `"ab" + "cd" + "e"`, `(+ "abcd" "e")`)
	checkParse(t, `"ab" + "cde"`, `(+ "ab" "cde")`)
	// Lengths are in UTF-16 code units: a pair is two, as at run time.
	checkParse(t, `"\uD800" + "\uDC00x"`, `"𐀀x"`)
	checkParse(t, `"\uD800" + "\uDC00xyz"`, `(+ "\xed\xa0\x80" "\xed\xb0\x80xyz")`)
}

func TestUpdateExpressions(t *testing.T) {
	checkParse(t, "a++", "(post++ a)")
	checkParse(t, "++a", "(pre++ a)")
	checkParse(t, "a--", "(post-- a)")
	checkParse(t, "--a", "(pre-- a)")
	checkParse(t, "a.b++", "(post++ (. a b))")
	checkError(t, "1++", "invalid assignment target")
}

func TestMemberAndCall(t *testing.T) {
	tests := []struct{ src, want string }{
		{"a.b", "(. a b)"},
		{"a.b.c", "(. (. a b) c)"},
		{"a[b]", "(idx a b)"},
		{"a()", "(call a)"},
		{"a(1, 2)", "(call a 1 2)"},
		{"a(...b)", "(call a (... b))"},
		{"a.b()", "(call (. a b))"},
		{"a()()", "(call (call a))"},
		{"a().b", "(. (call a) b)"},
		{"new a", "(new a)"},
		{"new a()", "(new a)"},
		{"new a.b()", "(new (. a b))"},
		{"new a()()", "(call (new a))"},
		{"new new a()", "(new (new a))"},
		{"a.if", "(. a if)"}, // reserved words are valid property names
	}
	for _, tt := range tests {
		checkParse(t, tt.src, tt.want)
	}
}

func TestOptionalChaining(t *testing.T) {
	tests := []struct{ src, want string }{
		{"a?.b", "(chain (?. a b))"},
		{"a?.[b]", "(chain (?idx a b))"},
		{"a?.()", "(chain (?call a))"},
		{"a?.b.c", "(chain (. (?. a b) c))"},
		{"a?.b?.c", "(chain (?. (?. a b) c))"},
	}
	for _, tt := range tests {
		checkParse(t, tt.src, tt.want)
	}
}

func TestTemplateLiterals(t *testing.T) {
	checkParse(t, "`abc`", `(tpl "abc")`)
	checkParse(t, "`a${x}b`", `(tpl "a" x "b")`)
	checkParse(t, "`${x}${y}`", `(tpl "" x "" y "")`)
	checkParse(t, "tag`a${x}b`", `(tagged tag (tpl "a" x "b"))`)
	checkParse(t, "`a${`b${c}`}d`", `(tpl "a" (tpl "b" c "") "d")`)
}

func TestArrowFunctions(t *testing.T) {
	tests := []struct{ src, want string }{
		{"x => x", "(arrow (x) ((return x)))"},
		{"() => 1", "(arrow () ((return 1)))"},
		{"(a, b) => a", "(arrow (a b) ((return a)))"},
		{"(a) => a", "(arrow (a) ((return a)))"},
		{"x => { return x; }", "(arrow (x) ((return x)))"},
		{"(a = 1) => a", "(arrow ((def a 1)) ((return a)))"},
		{"(...a) => a", "(arrow ((rest a)) ((return a)))"},
		{"([a, b]) => a", "(arrow ((apat a b)) ((return a)))"},
		{"({a}) => a", "(arrow ((opat (prop a a))) ((return a)))"},
		{"async x => x", "(async arrow (x) ((return x)))"},
		{"async (a, b) => a", "(async arrow (a b) ((return a)))"},
		{"a => b => a", "(arrow (a) ((return (arrow (b) ((return a))))))"},
	}
	for _, tt := range tests {
		checkParse(t, tt.src, tt.want)
	}
}

func TestParenthesizedIsNotArrow(t *testing.T) {
	// These look like arrow parameter lists until the `=>` fails to appear.
	checkParse(t, "(a)", "a")
	checkParse(t, "(a, b)", "(seq a b)")
	checkParse(t, "(a + b) * c", "(* (+ a b) c)")
	checkParse(t, "async(1)", "(call async 1)")
	checkParse(t, "async", "async")
	checkError(t, "()", "arrow parameter list")
	checkError(t, "(...a)", "rest element")
	checkError(t, "(a,)", "trailing comma")
}

func TestArrowNewlineBeforeArrowIsAnError(t *testing.T) {
	// ASI applies before `=>`, so this is not an arrow function.
	checkError(t, "x\n=> x", "unexpected")
}

func TestFunctions(t *testing.T) {
	tests := []struct{ src, want string }{
		{"function f() {}", "(decl (function f () ()))"},
		{"function f(a, b) {}", "(decl (function f (a b) ()))"},
		{"function f(a = 1) {}", "(decl (function f ((def a 1)) ()))"},
		{"function f(...a) {}", "(decl (function f ((rest a)) ()))"},
		{"function* g() {}", "(decl (function* g () ()))"},
		{"async function f() {}", "(decl (async function f () ()))"},
		{"async function* f() {}", "(decl (async function* f () ()))"},
		{"(function () {})", "(function () ())"},
		{"(function f() {})", "(function f () ())"},
		{"function f() { return 1; }", "(decl (function f () ((return 1))))"},
	}
	for _, tt := range tests {
		checkParse(t, tt.src, tt.want)
	}
}

func TestGeneratorsAndAsync(t *testing.T) {
	checkParse(t, "function* g() { yield 1; }", "(decl (function* g () ((yield 1))))")
	checkParse(t, "function* g() { yield* a; }", "(decl (function* g () ((yield* a))))")
	checkParse(t, "function* g() { yield; }", "(decl (function* g () ((yield))))")
	checkParse(t, "async function f() { await a; }", "(decl (async function f () ((await a))))")
	// Outside a generator, `yield` is an ordinary identifier in sloppy mode.
	checkParse(t, "yield", "yield")
	// `await` outside an async function is likewise an identifier.
	checkParse(t, "await", "await")
	// A line break before `async`, where an operand is expected, does not
	// stop it starting an async function or arrow (jsonpath-plus lists
	// `async function () {}.constructor` on a line of its own); one after it
	// still does.
	checkParse(t, "[a,\nasync function () {}]", "(array a (async function () ()))")
	checkParse(t, "f(1,\nasync x => x)", "(call f 1 (async arrow (x) ((return x))))")
	checkParse(t, "x = \nasync (y) => y", "(= x (async arrow (y) ((return y))))")
	checkError(t, "[a,\nasync\nfunction () {}]", "expected")
}

func TestClasses(t *testing.T) {
	tests := []struct{ src, want string }{
		{"class A {}", "(decl (class A))"},
		{"class A extends B {}", "(decl (class A extends B))"},
		{"(class {})", "(class)"},
		{"class A { m() {} }", "(decl (class A (method m (function () ()))))"},
		{"class A { static m() {} }", "(decl (class A (method static m (function () ()))))"},
		{"class A { get x() {} }", "(decl (class A (get x (function () ()))))"},
		{"class A { set x(v) {} }", "(decl (class A (set x (function (v) ()))))"},
		{"class A { constructor() {} }", "(decl (class A (method constructor (function () ()))))"},
		{"class A { x = 1; }", "(decl (class A (field x 1)))"},
		{"class A { x; }", "(decl (class A (field x)))"},
		{"class A { static x = 1; }", "(decl (class A (field static x 1)))"},
		{"class A { #x = 1; }", "(decl (class A (field #x 1)))"},
		{"class A { *m() {} }", "(decl (class A (method m (function* () ()))))"},
		{"class A { async m() {} }", "(decl (class A (method m (async function () ()))))"},
		{"class A { static {} }", "(decl (class A (static-block ())))"},
		// `static`, `get` and `async` are contextual and can name members.
		{"class A { static() {} }", "(decl (class A (method static (function () ()))))"},
		{"class A { get() {} }", "(decl (class A (method get (function () ()))))"},
		{"class A { async() {} }", "(decl (class A (method async (function () ()))))"},
	}
	for _, tt := range tests {
		checkParse(t, tt.src, tt.want)
	}
}

func TestClassErrors(t *testing.T) {
	checkError(t, "class A { constructor() {} constructor() {} }", "only one constructor")
	checkError(t, "class A { async constructor() {} }", "cannot be async")
	checkError(t, "class {}", "requires a name")
	checkError(t, "class A { m() { super(); } }", "derived constructor")
	// super.#x is a syntax error wherever it is, read or called.
	checkError(t, "class A { #x; m() { return super.#x } }", "through \"super\"")
	checkError(t, "class A extends Object { #x; m() { return super.#x() } }", "through \"super\"")
	checkError(t, "class A { x = 1; constructor = 2; }", "cannot be named")
}

func TestStatements(t *testing.T) {
	tests := []struct{ src, want string }{
		{"var a;", "(var a)"},
		{"var a = 1;", "(var (a 1))"},
		{"var a = 1, b = 2;", "(var (a 1) (b 2))"},
		{"let a = 1;", "(let (a 1))"},
		{"const a = 1;", "(const (a 1))"},
		{"{ a; }", "block(a)"},
		{";", "empty"},
		{"if (a) b;", "(if a b)"},
		{"if (a) b; else c;", "(if a b c)"},
		{"while (a) b;", "(while a b)"},
		{"do a; while (b)", "(do a b)"},
		{"for (;;) a;", "(for _ _ _ a)"},
		{"for (a; b; c) d;", "(for a b c d)"},
		{"for (var i = 0; i < 2; i++) a;", "(for (var (i 0)) (< i 2) (post++ i) a)"},
		{"for (a in b) c;", "(for-in a b c)"},
		{"for (var a in b) c;", "(for-in (var a) b c)"},
		{"for (const a of b) c;", "(for-of (const a) b c)"},
		{"for (a of b) c;", "(for-of a b c)"},
		{"throw a;", "(throw a)"},
		{"debugger;", "debugger"},
		{"a: while (1) { break a; }", "(label a (while 1 block((break a))))"},
		{"a: b: c;", "(label a (label b c))"},
	}
	for _, tt := range tests {
		checkParse(t, tt.src, tt.want)
	}
}

func TestTryStatement(t *testing.T) {
	checkParse(t, "try { a; } catch (e) { b; }", "(try (a) (catch e (b)))")
	checkParse(t, "try { a; } catch { b; }", "(try (a) (catch (b)))")
	checkParse(t, "try { a; } finally { b; }", "(try (a) (finally (b)))")
	checkParse(t, "try { a; } catch (e) { b; } finally { c; }", "(try (a) (catch e (b)) (finally (c)))")
	checkParse(t, "try { a; } catch ({message}) { b; }", "(try (a) (catch (opat (prop message message)) (b)))")
	checkError(t, "try { a; }", "requires a")
}

func TestSwitchStatement(t *testing.T) {
	checkParse(t, "switch (a) {}", "(switch a)")
	checkParse(t, "switch (a) { case 1: b; }", "(switch a (case 1 (b)))")
	checkParse(t, "switch (a) { case 1: case 2: b; }", "(switch a (case 1 ()) (case 2 (b)))")
	checkParse(t, "switch (a) { default: b; }", "(switch a (default (b)))")
	checkError(t, "switch (a) { default: ; default: ; }", "only one default")
}

func TestDestructuring(t *testing.T) {
	tests := []struct{ src, want string }{
		{"var [a, b] = c;", "(var ((apat a b) c))"},
		{"var [a, , b] = c;", "(var ((apat a hole b) c))"},
		{"var [a = 1] = c;", "(var ((apat (def a 1)) c))"},
		{"var [...a] = c;", "(var ((apat (rest a)) c))"},
		{"var {a} = c;", "(var ((opat (prop a a)) c))"},
		{"var {a: b} = c;", "(var ((opat (prop a b)) c))"},
		{"var {a = 1} = c;", "(var ((opat (prop a (def a 1))) c))"},
		{"var {...a} = c;", "(var ((opat (rest a)) c))"},
		{"var {a: [b]} = c;", "(var ((opat (prop a (apat b))) c))"},
		// Assignment destructuring permits any reference as a leaf.
		{"[a.b] = c", "(= (apat (. a b)) c)"},
		{"({a: b.c} = d)", "(= (opat (prop a (. b c))) d)"},
		{"[a, b] = c", "(= (apat a b) c)"},
	}
	for _, tt := range tests {
		checkParse(t, tt.src, tt.want)
	}
}

func TestDestructuringErrors(t *testing.T) {
	checkError(t, "var [a.b] = c;", "not a valid binding target")
	checkError(t, "var [a] ;", "requires an initializer")
	checkError(t, "const a;", "requires an initializer")
	checkError(t, "var [...a, b] = c;", "must be the last element")
	checkError(t, "var {...{a}} = c;", "must be an identifier")
}

func TestAutomaticSemicolonInsertion(t *testing.T) {
	// A newline lets the statement end without a semicolon.
	checkParse(t, "a\nb", "a b")
	// `return` is restricted: the operand must be on the same line.
	checkParse(t, "function f() { return\n1 }", "(decl (function f () ((return) 1)))")
	checkParse(t, "function f() { return 1\n}", "(decl (function f () ((return 1))))")
	// A postfix operator cannot be on the next line.
	checkParse(t, "a\n++b", "a (pre++ b)")
	// No ASI when the next line continues the expression.
	checkParse(t, "a\n.b", "(. a b)")
	checkParse(t, "a = b\n+ c", "(= a (+ b c))")
	// `throw` is restricted and has no operand-less form, so this is an error.
	checkError(t, "throw\n1", "newline is not allowed")
	checkError(t, "a b", "expected ';'")
}

func TestStrictMode(t *testing.T) {
	// A "use strict" directive turns on strict mode for the rest of the script.
	checkError(t, `"use strict"; var eval = 1;`, "strict mode")
	checkError(t, `"use strict"; with (a) {}`, "strict mode")
	checkError(t, `"use strict"; 0755;`, "octal")
	checkError(t, `"use strict"; delete a;`, "delete a variable")
	// Sloppy mode allows all of those.
	checkParse(t, "var eval = 1;", "(var (eval 1))")
	checkParse(t, "with (a) { b; }", "(with a block(b))")
	// An escaped directive is not a directive: the source must spell the
	// characters literally for "use strict" to take effect.
	prog, err := Parse("\"\\u0075se strict\"; var eval = 1;", Options{})
	if err != nil {
		t.Fatalf("an escaped directive should not enable strict mode: %v", err)
	}
	if prog.Strict {
		t.Error("an escaped \"use strict\" should not enable strict mode")
	}
	// A directive that is part of a larger expression is not a directive.
	if prog, err := Parse(`"use strict" + x;`, Options{}); err != nil {
		t.Fatal(err)
	} else if prog.Strict {
		t.Error("a string in a larger expression should not be a directive")
	}
}

func TestStrictModeInFunctionBody(t *testing.T) {
	checkError(t, `function f() { "use strict"; var eval = 1; }`, "strict mode")
	// A non-simple parameter list cannot be combined with a "use strict" body.
	checkError(t, `function f(a = 1) { "use strict"; }`, "non-simple parameter list")
	checkError(t, `function f(a, a) { "use strict"; }`, "duplicate parameter")
	// Sloppy mode outside is unaffected by an inner strict function.
	checkParse(t, `function f() { "use strict"; } var eval = 1;`,
		`(decl (function f () ("use strict"))) (var (eval 1))`)
}

func TestBreakContinueValidation(t *testing.T) {
	checkError(t, "break;", "only valid inside")
	checkError(t, "continue;", "only valid inside")
	checkError(t, "while (1) { break foo; }", "undefined label")
	// `continue` may only target a label on an iteration statement.
	checkError(t, "foo: { while(1) { continue foo; } }", "does not label an iteration")
	// Each label of a loop's labels does: `a: b: while`.
	checkParse(t, "a: b: while (1) { continue a; }", "(label a (label b (while 1 block((continue a)))))")
	checkError(t, "a: { b: while (1) { continue a; } }", "does not label an iteration")
	checkError(t, "a: b: { while (1) { continue a; } }", "does not label an iteration")
	checkParse(t, "switch (a) { case 1: break; }", "(switch a (case 1 ((break))))")
}

func TestReturnOutsideFunctionIsAnError(t *testing.T) {
	checkError(t, "return 1;", "only valid inside a function")
}

func TestDeclarationCannotBeStatementBody(t *testing.T) {
	checkError(t, "if (a) const x = 1;", "cannot be the body")
	checkError(t, "if (a) class X {}", "cannot be the body")
	checkError(t, "if (a) let [x] = y;", "cannot be the body")
	// A lexical declaration has no scope to bind into here, so `let` is an
	// ordinary identifier and what follows it is the error. That is what lets
	// `if (a) let\nx = 1` be two statements rather than a syntax error.
	checkError(t, "if (a) let x = 1;", "expected ';'")
	checkError(t, "while (a) let x = 1;", "expected ';'")
	// A generator or an async function is never a statement's body: Annex B's
	// allowance is for a plain function declaration and nothing else.
	checkError(t, "if (a) function* g() {}", "cannot be the body")
	checkError(t, "if (a) async function f() {}", "cannot be the body")
	checkError(t, "l: function* g() {}", "cannot be labelled")
	checkError(t, "l: async function f() {}", "cannot be labelled")
	// Sloppy mode permits a function declaration as an if branch, and Annex B
	// defines it as a block containing the declaration -- which is what gives
	// it the hoisting a bare declaration in that position would not have.
	checkParse(t, "if (a) function f() {}", "(if a block((decl (function f () ()))))")
}

func TestLetAsIdentifier(t *testing.T) {
	// `let` is contextual: it is a declaration only when a binding follows.
	checkParse(t, "let x = 1;", "(let (x 1))")
	checkParse(t, "let [a] = b;", "(let ((apat a) b))")
	checkParse(t, "let;", "let")
	checkParse(t, "let = 1;", "(= let 1)")
	checkParse(t, "let.a;", "(. let a)")
}

func TestForInOfHeadDoesNotTreatInAsOperator(t *testing.T) {
	// `in` inside the init of a three-clause for is still a for-in marker at
	// the top level but an operator inside parentheses.
	checkParse(t, "for (var a = (b in c); ;) d;", "(for (var (a (in b c))) _ _ d)")
	checkError(t, "for (var a in b of c) d;", "expected")
}

func TestForInOfErrors(t *testing.T) {
	// Annex B allows a var's initializer in sloppy code; strict code does not.
	checkError(t, "'use strict'; for (var a = 1 in b) c;", "cannot have an initializer")
	checkError(t, "for (var [a] = 1 in b) c;", "cannot have an initializer")
	checkError(t, "for (let a = 1 in b) c;", "cannot have an initializer")
	checkError(t, "for (var a, b in c) d;", "only one binding")
	checkError(t, "for await (a of b) c;", "expected")
}

func TestModuleMode(t *testing.T) {
	prog, err := Parse("var a = 1;", Options{Module: true})
	if err != nil {
		t.Fatal(err)
	}
	if !prog.Strict {
		t.Error("a module should be strict by default")
	}
	if !prog.Module {
		t.Error("Module should be recorded on the program")
	}
	// Module code is strict, so the strict-mode restrictions apply.
	if _, err := Parse("var eval = 1;", Options{Module: true}); err == nil {
		t.Error("expected a strict-mode error in module code")
	}
}

func TestErrorsCarryPosition(t *testing.T) {
	_, err := Parse("var a = 1;\nvar b = ;", Options{})
	if err == nil {
		t.Fatal("expected an error")
	}
	perr, ok := err.(*Error)
	if !ok {
		t.Fatalf("error is %T, want *parser.Error", err)
	}
	if perr.Line != 2 {
		t.Errorf("error on line %d, want 2", perr.Line)
	}
}

func TestDeeplyNestedInputDoesNotHang(t *testing.T) {
	// The cover grammar means parenthesized expressions must stay linear; a
	// backtracking parser would blow up exponentially here.
	src := strings.Repeat("(", 200) + "a" + strings.Repeat(")", 200)
	if _, err := Parse(src, Options{}); err != nil {
		t.Fatalf("deeply nested parentheses: %v", err)
	}
}

// fixture is a realistic script used by several benchmarks.
const fixture = `
function fib(n) {
  if (n < 2) return n;
  return fib(n - 1) + fib(n - 2);
}
class Point {
  #x = 0;
  constructor(x, y) { this.#x = x; this.y = y; }
  get x() { return this.#x; }
  distance({x, y} = {x: 0, y: 0}) {
    return Math.sqrt((this.#x - x) ** 2 + (this.y - y) ** 2);
  }
  static origin() { return new Point(0, 0); }
}
const points = [1, 2, 3].map((v, i) => new Point(v, i));
for (const p of points) {
  console.log(` + "`point ${p.x} at ${p.distance()}`" + `);
}
`

func TestFixtureParses(t *testing.T) {
	if _, err := Parse(fixture, Options{}); err != nil {
		t.Fatalf("the benchmark fixture must parse: %v", err)
	}
}

func BenchmarkParseFixture(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(fixture)))
	for b.Loop() {
		if _, err := Parse(fixture, Options{}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParseExpressionHeavy(b *testing.B) {
	src := strings.Repeat("a + b * c - d / e; ", 200)
	b.ReportAllocs()
	b.SetBytes(int64(len(src)))
	for b.Loop() {
		if _, err := Parse(src, Options{}); err != nil {
			b.Fatal(err)
		}
	}
}

// The lookaheads for an arrow and for a label settle most identifiers from
// the bytes that follow; what they cannot see past is lexed as before.
func TestArrowAndLabelLookahead(t *testing.T) {
	for _, src := range []string{
		"x => 1",
		"x /* c */ => 1",
		"x // c\n",
		"y = x /* c\n */\n+ 1",
		"l: for (;;) break l",
		"l /* c */ : for (;;) break l",
		"l // c\n: for (;;) break l",
		"l <!-- c\n: for (;;) break l",
		"l\n--> c\n: for (;;) break l",
		"l : for (;;) break l",
		"l\n\t: 1",
		"x\n/2/g",
		"x - y",
		"x --\ny",
		"x < y",
		"x = 1",
		"x == 1",
	} {
		parseOK(t, src)
	}
	// An arrow's line break is an error wherever the lookahead stops.
	checkError(t, "x\n=> 1", "")
	checkError(t, "x /* c\n */ => 1", "")
	checkError(t, "x /* c */ @", "")
	checkError(t, "x 'unterminated", "unterminated")
}
