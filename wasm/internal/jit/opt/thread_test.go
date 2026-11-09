//go:build amd64

package opt

import "testing"

// TestThreadDispatch threads a dispatch loop: every back edge sets the
// dispatch local to a constant, so after threading the loop's header is
// entered from the function's entry alone, its switch reads the parameter,
// and the cases' loop is a loop of its own, at depth 1 by loopDepths.
func TestThreadDispatch(t *testing.T) {
	m := compileWat(t, `(module (func (export "run") (param $pc i32) (param $n i32) (result i32)
  (local $a i32) (local $b i32)
  (loop $L
    (block $exit (block $c1 (block $c0
      (br_table $c0 $c1 $exit (local.get $pc)))
      ;; case 0: a loop of its own, through the dispatch.
      (local.set $a (i32.add (local.get $a) (local.get $n)))
      (local.set $n (i32.sub (local.get $n) (i32.const 1)))
      (local.set $pc (i32.const 0))
      (br_if $L (local.get $n))
      (local.set $pc (i32.const 1))
      (br $L))
      ;; case 1: swap, then out.
      (local.set $b (local.get $a))
      (local.set $a (i32.const 7))
      (local.set $pc (i32.const 2))
      (br $L)))
  (i32.sub (local.get $a) (local.get $b))))`)
	f, err := Build(m, 0)
	if err != nil {
		t.Fatal(err)
	}
	Optimize(f, func(pass string) {
		if err := Verify(f); err != nil {
			t.Fatalf("after %s: %v\n%v", pass, err, f)
		}
	})
	var sw *Block
	for _, b := range f.Blocks {
		if b.Kind == BSwitch {
			sw = b
		}
	}
	if sw == nil || sw.Control.Op != OpParam {
		t.Fatalf("the dispatch still switches on its local:\n%v", f)
	}
	for _, p := range sw.Preds {
		if p != f.Entry {
			t.Fatalf("the dispatch is entered from %v:\n%v", p, f)
		}
	}
	deep := 0
	for _, b := range f.Blocks {
		if b.Depth > 0 {
			deep++
		}
	}
	if deep == 0 {
		t.Fatalf("no block in a loop:\n%v", f)
	}
}
