//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"slices"
	"testing"
	"unsafe"
)

// jitLearnedPersistSrc compiles f, then has it leave its code -- an object
// of another layout at its reads, the callees of its method calls swapped,
// a built-in's call made by Go and a function of the script's -- and
// compiled again, its calls of Go made at sites it had not made them at
// before.
const jitLearnedPersistSrc = `
var script = function () { return "ab" }
var a = { length: 2, 0: 1, 1: 2, m: Array.prototype.join }, b = { m: script }
function f(n, o, x, y, m) {
	let s = 0
	for (let i = 0; i < n; i++) {
		if (i % m === 0) s += String(i).length + [i].join().length
		s += x.m().length + y.m().length
		s += o.k
	}
	return s
}
for (let k = 0; k < 40; k++) f(200, { k: 1 }, a, b, 1000)
for (let k = 0; k < 40; k++) f(200, { j: 0, k: 2 }, b, a, 7)
for (let k = 0; k < 40; k++) f(200, { k: 3 }, a, a, 3)
function mk(k) { return function () { return k } }
function g(n) { let s = 0; for (let i = 0; i < n; i++) s += mk(i)(); return s }
for (let k = 0; k < 40; k++) g(200)
function* g1() { yield 1; yield 2 }
function* g2() { yield 3 }
function h(n) { let s = 0; for (let i = 0; i < n; i++) for (const v of (i % 2 ? g1() : g2())) s += v; return s }
for (let k = 0; k < 40; k++) h(200)
`

// jitLearnedLists are the lists of a function's learning a snapshot of its
// feedback takes by their headers, each the list as it was when taken: the
// lists only grow, appended past every header taken, or are made anew.
func jitLearnedLists(e *jitEntry) map[string]any {
	m := map[string]any{"ssaHostsLeft": e.ssaHostsLeft, "regCallsMade": e.regCallsMade}
	if l := e.jitLearned; l != nil {
		m["failed"], m["failedEntries"], m["resumed"] = l.failed, l.failedEntries, l.resumed
		m["bigints"], m["bigwide"] = l.bigints, l.bigwide
		m["typedReads"], m["stringReads"], m["keyedReads"], m["keyedWrites"] = l.typedReads, l.stringReads, l.keyedReads, l.keyedWrites
		m["notInline"], m["genericCalls"] = l.notInline, l.genericCalls
		m["nativeCalls"], m["inlines"], m["viaLeaves"] = l.nativeCalls, l.inlines, l.viaLeaves
		m["genSteps"], m["stepResults"], m["genMakes"] = l.genSteps, l.stepResults, l.genMakes
	}
	return m
}

// jitLearnedCall is a learned call as a list's header holds it, its pool
// by whether it has one: a pool's objects are the code's to take.
type jitLearnedCall struct {
	pc                          int32
	cl, cb                      *closure
	obj, cbObj, proto, parent   *Object
	active                      *Object
	exits                       uint32
	via, forEach, byCode, isNew bool
	cbLeft                      bool
}

func jitLearnedCalls(v []jitInline) []jitLearnedCall {
	out := make([]jitLearnedCall, len(v))
	for i, x := range v {
		out[i] = jitLearnedCall{x.pc, x.cl, x.cb, x.obj, x.cbObj, x.proto, x.parent, x.active, x.exits, x.via, x.forEach, x.byCode, x.pool != nil, x.cbLeft}
	}
	return out
}

// jitGenStepsDeep is genSteps with each step's closures copied: what a
// header of the list holds, deep.
func jitGenStepsDeep(v []jitGenStep) []jitGenStep {
	out := slices.Clone(v)
	for i := range out {
		out[i].cls = slices.Clone(out[i].cls)
	}
	return out
}

// jitGenMakeAt is what of a generator function's call a snapshot reads: its
// pc and function, not its pool, made at its first ask.
type jitGenMakeAt struct {
	pc  int32
	obj *Object
}

// jitListClone is a list of jitLearnedLists' with its elements copied, and
// its header.
func jitListClone(x any) (any, unsafe.Pointer, int) {
	switch v := x.(type) {
	case []int32:
		return slices.Clone(v), unsafe.Pointer(unsafe.SliceData(v)), len(v)
	case []uint32:
		return slices.Clone(v), unsafe.Pointer(unsafe.SliceData(v)), len(v)
	case []uint64:
		return slices.Clone(v), unsafe.Pointer(unsafe.SliceData(v)), len(v)
	case []jitInline:
		return jitLearnedCalls(v), unsafe.Pointer(unsafe.SliceData(v)), len(v)
	case []jitViaLeaf:
		return slices.Clone(v), unsafe.Pointer(unsafe.SliceData(v)), len(v)
	case []jitGenStep:
		return jitGenStepsDeep(v), unsafe.Pointer(unsafe.SliceData(v)), len(v)
	case []jitStepResult:
		return slices.Clone(v), unsafe.Pointer(unsafe.SliceData(v)), len(v)
	case []jitGenMake:
		at := make([]jitGenMakeAt, len(v))
		for i, x := range v {
			at[i] = jitGenMakeAt{x.pc, x.obj}
		}
		return at, unsafe.Pointer(unsafe.SliceData(v)), len(v)
	}
	panic("jitListClone: unknown list")
}

// TestJITLearnedListsPersist holds the lists of a function's learning that
// a snapshot takes by their headers to stay, as each header has them, as
// they were when it was taken, whatever the exits, the calls of Go and the
// installs after write: taken at every compile asked, each list taken is
// the same at the end, while the function's live lists have moved past
// some of them -- ssaHostsLeft made anew at an install, regCallsMade at a
// call first made, among them -- and no install's ssaHostsLeft is in the
// array of one taken before it, which a list filled again in place with
// what it held would be, unchanged to the comparison. Truncated and filled
// again in place, or counted in place, a list a snapshot holds had changed
// under the worker reading it. The learned calls are held so too, a
// target marked called by its code and a step's generators grown among
// their writes, each a list made anew.
func TestJITLearnedListsPersist(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	type taken struct {
		e     *jitEntry
		name  string
		list  any
		elems any
		data  unsafe.Pointer
		n     int
	}
	type install struct {
		serial uint32
		data   unsafe.Pointer
	}
	var all []taken
	last := map[*jitEntry]install{}
	installs := 0
	r := jitRuntimeForTest(t, Config{JIT: true})
	jitTestSwitch(t, func(c *jitTestConfig) {
		c.compileNote = func(r *Runtime, _ *closure) {
			for _, e := range r.jitState().regLearned {
				for name, list := range jitLearnedLists(e) {
					elems, data, n := jitListClone(list)
					all = append(all, taken{e, name, list, elems, data, n})
				}
				data := unsafe.Pointer(unsafe.SliceData(e.ssaHostsLeft))
				if was, ok := last[e]; ok && was.serial != e.ssaSerial && was.data != nil {
					// Installed since it was taken (jitRegCached counts it).
					installs++
					if data == was.data {
						t.Errorf("an install's ssaHostsLeft %v is in the array of the one before it", e.ssaHostsLeft)
					}
				}
				last[e] = install{e.ssaSerial, data}
			}
		}
	})
	if _, err := r.Run(compileForTest(t, jitLearnedPersistSrc)); err != nil {
		t.Fatal(err)
	}
	if len(all) == 0 {
		t.Fatal("no compile asked")
	}
	moved := map[string]int{}
	for _, x := range all {
		if elems, _, _ := jitListClone(x.list); !jitSameList(elems, x.elems) {
			t.Errorf("%s changed under its header: %v, taken as %v", x.name, elems, x.elems)
		}
		_, data, n := jitListClone(jitLearnedLists(x.e)[x.name])
		if n != 0 && (data != x.data || n != x.n) {
			moved[x.name]++
		}
	}
	t.Logf("%d lists taken; moved past since: %v; %d installs after a list of hosts", len(all), moved, installs)
	if installs == 0 {
		t.Error("no install after a list of hosts taken: the test does not reach ssaHostsLeft's writes")
	}
	for _, name := range []string{"ssaHostsLeft", "regCallsMade"} {
		if moved[name] == 0 {
			t.Errorf("no %s taken was moved past: the test does not reach its writes", name)
		}
	}
	// The writes that had been in place: a call's target marked called by
	// its code, and a step's generators' functions grown.
	byCode, grown := false, false
	for _, e := range r.jitState().regLearned {
		if e.jitLearned == nil {
			continue
		}
		byCode = byCode || slices.ContainsFunc(e.nativeCalls, func(x jitInline) bool { return x.byCode })
		grown = grown || slices.ContainsFunc(e.genSteps, func(x jitGenStep) bool { return len(x.cls) > 1 })
	}
	if !byCode || !grown {
		t.Errorf("a call marked by its code %v, a step's generators grown %v: the test does not reach their writes", byCode, grown)
	}
}

// jitSameList reports whether two lists of jitListClone's have the same
// elements.
func jitSameList(a, b any) bool {
	switch v := a.(type) {
	case []int32:
		return slices.Equal(v, b.([]int32))
	case []uint32:
		return slices.Equal(v, b.([]uint32))
	case []uint64:
		return slices.Equal(v, b.([]uint64))
	case []jitLearnedCall:
		return slices.Equal(v, b.([]jitLearnedCall))
	case []jitViaLeaf:
		return slices.Equal(v, b.([]jitViaLeaf))
	case []jitGenStep:
		return slices.EqualFunc(v, b.([]jitGenStep), func(x, y jitGenStep) bool { return x.pc == y.pc && slices.Equal(x.cls, y.cls) })
	case []jitStepResult:
		return slices.Equal(v, b.([]jitStepResult))
	case []jitGenMakeAt:
		return slices.Equal(v, b.([]jitGenMakeAt))
	}
	return false
}
