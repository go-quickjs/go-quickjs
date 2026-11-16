//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import ()

// jitLazyLevels is a call of Go from a native callee whose levels below
// the calling one have no frames yet (jitRegGoCallNested): those of contexts
// root+1 to idx-1, at depth on, the calling level's frame past them. They
// are made when a frame among them is read -- frameAt, which a stack trace,
// a function's caller, a generator's search for its own run read through
// -- from the contexts, which hold where each level is while the call
// runs: what it runs natively runs in contexts past idx. made says they
// have been.
//
// The levels' code is pinned by none of it (the calling level's is): while
// any call has put frames off, no code is closed (closeSSA,
// jitCloseRetired), so that what the levels return to stays.
type jitLazyLevels struct {
	root, idx, depth int
	made             bool
	// realm is the realm the call was made in, which the levels are made
	// in (jitRegMakeLazy), from from on in regLazyLevels.
	realm *Realm
	from  int
}

// jitFramesDue makes the frames calls of Go put off (jitLazyLevels), if
// frame i is one of them.
func (r *Runtime) jitFramesDue(i int) {
	if s := r.jit; s != nil && i < s.lazyTop {
		r.jitMakeLazyFrames()
	}
}

// jitMakeLazyFrames makes the frames every call of Go under way put off,
// as jitRegGoCallNested would have made them, where it left room for them
// (jitRegMakeLazy).
func (r *Runtime) jitMakeLazyFrames() {
	s := r.jit
	// Cleared, and each set marked made, before any frame is filled: a
	// frame read while they are being filled would be handed back unfilled.
	// None is: the filling pushes frames, which reads none, and reads the
	// contexts, the stack and an accessor's property, none of which reads a
	// frame or meters memory. A change that has it read one -- a stack
	// trace, the memory meter's walk -- must fill them first.
	s.lazyTop = 0
	s.lazyMade++
	saved := r.frameDepth
	for j := range s.lazy {
		lz := &s.lazy[j]
		if lz.made {
			continue
		}
		lz.made = true
		r.frameDepth = lz.depth
		r.jitRegMakeLazy(lz)
	}
	r.frameDepth = saved
}

// jitLazyDone ends the innermost call of Go's put-off frames, as the call
// returns, keeping nothing of it: the frame and the realm it held, past
// the list's end, had kept a realm a host dropped alive -- a deep
// recursion's, whose calls of Go put their levels off.
func (r *Runtime) jitLazyDone() {
	s := r.jit
	s.lazy[len(s.lazy)-1] = jitLazyLevels{}
	s.lazy = s.lazy[:len(s.lazy)-1]
	s.lazyTop = 0
	if n := len(s.lazy); n != 0 && !s.lazy[n-1].made {
		lz := s.lazy[n-1]
		s.lazyTop = lz.depth + lz.idx - lz.root - 1
	}
}

// jitFrameBlocks has the blocks of frames up to the one depth is in, as
// pushes up to it would have made them: a call of Go leaves room for the
// frames it puts off, past which its calling level's frame is pushed.
func (r *Runtime) jitFrameBlocks(depth int) {
	for uint(depth)/frameBlockSize >= uint(len(r.frames)) {
		r.frames = append(r.frames, make([]frame, frameBlockSize))
	}
}
