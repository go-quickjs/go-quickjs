//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// ArgumentsRead's answers in a snapshot, asked by register pc on the
// runtime's goroutine (gatherArguments) and put at the program's PCs
// lowered of each (bindArguments): a length's answer names the realm's
// arguments objects' shapes, which the code keeps, and is asked of the
// register code, at the reads its function marked (jitArgsRead).

// jitArgumentsData is a snapshot's ArgumentsRead answers, by register pc.
type jitArgumentsData struct {
	asked []jitPropAsked[jitSnapAnsArgumentsRead]
}

func init() {
	jitSnapDerived |= 1 << snapArgumentsRead
	jitSnapAskedAtGather |= 1 << snapArgumentsRead
	jitSnapFuncGatherers = append(jitSnapFuncGatherers, gatherArguments)
	jitSnapBinders = append(jitSnapBinders, bindArguments)
}

// gatherArguments asks fb ArgumentsRead's answer at each register pc its
// function marked as an arguments object's read, for s, as the asking at
// its PCs does: the effects taken back, kept with the answer.
func gatherArguments(s *jitSnapshot, fb *jitRegFeedback) {
	d := &s.d.args
	*d = jitArgumentsData{}
	l := fb.fb.e
	if l == nil {
		return
	}
	for _, x := range l.typedReads {
		rpc := int(x &^ jitArgsRead)
		if x&jitArgsRead == 0 || x&jitBigRead != 0 || rpc >= len(fb.rc.Code) {
			continue
		}
		before := s.begin(fb.fb)
		r0, r1 := fb.argumentsAt(rpc)
		eff := s.end(fb.fb, before)
		if r1 || eff != 0 {
			d.asked = append(d.asked, jitPropAsked[jitSnapAnsArgumentsRead]{rpc: int32(rpc), eff: eff, ok: r1, v: jitSnapAnsArgumentsRead{r0: r0, r1: r1}})
		}
	}
}

// bindArguments puts the answers gatherArguments asked by register pc at
// each PC of s's program lowered of it whose op asks ArgumentsRead: an
// element read's, of any register instruction but a get_length, and a
// length's, of a get_length.
func bindArguments(s *jitSnapshot) {
	d := &s.d.args
	for pc, op := range s.ops {
		if jitSnapAsks[op]&(1<<snapArgumentsRead) == 0 {
			continue
		}
		var a *jitPropAsked[jitSnapAnsArgumentsRead]
		if rpc := int(s.p.Origin[pc]); rpc < len(s.rc.Code) && (op == ir.ArrayLength) == (s.rc.Code[rpc].Op() == bytecode.RGetLength) {
			for i := range d.asked {
				if d.asked[i].rpc == int32(rpc) {
					a = &d.asked[i]
					break
				}
			}
		}
		if a == nil {
			// None, as the asking at the PC answered.
			s.heard(snapArgumentsRead, int64(pc), false, ssa.ArgumentsSite{}, false)
			continue
		}
		a.bound = true
		s.heard(snapArgumentsRead, int64(pc), a.ok, a.v.r0, a.v.r1)
		s.a.ArgumentsRead.reserve(int(s.room[snapArgumentsRead]))
		s.a.ArgumentsRead.put(int64(pc), a.eff, a.ok, a.v)
	}
}
