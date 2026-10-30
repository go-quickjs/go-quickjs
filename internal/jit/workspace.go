//go:build quickjs_jit && !android && !ios && (((linux || windows) && amd64) || (darwin && arm64))

package jit

import (
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// A Workspace is the memory one compile builds and compiles in: its
// build's (ssa.Workspace) and its backend's (mir.Workspace). A compile
// takes one from the process's pool (GetWorkspace) and gives it back once
// its code is placed (PutWorkspace), so that a runtime holds none between
// its compiles: kept by each runtime, they were as big as its biggest
// recent compile, and outlived its compiling -- a server rendering React
// held 12.6 MiB of them a runtime after two renders, beside the 6.5 the
// JIT's budget counts. Pooled, the process keeps as many as compile at
// once, as big as the recent compiles needed, and lets them go once
// nothing has compiled in them for workspaceAge of the collector's cycles,
// as V8 gives a compile's zone back.
type Workspace struct {
	SSA ssa.Workspace
	MIR mir.Workspace
	// cycle is the collector's cycle (workCycle) it was given back in,
	// and bytes what it kept then.
	cycle uint64
	bytes int
}

// workspaceAge is how many of the collector's cycles a pooled workspace
// is kept unused.
const workspaceAge = 2

var (
	// workPool holds the workspaces no compile is using, the last given
	// back last: a compile takes the one used most recently, and the
	// others age.
	workPool struct {
		sync.Mutex
		free []*Workspace
	}
	// workPooled is what workPool's workspaces keep, in bytes.
	workPooled atomic.Int64
	// workCycle counts the collector's cycles, from the first workspace
	// given back (workArm).
	workCycle atomic.Uint64
	workOnce  sync.Once
)

// GetWorkspace is a workspace for one compile, the caller's until it
// gives it back (PutWorkspace): the one given back last, or a new one.
func GetWorkspace() *Workspace {
	workPool.Lock()
	defer workPool.Unlock()
	if n := len(workPool.free); n > 0 {
		w := workPool.free[n-1]
		workPool.free[n-1] = nil
		workPool.free = workPool.free[:n-1]
		workPooled.Add(-int64(w.bytes))
		return w
	}
	return &Workspace{}
}

// PutWorkspace gives back w, rewound: nothing built in it may be used
// after.
func PutWorkspace(w *Workspace) {
	w.SSA.Rewind()
	w.MIR.Rewind()
	w.bytes = w.SSA.Bytes() + w.MIR.Bytes()
	workOnce.Do(workArm)
	w.cycle = workCycle.Load()
	workPool.Lock()
	workPool.free = append(workPool.free, w)
	workPooled.Add(int64(w.bytes))
	workPool.Unlock()
}

// PooledWorkspaceBytes is what the process's pooled workspaces keep: the
// compiles' working memory, shared by every runtime, which no runtime's
// budget counts.
func PooledWorkspaceBytes() int64 { return workPooled.Load() }

// workMark is what the cycle's finalizer is set on: an object with a
// pointer, so that it is not a tiny allocation, whose finalizer could wait
// for the objects sharing its block.
type workMark struct{ _ *byte }

// workArm counts the collector's cycles: a finalizer it runs once a cycle
// lets go the workspaces unused for workspaceAge of them, and sets itself
// again.
func workArm() {
	runtime.SetFinalizer(&workMark{}, func(*workMark) {
		workAge(workCycle.Add(1))
		workArm()
	})
}

// workAge lets go the pooled workspaces given back workspaceAge cycles
// before cycle or earlier: the oldest, at the pool's start.
func workAge(cycle uint64) {
	workPool.Lock()
	defer workPool.Unlock()
	n := 0
	for n < len(workPool.free) && workPool.free[n].cycle+workspaceAge <= cycle {
		workPooled.Add(-int64(workPool.free[n].bytes))
		n++
	}
	if n == 0 {
		return
	}
	rest := copy(workPool.free, workPool.free[n:])
	clear(workPool.free[rest:])
	workPool.free = workPool.free[:rest]
}
