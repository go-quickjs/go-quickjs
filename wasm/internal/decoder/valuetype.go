package decoder

import "github.com/go-quickjs/go-quickjs/wasm/internal/module"

// Exported limits other packages check at run time, with the same values
// as V8Limits.
const (
	// MaxTableSize is the largest size a table may grow to, in elements.
	MaxTableSize = 10_000_000
	// MaxMemory32Pages and MaxMemory64Pages are the largest sizes a memory
	// may grow to, in pages.
	MaxMemory32Pages = 65_536
	MaxMemory64Pages = 262_144
	// MaxArrayNewFixedLength is the most elements array.new_fixed takes.
	MaxArrayNewFixedLength = 10_000
)

// Binary codes of value types and type definitions.
const (
	codeVoid      = 0x40
	codeI32       = 0x7f
	codeI64       = 0x7e
	codeF32       = 0x7d
	codeF64       = 0x7c
	codeS128      = 0x7b
	codeI8        = 0x78
	codeI16       = 0x77
	codeF16       = 0x76
	codeNoExn     = 0x74
	codeNoFunc    = 0x73
	codeNoExtern  = 0x72
	codeNone      = 0x71
	codeFuncRef   = 0x70
	codeExternRef = 0x6f
	codeAnyRef    = 0x6e
	codeEqRef     = 0x6d
	codeI31Ref    = 0x6c
	codeStructRef = 0x6b
	codeArrayRef  = 0x6a
	codeExnRef    = 0x69
	codeContRef   = 0x68
	codeStringRef = 0x67
	codeSVWtf8    = 0x66
	codeShared    = 0x65
	codeRef       = 0x64
	codeRefNull   = 0x63
	codeExact     = 0x62
	codeSVIter    = 0x61
	codeSVWtf16   = 0x60
	codeNoCont    = 0x75

	codeFuncType    = 0x60
	codeStructType  = 0x5f
	codeArrayType   = 0x5e
	codeContType    = 0x5d
	codeSubtype     = 0x50
	codeSubFinal    = 0x4f
	codeRecGroup    = 0x4e
	codeDescriptor  = 0x4d
	codeDescribes   = 0x4c
	exceptionAttrib = 0
)

// detected records the features a module has been seen to use, where
// validation depends on them: a module may not mix legacy exception handling
// and exnref.
type detected struct {
	legacyEH bool
	exnref   bool
}

// disabledHeapName returns the name of a heap type code of a feature that is
// not enabled.
func disabledHeapName(code byte) string {
	switch code {
	case codeStringRef:
		return "string"
	case codeSVWtf8:
		return "stringview_wtf8"
	case codeSVWtf16:
		return "stringview_wtf16"
	case codeSVIter:
		return "stringview_iter"
	case codeContRef:
		return "cont"
	case codeNoCont:
		return "nocont"
	}
	return "<unknown>"
}

// readHeapType reads a heap type at pos without consuming it, returning it
// and its length. On an error it returns HeapBottom. A type index is not
// checked against the module here; validateHeapType does that.
func readHeapType(d *Decoder, pos int, det *detected, l *Limits) (module.HeapType, int) {
	v, length := d.readI33v(pos, "heap type")
	if v >= 0 {
		if v >= int64(l.Types) {
			d.errorf(pos, "Type index %d is greater than the maximum number %d of type definitions supported by V8", v, l.Types)
			return module.HeapBottom, length
		}
		return module.HeapType(v), length
	}
	if v < -64 {
		d.errorf(pos, "Unknown heap type %d", v)
		return module.HeapBottom, length
	}
	code := byte(v) & 0x7f
	switch code {
	case codeShared:
		d.errorf(pos, "invalid heap type 0x%x, enable with --experimental-wasm-shared", codeShared)
		return module.HeapBottom, length
	case codeNoFunc, codeFuncRef, codeEqRef, codeI31Ref, codeStructRef, codeArrayRef,
		codeAnyRef, codeNone, codeNoExtern, codeExternRef:
		h, _ := module.HeapFromCode(code)
		return h, length
	case codeNoExn, codeExnRef:
		if det.legacyEH {
			d.errorAt(pos, "module uses a mix of legacy and new exception handling instructions")
			return module.HeapBottom, 0
		}
		det.exnref = true
		h, _ := module.HeapFromCode(code)
		return h, length
	case codeStringRef, codeSVWtf8, codeSVWtf16, codeSVIter:
		d.errorf(pos, "invalid heap type '%s', enable with --experimental-wasm-stringref", disabledHeapName(code))
		return module.HeapBottom, 0
	case codeNoCont, codeContRef:
		d.errorf(pos, "invalid heap type '%s', enable with --experimental-wasm-wasmfx", disabledHeapName(code))
		return module.HeapBottom, 0
	case codeExact:
		d.errorAt(pos, "invalid heap type 'exact', enable with --experimental-wasm-custom-descriptors")
		return module.HeapBottom, 0
	}
	d.errorf(pos, "Unknown heap type %d", v)
	return module.HeapBottom, length
}

// readValueType reads a value type at pos without consuming it, returning it
// and its length. On an error it returns Bottom and 0.
func readValueType(d *Decoder, pos int, det *detected, l *Limits) (module.ValueType, int) {
	val := d.readU8(pos, "value type opcode")
	if d.failed() {
		return module.Bottom, 0
	}
	switch val {
	case codeEqRef, codeI31Ref, codeStructRef, codeArrayRef, codeAnyRef, codeNone,
		codeNoExtern, codeNoFunc, codeExternRef, codeFuncRef:
		h, _ := module.HeapFromCode(val)
		return module.RefNull(h), 1
	case codeNoExn, codeExnRef:
		if det.legacyEH {
			d.errorAt(pos, "module uses a mix of legacy and new exception handling instructions")
			return module.Bottom, 0
		}
		det.exnref = true
		h, _ := module.HeapFromCode(val)
		return module.RefNull(h), 1
	case codeStringRef, codeSVWtf8, codeSVWtf16, codeSVIter:
		d.errorf(pos, "invalid value type '%sref', enable with --experimental-wasm-stringref", disabledHeapName(val))
		return module.Bottom, 0
	case codeContRef, codeNoCont:
		d.errorf(pos, "invalid value type '%s', enable with --experimental-wasm-wasmfx", disabledHeapName(val))
		return module.Bottom, 0
	case codeI32:
		return module.I32, 1
	case codeI64:
		return module.I64, 1
	case codeF32:
		return module.F32, 1
	case codeF64:
		return module.F64, 1
	case codeS128:
		return module.V128, 1
	case codeRef, codeRefNull:
		h, length := readHeapType(d, pos+1, det, l)
		if h == module.HeapBottom {
			return module.Bottom, length + 1
		}
		return module.RefMaybeNull(h, val == codeRefNull), length + 1
	}
	d.errorf(pos, "invalid value type 0x%x", val)
	return module.Bottom, 0
}

// validateHeapType checks that a heap type read at pos names a type of m.
func validateHeapType(d *Decoder, pos int, m *module.Module, h module.HeapType) bool {
	if h == module.HeapBottom {
		return false
	}
	if !h.IsIndex() {
		return true
	}
	if h.Index() >= uint32(len(m.Types)) {
		d.errorf(pos, "Type index %d is out of bounds", h.Index())
		return false
	}
	return true
}

// validateValueType checks that a value type read at pos names types of m.
func validateValueType(d *Decoder, pos int, m *module.Module, t module.ValueType) bool {
	if t.Kind == module.KindBottom {
		return false
	}
	if t.Kind != module.KindRef {
		return true
	}
	return validateHeapType(d, pos, m, t.Heap)
}
