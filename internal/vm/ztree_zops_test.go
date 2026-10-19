package vm

import (
	"sync/atomic"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// TestTreeOps pins that template literals, destructuring, spreads, rest
// parameters, the arguments object, computed and accessor members, private
// members, super, classes, the in and delete operators, typeof of a global,
// ** and unary + give in the tree tier what they give in the interpreter,
// errors and the order things run in included, and that the functions
// named in trees run as trees.
func TestTreeOps(t *testing.T) {
	defer SetTreeTier(true)
	for _, tt := range []struct {
		name, src, want string
		trees           []string
	}{
		{name: "template", src: "function f(a, b) { return `x${a}y${b}z${{ toString() { return 'T' } }}` } f(1, 'q')", want: "x1yqzT", trees: []string{"f"}},
		{name: "templateOrder", src: "var s = ''; function f() { try { return `${{ toString() { s += 'a'; return 'A' } }}${{ toString() { s += 'b'; throw 'B' } }}` } catch (e) { return e + s } } f()", want: "Bab", trees: []string{"f"}},
		{name: "templateSymbol", src: "function f() { try { return `${Symbol('q')}` } catch (e) { return e.name } } f()", want: "TypeError", trees: []string{"f"}},
		{name: "tagged", src: "function tag(st, ...v) { return st.raw.join('|') + v.join() } function f(x) { return tag`a${x}b\\n` } f(1)", want: "a|b\\n1", trees: []string{"f", "tag"}},
		{name: "taggedSite", src: "function g(s) { return s } function f() { return g`x` } f() === f()", want: "true", trees: []string{"f"}},
		{name: "indexThis", src: `function f(o, k) { return o[k](1) } f({ m(x) { return this.v + x }, v: 2 }, 'm')`, want: "3", trees: []string{"f"}},
		{name: "indexThisNull", src: `function f(o, k) { try { return o[k]() } catch (e) { return e.name } } f(null, 'x')`, want: "TypeError", trees: []string{"f"}},
		{name: "destructure", src: `function f(o) { const { a, b: { c } = { c: 5 }, ...rest } = o; return a + c + JSON.stringify(rest) } f({ a: 1, x: 2, y: 3 })`, want: "6{\"x\":2,\"y\":3}", trees: []string{"f"}},
		{name: "destructureNull", src: `function f(o) { try { const { a } = o } catch (e) { return e.name + ': ' + e.message } } f(null)`, want: "TypeError: cannot destructure null", trees: []string{"f"}},
		{name: "destructureParams", src: `function f({ a = 1, b } = {}, [c, ...d] = [9, 8, 7]) { return a + ',' + b + ',' + c + ',' + d.length } f() + ';' + f({ b: 2 }, [1])`, want: "1,undefined,9,2;1,2,1,0", trees: []string{"f"}},
		{name: "swap", src: `function f(x, y) { [x, y] = [y, x]; ({ x, y } = { x: x * 10, y }); return x + ',' + y } f(1, 2)`, want: "20,1", trees: []string{"f"}},
		{name: "destructureGetterOrder", src: `var s = ''; function f() { const { a, b } = { get a() { s += 'a'; return 1 }, get b() { s += 'b'; return 2 } }; return s + a + b } f()`, want: "ab12", trees: []string{"f"}},
		{name: "restParam", src: `function f(a, ...r) { return a + ':' + r.length + ':' + r.join() } f(1, 2, 3) + ';' + f()`, want: "1:2:2,3;undefined:0:", trees: []string{"f"}},
		{name: "arguments", src: `function f() { return arguments.length + ':' + arguments[1] } f(1, 2)`, want: "2:2", trees: []string{"f"}},
		{name: "lazyArguments", src: `function f() { var a = arguments; return a.length + arguments[0] } f(4, 5)`, want: "6", trees: []string{"f"}},
		{name: "applyArguments", src: `function g() { return this.v + arguments.length } function f() { return g.apply({ v: 1 }, arguments) } f(1, 2, 3)`, want: "4", trees: []string{"f", "g"}},
		{name: "computedKeys", src: `function f(k, o) { const x = { [k]: 1, ...o, get [k + 'g']() { return 2 }, set [k + 's'](v) { this.w = v } }; x.as = 7; return JSON.stringify(x) + x.ag } f('a', { b: 3 })`, want: "{\"a\":1,\"b\":3,\"ag\":2,\"w\":7}2", trees: []string{"f"}},
		{name: "computedKeyThrows", src: `var s = ''; function f() { try { return { [{ toString() { s += 'k'; throw 'K' } }]: (s += 'v') } } catch (e) { return e + s } } f()`, want: "Kk", trees: []string{"f"}},
		{name: "spreads", src: `function f(a) { return [...a, 0].join() + ',' + Math.max(...a) + ',' + new Array(...[3]).length + ',' + [...'hi'].join('') } f([3, 1, 2])`, want: "3,1,2,0,3,3,hi", trees: []string{"f"}},
		{name: "spreadMethod", src: `function f(o, a) { return o.m(...a) } f({ v: 10, m(x, y) { return this.v + x + y } }, [1, 2])`, want: "13", trees: []string{"f"}},
		{name: "spreadNotIterable", src: `function f(a) { try { return [...a] } catch (e) { return e.name } } f(1)`, want: "TypeError", trees: []string{"f"}},
		{name: "objectSpreadGetter", src: `function f() { var n = 0; const o = { ...{ get x() { return ++n } } }; return o.x + n } f()`, want: "2", trees: []string{"f"}},
		{name: "private", src: `class C { #x = 1; static #s = 5; static has(o) { return #x in o } get(o) { return o.#x } inc() { this.#x++; return this.#x } static s() { return C.#s } } function f() { const c = new C(); var r = C.has(c) + ',' + C.has({}) + ',' + c.inc() + ',' + C.s(); try { c.get({}) } catch (e) { r += ',' + e.name } try { C.has(1) } catch (e) { r += ',' + e.name } return r } f()`, want: "true,false,2,5,TypeError,TypeError", trees: []string{"f", "has", "get", "inc", "s"}},
		{name: "privateMethods", src: `class C { #v = 2; #m() { return this.#v * 3 } get #g() { return this.#v + 1 } set #g(x) { this.#v = x } run() { this.#g = 10; return this.#m() + this.#g } } function f() { return new C().run() } f()`, want: "41", trees: []string{"f", "run", "#m", "get #g", "set #g"}},
		{name: "privateTwice", src: `class B { constructor(o) { return o } } class C extends B { #x = 1; constructor(o) { super(o) } } function f() { var o = {}; new C(o); try { new C(o) } catch (e) { return e.name } } f()`, want: "TypeError", trees: []string{"f"}},
		{name: "super", src: `class A { m() { return 'A' } static s() { return 'S' } get g() { return 'G' } } class B extends A { constructor(x) { super(); this.x = x } mb() { return super.m() + 'B' + super['m']() + super.g } static sb() { return super.s() + 'T' } set v(x) { super.w = x; super['z'] = x + 1 } } function f() { const b = new B(1); b.v = 5; return b.mb() + B.sb() + b.x + b.w + b.z } f()`, want: "ABAGST156", trees: []string{"f", "B", "mb", "sb", "set v"}},
		{name: "superSpread", src: `class A { constructor(...a) { this.n = a.length } } class D extends A { constructor(...a) { super(...a) } } function f() { return new D(1, 2, 3).n } f()`, want: "3", trees: []string{"f", "D"}},
		{name: "superTwice", src: `class A {} class D extends A { constructor() { super(); try { super() } catch (e) { this.e = e.name } } } function f() { return new D().e } f()`, want: "ReferenceError", trees: []string{"f", "D"}},
		{name: "thisBeforeSuper", src: `class A {} class D extends A { constructor() { try { this.x = 1 } catch (e) { super(); this.e = e.name } } } function f() { return new D().e } f()`, want: "ReferenceError", trees: []string{"f", "D"}},
		{name: "in", src: `function f(o) { var r = ('a' in o) + ',' + (0 in [1]) + ',' + ('b' in o); try { 'x' in 1 } catch (e) { r += ',' + e.name } return r } f({ a: 1 })`, want: "true,true,false,TypeError", trees: []string{"f"}},
		{name: "delete", src: `function f(o) { return delete o.a + ',' + ('a' in o) + ',' + delete o[0] + ',' + delete 'x'.length } f({ a: 1 })`, want: "true,false,true,false", trees: []string{"f"}},
		{name: "deleteStrict", src: `'use strict'; function f() { try { delete Object.prototype } catch (e) { return e.name } } f()`, want: "TypeError", trees: []string{"f"}},
		{name: "deleteNull", src: `function f(o) { try { delete o.x } catch (e) { return e.name } } f(undefined)`, want: "TypeError", trees: []string{"f"}},
		{name: "deleteVar", src: `function f() { globalThis.gv = 1; var r = delete gv; return r + ',' + typeof gv } f()`, want: "true,undefined", trees: []string{"f"}},
		{name: "typeofGlobal", src: `function f() { return typeof notDeclaredAnywhere + ',' + typeof f + ',' + typeof Math } f()`, want: "undefined,function,object", trees: []string{"f"}},
		{name: "typeofDeadZone", src: `function f() { try { return typeof laterLet } catch (e) { return e.name } } var r = f(); let laterLet = 1; r`, want: "ReferenceError", trees: []string{"f"}},
		{name: "pow", src: `function f(a, b) { return (a ** b) + ',' + (2n ** 3n) + ',' + ((-8) ** (1 / 3)) + ',' + (2 ** -1) } f(3, 4)`, want: "81,8,NaN,0.5", trees: []string{"f"}},
		{name: "powBigIntMix", src: `function f() { try { return 2n ** 2 } catch (e) { return e.name } } f()`, want: "TypeError", trees: []string{"f"}},
		{name: "pos", src: `function f(x) { return +x + +'3' + ',' + (+{ valueOf() { return 4 } }) } f('2')`, want: "5,4", trees: []string{"f"}},
		{name: "posBigInt", src: `function f() { try { return +1n } catch (e) { return e.name } } f()`, want: "TypeError", trees: []string{"f"}},
		{name: "callee", src: `var g = function fact(n) { return n <= 1 ? 1 : n * fact(n - 1) }; function f() { return g(5) } f()`, want: "120", trees: []string{"f", "fact"}},
		{name: "classes", src: `function f() { class A { x = 1; static y = 2; m() { return this.x } get g() { return 3 } static n() { return A.y } } const o = { __proto__: { p: 4 }, get q() { return 5 } }; const h = function () {}; return new A().m() + A.n() + new A().g + o.p + o.q + h.name } f()`, want: "15h", trees: []string{"f"}},
		{name: "classStaticName", src: `function f() { class A { static name() { return 'N' } } return A.name() } f()`, want: "N", trees: []string{"f"}},
		{name: "classExtendsNull", src: `function f() { try { class A extends 5 {} } catch (e) { return e.name } } f()`, want: "TypeError", trees: []string{"f"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, on := range []bool{false, true} {
				SetTreeTier(on)
				fn := compileForTest(t, tt.src)
				r := New(Config{})
				v, err := r.Run(fn)
				if err != nil {
					r.Close()
					t.Fatalf("tree %v: %v", on, err)
				}
				got, err := r.ToString(v)
				if err != nil || got.Go() != tt.want {
					t.Errorf("tree %v: got %v, %v; want %s", on, got, err, tt.want)
				}
				if r.frameDepth != 0 || r.stackTop != 0 {
					t.Errorf("tree %v: left %d frames and %d stack slots", on, r.frameDepth, r.stackTop)
				}
				r.Close()
				if on {
					for _, name := range tt.trees {
						assertTreeIn(t, fn, name)
					}
				}
			}
		})
	}
}

// assertTreeIn fails t unless the function name, somewhere in fn, ran as a
// tree.
func assertTreeIn(t *testing.T, fn *bytecode.Function, name string) {
	t.Helper()
	var find func(fn *bytecode.Function) *bytecode.Function
	find = func(fn *bytecode.Function) *bytecode.Function {
		for _, k := range fn.Constants {
			if k.Fn == nil {
				continue
			}
			if k.Fn.Name == name {
				return k.Fn
			}
			if g := find(k.Fn); g != nil {
				return g
			}
		}
		return nil
	}
	g := find(fn)
	if g == nil {
		t.Fatalf("no function %s", name)
	}
	tr := (*tree)(atomic.LoadPointer(&g.VMCode))
	if tr == nil || tr == noTree {
		_, why := TreeReport(g)
		t.Fatalf("%s did not run as a tree: %s", name, why)
	}
}

// TestTreeHTMLDDA pins that the tree tier's tests of null and undefined
// take document.all as the interpreter does (Annex B's [[IsHTMLDDA]]): ==
// and != against null or undefined, from either side, find it equal to
// both, which the tier had taken for no object -- a function with a for
// (let ...) loop, built as a tree since its dead zone is, counted none --
// while ?. and ?? find it neither. Each function runs as a tree.
func TestTreeHTMLDDA(t *testing.T) {
	defer SetTreeTier(true)
	for _, tt := range []struct{ name, src, want string }{
		{"eqNull", `function f(x) { let n = 0; for (let i = 0; i < 3; i++) if (x == null) n++; return n } f(dda) + ',' + f(null) + ',' + f(0)`, "3,3,0"},
		{"neUndefined", `function f(x) { var n = 0; for (var i = 0; i < 3; i++) if (x != undefined) n++; return n } f(dda) + ',' + f(undefined) + ',' + f({})`, "0,0,3"},
		{"nullEq", `function f(x) { let n = 0; for (let i = 0; i < 3; i++) n += (null == x ? 1 : 0) + (undefined != x ? 10 : 0); return n } f(dda) + ',' + f(1)`, "3,30"},
		{"optional", `dda.k = 5; function f(x) { let n = 0; for (let i = 0; i < 3; i++) n += x?.k === undefined ? 1 : 10; return n } f(dda) + ',' + f(null)`, "30,3"},
		{"coalesce", `function f(x) { let s = ''; for (let i = 0; i < 2; i++) s += typeof (x ?? 1); return s } f(dda) + ',' + f(undefined)`, "undefinedundefined,numbernumber"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, on := range []bool{false, true} {
				SetTreeTier(on)
				fn := compileForTest(t, tt.src)
				r := New(Config{})
				dda := r.NewObject()
				dda.MarkHTMLDDA()
				r.global.setOwnRaw(r.atoms.intern("dda"), Obj(dda), propDefault)
				v, err := r.Run(fn)
				if err != nil {
					r.Close()
					t.Fatalf("tree %v: %v", on, err)
				}
				got, err := r.ToString(v)
				if err != nil || got.Go() != tt.want {
					t.Errorf("tree %v: got %v, %v; want %s", on, got, err, tt.want)
				}
				r.Close()
				if on {
					assertExceptionTree(t, fn, "f")
				}
			}
		})
	}
}
