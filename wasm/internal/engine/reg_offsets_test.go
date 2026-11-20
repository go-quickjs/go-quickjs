package engine

import (
	"context"
	"errors"
	"testing"
)

// TestRegLargeOffsets checks the offsets traps and stack traces report
// where they are past what a small module has, as a module near 4 GiB's
// are: the records' offsets are moved up by nearly 2^32, and a trap raised
// at a pc (a division) and one raised at a module offset (a null table
// entry) must report the moved offsets exact, natively and on the Go
// table, as must the caller's frame.
func TestRegLargeOffsets(t *testing.T) {
	const src = `(module
	  (type $v (func))
	  (table 1 funcref)
	  (func $div (param i32) (result i32) (i32.div_u (i32.const 1) (local.get 0)))
	  (func $ind (call_indirect (type $v) (i32.const 0)))
	  (func (export "div") (param i32) (result i32) (call $div (local.get 0)))
	  (func (export "ind") (call $ind)))`
	const up = 0xFFFF0000
	for _, native := range []bool{false, true} {
		if native && !nativeBuilt {
			continue
		}
		inst := instantiateReg(t, src)
		frames := func(name string, args ...uint64) []Frame {
			t.Helper()
			var err error
			withNative(inst, native, func() { _, err = call(t, inst, name, args...) })
			var tr *Trap
			if !errors.As(err, &tr) || len(tr.Frames) < 2 {
				t.Fatalf("%s: %v, want a trap of two frames", name, err)
			}
			return tr.Frames
		}
		want := map[string][]Frame{"div": frames("div", 0), "ind": frames("ind")}
		for i := range inst.Code.fns {
			p := inst.Code.fns[i].Load()
			if p == nil || p.reg == nil {
				continue
			}
			for k := range p.reg.offs {
				p.reg.offs[k] += up
			}
			for k := range p.reg.sites {
				p.reg.sites[k].at += up
			}
		}
		for name, w := range want {
			got := frames(name, 0)
			for k := 0; k < 2; k++ {
				if got[k].Offset != w[k].Offset+up {
					t.Errorf("native %v: %s's frame %d at %#x, want %#x", native, name, k, got[k].Offset, w[k].Offset+up)
				}
			}
		}
		// A module offset that reads as the complement of one of the
		// code's own pcs: the null entry's site moved to 2^32-2.
		ind := inst.Code.fns[1].Load()
		ind.reg.sites[0].at = 1<<32 - 2
		if got := frames("ind"); got[0].Offset != 1<<32-2 {
			t.Errorf("native %v: ind's trap at %#x, want %#x", native, got[0].Offset, uint32(1<<32-2))
		}
	}
}

// TestRegTooLarge checks that a body whose register code would pass what
// a pc holds stops its call with ErrFunctionTooLarge rather than a panic,
// the limit lowered to reach it.
func TestRegTooLarge(t *testing.T) {
	saved := maxRegCode
	maxRegCode = 20
	defer func() { maxRegCode = saved }()
	inst := instantiateReg(t, `(module
	  (func (export "big") (param i32) (result i32)
	    (local.get 0) (i32.const 1) (i32.add) (i32.const 2) (i32.mul) (i32.const 3) (i32.xor)
	    (i32.const 4) (i32.sub) (i32.const 5) (i32.add) (i32.const 6) (i32.mul) (i32.const 7) (i32.xor)
	    (i32.const 8) (i32.sub) (i32.const 9) (i32.add) (i32.const 10) (i32.mul) (i32.const 11) (i32.xor)
	    (i32.const 12) (i32.sub) (i32.const 13) (i32.add) (i32.const 14) (i32.mul) (i32.const 15) (i32.xor)
	    (i32.const 16) (i32.sub) (i32.const 17) (i32.add) (i32.const 18) (i32.mul) (i32.const 19) (i32.xor)
	    (i32.const 20) (i32.sub) (i32.const 21) (i32.add) (i32.const 22) (i32.mul) (i32.const 23) (i32.xor))
	  (func (export "small") (result i32) (i32.const 1)))`)
	ext, _ := inst.Export("big")
	_, err := Invoke(context.Background(), ext.Func, []Val{{N: 3}})
	if !errors.Is(err, ErrFunctionTooLarge) {
		t.Fatalf("big = %v, want ErrFunctionTooLarge", err)
	}
	if got, err := call(t, inst, "small"); err != nil || got[0].N != 1 {
		t.Fatalf("small = %v, %v", got, err)
	}
}
