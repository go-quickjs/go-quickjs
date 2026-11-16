//go:build !unix && !windows

package vm

import "time"

var processCPUStart = time.Now()

// processCPUForTest is, where the process's CPU time cannot be read, the
// time since it started.
func processCPUForTest() time.Duration { return time.Since(processCPUStart) }
