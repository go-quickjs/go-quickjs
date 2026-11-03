//go:build !quickjs_jit || android || ios || !((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "github.com/go-quickjs/go-quickjs/internal/regexp"

// jitBuilt is false: the hooks it guards are compiled out, so the
// interpreter's code is what it is without the JIT.
const jitBuilt = false

type jitFields struct{}
type jitCodeFields struct{}
type jitRealmFields struct{}

// The memory meter names these types; builds without the JIT have none.
type jitState struct{}
type jitEntry struct{}

// jitCacheSeen is nothing: a cache keeps no shapes for a JIT there is not
// (propCache.seen).
type jitCacheSeen struct{}

func jitNoteSeen(p *propPoly, _, _ propCache) *propPoly { return p }

func jitNoteDeepAdd(*propCache, *shape, *shape, *Object) {}

func (*Runtime) jitNoteDeepRead(*propCache, *Object, Atom) {}
func (*Runtime) jitNoteChainRead(*Object, Atom)            {}

func (*Runtime) recordJITStringIntrinsic(*Object) {}
func (*Runtime) recordJITSubstrings()             {}

func (*Runtime) jitOn() bool { return false }

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

func (*Runtime) initJIT(bool, int64)                                  {}
func (*Runtime) tryJITFrame(*frame) (Value, error, bool)              { return Value{}, nil, false }
func (*Runtime) jitCountPure()                                        {}
func (*Runtime) jitCountsCaller(int) bool                             { return false }
func (*Runtime) jitResume(*frame) (Value, error, bool)                { return Value{}, nil, false }
func (*Runtime) jitBackEdge(*frame, uint32, int) (Value, error, bool) { return Value{}, nil, false }
func (*Runtime) tryJITTreeLoop(*tctx, int, int, bool)                 {}
func (*Runtime) jitTreeRecovery(*frame) bool                          { return false }
func jitTreeResult(any) (Value, error, bool)                          { return Value{}, nil, false }
func (*Runtime) jitCodeBytes() int64                                  { return 0 }
func (*Runtime) releaseJIT()                                          {}

// jitFramesDue makes no frames: no native code puts one off.
func (r *Runtime) jitFramesDue(i int) {}

func (*Runtime) jitPureCounted() bool { return false }
func (*Runtime) jitCountPureCall()    {}

// pureCallCounted is pureCall: there is no JIT to count calls for.
func (r *Runtime) pureCallCounted(fd *funcData, this Value, args []Value) (Value, bool) {
	r.pureRefused = false
	return r.pureCallAt(fd, this, args, 0, true)
}
