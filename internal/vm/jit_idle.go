//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import "github.com/go-quickjs/go-quickjs/internal/jit"

// jitReturned is jitReturnedToHost: what the JIT does as r's turn ends
// with no frame running. The process's idle workspaces go
// (jit.DropIdleWorkspaces), and the cache looks to flush (flush): an idle
// runtime, which takes no back edge's check nor compiles, had kept all
// its code and what that holds until it ran again. The look is the
// flush's own, which drops only what has gone unused for the flush's age
// of program time, so most of a host's short calls pay a clock's read.
func jitReturned(r *Runtime) {
	s := r.jit
	if s == nil {
		return
	}
	jit.DropIdleWorkspaces()
	if !s.unavailable {
		s.flush()
	}
}
