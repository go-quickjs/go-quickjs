package quickjs_test

import (
	"fmt"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// TestModuleBlockFunctions pins a function declared in a block of a module's
// body: it is the block's binding, made when the block is entered, as in a
// script or a function -- not skipped as the module's own top-level functions
// are, which linking made already (issue #4).
func TestModuleBlockFunctions(t *testing.T) {
	for _, c := range []struct{ name, src, want string }{
		{"try block", `try { function f() { return 7; } result = f(); } catch (e) { result = String(e); }`, "7"},
		{"shadowing", `function f() { return 99; }
			try { function f() { return 7; } result = f(); } catch (e) { result = String(e); }
			result += "," + f();`, "7,99"},
		{"before its declaration", `{ result = f(); function f() { return 7; } }`, "7"},
		{"bare block", `{ function f() { return 7; } result = f(); } result += "," + typeof f;`, "7,undefined"},
		{"closure", `let g; { function f() { return 7; } g = () => f(); } result = g();`, "7"},
		{"generator and async", `{ function* g() { yield 7; } async function a() { return 8; } result = g().next().value + "," + (a() instanceof Promise); }`, "7,true"},
		{"switch", `switch (1) { case 1: result = f(); break; case 2: function f() { return 7; } }`, "7"},
		{"loop body", `result = ""; for (let i = 0; i < 2; i++) { function f() { return i; } result += f(); }`, "01"},
		{"nested blocks", `function f() { return 1; } { function f() { return 2; } { function f() { return 3; } result = f(); } result += "," + f(); } result += "," + f();`, "3,2,1"},
		{"exported top-level still hoisted", `result = f(); export function f() { return 7; }`, "7"},
	} {
		rt := quickjs.New()
		if _, err := rt.Eval(`globalThis.result = undefined`); err != nil {
			t.Fatal(err)
		}
		if _, err := rt.EvalModule("/entry.mjs", c.src); err != nil {
			t.Errorf("%s: %v", c.name, err)
		} else if v, err := rt.Get("result"); err != nil {
			t.Errorf("%s: %v", c.name, err)
		} else if got := v.String(); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
		rt.Close()
	}
}

// TestReExportColonSpecifier pins a named re-export from a module whose
// specifier, or whose export's name, holds a colon: the module asked for is the
// one named, whole (issue #5).
func TestReExportColonSpecifier(t *testing.T) {
	for _, c := range []struct{ name, barrel, entry, want string }{
		{"no colon", `export { f } from "demo-dep";`, `import { f } from "barrel"; result = f();`, "7"},
		{"colon", `export { f } from "demo:dep";`, `import { f } from "barrel"; result = f();`, "7"},
		{"renamed", `export { f as g } from "demo:dep";`, `import { g } from "barrel"; result = g();`, "7"},
		{"colon in the name", `export { "a:b" as h } from "demo:dep";`, `import { h } from "barrel"; result = h;`, "ab"},
		{"colons in both", `export { "a:b" } from "1:2:3";`, `import { "a:b" as x } from "barrel"; result = x;`, "123"},
		{"digits and colon", `export { f } from "12:x";`, `import { f } from "barrel"; result = f();`, "12"},
		{"through a namespace", `export { f } from "demo:dep";`, `import * as ns from "barrel"; result = ns.f() + "," + Object.keys(ns);`, "7,f"},
		{"two steps", `export { f } from "demo:mid";`, `import { f } from "barrel"; result = f();`, "7"},
		{"local workaround", `import { f } from "demo:dep"; export { f };`, `import { f } from "barrel"; result = f();`, "7"},
	} {
		rt := quickjs.New()
		if _, err := rt.Eval(`globalThis.result = undefined`); err != nil {
			t.Fatal(err)
		}
		rt.SetModuleLoader(func(specifier, referrer string) (string, string, error) {
			switch specifier {
			case "demo-dep", "demo:dep":
				return `export function f() { return 7; } const ab = "ab"; export { ab as "a:b" };`, specifier, nil
			case "demo:mid":
				return `export { f } from "demo:dep";`, specifier, nil
			case "1:2:3":
				return `const v = "123"; export { v as "a:b" };`, specifier, nil
			case "12:x":
				return `export function f() { return 12; }`, specifier, nil
			case "barrel":
				return c.barrel, specifier, nil
			}
			return "", "", fmt.Errorf("unexpected module specifier %q", specifier)
		})
		if _, err := rt.EvalModule("/entry.mjs", c.entry); err != nil {
			t.Errorf("%s: %v", c.name, err)
		} else if v, err := rt.Get("result"); err != nil {
			t.Errorf("%s: %v", c.name, err)
		} else if got := v.String(); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
		rt.Close()
	}
}

// TestModuleGlobalAccess pins a module's reads and writes of a global name it
// does not bind itself: they are the global object's, which getters and
// setters see as this, and an assignment changes the global rather than
// creating a binding of the module's own that hides it.
func TestModuleGlobalAccess(t *testing.T) {
	for _, c := range []struct{ name, src, want string }{
		{"assignment", `v = 5;`, "5"},
		{"compound assignment", `v += 4;`, "5"},
		{"in a function", `function set() { v = 5; } set();`, "5"},
		{"in a loop", `for (let i = 0; i < 100; i++) v = i;`, "99"},
		{"accessor this", `g; typeof g; g = 2; v = log.join();`, "true,true,true"},
		{"undeclared", `try { w = 1; v = "set"; } catch (e) { v = e.name + "," + ("w" in globalThis); }`, "ReferenceError,false"},
		{"own binding shadows", `let v2 = 3; v2 = 4; v = v2 + "," + globalThis.v2;`, "4,undefined"},
		{"import is read-only", `import { x } from "/dep.mjs"; try { x = 2; } catch (e) { v = e.name + "," + x; }`, "TypeError,1"},
	} {
		rt := quickjs.New()
		rt.SetModuleLoader(func(specifier, referrer string) (string, string, error) {
			return `export let x = 1;`, specifier, nil
		})
		if _, err := rt.Eval(`var v = 1, log = [];
			Object.defineProperty(globalThis, "g", {
				get() { log.push(this === globalThis); return 1; },
				set(_) { log.push(this === globalThis); },
			});`); err != nil {
			t.Fatal(err)
		}
		if _, err := rt.EvalModule("/entry.mjs", c.src); err != nil {
			t.Errorf("%s: %v", c.name, err)
		} else if v, err := rt.Eval(`String(v)`); err != nil {
			t.Errorf("%s: %v", c.name, err)
		} else if got := v.String(); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
		rt.Close()
	}
}
