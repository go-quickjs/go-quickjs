//go:build quickjs_jit && !android && !ios && (((linux || windows) && amd64) || ((darwin || linux) && arm64))

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
// or for a turn ended by a runtime's return to its host
// (DropIdleWorkspaces), as V8 gives a compile's zone back.
type Workspace struct {
	SSA ssa.Workspace
	MIR mir.Workspace
	// cycle is the collector's cycle (workCycle) it was given back in,
	// turn the hosts' returns' count (workTurn) then, and bytes what it
	// kept.
	cycle uint64
	turn  uint64
	bytes int
	// dirty marks one given back unrewound (ReturnWorkspace), which its
	// next taker rewinds, bytes what it kept when last rewound.
	dirty bool
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
	// workPooled is what workPool's workspaces keep, in bytes, and
	// workIdle how many it holds.
	workPooled atomic.Int64
	workIdle   atomic.Int64
	// workCycle counts the collector's cycles, from the first workspace
	// given back (workArm).
	workCycle atomic.Uint64
	workOnce  sync.Once
	// workTurn counts the looks at the pool as runtimes return to their
	// hosts (DropIdleWorkspaces) that found it holding a workspace.
	// workPool's lock guards it.
	workTurn uint64
)

// GetWorkspace is a workspace for one compile, the caller's until it
// gives it back (PutWorkspace, ReturnWorkspace): of those given back, the
// last one rewound, or else the last one, rewound here; or a new one.
func GetWorkspace() *Workspace { return getWorkspace(false) }

// GetWorkspaceOnWorker is GetWorkspace for a compile in the background's
// worker, which takes the last workspace given back unrewound
// (ReturnWorkspace) first, rewound here: the rewind is the worker's, not
// that of a runtime's goroutine compiling on the spot.
func GetWorkspaceOnWorker() *Workspace { return getWorkspace(true) }

// getWorkspace takes from the pool the last workspace given back that is
// dirty as dirty says, or else the last one, rewound if need be.
func getWorkspace(dirty bool) *Workspace {
	workPool.Lock()
	n := len(workPool.free)
	if n == 0 {
		workPool.Unlock()
		return &Workspace{}
	}
	i := n - 1
	for j := n - 1; j >= 0; j-- {
		if workPool.free[j].dirty == dirty {
			i = j
			break
		}
	}
	w := workPool.free[i]
	copy(workPool.free[i:], workPool.free[i+1:])
	workPool.free[n-1] = nil
	workPool.free = workPool.free[:n-1]
	workPooled.Add(-int64(w.bytes))
	workIdle.Add(-1)
	workPool.Unlock()
	if w.dirty {
		w.rewind()
	}
	return w
}

// rewind clears what the last compile built in w, at the collector's
// cycle it ended in (workStamp), and measures what it keeps.
func (w *Workspace) rewind() {
	w.SSA.RewindAt(w.cycle)
	w.MIR.RewindAt(w.cycle)
	w.bytes = w.SSA.Bytes() + w.MIR.Bytes()
	w.dirty = false
}

// PutWorkspace gives back w, rewound: nothing built in it may be used
// after. Its tables keep what the compiles of the collector's cycle and
// the one before needed (ssa's and mir's RewindAt), as the pool keeps a
// workspace given back for workspaceAge cycles.
func PutWorkspace(w *Workspace) {
	workStamp(w)
	w.rewind()
	workGiveBack(w)
}

// ReturnWorkspace gives back w as it is, its next taker rewinding it
// (GetWorkspace, GetWorkspaceOnWorker): nothing built in it may be used
// after. Kept, aged and let go of as one rewound is, its bytes counted as
// it kept them when last rewound, and rewound at the cycle it was given
// back in. A compile in the background's install gives its workspace
// back so, on the runtime's goroutine: rewound there, a clear of what the
// compile built had been more than half of an install's time, and handed
// to a worker to rewind -- a task queued, a worker started -- about a
// quarter of what was left.
func ReturnWorkspace(w *Workspace) {
	workStamp(w)
	w.dirty = true
	workGiveBack(w)
}

// workStamp records in w the collector's cycle its compile ended in,
// which its rewind and its age are counted from.
func workStamp(w *Workspace) {
	workOnce.Do(workArm)
	w.cycle = workCycle.Load()
}

// workGiveBack puts w in the pool, the last given back.
func workGiveBack(w *Workspace) {
	workPool.Lock()
	w.turn = workTurn
	workPool.free = append(workPool.free, w)
	workPooled.Add(int64(w.bytes))
	workIdle.Add(1)
	workPool.Unlock()
}

// DropIdleWorkspaces, as a runtime returns to its host, lets go of the
// pooled workspaces no compile has given back since the last such return:
// a program that has stopped compiling, and may run again much later or
// never, keeps none of its compiles' working memory past its next return,
// which had been tens of megabytes after a large function's compile -- 32
// MB after Octane, two of the collector's cycles past its last compile,
// which a host's one collection does not reach. A workspace given back
// since is kept: a host whose calls each compile -- a new function of
// each, made hot by its loop -- takes at each call the workspace the call
// before gave back, rather than make one anew at each, tens of
// allocations a call. A compile under way, its workspace out of the
// pool, keeps it; the compiles a turn makes share one as before.
func DropIdleWorkspaces() {
	if workIdle.Load() == 0 {
		return
	}
	workPool.Lock()
	defer workPool.Unlock()
	n := 0
	for n < len(workPool.free) && workPool.free[n].turn < workTurn {
		workPooled.Add(-int64(workPool.free[n].bytes))
		n++
	}
	workTurn++
	workPoolCut(n)
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
	workPoolCut(n)
}

// workPoolCut takes the first n workspaces, the oldest, out of the pool,
// whose bytes the caller has taken off workPooled. The caller holds the
// pool's lock.
func workPoolCut(n int) {
	if n == 0 {
		return
	}
	workIdle.Add(-int64(n))
	rest := copy(workPool.free, workPool.free[n:])
	clear(workPool.free[rest:])
	workPool.free = workPool.free[:rest]
}
