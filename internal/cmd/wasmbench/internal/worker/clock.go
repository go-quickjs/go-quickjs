package worker

import (
	"time"

	"github.com/go-quickjs/go-quickjs/internal/spanclock"
)

// clock is the time from some moment of the process's, read from a clock
// that resolves spans of microseconds on every system (internal/spanclock):
// Go's own on Windows moves only at the system timer's tick, every 0.5 or
// 15.6 ms.
func clock() time.Duration { return spanclock.Now() }
