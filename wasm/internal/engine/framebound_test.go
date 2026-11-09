//go:build quickjs_jit && !android && !ios && ((amd64 && (linux || windows)) || (arm64 && (darwin || linux)))

package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-quickjs/go-quickjs/wasm/internal/decoder"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/wat"
)

// checkFrameBounds checks that validation's bound of each defined function's
// frame (frameBound), by which native code calling it sizes its frame,
// holds the frame compileFunction makes, and counts the functions whose
// bound is that frame exactly.
func checkFrameBounds(t *testing.T, name string, m *module.Module) (funcs, exact int) {
	t.Helper()
	c := &Code{Module: m, fns: make([]atomic.Pointer[compiled], len(m.Functions))}
	for fn, f := range m.Functions {
		if f.Imported {
			continue
		}
		cd := compileFunction(c, uint32(fn))
		n, r, ok := c.frameBound(uint32(fn))
		if !ok {
			t.Fatalf("%s: function %d has no bound", name, fn)
		}
		if cd.nslots > n || cd.nativeRefSlots() > r {
			t.Errorf("%s: function %d: frame of %d slots and %d reference slots, bound %d and %d", name, fn, cd.nslots, cd.nativeRefSlots(), n, r)
		}
		funcs++
		if cd.nslots == n && cd.nativeRefSlots() == r {
			exact++
		}
	}
	return funcs, exact
}

// TestFrameBound checks validation's bounds of frames against the frames
// compileFunction makes: for cases where the bound is above the frame --
// code after a branch that only validation reads -- and where it is the
// frame; and, with WASM_SPEC_DIR or WASM_COMPILE_COST, every function of
// the spec's modules and of those files, reporting how many bounds are
// above their frames.
func TestFrameBound(t *testing.T) {
	cases := []struct {
		src   string
		exact bool
	}{
		{`(func (param i32 i64) (result i32) (local f64 v128 externref)
			(i32.add (local.get 0) (i32.wrap_i64 (i64.mul (local.get 1) (i64.const 3)))))`, true},
		{`(func (result i32) unreachable i32.const 1 i32.const 2 i32.add)`, true},
		{`(func (param i32) (result i32)
			(block (result i32) (br_if 0 (i32.const 1) (local.get 0)) drop (i32.const 2)))`, true},
		{`(func (result i32) (return (i32.const 0))
			(block (result i32 i32 i32) i32.const 1 i32.const 2 i32.const 3) drop drop)`, false},
		{`(func (result i32) (return (i32.const 0)) (block (result externref) ref.null extern) drop i32.const 1)`, false},
		{`(func (result externref) ref.null extern)`, true},
		{`(tag $e (param i32))
		  (func (result i32)
			(try (result i32) (do (try (result i32) (do (i32.const 1)) (catch $e))) (catch_all (i32.const 2))))`, true},
		{`(func (result v128) (v128.const i64x2 1 2))`, true},
	}
	for i, tc := range cases {
		b, err := wat.Compile([]byte("(module " + tc.src + ")"))
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		m, derr := decoder.Decode(b, decoder.CoreLimits())
		if derr == nil {
			derr = decoder.ValidateFunctions(m, decoder.CoreLimits())
		}
		if derr != nil {
			t.Fatalf("case %d: %v", i, derr)
		}
		funcs, exact := checkFrameBounds(t, fmt.Sprint("case ", i), m)
		if got := exact == funcs; got != tc.exact {
			t.Errorf("case %d: bound exact %v, want %v", i, got, tc.exact)
		}
	}

	var inputs []specModule
	if os.Getenv("WASM_SPEC_DIR") != "" {
		inputs = specModules(t)
	}
	for _, p := range strings.Split(os.Getenv("WASM_COMPILE_COST"), ",") {
		if p = strings.TrimSpace(p); p != "" && p != "spec" {
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			inputs = append(inputs, specModule{filepath.Base(p), b})
		}
	}
	if len(inputs) == 0 {
		return
	}
	funcs, exact := 0, 0
	for _, in := range inputs {
		m, derr := decoder.Decode(in.bin, decoder.CoreLimits())
		if derr == nil {
			derr = decoder.ValidateFunctions(m, decoder.CoreLimits())
		}
		if derr != nil {
			t.Fatalf("%s: %v", in.name, derr)
		}
		f, e := checkFrameBounds(t, in.name, m)
		funcs += f
		exact += e
	}
	t.Logf("%d modules, %d functions: bound exact for %d, above the frame for %d", len(inputs), funcs, exact, funcs-exact)
}
