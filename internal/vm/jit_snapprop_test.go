//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

func init() {
	jitSnapScribblers = append(jitSnapScribblers, jitPropScribble)
}

// jitPropScribble writes over what the property sites' gather copied the
// makings of (jitPropData), for TestJITSnapshotDerivesFromItsCopy: the
// closure's caches, their earlier entries, what the code learned of its
// sites and the caches the feedback made of its own.
func jitPropScribble(r *Runtime, s *jitSnapshot) (restore func()) {
	f := s.rf.fb
	var undo []func()
	if cl := f.cl; cl != nil {
		ic := append([]propCache(nil), cl.ic...)
		var polys []*propPoly
		var saved []propPoly
		var seens []*propSeen
		var savedSeen []propSeen
		for i := range cl.ic {
			if p := cl.ic[i].poly; p != nil {
				polys, saved = append(polys, p), append(saved, *p)
				if p.seen != nil {
					seens, savedSeen = append(seens, p.seen), append(savedSeen, *p.seen)
					*p.seen = propSeen{}
				}
				*p = propPoly{}
			}
			cl.ic[i] = propCache{}
		}
		undo = append(undo, func() {
			for i, p := range seens {
				*p = savedSeen[i]
			}
			for i, p := range polys {
				*p = saved[i]
			}
			copy(cl.ic, ic)
		})
	}
	if e := f.e; e != nil {
		poly, lits := e.poly, e.regLitReads
		e.poly = make([]jitPolySite, len(poly))
		e.regLitReads = nil
		undo = append(undo, func() { e.poly, e.regLitReads = poly, lits })
	}
	return func() {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
	}
}

// TestJITSnapshotPropertyUniqueShape holds a snapshot's property sites to
// the layout they were derived from when an object's unique shape, which
// a learned addition's prototype chain names, would change in place: the
// gather remembers the chain's shapes (gatherProperty's deep), so that the
// prototype's layout changing after it gives the prototype another shape,
// and the code's check of the one it was compiled for fails.
func TestJITSnapshotPropertyUniqueShape(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitRuntimeForTest(t, Config{JIT: true})
	r.jitState().unavailable = true
	if _, err := r.Run(compileForTest(t, `
		function P() {}
		var proto = {};
		for (var i = 0; i < 4; i++) proto["m" + i] = i;
		P.prototype = proto;
		function put(o, v) { o.z = v; return o }
		for (var k = 0; k < 50; k++) put(new P(), k);
	`)); err != nil {
		t.Fatal(err)
	}
	cl := jitGlobalClosureForTest(r, "put")
	c := r.regOf(cl)
	p, err := r.jitRegLower(cl, c.rc, jitLowerSSA)
	if err != nil {
		if p, err = r.jitRegLower(cl, c.rc, jitLowerInline); err != nil {
			t.Fatal(err)
		}
	}
	fb := r.jitRegFeedbackFor(cl, p)
	// A learned addition along a chain of a prototype whose shape is its
	// own and remembered by none.
	proto := r.global.getOwn(r.atoms.intern("proto")).value().Object()
	sh := proto.shape
	if sh == nil || !sh.unique {
		t.Skipf("the prototype's shape is not unique: %+v", sh)
	}
	sh.seen = false
	var write int
	for rpc, w := range c.rc.Code {
		if w.Op() == bytecode.RSetProp || w.Op() == bytecode.RAddProp {
			write = rpc
		}
	}
	from := &shape{}
	fb.fb.e.poly = append(fb.fb.e.poly, jitPolySite{pc: uint32(write), adds: []jitDeepAdd{{from: from, next: from, up: []*Object{proto}, shapes: []*shape{sh}}}})
	snap := jitRegSnapshotOf(fb, p)
	if !sh.seen {
		t.Fatal("the gather left the chain's unique shape unremembered")
	}
	// Changed after: another shape, not the one remembered changed in place.
	if _, err := r.Run(compileForTest(t, `proto.later = 1`)); err != nil {
		t.Fatal(err)
	}
	if proto.shape == sh {
		t.Fatal("the remembered shape was changed in place")
	}
	snap.s.derive()
}
