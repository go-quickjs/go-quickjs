//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"slices"
	"testing"
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
	jitTestSwitch(t, func(jc *jitTestConfig) { jc.epochNow = func() uint64 { return epoch } })
	r := jitRuntimeForTest(t, Config{JIT: true, MemoryLimit: 8 << 20})
	if _, err := r.Run(compileForTest(t, `function g(n) { let s = 0; for (let i = 0; i < n; i++) s += i * 2; return s }
		function h(n) { var s = 0; for (var i = 0; i < n; i++) s += i; return s }
		g(100); g(100); h(100); h(100)`)); err != nil {
		t.Fatal(err)
	}
	cl := r.global.getOwn(r.atoms.intern("g")).value().Object().fn().closure
	cv, ok := jitViewForTest(r, cl)
	if !ok {
		t.Fatalf("g was not compiled: %+v", cv.e)
	}
	other := r.jitRegKeyOf(r.global.getOwn(r.atoms.intern("h")).value().Object().fn().closure)
	if x := r.jit.regCache[other]; x == nil || x.code == nil {
		t.Fatalf("h was not compiled: %+v", x)
	}
	e := cv.e
	// The compile again (jitRegCompileFor) places its code or, with no
	// room, keeps the old and notes the room it was short of (reoptRoom,
	// jitRegPlace), its budget the runtime's (jitBudget), which each
	// compile reads: every byte held young, the clock not moving, a limit
	// of what the cache holds less g's code, which the new code replaces
	// (regRoomFor), leaves no room for code of any length. A limit a byte
	// below what the cache holds was no room only for code as long as the
	// old: arm64's code loads the addresses it embeds -- its calls of Go's
	// states, made for each compile -- in as many instructions as they
	// have half-words not zero, and g's, compiled again with one at a
	// 64 KB boundary, came to 896 bytes rather than 900, a line of the
	// arena less, and was placed: 29 and 54 of 6,000 runs under qemu.
	key := r.jitRegKeyOf(cl)
	limit := r.jitMemoryLimit
	old := r.jit.regReplaced(cv.reg)
	// The compile short of room lets go of g's program (jitRegPlace),
	// which takes the cache below the limit again, so that it lets go of
	// no young code to get there (settle): the program is larger than
	// g's code.
	program := 0
	if l := r.jit.regLowered[key]; l != nil {
		program = l.bytes
	}
	if program < old {
		t.Fatalf("g's program %d bytes, its code %d", program, old)
	}
	r.jitMemoryLimit = int64(r.jit.held() - old)
	reopts := r.jit.reoptimized
	for i := 0; i < 4; i++ {
		if ne := r.jitRegCompileFor(cl, cl.reg, key, false); ne != nil && ne.code != nil || e.reoptRoom <= 0 || r.jit.reoptimized != reopts || !cv.same(r, cl) {
			t.Fatalf("compiled again with no room: room %d, %d reoptimized of %d, code kept %v", e.reoptRoom, r.jit.reoptimized, reopts, cv.same(r, cl))
		}
	}
	// With the room -- the limit back, and the cache having let code go
	// (its generation, which a deferred compile waits for), as filling it
	// on lets the oldest go -- compiled again and kept; the code still
	// answers.
	r.jitMemoryLimit = limit
	r.jit.dropReg(other, r.jit.regCache[other])
	if ne := r.jitRegCompileFor(cl, cl.reg, key, false); ne == nil || ne.code == nil || r.jit.reoptimized != reopts+1 || e.reoptRoom != 0 || r.jit.regCache[key] != ne {
		t.Fatalf("not compiled again once there was room: %d reoptimized of %d, room %d", r.jit.reoptimized, reopts, e.reoptRoom)
	}
	if v, err := r.Run(compileForTest(t, `g(1000)`)); err != nil || v.Number() != 999000 {
		t.Fatal(v, err)
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
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	// A number's property written, at a site megamorphic for it, is left
	// to Go at an exit with no entry after it. The register JIT learns such
	// a write and makes it Go's from native code, with no exit
	// (jitPolySite's prim); primLeave keeps it an exit.
	setup := `var obj={a:1,p1:1,p2:2,p3:3,p4:4,p5:5,p6:6,p7:7,p8:8,p9:9,p10:10,p11:11,p12:12};
		function many(n){let t=0;for(let i=0;i<n;i++){const o=i&1?1:obj;o.a=1;
			t=(t+(obj.a|0)+obj.p1+obj.p2+obj.p3+obj.p4+obj.p5+obj.p6+obj.p7+obj.p8+obj.p9+obj.p10+obj.p11+obj.p12)|0}return t}`
	want := newTestRuntime(t, Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	r.jitStress.primLeave = true
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	cl := r.global.getOwn(r.atoms.intern("many")).value().Object().fn().closure
	r.jitState()
	checked := 0
	for round := 0; round < 8; round++ {
		wv, err := want.Run(compileForTest(t, `many(500)`))
		if err != nil {
			t.Fatal(err)
		}
		wentOn := r.jit.wentOn
		var serial uint32
		valid := false
		// The register form's compiles are counted in its life
		// (ssaSerial), its validity its cache entry's.
		if cv, ok := jitViewForTest(r, cl); ok {
			serial, valid = cv.e.ssaSerial, !cv.invalid()
		}
		gv, err := r.Run(compileForTest(t, `many(500)`))
		if err != nil || gv.Number() != wv.Number() {
			t.Fatalf("round %d: got %v, %v; interpreter %v", round, gv, err, wv)
		}
		cv, ok := jitViewForTest(r, cl)
		if !ok || cv.e.entrySlow {
			t.Fatalf("round %d: many is not native: %+v", round, cv.e)
		}
		e := cv.e
		if pcs, want := jitEntryPCsOfForTest(cv, cl); !slices.Equal(pcs, want) {
			t.Fatalf("round %d: entries at %v; want its start and loop header, %v", round, pcs, want)
		}
		if !valid || e.ssaSerial != serial || cv.invalid() {
			// The read's first primitive deoptimized, invalidating the
			// code, and made it megamorphic; or the code was compiled
			// again for it during the round.
			continue
		}
		checked++
		// 250 exits, at the odd turns; the loop's test is at its bottom,
		// so the last turn's exit, at 499, takes no back edge after it, and
		// the interpreter returns.
		if n := r.jit.wentOn - wentOn; n != 249 {
			t.Fatalf("round %d: went on natively at the header %d times, want 249", round, n)
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
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `var obj={a:1,p1:1,p2:2}; var flip=-1;
		function rare(n){let t=0;for(let i=0;i<n;i++){const o=i===flip?1:obj;
			t=(t+(o.a|0)+obj.p1+obj.p2)|0}return t}`
	want := newTestRuntime(t, Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	r.jitState()
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
	cv, ok := jitViewForTest(r, cl)
	if !ok || cv.e.entrySlow {
		t.Fatalf("rare did not stay native: %+v", cv.e)
	}
	if pcs, want := jitEntryPCsOfForTest(cv, cl); !slices.Equal(pcs, want) {
		t.Fatalf("entries at %v; want its start and loop header, %v", pcs, want)
	}
}

// perInstr rises to what entries come to hold, however little more than it
// that is, and falls toward what they hold less: a step of an eighth,
// truncated, had left it short by up to 7 bytes an instruction for good, a
// first compile asking for less room than its entry then took -- the budget
// filled by such compiles took the last one, and threw it away.
func TestJITPerInstrReachesWhatEntriesHold(t *testing.T) {
	t.Parallel()
	s := &jitState{}
	s.learn(10, 1000)
	for range 16 {
		s.learn(10, 1050)
	}
	if s.perInstr != 105 {
		t.Errorf("perInstr %d after entries of 105 bytes an instruction", s.perInstr)
	}
	for range 64 {
		s.learn(10, 900)
	}
	if s.perInstr < 90 || s.perInstr > 97 {
		t.Errorf("perInstr %d after entries of 90 bytes an instruction", s.perInstr)
	}
}
