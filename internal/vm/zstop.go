package vm

import "unsafe"

// The offsets in a Runtime of the counters its checks for an interrupt
// count down, and of what says it runs a script: a host that is to see a
// stop noticed at the next back edge or call -- the public Runtime's
// watchStop -- stores zero in the counters from another goroutine once the
// host has stopped the runtime, and stops storing once no script runs.
// The next back edge or call then finds its counter run out and makes the
// check it makes when one runs out, which reads the context and the abort
// channel, on the runtime's own goroutine, as it always has.
//
// It is V8's design: a stop request rides on a check every loop and call
// makes already -- V8's stack limit's, here the counters' -- so it costs
// the interpreter, the tree tier and native code nothing while nothing
// stops them. The counted checks alone come every backEdgeCheckInterval
// backward jumps and interruptCheckInterval calls, which a loop of a few
// long turns -- converting a string of millions of digits to a number --
// spaces seconds apart.
//
// They are constants, not a function storing them, so that package vm's
// code, and where its closures are laid out against its functions, stays
// as it was (AGENTS.md): any function or function literal added here moves
// the tree tier's closures.
const (
	OffsetInterruptCounter = unsafe.Offsetof(Runtime{}.interruptCounter)
	OffsetBackEdges        = unsafe.Offsetof(Runtime{}.backEdges)
	OffsetFrameDepth       = unsafe.Offsetof(Runtime{}.frameDepth)
	OffsetHostCalls        = unsafe.Offsetof(Runtime{}.hostCalls)
)
