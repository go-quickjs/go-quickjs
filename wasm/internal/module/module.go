package module

// ExternKind is the kind of an import or export.
type ExternKind uint8

// The import and export kinds, at their binary codes.
const (
	ExternFunc   ExternKind = 0
	ExternTable  ExternKind = 1
	ExternMemory ExternKind = 2
	ExternGlobal ExternKind = 3
	ExternTag    ExternKind = 4
)

// Name returns the kind's name as error messages print it.
func (k ExternKind) Name() string {
	switch k {
	case ExternFunc:
		return "function"
	case ExternTable:
		return "table"
	case ExternMemory:
		return "memory"
	case ExternGlobal:
		return "global"
	case ExternTag:
		return "tag"
	}
	return "unknown"
}

// Import is an entry of the import section. Index is the imported entity's
// index in its own index space (functions, tables, ...).
type Import struct {
	Module, Name string
	Kind         ExternKind
	Index        uint32
	// Offset is where the import's entry starts in the module's bytes.
	Offset uint32
}

// Export is an entry of the export section.
type Export struct {
	Name  string
	Kind  ExternKind
	Index uint32
}

// Function is an imported or defined function.
type Function struct {
	TypeIndex uint32
	Imported  bool
	Exported  bool
	// Declared reports whether the function may be the operand of ref.func in
	// a function body: exported, in an element segment, or named by ref.func
	// in a constant expression outside a function.
	Declared bool
	// Code locates a defined function's body (locals and instructions) in the
	// module's bytes.
	Code Range
}

// Range is a span of the module's bytes.
type Range struct {
	Offset, Len uint32
}

// End returns the offset just past the range.
func (r Range) End() uint32 { return r.Offset + r.Len }

// Limits are a table's or a memory's initial size and optional maximum, in
// elements or pages.
type Limits struct {
	Initial uint64
	Max     uint64
	HasMax  bool
	// Is64 reports an i64 address type: table64 or memory64.
	Is64 bool
}

// Table is an imported or defined table.
type Table struct {
	Type   ValueType
	Limits Limits
	// Init is a defined table's initializer, if it has one.
	Init     *ConstExpr
	Imported bool
	Exported bool
}

// Memory is an imported or defined memory.
type Memory struct {
	Limits   Limits
	Shared   bool
	Imported bool
	Exported bool
}

// Global is an imported or defined global.
type Global struct {
	Type     ValueType
	Mutable  bool
	Init     ConstExpr
	Imported bool
	Exported bool
}

// Tag is an imported or defined exception tag.
type Tag struct {
	TypeIndex uint32
}

// ConstKind classifies a constant expression. The common single-instruction
// forms are decoded; anything else is kept as its instructions.
type ConstKind uint8

// The kinds of constant expressions.
const (
	ConstI32 ConstKind = iota
	ConstRefFunc
	ConstRefNull
	// ConstCode is any other constant expression: Code locates its
	// instructions, up to and including the final end.
	ConstCode
)

// ConstExpr is a constant expression: a global's initializer, a segment's
// offset, an element or a table's initializer.
type ConstExpr struct {
	Kind ConstKind
	// Value is ConstI32's value or ConstRefFunc's function index.
	Value uint32
	// Heap is ConstRefNull's heap type.
	Heap HeapType
	Code Range
}

// ElemMode is an element segment's mode.
type ElemMode uint8

// The element segment modes.
const (
	ElemActive ElemMode = iota
	ElemPassive
	ElemDeclarative
)

// ElemSegment is an entry of the element section.
type ElemSegment struct {
	Mode   ElemMode
	Table  uint32
	Offset ConstExpr
	Type   ValueType
	// Elems are the segment's elements, each a ref.func for a segment of
	// function indices.
	Elems []ConstExpr
}

// DataSegment is an entry of the data section.
type DataSegment struct {
	Active bool
	Memory uint32
	Offset ConstExpr
	Init   Range
}

// CustomSection is a custom section the module carries.
type CustomSection struct {
	Name string
	Data Range
}

// Module is a decoded, validated module. Its Bytes are the module's bytes,
// which the Ranges in it locate.
type Module struct {
	Bytes []byte

	Types []TypeDef
	// Canonical holds each type's canonical index: two types of any modules
	// are the same type when their canonical indices are equal.
	Canonical []CanonicalIndex

	Imports   []Import
	Functions []Function
	Tables    []Table
	Memories  []Memory
	Globals   []Global
	Tags      []Tag
	Exports   []Export
	Elems     []ElemSegment
	Datas     []DataSegment

	NumImportedFunctions uint32
	NumImportedTables    uint32
	NumImportedMemories  uint32
	NumImportedGlobals   uint32
	NumImportedTags      uint32

	// Start is the start function's index, if HasStart.
	Start    uint32
	HasStart bool

	// DataCount is the data count section's count, if HasDataCount.
	DataCount    uint32
	HasDataCount bool

	Customs []CustomSection

	// Name is the name section's module name, if HasName.
	Name    string
	HasName bool
	// NameSection locates the first name section's payload, if any.
	NameSection Range
}

// FuncType returns function i's signature.
func (m *Module) FuncType(i uint32) *FuncType {
	return m.Types[m.Functions[i].TypeIndex].Func
}

// HasSignature reports whether type index i is a function type.
func (m *Module) HasSignature(i uint32) bool {
	return i < uint32(len(m.Types)) && m.Types[i].Kind == TypeFunc
}

// HasStruct reports whether type index i is a struct type.
func (m *Module) HasStruct(i uint32) bool {
	return i < uint32(len(m.Types)) && m.Types[i].Kind == TypeStruct
}

// HasArray reports whether type index i is an array type.
func (m *Module) HasArray(i uint32) bool {
	return i < uint32(len(m.Types)) && m.Types[i].Kind == TypeArray
}

// TagType returns tag i's signature.
func (m *Module) TagType(i uint32) *FuncType {
	return m.Types[m.Tags[i].TypeIndex].Func
}
