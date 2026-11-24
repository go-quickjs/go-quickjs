//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"slices"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/jit"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
)

// Native code's calls of Go that a destructuring's rest and an empty
// literal's pool make (jitRestPlain, iterRestDense, jitLiteralArraySource).
// They are laid out last in the range of the layout lock's pad N, just
// before it (native_r_layoutpad.go), so that what they add to the package
// moves nothing between anchors J and N but the pad: in jit_plain.go and
// jit_ssa.go, with their callers, they moved every function after them --
// the typed arrays' reads among them, typed_u64_read 7% slower with the
// JIT, its code byte-identical.

// jitRestPlain makes a destructuring's rest element (abi.GoCallRest) of
// the cursor native code passed as iter_rest makes it, without a frame,
// where that runs no script and cannot throw: of a cursor over an array
// whose rest is all in its dense storage (iterRestDense). It writes the
// array to the call's keep cell and reports whether it made it; anything
// else it leaves to the call with its frame (jitRegIterMade).
func (r *Runtime) jitRestPlain(ctx *abi.Context) bool {
	st := cursorOf(jitGoArg(ctx, 0))
	if st == nil || r.meter != nil {
		return false
	}
	v, ok := r.iterRestDense(st)
	if ok {
		*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = v
	}
	return ok
}

// iterRestDense is iterRest of st, a cursor over an array by the intrinsic
// iterator, where what is left of the array is all in its dense storage:
// copied at once, as each step would read an element there, running
// nothing, and the walk ends at its length, done, as iterRest's steps leave
// it. It reports whether it made the rest; any other cursor's steps it
// leaves to iterRest, which may run script and throw. Native code's rest
// (jitRestPlain) makes it so, as iter_rest's handler makes it in place.
func (r *Runtime) iterRestDense(st *iterState) (Value, bool) {
	if st.done || st.arr == nil || st.arr.flags&objHasSparseElements != 0 {
		return Undefined, false
	}
	el := st.arr.elemsOf()
	n := len(el)
	if int64(n) != int64(st.arr.arrayLength()) || st.idx > n || slices.ContainsFunc(el[st.idx:], isHole) {
		return Undefined, false
	}
	v := Obj(r.newArrayFrom(el[st.idx:]))
	st.idx, st.done = n, true
	return v, true
}

// jitLiteralArraySource is a pool's source for the empty array literal at
// register pc pc of cl's code, with the capacity the interpreter's site
// confirmed at a turn's end before the literal reached native code
// (arrayRoom's confirmed). Only that, of the interpreter's: a hint taken
// within a turn may be an array a callback is still growing, and left
// several callbacks 7 to 18% slower (docs/performance-plan.md, F3); the
// pool learns a large room of its own arrays, two in a row that agree
// (jitFillLiteral), as the interpreter's site learns its. The pc, unlike
// the site's ordinal, also names a literal in an inlined callee. The pool
// is kept weakly, so that this list keeps neither it nor code that has
// been thrown away.
func jitLiteralArraySource(r *Runtime, cl *closure, pool *abi.ObjectPool, pc, n int) *jitPoolSource {
	s := &jitPoolSource{array: true, n: n}
	for _, a := range cl.arraySites() {
		if a != nil && a.pc == int32(pc) && a.confirmed >= arrayRoomLeast && a.confirmed <= arrayRoomLimit {
			s.room = int(a.confirmed)
			if r.jit != nil && pool != nil {
				s.listed = true
				r.jit.arrayRooms = append(r.jit.arrayRooms, jit.WeakPool(pool))
			}
			break
		}
	}
	return s
}
