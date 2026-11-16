//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import ()

// jitBackgroundRuntimes is how many runtimes TestJITBackgroundRuntimes runs
// at once, and jitBackgroundRounds how many times each runs all the work.
const ()
