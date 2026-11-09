//go:build !(quickjs_jit && !android && !ios && ((amd64 && (linux || windows)) || (arm64 && (darwin || linux))))

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

func (c *Code) setOSRThreshold(int) {}

func (c *Code) setOptThreshold(int) {}

func (c *Code) enterLoopsForTest() {}

func (c *Code) setOSRCalls(int8) {}

func osrBudget(*Code, *compiled) int32 { return 0 }

func (x *exec) osr(*frame, uint32, uint32) (int32, bool) { return 0, false }

func (x *exec) runNative(*Function, *compiled, []uint64, []Ref) (int32, bool) { return 0, false }

func (x *exec) jitLevel() int { return 0 }

func (x *exec) setJITLevel(int) {}
