package quickjs_test

import (
	"strings"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// TestImportAttributes pins JSON, text and bytes modules: what each exports,
// that a module of a type is a different module from the same source without
// one, and that the attributes are checked.
func TestImportAttributes(t *testing.T) {
	files := map[string]string{
		"data.json": `{"a": [1, 2]}`,
		"note.txt":  "hello",
		"bad.json":  `{"a":`,
	}
	newRuntime := func() *quickjs.Runtime {
		rt := quickjs.New()
		rt.SetModuleLoader(func(specifier, referrer string) (string, string, error) {
			return files[strings.TrimPrefix(specifier, "./")], specifier, nil
		})
		return rt
	}

	rt := newRuntime()
	defer rt.Close()
	ns, err := rt.EvalModule("main.js", `
		import data from "./data.json" with { type: "json" };
		import * as again from "./data.json" with { "type": "json" };
		import note from "./note.txt" with { type: "text" };
		import bytes from "./note.txt" with { type: "bytes" };
		export const out = [
			data.a.join("+"), again.default === data, Object.keys(again).join(),
			note, bytes.length, bytes.buffer.immutable, bytes[0],
		].join("|");
	`)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := ns.Get("out")
	if got, want := out.String(), "1+2|true|default|hello|5|true|104"; got != want {
		t.Errorf("out = %q, want %q", got, want)
	}

	for src, want := range map[string]string{
		`import x from "./data.json" with { type: "json", type: "json" }`: "SyntaxError",
		`import x from "./data.json" with { kind: "json" }`:               "SyntaxError",
		`import x from "./data.json" with { type: json }`:                 "SyntaxError",
		`import x from "./bad.json" with { type: "json" }`:                "SyntaxError",
		`import x from "./data.json" with { type: "css" }`:                "TypeError",
	} {
		rt := newRuntime()
		_, err := rt.EvalModule("main.js", src)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want a %s", src, err, want)
		}
		rt.Close()
	}
}

func TestDynamicImportAttributes(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	rt.SetModuleLoader(func(specifier, referrer string) (string, string, error) {
		return `{"n": 7}`, specifier, nil
	})
	ns, err := rt.EvalModule("main.js", `
		const results = [];
		const ns = await import("./x.json", { with: { type: "json" } });
		results.push(ns.default.n);
		for (const options of [null, { with: 1 }, { with: { type: 1 } }, { with: { other: "x" } }]) {
			try { await import("./x.json", options) } catch (e) { results.push(e.constructor.name) }
		}
		export const out = results.join();
	`)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := ns.Get("out")
	if got, want := out.String(), "7,TypeError,TypeError,TypeError,SyntaxError"; got != want {
		t.Errorf("out = %q, want %q", got, want)
	}
}
