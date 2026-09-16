package quickjs_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-quickjs/go-quickjs"
)

// evalString evaluates src and returns its value's string form.
func evalString(t *testing.T, rt *quickjs.Runtime, src string) string {
	t.Helper()
	v, err := rt.Eval(src)
	if err != nil {
		t.Fatalf("evaluating %q: %v", src, err)
	}
	return v.String()
}

// checkEval asserts that src evaluates to a value whose string form is want.
func checkEval(t *testing.T, src, want string) {
	t.Helper()
	rt := quickjs.New()
	defer rt.Close()
	if got := evalString(t, rt, src); got != want {
		t.Errorf("%s\n got: %s\nwant: %s", src, got, want)
	}
}

func TestArithmetic(t *testing.T) {
	tests := []struct{ src, want string }{
		{"1 + 2", "3"},
		{"10 - 3", "7"},
		{"6 * 7", "42"},
		{"10 / 4", "2.5"},
		{"10 % 3", "1"},
		{"2 ** 10", "1024"},
		{"-5", "-5"},
		{"1 / 0", "Infinity"},
		{"-1 / 0", "-Infinity"},
		{"0 / 0", "NaN"},
		// Modulo keeps the sign of the dividend, unlike Go's integer %.
		{"-7 % 3", "-1"},
		{"7 % -3", "1"},
		{"0.1 + 0.2", "0.30000000000000004"},
	}
	for _, tt := range tests {
		checkEval(t, tt.src, tt.want)
	}
}

func TestStringConcatenationAndCoercion(t *testing.T) {
	tests := []struct{ src, want string }{
		{`"a" + "b"`, "ab"},
		{`"n: " + 1`, "n: 1"},
		{`1 + "2"`, "12"},     // string wins
		{`1 + 2 + "3"`, "33"}, // left to right
		{`"3" + 1 + 2`, "312"},
		{`"" + null`, "null"},
		{`"" + undefined`, "undefined"},
		{`"" + true`, "true"},
		{`"" + [1,2]`, "1,2"},
		{`"" + {}`, "[object Object]"},
	}
	for _, tt := range tests {
		checkEval(t, tt.src, tt.want)
	}
}

func TestComparison(t *testing.T) {
	tests := []struct{ src, want string }{
		{"1 < 2", "true"},
		{"2 <= 2", "true"},
		{"3 > 4", "false"},
		{"1 === 1", "true"},
		{"1 === '1'", "false"},
		{"1 == '1'", "true"},
		{"null == undefined", "true"},
		{"null === undefined", "false"},
		{"NaN === NaN", "false"},
		{"NaN < 1", "false"},
		{"NaN >= 1", "false"}, // every comparison with NaN is false
		// Strings compare by code unit, so "10" < "9".
		{`"10" < "9"`, "true"},
		{"10 < 9", "false"},
	}
	for _, tt := range tests {
		checkEval(t, tt.src, tt.want)
	}
}

func TestLogicalOperators(t *testing.T) {
	tests := []struct{ src, want string }{
		{"true && false", "false"},
		{"1 && 2", "2"}, // yields the operand, not a boolean
		{"0 || 'x'", "x"},
		{"null ?? 'default'", "default"},
		{"0 ?? 'default'", "0"}, // ?? only falls through for nullish
		{"!0", "true"},
		{"!!'x'", "true"},
	}
	for _, tt := range tests {
		checkEval(t, tt.src, tt.want)
	}
}

func TestVariablesAndScoping(t *testing.T) {
	checkEval(t, "var a = 1; a", "1")
	checkEval(t, "let a = 1; a", "1")
	checkEval(t, "const a = 1; a", "1")
	checkEval(t, "let a = 1; { let a = 2; } a", "1")
	checkEval(t, "var a = 1; a = 2; a", "2")
	checkEval(t, "let a = 1; a += 5; a", "6")
	checkEval(t, "let a = 10; a -= 3; a *= 2; a", "14")
}

func TestTemporalDeadZone(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	// Reading a let binding before its declaration is a ReferenceError, not
	// undefined, which is what distinguishes let from var.
	_, err := rt.Eval(`{ x; let x = 1; }`)
	if err == nil {
		t.Fatal("expected a ReferenceError for a use before initialization")
	}
	if !strings.Contains(err.Error(), "before initialization") {
		t.Errorf("error = %v, want one about initialization", err)
	}
}

func TestConstCannotBeReassigned(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	if _, err := rt.Eval(`const a = 1; a = 2;`); err == nil {
		t.Error("expected an error when assigning to a constant")
	}
}

func TestControlFlow(t *testing.T) {
	tests := []struct{ src, want string }{
		{"if (true) 1; else 2;", "1"},
		{"if (false) 1; else 2;", "2"},
		{"let t = 0; for (let i = 0; i < 5; i++) t += i; t", "10"},
		{"let t = 0; let i = 0; while (i < 5) { t += i; i++; } t", "10"},
		{"let t = 0; let i = 0; do { t += i; i++; } while (i < 5); t", "10"},
		{"let t = 0; for (let i = 0; i < 10; i++) { if (i > 4) break; t += i; } t", "10"},
		{"let t = 0; for (let i = 0; i < 5; i++) { if (i === 2) continue; t += i; } t", "8"},
		{"let r = 'x'; switch (2) { case 1: r = 'a'; break; case 2: r = 'b'; break; } r", "b"},
		{"let r = 'x'; switch (9) { case 1: r = 'a'; break; default: r = 'd'; } r", "d"},
	}
	for _, tt := range tests {
		checkEval(t, tt.src, tt.want)
	}
}

func TestLabeledBreakAndContinue(t *testing.T) {
	checkEval(t, `
		let count = 0;
		outer: for (let i = 0; i < 3; i++) {
			for (let j = 0; j < 3; j++) {
				if (j === 1) continue outer;
				count++;
			}
		}
		count`, "3")

	checkEval(t, `
		let count = 0;
		outer: for (let i = 0; i < 3; i++) {
			for (let j = 0; j < 3; j++) {
				if (i === 1) break outer;
				count++;
			}
		}
		count`, "3")
}

func TestFunctions(t *testing.T) {
	tests := []struct{ src, want string }{
		{"function f(n) { return n * 2; } f(21)", "42"},
		{"const f = function(n) { return n + 1; }; f(1)", "2"},
		{"const f = n => n * 3; f(4)", "12"},
		{"const f = (a, b) => a + b; f(1, 2)", "3"},
		{"const f = (a, b) => { return a * b; }; f(3, 4)", "12"},
		// A function is hoisted, so it may be called before its declaration.
		{"g(); function g() { return 1; } 'ok'", "ok"},
		// Missing arguments are undefined.
		{"function f(a, b) { return b; } String(f(1))", "undefined"},
		// Extra arguments are ignored.
		{"function f(a) { return a; } f(1, 2, 3)", "1"},
		// Default parameters.
		{"function f(a = 5) { return a; } f()", "5"},
		{"function f(a = 5) { return a; } f(1)", "1"},
	}
	for _, tt := range tests {
		checkEval(t, tt.src, tt.want)
	}
}

func TestRecursion(t *testing.T) {
	checkEval(t, `
		function fib(n) { return n < 2 ? n : fib(n - 1) + fib(n - 2); }
		fib(20)`, "6765")

	checkEval(t, `
		function fact(n) { return n <= 1 ? 1 : n * fact(n - 1); }
		fact(10)`, "3628800")
}

func TestNamedFunctionExpressionCanCallItself(t *testing.T) {
	// The name of a function expression is in scope inside its own body, even
	// though it is not visible outside.
	checkEval(t, `
		const f = function fact(n) { return n <= 1 ? 1 : n * fact(n - 1); };
		fact = null;
		f(5)`, "120")
}

func TestClosures(t *testing.T) {
	checkEval(t, `
		function counter() {
			let n = 0;
			return function() { n++; return n; };
		}
		const c = counter();
		c(); c(); c()`, "3")

	// Two closures over the same binding share it.
	checkEval(t, `
		function pair() {
			let n = 0;
			return [function() { n++; }, function() { return n; }];
		}
		const p = pair();
		p[0](); p[0]();
		p[1]()`, "2")

	// Two calls produce independent bindings.
	checkEval(t, `
		function counter() { let n = 0; return () => ++n; }
		const a = counter(), b = counter();
		a(); a();
		b()`, "1")
}

func TestClosureCapturesPerIteration(t *testing.T) {
	// A let binding in a for head is fresh each iteration, so the closures
	// capture distinct values. This is the classic difference from var.
	checkEval(t, `
		const fns = [];
		for (let i = 0; i < 3; i++) fns.push(() => i);
		fns[0]() + "," + fns[1]() + "," + fns[2]()`, "0,1,2")
}

func TestObjects(t *testing.T) {
	tests := []struct{ src, want string }{
		{"const o = {a: 1}; o.a", "1"},
		{"const o = {a: 1}; o.b = 2; o.b", "2"},
		{"const o = {}; o['x'] = 5; o.x", "5"},
		{"const o = {a: 1, b: 2}; Object.keys(o).join(',')", "a,b"},
		{"const o = {a: 1}; 'a' in o", "true"},
		{"const o = {a: 1}; 'b' in o", "false"},
		{"const o = {a: 1}; delete o.a; String(o.a)", "undefined"},
		{"const k = 'dyn'; const o = {[k]: 7}; o.dyn", "7"},
		{"const o = {m() { return 3; }}; o.m()", "3"},
		{"const o = {a: 1}; o.hasOwnProperty('a')", "true"},
		// A method's `this` is the receiver.
		{"const o = {n: 5, get2() { return this.n * 2; }}; o.get2()", "10"},
	}
	for _, tt := range tests {
		checkEval(t, tt.src, tt.want)
	}
}

func TestArrays(t *testing.T) {
	tests := []struct{ src, want string }{
		{"[1,2,3].length", "3"},
		{"[1,2,3][1]", "2"},
		{"const a = [1,2,3]; a[0] = 9; a[0]", "9"},
		{"[1,2,3].join('-')", "1-2-3"},
		{"[1,2,3].map(x => x * 2).join(',')", "2,4,6"},
		{"[1,2,3,4].filter(x => x % 2 === 0).join(',')", "2,4"},
		{"[1,2,3].reduce((a, b) => a + b)", "6"},
		{"[1,2,3].reduce((a, b) => a + b, 10)", "16"},
		{"[3,1,2].sort().join(',')", "1,2,3"},
		{"[1,2,3].reverse().join(',')", "3,2,1"},
		{"[1,2,3].indexOf(2)", "1"},
		{"[1,2,3].includes(2)", "true"},
		{"[1,2,3].slice(1).join(',')", "2,3"},
		{"[1,2,3].slice(-2).join(',')", "2,3"},
		{"[1,2].concat([3,4]).join(',')", "1,2,3,4"},
		{"const a = [1]; a.push(2); a.join(',')", "1,2"},
		{"const a = [1,2]; a.pop()", "2"},
		{"[1,2,3].find(x => x > 1)", "2"},
		{"[1,2,3].some(x => x > 2)", "true"},
		{"[1,2,3].every(x => x > 0)", "true"},
		{"Array.isArray([])", "true"},
		{"Array.isArray({})", "false"},
		// Default sort is by string form, which is why 10 sorts before 9.
		{"[10, 9, 1].sort().join(',')", "1,10,9"},
		{"[10, 9, 1].sort((a, b) => a - b).join(',')", "1,9,10"},
	}
	for _, tt := range tests {
		checkEval(t, tt.src, tt.want)
	}
}

func TestForOfIteration(t *testing.T) {
	checkEval(t, "let t = 0; for (const x of [1,2,3]) t += x; t", "6")
	checkEval(t, "let s = ''; for (const c of 'abc') s += c + '.'; s", "a.b.c.")
}

func TestForInIteration(t *testing.T) {
	checkEval(t, "let s = ''; for (const k in {a:1, b:2}) s += k; s", "ab")
}

func TestDestructuring(t *testing.T) {
	tests := []struct{ src, want string }{
		{"const [a, b] = [1, 2]; a + ',' + b", "1,2"},
		{"const {x, y} = {x: 1, y: 2}; x + ',' + y", "1,2"},
		{"const {x: a} = {x: 5}; a", "5"},
		{"const [a = 9] = []; a", "9"},
		{"const {x = 7} = {}; x", "7"},
		{"const [[a], [b]] = [[1], [2]]; a + ',' + b", "1,2"},
		{"const f = ([a, b]) => a + b; f([1, 2])", "3"},
		{"const f = ({a, b}) => a * b; f({a: 3, b: 4})", "12"},
	}
	for _, tt := range tests {
		checkEval(t, tt.src, tt.want)
	}
}

func TestTemplateLiterals(t *testing.T) {
	checkEval(t, "`hello`", "hello")
	checkEval(t, "const n = 'world'; `hello ${n}`", "hello world")
	checkEval(t, "`${1 + 2}`", "3")
	checkEval(t, "const a = 1, b = 2; `${a}+${b}=${a + b}`", "1+2=3")
}

func TestExceptions(t *testing.T) {
	checkEval(t, "try { throw 1; } catch (e) { e }", "1")
	checkEval(t, "try { throw new Error('boom'); } catch (e) { e.message }", "boom")
	checkEval(t, "try { null.x; } catch (e) { 'caught' }", "caught")
	checkEval(t, "try { undefinedVariable; } catch (e) { e.name }", "ReferenceError")
	// A catch binding is optional.
	checkEval(t, "try { throw 1; } catch { 'ok' }", "ok")
}

func TestUncaughtErrorReachesGo(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()

	_, err := rt.Eval(`throw new TypeError("bad thing")`)
	if err == nil {
		t.Fatal("expected an error")
	}
	var jsErr *quickjs.Error
	if !errors.As(err, &jsErr) {
		t.Fatalf("error is %T, want *quickjs.Error", err)
	}
	if !strings.Contains(jsErr.Error(), "bad thing") {
		t.Errorf("error = %q, want it to mention the message", jsErr.Error())
	}
	// The thrown value itself is preserved, not just its message.
	name, err := jsErr.Value().Get("name")
	if err != nil {
		t.Fatal(err)
	}
	if name.String() != "TypeError" {
		t.Errorf("thrown value's name = %q, want TypeError", name.String())
	}
}

func TestThrownValueKeepsItsIdentity(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	// A thrown plain object must arrive intact rather than being stringified.
	_, err := rt.Eval(`throw {code: 42, kind: "custom"}`)
	if err == nil {
		t.Fatal("expected an error")
	}
	var jsErr *quickjs.Error
	if !errors.As(err, &jsErr) {
		t.Fatalf("error is %T, want *quickjs.Error", err)
	}
	code, _ := jsErr.Value().Get("code")
	if code.Int() != 42 {
		t.Errorf("thrown object's code = %d, want 42", code.Int())
	}
}

func TestMath(t *testing.T) {
	tests := []struct{ src, want string }{
		{"Math.abs(-5)", "5"},
		{"Math.floor(1.7)", "1"},
		{"Math.ceil(1.2)", "2"},
		{"Math.round(2.5)", "3"},
		{"Math.round(-2.5)", "-2"}, // rounds half toward +Infinity
		{"Math.max(1, 5, 3)", "5"},
		{"Math.min(1, 5, 3)", "1"},
		{"Math.sqrt(16)", "4"},
		{"Math.pow(2, 8)", "256"},
		{"Math.sign(-3)", "-1"},
		{"Math.trunc(-1.7)", "-1"},
		{"Math.max()", "-Infinity"}, // the identity for max
		{"Math.min()", "Infinity"},
	}
	for _, tt := range tests {
		checkEval(t, tt.src, tt.want)
	}
}

func TestJSON(t *testing.T) {
	tests := []struct{ src, want string }{
		{`JSON.stringify(1)`, "1"},
		{`JSON.stringify("a")`, `"a"`},
		{`JSON.stringify(null)`, "null"},
		{`JSON.stringify([1,2])`, "[1,2]"},
		{`JSON.stringify({a: 1})`, `{"a":1}`},
		{`JSON.stringify({a: undefined})`, "{}"},  // undefined properties are dropped
		{`JSON.stringify([undefined])`, "[null]"}, // but array holes become null
		{`JSON.parse("1")`, "1"},
		{`JSON.parse('{"a":1}').a`, "1"},
		{`JSON.parse('[1,2,3]').length`, "3"},
		{`JSON.stringify(JSON.parse('{"a":[1,{"b":2}]}'))`, `{"a":[1,{"b":2}]}`},
		{`JSON.stringify({a: 1}, null, 2)`, "{\n  \"a\": 1\n}"},
	}
	for _, tt := range tests {
		checkEval(t, tt.src, tt.want)
	}
}

func TestJSONRejectsCycles(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	_, err := rt.Eval(`const a = {}; a.self = a; JSON.stringify(a)`)
	if err == nil {
		t.Fatal("expected an error for a circular structure")
	}
	if !strings.Contains(err.Error(), "circular") {
		t.Errorf("error = %v, want it to mention a cycle", err)
	}
}

func TestTypeOf(t *testing.T) {
	tests := []struct{ src, want string }{
		{"typeof 1", "number"},
		{"typeof 'a'", "string"},
		{"typeof true", "boolean"},
		{"typeof undefined", "undefined"},
		{"typeof null", "object"}, // the historical mistake
		{"typeof {}", "object"},
		{"typeof []", "object"},
		{"typeof function(){}", "function"},
		{"typeof Symbol()", "symbol"},
		{"typeof 1n", "bigint"},
		// typeof on an undeclared name must not throw.
		{"typeof notDeclaredAnywhere", "undefined"},
	}
	for _, tt := range tests {
		checkEval(t, tt.src, tt.want)
	}
}

func TestBigInt(t *testing.T) {
	checkEval(t, "1n + 2n", "3")
	checkEval(t, "2n ** 64n", "18446744073709551616")
	checkEval(t, "10n / 3n", "3") // truncates toward zero
	checkEval(t, "String(9007199254740993n)", "9007199254740993")
}

func TestOptionalChaining(t *testing.T) {
	tests := []struct{ src, want string }{
		{"const o = {a: {b: 1}}; o?.a?.b", "1"},
		{"const o = null; String(o?.a)", "undefined"},
		{"const o = {a: null}; String(o.a?.b)", "undefined"},
		// The whole chain short-circuits, so .c is never evaluated.
		{"const o = null; String(o?.a.b.c)", "undefined"},
		{"const o = {f: () => 5}; o.f?.()", "5"},
		{"const o = {}; String(o.f?.())", "undefined"},
	}
	for _, tt := range tests {
		checkEval(t, tt.src, tt.want)
	}
}

// ---------------------------------------------------------------------------
// Go interoperability
// ---------------------------------------------------------------------------

func TestSetGoFunction(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()

	if err := rt.Set("add", func(a, b int) int { return a + b }); err != nil {
		t.Fatal(err)
	}
	if got := evalString(t, rt, "add(1, 2)"); got != "3" {
		t.Errorf("add(1, 2) = %s, want 3", got)
	}
	// Arguments are converted, so a JavaScript number reaches a Go int.
	if got := evalString(t, rt, "add(1.0, 2.0)"); got != "3" {
		t.Errorf("add(1.0, 2.0) = %s, want 3", got)
	}
}

func TestGoFunctionWithStrings(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	rt.Set("greet", func(name string) string { return "hello " + name })
	if got := evalString(t, rt, `greet("world")`); got != "hello world" {
		t.Errorf("got %q", got)
	}
}

func TestGoFunctionReturningError(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	rt.Set("fail", func() (int, error) { return 0, errors.New("go failure") })

	// A returned error becomes a thrown exception the script can catch.
	got := evalString(t, rt, `try { fail(); "not reached" } catch (e) { e.message }`)
	if got != "go failure" {
		t.Errorf("caught message = %q, want %q", got, "go failure")
	}
}

func TestGoFunctionReceivingSliceAndMap(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	rt.Set("sum", func(ns []int) int {
		total := 0
		for _, n := range ns {
			total += n
		}
		return total
	})
	if got := evalString(t, rt, "sum([1,2,3,4])"); got != "10" {
		t.Errorf("sum = %s, want 10", got)
	}

	rt.Set("keysOf", func(m map[string]int) int { return len(m) })
	if got := evalString(t, rt, "keysOf({a:1, b:2})"); got != "2" {
		t.Errorf("keysOf = %s, want 2", got)
	}
}

func TestSetGoValues(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()

	rt.Set("num", 42)
	rt.Set("str", "hello")
	rt.Set("flag", true)
	rt.Set("list", []int{1, 2, 3})
	rt.Set("obj", map[string]any{"a": 1})

	checks := []struct{ src, want string }{
		{"num", "42"},
		{"str", "hello"},
		{"flag", "true"},
		{"list.length", "3"},
		{"list[1]", "2"},
		{"obj.a", "1"},
	}
	for _, c := range checks {
		if got := evalString(t, rt, c.src); got != c.want {
			t.Errorf("%s = %s, want %s", c.src, got, c.want)
		}
	}
}

func TestSetStruct(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()

	type point struct {
		X int    `js:"x"`
		Y int    `js:"y"`
		Z string `js:"-"`
	}
	rt.Set("p", point{X: 1, Y: 2, Z: "hidden"})

	if got := evalString(t, rt, "p.x + ',' + p.y"); got != "1,2" {
		t.Errorf("got %s, want 1,2", got)
	}
	// A field tagged "-" is not exposed.
	if got := evalString(t, rt, "String(p.Z)"); got != "undefined" {
		t.Errorf("a skipped field leaked: %s", got)
	}
}

func TestDecodeIntoStruct(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()

	v, err := rt.Eval(`({name: "Ada", age: 36, tags: ["math", "code"]})`)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Name string   `js:"name"`
		Age  int      `js:"age"`
		Tags []string `js:"tags"`
	}
	if err := v.Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Name != "Ada" || out.Age != 36 {
		t.Errorf("decoded %+v", out)
	}
	if len(out.Tags) != 2 || out.Tags[0] != "math" {
		t.Errorf("tags = %v", out.Tags)
	}
}

func TestDecodeIntoNativeTypes(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()

	var n int
	v, _ := rt.Eval(`42`)
	if err := v.Decode(&n); err != nil || n != 42 {
		t.Errorf("int decode = %d, err %v", n, err)
	}

	var s string
	v, _ = rt.Eval(`"text"`)
	if err := v.Decode(&s); err != nil || s != "text" {
		t.Errorf("string decode = %q, err %v", s, err)
	}

	var f float64
	v, _ = rt.Eval(`1.5`)
	if err := v.Decode(&f); err != nil || f != 1.5 {
		t.Errorf("float decode = %v, err %v", f, err)
	}

	var b bool
	v, _ = rt.Eval(`true`)
	if err := v.Decode(&b); err != nil || !b {
		t.Errorf("bool decode = %v, err %v", b, err)
	}

	var xs []int
	v, _ = rt.Eval(`[1,2,3]`)
	if err := v.Decode(&xs); err != nil || len(xs) != 3 || xs[2] != 3 {
		t.Errorf("slice decode = %v, err %v", xs, err)
	}

	var m map[string]int
	v, _ = rt.Eval(`({a: 1, b: 2})`)
	if err := v.Decode(&m); err != nil || m["a"] != 1 || m["b"] != 2 {
		t.Errorf("map decode = %v, err %v", m, err)
	}
}

func TestDecodeIntoAny(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()

	v, _ := rt.Eval(`({n: 1, s: "x", b: true, a: [1,2], o: {k: "v"}})`)
	var out any
	if err := v.Decode(&out); err != nil {
		t.Fatal(err)
	}
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("decoded to %T, want map[string]any", out)
	}
	if m["n"] != float64(1) {
		t.Errorf("n = %v (%T), want float64(1)", m["n"], m["n"])
	}
	if m["s"] != "x" || m["b"] != true {
		t.Errorf("decoded %v", m)
	}
	if a, ok := m["a"].([]any); !ok || len(a) != 2 {
		t.Errorf("a = %v", m["a"])
	}
}

func TestCallJavaScriptFunctionFromGo(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()

	if _, err := rt.Eval(`function double(n) { return n * 2; }`); err != nil {
		t.Fatal(err)
	}
	fn, err := rt.Get("double")
	if err != nil {
		t.Fatal(err)
	}
	if !fn.IsFunction() {
		t.Fatal("double should be a function")
	}
	res, err := fn.Call(21)
	if err != nil {
		t.Fatal(err)
	}
	if res.Int() != 42 {
		t.Errorf("double(21) = %d, want 42", res.Int())
	}
}

func TestRoundTripThroughGoAndBack(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()

	// A Go function that calls back into JavaScript through the runtime.
	rt.Set("twice", func(r *quickjs.Runtime, fn quickjs.Value, v int) (int, error) {
		once, err := fn.Call(v)
		if err != nil {
			return 0, err
		}
		twice, err := fn.Call(once.Int())
		if err != nil {
			return 0, err
		}
		return twice.Int(), nil
	})
	if got := evalString(t, rt, `twice(n => n + 3, 1)`); got != "7" {
		t.Errorf("twice = %s, want 7", got)
	}
}

// ---------------------------------------------------------------------------
// Sandboxing
// ---------------------------------------------------------------------------

func TestContextCancellationInterruptsInfiniteLoop(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := rt.EvalContext(ctx, `while (true) {}`)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("an infinite loop should have been interrupted")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want it to wrap context.DeadlineExceeded", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("interruption took %v, which is too slow", elapsed)
	}
}

func TestStackOverflowIsAnErrorNotACrash(t *testing.T) {
	rt := quickjs.New(quickjs.WithMaxCallDepth(256))
	defer rt.Close()

	// Unbounded recursion must raise a catchable error rather than exhausting
	// the goroutine stack and killing the process.
	_, err := rt.Eval(`function f() { return f(); } f()`)
	if err == nil {
		t.Fatal("expected a stack overflow error")
	}
	if !strings.Contains(err.Error(), "call stack") {
		t.Errorf("error = %v, want one about the call stack", err)
	}
}

func TestStackOverflowIsCatchableFromScript(t *testing.T) {
	rt := quickjs.New(quickjs.WithMaxCallDepth(256))
	defer rt.Close()
	got := evalString(t, rt, `
		function f() { return f(); }
		try { f(); "not reached" } catch (e) { "caught" }`)
	if got != "caught" {
		t.Errorf("got %q, want the overflow to be catchable", got)
	}
}

func TestNoAmbientIO(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	// A fresh runtime must expose nothing that reaches outside the engine.
	for _, name := range []string{
		"require", "process", "fetch", "XMLHttpRequest", "setTimeout",
		"setInterval", "eval", "Function", "import", "globalThis.process",
	} {
		src := fmt.Sprintf("typeof %s", name)
		v, err := rt.Eval(src)
		if err != nil {
			// A parse error for `import` is fine; it is not a binding.
			continue
		}
		switch name {
		case "Function":
			// Function exists but refuses to compile from a string.
			continue
		}
		if v.String() != "undefined" {
			t.Errorf("%s is defined (%s); a sandboxed runtime should not expose it",
				name, v.String())
		}
	}
}

func TestFunctionConstructorIsDisabled(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	// Compiling code from a string would let a script escape a static review,
	// so the Function constructor refuses.
	got := evalString(t, rt, `try { new Function("return 1") } catch (e) { "blocked" }`)
	if got != "blocked" {
		t.Errorf("got %q, want the Function constructor to be blocked", got)
	}
}

// TestTopLevelLetDoesNotPersistAcrossEval documents a known gap: a top-level
// let or const is compiled as a local of the program rather than into a global
// lexical environment, so it is not visible to a later Eval on the same
// runtime. A top-level var, which becomes a property of the global object,
// behaves correctly.
func TestTopLevelLetDoesNotPersistAcrossEval(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()

	if _, err := rt.Eval(`var persists = 1; let doesNot = 2;`); err != nil {
		t.Fatal(err)
	}
	if got := evalString(t, rt, `persists`); got != "1" {
		t.Errorf("a top-level var should persist across Eval, got %s", got)
	}
	if got := evalString(t, rt, `typeof doesNot`); got != "undefined" {
		t.Logf("top-level let now persists across Eval (%s); "+
			"the global lexical environment must have been implemented, "+
			"so this test should be updated to assert that", got)
	}
}

func TestRuntimesAreIsolated(t *testing.T) {
	a := quickjs.New()
	defer a.Close()
	b := quickjs.New()
	defer b.Close()

	if _, err := a.Eval(`globalThis.leaked = "from a"`); err != nil {
		t.Fatal(err)
	}
	if got := evalString(t, b, `typeof leaked`); got != "undefined" {
		t.Errorf("a global leaked between runtimes: %s", got)
	}
}

func TestClosedRuntimeReportsErrClosed(t *testing.T) {
	rt := quickjs.New()
	rt.Close()
	if _, err := rt.Eval("1"); !errors.Is(err, quickjs.ErrClosed) {
		t.Errorf("error = %v, want ErrClosed", err)
	}
}

func TestSyntaxErrorIsReported(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	_, err := rt.Eval(`function ( {`)
	if err == nil {
		t.Fatal("expected a syntax error")
	}
	var se *quickjs.SyntaxError
	if !errors.As(err, &se) {
		t.Errorf("error is %T, want *quickjs.SyntaxError", err)
	}
}

// ---------------------------------------------------------------------------
// Lone surrogates, end to end
// ---------------------------------------------------------------------------

func TestLoneSurrogatesThroughTheEngine(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()

	tests := []struct{ src, want string }{
		// A lone surrogate is one code unit.
		{`"\uD83D".length`, "1"},
		{`"\uD83D".charCodeAt(0)`, "55357"},
		{`String.fromCharCode(0xD83D).length`, "1"},
		{`String.fromCharCode(0xD83D).charCodeAt(0)`, "55357"},
		// A valid pair is two code units but one code point.
		{`"😀".length`, "2"},
		{`"😀".codePointAt(0)`, "128512"},
		{`[..."😀"].length`, "1"}, // iteration is by code point
		// Splitting a pair yields two lone surrogates that rejoin correctly.
		{`("😀"[0] + "😀"[1]).codePointAt(0)`, "128512"},
		{`"😀"[0].charCodeAt(0)`, "55357"},
		{`"😀"[1].charCodeAt(0)`, "56832"}, // 0xDE00, the low half of U+1F600
		// Well-formedness is observable.
		{`"\uD83D".isWellFormed()`, "false"},
		{`"😀".isWellFormed()`, "true"},
		{`"\uD83D".toWellFormed().charCodeAt(0)`, "65533"}, // U+FFFD
		// Comparison and concatenation preserve them.
		{`"\uD83D" === "\uD83D"`, "true"},
		{`"\uD83D" === "\uDE00"`, "false"},
		{`("a" + "\uD83D" + "b").length`, "3"},
		{`("a" + "\uD83D" + "b").charCodeAt(1)`, "55357"},
	}
	for _, tt := range tests {
		if got := evalString(t, rt, tt.src); got != tt.want {
			t.Errorf("%s = %s, want %s", tt.src, got, tt.want)
		}
	}
}

func TestLoneSurrogateSurvivesGoRoundTrip(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()

	// From JavaScript into Go and back must not lose the code unit.
	v, err := rt.Eval(`String.fromCharCode(0x61, 0xD83D, 0x62)`)
	if err != nil {
		t.Fatal(err)
	}
	var s string
	if err := v.Decode(&s); err != nil {
		t.Fatal(err)
	}
	rt.Set("roundTripped", s)
	if got := evalString(t, rt, "roundTripped.length"); got != "3" {
		t.Errorf("length after a Go round trip = %s, want 3", got)
	}
	if got := evalString(t, rt, "roundTripped.charCodeAt(1)"); got != "55357" {
		t.Errorf("surrogate after a Go round trip = %s, want 55357", got)
	}
}

func TestJSONEscapesLoneSurrogates(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	// A lone surrogate has no valid UTF-8 form, so it must be escaped rather
	// than emitted raw or replaced.
	got := evalString(t, rt, `JSON.stringify(String.fromCharCode(0xD83D))`)
	if got != `"\ud83d"` {
		t.Errorf("JSON.stringify of a lone surrogate = %s, want %q", got, `"\ud83d"`)
	}
	// And it round trips back through parse.
	got = evalString(t, rt, `JSON.parse(JSON.stringify(String.fromCharCode(0xD83D))).charCodeAt(0)`)
	if got != "55357" {
		t.Errorf("round trip through JSON = %s, want 55357", got)
	}
}

// ---------------------------------------------------------------------------
// Benchmarks
// ---------------------------------------------------------------------------

func BenchmarkEvalArithmetic(b *testing.B) {
	rt := quickjs.New()
	defer rt.Close()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := rt.Eval(`1 + 2 * 3 - 4 / 2`); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFibonacci(b *testing.B) {
	rt := quickjs.New()
	defer rt.Close()
	if _, err := rt.Eval(`function fib(n) { return n < 2 ? n : fib(n-1) + fib(n-2); }`); err != nil {
		b.Fatal(err)
	}
	fn, err := rt.Get("fib")
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := fn.Call(20); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPropertyAccess(b *testing.B) {
	rt := quickjs.New()
	defer rt.Close()
	// `var` is used rather than `const` because a top-level var becomes a
	// property of the global object and so outlives the Eval that declared it.
	// See TestTopLevelLetDoesNotPersistAcrossEval.
	if _, err := rt.Eval(`var o = {a: 1, b: 2, c: 3};`); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := rt.Eval(`o.a + o.b + o.c`); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCallGoFunction(b *testing.B) {
	rt := quickjs.New()
	defer rt.Close()
	rt.Set("add", func(a, b int) int { return a + b })
	b.ReportAllocs()
	for b.Loop() {
		if _, err := rt.Eval(`add(1, 2)`); err != nil {
			b.Fatal(err)
		}
	}
}

func TestClasses(t *testing.T) {
	tests := []struct{ src, want string }{
		{`class A { constructor(x) { this.x = x; } } new A(5).x`, "5"},
		{`class A { m() { return 3; } } new A().m()`, "3"},
		{`class A { get v() { return 7; } } new A().v`, "7"},
		{`class A { set v(n) { this.n = n; } } const a = new A(); a.v = 4; a.n`, "4"},
		{`class A { static s() { return 8; } } A.s()`, "8"},
		// Methods live on the prototype, so they are shared between instances.
		{`class A { m() {} } const a = new A(), b = new A(); a.m === b.m`, "true"},
		{`class A {} new A() instanceof A`, "true"},
		{`class A {} Object.getPrototypeOf(new A()) === A.prototype`, "true"},
		{`class A { constructor() { this.v = 1; } } new A().constructor === A`, "true"},
		// A class expression may be anonymous.
		{`const C = class { m() { return 2; } }; new C().m()`, "2"},
	}
	for _, tt := range tests {
		checkEval(t, tt.src, tt.want)
	}
}

func TestConstructorReturningObjectOverridesThis(t *testing.T) {
	// A constructor that returns an object replaces the newly created one;
	// returning anything else is ignored.
	checkEval(t, `function F() { this.a = 1; return {a: 2}; } new F().a`, "2")
	checkEval(t, `function F() { this.a = 1; return 5; } new F().a`, "1")
}

func TestPrototypeChain(t *testing.T) {
	checkEval(t, `
		function Base() {}
		Base.prototype.greet = function() { return "hi"; };
		function Derived() {}
		Derived.prototype = Object.create(Base.prototype);
		new Derived().greet()`, "hi")
}

func TestRestParameters(t *testing.T) {
	tests := []struct{ src, want string }{
		{`function f(...a) { return a.length; } f(1,2,3)`, "3"},
		{`function f(a, ...rest) { return rest.join(","); } f(1,2,3)`, "2,3"},
		{`function f(...a) { return a.length; } f()`, "0"},
		{`function f(...a) { return Array.isArray(a); } f(1)`, "true"},
		// A rest parameter is excluded from length.
		{`function f(a, ...rest) {} f.length`, "1"},
		{`const f = (...a) => a.length; f(1,2)`, "2"},
	}
	for _, tt := range tests {
		checkEval(t, tt.src, tt.want)
	}
}

func TestSpreadArguments(t *testing.T) {
	tests := []struct{ src, want string }{
		{`Math.max(...[1,5,2])`, "5"},
		{`function f(a,b,c) { return a+b+c; } f(...[1,2,3])`, "6"},
		{`function f(a,b,c) { return a+b+c; } f(1, ...[2,3])`, "6"},
		{`function f(...a) { return a.join(","); } f(...[1,2], 3, ...[4])`, "1,2,3,4"},
		{`[...[1,2],3].join(",")`, "1,2,3"},
		{`[..."abc"].join("-")`, "a-b-c"},
		// A spread call on a method still binds `this`.
		{`const o = {n: 2, f(a) { return this.n * a; }}; o.f(...[3])`, "6"},
		{`new (class { constructor(...a) { this.n = a.length; } })(1,2).n`, "2"},
	}
	for _, tt := range tests {
		checkEval(t, tt.src, tt.want)
	}
}

func TestDestructuringRest(t *testing.T) {
	tests := []struct{ src, want string }{
		{`const [a, ...r] = [1,2,3]; r.join(",")`, "2,3"},
		{`const [a, ...r] = [1]; r.length`, "0"},
		{`const {a, ...r} = {a:1,b:2,c:3}; JSON.stringify(r)`, `{"b":2,"c":3}`},
		{`const {a, ...r} = {a:1}; JSON.stringify(r)`, "{}"},
		{`function f([a, ...r]) { return r.length; } f([1,2,3])`, "2"},
		{`function f({a, ...r}) { return Object.keys(r).join(","); } f({a:1,b:2})`, "b"},
	}
	for _, tt := range tests {
		checkEval(t, tt.src, tt.want)
	}
}

func TestArgumentsObject(t *testing.T) {
	tests := []struct{ src, want string }{
		{`(function() { return arguments.length; })(1,2,3)`, "3"},
		{`(function() { return arguments[1]; })(1,2,3)`, "2"},
		{`(function() { return [...arguments].join(","); })(1,2)`, "1,2"},
		{`(function(a) { return arguments.length; })()`, "0"},
		// An arrow has no arguments of its own and sees the enclosing one.
		{`(function() { return (() => arguments.length)(); })(1,2)`, "2"},
	}
	for _, tt := range tests {
		checkEval(t, tt.src, tt.want)
	}
}

func TestTryFinally(t *testing.T) {
	tests := []struct{ src, want string }{
		{`let x = 0; try { x = 1; } finally { x = 2; } x`, "2"},
		{`try { 1 } finally { 2 }`, "2"},
		{`let x = 0; try { throw 1 } catch (e) { x = e } finally { x += 10 } x`, "11"},
		// The finally runs before the function returns, and does not change
		// the returned value.
		{`var log = ""; function f() { try { return "r"; } finally { log = "f"; } } f() + log`, "rf"},
		{`function f() { try { return 1; } finally { } } f()`, "1"},
		// A finally that a break passes through still runs.
		{`var log = ""; for (;;) { try { break } finally { log = "f" } } log`, "f"},
		{`var n = 0; for (let i = 0; i < 3; i++) { try { continue } finally { n++ } } n`, "3"},
	}
	for _, tt := range tests {
		checkEval(t, tt.src, tt.want)
	}
}

func TestTryFinallyRethrows(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	// An exception must survive the finally clause.
	var log string
	rt.Set("record", func(s string) { log = s })
	_, err := rt.Eval(`try { throw new Error("boom") } finally { record("ran") }`)
	if err == nil {
		t.Fatal("the exception should have been rethrown after the finally")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error = %v, want the original exception", err)
	}
	if log != "ran" {
		t.Errorf("the finally clause did not run (log = %q)", log)
	}
}

func TestHoistedFunctionSeesLaterLet(t *testing.T) {
	// All bindings of a scope exist before any code runs, so a hoisted
	// function that refers to a later let must resolve to that binding rather
	// than to a global.
	checkEval(t, `
		function f() { return later; }
		let later = "bound";
		f()`, "bound")
}

func TestClassInheritance(t *testing.T) {
	tests := []struct{ src, want string }{
		{`class A { m() { return 1; } } class B extends A {} new B().m()`, "1"},
		{`class A { constructor(x) { this.x = x; } }
		  class B extends A { constructor(x) { super(x * 2); } }
		  new B(3).x`, "6"},
		// An implicit derived constructor forwards its arguments.
		{`class A { constructor(x) { this.x = x; } } class B extends A {} new B(4).x`, "4"},
		{`class A {} class B extends A {} new B() instanceof A`, "true"},
		{`class A {} class B extends A {} new B() instanceof B`, "true"},
		// Static members are inherited, which has no analogue in most class
		// systems.
		{`class A { static s() { return 1; } } class B extends A {} B.s()`, "1"},
		{`class A {} class B extends A {} Object.getPrototypeOf(B) === A`, "true"},
		{`class A {} class B extends A {} Object.getPrototypeOf(B.prototype) === A.prototype`, "true"},
	}
	for _, tt := range tests {
		checkEval(t, tt.src, tt.want)
	}
}

func TestSuper(t *testing.T) {
	tests := []struct{ src, want string }{
		{`class A { m() { return "a"; } }
		  class B extends A { m() { return super.m() + "b"; } }
		  new B().m()`, "ab"},
		{`class A { get v() { return 1; } }
		  class B extends A { get v() { return super.v + 1; } }
		  new B().v`, "2"},
		// super.m() keeps the current receiver as `this`.
		{`class A { m() { return this.n; } }
		  class B extends A { constructor() { super(); this.n = 7; } m() { return super.m(); } }
		  new B().m()`, "7"},
		{`class A { constructor() { this.v = 1; } }
		  class B extends A { constructor() { super(); this.w = 2; } }
		  const b = new B(); b.v + b.w`, "3"},
	}
	for _, tt := range tests {
		checkEval(t, tt.src, tt.want)
	}
}

func TestClassFields(t *testing.T) {
	tests := []struct{ src, want string }{
		{`class A { x = 1; } new A().x`, "1"},
		{`class A { x; } String(new A().x)`, "undefined"},
		// A later field may refer to an earlier one through `this`.
		{`class A { x = 1; y = this.x + 1; } new A().y`, "2"},
		{`class A { static s = 7; } A.s`, "7"},
		{`class A { static { A.z = 3; } } A.z`, "3"},
		// Fields are own properties of the instance, not the prototype.
		{`class A { x = 1; } A.prototype.hasOwnProperty("x")`, "false"},
		{`class A { x = 1; } new A().hasOwnProperty("x")`, "true"},
		// A derived class's fields are initialized after super() runs.
		{`class A { constructor() { this.fromBase = 1; } }
		  class B extends A { own = this.fromBase + 1; }
		  new B().own`, "2"},
	}
	for _, tt := range tests {
		checkEval(t, tt.src, tt.want)
	}
}

func TestPrivateClassMembers(t *testing.T) {
	tests := []struct{ src, want string }{
		{`class A { #p = 5; get() { return this.#p; } } new A().get()`, "5"},
		{`class A { #m() { return 4; } call() { return this.#m(); } } new A().call()`, "4"},
		{`class Counter { #n = 0; inc() { this.#n++; return this.#n; } }
		  const c = new Counter(); c.inc(); c.inc()`, "2"},
		// A private member is invisible to every reflective operation.
		{`class A { #p = 5; } Object.keys(new A()).length`, "0"},
		{`class A { #p = 5; } JSON.stringify(new A())`, "{}"},
		{`class A { #p = 5; } Object.getOwnPropertyNames(new A()).length`, "0"},
	}
	for _, tt := range tests {
		checkEval(t, tt.src, tt.want)
	}
}

func TestPrivateMemberOnWrongObjectThrows(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	// Reaching for a private field on an object that does not have one is an
	// error, not undefined: the field is part of the class's shape.
	_, err := rt.Eval(`
		class A { #p = 1; static read(o) { return o.#p; } }
		A.read({})`)
	if err == nil {
		t.Fatal("expected a TypeError for a private member on a foreign object")
	}
	if !strings.Contains(err.Error(), "private") {
		t.Errorf("error = %v, want one mentioning the private member", err)
	}
}

func TestMap(t *testing.T) {
	tests := []struct{ src, want string }{
		{`new Map().set(1,2).get(1)`, "2"},
		{`new Map([[1,"a"],[2,"b"]]).size`, "2"},
		{`new Map([[1,"a"]]).has(1)`, "true"},
		{`String(new Map().get("missing"))`, "undefined"},
		{`const m = new Map([[1,1]]); m.delete(1); m.size`, "0"},
		{`new Map([[1,"a"],[2,"b"]]) .keys().next().value`, "1"},
		// Iteration follows insertion order, which is observable.
		{`const m = new Map(); m.set("z",1); m.set("a",2); [...m.keys()].join(",")`, "z,a"},
		// Keys are compared by SameValueZero: NaN matches itself and -0 is +0.
		{`const m = new Map(); m.set(NaN,"n"); m.get(NaN)`, "n"},
		{`const m = new Map(); m.set(0,"z"); m.get(-0)`, "z"},
		// Objects are keyed by identity, not by contents.
		{`const m = new Map(); m.set({}, 1); String(m.get({}))`, "undefined"},
		{`const k = {}; const m = new Map(); m.set(k,1); m.get(k)`, "1"},
		{`let s = ""; new Map([[1,"a"],[2,"b"]]).forEach((v,k) => s += k+v); s`, "1a2b"},
		{`Object.prototype.toString.call(new Map())`, "[object Map]"},
		{`let n = 0; for (const [k,v] of new Map([[1,2],[3,4]])) n += k+v; n`, "10"},
	}
	for _, tt := range tests {
		checkEval(t, tt.src, tt.want)
	}
}

func TestSet(t *testing.T) {
	tests := []struct{ src, want string }{
		{`new Set([1,2,2,3]).size`, "3"},
		{`[...new Set([3,1,3,2])].join(",")`, "3,1,2"},
		{`new Set([1]).has(1)`, "true"},
		{`const s = new Set(); s.add(1).add(2); s.size`, "2"},
		{`const s = new Set([1]); s.delete(1); s.size`, "0"},
		{`Object.prototype.toString.call(new Set())`, "[object Set]"},
		{`[...new Set("hello")].join("")`, "helo"},
	}
	for _, tt := range tests {
		checkEval(t, tt.src, tt.want)
	}
}

func TestWeakCollections(t *testing.T) {
	checkEval(t, `const wm = new WeakMap(); const k = {}; wm.set(k, 5); wm.get(k)`, "5")
	checkEval(t, `const ws = new WeakSet(); const k = {}; ws.add(k); ws.has(k)`, "true")
	// A primitive key is rejected, which is what distinguishes the weak forms.
	checkEval(t, `try { new WeakMap().set(1, 2); "no" } catch (e) { "rejected" }`, "rejected")
}

func TestDate(t *testing.T) {
	tests := []struct{ src, want string }{
		{`new Date(0).getTime()`, "0"},
		{`new Date(0).toISOString()`, "1970-01-01T00:00:00.000Z"},
		{`new Date("2024-03-15T10:30:00Z").toISOString()`, "2024-03-15T10:30:00.000Z"},
		{`new Date(Date.UTC(2024, 2, 15)).toISOString()`, "2024-03-15T00:00:00.000Z"},
		// Months are zero-based.
		{`new Date(Date.UTC(2024,0,1)).getUTCMonth()`, "0"},
		{`new Date(Date.UTC(2024,0,15)).getUTCDate()`, "15"},
		{`new Date(Date.UTC(2020,0,1)).getUTCDay()`, "3"},
		// Out-of-range components roll over.
		{`new Date(Date.UTC(2024,11,32)).toISOString()`, "2025-01-01T00:00:00.000Z"},
		{`new Date("nonsense").getTime()`, "NaN"},
		{`String(new Date("nonsense"))`, "Invalid Date"},
		{`Date.parse("2024-01-01T00:00:00Z")`, "1704067200000"},
		{`const d = new Date(0); d.setUTCFullYear(2000); d.getUTCFullYear()`, "2000"},
		{`JSON.stringify({d: new Date(0)})`, `{"d":"1970-01-01T00:00:00.000Z"}`},
		// Date is the only built-in whose default coercion hint is string.
		{`new Date(0) - 0`, "0"},
		{`typeof (new Date(0) + "")`, "string"},
		{`Object.prototype.toString.call(new Date())`, "[object Date]"},
	}
	for _, tt := range tests {
		checkEval(t, tt.src, tt.want)
	}
}

func TestDateUsesTheHostClock(t *testing.T) {
	// A sandboxed runtime should not read the wall clock unless the host lets
	// it, so the clock is injectable.
	rt := quickjs.New()
	defer rt.Close()
	rt.SetClock(func() time.Time { return time.Unix(1700000000, 0) })
	if got := evalString(t, rt, `Date.now()`); got != "1700000000000" {
		t.Errorf("Date.now() = %s, want the injected time", got)
	}
	if got := evalString(t, rt, `new Date().toISOString()`); got != "2023-11-14T22:13:20.000Z" {
		t.Errorf("new Date() = %s, want the injected time", got)
	}
}
