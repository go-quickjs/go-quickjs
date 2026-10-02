package vm

import "testing"

// TestPureStoreRefusalIsNotAMiss pins that a storing pure body called where
// its caller could still give up -- so that its store is refused -- is not
// counted as missing its fast paths: called directly, where it may store, it
// is still answered without a frame. Richards's markAsSuspended, called from
// suspendCurrent, lost its frameless calls everywhere when it was.
func TestPureStoreRefusalIsNotAMiss(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, `
		function T() { this.state = 0 }
		T.prototype.mark = function () { this.state = this.state | 2 };
		function S(t) { this.cur = t }
		S.prototype.suspend = function () { this.cur.mark(); return this.cur };
		var t = new T(), s = new S(t);
		for (var i = 0; i < 200; i++) s.suspend();
		for (var i = 0; i < 10; i++) t.mark();
		T.prototype.mark`))
	if err != nil {
		t.Fatal(err)
	}
	cl := v.Object().fn().closure
	// The first call misses once, before the store's cache is filled.
	if cl.pureMiss > 2 {
		t.Errorf("mark missed %d times; a refused store should not count", cl.pureMiss)
	}
}
