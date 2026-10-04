package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// leafCall answers a call of a function whose body bytecode.LeafKind names,
// with o as its `this`, without a frame. It does so only where nothing the
// body does could call code or throw -- its reads are of plain data
// properties, of an array's dense elements and of an array's or a string's
// length, and its writes are the ones the sites' caches answer -- so that no
// stack trace, caller or interrupt could have seen the frame. For anything
// else it reports false, having changed nothing, and the body is run as
// usual. A body that only writes answers undefined.
func (r *Runtime) leafCall(cl *closure, o *Object, args []Value) (Value, bool) {
	fn := cl.fn
	switch fn.Leaf {
	case bytecode.LeafSetThis:
		return Undefined, r.leafStores(cl, o, args)
	case bytecode.LeafForward, bytecode.LeafPure:
		// A construction is answered by leafForward, and a call of a pure
		// body by pureCall.
		return Undefined, false
	}
	in := fn.Code[1]
	v, ok := plainOwn(o, cl.names[in.A])
	if !ok {
		if v, ok = r.cachedData(&cl.ic[in.B], o, cl.names[in.A]); !ok {
			return Undefined, false
		}
	}
	switch fn.Leaf {
	case bytecode.LeafGetThis:
		return v, true
	case bytecode.LeafGetThisLength:
		if v.IsString() {
			return Int(v.String().Len()), true
		}
		if v.IsObject() && v.Object().class == ClassArray {
			return Uint32(v.Object().arrayLength()), true
		}
	case bytecode.LeafGetThisIndex:
		// As the interpreter's get_index reads an element first thing.
		key := Undefined
		if p := int(fn.Code[2].A); p < len(args) {
			key = args[p]
		}
		if v.IsObject() && key.IsNumber() {
			a := v.Object()
			if i := uint32(key.Number()); float64(i) == key.Number() &&
				uint(i) < uint(len(a.elems)) && a.flags&objMappedArguments == 0 {
				if e := a.elems[i]; !isHole(e) {
					return e, true
				}
			}
		}
	}
	return Undefined, false
}

// leafStores does the writes of a body LeafSetThis marks -- this.k = p, for
// parameters p -- if the sites' caches answer every one of them, and reports
// whether it did. Each is then a write to a plain writable property o has,
// or the addition of one past a prototype chain that cannot intercept it:
// nothing is called, and nothing throws. Every write is checked before any
// is made, against the shape the ones before it leave o with.
func (r *Runtime) leafStores(cl *closure, o *Object, args []Value) bool {
	code := cl.fn.Code
	s := o.shape
	for i := 0; i+2 < len(code) && code[i+2].Op == bytecode.OpSetProp; i += 3 {
		c := &cl.ic[code[i+2].B]
		// An entry for a setter calls it, which is no write of a leaf's.
		if c.shape != s || s == nil || c.getter {
			return false
		}
		if c.next != nil {
			if !c.adds(o) {
				return false
			}
			s = c.next
		}
	}
	for i := 0; i+2 < len(code) && code[i+2].Op == bytecode.OpSetProp; i += 3 {
		in := code[i+2]
		c := &cl.ic[in.B]
		v := Undefined
		if p := int(code[i+1].A); p < len(args) {
			v = args[p]
		}
		if verifyShapes {
			r.verifyStoreCache(c, o, cl.names[in.A])
		}
		if c.next == nil {
			o.props[c.idx].value = v
		} else {
			o.appendTransition(Property{key: cl.names[in.A], flags: c.next.flags, value: v}, c.next)
		}
	}
	return true
}

// leafForward constructs o with a constructor LeafForward marks, whose body
// is this.k.apply(this, arguments), by calling the method itself. The
// constructor's frame is pushed as it would stand at its apply, though the
// body is not run in it, and apply's as applyArguments pushes it, so that a
// stack trace, f.caller and f.arguments find what they would have. It does
// so where this.k and its apply are plain data properties the sites' caches
// answer, apply the built-in one, and k a function; it reports false for
// anything else, having done nothing, and the body is run as usual.
func (r *Runtime) leafForward(cl *closure, callee, o *Object, args []Value, newTarget Value) (bool, error) {
	code := cl.fn.Code
	in := code[1]
	m, ok := plainOwn(o, cl.names[in.A])
	if !ok {
		m, ok = r.cachedData(&cl.ic[in.B], o, cl.names[in.A])
	}
	if !ok || !isCallable(m) {
		return false, nil
	}
	in = code[2]
	if ap, ok := r.cachedData(&cl.ic[in.B], m.Object(), cl.names[in.A]); !ok || !ap.IsObject() || ap.Object() != r.applyFn {
		return false, nil
	}
	if r.frameDepth >= r.maxFrames {
		// run throws the RangeError.
		return false, nil
	}
	f := r.pushFrame()
	f.cl = cl
	f.locals = nil
	f.base = r.stackTop
	// The saved pc is past the instruction a frame is at: apply_arguments.
	f.pc = 5
	f.this = Obj(o)
	f.thisRef = nil
	f.newTarget = newTarget
	f.callee = callee
	f.args = args
	f.paramsOnly = false
	f.openUpvalues = f.openUpvalues[:0]
	f.evalVars = nil
	f.withScopes = nil
	f.handlers = f.handlers[:0]
	f.native = ""
	f.savedSP = 0
	// What the method returns, the body drops.
	_, err := r.applyCall(m, Obj(o), args)
	r.frameDepth--
	return true, err
}
