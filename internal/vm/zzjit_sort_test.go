//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"strings"
	"testing"
)

// sortSetup's functions each sort a copy of a typed array of a number
// type or of an Array of numbers by a comparator: ones native code sorts
// itself (ssa's sortCall, abi.LoopSort) -- a comparator of parameters,
// constants, arithmetic, comparisons and forward branches, an arrow, a
// function or a method, ascending, descending, by a branch, by a shift
// that makes ties, one that answers NaN, 0, a boolean or undefined, one
// that is not consistent, over -0, the infinities and NaN, each element
// type's rounding and clamping, a thousand elements -- and the ones it
// leaves to Go: a comparator that is a class, makes a string, reads a
// global, a captured binding or a property, calls, or stores; a
// BigInt64Array; an Array with a hole, undefined, a string, an element
// its prototype has, a getter; a frozen Array, a comparator that is not
// one, a detached view, one that tracks its buffer, a subclass's
// instance, an Array of another prototype; lengths of 0 to 17.
const sortSetup = `
	var i32 = new Int32Array([5, -3, 8, 13, 21, -1, 7, 2, 0, 13, -3, 100, 42, 6])
	var f64 = new Float64Array([0.5, -0, 1.25, 0, -Infinity, -2, 3.75, Infinity, 10, -0, 0])
	var f32 = new Float32Array([0.1, 1.5, -2.25, 1e10, -0, 3.5, 0.1])
	var u8c = new Uint8ClampedArray([200, 1, 2, 255, 3, 0, 2])
	var i8 = new Int8Array([-128, 127, 0, -1, 1, 64, -64])
	var u8 = new Uint8Array([9, 3, 250, 3, 7, 0])
	var i16 = new Int16Array([-32768, 32767, 5, -5, 0])
	var u16 = new Uint16Array([65535, 1, 300, 2])
	var u32 = new Uint32Array([4000000000, 1, 3, 2147483648, 0])
	var nums = [5, -3, 8.5, 13, -0, 21, 0, -1, 7, 2, Infinity, -Infinity, 1e300, -1e-300]
	var ties = new Int32Array([0x31, 0x12, 0x35, 0x10, 0x3f, 0x11, 0x20, 0x2f, 0x13, 0x30])
	var tiesArr = [0x31, 0x12, 0x35, 0x10, 0x3f, 0x11, 0x20, 0x2f, 0x13, 0x30]
	var withNaN = new Float64Array([3, NaN, 1, NaN, 2, -0, 0])
	var c_i32 = new Int32Array(i32.length), c_f64 = new Float64Array(f64.length), c_f32 = new Float32Array(f32.length)
	var c_u8c = new Uint8ClampedArray(u8c.length), c_i8 = new Int8Array(i8.length), c_u8 = new Uint8Array(u8.length)
	var c_i16 = new Int16Array(i16.length), c_u16 = new Uint16Array(u16.length), c_u32 = new Uint32Array(u32.length)
	var c_ties = new Int32Array(ties.length), c_nan = new Float64Array(withNaN.length)
	var long = new Int32Array(1000), longArr = new Array(1000), c_long = new Int32Array(1000)
	for (let i = 0, x = 12345; i < 1000; i++) { x = (x * 1103515245 + 12345) % 2147483648; long[i] = x % 500 - 250; longArr[i] = (x % 7919) / 8 }
	var log = []
	function show(t) { return typeof t === 'string' ? t : Array.prototype.map.call(t, (x) => Object.is(x, -0) ? '-0' : '' + x).join() }
	function up(a, b) { return a - b }
	function down(a, b) { return b - a }
	function up_i32(t, c) { c.set(t); c.sort((a, b) => a - b); return c }
	function down_i32(t, c) { c.set(t); c.sort((a, b) => b - a); return c }
	function up_f64(t, c) { c.set(t); c.sort((a, b) => a - b); return c }
	function down_f64(t, c) { c.set(t); c.sort((a, b) => b - a); return c }
	function up_f32(t, c) { c.set(t); c.sort((a, b) => a - b); return c }
	function down_f32(t, c) { c.set(t); c.sort((a, b) => b - a); return c }
	function up_u8c(t, c) { c.set(t); c.sort((a, b) => a - b); return c }
	function down_u8c(t, c) { c.set(t); c.sort((a, b) => b - a); return c }
	function up_i8(t, c) { c.set(t); c.sort((a, b) => a - b); return c }
	function down_i8(t, c) { c.set(t); c.sort((a, b) => b - a); return c }
	function up_u8(t, c) { c.set(t); c.sort((a, b) => a - b); return c }
	function down_u8(t, c) { c.set(t); c.sort((a, b) => b - a); return c }
	function up_i16(t, c) { c.set(t); c.sort((a, b) => a - b); return c }
	function down_i16(t, c) { c.set(t); c.sort((a, b) => b - a); return c }
	function up_u16(t, c) { c.set(t); c.sort((a, b) => a - b); return c }
	function down_u16(t, c) { c.set(t); c.sort((a, b) => b - a); return c }
	function up_u32(t, c) { c.set(t); c.sort((a, b) => a - b); return c }
	function down_u32(t, c) { c.set(t); c.sort((a, b) => b - a); return c }
	function up_nums(t) { const c = t.slice(); c.sort((a, b) => a - b); return c }
	function down_nums(t) { const c = t.slice(); c.sort((a, b) => b - a); return c }
	function sortUp(t) { const c = t.slice(); c.sort((a, b) => a - b); return show(c) }
	function sortDown(t) { const c = t.slice(); c.sort((a, b) => b - a); return show(c) }
	function sortBranch(t, c) { c.set(t); return c.sort((a, b) => a < b ? -1 : a > b ? 1 : 0) }
	function sortBranchArr(t) { return t.slice().sort((a, b) => a < b ? -1 : a > b ? 1 : 0) }
	var cmpObj = { cmp(a, b) { return a - b } }
	function sortMethod(t, c) { c.set(t); return c.sort(cmpObj.cmp) }
	function sortClass(t, c) { c.set(t); try { c.sort(class {}); return 'no' } catch (e) { return e.name } }
	function sortWithUp(t, c) { c.set(t); return c.sort(up) }
	function sortWith(t, f) { const c = t.slice(); return show(c.sort(f)) }
	function sortTies(t, c) { c.set(t); return c.sort((a, b) => (a >> 4) - (b >> 4)) }
	function sortTiesArr(t) { return t.slice().sort((a, b) => (a >> 4) - (b >> 4)) }
	function sortNaNResult(t, c) { c.set(t); return c.sort((a, b) => 0 / 0) }
	function sortNaNGlobal(t, c) { c.set(t); return c.sort((a, b) => NaN) }
	function sortZero(t) { return t.slice().sort(() => 0) }
	function sortInPlace(t) { const r = t.sort((a, b) => a - b); return (r === t) + ':' + show(t) }
	function sortBool(t, c) { c.set(t); return c.sort((a, b) => a > b) }
	function sortSometimes(t, c) { c.set(t); return c.sort((a, b) => { if (a < b) return -1; if (a > b) return 1 }) }
	function sortString(t, c) { c.set(t); return c.sort((a, b) => a < b ? '-1' : '1') }
	function sortNaNLast(t, c) { c.set(t); return c.sort((a, b) => (a !== a) - (b !== b) || a - b) }
	function digest(c) { let h = 0; for (let i = 0; i < c.length; i++) h = (h * 31 + c[i] * 8) | 0; return c.length + ':' + c[0] + ',' + c[c.length - 1] + ':' + h }
	function sortLong(t, c) { c.set(t); return digest(c.sort((a, b) => b - a)) }
	function sortLongTies(t, c) { c.set(t); return digest(c.sort((a, b) => (a & 7) - (b & 7))) }
	function sortLongArr(t) { return digest(t.slice().sort((a, b) => a - b)) }
	function multiset(t) { return Array.prototype.slice.call(t).map((x) => Object.is(x, -0) ? '-0' : '' + x).sort().join() }
	function sortWild(t, c) { c.set(t); c.sort((a, b) => (a * 7 + b * 3) % 5 - 2); return c.length + ':' + (multiset(c) === multiset(t)) }
	function sortWildArr(t) { const c = t.slice(); c.sort((a, b) => (a * 7 + b * 3) % 5 - 2); return c.length + ':' + (multiset(c) === multiset(t)) }
	function sortNaNs(t, c) { c.set(t); c.sort((a, b) => a - b); return c.length + ':' + (multiset(c) === multiset(t)) }
	function sortSmall(n) { const c = new Int32Array(n); for (let i = 0; i < n; i++) c[i] = n - i; const a = Array.from(c); c.sort((x, y) => x - y); a.sort((x, y) => x - y); return show(c) + '/' + show(a) }
	var k = 1
	function sortCaptured(t, c) { c.set(t); return c.sort((a, b) => (a - b) * k) }
	function sortProp(t, c) { const o = { s: -1 }; c.set(t); return c.sort((a, b) => (a - b) * o.s) }
	function sortCall(t, c) { c.set(t); return c.sort((a, b) => Math.abs(a) - Math.abs(b)) }
	function sortLogs(t, c) { log = []; c.set(t); c.sort((a, b) => { log.push(1); return a - b }); return show(c) + ':' + (log.length > 0) }
	var big = new BigInt64Array([3n, -1n, 2n, 0n])
	function sortBig() { const c = big.slice(); return c.sort((a, b) => a < b ? -1 : a > b ? 1 : 0).join() }
	function sortHoles() { const a = [3, , 1, 2]; a.sort((x, y) => x - y); return a.length + ':' + a.join() + ':' + (3 in a) }
	function sortUndef() { const a = [3, undefined, 1, 2]; a.sort((x, y) => x - y); return a.join() + ':' + (a[3] === undefined) }
	function sortStrings() { const a = ['3', 1, '2', 10]; a.sort((x, y) => x - y); return a.join() }
	function sortInherited() { Array.prototype[1] = 7; try { const a = [3, , 1]; a.sort((x, y) => x - y); return a.join() + ':' + a.hasOwnProperty(1) } finally { delete Array.prototype[1] } }
	function sortGetter() { const a = [3, 2, 1]; let n = 0; Object.defineProperty(a, 1, { get() { n++; return 5 }, set(v) { n += 10 }, configurable: true }); a.sort((x, y) => x - y); return n + ':' + a[0] + ',' + a[2] }
	function sortFrozen() { const a = Object.freeze([3, 1, 2]); try { a.sort((x, y) => x - y); return 'no' } catch (e) { return e.name } }
	function sortNotFn() { try { new Int32Array([2, 1]).sort(3); return 'no' } catch (e) { return e.name } }
	function sortDetached() { const t = new Int32Array([2, 1]); t.buffer.transfer(); try { t.sort((a, b) => a - b); return 'no' } catch (e) { return e.name } }
	function sortTracking() { const b = new ArrayBuffer(16, { maxByteLength: 32 }); const t = new Int32Array(b); t.set([4, 1, 3, 2]); t.sort((a, b) => a - b); return show(t) }
	class Mine extends Int32Array {}
	var mine = new Mine([3, 1, 2])
	function sortMine() { const c = new Mine(mine); c.sort((a, b) => a - b); return c.constructor.name + ':' + show(c) }
	function sortSub() { const c = [3, 1, 2]; Object.setPrototypeOf(c, Object.create(Array.prototype)); Array.prototype.sort.call(c, (a, b) => a - b); return show(c) }
`

// A typed array's sort and an Array's, by a comparator that does nothing
// but compute from its arguments (jitSortComparator), are the caller's
// native code: a merge sort, the comparator inlined, of a copy Go makes
// and writes back after (jitSortCopy). Each answer is the interpreter's
// and, but for a comparator that is not consistent, whose order is the
// engine's, Node's; and the sorts of numbers neither leave their code nor
// have Go finish or restart them.
func TestJITSortNative(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "marking"}[marking], func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			testJITSortNative(t)
		})
	}
}

func testJITSortNative(t *testing.T) {
	r, drive := jitSortsForTest(t, jitStressConfig{})
	entry := func(name string) *jitEntry {
		for k, x := range r.jit.cache {
			if fn := k.Value(); fn != nil && fn.Name == name {
				return x
			}
		}
		return nil
	}
	for _, c := range []struct{ fn, src string }{
		{"up_i32", "up_i32(i32, c_i32)"}, {"down_i32", "down_i32(i32, c_i32)"},
		{"up_f64", "up_f64(f64, c_f64)"}, {"down_f64", "down_f64(f64, c_f64)"},
		{"up_f32", "up_f32(f32, c_f32)"}, {"down_f32", "down_f32(f32, c_f32)"},
		{"up_u8c", "up_u8c(u8c, c_u8c)"}, {"down_u8c", "down_u8c(u8c, c_u8c)"},
		{"up_i8", "up_i8(i8, c_i8)"}, {"down_i8", "down_i8(i8, c_i8)"},
		{"up_u8", "up_u8(u8, c_u8)"}, {"down_u8", "down_u8(u8, c_u8)"},
		{"up_i16", "up_i16(i16, c_i16)"}, {"down_i16", "down_i16(i16, c_i16)"},
		{"up_u16", "up_u16(u16, c_u16)"}, {"down_u16", "down_u16(u16, c_u16)"},
		{"up_u32", "up_u32(u32, c_u32)"}, {"down_u32", "down_u32(u32, c_u32)"},
		{"up_nums", "up_nums(nums)"}, {"down_nums", "down_nums(nums)"},
		{"sortBranch", "sortBranch(f64, c_f64)"}, {"sortBranchArr", "sortBranchArr(nums)"},
		{"sortWithUp", "sortWithUp(i32, c_i32)"}, {"sortTies", "sortTies(ties, c_ties)"},
		{"sortTiesArr", "sortTiesArr(tiesArr)"}, {"sortNaNResult", "sortNaNResult(i32, c_i32)"},
		{"sortZero", "sortZero(nums)"}, {"sortInPlace", "sortInPlace(new Int32Array([3, 1, 2]))"},
		{"sortBool", "sortBool(i32, c_i32)"}, {"sortSometimes", "sortSometimes(i32, c_i32)"},
		{"sortWild", "sortWild(i32, c_i32)"}, {"sortWildArr", "sortWildArr(nums)"},
		{"sortNaNs", "sortNaNs(withNaN, c_nan)"}, {"sortNaNLast", "sortNaNLast(withNaN, c_nan)"},
		{"sortLong", "sortLong(long, c_long)"}, {"sortLongTies", "sortLongTies(long, c_long)"},
		{"sortLongArr", "sortLongArr(longArr)"}, {"sortMethod", "sortMethod(i32, c_i32)"},
	} {
		e := entry(c.fn)
		if e == nil || e.ssa == nil || !e.loops || len(e.ssaInlined) == 0 {
			t.Errorf("%s makes no sort of its own: %+v", c.fn, e)
			continue
		}
		hosts, finished, restarted := e.ssaStats.hosts, r.jit.loopsFinished, r.jit.sortsRestarted
		if _, err := r.Run(compileForTest(t, drive(c.src, "10"))); err != nil {
			t.Fatal(err)
		}
		if n := e.ssaStats.hosts - hosts; n != 0 {
			t.Errorf("%s left its code %d times", c.src, n)
		}
		if n := r.jit.loopsFinished - finished; n != 0 {
			t.Errorf("%s: Go finished the sort %d times", c.src, n)
		}
		if n := r.jit.sortsRestarted - restarted; n != 0 {
			t.Errorf("%s: Go sorted again %d times", c.src, n)
		}
	}
	// A comparator that is not a body to call again at will: Go's sort.
	for _, name := range []string{"sortString", "sortClass", "sortNaNGlobal", "sortCaptured", "sortProp", "sortCall", "sortLogs", "sortBig"} {
		if e := entry(name); e != nil && e.loops {
			t.Errorf("%s sorts natively", name)
		}
	}
}

// An exit from a sort native code makes, anywhere in it -- here a back
// edge's budget run out, finished in the interpreter (QJS_JIT_STRESS's
// deopt) -- has Go sort the array again from the first, as it was
// (jitLoopFinish): every answer is still the interpreter's.
func TestJITSortRestarts(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "marking"}[marking], func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			r, _ := jitSortsForTest(t, jitStressConfig{threshold: true, budget: 2, deopt: 3})
			if r.jit.sortsRestarted == 0 {
				t.Error("Go never sorted again")
			}
		})
	}
}

// jitSortsForTest runs each of sortSetup's cases, each from a loop of its
// own, in rounds, with stress, and checks the answers; it returns the
// JIT's runtime and what makes the source of a case's loop.
func jitSortsForTest(t *testing.T, stress jitStressConfig) (*Runtime, func(src, n string) string) {
	want := []struct{ src, want string }{
		{`up_i32(i32, c_i32)`, "-3,-3,-1,0,2,5,6,7,8,13,13,21,42,100"},
		{`down_i32(i32, c_i32)`, "100,42,21,13,13,8,7,6,5,2,0,-1,-3,-3"},
		{`up_f64(f64, c_f64)`, "-Infinity,-2,-0,0,-0,0,0.5,1.25,3.75,10,Infinity"},
		{`down_f64(f64, c_f64)`, "Infinity,10,3.75,1.25,0.5,-0,0,-0,0,-2,-Infinity"},
		{`up_f32(f32, c_f32)`, "-2.25,-0,0.10000000149011612,0.10000000149011612,1.5,3.5,10000000000"},
		{`down_f32(f32, c_f32)`, "10000000000,3.5,1.5,0.10000000149011612,0.10000000149011612,-0,-2.25"},
		{`up_u8c(u8c, c_u8c)`, "0,1,2,2,3,200,255"},
		{`down_u8c(u8c, c_u8c)`, "255,200,3,2,2,1,0"},
		{`up_i8(i8, c_i8)`, "-128,-64,-1,0,1,64,127"},
		{`down_i8(i8, c_i8)`, "127,64,1,0,-1,-64,-128"},
		{`up_u8(u8, c_u8)`, "0,3,3,7,9,250"},
		{`down_u8(u8, c_u8)`, "250,9,7,3,3,0"},
		{`up_i16(i16, c_i16)`, "-32768,-5,0,5,32767"},
		{`down_i16(i16, c_i16)`, "32767,5,0,-5,-32768"},
		{`up_u16(u16, c_u16)`, "1,2,300,65535"},
		{`down_u16(u16, c_u16)`, "65535,300,2,1"},
		{`up_u32(u32, c_u32)`, "0,1,3,2147483648,4000000000"},
		{`down_u32(u32, c_u32)`, "4000000000,2147483648,3,1,0"},
		{`up_nums(nums)`, "-Infinity,-3,-1,-1e-300,-0,0,2,5,7,8.5,13,21,1e+300,Infinity"},
		{`down_nums(nums)`, "Infinity,1e+300,21,13,8.5,7,5,2,-0,0,-1e-300,-1,-3,-Infinity"},
		{`sortUp(i32)`, "-3,-3,-1,0,2,5,6,7,8,13,13,21,42,100"},
		{`sortDown(i32)`, "100,42,21,13,13,8,7,6,5,2,0,-1,-3,-3"},
		{`sortUp(f64)`, "-Infinity,-2,-0,0,-0,0,0.5,1.25,3.75,10,Infinity"},
		{`sortDown(f64)`, "Infinity,10,3.75,1.25,0.5,-0,0,-0,0,-2,-Infinity"},
		{`sortUp(f32)`, "-2.25,-0,0.10000000149011612,0.10000000149011612,1.5,3.5,10000000000"},
		{`sortDown(f32)`, "10000000000,3.5,1.5,0.10000000149011612,0.10000000149011612,-0,-2.25"},
		{`sortUp(u8c)`, "0,1,2,2,3,200,255"},
		{`sortDown(u8c)`, "255,200,3,2,2,1,0"},
		{`sortUp(i8)`, "-128,-64,-1,0,1,64,127"},
		{`sortDown(i8)`, "127,64,1,0,-1,-64,-128"},
		{`sortUp(u8)`, "0,3,3,7,9,250"},
		{`sortDown(u8)`, "250,9,7,3,3,0"},
		{`sortUp(i16)`, "-32768,-5,0,5,32767"},
		{`sortDown(i16)`, "32767,5,0,-5,-32768"},
		{`sortUp(u16)`, "1,2,300,65535"},
		{`sortDown(u16)`, "65535,300,2,1"},
		{`sortUp(u32)`, "0,1,3,2147483648,4000000000"},
		{`sortDown(u32)`, "4000000000,2147483648,3,1,0"},
		{`sortUp(nums)`, "-Infinity,-3,-1,-1e-300,-0,0,2,5,7,8.5,13,21,1e+300,Infinity"},
		{`sortDown(nums)`, "Infinity,1e+300,21,13,8.5,7,5,2,-0,0,-1e-300,-1,-3,-Infinity"},
		{`sortBranch(f64, c_f64)`, "-Infinity,-2,-0,0,-0,0,0.5,1.25,3.75,10,Infinity"},
		{`sortBranchArr(nums)`, "-Infinity,-3,-1,-1e-300,-0,0,2,5,7,8.5,13,21,1e+300,Infinity"},
		{`sortWithUp(i32, c_i32)`, "-3,-3,-1,0,2,5,6,7,8,13,13,21,42,100"},
		{`sortMethod(i32, c_i32)`, "-3,-3,-1,0,2,5,6,7,8,13,13,21,42,100"},
		{`sortClass(i32, c_i32)`, "TypeError"},
		{`sortWith(i32, up)`, "-3,-3,-1,0,2,5,6,7,8,13,13,21,42,100"},
		{`sortWith(i32, down)`, "100,42,21,13,13,8,7,6,5,2,0,-1,-3,-3"},
		{`sortTies(ties, c_ties)`, "18,16,17,19,32,47,49,53,63,48"},
		{`sortTiesArr(tiesArr)`, "18,16,17,19,32,47,49,53,63,48"},
		{`sortNaNResult(i32, c_i32)`, "5,-3,8,13,21,-1,7,2,0,13,-3,100,42,6"},
		{`sortNaNGlobal(i32, c_i32)`, "5,-3,8,13,21,-1,7,2,0,13,-3,100,42,6"},
		{`sortZero(nums)`, "5,-3,8.5,13,-0,21,0,-1,7,2,Infinity,-Infinity,1e+300,-1e-300"},
		{`sortInPlace(new Int32Array([3, 1, 2]))`, "true:1,2,3"},
		{`sortSometimes(i32, c_i32)`, "-3,-3,-1,0,2,5,6,7,8,13,13,21,42,100"},
		{`sortString(i32, c_i32)`, "-3,-3,-1,0,2,5,6,7,8,13,13,21,42,100"},
		{`sortNaNLast(withNaN, c_nan)`, "-0,0,1,2,3,NaN,NaN"},
		{`sortLong(long, c_long)`, "1000:246,-250:-909472592"},
		{`sortLongTies(long, c_long)`, "1000:-144,151:-1019300128"},
		{`sortLongArr(longArr)`, "1000:0,988.125:42040358"},
		{`sortWild(i32, c_i32)`, "14:true"},
		{`sortWildArr(nums)`, "14:true"},
		{`sortNaNs(withNaN, c_nan)`, "7:true"},
		{`sortSmall(0)`, "/"},
		{`sortSmall(1)`, "1/1"},
		{`sortSmall(2)`, "1,2/1,2"},
		{`sortSmall(3)`, "1,2,3/1,2,3"},
		{`sortSmall(17)`, "1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17/1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17"},
		{`sortCaptured(i32, c_i32)`, "-3,-3,-1,0,2,5,6,7,8,13,13,21,42,100"},
		{`sortProp(i32, c_i32)`, "100,42,21,13,13,8,7,6,5,2,0,-1,-3,-3"},
		{`sortCall(i32, c_i32)`, "0,-1,2,-3,-3,5,6,7,8,13,13,21,42,100"},
		{`sortLogs(i32, c_i32)`, "-3,-3,-1,0,2,5,6,7,8,13,13,21,42,100:true"},
		{`sortBig()`, "-1,0,2,3"},
		{`sortHoles()`, "4:1,2,3,:false"},
		{`sortUndef()`, "1,2,3,:true"},
		{`sortStrings()`, "1,2,3,10"},
		{`sortInherited()`, "1,3,7:true"},
		{`sortGetter()`, "11:1,5"},
		{`sortFrozen()`, "TypeError"},
		{`sortNotFn()`, "TypeError"},
		{`sortDetached()`, "TypeError"},
		{`sortTracking()`, "1,2,3,4"},
		{`sortMine()`, "Mine:1,2,3"},
		{`sortSub()`, "1,2,3"},
	}
	// Each function called from a loop of its own, its arguments the case's.
	drivers, driven := "", map[string]bool{}
	drive := func(src, n string) string {
		name, args, _ := strings.Cut(strings.TrimSuffix(src, ")"), "(")
		if !driven[name] {
			driven[name] = true
			drivers += "function drive_" + name + "(n, a, b) { let r; for (let i = 0; i < n; i++) r = " + name + "(a, b); return show(r) };"
		}
		if args != "" {
			args = ", " + args
		}
		return "drive_" + name + "(" + n + args + ")"
	}
	rounds := []string{}
	for range 6 {
		for _, c := range want {
			rounds = append(rounds, drive(c.src, "20"))
		}
		rounds = append(rounds, drive("sortBool(i32, c_i32)", "20"))
	}
	r := jitAgreesForTestWith(t, stress, sortSetup+drivers, rounds...)
	for _, c := range want {
		v, err := r.Run(compileForTest(t, "show("+c.src+")"))
		if err != nil {
			t.Fatalf("%s: %v", c.src, err)
		}
		if got := v.String().Go(); got != c.want {
			t.Errorf("%s = %s, want %s", c.src, got, c.want)
		}
	}
	return r, drive
}
