package vm

import "sync/atomic"

// Calls callDirect makes out of line, as generic functions: Go lays out a
// generic function's instantiations after every function and closure of
// the package, where a function or an init's closure written anywhere
// moves the closures, or the functions after it, against the hot ones
// (AGENTS.md). Each is instantiated with struct{} alone, and called where
// the call it stands for was; the caller's code grows by the dictionary's
// argument and its moves, within the padding that rounds the function to
// 32 bytes, so that every function and closure of the package stays at
// its address (go tool nm).

// directRest is runFD's call for callDirect's compiled callee, this and
// newTarget an arrow's already. An arrow, or a function whose frame may
// end in a tail call, is run on the tree planTreeCall has found it may be
// run on (treeRest), as callRest runs one -- but with runTree's recover,
// callTreeWith's not nested: callDirect's caller is a built-in or the
// interpreter's loop, not a tree's node whose recover takes what the tree
// throws. A frame that ends in a tail call has the call made here, as
// tailFromTree makes it for callRest. Anything else is runFD's.
//
// Every callback a built-in calls for each element that is an arrow and
// no pure body -- forEach's, find's, every's, reduce's, map's -- had been
// run by runFD, its frame set up for anything, at every call.
func directRest[_ any](r *Runtime, o *Object, fd *funcData, this Value, args []Value, newTarget Value) (Value, error) {
	cl := fd.closure
	if !fd.treeRest || cl.fn.CoerceThis && !this.IsObject() {
		return r.runFD(cl, this, args, newTarget, o, fd)
	}
	v, err := r.callTreeWith(o, fd, (*tree)(atomic.LoadPointer(&cl.fn.VMCode)), this, args, newTarget, false)
	for err == errTailCall {
		cl, this, args, callee, ok, terr := r.takeTailCall()
		if !ok {
			// Made the ordinary way: this is its result.
			return this, terr
		}
		var cfd *funcData
		if callee != nil {
			cfd = callee.fn()
		}
		var t *tree
		switch {
		case cfd == nil || cl.fn.CoerceThis && !this.IsObject():
		case cfd.treeCall != nil:
			t = cfd.treeCall
		case cfd.treeRest:
			t = (*tree)(atomic.LoadPointer(&cl.fn.VMCode))
		}
		if t == nil {
			return r.runFD(cl, this, args, Undefined, callee, cfd)
		}
		v, err = r.callTreeWith(callee, cfd, t, this, args, Undefined, false)
	}
	return v, err
}
