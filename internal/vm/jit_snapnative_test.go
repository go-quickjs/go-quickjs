//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"slices"
	"sync/atomic"
)

// The learning's methods' scribbler (jitSnapScribblers): every list of the
// function's learning they read made anew, holding every register pc, and
// the interpreter's marks they read -- the operators' BigInt marks, the
// operands' lattice, the calls' counts -- written over in place, as the
// interpreter writes them.
func init() {
	jitSnapScribblers = append(jitSnapScribblers, func(r *Runtime, s *jitSnapshot) func() {
		fb := s.rf
		cl, n := fb.fb.cl, len(fb.rc.Code)
		all := make([]int32, n)
		for i := range all {
			all[i] = int32(i)
		}
		var restores []func()
		if l := fb.fb.e; l != nil {
			was := *l.jitLearned
			hosts, made, madeOf := l.ssaHostsLeft, l.regCallsMade, l.regCallsMadeOf
			l.failed, l.bigints, l.bigwide, l.notInline = all, all, all, all
			l.typedReads, l.stringReads, l.keyedReads, l.keyedWrites = all, all, all, all
			l.ssaHostsLeft = all
			l.regCallsMade, l.regCallsMadeOf = make([]uint64, (n+63)/64), n
			for i := range l.regCallsMade {
				l.regCallsMade[i] = ^uint64(0)
			}
			restores = append(restores, func() {
				*l.jitLearned = was
				l.ssaHostsLeft, l.regCallsMade, l.regCallsMadeOf = hosts, made, madeOf
			})
		}
		if cl != nil && cl.reg != nil {
			if m := cl.reg.bigSeen.Load(); m != nil {
				for _, words := range [][]atomic.Uint32{m.bigs, m.wide, m.heap} {
					was := make([]uint32, len(words))
					for i := range words {
						was[i] = words[i].Swap(^uint32(0))
					}
					restores = append(restores, func() {
						for i := range words {
							words[i].Store(was[i])
						}
					})
				}
			}
		}
		if cl != nil && cl.closureCode != nil {
			ops, counts := cl.jitRegOps, cl.jitRegCounts
			wasOps, wasCounts := slices.Clone(ops), slices.Clone(counts)
			for i := range ops {
				ops[i] = 0xff
			}
			for i := range counts {
				counts[i] = uint64(i) * 977
			}
			restores = append(restores, func() { copy(ops, wasOps); copy(counts, wasCounts) })
		}
		return func() {
			for i := len(restores) - 1; i >= 0; i-- {
				restores[i]()
			}
		}
	})
}
