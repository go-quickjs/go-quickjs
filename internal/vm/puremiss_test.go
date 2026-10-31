package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// A pure body's misses are its template's (closureCode.pureMiss): a
// callback made anew at each call of its maker, whose read of a getter
// misses at every call, stops being tried once its template has missed
// pureMissLimit times, and a closure made after is planned its tree's call,
// as one made once is. Counted per closure, each was tried and given up on
// at every call, then made with a frame.
func TestPureMissPerTemplate(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, `
		var last, o = { get g() { return 1; } };
		function each(f, n) { var s = 0; for (var i = 0; i < n; i++) s += f(i); return s }
		function maker(k) { last = function (i) { return o.g; }; return each(last, 3) + k; }
		var t = 0;
		for (var j = 0; j < 200; j++) t += maker(j);
		[t, last]`))
	if err != nil {
		t.Fatal(err)
	}
	elems := v.Object().elemsOf()
	if got := elems[0].Number(); got != 20500 {
		t.Errorf("sum %v, want 20500", got)
	}
	fd := elems[1].Object().fn()
	if fd.closure.fn.Leaf != bytecode.LeafPure {
		t.Fatalf("the callback is leaf %d, not pure", fd.closure.fn.Leaf)
	}
	if n := fd.closure.pureMiss; n < pureMissLimit {
		t.Errorf("the callback's template missed %d times, want %d", n, pureMissLimit)
	}
	if fd.treeCall == nil {
		t.Errorf("the last callback made has no tree's call")
	}
}
