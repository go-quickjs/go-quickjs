package opt

import "github.com/go-quickjs/go-quickjs/internal/jit/lsra"

// A Phase is a stretch of a compile that Times accounts for.
type Phase int

// The phases, in the order a compile runs them: the refusal check and the
// SSA's build, each pass of Optimize (a pass run twice counts twice in
// one), the lowering's description of the function to the allocator, the
// allocator's own phases, and the code's emission and encoding.
const (
	PhaseCheck Phase = iota
	PhaseBuild
	PhaseCopyelim
	PhaseGVN
	PhaseFold
	PhaseThread
	PhaseLICM
	PhaseDCE
	PhaseBCE
	PhaseSplit
	PhaseLoops
	PhaseDescribe
	PhaseLiveness
	PhaseRanges
	PhaseWebs
	PhaseScan
	PhaseSlots
	PhaseEmit
	PhaseEncode
	NumPhases
)

var phaseNames = [NumPhases]string{"check", "build", "copyelim", "gvn", "fold", "thread", "licm", "dce", "bce", "split",
	"loops", "describe", "liveness", "ranges", "webs", "scan", "slots", "emit", "encode"}

// String is the phase's name.
func (p Phase) String() string {
	if p >= 0 && p < NumPhases {
		return phaseNames[p]
	}
	return "phase?"
}

// Times accumulates how long each phase of the compiles takes, read from
// Clock.
type Times struct {
	Clock func() int64
	// Ticks are each phase's, in Clock's units.
	Ticks [NumPhases]int64
	last  int64
}

// Timing, if not nil, is what every compile's phases are timed in. A
// measuring host sets it before compiling, and compiles on one goroutine.
var Timing *Times

// start begins a compile's first phase, if compiles are timed.
func start() {
	if t := Timing; t != nil {
		t.last = t.Clock()
	}
}

// lap ends phase p, which began at the last lap or start.
func lap(p Phase) {
	if t := Timing; t != nil {
		now := t.Clock()
		t.Ticks[p] += now - t.last
		t.last = now
	}
}

// lsraPhases are the allocator's phases as this package's.
var lsraPhases = [lsra.NumPhases]Phase{
	lsra.PhaseLiveness: PhaseLiveness,
	lsra.PhaseRanges:   PhaseRanges,
	lsra.PhaseWebs:     PhaseWebs,
	lsra.PhaseScan:     PhaseScan,
	lsra.PhaseSlots:    PhaseSlots,
}

// lsraLap is the allocator's lap, while compiles are timed.
func lsraLap(p lsra.Phase) { lap(lsraPhases[p]) }
