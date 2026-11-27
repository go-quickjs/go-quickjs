//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"testing"
)

// manyCellsSrc's functions capture more of their locals than the four a
// function's pool of cells had held (jitCellsPoolOf): five, ten -- as
// EarleyBoyer's deriv_trees does, a closure made on one path only --
// sixteen, and seventeen, more than a block holds, whose cells stay Go's;
// trees recurses as deriv_trees does, capturing ten, a callback made of
// them on some levels.
// Each is called natively, never inlined (its locals are captured), by
// drivers whose loops run the pools of cells and of closures out many
// times over; turns makes a closure of a per-turn binding of eight cells,
// a scope's end at each turn (GoClose).
const manyCellsSrc = `
function five(a, b, c, d) { var e = a + b; var f = function () { return a + b + c + d + e }; return f() + e }
function ten(a, b, c, d, e, f) {
	var g = a + 1, h = b + 2, i = c + 3, j = d + 4
	if (a % 97 === 0) return (function () { return a + b + c + d + e + f + g + h + i + j })()
	return a + b + j
}
function sixteen(a, b, c, d, e, f, g, h) {
	var i = a + 1, j = b + 1, k = c + 1, l = d + 1, m = e + 1, n = f + 1, o = g + 1, p = h + 1
	var get = function () { return a + b + c + d + e + f + g + h + i + j + k + l + m + n + o + p }
	a = a + 100
	return get() + a
}
function seventeen(a, b, c, d, e, f, g, h, q) {
	var i = a + 1, j = b + 1, k = c + 1, l = d + 1, m = e + 1, n = f + 1, o = g + 1, p = h + 1
	var get = function () { return a + b + c + d + e + f + g + h + i + j + k + l + m + n + o + p + q }
	return get()
}
function trees(n, a, b, c, d, e) {
	var f = a + 1, g = b + 1, h = c + 1, i = d + 1, j = e + 1
	if (n === 0) return a
	var s = trees(n - 1, a + 1, b, c, d, e)
	if (n % 3 === 0) s += [0, 1].map(function (k) { return k + a + b + c + d + e + f + g + h + i + j }).length
	return s + j
}
function turns(n) {
	var fs = [], s = 0
	for (let t = 0; t < n; t++) {
		let a = t, b = t + 1, c = t + 2, d = t + 3, e = t + 4, f = t + 5, g = t + 6, h = t + 7
		fs.push(function () { return a + b + c + d + e + f + g + h })
	}
	for (var i = 0; i < fs.length; i++) s += fs[i]()
	return s
}
function hot(n) {
	var s = 0
	for (var i = 0; i < n; i++) s += five(i, 1, 2, 3) + ten(i, 1, 2, 3, 4, 5) + sixteen(i, 1, 2, 3, 4, 5, 6, 7) + trees(i & 15, i, 1, 2, 3, 4)
	return s
}
function cold(n) {
	var s = 0
	for (var i = 0; i < n; i++) s += seventeen(i, 1, 2, 3, 4, 5, 6, 7, 8) + turns(i & 7)
	return s
}
`

// TestJITManyCells runs manyCellsSrc natively and in a runtime with no JIT
// and compares them, plainly and with native code seeing the collector
// mark: once warm, the cells of functions capturing up to sixteen locals
// are taken from their pools natively (mir's cellsTake), the closures
// made of them too (closureTake), and a pool run out is filled again by a
// call of Go that finds no frame (ssa.GoRefill, mir's poolRefill): no call
// of Go makes them on the frame.
func TestJITManyCells(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			recordJITCompilesForTest(t)
			rts := regNativeRuntimes(t, manyCellsSrc)
			r := rts[0]
			r.SetJITWarmUp(1, 1)
			call := func(name string, n int) string {
				t.Helper()
				var got [2]string
				for i, r := range rts {
					fn, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
					v, err := r.Call(fn, Undefined, []Value{Float(float64(n))})
					got[i] = regNativeShow(r, v, err)
				}
				if got[0] != got[1] {
					t.Fatalf("%s: with the JIT %q, without %q", name, got[0], got[1])
				}
				return got[0]
			}
			const wantHot, wantCold = "1164121", "10600"
			for range 4 {
				if got := call("hot", 500); got != wantHot {
					t.Fatalf("hot: %q, want %q", got, wantHot)
				}
				if got := call("cold", 40); got != wantCold {
					t.Fatalf("cold: %q, want %q", got, wantCold)
				}
			}
			r.jitStress = jitStressConfig{}
			for range 3 {
				call("hot", 500)
			}
			calls, ops := 0, r.jit.frameOps
			jitTestSwitch(t, func(jc *jitTestConfig) { jc.goCallWork = func() { calls++ } })
			call("hot", 500)
			jitTestSwitch(t, func(jc *jitTestConfig) { jc.goCallWork = nil })
			if n := r.jit.frameOps - ops; n != 0 {
				t.Errorf("%d operations on a frame in a warm run of hot(500)", n)
			}
			// The calls of Go left are the pools' refills, which find no
			// frame, and trees' calls of map.
			t.Logf("%d calls of Go in a warm run of hot(500)", calls)
			for _, name := range []string{"five", "ten", "sixteen", "trees"} {
				v := jitCodeNamedForTest(t, r, name)
				if l := v.link(r, jitGlobalClosureForTest(r, name)); l == nil || l.in == 0 {
					t.Errorf("%s was never called natively", name)
				}
			}
			if fns, _, _ := regCompileARM64(t, r); fns == 0 {
				t.Errorf("arm64: no function compiled")
			}
		})
	}
}

// cellsRefillSrc's makers return closures of their captured locals --
// one, seven, twelve of them, an arrow among them -- which outlive the
// calls, and turns makes one at each turn of a per-turn binding of eight:
// every block of cells and every closure is kept, so their pools run out
// over and over. Each was a call of Go that made the object on the frame,
// finding its native callee's level; now Go only fills the pool again,
// with no frame to find (ssa.GoRefill), and native code takes again.
const cellsRefillSrc = `
function maker(a, b, c, d, e, f) { var g = a + b; return function () { return a + b + c + d + e + f + g } }
function maker2(a) { return function () { return a * 2 } }
function maker12(a, b, c, d, e, f, g, h, i, j, k, l) { return () => a + b + c + d + e + f + g + h + i + j + k + l }
function turns(n) {
	var fs = []
	for (let t = 0; t < n; t++) {
		let a = t, b = t + 1, c = t + 2, d = t + 3, e = t + 4, f = t + 5, g = t + 6, h = t + 7
		fs.push(function () { return a + b + c + d + e + f + g + h })
	}
	var s = 0
	for (var i = 0; i < fs.length; i++) s += fs[i]()
	return s
}
function makers(n) {
	var fs = []
	for (var i = 0; i < n; i++) fs.push(maker(i, 1, 2, 3, 4, 5), maker2(i), maker12(i, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11))
	var s = turns(n & 255)
	for (var i = 0; i < fs.length; i++) s += fs[i]()
	return s
}
`

// TestJITCellsRefilled runs cellsRefillSrc natively and in a runtime with
// no JIT and compares them, and with Node's answer, plainly and with
// native code seeing the collector mark: once warm, the pools of cells
// and of closures that run out are filled again by calls of Go that make
// no operation on a frame (mir's poolRefill), and native code takes from
// them again.
func TestJITCellsRefilled(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			recordJITCompilesForTest(t)
			rts := regNativeRuntimes(t, cellsRefillSrc)
			r := rts[0]
			r.SetJITWarmUp(1, 1)
			call := func(n int) string {
				t.Helper()
				var got [2]string
				for i, r := range rts {
					fn, _ := r.GetProp(Obj(r.Global()), r.Intern("makers"))
					v, err := r.Call(fn, Undefined, []Value{Float(float64(n))})
					got[i] = regNativeShow(r, v, err)
				}
				if got[0] != got[1] {
					t.Fatalf("makers(%d): with the JIT %q, without %q", n, got[0], got[1])
				}
				return got[0]
			}
			for range 4 {
				if got, want := call(1000), "2800364"; got != want {
					t.Fatalf("makers(1000): %q, want Node's %q", got, want)
				}
			}
			r.jitStress = jitStressConfig{}
			for range 3 {
				call(1000)
			}
			calls, ops := 0, r.jit.frameOps
			jitTestSwitch(t, func(jc *jitTestConfig) { jc.goCallWork = func() { calls++ } })
			call(1000)
			jitTestSwitch(t, func(jc *jitTestConfig) { jc.goCallWork = nil })
			if n := r.jit.frameOps - ops; n != 0 {
				t.Errorf("%d operations on a frame in a warm run of makers(1000)", n)
			}
			// 3,000 closures and as many blocks of cells kept, 1,000 more
			// of each of turns': the pools, of up to abi.PoolCapacity,
			// filled again some tens of times.
			if calls == 0 || calls > 400 {
				t.Errorf("%d calls of Go in a warm run of makers(1000)", calls)
			}
			t.Logf("%d calls of Go in a warm run of makers(1000)", calls)
			for _, name := range []string{"maker", "maker2", "maker12", "turns"} {
				v := jitCodeNamedForTest(t, r, name)
				if l := v.link(r, jitGlobalClosureForTest(r, name)); l == nil || l.in == 0 {
					t.Errorf("%s was never called natively", name)
				}
			}
		})
	}
}
