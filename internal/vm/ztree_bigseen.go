//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "sync/atomic"

// The tree tier's marks of arithmetic that met BigInts (treeBigSeen), made
// where the runtime's JIT is on. The file sorts before every ztree_ file
// with an init: Go lays the init closures out in descending init order, so
// this one's come after all of theirs, and move none of the hot ones.

func init() {
	treeBigInt = func(c *tctx, pc int, heap bool) {
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
			s = &treeBigSeen{bigs: make([]atomic.Uint32, n), heap: make([]atomic.Uint32, n)}
			if !t.bigSeen.CompareAndSwap(nil, s) {
				s = t.bigSeen.Load()
			}
		}
		words := s.bigs
		if heap {
			words = s.heap
		}
		if w, bit := &words[pc>>5], uint32(1)<<(pc&31); w.Load()&bit == 0 {
			w.Or(bit)
		}
	}
}
