//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"unsafe"

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

// The others take what jitGoFast takes -- the call's description st, and
// back, where the call goes on -- and their operands in the context's
// GoArgs, as the call's code wrote them: a convention of registers for
// them would change the code's sites and the routines' first
// instructions, which would write them there, and nothing here. Each
// makes its operation with the function jitHostCall's case calls, and
// refuses what that refuses, going on Fail bytes past back (abi.GoCallState's
// Fail), to the exit of the call, ctx as jit.goCall would have left it.

// jitStubCall is jitStubEnter for a call of Go whose description st its
// routine passed: ctx given what jit.ExpandGoCall gives it -- the call's
// operation and keep cell, the description as the last (GoLast, which Go
// reads of a call it makes, and an exit of one it refused), a call's PC
// where it says one -- but the operands, which the code wrote, and its
// status 1, which the operation's success clears. It reports false, ctx
// untouched, for a description of operands, which jitGoDirect reads: mir
// sends no such call here (goStub).
func jitStubCall(ctx *abi.Context, st *abi.GoCallState) bool {
	if st.Args != nil {
		return false
	}
	ctx.GoLast, ctx.GoState = uintptr(unsafe.Pointer(st)), 0
	ctx.GoOp, ctx.GoKeep = st.Op, st.Keep
	if st.SetsPC {
		ctx.ExitPC, ctx.ExitDepth = uint64(st.PC), uint64(st.Depth)
	}
	ctx.GoStatus = 1
	jitStubEnter(ctx)
	return true
}

// jitStubDone is where a routine's call goes on: back, its status cleared,
// where Go made it; Fail bytes past, where it did not.
func jitStubDone(ctx *abi.Context, st *abi.GoCallState, back uintptr, ok bool) uintptr {
	if ok {
		ctx.GoStatus = 0
		return back
	}
	return back + uintptr(int64(st.Fail))
}

// jitAppendGo is a loop's string builder's s += x for its routine
// (abi.GoStubAppend, jitAppendStub).
func jitAppendGo(ctx *abi.Context, st *abi.GoCallState, back uintptr) uintptr {
	if !jitStubCall(ctx, st) {
		return jitGoDirect(ctx, st, back)
	}
	return jitStubDone(ctx, st, back, (*Runtime)(ctx.Host).jitGoAppend(ctx))
}

// jitAddGo is any other + Go makes for its routine (abi.GoStubAdd,
// jitAddStub).
func jitAddGo(ctx *abi.Context, st *abi.GoCallState, back uintptr) uintptr {
	if !jitStubCall(ctx, st) {
		return jitGoDirect(ctx, st, back)
	}
	return jitStubDone(ctx, st, back, (*Runtime)(ctx.Host).jitGoAdd(ctx))
}

// jitConcatGo is a concatenation's parts joined for its routine
// (abi.GoStubConcat, jitConcatStub).
func jitConcatGo(ctx *abi.Context, st *abi.GoCallState, back uintptr) uintptr {
	if !jitStubCall(ctx, st) {
		return jitGoDirect(ctx, st, back)
	}
	return jitStubDone(ctx, st, back, (*Runtime)(ctx.Host).jitGoConcat(ctx))
}

// jitGoConcat joins the parts of a template or a concatenation
// (ssa.GoConcat) in ctx's GoArgs, its result to the call's keep cell, and
// reports whether it did, as concat joins its parts: primitives, converted
// without running anything, and nothing that throws -- the same refusals
// as GoAdd's. jitConcatGo and jitHostCall call it.
func (r *Runtime) jitGoConcat(ctx *abi.Context) bool {
	n := int(ctx.GoOp>>abi.GoOpArgcShift) & 0xff
	if n < 1 || n > abi.MaxGoArgs || r.meter != nil {
		return false
	}
	var parts [abi.MaxGoArgs]Value
	total := 0
	for i := range n {
		p := jitGoArg(ctx, i)
		if p.IsBigInt() {
			// Every part is joined to a string (the lowering's
			// stringConcats), a BigInt too.
			if !jitConcatOperand(p) {
				return false
			}
		} else if !jitPlainOperand(p) {
			return false
		}
		if !p.IsString() && !p.IsNumber() {
			s, err := r.toString(p)
			if err != nil {
				return false
			}
			p = Str(s)
		}
		if p.IsString() {
			if total += p.String().length; total > maxStringLength-64 {
				return false
			}
		}
		parts[i] = p
	}
	if n == 1 {
		// A template's part converted (to_string), as the interpreter
		// converts it: a string is itself, which a join had copied.
		p := parts[0]
		if p.IsNumber() {
			p = Str(r.numberString(p.num))
		}
		*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = p
		return true
	}
	s, err := r.joinParts(parts[:n])
	if err != nil || s.length > maxStringLength-64 {
		return false
	}
	*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = Str(s)
	return true
}

// The routines (jit_leaffast_*.s), and their addresses.
func jitPowStub()
func jitModStub()
func jitAtan2Stub()
func jitHypotStub()
func jitMathStub()
func jitAppendStub()
func jitAddStub()
func jitConcatStub()
func jitPowStubAddr() uintptr
func jitModStubAddr() uintptr
func jitAtan2StubAddr() uintptr
func jitHypotStubAddr() uintptr
func jitMathStubAddr() uintptr
func jitAppendStubAddr() uintptr
func jitAddStubAddr() uintptr
func jitConcatStubAddr() uintptr

// jitGoStubs is abi.Encoding's GoStubs: each routine's address.
func jitGoStubs() (s [abi.MaxGoStubs]uint64) {
	s[abi.GoStubPow] = uint64(jitPowStubAddr())
	s[abi.GoStubMod] = uint64(jitModStubAddr())
	s[abi.GoStubAtan2] = uint64(jitAtan2StubAddr())
	s[abi.GoStubHypot] = uint64(jitHypotStubAddr())
	s[abi.GoStubMath] = uint64(jitMathStubAddr())
	s[abi.GoStubAppend] = uint64(jitAppendStubAddr())
	s[abi.GoStubAdd] = uint64(jitAddStubAddr())
	s[abi.GoStubConcat] = uint64(jitConcatStubAddr())
	return s
}
