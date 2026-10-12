package vm

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
)

func jitFunctionForTest(t *testing.T, source string) *bytecode.Function {
	t.Helper()
	for _, c := range compileForTest(t, source).Constants {
		if c.Kind == bytecode.ConstFunction {
			return c.Fn
		}
	}
	t.Fatal("missing function")
	return nil
}

// Independent test adapters verify publication at every interpreter boundary.
// References stay in a Go root slice; scratch holds only indices.
func jitEncodeForTest(v Value, roots *[]Value) ir.Value {
	switch v.Kind() {
	case KindNumber:
		return ir.Float(v.Number())
	case KindBool:
		return ir.Bool(v.Truthy())
	case KindUndefined:
		return ir.Value{Kind: ir.Undefined}
	case KindNull:
		return ir.Value{Kind: ir.Null}
	case KindUninitialized:
		return ir.Value{Kind: ir.Uninitialized}
	}
	*roots = append(*roots, v)
	return ir.Value{Kind: ir.Opaque, Bits: uint64(len(*roots) - 1)}
}

func jitDecodeForTest(v ir.Value, roots []Value) Value {
	switch v.Kind {
	case ir.Number:
		return Float(math.Float64frombits(v.Bits))
	case ir.Boolean:
		return Bool(v.Bits != 0)
	case ir.Undefined:
		return Undefined
	case ir.Null:
		return Null
	case ir.Uninitialized:
		return uninitialized
	case ir.Opaque:
		return roots[v.Bits]
	}
	panic("invalid JIT scalar")
}

func jitInitialForTest(p *ir.Program, args []Value) ([]ir.Value, []Value) {
	slots := make([]ir.Value, p.Locals+p.StackSize)
	var roots []Value
	for i, v := range args {
		slots[i] = jitEncodeForTest(v, &roots)
	}
	return slots, roots
}

func jitResumeForTest(r *Runtime, fn *bytecode.Function, slots []ir.Value, roots []Value, state ir.StateMap) (Value, error) {
	base := r.stackTop
	need := fn.LocalCount + fn.MaxStack
	r.stackTop += need
	r.stackHigh = max(r.stackHigh, r.stackTop)
	f := r.pushFrame()
	*f = frame{cl: r.prepare(fn), base: base + fn.LocalCount, pc: state.PC, locals: r.stack[base : base+fn.LocalCount], this: Undefined, newTarget: Undefined}
	defer r.popFrameOf(f, base)
	for i := 0; i < fn.LocalCount+state.Depth; i++ {
		r.stack[base+i] = jitDecodeForTest(slots[i], roots)
	}
	return r.executeAt(f, f.base+state.Depth, nil)
}

func jitSameValueForTest(a, b Value) bool {
	if a.IsNumber() && b.IsNumber() {
		return math.Float64bits(a.Number()) == math.Float64bits(b.Number())
	}
	return a.StrictEquals(b)
}

// jitEvaluatorForTest runs a lowered program through the slot IR's own
// evaluator, the oracle the new pipeline is compared with: the tests here
// check that every state it stops in is one the interpreter goes on from.
func jitEvaluatorForTest(t *testing.T, p *ir.Program) func([]ir.Value, int, uint64) (ir.Exit, error) {
	t.Helper()
	return p.Evaluate
}

// Every budget up to the completed execution stops at an instruction boundary
// and hands real locals and operand slots to executeAt. This detects replayed
// writes, wrong branch depths, and confusion between fetched and resume PCs.
func TestJITStateEveryBoundary(t *testing.T) {
	minusZero := Float(math.Copysign(0, -1))
	for _, tc := range []struct {
		name, source string
		args         []Value
		want         Value
	}{
		{"loop", `function f(n) { let s=0; for(let i=0;i<n;i++) s+=i; return s }`, []Value{Int32(5)}, Int32(10)},
		{"nested", `function f(n) { let s=0; for(let i=0;i<n;i++) for(let j=0;j<2;j++) s+=i*j; return s }`, []Value{Int32(3)}, Int32(3)},
		{"branch", `function f(a,b) { let x; if(a<b) x=a+b; else x=a-b; return x }`, []Value{Int32(2), Int32(3)}, Int32(5)},
		{"else", `function f(a,b) { let x; if(a<b) x=a+b; else x=a-b; return x }`, []Value{Int32(4), Int32(3)}, Int32(1)},
		{"postfix", `function f(a) { let b=a++; return b+a }`, []Value{Int32(2)}, Int32(5)},
		{"prefix", `function f(a) { let b=--a; return b+a }`, []Value{Int32(2)}, Int32(2)},
		{"multiply", `function f(a,b) { return (a*b)/b }`, []Value{Float(0.5), Float(3)}, Float(0.5)},
		{"immediate", `function f(a) { return (a+1)*3-2 }`, []Value{Int32(4)}, Int32(13)},
		{"minus-zero", `function f(a) { return a*2 }`, []Value{minusZero}, minusZero},
		{"negate-zero", `function f(a) { return -a }`, []Value{Int32(0)}, minusZero},
		{"subnormal", `function f(a) { return a/2 }`, []Value{Float(2 * math.SmallestNonzeroFloat64)}, Float(math.SmallestNonzeroFloat64)},
		{"overflow", `function f(a) { return a*2 }`, []Value{Float(math.MaxFloat64)}, Float(math.Inf(1))},
		{"nan", `function f(a,b) { return a/b }`, []Value{Int32(0), Int32(0)}, Float(math.NaN())},
		{"nan-negate", `function f(a) { return -a }`, []Value{Float(math.NaN())}, Float(math.NaN())},
		{"nan-cmp", `function f(a,b) { return a<b }`, []Value{Float(math.NaN()), Int32(0)}, False},
		{"nan-ne", `function f(a,b) { return a!==b }`, []Value{Float(math.NaN()), Int32(0)}, True},
		{"zero-eq", `function f(a,b) { return a===b }`, []Value{minusZero, Int32(0)}, True},
		{"and-taken", `function f(a,b) { return a && b }`, []Value{False, Int32(8)}, False},
		{"and-fallthrough", `function f(a,b) { return a && b }`, []Value{True, Int32(8)}, Int32(8)},
		{"or-taken", `function f(a,b) { return a || b }`, []Value{Int32(8), Int32(9)}, Int32(8)},
		{"or-fallthrough", `function f(a,b) { return a || b }`, []Value{minusZero, Int32(9)}, Int32(9)},
		{"not-nan", `function f(a) { return !a }`, []Value{Float(math.NaN())}, True},
		{"undefined", `function f(a) { if(a) return 1 }`, nil, Undefined},
		{"null", `function f() { return null }`, nil, Null},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fn := jitFunctionForTest(t, tc.source)
			p, err := jitcompile.Lower(fn)
			if err != nil {
				t.Fatalf("%v\n%s", err, fn.Disassemble())
			}
			evaluate := jitEvaluatorForTest(t, p)
			r := New(Config{})
			defer r.Close()
			slots, roots := jitInitialForTest(p, tc.args)
			baseline, err := jitResumeForTest(r, fn, slots, roots, p.Maps[0])
			if err != nil || !jitSameValueForTest(baseline, tc.want) {
				t.Fatalf("interpreter = %v, %v; want %v", baseline, err, tc.want)
			}
			full, err := evaluate(slots, 0, 10000)
			if err != nil || full.Kind != ir.Returned || !jitSameValueForTest(jitDecodeForTest(full.Value, roots), tc.want) {
				t.Fatalf("IR = %+v, %v; want %v", full, err, tc.want)
			}
			for budget := uint64(0); budget <= full.Steps; budget++ {
				slots, roots := jitInitialForTest(p, tc.args)
				exit, err := evaluate(slots, 0, budget)
				if err != nil {
					t.Fatal(err)
				}
				got := jitDecodeForTest(exit.Value, roots)
				if exit.Kind == ir.BudgetExit {
					got, err = jitResumeForTest(r, fn, slots, roots, exit.State)
				} else if exit.Kind != ir.Returned {
					t.Fatalf("unexpected guard at budget %d: %+v", budget, exit)
				}
				if err != nil || !jitSameValueForTest(got, tc.want) {
					t.Fatalf("budget %d, state %+v: got %v, %v; want %v", budget, exit.State, got, err, tc.want)
				}
			}
		})
	}
}

func TestJITGuardPreservesEffects(t *testing.T) {
	fn := jitFunctionForTest(t, `function f(a) { let s=0; for(let i=0;i<3;i++) { s++; s+=a } return s }`)
	p, err := jitcompile.Lower(fn)
	if err != nil {
		t.Fatal(err)
	}
	evaluate := jitEvaluatorForTest(t, p)
	r := New(Config{})
	defer r.Close()
	conversions := 0
	o := r.NewObject()
	o.setOwnRaw(r.atoms.intern("valueOf"), r.NewFunction("valueOf", 0, func(*Runtime, Value, []Value) (Value, error) {
		conversions++
		return Int32(2), nil
	}), propDefault)
	slots, roots := jitInitialForTest(p, []Value{Obj(o)})
	exit, err := evaluate(slots, 0, 1000)
	if err != nil || exit.Kind != ir.GuardExit || conversions != 0 || slots[1] != ir.Float(1) {
		t.Fatalf("guard = %+v, %v; conversions %d, sum %+v", exit, err, conversions, slots[1])
	}
	// Re-entering the failing instruction must guard again without consuming
	// either operand, changing locals, or invoking valueOf.
	before := append([]ir.Value(nil), slots...)
	retry, err := evaluate(slots, int(exit.State.PC), 1000)
	if err != nil || retry.Kind != ir.GuardExit || retry.Steps != 0 || !reflect.DeepEqual(slots, before) {
		t.Fatalf("guard changed state: %+v, %v", retry, err)
	}
	got, err := jitResumeForTest(r, fn, slots, roots, exit.State)
	if err != nil || got.Number() != 9 || conversions != 3 {
		t.Fatalf("resumed result = %v, %v; conversions %d, want 9 and 3", got, err, conversions)
	}
}

func TestJITGuardErrors(t *testing.T) {
	for _, tc := range []struct {
		name, source, message string
		args                  []Value
	}{
		{"tdz", "function f() {\nlet a=a;\nreturn a\n}", "ReferenceError", nil},
		{"bigint", "function f(a) {\nlet s=1;\nreturn s+a\n}", "TypeError", []Value{shortBig(2)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fn := jitFunctionForTest(t, tc.source)
			p, err := jitcompile.Lower(fn)
			if err != nil {
				t.Fatal(err)
			}
			evaluate := jitEvaluatorForTest(t, p)
			r := New(Config{})
			defer r.Close()
			slots, roots := jitInitialForTest(p, tc.args)
			_, baselineErr := jitResumeForTest(r, fn, slots, roots, p.Maps[0])
			exit, err := evaluate(slots, 0, 1000)
			if err != nil || exit.Kind != ir.GuardExit {
				t.Fatalf("guard = %+v, %v", exit, err)
			}
			_, resumedErr := jitResumeForTest(r, fn, slots, roots, exit.State)
			var baseline, resumed *Thrown
			if !errors.As(baselineErr, &baseline) || !errors.As(resumedErr, &resumed) ||
				!strings.HasPrefix(resumed.Error(), tc.message+":") || baseline.Error() != resumed.Error() {
				t.Fatalf("baseline = %v, resumed = %v; state %+v", baselineErr, resumedErr, exit.State)
			}
			if len(resumed.trace.frames) != 1 || len(baseline.trace.frames) != 1 ||
				resumed.trace.frames[0].fn != fn || baseline.trace.frames[0].pc != resumed.trace.frames[0].pc ||
				resumed.trace.frames[0].pc != exit.State.PC {
				t.Fatalf("guard's source location lost: trace %+v, pc %d", resumed.trace.frames, exit.State.PC)
			}
		})
	}
}

func TestJITFusedInstructionExits(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		arg, want    Value
		op           bytecode.Op
		depth        int
		kind         ir.ExitKind
	}{
		{"local-immediate", `function f(a) { let b=0; b++; return (a+1)+b }`, Str(NewString("x")), Str(NewString("x11")), bytecode.OpLocalBinImm, 0, ir.GuardExit},
		{"binary-local", `function f(a) { let b=1; return b*2+a }`, Str(NewString("x")), Str(NewString("2x")), bytecode.OpBinLocal, 1, ir.GuardExit},
		{"postfix", `function f(a) { let b=0; b++; return a++ + b }`, Str(NewString("2")), Int32(3), bytecode.OpUpdateLocal, 0, ir.GuardExit},
		{"comparison-branch", `function f(a) { let b=0; b++; if(a<3) return b; return b+2 }`, Str(NewString("2")), Int32(1), bytecode.OpJumpIfCmpFalse, 2, ir.GuardExit},
		{"short-circuit", `function f(a) { return a && 7 }`, Str(NewString("")), Str(NewString("")), bytecode.OpJumpIfFalseKeep, 1, ir.GuardExit},
		{"boolean-equality", `function f(a) { return a===true }`, True, True, bytecode.OpStrictEq, 2, ir.HostExit},
		{"opaque-return", `function f(a) { let b=a; return b }`, Str(NewString("kept")), Str(NewString("kept")), bytecode.OpReturn, 1, ir.Returned},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fn := jitFunctionForTest(t, tc.source)
			p, err := jitcompile.Lower(fn)
			if err != nil {
				t.Fatal(err)
			}
			evaluate := jitEvaluatorForTest(t, p)
			slots, roots := jitInitialForTest(p, []Value{tc.arg})
			exit, err := evaluate(slots, 0, 1000)
			if err != nil || exit.Kind != tc.kind || exit.State.Depth != tc.depth || fn.Code[exit.State.PC].Op != tc.op {
				t.Fatalf("exit = %+v, %v; want kind %v at %s depth %d\n%s", exit, err, tc.kind, tc.op, tc.depth, fn.Disassemble())
			}
			r := New(Config{})
			defer r.Close()
			got := jitDecodeForTest(exit.Value, roots)
			if exit.Kind != ir.Returned {
				got, err = jitResumeForTest(r, fn, slots, roots, exit.State)
			}
			if err != nil || !jitSameValueForTest(got, tc.want) {
				t.Fatalf("resumed value = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

func TestJITIEEEOperators(t *testing.T) {
	values := []float64{
		math.Inf(-1), -math.MaxFloat64, -1, -math.SmallestNonzeroFloat64,
		math.Copysign(0, -1), 0, math.SmallestNonzeroFloat64, 1, math.MaxFloat64,
		math.Inf(1), math.NaN(),
	}
	for _, op := range []string{"+", "-", "*", "/", "<", "<=", ">", ">=", "==", "!=", "===", "!=="} {
		t.Run(op, func(t *testing.T) {
			fn := jitFunctionForTest(t, "function f(a,b) { return a "+op+" b }")
			p, err := jitcompile.Lower(fn)
			if err != nil {
				t.Fatal(err)
			}
			evaluate := jitEvaluatorForTest(t, p)
			r := New(Config{})
			defer r.Close()
			for _, x := range values {
				for _, y := range values {
					slots, roots := jitInitialForTest(p, []Value{Float(x), Float(y)})
					want, err := jitResumeForTest(r, fn, slots, roots, p.Maps[0])
					if err != nil {
						t.Fatal(err)
					}
					exit, err := evaluate(slots, 0, 100)
					if err != nil || exit.Kind != ir.Returned || !jitSameValueForTest(jitDecodeForTest(exit.Value, roots), want) {
						t.Fatalf("%g %s %g = %+v, %v; want %v", x, op, y, exit, err, want)
					}
				}
			}
		})
	}
}
