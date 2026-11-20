//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
)

// Object.create's objects made natively (ssa.CreatePoolFeedback, mir's
// createMake): a call of the realm's Object.create, its leaf's call
// (createleaf.go), takes an object of the prototype it is given from a pool
// of them its site has, as V8's code allocates the object
// CreateObjectWithoutProperties would -- the leaf's call of Go had been a
// fifth of a loop making objects so. The pool's objects are made as the
// leaf makes one (newCreated), of the pool's prototype (its Proto). The
// leaf's call, which native code leaves anything else to, fills the pool
// (jitCreateFill): of another prototype than the pool's, the pool is that
// prototype's, empty; of the pool's, the pool empty, it is filled, with
// more objects each time -- but not at the first such call, whose object,
// the leaf's alone, teaches the prototype's objects their room. A
// prototype made anew at each turn of a loop is given no filling of a
// pool, and a site given another prototype maxCacheFills times gives its
// pool up (jitCreateForm). The pool keeps the last prototype it was given, and what it holds
// of it, while the code is kept, as a construction's pool keeps its
// function's prototype.

// jitCreate is a call of Object.create at register pc pc of a function's
// code whose objects native code makes from pool.
type jitCreate struct {
	pc   int32
	pool *abi.ObjectPool
}

// CreatePool is ssa.CreatePoolFeedback's: the pool of the call at the
// program's instruction pc, where its site's cache holds the realm's
// Object.create, the leaf at fn (Leaf), made if the code has none.
func (f *jitRegFeedback) CreatePool(pc int, fn uintptr) uintptr {
	fb := f.fb
	if fb.e == nil || pc >= len(f.p.Origin) {
		return 0
	}
	callee, cl, ok := f.callTarget(pc)
	if !ok || cl != nil || callee == nil || uintptr(unsafe.Pointer(callee)) != fn {
		return 0
	}
	if fd := callee.fn(); fd == nil || fd.elemOp != elemObjCreate || fd.realm != nil && fd.realm != fb.r.Realm {
		return 0
	}
	pool := jitCreatePool[struct{}](fb.e, int(f.p.Origin[pc]))
	fb.keep().pools = append(fb.keep().pools, pool)
	return uintptr(unsafe.Pointer(pool))
}

// jitCreatePool is e's pool of its call of Object.create at register pc
// rpc, made, empty, if e has none.
func jitCreatePool[_ any](e *jitEntry, rpc int) *abi.ObjectPool {
	for _, x := range e.creates {
		if int(x.pc) == rpc {
			return x.pool
		}
	}
	pool := new(abi.ObjectPool)
	e.creates = append(e.creates, jitCreate{pc: int32(rpc), pool: pool})
	return pool
}

// jitCreateForm has form, the leaf's call of Object.create of a site with
// pool (abi.GoCallState's Give), fill the pool as it makes an object
// (jitCreateFill). A site whose pool is given other prototypes
// maxCacheFills times in a row, each before the pool was ever filled for
// the last, gives up, as a property's cache does: its pool is left empty
// and of no prototype, which native code meets none of, and the leaf is
// called as it was before there were pools -- a loop over objects of
// several prototypes in turn had had the pool given each, filled for
// none, at the cost of a call of Go more at each object. A site whose
// prototype is made anew at each call of its function, and fills the pool
// for it, keeps it.
func jitCreateForm[_ any](form *jitLeafForm, pool *abi.ObjectPool) {
	native := form.native
	var unfilled int
	form.native = func(r *Runtime, this Value, args []Value) (Value, error) {
		v, err := native(r, this, args)
		if err == nil && len(args) != 0 {
			switch jitCreateFill[struct{}](r, pool, args[0]) {
			case createFilled:
				unfilled = 0
			case createRetargeted:
				if unfilled++; unfilled == maxCacheFills {
					clear(pool.Objects[:pool.Count])
					pool.Count, pool.Proto, pool.Size = 0, nil, 0
					form.native = native
				}
			}
		}
		return v, err
	}
}

// What jitCreateFill did to a pool: nothing, filled it, or gave it
// another prototype before it had been filled for its last.
const (
	createKept = iota
	createFilled
	createRetargeted
)

// jitCreateFill fills pool, a site's of Object.create whose leaf has just
// made an object of proto, natively left to it: of another prototype than
// the pool's, the pool is proto's, empty, its count of objects to make
// none, which jitCreateFill reports where the pool had not been filled for
// its last (createRetargeted); of the pool's, run out, it is filled
// with objects made as the leaf makes them, PoolSize at first and twice as
// many as the last time after, up to abi.PoolCapacity -- but not at the
// first call that finds it so. Nothing where a memory meter counts the
// runtime's allocations, whose objects native code does not make.
func jitCreateFill[_ any](r *Runtime, pool *abi.ObjectPool, proto Value) int {
	if !proto.IsObject() || r.meter != nil {
		return createKept
	}
	p := proto.Object()
	if unsafe.Pointer(p) != pool.Proto {
		did := createKept
		if pool.Size <= abi.PoolSize/2 {
			did = createRetargeted
		}
		clear(pool.Objects[:pool.Count])
		pool.Count, pool.Proto, pool.Size = 0, unsafe.Pointer(p), 0
		return did
	}
	if pool.Count != 0 {
		return createKept
	}
	if pool.Size == 0 {
		// The prototype's first object since the pool was given it is the
		// leaf's alone: the room its objects come to need, its root's slack
		// (outgrowProps), is learned of it before any is made for the
		// pool, which had been made with none, every addition to them Go's.
		pool.Size = abi.PoolSize / 2
		return createKept
	}
	pool.Size = min(2*pool.Size, abi.PoolCapacity)
	for i := range int(pool.Size) {
		pool.Objects[i] = unsafe.Pointer(newCreated(p))
	}
	pool.Count = pool.Size
	return createFilled
}
