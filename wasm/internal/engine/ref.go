package engine

import "unsafe"

// Ref is a reference value: null, an i31, or a pointer to an object.
//
// An object begins with a header saying what it is, so a Ref is one pointer
// and a payload. An i31 is the address of i31Mark with its value in the
// payload: it allocates nothing, and the collector sees only pointers to
// real objects.
type Ref struct {
	p unsafe.Pointer
	i uint64
}

// ObjKind says what a referenced object is.
type ObjKind uint8

// The kinds of objects a reference may point to.
const (
	ObjFunc ObjKind = iota + 1
	ObjStruct
	ObjArray
	ObjExn
	// ObjHost is a value of the host's, an externref the host passed in.
	ObjHost
)

// header begins every object a Ref points to.
type header struct {
	kind ObjKind
}

// i31Mark is what an i31's pointer points to.
var i31Mark header

// Null is the null reference.
var Null Ref

// IsNull reports whether r is null.
func (r Ref) IsNull() bool { return r.p == nil }

// IsI31 reports whether r is an i31.
func (r Ref) IsI31() bool { return r.p == unsafe.Pointer(&i31Mark) }

// I31 returns the i31 reference of the low 31 bits of v.
func I31(v uint32) Ref { return Ref{p: unsafe.Pointer(&i31Mark), i: uint64(v & 0x7fff_ffff)} }

// I31Signed returns r's value, an i31, sign-extended.
func (r Ref) I31Signed() int32 { return int32(uint32(r.i)<<1) >> 1 }

// I31Unsigned returns r's value, an i31, zero-extended.
func (r Ref) I31Unsigned() uint32 { return uint32(r.i) }

// Kind returns what r points to; r must be neither null nor an i31.
func (r Ref) Kind() ObjKind { return (*header)(r.p).kind }

// Eq reports whether r and s are the same reference, as ref.eq compares
// them: the same object, or i31s of the same value, or both null.
func (r Ref) Eq(s Ref) bool { return r == s }

// Host is a host value referenced from wasm.
type Host struct {
	hdr header
	V   any
}

// HostRef returns a reference to host value v; a nil v is the null
// reference.
func HostRef(v any) Ref {
	if v == nil {
		return Null
	}
	return Ref{p: unsafe.Pointer(&Host{hdr: header{kind: ObjHost}, V: v})}
}

// Host returns the host value r refers to, and whether it refers to one.
func (r Ref) Host() (any, bool) {
	if r.p == nil || r.IsI31() || r.Kind() != ObjHost {
		return nil, false
	}
	return (*Host)(r.p).V, true
}

// Pointer returns r's object pointer: nil for null, the i31 mark for an i31.
func (r Ref) Pointer() unsafe.Pointer { return r.p }

// RefMatchesAbstract reports whether non-null reference r is of abstract
// heap type h: "any", "eq", "struct", "array", "i31", "func", "extern" or
// "exn".
func RefMatchesAbstract(r Ref, h string) bool {
	if r.IsI31() {
		return h == "any" || h == "eq" || h == "i31" || h == "extern"
	}
	switch r.Kind() {
	case ObjFunc:
		return h == "func"
	case ObjStruct:
		return h == "any" || h == "eq" || h == "struct" || h == "extern"
	case ObjArray:
		return h == "any" || h == "eq" || h == "array" || h == "extern"
	case ObjExn:
		return h == "exn"
	case ObjHost:
		return h == "extern" || h == "any"
	}
	return false
}
