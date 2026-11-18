//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
)

// A loop's step over a generator, natively.
//
// A for...of's or a destructuring's step over a generator is, in native
// code, a native call of the generator's code (ssa.CallSite's Gens), as
// V8's optimized code calls a generator's resumption: the step checks the
// cursor's iterator is a generator of a function it has met, suspended,
// whose next is the realm's, and jumps to its code's generator entry
// (mir's genEntry), which makes the generator's frame -- its locals the
// generator's own, its operands past the VM's stack's top, what its
// suspension saved on them and the value sent in -- in the next context,
// as a native callee's, and goes on at the entry for the generator's place.
// A yield there writes the frame, saves the generator's state as Go's
// suspension would -- its place, the operands beneath the value, the
// handlers in force -- and returns the value to the step; a return
// finishes the generator and returns the step's done. So a
// loop over a generator steps it with neither an exit nor a call of Go,
// where each step had been a call of Go from native code, which resumed
// the generator's code from Go.
//
// Anything else leaves native code as a native callee does, and Go makes
// the generator's frame from its context, as the step's resumption would
// have (jitRegGenLevel, jitRegGenFill), finishes it -- a yield it suspends
// at is Go's suspension -- and saves what its suspension, or its end,
// leaves (jitRegGenDone), the step's caller going on with the result. A
// generator a step resumes natively is one of these (jitRegGenCallable):
// synchronous, no yield*, whose captured locals none -- its frame's locals
// the generator's alone -- and no call in tail position.

// jitGenStep is a loop's step at pc that has met generators of the
// closures cls (jitRegGenStepSeen).
type jitGenStep struct {
	pc  int32
	cls []*closure
}

// jitGenTargets is how many generators' functions a step resumes
// natively, as a call calls jitCallTargets functions natively.
const jitGenTargets = 2

// jitGenResumesMax bounds the places a generator's code resumes it at
// natively (ssa.Func's GenResumes): its first, past its parameters, and
// after its yields, each a check at every resumption.
const jitGenResumesMax = 16

// GenNext is ssa.GenStepFeedback's: the realm's %GeneratorPrototype%.next,
// an object as long-lived as the realm.
func (fb *jitFeedback) GenNext() uintptr { return uintptr(unsafe.Pointer(fb.r.genNextFn)) }

// jitGenLevel is the generator a native call's context c, a level of
// closure cl that left as kind says, runs, where it is a loop's step's
// resumption of it or a call of next's (mir's genEntry), or nil: no other
// native call runs a generator's code -- a call of a generator function
// makes its object, which Go does (abi.ExitEnter). A call of next leaves
// the context's cursor 0, and its generator 0 or 1 once over (abi.Context's
// GenSent).
func jitGenLevel(c *abi.Context, cl *closure, kind uint64) *generator {
	if c.Live != abi.LiveCall || kind == abi.ExitEnter || cl == nil || !cl.fn.Generator || c.GenObject <= 1 {
		return nil
	}
	// Addresses native code wrote, which the step's frame keeps alive.
	if g := *(**generator)(unsafe.Pointer(&c.GenObject)); g.cl == cl {
		return g
	}
	return nil
}

// jitGenCursor is the cursor of the step whose resumption of a generator
// context c runs (jitGenLevel), or nil for a call of next's.
func jitGenCursor(c *abi.Context) *iterState {
	if c.GenCursor == 0 {
		return nil
	}
	return *(**iterState)(unsafe.Pointer(&c.GenCursor))
}

// jitPushGenNext pushes the frame of the realm's generators' next a step's
// or a call's native resumption of g calls -- the step's cursor's next,
// the call's function, which each checked is it -- as stepIter makes it,
// below the generator's own, for a stack trace to show.
func (r *Runtime) jitPushGenNext(g *generator) {
	f := r.pushFrame()
	gn := r.genNextFn
	f.cl = nil
	f.native = gn.fn().name
	f.this, f.newTarget, f.callee, f.args = Obj(g.object()), Undefined, gn, nil
	f.handlers, f.openUpvalues = f.handlers[:0], f.openUpvalues[:0]
	f.thisRef, f.withScopes, f.evalVars, f.savedSP = nil, nil, nil, 0
}

// jitGenMake is a call of a generator function at pc of an entry's code
// that native code makes the generator of (ssa.CallSite's GenMake): the
// function's object and the pool of its generators, made as a call of it
// makes one with no arguments, the call's own written to them.
type jitGenMake struct {
	pc   int32
	obj  *Object
	pool *abi.ObjectPool
}

// jitGenMakePool is the pool of e's call at pc of the generator function
// o, made and filled if it has none, or nil where native code no longer
// makes them: the function's prototype is not what it was made with.
func (r *Runtime) jitGenMakePool(e *jitEntry, pc int, o *Object) *abi.ObjectPool {
	for i := range e.genMakes {
		x := &e.genMakes[i]
		if int(x.pc) != pc || x.obj != o {
			continue
		}
		if x.pool == nil {
			x.pool = &abi.ObjectPool{Source: unsafe.Pointer(&jitPoolSource{gen: o})}
			r.jitFillGens(x.pool)
		}
		if x.pool.Count == 0 {
			return nil
		}
		return x.pool
	}
	return nil
}

// jitFillGens fills pool, of a call of a generator function (jitGenMake),
// with Size generators made as a call of it with no arguments makes them
// (newGenerator), each its own -- its locals undefined, its receiver
// undefined -- for native code to write the call's receiver and arguments
// to; none where the function's prototype is not its own object, or no
// generator is made so.
func (r *Runtime) jitFillGens(pool *abi.ObjectPool) {
	src := (*jitPoolSource)(pool.Source)
	clear(pool.Objects[:])
	pool.Count, pool.Proto = 0, nil
	if pool.Size == 0 {
		pool.Size = abi.PoolSize
	}
	o := src.gen
	p := o.getOwn(atomPrototype)
	if !p.ok() || p.flags()&(propAccessor|propPrivate|propDeleted) != 0 || !p.value().IsObject() || r.stopped != nil {
		return
	}
	fd := o.fn()
	for i := range int(pool.Size) {
		g, err := r.newGenerator(fd.closure, Undefined, nil, o, false, Undefined)
		if err != nil {
			clear(pool.Objects[:i])
			return
		}
		pool.Objects[i] = unsafe.Pointer(g)
	}
	pool.Count, pool.Proto = pool.Size, unsafe.Pointer(p.value().Object())
}

// jitGenResult is the pool of result objects, { value, done }, a call of
// next at pc of an entry's code takes its results from (ssa.CallSite's
// Pool on a call of next).
type jitGenResult struct {
	pc   int32
	pool *abi.ObjectPool
}

// jitGenResultPool is e's pool of the call of next at pc, made and filled
// if it has none.
func (r *Runtime) jitGenResultPool(e *jitEntry, pc int) *abi.ObjectPool {
	for _, x := range e.genResults {
		if int(x.pc) == pc {
			return x.pool
		}
	}
	pool := &abi.ObjectPool{Source: unsafe.Pointer(&jitPoolSource{results: true})}
	r.jitFillResults(pool)
	e.genResults = append(e.genResults, jitGenResult{pc: int32(pc), pool: pool})
	return pool
}

// jitFillResults fills pool, a call of next's (jitGenResultPool), with
// Size result objects as iterResult makes them -- the realm's
// Object.prototype their prototype, its two properties' shape, a value
// and a done -- which native code writes the two values of.
func (r *Runtime) jitFillResults(pool *abi.ObjectPool) {
	if pool.Size == 0 {
		pool.Size = abi.PoolSize
	}
	clear(pool.Objects[:])
	for i := range int(pool.Size) {
		pool.Objects[i] = unsafe.Pointer(r.iterResult(Undefined, false))
	}
	pool.Count, pool.Proto = pool.Size, unsafe.Pointer(r.proto.object)
}
