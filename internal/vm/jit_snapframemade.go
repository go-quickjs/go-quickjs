//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// FrameMade's answers in a snapshot, asked by register pc on the
// runtime's goroutine (gatherFrameMade) and put at the program's PCs
// lowered of each (bindFrameMade): an answer makes its site's pool where
// the function's learning has none (jitRegFeedback's frameMadeAt), which
// the runtime's goroutine does, and is asked of the register code, not
// the program, so that a snapshot gathered before its function is lowered
// has it.

// jitFrameMadeData is a snapshot's FrameMade answers, by register pc.
type jitFrameMadeData struct {
	asked []jitPropAsked[jitSnapAnsFrameMade]
}

func init() {
	jitSnapDerived |= 1 << snapFrameMade
	jitSnapAskedAtGather |= 1 << snapFrameMade
	jitSnapFuncGatherers = append(jitSnapFuncGatherers, gatherFrameMade)
	jitSnapBinders = append(jitSnapBinders, bindFrameMade)
}

// gatherFrameMade asks fb FrameMade's answer at each register pc of a rest
// parameter's array or an arguments object, for s, as the asking at its
// PCs does: the effects taken back, kept with the answer.
func gatherFrameMade(s *jitSnapshot, fb *jitRegFeedback) {
	d := &s.d.made
	*d = jitFrameMadeData{}
	for rpc, w := range fb.rc.Code {
		if op := w.Op(); op != bytecode.RRestParam && op != bytecode.RArguments {
			continue
		}
		before := s.begin(fb.fb)
		r0, r1 := fb.frameMadeAt(rpc, w)
		eff := s.end(fb.fb, before)
		if r1 || eff != 0 {
			d.asked = append(d.asked, jitPropAsked[jitSnapAnsFrameMade]{rpc: int32(rpc), eff: eff, ok: r1, v: jitSnapAnsFrameMade{r0: r0, r1: r1}})
		}
	}
}

// bindFrameMade puts the answers gatherFrameMade asked by register pc at
// each PC of s's program lowered of it whose op asks FrameMade: a host's.
func bindFrameMade(s *jitSnapshot) {
	d := &s.d.made
	for pc, op := range s.ops {
		if jitSnapAsks[op]&(1<<snapFrameMade) == 0 {
			continue
		}
		var a *jitPropAsked[jitSnapAnsFrameMade]
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
			s.heard(snapFrameMade, int64(pc), false, ssa.FrameMadeSite{}, false)
			continue
		}
		a.bound = true
		s.heard(snapFrameMade, int64(pc), a.ok, a.v.r0, a.v.r1)
		s.a.FrameMade.reserve(int(s.room[snapFrameMade]))
		s.a.FrameMade.put(int64(pc), a.eff, a.ok, a.v)
	}
}
