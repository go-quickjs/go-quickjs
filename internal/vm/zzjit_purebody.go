//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// PureBody is ssa.PureBodyFeedback's: fb's function is a pure body, which
// the call path evaluates without its tree (pureCall), so that the tree
// tier marks none of its sites.
func (fb *jitFeedback) PureBody() bool {
	return fb.fn != nil && fb.fn.Leaf == bytecode.LeafPure
}
