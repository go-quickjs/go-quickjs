//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"unsafe"
	"weak"

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
// program's time apart, and so does it the entries with no code of a realm
// none of whose entries has been used (jitFlushRealms), which it keeps for a
// realm that runs. It may be a realm that lives and has only gone quiet: its
// functions warm up again if it runs again.

// jitFlushRealms is the end of a look of the cache's: it drops the entries
// with no code of the realms none of whose entries are in used, the entries
// used since the last look, or have been invoked since, which is what moves
// for an entry warming, and lets go of what the JIT keeps of a realm rather
// than for an entry (jitLetRealmsGo). The current realm runs, and the entries
// of one that runs but compiled nothing, of functions the JIT refuses, may be
// dropped, and refused again, only while another runs.
func jitFlushRealms[_ any](s *jitState, used map[*jitEntry]bool) {
	cur := s.rt.jitRealmKey()
	live := map[weak.Pointer[Realm]]bool{cur: true}
	for key, e := range s.cache {
		if used[e] || e.invoked != e.flushInvoked {
			live[key.realm] = true
		}
		e.flushInvoked = e.invoked
		if key.realm != cur {
			s.manyRealms = true
		}
	}
	for key, e := range s.cache {
		if live[key.realm] || e.holdsCode() || e.used+jitCodeAge > s.epoch() || s.inUse(e) || e.busy != 0 {
			continue
		}
		if s.dropEntry(key, e) {
			if l := s.lowered[key.Pointer]; l != nil {
				if k := s.links[key]; k == nil || k.owner == nil {
					s.dropLowering(key.Pointer, l)
				}
			}
		}
	}
	if s.manyRealms || s.ssaShared.lexNames != unsafe.Pointer(&s.rt.lexNames) {
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
