//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"testing"
)

// regBoolsSrc's bools ORs and ANDs comparisons' booleans, as Emscripten's
// code does, each a compare-and-branch over two loads in register code
// (boolBranch), kept in registers and read in the block the comparison
// ends; calls reads a function table's element, calls it and ORs its
// result with 0 in the register the element was read to, which the call
// writes.
const regBoolsSrc = `
function bools(n) {
	var s = 0
	for (var i = 0; i < n; i++) {
		var x = i < 5, y = i > n - 3
		s = (s + (((i & 3) == 1) | (i > 900)) + (x | y) + (x & y) + ((i < 7) & 1) + ((i === 2) ^ (i !== 9))) | 0
	}
	return s
}
var table = [function (a) { return a + 1 }, function (a) { return a * 2 }, function (a) { return a - 3 }, function (a) { return a >> 1 }]
function calls(n) {
	var s = 0
	for (var i = 0; i < n; i++) s = (s + (table[i & 3](i) | 0)) | 0
	return s
}
function strict(x, n) {
	var s = 0;
	for (var i = 0; i < n; i++) s += (x === false ? 1 : 0) + (true === x ? 2 : 0) + (x !== true ? 4 : 0) + (false !== x ? 8 : 0);
	return s;
}
`

// TestJITRegBoolsNoDeopt runs regBoolsSrc's functions natively and without
// the JIT: the same answers, and native code that never deoptimizes --
// a comparison's boolean taken as its number natively, as the stack
// form's code takes it (ir.Instruction's Bools), and the table's element
// read as it is, not as a number -- bools compiled once. Both had
// deoptimized, an operator or a read at a time: Octane's zlib's largest
// function, of 13,648 register instructions, at 22 of them, each
// deoptimization a compile of a second.
func TestJITRegBoolsNoDeopt(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			rts := regNativeRuntimes(t, regBoolsSrc)
			run := func(name string, n float64) string {
				var got [2]string
				for i, r := range rts {
					fn, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
					v, err := r.Call(fn, Undefined, []Value{Float(n)})
					got[i] = regNativeShow(r, v, err)
				}
				if got[0] != got[1] {
					t.Errorf("%s: with the JIT %q, without %q", name, got[0], got[1])
				}
				return got[0]
			}
			r := rts[0]
			for range 30 {
				run("bools", 1000)
			}
			if got := run("bools", 1000); got != "1336" {
				t.Errorf("bools: %q", got)
			}
			if st := r.RegNativeStats(); st.Entries == 0 || st.Guards != 0 || r.jit.invalidated != 0 {
				t.Errorf("bools: %d entries, %d guards failed, %d codes invalidated; want some, none and none", st.Entries, st.Guards, r.jit.invalidated)
			}
			for range 30 {
				run("calls", 1000)
			}
			if got := run("calls", 1000); got != "561000" {
				t.Errorf("calls: %q", got)
			}
			// Its call learns the table's functions, which may invalidate
			// it (jitRegCallRepeats); its guards never fail.
			if st := r.RegNativeStats(); st.Guards != 0 {
				t.Errorf("calls: %d guards failed; want none", st.Guards)
			}
			for _, c := range []struct {
				value Value
				want  string
			}{{False, "5000"}, {True, "10000"}, {Int(0), "12000"}, {Int(1), "12000"}, {Null, "12000"}, {Undefined, "12000"}} {
				for range 30 {
					for _, rt := range rts {
						fn, _ := rt.GetProp(Obj(rt.Global()), rt.Intern("strict"))
						v, err := rt.Call(fn, Undefined, []Value{c.value, Int(1000)})
						if got := regNativeShow(rt, v, err); got != c.want {
							t.Fatalf("strict(%v): got %s, want %s", c.value, got, c.want)
						}
					}
				}
			}
		})
	}
}

// TestJITRegBoolLiterals runs boolLiteralSrc compiled by the JIT, the
// collector idle and marking, with Node's answers: jbool lowered as a
// strict comparison with the literal (compile's LowerReg), on both sides
// and both senses, its back edges in loop among them.
func TestJITRegBoolLiterals(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			rts := regNativeRuntimes(t, boolLiteralSrc)
			for _, r := range rts {
				fn, _ := r.GetProp(Obj(r.Global()), r.Intern("run"))
				for range 3 {
					v, err := r.Call(fn, Undefined, nil)
					if got := regNativeShow(r, v, err); got != boolLiteralWant {
						t.Fatalf("JIT %v:\n%s\nwant\n%s", r.jitEnabled, got, boolLiteralWant)
					}
				}
			}
			r := rts[0]
			for _, name := range []string{"strict", "loop"} {
				fn, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
				if e := fn.Object().fn().closure.closureCode.jitReg; e == nil || e.code == nil {
					t.Errorf("%s not compiled", name)
				}
			}
			if st := r.RegNativeStats(); st.Entries == 0 {
				t.Errorf("no native code entered: %+v", st)
			}
		})
	}
}
