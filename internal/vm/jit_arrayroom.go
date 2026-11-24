//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
)

// jitLiteralArrayRoom is the capacity pool's next arrays are made with, the
// literal's n elements at least. A pool with a large hint holds one array: a
// batch would retain unused buffers and delay observing a changed length.
// A large hint is the interpreter's confirmed room, or two of the pool's
// own samples in a row that agree, as the interpreter's site's do
// (jitFillLiteral); native code shrinks it. It is given its sample's
// length before the sample goes: past the limit, or short, the room falls
// back to what a small literal learns.
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
		if src == nil {
			continue
		}
		if src.room >= arrayRoomLeast && pool.Count == 0 {
			jitLiteralArrayRoom[struct{}](pool, src.n)
		}
		if src.room >= arrayRoomLeast {
			keep = append(keep, w)
		} else {
			// Listed again once it learns a large room anew.
			src.listed = false
		}
	}
	clear(s.arrayRooms[len(keep):])
	s.arrayRooms = keep
}
