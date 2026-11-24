package vm

import (
	"fmt"
	"os"
	"strconv"
	"testing"
)

// heavySlots bounds how many of the package's memory-heavy parallel tests
// run at once: those whose peak is in the gigabytes -- TestStrAppendRandom's
// seeds, whose strings grow to 2.9 GB in a runtime, TestBigIntStringLimit,
// TestStrAppend. Run as many at once as -parallel let, the package's
// process under the JIT's stress peaked at 16 GB on a machine of 32 CPUs,
// and on a runner of 4 CPUs and 16 GB, beside the root package's tests, ran
// out of memory. Each holds a slot (heavy); the other parallel tests run
// beside them, as many at once as -parallel lets. The serial tests of gigabytes -- the JIT's long code,
// TestJITCompileLinear, TestJITSameWorkWhileMarking -- run alone already.
var heavySlots = make(chan struct{}, heavyLimit())

// heavyLimit is how many heavy tests run at once: QUICKJS_VM_HEAVY's
// number, or 2 -- or 1 on a 32-bit platform, whose process has 4 GB of
// address space, where TestTypedArrayFromEndlessIterable and
// TestBigIntStringLimit each peak at 2.4 GB, and two of them at once ran
// the package out of memory.
func heavyLimit() int {
	var n int
	if _, err := fmt.Sscanf(os.Getenv("QUICKJS_VM_HEAVY"), "%d", &n); err == nil && n > 0 {
		return n
	}
	if strconv.IntSize == 32 {
		return 1
	}
	return 2
}

// heavy has t, a memory-heavy test, wait for a slot of heavySlots and hold
// it until t and its cleanups are done. A test calls it after t.Parallel
// and holds the slot for its own work: its subtests, if any, run in its
// body, not in parallel, so that none waits for a -parallel slot that a
// test waiting for a heavy slot holds. It runs while no other test holds
// the collector off (collector), whose holds wait for it in turn.
func heavy(t *testing.T) {
	t.Helper()
	heavySlots <- struct{}{}
	t.Cleanup(func() { <-heavySlots })
	t.Cleanup(collector.enterHeavy(t.Name()))
}
