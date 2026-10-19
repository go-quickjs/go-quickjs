//go:build !quickjs_jit || android || ios || !((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

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

func jitNoteSeen(seen jitCacheSeen, _, _ propCache) jitCacheSeen { return seen }

func jitNoteDeepAdd(*propCache, *shape, *shape, *Object) {}

func (*Runtime) jitNoteDeepRead(*propCache, *Object, Atom) {}
func (*Runtime) jitNoteChainRead(*Object, Atom)            {}

func (*Runtime) recordJITStringIntrinsic(*Object) {}
func (*Runtime) recordJITSubstrings()             {}

func (*Runtime) jitOn() bool { return false }

// JITStats is zero: a build without the JIT runs none of it.
func (*Runtime) JITStats() JITStats { return JITStats{} }

// SetJITWarmUp does nothing: there is no JIT to warm up.
func (*Runtime) SetJITWarmUp(int, int) {}

func (*Runtime) initJIT(bool, int64)                                  {}
func (*Runtime) tryJITFrame(*frame) (Value, error, bool)              { return Value{}, nil, false }
func (*Runtime) jitResume(*frame) (Value, error, bool)                { return Value{}, nil, false }
func (*Runtime) jitBackEdge(*frame, uint32, int) (Value, error, bool) { return Value{}, nil, false }
func (*Runtime) tryJITTreeLoop(*tctx, int, int, bool)                 {}
func (*Runtime) jitTreeRecovery(*frame) bool                          { return false }
func jitTreeResult(any) (Value, error, bool)                          { return Value{}, nil, false }
func (*Runtime) jitCodeBytes() int64                                  { return 0 }
func (*Runtime) releaseJIT()                                          {}
