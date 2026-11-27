//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import "github.com/go-quickjs/go-quickjs/internal/jit/abi"

// CellsPool derived on the worker (jit_snapderive.go): the pool the code
// takes its captured registers' cells from, which the answer reads of the
// program -- its captured registers, which the lowering finds -- and of
// the function's learning, the pool it holds, made where it holds none.
// The gather copies the pool's pointer; the derivation, of the program
// bound on the worker, makes a pool where it was none (jitFillCells:
// allocations of the pool's own, nothing of the runtime's read or
// written), which the finish gives the learning where it still has none,
// on the runtime's goroutine (jitFinishLearned). One implementation with
// the feedback's (jitCellsPoolOf).

// jitCellsData is a snapshot's copy of what CellsPool's answer is made of:
// the learning whose pool it is, nil for a callee's or a feedback of no
// closure or learning, its pool as the gather found it, and the one the
// derivation made.
type jitCellsData struct {
	gathered bool
	e        *jitEntry
	pool     *abi.ObjectPool
	made     *abi.ObjectPool
}

func init() {
	jitSnapDerived |= 1 << snapCellsPool
	jitSnapFuncGatherers = append(jitSnapFuncGatherers, gatherCells)
	jitSnapDerivers[jitDeriveCopies] = append(jitSnapDerivers[jitDeriveCopies], deriveCells)
	jitSnapFinishers[jitFinishLearned] = append(jitSnapFinishers[jitFinishLearned], finishCells)
}

// gatherCells copies, for s, the snapshot of fb, the learning's pool.
func gatherCells(s *jitSnapshot, fb *jitRegFeedback) {
	d := &s.d.cells
	*d = jitCellsData{gathered: true}
	if f := fb.fb; f.e != nil && f.root == nil && f.cl != nil {
		d.e, d.pool = f.e, f.e.cellsPool
	}
}

// deriveCells puts s's CellsPool answer, of its program and the pool its
// gather copied, made where there was none.
func deriveCells(s *jitSnapshot) {
	d := &s.d.cells
	if !d.gathered || s.p == nil {
		return
	}
	var pool **abi.ObjectPool
	if d.e != nil {
		pool = &d.pool
	}
	had := d.pool
	sink := s.sinkOf()
	before := s.begin(sink)
	r0, r1, r2 := jitCellsPoolOf(s.p, pool, sink.keep())
	eff := s.endDerived(sink, before)
	if d.pool != had {
		d.made = d.pool
	}
	s.heard(snapCellsPool, 0, true, r0, r1, r2)
	s.a.CellsPool.put(0, eff, true, jitSnapAnsCellsPool{r0, r1, r2})
}

// finishCells gives the learning the pool the derivation made, where it
// still has none.
func finishCells(s *jitSnapshot) {
	if d := &s.d.cells; d.made != nil && d.e.cellsPool == nil {
		d.e.cellsPool = d.made
	}
}
