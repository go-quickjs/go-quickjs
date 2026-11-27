//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
)

// cellsKeptSrc's functions capture their locals in cells of their own and
// make closures of them on some calls only, as EarleyBoyer's deriv_trees
// does: a native call's cells go back to their pool as it returns where
// nothing took them (ssa.Func's CellsKept), and each path that may keep
// them past the return keeps them there, which check holds to: every pair
// of closures made of one call's cells shares them -- set through one,
// read through the other -- after the calls that made none had their
// cells taken from the pool again. The paths: a closure native code makes
// (maybe, mir's closureTake); one Go makes on the frame, an arrow that
// reads new.target (newTarget); an arguments object Go makes on the frame,
// mapped (sloppyArgs) and unmapped (strictArgs); the frame Go makes at a
// throw and at an exit at a read of a shape the code has not met
// (thrower, shapes), its cells opened on its window; a generator's and an
// async function's cells, which live across a suspension (gen, asy). A
// function with a direct eval is no native callee (jitRegTarget). quiet
// calls maybe making none, whose cells then come from the pool, given
// back, with no call of Go to fill it again.
const cellsKeptSrc = `
var kept = []
function maybe(a, k) {
	var b = a + 1, c = a + 2, d = a + 3, e = a + 4
	if (k) return [function () { return a + b + c + d + e }, function (v) { a = v }]
	return a + e
}
function newTarget(a, k) {
	var b = a * 2
	if (k) return [() => (new.target === undefined ? a + b : -1), (v) => { a = new.target === undefined ? v : -1 }]
	return b
}
function sloppyArgs(a, k) {
	var b = a + 1
	if (k) return [function () { return a + b + arguments.length }, function (v) { b = v }, arguments.length]
	return b
}
function strictArgs(a, k) {
	'use strict'
	var b = a + 1
	var n = arguments.length
	if (k) return [function () { return a + b + n }, function (v) { b = v }]
	return b + n
}
function thrower(a, k) {
	var b = a + 1
	kept.push([function () { return a + b }, function (v) { b = v }])
	if (k) throw new Error('x' + a)
	return b
}
function shapes(o, a, k) {
	var b = a + 1
	var p = o.p
	if (k) return [function () { return a + b + p }, function (v) { b = v }]
	return b + p
}
function* gen(a) {
	var b = a + 1
	yield [function () { return a + b }, function (v) { b = v }]
	b += 10
	yield b
}
async function asy(a) {
	var b = a + 1
	await null
	return [function () { return a + b }, function (v) { b = v }]
}
function check(pairs, out) {
	// Each pair's closures share their cells: set through one, read
	// through the other, the values each its own.
	var s = 0
	for (var i = 0; i < pairs.length; i++) s += pairs[i][0]()
	for (var i = 0; i < pairs.length; i++) pairs[i][1](i * 3)
	for (var i = 0; i < pairs.length; i++) s += pairs[i][0]() * (i + 1)
	out.push(s)
}
function hot(n) {
	var out = [], pairs = []
	for (var i = 0; i < n; i++) {
		var k = i % 7 === 0
		var r = maybe(i, k)
		if (k) pairs.push(r)
		else out.push(r)
		r = newTarget(i, k)
		if (k) pairs.push(r)
		r = sloppyArgs(i, k)
		if (k) pairs.push(r)
		r = strictArgs(i, k)
		if (k) pairs.push(r)
		try { thrower(i, i % 11 === 0) } catch (x) { out.push(x.message) }
		r = shapes(i % 13 === 0 ? { q: 1, p: 2 } : { p: 3 }, i, k)
		if (k) pairs.push(r)
	}
	check(pairs, out)
	check(kept, out)
	kept.length = 0
	var g = gen(5), p0 = g.next().value
	maybe(1, false); maybe(2, false)
	out.push(p0[0](), g.next().value)
	p0[1](100)
	out.push(p0[0]())
	var t = out.join(), h = 0
	for (var i = 0; i < t.length; i++) h = (h * 31 + t.charCodeAt(i)) | 0
	return out.length + ':' + h + ':' + t.slice(-40)
}
var asyncOut = ''
function asyncRun() {
	var ps = []
	for (var i = 0; i < 20; i++) ps.push(asy(i))
	for (var i = 0; i < 50; i++) maybe(i, false)
	Promise.all(ps).then(function (pairs) { var out = []; check(pairs, out); asyncOut = out.join() })
	return 0
}
function asyncResult() { return asyncOut }
function quiet(n) { var s = 0; for (var i = 0; i < n; i++) s += maybe(i, false); return s }

`

// TestJITCellsKept runs cellsKeptSrc natively and in a runtime with no JIT
// and compares them, and with Node's answers, plainly and with native
// code seeing the collector mark.
func TestJITCellsKept(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			recordJITCompilesForTest(t)
			rts := regNativeRuntimes(t, cellsKeptSrc)
			r := rts[0]
			r.SetJITWarmUp(1, 1)
			call := func(name string, n int) string {
				t.Helper()
				var got [2]string
				for i, r := range rts {
					fn, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
					v, err := r.Call(fn, Undefined, []Value{Float(float64(n))})
					if err == nil {
						err = r.DrainJobs()
					}
					got[i] = regNativeShow(r, v, err)
				}
				if got[0] != got[1] {
					t.Fatalf("%s: with the JIT %q, without %q", name, got[0], got[1])
				}
				return got[0]
			}
			// Node's answers.
			const wantHot, wantAsync = "290:1045795159:x297,600,602,18281853,36089600,11,16,105", "11040"
			for range 4 {
				if got := call("hot", 300); got != wantHot {
					t.Fatalf("hot: %q, want %q", got, wantHot)
				}
				call("asyncRun", 0)
				if got := call("asyncResult", 0); got != wantAsync {
					t.Fatalf("asyncResult: %q, want %q", got, wantAsync)
				}
			}
			r.jitStress = jitStressConfig{}
			for range 3 {
				call("quiet", 2000)
			}
			calls := 0
			jitTestSwitch(t, func(jc *jitTestConfig) { jc.goCallWork = func() { calls++ } })
			call("quiet", 2000)
			jitTestSwitch(t, func(jc *jitTestConfig) { jc.goCallWork = nil })
			// 2,000 native calls of maybe, none making a closure: their
			// cells given back as each returns, the pool never runs out --
			// 19 calls of Go, its refills, had it not. What is left is the
			// loop's own entry.
			if calls > 5 {
				t.Errorf("%d calls of Go in a warm run of quiet(2000): its cells were not given back", calls)
			}
			for _, name := range []string{"maybe", "strictArgs", "shapes"} {
				v := jitCodeNamedForTest(t, r, name)
				if l := v.link(r, jitGlobalClosureForTest(r, name)); l == nil || l.in == 0 {
					t.Errorf("%s was never called natively", name)
				}
			}
		})
	}
}

// TestJITCellsHeldByGo holds each of Go's paths that has a context's cells
// to marking them held past the code's return (jitCellsHeld), so that they
// do not go back to their pool as it returns natively: an operation on the
// frame, whatever it makes (jitRegFrameOp), the frame made of the code's
// level, its cells opened on its window (jitRegReopenCells), and Go's
// reading of them for a closure it makes (jitCellsOf).
func TestJITCellsHeldByGo(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := newTestRuntime(t, Config{JIT: true})
	t.Cleanup(r.Close)
	r.jitState()
	for _, path := range []struct {
		name string
		run  func(ctx *abi.Context)
	}{
		{"jitRegFrameOp", func(ctx *abi.Context) { r.jitRegFrameOp(ctx) }},
		{"jitRegReopenCells", func(ctx *abi.Context) { r.jitRegReopenCells(nil, nil, 0, ctx, false) }},
		{"jitCellsOf", func(ctx *abi.Context) { jitCellsOf(ctx, &bytecode.Function{}) }},
	} {
		ctx := &abi.Context{Level: 1 << 20}
		path.run(ctx)
		if ctx.CellsKept == 0 {
			t.Errorf("%s: the cells not held", path.name)
		}
	}
}
