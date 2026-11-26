//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"os"
	"reflect"
	"testing"
)

// jitSnapScribblers are the derived methods' families' scribblers: each
// writes over what its gather copies the makings of in the runtime's state
// for the snapshot s -- the caches, what the code learned -- and gives
// back the function that puts it back. A derivation that reads the live
// state rather than its copy then answers otherwise. Each family's test
// file appends its own in an init.
var jitSnapScribblers []func(r *Runtime, s *jitSnapshot) (restore func())

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
		add(2, len(l.cells), func(j int) any { return l.cells[j] })
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
	if jitSnapDerived == 0 {
		t.Skip("no method is derived on the worker")
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
		// The feedback's answers, asked as the snapshot gathered.
		live := map[question]heard{}
		var order []question
		for _, s := range append([]*jitSnapshot{snap.s}, mapValues(snap.s.callees)...) {
			if s.rf == nil {
				continue
			}
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
				if jitSnapDerived&(1<<m) == 0 || jitRegSnapMethods&(1<<m) == 0 || jitRegSnapStatePC&(1<<m) != 0 {
					continue
				}
				if !jitSnapAtPC[m] {
					if reflect.ValueOf(s.rf).MethodByName(jitSnapNames[m]).Type().NumIn() == 0 {
						ask(m, 0)
					}
					continue
				}
				for pc, op := range s.ops {
					if jitSnapAsks[op]&(1<<m) != 0 {
						ask(m, int64(pc))
					}
				}
			}
		}
		var restores []func()
		for _, scribble := range jitSnapScribblers {
			for _, s := range append([]*jitSnapshot{snap.s}, mapValues(snap.s.callees)...) {
				if s.rf != nil {
					restores = append(restores, scribble(r, s))
				}
			}
		}
		snap.s.derive()
		for i := len(restores) - 1; i >= 0; i-- {
			restores[i]()
		}
		for _, q := range order {
			want := live[q]
			method := reflect.ValueOf(q.s.reg).MethodByName(jitSnapNames[q.m])
			var in []reflect.Value
			if method.Type().NumIn() == 1 {
				in = []reflect.Value{reflect.ValueOf(q.arg).Convert(method.Type().In(0))}
			}
			used := len(snap.s.uses)
			got := method.Call(in)
			for i, g := range got {
				if !reflect.DeepEqual(g.Interface(), want.results[i]) {
					t.Errorf("%s, %d callees deep: %s(%d) result %d: derived %+v, asked %+v", cl.fn.Name, q.s.depth, jitSnapNames[q.m], q.arg, i, g.Interface(), want.results[i])
				}
			}
			var own, kept jitSnapLists
			if len(snap.s.uses) > used {
				u := snap.s.uses[used]
				own, kept = snap.s.lists(u.s.effects[u.eff-1])
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
		if t.Failed() {
			return
		}
	}
	if functions == 0 || compared == 0 {
		t.Fatalf("%d functions, %d answers compared", functions, compared)
	}
	t.Logf("%d functions, %d questions compared", functions, compared)
}
