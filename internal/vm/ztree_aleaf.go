package vm

// Leaves: built-ins every tier calls without a frame, where they call
// nothing and cannot throw (elemOp past elemRandom), as V8's optimized code
// calls fast API functions with no frame of JavaScript's. A built-in
// declares its own where it is made (setElemOp); the interpreter, the tree
// tier and pure bodies apply it before they would make a frame (callDirect,
// callRest, pureInvoke, by pureElemOp and collectionOp), and native code's
// calls of Go do too (jitGoCallPlain), none of them keeping a list of
// functions. Each is applied as the built-in applies it, and reports false
// wherever the built-in would throw or do anything else -- a receiver of
// another kind, a key a weak collection takes none of, an object String
// would convert by its methods -- which the built-in, called the ordinary
// way, is left to do.
//
// The code is closures assigned in this file's init, whose closures are
// laid out after every other closure of the package's (see
// ztree_zkself.go): this file's init comes first.

// leafFunc applies a leaf to this and args, and reports whether it did; a
// store -- an addition or a deletion -- only where mayStore, from a pure
// body that may not store refusing it (pureRefused).
type leafFunc func(r *Runtime, this Value, args []Value, mayStore bool) (Value, bool)

// leafOps are the leaves' functions, by their elemOp: an index and a call,
// where one function asking which op it was had cost a Map's deletion of a
// String's key, with the JIT, 7% more than the two calls' own lists.
var leafOps [elemStringOf + 1]leafFunc

// setElemOp gives the built-in function o the frameless application op
// (elemOp), and notes it among the runtime's leaves (leafFns), which native
// code's calls of Go are asked to be of before anything else of them is
// looked at (jitPlainSet).
var setElemOp func(r *Runtime, o *Object, op uint8)

func init() {
	setElemOp = func(r *Runtime, o *Object, op uint8) {
		o.fn().elemOp = op
		r.leafFns = append(r.leafFns, o)
	}
	leafOps[elemDateNow] = func(r *Runtime, _ Value, _ []Value, _ bool) (Value, bool) {
		return Float(r.now()), true
	}
	leafOps[elemStringOf] = func(r *Runtime, _ Value, args []Value, _ bool) (Value, bool) {
		// As String called converts it, of a primitive: a symbol its
		// description, which only this conversion accepts. A string made is
		// the memory meter's to count, by the ordinary call.
		if len(args) == 0 {
			return Str(emptyString), true
		}
		x := args[0]
		if x.IsObject() || r.meter != nil {
			return Undefined, false
		}
		if x.IsSymbol() {
			return Str(NewString(x.Symbol().String())), true
		}
		s, err := r.toString(x)
		if err != nil {
			return Undefined, false
		}
		return Str(s), true
	}
	leafOps[elemMapDelete] = func(r *Runtime, this Value, args []Value, mayStore bool) (Value, bool) {
		return leafMapDelete(r, this, args, mayStore, ClassMap)
	}
	leafOps[elemSetDelete] = func(r *Runtime, this Value, args []Value, mayStore bool) (Value, bool) {
		return leafMapDelete(r, this, args, mayStore, ClassSet)
	}
	leafOps[elemWeakMapGet] = func(r *Runtime, this Value, args []Value, _ bool) (Value, bool) {
		m := leafWeakMap(this)
		if m == nil {
			return Undefined, false
		}
		v, _ := m.get(r, arg(args, 0))
		return v, true
	}
	leafOps[elemWeakMapHas] = func(r *Runtime, this Value, args []Value, _ bool) (Value, bool) {
		m := leafWeakMap(this)
		if m == nil {
			return Undefined, false
		}
		_, ok := m.get(r, arg(args, 0))
		return Bool(ok), true
	}
	leafOps[elemWeakMapSet] = func(r *Runtime, this Value, args []Value, mayStore bool) (Value, bool) {
		m := leafWeakMap(this)
		if m == nil {
			return Undefined, false
		}
		if !mayStore {
			r.pureRefused = true
			return Undefined, false
		}
		k := arg(args, 0)
		if !r.canBeWeak(k) {
			return Undefined, false
		}
		m.set(r, k, arg(args, 1))
		return this, true
	}
	leafOps[elemWeakMapDelete] = func(r *Runtime, this Value, args []Value, mayStore bool) (Value, bool) {
		m := leafWeakMap(this)
		if m == nil {
			return Undefined, false
		}
		if !mayStore {
			r.pureRefused = true
			return Undefined, false
		}
		return Bool(m.delete(arg(args, 0))), true
	}
	leafOps[elemWeakSetHas] = func(r *Runtime, this Value, args []Value, _ bool) (Value, bool) {
		s := leafWeakSet(this)
		if s == nil {
			return Undefined, false
		}
		return Bool(s.has(arg(args, 0))), true
	}
	leafOps[elemWeakSetAdd] = func(r *Runtime, this Value, args []Value, mayStore bool) (Value, bool) {
		s := leafWeakSet(this)
		if s == nil {
			return Undefined, false
		}
		if !mayStore {
			r.pureRefused = true
			return Undefined, false
		}
		v := arg(args, 0)
		if !r.canBeWeak(v) {
			return Undefined, false
		}
		s.add(v)
		return this, true
	}
	leafOps[elemWeakSetDelete] = func(r *Runtime, this Value, args []Value, mayStore bool) (Value, bool) {
		s := leafWeakSet(this)
		if s == nil {
			return Undefined, false
		}
		if !mayStore {
			r.pureRefused = true
			return Undefined, false
		}
		return Bool(s.delete(arg(args, 0))), true
	}
	leafMapDelete = func(r *Runtime, this Value, args []Value, mayStore bool, class Class) (Value, bool) {
		if !this.IsObject() {
			return Undefined, false
		}
		o := this.Object()
		m, ok := o.dataOf().(*jsMap)
		if !ok || o.class != class {
			return Undefined, false
		}
		if !mayStore {
			r.pureRefused = true
			return Undefined, false
		}
		return Bool(m.delete(arg(args, 0))), true
	}
	leafWeakMap = func(this Value) *weakMap {
		if !this.IsObject() {
			return nil
		}
		o := this.Object()
		if m, ok := o.dataOf().(*weakMap); ok && o.class == ClassWeakMap {
			return m
		}
		return nil
	}
	leafWeakSet = func(this Value) *weakSet {
		if !this.IsObject() {
			return nil
		}
		o := this.Object()
		if s, ok := o.dataOf().(*weakSet); ok && o.class == ClassWeakSet {
			return s
		}
		return nil
	}
}

// leafMapDelete is Map.prototype.delete's or Set.prototype.delete's leaf,
// of a receiver of class; leafWeakMap and leafWeakSet are the receiver's
// weak collection, nil for another kind of receiver.
var (
	leafMapDelete func(r *Runtime, this Value, args []Value, mayStore bool, class Class) (Value, bool)
	leafWeakMap   func(this Value) *weakMap
	leafWeakSet   func(this Value) *weakSet
)
