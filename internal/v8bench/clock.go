package v8bench

import (
	"time"

	"github.com/go-quickjs/go-quickjs/internal/spanclock"
)

// clock is the time from some moment of the process's, read from a clock
// that resolves a run of a few milliseconds on every system
// (internal/spanclock): Go's own on Windows moves only at the system
// timer's tick, so a run could be timed as none, and the next as two.
func clock() time.Duration { return spanclock.Now() }
