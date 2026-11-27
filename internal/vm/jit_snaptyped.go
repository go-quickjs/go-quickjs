//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// TypedKind's answers in a snapshot, asked by register pc on the
// runtime's goroutine (gatherTypedKind) and put at the program's PCs
// lowered of each (bindTypedKind): an answer is a cell of the function's
// lowering, made at the first ask and counted in what the JIT holds, which
// native code writes the element kinds it meets into -- the runtime's,
// made and read on its goroutine alone. The gather asks it at the register
// instructions an element's read or write is lowered of (jitTypedKindOps),
// every one the program's element reads and writes are lowered of, so that
// it makes the cells the asking at the PCs makes, and no other.

// jitTypedKindOps reports whether register op is one the lowering makes
// an element's read, write or update of (ir.ArrayRead, ArrayWrite,
// ArrayUpdate): the indexed reads and writes, their steps' and their
// additions', and a typed module's function table's read.
func jitTypedKindOps(op bytecode.ROp) bool {
	switch op {
	case bytecode.RGetIndex, bytecode.RGetIndexI, bytecode.RGetIndexPre, bytecode.RGetIndexPost,
		bytecode.RSetIndex, bytecode.RSetIndexI, bytecode.RAddIndex, bytecode.RAddIndexK, bytecode.RAddIndexI,
		bytecode.TTabGet:
		return true
	}
	return false
}

// jitTypedKindData is a snapshot's TypedKind answers, by register pc, and
// the PCs of its program asking it that the gather asked at no register
// pc of (missed), which a test counts.
type jitTypedKindData struct {
	asked  []jitPropAsked[jitSnapAnsTypedKind]
	missed int
}

func init() {
	jitSnapDerived |= 1 << snapTypedKind
	jitSnapAskedAtGather |= 1 << snapTypedKind
	jitSnapFuncGatherers = append(jitSnapFuncGatherers, gatherTypedKind)
	jitSnapBinders = append(jitSnapBinders, bindTypedKind)
}

// gatherTypedKind asks fb TypedKind's answer at each register instruction
// an element's read or write is lowered of, for s, as the asking at its
// PCs does: the effects taken back, kept with the answer.
func gatherTypedKind(s *jitSnapshot, fb *jitRegFeedback) {
	d := &s.d.typed
	*d = jitTypedKindData{}
	if fb.fb.r.jit == nil {
		return
	}
	for rpc, w := range fb.rc.Code {
		if !jitTypedKindOps(w.Op()) {
			continue
		}
		before := s.begin(fb.fb)
		r0, r1 := fb.typedKindAt(rpc)
		eff := s.end(fb.fb, before)
		if r1 || eff != 0 {
			d.asked = append(d.asked, jitPropAsked[jitSnapAnsTypedKind]{rpc: int32(rpc), eff: eff, ok: r1, v: jitSnapAnsTypedKind{r0: r0, r1: r1}})
		}
	}
}

// bindTypedKind puts the answers gatherTypedKind asked by register pc at
// each PC of s's program lowered of it whose op asks TypedKind.
func bindTypedKind(s *jitSnapshot) {
	d := &s.d.typed
	for pc, op := range s.ops {
		if jitSnapAsks[op]&(1<<snapTypedKind) == 0 {
			continue
		}
		var a *jitPropAsked[jitSnapAnsTypedKind]
		if int(s.p.Origin[pc]) < len(s.rc.Code) {
			rpc := int32(s.p.Origin[pc])
			for i := range d.asked {
				if d.asked[i].rpc == rpc {
					a = &d.asked[i]
					break
				}
			}
		}
		if a == nil {
			// Asked at no register pc: the gather's ops missed it.
			d.missed++
			s.heard(snapTypedKind, int64(pc), false, ssa.TypedSite{}, false)
			continue
		}
		a.bound = true
		s.heard(snapTypedKind, int64(pc), a.ok, a.v.r0, a.v.r1)
		s.a.TypedKind.reserve(int(s.room[snapTypedKind]))
		s.a.TypedKind.put(int64(pc), a.eff, a.ok, a.v)
	}
}
