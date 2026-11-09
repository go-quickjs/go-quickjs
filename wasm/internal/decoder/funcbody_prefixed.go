package decoder

import (
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	op "github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

func (v *funcValidator) decodeNumeric() int {
	o, n := v.readPrefixed("numeric index")
	if o>>8 != op.Opcode(op.PrefixNumeric) {
		v.errorf(v.pc, "invalid numeric opcode: 0x%x", uint32(o))
		return 0
	}
	switch o {
	case op.I32SConvertSatF32, op.I32UConvertSatF32, op.I32SConvertSatF64, op.I32UConvertSatF64,
		op.I64SConvertSatF32, op.I64UConvertSatF32, op.I64SConvertSatF64, op.I64UConvertSatF64:
		v.simpleOperator(op.Signature(o))
		return n
	case op.MemoryInit:
		seg, sn := v.readU32v(v.pc+n, "data segment index")
		memIdx, mn := v.readU32v(v.pc+n+sn, "memory index")
		if !v.validateDataSegment(v.pc+n, seg) {
			return 0
		}
		mem := v.validateMemory(v.pc+n+sn, memIdx)
		if mem == nil {
			return 0
		}
		v.popN(memoryAddressType(mem), module.I32, module.I32)
		return n + sn + mn
	case op.DataDrop:
		seg, sn := v.readU32v(v.pc+n, "data segment index")
		if !v.validateDataSegment(v.pc+n, seg) {
			return 0
		}
		return n + sn
	case op.MemoryCopy:
		dstIdx, dn := v.readU32v(v.pc+n, "memory index")
		srcIdx, sn := v.readU32v(v.pc+n+dn, "memory index")
		src := v.validateMemory(v.pc+n, srcIdx)
		if src == nil {
			return 0
		}
		dst := v.validateMemory(v.pc+n+sn, dstIdx)
		if dst == nil {
			return 0
		}
		dt, st := memoryAddressType(dst), memoryAddressType(src)
		size := st
		if dt == module.I32 {
			size = module.I32
		}
		v.popN(dt, st, size)
		return n + dn + sn
	case op.MemoryFill:
		memIdx, mn := v.readU32v(v.pc+n, "memory index")
		mem := v.validateMemory(v.pc+n, memIdx)
		if mem == nil {
			return 0
		}
		at := memoryAddressType(mem)
		v.popN(at, module.I32, at)
		return n + mn
	case op.TableInit:
		seg, sn := v.readU32v(v.pc+n, "element segment index")
		tableIdx, tn := v.readU32v(v.pc+n+sn, "table index")
		if !v.validateElemSegment(v.pc+n, seg) {
			return 0
		}
		t := v.validateTable(v.pc+n+sn, tableIdx)
		if t == nil {
			return 0
		}
		et := v.m.Elems[seg].Type
		if !module.IsSubtypeOf(et, t.Type, v.m) {
			v.errorf(v.pc+n, "table %d is not a super-type of %s", tableIdx, et.Name())
			return 0
		}
		v.popN(tableAddressType(t), module.I32, module.I32)
		return n + sn + tn
	case op.ElemDrop:
		seg, sn := v.readU32v(v.pc+n, "element segment index")
		if !v.validateElemSegment(v.pc+n, seg) {
			return 0
		}
		return n + sn
	case op.TableCopy:
		dstIdx, dn := v.readU32v(v.pc+n, "table index")
		srcIdx, sn := v.readU32v(v.pc+n+dn, "table index")
		src := v.validateTable(v.pc+n, srcIdx)
		if src == nil {
			return 0
		}
		dst := v.validateTable(v.pc+n+sn, dstIdx)
		if dst == nil {
			return 0
		}
		if !module.IsSubtypeOf(src.Type, dst.Type, v.m) {
			v.errorf(v.pc+n, "table %d is not a super-type of %s", dstIdx, src.Type.Name())
			return 0
		}
		dt, st := tableAddressType(dst), tableAddressType(src)
		size := st
		if dt == module.I32 {
			size = module.I32
		}
		v.popN(dt, st, size)
		return n + dn + sn
	case op.TableGrow:
		tableIdx, tn := v.readU32v(v.pc+n, "table index")
		t := v.validateTable(v.pc+n, tableIdx)
		if t == nil {
			return 0
		}
		at := tableAddressType(t)
		v.popN(t.Type, at)
		v.push(at)
		return n + tn
	case op.TableSize:
		tableIdx, tn := v.readU32v(v.pc+n, "table index")
		t := v.validateTable(v.pc+n, tableIdx)
		if t == nil {
			return 0
		}
		v.push(tableAddressType(t))
		return n + tn
	case op.TableFill:
		tableIdx, tn := v.readU32v(v.pc+n, "table index")
		t := v.validateTable(v.pc+n, tableIdx)
		if t == nil {
			return 0
		}
		at := tableAddressType(t)
		v.popN(at, t.Type, at)
		return n + tn
	case op.F32LoadMemF16, op.F32StoreMemF16:
		v.errorf(v.pc, "invalid numeric opcode: 0x%x, enable with --experimental-wasm-fp16", uint32(o))
		return 0
	}
	v.errorf(v.pc, "invalid numeric opcode: 0x%x", uint32(o))
	return 0
}

// simdLanes returns the number of lanes of a SIMD lane instruction.
func simdLanes(o op.Opcode) uint8 {
	switch o {
	case op.F64x2ExtractLane, op.F64x2ReplaceLane, op.I64x2ExtractLane, op.I64x2ReplaceLane,
		op.S128Load64Lane, op.S128Store64Lane:
		return 2
	case op.F32x4ExtractLane, op.F32x4ReplaceLane, op.I32x4ExtractLane, op.I32x4ReplaceLane,
		op.S128Load32Lane, op.S128Store32Lane:
		return 4
	case op.F16x8ExtractLane, op.F16x8ReplaceLane, op.I16x8ExtractLaneS, op.I16x8ExtractLaneU,
		op.I16x8ReplaceLane, op.S128Load16Lane, op.S128Store16Lane:
		return 8
	}
	return 16
}

func (v *funcValidator) validateLane(pos int, o op.Opcode, lane byte) bool {
	if lane >= simdLanes(o) {
		v.errorAt(pos, "invalid lane index")
		return false
	}
	return true
}

func (v *funcValidator) simdConst(n int) int {
	for i := 0; i < 16; i++ {
		v.readU8(v.pc+n+i, "value")
	}
	v.push(module.V128)
	return n + 16
}

func (v *funcValidator) simdExtractLane(o op.Opcode, t module.ValueType, n int) int {
	lane := v.readU8(v.pc+n, "lane")
	if !v.validateLane(v.pc+n, o, lane) {
		return 0
	}
	v.pop1(module.V128)
	v.push(t)
	return n + 1
}

func (v *funcValidator) simdReplaceLane(o op.Opcode, t module.ValueType, n int) int {
	lane := v.readU8(v.pc+n, "lane")
	if !v.validateLane(v.pc+n, o, lane) {
		return 0
	}
	v.popN(module.V128, t)
	v.push(module.V128)
	return n + 1
}

// decodeLoadTransform validates a SIMD load of size bytes, with alignment at
// most maxAlign.
func (v *funcValidator) decodeLoadTransform(maxAlign uint32, size uint64, n int) int {
	memIdx, offset, _, mn := v.readMemArg(v.pc+n, maxAlign)
	mem := v.validateMemArg(v.pc+n, memIdx, offset)
	if mem == nil {
		return 0
	}
	v.pop1(memoryAddressType(mem))
	v.push(module.V128)
	v.checkStaticallyOutOfBounds(mem, size, offset)
	return n + mn
}

func (v *funcValidator) decodeLaneMem(o op.Opcode, sizeLog2 uint32, load bool, n int) int {
	memIdx, offset, _, mn := v.readMemArg(v.pc+n, sizeLog2)
	mem := v.validateMemArg(v.pc+n, memIdx, offset)
	if mem == nil {
		return 0
	}
	lane := v.readU8(v.pc+n+mn, "lane")
	if !v.validateLane(v.pc+n, o, lane) {
		return 0
	}
	v.popN(memoryAddressType(mem), module.V128)
	if load {
		v.push(module.V128)
	}
	v.checkStaticallyOutOfBounds(mem, 1<<sizeLog2, offset)
	return n + mn + 1
}

func (v *funcValidator) decodeSIMD() int {
	o, n := v.readPrefixed("prefixed opcode")
	if !v.ok() {
		return 0
	}
	if v.constMode {
		if o != op.S128Const {
			v.errorf(v.pc, "opcode %s is not allowed in constant expressions", v.safeOpcodeNameAt(v.pc))
			return 0
		}
		return v.simdConst(n)
	}
	switch o {
	case op.F64x2ExtractLane:
		return v.simdExtractLane(o, module.F64, n)
	case op.F32x4ExtractLane:
		return v.simdExtractLane(o, module.F32, n)
	case op.I64x2ExtractLane:
		return v.simdExtractLane(o, module.I64, n)
	case op.I32x4ExtractLane, op.I16x8ExtractLaneS, op.I16x8ExtractLaneU, op.I8x16ExtractLaneS, op.I8x16ExtractLaneU:
		return v.simdExtractLane(o, module.I32, n)
	case op.F64x2ReplaceLane:
		return v.simdReplaceLane(o, module.F64, n)
	case op.F32x4ReplaceLane:
		return v.simdReplaceLane(o, module.F32, n)
	case op.I64x2ReplaceLane:
		return v.simdReplaceLane(o, module.I64, n)
	case op.I32x4ReplaceLane, op.I16x8ReplaceLane, op.I8x16ReplaceLane:
		return v.simdReplaceLane(o, module.I32, n)
	case op.I8x16Shuffle:
		maxLane := byte(0)
		for i := 0; i < 16; i++ {
			maxLane = max(maxLane, v.readU8(v.pc+n+i, "value"))
		}
		if maxLane >= 32 {
			v.errorAt(v.pc+n, "invalid shuffle mask")
			return 0
		}
		v.popN(module.V128, module.V128)
		v.push(module.V128)
		return n + 16
	case op.S128LoadMem:
		return v.decodeLoadMem(4, module.V128, n)
	case op.S128StoreMem:
		return v.decodeStoreMem(4, module.V128, n)
	case op.S128Load32Zero:
		return v.decodeLoadTransform(2, 4, n)
	case op.S128Load64Zero:
		return v.decodeLoadTransform(3, 8, n)
	case op.S128Load8Splat:
		return v.decodeLoadTransform(0, 1, n)
	case op.S128Load16Splat:
		return v.decodeLoadTransform(1, 2, n)
	case op.S128Load32Splat:
		return v.decodeLoadTransform(2, 4, n)
	case op.S128Load64Splat:
		return v.decodeLoadTransform(3, 8, n)
	case op.S128Load8x8S, op.S128Load8x8U, op.S128Load16x4S, op.S128Load16x4U, op.S128Load32x2S, op.S128Load32x2U:
		return v.decodeLoadTransform(3, 8, n)
	case op.S128Load8Lane:
		return v.decodeLaneMem(o, 0, true, n)
	case op.S128Load16Lane:
		return v.decodeLaneMem(o, 1, true, n)
	case op.S128Load32Lane:
		return v.decodeLaneMem(o, 2, true, n)
	case op.S128Load64Lane:
		return v.decodeLaneMem(o, 3, true, n)
	case op.S128Store8Lane:
		return v.decodeLaneMem(o, 0, false, n)
	case op.S128Store16Lane:
		return v.decodeLaneMem(o, 1, false, n)
	case op.S128Store32Lane:
		return v.decodeLaneMem(o, 2, false, n)
	case op.S128Store64Lane:
		return v.decodeLaneMem(o, 3, false, n)
	case op.S128Const:
		return v.simdConst(n)
	}
	if op.IsFP16SIMD(o) && o.Name() != "invalid opcode" {
		v.errorf(v.pc, "invalid simd opcode: 0x%x, enable with --experimental-wasm-fp16", uint32(o))
		return 0
	}
	sig := op.Signature(o)
	if sig == nil {
		v.errorAt(v.pc, "invalid simd opcode")
		return 0
	}
	v.popN(sig.Params...)
	v.pushTypes(sig.Results)
	return n
}

// atomicSizeLog2 returns the log2 of the size of the memory access of atomic
// memory operator o, if it is one.
func atomicSizeLog2(o op.Opcode) (uint32, bool) {
	switch {
	case o == op.AtomicNotify || o == op.I32AtomicWait:
		return 2, true
	case o == op.I64AtomicWait:
		return 3, true
	case o >= op.I32AtomicLoad && o <= op.I64AtomicCompareExchange32U:
		return [7]uint32{2, 3, 0, 1, 0, 1, 2}[(o-op.I32AtomicLoad)%7], true
	}
	return 0, false
}

func (v *funcValidator) decodeAtomic() int {
	o, n := v.readPrefixed("atomic index")
	if o>>8 != op.Opcode(op.PrefixAtomic) {
		v.errorf(v.pc, "invalid atomic opcode: 0x%x", uint32(o))
		return 0
	}
	sizeLog2, ok := atomicSizeLog2(o)
	if !ok {
		switch {
		case o == op.AtomicFence:
			if zero := v.readU8(v.pc+n, "zero"); zero != 0 {
				v.errorAt(v.pc+n, "invalid atomic operand")
				return 0
			}
			return 1 + n
		case o == op.Pause || (o >= op.StructAtomicGet && o <= op.ArrayAtomicCompareExchange):
			return v.prototypeError(o, "shared")
		}
		v.errorf(v.pc, "invalid atomic opcode: 0x%x", uint32(o))
		return 0
	}
	memIdx, offset, align, mn := v.readMemArg(v.pc+n, sizeLog2)
	mem := v.validateMemArg(v.pc+n, memIdx, offset)
	if mem == nil {
		return 0
	}
	if align != sizeLog2 {
		v.errorf(v.pc, "invalid alignment for atomic operation; expected alignment is %d, actual alignment is %d", sizeLog2, align)
	}
	sig := op.AtomicSignature(o, mem.Limits.Is64)
	v.popN(sig.Params...)
	v.pushTypes(sig.Results)
	v.checkStaticallyOutOfBounds(mem, 1<<sizeLog2, offset)
	return n + mn
}

// --- GC ---

func (v *funcValidator) structType(pos int, idx uint32) *module.StructType {
	if !v.m.HasStruct(idx) {
		v.errorf(pos, "invalid struct index: %d", idx)
		return nil
	}
	return v.m.Types[idx].Struct
}

func (v *funcValidator) arrayType(pos int, idx uint32) *module.ArrayType {
	if !v.m.HasArray(idx) {
		v.errorf(pos, "invalid array index: %d", idx)
		return nil
	}
	return v.m.Types[idx].Array
}

// readField reads and checks a struct and field index pair at pos.
func (v *funcValidator) readField(pos int) (structIdx, fieldIdx uint32, st *module.StructType, length int) {
	structIdx, sn := v.readU32v(pos, "struct index")
	fieldIdx, fn := v.readU32v(pos+sn, "field index")
	st = v.structType(pos, structIdx)
	if st == nil {
		return 0, 0, nil, 0
	}
	if fieldIdx >= uint32(len(st.Fields)) {
		v.errorf(pos+sn, "invalid field index: %d", fieldIdx)
		return 0, 0, nil, 0
	}
	return structIdx, fieldIdx, st, sn + fn
}

func (v *funcValidator) readHeapImm(pos int) (module.HeapType, int, bool) {
	h, n := readHeapType(&v.Decoder, pos, &v.det, v.lim)
	return h, n, validateHeapType(&v.Decoder, pos, v.m, h)
}

// popRepeated pops count operands of type t.
func (v *funcValidator) popRepeated(t module.ValueType, count uint32) {
	limit := v.top().stackDepth
	actual := uint32(len(v.stack)) - limit
	if actual < count {
		if v.top().reach != unreachable {
			v.notEnoughArguments(int(count), int(actual))
		}
	}
	have := min(actual, count)
	base := uint32(len(v.stack)) - have
	missing := count - have
	for i := uint32(0); i < have; i++ {
		v.validateStackValue(int(missing+i), v.stack[base+i], t)
	}
	v.stack = v.stack[:base]
}

// sameHierarchy reports whether two heap types have the same top.
func sameHierarchy(a, b module.HeapType, m *module.Module) bool {
	return nullOf(a, m) == nullOf(b, m)
}

func nullOf(h module.HeapType, m *module.Module) module.HeapType {
	if h.IsIndex() {
		if m.Types[h].Kind == module.TypeFunc {
			return module.HeapNoFunc
		}
		return module.HeapNone
	}
	switch h {
	case module.HeapFunc, module.HeapNoFunc:
		return module.HeapNoFunc
	case module.HeapExtern, module.HeapNoExtern:
		return module.HeapNoExtern
	case module.HeapExn, module.HeapNoExn:
		return module.HeapNoExn
	}
	return module.HeapNone
}

func isNoneType(h module.HeapType) bool {
	return h == module.HeapNone || h == module.HeapNoFunc || h == module.HeapNoExtern || h == module.HeapNoExn
}

func (v *funcValidator) typeCheckAlwaysSucceeds(obj value, h module.HeapType) bool {
	return module.IsSubtypeOf(obj.t, module.RefNull(h), v.m)
}

func (v *funcValidator) typeCheckAlwaysFails(obj value, h module.HeapType, nullSucceeds bool) bool {
	unrelated := !module.IsSubtypeOf(module.Ref(h), obj.t, v.m) && !module.IsSubtypeOf(obj.t, module.RefNull(h), v.m)
	nullable := obj.t.Kind == module.KindRef && obj.t.Nullable
	return (unrelated && (!nullSucceeds || !nullable)) || ((!nullSucceeds || !nullable) && isNoneType(h))
}

// popGeneric pops an operand of abstract reference type t. The expected type
// is shared when the operand is a number, numbers being shared, which only
// the message about it shows.
func (v *funcValidator) popGeneric(t module.ValueType) value {
	x := v.peek(0)
	if x.t.IsNumeric() {
		x = v.pop()
		v.popTypeError(0, x, "type shared "+t.Name())
		return x
	}
	return v.pop1(t)
}

func (v *funcValidator) nonConst() bool {
	if v.constMode {
		v.errorf(v.pc, "opcode %s is not allowed in constant expressions", v.safeOpcodeNameAt(v.pc))
		return true
	}
	return false
}

func (v *funcValidator) decodeGC() int {
	o, n := v.readPrefixed("gc index")
	if o == 0 {
		return 0
	}
	if o >= op.StringNewUtf8 {
		return v.prototypeError(op.Opcode(op.PrefixGC), "stringref")
	}
	p := v.pc + n
	switch o {
	case op.StructNew, op.StructNewDesc:
		idx, in := v.readU32v(p, "struct index")
		st := v.structType(p, idx)
		if st == nil {
			return 0
		}
		if o == op.StructNewDesc {
			v.errorf(v.pc, "descriptor allocation used for type %d without descriptor", idx)
			return 0
		}
		v.ensureStackArguments(len(st.Fields))
		base := len(v.stack) - len(st.Fields)
		for i, f := range st.Fields {
			v.validateStackValue(i, v.stack[base+i], f.Type.Unpacked())
		}
		v.stack = v.stack[:base]
		v.push(module.Ref(module.HeapIndex(idx)))
		return n + in
	case op.StructNewDefault, op.StructNewDefaultDesc:
		idx, in := v.readU32v(p, "struct index")
		st := v.structType(p, idx)
		if st == nil {
			return 0
		}
		for i, f := range st.Fields {
			if !f.Type.IsDefaultable() {
				v.errorf(v.pc, "%s: struct type %d has field %d of non-defaultable type %s", o.Name(), idx, i, f.Type.Name())
				return 0
			}
		}
		if o == op.StructNewDefaultDesc {
			v.errorf(v.pc, "descriptor allocation used for type %d without descriptor", idx)
			return 0
		}
		v.push(module.Ref(module.HeapIndex(idx)))
		return n + in
	case op.StructGet, op.StructGetS, op.StructGetU:
		if v.nonConst() {
			return 0
		}
		si, fi, st, fn := v.readField(p)
		if st == nil {
			return 0
		}
		ft := st.Fields[fi].Type
		if o == op.StructGet && ft.IsPacked() {
			v.errorf(v.pc, "struct.get: Field %d of type %d has packed type %s. Use struct.get_s or struct.get_u instead.", fi, si, ft.Name())
			return 0
		}
		if o != op.StructGet && !ft.IsPacked() {
			v.errorf(v.pc, "%s: Field %d of type %d has non-packed type %s. Use struct.get instead.", o.Name(), fi, si, ft.Name())
			return 0
		}
		v.pop1(module.RefNull(module.HeapIndex(si)))
		v.push(ft.Unpacked())
		return n + fn
	case op.StructSet:
		if v.nonConst() {
			return 0
		}
		si, fi, st, fn := v.readField(p)
		if st == nil {
			return 0
		}
		if !st.Fields[fi].Mutable {
			v.errorf(v.pc, "struct.set: Field %d of type %d is immutable.", fi, si)
			return 0
		}
		v.popN(module.RefNull(module.HeapIndex(si)), st.Fields[fi].Type.Unpacked())
		return n + fn
	case op.ArrayNew:
		idx, in := v.readU32v(p, "array index")
		at := v.arrayType(p, idx)
		if at == nil {
			return 0
		}
		v.popN(at.Elem.Type.Unpacked(), module.I32)
		v.push(module.Ref(module.HeapIndex(idx)))
		return n + in
	case op.ArrayNewDefault:
		idx, in := v.readU32v(p, "array index")
		at := v.arrayType(p, idx)
		if at == nil {
			return 0
		}
		if !at.Elem.Type.IsDefaultable() {
			v.errorf(v.pc, "%s: array type %d has non-defaultable element type %s", o.Name(), idx, at.Elem.Type.Name())
			return 0
		}
		v.pop1(module.I32)
		v.push(module.Ref(module.HeapIndex(idx)))
		return n + in
	case op.ArrayNewData, op.ArrayInitData:
		if v.nonConst() {
			return 0
		}
		idx, in := v.readU32v(p, "array index")
		at := v.arrayType(p, idx)
		if at == nil {
			return 0
		}
		name := "array.new_data"
		if o == op.ArrayInitData {
			name = "array.init_data"
			if !at.Elem.Mutable {
				v.errorf(v.pc, "array.init_data can only be used with mutable arrays, found array type #%d instead", idx)
				return 0
			}
		}
		if at.Elem.Type.IsRef() {
			v.errorf(v.pc, "%s can only be used with numeric-type arrays, found array type #%d instead", name, idx)
			return 0
		}
		seg, sn := v.readU32v(p+in, "data segment")
		if !v.validateDataSegment(p+in, seg) {
			return 0
		}
		if o == op.ArrayNewData {
			v.popN(module.I32, module.I32)
			v.push(module.Ref(module.HeapIndex(idx)))
		} else {
			v.popN(module.RefNull(module.HeapIndex(idx)), module.I32, module.I32, module.I32)
		}
		return n + in + sn
	case op.ArrayNewElem, op.ArrayInitElem:
		if v.nonConst() {
			return 0
		}
		idx, in := v.readU32v(p, "array index")
		at := v.arrayType(p, idx)
		if at == nil {
			return 0
		}
		name := "array.new_elem"
		if o == op.ArrayInitElem {
			name = "array.init_elem"
			if !at.Elem.Mutable {
				v.errorf(v.pc, "array.init_elem can only be used with mutable arrays, found array type #%d instead", idx)
				return 0
			}
		}
		et := at.Elem.Type
		if et.IsNumeric() {
			v.errorf(v.pc, "%s can only be used with reference-type arrays, found array type #%d instead", name, idx)
			return 0
		}
		seg, sn := v.readU32v(p+in, "element segment")
		if !v.validateElemSegment(p+in, seg) {
			return 0
		}
		segType := v.m.Elems[seg].Type
		if !module.IsSubtypeOf(segType, et, v.m) {
			v.errorf(v.pc, "%s: segment type %s is not a subtype of array element type %s", name, segType.Name(), et.Name())
			return 0
		}
		if o == op.ArrayNewElem {
			v.popN(module.I32, module.I32)
			v.push(module.Ref(module.HeapIndex(idx)))
		} else {
			v.popN(module.RefNull(module.HeapIndex(idx)), module.I32, module.I32, module.I32)
		}
		return n + in + sn
	case op.ArrayGet, op.ArrayGetS, op.ArrayGetU:
		if v.nonConst() {
			return 0
		}
		idx, in := v.readU32v(p, "array index")
		at := v.arrayType(p, idx)
		if at == nil {
			return 0
		}
		et := at.Elem.Type
		if o == op.ArrayGet && et.IsPacked() {
			v.errorf(v.pc, "array.get: Array type %d has packed type %s. Use array.get_s or array.get_u instead.", idx, et.Name())
			return 0
		}
		if o != op.ArrayGet && !et.IsPacked() {
			v.errorf(v.pc, "%s: Array type %d has non-packed type %s. Use array.get instead.", o.Name(), idx, et.Name())
			return 0
		}
		v.popN(module.RefNull(module.HeapIndex(idx)), module.I32)
		v.push(et.Unpacked())
		return n + in
	case op.ArraySet:
		if v.nonConst() {
			return 0
		}
		idx, in := v.readU32v(p, "array index")
		at := v.arrayType(p, idx)
		if at == nil {
			return 0
		}
		if !at.Elem.Mutable {
			v.errorf(v.pc, "array.set: Array type %d is immutable", idx)
			return 0
		}
		v.popN(module.RefNull(module.HeapIndex(idx)), module.I32, at.Elem.Type.Unpacked())
		return n + in
	case op.ArrayLen:
		if v.nonConst() {
			return 0
		}
		v.popGeneric(module.ArrayRef)
		v.push(module.I32)
		return n
	case op.ArrayCopy:
		if v.nonConst() {
			return 0
		}
		dstIdx, dn := v.readU32v(p, "array index")
		dst := v.arrayType(p, dstIdx)
		if dst == nil {
			return 0
		}
		if !dst.Elem.Mutable {
			v.errorf(v.pc, "array.copy: immediate destination array type #%d is immutable", dstIdx)
			return 0
		}
		srcIdx, sn := v.readU32v(p+dn, "array index")
		src := v.arrayType(p+dn, srcIdx)
		if src == nil {
			return 0
		}
		if !module.IsSubtypeOf(src.Elem.Type, dst.Elem.Type, v.m) {
			v.errorf(v.pc, "array.copy: source array's #%d element type is not a subtype of destination array's #%d element type", srcIdx, dstIdx)
			return 0
		}
		v.popN(module.RefNull(module.HeapIndex(dstIdx)), module.I32, module.RefNull(module.HeapIndex(srcIdx)), module.I32, module.I32)
		return n + dn + sn
	case op.ArrayFill:
		if v.nonConst() {
			return 0
		}
		idx, in := v.readU32v(p, "array index")
		at := v.arrayType(p, idx)
		if at == nil {
			return 0
		}
		if !at.Elem.Mutable {
			v.errorf(v.pc, "array.init: Array type #%d is immutable", idx)
			return 0
		}
		v.popN(module.RefNull(module.HeapIndex(idx)), module.I32, at.Elem.Type.Unpacked(), module.I32)
		return n + in
	case op.ArrayNewFixed:
		idx, in := v.readU32v(p, "array index")
		at := v.arrayType(p, idx)
		if at == nil {
			return 0
		}
		count, cn := v.readU32v(p+in, "array.new_fixed length")
		if count > v.lim.ArrayNewFixedLength {
			v.errorf(v.pc, "Requested length %d for array.new_fixed too large, maximum is %d", count, v.lim.ArrayNewFixedLength)
			return 0
		}
		v.popRepeated(at.Elem.Type.Unpacked(), count)
		v.push(module.Ref(module.HeapIndex(idx)))
		return n + in + cn
	case op.RefI31:
		v.pop1(module.I32)
		v.push(module.Ref(module.HeapI31))
		return n
	case op.RefI31Shared:
		return v.prototypeError(o, "shared")
	case op.I31GetS, op.I31GetU:
		if v.nonConst() {
			return 0
		}
		v.popGeneric(module.I31Ref)
		v.push(module.I32)
		return n
	case op.RefGetDesc, op.RefCastDescEq, op.RefCastDescEqNull, op.BrOnCastDescEq, op.BrOnCastDescEqFail:
		if v.nonConst() {
			return 0
		}
		return v.prototypeError(o, "custom_descriptors")
	case op.RefCast, op.RefCastNull, op.RefTest, op.RefTestNull:
		if v.nonConst() {
			return 0
		}
		h, hn, ok := v.readHeapImm(p)
		if !ok {
			return 0
		}
		obj := v.pop()
		test := o == op.RefTest || o == op.RefTestNull
		nullSucceeds := o == op.RefCastNull || o == op.RefTestNull
		if test {
			v.push(module.I32)
		}
		if !((obj.t.Kind == module.KindRef && sameHierarchy(obj.t.Heap, h, v.m)) || obj.t.Kind == module.KindBottom) {
			v.errorf(int(obj.pc), "Invalid types for %s: %s of type %s has to be in the same reference type hierarchy as %s",
				o.Name(), v.safeOpcodeNameAt(int(obj.pc)), obj.t.Name(), module.Ref(h).Name())
			return 0
		}
		if !test {
			v.push(module.RefMaybeNull(h, nullSucceeds))
			if v.reachableOK && !v.typeCheckAlwaysSucceeds(obj, h) && v.typeCheckAlwaysFails(obj, h, nullSucceeds) &&
				!(obj.t.Nullable && nullSucceeds) {
				v.setSucceedingCodeDynamicallyUnreachable()
			}
		}
		return n + hn
	case op.RefCastNop:
		if v.nonConst() {
			return 0
		}
		v.errorAt(v.pc, "Invalid opcode 0xfb4c (enable with --experimental-wasm-ref-cast-nop)")
		return 0
	case op.BrOnCast, op.BrOnCastFail:
		if v.nonConst() {
			return 0
		}
		return v.decodeBrOnCast(o, n)
	case op.AnyConvertExtern:
		x := v.popGeneric(module.ExternRef)
		v.push(module.RefMaybeNull(module.HeapAny, x.t.Kind == module.KindRef && x.t.Nullable))
		return n
	case op.ExternConvertAny:
		x := v.popGeneric(module.AnyRef)
		v.push(module.RefMaybeNull(module.HeapExtern, x.t.Kind == module.KindRef && x.t.Nullable))
		return n
	}
	v.errorf(v.pc, "invalid gc opcode: %x", uint32(o))
	return 0
}

func (v *funcValidator) decodeBrOnCast(o op.Opcode, n int) int {
	p := v.pc + n
	flags := v.readU8(p, "br_on_cast flags")
	if flags > 3 {
		v.errorf(p, "invalid br_on_cast flags %d", flags)
		flags = 0
	}
	p++
	srcNull, resNull := flags&1 != 0, flags&2 != 0
	depth, dn := v.readU32v(p, "branch depth")
	if !v.validateBranchDepth(p, depth, uint32(len(v.ctrl))) {
		return 0
	}
	p += dn
	srcHeap, sn, ok := v.readHeapImm(p)
	if !ok {
		return 0
	}
	p += sn
	dstHeap, tn, ok := v.readHeapImm(p)
	if !ok {
		return 0
	}
	p += tn
	srcType := module.RefMaybeNull(srcHeap, srcNull)
	targetType := module.RefMaybeNull(dstHeap, resNull)
	if !module.IsSubtypeOf(targetType, srcType, v.m) {
		v.errorf(v.pc, "invalid types for %s: %s is not a subtype of %s", o.Name(), targetType.Name(), srcType.Name())
		return 0
	}
	obj := v.pop1(srcType)
	c := v.controlAt(depth)
	if c.brMerge().arity == 0 {
		v.errorf(v.pc, "%s must target a branch of arity at least 1", o.Name())
		return 0
	}
	if o == op.BrOnCast {
		v.push(targetType)
		if !v.typeCheckBranch(c, true, true) {
			return 0
		}
		if v.reachableOK {
			if v.typeCheckAlwaysSucceeds(obj, dstHeap) {
				if !(obj.t.Nullable && !resNull) {
					v.setSucceedingCodeDynamicallyUnreachable()
				}
				c.brMerge().reached = true
			} else if !v.typeCheckAlwaysFails(obj, dstHeap, resNull) {
				c.brMerge().reached = true
			}
		}
		v.drop(1)
		obj.t = module.RefMaybeNull(srcHeap, srcNull && !resNull)
		v.pushValue(obj)
		return p - v.pc
	}
	branchType := srcType
	if resNull {
		branchType = srcType.AsNonNull()
	}
	v.push(branchType)
	if !v.typeCheckBranch(c, true, true) {
		return 0
	}
	result := value{uint32(v.pc), targetType}
	if v.reachableOK {
		if v.typeCheckAlwaysFails(obj, dstHeap, resNull) {
			v.setSucceedingCodeDynamicallyUnreachable()
			c.brMerge().reached = true
		} else if v.typeCheckAlwaysSucceeds(obj, dstHeap) {
			if obj.t.Nullable && !resNull {
				c.brMerge().reached = true
			} else {
				result = obj
			}
		} else {
			c.brMerge().reached = true
		}
	}
	v.drop(1)
	v.pushValue(result)
	return p - v.pc
}
