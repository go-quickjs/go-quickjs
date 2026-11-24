package quickjs_test

import (
	"strconv"
	"testing"
)

// heavySlots is held by the package's parallel tests whose peak is past
// half a gigabyte on a 32-bit platform, one at a time there: its process
// has 4 GB of address space, and TestGoRecursionIsBounded's 1.3 GB,
// TestMemoryLimitDoubling's 0.8 and TestTypedArrayIterableSources' 0.5,
// run together beside the rest, ran the package out of memory. A 64-bit
// platform has the room, and its tests run as they did.
var heavySlots = make(chan struct{}, 1)

// heavy has t, a memory-heavy parallel test, wait for heavySlots on a
// 32-bit platform and hold it until t and its cleanups are done. A test
// calls it after t.Parallel.
func heavy(t *testing.T) {
	t.Helper()
	if strconv.IntSize == 64 {
		return
	}
	heavySlots <- struct{}{}
	t.Cleanup(func() { <-heavySlots })
}
