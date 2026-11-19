package vm

// A for...in's cursor, used again. A loop's cursor is a 160-byte object no
// script can reach, and rendering a page makes thousands of them, a loop
// over each element's props: 15% of the bytes the SSR benchmark's render
// allocates. So the runtime keeps the last cursor it made
// (Runtime.forInLast), and the next loop takes it if its loop has finished.
//
// A cursor's done is set at its last step, by iterNext or native code's
// forInStep, and, in register code, by the iter_close that a break, a
// continue or a return leaving the loop runs on its way out (rhIterClose
// and its native handler): that is the whole of its release. After its
// last step what is left of the loop is iter_close, which leaves a
// for...in's cursor alone, and a drop; after a leaving iter_close, the
// jump out. So nothing reads the cursor again, whenever the next loop
// takes it.
// Every loop's start comes through here, the interpreter's and native code's by
// jitForInPlain, or takes the last cursor as newForInObject does, native
// code's own start (mir's forInStart), so native code needs no release of
// its own.
//
// What that rests on. No script reaches a cursor, so its loop is its only
// reader, and a loop that is running is not done: a nested loop, a generator
// suspended in one, a proxy's trap in the middle of a step and a recursive
// call all find the last cursor not done, and make their own. Unwinding never
// marks a for...in's cursor done (iterClose and its kin pass it by), so
// a loop left and re-entered by a finally's continue keeps its own. Only done
// and idx change in a loop's life: iter, next and arr are a for-of's, so that
// a used cursor needs nothing but those two and what it walks made again.
//
// A cursor made done, of null or undefined, is not kept: it is done before
// its loop's first step, which a statement's debugger pause may come ahead
// of. One whose loop was left by a throw is not done, and is the collector's; the next cursor takes its place.
//
// The cursor holds the object its loop walked, so the runtime lets it go
// each time the collector has run and when its turn ends
// (clearReturnedFrames), as it does a stale slot's value.
//
// The code here is generic, instantiated with struct{}, laid out after
// every function and closure of the package, and newForInObject checks
// the last in place: 32 bytes off it for the 32 clearReturnedFrames gains,
// so that the hot paths, past both, stay where they were.

// forInCursor is newForInObject's new cursor over in, kept unless it is
// made done.
//
//go:noinline
func forInCursor[_ any](r *Runtime, in *forInState, done bool) Value {
	fo := &forInObject{iterObject: iterObject{Object: Object{class: ClassCursor},
		st: iterState{forIn: true, done: done}}, in: *in}
	if !done {
		r.forInLast = fo
	}
	return Obj(&fo.Object)
}

// forInAgain is newForInObject's copy of what a used cursor walks now.
//
//go:noinline
func forInAgain[_ any](fo *forInObject, in *forInState) { fo.in = *in }

// forInLetGo lets the last cursor go, and what it walked: a for...in's
// and an array's (arrCursor); and the last dictionary's keys (dictForIn).
//
//go:noinline
func forInLetGo[_ any](r *Runtime) { r.forInLast, r.ofLast, r.dictKeys = nil, nil, dictKeys{} }

// arrCursor is startForOf's cursor over o, an array iterated by the
// intrinsic iterator: the last one made (Runtime.ofLast), where its loop
// or pattern has closed it, or a new one, kept.
//
// A cursor is released (iterState.released) by iter_close or
// iter_close_throw, the last instruction of its loop or pattern that reads
// it, natively or by Go; a break out of a loop, a pattern's end. No
// script reaches a cursor, so its register is its only reader, and that
// register is read no more once it is closed: what is left of a pattern
// after the close is the jump past its handler, and a loop's close is on
// its way out. A loop that ends by its last step, or by a throw, leaves
// its cursor unreleased, for the collector.
//
//go:noinline
func arrCursor[_ any](r *Runtime, o *Object) Value {
	if io := r.ofLast; io != nil && io.st.released {
		io.st = iterState{arr: o}
		return Obj(&io.Object)
	}
	io := &iterObject{Object: Object{class: ClassCursor}, st: iterState{arr: o}}
	r.ofLast = io
	return Obj(&io.Object)
}

// forInChain is a prototype chain a for...in found no enumerable key on,
// by its first object, while its validity cell is valid.
type forInChain struct {
	proto *Object
	cell  *protoCell
}

// protosNoEnum reports whether nothing on o's prototype chain has an
// enumerable key, as noEnumerableKeys finds of each: then a for...in over
// o walks o's own keys alone. The answer is kept for the chain, in the
// realm, while its validity cell (chainCell) is valid: a key added to an
// object of it, deleted or redefined, or a prototype changed, invalidates
// it. What a cell does not see, an element added to a prototype, is
// looked at each time: the objects' elements, no more than a few words.
//
//go:noinline
func protosNoEnum[_ any](r *Runtime, o *Object) bool {
	p := o.proto()
	if p == nil {
		return true
	}
	re := r.Realm
	for i := range re.forInChains {
		if e := &re.forInChains[i]; e.proto == p && e.cell.valid {
			for cur := p; cur != nil; cur = cur.proto() {
				if len(cur.elemsOf()) != 0 {
					return false
				}
			}
			return true
		}
	}
	for cur := p; cur != nil; cur = cur.proto() {
		if !noEnumerableKeys(cur) {
			return false
		}
	}
	if cell := r.chainCell(p); cell != nil {
		re.forInChains[1] = re.forInChains[0]
		re.forInChains[0] = forInChain{proto: p, cell: cell}
	}
	return true
}

// dictKeys is a dictionary's keys for a for...in, by the layout they are
// of (Runtime.dictKeys).
type dictKeys struct {
	shape *shape
	keys  *forInKeys
}

// dictForIn is the keys a for...in visits on o, a plain object with no
// elements whose layout is its own (a dictionary), with that layout, or
// nil where its table has an index key (tableKeys). They are made once
// for the layout and kept, the last dictionary's (Runtime.dictKeys), the
// layout remembered as a cache remembers one: a layout remembered is
// given a new identity, rather than changed, when a key is added, deleted
// or changes its attributes (layoutChanged), so that while the object
// keeps it its keys are these -- what a loop's steps then hand out
// without looking each up again, as they do a shared layout's
// (forInState.shape). A loose layout, which changes in place whatever
// remembers it, is not one.
//
//go:noinline
func dictForIn[_ any](r *Runtime, o *Object) (*forInKeys, *shape) {
	s := o.shape
	if s == nil || !s.unique || s.loose {
		return nil, nil
	}
	if d := &r.dictKeys; d.shape == s && s.seen {
		return d.keys, s
	}
	ks, ok := r.tableKeys(o)
	if !ok {
		return nil, nil
	}
	r.dictKeys = dictKeys{shape: remember(s), keys: &ks}
	return &ks, s
}
