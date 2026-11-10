package engine

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"testing"
)

// sameAsInterpreter calls each export of calls on an instance of src whose
// functions are compiled at their first call and on one that interprets
// them, in order, and fails on any difference in the results, the traps or
// the memories after each call. Each function it calls must have native
// code after the call.
func sameAsInterpreter(t *testing.T, src string, calls []jitCall) {
	t.Helper()
	sameAsInterpreterIn(t, nativeInstance(t, src), src, calls, nativeRan)
}

// sameAsInterpreterIn is sameAsInterpreter on nat, an instance of src,
// each function it calls having to have code ran says it has after the
// call.
func sameAsInterpreterIn(t *testing.T, nat *Instance, src string, calls []jitCall, ran func(*Instance, string) bool) {
	t.Helper()
	in := interpretedInstance(t, src)
	for _, cl := range calls {
		what := fmt.Sprintf("%s%#x", cl.name, cl.args)
		rn, en := call(t, nat, cl.name, cl.args...)
		ri, ei := call(t, in, cl.name, cl.args...)
		if !sameJITOutcome(rn, en, ri, ei) {
			t.Fatalf("%s: native %v %v, interpreted %v %v", what, rn, en, ri, ei)
		}
		if cl.trap != 0 {
			var tr *Trap
			if !errors.As(en, &tr) || tr.Code != cl.trap {
				t.Fatalf("%s: %v, want trap %q", what, en, cl.trap)
			}
		} else if en != nil {
			t.Fatalf("%s: %v", what, en)
		}
		if cl.want != nil && (len(rn) == 0 || rn[0].N != *cl.want) {
			t.Fatalf("%s = %v, want %#x", what, rn, *cl.want)
		}
		if !ran(nat, cl.name) {
			t.Fatalf("%s has no native code of the tier tested", cl.name)
		}
		for i := range in.Memories {
			if !bytes.Equal(nat.Memories[i].Bytes(), in.Memories[i].Bytes()) {
				t.Fatalf("%s: memory %d differs", what, i)
			}
		}
	}
}

// jitCall is a call sameAsInterpreter makes: what it traps with, if it
// does, and its first result, if want is set.
type jitCall struct {
	name string
	args []uint64
	trap TrapCode
	want *uint64
}

func u64p(v uint64) *uint64 { return &v }

func sameJITOutcome(rn []Val, en error, ri []Val, ei error) bool {
	if (en == nil) != (ei == nil) {
		return false
	}
	if en != nil {
		var tn, ti *Trap
		return errors.As(en, &tn) && errors.As(ei, &ti) && tn.Code == ti.Code
	}
	if len(rn) != len(ri) {
		return false
	}
	for i := range rn {
		if rn[i].N != ri[i].N {
			return false
		}
	}
	return true
}

// An i64 memory's address plus the offset and the size are checked in 64
// bits without wrapping: an address near 2^64 traps, whatever the offset.
func TestJITMemory64Bounds(t *testing.T) { memory64Bounds(t, sameAsInterpreter) }

// memory64Bounds runs TestJITMemory64Bounds's calls through same.
func memory64Bounds(t *testing.T, same func(*testing.T, string, []jitCall)) {
	const src = `(module
  (memory i64 1 3)
  (func (export "load8") (param i64) (result i32) (i32.load8_u (local.get 0)))
  (func (export "load16") (param i64) (result i64) (i64.load offset=16 (local.get 0)))
  (func (export "loadBig") (param i64) (result i32) (i32.load8_u offset=0xFFFFFFFFFFFFFFF0 (local.get 0)))
  (func (export "loadHuge") (param i64) (result i32) (i32.load offset=0x80000000 (local.get 0)))
  (func (export "store") (param i64 i64) (i64.store offset=8 (local.get 0) (local.get 1)))
  (func (export "storeF") (param i64 f64) (f64.store offset=3 (local.get 0) (local.get 1)))
  (func (export "loadF") (param i64) (result f32) (f32.load offset=1 (local.get 0)))
  (func (export "constLast") (result i32) (i32.store8 offset=65535 (i64.const 0) (i32.const 7)) (i32.load8_u (i64.const 65535)))
  (func (export "constWrap") (result i32) (i32.load offset=16 (i64.const -8)))
  (func (export "constPast") (result i32) (i32.load offset=65533 (i64.const 0)))
  (func (export "grow") (param i64) (result i64) (memory.grow (local.get 0)))
  (func (export "size") (result i64) (memory.size)))`
	oob := TrapMemOutOfBounds
	same(t, src, []jitCall{
		{name: "load8", args: []uint64{0}, want: u64p(0)},
		{name: "store", args: []uint64{65536 - 16, 0x0102030405060708}},
		{name: "load16", args: []uint64{65536 - 24}, want: u64p(0x0102030405060708)},
		{name: "load8", args: []uint64{65535}, want: u64p(1)},
		{name: "load8", args: []uint64{65536}, trap: oob},
		{name: "load8", args: []uint64{math.MaxUint64}, trap: oob},
		{name: "load16", args: []uint64{65536 - 23}, trap: oob},
		{name: "load16", args: []uint64{math.MaxUint64 - 15}, trap: oob},
		{name: "load16", args: []uint64{math.MaxUint64 - 16}, trap: oob},
		{name: "load16", args: []uint64{math.MaxUint64 - 20}, trap: oob},
		{name: "load16", args: []uint64{1 << 63}, trap: oob},
		{name: "loadBig", args: []uint64{0x10}, trap: oob},
		{name: "loadBig", args: []uint64{0x0F}, trap: oob},
		{name: "loadBig", args: []uint64{0x20}, trap: oob},
		{name: "loadBig", args: []uint64{0}, trap: oob},
		{name: "loadHuge", args: []uint64{0}, trap: oob},
		{name: "store", args: []uint64{math.MaxUint64 - 7, 1}, trap: oob},
		{name: "store", args: []uint64{65536 - 15, 1}, trap: oob},
		{name: "storeF", args: []uint64{65536 - 11, math.Float64bits(1.5)}},
		{name: "storeF", args: []uint64{65536 - 10, math.Float64bits(1.5)}, trap: oob},
		{name: "loadF", args: []uint64{65536 - 5}},
		{name: "loadF", args: []uint64{65536 - 4}, trap: oob},
		{name: "constLast", want: u64p(7)},
		{name: "constWrap", trap: oob},
		{name: "constPast", trap: oob},
		{name: "grow", args: []uint64{2}, want: u64p(1)},
		{name: "load8", args: []uint64{3*65536 - 1}, want: u64p(0)},
		{name: "load8", args: []uint64{3 * 65536}, trap: oob},
		{name: "loadHuge", args: []uint64{0x7fffffff}, trap: oob},
		{name: "size", want: u64p(3)},
	})
}

// The last byte of a memory is in bounds and the one past it is not, for
// each width, after a grow too.
func TestJITMemoryLastByte(t *testing.T) { memoryLastByte(t, sameAsInterpreter) }

// memoryLastByte runs TestJITMemoryLastByte's calls through same.
func memoryLastByte(t *testing.T, same func(*testing.T, string, []jitCall)) {
	for _, mem := range []string{"(memory 1 2)", "(memory i64 1 2)"} {
		idx := "i32"
		if mem != "(memory 1 2)" {
			idx = "i64"
		}
		src := fmt.Sprintf(`(module
  %s
  (func (export "l1") (param %[2]s) (result i32) (i32.load8_s (local.get 0)))
  (func (export "l2") (param %[2]s) (result i32) (i32.load16_u (local.get 0)))
  (func (export "l4") (param %[2]s) (result i32) (i32.load (local.get 0)))
  (func (export "l8") (param %[2]s) (result i64) (i64.load (local.get 0)))
  (func (export "s1") (param %[2]s) (i32.store8 (local.get 0) (i32.const -1)))
  (func (export "s8") (param %[2]s) (i64.store (local.get 0) (i64.const -1)))
  (func (export "grow") (result i32) (drop (memory.grow (%[2]s.const 1))) (i32.const 0)))`, mem, idx)
		oob := TrapMemOutOfBounds
		var calls []jitCall
		for _, size := range []uint64{65536, 2 * 65536} {
			calls = append(calls,
				jitCall{name: "s1", args: []uint64{size - 1}},
				jitCall{name: "l1", args: []uint64{size - 1}, want: u64p(math.MaxUint32)},
				jitCall{name: "l1", args: []uint64{size}, trap: oob},
				jitCall{name: "l2", args: []uint64{size - 2}, want: u64p(0xff00)},
				jitCall{name: "l2", args: []uint64{size - 1}, trap: oob},
				jitCall{name: "l4", args: []uint64{size - 4}},
				jitCall{name: "l4", args: []uint64{size - 3}, trap: oob},
				jitCall{name: "s8", args: []uint64{size - 8}},
				jitCall{name: "l8", args: []uint64{size - 8}, want: u64p(math.MaxUint64)},
				jitCall{name: "l8", args: []uint64{size - 7}, trap: oob},
				jitCall{name: "s8", args: []uint64{size - 7}, trap: oob},
				jitCall{name: "grow"},
			)
		}
		t.Run(idx, func(t *testing.T) { same(t, src, calls) })
	}
}

// Loads and stores of memories other than 0 -- an i32 one, an i64 one and
// a shared one -- check each memory's own size and reach its own bytes.
func TestJITMultiMemory(t *testing.T) { multiMemory(t, sameAsInterpreter) }

// multiMemory runs TestJITMultiMemory's calls through same.
func multiMemory(t *testing.T, same func(*testing.T, string, []jitCall)) {
	const src = `(module
  (memory $m0 1)
  (memory $m1 2 5)
  (memory $m2 i64 1 2)
  (memory $m3 1 3 shared)
  (func (export "st1") (param i32 i64) (i64.store $m1 offset=4 (local.get 0) (local.get 1)))
  (func (export "ld1") (param i32) (result i64) (i64.load $m1 offset=4 (local.get 0)))
  (func (export "ld1b") (param i32) (result i32) (i32.load8_s $m1 (local.get 0)))
  (func (export "st2") (param i64 f64) (f64.store $m2 offset=8 (local.get 0) (local.get 1)))
  (func (export "ld2") (param i64) (result f64) (f64.load $m2 offset=8 (local.get 0)))
  (func (export "st3") (param i32 i32) (i32.store16 $m3 (local.get 0) (local.get 1)))
  (func (export "ld3") (param i32) (result i32) (i32.load16_u $m3 (local.get 0)))
  (func (export "ld0") (param i32) (result i32) (i32.load $m0 (local.get 0)))
  (func (export "const") (result i32)
    (i32.store8 $m1 (i32.const 131071) (i32.const 9))
    (i32.store8 $m3 (i32.const 65535) (i32.const 10))
    (i32.add (i32.load8_u $m1 (i32.const 131071)) (i32.load8_u $m3 (i32.const 65535))))
  (func (export "constPast") (result i32) (i32.load8_u $m1 (i32.const 131072)))
  (func (export "grow3") (param i32) (result i32) (memory.grow $m3 (local.get 0))))`
	oob := TrapMemOutOfBounds
	same(t, src, []jitCall{
		{name: "st1", args: []uint64{131072 - 12, 0x1122334455667788}},
		{name: "ld1", args: []uint64{131072 - 12}, want: u64p(0x1122334455667788)},
		{name: "ld1b", args: []uint64{131071}, want: u64p(0x11)},
		{name: "ld1", args: []uint64{131072 - 11}, trap: oob},
		{name: "st1", args: []uint64{math.MaxUint32, 1}, trap: oob},
		{name: "ld0", args: []uint64{65532}, want: u64p(0)},
		{name: "ld0", args: []uint64{65533}, trap: oob},
		{name: "st2", args: []uint64{65536 - 16, math.Float64bits(-2.5)}},
		{name: "ld2", args: []uint64{65536 - 16}, want: u64p(math.Float64bits(-2.5))},
		{name: "ld2", args: []uint64{65536 - 15}, trap: oob},
		{name: "ld2", args: []uint64{math.MaxUint64 - 7}, trap: oob},
		{name: "st3", args: []uint64{65534, 0x12345}},
		{name: "ld3", args: []uint64{65534}, want: u64p(0x2345)},
		{name: "ld3", args: []uint64{65535}, trap: oob},
		{name: "const", want: u64p(19)},
		{name: "constPast", trap: oob},
		{name: "grow3", args: []uint64{1}, want: u64p(1)},
		{name: "ld3", args: []uint64{65535}, want: u64p(10)},
		{name: "st3", args: []uint64{131070, 7}},
		{name: "ld3", args: []uint64{131070}, want: u64p(7)},
		{name: "ld3", args: []uint64{131071}, trap: oob},
	})
}

// A memory other than 0 grown inside a loop, and by a native call between
// two accesses of the caller's, is accessed at its new size: native code
// reads its base and size where the grow left them.
func TestJITMultiMemoryGrow(t *testing.T) { multiMemoryGrow(t, sameAsInterpreter) }

// multiMemoryGrow runs TestJITMultiMemoryGrow's calls through same.
func multiMemoryGrow(t *testing.T, same func(*testing.T, string, []jitCall)) {
	const src = `(module
  (memory $m0 1)
  (memory $m1 1 8)
  (memory $m2 i64 1 8)
  (func $grow1 (param i32) (result i32) (memory.grow $m1 (local.get 0)))
  (func $grow2 (param i64) (result i64) (memory.grow $m2 (local.get 0)))
  ;; Grows memory 1 by a page a turn, writing the new last byte and
  ;; summing every last byte so far; then adds the byte at the end less
  ;; the second parameter.
  (func (export "loop") (param i32 i32) (result i32)
    (local $i i32) (local $sum i32) (local $end i32)
    (loop $l
      (drop (memory.grow $m1 (i32.const 1)))
      (local.set $end (i32.mul (memory.size $m1) (i32.const 65536)))
      (i32.store8 $m1 (i32.sub (local.get $end) (i32.const 1)) (i32.add (local.get $i) (i32.const 1)))
      (local.set $sum (i32.add (local.get $sum) (i32.load8_u $m1 (i32.sub (local.get $end) (i32.const 1)))))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br_if $l (i32.lt_u (local.get $i) (local.get 0))))
    (i32.add (local.get $sum) (i32.load8_u $m1 (i32.sub (local.get $end) (local.get 1)))))
  ;; Stores at the first byte past memory 2, which traps, unless the call
  ;; between grows it.
  (func (export "byCall") (param i64 i32) (result i32)
    (i64.store8 $m2 (i64.const 65535) (i64.const 3))
    (if (local.get 1) (then (drop (call $grow2 (local.get 0)))))
    (i64.store8 $m2 (i64.const 65536) (i64.const 4))
    (i32.add (i32.load8_u $m2 (i64.const 65535)) (i32.load8_u $m2 (i64.const 65536))))
  (func (export "grow1") (param i32) (result i32) (call $grow1 (local.get 0)))
  (func (export "size1") (result i32) (memory.size $m1)))`
	oob := TrapMemOutOfBounds
	same(t, src, []jitCall{
		{name: "byCall", args: []uint64{1, 0}, trap: oob},
		{name: "byCall", args: []uint64{1, 1}, want: u64p(7)},
		{name: "byCall", args: []uint64{0, 1}, want: u64p(7)},
		{name: "loop", args: []uint64{3, 0}, trap: oob},
		{name: "size1", want: u64p(4)},
		{name: "loop", args: []uint64{2, 1}, want: u64p(1 + 2 + 2)},
		{name: "size1", want: u64p(6)},
		{name: "loop", args: []uint64{3, 0}, trap: oob},
		{name: "size1", want: u64p(8)},
		{name: "loop", args: []uint64{1, 1}, want: u64p(2)},
		{name: "grow1", args: []uint64{1}, want: u64p(math.MaxUint32)},
	})
}
