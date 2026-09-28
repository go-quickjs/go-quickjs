package stdlib

import (
	"testing"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// TestBroadcastChannelLeavesWithItsRuntime pins that a BroadcastChannel the
// script never closes stops being one of its name when its runtime is
// closed: it stayed, keeping the runtime reachable, and every later
// broadcast of the name queued on it (KI-22).
func TestBroadcastChannelLeavesWithItsRuntime(t *testing.T) {
	const name = "left open by a closed runtime"
	rt := quickjs.New()
	if err := Install(rt, Config{Loop: NewLoop(rt)}); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Eval(`new BroadcastChannel("` + name + `"); new BroadcastChannel("` + name + `")`); err != nil {
		t.Fatal(err)
	}
	if n := registered(name); n != 2 {
		t.Fatalf("%d registered while the runtime is open, want 2", n)
	}
	rt.Close()
	// The runtime's context ends at once, but what waits on it runs on its
	// own goroutine.
	for deadline := time.Now().Add(5 * time.Second); registered(name) > 0 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	if n := registered(name); n != 0 {
		t.Errorf("%d still registered after the runtime closed", n)
	}
}

// registered is how many BroadcastChannels of a name there are.
func registered(name string) int {
	broadcasts.Lock()
	defer broadcasts.Unlock()
	return len(broadcasts.byName[name])
}
