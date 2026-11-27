//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"slices"
	"testing"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// The calls' targets' scribbler (jitSnapScribblers): the function's learned
// calls taken away, as exits write them anew, and the realm's apply and
// global object, which the targets' sites compare with; and their gather's
// questions, which the comparison leaves (jitSnapGatheredAt): NativeCalls
// where it reads the heap.
func init() {
	jitSnapScribblers = append(jitSnapScribblers, func(r *Runtime, s *jitSnapshot) func() {
		var restores []func()
		if e := s.rf.fb.e; e != nil && e.jitLearned != nil {
			calls, inlines, vias := e.nativeCalls, e.inlines, e.viaLeaves
			e.nativeCalls, e.inlines, e.viaLeaves = nil, nil, nil
			restores = append(restores, func() { e.nativeCalls, e.inlines, e.viaLeaves = calls, inlines, vias })
		}
		applyFn, global := r.applyFn, r.globalThis
		r.applyFn, r.globalThis = nil, Undefined
		restores = append(restores, func() { r.applyFn, r.globalThis = applyFn, global })
		return func() {
			for i := len(restores) - 1; i >= 0; i-- {
				restores[i]()
			}
		}
	})
	jitSnapGatheredAt = append(jitSnapGatheredAt, func(s *jitSnapshot, m jitSnapMethod, arg int64) bool {
		t := &s.d.calls.tg
		if m != snapNativeCalls && m != snapViaLeaf {
			return false
		}
		if !t.gathered {
			// Asked at its PCs (jitTargetsAskedAt).
			return true
		}
		// A construction whose pool the gather learned, which the
		// feedback asked again finds, where the asking's question had
		// not (allocHidden).
		rpc := s.p.Origin[arg]
		for _, h := range t.hidden {
			if uint32(h.rpc) == rpc {
				return true
			}
		}
		return false
	})
}

// TestJITLearnedCopyCut holds a snapshot's copy of a function's learned
// calls (jitLearnedCopy) to be cut to its lengths, its capacity too: an
// append to it makes a list of its own, the backing array the live list
// and its exits append to left as it was past the copy's length -- where
// an exit's append, on the runtime's goroutine, writes while a worker
// derives. Asked of a list with room past its length, and of every
// snapshot of a compile of jitSnapAskedSrc's functions, whose
// derivations append to their copies (regCallTargets), and one more
// append through each copy.
func TestJITLearnedCopyCut(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	o := &Object{}
	live := &jitEntry{jitLearned: &jitLearned{}}
	live.nativeCalls = make([]jitInline, 1, 4)
	live.nativeCalls[0] = jitInline{pc: 1}
	room := live.nativeCalls[1:4]
	for i := range room {
		room[i] = jitInline{pc: int32(100 + i)}
	}
	c := jitLearnedCopy(live)
	if cap(c.nativeCalls) != len(c.nativeCalls) || cap(c.inlines) != len(c.inlines) {
		t.Fatalf("a copy's capacity past its length: nativeCalls %d of %d, inlines %d of %d", len(c.nativeCalls), cap(c.nativeCalls), len(c.inlines), cap(c.inlines))
	}
	c.nativeCalls = append(c.nativeCalls, jitInline{pc: 7, obj: o})
	for i, x := range room {
		if x.pc != int32(100+i) || x.obj != nil {
			t.Fatalf("the live list's room written past its length at %d: %+v", 1+i, x)
		}
	}
	if unsafe.SliceData(c.nativeCalls) == unsafe.SliceData(live.nativeCalls) {
		t.Fatal("the copy's append wrote into the live list's array")
	}

	r := jitRuntimeForTest(t, Config{JIT: true})
	r.jitState().unavailable = true
	if _, err := r.Run(compileForTest(t, jitSnapAskedSrc)); err != nil {
		t.Fatal(err)
	}
	r.jitState().unavailable = false
	appended := 0
	for _, name := range []string{"work", "make", "add", "Point"} {
		cl := jitGlobalClosureForTest(r, name)
		rc := r.regOf(cl)
		if rc == nil {
			continue
		}
		p, err := r.jitRegLower(cl, rc.rc, jitLowerSSA)
		if err != nil {
			continue
		}
		r.jitRegSeenFeedback(cl, rc)
		snap := jitRegSnapshotOf(r.jitRegFeedbackFor(cl, p), p)
		type room struct {
			tg   *jitTargetsData
			past []jitInline
		}
		var rooms []room
		for _, s := range snap.s.d.nodes {
			tg := &s.d.calls.tg
			if !tg.gathered {
				continue
			}
			l := tg.live.nativeCalls
			rooms = append(rooms, room{tg, slices.Clone(l[len(l):cap(l)])})
		}
		snap.s.derive()
		for _, x := range rooms {
			// And an append through the copy, as a derivation's.
			x.tg.learned.nativeCalls = append(x.tg.learned.nativeCalls, jitInline{pc: 1 << 30, obj: o})
			l := x.tg.live.nativeCalls
			if past := l[len(l):cap(l)]; !slices.Equal(past, x.past) {
				t.Errorf("%s: a derivation wrote the live list's room past its length", name)
			}
			appended += len(x.tg.learned.nativeCalls) - x.tg.nNative
		}
	}
	if appended == 0 {
		t.Error("no derivation appended to its copy: the test does not reach the appends")
	}
}

// TestJITSnapshotFinishWritesTheAsking holds a compile's finish
// (jitSnapshot's finish, jitTargetsFinish) to leave a function's learned
// calls as the asking on the runtime's goroutine had: what the
// snapshot's derivations appended to the copy written after what the list
// had when copied, before what an exit appended meanwhile, in the asking's
// order for each call -- the feedback's own NativeCalls, each PC's Inline
// first, as the snapshot asks them, appended them before the exit; the
// calls apart, as their readers read them (jitLearnedByCall) -- whether the worker
// derived it, or the finish of a compile thrown away before a worker came
// to it derives it (FinishDerives).
func TestJITSnapshotFinishWritesTheAsking(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, derived := range []bool{true, false} {
		r := jitRuntimeForTest(t, Config{JIT: true})
		r.jitState().unavailable = true
		if _, err := r.Run(compileForTest(t, jitSnapAskedSrc)); err != nil {
			t.Fatal(err)
		}
		r.jitState().unavailable = false
		checked := 0
		for _, name := range []string{"work", "make", "add", "Point"} {
			cl := jitGlobalClosureForTest(r, name)
			rc := r.regOf(cl)
			if rc == nil {
				continue
			}
			p, err := r.jitRegLower(cl, rc.rc, jitLowerSSA)
			if err != nil {
				continue
			}
			r.jitRegSeenFeedback(cl, rc)
			e := r.jitState().regLearnedOf(r.jitRegKeyOf(cl))
			if e == nil || e.jitLearned == nil {
				continue
			}
			before := e.nativeCalls
			// The asking, as the snapshot asks it: Inline, then NativeCalls,
			// at each PC.
			fb := r.jitRegFeedbackFor(cl, p)
			for pc := range p.Code {
				op := p.Code[pc].Op
				if jitSnapAsks[op]&(1<<snapInline) != 0 {
					fb.Inline(pc)
				}
				if jitSnapAsks[op]&(1<<snapNativeCalls) != 0 {
					fb.NativeCalls(pc)
				}
			}
			want := jitLearnedCalls(e.nativeCalls)
			e.nativeCalls = slices.Clip(before)
			snap := jitRegSnapshotOf(r.jitRegFeedbackFor(cl, p), p)
			if derived {
				snap.s.derive()
			}
			// An exit's, meanwhile: of a call the function makes, of an
			// object no call is known to make.
			exit := jitInline{pc: 0, obj: &Object{}, cl: cl}
			e.nativeCalls = append(e.nativeCalls, exit)
			finishes := r.jitState().goroutineTimes.FinishDerives
			snap.s.finish()
			if n := r.jitState().goroutineTimes.FinishDerives - finishes; n != map[bool]int{true: 0, false: 1}[derived] {
				t.Errorf("%s: finished with %d derivations on the goroutine, derived before %v", name, n, derived)
			}
			want = append(want, jitLearnedCalls([]jitInline{exit})...)
			if got := jitLearnedCalls(e.nativeCalls); !slices.Equal(jitLearnedByCall(got), jitLearnedByCall(want)) {
				t.Errorf("%s, derived before %v: finished\n%+v\nasked\n%+v", name, derived, got, want)
			}
			checked++
		}
		if checked == 0 {
			t.Fatal("no function checked")
		}
	}
}

// jitLearnedByCall is calls in the order of their calls' register pcs,
// each call's in its own order: what readers of a learned list depend on,
// who read a call's entries apart from the others' (jitLearnedMerge).
func jitLearnedByCall(calls []jitLearnedCall) []jitLearnedCall {
	out := slices.Clone(calls)
	slices.SortStableFunc(out, func(a, b jitLearnedCall) int { return int(a.pc) - int(b.pc) })
	return out
}

// TestJITSnapshotConstructionSeed holds a snapshot's answer at a
// construction whose pool no question has learned yet to the asking's:
// its gather learns it (jitTargetsData's seed), as the asking's first
// question of the construction would -- Inline's, where it asks
// regCallTargets, or NativeCalls', which looks for an array's pool before
// it learns one (regAllocSite) and so answers a call of Go -- and the
// worker's NativeCalls answers as the asking's: new Array() made from the
// pool where Inline's question learned it first, a call of Go where
// Inline asks nothing there (notInline). The asking is the feedback's,
// each PC's Inline before its NativeCalls, of a runtime that ran the
// same code.
func TestJITSnapshotConstructionSeed(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const src = `function f(n) { let s = 0; for (let i = 0; i < n; i++) s += new Array().length + i; return s }
for (let k = 0; k < 20; k++) f(50)`
	for _, notInline := range []bool{false, true} {
		var answers [2][]ssa.CallSite
		for side := range 2 {
			r := jitRuntimeForTest(t, Config{JIT: true})
			r.jitState().unavailable = true
			if _, err := r.Run(compileForTest(t, src)); err != nil {
				t.Fatal(err)
			}
			r.jitState().unavailable = false
			cl := jitGlobalClosureForTest(r, "f")
			c := r.regOf(cl)
			p, err := r.jitRegLower(cl, c.rc, jitLowerSSA)
			if err != nil {
				t.Fatal(err)
			}
			pc := -1
			for i := range p.Code {
				if w, op, ok := jitRegAt(p, c.rc, i); ok && op == ir.Host && w.Op() == bytecode.RNewW {
					pc = i
				}
			}
			if pc < 0 {
				t.Fatal("no construction")
			}
			e := r.jitState().regLearnedOf(r.jitRegKeyOf(cl))
			if jitLearnedAt(e.nativeCalls, int(p.Origin[pc]), true) {
				t.Fatal("a pool learned before the question")
			}
			if notInline {
				e.notInline = append(slices.Clip(e.notInline), int32(p.Origin[pc]))
			}
			if side == 0 {
				fb := r.jitRegFeedbackFor(cl, p)
				fb.Inline(pc)
				answers[0] = fb.NativeCalls(pc)
			} else {
				snap := jitRegSnapshotOf(r.jitRegFeedbackFor(cl, p), p)
				snap.s.derive()
				answers[1] = snap.NativeCalls(pc)
			}
		}
		asked, derived := answers[0], answers[1]
		if len(asked) != 1 || len(derived) != 1 {
			t.Fatalf("notInline %v: asked %+v, derived %+v", notInline, asked, derived)
		}
		if asked[0].Alloc != !notInline || derived[0].Alloc != asked[0].Alloc || derived[0].Go != asked[0].Go {
			t.Errorf("notInline %v: asked Alloc %v Go %v, derived Alloc %v Go %v; want Alloc %v",
				notInline, asked[0].Alloc, asked[0].Go, derived[0].Alloc, derived[0].Go, !notInline)
		}
	}
}
