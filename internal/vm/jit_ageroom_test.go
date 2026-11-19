//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"strings"
	"testing"
)

// What a look of the cache's counts anew -- every function's state measured
// again (regAge), grown by what its code learned at its exits since, and
// the states kept with no room for them -- is made room for at once, young
// code taken if it must be (keepWithin, settle), wherever the look is: here
// at a function's first sample, outside any compile. Made room for only at
// a compile's end, it had the JIT hold past its budget from the look on:
// with all the cache held young, the look's makeRoom found that reclaiming
// all it might would not make the room, reclaimed none of it, the states
// it had just counted among it, and noted its floor, which refused the
// compiles after it -- none to end with a settle. Here it left the JIT 125
// KB past its budget of 257 KB on amd64 and 4 KB past 256 KB on arm64, and a
// program of 300 hot functions at 1 MB ended 185 KB past it on arm64 under
// load (TestJITMemoryLimitOption).
//
// The cache holds young code, compiled in one epoch, and its budget is then
// left just short of room for a function's state, so that the next state
// made looks. In the next epoch, the code still young, one function's state
// grows past all the look may reclaim besides code -- programs, refusals --
// as the calls Go went on at (jitLearned's resumed) grow it, and a function
// never sampled is: its first sample's look counts the growth. The budget
// is set from what the cache holds, not filled by the program, whose code's
// size is each architecture's: at a budget the program filled on amd64, it
// left arm64 room for the state, and no look was made.
func TestJITHeldWithinBudgetAfterALook(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	defer func(was func() uint64) { jitEpochNow = was }(jitEpochNow)
	epoch := uint64(1)
	jitEpochNow = func() uint64 { return epoch }
	const limit = 256 << 10
	r := jitRuntimeForTest(t, Config{JIT: true, JITMemoryLimit: limit})
	var b strings.Builder
	const n = 120
	for i := range n {
		fmt.Fprintf(&b, "function f%d(n){let t=%d;for(let i=0;i<n;i++)t=(t*31+i+%d)|0;return t}\n", i, i, i)
	}
	b.WriteString("function fresh(){return 1}\nvar fs=[")
	for i := range n {
		fmt.Fprintf(&b, "f%d,", i)
	}
	b.WriteString("];for(let r=0;r<3;r++)for(const f of fs)f(200)")
	if _, err := r.Run(compileForTest(t, b.String())); err != nil {
		t.Fatal(err)
	}
	s := r.jit
	if s.held() > s.budget {
		t.Fatalf("held %d of %d after the run", s.held(), s.budget)
	}
	var grown *jitRegEntry
	young := 0
	for _, e := range s.regCache {
		if e.code != nil {
			young++
			if grown == nil && e.life != nil && e.life.regCounted {
				grown = e
			}
		}
	}
	if grown == nil || young < 8 {
		t.Fatalf("too little code compiled: %d entries with code, held %d of %d", young, s.held(), s.budget)
	}
	// No room for a state, by a byte.
	r.jitMemoryLimit = int64(s.held() + jitEntryFixed - 1)
	// The next epoch: the code is still young (jitCodeAge).
	epoch++
	spare := 0
	for _, v := range s.regVictims(nil, epoch) {
		spare += v.bytes
	}
	l := grown.life
	l.resumed = make([]uint32, 0, (spare+4<<10)/4)
	before := l.bytes
	cl := r.global.getOwn(r.atoms.intern("fresh")).value().Object().fn().closure
	if life := r.jitRegLifeOf(cl); life == nil {
		t.Fatal("fresh has no state")
	}
	if l.bytes < before+4*cap(l.resumed) {
		t.Fatalf("the look did not count the state's growth: %d bytes, %d before", l.bytes, before)
	}
	if s.held() > s.budget {
		t.Errorf("held %d of %d after fresh's first sample, outside a compile: register code and state %d, programs %d, shared %d",
			s.held(), s.budget, s.regHeld, s.lowerBytes, s.sharedBytes())
	}
	// The answers are still the interpreter's.
	if v, err := r.Run(compileForTest(t, `f0(200) + fresh()`)); err != nil || v.Number() != float64(jitAgeRoomHot(0, 200)+1) {
		t.Fatalf("f0(200) + fresh() = %v, %v", v, err)
	}
}

// jitAgeRoomHot is what f<i>(n) of TestJITHeldWithinBudgetAfterALook
// returns.
func jitAgeRoomHot(i, n int32) int32 {
	t := i
	for k := int32(0); k < n; k++ {
		t = t*31 + k + i
	}
	return t
}
