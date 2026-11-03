package vm

import (
	"sync"
	"testing"
	"unsafe"
)

// allocsOrdinary reports whether the build allocates as an ordinary one
// does, which the tests that count allocations pin. -d=checkptr=2 moves to
// the heap every variable whose address becomes an unsafe.Pointer, so that
// its checks can find the variable's bounds, and the engine makes such
// pointers throughout: those tests' counts, there, are the instrumentation's,
// not the engine's. escapeProbe allocates nothing but there.
var allocsOrdinary = sync.OnceValue(func() bool {
	return testing.AllocsPerRun(10, escapeProbe) == 0
})

var escapeProbeSink uint64

//go:noinline
func escapeProbe() {
	var x [2]uint64
	escapeProbeSink = *(*uint64)(unsafe.Pointer(&x))
}

// skipUnlessAllocsOrdinary skips the rest of a test, its counting of
// allocations, where the build does not allocate as an ordinary one does.
// What the test checked before it is still reported.
func skipUnlessAllocsOrdinary(t *testing.T) {
	t.Helper()
	if !allocsOrdinary() {
		t.Skip("allocations are counted only in an ordinary build: checkptr=2 moves variables to the heap")
	}
}
