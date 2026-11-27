//go:build !windows

package spanclock

import "time"

// start is the moment Now counts from.
var start = time.Now()

// Now is the time from some moment of the process's, read from Go's
// monotonic clock.
func Now() time.Duration { return time.Since(start) }
