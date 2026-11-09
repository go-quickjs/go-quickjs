package api

import (
	"math"

	"github.com/go-quickjs/go-quickjs/wasm/internal/internalapi"
)

// Value is a value of any value type, as Function.CallValues, tables and
// globals carry it.
//
// A number is held in Lo as Call encodes it, a v128 in Lo and Hi. A
// reference is held in Ref:
//
//   - nil is null, of any reference type;
//   - for a funcref, a Function of this runtime's;
//   - for an externref, the host's value, any Go value; a Reference that
//     wasm made external is passed back as it came;
//   - for an anyref, a Reference, or a host value, which wasm sees as an
//     external value made internal;
//   - for any other reference -- structs, arrays, i31s, exceptions -- a
//     Reference, which the host can hold and pass back but not look into.
type Value struct {
	// Type is the value's type. CallValues takes each parameter's type from
	// the function's signature and does not look at it.
	Type ValueType
	// Lo holds a number, or a v128's low 64 bits.
	Lo uint64
	// Hi holds a v128's high 64 bits.
	Hi uint64
	// Ref holds a reference.
	Ref any
}

// Reference is a reference to a value wasm made -- a struct, an array, an
// i31 or an exception -- that the host holds without seeing into it.
type Reference interface {
	// Kind names what the reference refers to: "struct", "array", "i31",
	// "exn", or "func" or "extern" for one met where its type is not known.
	Kind() string

	internalapi.WasmOnly
}

// ValueI32 returns an i32 value.
func ValueI32(v int32) Value { return Value{Type: ValueTypeI32, Lo: EncodeI32(v)} }

// ValueI64 returns an i64 value.
func ValueI64(v int64) Value { return Value{Type: ValueTypeI64, Lo: EncodeI64(v)} }

// ValueF32 returns an f32 value.
func ValueF32(v float32) Value { return Value{Type: ValueTypeF32, Lo: EncodeF32(v)} }

// ValueF64 returns an f64 value.
func ValueF64(v float64) Value { return Value{Type: ValueTypeF64, Lo: EncodeF64(v)} }

// ValueV128 returns a v128 value of its low and high 64 bits.
func ValueV128(lo, hi uint64) Value { return Value{Type: ValueTypeV128, Lo: lo, Hi: hi} }

// ValueExternref returns an externref to v, null if v is nil.
func ValueExternref(v any) Value { return Value{Type: ValueTypeExternref, Ref: v} }

// ValueFuncref returns a funcref to f, null if f is nil.
func ValueFuncref(f Function) Value {
	if f == nil {
		return Value{Type: ValueTypeFuncref}
	}
	return Value{Type: ValueTypeFuncref, Ref: f}
}

// I32 returns the value as an i32.
func (v Value) I32() int32 { return DecodeI32(v.Lo) }

// I64 returns the value as an i64.
func (v Value) I64() int64 { return int64(v.Lo) }

// F32 returns the value as an f32.
func (v Value) F32() float32 { return DecodeF32(v.Lo) }

// F64 returns the value as an f64.
func (v Value) F64() float64 { return DecodeF64(v.Lo) }

// V128 returns the value's low and high 64 bits.
func (v Value) V128() (lo, hi uint64) { return v.Lo, v.Hi }

// IsNull reports whether the value is a null reference.
func (v Value) IsNull() bool { return IsReference(v.Type) && v.Ref == nil }

// EncodeExternref encodes an externref for Call.
func EncodeExternref(input uintptr) uint64 { return uint64(input) }

// DecodeExternref decodes an externref Call returned.
func DecodeExternref(input uint64) uintptr { return uintptr(input) }

// EncodeI32 encodes an int32 for Call.
func EncodeI32(input int32) uint64 { return uint64(uint32(input)) }

// DecodeI32 decodes an int32 Call returned.
func DecodeI32(input uint64) int32 { return int32(input) }

// EncodeU32 encodes a uint32 for Call.
func EncodeU32(input uint32) uint64 { return uint64(input) }

// DecodeU32 decodes a uint32 Call returned.
func DecodeU32(input uint64) uint32 { return uint32(input) }

// EncodeI64 encodes an int64 for Call.
func EncodeI64(input int64) uint64 { return uint64(input) }

// EncodeF32 encodes a float32 for Call.
func EncodeF32(input float32) uint64 { return uint64(math.Float32bits(input)) }

// DecodeF32 decodes a float32 Call returned.
func DecodeF32(input uint64) float32 { return math.Float32frombits(uint32(input)) }

// EncodeF64 encodes a float64 for Call.
func EncodeF64(input float64) uint64 { return math.Float64bits(input) }

// DecodeF64 decodes a float64 Call returned.
func DecodeF64(input uint64) float64 { return math.Float64frombits(input) }
