//go:build !quickjs_jit || android || ios || !((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/regexp"
)

// jitBuilt is false: the hooks it guards are compiled out, so the
// interpreter's code is what it is without the JIT.
const jitBuilt = false

type jitFields struct{}

// jitTestConfig is testConfig's for the JIT: nothing, without one.
type jitTestConfig struct{}
type jitCodeFields struct{}
type jitRealmFields struct{}

// The memory meter names these types; builds without the JIT have none.
type jitState struct{}
type jitEntry struct{}
type jitRegEntry struct{}

// jitCacheSeen is nothing: a cache keeps no shapes for a JIT there is not
// (propCache.seen).
type jitCacheSeen struct{}

func jitNoteSeen(p *propPoly, _, _ propCache) *propPoly { return p }

func (*propCache) jitNotePrimWrite() {}

func jitNoteDeepAdd(*propCache, *shape, *shape, *Object) {}

func (*Runtime) jitNoteDeepRead(*propCache, *Object, Atom) {}
func (*Runtime) jitNoteChainRead(*Object, Atom)            {}

func (*Runtime) recordJITStringIntrinsic(*Object) {}
func (*Runtime) recordJITSubstrings()             {}

func (*Runtime) jitOn() bool { return false }

// regBigOf marks nothing: no JIT reads the marks. The slow paths call it
// only where jitOn, which is false here, as they call the rest of these
// JIT's hooks: none is ever called.
func (*Runtime) regBigOf(*regCode, int, Value, Value, Value) {}

func (*Runtime) regColdWrapJIT(*closure, *regCode, *regKept, bytecode.RInstr, *Object, *funcData, bytecode.Op, Value, Value) {
}

func (st *regState) addMark(pc int, a, b Value, big bool) (Value, error) { return st.r.add(a, b) }
func (*regState) stringMark(int, Value)                                  {}
func (st *regState) addImmMark(pc int, op bytecode.Op, a Value, imm int32) (Value, error) {
	return st.r.regBinImm(op, a, imm)
}
func (st *regState) addPropMark(pc int, w bytecode.RInstr, obj, b Value) error {
	return st.r.regAddProp(st.cl, st.c, w, obj, b)
}

// newRegCallsCounted is newRegCalls: there are no counts to make.
func (r *Runtime) newRegCallsCounted(cc *closureCode, n int) []regCallCache {
	return r.newRegCalls(cc, n)
}

// regGenCaught catches nothing: there is no native code to resume.
func (*Runtime) regGenCaught(*frame, *regCode) int { return 0 }

// regBackEdgeJIT is the jump: no JIT samples it.
func (*Runtime) regBackEdgeJIT(_ *regState, target int) int { return target }

// nativeGrowUncount takes nothing back: no feedback dispatcher counts.
func nativeGrowUncount(*nativeCtx) {}

// Typed code has no native code (jit_typed.go).
func (*Runtime) jitTypedLinked(*closure, *regCode) {}

// The register tier's hooks (jit_tier.go), which jitBuilt's false
// compiles out.
func (*Runtime) jitRegResumed(*closure, *regCode) {}
func (*Runtime) jitRegHas(*closure) bool          { return false }
func (*Runtime) jitRegEnter(*frame, *closure, *regCode, int, int) (Value, error) {
	return Undefined, nil
}
func (*Runtime) jitRegLoop(st *regState, target int) int { return target }
func (*Runtime) jitRegCountsMake(*closureCode, int)      {}
func (*closureCode) jitRegUncount(int)                   {}

func regGenNative(*bytecode.Function, *regCode) bool { return false }

func (*Runtime) jitRegExpNative(*regexp.Regexp) {}

// JITStats is zero: a build without the JIT runs none of it.
func (*Runtime) JITStats() JITStats { return JITStats{} }

// SetJITWarmUp does nothing: there is no JIT to warm up.
func (*Runtime) SetJITWarmUp(int, int) {}

// SetJITBackground does nothing: there is no JIT to compile in the
// background.
func (*Runtime) SetJITBackground(bool) {}

// JITBackground is false: there is no JIT to compile in the background.
func (*Runtime) JITBackground() bool { return false }

func (*Runtime) initJIT(bool, int64) {}
func (*Runtime) releaseJIT()         {}

// frameAt is the frame at a depth, counting from the bottom: no native
// code puts one off (jit_lazyframes.go's has them made first).
func (r *Runtime) frameAt(i int) *frame {
	u := uint(i)
	return &r.frames[u/frameBlockSize][u%frameBlockSize]
}

// regTickRet counts nothing: there is no budget.
func regTickRet(*closureCode) {}

// regEdgeTick counts nothing: there is no budget.
func regEdgeTick(*regState, int32) {}

// regEdgesOff counts nothing: there is no budget.
func regEdgesOff(*closure) {}
