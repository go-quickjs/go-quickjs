package vm

import (
	"context"
	"time"
)

// watchStopForTest is the public Runtime's watch on its context
// (watchEngine in package quickjs) for this package's tests, which run
// scripts with Runtime.SetContext: once ctx ends, the counters are run out
// until r runs no script, so that the next back edge or call checks
// (zstop.go). The function it returns ends the watch. With the race
// detector on, it watches nothing.
func watchStopForTest(r *Runtime, ctx context.Context) (unwatch func()) {
	if raceDetector {
		return func() {}
	}
	quit := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		for wait := time.Millisecond; ; wait = min(2*wait, 16*time.Millisecond) {
			r.interruptCounter, r.backEdges = 0, 0
			if r.frameDepth == 0 && r.hostCalls == 0 {
				return
			}
			select {
			case <-quit:
				return
			case <-time.After(wait):
			}
		}
	})
	return func() {
		close(quit)
		stop()
	}
}
