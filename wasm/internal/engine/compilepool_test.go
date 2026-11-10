//go:build quickjs_jit && !android && !ios && ((amd64 && (linux || windows)) || (arm64 && (darwin || linux)))

package engine

import (
	"bytes"
	"context"
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

// TestFirstCallsAtOnce makes the first calls of one Code's functions from
// several goroutines at once, each with an instance of its own, the JIT
// compiling each function at its first call: each function is compiled
// once and every call runs its code, and only the function the JIT refuses
// has its body compiled for the interpreter, once.
func TestFirstCallsAtOnce(t *testing.T) {
	if !JITAvailable() {
		t.Skip("no native code in this build")
	}
	b, err := wat.Compile([]byte(`(module
		(type $v (struct (field v128)))
		(func $leaf (param i32) (result i32) (i32.mul (local.get 0) (i32.const 3)))
		(func $loop (param i32) (result i32) (local i32)
			(block $out (loop $l
				(br_if $out (i32.eqz (local.get 0)))
				(local.set 1 (i32.add (local.get 1) (call $leaf (local.get 0))))
				(local.set 0 (i32.sub (local.get 0) (i32.const 1)))
				(br $l)))
			(i32.add (local.get 1) (call $refused (local.get 1))))
		;; A read of a v128 field, which the JIT refuses.
		(func $refused (param i32) (result i32)
			(if (i32.const 0) (then (drop (struct.get $v 0 (ref.null $v)))))
			(i32.and (local.get 0) (i32.const 7)))
		(func (export "run") (param i32) (result i32) (call $loop (local.get 0))))`))
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
	for round := range 20 {
		code := NewCode(m)
		if !code.EnableJIT(0) {
			t.Fatal("EnableJIT refused where JITAvailable")
		}
		// Whatever WASM_JIT says: no first calls in the interpreter, and
		// the baseline alone.
		code.setOSRCalls(0)
		code.SetOptThreshold(-1)
		before := ReadJITStats()
		const n = 1000
		want := uint64(3*n*(n+1)/2 + (3*n*(n+1)/2)&7)
		start := make(chan struct{})
		errs := make([]error, 8)
		var wg sync.WaitGroup
		for g := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				inst, err := Instantiate(context.Background(), code, nil)
				if err != nil {
					errs[g] = err
					return
				}
				ext, _ := inst.Export("run")
				<-start
				res, err := Invoke(context.Background(), ext.Func, []Val{{N: n}})
				if err != nil || res[0].N != want {
					errs[g] = fmt.Errorf("run(%d) = %v, %v; want %d", n, res, err, want)
				}
			}()
		}
		close(start)
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				t.Fatalf("round %d: %v", round, err)
			}
		}
		after := ReadJITStats()
		refused := after.Refused["simd"] - before.Refused["simd"]
		if c, i := after.Compiled-before.Compiled, after.Interpreted-before.Interpreted; c != 3 || refused != 1 || i != 1 {
			t.Fatalf("round %d: %d functions compiled, %d refused, %d bodies built for the interpreter; want 3, 1 and 1", round, c, refused, i)
		}
		for fn := range m.Functions {
			cd := code.get(uint32(fn))
			if native, built := cd.native.Load() != nil, cd.built; native == (fn == 2) || built != (fn == 2) {
				t.Fatalf("round %d: function %d: native code %v, interpreter's body %v", round, fn, native, built)
			}
		}
	}
}
