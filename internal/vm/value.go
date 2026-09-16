// Package vm implements the JavaScript value representation, object model and
// interpreter.
//
// Values, objects, strings and the interpreter live in one package because they
// are mutually recursive: a property holds a value, a value may be an object,
// and an accessor property calls back into the interpreter. Splitting them
// would require either an interface boundary on the hottest paths or an import
// cycle.
package vm

import "math"

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
	ref any
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
	return Value{num: mkTag(KindString, 0), ref: s}
}

// Sym returns a symbol value.
func Sym(s *Symbol) Value {
	return Value{num: mkTag(KindSymbol, 0), ref: s}
}

// Big returns a BigInt value.
func Big(b *BigInt) Value {
	return Value{num: mkTag(KindBigInt, 0), ref: b}
}

// Obj returns an object value.
func Obj(o *Object) Value {
	return Value{num: mkTag(KindObject, 0), ref: o}
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
func (v Value) IsObject() bool { return v.isTag(KindObject) }

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
	s, _ := v.ref.(*String)
	return s
}

// Symbol returns the symbol payload.
func (v Value) Symbol() *Symbol {
	s, _ := v.ref.(*Symbol)
	return s
}

// BigInt returns the BigInt payload.
func (v Value) BigInt() *BigInt {
	b, _ := v.ref.(*BigInt)
	return b
}

// Object returns the object payload, or nil if the value is not an object.
func (v Value) Object() *Object {
	o, _ := v.ref.(*Object)
	return o
}

// ---------------------------------------------------------------------------
// Equality
// ---------------------------------------------------------------------------

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
	}
	// Objects and symbols are always truthy.
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
		if v.Object().IsCallable() {
			return "function"
		}
		return "object"
	}
	return "undefined"
}
