package vm

import "math"

// Leaves: built-ins every tier calls without a frame, where they call
// nothing and cannot throw (elemOp past elemRandom), as V8's optimized code
// calls fast API functions with no frame of JavaScript's. A built-in
// declares its own where it is made (setElemOp); the interpreter, the tree
// tier and pure bodies apply it before they would make a frame (callDirect,
// callRest, pureInvoke, by pureElemOp and collectionOp, or a native leaf by
// its check), and native code's calls of Go do too (jitGoCallPlain), none
// of them keeping a list of
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

// leafOps are the hand-made leaves' functions, by their elemOp: an index
// and a call, where one function asking which op it was had cost a Map's
// deletion of a String's key, with the JIT, 7% more than the two calls' own
// lists.
var leafOps [elemNativeFirst]leafFunc

// leafChecks are the native leaves' checks, by their elemOp: whether the
// built-in's own function, called with this and args, runs no script,
// throws nothing and keeps nothing -- a string receiver and arguments that
// convert without calling anything, results within a string's length --
// so that it is called without a frame. A check must be exact: a function
// called on what it passes, that made an error after all, had run what
// making one may run (Error.stackTraceLimit's getter) with no frame to show
// for it.
//
// Each call that applies leaves asks the check itself and calls the
// function it holds (funcData.native) -- callDirect, callRest, pureInvoke,
// jitPlainCall -- as the memory meter allows: a function of leafOps' that
// did, with the runtime's list of the functions, had cost a string's at()
// in the interpreter 10% more than its frame, in the four calls between.
var leafChecks [elemLeafEnd]func(r *Runtime, this Value, args []Value) bool

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
	initStringLeafChecks = func() {
		str := func(_ *Runtime, this Value, args []Value) bool {
			if !this.IsString() {
				return false
			}
			for _, a := range args {
				if !a.IsNumber() && !a.IsString() && !a.IsBool() && !a.IsUndefined() && !a.IsNull() {
					return false
				}
			}
			return true
		}
		for _, op := range []uint8{elemStrAt, elemStrCharAt, elemStrCharCodeAt, elemStrCodePointAt, elemStrIndexOf,
			elemStrLastIndexOf, elemStrIncludes, elemStrStartsWith, elemStrEndsWith, elemStrSlice, elemStrSubstring,
			elemStrSubstr, elemStrToUpperCase, elemStrToLowerCase, elemStrTrim, elemStrTrimStart, elemStrTrimEnd,
			elemStrIsWellFormed, elemStrToWellFormed} {
			leafChecks[op] = str
		}
		leafChecks[elemStrConcat] = func(r *Runtime, this Value, args []Value) bool {
			if !str(r, this, args) {
				return false
			}
			// What the arguments' strings may come to: a string's length, or
			// at most a number's, true's or null's.
			n := int64(this.String().length)
			for _, a := range args {
				if a.IsString() {
					n += int64(a.String().length)
				} else {
					n += 32
				}
			}
			return n <= maxStringLength
		}
		// repeat and the pads of a string by a number count first, asked
		// with no call, where str's and toInteger's, beside the conversion
		// the built-in makes again, had cost repeat in the interpreter more
		// than the frame they save; anything else as any other leaf.
		leafChecks[elemStrRepeat] = func(r *Runtime, this Value, args []Value) bool {
			if len(args) == 1 && this.IsString() && args[0].IsNumber() {
				n := args[0].num
				return n >= 0 && n <= maxStringLength && float64(this.String().length)*math.Trunc(n) <= maxStringLength
			}
			if !str(r, this, args) {
				return false
			}
			n, err := r.toInteger(arg(args, 0))
			return err == nil && n >= 0 && !math.IsInf(n, 0) && float64(this.String().length)*n <= maxStringLength
		}
		pad := func(r *Runtime, this Value, args []Value) bool {
			if len(args) >= 1 && len(args) <= 2 && this.IsString() && args[0].IsNumber() {
				if len(args) == 2 {
					if f := args[1]; !f.IsString() && !f.IsUndefined() {
						return str(r, this, args) && args[0].num <= maxStringLength
					}
				}
				return args[0].num <= maxStringLength
			}
			if !str(r, this, args) {
				return false
			}
			n, err := r.toInteger(arg(args, 0))
			return err == nil && n <= maxStringLength
		}
		leafChecks[elemStrPadStart], leafChecks[elemStrPadEnd] = pad, pad
		leafChecks[elemStrNormalize] = func(r *Runtime, this Value, args []Value) bool {
			if !str(r, this, args) {
				return false
			}
			if f := arg(args, 0); !f.IsUndefined() {
				if !f.IsString() {
					return false
				}
				if _, ok := normalizationForms[f.String().Go()]; !ok {
					return false
				}
			}
			// A form's expansion of a code unit is at most 18 of them.
			return int64(this.String().length)*18 <= maxStringLength
		}
		leafChecks[elemStrLocaleCompare] = func(r *Runtime, this Value, args []Value) bool {
			// Without locales and options, which the collator's choice may
			// refuse.
			return len(args) <= 1 && str(r, this, args)
		}
		leafChecks[elemStrFromCharCode] = func(_ *Runtime, _ Value, args []Value) bool {
			return leafPrimitives(args)
		}
		leafChecks[elemStrFromCodePoint] = func(_ *Runtime, _ Value, args []Value) bool {
			for _, a := range args {
				if !a.IsNumber() || a.num != math.Trunc(a.num) || a.num < 0 || a.num > 0x10FFFF {
					return false
				}
			}
			return true
		}
	}

	leafPrimitives = func(args []Value) bool {
		for _, a := range args {
			if !a.IsNumber() && !a.IsString() && !a.IsBool() && !a.IsUndefined() && !a.IsNull() {
				return false
			}
		}
		return true
	}
	initStringLeafChecks()
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

// initStringLeafChecks gives String's native leaves their checks: a string
// receiver, and arguments that are primitives converting without calling
// anything (as leafPrimitives), each method's results within a string's
// length. Like leafPrimitives, a closure assigned in init, not a package
// variable's function literal, which the compiler lays out among the
// package's other closures, ahead of the tree tier's: 3.5 KB of them moved.
var initStringLeafChecks func()

// leafPrimitives reports whether each of args is a primitive that converts
// to a number and to a string without calling anything or throwing: a
// number, a string, a boolean, undefined or null -- not an object, a
// symbol or a BigInt.
var leafPrimitives func(args []Value) bool
