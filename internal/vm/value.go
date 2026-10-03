// Package vm implements the JavaScript value representation, object model and
// interpreter.
//
// Values, objects, strings and the interpreter live in one package because they
// are mutually recursive: a property holds a value, a value may be an object,
// and an accessor property calls back into the interpreter. Splitting them
// would require either an interface boundary on the hottest paths or an import
// cycle.
//
// The files named vm*.go hold the code a running script spends its time in:
// the interpreter's loop and its calls, the frameless evaluator, the property
// caches, objects' and properties' lookups, and the tree tier's run. The Go
// linker lays out a package's functions file by file in the order of their
// names, and on this kind of code the processor is several per cent faster or
// slower for where hot functions sit against each other. Kept together, after
// the rest, they move together when anything else changes size, instead of
// being moved against each other by code that never runs while they do.
// Code that a benchmark's inner loop calls belongs in one of them.
package vm

import (
	"math"
	"unsafe"
)

// Kind enumerates the JavaScript language types, plus the internal
// KindUninitialized used for bindings in their temporal dead zone.
type Kind uint8

const (
	KindNumber Kind = iota
	KindUndefined
	KindNull
	KindBool
	KindString
	KindSymbol
	KindBigInt
	KindObject
	// KindUninitialized marks a let or const binding that has been allocated
	// but not yet initialized. It never escapes into user code: reading such a
	// binding raises a ReferenceError.
	KindUninitialized
)

func (k Kind) String() string {
	switch k {
	case KindNumber:
		return "number"
	case KindUndefined:
		return "undefined"
	case KindNull:
		return "null"
	case KindBool:
		return "boolean"
	case KindString:
		return "string"
	case KindSymbol:
		return "symbol"
	case KindBigInt:
		return "bigint"
	case KindObject:
		return "object"
	}
	return "uninitialized"
}

// Value is a JavaScript value.
//
// The representation is a NaN box split across two fields. num holds a real
// number when the value is a number, and otherwise holds a quiet NaN whose
// payload encodes the kind; ref holds the pointer for the reference kinds and
// is nil for primitives. This keeps a Value at 24 bytes with no allocation for
// any kind -- pointers stored in an interface do not allocate -- while making
// the kind check a single mask-and-compare rather than a dynamic dispatch.
//
// The one invariant this depends on is that a genuine NaN never collides with a
// tag. Arithmetic can produce a negative quiet NaN, which would land inside the
// tag range, so every number that enters a Value goes through Float, which
// normalizes NaN to the canonical positive form.
type Value struct {
	num float64
	// ref is what a string, symbol, BigInt or object value holds, as a
	// pointer of no type: the tag in num says which it is. One word is half
	// what an interface is to copy and to trace, and the kind is known
	// already, so nothing is asserted to find it. Two internal values ride
	// on an object's tag, told apart by its payload: an accessor pair, which
	// is an accessor property's value, and a closure template, which is a
	// function constant's.
	ref unsafe.Pointer
}

// The payloads of KindObject's tag: an object, or one of the internal values
// a Value carries.
const (
	objPlain    = 0
	objAccessor = 1
	objTemplate = 2
)

var (
	objectBits   = math.Float64bits(mkTag(KindObject, objPlain))
	accessorBits = math.Float64bits(mkTag(KindObject, objAccessor))
	templateBits = math.Float64bits(mkTag(KindObject, objTemplate))
)

// accessorValue is an accessor property's value.
func accessorValue(a *accessor) Value {
	return Value{num: mkTag(KindObject, objAccessor), ref: unsafe.Pointer(a)}
}

// templateValue is a function constant: the template OpClosure copies.
func templateValue(cl *closure) Value {
	return Value{num: mkTag(KindObject, objTemplate), ref: unsafe.Pointer(cl)}
}

// accessorPair is the getter and setter an accessor property's value holds,
// or nil.
func (v Value) accessorPair() *accessor {
	if math.Float64bits(v.num) == accessorBits {
		return (*accessor)(v.ref)
	}
	return nil
}

// template is the closure template a function constant holds, or nil.
func (v Value) template() *closure {
	if math.Float64bits(v.num) == templateBits {
		return (*closure)(v.ref)
	}
	return nil
}

const (
	// tagBase is a quiet NaN with the sign bit set. Tag payloads occupy the low
	// 16 bits, which no arithmetic result can reach once NaN is normalized.
	tagBase = 0xFFF8000000000000
	// tagMask clears the payload, leaving the pattern that identifies a tagged
	// value.
	tagMask = 0xFFFFFFFFFFFF0000
	// canonicalNaN is the positive quiet NaN that every NaN is normalized to,
	// chosen because it lies outside the tag range.
	canonicalNaN = 0x7FF8000000000000
)

// mkTag builds the num field for a non-number value.
func mkTag(k Kind, payload uint8) float64 {
	return math.Float64frombits(tagBase | uint64(payload)<<8 | uint64(k))
}

// Pre-built constants for the values that carry no payload, so the common
// pushes cost a copy rather than a computation.
var (
	Undefined     = Value{num: mkTag(KindUndefined, 0)}
	Null          = Value{num: mkTag(KindNull, 0)}
	True          = Value{num: mkTag(KindBool, 1)}
	False         = Value{num: mkTag(KindBool, 0)}
	uninitialized = Value{num: mkTag(KindUninitialized, 0)}
)

// Kind returns the value's language type.
func (v Value) Kind() Kind {
	bits := math.Float64bits(v.num)
	if bits&tagMask != tagBase {
		// Anything that is not a tagged NaN is a real number.
		return KindNumber
	}
	return Kind(bits & 0xFF)
}

// Float returns a number value.
//
// NaN is normalized to a single canonical bit pattern. This is required for
// correctness, not merely tidiness: an un-normalized negative NaN would be
// indistinguishable from a tagged value.
func Float(f float64) Value {
	if f != f {
		return Value{num: math.Float64frombits(canonicalNaN)}
	}
	return Value{num: f}
}

// Int returns a number value for an integer.
func Int(i int) Value { return Value{num: float64(i)} }

// Int32 returns a number value for a signed 32-bit integer.
func Int32(i int32) Value { return Value{num: float64(i)} }

// Uint32 returns a number value for an unsigned 32-bit integer.
func Uint32(u uint32) Value { return Value{num: float64(u)} }

// Bool returns true or false.
func Bool(b bool) Value {
	if b {
		return True
	}
	return False
}

// Str returns a string value, interning short strings through the runtime is
// the caller's choice; this wraps an already-built *String.
func Str(s *String) Value {
	return Value{num: mkTag(KindString, 0), ref: unsafe.Pointer(s)}
}

// Sym returns a symbol value.
func Sym(s *Symbol) Value {
	return Value{num: mkTag(KindSymbol, 0), ref: unsafe.Pointer(s)}
}

// Big returns a BigInt value.
func Big(b *BigInt) Value {
	return Value{num: mkTag(KindBigInt, 0), ref: unsafe.Pointer(b)}
}

// Obj returns an object value.
func Obj(o *Object) Value {
	return Value{num: mkTag(KindObject, 0), ref: unsafe.Pointer(o)}
}

// ---------------------------------------------------------------------------
// Predicates
// ---------------------------------------------------------------------------

// IsNumber reports whether the value is a number.
func (v Value) IsNumber() bool {
	return math.Float64bits(v.num)&tagMask != tagBase
}

// IsUndefined reports whether the value is undefined.
//
// The comparison must go through the tag rather than comparing num against
// Undefined.num: both are NaN bit patterns, and NaN is never equal to itself,
// so a float comparison would always report false.
func (v Value) IsUndefined() bool { return v.isTag(KindUndefined) }

// IsNull reports whether the value is null.
func (v Value) IsNull() bool { return v.isTag(KindNull) }

// IsNullish reports whether the value is null or undefined, the test that ?.
// and ?? perform.
func (v Value) IsNullish() bool {
	bits := math.Float64bits(v.num)
	if bits&tagMask != tagBase {
		return false
	}
	k := Kind(bits & 0xFF)
	return k == KindNull || k == KindUndefined
}

// IsBool reports whether the value is a boolean.
func (v Value) IsBool() bool { return v.isTag(KindBool) }

// IsString reports whether the value is a string.
func (v Value) IsString() bool { return v.isTag(KindString) }

// IsSymbol reports whether the value is a symbol.
func (v Value) IsSymbol() bool { return v.isTag(KindSymbol) }

// IsBigInt reports whether the value is a BigInt.
func (v Value) IsBigInt() bool { return v.isTag(KindBigInt) }

// IsObject reports whether the value is an object.
func (v Value) IsObject() bool { return math.Float64bits(v.num) == objectBits }

// IsUninitialized reports whether the value is a binding in its temporal dead
// zone.
func (v Value) IsUninitialized() bool { return v.isTag(KindUninitialized) }

// IsNaN reports whether the value is the number NaN.
func (v Value) IsNaN() bool { return v.IsNumber() && v.num != v.num }

func (v Value) isTag(k Kind) bool {
	bits := math.Float64bits(v.num)
	return bits&tagMask == tagBase && Kind(bits&0xFF) == k
}

// ---------------------------------------------------------------------------
// Accessors
//
// Each assumes the caller has already established the kind. Getting this wrong
// is a bug in the engine rather than in user code, so they do not report
// errors; the accessors that can be reached from a wrong kind return a zero
// value instead of panicking.
// ---------------------------------------------------------------------------

// Number returns the numeric payload.
func (v Value) Number() float64 { return v.num }

// BoolValue returns the boolean payload.
func (v Value) BoolValue() bool {
	return math.Float64bits(v.num)&0xFF00 != 0
}

// String returns the string payload.
func (v Value) String() *String {
	if v.isTag(KindString) {
		return (*String)(v.ref)
	}
	return nil
}

// Symbol returns the symbol payload.
func (v Value) Symbol() *Symbol {
	if v.isTag(KindSymbol) {
		return (*Symbol)(v.ref)
	}
	return nil
}

// BigInt returns the BigInt payload.
func (v Value) BigInt() *BigInt {
	if v.isTag(KindBigInt) {
		return (*BigInt)(v.ref)
	}
	return nil
}

// object is the object a value IsObject has just said it is, read without
// testing its kind a second time, which the compiler does not fold into the
// first.
func (v Value) object() *Object { return (*Object)(v.ref) }

// Object returns the object payload, or nil if the value is not an object.
func (v Value) Object() *Object {
	if math.Float64bits(v.num) == objectBits {
		return (*Object)(v.ref)
	}
	return nil
}

// refAny is what the value holds, as a pointer of its own type, for what
// has to know the type: the memory meter's reflection.
func (v Value) refAny() any {
	switch bits := math.Float64bits(v.num); {
	case v.ref == nil:
		return nil
	case bits == objectBits:
		return (*Object)(v.ref)
	case bits == accessorBits:
		return (*accessor)(v.ref)
	case bits == templateBits:
		return (*closure)(v.ref)
	case v.isTag(KindString):
		return (*String)(v.ref)
	case v.isTag(KindSymbol):
		return (*Symbol)(v.ref)
	case v.isTag(KindBigInt):
		return (*BigInt)(v.ref)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Equality
// ---------------------------------------------------------------------------

// sameBits reports whether two values are the very same bits: the same
// value, spelled the same way.
func (v Value) sameBits(w Value) bool {
	return math.Float64bits(v.num) == math.Float64bits(w.num) && v.ref == w.ref
}

// StrictEquals implements the === operator, which compares without coercion.
func (v Value) StrictEquals(w Value) bool {
	vk, wk := v.Kind(), w.Kind()
	if vk != wk {
		return false
	}
	switch vk {
	case KindNumber:
		// NaN is not equal to itself, and +0 equals -0. Go's == on float64
		// gives both of these for free.
		return v.num == w.num
	case KindUndefined, KindNull:
		return true
	case KindBool:
		return v.BoolValue() == w.BoolValue()
	case KindString:
		return v.String().Equals(w.String())
	case KindSymbol:
		return v.Symbol() == w.Symbol()
	case KindBigInt:
		return v.BigInt().Cmp(w.BigInt()) == 0
	case KindObject:
		return v.Object() == w.Object()
	}
	return false
}

// SameValueZero implements the equality used by Array.prototype.includes and
// the keys of Map and Set: like ===, except that NaN equals itself.
func (v Value) SameValueZero(w Value) bool {
	if v.IsNumber() && w.IsNumber() {
		if v.num != v.num && w.num != w.num {
			return true
		}
		return v.num == w.num
	}
	return v.StrictEquals(w)
}

// SameValue implements Object.is: like SameValueZero, except that +0 and -0
// are distinguished.
func (v Value) SameValue(w Value) bool {
	if v.IsNumber() && w.IsNumber() {
		if v.num != v.num && w.num != w.num {
			return true
		}
		if v.num == 0 && w.num == 0 {
			return math.Signbit(v.num) == math.Signbit(w.num)
		}
		return v.num == w.num
	}
	return v.StrictEquals(w)
}

// Truthy implements the ToBoolean abstract operation.
func (v Value) Truthy() bool {
	switch v.Kind() {
	case KindNumber:
		// Zero, negative zero and NaN are all falsy.
		return v.num != 0 && v.num == v.num
	case KindUndefined, KindNull:
		return false
	case KindBool:
		return v.BoolValue()
	case KindString:
		return v.String().Len() != 0
	case KindBigInt:
		return !v.BigInt().IsZero()
	case KindObject:
		// Every object is truthy but document.all.
		return v.Object().flags&objHTMLDDA == 0
	}
	// Symbols are always truthy.
	return true
}

// TypeOf implements the typeof operator.
func (v Value) TypeOf() string {
	switch v.Kind() {
	case KindNumber:
		return "number"
	case KindUndefined:
		return "undefined"
	case KindNull:
		// typeof null is "object", a mistake preserved for compatibility.
		return "object"
	case KindBool:
		return "boolean"
	case KindString:
		return "string"
	case KindSymbol:
		return "symbol"
	case KindBigInt:
		return "bigint"
	case KindObject:
		o := v.Object()
		if o.flags&objHTMLDDA != 0 {
			return "undefined"
		}
		if o.IsCallable() {
			return "function"
		}
		return "object"
	}
	return "undefined"
}

// trueBits and falseBits are how true and false are spelled, which a branch
// compares a value against before asking it anything more.
var trueBits, falseBits = math.Float64bits(True.num), math.Float64bits(False.num)

// typeofNames are the answers typeof gives, in typeofStrs's order.
var typeofNames = [8]string{"undefined", "object", "boolean", "number", "string", "symbol", "bigint", "function"}

// typeofString is the string typeof answers for v, made once per runtime.
func (r *Runtime) typeofString(v Value) *String {
	name := v.TypeOf()
	i := 0
	switch name {
	case "object":
		i = 1
	case "boolean":
		i = 2
	case "number":
		i = 3
	case "string":
		i = 4
	case "symbol":
		i = 5
	case "bigint":
		i = 6
	case "function":
		i = 7
	}
	if s := r.typeofStrs[i]; s != nil && typeofNames[i] == name {
		return s
	}
	s := NewString(name)
	if typeofNames[i] == name {
		r.typeofStrs[i] = s
	}
	return s
}
