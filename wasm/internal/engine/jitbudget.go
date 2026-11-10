package engine

import (
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// The JIT's memory.
//
// What the JIT holds for a Code -- its functions' native code, placed in
// the Code's arena, the records that describe each code, and the Code's
// tables native code reads -- is charged to a JITBudget, which several
// Codes may share: a runtime's. A compile reserves room for its code before
// placing it. Where the budget is full, the JIT first drops code no thread
// is in that nothing needs -- code the optimizing tier replaced, and the
// code of functions not called for a while, whose next call compiles them
// again -- and only where that does not make the room refuses the compile:
// the function runs in the interpreter, and its compile is tried again
// later (jit_mem.go has the policy and why dropping is safe).

// JITBudget bounds the memory the JIT holds for the Codes charged to it
// (Code.SetJITBudget). It is safe for concurrent use.
type JITBudget struct {
	limit int64
	held  atomic.Int64

	mu sync.Mutex
	// members are the Codes charged to the budget, whose code a compile of
	// any of them may drop to make room.
	members map[*codeJIT]struct{}
}

// NewJITBudget returns a budget of limit bytes; limit <= 0 is no limit,
// the budget only counting what its Codes hold.
func NewJITBudget(limit int64) *JITBudget {
	return &JITBudget{limit: max(limit, 0), members: map[*codeJIT]struct{}{}}
}

// Limit returns the budget's limit in bytes, 0 for none.
func (b *JITBudget) Limit() int64 { return b.limit }

// Held returns what the budget's Codes hold.
func (b *JITBudget) Held() int64 { return b.held.Load() }

// reserve counts n more bytes, reporting false, and counting nothing, if
// they would take the budget past its limit.
func (b *JITBudget) reserve(n int64) bool {
	for {
		h := b.held.Load()
		if b.limit > 0 && h+n > b.limit {
			return false
		}
		if b.held.CompareAndSwap(h, h+n) {
			return true
		}
	}
}

// charge counts n more bytes, past the limit if it must: what running code
// needs, which cannot be refused.
func (b *JITBudget) charge(n int64) { b.held.Add(n) }

// release counts n bytes fewer.
func (b *JITBudget) release(n int64) { b.held.Add(-n) }

// fits reports whether n more bytes fit now.
func (b *JITBudget) fits(n int64) bool { return b.limit <= 0 || b.held.Load()+n <= b.limit }

// JITMemory is what the JIT holds for a Code.
type JITMemory struct {
	// Code is the bytes of native code placed in the Code's arena: each
	// code's instructions rounded up to a cache line, the optimizing tier's,
	// the baseline's it replaced that are still placed and the baseline's
	// a loop's entry needed included. Records is what describes each code
	// -- its entries, its loops' entries and catch clauses -- and the code
	// that runs a SIMD instruction native code leaves to Go, and Tables
	// the Code's link table, frames table and counters, which do not shrink.
	Code, Records, Tables int64
	// Mapped is the memory the Code's arena has mapped, which holds the
	// code and the space dropped code left between other code.
	Mapped int64
	// Functions is the number of functions with native code, and Dropped
	// how many times a function's code was dropped to make room.
	Functions, Dropped int
}

// Held returns the bytes the JIT holds for the Code, what is charged to
// its budget.
func (m JITMemory) Held() int64 { return m.Code + m.Records + m.Tables }

// jitEpochTime is how long an epoch of the JIT's clock (jitEpoch) lasts,
// and jitCodeAge how many epochs a function's code goes uncalled before it
// may be dropped to make room: 100 ms or more.
const (
	jitEpochTime = 50 * time.Millisecond
	jitCodeAge   = 2
)

// jitClockStart is when the JIT's clock started.
var jitClockStart = time.Now()

// jitEpochForTest, where a test sets it, stands in for the JIT's clock.
var jitEpochForTest func() uint64

// jitClockSteps is the JIT's clock where WASM_JIT_CLOCK, read when the
// engine starts, is "step", for tests: an epoch passes at each reading, so
// that every compile that finds its budget full makes a pass that may drop
// code, and every function not called since the last pass is old.
var (
	jitClockStep  = os.Getenv("WASM_JIT_CLOCK") == "step"
	jitClockSteps atomic.Uint64
)

// jitEpoch is the JIT's clock, which ages code: the epochs of jitEpochTime
// since the engine started, from 1.
func jitEpoch() uint64 {
	if jitEpochForTest != nil {
		return jitEpochForTest()
	}
	if jitClockStep {
		return jitClockSteps.Add(1)
	}
	return uint64(time.Since(jitClockStart)/jitEpochTime) + 1
}

// jitMemEnv is WASM_JIT_MEMLIMIT, read when the engine starts, for tests:
// a limit in bytes each Code the JIT compiles for is given a budget of its
// own of, whatever budget it is given; -1 is none.
var jitMemEnv = func() int64 {
	v := os.Getenv("WASM_JIT_MEMLIMIT")
	if v == "" {
		return -1
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return -1
	}
	return n
}()

// SetJITBudget charges what the JIT holds for the code to b, which other
// Codes may share, moving what it holds already from the budget it had: a
// Code that has not been given one has a budget of its own without a
// limit. It does nothing for a Code without the JIT (EnableJIT), or where
// WASM_JIT_MEMLIMIT gives each Code a limit of its own, for tests.
func (c *Code) SetJITBudget(b *JITBudget) { c.setJITBudget(b) }

// JITMemory returns what the JIT holds for the code.
func (c *Code) JITMemory() JITMemory { return c.jitMemory() }
