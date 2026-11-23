package vm

import (
	"unsafe"
)

// Generators' allocation, and the cursor of a loop over one: genAlloc
// makes a generator and its frame's values in one allocation where they
// are few, and a generator of a function whose generators loops step with
// room for the loop's cursor too (genCursor), one allocation where there
// had been two.
//
// Its functions are variables, each assigned a closure in init, laid
// out after every other closure of the package, so that no hot code
// moves for them (see AGENTS.md).

var (

	// genAlloc makes a generator's object and its generator with room for
	// n arguments and locals, in the one allocation where there are few,
	// as kind says: an async function's with its promise, and, for
	// genLoopKind, with room for the cursor of the first loop over it
	// (genCursor) -- a generator of a function whose generators loops
	// step is made with its loop's cursor, in one allocation where there
	// had been two. The generator's room is set where it has it.
	genAlloc func(n int, kind int) (*generatorObject, []Value)

	// genCursor is the cursor of a loop over o, a generator whose next,
	// the realm's, is next: the one in its allocation, where it has room
	// for one no loop has taken, and otherwise one of its own -- which
	// teaches its function to make its generators with the room, unless
	// it is an async function's, which no loop steps.
	genCursor func(r *Runtime, o *Object, next *Object) Value
)

func init() {
	genAlloc = func(n int, kind int) (*generatorObject, []Value) {
		switch kind {
		case genLoopKind:
			var gl *generatorLoop
			var vals []Value
			switch {
			case n <= 2:
				o := &generatorLoopVals[[2]Value]{}
				gl, vals = &o.generatorLoop, o.vals[:n]
			case n <= 4:
				o := &generatorLoopVals[[4]Value]{}
				gl, vals = &o.generatorLoop, o.vals[:n]
			case n <= 6:
				o := &generatorLoopVals[[6]Value]{}
				gl, vals = &o.generatorLoop, o.vals[:n]
			case n <= 8:
				o := &generatorLoopVals[[8]Value]{}
				gl, vals = &o.generatorLoop, o.vals[:n]
			default:
				gl, vals = &generatorLoop{}, make([]Value, n)
			}
			gl.g.room = true
			return &gl.generatorObject, vals
		case genAsyncKind:
			switch {
			case n <= 2:
				o := &asyncVals[[2]Value]{}
				return &o.generatorObject, o.vals[:n]
			case n <= 4:
				o := &asyncVals[[4]Value]{}
				return &o.generatorObject, o.vals[:n]
			case n <= 6:
				o := &asyncVals[[6]Value]{}
				return &o.generatorObject, o.vals[:n]
			case n <= 8:
				o := &asyncVals[[8]Value]{}
				return &o.generatorObject, o.vals[:n]
			}
			return &(&asyncObject{}).generatorObject, make([]Value, n)
		}
		switch {
		case n <= 2:
			o := &generatorVals[[2]Value]{}
			return &o.generatorObject, o.vals[:n]
		case n <= 4:
			o := &generatorVals[[4]Value]{}
			return &o.generatorObject, o.vals[:n]
		case n <= 6:
			o := &generatorVals[[6]Value]{}
			return &o.generatorObject, o.vals[:n]
		case n <= 8:
			o := &generatorVals[[8]Value]{}
			return &o.generatorObject, o.vals[:n]
		}
		return &generatorObject{}, make([]Value, n)
	}

	genCursor = func(r *Runtime, o *Object, next *Object) Value {
		g := genOf(o)
		if g.room {
			g.room = false
			io := &(*generatorLoop)(unsafe.Pointer(o)).cur
			*io = iterObject{Object: Object{class: ClassCursor}, st: iterState{iter: Obj(o), next: Obj(next)}}
			return Obj(&io.Object)
		}
		if !g.async {
			g.cl.genLoops = true
		}
		return r.newIterObject(iterState{iter: Obj(o), next: Obj(next)})
	}

}
