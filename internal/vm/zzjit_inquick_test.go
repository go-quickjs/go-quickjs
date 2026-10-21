//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "testing"

// TestJITInOperator makes inOperatorSetup's cases, each a few times, in a
// runtime that compiles at the first call: native code's in, which Go
// answers (ssa.GoHas, jitHas, hasQuick) but where the object is an array
// and the key an index, which native code answers itself (mir's keyedHas)
// -- true for a dense element, false for none where the array's prototypes
// are as they were -- answers as the interpreter does and as Node does,
// while the prototypes gain and lose indices (changing). Warm, holes'
// loop, which asks of an array's elements, a hole, an index past them and
// a negative one, runs natively and calls no Go.
func TestJITInOperator(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	var rounds []string
	for range 4 {
		for _, c := range inOperatorCases {
			rounds = append(rounds, c.src)
		}
	}
	r := jitAgreesForTest(t, inOperatorSetup, rounds...)
	for _, name := range []string{"table", "holes", "changing", "mixed", "atoms", "fresh", "far", "sparseLoop"} {
		jitNativeForTest(t, r, name)
	}
	for _, c := range inOperatorCases {
		v, err := r.Run(compileForTest(t, "''+("+c.src+")"))
		if err != nil {
			t.Fatalf("%s: %v", c.src, err)
		}
		if got := v.String().Go(); got != c.want {
			t.Fatalf("%s:\n got %q\nwant %q", c.src, got, c.want)
		}
	}
	// In a runtime whose prototypes have never had an index -- an index
	// once in a table is remembered there (noIndexKeys), and no native
	// code answers an absent index with them -- warm.
	r = jitRuntimeForTest(t, Config{JIT: true})
	if _, err := r.Run(compileForTest(t, inOperatorSetup+"; holes(200); holes(200); sparseLoop(200); sparseLoop(200)")); err != nil {
		t.Fatal(err)
	}
	jitNativeForTest(t, r, "holes")
	// A sparse array, whose table has indices: Go answers them.
	jitNativeForTest(t, r, "sparseLoop")
	if v, err := r.Run(compileForTest(t, `''+sparseLoop(10)`)); err != nil || v.String().Go() != "110" {
		t.Errorf("sparseLoop(10) = %v (%v), want 110", v, err)
	}
	// Its 3,000 in operators call no Go: the calls it makes are the
	// delete that makes the array's hole (ssa.GoDelete) and, at most one,
	// the loop's poll at its back edge (ssa.GoPoll).
	v, calls := jitCountGoCalls(t, r, `''+holes(1000)`)
	if v.String().Go() != "875" || calls > 2 {
		t.Errorf("holes(1000) = %s with %d calls of Go, want 875 with the delete's and the poll's at most", v.String().Go(), calls)
	}
	// Object.prototype with indices past what an atom of an index carries,
	// which its table has as names (noIndexKeys does not tell of them),
	// before far is compiled: Go answers them.
	r2 := jitRuntimeForTest(t, Config{JIT: true})
	if _, err := r2.Run(compileForTest(t, inOperatorSetup+"; Object.prototype[3000000000] = 1; Object.prototype[2147483648] = 1; far(200); far(200)")); err != nil {
		t.Fatal(err)
	}
	jitNativeForTest(t, r2, "far")
	if v, err := r2.Run(compileForTest(t, `''+far(10)`)); err != nil || v.String().Go() != "10110" {
		t.Errorf("far(10) with Object.prototype's 3000000000 and 2147483648 = %v (%v), want 10110", v, err)
	}
	// Array.prototype given an index, which is in its elements: the code
	// notices, and Go answers.
	v, _ = jitCountGoCalls(t, r, `Array.prototype[3] = 1; Array.prototype[12] = 1; ''+holes(1000)`)
	if v.String().Go() != "101000" {
		t.Errorf("holes(1000) with Array.prototype's 3 and 12 = %s, want 101000", v.String().Go())
	}
}
