package decoder

// Limits are the implementation limits a module is validated against: the
// most entries of each kind it may declare, the largest sizes it may declare
// for its memories and tables, and the bounds on a function body. A module
// over one is rejected with the message that names the limit and its value.
type Limits struct {
	Types            uint32
	Functions        uint32 // defined functions
	Imports          uint32
	Exports          uint32
	Globals          uint32
	Tags             uint32
	DataSegments     uint32
	Tables           uint32
	Memories         uint32
	TableInitEntries uint32
	StructFields     uint32
	SubtypingDepth   uint32

	// Memory32Pages and Memory64Pages bound a memory's declared initial and
	// maximum sizes, in pages; TableSize and Table64Size a table's, in
	// elements, for an i32 and an i64 address type.
	Memory32Pages uint64
	Memory64Pages uint64
	TableSize     uint64
	Table64Size   uint64

	// The bounds on a function, which also bound what validating it costs.
	FunctionSize uint32 // bytes of a body
	Locals       uint32 // parameters and locals
	Params       uint32
	Returns      uint32
	BrTableSize  uint32

	ArrayNewFixedLength uint32
	ModuleSize          uint64 // bytes
}

// V8Limits returns the limits of V8 on a 64-bit host, the ones Node and
// Chrome enforce.
func V8Limits() *Limits {
	return &Limits{
		Types:               1_000_000,
		Functions:           1_000_000,
		Imports:             1_000_000,
		Exports:             1_000_000,
		Globals:             1_000_000,
		Tags:                1_000_000,
		DataSegments:        100_000,
		Tables:              100_000,
		Memories:            100_000,
		TableInitEntries:    10_000_000,
		StructFields:        10_000,
		SubtypingDepth:      63,
		Memory32Pages:       65_536,
		Memory64Pages:       262_144,
		TableSize:           10_000_000,
		Table64Size:         10_000_000,
		FunctionSize:        7_654_321,
		Locals:              50_000,
		Params:              1_000,
		Returns:             1_000,
		BrTableSize:         65_520,
		ArrayNewFixedLength: 10_000,
		ModuleSize:          1 << 30,
	}
}

// JSAPILimits returns the limits of the WebAssembly JavaScript interface:
// V8's, but for a memory64's declared size of up to 2^37-1 pages and at most
// 100 memories.
func JSAPILimits() *Limits {
	l := V8Limits()
	l.Memory64Pages = 1<<37 - 1
	l.Memories = 100
	return l
}

// CoreLimits returns limits for the core specification alone, which has
// none of the interface's: counts up to 1<<27, memories and tables up to the
// largest their address types allow. The bounds on a function body stay
// V8's, since they also bound what validating a function costs.
func CoreLimits() *Limits {
	const count = 1 << 27
	l := V8Limits()
	l.Types = count
	l.Functions = count
	l.Imports = count
	l.Exports = count
	l.Globals = count
	l.Tags = count
	l.DataSegments = count
	l.Tables = count
	l.Memories = count
	l.TableInitEntries = count
	l.StructFields = count
	l.SubtypingDepth = count
	l.BrTableSize = count
	l.ArrayNewFixedLength = count
	l.Memory32Pages = 65_536
	l.Memory64Pages = 1 << 48
	l.TableSize = 1<<32 - 1
	l.Table64Size = 1<<64 - 1
	l.ModuleSize = 1<<63 - 1
	return l
}
