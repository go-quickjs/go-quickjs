package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/go-quickjs/go-quickjs/wasm/internal/decoder"
	"github.com/go-quickjs/go-quickjs/wasm/wat"
)

func instantiateWAT(t *testing.T, src string) (*Instance, error) {
	t.Helper()
	b, err := wat.Compile([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	m, derr := decoder.Decode(b, decoder.CoreLimits())
	if derr != nil {
		t.Fatal(derr)
	}
	if derr := decoder.ValidateFunctions(m, decoder.CoreLimits()); derr != nil {
		t.Fatal(derr)
	}
	return Instantiate(context.Background(), NewCode(m), nil)
}

// TestCallIndirectNullEntry pins which trap a null table entry gives: a
// funcref table's call checks its callee's type, and a null fails that
// check; a table of the call's own type needs no check, and its null is a
// null function. Node 26 gives the same.
func TestCallIndirectNullEntry(t *testing.T) {
	inst, err := instantiateWAT(t, `(module
		(type $t (func))
		(table $a 1 funcref)
		(table $b 1 (ref null $t))
		(func (export "untyped") (call_indirect $a (type $t) (i32.const 0)))
		(func (export "typed") (call_indirect $b (type $t) (i32.const 0))))`)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"untyped": "function signature mismatch",
		"typed":   "null function",
	} {
		ext, _ := inst.Export(name)
		_, err := Invoke(context.Background(), ext.Func, nil)
		var tr *Trap
		if !errors.As(err, &tr) || tr.Error() != want {
			t.Errorf("%s: got %v, want trap %q", name, err, want)
		}
	}
}

// TestGrowByNothingTellsHost checks that memory.grow 0 tells a host keeping
// views of the memory, which a grow of any size refreshes.
func TestGrowByNothingTellsHost(t *testing.T) {
	inst, err := instantiateWAT(t, `(module (memory 1)
		(func (export "g") (result i32) (memory.grow (i32.const 0))))`)
	if err != nil {
		t.Fatal(err)
	}
	called := 0
	inst.Memories[0].OnGrow = func(o, n uint64) {
		called++
		if o != PageSize || n != PageSize {
			t.Errorf("OnGrow(%d, %d)", o, n)
		}
	}
	ext, _ := inst.Export("g")
	if res, err := Invoke(context.Background(), ext.Func, nil); err != nil || res[0].N != 1 {
		t.Fatal(res, err)
	}
	if called != 1 {
		t.Fatalf("OnGrow called %d times, want 1", called)
	}
}

// TestConstantExpressionTrap checks that a constant expression that traps
// makes Instantiate return the trap rather than panic.
func TestConstantExpressionTrap(t *testing.T) {
	_, err := instantiateWAT(t, `(module (type $a (array i8))
		(global (ref $a) (array.new_default $a (i32.const -1))))`)
	var tr *Trap
	if !errors.As(err, &tr) || tr.Code != TrapArrayTooLarge {
		t.Fatalf("got %v, want the array-too-large trap", err)
	}
}

// TestTailCallReturnFromDepth pins where a function a return_call reached
// in the interpreter leaves its results when it returns from deep in its
// operand stack: where the return put them, not copied over by the
// epilogue from its stack's bottom positions.
func TestTailCallReturnFromDepth(t *testing.T) {
	inst, err := instantiateWAT(t, `(module
  (func (export "f") (param i32) (result i32)
    (return_call $g (local.get 0)))
  (func $g (param i32) (result i32)
    (i32.const 1) (i32.const 2) (i32.add (local.get 0) (i32.const 40)) (return)))`)
	if err != nil {
		t.Fatal(err)
	}
	ext, _ := inst.Export("f")
	res, err := Invoke(context.Background(), ext.Func, []Val{{N: 2}})
	if err != nil || res[0].N != 42 {
		t.Fatalf("f(2) = %v, %v; want 42", res, err)
	}
}
