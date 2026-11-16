//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	compilerpkg "github.com/go-quickjs/go-quickjs/internal/compiler"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
)

// Register code's BigInts given back as the stack form's are
// (TestJITBigIntGivenBack): bigReleasePrograms, each answer Node's, run
// three times by register code compiled at the first call and by default,
// and under jitMarkingForTest -- what is given back and what may not be,
// a value an array, a call, a string, a global, a closure, another
// register or a postfix update holds.
func TestJITRegBigIntGivenBack(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		for _, stress := range []bool{true, false} {
			t.Run(fmt.Sprintf("marking=%v,stress=%v", marking, stress), func(t *testing.T) {
				if marking {
					jitMarkingForTest(t)
				}
				for i, p := range bigReleasePrograms {
					r := regBigRuntime(t)
					if stress {
						r.jitCallThreshold, r.jitICThreshold = 1, 1
						r.SetJITWarmUp(1, 1)
					}
					prog := regBigCompile(t, p.src)
					for round := range 3 {
						got, err := r.Run(prog)
						if err != nil {
							t.Fatal(err)
						}
						if g := got.String().Go(); g != p.want {
							t.Fatalf("program %d, round %d: %s, Node's %s", i, round, g, p.want)
						}
					}
				}
			})
		}
	}
}

// microbench.js's bigint_arith, register code compiled: warm, its blocks
// are given back as it goes -- sum's old value where the sum is stored over
// it, a * a's where the sum has taken it -- rather than allocated anew at
// each turn, as the stack form's code does (TestJITBigIntGivenBackAllocs).
func TestJITRegBigIntGivenBackAllocs(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := regBigRuntime(t)
	jitDefaultsForTest(r)
	if _, err := r.Run(regBigCompile(t, `
		function arith(n) {
			var sum = 0n, a = 1n << 123n, incr = 1n
			for (var i = 0; i < n; i++) {
				sum += a * a
				a += incr
			}
			return Number(sum % 1000003n)
		}`)); err != nil {
		t.Fatal(err)
	}
	run := regBigCompile(t, `arith(1000)`)
	for range 20 {
		if _, err := r.Run(run); err != nil {
			t.Fatal(err)
		}
	}
	cl := r.global.getOwn(r.atoms.intern("arith")).value().Object().fn().closure
	if e := r.jit.regCache[r.jitKeyOf(cl.fn)]; e == nil || e.code == nil || e.invalid {
		t.Fatal("arith has no register code")
	} else if f := r.jitRegFeedbackFor(cl, e.p); f != nil {
		if sole, ok := f.BigIntSole(); !ok || sole == 0 {
			t.Fatalf("arith's registers holding their values alone: %b, %v", sole, ok)
		}
	}
	made, fresh := r.jit.bigLeafs, r.jit.bigLeafNew
	got, err := r.Run(run)
	if err != nil {
		t.Fatal(err)
	}
	// Node's.
	if g := got.Number(); g != 273446 {
		t.Fatalf("arith(1000) = %v, want 273446", g)
	}
	// 3000 operators made by the leaf, of 2000 BigInts of 4 and 5 words:
	// in a few new blocks, not thousands.
	if n, m := r.jit.bigLeafs-made, r.jit.bigLeafNew-fresh; n < 3000 || m > 20 {
		t.Fatalf("arith(1000): the leaf made %d operators, in %d new blocks", n, m)
	}
}

// TestRegSoleRegisters pins which registers hold their values alone, of a
// function of one parameter (r2) and four more registers: those an
// operator reads -- an accumulator, a step, a temporary -- do; the
// parameter, a register a move, a call's window, a return, a global's
// store, a property's read or a postfix update reads, and one a closure
// captures do not; nor any where the function has an instruction the
// analysis does not know.
func TestRegSoleRegisters(t *testing.T) {
	R := bytecode.MakeR
	loop := []bytecode.RInstr{
		R(bytecode.RLoadInt, 0, 3, 0, 1),
		R(bytecode.RMul, 0, 3, 3, 4),
		R(bytecode.RAdd, 0, 5, 3, 4),
		R(bytecode.RInc, 0, 6, 6, 0),
		R(bytecode.RJLt, 0, 6, 2, 0),
	}
	all := uint64(1<<3 | 1<<4 | 1<<5 | 1<<6)
	for _, c := range []struct {
		name     string
		code     []bytecode.RInstr
		captured int
		want     uint64
	}{
		{"operators", nil, -1, all},
		{"move", []bytecode.RInstr{R(bytecode.RMove, 0, 4, 3, 0)}, -1, all &^ (1 << 3)},
		{"window", []bytecode.RInstr{R(bytecode.RCallW, 1, 4, 3, 0)}, -1, all &^ (1<<4 | 1<<5 | 1<<6)},
		{"return", []bytecode.RInstr{R(bytecode.RRet, 0, 5, 0, 0)}, -1, all &^ (1 << 5)},
		{"global", []bytecode.RInstr{R(bytecode.RSetGlobal, 0, 3, 0, 0)}, -1, all &^ (1 << 3)},
		{"property", []bytecode.RInstr{R(bytecode.RGetProp, 0, 4, 3, 0)}, -1, all &^ (1 << 3)},
		{"postfix", []bytecode.RInstr{R(bytecode.RPostInc, 0, 4, 3, 0)}, -1, all &^ (1 << 3)},
		{"captured", nil, 3, all &^ (1 << 3)},
		{"unknown", []bytecode.RInstr{R(bytecode.RGetIndex, 0, 4, 3, 5)}, -1, 0},
	} {
		rc := &bytecode.RegCode{Code: append(append([]bytecode.RInstr{}, loop...), c.code...), NumRegs: 7}
		fn := &bytecode.Function{ParamCount: 1}
		p := &ir.Program{Regs: 7}
		if c.captured >= 0 {
			p.Captured = make([]bool, 7)
			p.Captured[c.captured] = true
		}
		if got := regSoleRegisters(fn, rc, p); got != c.want {
			t.Errorf("%s: registers holding their values alone %b, want %b", c.name, got, c.want)
		}
	}
}

// regBigRuntime is a runtime with the JIT whose code is register code.
func regBigRuntime(t *testing.T) *Runtime {
	t.Helper()
	SetRegTier(true)
	was := SetJITRegTier(true)
	t.Cleanup(func() { SetRegTier(bytecode.RegDefault()); SetJITRegTier(was) })
	r := New(Config{JIT: true})
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	return r
}

// regBigCompile compiles src with the register form.
func regBigCompile(t *testing.T, src string) *bytecode.Function {
	t.Helper()
	compilerpkg.SetRegisterForm(true)
	defer compilerpkg.SetRegisterForm(false)
	return compileForTest(t, src)
}
