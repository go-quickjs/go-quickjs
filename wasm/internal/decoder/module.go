package decoder

import (
	"bytes"
	"fmt"
	"sort"
	"unicode/utf8"

	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
)

// Section codes. The custom sections V8 knows by name get codes after the
// standard ones, as V8 numbers them.
const (
	secUnknown   = 0
	secType      = 1
	secImport    = 2
	secFunction  = 3
	secTable     = 4
	secMemory    = 5
	secGlobal    = 6
	secExport    = 7
	secStart     = 8
	secElement   = 9
	secCode      = 10
	secData      = 11
	secDataCount = 12
	secTag       = 13
	secStringRef = 14

	secName = 15 + iota - 15
	secSourceMappingURL
	secDebugInfo
	secExternalDebugInfo
	secBuildID
	secInstTrace
	secBranchHints
	secCompilationPriority
	secInstFrequencies
	secCallTargets
	secDescriptors

	firstUnorderedSection = secDataCount
	lastKnownSection      = secStringRef
)

// sectionName returns V8's name for a section code.
func sectionName(code int) string {
	switch code {
	case secUnknown:
		return "Unknown"
	case secType:
		return "Type"
	case secImport:
		return "Import"
	case secFunction:
		return "Function"
	case secTable:
		return "Table"
	case secMemory:
		return "Memory"
	case secGlobal:
		return "Global"
	case secExport:
		return "Export"
	case secStart:
		return "Start"
	case secCode:
		return "Code"
	case secElement:
		return "Element"
	case secData:
		return "Data"
	case secTag:
		return "Tag"
	case secStringRef:
		return "StringRef"
	case secDataCount:
		return "DataCount"
	case secName:
		return "name"
	case secSourceMappingURL:
		return "sourceMappingURL"
	case secDebugInfo:
		return ".debug_info"
	case secExternalDebugInfo:
		return "external_debug_info"
	case secBuildID:
		return "build_id"
	case secInstTrace:
		return "metadata.code.trace_inst"
	case secBranchHints:
		return "metadata.code.branch_hint"
	case secCompilationPriority:
		return "metadata.code.compilation_priority"
	case secDescriptors:
		return "experimental-descriptors"
	}
	return "<unknown>"
}

// customSectionCodes are the custom sections V8 identifies by name.
var customSectionCodes = map[string]int{
	"name":                               secName,
	"sourceMappingURL":                   secSourceMappingURL,
	"metadata.code.trace_inst":           secInstTrace,
	"metadata.code.branch_hint":          secBranchHints,
	"metadata.code.compilation_priority": secCompilationPriority,
	"metadata.code.instr_freq":           secInstFrequencies,
	"metadata.code.call_targets":         secCallTargets,
	".debug_info":                        secDebugInfo,
	"external_debug_info":                secExternalDebugInfo,
	"build_id":                           secBuildID,
	"experimental-descriptors":           secDescriptors,
}

// consumeString reads a length-prefixed string, which must be valid UTF-8
// if utf8Required, returning its range.
func consumeString(d *Decoder, utf8Required bool, name string) module.Range {
	length := d.consumeU32v("length")
	offset := d.pc
	if length > 0 {
		start := d.pc
		d.consumeBytes(length)
		if d.ok() && utf8Required && !utf8.Valid(d.b[start:start+int(length)]) {
			d.errorf(start, "%s: no valid UTF-8 string", name)
		}
	}
	if d.failed() {
		return module.Range{Offset: uint32(offset)}
	}
	return module.Range{Offset: uint32(offset), Len: length}
}

// sectionIterator walks the sections of a module, as V8's
// WasmSectionIterator does, on a decoder of its own.
type sectionIterator struct {
	d                                  *Decoder
	code                               int
	sectionStart, payloadStart, secEnd int
	// custom reports a custom section, named customName.
	custom     bool
	customName module.Range
}

func newSectionIterator(d *Decoder) *sectionIterator {
	it := &sectionIterator{d: d, sectionStart: d.pc, secEnd: d.pc}
	it.next()
	return it
}

func (it *sectionIterator) more() bool { return it.d.ok() && it.d.more() }

// advance moves to the next section, checking that the current one was
// decoded exactly.
func (it *sectionIterator) advance(toEnd bool) {
	d := it.d
	if toEnd && d.pc < it.secEnd {
		d.consumeBytes(uint32(it.secEnd - d.pc))
	}
	if d.pc != it.secEnd {
		msg := "longer"
		if d.pc < it.secEnd {
			msg = "shorter"
		}
		d.errorf(d.pc, "section was %s than expected size (%d bytes expected, %d decoded)",
			msg, it.secEnd-it.sectionStart, d.pc-it.sectionStart)
	}
	it.next()
}

func (it *sectionIterator) next() {
	d := it.d
	if !d.more() {
		it.code = secUnknown
		return
	}
	it.sectionStart = d.pc
	code := int(d.consumeU8("section kind"))
	length := d.consumeU32v("section length")
	it.payloadStart = d.pc
	it.secEnd = it.payloadStart + int(length)
	if uint64(length) > uint64(d.end-d.pc) {
		d.errorf(it.sectionStart, "section (code %d, \"%s\") extends past end of the module (length %d, remaining bytes %d)",
			code, sectionName(code), length, d.end-d.pc)
		it.secEnd = it.payloadStart
	}
	it.custom = code == secUnknown
	if code == secUnknown {
		// Read the custom section's name within the section's bounds.
		moduleEnd := d.end
		d.end = it.secEnd
		name := consumeString(d, true, "section name")
		it.customName = name
		if d.ok() {
			d.end = moduleEnd
			if c, ok := customSectionCodes[string(d.b[name.Offset:name.End()])]; ok {
				code = c
			}
		}
		it.payloadStart = d.pc
	} else if code > lastKnownSection {
		d.errorf(d.pc, "unknown section code #0x%02x", code)
	}
	if d.failed() {
		it.code = secUnknown
	} else {
		it.code = code
	}
	if it.code == secUnknown && it.secEnd > d.pc {
		d.consumeBytes(uint32(it.secEnd - d.pc))
	}
}

// moduleDecoder decodes a module's sections into a module.Module, as V8's
// ModuleDecoderImpl does.
type moduleDecoder struct {
	Decoder
	m                    *module.Module
	det                  detected
	nextOrdered          int
	seenUnordered        uint32
	numDeclaredFunctions uint32
	codeSeen             bool
	numDeclaredData      uint32
	customs              []module.CustomSection
}

// Decode decodes the module in b and validates everything but its function
// bodies, returning V8's first error if there is one. ValidateFunctions
// validates the bodies.
func Decode(b []byte) (*module.Module, *Error) {
	if len(b) > maxModuleSize {
		return nil, &Error{Offset: 0, Msg: fmt.Sprintf("size > maximum module size (%d): %d", maxModuleSize, len(b))}
	}
	md := &moduleDecoder{m: &module.Module{Bytes: b}}
	md.b = b
	md.end = len(b)
	md.stopOnError = true
	md.nextOrdered = secType
	md.decodeHeader()
	if md.failed() {
		return nil, md.err
	}
	itd := newDecoder(b, 8, len(b))
	it := newSectionIterator(itd)
	for md.ok() {
		if it.custom && itd.ok() {
			md.customs = append(md.customs, module.CustomSection{
				Name: string(b[it.customName.Offset:it.customName.End()]),
				Data: module.Range{Offset: uint32(it.payloadStart), Len: uint32(it.secEnd - it.payloadStart)},
			})
		}
		if it.code != secUnknown {
			md.decodeSection(it.code, it.payloadStart, it.secEnd)
			if md.failed() {
				break
			}
		}
		if !it.more() {
			break
		}
		it.advance(true)
	}
	if itd.failed() {
		return nil, itd.err
	}
	if md.ok() {
		md.checkMismatchedCounts()
	}
	if md.failed() {
		return nil, md.err
	}
	md.m.Customs = md.customs
	return md.m, nil
}

func (md *moduleDecoder) decodeHeader() {
	pos := md.pc
	magic := md.consumeU32("wasm magic")
	const wasmMagic = 0x6d736100
	if magic != wasmMagic {
		md.errorf(pos, "expected magic word %02x %02x %02x %02x, found %02x %02x %02x %02x",
			0x00, 0x61, 0x73, 0x6d, magic&0xff, magic>>8&0xff, magic>>16&0xff, magic>>24)
	}
	pos = md.pc
	version := md.consumeU32("wasm version")
	if version != 1 {
		md.errorf(pos, "expected version %02x %02x %02x %02x, found %02x %02x %02x %02x",
			1, 0, 0, 0, version&0xff, version>>8&0xff, version>>16&0xff, version>>24)
	}
}

func (md *moduleDecoder) hasSeen(code int) bool { return md.seenUnordered&(1<<code) != 0 }
func (md *moduleDecoder) setSeen(code int)      { md.seenUnordered |= 1 << code }

func (md *moduleDecoder) checkSectionOrder(code int) bool {
	if code >= secType && code < firstUnorderedSection {
		if code < md.nextOrdered {
			md.errorf(md.pc, "unexpected section <%s>", sectionName(code))
			return false
		}
		md.nextOrdered = code + 1
		return true
	}
	if code == secUnknown || code > lastKnownSection {
		return true
	}
	if md.hasSeen(code) {
		md.errorf(md.pc, "Multiple %s sections not allowed", sectionName(code))
		return false
	}
	md.setSeen(code)
	checkOrder := func(before, after int) bool {
		if md.nextOrdered > after {
			md.errorf(md.pc, "The %s section must appear before the %s section", sectionName(code), sectionName(after))
			return false
		}
		if md.nextOrdered <= before {
			md.nextOrdered = before + 1
		}
		return true
	}
	switch code {
	case secDataCount:
		return checkOrder(secElement, secCode)
	case secTag, secStringRef:
		return checkOrder(secMemory, secGlobal)
	}
	return true
}

func (md *moduleDecoder) decodeSection(code, start, end int) {
	if md.failed() {
		return
	}
	md.pc, md.end = start, end
	if !md.checkSectionOrder(code) {
		return
	}
	switch code {
	case secType:
		md.decodeTypeSection()
	case secImport:
		md.decodeImportSection()
	case secFunction:
		md.decodeFunctionSection()
	case secTable:
		md.decodeTableSection()
	case secMemory:
		md.decodeMemorySection()
	case secGlobal:
		md.decodeGlobalSection()
	case secExport:
		md.decodeExportSection()
	case secStart:
		md.decodeStartSection()
	case secCode:
		md.decodeCodeSection()
	case secElement:
		md.decodeElementSection()
	case secData:
		md.decodeDataSection()
	case secDataCount:
		md.numDeclaredData = md.consumeCount("data segments count", maxDataSegments)
		md.m.DataCount = md.numDeclaredData
		md.m.HasDataCount = true
	case secTag:
		md.decodeTagSection()
	case secStringRef:
		md.errorf(md.pc, "unexpected section <%s> (enable with --experimental-wasm-stringref)", sectionName(code))
	case secName:
		md.decodeNameSection(start, end)
	default:
		// The other custom sections V8 knows are optional and lenient: their
		// errors do not fail the module.
		md.pc = end
	}
	if md.pc != end {
		msg := "longer"
		if md.pc < end {
			msg = "shorter"
		}
		md.errorf(md.pc, "section was %s than expected size (%d bytes expected, %d decoded)", msg, end-start, md.pc-start)
	}
}

func (md *moduleDecoder) consumeCount(name string, maximum int) uint32 {
	p := md.pc
	count := md.consumeU32v(name)
	if uint64(count) > uint64(maximum) {
		md.errorf(p, "%s of %d exceeds internal limit of %d", name, count, maximum)
		return 0
	}
	return count
}

func (md *moduleDecoder) consumeIndex(name string, size int) (uint32, bool) {
	pos := md.pc
	index := md.consumeU32v("index")
	if uint64(index) >= uint64(size) {
		md.errorf(pos, "%s index %d out of bounds (%d %s)", name, index, size, entries(size))
		return 0, false
	}
	return index, true
}

// entries returns "entry" or "entries", as V8 says it after a count.
func entries(n int) string {
	if n == 1 {
		return "entry"
	}
	return "entries"
}

// memories returns "memory" or "memories", as V8 says it after a count.
func memories(n int) string {
	if n == 1 {
		return "memory"
	}
	return "memories"
}

func (md *moduleDecoder) consumeValueType() module.ValueType {
	t, length := readValueType(&md.Decoder, md.pc, &md.det)
	validateValueType(&md.Decoder, md.pc, md.m, t)
	md.consumeBytes(uint32(length))
	return t
}

func (md *moduleDecoder) consumeHeapType() module.HeapType {
	h, length := readHeapType(&md.Decoder, md.pc, &md.det)
	validateHeapType(&md.Decoder, md.pc, md.m, h)
	md.consumeBytes(uint32(length))
	return h
}

func (md *moduleDecoder) consumeStorageType() module.ValueType {
	switch md.readU8(md.pc, "expected 1 byte") {
	case codeI8:
		md.consumeBytes(1)
		return module.I8
	case codeI16:
		md.consumeBytes(1)
		return module.I16
	}
	return md.consumeValueType()
}

func (md *moduleDecoder) consumeMutability() bool {
	v := md.consumeU8("mutability")
	if v > 1 {
		md.errorAt(md.pc-1, "invalid mutability")
	}
	return v != 0
}

func (md *moduleDecoder) consumeSig() *module.FuncType {
	paramCount := md.consumeCount("param count", maxFunctionParams)
	params := make([]module.ValueType, paramCount)
	for i := range params {
		params[i] = md.consumeValueType()
	}
	returnCount := md.consumeCount("return count", maxFunctionReturns)
	results := make([]module.ValueType, returnCount)
	for i := range results {
		results[i] = md.consumeValueType()
	}
	return &module.FuncType{Params: params, Results: results}
}

func (md *moduleDecoder) consumeStruct() *module.StructType {
	count := md.consumeCount(", field count", maxStructFields)
	if md.failed() {
		return nil
	}
	fields := make([]module.FieldType, count)
	for i := uint32(0); md.ok() && i < count; i++ {
		fields[i].Type = md.consumeStorageType()
		fields[i].Mutable = md.consumeMutability()
	}
	if md.failed() {
		return nil
	}
	return &module.StructType{Fields: fields}
}

func (md *moduleDecoder) consumeArray() *module.ArrayType {
	elem := md.consumeStorageType()
	mut := md.consumeMutability()
	if md.failed() {
		return nil
	}
	return &module.ArrayType{Elem: module.FieldType{Type: elem, Mutable: mut}}
}

func (md *moduleDecoder) consumeBaseTypeDefinition() (module.TypeDef, bool) {
	kind := md.consumeU8(" kind")
	switch kind {
	case codeFuncType:
		sig := md.consumeSig()
		if md.failed() {
			return module.TypeDef{}, false
		}
		return module.TypeDef{Kind: module.TypeFunc, Func: sig, Super: module.NoSuper, Final: true}, true
	case codeStructType:
		st := md.consumeStruct()
		if st == nil {
			return module.TypeDef{}, false
		}
		return module.TypeDef{Kind: module.TypeStruct, Struct: st, Super: module.NoSuper, Final: true}, true
	case codeArrayType:
		at := md.consumeArray()
		if at == nil {
			return module.TypeDef{}, false
		}
		return module.TypeDef{Kind: module.TypeArray, Array: at, Super: module.NoSuper, Final: true}, true
	case codeContType:
		md.errorAt(md.pc-1, "core stack switching not enabled (enable with --experimental-wasm-wasmfx)")
		return module.TypeDef{}, false
	}
	md.errorf(md.pc-1, "unknown type form: %d", kind)
	return module.TypeDef{}, false
}

// consumeDescribingType reads the composite type after a sub clause; the
// shared, describes and descriptor prefixes are features Node does not enable
// by default, rejected as V8 rejects them.
func (md *moduleDecoder) consumeDescribingType() (module.TypeDef, bool) {
	kind := md.readU8(md.pc, "type kind")
	switch kind {
	case codeShared:
		md.errorf(md.pc-1, "unknown type form: %d, enable with --experimental-wasm-shared", kind)
		return module.TypeDef{}, false
	case codeDescribes, codeDescriptor:
		md.errorAt(md.pc, "descriptor types need --experimental-wasm-custom-descriptors")
		return module.TypeDef{}, false
	}
	return md.consumeBaseTypeDefinition()
}

func (md *moduleDecoder) consumeSubtypeDefinition(current uint32) (module.TypeDef, bool) {
	kind := md.readU8(md.pc, "type kind")
	if kind != codeSubtype && kind != codeSubFinal {
		return md.consumeDescribingType()
	}
	final := kind == codeSubFinal
	md.consumeBytes(1)
	count := md.consumeCount("supertype count", 1)
	super := module.NoSuper
	if count == 1 {
		super = md.consumeU32v("supertype")
		if super >= current {
			md.errorf(md.pc, "type %d: invalid supertype %d", current, super)
			return module.TypeDef{}, false
		}
	}
	t, ok := md.consumeDescribingType()
	if !ok {
		return t, false
	}
	t.Super = super
	t.Final = final
	return t, true
}

func (md *moduleDecoder) decodeTypeSection() {
	count := md.consumeCount("types count", maxTypes)
	m := md.m
	for i := uint32(0); md.ok() && i < count; i++ {
		kind := md.readU8(md.pc, "type kind")
		initial := uint32(len(m.Types))
		groupSize := uint32(1)
		if kind == codeRecGroup {
			md.consumeBytes(1)
			groupSize = md.consumeCount("recursive group size", maxTypes)
		}
		if uint64(initial)+uint64(groupSize) > maxTypes {
			md.errorf(md.pc, "Type definition count exceeds maximum %d", maxTypes)
			return
		}
		// The group's types are visible to its definitions.
		for j := uint32(0); j < groupSize; j++ {
			m.Types = append(m.Types, module.TypeDef{})
			m.Canonical = append(m.Canonical, 0)
		}
		for j := uint32(0); j < groupSize; j++ {
			t, ok := md.consumeSubtypeDefinition(initial + j)
			if !ok || md.failed() {
				return
			}
			t.RecStart, t.RecSize = initial, groupSize
			m.Types[initial+j] = t
		}
		md.finalizeRecGroup(initial, groupSize)
	}
}

func (md *moduleDecoder) finalizeRecGroup(start, size uint32) {
	m := md.m
	module.Canonicalize(m, start, size)
	for i := start; md.ok() && i < start+size; i++ {
		t := &m.Types[i]
		if t.Super == module.NoSuper {
			continue
		}
		depth := m.Types[t.Super].Depth + 1
		t.Depth = depth
		if depth > maxSubtypingDepth {
			md.errorf(md.pc, "type %d: subtyping depth is greater than allowed", i)
			return
		}
		if m.Types[t.Super].Final {
			md.errorf(md.pc, "type %d extends final type %d", i, t.Super)
			return
		}
		if !module.ValidSubtypeDefinition(i, t.Super, m) {
			md.errorf(md.pc, "type %d has invalid explicit supertype %d", i, t.Super)
			return
		}
	}
}

func (md *moduleDecoder) consumeSigIndex() (uint32, bool) {
	pos := md.pc
	index := md.consumeU32v("signature index")
	if !md.m.HasSignature(index) {
		md.errorf(pos, "no signature at index %d (%d types)", index, len(md.m.Types))
		return 0, false
	}
	return index, true
}

func (md *moduleDecoder) consumeTagSigIndex() uint32 {
	pos := md.pc
	index, ok := md.consumeSigIndex()
	if ok && len(md.m.Types[index].Func.Results) != 0 {
		md.errorf(pos, "tag signature %d has non-void return", index)
		return 0
	}
	return index
}

func (md *moduleDecoder) consumeExceptionAttribute() {
	pos := md.pc
	attr := md.consumeU32v("exception attribute")
	if attr != exceptionAttrib {
		md.errorf(pos, "exception attribute %d not supported", attr)
	}
}

// consumeLimitsByte reads a table's or a memory's limits flags.
func (md *moduleDecoder) consumeLimitsByte(memory bool) byte {
	name, what := "table limits flags", "table"
	if memory {
		name, what = "memory limits flags", "memory"
	}
	flags := md.consumeU8(name)
	if flags&^7 != 0 {
		md.errorf(md.pc-1, "invalid %s limits flags 0x%x", what, flags)
	}
	if flags&2 != 0 {
		if memory {
			if flags&1 == 0 {
				md.errorAt(md.pc-1, "shared memory must have a maximum defined")
			}
		} else {
			md.errorAt(md.pc-1, "invalid table limits flags")
		}
	}
	return flags
}

// consumeResizableLimits reads an initial size and, if hasMax, a maximum, as
// 64-bit LEBs if is64.
func (md *moduleDecoder) consumeResizableLimits(name, units string, maxInitial uint64, hasMax bool, maxMaximum uint64, is64 bool) module.Limits {
	pos := md.pc
	var initial uint64
	if is64 {
		initial = md.consumeU64v("initial size")
	} else {
		initial = uint64(md.consumeU32v("initial size"))
	}
	if initial > maxInitial {
		md.errorf(pos, "initial %s size (%d %s) is larger than implementation limit (%d %s)", name, initial, units, maxInitial, units)
	}
	l := module.Limits{Initial: initial, HasMax: hasMax, Is64: is64}
	if hasMax {
		pos = md.pc
		var maximum uint64
		if is64 {
			maximum = md.consumeU64v("maximum size")
		} else {
			maximum = uint64(md.consumeU32v("maximum size"))
		}
		if maximum > maxMaximum {
			md.errorf(pos, "maximum %s size (%d %s) is larger than implementation limit (%d %s)", name, maximum, units, maxMaximum, units)
		}
		if maximum < uint64(uint32(initial)) {
			md.errorf(pos, "maximum %s size (%d %s) is less than initial (%d %s)", name, maximum, units, uint32(initial), units)
		}
		l.Max = maximum
	}
	return l
}

func (md *moduleDecoder) consumeTableLimits(t module.ValueType) module.Limits {
	flags := md.consumeLimitsByte(false)
	return md.consumeResizableLimits("table", "elements", maxTableSize, flags&1 != 0, ^uint64(0), flags&4 != 0)
}

func (md *moduleDecoder) consumeMemory() module.Memory {
	flags := md.consumeLimitsByte(true)
	is64 := flags&4 != 0
	maxPages := uint64(maxMemory32Pages)
	if is64 {
		maxPages = maxMemory64Pages
	}
	l := md.consumeResizableLimits("memory", "pages", maxPages, flags&1 != 0, maxPages, is64)
	return module.Memory{Limits: l, Shared: flags&2 != 0}
}

func (md *moduleDecoder) consumeGlobalFlags() bool {
	flags := md.consumeU8("global flags")
	if flags&^3 != 0 || flags&2 != 0 {
		md.errorf(md.pc-1, "invalid global flags 0x%x", flags)
		return false
	}
	return flags&1 != 0
}

func (md *moduleDecoder) decodeImportSection() {
	m := md.m
	count := md.consumeCount("imports count", maxImports)
	for i := uint32(0); md.ok() && i < count; i++ {
		pos := md.pc
		modName := consumeString(&md.Decoder, true, "module name")
		fieldName := consumeString(&md.Decoder, true, "field name")
		kind := md.consumeU8("kind")
		imp := module.Import{
			Module: string(md.b[modName.Offset:modName.End()]),
			Name:   string(md.b[fieldName.Offset:fieldName.End()]),
			Kind:   module.ExternKind(kind),
			Offset: uint32(pos),
		}
		switch kind {
		case byte(module.ExternFunc):
			imp.Index = uint32(len(m.Functions))
			m.NumImportedFunctions++
			sig, _ := md.consumeSigIndex()
			m.Functions = append(m.Functions, module.Function{TypeIndex: sig, Imported: true})
		case byte(module.ExternFunc) | 0x20:
			// An exact function import.
			md.errorf(md.pc-1, "Invalid import kind %d, enable with --experimental-wasm-custom-descriptors", kind)
		case byte(module.ExternTable):
			imp.Index = uint32(len(m.Tables))
			typePos := md.pc
			t := md.consumeValueType()
			if md.ok() && !t.IsRef() {
				md.errorf(typePos, "Invalid table type %s", t.Name())
				break
			}
			m.NumImportedTables++
			l := md.consumeTableLimits(t)
			m.Tables = append(m.Tables, module.Table{Type: t, Limits: l, Imported: true})
		case byte(module.ExternMemory):
			if len(m.Memories) >= maxMemories-1 {
				md.errorf(md.pc, "At most %d imported memories are supported", maxMemories)
				break
			}
			imp.Index = uint32(len(m.Memories))
			mem := md.consumeMemory()
			mem.Imported = true
			m.NumImportedMemories++
			m.Memories = append(m.Memories, mem)
		case byte(module.ExternGlobal):
			imp.Index = uint32(len(m.Globals))
			t := md.consumeValueType()
			mut := md.consumeGlobalFlags()
			if md.failed() {
				break
			}
			m.Globals = append(m.Globals, module.Global{Type: t, Mutable: mut, Imported: true})
			m.NumImportedGlobals++
		case byte(module.ExternTag):
			imp.Index = uint32(len(m.Tags))
			m.NumImportedTags++
			md.consumeExceptionAttribute()
			sig := md.consumeTagSigIndex()
			m.Tags = append(m.Tags, module.Tag{TypeIndex: sig})
		default:
			md.errorf(pos, "unknown import kind 0x%02x", kind)
		}
		m.Imports = append(m.Imports, imp)
	}
}

func (md *moduleDecoder) decodeFunctionSection() {
	m := md.m
	count := md.consumeCount("functions count", maxDefinedFunctions)
	md.numDeclaredFunctions = count
	for i := uint32(0); i < count; i++ {
		sig, _ := md.consumeSigIndex()
		m.Functions = append(m.Functions, module.Function{TypeIndex: sig})
		if md.failed() {
			return
		}
	}
}

func (md *moduleDecoder) decodeTableSection() {
	m := md.m
	count := md.consumeCount("table count", maxTables)
	for i := uint32(0); md.ok() && i < count; i++ {
		typePos := md.pc
		hasInit := false
		if md.readU8(md.pc, "table-with-initializer byte") == 0x40 {
			md.consumeBytes(1)
			hasInit = true
			typePos++
			reserved := md.consumeU8("reserved-byte")
			if reserved != 0 {
				md.errorAt(typePos, "Reserved byte must be 0x00")
				break
			}
			typePos++
		}
		t := md.consumeValueType()
		if md.ok() && !t.IsRef() {
			md.errorAt(typePos, "Only reference types can be used as table types")
			break
		}
		if md.ok() && !hasInit && !t.IsDefaultable() {
			md.errorf(typePos, "Table of non-defaultable table %s needs initial value", t.Name())
			break
		}
		l := md.consumeTableLimits(t)
		table := module.Table{Type: t, Limits: l}
		if hasInit {
			init := md.consumeInitExpr(t)
			table.Init = &init
		}
		m.Tables = append(m.Tables, table)
	}
}

func (md *moduleDecoder) decodeMemorySection() {
	m := md.m
	countPos := md.pc
	count := md.consumeCount("memory count", maxMemories)
	imported := len(m.Memories)
	if uint64(count) > uint64(maxMemories-imported) {
		md.errorf(countPos, "Exceeding maximum number of memories (%d; declared %d, imported %d)", maxMemories, count, imported)
	}
	for i := uint32(0); md.ok() && i < count; i++ {
		m.Memories = append(m.Memories, md.consumeMemory())
	}
}

func (md *moduleDecoder) decodeGlobalSection() {
	m := md.m
	count := md.consumeCount("globals count", maxGlobals)
	for i := uint32(0); md.ok() && i < count; i++ {
		t := md.consumeValueType()
		mut := md.consumeGlobalFlags()
		if md.failed() {
			return
		}
		init := md.consumeInitExpr(t)
		m.Globals = append(m.Globals, module.Global{Type: t, Mutable: mut, Init: init})
	}
}

func (md *moduleDecoder) decodeExportSection() {
	m := md.m
	count := md.consumeCount("exports count", maxExports)
	names := make([]module.Range, 0, count)
	for i := uint32(0); md.ok() && i < count; i++ {
		name := consumeString(&md.Decoder, true, "field name")
		kindPos := md.pc
		kind := md.consumeU8("kind")
		exp := module.Export{Name: string(md.b[name.Offset:name.End()]), Kind: module.ExternKind(kind)}
		switch kind {
		case byte(module.ExternFunc):
			idx, ok := md.consumeIndex("function", len(m.Functions))
			if ok {
				exp.Index = idx
				m.Functions[idx].Exported = true
				m.Functions[idx].Declared = true
			}
		case byte(module.ExternTable):
			idx, ok := md.consumeIndex("table", len(m.Tables))
			if ok {
				exp.Index = idx
				m.Tables[idx].Exported = true
			}
		case byte(module.ExternMemory):
			pos := md.pc
			idx := md.consumeU32v("memory index")
			exp.Index = idx
			if uint64(idx) >= uint64(len(m.Memories)) {
				md.errorf(pos, "invalid exported memory index %d (having %d %s)", idx, len(m.Memories), memories(len(m.Memories)))
				break
			}
			m.Memories[idx].Exported = true
		case byte(module.ExternGlobal):
			idx, ok := md.consumeIndex("global", len(m.Globals))
			if ok {
				exp.Index = idx
				m.Globals[idx].Exported = true
			}
		case byte(module.ExternTag):
			idx, _ := md.consumeIndex("tag", len(m.Tags))
			exp.Index = idx
		default:
			md.errorf(kindPos, "invalid export kind 0x%02x", kind)
		}
		m.Exports = append(m.Exports, exp)
		names = append(names, name)
	}
	if md.failed() || len(m.Exports) < 2 {
		return
	}
	// Report a duplicate at the later of the first equal pair, in V8's order:
	// by length, then by bytes.
	order := make([]int, len(m.Exports))
	for i := range order {
		order[i] = i
	}
	less := func(a, b int) bool {
		na, nb := names[a], names[b]
		if na.Len != nb.Len {
			return na.Len < nb.Len
		}
		return bytes.Compare(md.b[na.Offset:na.End()], md.b[nb.Offset:nb.End()]) < 0
	}
	sort.SliceStable(order, func(i, j int) bool { return less(order[i], order[j]) })
	for k := 1; k < len(order); k++ {
		last, cur := order[k-1], order[k]
		if !less(last, cur) {
			n := names[cur]
			md.errorf(int(n.Offset), "Duplicate export name '%s' for %s %d and %s %d",
				truncated(md.b[n.Offset:n.End()]), m.Exports[last].Kind.Name(), m.Exports[last].Index,
				m.Exports[cur].Kind.Name(), m.Exports[cur].Index)
			return
		}
	}
}

// truncated returns a name as V8's TruncatedUserString prints it: at most 50
// bytes, the last three of a longer one replaced by "...".
func truncated(b []byte) string {
	const maxLen = 50
	if len(b) <= maxLen {
		return string(b)
	}
	return string(b[:maxLen-3]) + "..."
}

func (md *moduleDecoder) decodeStartSection() {
	m := md.m
	pos := md.pc
	idx, ok := md.consumeIndex("function", len(m.Functions))
	if !ok {
		return
	}
	m.Start, m.HasStart = idx, true
	sig := m.FuncType(idx)
	if len(sig.Params) > 0 || len(sig.Results) > 0 {
		md.errorAt(pos, "invalid start function: non-zero parameter or return count")
	}
}

func (md *moduleDecoder) decodeElementSection() {
	m := md.m
	count := md.consumeCount("segment count", maxTableSize)
	for i := uint32(0); i < count; i++ {
		seg, n, exprs := md.consumeElementSegmentHeader()
		if md.failed() {
			return
		}
		seg.Elems = make([]module.ConstExpr, 0, min(n, 1024))
		for j := uint32(0); j < n; j++ {
			if exprs {
				seg.Elems = append(seg.Elems, md.consumeInitExpr(seg.Type))
			} else {
				seg.Elems = append(seg.Elems, md.consumeElementFuncIndex(seg.Type))
			}
			if md.failed() {
				return
			}
		}
		m.Elems = append(m.Elems, seg)
	}
}

// consumeElementSegmentHeader reads an element segment up to its elements,
// returning the segment, its element count, and whether its elements are
// expressions rather than function indices.
func (md *moduleDecoder) consumeElementSegmentHeader() (module.ElemSegment, uint32, bool) {
	const (
		nonActive          = 1 << 0
		tableOrDeclarative = 1 << 1
		expressions        = 1 << 2
		sharedFlag         = 1 << 3
		fullMask           = nonActive | tableOrDeclarative | expressions | sharedFlag
	)
	m := md.m
	pos := md.pc
	flag := md.consumeU32v("flag")
	if flag&fullMask != flag || flag&sharedFlag != 0 {
		md.errorf(pos, "illegal flag value %d", flag)
		return module.ElemSegment{}, 0, false
	}
	seg := module.ElemSegment{Mode: module.ElemActive}
	if flag&nonActive != 0 {
		seg.Mode = module.ElemPassive
		if flag&tableOrDeclarative != 0 {
			seg.Mode = module.ElemDeclarative
		}
	}
	active := seg.Mode == module.ElemActive
	exprs := flag&expressions != 0
	hasTableIndex := active && flag&tableOrDeclarative != 0
	if hasTableIndex {
		seg.Table = md.consumeU32v(", table index")
	}
	if active && uint64(seg.Table) >= uint64(len(m.Tables)) {
		implicit := " implicit"
		if hasTableIndex {
			implicit = ""
		}
		md.errorf(pos, "out of bounds%s table index %d", implicit, seg.Table)
		return module.ElemSegment{}, 0, false
	}
	var tableType module.ValueType
	if active {
		tableType = m.Tables[seg.Table].Type
		offsetType := module.I32
		if m.Tables[seg.Table].Limits.Is64 {
			offsetType = module.I64
		}
		seg.Offset = md.consumeInitExpr(offsetType)
		if md.failed() {
			return module.ElemSegment{}, 0, false
		}
	}
	// An active segment without a table index has neither a type nor an
	// element kind.
	compat := active && flag&tableOrDeclarative == 0
	if exprs {
		if compat {
			seg.Type = module.FuncRef
		} else {
			seg.Type = md.consumeValueType()
			if md.failed() {
				return module.ElemSegment{}, 0, false
			}
		}
	} else {
		if !compat {
			kind := md.consumeU8("element type: function")
			if kind != byte(module.ExternFunc) {
				md.errorf(pos, "illegal element kind 0x%x. Must be 0x%x", kind, module.ExternFunc)
				return module.ElemSegment{}, 0, false
			}
		}
		seg.Type = module.Ref(module.HeapFunc)
	}
	if active && !module.IsSubtypeOf(seg.Type, tableType, m) {
		md.errorf(pos, "Element segment of type %s is not a subtype of referenced table %d (of type %s)",
			seg.Type.Name(), seg.Table, tableType.Name())
		return module.ElemSegment{}, 0, false
	}
	n := md.consumeCount(" number of elements", maxTableInitEntries)
	return seg, n, exprs
}

// consumeElementFuncIndex reads an element given as a function index, as a
// ref.func.
func (md *moduleDecoder) consumeElementFuncIndex(expected module.ValueType) module.ConstExpr {
	m := md.m
	pos := md.pc
	idx, ok := md.consumeIndex("function", len(m.Functions))
	if !ok {
		return module.ConstExpr{}
	}
	entry := module.Ref(module.HeapIndex(m.Functions[idx].TypeIndex))
	if expected != module.FuncRef && !module.IsSubtypeOf(entry, expected, m) {
		md.errorf(pos, "Invalid type in element entry: expected %s, got %s instead.", expected.Name(), entry.Name())
		return module.ConstExpr{}
	}
	m.Functions[idx].Declared = true
	return module.ConstExpr{Kind: module.ConstRefFunc, Value: idx}
}

func (md *moduleDecoder) decodeCodeSection() {
	m := md.m
	sectionStart := md.pc
	count := md.consumeU32v("functions count")
	if count != md.numDeclaredFunctions {
		md.errorf(sectionStart, "function body count %d mismatch (%d expected)", count, md.numDeclaredFunctions)
	}
	for i := uint32(0); md.ok() && i < count; i++ {
		pos := md.pc
		size := md.consumeU32v("body size")
		if size > maxFunctionSize {
			md.errorf(pos, "size %d > maximum function size %d", size, maxFunctionSize)
			return
		}
		offset := md.pc
		md.consumeBytes(size)
		if md.failed() {
			break
		}
		m.Functions[m.NumImportedFunctions+i].Code = module.Range{Offset: uint32(offset), Len: size}
	}
	md.codeSeen = true
}

func (md *moduleDecoder) checkDataSegmentsCount(count uint32) bool {
	if md.hasSeen(secDataCount) && count != md.numDeclaredData {
		md.errorf(md.pc, "data segments count %d mismatch (%d expected)", count, md.numDeclaredData)
		return false
	}
	return true
}

func (md *moduleDecoder) decodeDataSection() {
	m := md.m
	count := md.consumeCount("data segments count", maxDataSegments)
	if !md.checkDataSegmentsCount(count) {
		return
	}
	for i := uint32(0); i < count; i++ {
		seg := md.consumeDataSegmentHeader()
		length := md.consumeU32v("source size")
		offset := md.pc
		md.consumeBytes(length)
		if md.failed() {
			break
		}
		seg.Init = module.Range{Offset: uint32(offset), Len: length}
		m.Datas = append(m.Datas, seg)
	}
}

func (md *moduleDecoder) consumeDataSegmentHeader() module.DataSegment {
	pos := md.pc
	flag := md.consumeU32v("flag")
	if flag&^0b1011 != 0 {
		md.errorf(pos, "illegal flag value %d", flag)
		return module.DataSegment{}
	}
	status := flag & 0b11
	if status == 3 {
		md.errorf(pos, "illegal flag value %d", flag)
		return module.DataSegment{}
	}
	if flag&0b1000 != 0 {
		md.errorf(pos, "illegal flag value %d.", flag)
		return module.DataSegment{}
	}
	active := status == 0 || status == 2
	var memIndex uint32
	if status == 2 {
		memIndex = md.consumeU32v("memory index")
	}
	seg := module.DataSegment{Active: active, Memory: memIndex}
	if active {
		n := len(md.m.Memories)
		if uint64(memIndex) >= uint64(n) {
			md.errorf(pos, "invalid memory index %d for data section (having %d %s)", memIndex, n, memories(n))
			return module.DataSegment{}
		}
		expected := module.I32
		if md.m.Memories[memIndex].Limits.Is64 {
			expected = module.I64
		}
		seg.Offset = md.consumeInitExpr(expected)
	}
	return seg
}

func (md *moduleDecoder) decodeTagSection() {
	m := md.m
	count := md.consumeCount("tag count", maxTags)
	for i := uint32(0); md.ok() && i < count; i++ {
		md.consumeExceptionAttribute()
		sig := md.consumeTagSigIndex()
		m.Tags = append(m.Tags, module.Tag{TypeIndex: sig})
	}
}

// decodeNameSection records the first name section and its module name. Its
// errors do not fail the module: an inner decoder reads it.
func (md *moduleDecoder) decodeNameSection(start, end int) {
	if !md.hasSeen(secName) {
		md.setSeen(secName)
		md.m.NameSection = module.Range{Offset: uint32(start), Len: uint32(end - start)}
		inner := newDecoder(md.b, md.pc, end)
		for inner.ok() && inner.more() {
			nameType := inner.consumeU8("name type")
			if nameType&0x80 != 0 {
				inner.errorAt(inner.pc, "name type if not varuint7")
			}
			payloadLen := inner.consumeU32v("name payload length")
			if !inner.checkAvailable(payloadLen) {
				break
			}
			if nameType == 0 {
				name := consumeString(inner, false, "module name")
				if inner.ok() && utf8.Valid(md.b[name.Offset:name.End()]) {
					md.m.Name = string(md.b[name.Offset:name.End()])
					md.m.HasName = true
				}
			} else {
				inner.consumeBytes(payloadLen)
			}
		}
	}
	md.pc = end
}

// checkMismatchedCounts checks the counts the function and data count
// sections declare against the sections that should follow.
func (md *moduleDecoder) checkMismatchedCounts() {
	if md.numDeclaredFunctions != 0 && !md.codeSeen {
		md.errorf(md.pc, "function count is %d, but code section is absent", md.numDeclaredFunctions)
		return
	}
	md.checkDataSegmentsCount(uint32(len(md.m.Datas)))
}
