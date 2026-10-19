//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "testing"

// arrayLoopsSetup's functions each make a map, filter, some, every, find
// or findIndex that native code makes a loop of, as it makes a forEach's,
// its callback an arrow -- and then make it meet what the loop leaves for
// Go: holes, an array the callback shortens, a callback that throws, a
// this given, a callback's result whose truth native code does not decide
// (a symbol, a BigInt), an array whose species is not Array's.
const arrayLoopsSetup = `
	var nums = [1, 2, 3, 4, 5, 6, 7, 8]
	var objs = []
	for (let i = 0; i < 6; i++) objs.push({ x: i, name: i % 2 ? 'odd' : '' })
	var holey = [1, , 3, , , 6, 7, , 9]
	var thisObj = { k: 10 }
	function mapNums(k) { const r = nums.map((x) => x * k); return r[0] + r[7] * 100 + r.length * 10000 }
	function mapObjs(k) { return JSON.stringify(objs.map((o, i, a) => ({ y: o.x * k + i, same: a === objs }))) }
	function mapHoles(k) { const r = holey.map((x, i) => x * k + i); return r.length + ':' + Object.keys(r).join() + ':' + r.join() }
	function mapShorten(n) { const a = [1, 2, 3, 4, 5, 6]; const r = a.map((x) => { if (x === n) a.length = 3; return x }); return r.length + ':' + Object.keys(r).join() }
	function mapThrows(n) { try { return nums.map((x) => { if (x === n) throw new RangeError('at ' + x); return x }).join() } catch (e) { return e.message } }
	function mapThis(n) { return nums.map(function (x) { return this.k * x + n }, thisObj).join() }
	function mapSpecies(n) {
		const a = [1, 2, 3]
		a.constructor = { [Symbol.species]: function (len) { this.made = len } }
		return JSON.stringify(a.map((x) => x * n))
	}
	function filterEven(k) { const r = nums.filter((x) => (x * k) % 2 === 0); return r[0] + r[r.length - 1] * 100 + r.length * 10000 }
	function filterNames() { return objs.filter((o) => o.name).map((o) => o.x).join() }
	function filterHoles(k) { const r = holey.filter((x, i) => i % k === 0); return r.length + ':' + r.join() }
	function filterSymbols(n) { return nums.filter((x) => x === n ? Symbol('s') : x % 3 === 0).join() }
	function filterBigInts(n) { return nums.filter((x) => x === n ? 0n : x > 6 ? 1n : false).join() }
	function filterShorten(n) { const a = [1, 2, 3, 4, 5, 6]; return a.filter((x) => { if (x === n) a.length = 2; return true }).join() }
	function someAt(n) { let calls = 0; const r = nums.some((x) => { calls++; return x === n }); return r + ':' + calls }
	function someHoles(n) { let calls = 0; const r = holey.some((x) => { calls++; return x === n }); return r + ':' + calls }
	function someSymbol(n) { let calls = 0; const r = nums.some((x) => { calls++; return x === n ? Symbol() : 0 }); return r + ':' + calls }
	function everyBelow(n) { let calls = 0; const r = nums.every((x) => { calls++; return x < n }); return r + ':' + calls }
	function everyThis(n) { return nums.every(function (x) { return this.k > x + n }, thisObj) }
	function findObj(n) { const o = objs.find((o) => o.x === n); return o === undefined ? 'none' : o.x + o.name }
	function findHoles(n) { let calls = 0; const r = holey.find((x, i) => { calls++; return i === n }); return r + ':' + calls }
	function findShorten(n) { const a = [1, 2, 3, 4, 5, 6]; let calls = 0; const r = a.find((x) => { calls++; if (x === n) a.length = 2; return x === undefined }); return r + ':' + calls }
	function findIndexAt(n) { let calls = 0; const r = nums.findIndex((x) => { calls++; return x === n }); return r + ':' + calls }
	function findIndexHoles(n) { return holey.findIndex((x) => x === undefined && n > 0) }
	function findIndexBigInt(n) { return nums.findIndex((x) => x === n ? 1n : 0n) }
`

// A map, filter, some, every, find or findIndex on an array is a loop of
// the caller's native code, as a forEach is (ssa's forEachCall): map's
// and filter's array made by Go once (ssa.GoNewArray) and appended to
// natively, some's and the others' loops left at the first answer; what
// the loop's checks find -- a hole, an element past the array's end now,
// a callback's result whose truth is Go's to decide -- Go goes on with
// from that index, after the callback's result where the loop has it
// (jitLoopFinish). Each answer is the interpreter's, and mapNums's,
// filterEven's, someAt's and findIndexAt's loops neither leave their
// code nor call Go but to make the array.
func TestJITArrayLoops(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	names := []string{"mapNums", "mapObjs", "mapHoles", "mapShorten", "mapThrows", "mapThis", "mapSpecies",
		"filterEven", "filterNames", "filterHoles", "filterSymbols", "filterBigInts", "filterShorten",
		"someAt", "someHoles", "someSymbol", "everyBelow", "everyThis",
		"findObj", "findHoles", "findShorten", "findIndexAt", "findIndexHoles", "findIndexBigInt"}
	drivers := ""
	for _, name := range names {
		drivers += "function drive_" + name + "(n, a) { let r; for (let i = 0; i < n; i++) r = " + name + "(a); return r };"
	}
	rounds := []string{}
	for i := range 8 {
		late := []string{"99", "4"}[i/6]
		rounds = append(rounds, `drive_mapNums(30, 2)`, `drive_mapObjs(30, 3)`, `drive_mapHoles(30, 2)`, `drive_mapShorten(30, 2)`,
			`drive_mapThrows(30, `+late+`)`, `drive_mapThis(30, 1)`, `drive_mapSpecies(30, 2)`,
			`drive_filterEven(30, 1)`, `drive_filterNames(30)`, `drive_filterHoles(30, 2)`, `drive_filterSymbols(30, `+late+`)`,
			`drive_filterBigInts(30, `+late+`)`, `drive_filterShorten(30, 3)`,
			`drive_someAt(30, 5)`, `drive_someAt(30, 99)`, `drive_someHoles(30, 6)`, `drive_someSymbol(30, `+late+`)`,
			`drive_everyBelow(30, 99)`, `drive_everyBelow(30, 4)`, `drive_everyThis(30, 1)`, `drive_everyThis(30, 4)`,
			`drive_findObj(30, 3)`, `drive_findObj(30, 99)`, `drive_findHoles(30, 3)`, `drive_findShorten(30, 2)`,
			`drive_findIndexAt(30, 6)`, `drive_findIndexAt(30, 99)`, `drive_findIndexHoles(30, 1)`, `drive_findIndexHoles(30, 0)`,
			`drive_findIndexBigInt(30, `+late+`)`)
	}
	rounds = append(rounds,
		// An element of Array.prototype's where holey has none, which find
		// visits and the others skip.
		`(Array.prototype[3] = 'proto', [drive_findHoles(30, 3), drive_mapHoles(30, 2), drive_filterHoles(30, 1), drive_findIndexHoles(30, 1)].join(' '))`,
		`(delete Array.prototype[3], drive_findHoles(30, 3))`)
	// Array's species another's: map and filter are Go's, after an exit.
	species := `(Object.defineProperty(Array, Symbol.species, { get() { return function (n) { this.n = n } }, configurable: true }), JSON.stringify([drive_mapNums(30, 2), drive_filterEven(30, 1)]))`
	jitAgreesForTest(t, arrayLoopsSetup+drivers, append(rounds, species)...)
	r := jitAgreesForTest(t, arrayLoopsSetup+drivers, rounds...)
	for _, name := range []string{"mapNums", "filterEven", "someAt", "findIndexAt", "everyBelow", "findObj"} {
		var e *jitEntry
		for k, x := range r.jit.cache {
			if fn := k.Value(); fn != nil && fn.Name == name {
				e = x
			}
		}
		if e == nil || e.ssa == nil || !e.loops {
			t.Errorf("%s makes no loop of its call: %+v", name, e)
			continue
		}
		if e.ssaStats.goCalls != 0 || e.ssaStats.hosts != 0 {
			t.Errorf("%s called Go %d times, left its code %d times", name, e.ssaStats.goCalls, e.ssaStats.hosts)
		}
	}
	if r.jit.loopsFinished == 0 {
		t.Error("Go went on with no loop")
	}
}

// map's results, numbers and objects alike, are appended to the array the
// loop made natively: Go is called once a call, to make it.
func TestJITMapAppends(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitAgreesForTest(t, arrayLoopsSetup+`
		var big = []
		for (let i = 0; i < 200; i++) big.push(i)
		function mapBig() { return big.map((x) => ({ x })).length + big.filter((x) => x % 4 === 0).length }
		function drive(n) { let r; for (let i = 0; i < n; i++) r = mapBig(); return r }`,
		`drive(30)`, `drive(30)`, `drive(30)`)
	var e *jitEntry
	for k, x := range r.jit.cache {
		if fn := k.Value(); fn != nil && fn.Name == "mapBig" {
			e = x
		}
	}
	if e == nil || e.ssa == nil || !e.loops {
		t.Fatalf("mapBig makes no loops: %+v", e)
	}
	in, calls := e.link.in, 0
	jitGoCallWork = func() { calls++ }
	defer func() { jitGoCallWork = nil }()
	if _, err := r.Run(compileForTest(t, `drive(10)`)); err != nil {
		t.Fatal(err)
	}
	jitGoCallWork = nil
	if e.link.in-in < 10 {
		t.Fatalf("mapBig was called natively %d times of 10", e.link.in-in)
	}
	// Each call: map's array, filter's, and filter's growing, 1, 2, 4 ...
	// 64 -- not an element's.
	if calls > 10*10 {
		t.Errorf("10 calls of mapBig called Go %d times", calls)
	}
}

// map's and filter's stores into the array the loop made define their
// elements, as CreateDataProperty does, whatever Array.prototype holds
// (ssa.StoreFresh): a setter at an index there is not called, and the
// loops go on natively past it. Each answer is the interpreter's.
func TestJITMapDefinesPastSetters(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `
		var hits = 0, big = []
		for (let i = 0; i < 40; i++) big.push(i)
		function mapped() { const r = big.map((x) => ({ x })); return r.length + ':' + Object.hasOwn(r, 2) + ':' + r[2].x }
		function kept() { const r = big.filter((x) => x % 2 === 0); return r.length + ':' + Object.hasOwn(r, 2) + ':' + r[2] }
		function drive(n) { let r; for (let i = 0; i < n; i++) r = mapped() + ' ' + kept(); return r }`
	r := jitAgreesForTest(t, setup, `drive(30)`, `drive(30)`, `drive(30)`,
		`(Object.defineProperty(Array.prototype, 2, { set(v) { hits++ }, get() { return 'p' }, configurable: true }), 'set')`)
	var entries []*jitEntry
	for k, x := range r.jit.cache {
		if fn := k.Value(); fn != nil && (fn.Name == "mapped" || fn.Name == "kept") && x.ssa != nil && x.loops {
			entries = append(entries, x)
		}
	}
	if len(entries) != 2 {
		t.Fatalf("%d of mapped and kept make loops", len(entries))
	}
	before := r.jit.loopsFinished
	v, err := r.Run(compileForTest(t, `drive(30) + ':' + hits`))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := v.String().Go(), "40:true:2 20:true:4:0"; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
	// The first calls' code compiled before the setter, whose prototypes'
	// shapes it checks, compiled again.
	if n := r.jit.loopsFinished - before; n > 4 {
		t.Errorf("Go finished %d loops past a setter of Array.prototype's", n)
	}
}
