//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import "github.com/go-quickjs/go-quickjs/internal/jit"

// jitReturned is jitReturnedToHost: what the JIT does as r's turn ends
// with no frame running. The process's idle workspaces go
// (jit.DropIdleWorkspaces).
func jitReturned(r *Runtime) {
	if r.jit == nil {
		return
	}
	jit.DropIdleWorkspaces()
}
