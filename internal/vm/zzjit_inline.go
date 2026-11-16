//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

// jitRunsSeen is how many runs of its code a function has made, at least,
// for a call none of them made to be one seldom made.
const jitRunsSeen = 16
