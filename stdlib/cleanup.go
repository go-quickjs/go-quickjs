package stdlib

import (
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// What the standard library holds for a script is released as the runtime
// closes, before Close returns -- a file closed, a port free, a program
// exited, a worker ended -- through the runtime's cleanup hooks, as a node
// worker's are when it ends. The runtime's Context, which is cancelled
// first, has by then told the work on other goroutines to stop; a hook
// releases what is left, or waits for that work to finish.

// closeWait bounds how long closing a runtime waits for work it has ended:
// a program being killed, a worker being stopped.
const closeWait = 5 * time.Second

// waitOnClose has rt, as it closes, wait for done -- the end of work a
// goroutine does for the script, which the runtime's Context has told to
// stop -- for at most closeWait. It returns what removes the hook, for work
// that finished first, which is called on the runtime's goroutine.
func waitOnClose(rt *quickjs.Runtime, done <-chan struct{}) (remove func()) {
	return rt.OnClose(func() {
		t := time.NewTimer(closeWait)
		defer t.Stop()
		select {
		case <-done:
		case <-t.C:
		}
	})
}
