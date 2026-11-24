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
// address space, and where the heavy tests that run in parallel there,
// TestStrAppendRandom's seeds and TestStrAppend, take up to 0.9 GB each
// beside the rest (the two largest run alone there, heavyAlone).
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

// heavyAlone has t, a test whose blocks on a 32-bit platform take much of
// the 4 GB of address space a process has there, run in parallel as a
// heavy test on a 64-bit platform, and on a 32-bit one among the package's
// serial tests, beside which no parallel test runs. TestBigIntStringLimit's
// strings, 2.4 GB there, and TestTypedArrayFromEndlessIterable's slices,
// each a block larger than the last, beside the parallel tests -- each
// other kept apart by heavy -- had reached 3.6 to 4.1 GB in use, and the
// package ran out of memory in one run of three. A test calls it in place
// of t.Parallel, and nothing of it may assume it runs in parallel.
func heavyAlone(t *testing.T) {
	t.Helper()
	if strconv.IntSize == 32 {
		return
	}
	t.Parallel()
	heavy(t)
}
