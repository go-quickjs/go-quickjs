//go:build quickjs_jit && !android && !ios && ((amd64 && (linux || windows)) || (arm64 && (darwin || linux)))

package engine

import (
	"bytes"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-quickjs/go-quickjs/wasm/internal/decoder"
	"github.com/go-quickjs/go-quickjs/wasm/wat"
)

// TestConcurrentCompiles compiles the functions of one module for several
// Codes at once, for the interpreter and natively, and checks that each
// goroutine's code is what one compile alone makes: the compilers' kept
// workspaces are never shared between compiles (run with -race).
func TestConcurrentCompiles(t *testing.T) {
	if !JITAvailable() {
		t.Skip("no native code in this build")
	}
	b, err := wat.Compile([]byte(`(module (memory 1) (global $g (mut i64) (i64.const 0))
		(func $f (param i32 i64) (result i64) (local i64 f64)
			(block $out (loop $l
				(br_if $out (i32.eqz (local.get 0)))
				(local.set 2 (i64.add (local.get 2) (i64.load (i32.and (local.get 0) (i32.const 1016)))))
				(i64.store (i32.const 8) (local.get 2))
				(local.set 0 (i32.sub (local.get 0) (i32.const 1)))
				(br $l)))
			(global.set $g (local.get 2))
			(i64.add (local.get 2) (call $h (local.get 1))))
		(func $h (param i64) (result i64)
			(block (block (block (br_table 0 1 2 (i32.wrap_i64 (local.get 0))))
				(return (i64.const 1))) (return (i64.const 2)))
			(i64.const 3))
		(func (export "run") (param i32) (result i64) (call $f (local.get 0) (i64.const 3))))`))
	if err != nil {
		t.Fatal(err)
	}
	m, derr := decoder.Decode(b, decoder.CoreLimits())
	if derr == nil {
		derr = decoder.ValidateFunctions(m, decoder.CoreLimits())
	}
	if derr != nil {
		t.Fatal(derr)
	}
	compileAll := func() [][]byte {
		c := &Code{Module: m, fns: make([]atomic.Pointer[compiled], len(m.Functions))}
		for fn := range m.Functions {
			c.fns[fn].Store(compileFunction(c, uint32(fn)))
		}
		var hashes [][]byte
		for fn := range m.Functions {
			code, err := compileNative(c, uint32(fn), c.get(uint32(fn)), 0)
			if err != nil {
				t.Error(err)
				return nil
			}
			hashes = append(hashes, codeHash(code))
		}
		return hashes
	}
	want := compileAll()
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for g := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				got := compileAll()
				for fn := range want {
					if fn >= len(got) || !bytes.Equal(got[fn], want[fn]) {
						errs[g] = fmt.Errorf("function %d compiled to other code", fn)
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
}
