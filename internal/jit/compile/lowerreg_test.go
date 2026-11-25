package compile

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

// regCompiled compiles source with the register backend on and returns
// its functions, each with its register code, by name.
func regCompiled(t *testing.T, source string) map[string]*bytecode.Function {
	t.Helper()
	prog, err := parser.Parse(source, parser.Options{})
	if err != nil {
		t.Fatal(err)
	}
	top, err := compiler.Compile(prog, compiler.Options{Text: source})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*bytecode.Function{}
	var walk func(fn *bytecode.Function)
	walk = func(fn *bytecode.Function) {
		for _, c := range fn.Constants {
			if c.Kind == bytecode.ConstFunction && c.Fn != nil {
				if c.Fn.Reg == nil {
					t.Fatalf("%s: no register code", c.Fn.Name)
				}
				out[c.Fn.Name] = c.Fn
				walk(c.Fn)
			}
		}
	}
	walk(top)
	return out
}

// regLowered is fn's register code lowered and validated.
func regLowered(t *testing.T, fn *bytecode.Function) *ir.Program {
	t.Helper()
	p, err := LowerRegInline(fn.RegCode(), fn)
	if err != nil {
		t.Fatalf("%s: %v\n%s", fn.Name, err, fn.RegCode())
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("%s: %v\n%s", fn.Name, err, fn.RegCode())
	}
	return p
}

// regEvaluate runs p, fn's, with args, as a register frame has them: r0
// the function and r1 this, handles of nothing, the parameters from r2,
// every other register undefined.
func regEvaluate(p *ir.Program, fn *bytecode.Function, args ...float64) (ir.Exit, error) {
	slots := make([]ir.Value, p.Locals)
	slots[0] = ir.Value{Kind: ir.Opaque}
	slots[1] = ir.Value{Kind: ir.Opaque, Bits: 1}
	for i := range p.Params {
		if i < len(args) {
			slots[p.ParamBase+i] = ir.Float(args[i])
		}
	}
	for i := p.UpvalueBase; i < p.UpvalueBase+p.Upvalues; i++ {
		slots[i] = ir.Value{Kind: ir.Opaque, Bits: 2}
	}
	return p.Evaluate(slots, 0, 1<<20)
}

// TestLowerRegEvaluates lowers register code of loops over numbers, its
// jumps, compare-and-branches, switches, updates and immediates, and runs
// it with the IR's evaluator to the answer the language gives.
func TestLowerRegEvaluates(t *testing.T) {
	fns := regCompiled(t, `
function sum(n) { let s = 0; for (let i = 0; i < n; i++) s += i; return s }
function bits(a, b) { let x = a ^ b; x = (x << 3) | (x >>> 1); x &= 0xffff; return (x >> 2) + (~x & 7) + (a | 0) }
function collatz(n) { let k = 0; while (n !== 1) { n = n % 2 === 0 ? n / 2 : 3 * n + 1; k++ } return k }
function sw(x) { switch (x) { case 1: return 10; case 2: case 3: return 20; case -0.5: return 30; default: return 40 } }
function nested(n) { let c = 0; for (let i = 0; i < n; i++) for (let j = i; j >= 0; j--) { if (j == 3) continue; if (i > 7) break; c += j * i } return c }
function post(n) { let i = 0, s = 0; while (i < n) { s += i++ * 2; s -= --i; i += 2 } return s }
function cmp(a, b) { let r = 0; if (a < b) r += 1; if (a <= b) r += 2; if (!(a > b)) r += 4; if (a >= b) r += 8; if (a != b) r += 16; if (a === b) r += 32; return r }
function neg(x) { return -x * +x - (x ? 1 : 2) + (x == null ? 100 : 0) }
function pow(a, b) { return a % b - b % a }
`)
	sum := func(n int) float64 {
		s := 0
		for i := range n {
			s += i
		}
		return float64(s)
	}
	for _, tc := range []struct {
		name string
		args []float64
		want float64
	}{
		{"sum", []float64{10}, sum(10)},
		{"sum", []float64{1000}, sum(1000)},
		{"sum", []float64{-3}, 0},
		{"bits", []float64{12345, 678}, func() float64 {
			a, b := int32(12345), int32(678)
			x := a ^ b
			x = (x << 3) | int32(uint32(x)>>1)
			x &= 0xffff
			return float64((x >> 2) + (^x & 7) + a)
		}()},
		{"collatz", []float64{27}, 111},
		{"sw", []float64{1}, 10},
		{"sw", []float64{3}, 20},
		{"sw", []float64{-0.5}, 30},
		{"sw", []float64{7}, 40},
		{"nested", []float64{12}, func() float64 {
			c := 0
			for i := range 12 {
				for j := i; j >= 0; j-- {
					if j == 3 {
						continue
					}
					if i > 7 {
						break
					}
					c += j * i
				}
			}
			return float64(c)
		}()},
		{"post", []float64{9}, func() float64 {
			i, s := 0, 0
			for i < 9 {
				s += i * 2
				i++
				i--
				s -= i
				i += 2
			}
			return float64(s)
		}()},
		{"cmp", []float64{1, 2}, 1 + 2 + 4 + 16},
		{"cmp", []float64{2, 2}, 2 + 4 + 8 + 32},
		{"cmp", []float64{math.NaN(), 2}, 4 + 16},
		{"neg", []float64{3}, -9 - 1},
		{"neg", []float64{0}, -2},
		{"pow", []float64{2, 10}, 2 - 0},
		{"pow", []float64{7.5, -2}, 1.5 - -2},
	} {
		fn := fns[tc.name]
		p := regLowered(t, fn)
		exit, err := regEvaluate(p, fn, tc.args...)
		if err != nil || exit.Kind != ir.Returned || exit.Value.Kind != ir.Number || math.Float64frombits(exit.Value.Bits) != tc.want {
			t.Errorf("%s%v: got %+v (%v), want %v\n%s", tc.name, tc.args, exit, err, tc.want, fn.RegCode())
		}
	}
}

// TestLowerRegExits checks that what the evaluator cannot make leaves at
// the register pc of the instruction that does it, the frame's registers
// as they were before it: a global's read, a property's, a call.
func TestLowerRegExits(t *testing.T) {
	fns := regCompiled(t, `
function g(n) { let s = n + 1; return s + glob }
function p(o, n) { let s = n * 2; return o.x + s }
function c(f, n) { let s = n - 1; return f(s) }
function instance(o) { if (o instanceof C) return 1; return 2 }
`)
	for _, name := range []string{"g", "p", "c", "instance"} {
		fn := fns[name]
		prog := regLowered(t, fn)
		exit, err := regEvaluate(prog, fn, 5, 5)
		if err != nil || exit.Kind != ir.HostExit && exit.Kind != ir.GuardExit {
			t.Fatalf("%s: got %+v, %v; want an exit\n%s", name, exit, err, fn.RegCode())
		}
		w := fn.RegCode().Code[exit.State.PC]
		switch w.Op() {
		case bytecode.RGetGlobal, bytecode.RGetGlobalInstanceOf, bytecode.RGetProp, bytecode.RCallW:
		default:
			t.Fatalf("%s: left at pc %d, %s\n%s", name, exit.State.PC, fn.RegCode().Format(int(exit.State.PC)), fn.RegCode())
		}
		if exit.State.Depth != 0 {
			t.Fatalf("%s: a register state with operands: %+v", name, exit.State)
		}
	}
}

// TestLowerRegFused lowers each fused register instruction into its
// expansion and checks the atomicity rule's shape: what may leave first,
// Generic after what may commit, the registers written last.
func TestLowerRegFused(t *testing.T) {
	fns := regCompiled(t, `
function addProp(o, v) { o.n += v; o.m++; return o }
function addIndex(a, i, v) { a[i] += v; a[3] += v; a[i]++; return a }
function pre(a, i) { let s = 0; while (i < 10) s += a[++i]; return s }
function post(a, i) { let s = 0; while (i < 10) s += a[i++]; return s }
function method(o) { return o.f(1) }
function lit(a, b) { return { x: a, y: b, z: a + b } }
function codes(s) { let h = 0; for (let i = 0; i < s.length; i++) h = h * 31 + s.charCodeAt(i) | 0; return h }
`)
	want := map[string]bytecode.ROp{"addProp": bytecode.RAddProp, "addIndex": bytecode.RAddIndex, "pre": bytecode.RGetIndexPre,
		"post": bytecode.RGetIndexPost, "method": bytecode.RGetMethodW, "lit": bytecode.RNewObjectLit, "codes": bytecode.RCharCodeAt}
	for name, op := range want {
		fn := fns[name]
		p := regLowered(t, fn)
		found := false
		for pc, w := range fn.RegCode().Code {
			if w.Op() != op {
				continue
			}
			found = true
			i := p.At(uint32(pc))
			end := i + 1
			for end < len(p.Code) && p.Origin[end] == uint32(pc) {
				end++
			}
			if end-i < 2 {
				t.Errorf("%s: %s is one instruction", name, fn.RegCode().Format(pc))
			}
			committed := false
			for k := i; k < end; k++ {
				in := &p.Code[k]
				if committed && in.MayExit() {
					t.Errorf("%s: %s: instruction %d leaves after a commit", name, fn.RegCode().Format(pc), k-i)
				}
				committed = committed || p.CommitsAt(k)
			}
			if op == bytecode.RAddProp && (!p.Code[i+1].Generic || !p.Code[i+2].Generic) {
				t.Errorf("%s: add_prop's addition and store are not Generic", name)
			}
		}
		if !found {
			t.Errorf("%s: no %s in its register code\n%s", name, op, fn.RegCode())
		}
	}
}

// TestLowerRegRefusals checks LowerReg's refusals by kind: a with
// statement's probes and direct eval, and a function that does nothing
// native code saves time on.
func TestLowerRegRefusals(t *testing.T) {
	fns := regCompiled(t, `
function withs(o) { with (o) { return x } }
function evals(s) { return eval(s) }
function wrapper(o) { return o.f() }
function loop(o) { let s = 0; for (let i = 0; i < 9; i++) s += o.f(); return s }
`)
	for name, want := range map[string]string{"withs": "unsupported opcode with_", "evals": "direct eval", "wrapper": hostOnly} {
		fn := fns[name]
		_, err := LowerReg(fn.RegCode(), fn)
		var rf *Refusal
		if !errors.As(err, &rf) || !strings.HasPrefix(rf.Reason, want) {
			t.Errorf("%s: got %v, want a refusal for %q", name, err, want)
		}
	}
	if _, err := LowerRegHot(fns["wrapper"].RegCode(), fns["wrapper"]); err != nil {
		t.Errorf("wrapper, hot: %v", err)
	}
	if p, err := LowerReg(fns["loop"].RegCode(), fns["loop"]); err != nil {
		t.Errorf("loop: %v", err)
	} else if err := p.Validate(); err != nil {
		t.Errorf("loop: %v", err)
	}
}

// A cold lowering (LowerRegCold), its scratch its own, makes the program
// LowerReg makes with the scratch the last lowering kept, whose
// instructions it copies at their lengths: what a measure of the cold
// lowering's memory measures is the lowering the JIT makes.
func TestLowerRegColdSameProgram(t *testing.T) {
	fns := regCompiled(t, `
function sum(n) { let s = 0; for (let i = 0; i < n; i++) s += i; return s }
function nested(n) { let c = 0; for (let i = 0; i < n; i++) for (let j = i; j >= 0; j--) { if (j == 3) continue; if (i > 7) break; c += j * i } return c }
function sw(x) { switch (x) { case 1: return 10; case 2: case 3: return 20; default: return 40 } }
`)
	for _, name := range []string{"nested", "sum", "sw", "nested"} {
		fn := fns[name]
		warm, err := LowerReg(fn.RegCode(), fn)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		cold, err := LowerRegCold(fn.RegCode(), fn)
		if err != nil {
			t.Fatalf("%s: cold: %v", name, err)
		}
		if !reflect.DeepEqual(warm, cold) {
			t.Errorf("%s: the cold lowering's program differs:\n%v\nwarm:\n%v", name, cold.Code, warm.Code)
		}
	}
}

// An element's store takes its value as it is, as a property's does: a
// property read only stored is read as a reference, not for a number it
// need not be, and one that arithmetic or an index uses for a number; an
// element read only stored keeps its numeric read, which its feedback
// makes a reference once its guard has failed (ssa's referenceReads).
func TestLowerRegStoredValues(t *testing.T) {
	fns := regCompiled(t, `
		function property(a, o) { let x = o.x; a[0] = x }
		function numeric(a, o) { a[0] = o.x * 2 }
		function index(a, o) { a[o.x] = 1 }
		function element(a, b) { let x = b[0]; a[0] = x }`)
	for _, tt := range []struct {
		name string
		op   ir.Op
	}{
		{"property", ir.ReferenceRead},
		{"numeric", ir.PropertyRead},
		{"index", ir.PropertyRead},
		{"element", ir.ArrayRead},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fn := fns[tt.name]
			p, err := LowerReg(fn.RegCode(), fn)
			if err != nil {
				t.Fatal(err)
			}
			reads := 0
			for _, in := range p.Code {
				if in.Op != ir.PropertyRead && in.Op != ir.ReferenceRead && in.Op != ir.ArrayRead {
					continue
				}
				reads++
				if in.Op != tt.op || in.Reference {
					t.Errorf("read = %v, reference %v; want %v, not a reference", in.Op, in.Reference, tt.op)
				}
			}
			if reads != 1 {
				t.Fatalf("%d reads, want 1", reads)
			}
		})
	}
}
