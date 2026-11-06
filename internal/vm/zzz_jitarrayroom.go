//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"weak"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
)

// jitLiteralArraySource is a pool's source for the literal at pc of cl's
// function, with the capacity the tree tier confirmed at a turn's end before
// the literal reached native code. Only that: a hint taken within a turn may
// be an array a callback is still growing, and left several callbacks
// 7 to 18% slower (docs/performance-plan.md, F3). The
// pc, unlike the tree node's ordinal, also names a literal in an inlined
// callee. The pool is kept weakly, so that this list keeps neither it nor
// code that has been thrown away.
func jitLiteralArraySource[_ any](r *Runtime, cl *closure, pool *abi.ObjectPool, pc, n int) *jitPoolSource {
	s := &jitPoolSource{array: true, n: n}
	for _, a := range cl.arrays {
		if a != nil && a.acrossTurns && a.pc == int32(pc) && a.room >= arrayRoomLeast && a.room <= arrayRoomLimit {
			s.room = a.room
			if r.jit != nil {
				r.jit.arrayRooms = append(r.jit.arrayRooms, weak.Make(pool))
			}
			break
		}
	}
	return s
}

// jitLiteralArrayRoom is the capacity pool's next arrays are made with, the
// literal's n elements at least. A pool with a large hint holds one array: a
// batch would retain unused buffers and delay observing a changed length.
// Native code may shrink a hint, but only the tree tier's two samples may
// establish a large one. It is given its sample's length before the sample
// goes: past the limit, or short, the room falls back to what a small literal
// learns.
func jitLiteralArrayRoom[_ any](pool *abi.ObjectPool, n int) int {
	src := (*jitPoolSource)(pool.Source)
	if src == nil {
		return n
	}
	if src.room >= arrayRoomLeast {
		if a := src.sample; a != nil {
			length := len(a.elemsOf())
			if length > arrayRoomLimit {
				length = jitLiteralRoom
			}
			src.room = min(src.room, length)
			if src.room < arrayRoomLeast {
				src.room = min(src.room, jitLiteralRoom)
			}
		}
		// Do not retain the previous result's graph through a collection
		// triggered by allocating its replacement's element buffer.
		src.sample = nil
		if src.room >= arrayRoomLeast {
			pool.Size = 1
		}
	}
	return max(n, src.room)
}

// jitLiteralArraysLetGo is the turn's end for pools with a large hint: a
// one-array pool's sample is its completed result, observed and released here
// so that the hint cannot retain a page between requests. An unused pool
// still owns its empty array and has no result to observe.
func jitLiteralArraysLetGo[_ any](r *Runtime) {
	s := r.jit
	if s == nil || len(s.arrayRooms) == 0 || r.frameDepth != 0 {
		return
	}
	jitLiteralArraySweep[struct{}](s)
}

// jitLiteralArraySweep drops the pools that are gone or no longer large from
// s.arrayRooms, and has each that has given out its array observe it.
func jitLiteralArraySweep[_ any](s *jitState) {
	keep := s.arrayRooms[:0]
	for _, w := range s.arrayRooms {
		pool := w.Value()
		if pool == nil {
			continue
		}
		src := (*jitPoolSource)(pool.Source)
		if src == nil || src.room < arrayRoomLeast {
			continue
		}
		if pool.Count == 0 {
			jitLiteralArrayRoom[struct{}](pool, src.n)
		}
		if src.room >= arrayRoomLeast {
			keep = append(keep, w)
		}
	}
	clear(s.arrayRooms[len(keep):])
	s.arrayRooms = keep
}
