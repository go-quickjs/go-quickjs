//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"testing"
)

// regCarriedExitSrc is osrSetup's loops, for register code: a loop a call
// leaves native code in at turn k, at a write of a number's property with
// no entry after it (jitStressConfig's primLeave), the way a frame comes
// to a loop whose entry the function's code does not have, its object s
// carried unmade.
const regCarriedExitSrc = `
function P(v) { this.v = v }
P.prototype.add = function (o) { return new P(this.v + o.v) }
var obj = {a: 1}
function early(n, k) {
	const one = new P(1)
	let s = new P(0)
	for (let i = 0; i < n; i++) {
		const o = i === k ? 1 : obj
		o.a = 1
		s = s.add(one)
	}
	return s.v
}
function calls(c, n, k) {
	let t = 0
	for (let j = 0; j < c; j++) t += early(n, k)
	return t
}
function mixed(c) {
	let t = 0
	for (let j = 0; j < c; j++) t += early(100, j % 2 === 0 ? 5 : -1)
	return t
}
`

// regCarriedCall calls name with args in both runtimes, the first the
// JIT's, and fails unless they agree on want.
func regCarriedCall(t *testing.T, rts [2]*Runtime, want string, name string, args ...float64) {
	t.Helper()
	var got [2]string
	for i, r := range rts {
		fn, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
		vs := make([]Value, len(args))
		for k, a := range args {
			vs[k] = Float(a)
		}
		v, err := r.Call(fn, Undefined, vs)
		got[i] = regNativeShow(r, v, err)
	}
	if got[0] != got[1] || got[0] != want {
		t.Fatalf("%s%v: with the JIT %q, without %q, want %q", name, args, got[0], got[1], want)
	}
}

// regCodeNamed is the register code the cache holds for the global
// function name.
func regCodeNamed(t *testing.T, r *Runtime, name string) *jitRegEntry {
	t.Helper()
	fn, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
	cl := fn.Object().fn().closure
	return r.jit.regCache[r.jitKeyOf(cl.fn)]
}

// TestJITRegCarriedExitsGoOn has every call of a loop leave native code
// early, with no entry after the exit, its object carried unmade: the
// frames go on natively at the loop's next back edge, in the code kept for
// the loop (jitRegLoopCode), not in the interpreter to the loop's end --
// whatever entries the function's code has. Asked only of code with a loop
// entry (jitRegLeaveTo), it had every call finish in the interpreter.
func TestJITRegCarriedExitsGoOn(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			rts := regNativeRuntimes(t, regCarriedExitSrc)
			rts[0].jitStress = jitStressConfig{primLeave: true}
			for range 2 {
				regCarriedCall(t, rts, "600000", "mixed", 6000)
			}
			r := rts[0]
			e := regCodeNamed(t, r, "early")
			if e == nil || e.code == nil {
				t.Fatalf("early has no register code: %+v", e)
			}
			t.Logf("early: loops %v unentered %v osr %v went on %d", e.loops, e.unentered, e.osr != nil, r.jit.wentOn)
			if len(e.unentered) == 0 {
				t.Fatalf("the function's code does not keep the loop's objects unmade: loops %v", e.loops)
			}
			o := e.osr
			if o == nil || o.code == nil {
				t.Fatalf("no code kept for the loop, after %d frames left native code", e.life.goOns)
			}
			if r.jit.wentOn < 2000 {
				t.Errorf("%d frames went on natively at the loop, of %d that left", r.jit.wentOn, e.life.goOns)
			}
		})
	}
}

// TestJITRegCarriedFramesLeave has three calls in four -- every call --
// leave a loop whose object the function's code keeps unmade: past
// jitFramesLeaving frames gone on in the code kept for the loop, the
// function's code is compiled with the loop's entry, its objects made
// (jitRegFramesLeave), and the code kept goes: each call had paid the
// exit's objects and a run of the code kept, for the few turns before it
// left.
func TestJITRegCarriedFramesLeave(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			rts := regNativeRuntimes(t, regCarriedExitSrc)
			rts[0].jitStress = jitStressConfig{primLeave: true}
			for range 2 {
				regCarriedCall(t, rts, "300000", "calls", 3000, 100, 5)
			}
			r := rts[0]
			e := regCodeNamed(t, r, "early")
			if e == nil || e.code == nil {
				t.Fatalf("early has no register code: %+v", e)
			}
			t.Logf("early: loops %v unentered %v osr %v went on %d resumed %v", e.loops, e.unentered, e.osr != nil, r.jit.wentOn, e.life.resumed)
			if len(e.unentered) != 0 || e.osr != nil || len(e.loops) == 0 {
				t.Fatalf("every call left and the code goes on leaving its objects unmade: loops %v, unentered %v, osr %v", e.loops, e.unentered, e.osr != nil)
			}
		})
	}
}

// regKeptSrc's long runs a loop carrying an object, a long first call
// compiling it for the frame waiting at the loop, as microbench's
// carried_long, after short, its carried_short, ran the same loop; longs
// calls it from a loop of its own, natively.
const regKeptSrc = regCarriedExitSrc + `
function V(x, y) { this.x = x; this.y = y }
V.prototype.add = function (o) { return new V(this.x + o.x, this.y + o.y) }
function short(turns) {
	let s = new V(0, 0)
	const d = new V(1, 2)
	for (let i = 0; i < turns; i++) s = s.add(d)
	return s.x + s.y
}
function warm(n) {
	let t = 0
	for (let j = 0; j < n; j++) t += short(100)
	return t
}
function long(turns, k) {
	let s = new V(0, 0)
	const d = new V(1, 2)
	for (let i = 0; i < turns; i++) {
		const o = i === k ? 1 : obj
		o.a = 1
		s = s.add(d)
	}
	return s.x + s.y
}
function longs(n) {
	let t = 0
	for (let j = 0; j < n; j++) t += long(j & 63, -1)
	return t
}
`

// regKeptStart has V.prototype.add compiled and its constructions
// learned, as carried_short has ExtraVec's before carried_long, then long
// compiled for the frame waiting at its loop, which keeps that loop's
// entry, its object made there (ssa.Func's EntryKept).
func regKeptStart(t *testing.T, rts [2]*Runtime) *jitRegEntry {
	t.Helper()
	regCarriedCall(t, rts, "6000000", "warm", 20000)
	for range 2 {
		regCarriedCall(t, rts, "600000", "long", 200000, -1)
	}
	e := regCodeNamed(t, rts[0], "long")
	if e == nil || e.code == nil || len(e.kept) != 1 {
		t.Fatalf("long not compiled with the frame's entry kept: %+v", e)
	}
	return e
}

// TestJITRegKeptEntryGoes has a function compiled for a frame waiting at
// its loop, which keeps that loop's entry, called again and again: it gives
// the entry up, its object unmade for every call from then on, and at once
// where a native caller calls it, whose calls Go does not see
// (jitRegKeptGone, jitRegCalleesKeptNoEntries). Kept, every later call made
// each turn's object: carried_long ran 3 times as long. The code compiled
// again only to give up the entry is judged by all it did. Exact answers,
// idle and marking.
func TestJITRegKeptEntryGoes(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			rts := regNativeRuntimes(t, regKeptSrc)
			rts[0].jitStress = jitStressConfig{primLeave: true}
			e := regKeptStart(t, rts)
			base := e.life.ssaStats.workBase
			for range jitKeptStarts + 1 {
				regCarriedCall(t, rts, "150", "long", 50, -1)
			}
			if e = regCodeNamed(t, rts[0], "long"); e == nil || e.code == nil || len(e.kept) != 0 || len(e.unentered) != 1 {
				t.Fatalf("called again, long kept its entry: %+v", e)
			}
			if e.life.ssaStats.workBase != base {
				t.Errorf("compiled again to give up its entry, long is judged from %d, not %d", e.life.ssaStats.workBase, base)
			}

			rts = regNativeRuntimes(t, regKeptSrc)
			rts[0].jitStress = jitStressConfig{primLeave: true}
			regKeptStart(t, rts)
			regCarriedCall(t, rts, "18900000", "longs", 200000)
			if e = regCodeNamed(t, rts[0], "long"); e == nil || e.code == nil || len(e.kept) != 0 {
				t.Fatalf("called natively, long kept its entry: %+v", e)
			}
		})
	}
}

// regKeptLeaving is regKeptStart for code that has learned its loop's
// exit: a call leaving at turn 5, a write to a number's property, which
// fails the code's guard, the code compiled again for the frame waiting
// at the loop of the next long call, the write left to Go there -- the
// calls after it the ones its kept entry goes by.
func regKeptLeaving(t *testing.T, rts [2]*Runtime) *jitRegEntry {
	t.Helper()
	regKeptStart(t, rts)
	regCarriedCall(t, rts, "300", "long", 100, 5)
	regCarriedCall(t, rts, "600000", "long", 200000, -1)
	e := regCodeNamed(t, rts[0], "long")
	if e == nil || e.code == nil || e.invalid || len(e.kept) != 1 {
		t.Fatalf("long not compiled again with the frame's entry kept: %+v", e)
	}
	return e
}

// TestJITRegKeptEntryStaysForCallsThatLeave has the calls after the one a
// function was compiled for, kept entry and all, leave its loop anyway:
// they gain nothing from its object's staying unmade and would ask for the
// entry back, so it stays (jitRegKeptCallsLeave); where one in two leaves,
// it goes.
func TestJITRegKeptEntryStaysForCallsThatLeave(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	rts := regNativeRuntimes(t, regKeptSrc)
	rts[0].jitStress = jitStressConfig{primLeave: true}
	regKeptLeaving(t, rts)
	// Two of its looks at its calls, jitKeptStarts calls each: later, what
	// its exits taught it has it compiled again at a call, with no frame
	// waiting at its loop.
	for range 2 * jitKeptStarts {
		regCarriedCall(t, rts, "300", "long", 100, 5)
	}
	if e := regCodeNamed(t, rts[0], "long"); e == nil || e.code == nil || e.invalid || len(e.kept) == 0 || e.osr != nil {
		t.Fatalf("calls that left gave up the entry: %+v", e)
	}
	rts = regNativeRuntimes(t, regKeptSrc)
	rts[0].jitStress = jitStressConfig{primLeave: true}
	regKeptLeaving(t, rts)
	for i := range 2 * jitKeptStarts {
		regCarriedCall(t, rts, "300", "long", 100, float64(map[bool]int{true: 5, false: -1}[i%2 == 0]))
	}
	if e := regCodeNamed(t, rts[0], "long"); e == nil || e.code == nil || e.invalid || len(e.kept) != 0 {
		t.Fatalf("calls that ran their loop through kept the entry: %+v", e)
	}
}

// TestJITRegGoOnJudgedAsRun has every call of a short loop leave native
// code at its sixth turn of seven, the call begun at the function's start:
// the turn after it goes on natively at the loop's header in a run judged
// as the run that left (jitRegLeaveTo's osr, regGoOnRun), begun at the
// start. Judged as begun at the loop, by on-stack replacement, each
// one-turn stretch had the code found slow for its loop (loopSlow) at once:
// the rest of every call ran in the interpreter, and the loop's entry was
// never learned back for the frames that leave (jitRegFramesLeave).
func TestJITRegGoOnJudgedAsRun(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			rts := regNativeRuntimes(t, regCarriedExitSrc)
			rts[0].jitStress = jitStressConfig{primLeave: true}
			for range 3 {
				regCarriedCall(t, rts, "140000", "calls", 20000, 7, 5)
			}
			e := regCodeNamed(t, rts[0], "early")
			if e == nil || e.code == nil {
				t.Fatalf("early has no register code: %+v", e)
			}
			t.Logf("early: loops %v unentered %v loopSlow %v entrySlow %v starts %d entries %d work %d loopMean %d", e.loops, e.unentered,
				e.life.loopSlow, e.life.entrySlow, e.life.starts, e.life.ssaStats.entries, e.life.ssaStats.work, e.loopMean)
			if e.life.loopSlow || len(e.loops) == 0 {
				t.Errorf("the frames that leave do not go on natively at the loop: loopSlow %v, loops %v", e.life.loopSlow, e.loops)
			}
		})
	}
}
