package quickjs_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs"
)

// TestCaptureByValueEval pins that a direct eval's assignment of a strict
// function's parameter is seen by a closure over it: the eval, in the
// function or in a closure of it, keeps the closure sharing the binding
// rather than taking its value (bytecode.UpvalueDesc's ByValue).
func TestCaptureByValueEval(t *testing.T) {
	checkEval(t, `'use strict'; function f(x) { const g = () => x; eval("x = 6"); return g() } f(1)`, "6")
	checkEval(t, `'use strict'; function f(x) { const g = () => x; (() => eval("x = 7"))(); return g() } f(1)`, "7")
	checkEval(t, `'use strict'; function f(x) { const g = () => x; (function () { eval("x = 8") })(); return g() } f(1)`, "8")
}

// TestCaptureByValueTopLevel pins what closures see of a script's and a
// module's top-level bindings, which no closure takes by value where they
// can change: a script's let assigned after, read by an arrow; a const
// in its dead zone when a hoisted function reads it; a module's exports,
// live to its own closures and to an importer's, a let assigned by an
// exported function after both were made; and a module's const that a
// hoisted function's closure, made by an importer before the const is
// initialized, reads after. Each as Node gives it, with the JIT too.
func TestCaptureByValueTopLevel(t *testing.T) {
	modules := map[string]string{
		"a.js": `import { h, early } from './b.js'
			export function mk() { return () => K }
			export const K = 3
			export let v = 1
			export function setV(n) { v = n }
			export const read = () => v + K
			export const first = h() + ',' + early + ',' + read()`,
		"b.js": `import { mk, v } from './a.js'
			export const h = mk()
			let early
			try { early = h() } catch (e) { early = e.name }
			export { early }
			export function seeV() { return v }`,
	}
	for _, jit := range []bool{false, true} {
		var opts []quickjs.Option
		if jit {
			opts = append(opts, quickjs.WithJIT())
		}
		rt := quickjs.New(opts...)
		rt.SetModuleLoader(func(spec, referrer string) (string, string, error) {
			name := strings.TrimPrefix(spec, "./")
			if src, ok := modules[name]; ok {
				return src, name, nil
			}
			return "", "", fmt.Errorf("no module %q", spec)
		})
		if _, err := rt.Eval(`'use strict'
			function g() { return y }
			var r; try { r = g() } catch (e) { r = e.name }
			const y = 7
			const k = () => y + z
			let z = 1
			z = 2
			var hh = () => tl
			let tl = 1
			tl = 5`); err != nil {
			t.Fatal(err)
		}
		if got := evalString(t, rt, `tl = 6; [r, g(), k(), hh()].join()`); got != "ReferenceError,7,9,6" {
			t.Errorf("jit %v: script: %s, want ReferenceError,7,9,6", jit, got)
		}
		if _, err := rt.EvalModule("c.js", `import { setV, read, v, first } from './a.js'
			import { seeV } from './b.js'
			const g = () => v
			const before = read()
			setV(20)
			globalThis.out = [first, before, read(), v, g(), seeV()].join()`); err != nil {
			t.Fatal(err)
		}
		if got := evalString(t, rt, `out`); got != "3,ReferenceError,4,4,23,20,20,20" {
			t.Errorf("jit %v: modules: %s, want 3,ReferenceError,4,4,23,20,20,20", jit, got)
		}
		rt.Close()
	}
}
