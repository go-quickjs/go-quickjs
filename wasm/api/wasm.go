// Package api holds the types the wasm package's runtime hands out and host
// functions are given: modules, functions, memories, globals and tables, and
// the encoding of values in calls.
//
// The interfaces here are implemented by the wasm package alone; they are
// interfaces to keep the two apart, not for other implementations.
package api

import (
	"context"
	"fmt"

	"github.com/go-quickjs/go-quickjs/wasm/internal/internalapi"
)

// ExternType is the kind of an import or an export, at its binary code.
type ExternType = byte

// The kinds of imports and exports.
const (
	ExternTypeFunc   ExternType = 0x00
	ExternTypeTable  ExternType = 0x01
	ExternTypeMemory ExternType = 0x02
	ExternTypeGlobal ExternType = 0x03
	ExternTypeTag    ExternType = 0x04
)

// The text format's names of the kinds of imports and exports.
const (
	ExternTypeFuncName   = "func"
	ExternTypeTableName  = "table"
	ExternTypeMemoryName = "memory"
	ExternTypeGlobalName = "global"
	ExternTypeTagName    = "tag"
)

// ExternTypeName returns the text format's name of et.
func ExternTypeName(et ExternType) string {
	switch et {
	case ExternTypeFunc:
		return ExternTypeFuncName
	case ExternTypeTable:
		return ExternTypeTableName
	case ExternTypeMemory:
		return ExternTypeMemoryName
	case ExternTypeGlobal:
		return ExternTypeGlobalName
	case ExternTypeTag:
		return ExternTypeTagName
	}
	return fmt.Sprintf("%#x", et)
}

// ValueType is the type of a parameter, a result, a global or a table's
// elements, at its binary code.
//
// A Call passes and returns each value as one uint64, or two for a v128:
//
//   - ValueTypeI32: EncodeI32 and DecodeI32 for an int32, EncodeU32 and
//     DecodeU32 for a uint32; the upper 32 bits of a result are zero.
//   - ValueTypeI64: the int64 or uint64 itself.
//   - ValueTypeF32: EncodeF32 and DecodeF32.
//   - ValueTypeF64: EncodeF64 and DecodeF64.
//   - ValueTypeV128: two values, the low 64 bits first.
//   - ValueTypeExternref: a uintptr, EncodeExternref and DecodeExternref,
//     zero for null. The engine keeps the uintptr as it is; a host passing a
//     Go pointer so keeps the object reachable itself.
//
// Every other reference type is reported as the shorthand of its heap type:
// a reference to a defined function type as ValueTypeFuncref, to a struct
// type as ValueTypeStructref, to an array type as ValueTypeArrayref, and a
// non-nullable reference as its nullable form. Call can pass only null for
// one; Value, with Function.CallValues, carries any of them.
type ValueType = byte

// The value types.
const (
	// ValueTypeI32 is a 32-bit integer.
	ValueTypeI32 ValueType = 0x7f
	// ValueTypeI64 is a 64-bit integer.
	ValueTypeI64 ValueType = 0x7e
	// ValueTypeF32 is a 32-bit float.
	ValueTypeF32 ValueType = 0x7d
	// ValueTypeF64 is a 64-bit float.
	ValueTypeF64 ValueType = 0x7c
	// ValueTypeV128 is a 128-bit vector.
	ValueTypeV128 ValueType = 0x7b

	// ValueTypeFuncref is a reference to a function.
	ValueTypeFuncref ValueType = 0x70
	// ValueTypeExternref is a reference to a value of the host's.
	ValueTypeExternref ValueType = 0x6f
	// ValueTypeAnyref is a reference to any internal value: a struct, an
	// array, an i31, or a host value made internal.
	ValueTypeAnyref ValueType = 0x6e
	// ValueTypeEqref is a reference that ref.eq compares.
	ValueTypeEqref ValueType = 0x6d
	// ValueTypeI31ref is an unboxed 31-bit integer.
	ValueTypeI31ref ValueType = 0x6c
	// ValueTypeStructref is a reference to a struct.
	ValueTypeStructref ValueType = 0x6b
	// ValueTypeArrayref is a reference to an array.
	ValueTypeArrayref ValueType = 0x6a
	// ValueTypeExnref is a reference to an exception.
	ValueTypeExnref ValueType = 0x69
	// ValueTypeNullexnref is the null exception reference.
	ValueTypeNullexnref ValueType = 0x74
	// ValueTypeNullfuncref is the null function reference.
	ValueTypeNullfuncref ValueType = 0x73
	// ValueTypeNullexternref is the null external reference.
	ValueTypeNullexternref ValueType = 0x72
	// ValueTypeNullref is the null internal reference.
	ValueTypeNullref ValueType = 0x71
)

// ValueTypeName returns the text format's name of t, or "unknown".
func ValueTypeName(t ValueType) string {
	switch t {
	case ValueTypeI32:
		return "i32"
	case ValueTypeI64:
		return "i64"
	case ValueTypeF32:
		return "f32"
	case ValueTypeF64:
		return "f64"
	case ValueTypeV128:
		return "v128"
	case ValueTypeFuncref:
		return "funcref"
	case ValueTypeExternref:
		return "externref"
	case ValueTypeAnyref:
		return "anyref"
	case ValueTypeEqref:
		return "eqref"
	case ValueTypeI31ref:
		return "i31ref"
	case ValueTypeStructref:
		return "structref"
	case ValueTypeArrayref:
		return "arrayref"
	case ValueTypeExnref:
		return "exnref"
	case ValueTypeNullexnref:
		return "nullexnref"
	case ValueTypeNullfuncref:
		return "nullfuncref"
	case ValueTypeNullexternref:
		return "nullexternref"
	case ValueTypeNullref:
		return "nullref"
	}
	return "unknown"
}

// IsReference reports whether t is a reference type.
func IsReference(t ValueType) bool {
	switch t {
	case ValueTypeI32, ValueTypeI64, ValueTypeF32, ValueTypeF64, ValueTypeV128:
		return false
	}
	return true
}

// Module is an instantiated module: a wasm module's instance, or a host
// module's.
//
// Closing the Runtime closes every Module it instantiated.
type Module interface {
	fmt.Stringer

	// Name is the name the module was instantiated under, which other
	// modules import it by. It is empty for an anonymous module.
	Name() string

	// Memory returns the module's first memory, defined or imported, or
	// nil if it has none.
	Memory() Memory

	// ExportedFunction returns the function exported as name, or nil.
	ExportedFunction(name string) Function

	// ExportedFunctionDefinitions returns the definitions of the exported
	// functions, keyed by export name.
	ExportedFunctionDefinitions() map[string]FunctionDefinition

	// ExportedMemory returns the memory exported as name, or nil.
	ExportedMemory(name string) Memory

	// ExportedMemoryDefinitions returns the definitions of the exported
	// memories, keyed by export name.
	ExportedMemoryDefinitions() map[string]MemoryDefinition

	// ExportedGlobal returns the global exported as name, or nil. A mutable
	// global is also a MutableGlobal.
	ExportedGlobal(name string) Global

	// ExportedTable returns the table exported as name, or nil.
	ExportedTable(name string) Table

	// CloseWithExitCode closes the module and frees its name for another
	// module to be instantiated under. A call of one of its functions made
	// afterwards returns a sys.ExitError of exitCode. Under the runtime's
	// WithCloseOnContextDone, calls running when it is closed end with that
	// error too; otherwise they run on.
	//
	// It may be called from a host function, to exit. Closing a closed
	// module does nothing and returns nil.
	CloseWithExitCode(ctx context.Context, exitCode uint32) error

	// Closer closes the module with exit code zero.
	Closer

	// IsClosed reports whether the module was closed.
	IsClosed() bool

	internalapi.WasmOnly
}

// Closer closes a resource.
type Closer interface {
	// Close closes the resource. The context is for values it carries; a
	// done context does not keep Close from closing.
	Close(context.Context) error
}

// ExportDefinition describes an entity a module defines or imports.
type ExportDefinition interface {
	// ModuleName is the name of the module that defines the entity, from
	// its name section, or the host module's name.
	ModuleName() string

	// Index is the entity's index in its module's index space of its kind,
	// imports first.
	Index() uint32

	// Import returns the module and name the entity is imported by, and
	// whether it is imported.
	Import() (moduleName, name string, isImport bool)

	// ExportNames are the names the entity is exported as.
	ExportNames() []string

	internalapi.WasmOnly
}

// MemoryDefinition describes a memory. Sizes are in pages of 64 KiB.
type MemoryDefinition interface {
	ExportDefinition

	// Min returns the initial size in pages, saturated at the largest
	// uint32 for a 64-bit memory larger than that.
	Min() uint32

	// Max returns the declared maximum in pages, saturated as Min is, and
	// false if there is none.
	Max() (uint32, bool)

	// Min64 returns the initial size in pages.
	Min64() uint64

	// Max64 returns the declared maximum in pages, and false if there is
	// none.
	Max64() (uint64, bool)

	// Is64 reports a memory with i64 addresses, which may pass 4 GiB.
	Is64() bool

	// IsShared reports a shared memory.
	IsShared() bool

	internalapi.WasmOnly
}

// FunctionDefinition describes a function.
type FunctionDefinition interface {
	ExportDefinition

	// Name is the function's name in its module's name section, or the
	// host function's name; it may be empty.
	Name() string

	// DebugName is the module name and the function name joined by a dot,
	// the function name being "$" and its index when it has none: "env.f",
	// ".$0".
	DebugName() string

	// GoFunction is a host function's implementation, a GoFunction or a
	// GoModuleFunction, or nil for a wasm function.
	GoFunction() interface{}

	// ParamTypes are the parameters' types. See ValueType for how each is
	// reported and encoded.
	ParamTypes() []ValueType

	// ParamNames are the parameters' names, or nil if any has none.
	ParamNames() []string

	// ResultTypes are the results' types.
	ResultTypes() []ValueType

	// ResultNames are the results' names, or nil if any has none.
	ResultNames() []string

	internalapi.WasmOnly
}

// Function is a function of an instantiated module.
type Function interface {
	// Definition describes the function.
	Definition() FunctionDefinition

	// Call calls the function with params, one uint64 for each parameter
	// and two for a v128, encoded as ValueType says, and returns its
	// results the same way. A wrong number of params is an error.
	//
	// A reference parameter other than an externref can only be passed as
	// null, zero; a non-null result of such a type is an error that names
	// CallValues, which passes and returns references of every type.
	//
	// A trap returns a *wasm.RuntimeError. If the function's module was
	// closed, or is closed during the call under the runtime's
	// WithCloseOnContextDone, the error is a *sys.ExitError. A host
	// function's error is returned as it is.
	//
	// Calls of the functions of one module may be made from any goroutine,
	// but a call must not overlap another call of the same module's
	// functions unless the program synchronizes what they share.
	Call(ctx context.Context, params ...uint64) ([]uint64, error)

	// CallWithStack is Call with the parameters and the results in stack,
	// which must be as long as the longer of the two, counted as Call
	// counts them. It allocates nothing for them.
	CallWithStack(ctx context.Context, stack []uint64) error

	// CallValues calls the function with params, one Value for each
	// parameter, and returns its results as Values. It carries every value
	// type: a v128 in one Value, and references of every kind. A parameter
	// of the wrong type is an error.
	CallValues(ctx context.Context, params ...Value) ([]Value, error)

	internalapi.WasmOnly
}

// GoModuleFunction is a host function implemented in Go, which is given the
// module that called it, to reach its memory or its exports.
//
// The stack holds the parameters, encoded as ValueType says, and is as long
// as the longer of the parameters and the results; the function writes its
// results to it from index zero. It must not keep the stack.
//
// A panic in the function ends the call: a panic with an error returns that
// error from the wasm function's Call, any other value an error describing
// it.
type GoModuleFunction interface {
	Call(ctx context.Context, mod Module, stack []uint64)
}

// GoModuleFunc is a GoModuleFunction of a func.
type GoModuleFunc func(ctx context.Context, mod Module, stack []uint64)

// Call calls f.
func (f GoModuleFunc) Call(ctx context.Context, mod Module, stack []uint64) {
	f(ctx, mod, stack)
}

// GoFunction is a host function implemented in Go that does not need the
// module that called it. Its stack is as GoModuleFunction's.
type GoFunction interface {
	Call(ctx context.Context, stack []uint64)
}

// GoFunc is a GoFunction of a func.
type GoFunc func(ctx context.Context, stack []uint64)

// Call calls f.
func (f GoFunc) Call(ctx context.Context, stack []uint64) {
	f(ctx, stack)
}

// Global is a global of an instantiated module.
//
// A mutable global is also a MutableGlobal:
//
//	if g, ok := mod.ExportedGlobal("counter").(api.MutableGlobal); ok {
//		g.Set(0)
//	}
type Global interface {
	fmt.Stringer

	// Type is the global's type.
	Type() ValueType

	// Get returns the global's value encoded as ValueType says. A v128's
	// is its low 64 bits; a reference's other than an externref is zero
	// for null and one for any other. GetValue returns any value whole.
	Get() uint64

	// GetValue returns the global's value.
	GetValue() Value

	internalapi.WasmOnly
}

// MutableGlobal is a global whose value may change.
type MutableGlobal interface {
	Global

	// Set sets the value, encoded as ValueType says. For a v128 it sets
	// the low 64 bits and clears the high ones; a reference global other
	// than an externref can only be set to null, zero, this way.
	Set(v uint64)

	// SetValue sets the value; one of the wrong type is an error.
	SetValue(v Value) error

	internalapi.WasmOnly
}

// Memory is a linear memory of an instantiated module.
//
// A memory with i64 addresses may grow past 4 GiB, which the methods taking
// uint32 offsets cannot reach: Size64, Grow64, Read64 and Write64 take and
// return uint64s and work on every memory.
//
// Every value is read and written little-endian.
type Memory interface {
	// Definition describes the memory.
	Definition() MemoryDefinition

	// Size returns the memory's size in bytes, truncated to 32 bits: a
	// memory of 4 GiB reports 0. Size64 reports any size.
	Size() uint32

	// Size64 returns the memory's size in bytes.
	Size64() uint64

	// Grow grows the memory by deltaPages pages of 64 KiB and returns its
	// size before, in pages, or false if it cannot grow so far: past its
	// maximum or the runtime's limit, or out of memory.
	Grow(deltaPages uint32) (previousPages uint32, ok bool)

	// Grow64 is Grow for any size.
	Grow64(deltaPages uint64) (previousPages uint64, ok bool)

	// ReadUint8 reads a byte at offset, or returns false if it is out of
	// range.
	ReadUint8(offset uint32) (byte, bool)

	// ReadUint16Le reads a uint16 at offset, or returns false if it is out
	// of range.
	ReadUint16Le(offset uint32) (uint16, bool)

	// ReadUint32Le reads a uint32 at offset, or returns false if it is out
	// of range.
	ReadUint32Le(offset uint32) (uint32, bool)

	// ReadFloat32Le reads a float32 at offset, or returns false if it is
	// out of range.
	ReadFloat32Le(offset uint32) (float32, bool)

	// ReadUint64Le reads a uint64 at offset, or returns false if it is out
	// of range.
	ReadUint64Le(offset uint32) (uint64, bool)

	// ReadFloat64Le reads a float64 at offset, or returns false if it is
	// out of range.
	ReadFloat64Le(offset uint32) (float64, bool)

	// Read returns the byteCount bytes at offset, or false if they are out
	// of range. The slice is a view of the memory, not a copy: a write to
	// it is seen by wasm, and wasm's writes are seen in it.
	//
	// The view stays valid when the memory grows wherever the platform
	// lets the engine reserve a memory's address space up front (64-bit
	// Linux, macOS, the BSDs and Windows); elsewhere a grow may move the
	// memory, unless the runtime is configured WithMemoryCapacityFromMax
	// and the memory has a maximum.
	Read(offset, byteCount uint32) ([]byte, bool)

	// Read64 is Read for any offset and length.
	Read64(offset, byteCount uint64) ([]byte, bool)

	// WriteUint8 writes a byte at offset, or returns false if it is out of
	// range.
	WriteUint8(offset uint32, v byte) bool

	// WriteUint16Le writes a uint16 at offset, or returns false if it is
	// out of range.
	WriteUint16Le(offset uint32, v uint16) bool

	// WriteUint32Le writes a uint32 at offset, or returns false if it is
	// out of range.
	WriteUint32Le(offset, v uint32) bool

	// WriteFloat32Le writes a float32 at offset, or returns false if it is
	// out of range.
	WriteFloat32Le(offset uint32, v float32) bool

	// WriteUint64Le writes a uint64 at offset, or returns false if it is
	// out of range.
	WriteUint64Le(offset uint32, v uint64) bool

	// WriteFloat64Le writes a float64 at offset, or returns false if it is
	// out of range.
	WriteFloat64Le(offset uint32, v float64) bool

	// Write writes v at offset, or returns false if it does not fit.
	Write(offset uint32, v []byte) bool

	// Write64 is Write for any offset.
	Write64(offset uint64, v []byte) bool

	// WriteString writes v at offset, or returns false if it does not fit.
	WriteString(offset uint32, v string) bool

	internalapi.WasmOnly
}

// Table is a table of references of an instantiated module.
type Table interface {
	// Type is the elements' type.
	Type() ValueType

	// Is64 reports a table with i64 indices.
	Is64() bool

	// Size returns the number of elements.
	Size() uint64

	// Max returns the table's declared maximum size, and false if it has
	// none.
	Max() (uint64, bool)

	// Grow adds delta elements, each init, and returns the size before. It
	// returns an error if the table would pass its maximum, or init is not
	// of the elements' type.
	Grow(delta uint64, init Value) (previousSize uint64, err error)

	// Get returns the element at index, or false if index is out of range.
	// A funcref's Ref is a Function, an externref's the host's value.
	Get(index uint64) (Value, bool)

	// Set sets the element at index. It returns an error if index is out of
	// range or v is not of the elements' type.
	Set(index uint64, v Value) error

	internalapi.WasmOnly
}

// CustomSection is a custom section of a module.
type CustomSection interface {
	// Name is the section's name.
	Name() string
	// Data is the section's bytes after its name.
	Data() []byte

	internalapi.WasmOnly
}
