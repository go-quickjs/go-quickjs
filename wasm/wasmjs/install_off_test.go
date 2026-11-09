//go:build !quickjs_wasm

package wasmjs_test

import (
	"errors"
	"testing"

	"github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/wasm/wasmjs"
)

// TestInstallNotBuilt pins that a build without the quickjs_wasm tag has no
// WebAssembly: Install returns ErrNotBuilt and defines nothing.
func TestInstallNotBuilt(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	if err := wasmjs.Install(rt); !errors.Is(err, wasmjs.ErrNotBuilt) {
		t.Fatalf("Install: got %v, want ErrNotBuilt", err)
	}
	v, err := rt.Eval(`typeof WebAssembly`)
	if err != nil || v.String() != "undefined" {
		t.Fatalf("typeof WebAssembly = %v, %v; want undefined", v, err)
	}
}
