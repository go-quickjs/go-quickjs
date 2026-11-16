//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/compiler"
)

// Register code's sort of a typed array of a number type, or of an Array
// of numbers, by a comparator native code may call again at will
// (jitRegSortComparator over its register code), is the caller's native
// code, as the stack form's is: a merge sort, the comparator inlined
// (ssa's sortCall). The register form had left every sort to Go's
// (jitRegForEachSeen's and ForEach's refusal of abi.LoopSort), each
// comparison a call of the comparator from Go. Each of sortSetup's
// answers is the interpreter's -- stable sorts, ties kept in order, a
// comparator that throws leaving the array as it was, the TypeErrors of
// a class and of no function -- with the collector idle and marking; the
// sorts of numbers by such a comparator call no Go's sort, nor leave
// their code, nor have Go finish or restart them, and those by any other
// comparator, or of a BigInt64Array, are Go's.
func TestJITRegSortNative(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			testJITRegSortNative(t)
		})
	}
}

func testJITRegSortNative(t *testing.T) {
	drivers, drive := jitSortDrivers()
	// sortInherited gives Array.prototype an element, after which no
	// Array is one native code sorts (jitSortArray's noInheritedIndices):
	// it runs after the counts.
	var rounds, inherited []string
	for range 6 {
		for _, c := range jitSortWant {
			if strings.HasPrefix(c.src, "sortInherited") {
				inherited = append(inherited, drive(c.src, "20"))
				continue
			}
			rounds = append(rounds, drive(c.src, "20"))
		}
		rounds = append(rounds, drive("sortBool(i32, c_i32)", "20"))
	}
	native := []string{
		"up_i32(i32, c_i32)", "down_i32(i32, c_i32)", "up_f64(f64, c_f64)", "down_f64(f64, c_f64)",
		"up_f32(f32, c_f32)", "up_u8c(u8c, c_u8c)", "down_i8(i8, c_i8)", "up_u8(u8, c_u8)",
		"up_i16(i16, c_i16)", "up_u16(u16, c_u16)", "down_u32(u32, c_u32)",
		"up_nums(nums)", "down_nums(nums)", "sortBranch(f64, c_f64)", "sortBranchArr(nums)",
		"sortWithUp(i32, c_i32)", "sortTies(ties, c_ties)", "sortTiesArr(tiesArr)",
		"sortNaNResult(i32, c_i32)", "sortZero(nums)", "sortBool(i32, c_i32)", "sortSometimes(i32, c_i32)",
		"sortWild(i32, c_i32)", "sortNaNs(withNaN, c_nan)", "sortNaNLast(withNaN, c_nan)",
		"sortLong(long, c_long)", "sortLongTies(long, c_long)", "sortLongArr(longArr)", "sortMethod(i32, c_i32)",
	}
	gos := []string{
		"sortString(i32, c_i32)", "sortNaNGlobal(i32, c_i32)", "sortCaptured(i32, c_i32)", "sortProp(i32, c_i32)",
		"sortCall(i32, c_i32)", "sortLogs(i32, c_i32)", "sortBig()", "sortThrows(i32, c_i32)", "sortThrowsArr(nums)",
	}
	rts := regNativeRuntimes(t, sortSetup+drivers())
	r := rts[0]
	// Counted as the code asks, not as stress configures it, each
	// function compiled at its first calls.
	r.jitStress = jitStressConfig{}
	r.SetJITWarmUp(1, 1)
	name := func(src string) string { n, _, _ := strings.Cut(src, "("); return n }
	entry := func(src string) *jitRegEntry {
		for k, e := range r.jit.regCache {
			if fn := k.Value(); fn != nil && fn.Name == name(src) {
				return e
			}
		}
		return nil
	}
	// learned says the function's code learned a loop of a built-in's
	// call (jitRegForEachSeen).
	learned := func(src string) bool {
		for k, l := range r.jit.regLearned {
			if fn := k.Value(); fn != nil && fn.Name == name(src) && slices.ContainsFunc(l.nativeCalls, func(x jitInline) bool { return x.forEach }) {
				return true
			}
		}
		return false
	}
	run := func(src string) string {
		compiler.SetRegisterForm(true)
		prog := compileForTest(t, src)
		compiler.SetRegisterForm(false)
		var got [2]string
		for k, r := range rts {
			v, err := r.Run(prog)
			got[k] = regNativeShow(r, v, err)
		}
		if got[0] != got[1] {
			t.Fatalf("%s: with the JIT %q, without %q", src, got[0], got[1])
		}
		return got[0]
	}
	for _, src := range rounds {
		run(src)
	}
	s := r.jitState()
	for _, src := range native {
		if e := entry(src); e == nil || e.code == nil || e.invalid || e.keep == nil || len(e.keep.inlined) == 0 || !learned(src) {
			t.Errorf("%s has no native sort inlining its comparator", src)
		}
		hosts, finished, restarted := s.hosts, s.loopsFinished, s.sortsRestarted
		run(drive(src, "10"))
		if n := s.hosts - hosts; n != 0 {
			t.Errorf("%s left native code %d times", src, n)
		}
		if n := s.loopsFinished - finished; n != 0 {
			t.Errorf("%s: Go finished the sort %d times", src, n)
		}
		if n := s.sortsRestarted - restarted; n != 0 {
			t.Errorf("%s: Go sorted again %d times", src, n)
		}
	}
	// A comparator that is not a body to call again at will, or a
	// BigInt64Array: Go's sort, never learned as a loop.
	for _, src := range gos {
		if learned(src) {
			t.Errorf("%s sorts natively", src)
		}
	}
	for _, src := range inherited {
		run(src)
	}
	for _, c := range jitSortWant {
		if got := run("show(" + c.src + ")"); got != c.want {
			t.Errorf("%s = %s, want %s", c.src, got, c.want)
		}
	}
}

// An exit from a sort register code makes natively, anywhere in it --
// here a back edge's budget run out, or a deoptimization of QJS_JIT_STRESS's
// -- has Go sort the array again from the first, as it was (jitLoopFinish):
// every answer is still the interpreter's.
func TestJITRegSortRestarts(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			drivers, drive := jitSortDrivers()
			var rounds []string
			for range 6 {
				for _, c := range jitSortWant {
					rounds = append(rounds, drive(c.src, "20"))
				}
			}
			rts := regNativeRuntimes(t, sortSetup+drivers())
			regStressForTest(rts[0], jitStressConfig{budget: 2, deopt: 3})
			for _, src := range rounds {
				compiler.SetRegisterForm(true)
				prog := compileForTest(t, src)
				compiler.SetRegisterForm(false)
				var got [2]string
				for k, r := range rts {
					v, err := r.Run(prog)
					got[k] = regNativeShow(r, v, err)
				}
				if got[0] != got[1] {
					t.Fatalf("%s: with the JIT %q, without %q", src, got[0], got[1])
				}
			}
			if rts[0].jitState().sortsRestarted == 0 {
				t.Error("Go never sorted again")
			}
		})
	}
}
