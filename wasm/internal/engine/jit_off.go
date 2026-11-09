//go:build !(quickjs_jit && amd64 && (linux || windows) && !android && !ios)

package engine

// Without the tag, or without a backend, there is no native code: the hooks
// the interpreter calls are constants and empty types, compiled out.

const jitBuilt = false

type codeJIT struct{}

type nativeFn struct{}

type jitExec struct{}

type jitInstance struct{}

func jitAvailable() bool { return false }

func (c *Code) enableJIT(int) bool { return false }

func (c *Code) jitFromEnv() {}

func (x *exec) runNative(*Function, *compiled, []uint64) (int32, bool) { return 0, false }

func (x *exec) jitLevel() int { return 0 }

func (x *exec) setJITLevel(int) {}
