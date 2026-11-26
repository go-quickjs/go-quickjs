//go:build quickjs_jit && !android && !ios && ((amd64 && (linux || windows)) || (arm64 && (darwin || linux)))

package engine

import (
	"fmt"
	"strings"
	"testing"
)

// TestOptMemLimit runs, optimized, loops of accesses whose checks compare
// their addresses with memory 0's limit (its size less 4 KiB) and go out
// of line past it: within the last 4 KiB of the memory, across its end,
// in memories smaller than the slack, after calls that grow the memory,
// and after a callee that holds large numbers in every register it has,
// against the interpreter. On arm64 the limit is in a register the
// allocator gave nothing, made again after every call.
func TestOptMemLimit(t *testing.T) {
	var fill strings.Builder
	for i := range 24 {
		fmt.Fprintf(&fill, "(local.set %d (i64.add (local.get 0) (i64.const %d)))\n", i+1, 0x7fffffffffff-i)
	}
	for i := range 24 {
		fmt.Fprintf(&fill, "(local.set 0 (i64.xor (local.get 0) (local.get %d)))\n", i+1)
	}
	for _, pages := range []int{0, 1} {
		t.Run(fmt.Sprint(pages, " pages"), func(t *testing.T) {
			src := fmt.Sprintf(`(module (memory %d)
  (func $grow (result i32) (memory.grow (i32.const 1)))
  (func $fill (param i64) (result i64) (local i64 i64 i64 i64 i64 i64 i64 i64 i64 i64 i64 i64
      i64 i64 i64 i64 i64 i64 i64 i64 i64 i64 i64 i64)
    %s
    (local.get 0))
  (func (export "sum") (param $a i32) (param $n i32) (param $g i32) (result i64)
    (local $i i32) (local $s i64)
    (loop $l
      (if (i32.eq (local.get $i) (local.get $g)) (then (drop (call $grow))))
      (local.set $s (i64.add (local.get $s) (i64.load (i32.add (local.get $a) (local.get $i)))))
      (local.set $s (i64.add (local.get $s) (i64.load offset=8 (i32.add (local.get $a) (local.get $i)))))
      (i32.store8 offset=15 (i32.add (local.get $a) (local.get $i)) (local.get $i))
      (local.set $s (i64.xor (local.get $s) (i64.load16_u offset=2 (i32.add (local.get $a) (local.get $i)))))
      (local.set $s (i64.add (local.get $s) (i64.load32_u (i32.add (local.get $a) (i32.shr_u (local.get $i) (i32.const 1))))))
      (local.set $s (i64.add (local.get $s) (i64.load8_u offset=3 (i32.add (local.get $a) (i32.shr_u (local.get $i) (i32.const 2))))))
      (local.set $s (i64.add (local.get $s) (i64.load (i32.sub (i32.add (local.get $a) (local.get $n)) (i32.const 8)))))
      (local.set $s (i64.add (local.get $s) (i64.load (i32.add (local.get $a) (i32.and (local.get $i) (i32.const -8))))))
      (if (i32.eq (local.get $i) (i32.const 3)) (then (local.set $s (call $fill (local.get $s)))))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br_if $l (i32.lt_u (local.get $i) (local.get $n))))
    (local.get $s)))`, pages, fill.String())
			want := interpretedInstance(t, src)
			got := exitInstance(t, src, true, nil)
			const page = 65536
			for _, c := range []struct{ a, n, g int }{
				{0, 64, -1},
				{page - 4096 - 64, 128, -1},
				{page - 16 - 4095, 4096, -1},
				{page - 16 - 4095 + 1, 4096, -1},
				{page - 20, 8, -1},
				{page - 20, 8, 2},
				{page + 100, 8, 1},
				{2*page - 4096, 4096, 0},
				{1 << 31, 4, -1},
				{-8, 4, -1},
			} {
				sameOutcome(t, want, got, "sum", uint64(uint32(c.a)), uint64(c.n), uint64(uint32(c.g)))
			}
			if !nativeRan(got, "sum") {
				t.Fatal("sum ran no native code")
			}
		})
	}
}
