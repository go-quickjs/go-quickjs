//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"sync"
)

// jitSnapPending is a build the oracle is to compare: its snapshot, and
// its feedback's lists before it.
type jitSnapPending struct {
}

// jitSnapChecks is what the oracle found.
var jitSnapChecks = struct {
	mu      sync.Mutex
	builds  int
	misses  map[string]int
	found   []string
	note    func(string)
	pending map[*jitFeedback]jitSnapPending
}{misses: map[string]int{}, pending: map[*jitFeedback]jitSnapPending{}}

func init() {
	jitSnapChecks.note = func(s string) { jitSnapChecks.found = append(jitSnapChecks.found, s) }
}
