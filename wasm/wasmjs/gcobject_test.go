//go:build quickjs_wasm

package wasmjs_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/wasm/wat"
)

// TestGCObjectsGeneric pins what the language's generic operations do with
// wasm's structs and arrays, which are opaque: JSON leaves them out as it
// leaves out a function, they have no keys to spread or assign, they cannot
// be cloned, and they refuse every change -- as in Node (testdata/node/gc.js
// compares them there).
func TestGCObjectsGeneric(t *testing.T) {
	bin, err := wat.Compile([]byte(`(module
		(type $s (struct (field (mut i32))))
		(type $a (array (mut i32)))
		(func (export "struct") (result (ref $s)) (struct.new $s (i32.const 42)))
		(func (export "array") (result (ref $a)) (array.new $a (i32.const 5) (i32.const 3))))`))
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("const gcBytes = new Uint8Array([")
	for i, c := range bin {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprint(&b, c)
	}
	b.WriteString(`]);
const e = new WebAssembly.Instance(new WebAssembly.Module(gcBytes)).exports;
const s = e.struct(), a = e.array();
p("JSON top", () => JSON.stringify(s));
p("JSON array", () => JSON.stringify([s, a]));
p("JSON object", () => JSON.stringify({ x: s, y: a, z: 1 }));
p("JSON replacer", () => JSON.stringify({ x: s }, (k, v) => (k === "x" ? typeof v : v)));
p("JSON property list", () => JSON.stringify(s, ["a"]));
p("JSON indented", () => JSON.stringify([s, a], null, 1));
p("JSON reviver", () => JSON.stringify(JSON.parse('{"a":1,"b":2}', (k, v) => (k === "a" ? s : v))));
p("spread", () => Object.keys({ ...s }).length);
p("Object.assign", () => Object.keys(Object.assign({}, s, a)).length);
p("structuredClone", () => structuredClone(s));
p("structuredClone nested", () => structuredClone({ list: [a] }));
p("set", () => { "use strict"; s.x = 1; });
p("typeof", () => [typeof s, Object.prototype.toString.call(a)]);
`)
	path := filepath.Join(t.TempDir(), "gc.js")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	got := runQuickJS(t, filepath.Join("testdata", "node_prelude.js"), path)
	want := `JSON top -> undefined
JSON array -> "[null,null]"
JSON object -> "{\"z\":1}"
JSON replacer -> "{\"x\":\"object\"}"
JSON property list -> undefined
JSON indented -> "[\n null,\n null\n]"
JSON reviver -> "{\"b\":2}"
spread -> 0
Object.assign -> 0
structuredClone -> DOMException: [object Object] could not be cloned.
structuredClone nested -> DOMException: [object Object] could not be cloned.
set -> TypeError: WebAssembly objects are opaque
typeof -> ["object", "[object Object]"]
`
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}
