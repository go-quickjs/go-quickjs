package opt

import "testing"

// reads counts f's reads of globals and loads of memory.
func reads(f *Func) (globals, loads int) {
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			switch v.Op {
			case OpGlobalGet:
				globals++
			case OpLoad:
				loads++
			}
		}
	}
	return globals, loads
}

// A block of one predecessor knows what the predecessor knew at its end:
// the global it wrote and the word it stored, read again in an if's arms.
// A call, a store that may overlap, or a merge of two blocks leaves them
// unknown, and the reads stay.
func TestForwardChains(t *testing.T) {
	for _, tc := range []struct {
		name           string
		body           string
		globals, loads int
	}{
		{"chain", `(global.set $g (local.get 0))
			(i32.store offset=8 (local.get 1) (local.get 0))
			(if (result i32) (local.get 0)
				(then (i32.add (global.get $g) (i32.load offset=8 (local.get 1))))
				(else (i32.sub (global.get $g) (i32.load offset=8 (local.get 1)))))`, 0, 0},
		{"call", `(global.set $g (local.get 0))
			(i32.store offset=8 (local.get 1) (local.get 0))
			(if (result i32) (local.get 0)
				(then (call $f) (i32.add (global.get $g) (i32.load offset=8 (local.get 1))))
				(else (i32.const 0)))`, 1, 1},
		{"overlap", `(global.set $g (local.get 0))
			(i32.store offset=8 (local.get 1) (local.get 0))
			(if (result i32) (local.get 0)
				(then (i32.store8 offset=9 (local.get 1) (local.get 0))
					(i32.add (global.get $g) (i32.load offset=8 (local.get 1))))
				(else (i32.const 0)))`, 0, 1},
		{"merge", `(global.set $g (local.get 0))
			(i32.store offset=8 (local.get 1) (local.get 0))
			(if (local.get 0) (then (local.set 0 (i32.const 1))))
			(i32.add (global.get $g) (i32.load offset=8 (local.get 1)))`, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := `(module (memory 1) (global $g (mut i32) (i32.const 0))
				(func $f)
				(func (param i32 i32) (result i32) ` + tc.body + `))`
			f := buildText(t, src, 1)
			if g, l := reads(f); g != tc.globals || l != tc.loads {
				t.Errorf("%d reads of globals and %d loads, want %d and %d\n%v", g, l, tc.globals, tc.loads, f)
			}
		})
	}
}
