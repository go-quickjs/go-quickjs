//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// A rest parameter's array and an unmapped arguments object made by native
// code (ssa.CallSite's Made, mir's frameMade), as V8's code allocates them
// inline from its frame: where the code runs as a native callee, its call
// wrote its arguments to its context, and native code takes the object
// from a pool of the site's, filled here, and writes the arguments into it
// and its length. The frame Go entered the code with, and a mapped
// arguments object, whose callee Go finds from the frame, are made by Go
// on the frame (jitRegFrameOp) as before.

var _ ssa.FrameMadeFeedback = (*jitRegFeedback)(nil)

// jitMadePool is a site's pool of rest parameters' arrays or of arguments
// objects, by its register pc (jitEntry's madePools).
type jitMadePool struct {
	pc   int32
	pool *abi.ObjectPool
}

// jitMadeRoom is the first room of a site's objects: a rest parameter's
// arrays', as `new Array(n)`'s small ones, or arguments objects', one of
// the rooms their allocation has (newArgumentsStorage).
const jitMadeRoom = 4

// FrameMade is ssa.FrameMadeFeedback's: the rest parameter's array or the
// arguments object at the program's instruction pc, made natively from a
// pool of the site's, where the function's own code keeps its arguments
// (jitFrameArgs) -- not a generator's or an async function's, whose
// steps' contexts hold no call's arguments, nor a mapped arguments
// object, whose callee native code does not have.
func (f *jitRegFeedback) FrameMade(pc int) (ssa.FrameMadeSite, bool) {
	w, op, ok := f.at(pc)
	if !ok || op != ir.Host {
		return ssa.FrameMadeSite{}, false
	}
	return f.frameMadeAt(int(f.p.Origin[pc]), w)
}

// frameMadeAt is FrameMade's answer at register pc rpc, of its
// instruction w, asked of the register code -- as a snapshot's gather asks
// it before the function is lowered (jit_snapframemade.go) -- not of the
// program.
func (f *jitRegFeedback) frameMadeAt(rpc int, w bytecode.RInstr) (ssa.FrameMadeSite, bool) {
	fb := f.fb
	if fb.e == nil || fb.root != nil || fb.cl == nil || fb.cl.realm == nil {
		return ssa.FrameMadeSite{}, false
	}
	fn := fb.fn
	if fn.Generator || fn.Async || !jitFrameArgs(fn) {
		return ssa.FrameMadeSite{}, false
	}
	var made ssa.FrameMade
	from := 0
	switch w.Op() {
	case bytecode.RRestParam:
		made, from = ssa.MadeRest, int(w.B())
	case bytecode.RArguments:
		if fn.MappedArguments {
			return ssa.FrameMadeSite{}, false
		}
		made = ssa.MadeArguments
	default:
		return ssa.FrameMadeSite{}, false
	}
	var pool *abi.ObjectPool
	for i := range fb.e.madePools {
		if fb.e.madePools[i].pc == int32(rpc) {
			pool = fb.e.madePools[i].pool
		}
	}
	if pool == nil {
		pool = &abi.ObjectPool{Size: abi.PoolSize, Room: jitMadeRoom}
		pool.Source = unsafe.Pointer(&jitPoolSource{made: made, realm: fb.cl.realm})
		fb.r.jitFillMade(pool)
		fb.e.madePools = append(fb.e.madePools, jitMadePool{pc: int32(rpc), pool: pool})
	}
	fb.keep().pools = append(fb.keep().pools, pool)
	return ssa.FrameMadeSite{Pool: uintptr(unsafe.Pointer(pool)), Made: made, From: from}, true
}

// jitRefillMade fills pool, a site's of rest parameters' arrays or of
// arguments objects, src its source, again: with objects of room for one
// less than its Want where native code met more arguments than its room,
// or with more where it ran out. It reports whether it has one now: past
// the most an arguments object's allocation holds, none, and the site is
// Go's.
func (r *Runtime) jitRefillMade(pool *abi.ObjectPool, src *jitPoolSource) bool {
	if want := pool.Want; want != 0 {
		pool.Want = 0
		room := jitMadeRoomOf(src.made, int(want-1))
		if room == 0 || uint64(room) <= pool.Room {
			return false
		}
		pool.Room = uint64(room)
	} else if pool.Count != 0 {
		return true
	} else {
		pool.Size = min(2*max(pool.Size, abi.PoolSize), abi.PoolCapacity)
	}
	r.jitFillMade(pool)
	return pool.Count != 0
}

// jitMadeRoomOf is the room a site's objects, of what made says, take to
// hold n elements: a rest parameter's array that of `new Array(n)`'s
// (jitArrayRoom), an arguments object one of its allocation's
// (newArgumentsStorage), 0 past them.
func jitMadeRoomOf(made ssa.FrameMade, n int) int {
	if made == ssa.MadeRest {
		return jitArrayRoom(float64(n))
	}
	for _, room := range [...]int{2, 4, 7} {
		if n <= room {
			return room
		}
	}
	return 0
}

// jitFillMade fills pool with Size objects of its source's realm: a rest
// parameter's arrays, of the realm's Array.prototype, as `new Array(n)`'s
// are made (jitFillPool), or unmapped arguments objects, each with room
// for Room elements, its properties those every unmapped arguments object
// of the realm has (Realm's argsLayouts), its length 0 until native code
// writes it.
func (r *Runtime) jitFillMade(pool *abi.ObjectPool) {
	src := (*jitPoolSource)(pool.Source)
	clear(pool.Objects[:])
	pool.Count, pool.Proto = 0, nil
	realm, room, n := src.realm, int(pool.Room), int(pool.Size)
	if src.made == ssa.MadeRest {
		for i := range n {
			a := newArrayObject(realm.proto.array, room)
			e := a.elemsOf()
			for j := range e {
				e[j] = elemHole
			}
			a.extOf().elems = e[:0]
			r.ensureShape(a)
			pool.Objects[i] = unsafe.Pointer(a)
		}
		pool.Count, pool.Proto = uint64(n), unsafe.Pointer(realm.proto.array)
		return
	}
	if t := realm.argsLayouts[0]; t == nil || t.shape.proto != realm.proto.object {
		// The realm's first, whose layout the rest copy.
		prev := r.Realm
		r.Realm = realm
		r.newArgumentsObject(&frame{})
		r.Realm = prev
	}
	t := realm.argsLayouts[0]
	if t == nil || t.shape.proto != realm.proto.object || room > 7 {
		return
	}
	for i := range n {
		o := newArgumentsStorage(realm.proto.object, room)
		o.setVals(append(o.vals(), t.vals[:]...))
		o.shape = t.shape
		o.vals()[0] = Int(0)
		pool.Objects[i] = unsafe.Pointer(o)
	}
	pool.Count, pool.Proto = uint64(n), unsafe.Pointer(realm.proto.object)
}
