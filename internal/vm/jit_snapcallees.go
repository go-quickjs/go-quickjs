//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import "github.com/go-quickjs/go-quickjs/internal/jit/ssa"

// jitSnapMayInline reports whether cl's function may be inlined as far
// as its register code's length says (ssa.MayInlineSize): one with none
// yet, made at its first call or as a call of it is asked, may. Every
// inlining of a callee is bounded by its program's size, which is no less
// than its register code's (TestJITLowerRegAnInstructionAPC): a callee a
// snapshot's gather takes that is longer (jit_snapwalk.go) is asked
// nothing, its program lowered alone. Its code is not made here
// (RegCompiled).
func jitSnapMayInline(cl *closure) bool {
	if cl.reg != nil && cl.reg.rc != nil {
		return ssa.MayInlineSize(len(cl.reg.rc.Code))
	}
	if rc := cl.fn.RegCompiled(); rc != nil {
		return ssa.MayInlineSize(len(rc.Code))
	}
	return true
}
