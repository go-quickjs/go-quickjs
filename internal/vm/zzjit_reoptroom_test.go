//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"slices"
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
		r.jitCallThreshold, r.jitICThreshold = 1, 1
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

// Code Go goes on in the interpreter in, after an exit with no entry to go
// on natively at, is entered at its start and its loop's header alone, as
// V8's code is -- never compiled with an entry where Go went on, nor after
// every guard that may leave for Go: a loop whose one read of a primitive
// leaves at every other iteration goes on natively at its header each
// time, its twelve other reads, which never leave, compiled as they were.
// Every entry loads its live slots and merges with phis; Typescript's
// 3,148-instruction function, given one after every guard, had 1,666, two
// thirds of its 145,000 values, its compile 200 ms, and Gameboy's
// instruction dispatch, given one at each PC Go had gone on at, ran
// natively 40% longer with 46. Each answer is the interpreter's.
func TestJITEntriesAtStartAndLoopsOnly(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `var obj={a:1,p1:1,p2:2,p3:3,p4:4,p5:5,p6:6,p7:7,p8:8,p9:9,p10:10,p11:11,p12:12};
		function many(n){let t=0;for(let i=0;i<n;i++){const o=i&1?1:obj;
			t=(t+(o.a|0)+obj.p1+obj.p2+obj.p3+obj.p4+obj.p5+obj.p6+obj.p7+obj.p8+obj.p9+obj.p10+obj.p11+obj.p12)|0}return t}`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	cl := r.global.getOwn(r.atoms.intern("many")).value().Object().fn().closure
	checked := 0
	for round := 0; round < 8; round++ {
		wv, err := want.Run(compileForTest(t, `many(500)`))
		if err != nil {
			t.Fatal(err)
		}
		wentOn := r.jit.wentOn
		var serial uint32
		valid := false
		if e := r.jit.hint(cl); e != nil {
			serial, valid = e.ssaSerial, e.ssa != nil && !e.invalid
		}
		gv, err := r.Run(compileForTest(t, `many(500)`))
		if err != nil || gv.Number() != wv.Number() {
			t.Fatalf("round %d: got %v, %v; interpreter %v", round, gv, err, wv)
		}
		e := r.jit.hint(cl)
		if e == nil || e.ssa == nil || e.entrySlow {
			t.Fatalf("round %d: many is not native: %+v", round, e)
		}
		if pcs, want := jitEntryPCs(e, cl.fn), jitLoopHeadersOf(cl.fn); !slices.Equal(pcs, want) {
			t.Fatalf("round %d: entries at %v; want its start and loop header, %v", round, pcs, want)
		}
		if !valid || e.ssaSerial != serial || e.invalid {
			// The read's first primitive deoptimized, invalidating the
			// code, and made it megamorphic; or the code was compiled
			// again for it during the round.
			continue
		}
		checked++
		if n := r.jit.wentOn - wentOn; n != 250 {
			t.Fatalf("round %d: went on natively at the header %d times, want 250", round, n)
		}
	}
	if checked < 3 {
		t.Fatalf("%d rounds ran settled code", checked)
	}
}

// One miss in a loop's whole run, at a read of a primitive, deoptimizes:
// the interpreter finishes the invocation, the code invalidated, and the
// code compiled again -- the read megamorphic, a primitive met -- has no
// entry at the read either, and leaves no more: what the loop proves
// before it -- obj's shape -- it goes on using. The answers are the
// interpreter's.
func TestJITOneMissNoEntry(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `var obj={a:1,p1:1,p2:2}; var flip=-1;
		function rare(n){let t=0;for(let i=0;i<n;i++){const o=i===flip?1:obj;
			t=(t+(o.a|0)+obj.p1+obj.p2)|0}return t}`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	for round, src := range []string{`rare(500)`, `rare(500)`, `flip=250; rare(500)`, `flip=-1; rare(500)`, `rare(500)`, `rare(500)`} {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		guards, hosts := r.jit.guards, r.jit.hosts
		gv, err := r.Run(compileForTest(t, src))
		if err != nil || gv.Number() != wv.Number() {
			t.Fatalf("round %d: got %v, %v; interpreter %v", round, gv, err, wv)
		}
		if g, h := r.jit.guards-guards, r.jit.hosts-hosts; round >= 1 && (g != map[bool]uint64{true: 1}[round == 2] || h != 0) {
			t.Fatalf("round %d: %d guards failed and %d exits left to Go", round, g, h)
		}
	}
	cl := r.global.getOwn(r.atoms.intern("rare")).value().Object().fn().closure
	e := r.jit.hint(cl)
	if e == nil || e.ssa == nil || e.entrySlow {
		t.Fatalf("rare did not stay native: %+v", e)
	}
	if pcs, want := jitEntryPCs(e, cl.fn), jitLoopHeadersOf(cl.fn); !slices.Equal(pcs, want) {
		t.Fatalf("entries at %v; want its start and loop header, %v", pcs, want)
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

// The room a first compile asks for has in it what the hint table grows by
// if the compile is the one that has it double (jitState's hintRoom): known
// before the compile, it is no average. Averaged into perInstr instead, a
// compile meeting the doubling at a budget with room for an entry like
// the last but not for the table's growth was made and thrown away -- at
// one of TestJITFillDiscardsNothing's budgets once the code had grown by
// a few bytes.
func TestJITRoomForHintGrowth(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	epoch := uint64(0)
	defer func(was func() uint64) { jitEpochNow = was }(jitEpochNow)
	jitEpochNow = func() uint64 { return epoch }
	var keep []*bytecode.Function
	defer runtime.KeepAlive(&keep)
	fn := jitFunctionForTest(t, `function f(n) { var s=0; for(var i=0;i<n;i++) s+=i; return s }`)
	copyOf := jitCopiesForTest(fn, &keep)
	// A runtime filled until its next compile is the one that grows the
	// table, its budget (jitBudget) room more than it holds.
	filled := func(room int) *Runtime {
		r := New(Config{JIT: true})
		r.jitCallThreshold, r.jitICThreshold = 1, 1
		for n := 0; r.jit == nil || r.jit.hintGrowth() == 0; n++ {
			if n > 1<<10 || r.jitForFunction(copyOf(), nil) == nil {
				t.Fatalf("the hint table never filled: %d compiles", n)
			}
		}
		r.jitMemoryLimit = int64(r.jit.held() + room)
		return r
	}
	r := filled(0)
	est, growth := len(fn.Code)*r.jit.perInstr, r.jit.hintGrowth()
	r.Close()
	r.ReleaseClosed()
	// Every room from what the estimate alone asks for to past what the
	// estimate and the growth do.
	for room := est - growth; room <= est+2*growth; room += 16 {
		r := filled(room)
		r.jitForFunction(copyOf(), nil)
		if r.jit.discarded != 0 {
			t.Fatalf("room %d (estimate %d, the table's growth %d): compiled for nothing, %d discarded", room, est, growth, r.jit.discarded)
		}
		r.Close()
		r.ReleaseClosed()
	}
}
