//go:build quickjs_wasm

package wasmjs_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/stdlib"
	"github.com/go-quickjs/go-quickjs/wasm/wasmjs"
)

// builderPath is the WebAssembly specification's module builder, which the
// comparison scripts build their modules with.
func builderPath(t *testing.T) string {
	dir := os.Getenv("WASM_SPEC_DIR")
	if dir == "" {
		dir = filepath.Join("..", "..", "..", "wasm-spec")
	}
	p := filepath.Join(dir, "test", "js-api", "wasm-module-builder.js")
	if _, err := os.Stat(p); err != nil {
		t.Skipf("no module builder (set WASM_SPEC_DIR): %v", err)
	}
	return p
}

// runQuickJS runs the files in a runtime as qjs makes one, with Node's
// quirks, the standard library and the WebAssembly namespace, and returns
// what the script recorded.
func runQuickJS(t *testing.T, files ...string) string {
	t.Helper()
	rt := quickjs.New(quickjs.WithNodeQuirks())
	defer rt.Close()
	loop := stdlib.NewLoop(rt)
	if err := stdlib.Install(rt, stdlib.Config{Loop: loop}); err != nil {
		t.Fatal(err)
	}
	if err := wasmjs.Install(rt); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := rt.EvalFile(filepath.Base(f), string(src)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
	if _, err := rt.Eval(`chain.then(() => { globalThis.finished = true })`); err != nil {
		t.Fatal(err)
	}
	if err := loop.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	v, err := rt.Eval(`globalThis.finished ? out.join("\n") + "\n" : "(the script did not finish)\n"`)
	if err != nil {
		t.Fatal(err)
	}
	return v.String()
}

// TestCompareNode runs each script of testdata/node in Node and in
// go-quickjs and compares what they record: each operation's result, or the
// constructor and message of what it threw, line by line. It needs a Node
// with WebAssembly's JS Promise Integration (Node 24 or later) on PATH.
func TestCompareNode(t *testing.T) {
	if os.Getenv("QUICKJS_COMPARE_NODE_WASM") != "1" {
		t.Skip("set QUICKJS_COMPARE_NODE_WASM=1 to compare with Node")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node on PATH")
	}
	builder := builderPath(t)
	prelude := filepath.Join("testdata", "node_prelude.js")
	scripts, err := filepath.Glob(filepath.Join("testdata", "node", "*.js"))
	if err != nil {
		t.Fatal(err)
	}
	for _, script := range scripts {
		t.Run(filepath.Base(script), func(t *testing.T) {
			cmd := exec.Command(node, filepath.Join("testdata", "node_runner.js"), builder, prelude, script)
			want, err := cmd.Output()
			if err != nil {
				t.Fatalf("node: %v", err)
			}
			got := runQuickJS(t, builder, prelude, script)
			compareLines(t, filepath.Base(script), string(want), got)
		})
	}
}

// nodeKnownDifferences are the lines, by script and label, where go-quickjs
// is known to differ from Node, and why. None is the WebAssembly namespace's
// own: each is a message of the runtime's, or of the wasm engine's.
var nodeKnownDifferences = map[string]string{
	"runtime.js: new exported":              "the runtime names the callee by its name, not by the expression",
	"runtime.js: exported toString":         "the runtime writes a name that is not an identifier as a computed key",
	"runtime.js: deep recursion through JS": "the runtime's own stack overflow message",
	"api.js: subclass Tag":                  "super() gives what a built-in constructor makes the subclass's prototype, which V8 does not for a Tag",
	"api.js: subclass Exception":            "super() gives what a built-in constructor makes the subclass's prototype, which V8 does not for an Exception",
	"api.js: subclass Suspending":           "super() gives what a built-in constructor makes the subclass's prototype, which V8 does not for a Suspending",
	"gc.js: method":                         "the runtime names the callee of a call through a proxy undefined",
	"gc.js: JSON":                           "JSON.stringify writes a GC object as an empty object, where V8 writes null",
}

// v8Conversion matches the messages of V8's ToPrimitive and ToBigInt, which
// the runtime words its own way.
var v8Conversion = regexp.MustCompile(`^(TypeError|SyntaxError): (Cannot convert .* to a BigInt|Cannot convert object to primitive value)$`)

// runtimeConversion reports whether two lines differ only in the message of
// a conversion the runtime made: the same label, the same error type.
func runtimeConversion(node, ours string) bool {
	nl, nm, _ := strings.Cut(node, " -> ")
	ol, om, _ := strings.Cut(ours, " -> ")
	nt, _, _ := strings.Cut(nm, ":")
	ot, _, _ := strings.Cut(om, ":")
	return nl == ol && nt == ot && v8Conversion.MatchString(nm)
}

// compareLines reports the lines that differ, but for the known
// differences.
func compareLines(t *testing.T, script, want, got string) {
	t.Helper()
	w := strings.Split(strings.TrimRight(want, "\n"), "\n")
	g := strings.Split(strings.TrimRight(got, "\n"), "\n")
	diffs, known := 0, 0
	for i := 0; i < max(len(w), len(g)); i++ {
		var wl, gl string
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if wl == gl {
			continue
		}
		label, _, _ := strings.Cut(wl, " -> ")
		why := nodeKnownDifferences[script+": "+label]
		if why == "" && runtimeConversion(wl, gl) {
			why = "the runtime's own conversion message"
		}
		if why != "" {
			known++
			t.Logf("line %d differs, as known (%s):\n node:       %s\n go-quickjs: %s", i+1, why, wl, gl)
			continue
		}
		diffs++
		t.Errorf("line %d:\n node:       %s\n go-quickjs: %s", i+1, wl, gl)
	}
	t.Logf("%d lines, %d differ, %d as known", len(w), diffs, known)
}
