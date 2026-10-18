package ssa

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

var testValues = []ir.Value{
	ir.Float(0), ir.Float(math.Copysign(0, -1)), ir.Float(1), ir.Float(-1), ir.Float(0.5), ir.Float(3),
	ir.Float(1 << 31), ir.Float(1<<32 + 1), ir.Float(-(1 << 31) - 1), ir.Float(math.NaN()),
	{Kind: ir.Number, Bits: 0x7ff0000000004567}, ir.Float(math.Inf(1)), ir.Float(math.Inf(-1)),
	ir.Float(5e-324), ir.Float(1e300), ir.Float(9007199254740993),
	ir.Bool(true), ir.Bool(false), {Kind: ir.Undefined}, {Kind: ir.Null}, {Kind: ir.Uninitialized},
	{Kind: ir.Opaque, Bits: 3}, {Kind: ir.String, Bits: 4},
	{Kind: ir.Opaque, Bits: 0}, {Kind: ir.Opaque, Bits: 1}, {Kind: ir.Opaque, Bits: 2},
}

func randomValue(r *rand.Rand) ir.Value {
	if r.IntN(3) == 0 {
		return ir.Float(float64(r.IntN(20) - 5))
	}
	return testValues[r.IntN(len(testValues))]
}

// randomProgram makes a program over locals from the operations Build
// translates; the caller keeps those Validate accepts.
func randomProgram(r *rand.Rand) *ir.Program {
	locals := 1 + r.IntN(6)
	n := 2 + r.IntN(24)
	operand := func() ir.Operand {
		if r.IntN(4) == 0 {
			return ir.Literal(randomValue(r))
		}
		return ir.Slot(r.IntN(locals))
	}
	ops := []ir.Op{ir.Copy, ir.Binary, ir.Binary, ir.Binary, ir.Unary, ir.Update, ir.Update, ir.Branch, ir.Branch,
		ir.Jump, ir.Swap, ir.CopyPair, ir.StoreLoad, ir.Host, ir.Nop,
		ir.ArrayRead, ir.ArrayWrite, ir.ArrayLength, ir.ArrayKey, ir.ArrayUpdate}
	operators := []ir.Operator{ir.Add, ir.Sub, ir.Mul, ir.Div, ir.Lt, ir.Le, ir.Gt, ir.Ge, ir.Eq, ir.Ne,
		ir.BitAnd, ir.BitOr, ir.BitXor, ir.Shl, ir.Shr, ir.UShr}
	p := &ir.Program{Locals: locals}
	for pc := 0; pc < n; pc++ {
		in := ir.Instruction{Op: ops[r.IntN(len(ops))], Left: operand(), Right: operand(), Third: operand(),
			Dest: r.IntN(locals), Extra: r.IntN(locals), Target: r.IntN(n + 1), Postfix: r.IntN(2) == 0, When: r.IntN(2) == 0}
		switch in.Op {
		case ir.ArrayRead, ir.ArrayWrite, ir.ArrayLength, ir.ArrayKey:
			in.Left = ir.Slot(r.IntN(locals))
		case ir.ArrayUpdate:
			in.Left = ir.Slot(r.IntN(locals))
			in.Right = ir.Slot(in.Extra)
			in.Operator = []ir.Operator{ir.Add, ir.Sub}[r.IntN(2)]
		case ir.Binary:
			in.Operator = operators[r.IntN(len(operators))]
		case ir.Unary:
			in.Operator = []ir.Operator{ir.Neg, ir.Pos, ir.Not, ir.Int32, ir.BitNot}[r.IntN(5)]
		case ir.Update:
			in.Operator = []ir.Operator{ir.Add, ir.Sub}[r.IntN(2)]
			if r.IntN(2) == 0 {
				in.Extra = -1
			}
		case ir.Branch:
			in.Operator = []ir.Operator{ir.Truth, ir.Lt, ir.Le, ir.Gt, ir.Ge, ir.Eq, ir.Ne}[r.IntN(7)]
		}
		if r.IntN(8) == 0 {
			in.Check, in.CheckSlot = true, r.IntN(locals)
		}
		p.Code = append(p.Code, in)
	}
	p.Code = append(p.Code, ir.Instruction{Op: ir.Return, Left: operand()})
	p.Maps = make([]ir.StateMap, len(p.Code))
	for pc := range p.Maps {
		p.Maps[pc].PC = uint32(pc)
	}
	return p
}

// beyondSlotIR reports an exit of the slot IR at an operation SSA does
// natively where the slot IR leaves it to Go or the interpreter: an
// equality, which it compares by the values' words; a truth test, of an
// object or a string too; a property write, of any value.
func beyondSlotIR(p *ir.Program, at ir.StateMap) bool {
	for pc, m := range p.Maps {
		if m != at {
			continue
		}
		switch in := p.Code[pc]; {
		case (in.Op == ir.Binary || in.Op == ir.Branch) && (in.Operator == ir.Eq || in.Operator == ir.Ne),
			in.Op == ir.Branch && in.Operator == ir.Truth, in.Op == ir.Unary && in.Operator == ir.Not,
			in.Op == ir.PropertyWrite:
			return true
		}
	}
	return false
}

// compare runs p's slot IR evaluator and f's SSA evaluator from an entry and
// requires the same exit and live slots. It returns false when the slot IR
// does not finish within its budget, which the test skips.
func compare(t *testing.T, p *ir.Program, f *Func, pc int, slots []ir.Value, poll int, heap testHeap) bool {
	t.Helper()
	x := append([]ir.Value(nil), slots...)
	hx, hy := heap.instance(), heap.instance()
	want, err := p.EvaluateArrays(x, hx.views, pc, 20000)
	if err != nil {
		t.Fatalf("slot IR: %v", err)
	}
	if want.Kind == ir.BudgetExit {
		return false
	}
	if (want.Kind == ir.HostExit || want.Kind == ir.GuardExit) && beyondSlotIR(p, want.State) {
		// SSA goes on where the slot IR leaves the operation to Go, and the
		// slot IR has nothing to say about what follows. The native
		// harness and the VM's tests check those operations.
		return false
	}
	y := append([]ir.Value(nil), slots...)
	var got ir.Exit
	entry := pc
	for round := 0; ; round++ {
		got, err = EvaluateArrays(f, entry, y, hy.views, poll)
		if errors.Is(err, ErrUnsupported) {
			// A store SSA makes where the slot IR leaves it to Go, of a
			// reference into an element, which the model does not make:
			// the VM's tests check it.
			return false
		}
		if err != nil {
			t.Fatalf("ssa from pc %d: %v\n%s", entry, err, f)
		}
		if got.Kind == ir.GuardExit {
			// The interpreter takes over, as in the VM: here, the slot IR.
			got, err = p.EvaluateArrays(y, hy.views, int(got.State.PC), 20000)
			if err != nil {
				t.Fatalf("slot IR after a guard: %v", err)
			}
			if got.Kind == ir.BudgetExit {
				return false
			}
			break
		}
		if got.Kind != ir.BudgetExit {
			break
		}
		if round > 30000 {
			t.Fatalf("ssa polled forever where the slot IR finished\n%s", f)
		}
		entry = int(got.State.PC)
	}
	same := func(a, b ir.Value) bool { return a == b }
	bad := got.Kind != want.Kind || got.Kind == ir.Returned && !same(got.Value, want.Value) ||
		got.Kind != ir.Returned && got.State != want.State
	if !bad && got.Kind != ir.Returned {
		for i := 0; i < p.Locals+want.State.Depth; i++ {
			if !same(x[i], y[i]) {
				bad = true
			}
		}
	}
	if !bad && !hx.same(hy) {
		bad = true
	}
	if bad {
		t.Fatalf("from pc %d, poll %d, slots %v, heap %v:\nslot IR %+v slots %v heap %v\nssa     %+v slots %v heap %v\n%s",
			pc, poll, slots, heap, want, x, hx.cells, got, y, hy.cells, f)
	}
	return true
}

// optimizeAll makes checkProgram run every function through Optimize.
var optimizeAll bool

// knowsNothing is Feedback that knows nothing, as none does, but is
// Feedback all the same: half the programs are built so.
type knowsNothing struct{}

func (knowsNothing) Property(int) (PropertySite, bool) { return PropertySite{}, false }
func (knowsNothing) Global(int) (GlobalSite, bool)     { return GlobalSite{}, false }
func (knowsNothing) Generic(int) bool                  { return false }
func (knowsNothing) EntryGeneric(int) bool             { return false }
func (knowsNothing) Inline(int) (InlineSite, bool)     { return InlineSite{}, false }
func (knowsNothing) NativeCalls(int) []CallSite        { return nil }

func checkProgram(t *testing.T, r *rand.Rand, p *ir.Program) (*Func, int) {
	t.Helper()
	f, err := Build(p)
	if len(p.Code)%2 == 0 {
		f, err = BuildWith(p, knowsNothing{})
	}
	if errors.Is(err, ErrUnsupported) {
		return nil, 0
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := Check(f); err != nil {
		t.Fatalf("built: %v\n%s", err, f)
	}
	if optimizeAll {
		Optimize(f)
		if err := Check(f); err != nil {
			t.Fatalf("optimized: %v\n%s", err, f)
		}
	}
	finished := 0
	for _, e := range f.Entries {
		for trial := 0; trial < 6; trial++ {
			slots := make([]ir.Value, p.Locals+p.StackSize)
			for i := range slots {
				slots[i] = randomValue(r)
			}
			heap := randomHeap(r)
			for _, poll := range []int{0, 1, 3} {
				if compare(t, p, f, e.PC, slots, poll, heap) {
					finished++
				}
			}
		}
	}
	return f, finished
}

func TestBuildMatchesSlotIR(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	programs, finished, loops := 0, 0, 0
	for attempt := 0; attempt < 100000 && programs < 3000; attempt++ {
		p := randomProgram(r)
		if p.Validate() != nil {
			continue
		}
		f, n := checkProgram(t, r, p)
		if f == nil {
			t.Fatalf("Build refused a program of supported operations:\n%+v", p.Code)
		}
		programs++
		finished += n
		for _, b := range f.Blocks {
			if b.LoopHeader {
				loops++
				break
			}
		}
	}
	t.Logf("%d programs, %d with loops, %d finished comparisons", programs, loops, finished)
	if programs < 1000 || loops < 100 || finished < 10000 {
		t.Fatalf("too little coverage: %d programs, %d loops, %d comparisons", programs, loops, finished)
	}
}

// Functions lowered from JavaScript bring the operand stack, stack
// shuffles and fused instructions the random programs lack.
var jsCorpus = []string{
	`function f(n){let s=0;for(let i=0;i<n;i++)s+=i*0.5-i/3;return s}`,
	`function f(n){var x=0.1;for(var i=0;i<n;i++)x=3.7*x*(1-x);return x}`,
	`function f(n){let h=0;for(let i=0;i<n;i++){h=(h<<5)-h+i|0;h^=h>>>13;h&=0x7fffffff;h|=1;h=~h;h>>=1}return h}`,
	`function f(n){let s=0;for(let i=0;i<n;i++){if(s>1e6)break;s+=i<10?i:-i}return s}`,
	`function f(n){let x=-0,y=NaN,z=Infinity;for(let i=0;i<n;i++){x=x*-1;y=y===y?0:1;z=1/z}return x+y+z}`,
	`function f(a,b){let c=0;while(a>0){c+=a&b;a=a>>1;b=b<<1|0}return c}`,
	`function f(n){let a=1,b=1;for(let i=2;i<n;i++){let t=a+b;a=b;b=t}return b}`,
	`function f(x){let n=0;do{x=x%2?3*x+1:x/2;n++}while(x!==1&&n<1000);return n}`,
	`function f(n,m){let s=0;for(let i=0;i<n;i++)for(let j=0;j<m;j++)s+=i^j;return s}`,
	`function f(x){return !x?-x:+x}`,
	`function f(x,y){let t;if(x<y)t=x;else t=y;return t}`,
	`function f(n){let s=0;for(let i=n;i--;)s+=i;return s}`,
}

func TestBuildMatchesSlotIRFromJavaScript(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	built := 0
	for _, src := range jsCorpus {
		prog, err := parser.Parse(src, parser.Options{})
		if err != nil {
			t.Fatal(err)
		}
		top, err := compiler.Compile(prog, compiler.Options{})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range top.Constants {
			if c.Kind != bytecode.ConstFunction {
				continue
			}
			p, err := jitcompile.Lower(c.Fn)
			if err != nil {
				continue
			}
			f, _ := checkProgram(t, r, p)
			if f != nil {
				built++
			}
		}
	}
	if built < len(jsCorpus)-2 {
		t.Fatalf("only %d of %d functions built", built, len(jsCorpus))
	}
}

// TestBuildEveryOperation pins the builder's coverage: every slot IR operation
// builds, each in a program of its own followed by a return.
func TestBuildEveryOperation(t *testing.T) {
	for op := ir.Nop; op <= ir.StringCode; op++ {
		in := ir.Instruction{Op: op, Left: ir.Slot(0), Right: ir.Slot(1), Third: ir.Slot(2), Dest: 3, Extra: 2, Target: 1}
		switch op {
		case ir.ArrayUpdate:
			in.Right = ir.Slot(in.Extra)
		case ir.Unary:
			in.Operator = ir.Neg
		case ir.Branch:
			in.Operator = ir.Lt
		case ir.BindingRead:
			in.Left = ir.Literal(ir.Value{Kind: ir.Undefined})
		case ir.Insert2, ir.Insert3:
			in.Dest = 0
		}
		p := &ir.Program{Locals: 8, Code: []ir.Instruction{in, {Op: ir.Return, Left: ir.Slot(3)}},
			Maps: []ir.StateMap{{PC: 0}, {PC: 1}}}
		if err := p.Validate(); err != nil {
			t.Fatalf("op %d: the test's program is invalid: %v", op, err)
		}
		if _, err := Build(p); err != nil {
			t.Errorf("op %d: %v", op, err)
		}
	}
}

func ExampleFunc_String() {
	p := &ir.Program{Locals: 2, Code: []ir.Instruction{
		{Op: ir.Binary, Operator: ir.Add, Left: ir.Slot(0), Right: ir.Literal(ir.Float(1)), Dest: 1},
		{Op: ir.Return, Left: ir.Slot(1)},
	}, Maps: []ir.StateMap{{PC: 0}, {PC: 1}}}
	f, _ := Build(p)
	fmt.Print(f)
}

func count(f *Func, pred func(*Value) bool) int {
	n := 0
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if pred(v) {
				n++
			}
		}
	}
	return n
}

// Optimized functions still match the slot IR, and the passes do their
// work: no unboxing of a box survives, and the functions shrink.
func TestOptimizeMatchesSlotIR(t *testing.T) {
	optimizeAll = true
	defer func() { optimizeAll = false }()
	TestBuildMatchesSlotIR(t)
	TestBuildMatchesSlotIRFromJavaScript(t)

	r := rand.New(rand.NewPCG(7, 8))
	before, after := 0, 0
	for attempt, programs := 0, 0; attempt < 20000 && programs < 500; attempt++ {
		p := randomProgram(r)
		if p.Validate() != nil {
			continue
		}
		programs++
		f, err := Build(p)
		if err != nil {
			t.Fatal(err)
		}
		before += count(f, func(*Value) bool { return true })
		Optimize(f)
		after += count(f, func(*Value) bool { return true })
		if n := count(f, func(v *Value) bool { return v.Op == OpUnboxF64 && v.Args[0].Op == OpBoxF64 }); n != 0 {
			t.Fatalf("%d unboxings of a box survived\n%s", n, f)
		}
		if n := count(f, func(v *Value) bool { return v.Op == OpPhi && len(v.Args) > 0 && trivial(v) }); n != 0 {
			t.Fatalf("%d trivial phis survived\n%s", n, f)
		}
	}
	t.Logf("values: %d built, %d after Optimize", before, after)
	if after >= before {
		t.Fatalf("Optimize removed nothing: %d values before, %d after", before, after)
	}
}

func trivial(phi *Value) bool {
	var same *Value
	for _, a := range phi.Args {
		if a == phi || a == same {
			continue
		}
		if same != nil {
			return false
		}
		same = a
	}
	return true
}

// The evaluator's limits are the native code's (abi).
func TestLimitsMatchABI(t *testing.T) {
	if maxScan != abi.MaxScan || maxEqualUnits != abi.MaxEqualUnits {
		t.Fatalf("maxScan %d, maxEqualUnits %d; abi's %d, %d", maxScan, maxEqualUnits, abi.MaxScan, abi.MaxEqualUnits)
	}
}
