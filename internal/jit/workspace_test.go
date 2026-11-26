//go:build quickjs_jit && !android && !ios && (((linux || windows) && amd64) || ((darwin || linux) && arm64))

package jit

import (
	"runtime"
	"sync"
	"testing"
	"time"
)

// A workspace given back is the next one taken: the one used last.
func TestWorkspaceReused(t *testing.T) {
	a := GetWorkspace()
	PutWorkspace(a)
	if b := GetWorkspace(); b != a {
		t.Error("the workspace given back was not the next taken")
	} else {
		PutWorkspace(b)
	}
}

// A pooled workspace is let go once workspaceAge of the collector's
// cycles have passed with no compile using it, and the pool's bytes with
// it: kept, the collector marks it at every cycle of a process that
// compiles no more.
func TestWorkspaceAged(t *testing.T) {
	w := GetWorkspace()
	PutWorkspace(w)
	cycle := w.cycle
	workAge(cycle + workspaceAge - 1)
	if !pooled(w) {
		t.Fatalf("let go %d cycles after it was given back", workspaceAge-1)
	}
	workAge(cycle + workspaceAge)
	if pooled(w) {
		t.Fatalf("kept %d cycles after it was given back", workspaceAge)
	}
	// And by the collector's cycles themselves.
	w = GetWorkspace()
	PutWorkspace(w)
	for i := 0; i < 100 && pooled(w); i++ {
		runtime.GC()
		time.Sleep(time.Millisecond)
	}
	if pooled(w) {
		t.Fatal("kept through a hundred of the collector's cycles")
	}
}

// A pooled workspace goes as a runtime returns to its host
// (DropIdleWorkspaces) when no compile has given it back since the return
// before, and the pool's bytes with it; one given back since is kept, for
// the next turn's compiles to take; one a compile is using stays its.
func TestWorkspaceDroppedIdle(t *testing.T) {
	busy := GetWorkspace()
	w := GetWorkspace()
	PutWorkspace(w)
	if !pooled(w) {
		t.Fatal("not pooled")
	}
	DropIdleWorkspaces()
	if !pooled(w) {
		t.Fatal("a workspace given back in the turn let go of at its end")
	}
	// A turn whose compile takes it and gives it back: kept again.
	if GetWorkspace() != w {
		t.Fatal("the workspace given back was not the next taken")
	}
	PutWorkspace(w)
	DropIdleWorkspaces()
	if !pooled(w) {
		t.Fatal("a workspace used in the turn let go of at its end")
	}
	// A turn with no compile: it goes.
	DropIdleWorkspaces()
	if pooled(w) {
		t.Fatal("an idle workspace kept through a turn")
	}
	PutWorkspace(busy)
	if !pooled(busy) {
		t.Fatal("the workspace in use was not given back")
	}
	DropIdleWorkspaces()
	DropIdleWorkspaces()
	if pooled(busy) || workIdle.Load() != 0 || PooledWorkspaceBytes() != 0 {
		t.Fatalf("%d workspaces, %d bytes kept", workIdle.Load(), PooledWorkspaceBytes())
	}
}

// Runtimes on goroutines of their own compile at once, each in a
// workspace no other uses meanwhile.
func TestWorkspaceConcurrent(t *testing.T) {
	var wg sync.WaitGroup
	var mu sync.Mutex
	inUse := map[*Workspace]bool{}
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 1000 {
				w := GetWorkspace()
				mu.Lock()
				if inUse[w] {
					t.Error("a workspace taken twice")
				}
				inUse[w] = true
				mu.Unlock()
				mu.Lock()
				delete(inUse, w)
				mu.Unlock()
				PutWorkspace(w)
			}
		}()
	}
	wg.Wait()
	if n := PooledWorkspaceBytes(); n < 0 {
		t.Fatalf("%d bytes pooled", n)
	}
}

// pooled reports whether w is in the pool.
func pooled(w *Workspace) bool {
	workPool.Lock()
	defer workPool.Unlock()
	for _, x := range workPool.free {
		if x == w {
			return true
		}
	}
	return false
}

// A workspace given back unrewound (ReturnWorkspace) is kept, aged and let
// go of as one given back rewound is, a worker's ask takes it first and
// rewinds it (GetWorkspaceOnWorker), and an ask on the spot takes one
// rewound first (GetWorkspace): an idle process holds none of either.
func TestWorkspaceReturnedUnrewound(t *testing.T) {
	clean, dirty := GetWorkspace(), GetWorkspace()
	PutWorkspace(clean)
	ReturnWorkspace(dirty)
	if !pooled(clean) || !pooled(dirty) || !dirty.dirty || clean.dirty {
		t.Fatal("not pooled as given back")
	}
	if w := GetWorkspace(); w != clean {
		t.Fatal("the spot's ask did not take the rewound one first")
	} else {
		PutWorkspace(w)
	}
	if w := GetWorkspaceOnWorker(); w != dirty || w.dirty {
		t.Fatalf("the worker's ask took %p (dirty %v), want %p rewound", w, w.dirty, dirty)
	} else {
		ReturnWorkspace(w)
	}
	// Aged by the collector's cycles, as a rewound one is.
	cycle := dirty.cycle
	workAge(cycle + workspaceAge - 1)
	if !pooled(dirty) {
		t.Fatalf("let go %d cycles after it was given back", workspaceAge-1)
	}
	workAge(cycle + workspaceAge)
	if pooled(dirty) || pooled(clean) {
		t.Fatalf("kept %d cycles after it was given back", workspaceAge)
	}
	// And at two returns to the host with no compile.
	w := GetWorkspace()
	ReturnWorkspace(w)
	DropIdleWorkspaces()
	DropIdleWorkspaces()
	if pooled(w) || workIdle.Load() != 0 || PooledWorkspaceBytes() != 0 {
		t.Fatalf("%d workspaces, %d bytes kept idle", workIdle.Load(), PooledWorkspaceBytes())
	}
}
