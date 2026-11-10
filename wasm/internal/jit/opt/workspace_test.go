//go:build amd64 || arm64

package opt

import (
	"bytes"
	"sync"
	"testing"
)

// TestWorkspaceReuse compiles a module's functions one after another and
// from goroutines at once, each compile in a workspace another left: the
// code must be what a compile in a new workspace makes, whatever the
// workspace held -- a larger function's tables, a refused function's
// state -- and compiles at once must not share one (run it with -race).
func TestWorkspaceReuse(t *testing.T) {
	m := compileWat(t, `(module
  (memory 1)
  (global $g (mut i32) (i32.const 0))
  (func $leaf (param i32) (result i32)
    (i32.add (i32.mul (local.get 0) (i32.const 3)) (global.get $g)))
  (func $loop (param $n i32) (result i32)
    (local $i i32) (local $acc i32)
    (loop $l
      (local.set $acc (i32.xor (local.get $acc) (call $leaf (local.get $i))))
      (i32.store (i32.and (i32.mul (local.get $i) (i32.const 4)) (i32.const 0xfff)) (local.get $acc))
      (br_if $l (i32.lt_u (local.tee $i (i32.add (local.get $i) (i32.const 1))) (local.get $n))))
    (local.get $acc))
  (func $dispatch (param $pc i32) (param $n i32) (result i32)
    (local $a i32) (local $b i32)
    (loop $L
      (block $exit (block $c1 (block $c0
        (br_table $c0 $c1 $exit (local.get $pc)))
        (local.set $a (i32.add (local.get $a) (local.get $n)))
        (local.set $n (i32.sub (local.get $n) (i32.const 1)))
        (local.set $pc (i32.const 0))
        (br_if $L (local.get $n))
        (local.set $pc (i32.const 1))
        (br $L))
        (local.set $b (local.get $a))
        (local.set $a (i32.const 7))
        (local.set $pc (i32.const 2))
        (br $L)))
    (i32.sub (local.get $a) (local.get $b)))
  (func $floats (param f64 f64) (result f64)
    (local $x f64)
    (local.set $x (f64.mul (local.get 0) (local.get 1)))
    (if (result f64) (f64.lt (local.get $x) (f64.const 1))
      (then (f64.sqrt (local.get $x)))
      (else (f64.div (local.get $x) (f64.add (local.get 0) (f64.const 2))))))
  (func $vec (param v128 v128) (result v128)
    (i32x4.add (f32x4.min (local.get 0) (local.get 1)) (v128.const i32x4 1 2 3 4)))
  (func $refused (result i32) (i31.get_s (ref.i31 (i32.const 1)))))`)
	n := uint32(len(m.Functions))
	// What a compile makes in a workspace of its own; the last function
	// is refused.
	want := make([][]byte, n)
	for fn := range n - 1 {
		ws := new(workspace)
		f := build(ws, m, fn)
		Optimize(f, nil)
		want[fn] = lower(f, testRequest(m, fn), ws).Bytes
	}
	check := func(fn uint32) {
		code, err := Compile(testRequest(m, fn))
		if fn == n-1 {
			if err == nil {
				t.Errorf("function %d compiled, not refused", fn)
			}
			return
		}
		if err != nil {
			t.Errorf("function %d: %v", fn, err)
			return
		}
		if !bytes.Equal(code.Bytes, want[fn]) {
			t.Errorf("function %d: the code differs from a new workspace's", fn)
		}
	}
	for range 3 {
		for fn := range n {
			check(fn)
		}
	}
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 200 {
				check(uint32(g+i) % n)
			}
		}()
	}
	wg.Wait()
}
