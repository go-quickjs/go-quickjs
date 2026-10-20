//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "sync/atomic"

// The tree tier's marks of arithmetic that met BigInts (treeBigSeen), made
// where the runtime's JIT is on. The file sorts before every ztree_ file
// with an init: Go lays the init closures out in descending init order, so
// this one's come after all of theirs, and move none of the hot ones. Its
// functions are closures too, which a function would move every one of.

// treeBigMark marks the site at pc of c's function's tree in the marks
// kind picks of its treeBigSeen, made if it has none, where the runtime's
// JIT is on.
var treeBigMark func(c *tctx, pc int, kind func(*treeBigSeen) []atomic.Uint32)

func init() {
	treeBigMark = func(c *tctx, pc int, kind func(*treeBigSeen) []atomic.Uint32) {
		if !c.r.jitEnabled {
			return
		}
		fn := c.cl.fn
		t := (*tree)(atomic.LoadPointer(&fn.VMCode))
		if t == nil || t == noTree || pc < 0 || pc >= len(fn.Code) {
			return
		}
		s := t.bigSeen.Load()
		if s == nil {
			n := (len(fn.Code) + 31) / 32
			s = &treeBigSeen{bigs: make([]atomic.Uint32, n), wide: make([]atomic.Uint32, n), heap: make([]atomic.Uint32, n), recvs: make([]atomic.Uint32, n)}
			if !t.bigSeen.CompareAndSwap(nil, s) {
				s = t.bigSeen.Load()
			}
		}
		if w, bit := &kind(s)[pc>>5], uint32(1)<<(pc&31); w.Load()&bit == 0 {
			w.Or(bit)
		}
	}
	treeBigInt = func(c *tctx, pc int, heap bool) {
		if heap {
			treeBigMark(c, pc, func(s *treeBigSeen) []atomic.Uint32 { return s.heap })
		} else {
			treeBigMark(c, pc, func(s *treeBigSeen) []atomic.Uint32 { return s.bigs })
		}
	}
	treeBigRecv = func(c *tctx, pc int) {
		treeBigMark(c, pc, func(s *treeBigSeen) []atomic.Uint32 { return s.recvs })
	}
	treeBigOf = func(c *tctx, pc int, a, b, v Value) {
		// From 0 to 2^64-1: held in its value, of no sign but zero's.
		unsigned := func(v Value) bool {
			if !v.isShortBig() {
				return false
			}
			neg, mag := v.shortBigParts()
			return !neg || mag == 0
		}
		switch {
		case a.isInt64Big() && b.isInt64Big() && v.isInt64Big():
			treeBigMark(c, pc, func(s *treeBigSeen) []atomic.Uint32 { return s.bigs })
		case unsigned(a) && unsigned(b) && unsigned(v):
			treeBigMark(c, pc, func(s *treeBigSeen) []atomic.Uint32 { return s.wide })
		default:
			treeBigMark(c, pc, func(s *treeBigSeen) []atomic.Uint32 { return s.heap })
		}
	}
}
