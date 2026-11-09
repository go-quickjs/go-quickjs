package wasmjs_test

import (
	"os/exec"
	"strings"
	"testing"
)

// deps lists the packages pkg imports, directly or not, built with tags.
func deps(t *testing.T, pkg, tags string) []string {
	t.Helper()
	args := []string{"list", "-deps"}
	if tags != "" {
		args = append(args, "-tags", tags)
	}
	out, err := exec.Command("go", append(args, pkg)...).Output()
	if err != nil {
		t.Fatalf("go list %s: %v", pkg, err)
	}
	return strings.Fields(string(out))
}

const wasmEngine = "github.com/go-quickjs/go-quickjs/wasm/internal/engine"

// TestEnginesApart pins that the JavaScript engine does not import the wasm
// engine, nor the standard library the WebAssembly namespace: a host opts
// in by installing it.
func TestEnginesApart(t *testing.T) {
	for _, pkg := range []string{"github.com/go-quickjs/go-quickjs", "github.com/go-quickjs/go-quickjs/stdlib"} {
		for _, dep := range deps(t, pkg, "quickjs_wasm") {
			if strings.HasPrefix(dep, "github.com/go-quickjs/go-quickjs/wasm") {
				t.Errorf("%s imports %s", pkg, dep)
			}
		}
	}
}

// TestWasmBuildTag pins that the namespace is built only with the
// quickjs_wasm tag: without it, neither this package nor qjs links the wasm
// engine; with it, qjs does.
func TestWasmBuildTag(t *testing.T) {
	has := func(pkgs []string, want string) bool {
		for _, p := range pkgs {
			if p == want {
				return true
			}
		}
		return false
	}
	for _, pkg := range []string{"github.com/go-quickjs/go-quickjs/wasm/wasmjs", "github.com/go-quickjs/go-quickjs/cmd/qjs"} {
		if has(deps(t, pkg, ""), wasmEngine) {
			t.Errorf("%s links the wasm engine without the quickjs_wasm tag", pkg)
		}
	}
	if !has(deps(t, "github.com/go-quickjs/go-quickjs/cmd/qjs", "quickjs_wasm"), wasmEngine) {
		t.Error("qjs does not link the wasm engine with the quickjs_wasm tag")
	}
}
