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
	if fn.Leaf == bytecode.LeafSetThis {
		return Undefined, r.leafStores(cl, o, args)
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
		if c.shape != s || s == nil {
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
