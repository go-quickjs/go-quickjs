//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
)

// Forwarding calls: f.apply(x, arguments) in a function whose arguments
// object is never made (bytecode's ForwardsArguments, apply_arguments),
// made by native code as V8's TurboFan makes JSCallForwardVarargs -- a
// call of f with the arguments the frame was given, copied from where they
// are to the callee's frame, their count read at run time
// (ssa.CallSite's Forward). A native callee's are in its context, where
// its call wrote them (jitFrameArgs); a frame Go enters the code with has
// them in the VM's frame, which Go copies to the context at each entry
// (jitForwardEntry).

// jitForwardEntry writes args, the arguments of the frame Go enters code
// that hands them on with (ir.Program's Forwards), to its context, as a
// native call writes its callee's (abi.Context's Argc and Args): their
// count, which past the context's room for them has the code's forwarding
// calls made by Go, from the frame's; and their words, as the context
// holds them. The frame keeps them alive while the code runs.
func jitForwardEntry(ctx *abi.Context, args []Value) {
	ctx.Argc = uint64(len(args))
	if len(args) > len(ctx.Args) {
		return
	}
	for i := range args {
		ctx.Args[i] = *(*abi.GoArg)(unsafe.Pointer(&args[i]))
	}
}

// jitForwardPlain makes Array.apply(null, arguments) -- a forwarding call
// (abi.GoCallForward) of the realm's Array through its apply -- as the
// constructor called so makes it, an array of the frame's arguments,
// without a frame: what runs no script and cannot throw. Array(n) of one
// number, which makes an array of that length or throws, it leaves to
// jitGoCall, as it does more arguments than the context holds -- the
// frame's, read there -- and a runtime that meters its memory.
func (r *Runtime) jitForwardPlain(ctx *abi.Context) bool {
	if ctx.Argc > uint64(len(ctx.Args)) || r.meter != nil || r.applyFn == nil {
		return false
	}
	t, ap := jitGoArg(ctx, 0), jitGoArg(ctx, 1)
	if !t.IsObject() || t.Object() != r.proto.arrayCtor || !ap.IsObject() || ap.Object() != r.applyFn {
		return false
	}
	args := jitContextArgs(ctx)
	if len(args) == 1 && args[0].IsNumber() {
		return false
	}
	*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = Obj(r.newArrayFrom(args))
	return true
}
