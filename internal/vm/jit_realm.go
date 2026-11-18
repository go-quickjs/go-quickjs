//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
)

// What the JIT holds of a realm a host has dropped.
//
// An entry's key holds its realm weakly (jitKey), but its code does not: its
// pools hold the realm's objects and closures, its guards the layouts of its
// prototypes, and an entry warming up the closures it has seen. So a realm
// whose functions have entries in the cache is reachable from the runtime
// however it was let go, its key is never gone, and it has to be found
// another way. The cache's flush (jitState.flush), as V8 flushes code unused
// for long, drops code unused between two of its looks, a second of the
// program's time apart (regFlush). It may be a realm that lives and has only
// gone quiet: its functions warm up again if it runs again.

// jitFlushRealms is the end of a look of the cache's: it lets go of what
// the JIT keeps of a realm rather than for an entry (jitLetRealmsGo) once
// the contexts share another realm's than the current one's.
func jitFlushRealms[_ any](s *jitState) {
	if s.ssaShared.lexNames != unsafe.Pointer(&s.rt.lexNames) {
		jitLetRealmsGo[struct{}](s)
	}
}

// jitLetRealmsGo clears what the JIT keeps of the realm code last ran in
// rather than for an entry: the megamorphic cache, which is keyed by what it
// holds, what the contexts share (jitShareContexts), and all they hold of the
// runs that have ended: a suspended generator's locals, the closure of a level
// past those a run's end cleared, which each entry and native call writes
// again. Only code running natively reads those, as may be below a look at a
// back edge.
func jitLetRealmsGo[_ any](s *jitState) {
	if s.mega != nil {
		clear(s.mega[:])
	}
	if s.ctxTop != 0 {
		return
	}
	s.calleeFrame = frame{}
	s.ssaShared, s.ssaSharedFrom = jitShared{}, 0
	s.ssaSharedGen++
	for _, c := range s.ssaCtxs {
		c.Locals, c.Stack, c.Upvalues, c.Closure, c.Cells, c.FrameThis = nil, nil, nil, nil, nil, nil
		c.RetValue, c.This, c.Instance, c.CharCodeAt = abi.Slot{}, abi.Slot{}, abi.Slot{}, abi.Slot{}
		c.Global, c.GlobalLex, c.LexNames, c.StringMethods = nil, nil, nil, nil
		c.StringProto, c.StringShape, c.CharCodeAtOffset = nil, 0, 0
		clear(c.Keep[:])
		clear(c.RecordRef[:])
		c.RecordHigh = 0
	}
}
