//go:build quickjs_wasm

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/wasm/wat"
)

// TestImportsWasmModules pins qjs's import of .wasm files: a module's
// exports as bindings, its imports resolved as module imports, its source
// phase, and the errors naming the file -- by its path where it does not
// compile, by its URL where a name it uses is reserved.
func TestImportsWasmModules(t *testing.T) {
	dir := realPath(t.TempDir())
	write := func(name, src string) {
		b := []byte(src)
		if strings.HasSuffix(name, ".wat") {
			var err error
			if b, err = wat.Compile(b); err != nil {
				t.Fatal(err)
			}
			name = strings.TrimSuffix(name, ".wat") + ".wasm"
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("m.wat", `(module
		(import "./js.mjs" "base" (global $base i32))
		(func (export "add") (param i32) (result i32) global.get $base local.get 0 i32.add)
		(global (export "g") i32 (i32.const 7)))`)
	write("js.mjs", `export const base = 40;`)
	write("bad.wasm", "not wasm")
	write("reserved.wat", `(module (import "wasm-js:x" "f" (func)))`)
	write("main.mjs", `
		import { add, g } from "./m.wasm";
		import source m from "./m.wasm";
		console.log(add(2), g, m instanceof WebAssembly.Module, WebAssembly.Module.imports(m)[0].module);
		for (const f of ["./bad.wasm", "./reserved.wasm"]) {
			try { await import(f) } catch (e) { console.log(e.constructor.name + ": " + e.message) }
		}
	`)
	code, out, errOut := exec(t, "", filepath.Join(dir, "main.mjs"))
	want := "42 7 true ./js.mjs\n" +
		"CompileError: " + filepath.Join(dir, "bad.wasm") + ": WebAssembly.Module(): expected magic word 00 61 73 6d, found 6e 6f 74 20 @+0\n" +
		`LinkError: Invalid Wasm import "wasm-js:x" in ` + fileURL(filepath.Join(dir, "reserved.wasm")) + "\n"
	if code != 0 || out != want || errOut != "" {
		t.Errorf("code=%d err=%q\nout:\n%s\nwant:\n%s", code, errOut, out, want)
	}
}
