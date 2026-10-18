//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// A function compiled again whose code the cache found no room for once it
// was compiled -- the budget full of code too young to drop -- is not
// compiled again until there is the room it was short of (jitEntry's
// reoptRoom), as a first compile asks for its room before it compiles:
// Typescript's functions, inlining more each time, had been compiled and
// thrown away over and over, an eighth of its run. Once there is the room,
// it is compiled again, and kept.
func TestJITReoptWaitsForRoom(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	epoch := uint64(0)
	defer func(was func() uint64) { jitEpochNow = was }(jitEpochNow)
	jitEpochNow = func() uint64 { return epoch }
	r := jitRuntimeForTest(t, Config{JIT: true, MemoryLimit: 8 << 20})
	if _, err := r.Run(compileForTest(t, `function g(n) { let s = 0; for (let i = 0; i < n; i++) s += i * 2; return s }
		g(100); g(100)`)); err != nil {
		t.Fatal(err)
	}
	cl := r.global.getOwn(r.atoms.intern("g")).value().Object().fn().closure
	e := r.jit.hint(cl)
	if e == nil || e.ssa == nil {
		t.Fatalf("g was not compiled: %+v", e)
	}
	var keep []*bytecode.Function
	defer runtime.KeepAlive(&keep)
	copyOf := jitCopiesForTest(jitFunctionForTest(t, `function f(n) { var s=0; for(var i=0;i<n;i++) s+=i; return s }`), &keep)
	for n := 0; r.jitForFunction(copyOf(), nil) != nil; n++ {
		if n > 1<<14 {
			t.Fatal("the budget never filled")
		}
	}
	if r.jit.evicted != 0 || r.jit.hint(cl) != e {
		t.Fatalf("filling dropped code: %d evicted", r.jit.evicted)
	}
	// Every byte held young: a budget below what the cache holds leaves
	// no room to make.
	budget := r.jit.budget
	r.jit.budget = r.jit.held() - 1
	reopts := r.jit.reoptimized
	r.jitReoptimize(cl, e)
	if r.jit.discarded != 1 || e.reoptRoom <= 0 || r.jit.reoptimized != reopts {
		t.Fatalf("the first compile again: %d discarded, room %d, %d reoptimized of %d", r.jit.discarded, e.reoptRoom, r.jit.reoptimized, reopts)
	}
	for i := 0; i < 3; i++ {
		r.jitReoptimize(cl, e)
	}
	if r.jit.discarded != 1 || r.jit.reoptimized != reopts {
		t.Fatalf("compiled again with no room: %d discarded, %d reoptimized of %d", r.jit.discarded, r.jit.reoptimized, reopts)
	}
	// With the room, compiled again and kept; the code still answers.
	r.jit.budget = budget
	r.jitReoptimize(cl, e)
	if r.jit.reoptimized != reopts+1 || e.reoptRoom != 0 || r.jit.discarded != 1 {
		t.Fatalf("not compiled again once there was room: %d reoptimized of %d, room %d", r.jit.reoptimized, reopts, e.reoptRoom)
	}
	if v, err := r.Run(compileForTest(t, `g(1000)`)); err != nil || v.Number() != 999000 {
		t.Fatal(v, err)
	}
}

// The room a first compile asks for before it compiles (jitState's
// perInstr) is all its entry comes to hold: its code and what describes
// it, its function's lowered program and the lowering that holds it, and
// its link. Filled with copies of one function at any budget, the cache
// takes each compile or refuses it before compiling, and throws none away
// once compiled: the lowering's own bytes, left out, had made a compile
// meeting a budget within them -- at 8 of 257 budgets here, and on arm64
// at TestJITCacheDropsLeastRecentlyUsed's -- compiled for nothing.
func TestJITFillDiscardsNothing(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	epoch := uint64(0)
	defer func(was func() uint64) { jitEpochNow = was }(jitEpochNow)
	jitEpochNow = func() uint64 { return epoch }
	var keep []*bytecode.Function
	defer runtime.KeepAlive(&keep)
	copyOf := jitCopiesForTest(jitFunctionForTest(t, `function f(n) { var s=0; for(var i=0;i<n;i++) s+=i; return s }`), &keep)
	step := int64(16 << 10)
	if testing.Short() {
		step = 64 << 10
	}
	for limit := int64(6 << 20); limit <= 10<<20; limit += step {
		r := New(Config{JIT: true, MemoryLimit: limit})
		r.jitCallThreshold = 1
		for n := 0; ; n++ {
			if n > 1<<14 {
				t.Fatalf("limit %d: the budget never filled", limit)
			}
			e := r.jitForFunction(copyOf(), nil)
			if e == nil {
				break
			}
			if e.ssa == nil || r.jit.discarded != 0 {
				t.Fatalf("limit %d: compile %d made and refused room: %d discarded, %d of %d held",
					limit, n, r.jit.discarded, r.jit.held(), r.jit.budget)
			}
		}
		r.Close()
		r.ReleaseClosed()
	}
}

// A refusal reclaims nothing, at any budget, and old code stays through
// it only where reclaiming that too would not make the room
// (TestJITRefusesRoomOnlyWhenNothingIsOld at one budget). The floor a
// refusal notes had counted the list of what may be reclaimed (jitState's
// victims) kept from the epoch's last look, which goes once reclaimed
// from: its size too high, a compile at a budget within that of the room
// was refused with room for it -- at 7,421,952 here, and at 8 MiB on
// arm64 -- the old code it could have reclaimed kept.
func TestJITRefusalsAtEveryBudget(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	var keep []*bytecode.Function
	defer runtime.KeepAlive(&keep)
	copyOf := jitCopiesForTest(jitFunctionForTest(t, `function f(n) { var s=0; for(var i=0;i<n;i++) s+=i; return s }`), &keep)
	refusals := 0
	for limit := int64(7 << 20); limit <= 9<<20; limit += 16 << 10 {
		func() {
			epoch := uint64(0)
			defer func(was func() uint64) { jitEpochNow = was }(jitEpochNow)
			jitEpochNow = func() uint64 { return epoch }
			r := jitRuntimeForTest(t, Config{JIT: true, MemoryLimit: limit})
			entries := jitFillForTest(t, r, copyOf)
			// Two entries old, the others used since: only they may go.
			epoch += jitCodeAge
			for _, e := range entries[2:] {
				e.starts++
			}
			if e := r.jitForFunction(copyOf(), nil); e == nil || e.ssa == nil {
				t.Fatalf("limit %d: no room made from old code", limit)
			}
			for i := 0; ; i++ {
				if i > 8 {
					t.Fatalf("limit %d: no compile refused", limit)
				}
				evicted, refused, lowered, held := r.jit.evicted, r.jit.refusedRoom, len(r.jit.lowered), jitHeldButScratch(r)
				if e := r.jitForFunction(copyOf(), nil); e != nil {
					continue
				}
				if r.jit.refusedRoom == refused || r.jit.evicted != evicted || len(r.jit.lowered) != lowered || jitHeldButScratch(r) != held {
					t.Fatalf("limit %d: a refusal reclaimed: %d evicted of %d, %d lowerings of %d, %d bytes held of %d",
						limit, r.jit.evicted, evicted, len(r.jit.lowered), lowered, jitHeldButScratch(r), held)
				}
				refusals++
				break
			}
			if e := jitOldStaysUncounted(r, entries[:2]); e != nil {
				t.Fatalf("limit %d: old code of %d bytes stayed, not counted reclaimable: floor %d, %d held", limit, e.bytes, r.jit.floor, jitHeldButScratch(r))
			}
		}()
	}
	if refusals == 0 {
		t.Fatal("no budget refused a compile")
	}
}

// jitOldStaysUncounted is the entry of old, if any, that stayed held
// through a refusal without the refusal's floor counting it reclaimable,
// with its lowering, as listVictims counts it: old code stays only where
// reclaiming it too would not make the room.
func jitOldStaysUncounted(r *Runtime, old []*jitEntry) *jitEntry {
	for _, e := range old {
		if e.hint == 0 {
			continue
		}
		n := e.bytes
		if l := r.jit.lowered[e.link.key]; l != nil {
			n += l.bytes
		}
		if int64(r.jit.floor) > jitHeldButScratch(r)-int64(n) {
			return e
		}
	}
	return nil
}

// jitHeldButScratch is what r's JIT holds but makeRoom's list of what it
// may reclaim, which a look for room lets go of, reclaiming nothing.
func jitHeldButScratch(r *Runtime) int64 {
	return r.jitHeldBytes() - int64(jitCapBytes(r.jit.victims))
}
