package vm

import (
	"fmt"
	"math"
	"os"
	"runtime/debug"
	"strconv"
	"sync"
	"testing"
	"time"
)

// heavySlots bounds how many of the package's memory-heavy parallel tests
// run at once: those whose peak is in the gigabytes -- TestStrAppendRandom's
// seeds, whose strings grow to 2.9 GB in a runtime, TestBigIntStringLimit,
// TestStrAppend. Run as many at once as -parallel let, the package's
// process under the JIT's stress peaked at 16 GB on a machine of 32 CPUs,
// and on a runner of 4 CPUs and 16 GB, beside the root package's tests, ran
// out of memory. Each holds a slot (heavy), or every slot (heavyOnly); the
// other parallel tests run beside them, as many at once as -parallel lets.
// The serial tests of gigabytes -- the JIT's long code,
// TestJITCompileLinear, TestJITSameWorkWhileMarking -- run alone already.
var heavySlots = newHeavyGate(heavyLimit())

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

// heavyMemoryLimit is the package's process's memory limit on a 64-bit
// platform where GOMEMLIMIT sets none: the most the collector lets the
// heap and the memory it has kept take before it collects, whatever its
// percent, and gives back what it keeps past it.
//
// At the collector's percent of 100 a heap's goal is twice what is live,
// and the memory a peak took is given back at the scavenger's pace, a few
// tens of MB a second. Under the JIT's stress, on 4 CPUs, the serial long
// code tests (TestJITLongCode, TestJITLongCodeCompilesARM64) had 6.2 to
// 7.8 GB live, a goal of 12 to 15.6 GB and 12.2 GB committed, which the
// process kept for the rest of its run; on it the heavy tests' pairs --
// TestTypedArrayFromEndlessIterable's 2 GiB of values, seed 388's strings,
// with another seed -- had up to 8.2 GB live and a goal of 16.5 GB. The
// process's commit peaked at 13.3 to 17.2 GB, and a runner of 16 GB ran
// out of memory in the parallel tests' heavy part. Under the limit it
// collects, and gives back, where those goals passed it; with the largest
// heavy tests apart (heavyOnly) what is live stays well under it, so that
// the collector does not run back to back.
const heavyMemoryLimit = 10 << 30

func init() {
	if strconv.IntSize == 64 && debug.SetMemoryLimit(-1) == math.MaxInt64 {
		debug.SetMemoryLimit(heavyMemoryLimit)
	}
}

// heavyGate is slots for the heavy tests: each holds one, or every one.
type heavyGate struct {
	slots chan struct{}
	// taking is held by the test taking every slot, so that two such
	// tests, each with some of them, do not wait for each other.
	taking sync.Mutex
}

func newHeavyGate(n int) *heavyGate {
	return &heavyGate{slots: make(chan struct{}, n)}
}

// enter waits for a slot, or for every slot if only, and returns their
// leaving.
func (g *heavyGate) enter(only bool) (leave func()) {
	n := 1
	if only {
		n = cap(g.slots)
		g.taking.Lock()
		defer g.taking.Unlock()
	}
	for range n {
		g.slots <- struct{}{}
	}
	return func() {
		for range n {
			<-g.slots
		}
	}
}

// heavy has t, a memory-heavy test, wait for a slot of heavySlots and hold
// it until t and its cleanups are done. A test calls it after t.Parallel
// and holds the slot for its own work: its subtests, if any, run in its
// body, not in parallel, so that none waits for a -parallel slot that a
// test waiting for a heavy slot holds. It runs while no other test holds
// the collector off (collector), whose holds wait for it in turn.
func heavy(t *testing.T) {
	t.Helper()
	t.Cleanup(heavySlots.enter(false))
	t.Cleanup(collector.enterHeavy(t.Name()))
}

// heavyOnly is heavy for a test of several gigabytes on its own --
// TestTypedArrayFromEndlessIterable's 2 GiB of values and the 1.6 GiB they
// were grown from, TestStrAppendRandom's seeds 382 and 388 -- which holds
// every slot of heavySlots, so that no other heavy test runs beside it:
// two of them, or one with another seed, had 8.2 GB live under the JIT's
// stress (heavyMemoryLimit).
func heavyOnly(t *testing.T) {
	t.Helper()
	t.Cleanup(heavySlots.enter(true))
	t.Cleanup(collector.enterHeavy(t.Name()))
}

// A test holding every slot runs beside no other heavy test, and two such
// tests, each waiting while another holds a slot, do not wait for each
// other.
func TestHeavyOnlyRunsApart(t *testing.T) {
	t.Parallel()
	g := newHeavyGate(2)
	one := g.enter(false)
	only := make(chan func())
	go func() { only <- g.enter(true) }()
	go func() { only <- g.enter(true) }()
	select {
	case <-only:
		t.Fatal("a test holding every slot began beside a heavy test")
	case <-time.After(50 * time.Millisecond):
	}
	one()
	within(t, 5*time.Second, "the first test holding every slot", func() {
		leave := <-only
		select {
		case g.slots <- struct{}{}:
			t.Error("a slot was free beside a test holding every slot")
		default:
		}
		leave()
	})
	within(t, 5*time.Second, "the second test holding every slot", func() { (<-only)() })
	within(t, 5*time.Second, "two heavy tests after", func() { g.enter(false)(); g.enter(false)() })
}

// heavyAlone has t, a test whose blocks on a 32-bit platform take much of
// the 4 GB of address space a process has there, run in parallel as a
// heavy test beside no other (heavyOnly) on a 64-bit platform, and on a
// 32-bit one among the package's serial tests, beside which no parallel
// test runs. TestBigIntStringLimit's strings, 2.4 GB there, and
// TestTypedArrayFromEndlessIterable's slices, each a block larger than the
// last, beside the parallel tests -- each other kept apart by heavy -- had
// reached 3.6 to 4.1 GB in use, and the package ran out of memory in one
// run of three. A test calls it in place of t.Parallel, and nothing of it
// may assume it runs in parallel.
func heavyAlone(t *testing.T) {
	t.Helper()
	if strconv.IntSize == 32 {
		return
	}
	t.Parallel()
	heavyOnly(t)
}
