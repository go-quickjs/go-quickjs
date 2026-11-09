package engine

import (
	"errors"
	"testing"
)

// trapCase is a module whose export "f", called with no arguments, traps
// with code, its innermost frame at offset: the trapping instruction's, its
// first byte -- a prefixed instruction's prefix. op is that byte.
type trapCase struct {
	name   string
	src    string
	code   TrapCode
	offset uint32
	op     byte
}

// trapCases cover each trap an instruction raises. Node 26 reports the same
// offset for each, in the innermost frame of the RuntimeError's stack.
var trapCases = []trapCase{
	{"unreachable", `(module (func (export "f") nop nop unreachable))`,
		TrapUnreachable, 0x20, 0x00},
	{"i32.load", `(module (memory 1) (func (export "f") (result i32) (i32.load offset=4 (i32.const 65534))))`,
		TrapMemOutOfBounds, 0x28, 0x28},
	{"i32.load8_u of a local", `(module (memory 1) (func (export "f") (result i32) (local i32)
		(local.set 0 (i32.const 70000)) (i32.load8_u (local.get 0))))`,
		TrapMemOutOfBounds, 0x2e, 0x2d},
	{"i64.store", `(module (memory 1) (func (export "f") (i64.store (i32.const 65530) (i64.const 1))))`,
		TrapMemOutOfBounds, 0x29, 0x37},
	{"f64.load of memory 1", `(module (memory 1) (memory $m 1) (func (export "f") (result f64) (f64.load $m (i32.const 65530))))`,
		TrapMemOutOfBounds, 0x2a, 0x2b},
	{"i32.store16 of memory 1", `(module (memory 1) (memory $m 1) (func (export "f") (i32.store16 $m (i32.const 65535) (i32.const 1))))`,
		TrapMemOutOfBounds, 0x2b, 0x3b},
	{"i64 address overflow", `(module (memory i64 1) (func (export "f") (result i32) (i32.load offset=16 (i64.const -8))))`,
		TrapMemOutOfBounds, 0x26, 0x28},
	{"i64 memory store", `(module (memory i64 1) (func (export "f") (i32.store (i64.const 65536) (i32.const 0))))`,
		TrapMemOutOfBounds, 0x29, 0x36},
	{"i32.div_s by zero", `(module (func (export "f") (result i32) (i32.div_s (i32.const 1) (i32.const 0))))`,
		TrapDivByZero, 0x23, 0x6d},
	{"i32.div_s overflow", `(module (func (export "f") (result i32) (i32.div_s (i32.const 0x80000000) (i32.const -1))))`,
		TrapDivUnrepresentable, 0x27, 0x6d},
	{"i32.rem_u by zero", `(module (func (export "f") (result i32) (local i32) (i32.rem_u (i32.const 7) (local.get 0))))`,
		TrapRemByZero, 0x25, 0x70},
	{"i64.div_u by zero", `(module (func (export "f") (result i64) (local i64) (i64.div_u (local.get 0) (local.get 0))))`,
		TrapDivByZero, 0x25, 0x80},
	{"i64.rem_s by zero", `(module (func (export "f") (result i64) (i64.rem_s (i64.const 5) (i64.const 0))))`,
		TrapRemByZero, 0x23, 0x81},
	{"i32.trunc_f32_s", `(module (func (export "f") (result i32) (i32.trunc_f32_s (f32.const nan))))`,
		TrapFloatUnrepresentable, 0x24, 0xa8},
	{"i64.trunc_f64_u", `(module (func (export "f") (result i64) (local f64)
		(local.set 0 (f64.const -1)) (i64.trunc_f64_u (local.get 0))))`,
		TrapFloatUnrepresentable, 0x2e, 0xb1},
	{"call_indirect past the table", `(module (type $t (func)) (table 1 funcref)
		(func (export "f") (call_indirect (type $t) (i32.const 5))))`,
		TrapTableOutOfBounds, 0x26, 0x11},
	{"call_indirect of null", `(module (type $t (func)) (table 1 funcref)
		(func (export "f") (call_indirect (type $t) (i32.const 0))))`,
		TrapFuncSigMismatch, 0x26, 0x11},
	{"call_indirect of null in a typed table", `(module (type $t (func)) (table 1 (ref null $t))
		(func (export "f") (call_indirect (type $t) (i32.const 0))))`,
		TrapNullFunc, 0x27, 0x11},
	{"call_indirect of the wrong type", `(module (type $t (func)) (table 1 funcref) (elem (i32.const 0) $g)
		(func $g (param i32))
		(func (export "f") (call_indirect (type $t) (i32.const 0))))`,
		TrapFuncSigMismatch, 0x37, 0x11},
	{"return_call_indirect past the table", `(module (type $t (func)) (table 1 funcref)
		(func (export "f") (return_call_indirect (type $t) (i32.const 1))))`,
		TrapTableOutOfBounds, 0x26, 0x13},
	{"call_ref of null", `(module (type $t (func))
		(func (export "f") (call_ref $t (ref.null $t))))`,
		TrapNullDereference, 0x20, 0x14},
	{"ref.as_non_null", `(module (func (export "f") (drop (ref.as_non_null (ref.null func)))))`,
		TrapNullDereference, 0x20, 0xd4},
	{"table.get", `(module (table 2 funcref) (func (export "f") (drop (table.get (i32.const 2)))))`,
		TrapTableOutOfBounds, 0x26, 0x25},
	{"table.set", `(module (table 2 funcref) (func (export "f") (table.set (i32.const 7) (ref.null func))))`,
		TrapTableOutOfBounds, 0x28, 0x26},
	{"table.fill", `(module (table 2 funcref) (func (export "f") (table.fill (i32.const 1) (ref.null func) (i32.const 2))))`,
		TrapTableOutOfBounds, 0x2a, 0xfc},
	{"table.copy", `(module (table 2 funcref) (func (export "f") (table.copy (i32.const 0) (i32.const 1) (i32.const 2))))`,
		TrapTableOutOfBounds, 0x2a, 0xfc},
	{"table.init past the table", `(module (table 2 funcref) (elem func $g $g) (func $g)
		(func (export "f") (table.init 0 (i32.const 1) (i32.const 0) (i32.const 2))))`,
		TrapTableOutOfBounds, 0x36, 0xfc},
	{"table.init past the segment", `(module (table 2 funcref) (elem func $g) (func $g)
		(func (export "f") (table.init 0 (i32.const 0) (i32.const 0) (i32.const 2))))`,
		TrapElementSegmentOutOfBounds, 0x35, 0xfc},
	{"table.init past both", `(module (table 2 funcref) (elem func $g) (func $g)
		(func (export "f") (table.init 0 (i32.const 5) (i32.const 3) (i32.const 1))))`,
		TrapTableOutOfBounds, 0x35, 0xfc},
	{"table.init of a dropped segment", `(module (table 2 funcref) (elem func $g) (func $g)
		(func (export "f") (elem.drop 0) (table.init 0 (i32.const 0) (i32.const 0) (i32.const 1))))`,
		TrapElementSegmentOutOfBounds, 0x38, 0xfc},
	{"memory.fill", `(module (memory 1) (func (export "f") (memory.fill (i32.const 65535) (i32.const 0) (i32.const 2))))`,
		TrapMemOutOfBounds, 0x2b, 0xfc},
	{"memory.copy", `(module (memory 1) (func (export "f") (memory.copy (i32.const 0) (i32.const 65535) (i32.const 2))))`,
		TrapMemOutOfBounds, 0x2b, 0xfc},
	{"memory.init of a dropped segment", `(module (memory 1) (data "ab")
		(func (export "f") (data.drop 0) (memory.init 0 (i32.const 0) (i32.const 0) (i32.const 1))))`,
		TrapMemOutOfBounds, 0x2f, 0xfc},
	{"struct.get of null", `(module (type $s (struct (field i32)))
		(func (export "f") (result i32) (struct.get $s 0 (ref.null $s))))`,
		TrapNullDereference, 0x25, 0xfb},
	{"struct.set of null", `(module (type $s (struct (field (mut i32))))
		(func (export "f") (struct.set $s 0 (ref.null $s) (i32.const 1))))`,
		TrapNullDereference, 0x26, 0xfb},
	{"array.len of null", `(module (type $a (array i8))
		(func (export "f") (result i32) (array.len (ref.null $a))))`,
		TrapNullDereference, 0x24, 0xfb},
	{"array.get past the end", `(module (type $a (array i32))
		(func (export "f") (result i32) (array.get $a (array.new_default $a (i32.const 3)) (i32.const 3))))`,
		TrapArrayOutOfBounds, 0x29, 0xfb},
	{"array.set past the end", `(module (type $a (array (mut i64)))
		(func (export "f") (array.set $a (array.new_default $a (i32.const 1)) (i32.const 1) (i64.const 0))))`,
		TrapArrayOutOfBounds, 0x2a, 0xfb},
	{"array.new too large", `(module (type $a (array i64))
		(func (export "f") (drop (array.new_default $a (i32.const 0x7fffffff)))))`,
		TrapArrayTooLarge, 0x27, 0xfb},
	{"array.new_data past the segment", `(module (type $a (array i8)) (data "abc")
		(func (export "f") (drop (array.new_data $a 0 (i32.const 1) (i32.const 3)))))`,
		TrapDataSegmentOutOfBounds, 0x28, 0xfb},
	{"array.new_elem past the segment", `(module (type $a (array funcref)) (elem func $g) (func $g)
		(func (export "f") (drop (array.new_elem $a 0 (i32.const 0) (i32.const 2)))))`,
		TrapElementSegmentOutOfBounds, 0x30, 0xfb},
	{"array.fill past the end", `(module (type $a (array (mut i8)))
		(func (export "f") (array.fill $a (array.new_default $a (i32.const 4)) (i32.const 2) (i32.const 0) (i32.const 3))))`,
		TrapArrayOutOfBounds, 0x2c, 0xfb},
	{"array.copy past the end", `(module (type $a (array (mut i8)))
		(func (export "f") (local (ref $a)) (local.set 0 (array.new_default $a (i32.const 4)))
			(array.copy $a $a (local.get 0) (i32.const 0) (local.get 0) (i32.const 1) (i32.const 4))))`,
		TrapArrayOutOfBounds, 0x35, 0xfb},
	{"array.init_data past the segment", `(module (type $a (array (mut i8))) (data "ab")
		(func (export "f") (array.init_data $a 0 (array.new_default $a (i32.const 4)) (i32.const 0) (i32.const 1) (i32.const 2))))`,
		TrapDataSegmentOutOfBounds, 0x2f, 0xfb},
	{"ref.cast", `(module (type $s (struct)) (type $t (struct (field i32)))
		(func (export "f") (drop (ref.cast (ref $t) (struct.new $s)))))`,
		TrapIllegalCast, 0x27, 0xfb},
	{"i31.get_s of null", `(module (func (export "f") (result i32) (i31.get_s (ref.null i31))))`,
		TrapNullDereference, 0x21, 0xfb},
	{"i32.atomic.load unaligned", `(module (memory 1 1 shared) (func (export "f") (result i32) (i32.atomic.load (i32.const 2))))`,
		TrapUnalignedAccess, 0x27, 0xfe},
	{"i64.atomic.rmw.add past the end", `(module (memory 1) (func (export "f") (result i64) (i64.atomic.rmw.add (i32.const 65536) (i64.const 1))))`,
		TrapMemOutOfBounds, 0x2a, 0xfe},
	{"i32.atomic.store past the end", `(module (memory 1) (func (export "f") (i32.atomic.store (i32.const 65536) (i32.const 1))))`,
		TrapMemOutOfBounds, 0x29, 0xfe},
	{"memory.atomic.notify unaligned", `(module (memory 1) (func (export "f") (result i32) (memory.atomic.notify (i32.const 1) (i32.const 1))))`,
		TrapUnalignedAccess, 0x28, 0xfe},
	{"memory.atomic.wait32 of an unshared memory", `(module (memory 1) (func (export "f") (result i32) (memory.atomic.wait32 (i32.const 0) (i32.const 0) (i64.const 0))))`,
		TrapAtomicsWaitNotAllowed, 0x2a, 0xfe},
	{"v128.load", `(module (memory 1) (func (export "f") (drop (v128.load (i32.const 65530)))))`,
		TrapMemOutOfBounds, 0x27, 0xfd},
	{"v128.store", `(module (memory 1) (func (export "f") (v128.store (i32.const 65521) (v128.const i64x2 0 0))))`,
		TrapMemOutOfBounds, 0x39, 0xfd},
	{"v128.load8_splat", `(module (memory 1) (func (export "f") (drop (v128.load8_splat (i32.const 65536)))))`,
		TrapMemOutOfBounds, 0x27, 0xfd},
	{"v128.load32_lane", `(module (memory 1) (func (export "f") (drop (v128.load32_lane 1 (i32.const 65534) (v128.const i64x2 0 0)))))`,
		TrapMemOutOfBounds, 0x39, 0xfd},
	{"v128.store16_lane", `(module (memory 1) (func (export "f") (v128.store16_lane 3 (i32.const 65535) (v128.const i64x2 0 0))))`,
		TrapMemOutOfBounds, 0x39, 0xfd},
	{"throw_ref of null", `(module (func (export "f") (throw_ref (ref.null exn))))`,
		TrapRethrowNull, 0x20, 0x0a},
}

// TestTrapOffsets checks that a trap's innermost frame is at the
// instruction that trapped, for each kind of trap, where the function has
// made no call: interpreted, or natively where WASM_JIT compiles every
// function.
func TestTrapOffsets(t *testing.T) {
	for _, c := range trapCases {
		t.Run(c.name, func(t *testing.T) {
			inst, err := instantiateWAT(t, c.src)
			if err != nil {
				t.Fatal(err)
			}
			ext, _ := inst.Export("f")
			_, err = call(t, inst, "f")
			var tr *Trap
			if !errors.As(err, &tr) || tr.Code != c.code {
				t.Fatalf("f() = %v, want the trap %q", err, c.code)
			}
			if len(tr.Frames) != 1 {
				t.Fatalf("frames %+v, want f's alone", tr.Frames)
			}
			fr := tr.Frames[0]
			if fr.Func != ext.Func.Index || fr.Offset != c.offset {
				t.Fatalf("innermost frame: function %d at %#x, want %d at %#x", fr.Func, fr.Offset, ext.Func.Index, c.offset)
			}
			if op := inst.Code.Module.Bytes[fr.Offset]; op != c.op {
				t.Fatalf("the byte at %#x is %#02x, want %#02x", fr.Offset, op, c.op)
			}
		})
	}
}

// TestTrapOffsetUnderCall checks a trap's frames below a call: the
// trapping function at the trapping instruction -- not at the call it made
// before -- and its caller at its call.
func TestTrapOffsetUnderCall(t *testing.T) {
	inst, err := instantiateWAT(t, `(module
		(func $id (param i32) (result i32) (local.get 0))
		(func $g (param i32) (result i32)
			(i32.div_u (call $id (i32.const 1)) (local.get 0)))
		(func (export "f") (param i32) (result i32) nop (call $g (local.get 0))))`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = call(t, inst, "f", 0)
	var tr *Trap
	if !errors.As(err, &tr) || tr.Code != TrapDivByZero {
		t.Fatalf("f(0) = %v, want divide by zero", err)
	}
	want := []Frame{{Module: inst, Func: 1, Offset: 0x2d}, {Module: inst, Func: 2, Offset: 0x34}}
	if len(tr.Frames) != 2 || tr.Frames[0] != want[0] || tr.Frames[1] != want[1] {
		t.Fatalf("frames %+v, want %+v", tr.Frames, want)
	}
}

// TestStackOverflowFrames checks a stack overflow's frames, as Node 26
// gives them: the function that could not be entered, at the start of its
// body -- its locals' declaration -- then each caller at its call, one
// frame for each call made.
func TestStackOverflowFrames(t *testing.T) {
	inst, err := instantiateWAT(t, `(module
		(func $a (export "f") (call $b))
		(func $b (local i32) i32.const 1 drop (call $a)))`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = call(t, inst, "f")
	var tr *Trap
	if !errors.As(err, &tr) || tr.Code != TrapStackOverflow {
		t.Fatalf("f() = %v, want a stack overflow", err)
	}
	if len(tr.Frames) != MaxCallDepth+1 {
		t.Fatalf("%d frames, want %d", len(tr.Frames), MaxCallDepth+1)
	}
	// The call at the even depth MaxCallDepth is $b's, of $a.
	want := []Frame{{Module: inst, Func: 0, Offset: 0x1e}, {Module: inst, Func: 1, Offset: 0x29}, {Module: inst, Func: 0, Offset: 0x1f}}
	for i, w := range want {
		if tr.Frames[i] != w {
			t.Fatalf("frame %d is %+v, want %+v", i, tr.Frames[i], w)
		}
	}
	if code := inst.Code.Module.Functions[0].Code; code.Offset != 0x1e {
		t.Fatalf("$a's body is at %#x, not 0x1e", code.Offset)
	}
}
