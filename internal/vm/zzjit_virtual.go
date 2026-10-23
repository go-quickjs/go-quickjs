//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

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
// which the code checks it has still (ssa.OpProtoIs) -- each kept alive
// with the code, the shape remembered, as a cache's are.
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
	k := fb.keep()
	k.shapes = append(k.shapes, remember(root))
	k.holders = append(k.holders, proto)
	return uintptr(unsafe.Pointer(root)), uintptr(unsafe.Pointer(proto)), true
}

// jitMaterializeObjects makes, in f, the frame of e's code that an exit
// left, the objects the code did not make (ssa's escape.go): a slot that
// holds the word of one (abi.VirtualWord) gets the object, made of its
// home, which the exit wrote past the frame's slots -- its shape, its
// prototype, its properties' values, words of others among them -- one for
// each home, which every slot naming it shares, as the code would have
// made one: as V8's deoptimizer materializes the objects escape analysis
// removed. The homes are cleared after.
func (r *Runtime) jitMaterializeObjects(f *frame, e *jitEntry) {
	type made struct {
		home, fields int
		o            *Object
	}
	var buf [4]made
	objs := buf[:0]
	at := func(slot int) *Value { return r.jitSlot(f, e.this, slot) }
	var get func(w uint64) *Object
	fix := func(v *Value) {
		if v.ref != nil {
			return
		}
		if w := math.Float64bits(v.num); w&^(1<<(2*abi.VirtualHomeShift)-1) == abi.VirtualBase {
			*v = Obj(get(w))
		}
	}
	get = func(w uint64) *Object {
		home, fields, _ := abi.IsVirtualWord(w)
		for _, m := range objs {
			if m.home == home {
				return m.o
			}
		}
		sw, pw := math.Float64bits(at(home).num), math.Float64bits(at(home+1).num)
		o := newLiteralObject(*(**Object)(unsafe.Pointer(&pw)), ClassObject, fields)
		o.shape = *(**shape)(unsafe.Pointer(&sw))
		objs = append(objs, made{home, fields, o})
		// Its properties, the words of others among them made too.
		for k := range fields {
			v := *at(home + abi.VirtualFields + k)
			fix(&v)
			o.vals = append(o.vals, v)
		}
		return o
	}
	for i := range f.locals {
		fix(&f.locals[i])
	}
	end := min(len(r.stack), f.base+max(f.cl.fn.MaxStack, e.loopSlots, e.ssaHomes))
	for i := f.base; i < end; i++ {
		fix(&r.stack[i])
	}
	for _, m := range objs {
		for k := range abi.VirtualFields + m.fields {
			*at(m.home + k) = Undefined
		}
	}
	r.jit.materialized += uint64(len(objs))
}
