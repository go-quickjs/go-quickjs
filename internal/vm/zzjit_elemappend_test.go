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

// A store with checks -- an element read from an array, a reference, live
// past it (ssa's storeChecks) -- appends natively where the array has room,
// its checks made on the cell before the length grows, and is Go's from
// native code where it grows the array or meets anything else
// (GoElemStore, GoElemWrite), unless a live reference was read from that
// array's storage, which Go's store could write over or move: it exits
// then. Typescript's getParameterList, filling result from [] with
// result[result.length] = parameterSymbol, a parameter read from
// args.members live past the store, had left native code at each. The
// references read from the array itself -- one the append's cell holds
// after the array was cut short, one into storage the append grows -- keep
// their values: each answer is the interpreter's. In the warm round
// params leaves native code at no store.
func TestJITCheckedStoresAppend(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `
		function P(n) { this.n = n; this.opt = n & 1 }
		P.prototype.isOpt = function () { return this.opt == 1 }
		function params(args) {
			var result = [], nums = [], non = 0
			for (var i = 0; i < args.members.length; i++) {
				var parameter = args.members[i]
				result[result.length] = { p: parameter, k: i }
				nums[nums.length] = i * 2
				if (!parameter.isOpt()) non++
			}
			return result.length * 10 + nums.length + non
		}
		var lists = []
		for (let i = 0; i < 8; i++) { const m = []; for (let j = 0; j <= i % 4; j++) m.push(new P(j)); lists.push({ members: m }) }
		function all(n) { let t = 0; for (let k = 0; k < n; k++) for (const l of lists) t = (t + params(l)) | 0; return t }
		function cut(a, v) { var x = a[3]; a.length = 3; a[a.length] = v; return x.n + ':' + a[3].n + ':' + a.length }
		function grow(a, v) { var x = a[0]; a[a.length] = v; a[a.length] = v; return x.n + ':' + a.length + ':' + a[a.length - 1].n }
		function own(a) { var x = a[0]; for (var i = 0; i < 40; i++) { a[a.length] = x } return x.n + ':' + a.length }
		function cuts(n) { let s = ''; for (let i = 0; i < n; i++) s = cut([new P(1), new P(2), new P(3), new P(4)], new P(i)); return s }
		function grows(n) { let s = ''; for (let i = 0; i < n; i++) s = grow([new P(7)], new P(i)); return s }
		function owns(n) { let s = ''; for (let i = 0; i < n; i++) s = own([new P(i)]); return s }
	`
	rounds := []string{}
	for range 6 {
		rounds = append(rounds, `all(100)`, `cuts(50)`, `grows(50)`, `owns(20)`)
	}
	r := jitAgreesForTest(t, setup, rounds...)
	e := jitNativeOrCalleeForTest(t, r, "params")
	hosts := e.ssaStats.hosts
	if _, err := r.Run(compileForTest(t, `all(100)`)); err != nil {
		t.Fatal(err)
	}
	if n := e.ssaStats.hosts - hosts; n != 0 {
		t.Errorf("params left native code %d times", n)
	}
}
