//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"math"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
)

// latticeSrc has a function of each lattice form, none of them a pure
// body, which pureCall would answer without a frame and so without the
// feedback dispatcher, and values of every kind.
const latticeSrc = `
function sub(a, b) { let r = a - b; return r }
function div(a, b) { let r = a / b; return r }
function addi32(a, b) { let r = (a + b) | 0; return r }
function subi(a) { let r = a - 1; return r }
function andi(a) { let r = a & 3; return r }
function neg(a) { let r = -a; return r }
function inc(a) { let r = ++a; return r }
function lt(a, b) { let r = 2; if (a < b) r = 1; return r }
function lti(a) { let r = 2; if (a < 5) r = 1; return r }
var vals = [1, -1, 0, -0, 2147483647, 2147483648, -2147483648, -2147483649, 1.5, NaN,
	Infinity, -Infinity, 1e300, undefined, null, true, false, "s", "", 1n, -5n,
	2n ** 63n - 1n, -(2n ** 63n), 2n ** 63n, 2n ** 64n - 1n, 2n ** 80n, -(2n ** 70n),
	{}, [], Symbol(), function () {}];
`

// TestJITLattice has the feedback dispatcher run each lattice form's
// instruction on operands of every kind, the function never compiled, and
// checks the instruction's byte of its lattice (jitRegOps) is the kinds of
// what it met, as jitLatticeKind finds them -- a number's where the byte
// had no number's bit (latticeAddForTest) -- natively on both of the
// operands' sides: with the collector marking and not, since the handlers
// keep the barrier's flag in the least tag's register they compare with.
func TestJITLattice(t *testing.T) {
	t.Parallel()
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			prog := compileForTest(t, latticeSrc)
			r := newTestRuntime(t, Config{JIT: true})
			t.Cleanup(r.Close)
			r.jitStress = jitStressConfig{}
			r.SetJITWarmUp(1<<16, 1<<16)
			if _, err := r.Run(prog); err != nil {
				t.Fatal(err)
			}
			global := func(name string) Value {
				v, err := r.GetProp(Obj(r.Global()), r.Intern(name))
				if err != nil {
					t.Fatal(err)
				}
				return v
			}
			arr := global("vals").Object()
			n, _ := r.GetProp(Obj(arr), r.Intern("length"))
			var vals []Value
			for i := 0; i < int(n.Number()); i++ {
				v, _ := r.GetProp(Obj(arr), r.Intern(fmt.Sprint(i)))
				vals = append(vals, v)
			}
			for _, c := range []struct {
				name string
				op   bytecode.ROp
				// binary says the second operand is a register, imm an
				// immediate.
				binary bool
				imm    bool
			}{
				{"sub", bytecode.RSub, true, false},
				{"div", bytecode.RBin, true, false},
				{"addi32", bytecode.RAddI32, true, false},
				{"subi", bytecode.RSubI, false, true},
				{"andi", bytecode.RAndI, false, true},
				{"neg", bytecode.RUnary, false, false},
				{"inc", bytecode.RInc, false, false},
				{"lt", bytecode.RJNLt, true, false},
				{"lti", bytecode.RJNLtI, false, true},
			} {
				fv := global(c.name)
				cl := fv.Object().fn().closure
				cl.jitRegRefused = true
				// The first call makes the template's code, and its
				// lattice.
				r.Call(fv, Undefined, []Value{Float(0), Float(0)})
				if !regNativeDefault {
					// The Go table records nothing, and no lattice is made
					// for it.
					if cl.jitRegOps != nil {
						t.Fatalf("%s: a lattice without the native handlers", c.name)
					}
					continue
				}
				rpc := -1
				for pc, w := range cl.reg.rc.Code {
					if w.Op() == c.op {
						rpc = pc
					}
				}
				if rpc < 0 || len(cl.jitRegOps) != len(cl.reg.rc.Code)+2 || cl.jitRegOps[len(cl.jitRegOps)-1] != 1 {
					t.Fatalf("%s: no %v, or a lattice of %d bytes for %d instructions, or its one run not counted", c.name, c.op, len(cl.jitRegOps), len(cl.reg.rc.Code))
				}
				seconds := []Value{Float(7)}
				if c.binary {
					seconds = vals
				}
				for i, x := range vals {
					for j, y := range seconds {
						cl.jitRegOps[rpc] = 0
						r.Call(fv, Undefined, []Value{x, y})
						want := latticeAddForTest(0, jitLatticeKind(x))
						switch {
						case c.binary:
							want = latticeAddForTest(want, jitLatticeKind(y))
						case c.imm:
							want |= abi.OperandInt32
						}
						if got := abi.Operands(cl.jitRegOps[rpc]); got != want {
							t.Errorf("%s(vals[%d], #%d): lattice %#x, want %#x", c.name, i, j, got, want)
						}
					}
				}
			}
		})
	}
}

// latticeAddForTest is byte b with the kind k the feedback dispatcher's
// handler records of an operand (nativegen's lattice.go): a number's
// only where b has no number's bit yet.
func latticeAddForTest(b, k abi.Operands) abi.Operands {
	if k&abi.OperandsNumber != 0 && b&abi.OperandsNumber != 0 {
		return b
	}
	return b | k
}

// TestJITLatticeKind pins jitLatticeKind's answers, which the native
// handlers' are checked against.
func TestJITLatticeKind(t *testing.T) {
	for _, c := range []struct {
		v    Value
		want abi.Operands
	}{
		{Float(0), abi.OperandInt32},
		{Float(-1), abi.OperandInt32},
		{Float(2147483647), abi.OperandInt32},
		{Float(-2147483648), abi.OperandInt32},
		{Float(2147483648), abi.OperandNumber},
		{Float(-2147483649), abi.OperandNumber},
		{Float(0.5), abi.OperandNumber},
		{Float(math.Copysign(0, -1)), abi.OperandNumber},
		{Float(math.NaN()), abi.OperandNumber},
		{Undefined, abi.OperandOddball},
		{Null, abi.OperandOddball},
		{True, abi.OperandOddball},
		{shortBig(-5), abi.OperandBigInt64},
		{shortBigMag(false, 1<<63), abi.OperandBigInt},
		{uninitialized, abi.OperandOther},
	} {
		if got := jitLatticeKind(c.v); got != c.want {
			t.Errorf("%v: %#x, want %#x", c.v, got, c.want)
		}
	}
}

// latticeSpecSrc's functions are compiled after runs that met strings,
// oddballs beside numbers and, for rare's arithmetic, nothing.
const latticeSpecSrc = `
function strs(a, b) { let r = a + b; return r }
function odd(a) { let r = (a | 0) + 1; return r }
function oadd(a, b) { let r = a + b; return r }
function olt(a, b) { let r = 0; if (a < b) r = 1; return r }
function obj(a, b) { let r = a * b; return r }
var ov = { valueOf() { return 3 } };
function rare(a, flag) { let r = a; if (flag) r = a * 3 - 1; return r }
function drive(n, f, x, y) { let t; for (let i = 0; i < n; i++) t = f(x, y); return t }
function warm() {
	for (let k = 0; k < 4; k++) {
		drive(1000, strs, "a", "b"); drive(1000, strs, "a", 1);
		drive(1000, odd, undefined); drive(1000, odd, 2.5); drive(1000, odd, null);
		drive(1000, oadd, undefined, 1); drive(1000, oadd, 1.5, 2);
		drive(1000, olt, undefined, 1); drive(1000, olt, 1.5, 2);
		drive(1000, obj, ov, 2); drive(1000, obj, 2, 2);
		drive(3000, rare, 2, false);
	}
}
`

// TestJITLatticeSpeculates checks the builder speculates what the lattice
// met (ssa.OperandFeedback): an addition that met strings is generic from
// the first compile, and runs on strings with no deoptimization; an
// operator that met an object is generic from the first compile, with no
// exit at each object and no deoptimization; operators that met oddballs
// beside numbers still give the interpreter's answers; and arithmetic that
// met nothing, left to Go, makes
// its first run at a host exit that invalidates nothing, the lattice
// learning what it met and the code marked to be compiled again for it.
func TestJITLatticeSpeculates(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	t.Parallel()
	r := newTestRuntime(t, Config{JIT: true})
	t.Cleanup(r.Close)
	// The JIT's own budgets, not QJS_JIT_STRESS's, at which a function is
	// compiled before the lattice has seen it run (jitLatticeRan).
	jitDefaultsForTest(r)
	if _, err := r.Run(compileForTest(t, latticeSpecSrc)); err != nil {
		t.Fatal(err)
	}
	// rare's first runs from a loop of the interpreter's, so that the
	// lattice has seen it run to its end before any compile asks it
	// (jitLatticeRan).
	if _, err := r.Run(compileForTest(t, "for (let q = 0; q < 20; q++) rare(2, false)")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(compileForTest(t, "warm()")); err != nil {
		t.Fatal(err)
	}
	run := func(src, want string) {
		t.Helper()
		v, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		got := fmt.Sprint(v.Number())
		if v.IsString() {
			got = `"` + v.String().Go() + `"`
		}
		if got != want {
			t.Fatalf("%s = %s, want %s", src, got, want)
		}
	}
	if !regNativeDefault {
		// No lattice: what the code learns, it learns at exits.
		run(`drive(10, rare, 2, true)`, "5")
		return
	}
	for _, name := range []string{"strs", "odd", "oadd", "olt", "obj", "rare"} {
		if e := jitCodeForTest(r, name); e == nil || e.invalid {
			t.Fatalf("%s has no code", name)
		}
	}
	run(`drive(100, odd, undefined) + drive(100, odd, 7.5) + drive(100, odd, true)`, "11")
	for _, c := range []struct{ name, src, want string }{
		{"strs", `drive(100, strs, "x", "y") + drive(100, strs, "x", 2)`, `"xyx2"`},
	} {
		e := jitCodeForTest(r, c.name)
		run(c.src, c.want)
		if e.invalid || e.deopts != 0 || len(e.failed) != 0 {
			t.Errorf("%s: invalid %v, %d deoptimizations, failed at %v", c.name, e.invalid, e.deopts, e.failed)
		}
	}
	// Oddballs beside numbers, taken for numbers as without the lattice,
	// and then a string: concatenated, and compared as a string.
	run(`drive(100, oadd, null, 2) + drive(100, oadd, true, 0.5)`, "3.5")
	run(`drive(100, olt, null, 1) + drive(100, olt, 2, 1)`, "1")
	run(`drive(10, oadd, "s", 1)`, `"s1"`)
	run(`drive(10, olt, "a", "b") + drive(10, olt, "b", "a") + drive(10, olt, "10", "9")`, "2")
	// An operator that met an object is generic from the first compile:
	// no deoptimization, and no exit at each object.
	if e := jitCodeForTest(r, "obj"); e != nil {
		hosts := e.ssaStats.hosts
		run(`drive(100, obj, ov, 2) + drive(100, obj, 2, 5)`, "16")
		if e.ssaStats.hosts != hosts || e.invalid || e.deopts != 0 {
			t.Errorf("obj: %d host exits, invalid %v, %d deoptimizations", e.ssaStats.hosts-hosts, e.invalid, e.deopts)
		}
	}
	e := jitCodeForTest(r, "rare")
	cl := r.global.getOwn(r.atoms.intern("rare")).value().Object().fn().closure
	mul := -1
	for pc, w := range cl.reg.rc.Code {
		if w.Op() == bytecode.RMulI {
			mul = pc
		}
	}
	if mul < 0 || cl.jitRegOps[mul] != 0 {
		t.Fatalf("rare's multiplication at %d, its lattice %v: want one that met nothing", mul, cl.jitRegOps)
	}
	hosts, asks := e.ssaStats.hosts, e.reoptAsks
	run(`rare(2, true)`, "5")
	if e.invalid || e.deopts != 0 {
		t.Errorf("rare: invalid %v, %d deoptimizations at a site that met nothing", e.invalid, e.deopts)
	}
	if e.ssaStats.hosts == hosts || e.reoptAsks == asks {
		t.Errorf("rare: %d host exits, %d asks to compile again", e.ssaStats.hosts-hosts, e.reoptAsks-asks)
	}
	if got := abi.Operands(cl.jitRegOps[mul]); got != abi.OperandInt32 {
		t.Errorf("rare's multiplication learned %#x at its host exit, want an int32's", got)
	}
}
