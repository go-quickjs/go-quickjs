//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"github.com/go-quickjs/go-quickjs/internal/fdlibm"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
)

// A call of Go from native code that has a routine of its own
// (abi.Encoding's GoStubs, abi.GoStub) is made as V8's optimized code calls
// a builtin at its own entry, with its descriptor's registers: native code
// jumps to the operation's routine (jit_leaffast_*.s), which calls the
// operation's own function here with what the call passed in its registers,
// and goes straight back. Nothing of the call is found out at the call --
// no description read (jit.ExpandGoCall), no dispatch of every kind of
// call of Go (jitGoDirect, jitHostCall), and none of jitHostCall's frame,
// made for every case's locals at every call, which had been the largest
// part of a small call's way to Go. Each routine is, as jit's callGo is, a
// function the Go function that entered the code called, so that what it
// calls is ordinary Go, which may grow the stack, be preempted and
// allocate; native code has saved what it keeps.
//
// The Math functions Go computes (ssa.GoPow, GoMod, GoMath, GoAtan2,
// GoHypot) take their operands and give their result in floating-point
// registers, as V8's code calls its ieee754 functions: no description, and
// none refused.

// jitStubEnter is what each call through a routine of its own does before
// its operation, as jitHostCall does before its: a runtime that waits while
// the collector marks (WithGCYield) waits at the top of every call of Go
// from native code; a test's work at each call of Go (jitTestConfig's
// goCallWork) is done; the call is counted (jitState's goStub). Every
// context's Host is its runtime (jitGrowContexts).
func jitStubEnter(ctx *abi.Context) {
	r := (*Runtime)(ctx.Host)
	if r.gcYield.on && r.gcMarking() {
		r.gcYieldNow()
	}
	if w := r.jitTest.goCallWork; w != nil {
		w()
	}
	r.jit.goStub++
}

// jitPowGo is Math.pow(x, y) for its routine (abi.GoStubPow, jitPowStub).
func jitPowGo(ctx *abi.Context, x, y float64) float64 {
	jitStubEnter(ctx)
	return jsPow(x, y)
}

// jitModGo is x % y of numbers for its routine (abi.GoStubMod, jitModStub).
func jitModGo(ctx *abi.Context, x, y float64) float64 {
	jitStubEnter(ctx)
	return jsMod(x, y)
}

// jitAtan2Go is Math.atan2(x, y) for its routine (abi.GoStubAtan2,
// jitAtan2Stub).
func jitAtan2Go(ctx *abi.Context, x, y float64) float64 {
	jitStubEnter(ctx)
	return fdlibm.Atan2(x, y)
}

// jitHypotGo is Math.hypot(x, y) for its routine (abi.GoStubHypot,
// jitHypotStub).
func jitHypotGo(ctx *abi.Context, x, y float64) float64 {
	jitStubEnter(ctx)
	return jsHypot([]float64{x, y})
}

// jitMathGo is Math's function fn of x, an index of unaryMath, for its
// routine (abi.GoStubMath, jitMathStub): the code's constant, which the
// compile took only of one unaryMath has (jitFeedback's mathIntrinsic,
// jitRegFeedback's RawMath).
func jitMathGo(ctx *abi.Context, x float64, fn uint64) float64 {
	jitStubEnter(ctx)
	return unaryMath[fn](x)
}

// The routines (jit_leaffast_*.s), and their addresses.
func jitPowStub()
func jitModStub()
func jitAtan2Stub()
func jitHypotStub()
func jitMathStub()
func jitPowStubAddr() uintptr
func jitModStubAddr() uintptr
func jitAtan2StubAddr() uintptr
func jitHypotStubAddr() uintptr
func jitMathStubAddr() uintptr

// jitGoStubs is abi.Encoding's GoStubs: each routine's address.
func jitGoStubs() (s [abi.MaxGoStubs]uint64) {
	s[abi.GoStubPow] = uint64(jitPowStubAddr())
	s[abi.GoStubMod] = uint64(jitModStubAddr())
	s[abi.GoStubAtan2] = uint64(jitAtan2StubAddr())
	s[abi.GoStubHypot] = uint64(jitHypotStubAddr())
	s[abi.GoStubMath] = uint64(jitMathStubAddr())
	return s
}
