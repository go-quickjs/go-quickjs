//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "sync/atomic"

// The tree tier's marks for the JIT (ztree_zzzzseen.go), made where the
// runtime's JIT is on.

func init() {
	treeStringAdd = func(c *tctx, pc int) {
		if !c.r.jitEnabled {
			return
		}
		fn := c.cl.fn
		t := (*tree)(atomic.LoadPointer(&fn.VMCode))
		if t == nil || t == noTree || pc < 0 || pc >= len(fn.Code) {
			return
		}
		s := t.seen.Load()
		if s == nil {
			s = &treeSeen{adds: make([]atomic.Uint32, (len(fn.Code)+31)/32)}
			if !t.seen.CompareAndSwap(nil, s) {
				s = t.seen.Load()
			}
		}
		w, bit := &s.adds[pc>>5], uint32(1)<<(pc&31)
		if w.Load()&bit == 0 {
			w.Or(bit)
			s.n.Add(1)
		}
	}
}
