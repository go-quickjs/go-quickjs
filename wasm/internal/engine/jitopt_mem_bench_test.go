//go:build quickjs_jit && !android && !ios && ((amd64 && (linux || windows)) || (arm64 && (darwin || linux)))

package engine

import (
	"fmt"
	"testing"
)

// memoriesBenchWAT has loops over a memory other than 0, an i64 memory and
// a shared one -- a load, a store and a load at offset 4 every 8 bytes --
// and of atomic read-modify-writes of the shared one.
const memoriesBenchWAT = `(module
  (memory $m0 1)
  (memory $m1 1)
  (memory $m2 i64 1)
  (memory $m3 1 1 shared)
  (func (export "m1") (param $n i32) (result i32)
    (local $i i32) (local $acc i32) (local $k i32)
    (loop $outer
      (local.set $i (i32.const 0))
      (loop $l
        (i32.store $m1 (local.get $i) (i32.add (i32.load $m1 (local.get $i)) (local.get $i)))
        (local.set $acc (i32.add (local.get $acc) (i32.load $m1 offset=4 (local.get $i))))
        (br_if $l (i32.lt_u (local.tee $i (i32.add (local.get $i) (i32.const 8))) (i32.const 65528))))
      (br_if $outer (i32.lt_u (local.tee $k (i32.add (local.get $k) (i32.const 1))) (local.get $n))))
    (local.get $acc))
  (func (export "m64") (param $n i32) (result i32)
    (local $i i64) (local $acc i32) (local $k i32)
    (loop $outer
      (local.set $i (i64.const 0))
      (loop $l
        (i32.store $m2 (local.get $i) (i32.add (i32.load $m2 (local.get $i)) (i32.wrap_i64 (local.get $i))))
        (local.set $acc (i32.add (local.get $acc) (i32.load $m2 offset=4 (local.get $i))))
        (br_if $l (i64.lt_u (local.tee $i (i64.add (local.get $i) (i64.const 8))) (i64.const 65528))))
      (br_if $outer (i32.lt_u (local.tee $k (i32.add (local.get $k) (i32.const 1))) (local.get $n))))
    (local.get $acc))
  (func (export "shared") (param $n i32) (result i32)
    (local $i i32) (local $acc i32) (local $k i32)
    (loop $outer
      (local.set $i (i32.const 0))
      (loop $l
        (i32.store $m3 (local.get $i) (i32.add (i32.load $m3 (local.get $i)) (local.get $i)))
        (local.set $acc (i32.add (local.get $acc) (i32.load $m3 offset=4 (local.get $i))))
        (br_if $l (i32.lt_u (local.tee $i (i32.add (local.get $i) (i32.const 8))) (i32.const 65528))))
      (br_if $outer (i32.lt_u (local.tee $k (i32.add (local.get $k) (i32.const 1))) (local.get $n))))
    (local.get $acc))
  (func (export "atomic") (param $n i32) (result i32)
    (local $i i32) (local $acc i32) (local $k i32)
    (loop $outer
      (local.set $i (i32.const 0))
      (loop $l
        (local.set $acc (i32.add (local.get $acc) (i32.atomic.rmw.add $m3 (local.get $i) (local.get $k))))
        (drop (i32.atomic.rmw.cmpxchg $m3 offset=4 (local.get $i) (local.get $acc) (local.get $i)))
        (br_if $l (i32.lt_u (local.tee $i (i32.add (local.get $i) (i32.const 8))) (i32.const 65528))))
      (br_if $outer (i32.lt_u (local.tee $k (i32.add (local.get $k) (i32.const 1))) (local.get $n))))
    (local.get $acc)))`

// BenchmarkOptMemories times ten passes of memoriesBenchWAT's loops over
// 64 KiB in the baseline code and in the optimizing tier's.
func BenchmarkOptMemories(b *testing.B) {
	for _, name := range []string{"m1", "m64", "shared", "atomic"} {
		for _, tier := range []string{"base", "opt"} {
			b.Run(fmt.Sprintf("%s/%s", name, tier), func(b *testing.B) {
				inst := thresholdInstance(b, memoriesBenchWAT, 0)
				inst.Code.setOSRCalls(0)
				if tier == "opt" {
					inst.Code.SetOptThreshold(0)
					inst.Code.SetOptBackground(false)
				} else {
					inst.Code.SetOptThreshold(-1)
				}
				ext, _ := inst.Export(name)
				if _, err := Invoke(b.Context(), ext.Func, []Val{{N: 1}}); err != nil {
					b.Fatal(err)
				}
				if got := optimized(inst, name); got != (tier == "opt") {
					b.Fatalf("optimized %v", got)
				}
				b.ResetTimer()
				for range b.N {
					if _, err := Invoke(b.Context(), ext.Func, []Val{{N: 10}}); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
