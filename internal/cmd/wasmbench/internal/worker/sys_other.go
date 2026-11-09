//go:build !unix && !windows

package worker

import "time"

// clockStart is the moment clock counts from.
var clockStart = time.Now()

// clock is the time from some moment of the process's.
func clock() time.Duration { return time.Since(clockStart) }

// processCPU is 0 where the platform does not say what CPU time the process
// has used.
func processCPU() time.Duration { return 0 }

// peakRSS is 0 where the platform does not say.
func peakRSS() uint64 { return 0 }
