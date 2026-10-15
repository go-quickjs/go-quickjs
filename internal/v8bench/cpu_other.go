//go:build !unix && !windows

package v8bench

import "time"

// processCPU is 0 where the platform does not say what CPU time the process
// has used.
func processCPU() time.Duration { return 0 }
