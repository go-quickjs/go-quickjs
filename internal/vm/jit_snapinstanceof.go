//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// InstanceOf's answers in a snapshot, asked by register pc on the
// runtime's goroutine (gatherInstanceOf) and put at the program's PCs
// lowered of each (bindInstanceOf): an answer reads the heap -- the
// constructor its class register holds now (regDefined), its prototype
// made if it is not yet (instanceOfSite) -- and is asked of the register
// code, not the program, so that a snapshot gathered before its function
// is lowered has it.

// jitInstanceOfData is a snapshot's InstanceOf answers, by register pc.
type jitInstanceOfData struct {
	asked []jitPropAsked[jitSnapAnsInstanceOf]
}

func init() {
	jitSnapDerived |= 1 << snapInstanceOf
	jitSnapAskedAtGather |= 1 << snapInstanceOf
	jitSnapFuncGatherers = append(jitSnapFuncGatherers, gatherInstanceOf)
	jitSnapBinders = append(jitSnapBinders, bindInstanceOf)
}

// gatherInstanceOf asks fb InstanceOf's answer at each register pc of an
// instanceof, for s, as the asking at its PCs does: the effects taken
// back, kept with the answer.
func gatherInstanceOf(s *jitSnapshot, fb *jitRegFeedback) {
	d := &s.d.inst
	*d = jitInstanceOfData{}
	if fb.fb.r.hasInstanceFn == nil {
		return
	}
	for rpc, w := range fb.rc.Code {
		if op := w.Op(); op != bytecode.RInstanceOf && op != bytecode.RInstanceOfNext {
			continue
		}
		before := s.begin(fb.fb)
		r0, r1 := fb.instanceOfAt(rpc, w)
		eff := s.end(fb.fb, before)
		if r1 || eff != 0 {
			d.asked = append(d.asked, jitPropAsked[jitSnapAnsInstanceOf]{rpc: int32(rpc), eff: eff, ok: r1, v: jitSnapAnsInstanceOf{r0, r1}})
		}
	}
}

// bindInstanceOf puts the answers gatherInstanceOf asked by register pc
// at each PC of s's program lowered of it whose op asks InstanceOf: a
// host's, of an instanceof.
func bindInstanceOf(s *jitSnapshot) {
	d := &s.d.inst
	for pc, op := range s.ops {
		if jitSnapAsks[op]&(1<<snapInstanceOf) == 0 {
			continue
		}
		var a *jitPropAsked[jitSnapAnsInstanceOf]
		if op == ir.Host && int(s.p.Origin[pc]) < len(s.rc.Code) {
			rpc := int32(s.p.Origin[pc])
			for i := range d.asked {
				if d.asked[i].rpc == rpc {
					a = &d.asked[i]
					break
				}
			}
		}
		if a == nil {
			// None, as the asking at the PC answered.
			s.heard(snapInstanceOf, int64(pc), false, ssa.InstanceOfSite{}, false)
			continue
		}
		a.bound = true
		s.heard(snapInstanceOf, int64(pc), a.ok, a.v.r0, a.v.r1)
		s.a.InstanceOf.reserve(int(s.room[snapInstanceOf]))
		s.a.InstanceOf.put(int64(pc), a.eff, a.ok, a.v)
	}
}
