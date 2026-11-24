//go:build quickjs_jit && !android && !ios && ((amd64 && (linux || windows)) || (arm64 && (darwin || linux)))

package engine

import (
	"errors"
	"testing"
)

// matchOpt calls each case on an interpreted instance and one whose
// functions the optimizing tier compiled at once, and compares results,
// traps and the traps' frames.
func matchOpt(t *testing.T, src string, cases []struct {
	name string
	args []uint64
}) {
	t.Helper()
	interp := interpretedInstance(t, src)
	inst := optInstance(t, src)
	defer func() {
		if cj := inst.Code.jit.Load(); cj == nil || !cj.inline.any() {
			t.Errorf("no call was inlined")
		}
	}()
	for _, c := range cases {
		want, werr := call(t, interp, c.name, c.args...)
		got, gerr := call(t, inst, c.name, c.args...)
		if (werr == nil) != (gerr == nil) {
			t.Fatalf("%s%v = %v, %v; want %v, %v", c.name, c.args, got, gerr, want, werr)
		}
		if werr == nil {
			for i := range want {
				if got[i].N != want[i].N {
					t.Fatalf("%s%v = %v; want %v", c.name, c.args, got, want)
				}
			}
			continue
		}
		var wt, gt *Trap
		if !errors.As(werr, &wt) || !errors.As(gerr, &gt) || wt.Code != gt.Code || len(wt.Frames) != len(gt.Frames) {
			t.Fatalf("%s%v traps %v (%d frames), want %v (%d frames)", c.name, c.args, gerr, frameCount(gt), werr, frameCount(wt))
		}
		for i := range wt.Frames {
			if w, g := wt.Frames[i], gt.Frames[i]; w.Func != g.Func || w.Offset != g.Offset {
				t.Fatalf("%s%v: frame %d is %+v, want %+v", c.name, c.args, i, g, w)
			}
		}
	}
}

func frameCount(t *Trap) int {
	if t == nil {
		return -1
	}
	return len(t.Frames)
}

// TestOptInline checks calls the optimizing tier inlines against the
// interpreter: results, traps in inlined bodies at every depth with their
// frames, calls made from inlined bodies, recursion, and callees that
// never return.
func TestOptInline(t *testing.T) {
	src := `(module
  (memory 1)
  (global $g (mut i32) (i32.const 0))
  (func $fib (param i32) (result i64)
    (if (result i64) (i32.lt_u (local.get 0) (i32.const 2))
      (then (i64.extend_i32_u (local.get 0)))
      (else (i64.add (call $fib (i32.sub (local.get 0) (i32.const 1)))
                     (call $fib (i32.sub (local.get 0) (i32.const 2)))))))
  (func $div (param i32 i32) (result i32) (i32.div_s (local.get 0) (local.get 1)))
  (func $load (param i32) (result i32) (i32.load offset=4 (local.get 0)))
  (func $mid (param i32 i32) (result i32)
    (global.set $g (i32.add (global.get $g) (i32.const 1)))
    (i32.add (call $div (local.get 0) (local.get 1)) (call $load (local.get 1))))
  (func $outer (param i32 i32) (result i32)
    (local $t i32)
    (local.set $t (call $mid (local.get 0) (local.get 1)))
    (i32.add (local.get $t) (call $mid (local.get 1) (local.get 0))))
  (func $loop (param i32) (result i32) (local $s i32)
    (block $done (loop $l
      (br_if $done (i32.eqz (local.get 0)))
      (local.set $s (i32.add (local.get $s) (local.get 0)))
      (local.set 0 (i32.sub (local.get 0) (i32.const 1)))
      (br $l)))
    (local.get $s))
  (func $never (param i32) (result i32) (if (local.get 0) (then unreachable)) (return (i32.const 7)))
  (func $even (param i32) (result i32)
    (if (result i32) (i32.eqz (local.get 0)) (then (i32.const 1)) (else (call $odd (i32.sub (local.get 0) (i32.const 1))))))
  (func $odd (param i32) (result i32)
    (if (result i32) (i32.eqz (local.get 0)) (then (i32.const 0)) (else (call $even (i32.sub (local.get 0) (i32.const 1))))))
  (func $grow (param i32) (result i32) (memory.grow (local.get 0)))
  (func (export "fib") (param i32) (result i64) (call $fib (local.get 0)))
  (func (export "outer") (param i32 i32) (result i32) (call $outer (local.get 0) (local.get 1)))
  (func (export "g") (result i32) (global.get $g))
  (func (export "loop") (param i32) (result i32) (i32.add (call $loop (local.get 0)) (call $loop (i32.const 3))))
  (func (export "never") (param i32) (result i32) (i32.add (call $never (local.get 0)) (i32.const 1)))
  (func (export "even") (param i32) (result i32) (call $even (local.get 0)))
  (func (export "grow") (param i32) (result i32) (call $grow (local.get 0))))`
	matchOpt(t, src, []struct {
		name string
		args []uint64
	}{
		{"fib", []uint64{0}}, {"fib", []uint64{1}}, {"fib", []uint64{2}}, {"fib", []uint64{20}},
		{"outer", []uint64{12, 5}}, {"outer", []uint64{5, 0}}, {"outer", []uint64{0, 5}},
		{"outer", []uint64{0x80000000, 0xffffffff}}, {"outer", []uint64{7, 65534}},
		{"loop", []uint64{0}}, {"loop", []uint64{100}},
		{"never", []uint64{0}}, {"never", []uint64{1}},
		{"even", []uint64{10}}, {"even", []uint64{10001}},
		{"grow", []uint64{1}}, {"grow", []uint64{100000}},
	})
}

// TestOptInlineStackOverflow checks the call depth across inlined calls:
// a recursion through calls the optimizing tier inlines traps where the
// calls would, with one frame for each.
func TestOptInlineStackOverflow(t *testing.T) {
	src := `(module
  (func $a (param i32) (result i32) (i32.add (call $b (local.get 0)) (i32.const 1)))
  (func $b (param i32) (result i32) (i32.add (call $c (local.get 0)) (i32.const 1)))
  (func $c (param i32) (result i32)
    (if (result i32) (i32.eqz (local.get 0)) (then (i32.const 0))
      (else (call $a (i32.sub (local.get 0) (i32.const 1))))))
  (func (export "f") (param i32) (result i32) (call $a (local.get 0))))`
	var cases []struct {
		name string
		args []uint64
	}
	for _, n := range []uint64{0, 10, 1000, uint64(MaxCallDepth)/3 - 2, uint64(MaxCallDepth)/3 - 1,
		uint64(MaxCallDepth) / 3, uint64(MaxCallDepth)/3 + 1, uint64(MaxCallDepth)} {
		cases = append(cases, struct {
			name string
			args []uint64
		}{"f", []uint64{n}})
	}
	matchOpt(t, src, cases)
}
