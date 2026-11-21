package vm

import (
	"fmt"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// farForms counts code's far jumps: a branch over a jmp over a jmp of 32
// bits (regcomp's gen.jump), at each pc where one is.
func farForms(code []bytecode.RInstr) int {
	n := 0
	for k := 0; k+2 < len(code); k++ {
		if w := code[k]; w.Op() != bytecode.RJmp && w.SC() == 1 && code[k+1].Op() == bytecode.RJmp && code[k+1].BC() == 1 && code[k+2].Op() == bytecode.RJmp {
			n++
		}
	}
	return n
}

// TestRegFarJumpsOnlyWhereNeeded compiles a function one of whose jumps,
// an if's over its long body, does not reach as a 16-bit offset, among
// loops of short ifs before and after it: that jump alone is made far
// (regcomp's generate), the others branching as one instruction, and the
// answers are exact. Compiled again with every jump far once one did not
// reach, each of its conditionals had been three instructions.
func TestRegFarJumpsOnlyWhereNeeded(t *testing.T) {
	small := "for (var i = 0; i < 8; i++) { if (x > i) a = (a + 1) | 0; if (x < i) a = (a - 1) | 0; if (i == 3) a = a ^ 5 }"
	src := `
		function f(x) {
			var a = 0;
			` + small + `
			if (x > 3) { ` + strings.Repeat("a = (a + 3) | 0; ", 40000) + ` } else { a = -a }
			` + small + `
			return a
		}
		[f(1), f(2), f(7), f(0)].join()`
	fn := compileForTest(t, src)
	r := newTestRuntime(t, Config{})
	defer r.Close()
	v, err := r.Run(fn)
	if err != nil {
		t.Fatal(err)
	}
	// Each answer as the source computes it.
	want := func(x int32) int32 {
		a := int32(0)
		loop := func() {
			for i := int32(0); i < 8; i++ {
				if x > i {
					a++
				}
				if x < i {
					a--
				}
				if i == 3 {
					a ^= 5
				}
			}
		}
		loop()
		if x > 3 {
			a += 3 * 40000
		} else {
			a = -a
		}
		loop()
		return a
	}
	if got, w := v.String().Go(), fmt.Sprintf("%d,%d,%d,%d", want(1), want(2), want(7), want(0)); got != w {
		t.Fatalf("got %s, want %s", got, w)
	}
	f := r.global.getOwn(r.atoms.intern("f")).value().Object().fn().closure
	rc := r.regOf(f)
	if rc == nil {
		t.Fatal("f has no register code")
	}
	if n := farForms(rc.rc.Code); n < 1 || n > 2 {
		t.Errorf("%d far jumps, want 1 or 2: the if's over its body and no more", n)
	}
}

// TestAsmFarBranches has typed code a branch past a 16-bit offset, an if
// over a body of some 40,000 instructions: the module's function gets its
// typed code, that branch made far (regcomp's CompileTyped), where it had
// had none and ran its ordinary code, and its answers are exact.
func TestAsmFarBranches(t *testing.T) {
	src := `
		function FarBranches(stdlib, foreign, heap) {
			"use asm";
			function f(x) {
				x = x | 0;
				var a = 0, i = 0;
				for (i = 0; (i | 0) < 8; i = (i + 1) | 0)
					if ((x | 0) > (i | 0)) a = (a + 1) | 0;
				if (x & 1) { ` + strings.Repeat("a = (a + 3) | 0; ", 40000) + ` } else { a = (0 - a) | 0; }
				return a | 0;
			}
			return { f: f };
		}
		var farMod = FarBranches(globalThis, {}, new ArrayBuffer(65536));
		[farMod.f(1), farMod.f(2), farMod.f(9)].join()`
	fn := compileForTest(t, src)
	r := newTestRuntime(t, Config{})
	defer r.Close()
	v, err := r.Run(fn)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := v.String().Go(), "120001,-2,120008"; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
	m, _ := r.GetProp(Obj(r.Global()), r.Intern("farMod"))
	fv, _ := r.GetProp(m, r.Intern("f"))
	if t2 := fv.Object().fn().closure.regTyped; t2 == nil || !t2.typed {
		t.Fatal("f has no typed code")
	} else if n := farForms(t2.rc.Code); n < 1 || n > 2 {
		t.Errorf("%d far branches in its typed code, want 1 or 2", n)
	}
}
