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

// A computed key is a complete expression, so `in` is the operator there even
// in a for head, where `in` otherwise separates the binding from the subject.
func TestComputedKeyAllowsIn(t *testing.T) {
	tests := []struct{ src, want string }{
		{`const o = {["a" in {a: 1}]: 7}; o.true`, "7"},
		{`class C { ["x" in {x: 0}]() { return 5 } } new C().true()`, "5"},
		{`class C { static ["x" in {}] = 3 } C.false`, "3"},
		{`let s = ""; for (const k in {["a" in {}]: 1}) s += k; s`, "false"},
		{`let n = 0; for ({["a" in {}]: n} of [{false: 8}]); n`, "8"},
		// The suppression is restored afterwards, so the head still reads as a
		// for-in rather than as a comparison.
		{`let s = ""; for (var i in {a: 1, b: 2}) s += i; s`, "ab"},
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
	// A fresh runtime must expose nothing that reaches outside the engine. It
	// does expose eval and Function, which grant no capability a script does
	// not already have, and which WithoutCodeGeneration removes.
	for _, name := range []string{
		"require", "process", "fetch", "XMLHttpRequest", "setTimeout",
		"setInterval", "Deno", "Bun", "__dirname", "module", "exports",
	} {
		v, err := rt.Eval(fmt.Sprintf("typeof %s", name))
		if err != nil {
			t.Errorf("probing %s: %v", name, err)
			continue
		}
		if v.String() != "undefined" {
			t.Errorf("%s is defined (%s); a sandboxed runtime should not expose it",
				name, v.String())
		}
	}
}

func TestCodeGenerationCanBeDisabled(t *testing.T) {
	// eval and Function are available by default, because a conformant engine
	// has them and they grant nothing new. A host that audits source before
	// running it can take them away.
	rt := quickjs.New()
	defer rt.Close()
	if got := evalString(t, rt, `eval("1+1")`); got != "2" {
		t.Errorf("eval = %s, want 2", got)
	}
	if got := evalString(t, rt, `new Function("a", "return a*2")(21)`); got != "42" {
		t.Errorf("Function = %s, want 42", got)
	}

	locked := quickjs.New(quickjs.WithoutCodeGeneration())
	defer locked.Close()
	if got := evalString(t, locked, `typeof eval`); got != "undefined" {
		t.Errorf("eval is still present in a locked runtime: %s", got)
	}
	if got := evalString(t, locked,
		`try { new Function("return 1"); "not blocked" } catch (e) { "blocked" }`); got != "blocked" {
		t.Errorf("Function constructor = %s, want it blocked", got)
	}
}

func TestEvalRunsInGlobalScope(t *testing.T) {
	// Only indirect-eval semantics are implemented: evaluated code sees the
	// globals but not the calling function's locals.
	rt := quickjs.New()
	defer rt.Close()
	if got := evalString(t, rt, `eval("var fromEval = 5"); fromEval`); got != "5" {
		t.Errorf("a var declared in eval should reach the global scope, got %s", got)
	}
	// Anything that is not a string passes through untouched.
	if got := evalString(t, rt, `eval(42)`); got != "42" {
		t.Errorf("eval(42) = %s, want 42", got)
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
		// The try block's value is the statement's; a finally clause runs for
		// its effects and its value is discarded.
		{`try { 1 } finally { 2 }`, "1"},
		{`1; try { } finally { 2 }`, "undefined"},
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

func TestRegExp(t *testing.T) {
	tests := []struct{ src, want string }{
		{`/ab/.test("xaby")`, "true"},
		{`/ab/.test("xyz")`, "false"},
		{`/a(b)c/.exec("abc")[1]`, "b"},
		{`/a(b)c/.exec("abc").index`, "0"},
		{`String(/nope/.exec("abc"))`, "null"},
		{`String(/ab/gi)`, "/ab/gi"},
		{`/ab/gi.flags`, "gi"},
		{`/ab/g.source`, "ab"},
		{`/ab/g.global`, "true"},
		{`/ab/.global`, "false"},
		{`new RegExp("a+", "g").test("aaa")`, "true"},
		// A literal produces a fresh object each evaluation, so lastIndex is
		// not shared between them.
		{`/a/g.lastIndex`, "0"},
		{`const re = /a/g; re.test("aa"); re.lastIndex`, "1"},
		{`/A/i.test("a")`, "true"},
		{`/\u{1F600}/u.test("😀")`, "true"},
		{`/(?<y>\d{4})/.exec("2024").groups.y`, "2024"},
		// A group that did not participate is undefined, not empty.
		{`String(/(a)?b/.exec("b")[1])`, "undefined"},
	}
	for _, tt := range tests {
		checkEval(t, tt.src, tt.want)
	}
}

func TestStringRegExpMethods(t *testing.T) {
	tests := []struct{ src, want string }{
		{`"2024-03-15".match(/(\d+)-(\d+)-(\d+)/).slice(1).join("/")`, "2024/03/15"},
		{`"a1b2c3".match(/\d/g).join("")`, "123"},
		{`String("abc".match(/z/))`, "null"},
		{`"hello world".replace(/o/g, "0")`, "hell0 w0rld"},
		{`"hello".replace(/l/, "L")`, "heLlo"},
		{`"John Smith".replace(/(\w+) (\w+)/, "$2 $1")`, "Smith John"},
		{`"aaa".replace(/a/g, (m, i) => i)`, "012"},
		{`"a-b-c".split(/-/).join(",")`, "a,b,c"},
		{`"abc".search(/b/)`, "1"},
		{`"abc".search(/z/)`, "-1"},
		{`[..."a1b2".matchAll(/\d/g)].length`, "2"},
		{`[..."a1b2".matchAll(/\d/g)][0][0]`, "1"},
		// A plain string separator is matched literally, not as a pattern.
		{`"a.b".split(".").length`, "2"},
		{`"test".replace("t", "T")`, "Test"},
		{`"a1b".replace(/\d/, "$&$&")`, "a11b"},
	}
	for _, tt := range tests {
		checkEval(t, tt.src, tt.want)
	}
}

func TestRegExpSyntaxErrorIsThrown(t *testing.T) {
	checkEval(t, `try { new RegExp("(") } catch (e) { e.name }`, "SyntaxError")
}

func TestCatastrophicRegExpIsBounded(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	// A backtracking engine has an exponential worst case. A hostile pattern
	// must fail rather than stall the host.
	done := make(chan struct{})
	go func() {
		defer close(done)
		rt.Eval(`/(a+)+b/.test("` + strings.Repeat("a", 60) + `")`)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("a catastrophic pattern did not terminate")
	}
}

// evalThen runs setup, then reads an expression once the microtask queue has
// drained, which is how an asynchronous result becomes observable.
func evalThen(t *testing.T, setup, read string) string {
	t.Helper()
	rt := quickjs.New()
	defer rt.Close()
	if _, err := rt.Eval(setup); err != nil {
		t.Fatalf("setup %q: %v", setup, err)
	}
	return evalString(t, rt, read)
}

func checkAsync(t *testing.T, setup, read, want string) {
	t.Helper()
	if got := evalThen(t, setup, read); got != want {
		t.Errorf("%s\nreading %s\n got: %s\nwant: %s", setup, read, got, want)
	}
}

func TestGenerators(t *testing.T) {
	tests := []struct{ src, want string }{
		{`function* g() { yield 1; yield 2; } [...g()].join(",")`, "1,2"},
		{`function* g() { yield 1; return 9; } const it = g(); it.next().value + "," + it.next().value`, "1,9"},
		{`function* g() { yield 1; } const it = g(); it.next(); it.next().done`, "true"},
		{`function* g() { yield 1; } let s = ""; for (const v of g()) s += v; s`, "1"},
		{`function* g() {} g().next().done`, "true"},
		// A value sent in becomes the result of the yield that suspended.
		{`function* g() { const x = yield 1; yield x * 2; } const it = g(); it.next(); it.next(5).value`, "10"},
		// State persists across suspensions, including loop variables.
		{`function* g() { let t = 0; for (let i = 0; i < 3; i++) t += yield i; return t; }
		  const it = g(); it.next(); it.next(1); it.next(2); it.next(3).value`, "6"},
		// A generator is its own iterator.
		{`function* g() { yield 1; } const it = g(); it[Symbol.iterator]() === it`, "true"},
	}
	for _, tt := range tests {
		checkEval(t, tt.src, tt.want)
	}
}

func TestGeneratorThrowAndReturn(t *testing.T) {
	// throw() raises at the suspension point, so a try inside the body catches
	// it.
	checkEval(t, `
		function* g() { try { yield 1; } catch (e) { yield "caught:" + e; } }
		const it = g(); it.next(); it.throw("x").value`, "caught:x")
	// return() finishes the generator.
	checkEval(t, `
		function* g() { yield 1; yield 2; }
		const it = g(); it.next(); const r = it.return(9);
		r.value + "," + r.done`, "9,true")
	checkEval(t, `
		function* g() { yield 1; }
		const it = g(); it.return(); it.next().done`, "true")
}

func TestGeneratorDelegation(t *testing.T) {
	checkEval(t, `
		function* inner() { yield 1; yield 2; }
		function* outer() { yield* inner(); yield 3; }
		[...outer()].join(",")`, "1,2,3")
	checkEval(t, `function* g() { yield* [1,2]; } [...g()].join(",")`, "1,2")
	checkEval(t, `function* g() { yield* "ab"; } [...g()].join(",")`, "a,b")
}

func TestPromise(t *testing.T) {
	checkAsync(t, `var out = ""; Promise.resolve(1).then(v => out = "got" + v)`, `out`, "got1")
	checkAsync(t, `var r = ""; Promise.reject("x").catch(e => r = "c" + e)`, `r`, "cx")
	checkAsync(t, `var r = []; Promise.resolve().then(() => r.push(1)).then(() => r.push(2))`,
		`r.join(",")`, "1,2")
	// A rejection propagates through a then that has no rejection handler.
	checkAsync(t, `var r = ""; Promise.reject("e").then(v => r = "no").catch(e => r = "yes" + e)`,
		`r`, "yese")
	// A throwing handler rejects the promise it resolves.
	checkAsync(t, `var r = ""; Promise.resolve(1).then(() => { throw "t" }).catch(e => r = "got" + e)`,
		`r`, "gott")
	// Resolving with a promise adopts it rather than nesting.
	checkAsync(t, `var r = ""; Promise.resolve(Promise.resolve(5)).then(v => r = v)`, `r`, "5")
	checkAsync(t, `var r = ""; new Promise(res => res(7)).then(v => r = v)`, `r`, "7")
	checkAsync(t, `var r = ""; new Promise((_, rej) => rej("bad")).catch(e => r = e)`, `r`, "bad")
	checkAsync(t, `var r = ""; Promise.resolve(1).finally(() => r += "f").then(v => r += v)`, `r`, "f1")
}

func TestPromiseOrderingIsAlwaysAsynchronous(t *testing.T) {
	// A then callback must never run before the code that registered it has
	// finished. This is the guarantee the whole design exists for.
	checkAsync(t,
		`var order = []; Promise.resolve().then(() => order.push("micro")); order.push("sync")`,
		`order.join(",")`, "sync,micro")
}

func TestPromiseCombinators(t *testing.T) {
	checkAsync(t, `var r = ""; Promise.all([1, Promise.resolve(2)]).then(v => r = v.join(","))`,
		`r`, "1,2")
	checkAsync(t, `var r = ""; Promise.all([Promise.reject("e"), 1]).catch(e => r = "c" + e)`,
		`r`, "ce")
	checkAsync(t, `var r = ""; Promise.all([]).then(v => r = v.length)`, `r`, "0")
	checkAsync(t, `var r = ""; Promise.race([Promise.resolve("a")]).then(v => r = v)`, `r`, "a")
	checkAsync(t,
		`var r = ""; Promise.allSettled([Promise.resolve(1), Promise.reject(2)])
		   .then(v => r = v.map(x => x.status).join(","))`,
		`r`, "fulfilled,rejected")
	checkAsync(t, `var r = ""; Promise.any([Promise.reject(1), Promise.resolve(2)]).then(v => r = v)`,
		`r`, "2")
}

func TestAsyncFunctions(t *testing.T) {
	checkEval(t, `async function f() { return 1; } typeof f().then`, "function")
	checkAsync(t, `var r = ""; async function f() { r = await Promise.resolve("v"); } f()`, `r`, "v")
	// Awaiting a plain value works too.
	checkAsync(t, `var r = ""; (async () => { r = await 5; })()`, `r`, "5")
	checkAsync(t, `var r = ""; async function f() { return 42; } f().then(v => r = v)`, `r`, "42")
	// An exception escaping the body rejects the promise.
	checkAsync(t, `var r = ""; async function f() { throw new Error("boom"); } f().catch(e => r = e.message)`,
		`r`, "boom")
	// A rejected await throws at the await, so a try can catch it.
	checkAsync(t,
		`var r = ""; (async () => { try { await Promise.reject("e"); } catch (x) { r = "caught" + x; } })()`,
		`r`, "caughte")
	// Awaiting in a loop keeps the frame's state.
	checkAsync(t,
		`var r = ""; async function f() { let s = 0; for (const x of [1,2,3]) s += await x; r = s; } f()`,
		`r`, "6")
	checkAsync(t,
		`var r = ""; async function f() { const a = await 1, b = await 2; r = a + b; } f()`, `r`, "3")
}

func TestProxy(t *testing.T) {
	tests := []struct{ src, want string }{
		{`new Proxy({a:1}, {}).a`, "1"},
		{`new Proxy({}, {get: (t,k) => "got:" + k}).x`, "got:x"},
		{`const p = new Proxy({}, {set: (t,k,v) => { t[k] = v*2; return true }}); p.n = 5; p.n`, "10"},
		{`"x" in new Proxy({}, {has: () => true})`, "true"},
		{`Object.keys(new Proxy({a:1,b:2}, {})).join(",")`, "a,b"},
		{`new Proxy(function(){ return 1 }, {})()`, "1"},
		{`new Proxy(function(){}, {apply: () => 42})()`, "42"},
		{`new (new Proxy(class { constructor() { this.v = 1 } }, {}))().v`, "1"},
		// A trap that forwards sees the untrapped behaviour.
		{`new Proxy({a:5}, {get: (t,k) => Reflect.get(t,k)}).a`, "5"},
	}
	for _, tt := range tests {
		checkEval(t, tt.src, tt.want)
	}
}

func TestProxyRevocable(t *testing.T) {
	checkEval(t, `
		const {proxy, revoke} = Proxy.revocable({a:1}, {});
		const before = proxy.a;
		revoke();
		try { proxy.a; "not reached" } catch (e) { before + ",revoked" }`, "1,revoked")
}

func TestReflect(t *testing.T) {
	tests := []struct{ src, want string }{
		{`Reflect.has({a:1}, "a")`, "true"},
		{`Reflect.get({a:5}, "a")`, "5"},
		{`const o = {}; Reflect.set(o, "k", 1); o.k`, "1"},
		{`Reflect.ownKeys({a:1,b:2}).join(",")`, "a,b"},
		{`Reflect.getPrototypeOf([]) === Array.prototype`, "true"},
		{`Reflect.apply(Math.max, null, [1,5,2])`, "5"},
		{`Reflect.construct(Array, [1,2,3]).length`, "3"},
		{`Reflect.deleteProperty({a:1}, "a")`, "true"},
		{`Reflect.isExtensible({})`, "true"},
		{`const o = {}; Reflect.defineProperty(o, "x", {value: 3}); o.x`, "3"},
	}
	for _, tt := range tests {
		checkEval(t, tt.src, tt.want)
	}
}

func TestTypedArrays(t *testing.T) {
	tests := []struct{ src, want string }{
		{`new Int8Array(3).length`, "3"},
		{`const a = new Int32Array(3); a[0] = 42; a[0]`, "42"},
		{`new Uint8Array([1,2,3]).join(",")`, "1,2,3"},
		{`new Int32Array(4).byteLength`, "16"},
		{`Int32Array.BYTES_PER_ELEMENT`, "4"},
		// Integer types wrap; the clamped type saturates instead.
		{`const a = new Uint8Array(1); a[0] = 300; a[0]`, "44"},
		{`const a = new Uint8ClampedArray(1); a[0] = 300; a[0]`, "255"},
		{`const a = new Int8Array(1); a[0] = 200; a[0]`, "-56"},
		{`const a = new Float64Array(1); a[0] = 1.5; a[0]`, "1.5"},
		// Writing past the end is ignored rather than growing the array.
		{`const a = new Int32Array(1); a[5] = 1; a.length`, "1"},
		{`new Int32Array([1,2,3,4]).subarray(1,3).join(",")`, "2,3"},
		{`new Int32Array([1,2,3,4]).slice(1,3).join(",")`, "2,3"},
		{`new Int32Array([1,2,3]).map(x => x*2).join(",")`, "2,4,6"},
		{`[...new Int32Array([1,2])].join(",")`, "1,2"},
		{`const a = new Int32Array(2); a.set([7,8]); a.join(",")`, "7,8"},
		{`new BigInt64Array([1n, 2n]).join(",")`, "1,2"},
	}
	for _, tt := range tests {
		checkEval(t, tt.src, tt.want)
	}
}

func TestArrayBufferViewsAlias(t *testing.T) {
	// Two views over the same buffer share storage; that aliasing is the whole
	// reason typed arrays exist.
	checkEval(t, `
		const b = new ArrayBuffer(8);
		const x = new Int32Array(b), y = new Int32Array(b);
		x[0] = 7;
		y[0]`, "7")
	// slice copies, subarray aliases.
	checkEval(t, `
		const a = new Int32Array([1,2,3]);
		const s = a.subarray(0,1); s[0] = 9; a[0]`, "9")
	checkEval(t, `
		const a = new Int32Array([1,2,3]);
		const s = a.slice(0,1); s[0] = 9; a[0]`, "1")
}

func TestModules(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()

	mods := map[string]string{
		"math":     `export const pi = 3.14; export function add(a,b) { return a+b } export default "d";`,
		"counter":  `export let n = 0; export function inc() { n++ }`,
		"reexport": `export * from "math";`,
	}
	rt.SetModuleLoader(func(spec, referrer string) (string, string, error) {
		src, ok := mods[spec]
		if !ok {
			return "", "", fmt.Errorf("module %q not found", spec)
		}
		return src, spec, nil
	})

	tests := []struct{ name, src, want string }{
		{"named", `import {pi} from "math"; globalThis.r = pi;`, "3.14"},
		{"function", `import {add} from "math"; globalThis.r = add(2,3);`, "5"},
		{"default", `import d from "math"; globalThis.r = d;`, "d"},
		{"namespace", `import * as m from "math"; globalThis.r = m.pi;`, "3.14"},
		{"renamed", `import {pi as p} from "math"; globalThis.r = p;`, "3.14"},
		{"re-export", `import {add} from "reexport"; globalThis.r = add(1,1);`, "2"},
		{"own export", `export const x = 5; globalThis.r = x;`, "5"},
	}
	for i, tt := range tests {
		if _, err := rt.EvalModule(fmt.Sprintf("entry%d", i), tt.src); err != nil {
			t.Errorf("%s: %v", tt.name, err)
			continue
		}
		got, err := rt.Get("r")
		if err != nil {
			t.Fatal(err)
		}
		if got.String() != tt.want {
			t.Errorf("%s: got %s, want %s", tt.name, got, tt.want)
		}
	}
}

func TestModuleBindingsAreLive(t *testing.T) {
	// An importer reads through to the exporter's current value rather than a
	// copy taken at link time.
	rt := quickjs.New()
	defer rt.Close()
	rt.SetModuleLoader(func(spec, referrer string) (string, string, error) {
		if spec != "counter" {
			return "", "", fmt.Errorf("unknown module %q", spec)
		}
		return `export let n = 0; export function inc() { n++ }`, spec, nil
	})
	if _, err := rt.EvalModule("entry", `
		import {n, inc} from "counter";
		inc(); inc();
		globalThis.r = n;`); err != nil {
		t.Fatal(err)
	}
	got, _ := rt.Get("r")
	if got.String() != "2" {
		t.Errorf("imported binding = %s, want 2; it is not live", got)
	}
}

// A module's top-level let, const and class live in the module environment so
// that the linker can forward to them, and they have the dead zone they would
// have anywhere else: the property exists from the start, holding a marker
// until the declaration runs.
func TestModuleLexicalDeadZone(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"let", `try { x; out = "no error" } catch (e) { out = e.name } let x = 1;`,
			"ReferenceError"},
		{"const", `try { x; out = "no error" } catch (e) { out = e.name } const x = 1;`,
			"ReferenceError"},
		{"class", `try { C; out = "no error" } catch (e) { out = e.name } class C {}`,
			"ReferenceError"},
		{"through a call", `function f() { return x }
		  try { f(); out = "no error" } catch (e) { out = e.name } let x = 1;`,
			"ReferenceError"},
		// typeof does not excuse a dead zone, only an undeclared name.
		{"typeof", `try { out = typeof x } catch (e) { out = e.name } let x = 1;`,
			"ReferenceError"},
		{"typeof undeclared", `out = typeof nowhere;`, "undefined"},
		// After the declaration everything is ordinary again.
		{"initialized", `let x = 1; out = x + 1;`, "2"},
		{"reassigned", `let x = 1; x = 4; out = x;`, "4"},
		{"no initializer", `let x; out = x;`, "undefined"},
		{"destructured", `const {a, b} = {a: 1, b: 2}; out = a + b;`, "3"},
		// A const is a const wherever it is written to.
		{"const assignment", `const k = 1;
		  try { k = 2; out = "no error" } catch (e) { out = e.name }`, "TypeError"},
		{"const assignment nested", `const k = 1;
		  try { (() => { k = 2 })(); out = "no error" } catch (e) { out = e.name }`,
			"TypeError"},
		{"exported const", `export const k = 1;
		  try { k = 2; out = "no error" } catch (e) { out = e.name }`, "TypeError"},
		// A module sees a script's top-level lexical bindings, which sit
		// between its environment and the global object.
		{"script lexical", `out = scriptLet;`, "7"},
		{"script lexical write", `scriptLet = 8; out = scriptLet;`, "8"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := quickjs.New()
			defer rt.Close()
			if _, err := rt.Eval(`let scriptLet = 7;`); err != nil {
				t.Fatal(err)
			}
			if _, err := rt.EvalModule("entry",
				"var out;\n"+tc.src+"\nglobalThis.r = String(out);"); err != nil {
				t.Fatalf("%s: %v", tc.src, err)
			}
			got, _ := rt.Get("r")
			if got.String() != tc.want {
				t.Errorf("%s = %q, want %q", tc.src, got.String(), tc.want)
			}
		})
	}
}

// An export read through a namespace before the module that owns it has run
// the declaration is the same ReferenceError, which a cycle makes reachable.
// Describing the property reads it, so even asking about it throws.
func TestModuleNamespaceDeadZone(t *testing.T) {
	src := `
		import * as ns from "entry";
		function probe(f) { try { f(); return "no error" } catch (e) { return e.name } }
		globalThis.r = [
			probe(() => ns.later),
			probe(() => ns.default),
			probe(() => Object.prototype.hasOwnProperty.call(ns, "later")),
			probe(() => Object.getOwnPropertyDescriptor(ns, "later")),
			probe(() => Object.keys(ns)),
			probe(() => { for (var k in ns) {} }),
			probe(() => Object.prototype.propertyIsEnumerable.call(ns, "later")),
			probe(() => ({...ns})),
			probe(() => JSON.stringify(ns)),
			// The names are known without reading anything, and deleting an
			// export is refused rather than attempted.
			probe(() => Object.getOwnPropertyNames(ns).join(",")),
			String(Reflect.deleteProperty(ns, "later")),
		].join("|");
		export let later = 3;
		export default 4;`

	rt := quickjs.New()
	defer rt.Close()
	rt.SetModuleLoader(func(spec, referrer string) (string, string, error) {
		return src, spec, nil
	})
	if _, err := rt.EvalModule("entry", src); err != nil {
		t.Fatal(err)
	}
	const want = "ReferenceError|ReferenceError|ReferenceError|ReferenceError|" +
		"ReferenceError|ReferenceError|ReferenceError|ReferenceError|" +
		"ReferenceError|no error|false"
	if got, _ := rt.Get("r"); got.String() != want {
		t.Errorf("\n got: %s\nwant: %s", got.String(), want)
	}
}

// TestDefaultExportForms covers what `export default` binds. A declaration
// with a name is exported through that binding, which keeps the export live;
// anything else is bound under a name no identifier can spell.
func TestDefaultExportForms(t *testing.T) {
	cases := []struct{ dep, want string }{
		// The function reassigns its own binding, which the namespace sees
		// because the export is that binding rather than a copy of it.
		{`export default function fn() { fn = 2; return 1 }`, "fn|1|number"},
		{`export default function () { return 1 }`, "default|1|function"},
		{`export default class C { static m() { return 1 } }`, "C|1|function"},
		{`export default class { static m() { return 1 } }`, "default|1|function"},
		{`export default {m() { return 1 }}`, "|1|object"},
		// An async declaration binds its name too, which is what makes the
		// name visible here at all.
		{`export default async function fn() { return 1 }`,
			"fn|[object Promise]|function"},
		{`export default async function* g() {}`, "g|[object AsyncGenerator]|function"},
	}
	for _, tc := range cases {
		rt := quickjs.New()
		dep := tc.dep
		rt.SetModuleLoader(func(spec, referrer string) (string, string, error) {
			if spec != "d" {
				return "", "", fmt.Errorf("unknown module %q", spec)
			}
			return dep, spec, nil
		})
		_, err := rt.EvalModule("entry", `
			import d from "d";
			var name = typeof d === "function" ? d.name : "";
			var called = d.m ? d.m() : d();
			import("d").then(function (ns) {
				globalThis.r = [name, called, typeof ns.default].join("|");
			});`)
		if err != nil {
			t.Errorf("%s: %v", tc.dep, err)
		} else if got, _ := rt.Get("r"); got.String() != tc.want {
			t.Errorf("%s\n got: %s\nwant: %s", tc.dep, got, tc.want)
		}
		rt.Close()
	}
}

func TestModuleNamespaceIsReturned(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	ns, err := rt.EvalModule("entry", `export const a = 1; export const b = 2;`)
	if err != nil {
		t.Fatal(err)
	}
	av, err := ns.Get("a")
	if err != nil {
		t.Fatal(err)
	}
	if av.Int() != 1 {
		t.Errorf("namespace.a = %v, want 1", av)
	}
}

func TestImportWithoutALoaderIsRejected(t *testing.T) {
	// A runtime with no loader has no filesystem access, which is the default.
	rt := quickjs.New()
	defer rt.Close()
	_, err := rt.EvalModule("entry", `import {x} from "anything";`)
	if err == nil {
		t.Fatal("an import should fail without a module loader")
	}
	if !strings.Contains(err.Error(), "module loader") {
		t.Errorf("error = %v, want one mentioning the missing loader", err)
	}
}

func TestImportIsRejectedInAScript(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	if _, err := rt.Eval(`import {x} from "m";`); err == nil {
		t.Error("an import declaration should be rejected in script code")
	}
}

func TestDeepRecursionDoesNotCorruptFrames(t *testing.T) {
	// The interpreter holds a pointer to its frame across nested calls, so the
	// frame stack must never be reallocated underneath it. A recursion deeper
	// than any plausible initial capacity is what would expose that.
	rt := quickjs.New(quickjs.WithMaxCallDepth(4000))
	defer rt.Close()

	v, err := rt.Eval(`
		function down(n) { return n === 0 ? 0 : 1 + down(n - 1); }
		down(2000)`)
	if err != nil {
		t.Fatalf("deep recursion failed: %v", err)
	}
	if v.Int() != 2000 {
		t.Errorf("down(2000) = %d, want 2000", v.Int())
	}
}

func TestDeepRecursionUnwindsCorrectly(t *testing.T) {
	// Each frame must resume at the right instruction after the one above it
	// returns, which a stale frame pointer would break.
	rt := quickjs.New(quickjs.WithMaxCallDepth(4000))
	defer rt.Close()
	v, err := rt.Eval(`
		function sum(n) { if (n === 0) return 0; const rest = sum(n - 1); return n + rest; }
		sum(1000)`)
	if err != nil {
		t.Fatal(err)
	}
	if v.Int() != 500500 {
		t.Errorf("sum(1000) = %d, want 500500", v.Int())
	}
}

func TestAsyncGenerators(t *testing.T) {
	// An async generator's next returns a promise, and its body may suspend on
	// await as well as on yield.
	checkAsync(t, `var r = ""; async function* g() { yield 1; yield 2 }
		g().next().then(v => r = v.value + "," + v.done)`, `r`, "1,false")
	checkAsync(t, `var r = ""; async function* g() { } g().next().then(v => r = String(v.done))`,
		`r`, "true")
	// An await inside is serviced internally and never reaches the caller.
	checkAsync(t, `var r = ""; async function* g() { const x = await 5; yield x * 2 }
		g().next().then(v => r = v.value)`, `r`, "10")
	// A yielded promise is awaited, so the consumer sees the value.
	checkAsync(t, `var r = ""; async function* g() { yield Promise.resolve(9) }
		g().next().then(v => r = v.value)`, `r`, "9")
	// An exception escaping the body rejects the promise.
	checkAsync(t, `var r = ""; async function* g() { throw new Error("x") }
		g().next().catch(e => r = "caught:" + e.message)`, `r`, "caught:x")
	// Async generator methods on objects and classes.
	checkAsync(t, `var r = ""; const o = { async *m() { yield 7 } }; o.m().next().then(v => r = v.value)`,
		`r`, "7")
	checkAsync(t, `var r = ""; class C { static async *m(a = 1) { yield a } }
		C.m().next().then(v => r = v.value)`, `r`, "1")
}

func TestForAwaitOf(t *testing.T) {
	checkAsync(t, `var r = ""; async function* g() { yield 1; yield 2 }
		(async () => { for await (const v of g()) r += v })()`, `r`, "12")
	checkAsync(t, `var r = ""; async function* g() { yield* [1,2,3] }
		(async () => { for await (const v of g()) r += v })()`, `r`, "123")
	// A plain iterable works too, with each value awaited.
	checkAsync(t, `var r = ""; (async () => { for await (const v of [1, Promise.resolve(2)]) r += v })()`,
		`r`, "12")
}

func TestWrapperConstructors(t *testing.T) {
	// Called as a function a wrapper constructor produces a primitive; called
	// with new it produces an object. Nothing else distinguishes the two, and
	// the difference is observable.
	tests := []struct{ src, want string }{
		{`typeof Boolean(true)`, "boolean"},
		{`typeof new Boolean(true)`, "object"},
		{`typeof Number(5)`, "number"},
		{`typeof new Number(5)`, "object"},
		{`typeof String("x")`, "string"},
		{`typeof new String("x")`, "object"},
		// A wrapper coerces back to its primitive in an expression.
		{`new Boolean(true) + true`, "2"},
		{`new Number(5) + 1`, "6"},
		{`new String("ab").length`, "2"},
		{`new String("ab")[0]`, "a"},
		// Every object is truthy, including a Boolean wrapping false. This is
		// the classic reason not to use the wrappers.
		{`new Boolean(false) ? "truthy" : "falsy"`, "truthy"},
		{`Object.prototype.toString.call(new Boolean(true))`, "[object Boolean]"},
		{`Object.prototype.toString.call(new Number(1))`, "[object Number]"},
		{`Object.prototype.toString.call(new String("a"))`, "[object String]"},
		// Symbol is deliberately not constructible.
		{`try { new Symbol(); "no" } catch (e) { e.name }`, "TypeError"},
	}
	for _, tt := range tests {
		checkEval(t, tt.src, tt.want)
	}
}

func TestDynamicImport(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	rt.SetModuleLoader(func(spec, ref string) (string, string, error) {
		if spec == "m" {
			return `export const v = 42; export default "d";`, spec, nil
		}
		return "", "", fmt.Errorf("not found: %s", spec)
	})

	// A dynamic import always returns a promise, so a failure rejects rather
	// than throws.
	if _, err := rt.Eval(`var r = ""; import("m").then(ns => r = ns.v)`); err != nil {
		t.Fatal(err)
	}
	if got := evalString(t, rt, `r`); got != "42" {
		t.Errorf("named export = %s, want 42", got)
	}

	rt.Eval(`r = ""; import("m").then(ns => r = ns.default)`)
	if got := evalString(t, rt, `r`); got != "d" {
		t.Errorf("default export = %s, want d", got)
	}

	rt.Eval(`r = ""; import("nope").catch(() => r = "rejected")`)
	if got := evalString(t, rt, `r`); got != "rejected" {
		t.Errorf("a failed import should reject, got %s", got)
	}

	rt.Eval(`r = ""; (async () => { const ns = await import("m"); r = ns.v })()`)
	if got := evalString(t, rt, `r`); got != "42" {
		t.Errorf("awaited import = %s, want 42", got)
	}
}

func TestStaticImportDefaultIsNamedInNamespace(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	ns, err := rt.EvalModule("entry", `export default 7;`)
	if err != nil {
		t.Fatal(err)
	}
	v, err := ns.Get("default")
	if err != nil {
		t.Fatal(err)
	}
	if v.Int() != 7 {
		t.Errorf("namespace.default = %v, want 7", v)
	}
}

// A module namespace is deliberately rigid: no prototype, not extensible,
// nothing added or removed, nothing written. What a module exports is fixed
// when it is compiled, so an object that let any of that change would be lying.
//
// The exports themselves are live, which is what makes a cycle between two
// modules work, so the storage is an accessor while the object reports a data
// property -- the one place the two have to disagree.
func TestModuleNamespaceObject(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()

	mods := map[string]string{
		"m":    `export var x = 1; export default 2; export let later = 3;`,
		"host": ``,
	}
	rt.SetModuleLoader(func(spec, referrer string) (string, string, error) {
		src, ok := mods[spec]
		if !ok {
			return "", "", fmt.Errorf("module %q not found", spec)
		}
		return src, spec, nil
	})

	cases := []struct{ name, src, want string }{
		{"tag", `ns[Symbol.toStringTag]`, "Module"},
		{"toString", `Object.prototype.toString.call(ns)`, "[object Module]"},
		{"no prototype", `String(Object.getPrototypeOf(ns))`, "null"},
		{"not extensible", `String(Object.isExtensible(ns))`, "false"},
		// The exports are listed in code unit order, and nothing the compiler
		// put there for its own use appears.
		{"keys", `Object.getOwnPropertyNames(ns).join("|")`, "default|later|x"},
		{"enumerable keys", `Object.keys(ns).join("|")`, "default|later|x"},
		{"values", `[ns.x, ns.default, ns.later].join(",")`, "1,2,3"},
		{"spread", `JSON.stringify({...ns})`, `{"default":2,"later":3,"x":1}`},

		// An export reports as a data property: writable, enumerable, and not
		// configurable.
		{"export descriptor", `var d = Object.getOwnPropertyDescriptor(ns, "x");
		  [d.value, d.writable, d.enumerable, d.configurable].join("/")`, "1/true/true/false"},
		{"tag descriptor", `var d = Object.getOwnPropertyDescriptor(ns, Symbol.toStringTag);
		  [d.value, d.writable, d.enumerable, d.configurable].join("/")`,
			"Module/false/false/false"},
		{"absent", `String(Object.getOwnPropertyDescriptor(ns, "nope"))`, "undefined"},
		{"has", `[("x" in ns), ("nope" in ns)].join(",")`, "true,false"},

		// Nothing can be changed. In sloppy code the write simply does not
		// happen; in strict code it throws.
		{"write ignored", `ns.x = 9; String(ns.x)`, "1"},
		{"delete false", `String(delete ns.x)`, "false"},
		{"strict write", `(function () { "use strict";
		  try { ns.x = 9; return "no error" } catch (e) { return e.constructor.name } })()`,
			"TypeError"},
		// Every assignment is refused, including one naming something that is
		// not an export at all, and one arriving through an heir.
		{"set refused", `[Reflect.set(ns, "x", 9), Reflect.set(ns, "nope", 9),
		  Reflect.set(ns, Symbol.toStringTag, 9), Reflect.set(ns, Symbol.iterator, 9)].join(",")`,
			"false,false,false,false"},
		{"set through heir", `String(Reflect.set(Object.create(ns), "x", 9))`, "false"},
		{"define", `try { Object.defineProperty(ns, "y", {value: 1}); "no error" }
		  catch (e) { e.constructor.name }`, "TypeError"},

		// A define is accepted only when it describes what is already there,
		// which is what makes Object.freeze's redefinition of each property
		// succeed while any actual change is refused.
		{"define same", `String(Reflect.defineProperty(ns, "x",
		  {value: 1, writable: true, enumerable: true, configurable: false}))`, "true"},
		{"define nothing", `String(Reflect.defineProperty(ns, "x", {}))`, "true"},
		{"define other value", `String(Reflect.defineProperty(ns, "x", {value: 2}))`, "false"},
		{"define non-writable", `String(Reflect.defineProperty(ns, "x", {writable: false}))`, "false"},
		{"define configurable", `String(Reflect.defineProperty(ns, "x", {configurable: true}))`, "false"},
		{"define non-enumerable", `String(Reflect.defineProperty(ns, "x", {enumerable: false}))`, "false"},
		{"define accessor", `String(Reflect.defineProperty(ns, "x", {get() { return 1 }}))`, "false"},
		{"define absent", `String(Reflect.defineProperty(ns, "nope", {value: 1}))`, "false"},
		// The symbol-keyed properties are ordinary, and so is defining them.
		{"define tag", `String(Reflect.defineProperty(ns, Symbol.toStringTag, {value: "Module"}))`,
			"true"},
		{"define other tag", `String(Reflect.defineProperty(ns, Symbol.toStringTag, {value: "M"}))`,
			"false"},
		{"define new symbol", `String(Reflect.defineProperty(ns, Symbol.iterator, {value: 1}))`,
			"false"},
		{"set prototype", `try { Object.setPrototypeOf(ns, {}); "no error" }
		  catch (e) { e.constructor.name }`, "TypeError"},
		// Setting it to null is what it already is, so that succeeds.
		{"set prototype null", `String(Object.setPrototypeOf(ns, null) === ns)`, "true"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := rt.Eval(`
				var out;
				import("m").then(ns => { globalThis.ns = ns; out = "ready" });
			`); err != nil {
				t.Fatal(err)
			}
			v, err := rt.Eval(tc.src)
			if err != nil {
				t.Fatalf("%s: %v", tc.src, err)
			}
			if got := v.String(); got != tc.want {
				t.Errorf("%s = %q, want %q", tc.src, got, tc.want)
			}
		})
	}
}

// `export default class {}` declares no binding, so the class is the
// expression it looks like and takes its name from the export. Compiling it as
// a declaration read a name that was not there.
func TestExportDefaultAnonymousClass(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()

	ns, err := rt.EvalModule("entry",
		`export default class { valueOf() { return 45 } }`)
	if err != nil {
		t.Fatal(err)
	}
	c, err := ns.Get("default")
	if err != nil {
		t.Fatal(err)
	}
	name, err := c.Get("name")
	if err != nil {
		t.Fatal(err)
	}
	if got := name.String(); got != "default" {
		t.Errorf("name = %q, want %q", got, "default")
	}
	inst, err := c.New()
	if err != nil {
		t.Fatal(err)
	}
	m, err := inst.Get("valueOf")
	if err != nil {
		t.Fatal(err)
	}
	v, err := m.CallWithThis(inst)
	if err != nil {
		t.Fatal(err)
	}
	if v.Int() != 45 {
		t.Errorf("valueOf() = %v, want 45", v)
	}
}

// import.meta is an ordinary object the host may put anything on, one per
// module and the same one every time. It is not a property access, so it
// cannot be assigned to.
func TestImportMeta(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()

	sources := map[string]string{
		"a": `import.meta.tag = "a"; export var probe = import.meta;`,
		"b": `import {probe} from "a";
		      export var out = [
		        typeof import.meta,
		        Object.getPrototypeOf(import.meta) === null,
		        Object.isExtensible(import.meta),
		        import.meta === import.meta,
		        import.meta !== probe,
		        probe.tag,
		      ].join(",");`,
	}
	rt.SetModuleLoader(func(spec, ref string) (string, string, error) {
		return sources[spec], spec, nil
	})

	ns, err := rt.EvalModule("b", sources["b"])
	if err != nil {
		t.Fatal(err)
	}
	v, err := ns.Get("out")
	if err != nil {
		t.Fatal(err)
	}
	want := "object,true,true,true,true,a"
	if got := v.String(); got != want {
		t.Errorf("out = %q, want %q", got, want)
	}

	// It is neither exported nor visible to the module's own code.
	if _, err := rt.EvalModule("c", `export var out = 1;`); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.EvalModule("d", `import.meta; var x = [1]; [import.meta] = x;`); err == nil {
		t.Error("assigning to import.meta was accepted, want SyntaxError")
	}
}

// A module's top level is a lexical scope in a way a script's is not: a
// function declaration there is a lexical binding rather than a var, and so is
// every imported name. Its exports have rules of their own.
func TestModuleDeclarationErrors(t *testing.T) {
	bad := []struct{ name, src string }{
		{"duplicate export", `export var x = 1; export var x = 2;`},
		{"duplicate export name", `var x = 1, y = 2; export {x, y as x};`},
		{"duplicate default", `export default 1; export default 2;`},
		{"export of nothing", `export {nope};`},
		{"duplicate let", `let z; let z;`},
		{"two top-level functions", `function f() {} function f() {}`},
		{"function and var", `var smoosh; function smoosh() {}`},
		{"default function and a declaration", `export default function f() {}; function f() {}`},
		{"import and var", `import {a} from "d"; var a = 1;`},
		{"import and let", `import {a} from "d"; let a;`},
	}
	for _, tc := range bad {
		rt := quickjs.New()
		rt.SetModuleLoader(func(spec, ref string) (string, string, error) {
			return `export var a = 1;`, spec, nil
		})
		if _, err := rt.EvalModule("m", tc.src); err == nil {
			t.Errorf("%s: accepted, want SyntaxError", tc.name)
		} else if !strings.Contains(err.Error(), "SyntaxError") {
			t.Errorf("%s: got %v, want SyntaxError", tc.name, err)
		}
		rt.Close()
	}

	// What a script allows but a module does not is exactly the function rule,
	// so the same source is fine as a script.
	rt := quickjs.New()
	defer rt.Close()
	if _, err := rt.Eval(`var smoosh; function smoosh() {}`); err != nil {
		t.Errorf("script: %v", err)
	}

	// And the forms that are legal in a module stay legal.
	for _, src := range []string{
		`var q = 1; export {q}; export var out = q;`,
		`export * from "d"; export * from "e";`,
		`export {a} from "d";`,
		`var v; { var v; } export var out = v;`,
		`export default function f() {} export var out = f;`,
	} {
		rt := quickjs.New()
		rt.SetModuleLoader(func(spec, ref string) (string, string, error) {
			return `export var a = 1;`, spec, nil
		})
		if _, err := rt.EvalModule("m", src); err != nil {
			t.Errorf("%s: %v", src, err)
		}
		rt.Close()
	}
}

// A module that will not load, parse or link fails the way a static import of
// it would: as an error the script can catch and inspect.
func TestDynamicImportRejectsWithAJavaScriptError(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	rt.SetModuleLoader(func(spec, ref string) (string, string, error) {
		// Legal in a script, not in a module.
		return `var smoosh; function smoosh() {}`, spec, nil
	})

	v, err := rt.Eval(`
	    var seen = "none";
	    import("d").then(function () { seen = "resolved" },
	                     function (e) { seen = e.name });
	    seen`)
	if err != nil {
		t.Fatal(err)
	}
	_ = v
	got, err := rt.Eval(`seen`)
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "SyntaxError" {
		t.Errorf("rejection = %q, want %q", got.String(), "SyntaxError")
	}
}

// An array's length is synthesized rather than stored, which is what makes a
// dense array cheap. It is still an own property, and everything that looks at
// the property table rather than reading through a getter has to see one.
func TestArrayLengthIsAnOwnProperty(t *testing.T) {
	cases := []struct{ src, want string }{
		// It is listed, after the indices and before any other string key.
		{`Object.getOwnPropertyNames([1, 2]).join(",")`, "0,1,length"},
		{`var a = [1]; a.x = 1; Object.getOwnPropertyNames(a).join(",")`, "0,length,x"},
		{`Reflect.ownKeys([1]).join(",")`, "0,length"},
		// But it is not enumerable, so it is not a key or an entry.
		{`Object.keys([1, 2]).join(",")`, "0,1"},
		{`var s = ""; for (var k in [1, 2]) s += k; s`, "01"},

		// It describes itself, and freezing makes it read-only.
		{`JSON.stringify(Object.getOwnPropertyDescriptor([1, 2], "length"))`,
			`{"value":2,"writable":true,"enumerable":false,"configurable":false}`},
		{`JSON.stringify(Object.getOwnPropertyDescriptor(Object.freeze([]), "length"))`,
			`{"value":0,"writable":false,"enumerable":false,"configurable":false}`},

		// Writing a read-only length fails, and so does every method that ends
		// by setting it -- even when the value would not change.
		{`"use strict"; var a = Object.freeze([]);
		  try { a.length = 5 } catch (e) { e.constructor.name }`, "TypeError"},
		{`var a = Object.freeze([]); try { a.push() } catch (e) { e.constructor.name }`,
			"TypeError"},
		{`var a = Object.freeze([]); try { a.pop() } catch (e) { e.constructor.name }`,
			"TypeError"},
		{`var a = []; Object.defineProperty(a, "length", {writable: false});
		  try { a.push(1) } catch (e) { e.constructor.name }`, "TypeError"},
		// A string's length is read-only too, so a generic method that sets it
		// through a string receiver fails.
		{`try { Array.prototype.push.call("str", 1) } catch (e) { e.constructor.name }`,
			"TypeError"},

		// Freezing a function reaches its synthesized name and length.
		{`var f = Object.freeze(function g() {});
		  JSON.stringify(Object.getOwnPropertyDescriptor(f, "name"))`,
			`{"value":"g","writable":false,"enumerable":false,"configurable":false}`},
	}

	for _, tc := range cases {
		rt := quickjs.New()
		v, err := rt.Eval(tc.src)
		if err != nil {
			t.Errorf("%s: %v", tc.src, err)
		} else if got := v.String(); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.src, got, tc.want)
		}
		rt.Close()
	}
}

// In sloppy mode with a plain parameter list the arguments object is mapped:
// its indices alias the parameters they were passed to, so writing one is
// visible through the other. Strict mode, and anything more elaborate than
// plain parameters, get the snapshot instead.
func TestMappedArguments(t *testing.T) {
	cases := []struct{ src, want string }{
		{`(function (a) { arguments[0] = 9; return a })(1)`, "9"},
		{`(function (a) { a = 9; return arguments[0] })(1)`, "9"},
		{`(function (a, b) { b = 5; return arguments[1] })(1, 2)`, "5"},
		// The alias survives the call: the object may outlive the frame.
		{`var args = (function (a) { a = 7; return arguments })(1); String(args[0])`, "7"},

		// Only an argument that was passed, and only one whose parameter the
		// name still resolves to, is mapped.
		{`(function (a) { a = 9; return String(arguments[1]) })(1, 2)`, "2"},
		{`(function (a, b) { b = 9; return String(arguments[1]) })(1)`, "undefined"},
		{`(function (a, a) { a = 9; return [arguments[0], arguments[1]].join(",") })(1, 2)`,
			"1,9"},

		// Deleting or making the property read-only breaks the alias; making
		// it non-configurable does not.
		{`(function (a) { delete arguments[0]; a = 3; return String(arguments[0]) })(1)`,
			"undefined"},
		{`(function (a) { Object.defineProperty(arguments, "0", {writable: false});
		    a = 2; return [a, arguments[0]].join(",") })(1)`, "2,1"},
		{`(function (a) { Object.defineProperty(arguments, "0", {configurable: false});
		    a = 2; return [a, arguments[0]].join(",") })(1)`, "2,2"},
		// Defining a value writes through even when the result is read-only.
		{`(function (a) {
		    Object.defineProperty(arguments, "0",
		      {value: 20, writable: false, enumerable: false, configurable: false});
		    return a })(1)`, "20"},

		// Anything but a sloppy plain parameter list gets the snapshot.
		{`(function (a) { "use strict"; arguments[0] = 9; return a })(1)`, "1"},
		{`(function (a = 1) { arguments[0] = 9; return a })(1)`, "1"},
		{`(function (...a) { arguments[0] = 9; return a[0] })(1)`, "1"},
		{`(function ([a]) { arguments[0] = 9; return a })([1])`, "1"},

		// An unmapped arguments object refuses to say what called it; a mapped
		// one answers.
		{`(function () { "use strict";
		    try { arguments.callee } catch (e) { return e.constructor.name } })()`,
			"TypeError"},
		// A sloppy function whose parameters are not plain gets the unmapped
		// object too, and with it the same refusal.
		{`(function (a = 1) {
		    try { arguments.callee } catch (e) { return e.constructor.name } })()`,
			"TypeError"},
		{`(function f() { return arguments.callee === f })()`, "true"},
		// The same function does both halves of every restricted property.
		{`var d = Object.getOwnPropertyDescriptor(
		      (function () { "use strict"; return arguments })(), "callee");
		  var fp = Object.getOwnPropertyDescriptor(Function.prototype, "caller");
		  [d.get === d.set, d.get === fp.get, d.get === fp.set].join(",")`,
			"true,true,true"},

		// Two parameters may share a name only where the list is simple and
		// the code is sloppy.
		{`function f(a, a) { return a } String(f(1, 2))`, "2"},
	}

	for _, tc := range cases {
		rt := quickjs.New()
		v, err := rt.Eval(tc.src)
		if err != nil {
			t.Errorf("%s: %v", tc.src, err)
		} else if got := v.String(); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.src, got, tc.want)
		}
		rt.Close()
	}

	for _, src := range []string{
		`"use strict"; throw 0; function f(a, a) {}`,
		`throw 0; (a, a) => {}`,
		`throw 0; function f(a, a = 1) {}`,
		`throw 0; async function f(a, a) {}`,
		`throw 0; ({ m(a, a) {} })`,
	} {
		rt := quickjs.New()
		if _, err := rt.Eval(src); err == nil {
			t.Errorf("%s: accepted, want SyntaxError", src)
		} else if !strings.Contains(err.Error(), "SyntaxError") {
			t.Errorf("%s: got %v, want SyntaxError", src, err)
		}
		rt.Close()
	}
}

// A computed member access checks that there is something to look the key up
// on before it converts the key. A key whose toString throws must not run at
// all when the base is null.
func TestComputedKeyOrdering(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var b = null, p = {toString: function () { throw new Error("key") }};
		  try { b[p] } catch (e) { e.constructor.name }`, "TypeError"},
		{`var b = null, p = {toString: function () { throw new Error("key") }};
		  try { b[p] = 1 } catch (e) { e.constructor.name }`, "TypeError"},
		{`var b = null, p = {toString: function () { throw new Error("key") }};
		  try { b[p] ^= 1 } catch (e) { e.constructor.name }`, "TypeError"},
		{`var b = null, p = {toString: function () { throw new Error("key") }};
		  try { b[p]++ } catch (e) { e.constructor.name }`, "TypeError"},
		// The property expression is still evaluated, and its own failure wins.
		{`var b = null;
		  try { b[(function () { throw new RangeError() })()] } catch (e) { e.constructor.name }`,
			"RangeError"},
		// An ordinary access is unaffected.
		{`var o = {}; o[{toString: function () { return "k" }}] = 5; String(o.k)`, "5"},
	}
	for _, tc := range cases {
		rt := quickjs.New()
		v, err := rt.Eval(tc.src)
		if err != nil {
			t.Errorf("%s: %v", tc.src, err)
		} else if got := v.String(); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.src, got, tc.want)
		}
		rt.Close()
	}
}

// A destructuring pattern interleaves asking the source for a value with
// evaluating the target that receives it, because both are observable: a
// target can run a getter, and a source can be a generator. The iterator stays
// open across the whole pattern, so an abrupt exit closes it -- and a pattern
// that stops short of the end tells it so, with nothing else in flight to
// swallow what the return method says.
func TestDestructuringOrder(t *testing.T) {
	cases := []struct{ src, want string }{
		// The target's reference comes before the source is read.
		{`var log = [];
		  function src() { log.push("source"); return {get p() { log.push("get") }} }
		  function tgt() { log.push("target"); return {set q(v) { log.push("set") }} }
		  function sk() { log.push("source-key");
		    return {toString: function () { log.push("source-key-tostring"); return "p" }} }
		  function tk() { log.push("target-key");
		    return {toString: function () { log.push("target-key-tostring"); return "q" }} }
		  ({[sk()]: tgt()[tk()]} = src());
		  log.join(",")`,
			"source,source-key,source-key-tostring,target,target-key,get,target-key-tostring,set"},

		// A pattern that does not exhaust its iterator closes it, and a return
		// method that throws is what the destructuring throws.
		{`var n = 0, rc = 0, r;
		  var it = {next: function () { n += 1; return {done: n > 10, value: n} },
		            "return": function () { rc += 1; throw new Error("ret") }};
		  var iterable = {}; iterable[Symbol.iterator] = function () { return it };
		  try { var [a] = iterable } catch (e) { r = e.message }
		  [r, n, rc].join(",")`, "ret,1,1"},
		// One that returns a non-object is a TypeError, which is the one place
		// the protocol checks that.
		{`var it = {next: function () { return {done: false, value: 1} },
		            "return": function () { return null }};
		  var iterable = {}; iterable[Symbol.iterator] = function () { return it };
		  try { var [a] = iterable } catch (e) { e.constructor.name }`, "TypeError"},
		// An exhausted iterator is not closed again.
		{`var rc = 0;
		  var it = {next: function () { return {done: true} },
		            "return": function () { rc += 1 }};
		  var iterable = {}; iterable[Symbol.iterator] = function () { return it };
		  var [a] = iterable; String(rc)`, "0"},
		// A failure while the pattern is running closes it, and that close's
		// own failure does not replace the original.
		{`var rc = 0;
		  var it = {next: function () { return {done: false} },
		            "return": function () { rc += 1; throw new Error("ret") }};
		  var iterable = {}; iterable[Symbol.iterator] = function () { return it };
		  var r;
		  try { var [a = (function () { throw new RangeError("boom") })()] = iterable }
		  catch (e) { r = e.constructor.name }
		  r + "," + rc`, "RangeError,1"},

		// The ordinary forms still work.
		{`var [a, b] = [1, 2]; a + "," + b`, "1,2"},
		{`var [a, ...r] = [1, 2, 3]; a + "|" + r.join(",")`, "1|2,3"},
		{`var [, b] = [1, 2]; String(b)`, "2"},
		{`var [a = 5] = []; String(a)`, "5"},
		{`var [a, [b, c]] = [1, [2, 3]]; [a, b, c].join(",")`, "1,2,3"},
		{`var s = new Set([1, 2]); var [x, y] = s; x + "," + y`, "1,2"},
		{`var o = {}; [o.x] = [3]; String(o.x)`, "3"},
		{`var {a, ...r} = {a: 1, b: 2}; a + "|" + JSON.stringify(r)`, `1|{"b":2}`},
		{`var k = "a"; var {[k]: v} = {a: 9}; String(v)`, "9"},
	}
	for _, tc := range cases {
		rt := quickjs.New()
		v, err := rt.Eval(tc.src)
		if err != nil {
			t.Errorf("%s: %v", tc.src, err)
		} else if got := v.String(); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.src, got, tc.want)
		}
		rt.Close()
	}
}

// An export name does not always name a binding of the module it is asked of:
// `export {x} from "m"` forwards, and `export * from "m"` forwards everything.
// Answering where a name comes from means walking the graph, and two of the
// answers are errors -- at the point somebody asks, not where it was written.
func TestModuleExportResolution(t *testing.T) {
	run := func(t *testing.T, entry string, mods map[string]string) (string, error) {
		t.Helper()
		rt := quickjs.New()
		defer rt.Close()
		rt.SetModuleLoader(func(spec, ref string) (string, string, error) {
			src, ok := mods[spec]
			if !ok {
				return "", "", errors.New("no such module")
			}
			return src, spec, nil
		})
		ns, err := rt.EvalModule(entry, mods[entry])
		if err != nil {
			return "", err
		}
		v, err := ns.Get("out")
		if err != nil {
			return "", err
		}
		return v.String(), nil
	}

	bad := []struct {
		name  string
		entry string
		mods  map[string]string
	}{
		{"import of a name nothing exports", "m", map[string]string{
			"m": `import {missing} from "d"; export var out = missing;`,
			"d": `export var a = 1;`,
		}},
		{"re-export of a name nothing exports", "m", map[string]string{
			"m": `export {missing} from "d"; export var out = 1;`,
			"d": `export var a = 1;`,
		}},
		{"import of an ambiguous name", "m", map[string]string{
			"m": `import {a} from "x"; export var out = a;`,
			"x": `export * from "d"; export * from "e";`,
			"d": `export var a = 1;`,
			"e": `export var a = 2;`,
		}},
	}
	for _, tc := range bad {
		if _, err := run(t, tc.entry, tc.mods); err == nil {
			t.Errorf("%s: accepted, want SyntaxError", tc.name)
		} else if !strings.Contains(err.Error(), "SyntaxError") {
			t.Errorf("%s: got %v, want SyntaxError", tc.name, err)
		}
	}

	good := []struct {
		name  string
		want  string
		entry string
		mods  map[string]string
	}{
		{"a star re-export forwards", "1", "m", map[string]string{
			"m": `import {a} from "x"; export var out = a;`,
			"x": `export * from "d";`,
			"d": `export var a = 1;`,
		}},
		{"an ambiguous name is left out of the namespace", "b", "m", map[string]string{
			"m": `import * as ns from "x"; export var out = Object.keys(ns).join(",");`,
			"x": `export * from "d"; export * from "e";`,
			"d": `export var a = 1; export var b = 3;`,
			"e": `export var a = 2;`,
		}},
		{"a local export wins over a star", "9", "m", map[string]string{
			"m": `import {a} from "x"; export var out = a;`,
			"x": `export * from "d"; export var a = 9;`,
			"d": `export var a = 1;`,
		}},
		{"two modules re-exporting one binding agree", "1", "m", map[string]string{
			"m": `import {a} from "x"; export var out = a;`,
			"x": `export * from "p"; export * from "q";`,
			"p": `export {a} from "d";`,
			"q": `import {a} from "d"; export {a};`,
			"d": `export var a = 1;`,
		}},
		{"a string name is exported and imported", "2", "m", map[string]string{
			"m": `import {"☿" as merc} from "d"; export var out = merc;`,
			"d": `var v = 2; export {v as "☿"};`,
		}},
		{"a namespace re-export", "1", "m", map[string]string{
			"m": `import {inner} from "x"; export var out = inner.a;`,
			"x": `export * as inner from "d";`,
			"d": `export var a = 1;`,
		}},
		{"a cycle resolves", "1", "m", map[string]string{
			"m": `import {a} from "d"; export var out = a;`,
			"d": `import {out} from "m"; export var a = 1;`,
		}},
	}
	for _, tc := range good {
		got, err := run(t, tc.entry, tc.mods)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
		} else if got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// An iterator's next method belongs to the prototype its kind shares, not to
// each iterator, which a script can check -- and which is what makes the
// prototype worth having at all.
func TestIteratorPrototypes(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var p = Object.getPrototypeOf([].values());
		  [typeof p.next, p === Object.getPrototypeOf([].keys()),
		   p[Symbol.toStringTag], Object.prototype.hasOwnProperty.call([].values(), "next")
		  ].join(",")`, "function,true,Array Iterator,false"},
		{`Object.getPrototypeOf(new Map().keys())[Symbol.toStringTag]`, "Map Iterator"},
		{`Object.getPrototypeOf(new Set().values())[Symbol.toStringTag]`, "Set Iterator"},
		{`Object.getPrototypeOf(""[Symbol.iterator]())[Symbol.toStringTag]`, "String Iterator"},
		{`Object.getPrototypeOf("".matchAll(/a/g))[Symbol.toStringTag]`,
			"RegExp String Iterator"},
		// Every kind refuses a receiver that is not one of its own.
		{`try { [].values().next.call({}) } catch (e) { e.constructor.name }`, "TypeError"},
		{`try { new Map().keys().next.call([]) } catch (e) { e.constructor.name }`, "TypeError"},
		// An exhausted iterator stays exhausted, even if the collection grows.
		{`var m = new Map(); var it = m.keys(); it.next();
		  m.set("a", 1); String(it.next().done)`, "true"},

		// A surrogate pair split across a concatenation is one code point, and
		// WTF-8 spells a code point one way -- so the two halves joined equal
		// the same pair written directly.
		{`var lo = "\uD834", hi = "\uDF06", pair = lo + hi;
		  var s = "a" + pair + "b";
		  var it = s[Symbol.iterator]();
		  it.next();
		  [it.next().value === pair, pair.length, pair === "𝌆"].join(",")`,
			"true,2,true"},
		// A lone surrogate is still itself.
		{`var lo = "\uD834"; [lo.length, lo.charCodeAt(0), lo === "\uD834"].join(",")`,
			"1,55348,true"},
		{`("\uD834" + "a").length + "," + ("a" + "\uDF06").length`, "2,2"},
	}
	for _, tc := range cases {
		rt := quickjs.New()
		v, err := rt.Eval(tc.src)
		if err != nil {
			t.Errorf("%s: %v", tc.src, err)
		} else if got := v.String(); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.src, got, tc.want)
		}
		rt.Close()
	}
}

// A Date setter coerces every argument it was given, in order, before it looks
// at anything else -- a valueOf can see that it was called, and is called even
// when the date is already invalid. A setter that finds an invalid date reports
// NaN without writing anything, so a valueOf that revived the date is not
// undone.
func TestDateSetters(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var log = [], d = new Date(NaN);
		  d.setHours({valueOf: function () { log.push("h"); return 1 }},
		             {valueOf: function () { log.push("m"); return 2 }});
		  log.join(",")`, "h,m"},
		{`var d = new Date(NaN);
		  var v = {valueOf: function () { d.setTime(0); return 1 }};
		  var r = d.setDate(v);
		  [String(r), String(d.getTime())].join(",")`, "NaN,0"},
		// The first argument is not optional: with none it is undefined.
		{`String(new Date(0).setHours())`, "NaN"},
		{`var d = new Date(0); d.setUTCHours(5, 6);
		  d.getUTCHours() + "," + d.getUTCMinutes()`, "5,6"},

		// toJSON is generic: it asks for a number and then for a string.
		{`Date.prototype.toJSON.call({toISOString: function () { return "x" }})`, "x"},
		{`String(Date.prototype.toJSON.call(
		      {valueOf: function () { return NaN }, toISOString: function () { return "x" }}))`,
			"null"},
		{`JSON.stringify({d: new Date(NaN)})`, `{"d":null}`},

		// The epoch is the epoch however it was arrived at.
		{`String(1 / new Date(-0).valueOf())`, "Infinity"},
		// Called rather than constructed, Date reports the time as a string.
		{`typeof Date()`, "string"},
		{`typeof Date(1970, 0)`, "string"},
		// The hint is checked.
		{`try { new Date()[Symbol.toPrimitive]("bad") } catch (e) { e.constructor.name }`,
			"TypeError"},
		{`typeof new Date()[Symbol.toPrimitive]("number")`, "number"},
		{`Object.getOwnPropertyDescriptor(Date.prototype, Symbol.toPrimitive).writable + ""`,
			"false"},
	}
	for _, tc := range cases {
		rt := quickjs.New()
		v, err := rt.Eval(tc.src)
		if err != nil {
			t.Errorf("%s: %v", tc.src, err)
		} else if got := v.String(); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.src, got, tc.want)
		}
		rt.Close()
	}
}

// Array.from and Array.of build their result with the constructor they were
// called on, and define each element through the object's own machinery -- so
// a result that refuses an element says so rather than quietly dropping it.
func TestArrayFromAndOf(t *testing.T) {
	cases := []struct{ src, want string }{
		{`Array.from([1, 2]).join(",")`, "1,2"},
		{`Array.from({length: 2, 0: "a", 1: "b"}).join(",")`, "a,b"},
		{`Array.from([1, 2], function (x) { return x * 2 }).join(",")`, "2,4"},
		{`Array.from(new Set([1, 2])).join(",")`, "1,2"},
		{`var t = {}, got; Array.from([1], function () { got = this }, t); String(got === t)`,
			"true"},
		{`try { Array.from([], null) } catch (e) { e.constructor.name }`, "TypeError"},

		// The constructor it was called on is what makes the result.
		{`function C() { this.x = 1 }
		  var r = Array.from.call(C, [1]);
		  [r instanceof C, r[0], r.length].join(",")`, "true,1,1"},
		{`function C(n) { this.n = n }
		  var r = Array.of.call(C, "a", "b");
		  [r instanceof C, r.n, r[0], r.length].join(",")`, "true,2,a,2"},
		{`Array.of(1, 2).join(",")`, "1,2"},

		// A result that cannot take an element reports it.
		{`function C() { Object.preventExtensions(this) }
		  try { Array.of.call(C, 1) } catch (e) { e.constructor.name }`, "TypeError"},
		{`var A = function () { this.length = 0; Object.preventExtensions(this) };
		  var arr = []; arr.constructor = {}; arr.constructor[Symbol.species] = A;
		  try { arr.concat([1]) } catch (e) { e.constructor.name }`, "TypeError"},

		// fill goes through the property protocol, so a frozen array refuses.
		{`var a = [1]; Object.freeze(a);
		  try { a.fill(2) } catch (e) { e.constructor.name }`, "TypeError"},
		{`[1, 2, 3].fill(9, 1).join(",")`, "1,9,9"},
		{`var o = {length: 3}; Array.prototype.fill.call(o, 7); JSON.stringify(o)`,
			`{"0":7,"1":7,"2":7,"length":3}`},
		// Called on a primitive, what was filled is the wrapper.
		{`String(Array.prototype.fill.call(true) instanceof Boolean)`, "true"},
		{`String(Array.prototype.copyWithin.call(true) instanceof Boolean)`, "true"},
	}
	for _, tc := range cases {
		rt := quickjs.New()
		v, err := rt.Eval(tc.src)
		if err != nil {
			t.Errorf("%s: %v", tc.src, err)
		} else if got := v.String(); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.src, got, tc.want)
		}
		rt.Close()
	}
}

// The Promise combinators hand each element a settle function it may call
// once, and where the whole thing settles on one element they hand it the
// capability's own function -- the same object every time, which a script can
// check. Promise.any collects the reasons it was given.
func TestPromiseCombinatorProtocol(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var r = "none";
		  Promise.any([Promise.reject(1), Promise.reject(2)]).catch(function (e) {
		    r = [e.constructor.name, e.errors.join("|")].join(",")
		  });`, "AggregateError,1|2"},
		{`var r = "none";
		  Promise.any([]).catch(function (e) {
		    r = [e.constructor.name, e.errors.length].join(",")
		  });`, "AggregateError,0"},
		// One element's thenable calling its settle function twice counts once.
		{`var r = "none", n = 0;
		  var thenable = {then: function (f) { f(1); f(2) }};
		  Promise.all([thenable, Promise.resolve(3)]).then(function (v) {
		    r = v.join(",")
		  });`, "1,3"},
		{`var r = "none";
		  Promise.allSettled([Promise.resolve(1), Promise.reject(2)]).then(function (v) {
		    r = v.map(function (o) { return o.status }).join(",")
		  });`, "fulfilled,rejected"},

		// catch and finally are generic: they add handlers to whatever they
		// were called on.
		{`var called = 0, got;
		  var o = {then: function (f, j) { called += 1; got = j; return "x" }};
		  var res = Promise.prototype.catch.call(o, 1);
		  var r = [called, res, got].join(",");`, "1,x,1"},
		// finally awaits what the callback returned before passing the outcome
		// on, and leaves the outcome alone.
		{`var r = "none", order = [];
		  Promise.resolve(7)
		    .finally(function () { order.push("f"); return Promise.resolve(0) })
		    .then(function (v) { order.push("t" + v); r = order.join(",") });`,
			"f,t7"},
		{`var r = "none";
		  Promise.reject(new RangeError()).finally(function () {})
		    .catch(function (e) { r = e.constructor.name });`, "RangeError"},

		// Promise.try builds its result with the constructor it was called on
		// and adopts a promise the callback returned.
		{`var r = "none";
		  Promise.try(function () { return 5 }).then(function (v) { r = String(v) });`, "5"},
		{`var r = "none";
		  Promise.try(function () { throw new TypeError() })
		    .catch(function (e) { r = e.constructor.name });`, "TypeError"},

		// The pair the executor is handed are anonymous.
		{`var f; new Promise(function (resolve) { f = resolve });
		  var r = [f.name, f.length].join(",");`, ",1"},
	}
	for _, tc := range cases {
		rt := quickjs.New()
		if _, err := rt.Eval(tc.src); err != nil {
			t.Errorf("%s: %v", tc.src, err)
			rt.Close()
			continue
		}
		v, err := rt.Eval(`r`)
		if err != nil {
			t.Errorf("%s: %v", tc.src, err)
		} else if got := v.String(); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.src, got, tc.want)
		}
		rt.Close()
	}
}

// `yield*` forwards in both directions and in all three ways a generator can
// be resumed: a value sent in goes to the delegate's next, an exception
// injected at the yield goes to its throw, and a forced return to its return.
func TestYieldStarForwardsEveryResumption(t *testing.T) {
	cases := []struct{ src, want string }{
		{`function* inner() { yield 1; yield 2 }
		  function* g() { yield* inner(); yield 3 }
		  [...g()].join(",")`, "1,2,3"},
		{`function* inner() { yield 1; return 5 }
		  function* g() { var v = yield* inner(); yield v }
		  [...g()].join(",")`, "1,5"},

		// A forced return reaches the delegate, which runs its finally.
		{`var log = [];
		  function* inner() { try { yield 1; yield 2 } finally { log.push("f") } }
		  function* g() { yield* inner() }
		  var it = g(); it.next();
		  var r = it.return(9);
		  [log.join(","), r.value, r.done].join("|")`, "f|9|true"},
		{`function* g() { yield* [1, 2] }
		  var it = g(); it.next(); JSON.stringify(it.return(7))`,
			`{"value":7,"done":true}`},
		// And the outer generator's own finally runs too.
		{`var log = [];
		  function* g() { try { yield* [1, 2] } finally { log.push("of") } }
		  var it = g(); it.next(); it.return(3); log.join(",")`, "of"},

		// An injected exception reaches the delegate's throw.
		{`var log = [];
		  function* inner() { try { yield 1 } catch (e) { log.push("caught:" + e); yield 2 } }
		  function* g() { yield* inner() }
		  var it = g(); it.next();
		  var r = it.throw("x");
		  [log.join(","), r.value].join("|")`, "caught:x|2"},
		// A delegate with no throw is closed, and the delegation fails.
		{`var log = [];
		  var it = {};
		  it[Symbol.iterator] = function () {
		    return {next: function () { return {done: false, value: 1} },
		            "return": function () { log.push("closed"); return {} }}
		  };
		  function* g() { yield* it }
		  var i = g(); i.next();
		  try { i.throw("x") } catch (e) { log.push(e.constructor.name) }
		  log.join(",")`, "closed,TypeError"},

		// A synchronous delegation hands the delegate's result object out as
		// it is, so nothing reads its value on the way past.
		{`var n = 0;
		  var spy = Object.defineProperty({done: false}, "value", {get: function () { n += 1 }});
		  var it = {};
		  it[Symbol.iterator] = function () { return {next: function () { return spy }} };
		  function* g() { yield* it }
		  var i = g(); i.next(); String(n)`, "0"},
	}
	for _, tc := range cases {
		rt := quickjs.New()
		v, err := rt.Eval(tc.src)
		if err != nil {
			t.Errorf("%s: %v", tc.src, err)
		} else if got := v.String(); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.src, got, tc.want)
		}
		rt.Close()
	}
}

// A generator inherits from its function's own prototype object, which
// inherits in turn from the shared one -- so what a script puts on
// `g.prototype` every generator g makes has.
func TestGeneratorInstancePrototype(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var f = function* () {}; f.prototype.x = 1; String(f().x)`, "1"},
		{`var f = function* () {}; String(Object.getPrototypeOf(f()) === f.prototype)`,
			"true"},
		{`var f = async function* () {};
		  String(Object.getPrototypeOf(f()) === f.prototype)`, "true"},
		{`var f = function* () {};
		  var shared = Object.getPrototypeOf(Object.getPrototypeOf(f()));
		  shared[Symbol.toStringTag]`, "Generator"},
		{`var f = async function* () {};
		  var shared = Object.getPrototypeOf(Object.getPrototypeOf(f()));
		  shared[Symbol.toStringTag]`, "AsyncGenerator"},
		{`function* g() { yield 1 } [...g()].join(",")`, "1"},
		{`Object.prototype.toString.call((function* () {})())`, "[object Generator]"},
	}
	for _, tc := range cases {
		rt := quickjs.New()
		v, err := rt.Eval(tc.src)
		if err != nil {
			t.Errorf("%s: %v", tc.src, err)
		} else if got := v.String(); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.src, got, tc.want)
		}
		rt.Close()
	}
}

// Sealing and freezing go through the object's own machinery rather than its
// property table: a proxy has none, and an object that refuses to make a
// property permanent has to say so.
func TestIntegrityLevels(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var o = Object.seal({a: 1});
		  [Object.isSealed(o), Object.isFrozen(o), Object.isExtensible(o)].join(",")`,
			"true,false,false"},
		{`var o = Object.freeze({a: 1});
		  [Object.isSealed(o), Object.isFrozen(o)].join(",")`, "true,true"},
		{`var a = Object.freeze([1, 2]);
		  [Object.isFrozen(a),
		   Object.getOwnPropertyDescriptor(a, "length").writable].join(",")`, "true,false"},
		{`var a = Object.seal([1]); Object.isSealed(a) + "," + Object.isFrozen(a)`,
			"true,false"},

		// A proxy is asked through its traps, and one that refuses reports it.
		{`var t = {a: 1}; var p = new Proxy(t, {});
		  Object.freeze(p); [Object.isFrozen(t), Object.isFrozen(p)].join(",")`,
			"true,true"},
		{`var p = new Proxy({a: 1}, {defineProperty: function () { return false }});
		  try { Object.freeze(p) } catch (e) { e.constructor.name }`, "TypeError"},

		// The legacy accessors go through the same define, so a refusal is an
		// error and a proxy sees it.
		{`var o = {}; Object.preventExtensions(o);
		  try { o.__defineGetter__("x", function () {}) } catch (e) { e.constructor.name }`,
			"TypeError"},
		{`var seen; var p = new Proxy({}, {defineProperty: function (t, k) {
		    seen = k; return true }});
		  p.__defineSetter__("y", function () {}); seen`, "y"},
		// And the lookups walk the chain through [[GetOwnProperty]].
		{`var base = {}; Object.defineProperty(base, "x", {get: function () { return 1 }});
		  var o = Object.create(base);
		  String(typeof o.__lookupGetter__("x"))`, "function"},
	}
	for _, tc := range cases {
		rt := quickjs.New()
		v, err := rt.Eval(tc.src)
		if err != nil {
			t.Errorf("%s: %v", tc.src, err)
		} else if got := v.String(); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.src, got, tc.want)
		}
		rt.Close()
	}
}

// TestHoistedFunctionsSeeEachOther covers a function declaration that calls
// one declared after it. Both bindings exist before either body runs, so the
// call resolves to the binding rather than to a global that would shadow it.
func TestHoistedFunctionsSeeEachOther(t *testing.T) {
	cases := []struct{ src, want string }{
		{`(function () {
		    function a() { return b() }
		    function b() { return 7 }
		    return String(a())
		  })()`, "7"},
		{`(function () {
		    function a() { return typeof b }
		    function b() {}
		    return a()
		  })()`, "function"},
		{`function a() { return b() }
		  function b() { return 7 }
		  String(a())`, "7"},
		// The later declaration wins, and both names still resolve.
		{`(function () {
		    function f() { return 1 }
		    function f() { return 2 }
		    return String(f())
		  })()`, "2"},
		// A generator or async function is hoisted the same way.
		{`(function () {
		    function a() { return g().next().value }
		    function* g() { yield 3 }
		    return String(a())
		  })()`, "3"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestSymbolToPrimitive covers Symbol.prototype[Symbol.toPrimitive], which is
// what lets a symbol wrapper compare equal to the symbol it wraps.
func TestSymbolToPrimitive(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var s = Symbol("a"); String(Object(s) == s)`, "true"},
		{`var s = Symbol("a"); String(Symbol.prototype[Symbol.toPrimitive].call(s) === s)`,
			"true"},
		// The hint is ignored: there is nothing else a symbol could produce.
		{`var s = Symbol("a")
		  var f = Symbol.prototype[Symbol.toPrimitive]
		  String(f.call(s, "string") === s && f.call(s, "number") === s)`, "true"},
		{`var f = Symbol.prototype[Symbol.toPrimitive]
		  f.name + "," + f.length`, "[Symbol.toPrimitive],1"},
		{`var d = Object.getOwnPropertyDescriptor(Symbol.prototype, Symbol.toPrimitive);
		  [d.writable, d.enumerable, d.configurable].join(",")`, "false,false,true"},
		{`var f = Symbol.prototype[Symbol.toPrimitive]
		  try { f.call(1) } catch (e) { e.constructor.name }`, "TypeError"},
		// An implicit string conversion of a symbol is still a TypeError.
		{`var s = Symbol("a"); try { Object(s) + "" } catch (e) { e.constructor.name }`,
			"TypeError"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestGeneratorNewTarget covers new.target in a generator or async function.
// Nothing constructs either, so it is undefined -- which has to be said rather
// than left to a zero value.
func TestGeneratorNewTarget(t *testing.T) {
	cases := []struct{ src, want string }{
		{`function* g() { yield new.target } String(g().next().value)`, "undefined"},
		{`function* g() { yield typeof new.target } g().next().value`, "undefined"},
		{`var g = function* () { yield new.target }
		  String(g().next().value)`, "undefined"},
		// An arrow captures the enclosing function's, which is undefined too.
		{`function* g() { yield (() => new.target)() }
		  String(g().next().value)`, "undefined"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
	checkAsync(t,
		`var o = {async m() { return async () => new.target }}
		 var count = 0
		 o.m().then(f => { count++; return f() }).then(v => { r = String(v) + "," + count })`,
		"r", "undefined,1")
	checkAsync(t,
		`async function f() { return new.target }
		 f().then(v => { r = String(v) })`, "r", "undefined")
}

// TestArrayFromAsyncProtocol covers how Array.fromAsync reads its source and
// what it builds the result with.
func TestArrayFromAsyncProtocol(t *testing.T) {
	checkAsync(t, `Array.fromAsync([1, Promise.resolve(2), 3]).then(a => { r = a.join() })`,
		"r", "1,2,3")
	// A synchronous source's values are awaited; an asynchronous one's are
	// not, so a promise it means to yield stays a promise.
	checkAsync(t, `
		var p = Promise.resolve({})
		var src = {[Symbol.asyncIterator]() {
		  var i = 0
		  return {async next() { return i++ ? {done: true} : {value: p, done: false} }}
		}}
		Array.fromAsync(src).then(a => { r = String(a.length) + "," + (a[0] === p) })`,
		"r", "1,true")

	// Called on a constructor, the result is what that constructor makes.
	checkAsync(t, `
		var log = []
		function MyArray(...args) { log.push("construct " + args.length) }
		Array.fromAsync.call(MyArray, [1, 2]).then(a => {
		  r = log.join("|") + " => " + (a instanceof MyArray) + "," + a.length
		})`,
		"r", "construct 0 => true,2")
	// An array-like tells the constructor its length up front.
	checkAsync(t, `
		var log = []
		function MyArray(...args) { log.push("construct " + args.join()) }
		Array.fromAsync.call(MyArray, {length: 2, 0: "a", 1: "b"}).then(a => {
		  r = log.join("|") + " => " + a[0] + a[1]
		})`,
		"r", "construct 2 => ab")
	// A length no array can hold is a rejection, not a hang.
	checkAsync(t, `
		Array.fromAsync.call({}, {length: 4294967296})
		  .then(() => { r = "resolved" }, e => { r = e.constructor.name })`,
		"r", "RangeError")

	// A member named by one of the iterator symbols has to be callable.
	checkAsync(t, `
		Array.fromAsync({[Symbol.iterator]: true})
		  .then(() => { r = "resolved" }, e => { r = e.constructor.name })`,
		"r", "TypeError")

	// A map function that fails closes the iterator before the rejection.
	checkAsync(t, `
		var closed = 0
		var src = {[Symbol.iterator]() { return {
		  next() { return {value: 1, done: false} },
		  return() { closed++; return {done: true} },
		}}}
		Array.fromAsync(src, () => { throw new RangeError() })
		  .then(() => { r = "resolved" }, e => { r = e.constructor.name + "," + closed })`,
		"r", "RangeError,1")
	checkAsync(t, `
		var closed = 0
		var src = {[Symbol.asyncIterator]() { return {
		  async next() { return {value: 1, done: false} },
		  async return() { closed++; return {done: true} },
		}}}
		Array.fromAsync(src, async () => { throw new RangeError() })
		  .then(() => { r = "resolved" }, e => { r = e.constructor.name + "," + closed })`,
		"r", "RangeError,1")

	// In sloppy mode a map function called with no thisArg sees the global.
	checkAsync(t, `
		Array.fromAsync([1], async function () { return this === globalThis })
		  .then(a => { r = String(a[0]) })`,
		"r", "true")
}

// TestAsyncGeneratorQueue covers calling an async generator again before the
// previous call has settled. The requests are queued rather than interleaved:
// there is one body, and letting a second call re-enter it while the first is
// suspended at an await would scramble both.
func TestAsyncGeneratorQueue(t *testing.T) {
	checkAsync(t, `
		var log = []
		var it = (async function* () { yield await "a" })()
		it.next().then(v => log.push("1:" + v.value + "," + v.done))
		it.next().then(v => log.push("2:" + v.value + "," + v.done))
		it.next().then(v => log.push("3:" + v.value + "," + v.done))
		Promise.resolve().then(() => Promise.resolve()).then(() => Promise.resolve())
		  .then(() => { r = log.join(" | ") })`,
		"r", "1:a,false | 2:undefined,true | 3:undefined,true")

	checkAsync(t, `
		var log = []
		var it = (async function* () { yield 1; yield 2; yield 3 })()
		it.next().then(v => log.push(v.value))
		it.next().then(v => log.push(v.value))
		it.next().then(v => log.push(v.value))
		Promise.resolve().then(() => Promise.resolve()).then(() => Promise.resolve())
		  .then(() => { r = log.join(",") })`,
		"r", "1,2,3")

	// The value a return injects is awaited before the generator sees it.
	checkAsync(t, `
		var it = (async function* () { yield 1 })()
		it.return(Promise.resolve(9)).then(v => { r = v.value + "," + v.done })`,
		"r", "9,true")
	// A promise whose constructor throws is a rejection, not a result.
	checkAsync(t, `
		var it = (async function* () { yield 1 })()
		var broken = Promise.resolve(42)
		Object.defineProperty(broken, "constructor", {
		  get() { throw new RangeError("broken") },
		})
		it.return(broken).then(() => { r = "resolved" }, e => { r = e.constructor.name })`,
		"r", "RangeError")

	// The prototypes are not interchangeable.
	checkAsync(t, `
		var sync = (function* () {})()
		var next = Object.getPrototypeOf(Object.getPrototypeOf((async function* () {})())).next
		next.call(sync).then(() => { r = "resolved" }, e => { r = e.constructor.name })`,
		"r", "TypeError")

	cases := []struct{ src, want string }{
		{`var p = Object.getPrototypeOf(Object.getPrototypeOf((async function* () {})()))
		  String(p.hasOwnProperty("constructor")) + "," +
		  String(p.constructor === Object.getPrototypeOf(async function* () {}))`, "true,true"},
		{`var p = Object.getPrototypeOf(Object.getPrototypeOf((function* () {})()))
		  String(p.hasOwnProperty("constructor")) + "," +
		  String(p.constructor === Object.getPrototypeOf(function* () {}))`, "true,true"},
		{`var d = Object.getOwnPropertyDescriptor(
		    Object.getPrototypeOf(Object.getPrototypeOf((function* () {})())), "constructor");
		  [d.writable, d.enumerable, d.configurable].join(",")`, "false,false,true"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestGlobalDeclarationConflicts covers a top-level declaration whose name the
// global object already holds. A property that can be deleted may always be
// replaced; one that cannot must already look like what the declaration would
// create.
func TestGlobalDeclarationConflicts(t *testing.T) {
	cases := []struct{ src, want string }{
		// An ordinary name is fine, and so is one inside a function. A
		// lexical binding eval declares is its own and collides with nothing.
		{`eval("let zzz = 1; zzz")`, "1"},
		{`eval("const [a, b] = [1, 2]; a + b")`, "3"},
		{`(function () { let undefined = 1; return undefined })()`, "1"},
		{`eval("let undefined; typeof undefined")`, "undefined"},

		// A function declaration may replace a configurable property, and one
		// that is a writable enumerable data property, but nothing else.
		{`Object.defineProperty(globalThis, "d1",
		    {configurable: false, value: 0, writable: true, enumerable: false})
		  try { eval("function d1() {}"); "no throw" } catch (e) { e.constructor.name }`,
			"TypeError"},
		{`Object.defineProperty(globalThis, "d2",
		    {configurable: false, value: 0, writable: false, enumerable: true})
		  try { eval("function d2() {}"); "no throw" } catch (e) { e.constructor.name }`,
			"TypeError"},
		{`Object.defineProperty(globalThis, "a1", {configurable: false, get() { return 1 }})
		  try { eval("function a1() {}"); "no throw" } catch (e) { e.constructor.name }`,
			"TypeError"},
		{`Object.defineProperty(globalThis, "d3",
		    {configurable: false, value: 0, writable: true, enumerable: true})
		  eval("function d3() {}"); typeof d3`, "function"},

		// Nothing new can be declared on a global that will take no more
		// properties.
		{`Object.preventExtensions(globalThis)
		  try { eval("function zz() {}"); "no throw" } catch (e) { e.constructor.name }`,
			"TypeError"},
		{`Object.preventExtensions(globalThis)
		  try { eval("var zz"); "no throw" } catch (e) { e.constructor.name }`,
			"TypeError"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}

	// A script's own top-level lexical binding does reach the global
	// environment, so one whose name the global object holds in a property
	// that cannot be removed is refused.
	for _, src := range []string{
		`let undefined`,
		`const Infinity = 1`,
		`class NaN {}`,
		`let [undefined] = []`,
	} {
		rt := quickjs.New()
		_, err := rt.Eval(src)
		if err == nil || !strings.Contains(err.Error(), "SyntaxError") {
			t.Errorf("%s: got %v, want a SyntaxError", src, err)
		}
		rt.Close()
	}
}

// TestNumberFormatting covers the three methods that place a decimal point.
// Each rounds to the larger value on a tie, which is what the specification
// asks for and not what formatting a float would do.
func TestNumberFormatting(t *testing.T) {
	cases := []struct{ src, want string }{
		// toPrecision keeps the trailing zeros of the mantissa.
		{`(100).toPrecision(2)`, "1.0e+2"},
		{`(123).toPrecision(1)`, "1e+2"},
		{`(123.456).toPrecision(5)`, "123.46"},
		{`(1e21).toPrecision(2)`, "1.0e+21"},
		{`(0).toPrecision(3)`, "0.00"},
		{`(-1.5).toPrecision(3)`, "-1.50"},
		{`(0.000001).toPrecision(1)`, "0.000001"},
		{`(1e-7).toPrecision(3)`, "1.00e-7"},
		{`(25).toPrecision(1)`, "3e+1"},

		// toExponential, where a tie goes up and negative zero has no sign.
		{`(25).toExponential(0)`, "3e+1"},
		{`(-0).toExponential(0)`, "0e+0"},
		{`(-0).toExponential(1)`, "0.0e+0"},
		{`(123).toExponential(2)`, "1.23e+2"},
		{`(123).toExponential()`, "1.23e+2"},
		{`(1.45).toExponential(1)`, "1.4e+0"},

		// toFixed, which hands a magnitude of 10**21 or more to ToString.
		{`(1e21).toFixed()`, "1e+21"},
		{`(0.5).toFixed(0)`, "1"},
		{`(1.5).toFixed(0)`, "2"},
		{`(2.5).toFixed(0)`, "3"},
		{`(-1.5).toFixed(0)`, "-2"},
		{`(1.45).toFixed(1)`, "1.4"},
		{`(123.456).toFixed(2)`, "123.46"},
		{`(0).toFixed(2)`, "0.00"},
		{`(5e-10).toFixed(9)`, "0.000000001"},
		{`(4e-10).toFixed(9)`, "0.000000000"},
		{`(1000000000000000128).toFixed(0)`, "1000000000000000128"},

		// The argument is coerced before the value is looked at.
		{`try { NaN.toExponential(Symbol()) } catch (e) { e.constructor.name }`, "TypeError"},
		{`try { NaN.toPrecision(Symbol()) } catch (e) { e.constructor.name }`, "TypeError"},
		{`NaN.toPrecision(1)`, "NaN"},

		// Number.parseInt is the global function, not a copy of it.
		{`String(Number.parseInt === parseInt) + "," + String(Number.parseFloat === parseFloat)`,
			"true,true"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestHashbangAndCommentTerminators covers two pieces of source that are not
// code: the hashbang a shell reads, and a line terminator inside a comment.
func TestHashbangAndCommentTerminators(t *testing.T) {
	cases := []struct{ src, want string }{
		{"String(eval('#!/usr/bin/env node\\n1'))", "1"},
		{"String(eval('#!only'))", "undefined"},
		// Only at the very start: nothing may precede it, not even space.
		{"try { eval(' #!/x\\n1') } catch (e) { e.constructor.name }", "SyntaxError"},
		{"try { eval('1\\n#!x') } catch (e) { e.constructor.name }", "SyntaxError"},

		// A line terminator inside a comment still ends a statement.
		{"String(eval(\"''/*\\r*/''\"))", ""},
		{"String(eval(\"''/*\\u2028*/''\"))", ""},
		{"String(eval('1/*\\n*/2'))", "2"},
		{"String(eval('var a = 1/*\\n*/+2; a'))", "3"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestOptionalChainEdges covers two shapes an optional chain may and may not
// take: a super reference at its head, and a tagged template anywhere in it.
func TestOptionalChainEdges(t *testing.T) {
	cases := []struct{ src, want string }{
		{`class B { m() { return 1 } }
		  class C extends B { go() { return super.m?.() } }
		  String(new C().go())`, "1"},
		{`class B {}
		  class C extends B { go() { return super.m?.() } }
		  String(new C().go())`, "undefined"},
		{`class B { m() { return 3 } }
		  class C extends B { go() { return super["m"]?.() } }
		  String(new C().go())`, "3"},
		{`class B { m() { return 2 } }
		  class C extends B { go() { return super.m?.x } }
		  String(new C().go())`, "undefined"},
		{`class B { get p() { return {q: 4} } }
		  class C extends B { go() { return super.p?.q } }
		  String(new C().go())`, "4"},

		// A tag cannot be told that its chain short-circuited, so the grammar
		// refuses the combination outright.
		{"try { eval('a?.b`x`') } catch (e) { e.constructor.name }", "SyntaxError"},
		{"try { eval('a?.b.c`x`') } catch (e) { e.constructor.name }", "SyntaxError"},
		{"try { eval('a?.b()`x`') } catch (e) { e.constructor.name }", "SyntaxError"},
		{"try { eval('null?.`x`') } catch (e) { e.constructor.name }", "SyntaxError"},
		// An ordinary tagged template is unaffected.
		{"function t(s) { return s[0] } t`x`", "x"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestFinallyOverridesCompletion covers a break or continue written inside a
// finally clause, which replaces the completion that brought control there.
// The clause is not pending while its own body runs, so such a jump leaves
// through the clauses outside it rather than through itself again.
func TestFinallyOverridesCompletion(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var c = 0, fin = 0
		  do {
		    try { c += 1; break } catch (e) {} finally { fin = 1; continue }
		    fin = -1
		    c += 2
		  } while (c < 2)
		  fin + "," + c`, "1,2"},
		{`var c = 0, fin = 0
		  do {
		    try { c += 1; throw "x" } catch (e) { break } finally { fin = 1; continue }
		    c += 2
		  } while (c < 2)
		  fin + "," + c`, "1,2"},
		{`var r = ""
		  for (var i = 0; i < 3; i++) { try { r += "t"; continue } finally { r += "f" } }
		  r`, "tftftf"},
		{`var r = ""
		  outer: for (var i = 0; i < 2; i++) { try { break outer } finally { r += "f" } }
		  r`, "f"},
		// A nested clause still runs the one outside it.
		{`var r = ""
		  for (var i = 0; i < 1; i++) {
		    try { try { break } finally { r += "i" } } finally { r += "o" }
		  }
		  r`, "io"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestDerivedConstructorResult covers what `new` on a derived class produces.
// Which object comes back is settled where the constructor actually returns,
// because a finally clause may still call super() after the return statement.
func TestDerivedConstructorResult(t *testing.T) {
	cases := []struct{ src, want string }{
		{`class C extends class {} {
		    constructor() { try { throw null } catch (e) { return } finally { super() } }
		  }
		  typeof new C()`, "object"},
		{`class C extends class {} { constructor() { super() } } typeof new C()`, "object"},
		{`class C extends class {} { constructor() { super(); return {x: 1} } }
		  String(new C().x)`, "1"},
		{`class C extends class {} { constructor() { return {x: 2} } }
		  String(new C().x)`, "2"},

		// Returning nothing before super() has run is a ReferenceError; a
		// value that is neither an object nor undefined is a TypeError.
		{`class C extends class {} { constructor() { return } }
		  try { new C() } catch (e) { e.constructor.name }`, "ReferenceError"},
		{`class C extends class {} { constructor() {} }
		  try { new C() } catch (e) { e.constructor.name }`, "ReferenceError"},
		{`class C extends class {} { constructor() { super(); return 1 } }
		  try { new C() } catch (e) { e.constructor.name }`, "TypeError"},
		// An arrow inside one shares the binding but is not the constructor.
		{`class C extends class {} {
		    constructor() { var f = () => 1; super(); this.v = f() }
		  }
		  String(new C().v)`, "1"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestDateParsing covers Date.parse, which has to read back what the Date
// methods write, and the extended year form an out-of-range date needs.
func TestDateParsing(t *testing.T) {
	cases := []struct{ src, want string }{
		// Every form a Date writes round-trips, including the zone name
		// toString puts in parentheses after the offset.
		{`var d = new Date(0); String(Date.parse(d.toString()))`, "0"},
		{`var d = new Date(1234567890000); String(Date.parse(d.toString()))`,
			"1234567890000"},
		{`var d = new Date(0); String(Date.parse(d.toUTCString()))`, "0"},
		{`var d = new Date(1234567890123); String(Date.parse(d.toISOString()))`,
			"1234567890123"},

		// The extreme dates use a six-digit year, which is not a shape any
		// ordinary layout describes.
		{`String(Date.parse(new Date(-8640000000000000).toISOString()))`,
			"-8640000000000000"},
		{`String(Date.parse(new Date(8640000000000000).toISOString()))`,
			"8640000000000000"},
		{`new Date(Date.parse("+020000-02-29T00:00:00.000Z")).toISOString()`,
			"+020000-02-29T00:00:00.000Z"},
		{`new Date("-000001-07-01T00:00Z").toString().split(" ")[3]`, "-0001"},
		// There is no year minus zero.
		{`String(Date.parse("-000000-03-31T00:45Z"))`, "NaN"},

		// A Date argument is taken at its time value rather than through its
		// string form.
		{`var d = new Date(0); String(new Date(d).valueOf())`, "0"},
		{`var d = new Date(1234567890123); String(new Date(d).valueOf())`, "1234567890123"},

		// setFullYear revives an invalid date from the epoch, which is a local
		// time value rather than a moment to be converted.
		{`var d = new Date(NaN); d.setFullYear(2016, 0, 1)
		  String(d.valueOf() === new Date(2016, 0, 1).valueOf())`, "true"},
		{`var d = new Date(NaN); d.setMonth(3); String(d.valueOf())`, "NaN"},

		// The hint is compared as given rather than coerced.
		{`try { new Date()[Symbol.toPrimitive](Object("number")) } catch (e) { e.constructor.name }`,
			"TypeError"},
		{`try { new Date()[Symbol.toPrimitive]("String") } catch (e) { e.constructor.name }`,
			"TypeError"},
		{`String(new Date(0)[Symbol.toPrimitive]("number"))`, "0"},

		// getTimezoneOffset is not rounded to whole minutes.
		{`var d = new Date(1899, 11); String(d.valueOf() - d.getTimezoneOffset() * 60000)`,
			"-2211667200000"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestGoCallbackRethrowsJavaScriptErrors covers an error a Go callback returns
// after letting one through from script: it is the same value again, not a new
// Error wrapping its text.
func TestGoCallbackRethrowsJavaScriptErrors(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	rt.Set("runScript", func(r *quickjs.Runtime, src string) (quickjs.Value, error) {
		return r.Eval(src)
	})
	cases := []struct{ src, want string }{
		{`try { runScript("null.x") } catch (e) { e.constructor.name }`, "TypeError"},
		{`try { runScript("undeclared") } catch (e) { e.constructor.name }`, "ReferenceError"},
		{`try { runScript("throw new RangeError('boom')") } catch (e) {
		    e.constructor.name + ":" + e.message
		  }`, "RangeError:boom"},
		{`try { runScript("throw 42") } catch (e) { typeof e + ":" + e }`, "number:42"},
		// Source that does not compile is a SyntaxError.
		{`try { runScript("(") } catch (e) { e.constructor.name }`, "SyntaxError"},
	}
	for _, tc := range cases {
		v, err := rt.Eval(tc.src)
		if err != nil {
			t.Errorf("%s: %v", tc.src, err)
		} else if got := v.String(); got != tc.want {
			t.Errorf("%s\n got: %s\nwant: %s", tc.src, got, tc.want)
		}
	}
}

// TestSymbolAndErrorShape covers a few pieces of the built-in objects that a
// script can see but nothing else depends on.
func TestSymbolAndErrorShape(t *testing.T) {
	cases := []struct{ src, want string }{
		// Symbol.prototype has valueOf and a toStringTag of its own.
		{`var s = Symbol("a"); String(Symbol.prototype.valueOf.call(s) === s)`, "true"},
		{`var s = Symbol("a"); String(Object(s).valueOf() === s)`, "true"},
		{`try { Symbol.prototype.valueOf.call(1) } catch (e) { e.constructor.name }`,
			"TypeError"},
		{`Symbol.prototype[Symbol.toStringTag]`, "Symbol"},
		{`Object.prototype.toString.call(Object(Symbol("a")))`, "[object Symbol]"},

		// AggregateError takes the errors before the message, so it has one
		// more parameter -- and reads them last, after the message and the
		// options, because draining them runs user code.
		{`String(AggregateError.length) + "," + String(Error.length)`, "2,1"},
		{`var order = []
		  var errors = {[Symbol.iterator]() { order.push("errors"); return [][Symbol.iterator]() }}
		  var message = {toString() { order.push("message"); return "" }}
		  var options = {get cause() { order.push("cause"); return 1 }}
		  new AggregateError(errors, message, options)
		  order.join()`, "message,cause,errors"},
		{`var e = new AggregateError([1, 2], "m"); e.errors.join() + "|" + e.message`, "1,2|m"},

		// A native constructor keeps the prototype it chose when new.target
		// names one that is not an object.
		{`function F() {}
		  F.prototype = undefined
		  Object.prototype.toString.call(Reflect.construct(ArrayBuffer, [8], F))`,
			"[object ArrayBuffer]"},

		// Slicing a detached buffer is a TypeError rather than an empty copy.
		{`var b = new ArrayBuffer(4); b.transfer()
		  try { b.slice() } catch (e) { e.constructor.name }`, "TypeError"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestFunctionExpressionName covers the name a function expression gives
// itself: a binding of a scope around its body, immutable, and visible to
// everything written inside it.
func TestFunctionExpressionName(t *testing.T) {
	cases := []struct{ src, want string }{
		// A nested function sees it, which only a real binding can manage.
		{`var f = function g() { return typeof (() => g)() }; f()`, "function"},
		{`var f = function g() { return typeof (function () { return g })() }; f()`,
			"function"},
		{`var f = function g() { return eval("typeof g") }; f()`, "function"},

		// It is immutable: a strict assignment is refused and a sloppy one is
		// quietly discarded, wherever it is written.
		{`var f = function g() { "use strict"; g = 1 }
		  try { f() } catch (e) { e.constructor.name }`, "TypeError"},
		{`var f = function g() { "use strict"; return (() => { g = 1 })() }
		  try { f() } catch (e) { e.constructor.name }`, "TypeError"},
		{`var f = function g() { "use strict"; return eval("g = 1") }
		  try { f() } catch (e) { e.constructor.name }`, "TypeError"},
		{`var f = function g() { g = 1; return typeof g }; f()`, "function"},
		{`var f = function g() { return (() => { g = 1; return typeof g })() }; f()`,
			"function"},

		// A parameter, a var or a top-level let of the same name shadows it.
		{`var f = function g(g) { return g }; String(f(5))`, "5"},
		{`var f = function g() { var g = 1; return g }; String(f())`, "1"},
		{`var f = function n() { let n = "inside"; return n }; f()`, "inside"},
		{`var f = function n() { let n = "inside"; return (() => n)() }; f()`, "inside"},
		// A binding in a block does not.
		{`var f = function g() { { let g = 1 } return typeof g }; f()`, "function"},

		// A function declaration's name is an ordinary binding of the
		// enclosing scope, which reassigning replaces for everyone.
		{`function f(n) { return n ? f(n - 1) : "orig" }
		  var g = f
		  f = function () { return "new" }
		  g(1)`, "new"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestBigIntBitwise covers the bitwise operators on BigInts, which have no
// width: the result is whatever the arithmetic says, not a wrapped 32 bits.
func TestBigIntBitwise(t *testing.T) {
	cases := []struct{ src, want string }{
		{`String(12n & 10n)`, "8"},
		{`String(12n | 10n)`, "14"},
		{`String(12n ^ 10n)`, "6"},
		{`String(~5n)`, "-6"},
		{`String(~(-1n))`, "0"},
		{`String(1n << 64n)`, "18446744073709551616"},
		{`String(16n >> 2n)`, "4"},
		{`String(-16n >> 2n)`, "-4"},
		// Shifting past every bit leaves the sign.
		{`String(1n >> 100n)`, "0"},
		{`String(-1n >> 100n)`, "-1"},
		// A negative count shifts the other way.
		{`String(1n << -1n)`, "0"},
		{`String(4n >> -1n)`, "8"},
		// The operands are coerced, and mixing kinds is an error.
		{`String(1n >> {valueOf() { return 1n }})`, "0"},
		{`try { 1n << 1 } catch (e) { e.constructor.name }`, "TypeError"},
		{`try { 1 & 1n } catch (e) { e.constructor.name }`, "TypeError"},
		// There is no unsigned shift: a BigInt has no sign bit to shift out.
		{`try { 1n >>> 1n } catch (e) { e.constructor.name }`, "TypeError"},
		// Numbers are unaffected.
		{`String(12 & 10) + "," + String(1 << 31) + "," + String(-1 >>> 0)`,
			"8,-2147483648,4294967295"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestBigIntPropertyKey covers a BigInt literal used as a property name, which
// names the property its digits spell.
func TestBigIntPropertyKey(t *testing.T) {
	cases := []struct{ src, want string }{
		{`Object.keys({999999999999999999n: 1}).join()`, "999999999999999999"},
		{`Object.keys({1n: "a", 0x10n: "b"}).join()`, "1,16"},
		{`JSON.stringify({1n: "a"})`, `{"1":"a"}`},
		{`({1n: "a"})[1]`, "a"},
		{`class C { 1n = 2 } Object.keys(new C()).join()`, "1"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestPromiseWithResolvers covers Promise.withResolvers, which builds its
// promise with the constructor it was reached through.
func TestPromiseWithResolvers(t *testing.T) {
	cases := []struct{ src, want string }{
		{`class S extends Promise {}
		  String(S.withResolvers().promise.constructor === S)`, "true"},
		{`String(Promise.withResolvers().promise.constructor === Promise)`, "true"},
		{`try { Promise.withResolvers.call({}) } catch (e) { e.constructor.name }`,
			"TypeError"},
		{`var r = Promise.withResolvers();
		  [typeof r.promise, typeof r.resolve, typeof r.reject].join()`,
			"object,function,function"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
	checkAsync(t, `
		var w = Promise.withResolvers()
		w.promise.then(v => { r = String(v) })
		w.resolve(5)`, "r", "5")
}

// TestIdentifierExclusions covers the few characters Unicode's derived
// identifier properties take back out: they are letters by category but are
// reserved for syntax.
func TestIdentifierExclusions(t *testing.T) {
	cases := []struct{ src, want string }{
		{"try { eval('var aⸯ = 1'); \"no throw\" } catch (e) { e.constructor.name }",
			"SyntaxError"},
		{"try { eval('var ⸯ = 1'); \"no throw\" } catch (e) { e.constructor.name }",
			"SyntaxError"},
		{`try { eval("var \\u2e2f = 1"); "no throw" } catch (e) { e.constructor.name }`,
			"SyntaxError"},
		// Ordinary letters outside ASCII are still identifiers.
		{"var café = 1; String(café)", "1"},
		{"var αβ = 2; String(αβ)", "2"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// A catch clause's parameter is a binding like a let: it is initialized when
// the clause is entered, and the body may assign to it.
func TestCatchParameterIsMutable(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"assigned", `try { throw 1 } catch (e) { e = 2; String(e) }`, "2"},
		{"in a function", `(function () {
		  try { throw 1 } catch (e) { e = 2; return String(e) } })()`, "2"},
		{"through a closure", `(function () {
		  try { throw 1 } catch (e) {
		    var f = function () { e = 3 }; f(); return String(e) } })()`, "3"},
		{"destructured", `try { throw {e: 1} } catch ({e}) { e = 2; String(e) }`, "2"},
		// Shadowing is what the scope is for: the assignment changes the
		// parameter, not what the name meant outside.
		{"shadows a let", `let e = "outer"
		  try { throw "caught" } catch (e) { e = "inner" }
		  e`, "outer"},
		{"shadows a parameter", `(function (e) {
		  try { throw "caught" } catch (e) { e = "inner" }
		  return e })("param")`, "param"},
		// A const still refuses, and says which one.
		{"const still refuses", `(function () {
		  try { const k = 1; k = 2 } catch (e) { return e.message } })()`,
			`assignment to constant variable "k"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { checkEval(t, tc.src, tc.want) })
	}
}

// A break or continue that leaves a finally clause abandons the completion the
// clause was running for, and the two operands that record it: what it jumps to
// is not expecting values it never pushed.
func TestJumpOutOfFinally(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"continue from a finally", `var cars = {a: 1, b: 2, c: 3}, c = 0, fin = 0
		  for (var x in cars) {
		    try { throw "ex" } catch (e) { c += 1 } finally { fin = 1; continue }
		    fin = 0
		  }
		  c + "," + fin`, "3,1"},
		{"continue with no catch", `var c = 0, fin = 0
		  for (var i = 0; i < 3; i++) {
		    try { c += 1; throw "ex" } finally { fin = 1; continue }
		    fin = -1
		  }
		  c + "," + fin`, "3,1"},
		{"break from a finally", `var c = 0
		  for (var x of [1, 2, 3]) { try { c += 1 } finally { break } }
		  String(c)`, "1"},
		{"continue in a for-of", `var c = 0
		  for (var x of [1, 2, 3]) { try { c += 1; throw "e" } catch (e) {} finally { continue } }
		  String(c)`, "3"},
		// The ordinary paths still run the clause and keep the completion.
		{"return through a finally", `function f() { try { return 1 } finally { } }
		  String(f())`, "1"},
		{"throw through a finally", `var fin = 0
		  try { (function () { try { throw "e" } finally { fin = 1 } })() } catch (e) {}
		  String(fin)`, "1"},
		{"nested loops", `var pairs = []
		  outer: for (var i of [1, 2]) {
		    for (var j of [1, 2]) { try { pairs.push(i + "" + j) } finally { continue outer } }
		  }
		  pairs.join()`, "11,21"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { checkEval(t, tc.src, tc.want) })
	}
}

// An optional chain in parentheses keeps the reference it produced, so the
// object it read from is what a call after it binds `this` to. A chain also
// carries spread arguments and a super call like any other expression.
func TestOptionalChainCalls(t *testing.T) {
	const setup = `var a = {b: function () { return this._b }, _b: {c: 42}};`
	cases := []struct{ name, src, want string }{
		{"method in a chain", setup + `String(a?.b().c)`, "42"},
		{"parenthesized chain", setup + `String((a?.b)().c)`, "42"},
		{"optional call", setup + `String(a.b?.().c)`, "42"},
		{"parenthesized member", setup + `String((a.b)?.().c)`, "42"},
		{"both optional", setup + `String(a?.b?.().c)`, "42"},
		{"parenthesized and optional", setup + `String((a?.b)?.().c)`, "42"},
		// A chain that short-circuits has no receiver and no method, and
		// calling what it produced is the error it should be.
		{"short circuit then call", `var a = null
		  try { (a?.b)(); "no error" } catch (e) { e.constructor.name }`, "TypeError"},
		{"short circuit optional call", `var a = null; String((a?.b)?.())`, "undefined"},
		// Spread arguments work in every position a chain allows.
		{"spread in an optional call", `var o = {m: function () { return arguments.length }}
		  String(o.m?.(...[1, 2]))`, "2"},
		{"spread in a chained call", `var o = {m: function () { return arguments.length }}
		  String(o?.m(...[1, 2, 3]))`, "3"},
		{"spread with a plain callee", `function f() { return arguments.length }
		  var g = f; String(g?.(...[1, 2]))`, "2"},
		// A super call may be the base of a chain, and what it produces is the
		// object it constructed.
		{"super call in a chain", `var out
		  class B { constructor() { this.a = 7 } }
		  class C extends B { constructor() { out = String(super()?.a) } }
		  new C(); out`, "7"},
		{"super call without the property", `var out
		  class B {}
		  class C extends B { constructor() { out = String(super()?.a) } }
		  new C(); out`, "undefined"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { checkEval(t, tc.src, tc.want) })
	}
}

// Comparing a BigInt with a string reads the string as a BigInt literal rather
// than as a number: a string that is not an integer literal compares with
// nothing, and every relational operator on it is false.
func TestBigIntStringComparison(t *testing.T) {
	cases := []struct{ src, want string }{
		{`"1" < 2n`, "true"},
		{`"3" > 2n`, "true"},
		{`2n <= "2"`, "true"},
		// Exactly, rather than through a float that cannot hold it.
		{`"9007199254740993" <= 9007199254740993n`, "true"},
		{`"9007199254740993" < 9007199254740994n`, "true"},
		// Whitespace is allowed around it, and nothing but whitespace is zero.
		{`"  2  " < 3n`, "true"},
		{`"" < 1n`, "true"},
		// Anything that is not an integer literal is incomparable.
		{`"0." <= 1n`, "false"},
		{`"0." > 1n`, "false"},
		{`".0" <= 1n`, "false"},
		{`"0e0" <= 1n`, "false"},
		{`"0n" <= 1n`, "false"},
		{`"Infinity" <= 1n`, "false"},
		{`"abc" < 1n`, "false"},
		{`1n < "abc"`, "false"},
		{`0n <= "1e0"`, "false"},
		// The other literal forms are integers and do compare.
		{`"0x10" > 15n`, "true"},
		{`"-3" < 0n`, "true"},
	}
	for _, tc := range cases {
		checkEval(t, "String("+tc.src+")", tc.want)
	}
}

// A jump into a finally clause leaves every handler between it and the jump,
// the clause's own included: a clause still protected by itself would run a
// second time when the completion it was carrying reached the end.
func TestFinallyRunsOnce(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"return from a try with a catch", `var n = 0
		  var f = function () {
		    try { return "try" } catch (e) { return "catch" } finally { n += 1 }
		    return "wat"
		  }
		  f() + "," + n`, "try,1"},
		{"return from the catch", `var n = 0
		  var f = function () {
		    try { throw "t" } catch (e) { return "catch" } finally { n += 1 }
		    return "wat"
		  }
		  f() + "," + n`, "catch,1"},
		{"return with no catch", `var n = 0
		  var f = function () { try { return "try" } finally { n += 1 } }
		  f() + "," + n`, "try,1"},
		{"the finally returns", `var n = 0
		  var f = function () { try { return "try" } finally { n += 1; return "fin" } }
		  f() + "," + n`, "fin,1"},
		{"nested finallys", `var order = []
		  var f = function () {
		    try { try { return "x" } finally { order.push("inner") } }
		    finally { order.push("outer") }
		  }
		  f() + "," + order.join()`, "x,inner,outer"},
		{"a throw through both", `var order = []
		  try {
		    (function () { try { try { throw "e" } finally { order.push("inner") } }
		      finally { order.push("outer") } })()
		  } catch (e) { order.push("caught") }
		  order.join()`, "inner,outer,caught"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { checkEval(t, tc.src, tc.want) })
	}
}
