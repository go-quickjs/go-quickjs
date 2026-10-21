//go:build !windows

package v8bench

import "time"

// clockStart is the moment clock counts from.
var clockStart = time.Now()

// clock is the time from some moment of the process's, read from Go's
// monotonic clock.
func clock() time.Duration { return time.Since(clockStart) }
