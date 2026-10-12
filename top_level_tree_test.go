package quickjs_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/internal/vm"
)

// TestTopLevelTrees pins that the top level of a script, of eval code and of
// a module, run as a tree where it has a loop, does what the interpreter
// does: its declarations of vars, functions, lets and consts, the errors
// they throw for a name already declared, a property that cannot be
// shadowed or a global that cannot be extended, its completion value, eval
// code's vars going to the function that called eval, and a module's
// bindings, imports and top-level await.
func TestTopLevelTrees(t *testing.T) {
	defer vm.SetTreeTier(true)
	// Each script runs in turn in one runtime. One marked must build a tree,
	// which only its top level, or the eval code it runs, has a loop to
	// build. (Eval code a sloppy function runs directly finds its names
	// with with_resolve, as a with statement's body does, which the tier
	// does not build.)
	scripts := []struct {
		src   string
		alone bool
	}{
		{`var s = 0; for (var i = 0; i < 10; i++) s += i; s`, true},
		{`let t = 0; const k = 3; for (let i = 0; i < 4; i++) t += k * i; t`, true},
		{`for (var j = 0; j < 3; j++) j * 2`, true},
		{`[s, i, t, k, j, typeof globalThis.t, Object.getOwnPropertyDescriptor(globalThis, 's').configurable].join()`, false},
		{`function h(x) { return x + 1 } class C { m() { return 2 } } var a = 0; for (let i = 0; i < 3; i++) a += h(i) + new C().m(); a + ' ' + typeof globalThis.C`, false},
		{`var r; try { for (let i = 0; i < 1; i++) tdz } catch (e) { r = e.name } let tdz = 1; r`, true},
		{`var z1 = 0; for (;;) break; let t = 1`, true},
		{`let s; for (;;) break`, true},
		{`Object.defineProperty(globalThis, 'nc', { value: 1 }); for (;;) break; nc`, true},
		{`for (;;) break; let nc = 2`, true},
		{`function NaN() {} for (;;) break`, false},
		{`for (;;) break; function nc() {}`, false},
		{`var e0 = 0; for (let i = 0; i < 2; i++) e0 += (0, eval)("var ie = 0; for (var w = 0; w < 3; w++) ie += w; ie"); e0 + ' ' + ie + ' ' + w`, false},
		{`function f() { eval("var q = 0; for (var i = 0; i < 5; i++) q += i; function g() { return q + i }"); return g() + ' ' + q + ' ' + typeof globalThis.q } f()`, false},
		{`function f2() { "use strict"; return eval("var e1 = 1; for (let i = 0; i < 2; i++) e1++; e1") + ' ' + typeof e1 } f2()`, false},
		{`function f3() { var log = []; eval("for (var n = 0; n < 2; n++) log.push(n); function d() { return n }"); return log + ' ' + d() + ' ' + typeof globalThis.d } f3()`, false},
		{`var b = 0; for (var i of [1, 2, 3]) b += i; for (var p in { x: 1, y: 2 }) b += p; b`, true},
		{`(0, eval)("var ie2 = 0; for (var w2 = 0; w2 < 3; w2++) ie2 += w2; ie2")`, true},
		{`eval("var q2 = 0; for (var i2 = 0; i2 < 3; i2++) q2 += i2; q2") + ' ' + typeof globalThis.q2 + ' ' + Object.getOwnPropertyDescriptor(globalThis, 'q2').configurable`, true},
		{`yy = 1; function rd() { let s = 0; for (let i = 0; i < 3; i++) s += yy; return s } var r1 = 0; for (let i = 0; i < 3; i++) r1 += rd(); r1`, false},
		{`let yy = 10; var r2 = 0; for (let i = 0; i < 3; i++) r2 += rd(); r2 + ' ' + globalThis.yy`, true},
		{`Object.preventExtensions(globalThis); var later; for (;;) break`, true},
		{`for (;;) break; function later2() {}`, false},
		{`typeof later + ' ' + typeof later2`, false},
	}
	run := func(tree bool) (string, []int64) {
		vm.SetTreeTier(tree)
		rt := quickjs.New()
		defer rt.Close()
		var out []string
		var built []int64
		for _, s := range scripts {
			before := vm.TreesBuilt()
			v, err := rt.Eval(s.src)
			built = append(built, vm.TreesBuilt()-before)
			if err != nil {
				out = append(out, "E "+err.Error())
				continue
			}
			out = append(out, v.String())
		}
		return strings.Join(out, "\n"), built
	}
	want, _ := run(false)
	got, built := run(true)
	if got != want {
		t.Errorf("tree:\n%s\ninterpreter:\n%s", got, want)
	}
	for i, s := range scripts {
		if s.alone && built[i] < 1 {
			t.Errorf("%q built no tree", s.src)
		}
	}
	const answer = `45
18
4
45,10,18,3,3,undefined,false
12 undefined
ReferenceError
E SyntaxError: "t" has already been declared
E SyntaxError: "s" is already a property of the global object that cannot be removed
1
E SyntaxError: "nc" is already a property of the global object that cannot be removed
E TypeError: cannot redeclare "NaN" as a function
E TypeError: cannot redeclare "nc" as a function
6 3 3
15 10 undefined
3 undefined
0,1 2 undefined
6xy
3
3 number true
9
90 1
undefined
E TypeError: cannot declare "later2" on a non-extensible global
undefined undefined`
	if got != answer {
		t.Errorf("got:\n%s\nwant:\n%s", got, answer)
	}
}

// TestTopLevelModuleTrees is TestTopLevelTrees for modules: a module's
// lets, consts, functions and classes, the bindings it imports and exports,
// a loop that awaits at its top level, and a cycle that reads a binding
// before its module has initialized it.
func TestTopLevelModuleTrees(t *testing.T) {
	defer vm.SetTreeTier(true)
	modules := map[string]string{
		"/app/a.mjs": `export let n = 0; for (let i = 0; i < 4; i++) n += i; export function bump() { n++ }`,
		"/app/b.mjs": `import { c } from "./c.mjs"; export let fromC; for (let i = 0; i < 1; i++) { try { fromC = c } catch (e) { fromC = e.name } } export const late = 1;`,
		"/app/c.mjs": `import { late } from "./b.mjs"; export let c = 7; export let sawLate; for (let i = 0; i < 1; i++) { try { sawLate = late } catch (e) { sawLate = e.name } }`,
	}
	run := func(tree bool) (string, int64) {
		vm.SetTreeTier(tree)
		rt := quickjs.New()
		defer rt.Close()
		rt.SetModuleLoader(func(specifier, referrer string) (string, string, error) {
			name := "/app/" + strings.TrimPrefix(specifier, "./")
			src, ok := modules[name]
			if !ok {
				return "", "", fmt.Errorf("no module %s", name)
			}
			return src, name, nil
		})
		before := vm.TreesBuilt()
		ns, err := rt.EvalModule("/app/main.mjs", `
			import { n, bump } from "./a.mjs";
			import { fromC } from "./b.mjs";
			import { sawLate } from "./c.mjs";
			let seen = [n];
			for (let i = 0; i < 2; i++) { bump(); seen.push(n) }
			class K { static v = 5 }
			const k = K.v;
			let awaited = 0;
			for (let i = 0; i < 3; i++) awaited += await Promise.resolve(i);
			function local() { return k }
			export let all = [seen, fromC, sawLate, k, awaited, local(), typeof import.meta].join(" ");
			export default all;`)
		if err != nil {
			t.Fatalf("tree %v: %v", tree, err)
		}
		if p, err := ns.Get("all"); err == nil {
			if _, err := p.Await(context.Background()); err != nil {
				t.Fatalf("tree %v: %v", tree, err)
			}
		}
		all, err := ns.Get("all")
		if err != nil {
			t.Fatalf("tree %v: %v", tree, err)
		}
		def, _ := ns.Get("default")
		return all.String() + " | " + def.String(), vm.TreesBuilt() - before
	}
	want, _ := run(false)
	got, built := run(true)
	if got != want {
		t.Errorf("tree: %s\ninterpreter: %s", got, want)
	}
	// main, a, b and c each have a loop at the top, besides bump and local.
	if built < 6 {
		t.Errorf("%d trees built, want the four modules' and two functions'", built)
	}
	const answer = "6,7,8 7 ReferenceError 5 3 5 object | 6,7,8 7 ReferenceError 5 3 5 object"
	if got != answer {
		t.Errorf("got %s, want %s", got, answer)
	}
}
