package quickjs_test

import (
	"errors"
	"strings"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// TestSyntaxErrorPosition pins where a SyntaxError says source failed: the
// name it was compiled under and the line and column in it, placed as
// WithOffset places the source -- for the parser's errors and the compiler's,
// for a script, a module and eval code -- and that script handed the error
// sees it too. The parser's positions ignored WithOffset, so a host's module
// wrapper put every error a line down, and no SyntaxError named its file.
func TestSyntaxErrorPosition(t *testing.T) {
	position := func(err error) (string, int, int, string) {
		t.Helper()
		var se *quickjs.SyntaxError
		if !errors.As(err, &se) {
			t.Fatalf("not a SyntaxError: %v", err)
		}
		file, line, column := se.Position()
		return file, line, column, se.Error()
	}
	type result struct {
		file         string
		line, column int
		message      string
	}
	check := func(name string, err error, want result) {
		t.Helper()
		file, line, column, msg := position(err)
		if got := (result{file, line, column, msg}); got != want {
			t.Errorf("%s:\ngot  %+v\nwant %+v", name, got, want)
		}
	}

	_, err := quickjs.Compile("/app/a.js", "let ok;\nlet y = ;")
	check("parser", err, result{"/app/a.js", 2, 9, `quickjs: SyntaxError: unexpected punctuator ";" (/app/a.js:2:9)`})
	_, err = quickjs.Compile("/app/a.js", "let y = ;", quickjs.WithOffset(10, 4))
	check("first line, offset", err, result{"/app/a.js", 11, 13, `quickjs: SyntaxError: unexpected punctuator ";" (/app/a.js:11:13)`})
	_, err = quickjs.Compile("/app/a.js", "let ok;\nlet y = ;", quickjs.WithOffset(10, 4))
	check("second line, offset", err, result{"/app/a.js", 12, 9, `quickjs: SyntaxError: unexpected punctuator ";" (/app/a.js:12:9)`})
	_, err = quickjs.Compile("/app/a.js", "x;\n  let q; var q;", quickjs.WithOffset(10, 4))
	check("compiler, offset", err, result{"/app/a.js", 12, 14, `quickjs: SyntaxError: identifier "q" has already been declared (/app/a.js:12:14)`})
	// A host's module wrapper, a line above the file.
	_, err = quickjs.Compile("/app/m.js", "(function (exports) {\nlet y = ;\n})", quickjs.WithOffset(-1, 0))
	check("wrapper", err, result{"/app/m.js", 1, 9, `quickjs: SyntaxError: unexpected punctuator ";" (/app/m.js:1:9)`})

	rt := quickjs.New()
	defer rt.Close()
	_, err = rt.EvalFile("/app/b.js", "\n\nlet y = ;")
	check("EvalFile", err, result{"/app/b.js", 3, 9, `quickjs: SyntaxError: unexpected punctuator ";" (/app/b.js:3:9)`})
	_, err = rt.EvalModule("/app/c.mjs", "export let y = ;")
	check("module", err, result{"/app/c.mjs", 1, 16, `quickjs: SyntaxError: unexpected punctuator ";" (/app/c.mjs:1:16)`})
	_, err = rt.Eval("let y = ;")
	check("Eval", err, result{"", 1, 9, `quickjs: SyntaxError: unexpected punctuator ";" (line 1, column 9)`})
	rt.SetModuleLoader(func(specifier, referrer string) (string, string, error) {
		return "export let x = ;", "/app/lib/broken.mjs", nil
	})
	_, err = rt.EvalModule("/app/e.mjs", `import "./lib/broken.mjs"`)
	check("loaded module", err, result{"/app/lib/broken.mjs", 1, 16, `quickjs: SyntaxError: unexpected punctuator ";" (/app/lib/broken.mjs:1:16)`})

	// Handed to script, as a host's require hands it, it says the same.
	rt.Set("compile", func(name, src string) error {
		_, err := quickjs.Compile(name, src)
		return err
	})
	got, err := rt.Eval(`
		const messages = [];
		try { compile("/app/d.js", "\n  let y = ;") } catch (e) { messages.push(e.name + ": " + e.message) }
		try { eval("let y = ;") } catch (e) { messages.push(e.name + ": " + e.message) }
		import("./lib/broken.mjs").catch(e => messages.push(e.name + ": " + e.message));
		messages`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Eval(`0`); err != nil { // runs the import's jobs
		t.Fatal(err)
	}
	want := strings.Join([]string{
		`SyntaxError: unexpected punctuator ";" (/app/d.js:2:11)`,
		`SyntaxError: unexpected punctuator ";" (line 1, column 9)`,
		`SyntaxError: unexpected punctuator ";" (/app/lib/broken.mjs:1:16)`,
	}, "\n")
	var messages []string
	if err := got.Decode(&messages); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(messages, "\n"); got != want {
		t.Errorf("in script:\ngot  %s\nwant %s", got, want)
	}
}
