//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// KeyedStore's answers derived on the worker (jit_snapderive.go), from the
// copy of the function's caches its compile's gathers made (closureCopy),
// by the same function the feedback on the spot answers with
// (jitKeyedStoreOf): a keyed store's site is its cache's entry and its
// earlier entries, and what of the heap an addition among them reads --
// whether its next shape wants its index built, a chain's cell's addition
// -- which the gather takes on the runtime's goroutine, remembering the
// shapes the answer names as the asking remembers them.

// jitKeyedStoreData is a snapshot's copy of what its keyed stores'
// answers are made of: the closure's caches (its closure's copy's), its
// names, what of the heap the additions among the keyed sites' entries
// read (the shapes whose index an addition wants built, the additions of
// chains' cells), and the register pcs of the keyed stores gathered, each
// marked once a PC of the program asks of it (bound).
type jitKeyedStoreData struct {
	gathered bool
	ic       []propCache
	names    []Atom
	unindex  map[*shape]bool
	cells    []jitPropCellAdd
	sites    []jitKeyedSite
}

// jitKeyedSite is a keyed store a gather took, by register pc.
type jitKeyedSite struct {
	rpc   int32
	bound bool
}

func init() {
	jitSnapDerived |= 1 << snapKeyedStore
	jitSnapFuncGatherers = append(jitSnapFuncGatherers, gatherKeyedStore)
	jitSnapDerivers[jitDeriveCopies] = append(jitSnapDerivers[jitDeriveCopies], deriveKeyedStore)
}

// jitKeyedGather is the live state of a keyed store's answer, as the
// gather reads it, of the closure's copy of its caches: what the answer
// reads of the heap noted in d as it is read, the shapes it keeps
// remembered as the asking remembers them.
type jitKeyedGather struct {
	jitPropLive
	d *jitKeyedStoreData
}

func (g jitKeyedGather) cache(site int) *propCache {
	if site < len(g.d.ic) {
		return &g.d.ic[site]
	}
	return nil
}

func (g jitKeyedGather) unindexed(next *shape) bool {
	u := g.jitPropLive.unindexed(next)
	if u {
		if g.d.unindex == nil {
			g.d.unindex = map[*shape]bool{}
		}
		g.d.unindex[next] = true
	}
	return u
}

func (g jitKeyedGather) cellAdd(c *propCache) (jitDeepAdd, bool) {
	a, ok := g.jitPropLive.cellAdd(c)
	g.d.cells = append(g.d.cells, jitPropCellAdd{from: c.shape, next: c.next, p1: c.p1, add: a, ok: ok})
	return a, ok
}

// jitKeyedSrc is a snapshot's copy of a keyed store's state, as the worker
// derives its answer from it (jitPropSrc): the methods a keyed store's
// answer reads, the others answering nothing.
type jitKeyedSrc struct{ d *jitKeyedStoreData }

func (k jitKeyedSrc) cache(site int) *propCache {
	if site < len(k.d.ic) {
		return &k.d.ic[site]
	}
	return nil
}

func (k jitKeyedSrc) litRead(int, bytecode.RInstr) (Atom, *propCache, bool) { return 0, nil, false }
func (k jitKeyedSrc) knownRead(int, Atom) (*propCache, bool)                { return nil, false }
func (k jitKeyedSrc) forward(int, Atom) (ssa.PropertySite, bool)            { return ssa.PropertySite{}, false }
func (k jitKeyedSrc) polyAt(int) *jitPolySite                               { return nil }
func (k jitKeyedSrc) symbol(Atom) *Symbol                                   { return nil }
func (k jitKeyedSrc) unindexed(next *shape) bool                            { return k.d.unindex[next] }
func (k jitKeyedSrc) ctx() jitPropCtx                                       { return jitPropCtx{} }

func (k jitKeyedSrc) cellAdd(c *propCache) (jitDeepAdd, bool) {
	for i := range k.d.cells {
		if x := &k.d.cells[i]; x.from == c.shape && x.next == c.next && x.p1 == c.p1 {
			return x.add, x.ok
		}
	}
	return jitDeepAdd{}, false
}

// keep is s: the gather remembered it.
func (k jitKeyedSrc) keep(s *shape) *shape { return s }

// gatherKeyedStore takes, for s, the snapshot of fb, the copy of the
// closure's caches and, for each keyed store of its register code, what
// its answer reads of the heap, by asking it of that copy with the live
// heap (jitKeyedGather), its answer and what it keeps thrown away: the
// shapes it names remembered, as the asking at its PCs remembers them.
func gatherKeyedStore(s *jitSnapshot, fb *jitRegFeedback) {
	d := &s.d.kstore
	*d = jitKeyedStoreData{}
	cl := fb.fb.cl
	if cl == nil {
		return
	}
	d.gathered = true
	d.ic, d.names = s.root.d.closureCopy(cl).ic, cl.names
	code := jitPropCode{rc: fb.rc, names: cl.names, cl: true}
	g := jitKeyedGather{jitPropLive: jitPropLive{f: fb, r: fb.fb.r}, d: d}
	var scratch jitFeedback
	for rpc, w := range fb.rc.Code {
		op := w.Op()
		if op != bytecode.RSetIndex && op != bytecode.RDefineIndex {
			continue
		}
		d.sites = append(d.sites, jitKeyedSite{rpc: int32(rpc)})
		jitKeyedStoreAt(&code, rpc, op == bytecode.RDefineIndex, g, &scratch)
		scratch.shapes, scratch.holders = scratch.shapes[:0], scratch.holders[:0]
	}
}

// deriveKeyedStore puts s's KeyedStore answers, at every PC whose op asks
// it, derived from what its gather copied, their effects appended to its
// sink.
func deriveKeyedStore(s *jitSnapshot) {
	d := &s.d.kstore
	if !d.gathered {
		return
	}
	code := jitPropCode{rc: s.rc, p: s.p, names: d.names, cl: true}
	src := jitKeyedSrc{d}
	sink := s.sinkOf()
	for pc, op := range s.ops {
		if jitSnapAsks[op]&(1<<snapKeyedStore) == 0 {
			continue
		}
		if int(s.p.Origin[pc]) < len(s.rc.Code) {
			rpc := int32(s.p.Origin[pc])
			for i := range d.sites {
				if d.sites[i].rpc == rpc {
					d.sites[i].bound = true
				}
			}
		}
		before := s.begin(sink)
		site, ok := jitKeyedStoreOf(&code, pc, src, sink)
		eff := s.endDerived(sink, before)
		s.heard(snapKeyedStore, int64(pc), ok, site, ok)
		if ok || eff != 0 {
			s.a.KeyedStore.reserve(int(s.room[snapKeyedStore]))
			s.a.KeyedStore.put(int64(pc), eff, ok, jitSnapAnsKeyedStore{r0: site, r1: ok})
		}
	}
}
