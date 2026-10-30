//go:build quickjs_jit && !android && !ios && (((linux || windows) && amd64) || (darwin && arm64))

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
