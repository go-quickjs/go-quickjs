package opt

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/wasm/internal/decoder"
	"github.com/go-quickjs/go-quickjs/wasm/wat"
)

// buildText builds function fn of a module in the text format, optimized.
func buildText(t *testing.T, src string, fn uint32) *Func {
	t.Helper()
	b, err := wat.Compile([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	m, derr := decoder.Decode(b, decoder.CoreLimits())
	if derr != nil {
		t.Fatal(derr)
	}
	f, err := Build(m, fn)
	if err != nil {
		t.Fatal(err)
	}
	Optimize(f, func(pass string) {
		if err := Verify(f); err != nil {
			t.Fatalf("after %s: %v\n%v", pass, err, f)
		}
	})
	return f
}

// globalReads counts the reads of globals in the loops' blocks and out of
// them.
func globalReads(f *Func) (inLoops, outside int) {
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Op == OpGlobalGet {
				if b.Depth > 0 {
					inLoops++
				} else {
					outside++
				}
			}
		}
	}
	return inLoops, outside
}

// A loop that makes no call carries the globals it reads and writes, each
// read once before it; a loop that calls reads them on each turn; one that
// only reads a global reads it once before it.
func TestForwardGlobals(t *testing.T) {
	for _, tc := range []struct {
		name          string
		body          string
		loops, before int
	}{
		{"carried", `(loop $l
			(global.set $a (i32.add (global.get $a) (i32.const 1)))
			(global.set $x (f64.add (f64.mul (global.get $x) (f64.const 0.5)) (f64.const 1)))
			(br_if $l (i32.lt_u (global.get $a) (local.get $n))))`, 0, 2},
		{"read only", `(loop $l
			(local.set $n (i32.sub (local.get $n) (global.get $a)))
			(br_if $l (i32.gt_s (local.get $n) (i32.const 0))))`, 0, 1},
		{"call", `(loop $l
			(global.set $a (i32.add (global.get $a) (i32.const 1)))
			(call $f)
			(br_if $l (i32.lt_u (global.get $a) (local.get $n))))`, 2, 0},
		{"branches", `(loop $l
			(if (i32.and (local.get $n) (i32.const 1))
				(then (global.set $a (i32.add (global.get $a) (i32.const 3))))
				(else (global.set $a (i32.mul (global.get $a) (i32.const 5)))))
			(local.set $n (i32.sub (local.get $n) (i32.const 1)))
			(br_if $l (i32.ne (global.get $a) (local.get $n))))`, 0, 1},
		{"after", `(loop $l
			(local.set $n (i32.sub (local.get $n) (i32.const 1)))
			(br_if $l (local.get $n)))
			(global.set $a (global.get $a))`, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := `(module (global $a (mut i32) (i32.const 0)) (global $x (mut f64) (f64.const 0))
				(func $f (global.set $a (i32.const 7)))
				(func (param $n i32) ` + tc.body + `))`
			f := buildText(t, src, 1)
			if in, out := globalReads(f); in != tc.loops || out != tc.before {
				t.Errorf("%d reads in the loop and %d out of it, want %d and %d\n%v", in, out, tc.loops, tc.before, f)
			}
		})
	}
}
