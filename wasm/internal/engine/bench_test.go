package engine

import (
	"context"
	"testing"

	"github.com/go-quickjs/go-quickjs/wasm/internal/decoder"
	"github.com/go-quickjs/go-quickjs/wasm/wat"
)

// benchInstance instantiates a module given in the text format.
func benchInstance(tb testing.TB, src string) *Instance {
	b, err := wat.Compile([]byte(src))
	if err != nil {
		tb.Fatal(err)
	}
	m, derr := decoder.Decode(b, decoder.CoreLimits())
	if derr != nil {
		tb.Fatal(derr)
	}
	if derr := decoder.ValidateFunctions(m, decoder.CoreLimits()); derr != nil {
		tb.Fatal(derr)
	}
	inst, err := Instantiate(context.Background(), NewCode(m), nil)
	if err != nil {
		tb.Fatal(err)
	}
	return inst
}

const benchWAT = `(module
  (func $fib (export "fib") (param i32) (result i32)
    (if (result i32) (i32.lt_u (local.get 0) (i32.const 2))
      (then (local.get 0))
      (else (i32.add (call $fib (i32.sub (local.get 0) (i32.const 1)))
                     (call $fib (i32.sub (local.get 0) (i32.const 2)))))))
  (func (export "loop") (param i32) (result i32) (local i32 i32)
    (block $done
      (loop $l
        (br_if $done (i32.ge_u (local.get 1) (local.get 0)))
        (local.set 2 (i32.add (local.get 2) (i32.mul (local.get 1) (i32.const 3))))
        (local.set 1 (i32.add (local.get 1) (i32.const 1)))
        (br $l)))
    (local.get 2))
)`

func BenchmarkFib25(b *testing.B) {
	inst := benchInstance(b, benchWAT)
	fn, _ := inst.Export("fib")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		res, err := Invoke(context.Background(), fn.Func, []Val{{N: 25}})
		if err != nil || res[0].N != 75025 {
			b.Fatal(err, res)
		}
	}
}

func BenchmarkLoop1M(b *testing.B) {
	inst := benchInstance(b, benchWAT)
	fn, _ := inst.Export("loop")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := Invoke(context.Background(), fn.Func, []Val{{N: 1_000_000}}); err != nil {
			b.Fatal(err)
		}
	}
}

const benchEntryWAT = `(module
  (type $t (func (param i32) (result i32)))
  (table 1 funcref)
  (elem (i32.const 0) $id)
  (func $id (export "id") (param i32) (result i32) (local.get 0))
  (func (export "ind") (param i32) (result i32) (local i32)
    (loop $l
      (local.set 1 (i32.add (local.get 1)
        (call_indirect (type $t) (local.get 0) (i32.const 0))))
      (br_if $l (local.tee 0 (i32.sub (local.get 0) (i32.const 1)))))
    (local.get 1))
)`

// entryInstance instantiates benchEntryWAT with its functions compiled by
// the baseline tier at their first call, or skips without native code.
func entryInstance(b *testing.B) *Instance {
	inst := thresholdInstance(b, benchEntryWAT, 0)
	inst.Code.SetOptThreshold(-1)
	return inst
}

// BenchmarkInvokeIdentity times an entry into native code from Go and its
// return: an Invoke of a function returning its argument.
func BenchmarkInvokeIdentity(b *testing.B) {
	inst := entryInstance(b)
	fn, _ := inst.Export("id")
	ctx := context.Background()
	args := []Val{{N: 7}}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if res, err := Invoke(ctx, fn.Func, args); err != nil || res[0].N != 7 {
			b.Fatal(err, res)
		}
	}
}

// BenchmarkIndirectThroughGo times 1000 call_indirects the baseline tier
// makes through Go, each an exit, an entry into the callee's native code
// and a resumption.
func BenchmarkIndirectThroughGo(b *testing.B) {
	inst := entryInstance(b)
	fn, _ := inst.Export("ind")
	ctx := context.Background()
	args := []Val{{N: 1000}}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if res, err := Invoke(ctx, fn.Func, args); err != nil || res[0].N != 500500 {
			b.Fatal(err, res)
		}
	}
}
