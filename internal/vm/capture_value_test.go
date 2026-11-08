package vm

import (
	"strconv"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

// TestCaptureByValue pins what a closure sees of a binding it may take by
// value (bytecode.UpvalueDesc's ByValue) -- a const initialized when it is
// made, a strict function's parameter nothing stores to -- and of every
// binding that looks like one and is not: one assigned after the closure
// is made, by its maker, by another closure or one a closure makes, by an
// update, by an eval, through a mapped arguments object; one still in its
// dead zone, as a hoisted function or a switch's other case sees it, or a
// parameter list's later parameter. Each on both tiers.
func TestCaptureByValue(t *testing.T) {
	defer SetTreeTier(true)
	for _, tt := range []struct {
		name, src, want string
	}{
		{name: "param", src: `'use strict'; function f(x) { const g = () => x; return g() + g() } f(21)`, want: "42"},
		{name: "params three", src: `'use strict'; function f(a, b, c) { return () => a + b * c } f(1, 2, 3)()`, want: "7"},
		{name: "const", src: `'use strict'; function f() { const y = 5; return () => y } f()()`, want: "5"},
		{name: "loop consts", src: `'use strict'; function f() { const fs = []; for (const v of [1, 2, 3]) { const w = v * 10; fs.push(() => v + w) } return fs.map((g) => g()).join() } f()`, want: "11,22,33"},
		{name: "after return", src: `'use strict'; function mk(x) { return () => x } const p = mk(1), q = mk(2); p() + ',' + q()`, want: "1,2"},
		{name: "mixed", src: `'use strict'; function f(a, b) { const g = function () { return a + b }; b = 10; return g() } f(1, 2)`, want: "11"},
		{name: "nested", src: `'use strict'; function f(x) { return () => () => x } f(9)()()`, want: "9"},
		{name: "assigned after", src: `'use strict'; function f(x) { const g = () => x; x = 2; return g() } f(1)`, want: "2"},
		{name: "assigned before", src: `'use strict'; function f(x) { x = 3; const g = () => x; return g() } f(1)`, want: "3"},
		{name: "updated", src: `'use strict'; function f(x) { const g = () => x; x++; return g() } f(1)`, want: "2"},
		{name: "updated as a key", src: `'use strict'; function f(x, a) { const g = () => x; a[x++]; return g() } f(0, [7])`, want: "1"},
		{name: "compound", src: `'use strict'; function f(x) { const g = () => x; x += 5; return g() } f(1)`, want: "6"},
		{name: "destructured", src: `'use strict'; function f(x) { const g = () => x; [x] = [8]; return g() } f(1)`, want: "8"},
		{name: "loop target", src: `'use strict'; function f(x) { const g = () => x; for (x of [4]) {} return g() } f(1)`, want: "4"},
		{name: "sibling assigns", src: `'use strict'; function f(x) { const g = () => x; const h = () => { x = 3 }; h(); return g() } f(1)`, want: "3"},
		{name: "grandchild assigns", src: `'use strict'; function f(x) { const g = () => x; const h = () => () => { x = 4 }; h()(); return g() } f(1)`, want: "4"},
		{name: "itself assigns", src: `'use strict'; function f(x) { const g = () => { x++; return x }; return g() + g() } f(1)`, want: "5"},

		{name: "sloppy arguments", src: `function f(x) { const g = () => x; arguments[0] = 5; return g() } f(1)`, want: "5"},
		{name: "hoisted function", src: `'use strict'; function f() { function g() { return y } const y = 7; return g() } f()`, want: "7"},
		{name: "switch case", src: `'use strict'; function f(k) { switch (k) { case 0: const y = 1; case 1: return (() => typeof y)() } } try { f(1) } catch (e) { e.name }`, want: "ReferenceError"},
		{name: "later parameter", src: `function f(g = () => b, b = 8) { return g() } f()`, want: "8"},
		{name: "class name", src: `'use strict'; class C { static m() { return C } } C.m() === C`, want: "true"},
		{name: "generator", src: `'use strict'; function* f(x) { const g = () => x; yield 1; yield g() } const it = f(3); it.next(); it.next().value`, want: "3"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, on := range []bool{false, true} {
				SetTreeTier(on)
				r := New(Config{})
				v, err := r.Run(compileForTest(t, tt.src))
				if err != nil {
					r.Close()
					t.Fatalf("tree %v: %v", on, err)
				}
				got, err := r.ToString(v)
				if err != nil || got.Go() != tt.want {
					t.Errorf("tree %v: got %v, %v; want %s", on, got, err, tt.want)
				}
				r.Close()
			}
		})
	}
}

// TestCaptureByValueAllocs pins what a closure over its strict maker's
// parameters costs: an arrow over three it takes by value is the function
// object alone, its copies in the same allocation; a function taking one
// by value and sharing another, the object and one room for both cells,
// as when it shared both.
func TestCaptureByValueAllocs(t *testing.T) {
	skipUnlessAllocsOrdinary(t)
	defer SetTreeTier(true)
	for _, tt := range []struct {
		name, src string
		want      float64
	}{
		{name: "by value", src: `'use strict'; function f(a, b, c) { return () => a + b + c }; for (let i = 0; i < 10; i++) f(1, 2, 3); f`, want: 1},
		{name: "mixed", src: `'use strict'; function f(a, b, c) { b++; return function () { return a + b + c } }; for (let i = 0; i < 10; i++) f(1, 2, 3); f`, want: 2},
	} {
		for _, on := range []bool{false, true} {
			SetTreeTier(on)
			r := New(Config{})
			v, err := r.Run(compileForTest(t, tt.src))
			if err != nil {
				t.Fatal(err)
			}
			args := []Value{Int(1), Int(2), Int(3)}
			if n := testing.AllocsPerRun(100, func() {
				if _, err := r.Call(v, Undefined, args); err != nil {
					t.Fatal(err)
				}
			}); n != tt.want {
				t.Errorf("%s, tree %v: %v allocations a call, want %v", tt.name, on, n, tt.want)
			}
			r.Close()
		}
	}
}

// captureCase is a script whose completion value is a function of no
// arguments, and what every call of it gives, as Node gives it: a case of a
// binding a closure may take by value (bytecode.UpvalueDesc's ByValue) or
// one that only looks like one -- a dead zone a closure is made in, an
// assignment by any route the analysis must see (an arguments object, an
// eval, a with, a hoisted declaration, a class element, a generator or
// async function resumed, a finally, a nested closure, the closure
// itself), and a module's or a script's top-level bindings are pinned at
// the root. A promise is waited for.
type captureCase struct{ name, want, src string }

var captureCases = []captureCase{
	{name: "tdz hoisted fn", want: "ReferenceError,7", src: `'use strict'; (function () { function g() { return y } let r; try { r = g() } catch (e) { r = e.name } const y = 7; return r + ',' + g() })`},
	{name: "tdz block hoisted", want: "ReferenceError,3", src: `'use strict'; (function () { { function g() { return y } let r; try { r = g() } catch (e) { r = e.name } const y = 3; return r + ',' + g() } })`},
	{name: "tdz sloppy annexB", want: "ReferenceError,4", src: `(function () { { function g() { return y } let r; try { r = g() } catch (e) { r = e.name } const y = 4; return r + ',' + g() } })`},
	{name: "tdz switch", want: "ReferenceError,1", src: `'use strict'; (function () { const out = []; for (const k of [1, 0]) { switch (k) { case 0: const y = 1; case 1: const g = () => y; try { out.push(g()) } catch (e) { out.push(e.name) } } } return out.join() })`},
	{name: "tdz switch made early", want: "threw ReferenceError", src: `'use strict'; (function () { let g; for (const k of [1, 0]) { switch (k) { case 1: g = () => y; break; case 0: const y = 5; return g() + ',' + (() => y)() } } })`},
	{name: "class name static", want: "C,true,true,true", src: `'use strict'; (function () { class C { static a = C.name; static f = () => C; static { this.b = C === this } static m() { return C } } return [C.a, C.f() === C, C.b, C.m() === C].join() })`},
	{name: "class name field", want: "true,true", src: `'use strict'; (function () { class C { g = () => C; h = C } const c = new C(); return (c.g() === C) + ',' + (c.h === C) })`},
	{name: "class heritage", want: "true", src: `'use strict'; (function () { let f; class C extends (f = () => C, Object) {} return f() === C })`},
	{name: "class computed key", want: "true", src: `'use strict'; (function () { let f; class C { [(f = () => C, 'm')]() {} } return f() === C })`},
	{name: "class expr name", want: "true", src: `'use strict'; (function () { let f; const D = class C { static [(f = () => C, 'k')] = 1 }; return f() === D })`},
	{name: "destructure later", want: "6", src: `'use strict'; (function () { const { a = () => b, b } = { b: 6 }; return String(a()) })`},
	{name: "destructure earlier", want: "5", src: `'use strict'; (function () { const [p, q = () => p] = [5]; return q() })`},
	{name: "const self", want: "true", src: `'use strict'; (function () { const f = () => f; return f() === f })`},
	{name: "const in initializer", want: "4", src: `'use strict'; (function () { let g; const x = (g = () => x, 4); return g() })`},
	{name: "const two decls", want: "3", src: `'use strict'; (function () { let g; const a = (g = () => b, 1), b = 2; return g() + a })`},
	{name: "eval closure in tdz", want: "1", src: `(function () { const g = () => eval('() => x'); const h = g(); const x = 1; return h() })`},
	{name: "eval closure in tdz strict", want: "2", src: `'use strict'; (function () { const g = () => eval('() => x'); const h = g(); const x = 2; return h() })`},
	{name: "eval tdz direct", want: "3", src: `(function () { let h; try { h = eval('() => x') } catch (e) { return e.name } const x = 3; return h() })`},
	{name: "for-of head tdz", want: "ReferenceError", src: `(function () { let f; try { for (const x of (f = () => x, [1])) {} } catch (e) { return 'threw ' + e.name } try { return String(f()) } catch (e) { return e.name } })`},
	{name: "for head const init closure", want: "1", src: `'use strict'; (function () { let f; for (const x = (f = () => x, 1); ;) { break } return f() })`},
	{name: "loop closure before const", want: "0,2,4", src: `'use strict'; (function () { const fs = []; for (let k = 0; k < 3; k++) { fs.push(() => x); const x = k * 2 } return fs.map(f => f()).join() })`},
	{name: "labeled block", want: "1", src: `'use strict'; (function () { let g; L: { const x = 1; g = () => x; break L } return g() })`},
	{name: "generator const", want: "9", src: `'use strict'; (function () { function* gen() { const x = yield 1; const g = () => x; yield 2; yield g() } const it = gen(); it.next(); it.next(9); return it.next().value })`},
	{name: "generator tdz", want: "4", src: `'use strict'; (function () { function* gen() { const g = () => x; yield 1; const x = 4; yield g() } const it = gen(); it.next(); return it.next().value })`},
	{name: "try const", want: "3", src: `'use strict'; (function () { let g; try { const x = 3; g = () => x; throw 0 } catch (e) {} finally {} return g() })`},
	{name: "sloppy arguments[0]", want: "5", src: `(function () { function f(x) { const g = () => x; arguments[0] = 5; return g() } return f(1) })`},
	{name: "sloppy arguments to callee", want: "9", src: `(function () { function w(a) { a[0] = 9 } function f(x) { const g = () => x; w(arguments); return g() } return f(1) })`},
	{name: "strict arguments to callee", want: "1,9", src: `'use strict'; (function () { function w(a) { a[0] = 9 } function f(x) { const g = () => x; w(arguments); return g() + ',' + arguments[0] } return f(1) })`},
	{name: "strict arguments[0]", want: "1", src: `'use strict'; (function () { function f(x) { const g = () => x; arguments[0] = 5; return g() } return f(1) })`},
	{name: "fn.arguments sloppy", want: "1", src: `(function () { function h() { try { f.arguments[0] = 7 } catch (e) {} } function f(x) { const g = () => x; h(); return g() } return f(1) })`},
	{name: "sloppy maker strict closure", want: "6", src: `(function () { function f(x) { const g = () => { 'use strict'; return x }; arguments[0] = 6; return g() } return f(1) })`},
	{name: "strict arrow arguments of sloppy", want: "8", src: `(function () { function f(x) { const g = () => { 'use strict'; return x }; const s = () => { arguments[0] = 8 }; s(); return g() } return f(1) })`},
	{name: "eval in maker strict", want: "6", src: `'use strict'; (function () { function f(x) { const g = () => x; eval('x = 6'); return g() } return f(1) })`},
	{name: "eval in arrow strict", want: "7", src: `'use strict'; (function () { function f(x) { const g = () => x; (() => eval('x = 7'))(); return g() } return f(1) })`},
	{name: "eval in nested fn strict", want: "8", src: `'use strict'; (function () { function f(x) { const g = () => x; (function () { eval('x = 8') })(); return g() } return f(1) })`},
	{name: "eval deep strict", want: "9", src: `'use strict'; (function () { function f(x) { const g = () => x; function k() { return () => eval('x = 9') } k()(); return g() } return f(1) })`},
	{name: "eval made after strict", want: "3", src: `'use strict'; (function () { function f(x) { const g = () => x; const h = () => eval('x += 1'); h(); h(); return g() } return f(1) })`},
	{name: "eval in maker sloppy", want: "6", src: `(function () { function f(x) { const g = () => x; eval('x = 6'); return g() } return f(1) })`},
	{name: "eval var sloppy", want: "6", src: `(function () { function f(x) { const g = () => x; eval('var x = 6'); return g() } return f(1) })`},
	{name: "eval indirect strict", want: "1", src: `'use strict'; (function () { function f(x) { const g = () => x; (0, eval)('var x = 99'); return g() } return f(1) })`},
	{name: "eval const assign", want: "TypeError,1", src: `(function () { const y = 1; const g = () => y; let r; try { eval('y = 2') } catch (e) { r = e.name } return r + ',' + g() })`},
	{name: "eval const assign strict", want: "TypeError,1", src: `'use strict'; (function () { const y = 1; const g = () => y; let r; try { eval('y = 2') } catch (e) { r = e.name } return r + ',' + g() })`},
	{name: "eval var shadow block const", want: "1,2", src: `(function () { { const y = 1; var g = () => y } eval('var y = 2'); return g() + ',' + y })`},
	{name: "eval in closure reads const", want: "4", src: `'use strict'; (function () { const y = 4; const g = () => eval('y'); return g() })`},
	{name: "with shadows const", want: "5", src: `(function () { const y = 1; const o = {}; let g; with (o) { g = () => y } o.y = 5; return g() })`},
	{name: "with outside const", want: "3", src: `(function () { const o = { y: 2 }; let g; with (o) { const y = 3; g = () => y } return g() })`},
	{name: "with param sloppy", want: "4", src: `(function () { function f(x) { const o = {}; let g; with (o) { g = () => x } o.x = 4; return g() } return f(1) })`},
	{name: "new Function", want: "1", src: `'use strict'; (function () { function f(x) { const g = () => x; new Function('x = 9')(); return g() } return f(1) })`},
	{name: "hoisted fn param", want: "function", src: `'use strict'; (function () { function f(p) { const g = () => p; function p() {} return typeof g() } return f(1) })`},
	{name: "var redeclared no init", want: "1", src: `'use strict'; (function () { function f(p) { const g = () => p; var p; return g() } return f(1) })`},
	{name: "var redeclared init", want: "2", src: `'use strict'; (function () { function f(p) { const g = () => p; var p = 2; return g() } return f(1) })`},
	{name: "annexB block fn param sloppy", want: "number", src: `(function () { function f(p) { const g = () => p; { function p() {} } return typeof g() } return f(1) })`},
	{name: "let per iteration", want: "1,3", src: `'use strict'; (function () { const fs = []; for (let i = 0; i < 4; i++) { fs.push(() => i); i++ } return fs.map(f => f()).join() })`},
	{name: "let per iteration update", want: "0,1,2", src: `'use strict'; (function () { const fs = []; for (let i = 0; i < 3; i++) fs.push(() => i); return fs.map(f => f()).join() })`},
	{name: "param loop closures", want: "10,11,12", src: `'use strict'; (function () { function f(p) { const fs = []; for (let i = 0; i < 3; i++) fs.push(() => p + i); return fs.map(g => g()).join() } return f(10) })`},
	{name: "??=", want: "5", src: `'use strict'; (function () { function f(x) { const g = () => x; x ??= 5; return g() } return f(null) })`},
	{name: "||=", want: "5", src: `'use strict'; (function () { function f(x) { const g = () => x; x ||= 5; return g() } return f(0) })`},
	{name: "&&=", want: "5", src: `'use strict'; (function () { function f(x) { const g = () => x; x &&= 5; return g() } return f(1) })`},
	{name: "&&= short", want: "0", src: `'use strict'; (function () { function f(x) { const g = () => x; x &&= 5; return g() } return f(0) })`},
	{name: "prefix --", want: "0", src: `'use strict'; (function () { function f(x) { const g = () => x; --x; return g() } return f(1) })`},
	{name: "postfix -- value", want: "1,0", src: `'use strict'; (function () { function f(x) { const g = () => x; const a = x--; return a + ',' + g() } return f(1) })`},
	{name: "obj destructuring assign", want: "8", src: `'use strict'; (function () { function f(x) { const g = () => x; ({ x } = { x: 8 }); return g() } return f(1) })`},
	{name: "nested destructuring assign", want: "6", src: `'use strict'; (function () { function f(x) { const g = () => x; ({ a: [x] } = { a: [6] }); return g() } return f(1) })`},
	{name: "default destructuring assign", want: "3", src: `'use strict'; (function () { function f(x) { const g = () => x; [x = 3] = []; return g() } return f(1) })`},
	{name: "rest destructuring assign", want: "1+2", src: `'use strict'; (function () { function f(x) { const g = () => x; [...x] = [1, 2]; return g().join('+') } return f(1) })`},
	{name: "for-in target", want: "k", src: `'use strict'; (function () { function f(x) { const g = () => x; for (x in { k: 1 }) {} return g() } return f(1) })`},
	{name: "for-of target member-free", want: "5", src: `'use strict'; (function () { function f(x) { const g = () => x; for ([x] of [[5]]) {} return g() } return f(1) })`},
	{name: "catch param shadow", want: "1", src: `'use strict'; (function () { function f(x) { const g = () => x; try { throw 7 } catch (x) { x = 3 } return g() } return f(1) })`},
	{name: "catch destructure shadow", want: "1", src: `'use strict'; (function () { function f(x) { const g = () => x; try { throw { x: 7 } } catch ({ x }) {} return g() } return f(1) })`},
	{name: "class static block assigns", want: "5", src: `'use strict'; (function () { function f(x) { const g = () => x; class C { static { x = 5 } } return g() } return f(1) })`},
	{name: "class field assigns", want: "6", src: `'use strict'; (function () { function f(x) { const g = () => x; class C { y = (x = 6) } new C(); return g() } return f(1) })`},
	{name: "class static field assigns", want: "6", src: `'use strict'; (function () { function f(x) { const g = () => x; class C { static y = (x = 6) } return g() } return f(1) })`},
	{name: "class method assigns", want: "7", src: `'use strict'; (function () { function f(x) { const g = () => x; class C { m() { x = 7 } } new C().m(); return g() } return f(1) })`},
	{name: "class computed key assigns", want: "8", src: `'use strict'; (function () { function f(x) { const g = () => x; class C { [x = 8]() {} } return g() } return f(1) })`},
	{name: "class ctor assigns", want: "9", src: `'use strict'; (function () { function f(x) { const g = () => x; class C { constructor() { x = 9 } } new C(); return g() } return f(1) })`},
	{name: "class getter assigns", want: "10", src: `'use strict'; (function () { function f(x) { const g = () => x; const o = { get k() { x = 10; return 0 } }; o.k; return g() } return f(1) })`},
	{name: "object method assigns", want: "3", src: `'use strict'; (function () { function f(x) { const g = () => x; const o = { m() { x = 3 } }; o.m(); return g() } return f(1) })`},
	{name: "generator assigns after resume", want: "5", src: `'use strict'; (function () { function* f(x) { const g = () => x; yield 1; x = 5; yield g() } const it = f(3); it.next(); return it.next().value })`},
	{name: "generator nested assigns", want: "6", src: `'use strict'; (function () { function* f(x) { const g = () => x; yield* (function* () { x = 6; yield 0 })(); yield g() } const it = f(3); it.next(); return it.next().value })`},
	{name: "async assigns after await", want: "5", src: `'use strict'; (function () { async function f(x) { const g = () => x; await 0; x = 5; return g() } return f(1) })`},
	{name: "async arrow param", want: "4", src: `'use strict'; (function () { const f = async (x) => { const g = () => x; await 0; return g() }; return f(4) })`},
	{name: "async for-await target", want: "3", src: `'use strict'; (function () { async function f(x) { const g = () => x; for await (x of [Promise.resolve(3)]) {} return g() } return f(1) })`},
	{name: "labeled break skip", want: "1", src: `'use strict'; (function () { function f(x) { const g = () => x; L: { break L; x = 2 } return g() } return f(1) })`},
	{name: "finally assigns", want: "9", src: `'use strict'; (function () { function f(x) { const g = () => x; try { throw 0 } catch (e) {} finally { x = 9 } return g() } return f(1) })`},
	{name: "finally after return", want: "9", src: `'use strict'; (function () { function f(x) { const g = () => x; try { return g } finally { x = 9 } } return f(1)() })`},
	{name: "switch assigns", want: "4", src: `'use strict'; (function () { function f(x) { const g = () => x; switch (x) { case 1: x = 4 } return g() } return f(1) })`},
	{name: "delete sloppy", want: "false,1", src: `(function () { function f(x) { const g = () => x; const d = delete x; return d + ',' + g() } return f(1) })`},
	{name: "closure assigns maker sees", want: "10,10", src: `'use strict'; (function () { function f(x) { const g = () => { x = 10 }; const h = () => x; g(); return x + ',' + h() } return f(1) })`},
	{name: "closure increments", want: "3", src: `'use strict'; (function () { function f(x) { const g = () => ++x; g(); g(); return x } return f(1) })`},
	{name: "recursion through binding", want: "8", src: `'use strict'; (function () { function mk(n) { let r = () => n; const f = () => r(); r = () => n * 2; return f() } return mk(4) })`},
	{name: "recursion param", want: "55", src: `'use strict'; (function () { function f(n) { const g = () => n > 0 ? f(n - 1) + n : 0; return g() } return f(10) })`},
	{name: "param default later fn", want: "8", src: `(function () { function f(g = () => b, b = 8) { return g() } return f() })`},
	{name: "param default assigned", want: "3", src: `(function () { function f(a, g = () => a) { a = 3; return g() } return f(1) })`},
	{name: "class method default params", want: "12", src: `'use strict'; (function () { class A { m(x = 1, y) { const g = () => x + y; x = 10; return g() } } return new A().m(undefined, 2) })`},
	{name: "setter param", want: "5", src: `'use strict'; (function () { const o = { set k(v) { this.g = () => v } }; o.k = 5; return o.g() })`},
	{name: "arrow param strict", want: "7", src: `'use strict'; (function () { const mk = (x) => () => x; return mk(3)() + mk(4)() })`},
	{name: "many params", want: "11", src: `'use strict'; (function () { function f(a, b, c, d, e) { const g = () => a + b + c + d + e; d = 0; return g() } return f(1, 2, 3, 4, 5) })`},
	{name: "mixed own and shared", want: "203", src: `'use strict'; (function () { function f(a, b) { let c = 1; const g = () => a + b + c; c = 100; const h = () => c; return g() + h() } return f(1, 2) })`},
	{name: "two closures same param", want: "15", src: `'use strict'; (function () { function f(a) { const g = () => a; const h = () => a * 2; return g() + h() } return f(5) })`},
	{name: "closure of closure param", want: "3", src: `'use strict'; (function () { function f(a) { return function (b) { return () => a + b } } return f(1)(2)() })`},
	{name: "grandchild assigns via middle", want: "50", src: `'use strict'; (function () { function f(a) { const g = () => a; const m = () => { const n = () => { a = 50 }; n() }; m(); return g() } return f(1) })`},
	{name: "arguments object strict param untouched", want: "4", src: `'use strict'; (function () { function f(a) { const g = () => a; const s = arguments; s[0] = 3; return g() + s[0] } return f(1) })`},
	{name: "tail restart closures", want: "5,4,3,2,1", src: `'use strict'; (function () { function c(n, fs) { if (n === 0) return fs.map(f => f()).join(); fs.push(() => n); return c(n - 1, fs) } return c(5, []) })`},
	{name: "tail restart open", want: "5,4,3,2,1", src: `'use strict'; (function () { function c(n, fs) { if (n === 0) return fs.map(f => f()).join(); fs.push(() => n++); return c(n - 1, fs) } return c(5, []) })`},
	{name: "many upvalues", want: "35", src: `'use strict'; (function () { function f(a, b, c, d, e, g1, h) { const k = 7; const z = () => a + b + c + d + e + g1 + h + k; return z() } return f(1, 2, 3, 4, 5, 6, 7) })`},
	{name: "const object identity", want: "3,true", src: `'use strict'; (function () { const o = {}; const g = () => o; o.x = 3; return g().x + ',' + (g() === o) })`},
	{name: "derived this arrow", want: "true", src: `'use strict'; (function () { class A {} class B extends A { constructor() { const f = () => this; super(); this.ok = f() === this } } return new B().ok })`},
	{name: "derived super arrow", want: "5", src: `'use strict'; (function () { class A { constructor() { this.v = 4 } } class B extends A { constructor() { const s = () => super(); s(); this.w = this.v + 1 } } return new B().w })`},
	{name: "new.target arrow", want: "true", src: `'use strict'; (function () { function F() { const g = () => new.target; this.ok = g() === F } return new F().ok })`},
	{name: "rest param", want: "1+2", src: `'use strict'; (function () { function f(...r) { const g = () => r; return g().join('+') } return f(1, 2) })`},
	{name: "pattern param defaults", want: "10", src: `'use strict'; (function () { class A { m({ a, g = () => b, b }) { b = 9; return g() + a } } return new A().m({ a: 1, b: 2 }) })`},
	{name: "strict eval assigns let", want: "7", src: `'use strict'; (function () { function f() { let x = 1; const g = () => eval('x = 2'); let s = 0; for (let i = 0; i < 3; i++) { s += x; g() } return s + x } return f() })`},
	{name: "strict eval assigns param in loop", want: "10", src: `'use strict'; (function () { function f(x) { const g = () => eval('x = x + 1'); const h = () => x; let s = 0; for (let i = 0; i < 3; i++) { s += h(); g() } return s + x } return f(1) })`},
	{name: "kinds by value", want: "length.name,length.name.prototype,constructor,length.name.prototype,,true,length.name,length.name.prototype,length.name.prototype,constructor.m,1,2,3,true,function,1,false,f,,true,7", src: `'use strict'; (function () { function mk(a, b) { return [() => a + b, function f() { return a }, function* g() { yield a }, async function h() { return a }, async function* ag() { yield a }, class C { m() { return a } }, class D extends Object { constructor() { super(); this.v = b } }, { m() { return a }, get k() { return b } }] } const [arrow, f, g, h, ag, C, D, o] = mk(1, 2); const desc = (x) => Object.getOwnPropertyNames(x).sort().join('.'); return [desc(arrow), desc(f), desc(f.prototype), desc(g), desc(g.prototype), Object.getPrototypeOf(g.prototype) === Object.getPrototypeOf(function* () {}).prototype, desc(h), desc(ag), desc(C), desc(C.prototype), new C().m(), new D().v, o.m() + o.k, f.prototype.constructor === f, typeof h().then, g().next().value, Object.getOwnPropertyDescriptor(C, 'prototype').writable, f.name, arrow.name, new f() instanceof f, mk(3, 4)[0]()].join() })`},
	{name: "arrow this super new.target by value", want: "A1true,5", src: `'use strict'; (function () { class A { m() { return 'A' } } class B extends A { m(x) { const g = () => super.m() + x + (this instanceof B); return g() } constructor(y) { super(); const h = () => new.target === B && y; this.ok = h() } } const b = new B(5); return b.m(1) + ',' + b.ok })`},
}

// captureEvaluator compiles eval's and the Function constructor's code as
// the public package does (quickjs.go's compileEval).
func captureEvaluator(source string, req EvalRequest) (*bytecode.Function, error) {
	popts := parser.Options{}
	copts := compiler.Options{Source: "<eval>", Text: source, EvalConfigurable: true, EvalOwnVarScope: true}
	if req.Direct {
		popts.Strict = req.Scope.Strict
		popts.AllowSuperProp, popts.AllowSuperCall, popts.AllowNewTarget = req.Scope.AllowSuperProp, req.Scope.AllowSuperCall, req.Scope.AllowNewTarget
		copts.EvalScope, copts.EvalWithDepth = req.Scope.Bindings, req.Scope.WithDepth
		copts.PrivateNames, copts.ArgumentNames, copts.InFieldInit = req.Scope.PrivateNames, req.Scope.ArgumentNames, req.Scope.InFieldInit
		copts.AllowSuperProp, copts.AllowSuperCall, copts.AllowNewTarget = req.Scope.AllowSuperProp, req.Scope.AllowSuperCall, req.Scope.AllowNewTarget
		copts.EvalVarScopeIsGlobal = req.Scope.VarScopeIsGlobal
	}
	prog, err := parser.Parse(source, popts)
	if err != nil {
		return nil, err
	}
	return compiler.Compile(prog, copts)
}

// runCaptureCase makes c's function in r and calls it rounds times, and
// returns the one answer every call gave, or what they gave if not one.
func runCaptureCase(t *testing.T, r *Runtime, c captureCase, rounds int) string {
	t.Helper()
	src := "globalThis.__t = (0, eval)(" + strconv.Quote(c.src) + ");" +
		"var __out = []; for (let i = 0; i < " + strconv.Itoa(rounds) + "; i++) { try { Promise.resolve(__t()).then(" +
		"v => { __out.push(String(v)) }, e => { __out.push('threw ' + e.name) }) } catch (e) { __out.push('threw ' + e.name) } }"
	if _, err := r.Run(compileForTest(t, src)); err != nil {
		t.Fatalf("%s: %v", c.name, err)
	}
	if err := r.DrainJobs(); err != nil {
		t.Fatalf("%s: %v", c.name, err)
	}
	v, err := r.Run(compileForTest(t, "__out.length === "+strconv.Itoa(rounds)+" && __out.every(x => x === __out[0]) ? __out[0] : 'calls gave ' + __out.join(' | ')"))
	if err != nil {
		t.Fatalf("%s: %v", c.name, err)
	}
	return v.String().Go()
}

// TestCaptureByValueCases runs captureCases on both tiers, three calls of
// each case's function in a runtime of its own.
func TestCaptureByValueCases(t *testing.T) {
	defer SetTreeTier(true)
	for _, c := range captureCases {
		for _, on := range []bool{false, true} {
			SetTreeTier(on)
			r := New(Config{})
			r.SetEvaluator(captureEvaluator)
			if got := runCaptureCase(t, r, c, 3); got != c.want {
				t.Errorf("%s, tree %v: got %s, want %s", c.name, on, got, c.want)
			}
			r.Close()
		}
	}
}

// TestCaptureByValueMarks pins which captures the compiler marks by value:
// in kinds of closure -- an arrow, a function, a generator, an async
// function and generator, classes and their methods, an object
// literal's method and getter -- every capture of a strict maker's
// parameters nothing assigns; none where the maker assigns one, nor in
// code compiled for a debugger.
func TestCaptureByValueMarks(t *testing.T) {
	kinds := `'use strict'; function mk(a, b) { const c = 3; return [() => a + b + c, function f() { return a }, function* g() { yield a }, async function h() { return a }, async function* ag() { yield a }, class C { m() { return a + c } }, class D extends Object { constructor() { super(); this.v = b } }, { m() { return a }, get k() { return b } }] }`
	for _, tt := range []struct {
		name, src     string
		debug         bool
		byVal, shared int
	}{
		{name: "kinds", src: kinds, byVal: -1, shared: 0},
		{name: "kinds for a debugger", src: kinds, debug: true, byVal: 0, shared: -1},
		{name: "assigned", src: `'use strict'; function mk(a) { const g = () => a, h = function () { return a }; a = 2; return [g, h] }`, byVal: 0, shared: 2},
		{name: "sloppy", src: `function mk(a) { return () => a }`, byVal: 0, shared: 1},
	} {
		prog, err := parser.Parse(tt.src, parser.Options{})
		if err != nil {
			t.Fatal(err)
		}
		fn, err := compiler.Compile(prog, compiler.Options{Source: "test", Text: tt.src, Debug: tt.debug})
		if err != nil {
			t.Fatal(err)
		}
		var byVal, shared int
		var walk func(*bytecode.Function)
		walk = func(fn *bytecode.Function) {
			n := 0
			for _, d := range fn.Upvalues {
				switch {
				case d.ByValue:
					byVal++
					n++
				case d.FromParent:
					shared++
				}
			}
			if fn.CapturesByValue != (n != 0) {
				t.Errorf("%s: %s CapturesByValue %v with %d by value", tt.name, fn.Name, fn.CapturesByValue, n)
			}
			for _, c := range fn.Constants {
				if c.Fn != nil {
					walk(c.Fn)
				}
			}
		}
		for _, c := range fn.Constants {
			if c.Fn != nil && c.Fn.Name == "mk" {
				for _, k := range c.Fn.Constants {
					if k.Fn != nil {
						walk(k.Fn)
					}
				}
			}
		}
		if tt.byVal >= 0 && byVal != tt.byVal || tt.shared >= 0 && shared != tt.shared || byVal+shared == 0 {
			t.Errorf("%s: %d captures by value and %d shared, want %d and %d", tt.name, byVal, shared, tt.byVal, tt.shared)
		}
	}
}
