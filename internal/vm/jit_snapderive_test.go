//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"math"
	"os"
	"reflect"
	"slices"
	"testing"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/jit"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// jitSnapScribblers are the derived methods' families' scribblers: each
// writes over what its gather copies the makings of in the runtime's state
// for the snapshot s -- the caches, what the code learned -- and gives
// back the function that puts it back. A derivation that reads the live
// state rather than its copy then answers otherwise. Each family's test
// file appends its own in an init.
var jitSnapScribblers []func(r *Runtime, s *jitSnapshot) (restore func())

// jitSnapGatheredAt are the families' reports of a question of a derived
// method that their gather asked of the feedback on the runtime's
// goroutine, the method derived at the other PCs: the snapshot holds the
// effect that asking took, which the feedback asked again gives no more
// (askedOf), so it is not compared. Each family's test file appends its
// own in an init.
var jitSnapGatheredAt []func(s *jitSnapshot, m jitSnapMethod, arg int64) bool

// jitSnapLiveAsk is what call, asking fb a question, appended to fb's
// lists and to its keep's, fb's left as they were: the effect the
// feedback's own asking has.
func jitSnapLiveAsk(fb *jitFeedback, call func()) (own, kept jitSnapLists) {
	k := fb.keep()
	before, beforeKept := jitSnapLensOf(fb), jitSnapLensOf(k)
	call()
	own = jitSnapCut(fb, before)
	if k != fb {
		kept = jitSnapCut(k, beforeKept)
	}
	return own, kept
}

// jitSnapCut is what f's lists gained past the lengths n, copied, f's cut
// back to them.
func jitSnapCut(f *jitFeedback, n jitSnapLens) (l jitSnapLists) {
	l.shapes, f.shapes = append([]*shape(nil), f.shapes[n[0]:]...), f.shapes[:n[0]]
	l.holders, f.holders = append([]*Object(nil), f.holders[n[1]:]...), f.holders[:n[1]]
	l.cells, f.cells = append([]*Value(nil), f.cells[n[2]:]...), f.cells[:n[2]]
	l.links, f.links = append([]*jitLink(nil), f.links[n[3]:]...), f.links[:n[3]]
	l.inlined, f.inlined = append([]*closure(nil), f.inlined[n[4]:]...), f.inlined[:n[4]]
	l.made, f.made = append([]*closure(nil), f.made[n[5]:]...), f.made[:n[5]]
	l.linked, f.linked = append([]*closure(nil), f.linked[n[6]:]...), f.linked[:n[6]]
	l.pools, f.pools = append(l.pools, f.pools[n[7]:]...), f.pools[:n[7]]
	return l
}

// jitSnapListsEqual reports whether a and b hold the same, empty and nil
// alike.
func jitSnapListsEqual(a, b jitSnapLists) bool {
	norm := func(l jitSnapLists) [8][]any {
		var out [8][]any
		add := func(i int, n int, at func(int) any) {
			for j := range n {
				out[i] = append(out[i], at(j))
			}
		}
		add(0, len(l.shapes), func(j int) any { return l.shapes[j] })
		add(1, len(l.holders), func(j int) any { return l.holders[j] })
		add(2, len(l.cells), func(j int) any { return jitSnapCellOf(l.cells[j]) })
		add(3, len(l.links), func(j int) any { return l.links[j] })
		add(4, len(l.inlined), func(j int) any { return l.inlined[j] })
		add(5, len(l.made), func(j int) any { return l.made[j] })
		add(6, len(l.linked), func(j int) any { return l.linked[j] })
		add(7, len(l.pools), func(j int) any { return l.pools[j] })
		return out
	}
	return reflect.DeepEqual(norm(a), norm(b))
}

// TestJITSnapshotDerivesFromItsCopy holds the answers a snapshot's worker
// derives (jit_snapderive.go) to the feedback's, asked of the runtime as
// it was when the snapshot gathered them: for every derived method, at
// every PC whose op asks it, or once where it takes no argument, in the
// function and in each callee the snapshot asked, the derived answer and
// its effect -- what it appended to the feedback's lists and its keep's --
// are the feedback's, with what the gathers copied the makings of
// scribbled over in the runtime (jitSnapScribblers) while the worker
// derives; and every shape an effect keeps is one that no change of an
// object's layout changes in place -- shared, or remembered (remember) --
// so that the code's checks of it hold for the layout the answer was
// derived from. The functions are jitSnapAskedSrc's, and with
// QUICKJS_JIT_CORPUS, Octane's after a warm-up.
func TestJITSnapshotDerivesFromItsCopy(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	if jitSnapDerived&^jitSnapAskedAtGather == 0 {
		t.Skip("no method is derived on the worker")
	}
	// The feedback each snapshot was asked of, asked again here.
	jitTestSwitch(t, func(c *jitTestConfig) { c.snapKeepsFeedback = true })
	var r *Runtime
	var cls []*closure
	if dir := os.Getenv("QUICKJS_JIT_CORPUS"); dir != "" {
		r, cls = jitOctaneCorpusForTest(t, dir)
	} else {
		r = jitRuntimeForTest(t, Config{JIT: true})
		r.jitState().unavailable = true
		if _, err := r.Run(compileForTest(t, jitSnapAskedSrc)); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"work", "make", "add", "Point"} {
			cls = append(cls, jitGlobalClosureForTest(r, name))
		}
	}
	r.jitState().unavailable = false
	type question struct {
		s   *jitSnapshot
		m   jitSnapMethod
		arg int64
	}
	type heard struct {
		results   []any
		own, kept jitSnapLists
	}
	compared, functions := 0, 0
	for _, cl := range cls {
		c := r.regOf(cl)
		if c == nil {
			continue
		}
		p, err := r.jitRegLower(cl, c.rc, jitLowerSSA)
		if err != nil {
			if p, err = r.jitRegLower(cl, c.rc, jitLowerInline); err != nil {
				continue
			}
		}
		r.jitRegSeenFeedback(cl, c)
		snap := jitRegSnapshotOf(r.jitRegFeedbackFor(cl, p), p)
		functions++
		// The feedback's answers, asked as the snapshot gathered: of the
		// snapshots bound to their programs, here; of the walk's nodes a
		// derivation binds (jit_snapwalk.go), once it has, their programs
		// lowered on the worker.
		all := append([]*jitSnapshot{snap.s}, mapValues(snap.s.callees)...)
		bound := map[*jitSnapshot]bool{}
		for _, s := range all {
			bound[s] = s.p != nil
		}
		live := map[question]heard{}
		var order []question
		askAll := func(s *jitSnapshot) {
			ask := func(m jitSnapMethod, arg int64) {
				method := reflect.ValueOf(s.rf).MethodByName(jitSnapNames[m])
				var in []reflect.Value
				if method.Type().NumIn() == 1 {
					in = []reflect.Value{reflect.ValueOf(arg).Convert(method.Type().In(0))}
				}
				var got []reflect.Value
				own, kept := jitSnapLiveAsk(s.rf.fb, func() { got = method.Call(in) })
				h := heard{own: own, kept: kept}
				for _, g := range got {
					h.results = append(h.results, g.Interface())
				}
				q := question{s, m, arg}
				live[q] = h
				order = append(order, q)
			}
			for m := range jitSnapNames {
				m := jitSnapMethod(m)
				if (jitSnapDerived&^jitSnapAskedAtGather)&(1<<m) == 0 || jitRegSnapMethods&(1<<m) == 0 || jitRegSnapStatePC&(1<<m) != 0 {
					continue
				}
				if reflect.ValueOf(s.rf).MethodByName(jitSnapNames[m]).Type().NumIn() > 1 {
					// A leaf's call's (CreatePool, ExecReuse), asked by the
					// leaf its PC's Leaf answered: TestJITSnapshotAnswersAsAsked's.
					continue
				}
				if !jitSnapAtPC[m] {
					if reflect.ValueOf(s.rf).MethodByName(jitSnapNames[m]).Type().NumIn() == 0 {
						ask(m, 0)
					}
					continue
				}
				for pc, op := range s.ops {
					if jitSnapAsks[op]&(1<<m) != 0 && !slices.ContainsFunc(jitSnapGatheredAt, func(g func(*jitSnapshot, jitSnapMethod, int64) bool) bool {
						return g(s, m, int64(pc))
					}) {
						ask(m, int64(pc))
					}
				}
			}
		}
		compare := func() {
			for _, q := range order {
				want := live[q]
				method := reflect.ValueOf(q.s.reg).MethodByName(jitSnapNames[q.m])
				var in []reflect.Value
				if method.Type().NumIn() == 1 {
					in = []reflect.Value{reflect.ValueOf(q.arg).Convert(method.Type().In(0))}
				}
				used := len(snap.s.uses)
				got := method.Call(in)
				var own, kept jitSnapLists
				if len(snap.s.uses) > used {
					u := snap.s.uses[used]
					own, kept = snap.s.lists(u.s.effects[u.eff-1])
				}
				for i, g := range got {
					gv, wv := g.Interface(), want.results[i]
					if i == 0 && jitSnapCellResults&(1<<q.m) != 0 {
						// A cell made anew at each question, which its effect
						// keeps: alike by what it holds.
						gv, wv = jitSnapCellAt(gv.(uintptr), own, kept), jitSnapCellAt(wv.(uintptr), want.own, want.kept)
					}
					// An inline site's: its callee's feedback by its closure,
					// its cells by what they hold.
					gv, wv = jitSnapInlineNormal(gv, own, kept), jitSnapInlineNormal(wv, want.own, want.kept)
					if !reflect.DeepEqual(gv, wv) {
						t.Errorf("%s, %d callees deep: %s(%d) result %d: derived %+v, asked %+v", cl.fn.Name, q.s.depth, jitSnapNames[q.m], q.arg, i, g.Interface(), want.results[i])
					}
				}
				if !jitSnapListsEqual(own, want.own) || !jitSnapListsEqual(kept, want.kept) {
					t.Errorf("%s, %d callees deep: %s(%d): derived effect %+v, kept %+v; asked %+v, kept %+v", cl.fn.Name, q.s.depth, jitSnapNames[q.m], q.arg, own, kept, want.own, want.kept)
				}
				for _, l := range []jitSnapLists{own, kept} {
					for _, sh := range l.shapes {
						if sh != nil && sh.unique && !sh.seen {
							t.Errorf("%s, %d callees deep: %s(%d) keeps a unique shape no cache remembered", cl.fn.Name, q.s.depth, jitSnapNames[q.m], q.arg)
						}
					}
				}
				compared++
			}
			clear(live)
			order = order[:0]
		}
		for _, s := range all {
			if s.rf != nil && bound[s] {
				askAll(s)
			}
		}
		var restores []func()
		for _, scribble := range jitSnapScribblers {
			for _, s := range all {
				if s.rf != nil {
					restores = append(restores, scribble(r, s))
				}
			}
		}
		snap.s.derive()
		for i := len(restores) - 1; i >= 0; i-- {
			restores[i]()
		}
		compare()
		for _, s := range all {
			if s.rf != nil && !bound[s] && s.p != nil {
				// A node a derivation took, bound on the worker: its feedback
				// asked of the program it was bound to.
				s.rf.p = s.p
				askAll(s)
			}
		}
		compare()
		if t.Failed() {
			return
		}
	}
	if functions == 0 || compared == 0 {
		t.Fatalf("%d functions, %d answers compared", functions, compared)
	}
	t.Logf("%d functions, %d questions compared", functions, compared)
}

// A compile in the background installed keeps no snapshot: its copies and
// answers are done with at the install, whatever keeps the job.
func TestJITBackgroundInstallDropsSnapshot(t *testing.T) {
	r := jitBackgroundForTest(t, 0)
	cl := jitGlobalClosureForTest(r, "sum")
	stress := r.jitStress
	r.jitBackground, r.jitStress = jitOnSpot, jitStressConfig{}
	for range 2 {
		jitCallForTest(t, r, `sum(10)`, 45)
	}
	r.jitBackground, r.jitStress = jitInWorkers, stress
	old := jitRegOfForTest(r, cl)
	j := jitRegJobForTest(t, r, cl)
	r.jitRegSubmit(j)
	if r.jit.regPendingJobs[j.key] != nil || jitRegOfForTest(r, cl) == old {
		t.Fatalf("pending %v, same code %v: want it installed", r.jit.regPendingJobs[j.key] != nil, jitRegOfForTest(r, cl) == old)
	}
	if j.snap != nil {
		t.Fatal("the installed job keeps its snapshot")
	}
	jitCallForTest(t, r, `sum(100)`, 4950)
}

// TestJITSnapshotBuildsTheSame holds a compile in the background to one
// answer for one state: two snapshots of a function asked back to back,
// nothing run between them, build the same function, again and again --
// no order of a map's or of the asking's that varies from one to the next
// decides what is built. The functions are jitSnapAskedSrc's, and with
// QUICKJS_JIT_CORPUS, Octane's.
func TestJITSnapshotBuildsTheSame(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	var r *Runtime
	var cls []*closure
	if dir := os.Getenv("QUICKJS_JIT_CORPUS"); dir != "" {
		r, cls = jitOctaneCorpusForTest(t, dir)
	} else {
		r = jitRuntimeForTest(t, Config{JIT: true})
		r.jitState().unavailable = true
		if _, err := r.Run(compileForTest(t, jitSnapAskedSrc)); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"work", "make", "add", "Point"} {
			cls = append(cls, jitGlobalClosureForTest(r, name))
		}
	}
	r.jitState().unavailable = false
	built, differ := 0, 0
	for _, cl := range cls {
		c := r.regOf(cl)
		if c == nil {
			continue
		}
		if _, err := r.jitRegLower(cl, c.rc, jitLowerSSA); err != nil {
			continue
		}
		var first string
		for i := range 4 {
			j := jitRegJobForTest(t, r, cl)
			listing := jitSnapBuildForTest(t, j, j.snap)
			jit.PutWorkspace(j.w)
			if i == 0 {
				first = listing
				continue
			}
			if listing != first {
				differ++
				if differ <= 5 {
					t.Errorf("%s: build %d differs from the first:\n%s\nfirst:\n%s", cl.fn.Name, i, listing, first)
				}
				break
			}
		}
		built++
	}
	if built == 0 {
		t.Fatal("nothing built")
	}
	t.Logf("%d functions built four times each, %d differed", built, differ)
}

// TestJITSnapshotDeriveStages holds a snapshot's derivations to run stage
// by stage (jitSnapDeriveStage), each stage's after every earlier one's,
// in the order each stage's were appended, whatever the files that
// appended them: Inline's appends to the learned calls are there for
// NativeCalls' and Resumed's.
func TestJITSnapshotDeriveStages(t *testing.T) {
	var ran []int
	var derivers [jitDeriveStages][]func(s *jitSnapshot)
	for i := jitDeriveStages; i > 0; i-- {
		stage := i - 1
		for k := range 2 {
			at := int(stage)*2 + k
			derivers[stage] = append(derivers[stage], func(*jitSnapshot) { ran = append(ran, at) })
		}
	}
	(&jitSnapshot{}).runDerivers(&derivers)
	for i, at := range ran {
		if at != i {
			t.Fatalf("derivations ran in the order %v", ran)
		}
	}
	if len(ran) != 2*int(jitDeriveStages) {
		t.Fatalf("%d derivations of %d ran", len(ran), 2*int(jitDeriveStages))
	}
}

// jitSnapInlineNormal is v, a result of a method's, with what an inline
// site holds that a derivation makes otherwise than the feedback alike:
// its callee's feedback by its closure (a snapshot's, the feedback's), and
// the cells it names by what they hold, of the effects own and kept.
func jitSnapInlineNormal(v any, own, kept jitSnapLists) any {
	cell := func(p uintptr) any {
		if p == 0 {
			return nil
		}
		return jitSnapCellAt(p, own, kept)
	}
	site := func(in ssa.InlineSite) any {
		var cl *closure
		switch f := in.Feedback.(type) {
		case *jitRegFeedback:
			cl = f.fb.cl
		case *jitRegSnapshot:
			cl = f.s.fb.cl
		}
		this, fn := cell(in.ThisCell), cell(in.FunctionCell)
		in.Feedback, in.ThisCell, in.FunctionCell = nil, 0, 0
		return struct {
			In        ssa.InlineSite
			Cl        *closure
			This, Fun any
		}{in, cl, this, fn}
	}
	made := func(m *ssa.MadeCallback) any {
		if m == nil {
			return nil
		}
		c := *m
		t := cell(c.Template)
		c.Template = 0
		return struct {
			M ssa.MadeCallback
			T any
		}{c, t}
	}
	switch x := v.(type) {
	case ssa.InlineSite:
		return site(x)
	case ssa.MadeCallback:
		return made(&x)
	case ssa.ForEachSite:
		var in any
		if x.Inline != nil {
			in = site(*x.Inline)
		}
		m := made(x.Made)
		x.Inline, x.Made = nil, nil
		return struct {
			F      ssa.ForEachSite
			In, Ma any
		}{x, in, m}
	}
	return v
}

// jitSnapCellResults are the methods whose first result is a cell made
// anew at each question (StringCell, BigIntCell), which a derivation's and
// the feedback's answers hold alike by what it holds.
const jitSnapCellResults uint64 = 1<<snapStringCell | 1<<snapBigIntCell

// jitSnapCellOf is what the cell c holds, as words: two cells made anew
// alike compare equal.
func jitSnapCellOf(c *Value) [2]uintptr {
	if c == nil {
		return [2]uintptr{}
	}
	return [2]uintptr{uintptr(math.Float64bits(c.num)), uintptr(c.ref)}
}

// jitSnapCellAt is what the cell at address p, one of the effects' own and
// kept keep, holds; p itself for a cell they do not keep.
func jitSnapCellAt(p uintptr, own, kept jitSnapLists) any {
	for _, l := range []jitSnapLists{own, kept} {
		for _, c := range l.cells {
			if uintptr(unsafe.Pointer(c)) == p {
				return jitSnapCellOf(c)
			}
		}
	}
	return p
}
