package engine

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/go-quickjs/go-quickjs/wasm/internal/decoder"
	"github.com/go-quickjs/go-quickjs/wasm/wat"
)

func budgetInstance(t *testing.T, src string, b *Budget) (*Instance, error) {
	t.Helper()
	bin, err := wat.Compile([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	m, derr := decoder.Decode(bin, decoder.CoreLimits())
	if derr != nil {
		t.Fatal(derr)
	}
	if derr := decoder.ValidateFunctions(m, decoder.CoreLimits()); derr != nil {
		t.Fatal(derr)
	}
	return InstantiateBudget(context.Background(), NewCode(m), nil, b)
}

func callExport(t *testing.T, inst *Instance, name string, args ...Val) ([]Val, error) {
	t.Helper()
	ext, ok := inst.Export(name)
	if !ok {
		t.Fatalf("no export %q", name)
	}
	return Invoke(context.Background(), ext.Func, args)
}

// TestBudgetMemory checks that memories are charged as they are made and
// grown: a grow past the budget returns -1, and a memory too large fails
// the instantiation.
func TestBudgetMemory(t *testing.T) {
	b := NewBudget(3 * PageSize)
	inst, err := budgetInstance(t, `(module (memory 2)
		(func (export "grow") (param i32) (result i32) (memory.grow (local.get 0))))`, b)
	if err != nil {
		t.Fatal(err)
	}
	if got := b.Used(); got != 2*PageSize {
		t.Fatalf("used %d, want two pages", got)
	}
	if res, err := callExport(t, inst, "grow", Val{N: 2}); err != nil || int32(res[0].N) != -1 {
		t.Fatalf("grow past the budget = %v, %v; want -1", res, err)
	}
	if res, err := callExport(t, inst, "grow", Val{N: 1}); err != nil || res[0].N != 2 {
		t.Fatalf("grow within the budget = %v, %v; want 2", res, err)
	}
	if _, err := budgetInstance(t, `(module (memory 4))`, NewBudget(3*PageSize)); err == nil {
		t.Fatal("a memory past the budget was made")
	}
}

// TestBudgetMemoryReleased checks that a memory gives back what it held once
// it is collected.
func TestBudgetMemoryReleased(t *testing.T) {
	b := NewBudget(4 * PageSize)
	if _, err := budgetInstance(t, `(module (memory 3))`, b); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for b.Used() != 0 && time.Now().Before(deadline) {
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}
	if got := b.Used(); got != 0 {
		t.Fatalf("used %d after the memory was collected", got)
	}
}

// TestBudgetTable checks that tables are charged and that a grow past the
// budget returns -1.
func TestBudgetTable(t *testing.T) {
	b := NewBudget(100 * refBytes)
	inst, err := budgetInstance(t, `(module (table 50 externref)
		(func (export "grow") (param i32) (result i32) (table.grow (ref.null extern) (local.get 0))))`, b)
	if err != nil {
		t.Fatal(err)
	}
	if res, err := callExport(t, inst, "grow", Val{N: 60}); err != nil || int32(res[0].N) != -1 {
		t.Fatalf("grow past the budget = %v, %v; want -1", res, err)
	}
	if res, err := callExport(t, inst, "grow", Val{N: 50}); err != nil || res[0].N != 50 {
		t.Fatalf("grow within the budget = %v, %v; want 50", res, err)
	}
}

// TestBudgetGC checks that GC objects let go of are not counted against the
// budget once it counts again, and that those still held are: a program that
// keeps what it allocates stops with ErrMemoryLimit, one that drops it runs.
func TestBudgetGC(t *testing.T) {
	const src = `(module
		(type $node (struct (field (ref null $node)) (field i64)))
		(global $keep (mut (ref null $node)) (ref.null $node))
		(func (export "churn") (param i32)
			(loop $l
				(drop (struct.new $node (ref.null $node) (i64.const 0)))
				(br_if $l (local.tee 0 (i32.sub (local.get 0) (i32.const 1))))))
		(func (export "hoard") (param i32)
			(loop $l
				(global.set $keep (struct.new $node (global.get $keep) (i64.const 0)))
				(br_if $l (local.tee 0 (i32.sub (local.get 0) (i32.const 1)))))))`
	b := NewBudget(1 << 20)
	inst, err := budgetInstance(t, src, b)
	if err != nil {
		t.Fatal(err)
	}
	// A million structs of 48 bytes, none kept: far past the limit in all,
	// never near it in what is held.
	if _, err := callExport(t, inst, "churn", Val{N: 1_000_000}); err != nil {
		t.Fatalf("churn: %v", err)
	}
	if _, err := callExport(t, inst, "hoard", Val{N: 1_000_000}); !errors.Is(err, ErrMemoryLimit) {
		t.Fatalf("hoard: got %v, want ErrMemoryLimit", err)
	}
}

// TestBudgetOperandsFirst checks that an allocation's operands run before it
// is charged, as they run before the instruction: with the budget full, a
// struct.new or array.new_fixed whose operand traps fails with the trap,
// not ErrMemoryLimit, interpreted and native alike.
func TestBudgetOperandsFirst(t *testing.T) {
	const src = `(module
		(type $node (struct (field (ref null $node)) (field i64)))
		(type $arr (array (mut i64)))
		(global $keep (mut (ref null $node)) (ref.null $node))
		(func (export "hoard") (param i32)
			(loop $l
				(global.set $keep (struct.new $node (global.get $keep) (i64.const 0)))
				(br_if $l (local.tee 0 (i32.sub (local.get 0) (i32.const 1))))))
		(func (export "struct") (param i64)
			(drop (struct.new $node (ref.null $node) (i64.div_s (i64.const 1) (local.get 0)))))
		(func (export "array") (param i64)
			(drop (array.new_fixed $arr 2 (i64.const 1) (i64.div_s (i64.const 1) (local.get 0))))))`
	b := NewBudget(512)
	inst, err := budgetInstance(t, src, b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := callExport(t, inst, "hoard", Val{N: 1000}); !errors.Is(err, ErrMemoryLimit) {
		t.Fatalf("hoard: got %v, want ErrMemoryLimit", err)
	}
	for _, name := range []string{"struct", "array"} {
		_, err := callExport(t, inst, name, Val{N: 0})
		var trap *Trap
		if !errors.As(err, &trap) || trap.Code != TrapDivByZero {
			t.Errorf("%s: got %v, want the operand's divide by zero", name, err)
		}
	}
}
