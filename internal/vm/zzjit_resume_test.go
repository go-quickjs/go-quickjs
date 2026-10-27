//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime/debug"
	"slices"
	"testing"
)

// A call of a function native code makes returns to the code: Go goes on
// after it only where the call left -- here its callee is not the one the
// code checks for -- so the code has no entry after it (ssa.Func's
// Unresumed) until Go has gone on there, when the code is compiled again
// with one (ssa.ResumeFeedback). A call of the function itself has its
// entry from the first compile: one leaving deep in the recursion has Go go
// on after the call in every outer run. Each answer is the interpreter's.
func TestJITResumeEntriesOnDemand(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	both := jitBothForTest(t, `
		function g(x) { let s = 0; for (let i = 0; i < 3; i++) s += x; return s }
		function g2(x) { let s = 1; for (let i = 0; i < 2; i++) s += x; return s }
		var h = g
		function f(n) { let t = 0; for (let i = 0; i < n; i++) { t = (t + h(i)) | 0; t = (t ^ i) | 0 } return t }
		function fact(n) { if (n <= 1) return 1; let s = 0; for (let i = 0; i < 2; i++) s += n; return (s * fact(n - 1)) % 1000003 }`)
	for range 20 {
		both.agree(t, `f(50)`)
	}
	e := jitNativeForTest(t, both.jit, "f")
	if len(e.ssaUnresumed) != 1 {
		t.Fatalf("f's code has no entries left out after its calls: %v", e.ssaUnresumed)
	}
	pc := int(e.ssaUnresumed[0])
	if e.ssa.HasEntry(pc) || len(e.resumed) != 0 {
		t.Fatalf("f's code has an entry after its call at %d before Go went on there: %v", pc-1, e.resumed)
	}
	// Another callee: the call leaves, Go makes it and goes on after it --
	// in code compiled again with the entry at once, as V8 compiles code
	// to enter where a frame needs it: not in the interpreter at each of
	// the loop's turns until a budget is spent. Waiting for one, a loop
	// whose call left at each turn (microbench_extra's private_method) had
	// its stretches cut so short that its code was judged slow, and never
	// entered once it had the entry.
	goOns := e.goOns
	both.agree(t, `(h = g2, f(50))`)
	if !slices.Contains(e.resumed, uint32(pc)) {
		t.Fatalf("Go's going on after the call at %d was not learned: %v", pc-1, e.resumed)
	}
	if e = jitNativeForTest(t, both.jit, "f"); !e.ssa.HasEntry(pc) || slices.Contains(e.ssaUnresumed, int32(pc)) {
		t.Fatalf("f was not compiled again with an entry after its call at %d: %v", pc-1, e.ssaUnresumed)
	}
	if n := e.goOns - goOns; n != 1 || e.entrySlow {
		t.Fatalf("the interpreter went on with f %d times after the call at %d, f judged slow %v; once, where the entry was learned", n, pc-1, e.entrySlow)
	}
	for range 40 {
		both.agree(t, `f(50)`)
	}
	for range 20 {
		both.agree(t, `fact(12)`)
	}
	fe := jitNativeForTest(t, both.jit, "fact")
	fn := both.jit.global.getOwn(both.jit.atoms.intern("fact")).value().Object().fn().closure.fn
	self := -1
	for _, x := range jitTargets(fe) {
		if x.cl != nil && x.cl.fn == fn {
			self = int(x.pc)
		}
	}
	if self < 0 {
		t.Fatal("fact does not call itself natively")
	}
	if !fe.ssa.HasEntry(self+1) || slices.Contains(fe.ssaUnresumed, int32(self+1)) {
		t.Fatalf("fact's code has no entry after its call of itself at %d: %v", self, fe.ssaUnresumed)
	}
}

// An inlined callee is the code itself: an exit inside it leaves the code
// there, as the code's own exits do, and Go goes on after the call, so the
// call keeps its entry from the first compile. AWFY's Queens: placeQueen
// built with none after its inlined getRowColumn and setRowColumn held its
// column boxed through its loop, unboxed at each use, and spilled twice
// as many values. Each answer is the interpreter's.
func TestJITEntriesAfterInlinedCalls(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	both := jitBothForTest(t, `
		var Q = class {
			constructor() { this.rows = [true, true, true, true, true, true, true, true]; this.maxs = []; this.mins = []; for (let i = 0; i < 16; i++) { this.maxs[i] = true; this.mins[i] = true } }
			get(r, c) { return this.rows[r] && this.maxs[c + r] && this.mins[c - r + 7] }
			set(r, c, v) { this.rows[r] = v; this.maxs[c + r] = v; this.mins[c - r + 7] = v }
			place(c) {
				for (let r = 0; r < 8; r += 1) {
					if (this.get(r, c)) {
						this.set(r, c, false)
						if (c === 7 || this.place(c + 1)) return true
						this.set(r, c, true)
					}
				}
				return false
			}
		}
		function run() { let n = 0; for (let i = 0; i < 4; i++) if (new Q().place(0)) n++; return n }`)
	for range 20 {
		both.agree(t, `run()`)
	}
	r := both.jit
	cl := r.global.getOwn(r.atoms.intern("Q")).value().Object().getOwn(r.atoms.intern("prototype")).value().Object().getOwn(r.atoms.intern("place")).value().Object().fn().closure
	e := r.jit.hint(cl)
	if e == nil || e.ssa == nil || len(e.ssaInlinedAt) == 0 {
		t.Fatalf("place inlines none of its calls: %+v", e)
	}
	for _, pc := range e.ssaInlinedAt {
		if !e.ssa.HasEntry(int(pc)+1) || slices.Contains(e.ssaUnresumed, pc+1) {
			t.Errorf("place's code has no entry after its inlined call at %d", pc)
		}
	}
}
