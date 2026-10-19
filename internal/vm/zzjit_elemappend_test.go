//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "testing"

// elemAppendSetup's functions store values that are no numbers at their
// arrays' lengths -- objects, strings -- and then at what native code
// leaves for Go: a store past the length, an array that is not
// extensible or whose length is not writable, one whose length was cut
// short, whose old elements are still in its storage, and a setter on
// Array.prototype at an index.
const elemAppendSetup = `
	function fill(n) { const a = []; for (let i = 0; i < n; i++) a[i] = { v: i }; return a }
	function fillSum(n) { let s = 0; for (const o of fill(n)) s += o.v; return s }
	function strings(n) { const a = []; for (let i = 0; i < n; i++) a[a.length] = 's' + i; return a.join() }
	function gaps(n) { const a = []; for (let i = 0; i < n; i += 2) a[i] = { v: i }; return a.length + ':' + Object.keys(a).length }
	function closed(n) { const a = Object.preventExtensions([]); for (let i = 0; i < n; i++) a[i] = { v: i }; return a.length }
	function fixed(n) {
		const a = [{ v: -1 }]
		Object.defineProperty(a, 'length', { writable: false })
		for (let i = 0; i < n; i++) a[i] = { v: i }
		return a.length + ':' + a[0].v
	}
	function reused(n) {
		const a = []
		for (let i = 0; i < n; i++) a[i] = { v: i }
		a.length = 2
		for (let i = 2; i < n; i++) a[i] = 'r' + i
		return a.length + ':' + a[1].v + ':' + a[n - 1]
	}
	var hits = 0
	function setter(n) { hits = 0; fill(n); return hits }
`

// A store of a value that is no number at an array's length, where the
// array has room, is appended natively (mir's storeCell), as a number's
// is: Go had made each one, calls of it as many as the elements an array
// was filled with. Each answer is the interpreter's, and fill, the array
// grown from none, calls Go only to grow its storage.
func TestJITElemStoreAppends(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	drivers := ""
	for _, name := range []string{"fillSum", "strings", "gaps", "closed", "fixed", "reused", "setter"} {
		drivers += "function drive_" + name + "(n, a) { let r; for (let i = 0; i < n; i++) r = " + name + "(a); return r };"
	}
	rounds := []string{}
	for range 8 {
		rounds = append(rounds, `drive_fillSum(30, 64)`, `drive_strings(30, 40)`, `drive_gaps(30, 20)`, `drive_closed(30, 10)`,
			`drive_fixed(30, 10)`, `drive_reused(30, 12)`, `drive_setter(30, 4)`)
	}
	rounds = append(rounds, `(Object.defineProperty(Array.prototype, 3, { set(v) { hits++ }, configurable: true }), drive_setter(30, 8))`,
		`(delete Array.prototype[3], drive_setter(30, 8) + ':' + drive_fillSum(30, 64))`)
	r := jitAgreesForTest(t, elemAppendSetup+drivers, rounds[:len(rounds)-2]...)
	var e *jitEntry
	for k, x := range r.jit.cache {
		if fn := k.Value(); fn != nil && fn.Name == "fill" {
			e = x
		}
	}
	if e == nil || e.ssa == nil {
		t.Fatalf("fill has no native code: %+v", e)
	}
	in, calls := e.link.in, 0
	jitGoCallWork = func() { calls++ }
	defer func() { jitGoCallWork = nil }()
	if _, err := r.Run(compileForTest(t, `fillSum(256)`)); err != nil {
		t.Fatal(err)
	}
	jitGoCallWork = nil
	if e.link.in == in {
		t.Fatal("fill was not called natively")
	}
	// 256 stores, Go growing the storage from none: 1, 2, 4 ... 256.
	if calls > 12 {
		t.Errorf("fill(256) called Go %d times", calls)
	}
	jitAgreesForTest(t, elemAppendSetup+drivers, rounds...)
}
