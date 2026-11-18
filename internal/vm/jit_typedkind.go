//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"math"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
)

// The element types of the typed arrays an element read or write of a
// number has met, as V8's feedback keeps a keyed access's elements kind,
// are learned as native code runs, which is where most such reads run: the
// interpreter keeps nothing of a site's. Each site has a cell, its
// function's lowering's (jitLowering's typedCells), whose number word has
// bit 1<<k for each element type k it met. Code compiled with the cell
// reads it as it reads a typed array's element the site's code was not
// made for -- of another type, or at a site that met none, or several --
// and calls Go (ssa.GoTypedSeen) to set the type's bit where it has none,
// once for each type: Go marks the code to be compiled again (jitEntry's
// reopt), for the type it met if that is the only one, inline, with no
// other type's case (ssa.TypedKindFeedback).

// jitTypedCellBytes is what a site's cell costs its lowering: the cell
// and its slot in the lowering's table.
const jitTypedCellBytes = int(unsafe.Sizeof(Value{})) + jitMapSlot

// jitTypedSeen is ssa.GoTypedSeen: the typed array in the first operand,
// the site's cell in the second's pointer word. It sets the bit of the
// array's element type in the cell, and marks the code the call was made
// from to be compiled again (jitRegReopt). It refuses only what native
// code never asks, which exits.
func (r *Runtime) jitTypedSeen(ctx *abi.Context) bool {
	o := *(**Object)(unsafe.Pointer(&ctx.GoArgs[0].Ref))
	cell := *(**Value)(unsafe.Pointer(&ctx.GoArgs[1].Ref))
	if o == nil || cell == nil || o.class != ClassTypedArray {
		return false
	}
	t, ok := o.dataOf().(*typedArrayData)
	if !ok || t.kind > abi.ElemBigUint64 {
		return false
	}
	bits := math.Float64bits(cell.num)
	if bit := uint64(1) << t.kind; bits&bit == 0 {
		cell.num = math.Float64frombits(bits | bit)
		if e := r.jitRegCtxEntry(ctx); e != nil {
			// Register code's: compiled again as what its calls of Go
			// learn has it (jitRegReopt).
			r.jitRegReopt(nil, e)
		}
	}
	return true
}

// jitTypedView is ssa.GoTypedView: the typed array in the first operand,
// whose view's copy of where its elements are and how many (cacheView) is
// of an epoch before the runtime's -- a buffer's bytes moved or changed
// length since, or Go never made it. It makes the copy anew, for native
// code to read it again, and leaves the operand as it is. It refuses only
// what is no typed array, which native code never asks.
func (r *Runtime) jitTypedView(ctx *abi.Context) bool {
	o := *(**Object)(unsafe.Pointer(&ctx.GoArgs[0].Ref))
	if o == nil || o.class != ClassTypedArray {
		return false
	}
	t, ok := o.dataOf().(*typedArrayData)
	if !ok {
		return false
	}
	r.cacheView(t)
	if jitTypedViewMade != nil {
		jitTypedViewMade()
	}
	return true
}

// jitTypedViewMade, if set, is called at each view's copy jitTypedView
// makes: tests count them.
var jitTypedViewMade func()
