package vm

import (
	"runtime"
	"time"
	_ "unsafe" // linkname
)

// nativeWB is the runtime's write-barrier flag, set while the collector
// marks, as native code and the JIT read it (internal/jit/barrier.go).
//
//go:linkname nativeWB runtime.writeBarrier
var nativeWB struct {
	enabled bool
	pad     [3]byte
	alignme uint64
}

// gcYieldState is Config.GCYield's: while the collector marks, a runtime
// whose process has one P waits, at the points Go runs between its
// instructions, until the marking is done.
//
// With one P, Go runs no dedicated mark worker: a fractional one takes a
// quarter of the P, so a mark phase lasts four times its work or more, and
// all that while every pointer store of the program goes through the write
// barrier -- every write of a register by the native handlers, every
// pointer word native code stores -- and what it shades. Waiting leaves the
// P idle, and Go gives an idle P to its idle mark worker, which marks at
// full speed: the mark phase is over in its own work's time, and the
// program runs none of it under the barrier, as with
// GODEBUG=gcstoptheworld=1 but for this goroutine alone.
//
// The points are where the program is in Go already, none of them in the
// native handlers' loop or the JIT's code: the interrupt check, which the
// interpreter, the native handlers and native code reach at least every
// thousand back edges and every four thousand calls (checkInterruptNow);
// each return of native code to Go and each call of Go from it
// (jitRegStretch, jitHostCall); and a pool's refill (jitRefillPool). With
// the option off, each costs one test of a field that is false.
type gcYieldState struct {
	// on is the option.
	on bool
	// one is whether GOMAXPROCS was 1 when last read, which checks says
	// when to read again.
	one bool
	// checks counts the checks made while marking since GOMAXPROCS was
	// last read: it is read at the first, after each wait, and every
	// gcYieldRecheck checks made while the collector marks. So a change of
	// GOMAXPROCS from 1 is seen at the next mark phase, and one to 1 within
	// gcYieldRecheck checks made while the collector marks.
	checks int
	// pauses, waited and worst count the waits, their sum and the longest
	// (GCYieldStats).
	pauses        int
	waited, worst time.Duration
	// marking, when a test sets it (SetGCMarkingForTest), is what the
	// runtime takes for the collector's marking in place of the
	// write-barrier flag, so that a test knows when the runtime finds the
	// collector marking rather than leaving it to where the collector's
	// cycles fall.
	marking func() bool
}

// gcMarking reports whether the collector marks, as Config.GCYield's checks
// see it: the write-barrier flag, or a test's answer (SetGCMarkingForTest).
func (r *Runtime) gcMarking() bool {
	if f := r.gcYield.marking; f != nil {
		return f()
	}
	return nativeWB.enabled
}

// SetGCMarkingForTest has Config.GCYield's checks take marking's answer for
// whether the collector marks, in place of the collector's own state; nil
// gives them the collector's again. Each check, and each look of a wait,
// asks it once. It is for tests, which then know when the runtime finds the
// collector marking: the collector's cycles start and end where the
// process's allocation and the machine's speed put them, which a check may
// meet or not.
func (r *Runtime) SetGCMarkingForTest(marking func() bool) { r.gcYield.marking = marking }

// gcYieldRecheck is how many checks made while the collector marks a
// runtime makes between readings of GOMAXPROCS when it was not 1: a reading
// takes the scheduler's lock, a few tens of nanoseconds, and the JIT's
// calls of Go check at each.
const gcYieldRecheck = 64

// gcYieldPoll is how long a waiting runtime sleeps between looks at the
// write-barrier flag. The runtime gives no signal at a mark phase's end that
// a goroutine can wait on -- a cleanup or a finalizer runs only once its
// object's span is swept, up to milliseconds later -- so the flag is polled:
// each look interrupts the idle mark worker briefly, and the wait outlasts
// the marking by up to one sleep, which the platform's timer rounds up (on
// Windows to a few hundred microseconds). Polls of 1, 20 and 100
// microseconds won back the same share of the V8 suite's time at one P.
const gcYieldPoll = 20 * time.Microsecond

// gcYieldNow waits while the collector marks if the process has one P. It
// is called only with the option on and the write-barrier flag set.
//
//go:noinline
func (r *Runtime) gcYieldNow() {
	y := &r.gcYield
	if y.checks == 0 {
		y.one = runtime.GOMAXPROCS(0) == 1
	}
	if y.checks++; y.checks == gcYieldRecheck {
		y.checks = 0
	}
	if !y.one {
		return
	}
	start := time.Now()
	for r.gcMarking() {
		time.Sleep(gcYieldPoll)
	}
	d := time.Since(start)
	y.checks = 0
	y.pauses++
	y.waited += d
	y.worst = max(y.worst, d)
}

// GCYieldStats reports the waits Config.GCYield has made the runtime take:
// how many, how long in all, and the longest.
func (r *Runtime) GCYieldStats() (pauses int, waited, worst time.Duration) {
	y := &r.gcYield
	return y.pauses, y.waited, y.worst
}
