package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// Loop iteration.
//
// for-in and for-of are driven by the same three instructions -- a start, a
// step and an implicit end -- but their semantics differ enough that the state
// object carries both shapes. for-of speaks the iterator protocol, calling a
// user-visible next method that may run arbitrary code; for-in enumerates
// property keys, and takes a snapshot up front because the specification
// permits an implementation to ignore properties added during the loop.

// iterState is the loop cursor that OpForInStart and OpForOfStart push.
type iterState struct {
	// forIn selects between the snapshot and the protocol.
	forIn bool

	// keys is the snapshot for-in walks.
	keys []Value
	idx  int

	// iter and next drive the for-of protocol.
	iter Value
	next Value

	// arr is set for a plain dense array iterated with the intrinsic array
	// iterator, where walking the elements directly is indistinguishable from
	// the protocol and costs no result object per step.
	arr *Object

	done bool
}

// newIterObject wraps a cursor so that it can live on the operand stack.
func (r *Runtime) newIterObject(st *iterState) Value {
	o := newObject(nil, ClassIterator)
	o.data = st
	return Obj(o)
}

// iterStateOf recovers the cursor from a stack slot.
func iterStateOf(v Value) *iterState {
	if !v.IsObject() {
		return nil
	}
	st, _ := v.Object().data.(*iterState)
	return st
}

// startForIn snapshots the enumerable string keys of a value.
//
// The prototype chain is included, and a key shadowed by a nearer object is
// reported once, at the position where it is first found.
func (r *Runtime) startForIn(v Value) (Value, error) {
	st := &iterState{forIn: true}
	// null and undefined are not an error here: the loop simply runs zero
	// times, which is one of the few places JavaScript is forgiving.
	if v.IsNullish() {
		st.done = true
		return r.newIterObject(st), nil
	}
	o, err := r.toObject(v)
	if err != nil {
		return Undefined, err
	}

	seen := make(map[Atom]bool)
	for cur := o; cur != nil; cur = cur.proto {
		keys, err := r.ownKeysOf(cur, false)
		if err != nil {
			return Undefined, err
		}
		for _, k := range keys {
			if seen[k] {
				continue
			}
			seen[k] = true
			// A non-enumerable property still shadows an enumerable one of the
			// same name further up the chain, which is why it is recorded in
			// seen before being skipped.
			enumerable, err := r.isEnumerable(cur, k)
			if err != nil {
				return Undefined, err
			}
			if !enumerable {
				continue
			}
			st.keys = append(st.keys, Str(NewString(r.atoms.name(k))))
		}
	}
	return r.newIterObject(st), nil
}

// startForOf opens an iterator over a value.
func (r *Runtime) startForOf(v Value) (Value, error) {
	// A plain dense array walked with the intrinsic iterator is stepped
	// directly: the protocol would allocate an iterator and a result object per
	// element, and nothing could tell the difference. The length is read again
	// at each step, so an array that grows or shrinks while it is iterated is
	// seen to, as the real iterator would see it.
	if v.IsObject() {
		o := v.Object()
		if o.class == ClassArray && o.flags&objHasSparseElements == 0 &&
			r.usesIntrinsicArrayIterator(o) {
			return r.newIterObject(&iterState{arr: o}), nil
		}
	}
	method, err := r.getValueProp(v, r.atoms.internSymbol(r.wellKnown.iterator))
	if err != nil {
		return Undefined, err
	}
	if !isCallable(method) {
		return Undefined, r.throwTypeError("%s is not iterable", r.describe(v))
	}
	iter, err := r.call(method, v, nil)
	if err != nil {
		return Undefined, err
	}
	next, err := r.getValueProp(iter, atomNext)
	if err != nil {
		return Undefined, err
	}
	if !isCallable(next) {
		return Undefined, r.throwTypeError("the iterator has no next method")
	}
	return r.newIterObject(&iterState{iter: iter, next: next}), nil
}

// iterNext advances a cursor, reporting whether a value was produced.
func (r *Runtime) iterNext(cursor Value) (Value, bool, error) {
	st := iterStateOf(cursor)
	if st == nil || st.done {
		return Undefined, false, nil
	}

	if st.arr != nil {
		if st.idx >= len(st.arr.elems) {
			st.done = true
			return Undefined, false, nil
		}
		i := st.idx
		st.idx++
		v := st.arr.elems[i]
		if isHole(v) {
			// A hole is read through the prototype chain, which is what the
			// real iterator's Get would do.
			got, err := r.getProp(st.arr, internIndex(uint32(i)), Obj(st.arr))
			return got, err == nil, err
		}
		return v, true, nil
	}

	if st.forIn {
		// A key deleted since the snapshot was taken must be skipped, because
		// the loop body may have removed it.
		for st.idx < len(st.keys) {
			k := st.keys[st.idx]
			st.idx++
			return k, true, nil
		}
		st.done = true
		return Undefined, false, nil
	}

	res, err := r.call(st.next, st.iter, nil)
	if err != nil {
		st.done = true
		return Undefined, false, err
	}
	if !res.IsObject() {
		st.done = true
		return Undefined, false, r.throwTypeError("an iterator result must be an object")
	}
	done, err := r.getValueProp(res, atomDone)
	if err != nil {
		return Undefined, false, err
	}
	if done.Truthy() {
		st.done = true
		return Undefined, false, nil
	}
	val, err := r.getValueProp(res, atomValue)
	if err != nil {
		return Undefined, false, err
	}
	return val, true, nil
}

// closeIter runs an iterator's return method when a loop exits early, which is
// how a generator learns that nothing more will be requested of it.
func (r *Runtime) closeIter(cursor Value) {
	st := iterStateOf(cursor)
	if st == nil || st.forIn || st.done || st.arr != nil {
		// A directly-walked array has no return method to call.
		return
	}
	st.done = true
	r.closeIterator(st.iter)
}

// closeIteratorsIn closes the for-of cursors sitting in a region of the operand
// stack that is about to be discarded.
//
// A cursor lives on the operand stack for as long as its loop is running, so
// the cursors in a discarded region are exactly the for-of loops being left
// abruptly -- by a throw unwinding to a handler, or by a return leaving the
// frame. Closing them there rather than at each exit point means every abrupt
// exit is covered by one rule, including ones the compiler cannot see, and a
// generator being iterated still gets to run its finally blocks.
//
// Closing runs user code, which may itself throw; that error is discarded,
// because the completion that caused the unwind is the one that matters.
func (r *Runtime) closeIteratorsIn(from, to int) {
	for i := to - 1; i >= from; i-- {
		st := iterStateOf(r.stack[i])
		if st == nil || st.forIn || st.done || st.arr != nil {
			continue
		}
		st.done = true
		r.closeIterator(st.iter)
	}
}

// closeIteratorsReturning closes the for-of cursors in a region that a return
// is leaving.
//
// Unlike a throw, a return carries no completion that outranks a failure in the
// iterator's own return method, so the first such failure becomes the result.
func (r *Runtime) closeIteratorsReturning(from, to int) error {
	var first error
	for i := to - 1; i >= from; i-- {
		if err := r.iterCloseNormal(r.stack[i]); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// iterToArray drains an array-destructuring source into a dense array.
//
// Only as many values as the pattern names are pulled, so destructuring an
// infinite generator terminates; the iterator is then closed, because the
// pattern is done with it. A pattern with a rest element asks for all of them,
// and the iterator ends exhausted.
func (r *Runtime) iterToArray(src Value, want uint32) (Value, error) {
	// A plain dense array with the standard iterator is copied directly. This
	// is overwhelmingly the common case, and going through the protocol would
	// allocate an iterator and a result object per element for no observable
	// difference.
	if src.IsObject() && src.Object().IsArray() && r.usesIntrinsicArrayIterator(src.Object()) {
		return src, nil
	}

	cursor, err := r.startForOf(src)
	if err != nil {
		return Undefined, err
	}
	out := newObject(r.proto.array, ClassArray)
	for want == bytecode.IterAll || uint32(len(out.elems)) < want {
		v, ok, err := r.iterNext(cursor)
		if err != nil {
			return Undefined, err
		}
		if !ok {
			break
		}
		out.elems = append(out.elems, v)
	}
	// The pattern has what it needs, so anything still suspended upstream is
	// told to stop.
	r.closeIter(cursor)
	return Obj(out), nil
}

// spreadInto appends the elements of an iterable to an array.
func (r *Runtime) spreadInto(arr *Object, src Value) error {
	// A plain dense array is copied directly, skipping the protocol entirely.
	// This is by far the common case and avoids allocating an iterator and a
	// result object per element.
	if els, ok := r.plainDenseElems(src); ok {
		arr.elems = append(arr.elems, els...)
		return nil
	}
	return r.iterate(src, func(v Value) error {
		arr.elems = append(arr.elems, v)
		return nil
	})
}

// plainDenseElems returns an array's elements when reading them directly is
// indistinguishable from walking it with the iteration protocol.
//
// Three things rule that out: an own Symbol.iterator, which would mean skipping
// user code; elements that have moved into the property table, which freezing
// and defineProperty both do; and a hole, which the protocol would look up the
// prototype chain for rather than report as undefined.
func (r *Runtime) plainDenseElems(src Value) ([]Value, bool) {
	if !src.IsObject() {
		return nil, false
	}
	o := src.Object()
	if !o.IsArray() || o.flags&objHasSparseElements != 0 || !r.usesIntrinsicArrayIterator(o) {
		return nil, false
	}
	for _, el := range o.elems {
		if isHole(el) {
			return nil, false
		}
	}
	return o.elems, true
}

// usesIntrinsicArrayIterator reports whether an array still iterates the way
// Array.prototype does.
//
// Anything else -- an own Symbol.iterator, a replaced one on the prototype, or
// none at all, which `delete Array.prototype[Symbol.iterator]` leaves -- means
// the fast path would answer differently from the protocol. The last case is
// the one that matters most: without an iterator, destructuring an array is a
// TypeError, and a fast path that skipped the lookup would quietly succeed.
func (r *Runtime) usesIntrinsicArrayIterator(o *Object) bool {
	key := r.atoms.internSymbol(r.wellKnown.iterator)
	for cur := o; cur != nil; cur = cur.proto {
		if p := cur.getOwn(key); p != nil {
			return !p.isAccessor() && p.value.IsObject() &&
				p.value.Object() == r.arrayValuesFn
		}
	}
	return false
}

// spreadToStack collects an iterable's elements for a call's argument list.
func (r *Runtime) spreadToStack(src Value) ([]Value, error) {
	var out []Value
	if els, ok := r.plainDenseElems(src); ok {
		return append(out, els...), nil
	}
	err := r.iterate(src, func(v Value) error {
		out = append(out, v)
		return nil
	})
	return out, err
}

// copyDataProps implements object spread, copying own enumerable properties.
//
// It reads through getters rather than copying descriptors, which is what makes
// {...obj} produce plain data properties even when the source has accessors.
func (r *Runtime) copyDataProps(target *Object, src Value) error {
	// A string spreads as its indexed characters.
	if src.IsString() {
		s := src.String()
		for i := 0; i < s.Len(); i++ {
			if err := r.defineOwnProp(target, internIndex(uint32(i)),
				Str(s.Substring(i, i+1)), propDefault); err != nil {
				return err
			}
		}
		return nil
	}
	if !src.IsObject() {
		// A number or boolean has no own enumerable properties, so spreading
		// one contributes nothing rather than failing.
		return nil
	}
	o := src.Object()
	keys, err := r.ownKeysOf(o, true)
	if err != nil {
		return err
	}
	for _, k := range keys {
		enumerable, err := r.isEnumerable(o, k)
		if err != nil {
			return err
		}
		if !enumerable {
			continue
		}
		v, err := r.getProp(o, k, src)
		if err != nil {
			return err
		}
		if err := r.defineOwnProp(target, k, v, propDefault); err != nil {
			return err
		}
	}
	return nil
}

// objectRest builds the rest object of an object destructuring pattern: every
// own enumerable property of the source except those already bound.
func (r *Runtime) objectRest(src Value, excluded []Value) (Value, error) {
	out := newObject(r.proto.object, ClassObject)
	if src.IsNullish() {
		return Obj(out), nil
	}
	o, err := r.toObject(src)
	if err != nil {
		return Undefined, err
	}

	skip := make(map[Atom]bool, len(excluded))
	for _, e := range excluded {
		k, err := r.toPropertyKey(e)
		if err != nil {
			return Undefined, err
		}
		skip[k] = true
	}

	keys, err := r.ownKeysOf(o, true)
	if err != nil {
		return Undefined, err
	}
	for _, k := range keys {
		if skip[k] {
			continue
		}
		enumerable, err := r.isEnumerable(o, k)
		if err != nil {
			return Undefined, err
		}
		if !enumerable {
			continue
		}
		v, err := r.getProp(o, k, src)
		if err != nil {
			return Undefined, err
		}
		if err := r.defineOwnProp(out, k, v, propDefault); err != nil {
			return Undefined, err
		}
	}
	return Obj(out), nil
}

// argumentsData holds what a mapped arguments object aliases.
//
// mapped[i] is the parameter the index i was passed to, or nil where there is
// none: an argument beyond the declared parameters, one whose parameter is
// shadowed by a later one of the same name, or an index the program has since
// deleted or redefined.
type argumentsData struct {
	mapped []*upvalue
}

// newArgumentsObject materializes the arguments object for a call.
//
// In sloppy mode with a simple parameter list the object is mapped: its indices
// alias the parameters they were passed to, so that writing one is visible
// through the other. Strict mode and anything more elaborate than plain
// parameters get the unmapped form, where the elements are a snapshot.
//
// The aliases are the ordinary captured-binding cells, so they keep working
// after the call returns -- which they must, since the object can outlive it.
func (r *Runtime) newArgumentsObject(f *frame) *Object {
	o := newObject(r.proto.object, ClassArguments)
	o.elems = append(o.elems, f.args...)
	o.setOwnRaw(atomLength, Int(len(f.args)), propWritable|propConfigurable)
	mapped := f.cl != nil && f.cl.fn.MappedArguments
	if mapped {
		r.mapArguments(o, f)
	}
	switch {
	case !mapped:
		// An unmapped arguments object refuses to say what called it: the
		// property is there, and reading or writing it throws.
		r.defineAccessor(o, atomCallee, r.throwTypeErrorFn, r.throwTypeErrorFn, 0)
	case f.callee != nil:
		o.setOwnRaw(atomCallee, Obj(f.callee), propWritable|propConfigurable)
	}
	// An arguments object is iterable, using the same iterator as an array.
	o.setOwnRaw(r.atoms.internSymbol(r.wellKnown.iterator),
		Obj(r.newNativeFunc("[Symbol.iterator]", 0,
			func(rt *Runtime, this Value, args []Value) (Value, error) {
				return rt.newArrayIterator(this)
			})), propWritable|propConfigurable)
	return o
}

// mapArguments aliases each index to the parameter it was passed to.
//
// Two parameters may share a name in a sloppy simple list, and only the last of
// them is the one the name resolves to -- so only that one is mapped, and the
// earlier index is left alone.
func (r *Runtime) mapArguments(o *Object, f *frame) {
	fn := f.cl.fn
	n := fn.ParamCount
	if n > len(f.args) {
		n = len(f.args)
	}
	if n <= 0 {
		return
	}
	mapped := make([]*upvalue, n)
	seen := make(map[string]bool, n)
	for i := n - 1; i >= 0; i-- {
		name := ""
		if i < len(fn.Locals) {
			name = fn.Locals[i].Name
		}
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		mapped[i] = r.captureLocal(f, i)
	}
	o.data = &argumentsData{mapped: mapped}
	o.flags |= objMappedArguments
}

// completionKind says how a protected block finished, which a finally clause
// must reproduce after it runs.
type completionKind uint8

const (
	completionNormal completionKind = iota
	completionThrow
	completionReturn
)

// startForAwaitOf opens an async iterator over a value.
//
// Symbol.asyncIterator is preferred, but a plain iterable is accepted too: its
// values are awaited individually, which is what makes `for await` work over an
// array of promises.
func (r *Runtime) startForAwaitOf(v Value) (Value, error) {
	method, err := r.getValueProp(v, r.atoms.internSymbol(r.wellKnown.asyncIterator))
	if err != nil {
		return Undefined, err
	}
	// Only an absent Symbol.asyncIterator falls back to the synchronous
	// protocol. One that is present but not callable is a mistake rather than
	// an absence, and saying so here is what stops the sync iterator from
	// being called for an object that declared itself async.
	if method.IsNullish() {
		return r.startForOf(v)
	}
	if !isCallable(method) {
		return Undefined, r.throwTypeError("Symbol.asyncIterator is not a function")
	}
	iter, err := r.call(method, v, nil)
	if err != nil {
		return Undefined, err
	}
	next, err := r.getValueProp(iter, atomNext)
	if err != nil {
		return Undefined, err
	}
	if !isCallable(next) {
		return Undefined, r.throwTypeError("the async iterator has no next method")
	}
	return r.newIterObject(&iterState{iter: iter, next: next}), nil
}

// iterSend calls a cursor's next method with a value, which is what `yield*`
// needs: the value its own caller sent in has to reach the delegate, or a
// generator delegating to another cannot be driven at all.
//
// The raw iterator result is returned rather than an unpacked value, because
// the caller needs the done flag and the returned value separately.
func (r *Runtime) iterSend(cursor Value, sent Value, async bool) (Value, error) {
	st := iterStateOf(cursor)
	if st == nil {
		return Undefined, r.throwTypeError("not an iterator")
	}
	if st.forIn {
		return Undefined, r.throwTypeError("cannot delegate to a property enumeration")
	}
	if st.arr != nil {
		// A directly-walked array has no next to send to, and nothing to do
		// with the value that would have been sent.
		v, ok, err := r.iterNext(cursor)
		if err != nil {
			return Undefined, err
		}
		res := Obj(r.iterResult(v, !ok))
		if !async {
			return res, nil
		}
		return r.awaitIterResult(st, res)
	}
	res, err := r.call(st.next, st.iter, []Value{sent})
	if err != nil {
		return Undefined, err
	}
	if !async {
		return res, nil
	}
	// An async iterator hands back a promise for the whole result; a
	// synchronous one hands back a plain result whose value still has to be
	// awaited, so it is wrapped in a promise the same way.
	if res.IsObject() && res.Object().class == ClassPromise {
		return res, nil
	}
	return Obj(r.toPromise(res)), nil
}

// asyncIterNext calls an async iterator's next method, returning the promise it
// produces. A synchronous iterator is wrapped so that both protocols can be
// driven by the same instruction sequence.
func (r *Runtime) asyncIterNext(cursor Value) (Value, error) {
	st := iterStateOf(cursor)
	if st == nil {
		return Undefined, r.throwTypeError("not an iterator")
	}
	if st.forIn {
		return Undefined, r.throwTypeError("cannot iterate properties asynchronously")
	}
	if st.arr != nil {
		// A directly-walked array has no next to call, so the plain result
		// object the rest of this needs is built here.
		v, ok, err := r.iterNext(cursor)
		if err != nil {
			return Undefined, err
		}
		return r.awaitIterResult(st, Obj(r.iterResult(v, !ok)))
	}
	res, err := r.call(st.next, st.iter, nil)
	if err != nil {
		return Undefined, err
	}
	// An async iterator already returns a promise for the whole result.
	if res.IsObject() && res.Object().class == ClassPromise {
		return res, nil
	}

	return r.awaitIterResult(st, res)
}

// awaitIterResult turns a synchronous iterator's result into the promise a
// for-await loop expects.
//
// A synchronous iterator returns a plain result object, whose value must be
// awaited individually: `for await (const v of [1, promise])` yields the
// promise's value, not the promise. So the result is rebuilt around the settled
// value rather than merely wrapped.
func (r *Runtime) awaitIterResult(st *iterState, res Value) (Value, error) {
	done, err := r.getValueProp(res, atomDone)
	if err != nil {
		return Undefined, err
	}
	value, err := r.getValueProp(res, atomValue)
	if err != nil {
		return Undefined, err
	}
	isDone := done.Truthy()

	out := r.newPromise()
	onFulfilled := r.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, error) {
		rt.resolvePromise(out, Obj(rt.iterResult(arg(a, 0), isDone)))
		return Undefined, nil
	})
	onRejected := r.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, error) {
		// A synchronous iterator that produced a rejected promise is done
		// with: the loop will never ask it for another value, so it is closed
		// here rather than left for the unwinding to reach. That is what makes
		// a generator's finally run before the rejection is delivered. The
		// cursor is marked done so that the unwinding does not close it again.
		if !isDone {
			st.done = true
			rt.closeIterator(st.iter)
		}
		rt.rejectPromise(out, arg(a, 0))
		return Undefined, nil
	})
	r.promiseThen(r.toPromise(value), Obj(onFulfilled), Obj(onRejected))
	return Obj(out), nil
}

// iterStep advances a cursor for a destructuring pattern, reporting undefined
// once the iterator is exhausted.
//
// A pattern asks for as many values as it names, and a source with fewer simply
// leaves the rest undefined -- which is why this reports a value rather than
// whether there was one.
func (r *Runtime) iterStep(cursor Value) (Value, error) {
	v, ok, err := r.iterNext(cursor)
	if err != nil {
		return Undefined, err
	}
	if !ok {
		return Undefined, nil
	}
	return v, nil
}

// iterRest drains what is left of a cursor into a dense array, which is what a
// pattern's rest element binds.
func (r *Runtime) iterRest(cursor Value) (Value, error) {
	out := newObject(r.proto.array, ClassArray)
	for {
		v, ok, err := r.iterNext(cursor)
		if err != nil {
			return Undefined, err
		}
		if !ok {
			return Obj(out), nil
		}
		out.elems = append(out.elems, v)
	}
}

// iterCloseNormal closes a destructuring pattern's cursor once the pattern is
// done with it.
//
// A pattern that stopped short of the end tells the iterator so, and unlike a
// close during an abrupt completion this one has nothing else in flight: a
// failure in the return method is the result. So is a return method that hands
// back something other than an object, which is the one place the protocol
// checks that.
func (r *Runtime) iterCloseNormal(cursor Value) error {
	st := iterStateOf(cursor)
	if st == nil || st.forIn || st.done || st.arr != nil {
		return nil
	}
	st.done = true
	ret, err := r.getValueProp(st.iter, atomReturn)
	if err != nil {
		return err
	}
	// An iterator without a return method simply ends; one whose return is
	// neither absent nor callable is a TypeError, which is what looking a
	// method up means.
	if ret.IsUndefined() || ret.IsNull() {
		return nil
	}
	if !isCallable(ret) {
		return r.throwTypeError("the iterator's return method is not callable")
	}
	res, err := r.call(ret, st.iter, nil)
	if err != nil {
		return err
	}
	if !res.IsObject() {
		return r.throwTypeError("the iterator's return method must return an object")
	}
	return nil
}

// iterResume drives one step of a `yield*`.
//
// How the outer generator was resumed decides which of the delegate's methods
// is called: next for an ordinary resumption, throw for an injected exception,
// return for a forced return. A delegate that has no throw is closed and the
// delegation fails, since there is no way to deliver the exception; one that
// has no return simply ends, which is what lets `yield*` work over an iterator
// that never expected to be abandoned.
//
// The bool reports that the delegation ended with a return the delegate could
// not take, which the outer generator turns into a return of its own.
func (r *Runtime) iterResume(cursor Value, sent Value, mode resumeMode,
	async bool) (Value, bool, error) {
	st := iterStateOf(cursor)
	if st == nil {
		return Undefined, false, r.throwTypeError("not an iterator")
	}
	switch mode {
	case resumeThrow:
		if st.arr != nil {
			// A directly-walked array has no throw, so the delegation fails
			// with the exception it was given.
			st.done = true
			return Undefined, false, r.throw(sent)
		}
		method, err := r.getValueProp(st.iter, r.atoms.intern("throw"))
		if err != nil {
			return Undefined, false, err
		}
		if !isCallable(method) {
			// The delegate is told the delegation is over before the failure
			// is reported, which is what gives a generator its finally -- and
			// a failure there is what the delegation fails with, since the
			// TypeError has not been raised yet.
			st.done = true
			if err := r.closeIteratorErr(st.iter); err != nil {
				return Undefined, false, err
			}
			return Undefined, false, r.throwTypeError(
				"the delegate has no throw method")
		}
		res, err := r.call(method, st.iter, []Value{sent})
		if err != nil {
			return Undefined, false, err
		}
		return r.delegateResult(st, res, async)

	case resumeReturn:
		if st.arr != nil {
			return sent, true, nil
		}
		method, err := r.getValueProp(st.iter, atomReturn)
		if err != nil {
			return Undefined, false, err
		}
		if !isCallable(method) {
			return sent, true, nil
		}
		res, err := r.call(method, st.iter, []Value{sent})
		if err != nil {
			return Undefined, false, err
		}
		out, _, err := r.delegateResult(st, res, async)
		if err != nil {
			return Undefined, false, err
		}
		if async {
			// The result is a promise; whether it says done is only known once
			// it settles, so the instruction that reads it afterwards decides,
			// which is why it is told what kind of resumption this was.
			return out, false, nil
		}
		if !out.IsObject() {
			return Undefined, false, r.throwTypeError(
				"an iterator result must be an object")
		}
		done, err := r.getValueProp(out, atomDone)
		if err != nil {
			return Undefined, false, err
		}
		if done.Truthy() {
			v, err := r.getValueProp(out, atomValue)
			return v, true, err
		}
		return out, false, nil
	}
	res, err := r.iterSend(cursor, sent, async)
	return res, false, err
}

// delegateResult checks what a delegate's method handed back.
func (r *Runtime) delegateResult(st *iterState, res Value, async bool) (Value, bool, error) {
	if async {
		if res.IsObject() && res.Object().class == ClassPromise {
			return res, false, nil
		}
		out, err := r.awaitIterResult(st, res)
		return out, false, err
	}
	if !res.IsObject() {
		return Undefined, false, r.throwTypeError("an iterator result must be an object")
	}
	return res, false, nil
}
