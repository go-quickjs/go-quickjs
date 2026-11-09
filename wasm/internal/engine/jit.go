package engine

import (
	"maps"
	"sync"
)

// The baseline JIT (wasm/internal/jit) compiles a function to native code
// once it has been called often enough, in a build with the quickjs_jit tag
// on a platform with a backend, for Code that asks for it (EnableJIT). Its
// frames are the interpreter's, so a call may cross between the tiers in
// either direction; jit_on.go enters the code and handles its exits.
//
// WASM_JIT, read when a Code is made, turns it on for tests: "eager"
// compiles each function at its first call, "on" or "1" at the default
// threshold, another number at that many calls, and "off", "0" or nothing
// leaves it to EnableJIT.

// DefaultJITThreshold is the number of calls after which a function is
// compiled, unless EnableJIT is given another.
const DefaultJITThreshold = 10

// EnableJIT has the code's functions compiled to native code once each has
// been called threshold times, at its first call for 0. It reports whether
// this build and platform can run native code; where they cannot, the
// interpreter runs everything.
func (c *Code) EnableJIT(threshold int) bool { return c.enableJIT(threshold) }

// JITAvailable reports whether this build and platform can run native code.
func JITAvailable() bool { return jitAvailable() }

// JITStats are the process's counts of the functions the JIT compiled and
// those it refused, by reason.
type JITStats struct {
	Compiled int
	Refused  map[string]int
}

var jitStats struct {
	mu       sync.Mutex
	compiled int
	refused  map[string]int
}

// ReadJITStats returns the JIT's counts so far.
func ReadJITStats() JITStats {
	jitStats.mu.Lock()
	defer jitStats.mu.Unlock()
	return JITStats{Compiled: jitStats.compiled, Refused: maps.Clone(jitStats.refused)}
}

func countCompiled() {
	jitStats.mu.Lock()
	jitStats.compiled++
	jitStats.mu.Unlock()
}

func countRefused(reason string) {
	jitStats.mu.Lock()
	if jitStats.refused == nil {
		jitStats.refused = map[string]int{}
	}
	jitStats.refused[reason]++
	jitStats.mu.Unlock()
}
