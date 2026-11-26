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
