//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"strings"
	"testing"
)

// A for...in over an array's indices is stepped by native code (mir's
// forInIndex), each key the runtime's string of its index, recorded with
// its atom in the runtime's table of key strings, and an element read by
// such a key, a[k], is native code's too (mir's keyedRead): a loop over an
// array makes one call of Go a loop, its cursor's, not two a key. Each
// answer of forInKeyReadSetup's -- elements deleted, cut off, added, made
// sparse, read through a prototype, an accessor, an arguments object, a
// typed array, a string, a proxy, keys looked up again -- is the
// interpreter's.
func TestJITForInArray(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	jitForInArrayChecked(t)
}

// TestJITForInArrayMarking is TestJITForInArray while the collector marks:
// a key's entry in the table of key strings is written through the write
// barrier, natively, so the loop makes the same calls of Go.
func TestJITForInArrayMarking(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	jitMarkingForTest(t)
	jitClobberStoresForTest(t)
	jitForInArrayChecked(t)
}

const forInArraySum = `
	var arr100 = []; for (var i = 0; i < 100; i++) arr100.push(i)
	function sumKeys(a, n) { var s = 0; for (var j = 0; j < n; j++) for (var k in a) s += a[k]; return s }`

func jitForInArrayChecked(t *testing.T) {
	t.Helper()
	rounds := []string{}
	for range 30 {
		rounds = append(rounds, `run()`, `sumKeys(arr100, 20)`)
	}
	r := jitAgreesForTest(t, forInKeyReadSetup+forInArraySum, rounds...)
	v, err := r.Run(compileForTest(t, `run()`))
	if err != nil || v.String().Go() != forInKeyReadWant {
		t.Fatalf("run() = %v, %v; want %s", v, err, forInKeyReadWant)
	}
	v, calls := jitCountGoCalls(t, r, `''+sumKeys(arr100, 1000)`)
	if v.String().Go() != "4950000" {
		t.Fatalf("sumKeys(arr100, 1000) = %v", v)
	}
	// A thousand loops of a hundred keys each: their cursors Go makes, their
	// steps and reads native code.
	if calls < 900 || calls > 1500 {
		t.Errorf("sumKeys(arr100, 1000) made %d calls of Go; want about one a loop, 1000, not two a key, 200000", calls)
	}
	e := jitNativeForTest(t, r, "sumKeys")
	if e.loopSlow {
		t.Errorf("sumKeys judged slow: %+v", e.ssaStats)
	}
	// The keys it recorded name their indices.
	for i := range 100 {
		s := r.intStrings[i]
		if s == nil {
			t.Fatalf("no string of %d", i)
		}
		if e := r.keyAtoms[keySlot(s)]; e.s == s && (e.none || e.atom != r.atoms.indexAtom(uint32(i))) {
			t.Fatalf("the key %q recorded as %+v", s.Go(), e)
		}
	}
	if got := strings.Count(forInKeyReadWant, "|"); got < 20 {
		t.Fatalf("forInKeyReadWant has %d parts", got)
	}
}
