// Package module holds a decoded WebAssembly module: its types, imports,
// functions, tables, memories, globals, tags, exports and segments, and the
// type relations -- subtyping and the isorecursive canonicalization of type
// definitions -- that validation and linking ask of them.
//
// The type names printed in errors are V8's, so that an error a module causes
// here reads as it does in V8 (src/wasm/value-type.cc).
package module

import (
	"strconv"
	"strings"
)

// Kind classifies a ValueType.
type Kind uint8

// The kinds of value types. I8 and I16 are packed storage types, valid only
// as struct fields and array elements. Void, Top and Bottom are sentinels the
// validator uses: Bottom is the type of a value popped from an unreachable
// stack, which matches every type.
const (
	KindVoid Kind = iota
	KindI32
	KindI64
	KindF32
	KindF64
	KindV128
	KindI8
	KindI16
	KindRef
	KindTop
	KindBottom
)

// HeapType is an index into the module's type section, or, at abstractBit and
// above, an abstract heap type named by its binary code.
type HeapType uint32

const abstractBit HeapType = 1 << 31

// The abstract heap types, at their binary codes.
const (
	HeapFunc     = abstractBit | 0x70
	HeapExtern   = abstractBit | 0x6f
	HeapAny      = abstractBit | 0x6e
	HeapEq       = abstractBit | 0x6d
	HeapI31      = abstractBit | 0x6c
	HeapStruct   = abstractBit | 0x6b
	HeapArray    = abstractBit | 0x6a
	HeapExn      = abstractBit | 0x69
	HeapNoExn    = abstractBit | 0x74
	HeapNoFunc   = abstractBit | 0x73
	HeapNoExtern = abstractBit | 0x72
	HeapNone     = abstractBit | 0x71
	// HeapBottom is the heap type of a bottom reference.
	HeapBottom = abstractBit | 0xff
)

// HeapIndex returns the heap type of type definition i.
func HeapIndex(i uint32) HeapType { return HeapType(i) }

// IsIndex reports whether h names a type definition.
func (h HeapType) IsIndex() bool { return h < abstractBit }

// Index returns the type index h names.
func (h HeapType) Index() uint32 { return uint32(h) }

// Code returns the binary code of an abstract heap type.
func (h HeapType) Code() byte { return byte(h) }

// HeapFromCode returns the abstract heap type with binary code c, and whether
// c is one.
func HeapFromCode(c byte) (HeapType, bool) {
	switch c {
	case 0x70, 0x6f, 0x6e, 0x6d, 0x6c, 0x6b, 0x6a, 0x69, 0x74, 0x73, 0x72, 0x71:
		return abstractBit | HeapType(c), true
	}
	return 0, false
}

// abstractName is V8's name for an abstract heap type.
func (h HeapType) abstractName() string {
	switch h {
	case HeapFunc:
		return "func"
	case HeapExtern:
		return "extern"
	case HeapAny:
		return "any"
	case HeapEq:
		return "eq"
	case HeapI31:
		return "i31"
	case HeapStruct:
		return "struct"
	case HeapArray:
		return "array"
	case HeapExn:
		return "exn"
	case HeapNoExn:
		return "noexn"
	case HeapNoFunc:
		return "nofunc"
	case HeapNoExtern:
		return "noextern"
	case HeapNone:
		return "none"
	case HeapBottom:
		return "<bot>"
	}
	return "<unknown>"
}

// Name returns V8's name for the heap type: its index, or its abstract name.
func (h HeapType) Name() string {
	if h.IsIndex() {
		return strconv.FormatUint(uint64(h), 10)
	}
	return h.abstractName()
}

// ValueType is a WebAssembly value type, a packed storage type, or one of the
// validator's sentinels. It is comparable: two equal ValueTypes of one module
// are the same type.
type ValueType struct {
	Kind     Kind
	Nullable bool
	Heap     HeapType
}

// The numeric, vector and sentinel types, and the common reference types.
var (
	Void   = ValueType{Kind: KindVoid}
	I32    = ValueType{Kind: KindI32}
	I64    = ValueType{Kind: KindI64}
	F32    = ValueType{Kind: KindF32}
	F64    = ValueType{Kind: KindF64}
	V128   = ValueType{Kind: KindV128}
	I8     = ValueType{Kind: KindI8}
	I16    = ValueType{Kind: KindI16}
	Top    = ValueType{Kind: KindTop}
	Bottom = ValueType{Kind: KindBottom, Heap: HeapBottom}

	FuncRef     = RefNull(HeapFunc)
	ExternRef   = RefNull(HeapExtern)
	AnyRef      = RefNull(HeapAny)
	EqRef       = RefNull(HeapEq)
	I31Ref      = RefNull(HeapI31)
	StructRef   = RefNull(HeapStruct)
	ArrayRef    = RefNull(HeapArray)
	ExnRef      = RefNull(HeapExn)
	NullRef     = RefNull(HeapNone)
	NullFuncRef = RefNull(HeapNoFunc)
	NullExtern  = RefNull(HeapNoExtern)
	NullExnRef  = RefNull(HeapNoExn)
)

// Ref returns the non-nullable reference type to h.
func Ref(h HeapType) ValueType { return ValueType{Kind: KindRef, Heap: h} }

// RefNull returns the nullable reference type to h.
func RefNull(h HeapType) ValueType { return ValueType{Kind: KindRef, Nullable: true, Heap: h} }

// RefMaybeNull returns the reference type to h, nullable as asked.
func RefMaybeNull(h HeapType, nullable bool) ValueType {
	return ValueType{Kind: KindRef, Nullable: nullable, Heap: h}
}

// IsRef reports whether t is a reference type.
func (t ValueType) IsRef() bool { return t.Kind == KindRef }

// IsNumeric reports whether t is a number, a vector or a packed type.
func (t ValueType) IsNumeric() bool { return t.Kind >= KindI32 && t.Kind <= KindI16 }

// IsPacked reports whether t is i8 or i16.
func (t ValueType) IsPacked() bool { return t.Kind == KindI8 || t.Kind == KindI16 }

// IsBottom reports whether t is the bottom sentinel.
func (t ValueType) IsBottom() bool { return t.Kind == KindBottom }

// HasIndex reports whether t is a reference to a type definition.
func (t ValueType) HasIndex() bool { return t.Kind == KindRef && t.Heap.IsIndex() }

// IsDefaultable reports whether t has a default value: a number, a vector or a
// nullable reference.
func (t ValueType) IsDefaultable() bool { return t.Kind != KindRef || t.Nullable }

// Unpacked returns the value type a packed storage type is read as: i32 for
// i8 and i16, t otherwise.
func (t ValueType) Unpacked() ValueType {
	if t.IsPacked() {
		return I32
	}
	return t
}

// AsNonNull returns t without its null.
func (t ValueType) AsNonNull() ValueType {
	t.Nullable = false
	return t
}

// AsNullable returns t with a null.
func (t ValueType) AsNullable() ValueType {
	if t.Kind == KindRef {
		t.Nullable = true
	}
	return t
}

// Size returns the size in bytes of a value of type t as a field: 1 or 2 for
// packed types, 4, 8 or 16 for numbers and vectors, 8 for references.
func (t ValueType) Size() int {
	switch t.Kind {
	case KindI8:
		return 1
	case KindI16:
		return 2
	case KindI32, KindF32:
		return 4
	case KindV128:
		return 16
	}
	return 8
}

// Name returns V8's name for t, as it appears in error messages: i32, v128,
// funcref, nullref, (ref func), (ref null 3), <bot>.
func (t ValueType) Name() string {
	switch t.Kind {
	case KindVoid:
		return "<void>"
	case KindI32:
		return "i32"
	case KindI64:
		return "i64"
	case KindF32:
		return "f32"
	case KindF64:
		return "f64"
	case KindV128:
		return "v128"
	case KindI8:
		return "i8"
	case KindI16:
		return "i16"
	case KindTop:
		return "<top>"
	case KindBottom:
		return "<bot>"
	}
	if t.Heap.IsIndex() {
		if t.Nullable {
			return "(ref null " + t.Heap.Name() + ")"
		}
		return "(ref " + t.Heap.Name() + ")"
	}
	if t.Nullable {
		switch t.Heap {
		case HeapNone:
			return "nullref"
		case HeapNoExn:
			return "nullexnref"
		case HeapNoExtern:
			return "nullexternref"
		case HeapNoFunc:
			return "nullfuncref"
		}
		return t.Heap.abstractName() + "ref"
	}
	return "(ref " + t.Heap.abstractName() + ")"
}

// String returns t's name.
func (t ValueType) String() string { return t.Name() }

// TypeKind is what a type definition defines.
type TypeKind uint8

// The kinds of type definitions.
const (
	TypeFunc TypeKind = iota
	TypeStruct
	TypeArray
)

// NoSuper marks a type definition without a declared supertype.
const NoSuper = ^uint32(0)

// FuncType is a function signature.
type FuncType struct {
	Params  []ValueType
	Results []ValueType
}

// String returns the signature as V8 prints one in a LinkError:
// "(i32, i64) -> (f32)".
func (f *FuncType) String() string {
	var b strings.Builder
	b.WriteByte('(')
	for i, p := range f.Params {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(p.Name())
	}
	b.WriteString(") -> (")
	for i, r := range f.Results {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(r.Name())
	}
	b.WriteByte(')')
	return b.String()
}

// FieldType is a struct field or an array element: a storage type and whether
// it is mutable.
type FieldType struct {
	Type    ValueType
	Mutable bool
}

// StructType is a struct definition's fields.
type StructType struct {
	Fields []FieldType
}

// ArrayType is an array definition's element.
type ArrayType struct {
	Elem FieldType
}

// TypeDef is one entry of the type section.
type TypeDef struct {
	Kind   TypeKind
	Func   *FuncType
	Struct *StructType
	Array  *ArrayType
	// Super is the declared supertype's index, or NoSuper.
	Super uint32
	// Final reports whether the type may have no subtypes. A type written
	// without a sub clause is final.
	Final bool
	// RecStart and RecSize locate the type's recursion group in the section.
	RecStart, RecSize uint32
	// Depth is the length of the chain of declared supertypes above the type.
	Depth uint32
}

// Name returns V8's name for a type definition's kind.
func (k TypeKind) Name() string {
	switch k {
	case TypeFunc:
		return "func"
	case TypeStruct:
		return "struct"
	case TypeArray:
		return "array"
	}
	return "unknown"
}
