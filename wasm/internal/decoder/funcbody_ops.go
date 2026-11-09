package decoder

import (
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	op "github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// hasHandler marks the single-byte opcodes the validator decodes; any other
// byte is an invalid opcode.
var hasHandler = func() (t [256]bool) {
	for b := 0; b < 256; b++ {
		switch {
		case b <= 0x16 && b != 0x17, b >= 0x18 && b <= 0x1c, b == 0x1f,
			b >= 0x20 && b <= 0x26, b >= 0x28 && b <= 0x44, b >= 0x45 && b <= 0xc4,
			b >= 0xd0 && b <= 0xd6, b >= 0xe0 && b <= 0xe6, b >= 0xfa && b <= 0xfe:
			t[b] = true
		}
	}
	return t
}()

// constHandler marks the opcodes a constant expression may use.
var constHandler = func() (t [256]bool) {
	for _, o := range []op.Opcode{op.End, op.I32Const, op.I64Const, op.F32Const, op.F64Const,
		op.RefNull, op.RefFunc, op.GlobalGet, op.I32Add, op.I32Sub, op.I32Mul,
		op.I64Add, op.I64Sub, op.I64Mul} {
		t[o] = true
	}
	t[op.PrefixSIMD] = true
	t[op.PrefixGC] = true
	return t
}()

// The memory access each load and store opcode makes: the log2 of its size
// and the type of the value loaded or stored.
type memAccess struct {
	sizeLog2 uint8
	t        module.ValueType
}

var loadAccess = [...]memAccess{
	{2, module.I32}, {3, module.I64}, {2, module.F32}, {3, module.F64},
	{0, module.I32}, {0, module.I32}, {1, module.I32}, {1, module.I32},
	{0, module.I64}, {0, module.I64}, {1, module.I64}, {1, module.I64}, {2, module.I64}, {2, module.I64},
}

var storeAccess = [...]memAccess{
	{2, module.I32}, {3, module.I64}, {2, module.F32}, {3, module.F64},
	{0, module.I32}, {1, module.I32}, {0, module.I64}, {1, module.I64}, {2, module.I64},
}

// prototypeError rejects opcode o of a feature that is not enabled.
func (v *funcValidator) prototypeError(o op.Opcode, feature string) int {
	v.errorf(v.pc, "Invalid opcode 0x%02x (enable with --experimental-wasm-%s)", uint32(o), feature)
	return 0
}

// decodeInstruction validates the instruction at v.pc, returning its length.
func (v *funcValidator) decodeInstruction() int {
	b := v.b[v.pc]
	// local.get is decoded before the opcode is checked against those a
	// constant expression may use; there it has no locals to name.
	if v.constMode && !constHandler[b] && b != byte(op.LocalGet) {
		if hasHandler[b] {
			v.errorf(v.pc, "opcode %s is not allowed in constant expressions", op.Opcode(b).Name())
		} else {
			v.errorf(v.pc, "Invalid opcode 0x%x", b)
		}
		return 0
	}
	o := op.Opcode(b)
	switch {
	case b >= 0x45 && b <= 0xbf:
		sig := op.Signature(o)
		return v.simpleOperator(sig)
	case b >= 0x28 && b <= 0x35:
		a := loadAccess[b-0x28]
		return v.decodeLoadMem(a.sizeLog2, a.t, 1)
	case b >= 0x36 && b <= 0x3e:
		a := storeAccess[b-0x36]
		return v.decodeStoreMem(a.sizeLog2, a.t, 1)
	case b >= 0xc0 && b <= 0xc4:
		return v.simpleOperator(op.Signature(o))
	case b >= 0xe0 && b <= 0xe6:
		return v.prototypeError(o, "wasmfx")
	}
	switch o {
	case op.Nop:
		return 1
	case op.NopForTestingUnsupportedInLiftoff:
		v.errorf(v.pc, "Invalid opcode 0x%x", b)
		return 0
	case op.Block:
		bt := v.readBlockType(v.pc + 1)
		if !v.validateBlockType(v.pc+1, &bt) {
			return 0
		}
		v.pushControl(ctrlBlock, &bt)
		return 1 + bt.length
	case op.Loop:
		bt := v.readBlockType(v.pc + 1)
		if !v.validateBlockType(v.pc+1, &bt) {
			return 0
		}
		c := v.pushControl(ctrlLoop, &bt)
		v.pushMergeValues(c, &c.start)
		return 1 + bt.length
	case op.If:
		bt := v.readBlockType(v.pc + 1)
		if !v.validateBlockType(v.pc+1, &bt) {
			return 0
		}
		v.pop1(module.I32)
		v.pushControl(ctrlIf, &bt)
		return 1 + bt.length
	case op.Else:
		return v.decodeElse()
	case op.End:
		return v.decodeEnd()
	case op.Try:
		v.det.legacyEH = true
		if v.det.exnref {
			v.errorAt(v.pc, "module uses a mix of legacy and new exception handling instructions")
			return 0
		}
		bt := v.readBlockType(v.pc + 1)
		if !v.validateBlockType(v.pc+1, &bt) {
			return 0
		}
		c := v.pushControl(ctrlTry, &bt)
		c.prevCatch = v.currentCatch
		v.currentCatch = int32(len(v.ctrl) - 1)
		return 1 + bt.length
	case op.Catch:
		return v.decodeCatch()
	case op.CatchAll:
		return v.decodeCatchAll()
	case op.Delegate:
		return v.decodeDelegate()
	case op.Rethrow:
		v.det.legacyEH = true
		depth, n := v.readU32v(v.pc+1, "branch depth")
		if !v.validateBranchDepth(v.pc+1, depth, uint32(len(v.ctrl))) {
			return 0
		}
		c := v.controlAt(depth)
		if !(c.kind == ctrlTryCatchAll || c.kind == ctrlTryCatch) {
			v.errorAt(v.pc, "rethrow not targeting catch or catch-all")
			return 0
		}
		v.markMightThrow()
		v.endControl()
		return 1 + n
	case op.Throw:
		tag, n := v.readU32v(v.pc+1, "tag index")
		sig := v.validateTag(v.pc+1, tag)
		if sig == nil {
			return 0
		}
		if len(sig.Results) != 0 {
			v.errorf(v.pc, "tag signature %d has non-void return", tag)
			return 0
		}
		v.popN(sig.Params...)
		v.markMightThrow()
		v.endControl()
		return 1 + n
	case op.TryTable:
		return v.decodeTryTable()
	case op.ThrowRef:
		v.det.exnref = true
		if v.det.legacyEH {
			v.errorAt(v.pc, "module uses a mix of legacy and new exception handling instructions")
			return 0
		}
		v.pop1(module.ExnRef)
		v.markMightThrow()
		v.endControl()
		return 1
	case op.Br:
		depth, n := v.readU32v(v.pc+1, "branch depth")
		if !v.validateBranchDepth(v.pc+1, depth, uint32(len(v.ctrl))) {
			return 0
		}
		c := v.controlAt(depth)
		if !v.typeCheckBranch(c, false, false) {
			return 0
		}
		if v.reachableOK {
			c.brMerge().reached = true
		}
		v.endControl()
		return 1 + n
	case op.BrIf:
		depth, n := v.readU32v(v.pc+1, "branch depth")
		if !v.validateBranchDepth(v.pc+1, depth, uint32(len(v.ctrl))) {
			return 0
		}
		v.pop1(module.I32)
		c := v.controlAt(depth)
		if !v.typeCheckBranch(c, true, true) {
			return 0
		}
		if v.reachableOK {
			c.brMerge().reached = true
		}
		return 1 + n
	case op.BrTable:
		return v.decodeBrTable()
	case op.Return:
		if !v.doReturn(false, "return") {
			return 0
		}
		return 1
	case op.Unreachable:
		v.endControl()
		return 1
	case op.BrOnNull:
		return v.decodeBrOnNull()
	case op.BrOnNonNull:
		return v.decodeBrOnNonNull()
	case op.Select:
		vals := v.popN(module.Bottom, module.Bottom, module.I32)
		tval, fval := vals[0], vals[1]
		result := tval.t
		if result.Kind == module.KindBottom {
			result = fval.t
		} else {
			v.validateStackValue(1, fval, result)
		}
		if result.IsRef() {
			v.errorAt(v.pc, "select without type is only valid for value type inputs")
			return 0
		}
		v.push(result)
		return 1
	case op.SelectWithType:
		count, n := v.readU32v(v.pc+1, "number of select types")
		if uint8(count) != 1 {
			v.errorAt(v.pc+1, "Invalid number of types. Select accepts exactly one type")
			return 0
		}
		t, tl := readValueType(&v.Decoder, v.pc+1+n, &v.det, v.lim)
		if !validateValueType(&v.Decoder, v.pc+1, v.m, t) {
			return 0
		}
		v.popN(t, t, module.I32)
		v.push(t)
		return 1 + n + tl
	case op.I32Const:
		_, n := v.readI32v(v.pc+1, "immi32")
		v.push(module.I32)
		return 1 + n
	case op.I64Const:
		_, n := v.readI64v(v.pc+1, "immi64")
		v.push(module.I64)
		return 1 + n
	case op.F32Const:
		v.readU32(v.pc+1, "immf32")
		v.push(module.F32)
		return 5
	case op.F64Const:
		v.readU64(v.pc+1, "immf64")
		v.push(module.F64)
		return 9
	case op.RefNull:
		h, n := readHeapType(&v.Decoder, v.pc+1, &v.det, v.lim)
		if !validateHeapType(&v.Decoder, v.pc+1, v.m, h) {
			return 0
		}
		v.push(module.RefNull(h))
		return 1 + n
	case op.RefIsNull:
		x := v.pop()
		v.push(module.I32)
		if x.t.Kind == module.KindRef || x.t.Kind == module.KindBottom {
			return 1
		}
		v.popTypeError(0, x, "reference type")
		return 0
	case op.RefFunc:
		idx, n := v.readU32v(v.pc+1, "function index")
		if idx >= uint32(len(v.m.Functions)) {
			v.errorf(v.pc+1, "function index #%d is out of bounds", idx)
			return 0
		}
		if !v.constMode && !v.m.Functions[idx].Declared {
			v.errorf(v.pc+1, "undeclared reference to function #%d", idx)
			return 0
		}
		v.push(module.Ref(module.HeapIndex(v.m.Functions[idx].TypeIndex)))
		if v.constMode && v.reachableOK {
			v.m.Functions[idx].Declared = true
		}
		return 1 + n
	case op.RefAsNonNull:
		x := v.pop()
		switch {
		case x.t.Kind == module.KindBottom || (x.t.Kind == module.KindRef && !x.t.Nullable):
			v.pushValue(x)
		case x.t.Kind == module.KindRef:
			v.push(module.Ref(x.t.Heap))
		default:
			v.popTypeError(0, x, "reference type")
			return 0
		}
		return 1
	case op.RefEq:
		rhs := v.pop()
		lhs := v.pop()
		unreach := v.top().reach == unreachable
		if !module.IsSubtypeOf(lhs.t, module.EqRef, v.m) && !unreach {
			v.errorf(v.pc, "ref.eq[0] expected either eqref or (ref null shared eq), found %s of type %s",
				v.safeOpcodeNameAt(int(lhs.pc)), lhs.t.Name())
			return 0
		}
		if !module.IsSubtypeOf(rhs.t, module.EqRef, v.m) && !unreach {
			v.errorf(v.pc, "ref.eq[1] expected either eqref or (ref null shared eq), found %s of type %s",
				v.safeOpcodeNameAt(int(rhs.pc)), rhs.t.Name())
			return 0
		}
		v.push(module.I32)
		return 1
	case op.LocalGet:
		idx, n := v.readU32v(v.pc+1, "local index")
		if !v.validateLocal(v.pc+1, idx) {
			return 0
		}
		if !v.isLocalInitialized(idx) {
			v.errorf(v.pc, "uninitialized non-defaultable local: %d", idx)
			return 0
		}
		v.push(v.locals[idx])
		return 1 + n
	case op.LocalSet:
		idx, n := v.readU32v(v.pc+1, "local index")
		if !v.validateLocal(v.pc+1, idx) {
			return 0
		}
		v.pop1(v.locals[idx])
		v.setLocalInitialized(idx)
		return 1 + n
	case op.LocalTee:
		idx, n := v.readU32v(v.pc+1, "local index")
		if !v.validateLocal(v.pc+1, idx) {
			return 0
		}
		t := v.locals[idx]
		v.pop1(t)
		v.push(t)
		v.setLocalInitialized(idx)
		return 1 + n
	case op.Drop:
		v.pop()
		return 1
	case op.GlobalGet:
		idx, n := v.readU32v(v.pc+1, "global index")
		g := v.validateGlobal(v.pc+1, idx)
		if g == nil {
			return 0
		}
		v.push(g.Type)
		return 1 + n
	case op.GlobalSet:
		idx, n := v.readU32v(v.pc+1, "global index")
		g := v.validateGlobal(v.pc+1, idx)
		if g == nil {
			return 0
		}
		if !g.Mutable {
			v.errorf(v.pc, "immutable global #%d cannot be assigned", idx)
			return 0
		}
		v.pop1(g.Type)
		return 1 + n
	case op.TableGet:
		idx, n := v.readU32v(v.pc+1, "table index")
		t := v.validateTable(v.pc+1, idx)
		if t == nil {
			return 0
		}
		v.pop1(tableAddressType(t))
		v.push(t.Type)
		return 1 + n
	case op.TableSet:
		idx, n := v.readU32v(v.pc+1, "table index")
		t := v.validateTable(v.pc+1, idx)
		if t == nil {
			return 0
		}
		v.popN(tableAddressType(t), t.Type)
		return 1 + n
	case op.MemoryGrow:
		idx, n := v.readU32v(v.pc+1, "memory index")
		mem := v.validateMemory(v.pc+1, idx)
		if mem == nil {
			return 0
		}
		at := memoryAddressType(mem)
		v.pop1(at)
		v.push(at)
		return 1 + n
	case op.MemorySize:
		idx, n := v.readU32v(v.pc+1, "memory index")
		mem := v.validateMemory(v.pc+1, idx)
		if mem == nil {
			return 0
		}
		v.push(memoryAddressType(mem))
		return 1 + n
	case op.CallFunction, op.ReturnCall:
		idx, n := v.readU32v(v.pc+1, "function index")
		if idx >= uint32(len(v.m.Functions)) {
			v.errorf(v.pc+1, "function index #%d is out of bounds", idx)
			return 0
		}
		sig := v.m.FuncType(idx)
		if o == op.ReturnCall {
			if !v.canReturnCall(sig) {
				v.errorAt(v.pc, "return_call: tail call type error")
				return 0
			}
			v.popN(sig.Params...)
			v.endControl()
			return 1 + n
		}
		v.popN(sig.Params...)
		v.pushTypes(sig.Results)
		v.markMightThrow()
		return 1 + n
	case op.CallIndirect, op.ReturnCallIndirect:
		sigIdx, sn := v.readU32v(v.pc+1, "signature index")
		tableIdx, tn := v.readU32v(v.pc+1+sn, "table index")
		if !v.m.HasSignature(sigIdx) {
			v.errorf(v.pc+1, "invalid signature index: %d", sigIdx)
			return 0
		}
		t := v.validateTable(v.pc+1+sn, tableIdx)
		if t == nil {
			return 0
		}
		if !isFuncHierarchy(t.Type.Heap, v.m) {
			v.errorf(v.pc+1, "call_indirect: immediate table #%d is not of a function type", tableIdx)
			return 0
		}
		sig := v.m.Types[sigIdx].Func
		if o == op.ReturnCallIndirect {
			if !v.canReturnCall(sig) {
				v.errorAt(v.pc, "return_call_indirect: tail call return types mismatch")
				return 0
			}
			v.pop1(tableAddressType(t))
			v.popN(sig.Params...)
			v.endControl()
			return 1 + sn + tn
		}
		v.pop1(tableAddressType(t))
		v.popN(sig.Params...)
		v.pushTypes(sig.Results)
		v.markMightThrow()
		return 1 + sn + tn
	case op.CallRef, op.ReturnCallRef:
		sigIdx, n := v.readU32v(v.pc+1, "signature index")
		if !v.m.HasSignature(sigIdx) {
			v.errorf(v.pc+1, "invalid signature index: %d", sigIdx)
			return 0
		}
		sig := v.m.Types[sigIdx].Func
		if o == op.ReturnCallRef {
			if !v.canReturnCall(sig) {
				v.errorAt(v.pc, "return_call_ref: tail call return types mismatch")
				return 0
			}
			v.pop1(module.RefNull(module.HeapIndex(sigIdx)))
			v.popN(sig.Params...)
			v.endControl()
			return 1 + n
		}
		v.pop1(module.RefNull(module.HeapIndex(sigIdx)))
		v.popN(sig.Params...)
		v.pushTypes(sig.Results)
		v.markMightThrow()
		return 1 + n
	case op.Opcode(op.PrefixNumeric):
		return v.decodeNumeric()
	case op.Opcode(op.PrefixSIMD):
		return v.decodeSIMD()
	case op.Opcode(op.PrefixAtomic):
		return v.decodeAtomic()
	case op.Opcode(op.PrefixGC):
		return v.decodeGC()
	case op.Opcode(op.PrefixAsmJS):
		full, n := v.readPrefixed("asmjs index")
		v.errorf(v.pc, "Invalid opcode: 0x%x", uint32(full))
		_ = n
		return 0
	}
	v.errorf(v.pc, "Invalid opcode 0x%x", b)
	return 0
}

// readPrefixed reads the opcode of the prefixed instruction at v.pc,
// returning it and its length with the prefix byte.
func (v *funcValidator) readPrefixed(name string) (op.Opcode, int) {
	idx, n := v.readU32v(v.pc+1, "prefixed opcode index")
	if idx > 0xfff {
		v.errorf(v.pc, "Invalid prefixed opcode %d", idx)
		return 0, 0
	}
	return op.Prefixed(v.b[v.pc], idx), n + 1
}

// simpleOperator validates an operator of fixed signature sig.
func (v *funcValidator) simpleOperator(sig *op.Sig) int {
	if len(sig.Params) == 1 {
		v.pop1(sig.Params[0])
	} else {
		v.popN(sig.Params[0], sig.Params[1])
	}
	if len(sig.Results) != 0 {
		v.push(sig.Results[0])
	}
	return 1
}

func (v *funcValidator) validateBranchDepth(pos int, depth, controlDepth uint32) bool {
	if depth >= controlDepth {
		v.errorf(pos, "invalid branch depth: %d", depth)
		return false
	}
	return true
}

func (v *funcValidator) validateLocal(pos int, idx uint32) bool {
	if idx >= uint32(len(v.locals)) {
		v.errorf(pos, "invalid local index: %d", idx)
		return false
	}
	return true
}

func (v *funcValidator) validateTag(pos int, idx uint32) *module.FuncType {
	if idx >= uint32(len(v.m.Tags)) {
		v.errorf(pos, "Invalid tag index: %d", idx)
		return nil
	}
	return v.m.TagType(idx)
}

func (v *funcValidator) validateGlobal(pos int, idx uint32) *module.Global {
	if idx >= uint32(len(v.m.Globals)) {
		v.errorf(pos, "Invalid global index: %d", idx)
		return nil
	}
	g := &v.m.Globals[idx]
	if v.constMode && g.Mutable {
		v.errorAt(pos, "mutable globals cannot be used in constant expressions")
		return nil
	}
	return g
}

func (v *funcValidator) validateTable(pos int, idx uint32) *module.Table {
	if idx >= uint32(len(v.m.Tables)) {
		v.errorf(pos, "table index %d exceeds number of tables (%d)", idx, len(v.m.Tables))
		return nil
	}
	return &v.m.Tables[idx]
}

func (v *funcValidator) validateMemory(pos int, idx uint32) *module.Memory {
	if idx >= uint32(len(v.m.Memories)) {
		v.errorf(pos, "memory index %d exceeds number of declared memories (%d)", idx, len(v.m.Memories))
		return nil
	}
	return &v.m.Memories[idx]
}

func (v *funcValidator) validateElemSegment(pos int, idx uint32) bool {
	if idx >= uint32(len(v.m.Elems)) {
		v.errorf(pos, "invalid element segment index: %d", idx)
		return false
	}
	return true
}

// validateDataSegment checks a data segment index against the data count
// section's count, which an instruction naming a segment requires.
func (v *funcValidator) validateDataSegment(pos int, idx uint32) bool {
	if idx >= v.m.DataCount || !v.m.HasDataCount {
		v.errorf(pos, "invalid data segment index: %d", idx)
		return false
	}
	return true
}

func tableAddressType(t *module.Table) module.ValueType {
	if t.Limits.Is64 {
		return module.I64
	}
	return module.I32
}

func memoryAddressType(m *module.Memory) module.ValueType {
	if m.Limits.Is64 {
		return module.I64
	}
	return module.I32
}

// isFuncHierarchy reports whether heap type h is in the function hierarchy.
func isFuncHierarchy(h module.HeapType, m *module.Module) bool {
	if h.IsIndex() {
		return m.Types[h].Kind == module.TypeFunc
	}
	return h == module.HeapFunc || h == module.HeapNoFunc
}

// canReturnCall reports whether a tail call to a function of signature sig
// returns what this function does.
func (v *funcValidator) canReturnCall(sig *module.FuncType) bool {
	if len(v.results) != len(sig.Results) {
		return false
	}
	for i, r := range v.results {
		if !module.IsSubtypeOf(sig.Results[i], r, v.m) {
			return false
		}
	}
	return true
}

func (v *funcValidator) decodeElse() int {
	c := v.top()
	if !c.isIf() {
		v.errorAt(v.pc, "else does not match an if")
		return 0
	}
	if c.kind != ctrlIf {
		v.errorAt(v.pc, "else already present for if")
		return 0
	}
	if !v.typeCheckFallThru() {
		return 0
	}
	c.kind = ctrlIfElse
	if c.reach == reachable {
		c.end.reached = true
	}
	v.rollbackLocalsInitialization(c)
	v.pushMergeValues(c, &c.start)
	c.reach = innerReachability(v.controlAt(1).reach)
	v.reachableOK = v.ok() && c.reach == reachable
	return 1
}

func (v *funcValidator) decodeEnd() int {
	if !v.constMode {
		c := v.top()
		if c.kind == ctrlTry {
			c.kind = ctrlTryCatch
			v.currentCatch = c.prevCatch
		}
		if c.kind == ctrlTryCatch {
			v.fallThrough()
			c.reach = innerReachability(v.controlAt(1).reach)
			v.reachableOK = v.ok() && c.reach == reachable
			mightThrow := c.mightThrow
			v.endControl()
			v.popControl()
			if mightThrow && v.currentCatch != -1 {
				v.ctrl[v.currentCatch].mightThrow = true
			}
			return 1
		}
		if c.kind == ctrlIf && !v.typeCheckOneArmedIf(c) {
			return 0
		}
		if c.kind == ctrlTryTable {
			if c.catchCount > 0 {
				v.currentCatch = c.prevCatch
			}
			v.fallThrough()
			reachAtEnd := c.reach
			c.reach = innerReachability(v.controlAt(1).reach)
			v.reachableOK = v.ok() && c.reach == reachable
			hasCatchAll := false
			for i := c.catchAt; i < c.catchAt+c.catchCount; i++ {
				cc := v.catches[i]
				if cc.kind == catchAllKind || cc.kind == catchAllRefKind {
					hasCatchAll = true
				}
			}
			for i := c.catchAt; i < c.catchAt+c.catchCount; i++ {
				cc := v.catches[i]
				if c.mightThrow && v.reachableOK {
					v.controlAt(cc.depth).brMerge().reached = true
				}
				if cc.kind == catchAllKind || cc.kind == catchAllRefKind {
					break
				}
			}
			c.reach = reachAtEnd
			if c.mightThrow && !hasCatchAll && v.currentCatch != -1 {
				v.ctrl[v.currentCatch].mightThrow = true
			}
			v.endControl()
			v.popControl()
			return 1
		}
	}
	if len(v.ctrl) == 1 {
		what := "fallthru"
		if v.constMode {
			what = "constant expression"
		}
		v.doReturn(true, what)
		if v.pc+1 != v.end {
			v.errorAt(v.pc+1, "trailing code after function end")
			return 0
		}
		v.ctrl = v.ctrl[:0]
		return 1
	}
	if !v.typeCheckFallThru() {
		return 0
	}
	v.popControl()
	return 1
}

func (v *funcValidator) typeCheckOneArmedIf(c *control) bool {
	if c.end.arity != c.start.arity {
		v.errorAt(int(c.pc), "start-arity and end-arity of one-armed if must match")
		return false
	}
	for i := uint32(0); i < c.start.arity; i++ {
		start, end := v.merges[c.start.at+i].t, v.merges[c.end.at+i].t
		if !module.IsSubtypeOf(start, end, v.m) {
			v.errorf(v.pc, "type error in merge[%d] (expected %s, got %s)", i, end.Name(), start.Name())
			return false
		}
	}
	return true
}

func (v *funcValidator) decodeCatch() int {
	v.det.legacyEH = true
	tag, n := v.readU32v(v.pc+1, "tag index")
	sig := v.validateTag(v.pc+1, tag)
	if sig == nil {
		return 0
	}
	c := v.top()
	if !c.isTry() {
		v.errorAt(v.pc, "catch does not match a try")
		return 0
	}
	if c.kind == ctrlTryCatchAll {
		v.errorAt(v.pc, "catch after catch-all for try")
		return 0
	}
	v.fallThrough()
	c.kind = ctrlTryCatch
	v.stack = v.stack[:c.stackDepth]
	c.reach = innerReachability(v.controlAt(1).reach)
	v.reachableOK = v.ok() && c.reach == reachable
	v.rollbackLocalsInitialization(c)
	if len(sig.Results) != 0 {
		v.errorf(v.pc, "tag signature %d has non-void return", tag)
		return 0
	}
	v.pushTypes(sig.Params)
	v.currentCatch = c.prevCatch
	if !c.mightThrow {
		v.setSucceedingCodeDynamicallyUnreachable()
	}
	return 1 + n
}

func (v *funcValidator) decodeCatchAll() int {
	v.det.legacyEH = true
	c := v.top()
	if !c.isTry() {
		v.errorAt(v.pc, "catch-all does not match a try")
		return 0
	}
	if c.kind == ctrlTryCatchAll {
		v.errorAt(v.pc, "catch-all already present for try")
		return 0
	}
	v.fallThrough()
	c.kind = ctrlTryCatchAll
	c.reach = innerReachability(v.controlAt(1).reach)
	v.reachableOK = v.ok() && c.reach == reachable
	v.rollbackLocalsInitialization(c)
	v.currentCatch = c.prevCatch
	if !c.mightThrow {
		v.setSucceedingCodeDynamicallyUnreachable()
	}
	v.stack = v.stack[:c.stackDepth]
	return 1
}

func (v *funcValidator) decodeDelegate() int {
	v.det.legacyEH = true
	depth, n := v.readU32v(v.pc+1, "branch depth")
	if !v.validateBranchDepth(v.pc+1, depth, uint32(len(v.ctrl))-1) {
		return 0
	}
	c := v.top()
	if c.kind != ctrlTry {
		v.errorAt(v.pc, "delegate does not match a try")
		return 0
	}
	ctrlDepth := uint32(len(v.ctrl))
	target := depth + 1
	for target < ctrlDepth-1 && v.controlAt(target).kind != ctrlTry {
		target++
	}
	v.fallThrough()
	if c.mightThrow {
		if v.controlAt(1).reach == reachable && target != ctrlDepth-1 {
			v.controlAt(target).mightThrow = true
		}
	}
	v.currentCatch = c.prevCatch
	v.endControl()
	v.popControl()
	return 1 + n
}

func (v *funcValidator) decodeTryTable() int {
	v.det.exnref = true
	if v.det.legacyEH {
		v.errorAt(v.pc, "module uses a mix of legacy and new exception handling instructions")
		return 0
	}
	bt := v.readBlockType(v.pc + 1)
	if !v.validateBlockType(v.pc+1, &bt) {
		return 0
	}
	v.pushControl(ctrlTryTable, &bt)
	tablePos := v.pc + 1 + bt.length
	count, n := v.readU32v(tablePos, "table count")
	if count > 0 {
		v.top().prevCatch = v.currentCatch
		v.currentCatch = int32(len(v.ctrl) - 1)
	}
	if !v.validateBrTableCount(v.pc+2, count) {
		return 0
	}
	p := tablePos + n
	catchAt := uint32(len(v.catches))
	for i := uint32(0); v.ok() && i < count; i++ {
		kind := v.readU8(p, "expected 1 byte")
		p++
		var tag uint32
		if kind == catchKind || kind == catchRefKind {
			var tn int
			tag, tn = v.readU32v(p, "tag index")
			p += tn
		}
		depth, dn := v.readU32v(p, "branch depth")
		p += dn
		if !v.ok() {
			break
		}
		if kind > catchAllRefKind {
			v.errorAt(v.pc, "invalid catch kind in try table")
			return 0
		}
		var sig *module.FuncType
		if kind == catchKind || kind == catchRefKind {
			sig = v.validateTag(v.pc, tag)
			if sig == nil {
				return 0
			}
			if len(sig.Results) != 0 {
				v.errorf(v.pc, "tag signature %d has non-void return", tag)
				return 0
			}
		}
		depth++
		if !v.validateBranchDepth(v.pc, depth, uint32(len(v.ctrl))) {
			return 0
		}
		stackSize := len(v.stack)
		pushCount := uint32(0)
		if sig != nil {
			v.pushTypes(sig.Params)
			pushCount += uint32(len(sig.Params))
		}
		if kind == catchRefKind || kind == catchAllRefKind {
			v.push(module.Ref(module.HeapExn))
			pushCount++
		}
		target := v.controlAt(depth)
		if arity := target.brMerge().arity; pushCount != arity {
			s := "s"
			if pushCount == 1 {
				s = ""
			}
			v.errorf(v.pc, "catch kind generates %d operand%s, target block expects %d", pushCount, s, arity)
			return 0
		}
		if !v.typeCheckBranch(target, true, false) {
			return 0
		}
		v.stack = v.stack[:stackSize]
		v.catches = append(v.catches, catchCase{kind: kind, tag: tag, depth: depth})
	}
	c := v.top()
	c.catchAt, c.catchCount = catchAt, count
	return p - v.pc
}

// validateBrTableCount checks a br_table's or try_table's entry count: under
// the limit, and no more entries than bytes left.
func (v *funcValidator) validateBrTableCount(pos int, count uint32) bool {
	if count > v.lim.BrTableSize {
		v.errorf(pos, "invalid table count (> max br_table size): %d", count)
		return false
	}
	return v.checkAvailable(count)
}

func (v *funcValidator) decodeBrTable() int {
	count, n := v.readU32v(v.pc+1, "table count")
	v.pop1(module.I32)
	if !v.ok() {
		return 0
	}
	if !v.validateBrTableCount(v.pc+1, count) {
		return 0
	}
	ctrlDepth := uint32(len(v.ctrl))
	var seen []bool
	var seenBuf [32]bool
	if ctrlDepth <= 32 {
		seen = seenBuf[:ctrlDepth]
	} else {
		seen = make([]bool, ctrlDepth)
	}
	p := v.pc + 1 + n
	arity := uint32(0)
	for i := uint32(0); v.ok() && i <= count; i++ {
		pos := p
		target, tn := v.readU32v(p, "branch table entry")
		p += tn
		if !v.ok() {
			break
		}
		if target >= ctrlDepth {
			v.errorf(pos, "invalid branch depth: %d", target)
			return 0
		}
		if seen[target] {
			continue
		}
		seen[target] = true
		c := v.controlAt(target)
		if i == 0 {
			arity = c.brMerge().arity
		} else if c.brMerge().arity != arity {
			v.errorf(pos, "br_table: label arity inconsistent with previous arity %d", arity)
			return 0
		}
		if !v.typeCheckBranch(c, false, false) {
			return 0
		}
	}
	if v.reachableOK {
		for d := uint32(0); d < ctrlDepth; d++ {
			if seen[d] {
				v.controlAt(d).brMerge().reached = true
			}
		}
	}
	v.endControl()
	return p - v.pc
}

func (v *funcValidator) decodeBrOnNull() int {
	depth, n := v.readU32v(v.pc+1, "branch depth")
	if !v.validateBranchDepth(v.pc+1, depth, uint32(len(v.ctrl))) {
		return 0
	}
	x := v.pop()
	c := v.controlAt(depth)
	if !v.typeCheckBranch(c, true, true) {
		return 0
	}
	switch {
	case x.t.Kind == module.KindBottom || (x.t.Kind == module.KindRef && !x.t.Nullable):
		v.pushValue(x)
	case x.t.Kind == module.KindRef:
		v.push(module.Ref(x.t.Heap))
		if v.reachableOK {
			c.brMerge().reached = true
		}
	default:
		v.popTypeError(0, x, "object reference")
		return 0
	}
	return 1 + n
}

func (v *funcValidator) decodeBrOnNonNull() int {
	depth, n := v.readU32v(v.pc+1, "branch depth")
	if !v.validateBranchDepth(v.pc+1, depth, uint32(len(v.ctrl))) {
		return 0
	}
	x := v.pop()
	if !(x.t.Kind == module.KindRef || x.t.Kind == module.KindBottom) {
		v.popTypeError(0, x, "subtype of ((ref null any), (ref null extern) or (ref null func))")
		return 0
	}
	v.push(x.t.AsNonNull())
	c := v.controlAt(depth)
	if !v.typeCheckBranch(c, true, true) {
		return 0
	}
	if x.t.Kind == module.KindRef && v.reachableOK {
		if !x.t.Nullable {
			v.setSucceedingCodeDynamicallyUnreachable()
		}
		c.brMerge().reached = true
	}
	v.drop(1)
	return 1 + n
}

// --- Memory ---

// readMemArg reads a memory access immediate at pos for an access of at most
// maxAlign, returning the memory index, the offset, the alignment and the
// immediate's length.
func (v *funcValidator) readMemArg(pos int, maxAlign uint32) (memIdx uint32, offset uint64, align uint32, length int) {
	if v.end-pos >= 2 && v.b[pos]&0xe0 == 0 && v.b[pos+1]&0x80 == 0 {
		align, offset, length = uint32(v.b[pos]), uint64(v.b[pos+1]), 2
	} else {
		var n int
		align, n = v.readU32v(pos, "alignment")
		length = n
		if align&0x40 != 0 {
			align &^= 0x40
			memIdx, n = v.readU32v(pos+length, "memory index")
			length += n
		}
		if align&0x20 != 0 {
			v.errorAt(pos, "invalid memory ordering: acquire-release requires --experimental-wasm-acquire-release flag")
		}
		offset, n = v.readU64v(pos+length, "offset")
		length += n
	}
	if align > maxAlign {
		v.errorf(pos, "invalid alignment; expected maximum alignment is %d, actual alignment is %d", maxAlign, align)
	}
	return memIdx, offset, align, length
}

// validateMemArg checks a memory access immediate's memory and offset.
func (v *funcValidator) validateMemArg(pos int, memIdx uint32, offset uint64) *module.Memory {
	mem := v.validateMemory(pos, memIdx)
	if mem == nil {
		return nil
	}
	if !mem.Limits.Is64 && offset > 0xffffffff {
		v.errorf(pos, "memory offset outside 32-bit range: %d", offset)
		return nil
	}
	return mem
}

// checkStaticallyOutOfBounds marks the code after an access of size bytes
// at offset that can never be in bounds of mem as never running.
func (v *funcValidator) checkStaticallyOutOfBounds(mem *module.Memory, size, offset uint64) {
	pages := v.lim.Memory32Pages
	if mem.Limits.Is64 {
		pages = v.lim.Memory64Pages
	}
	if mem.Limits.HasMax && mem.Limits.Max < pages {
		pages = mem.Limits.Max
	}
	maxSize := pages * 65536
	if pages > 1<<47 {
		maxSize = 1<<63 - 1
	}
	if size > maxSize || offset > maxSize-size {
		v.setSucceedingCodeDynamicallyUnreachable()
	}
}

func (v *funcValidator) decodeLoadMem(sizeLog2 uint8, t module.ValueType, prefixLen int) int {
	memIdx, offset, _, n := v.readMemArg(v.pc+prefixLen, uint32(sizeLog2))
	mem := v.validateMemArg(v.pc+prefixLen, memIdx, offset)
	if mem == nil {
		return 0
	}
	v.pop1(memoryAddressType(mem))
	v.push(t)
	v.checkStaticallyOutOfBounds(mem, 1<<sizeLog2, offset)
	return prefixLen + n
}

func (v *funcValidator) decodeStoreMem(sizeLog2 uint8, t module.ValueType, prefixLen int) int {
	memIdx, offset, _, n := v.readMemArg(v.pc+prefixLen, uint32(sizeLog2))
	mem := v.validateMemArg(v.pc+prefixLen, memIdx, offset)
	if mem == nil {
		return 0
	}
	v.popN(memoryAddressType(mem), t)
	v.checkStaticallyOutOfBounds(mem, 1<<sizeLog2, offset)
	return prefixLen + n
}
