//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "testing"

// typedMapFilterSetup's functions each make a typed array's map or filter
// that native code makes a loop of (ssa's forEachCall, abi.LoopTyped with
// abi.LoopMap or abi.LoopFilter), and then make it meet what the loop
// leaves for Go: a callback's result that is no number -- a string, an
// object whose valueOf runs, logged -- converted as a store converts it;
// a Uint8ClampedArray's clamping and a Float32Array's rounding; a view the
// callback shrinks, map's elements past its end undefined, filter's kept
// copied after; a subclass's instance and a species of the script's own,
// whose constructors make the result; a constructor given to one typed
// array; a BigInt64Array, Go's.
const typedMapFilterSetup = `
	var i32 = new Int32Array([5, -3, 8, 13, 21, -1, 7, 2])
	var f64 = new Float64Array([0.5, 1.25, -2, 3.75, 10])
	var u8c = new Uint8ClampedArray([200, 1, 2, 255, 3])
	var f32 = new Float32Array([0.1, 1.5, -2.25, 1e10])
	var u32 = new Uint32Array([1, 4000000000, 3])
	var log = []
	function mapTwice(t) { let _k = 0; return t.map(function mapCb(x, i, a) { return x * 2 + i + (a === t ? _k : 1) }).join() }
	function mapClamp(t) { return t.map((x) => x * 3 - 100).join() }
	function mapRound(t) { return t.map((x) => x / 3).join() }
	function mapOther(t, n) { log = []; return t.map((x, i) => i === n ? { valueOf() { log.push('v' + i); return 7 } } : i === n + 1 ? '12' : x).join() + '|' + log.join() }
	function mapType(t) { const r = t.map((x) => x); return r.constructor.name + ':' + r.length + ':' + (r.buffer !== t.buffer) }
	function filterOdd(t) { let _k = 0; return t.filter(function filterCb(x, i) { return ((x | 0) & 1) === 1 + _k }).join() }
	function filterType(t) { const r = t.filter((x, i) => i % 2 === 0); return r.constructor.name + ':' + r.length + ':' + r.join() }
	function filterNone(t) { return t.filter(() => false).length }
	function shrinkMap(n) {
		const buf = new ArrayBuffer(32, { maxByteLength: 64 })
		const t = new Int32Array(buf, 0, 8)
		for (let i = 0; i < t.length; i++) t[i] = i + 1
		return t.map((x, i) => { if (i === n) buf.resize(8); return x === undefined ? -1 : x * 10 }).join()
	}
	function shrinkFilter(n) {
		const buf = new ArrayBuffer(32, { maxByteLength: 64 })
		const t = new Int32Array(buf, 0, 8)
		for (let i = 0; i < t.length; i++) t[i] = i + 1
		return t.filter((x, i) => { if (i === n) buf.resize(8); return x !== 2 }).join()
	}
	class Mine extends Int32Array {}
	var mine = new Mine([1, 2, 3])
	function mapEither(t) { const r = t.map((x) => x + 1); return r.constructor.name + ':' + r.join() }
	function filterEither(t) { const r = t.filter((x) => x > 1); return r.constructor.name + ':' + r.join() }
	var own = new Int32Array([4, 5, 6])
	function ownCtor() { own.constructor = Mine; return mapEither(own) + ';' + filterEither(own) }
	var big = new BigInt64Array([1n, 2n, 3n])
	function bigMap() { return big.map((x) => x * 2n).join() + ';' + big.filter((x) => x !== 2n).join() }
	function throwsMap(t, n) { try { return t.map((x, i) => { if (i === n) throw new RangeError('at ' + i); return x }).join() } catch (e) { return e.name + ' ' + e.message } }
	function badResult(t, n) { try { return t.map((x, i) => i === n ? 1n : x).join() } catch (e) { return e.name } }
`

// A typed array's map and filter are loops of the caller's native code, as
// an array's are: map's result a typed array of the receiver's type Go
// makes before the loop, where the species is the type's own constructor
// as it was made, each callback's result stored in it as a number
// (OpElemWrite) -- anything else Go's to convert and store, the loop going
// on from there in Go (jitLoopFinish); filter's elements kept appended to
// an Array, of which Go makes the typed array after the loop, as the
// method makes it then. Each answer is the interpreter's, and mapTwice's
// and filterOdd's loops neither leave their code nor call their callbacks
// natively, which are inlined.
func TestJITTypedArrayMapFilter(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "marking"}[marking], func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			testJITTypedArrayMapFilter(t)
		})
	}
}

func testJITTypedArrayMapFilter(t *testing.T) {
	names := []string{"mapTwice", "mapClamp", "mapRound", "mapOther", "mapType", "filterOdd", "filterType", "filterNone",
		"shrinkMap", "shrinkFilter", "mapEither", "filterEither", "ownCtor", "bigMap", "throwsMap", "badResult"}
	drivers := ""
	for _, name := range names {
		drivers += "function drive_" + name + "(n, a, b) { let r; for (let i = 0; i < n; i++) r = " + name + "(a, b); return r };"
	}
	rounds := []string{}
	for i := range 6 {
		late := []string{"99", "2"}[i/4]
		rounds = append(rounds, `drive_mapTwice(30, i32)`, `drive_mapTwice(30, f64)`, `drive_mapClamp(30, u8c)`, `drive_mapRound(30, f32)`,
			`drive_mapRound(30, u32)`, `drive_mapOther(30, i32, `+late+`)`, `drive_mapType(30, f64)`, `drive_filterOdd(30, i32)`,
			`drive_filterOdd(30, u32)`, `drive_filterType(30, u8c)`, `drive_filterNone(30, f64)`, `drive_shrinkMap(30, `+late+`)`,
			`drive_shrinkFilter(30, `+late+`)`, `drive_mapEither(30, i32)`, `drive_filterEither(30, i32)`, `drive_bigMap(30)`,
			`drive_throwsMap(30, i32, `+late+`)`, `drive_badResult(30, i32, `+late+`)`)
	}
	rounds = append(rounds, `drive_mapEither(30, mine)`, `drive_filterEither(30, mine)`, `drive_mapEither(30, i32)`, `drive_ownCtor(3)`,
		`drive_mapEither(30, i32)`, `drive_filterEither(30, i32)`)
	r := jitAgreesForTest(t, typedMapFilterSetup+drivers, rounds...)
	// The answers themselves, as Node gives them.
	for _, c := range []struct{ src, want string }{
		{`mapTwice(i32)`, "10,-5,18,29,46,3,20,11"},
		{`mapClamp(u8c)`, "255,0,0,255,0"},
		{`mapRound(f32)`, "0.03333333507180214,0.5,-0.75,3333333248"},
		{`mapOther(i32, 2)`, "5,-3,7,12,21,-1,7,2|v2"},
		{`filterOdd(u32)`, "1,3"},
		{`filterType(u8c)`, "Uint8ClampedArray:3:200,2,3"},
		{`shrinkMap(2)`, "10,20,30,-1,-1,-1,-1,-1"},
		{`shrinkFilter(2)`, "1,3,0,0,0,0,0"},
		{`mapEither(mine)`, "Mine:2,3,4"},
		{`ownCtor()`, "Mine:5,6,7;Mine:4,5,6"},
		{`bigMap()`, "2,4,6;1,3"},
		{`badResult(i32, 1)`, "TypeError"},
	} {
		v, err := r.Run(compileForTest(t, c.src))
		if err != nil {
			t.Fatalf("%s: %v", c.src, err)
		}
		if got := v.String().Go(); got != c.want {
			t.Errorf("%s = %s, want %s", c.src, got, c.want)
		}
	}
	entry := func(name string) *jitEntry {
		for k, x := range r.jit.cache {
			if fn := k.Value(); fn != nil && fn.Name == name {
				return x
			}
		}
		return nil
	}
	if r.jit.loopsFinished == 0 {
		t.Error("Go went on with no loop")
	}
	for _, c := range []struct{ fn, cb, run string }{
		{"mapTwice", "mapCb", "drive_mapTwice(10, i32)"}, {"filterOdd", "filterCb", "drive_filterOdd(10, i32)"},
	} {
		e, cb := entry(c.fn), entry(c.cb)
		if e == nil || e.ssa == nil || !e.loops || cb == nil || cb.link == nil {
			t.Errorf("%s makes no loop of its call: %+v", c.fn, e)
			continue
		}
		hosts, in, finished := e.ssaStats.hosts, cb.link.in, r.jit.loopsFinished
		if _, err := r.Run(compileForTest(t, c.run)); err != nil {
			t.Fatal(err)
		}
		if n := cb.link.in - in; n != 0 || len(e.ssaInlined) == 0 {
			t.Errorf("%s's callback called natively %d times, not inlined", c.fn, n)
		}
		if n := e.ssaStats.hosts - hosts; n != 0 {
			t.Errorf("%s left its code %d times", c.fn, n)
		}
		if n := r.jit.loopsFinished - finished; n != 0 {
			t.Errorf("Go finished %s's loop %d times", c.fn, n)
		}
	}
	for _, name := range []string{"mapOther", "shrinkMap", "shrinkFilter", "throwsMap", "badResult"} {
		if e := entry(name); e == nil || e.ssa == nil || !e.loops {
			t.Errorf("%s makes no loop of its call: %+v", name, e)
		}
	}
}
