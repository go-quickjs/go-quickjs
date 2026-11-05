//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "testing"

// rerunJITSource is a program whose functions read literals of two sites:
// one its top-level code makes, which the interpreter runs at each run, and
// one a function makes, natively once it is compiled.
const rerunJITSource = `
	function make(i) { return {a: i, b: i + 1} }
	function read(o) { return o.a + o.b }
	var top = {a: 1, b: 2}, s = 0
	for (var i = 0; i < 3000; i++) s += read(i & 1 ? make(i) : top)
	s`

// Code compiled in one run of a program holds for the next: a literal native
// code makes objects of has the root the next run's interpreter makes them of
// too. Each run's own roots left code with the first run's, which it kept
// making its objects of while each run's other tiers made another's, and in
// this program's first ten runs it was compiled again 8 times, not 3.
func TestJITProgramRerunsKeepLayouts(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitRuntimeForTest(t, Config{JIT: true})
	fn := compileForTest(t, rerunJITSource)
	for run := 1; run <= 6; run++ {
		if got := runProgramForTest(t, r, fn).Number(); got != 4506000 {
			t.Fatalf("run %d: got %v", run, got)
		}
		cl := globalObject(r, "make").fn().closure
		found := 0
		for _, e := range r.jit.cache {
			for i := range e.literals {
				l := &e.literals[i]
				if l.root == nil || l.fn != cl.fn {
					continue
				}
				found++
				if root := cl.ic[cl.fn.Code[l.pc].B].shape; root != l.root {
					t.Fatalf("run %d: native code makes objects of root %p, the site's is %p", run, l.root, root)
				}
			}
		}
		if found == 0 {
			t.Fatalf("run %d: no native code makes make's literal", run)
		}
	}
}

// With the JIT the tree is as flat, what native code keeps of a run's literals
// being no more than the sites it compiled, and its pools' objects holding
// the chunks of a few budgets.
func TestJITShapeReruns(t *testing.T) {
	testShapeReruns(t, 8*budgetSlots, func() *Runtime {
		r := New(Config{JIT: true})
		r.jitCallThreshold, r.jitICThreshold = 1, 1
		return r
	})
}
