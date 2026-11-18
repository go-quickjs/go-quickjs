//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"sync"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// jitCompileLog is the closures each runtime compiled since
// recordJITCompilesForTest, by function.
var jitCompileLog struct {
	sync.Mutex
	seen map[*Runtime]map[*bytecode.Function]*closure
}

// recordJITCompilesForTest has every runtime note the closures whose
// register code it compiles (jitCompileNote), until t ends.
func recordJITCompilesForTest(t *testing.T) {
	jitCompileLog.Lock()
	jitCompileLog.seen = map[*Runtime]map[*bytecode.Function]*closure{}
	jitCompileLog.Unlock()
	jitCompileNote = func(r *Runtime, cl *closure) {
		jitCompileLog.Lock()
		defer jitCompileLog.Unlock()
		m := jitCompileLog.seen[r]
		if m == nil {
			m = map[*bytecode.Function]*closure{}
			jitCompileLog.seen[r] = m
		}
		m[cl.fn] = cl
	}
	t.Cleanup(func() {
		jitCompileNote = nil
		jitCompileLog.Lock()
		jitCompileLog.seen = nil
		jitCompileLog.Unlock()
	})
}
