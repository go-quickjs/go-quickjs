package engine

import (
	"maps"
	"sync"
	"sync/atomic"
	"time"
)

// The baseline JIT (wasm/internal/jit) compiles a function to native code,
// in a build with the quickjs_jit tag on a platform with a backend, for
// Code that asks for it (EnableJIT): by default at its first call, with the
// frame validation bounded (module.FrameBound), so that a function native
// code runs has no body compiled for the interpreter; or, with a threshold,
// once it has been called that many times, running in the interpreter
// until then. Its frames are the interpreter's, so a call may cross between
// the tiers in either direction; jit_on.go enters the code and handles its
// exits. The interpreter compiles a function's body when a call first runs
// there: one the JIT refuses, one called before the threshold, or a call
// entering its loops' native code (below).
//
// A call running in the interpreter -- of a function called too few times
// to be compiled -- counts the back edges of its loops; past the Code's
// OSR threshold the function is compiled and the call goes on in its native
// code, entered at the head of the loop it is in (on-stack replacement),
// which runs it to its return.
//
// WASM_JIT, read when a Code is made, turns it on for tests: "on" or "1"
// compiles each function at its first call (DefaultJITThreshold), as
// "eager" does, another number at that many calls, and "off", "0" or
// nothing leaves it to EnableJIT. Under "eager" the first call of a
// function with loops runs in the interpreter and enters the native code at
// its first back edge, so that loops' entries are tried; "osr" does so for
// every call Go or the interpreter makes (EnterLoopsForTest). WASM_JIT_OSR
// sets the OSR threshold: a number of back edges, or "off".
//
// The optimizing tier (wasm/internal/jit/opt) recompiles a function whose
// baseline code was called often enough (DefaultOptThreshold), where it is
// built; SetOptThreshold or WASM_JIT_OPT sets the count ("on" for the
// default, a number of calls, "eager" for every function at its first
// compile, "off" to turn the tier off); WASM_JIT=opt is "eager" with every
// function compiled at its first call. The recompiles run on the process's
// workers, the function's baseline code running until its optimized code
// is published (jit_bg.go), or on the spot (SetOptBackground,
// WASM_JIT_OPT_BACKGROUND).
//
// WASM_JIT_CODE, read when the engine starts, names a file each code placed
// is written to, with the compilers' marks, for a profile's native samples
// (wasm/internal/jit/opt/README.md, Validation).

// DefaultJITThreshold is the number of calls a function runs in the
// interpreter before it is compiled, unless EnableJIT is given another:
// none, each function is compiled at its first call.
const DefaultJITThreshold = 0

// DefaultOptThreshold is the number of calls of a function's baseline code
// after which the optimizing tier compiles it, unless SetOptThreshold gives
// another. A poll the baseline code exits for -- every 4,096 loop back
// edges and calls -- counts for 250 calls, and a call or an instruction Go
// makes for it for one, so that a function whose loops run long is
// compiled too, for its later calls. The count weighs the
// tier's compile against what it saves (wasm/internal/jit/opt/README.md,
// Tier-up).
const DefaultOptThreshold = 30000

// SetOptThreshold sets the number of calls of a function's baseline code
// after which the optimizing tier compiles it in the baseline's place: 0
// has the optimizing tier compile each function it accepts at its first
// compile, and the baseline only those it refuses; a negative n turns the
// tier off. By default it is DefaultOptThreshold, or what WASM_JIT_OPT says.
// It applies once EnableJIT has turned the JIT on, to the functions
// compiled after it. The optimizing tier is built wherever the JIT is.
func (c *Code) SetOptThreshold(n int) { c.setOptThreshold(n) }

// SetOptBackground has the optimizing tier compile the code's functions
// whose baseline code counted enough calls on the process's background
// workers (on), the program running their baseline code until their
// optimized code is published, or on the goroutine that ran the count out
// (off), which then waits for it, so that the tiering is the same on every
// run. By default the workers compile where GOMAXPROCS is above 1, and
// the goroutine itself where it is 1; WASM_JIT_OPT_BACKGROUND ("on",
// "off") sets the mode for tests and benchmarks, and WASM_JIT's "eager",
// "osr" and "opt" turn it off. It applies once EnableJIT has turned the JIT
// on.
func (c *Code) SetOptBackground(on bool) { c.setOptBackground(on) }

// DefaultOSRThreshold is the number of loop back edges after which a call
// running in the interpreter goes on in its function's native code, unless
// SetOSRThreshold gives another. Below it a loop's interpreted iterations
// cost less than compiling the function would.
const DefaultOSRThreshold = 1000

// EnableJIT has the code's functions compiled to native code once each has
// been called threshold times, running in the interpreter until then; for 0
// each is compiled at its first call, without the interpreter's compile of
// its body unless a call runs there, and a negative threshold is
// DefaultJITThreshold. It reports whether this build and platform can run
// native code; where they cannot, the interpreter runs everything.
func (c *Code) EnableJIT(threshold int) bool { return c.enableJIT(threshold) }

// JITAvailable reports whether this build and platform can run native code.
func JITAvailable() bool { return jitAvailable() }

// SetOSRThreshold sets the number of loop back edges after which a call of
// the code's running in the interpreter goes on in native code, compiling
// the function if it is not: at the first back edge for 0, never for a
// negative n. It applies once EnableJIT has turned the JIT on, to the
// calls that begin after it, and to the loops of functions first called
// after EnableJIT.
func (c *Code) SetOSRThreshold(n int) { c.setOSRThreshold(n) }

// EnterLoopsForTest has every call of a function with loops that Go or the
// interpreter makes run in the interpreter until the function's first back
// edge, and go on in its native code from there, so that a test runs each
// loop's entry on every call; native code still calls native code. It
// applies once EnableJIT has turned the JIT on.
func (c *Code) EnterLoopsForTest() { c.enterLoopsForTest() }

// JITStats are the process's counts of the functions the JIT compiled and
// those it refused, by reason, and of the calls that went on from a loop in
// native code (OSR).
type JITStats struct {
	Compiled int
	Refused  map[string]int
	OSR      int
	// Optimized and OptRefused are the optimizing tier's: the functions
	// it compiled, and those it refused, which kept their baseline code.
	Optimized  int
	OptRefused map[string]int
	// BaselineTime and OptTime are the time each tier spent compiling,
	// refusals included, and PlaceTime the time placing their code.
	BaselineTime, OptTime, PlaceTime time.Duration
	// Interpreted is the number of function bodies the interpreter
	// compiled, and InterpTime the time it took.
	Interpreted int
	InterpTime  time.Duration
}

var jitStats struct {
	mu                sync.Mutex
	compiled          int
	refused           map[string]int
	osr               atomic.Int64
	optimized         int
	optRefused        map[string]int
	baseTime, optTime atomic.Int64
	placeTime         atomic.Int64
	interpreted       atomic.Int64
	interpTime        atomic.Int64
}

// ReadJITStats returns the JIT's counts so far.
func ReadJITStats() JITStats {
	jitStats.mu.Lock()
	defer jitStats.mu.Unlock()
	return JITStats{Compiled: jitStats.compiled, Refused: maps.Clone(jitStats.refused), OSR: int(jitStats.osr.Load()),
		Optimized: jitStats.optimized, OptRefused: maps.Clone(jitStats.optRefused),
		BaselineTime: time.Duration(jitStats.baseTime.Load()), OptTime: time.Duration(jitStats.optTime.Load()),
		PlaceTime:   time.Duration(jitStats.placeTime.Load()),
		Interpreted: int(jitStats.interpreted.Load()), InterpTime: time.Duration(jitStats.interpTime.Load())}
}

func countOptimized() {
	jitStats.mu.Lock()
	jitStats.optimized++
	jitStats.mu.Unlock()
}

func countOptRefused(reason string) {
	jitStats.mu.Lock()
	if jitStats.optRefused == nil {
		jitStats.optRefused = map[string]int{}
	}
	jitStats.optRefused[reason]++
	jitStats.mu.Unlock()
}

func countTime(total *atomic.Int64, start time.Time) { total.Add(int64(time.Since(start))) }

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
