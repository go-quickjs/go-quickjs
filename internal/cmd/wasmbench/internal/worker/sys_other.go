//go:build !unix && !windows

package worker

import "time"

// processCPU is 0 where the platform does not say what CPU time the process
// has used.
func processCPU() time.Duration { return 0 }

// peakRSS is 0 where the platform does not say.
func peakRSS() uint64 { return 0 }
