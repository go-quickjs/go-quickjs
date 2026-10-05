package quickjs_test

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// TestLoaderErrors pins what a module loader's error makes the import throw:
// an error the loader threw, with a code of node's, a RangeError, or what
// script the loader ran threw, is thrown as it is -- to script by import(),
// and to Go by EvalModule -- and any other error is a TypeError naming the
// module, through which the loader's error is found. Every error was that
// TypeError, its message all that was left of a thrown one.
func TestLoaderErrors(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	thrower, err := rt.Eval(`() => { throw Object.assign(new Error("from script"), { code: "E_SCRIPT" }) }`)
	if err != nil {
		t.Fatal(err)
	}
	rt.SetModuleLoader(func(specifier, referrer string) (string, string, error) {
		switch specifier {
		case "coded":
			e := rt.NewError("Error", "Cannot find module 'coded'")
			e.Set("code", "MODULE_NOT_FOUND")
			return "", "", rt.Throw(e)
		case "range":
			return "", "", rt.ThrowRangeError("too far: %d", 3)
		case "script":
			_, err := thrower.Call()
			return "", "", err
		case "wrapped":
			return "", "", fmt.Errorf("reading %s: %w", specifier, fs.ErrNotExist)
		}
		return "export const ok = 1", specifier, nil
	})

	got, err := rt.EvalModule("/main.mjs", `
		const describe = e => [e.constructor.name, e.code, e.message].join(" | ");
		export const all = await Promise.all(["coded", "range", "script", "wrapped"].map(
			s => import(s).then(() => "loaded " + s, describe)));
		export const typed = await import("coded", { with: { type: "json" } }).catch(describe);
	`)
	if err != nil {
		t.Fatal(err)
	}
	all, _ := got.Get("all")
	typed, _ := got.Get("typed")
	var lines []string
	if err := all.Decode(&lines); err != nil {
		t.Fatal(err)
	}
	lines = append(lines, typed.String())
	want := []string{
		"Error | MODULE_NOT_FOUND | Cannot find module 'coded'",
		"RangeError |  | too far: 3",
		"Error | E_SCRIPT | from script",
		`TypeError |  | cannot resolve "wrapped": reading wrapped: file does not exist`,
		"Error | MODULE_NOT_FOUND | Cannot find module 'coded'",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("import():\ngot  %s\nwant %s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}

	// A static import, seen from Go.
	_, err = rt.EvalModule("/static.mjs", `import "coded"`)
	var jsErr *quickjs.Error
	if !errors.As(err, &jsErr) {
		t.Fatalf("static import: %v", err)
	}
	if code, _ := jsErr.Value().Get("code"); code.String() != "MODULE_NOT_FOUND" {
		t.Errorf("static import: code %v in %v", code, err)
	}
	_, err = rt.EvalModule("/static2.mjs", `import "wrapped"`)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("static import: %v does not wrap fs.ErrNotExist", err)
	}
}
