//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"math"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
)

// The words naming objects native code does not make (abi.VirtualBase) are
// NaNs no value has: past every tag's, below every BigInt's.
const (
	_ = uint64(abi.VirtualBase - (tagBase | 1<<16))
	_ = uint64(heapBigBits - (abi.VirtualBase | 1<<(2*abi.VirtualHomeShift)))
)

// VirtualRoot is ssa.VirtualFeedback's: the shape and the prototype Go
// makes an object of pool with, where the code does not make it, as the
// pool's objects are made -- an object literal's, its site's root shape and
// the realm's Object.prototype; a construction's receiver's, its
// constructor's root shape and its prototype now (jitConstructLayout),
// which the code checks it has still (ssa.OpProtoIs) -- the shape
// remembered, as a cache's is. The build asks it of every pool escape
// analysis may take (ssa.VirtualFeedback), on the runtime's goroutine, and
// the answers are held until the compile keeps alive with the code those
// escape analysis took (keepVirtuals): its optimization then reads nothing
// of the runtime's. A pool's root is a layout's first, shared, never
// unique: remembering it changes nothing of how its objects' layouts
// change (shape's seen), however many of the pools asked are taken.
func (fb *jitFeedback) VirtualRoot(pool uintptr) (uintptr, uintptr, bool) {
	p := *(**abi.ObjectPool)(unsafe.Pointer(&pool))
	src := (*jitPoolSource)(p.Source)
	if src == nil {
		return 0, 0, false
	}
	var root *shape
	var proto *Object
	switch {
	case src.ctor != nil:
		var ok bool
		if proto, root, _, ok = fb.r.jitConstructLayout(src.ctor); !ok {
			return 0, 0, false
		}
	case src.root != nil && !src.array && src.lit == nil && src.tmpl.template() == nil:
		root, proto = src.root, (*Object)(p.Proto)
	}
	if root == nil || proto == nil {
		return 0, 0, false
	}
	if fb.virtualRoots == nil {
		fb.virtualRoots = map[uintptr]jitVirtualRoot{}
	}
	fb.virtualRoots[pool] = jitVirtualRoot{root: remember(root), proto: proto}
	return uintptr(unsafe.Pointer(root)), uintptr(unsafe.Pointer(proto)), true
}

// VirtualRoot is ssa.VirtualFeedback's for register code: as the stack
// form's (jitFeedback's), the pools' sources being the same -- an object
// literal's, a construction's receiver's -- and the answers held, then
// kept with the code, by the root's feedback (jitRegPlace).
func (f *jitRegFeedback) VirtualRoot(pool uintptr) (uintptr, uintptr, bool) {
	return f.fb.VirtualRoot(pool)
}

// jitVirtualRoot is VirtualRoot's answer for a pool, which holds them alive
// until the compile keeps them (keepVirtuals).
type jitVirtualRoot struct {
	root  *shape
	proto *Object
}

// keepVirtuals keeps alive with fb's code the roots and prototypes of the
// pools escape analysis took of the build's answers (ssa.Func's
// VirtualAsked), as often and in the order it took them, as a cache's
// shapes are kept.
func (fb *jitFeedback) keepVirtuals(pools []uintptr) {
	if len(pools) == 0 {
		return
	}
	k := fb.keep()
	for _, pool := range pools {
		v := fb.virtualRoots[pool]
		k.shapes = append(k.shapes, v.root)
		k.holders = append(k.holders, v.proto)
	}
}

// isVirtualWord reports whether v is a word naming an object the code did
// not make (abi.VirtualWord).
func isVirtualWord(v *Value) bool {
	return v.ref == nil && math.Float64bits(v.num)&^(1<<(2*abi.VirtualHomeShift)-1) == abi.VirtualBase
}

// namesVirtual reports whether a slot of vs is such a word.
func namesVirtual(vs []Value) bool {
	for i := range vs {
		if isVirtualWord(&vs[i]) {
			return true
		}
	}
	return false
}
