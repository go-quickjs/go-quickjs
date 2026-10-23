package mir

// A Phase is a stretch of a compile that Times accounts for: the SSA's
// build and optimization, which the compile's host times, and the
// backend's own phases.
type Phase int

const (
	// PhaseBuild and PhaseOptimize are ssa's BuildIn and Optimize, the
	// host's laps.
	PhaseBuild Phase = iota
	PhaseOptimize
	// PhasePrepare is the backend's checks and analyses before the
	// allocation: ssa.Origins, the layout, the lazy and fused values, the
	// budget loops.
	PhasePrepare
	// PhaseUses numbers the positions and lists every value's uses.
	PhaseUses
	// PhaseLiveness is the blocks' live sets.
	PhaseLiveness
	// PhaseIntervals is the intervals, sorted by start.
	PhaseIntervals
	// PhaseHints is the phis' hints and groups.
	PhaseHints
	// PhaseScan is the linear scan, both classes.
	PhaseScan
	// PhaseSaves is what each native call saves, and the keep cells'
	// coloring.
	PhaseSaves
	// PhaseEmit is the code generator's instruction selection: every
	// block's code, the edges' moves, the exits, the cold code.
	PhaseEmit
	// PhaseEncode is the assembler's finish -- jumps shortened, labels
	// bound -- and the code's descriptions copied out.
	PhaseEncode
	// NumPhases bounds the phases.
	NumPhases
)

var phaseNames = [NumPhases]string{"build", "optimize", "prepare", "uses", "liveness", "intervals", "hints", "scan", "saves", "emit", "encode"}

// String is the phase's name.
func (p Phase) String() string {
	if p >= 0 && p < NumPhases {
		return phaseNames[p]
	}
	return "phase?"
}

// Times accumulates how long each phase of the compiles takes, read from
// Clock: a compile's host calls Start before its build, and Lap at the end
// of each phase it runs itself; the backend laps its own phases.
type Times struct {
	Clock func() int64
	// Ticks are each phase's, in Clock's units; Compiles counts the
	// backend's compiles.
	Ticks    [NumPhases]int64
	Compiles int
	last     int64
}

// Timing, if not nil, is what every compile's phases are timed in. It is
// set by a measuring host before any runtime compiles, and read on the
// compiling goroutine: one runtime at a time.
var Timing *Times

// Start begins a compile's first phase.
func (t *Times) Start() { t.last = t.Clock() }

// Lap ends phase p, which began at the last lap or Start.
func (t *Times) Lap(p Phase) {
	now := t.Clock()
	t.Ticks[p] += now - t.last
	t.last = now
}

// lap ends phase p of the compile being timed, if one is.
func lap(p Phase) {
	if t := Timing; t != nil {
		t.Lap(p)
	}
}
