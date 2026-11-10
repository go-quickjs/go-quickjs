//go:build quickjs_jit && !android && !ios && ((amd64 && (linux || windows)) || (arm64 && (darwin || linux)))

package engine

import (
	"fmt"
	"testing"
)

// TestOptGlobalSetSpilledAddress pins a global.set of a float constant to a
// global whose address was spilled: the constant went through the register
// the address had been loaded into, and the store went to the constant's
// bits as an address, a crash in native code.
func TestOptGlobalSetSpilledAddress(t *testing.T) {
	src := `(module
  (global $g (mut f64) (f64.const 0))
  (global $h (mut i64) (i64.const 0))
  (func (export "run") (param $n i64) (result f64)
    (local $a i64) (local $b i64) (local $c i64) (local $d i64) (local $e i64) (local $f i64) (local $k i64) (local $i i64)
    (global.set $h (local.get $n))
    (loop $l
      (local.set $a (i64.add (local.get $a) (i64.mul (local.get $i) (i64.const 3))))
      (local.set $b (i64.xor (local.get $b) (local.get $a)))
      (local.set $c (i64.add (local.get $c) (local.get $b)))
      (local.set $d (i64.mul (i64.or (local.get $d) (i64.const 1)) (local.get $c)))
      (local.set $e (i64.add (local.get $e) (local.get $d)))
      (local.set $f (i64.xor (local.get $f) (local.get $e)))
      (local.set $k (i64.add (local.get $k) (local.get $f)))
      (br_if $l (i64.lt_u (local.tee $i (i64.add (local.get $i) (i64.const 1))) (local.get $n))))
    (global.set $g (f64.const 1.5))
    (global.set $h (i64.add (local.get $a) (i64.add (local.get $b) (i64.add (local.get $c) (i64.add (local.get $d) (i64.add (local.get $e) (i64.add (local.get $f) (local.get $k))))))))
    (global.get $g)))`
	interp := interpretedInstance(t, src)
	inst := tierInstance(t, src, 0)
	want, werr := call(t, interp, "run", 100)
	got, gerr := call(t, inst, "run", 100)
	if fmt.Sprint(werr) != fmt.Sprint(gerr) || got[0].N != want[0].N {
		t.Fatalf("got %v %v want %v %v", got, gerr, want, werr)
	}
}
