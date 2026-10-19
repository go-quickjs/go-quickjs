//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"testing"
)

// holeSetup fills new Array(n)'s holes, as AWFY's Storage, Havlak and
// Vector do: with objects (elemStore, any value), numbers (OpElemWrite) and
// strings. The prototypes then change under the code, which must leave the
// stores to Go: a setter for an index on Array.prototype, a non-writable
// index on Object.prototype, an array no longer extensible, one of another
// prototype.
const holeSetup = `
	var objs = [{v:1}, {v:2}, {v:3}, {v:4}], seen = []
	function fill(n) {
		const a = new Array(n), b = new Array(n), c = new Array(n)
		for (let i = 0; i < n; i++) { a[i] = objs[i & 3]; b[i] = i + 0.5; c[i] = i & 1 ? 'odd' : 'even' }
		let s = 0
		for (let i = 0; i < n; i++) s += a[i].v + b[i] + c[i].length
		return s + ':' + a.length + ':' + Object.keys(b).length
	}
	function into(a, n) { for (let i = 0; i < n; i++) a[i] = objs[i & 3]; return a }
	function nums(a, n) { for (let i = 0; i < n; i++) a[i] = i * 2; return a }
	function show(a) { let s = ''; for (let i = 0; i < a.length; i++) s += (i in a ? (typeof a[i] === 'object' ? a[i].v : a[i]) : '_') + ','; return s }
	function setter() {
		Object.defineProperty(Array.prototype, 3, { set(v) { seen.push(typeof v) }, get() { return 'proto' }, configurable: true })
		const r = show(into(new Array(6), 6)) + show(nums(new Array(6), 6)) + seen.join()
		delete Array.prototype[3]
		return r
	}
	function readonly() {
		Object.defineProperty(Object.prototype, 2, { value: 'ro', writable: false, configurable: true })
		const r = show(into(new Array(5), 5)) + show(nums(new Array(5), 5))
		delete Object.prototype[2]
		return r
	}
	function sealed() {
		const a = new Array(5), b = new Array(5)
		a[1] = objs[0]; b[1] = 7
		Object.preventExtensions(a); Object.preventExtensions(b)
		return show(into(a, 5)) + show(nums(b, 5)) + Object.isExtensible(a)
	}
	function other() {
		const P = Object.create(Array.prototype)
		Object.defineProperty(P, 4, { set(v) { seen.push('P' + typeof v) }, configurable: true })
		const a = new Array(6), b = new Array(6)
		Object.setPrototypeOf(a, P); Object.setPrototypeOf(b, P)
		return show(into(a, 6)) + show(nums(b, 6)) + seen.join()
	}
`

// Native code stores into a hole of an array that may take an element
// there -- an Array, extensible, dense, its prototypes the realm's as they
// were, holding no index (mir's storeCell, appendingCall) -- as V8's code
// stores into a holey array's: a run of fill makes none of its stores a
// call of Go, as each had been (ssa.GoElemStore). Every other hole store
// is Go's still, and each answer the interpreter's.
func TestJITHoleStores(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	calls := 0
	jitGoCallWork = func() { calls++ }
	defer func() { jitGoCallWork = nil }()
	// The collector marking would have Go make calls the count is not
	// about (TestJITHoleStoresMarking runs the stores so).
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	want := New(Config{})
	t.Cleanup(func() { want.Close(); want.ReleaseClosed() })
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, holeSetup)); err != nil {
			t.Fatal(err)
		}
	}
	before := 0
	for i := range 8 {
		before = calls
		wv, err := want.Run(compileForTest(t, `fill(300)`))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, `fill(300)`))
		if err != nil || gv.String().Go() != wv.String().Go() {
			t.Fatalf("round %d: %v, %v; interpreter %v", i, gv, err, wv)
		}
	}
	// Each array's 300 stores had been as many calls of Go; what calls Go
	// is the rest: the concatenations, Object.keys.
	if calls-before >= 300 {
		t.Errorf("filling holes natively: %d calls of Go in the last run", calls-before)
	}
	jitNativeForTest(t, r, "fill")
	rounds := []string{}
	for range 8 {
		rounds = append(rounds, `fill(300)`, `show(into(new Array(9), 9))+show(nums(new Array(9), 9))`)
	}
	for range 2 {
		rounds = append(rounds, `setter()`, `readonly()`, `sealed()`, `other()`, `fill(300)`)
	}
	r = jitAgreesForTest(t, holeSetup, rounds...)
	jitNativeForTest(t, r, "into")
	jitNativeForTest(t, r, "nums")
}

// What TestJITHoleStores does while the collector marks: a reference
// stored into a hole with Go's write barrier (jit's storeRef), every
// register native code may keep given junk across it.
func TestJITHoleStoresMarking(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	jitMarkingForTest(t)
	jitClobberStoresForTest(t)
	rounds := []string{}
	for range 8 {
		rounds = append(rounds, `fill(300)`)
	}
	rounds = append(rounds, `setter()`, `readonly()`, `sealed()`, `other()`, `fill(300)`)
	jitAgreesForTest(t, holeSetup, rounds...)
}
