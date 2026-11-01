//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"sync/atomic"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// PureBody is ssa.PureBodyFeedback's: fb's function is a pure body the call
// path evaluates without its tree (pureCallAt), so that the tree tier has
// marked none of its sites and can mark none.
//
// Not every pure body is one. planTreeCall gives a body its tree when
// pureByTree says so -- one of pureTreeCode instructions or more that calls
// nothing -- and a closure whose pureCall gave up pureMissLimit times runs
// its tree from then on. Either way its sites are marked as any other
// function's are, and those marks are its feedback: native code must learn
// them in their order, not guess them from an operand already unboxed,
// which, in a loop compiled before its sites were marked, left a sum boxed
// on every iteration (TestJITBigWords under QJS_JIT_STRESS). So a body that
// pureByTree gives its tree is none, and neither is one whose tree has
// marked a site.
func (fb *jitFeedback) PureBody() bool {
	fn := fb.fn
	if fn == nil || fn.Leaf != bytecode.LeafPure || pureByTree(fn) {
		return false
	}
	t := (*tree)(atomic.LoadPointer(&fn.VMCode))
	return t == nil || t == noTree || t.bigSeen.Load() == nil
}
