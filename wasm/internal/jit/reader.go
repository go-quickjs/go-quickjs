package jit

import (
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// reader reads the instructions of a validated function body. It checks
// nothing: the body is known to be well formed. It is the interpreter's
// reader (engine/reader.go), which the engine, importing this package,
// cannot give it.
type reader struct {
	b  []byte
	pc int
}

func (r *reader) byte() byte {
	v := r.b[r.pc]
	r.pc++
	return v
}

func (r *reader) u64() uint64 {
	var v uint64
	for shift := uint(0); ; shift += 7 {
		b := r.b[r.pc]
		r.pc++
		v |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			return v
		}
	}
}

func (r *reader) u32() uint32 { return uint32(r.u64()) }

func (r *reader) s64() int64 {
	var v int64
	var shift uint
	for {
		b := r.b[r.pc]
		r.pc++
		v |= int64(b&0x7f) << shift
		shift += 7
		if b&0x80 == 0 {
			if shift < 64 && b&0x40 != 0 {
				v |= -1 << shift
			}
			return v
		}
	}
}

func (r *reader) s32() int32 { return int32(r.s64()) }

func (r *reader) fixed32() uint32 {
	b := r.b[r.pc:]
	r.pc += 4
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

func (r *reader) fixed64() uint64 {
	lo := r.fixed32()
	return uint64(lo) | uint64(r.fixed32())<<32
}

// opcode reads an instruction's opcode.
func (r *reader) opcode() opcode.Opcode {
	b := r.byte()
	if opcode.IsPrefix(b) {
		return opcode.Prefixed(b, r.u32())
	}
	return opcode.Opcode(b)
}

// heapType reads a heap type.
func (r *reader) heapType() module.HeapType {
	v := r.s64()
	if v >= 0 {
		return module.HeapType(v)
	}
	h, _ := module.HeapFromCode(byte(v) & 0x7f)
	return h
}

// valueType reads a value type.
func (r *reader) valueType() module.ValueType {
	b := r.byte()
	switch b {
	case 0x7f:
		return module.I32
	case 0x7e:
		return module.I64
	case 0x7d:
		return module.F32
	case 0x7c:
		return module.F64
	case 0x7b:
		return module.V128
	case 0x64, 0x63:
		return module.RefMaybeNull(r.heapType(), b == 0x63)
	}
	h, _ := module.HeapFromCode(b)
	return module.RefNull(h)
}

// blockType reads a block type, returning its parameters and results.
func (r *reader) blockType(m *module.Module) (params, results []module.ValueType) {
	b := r.b[r.pc]
	if b == 0x40 {
		r.pc++
		return nil, nil
	}
	if b&0xc0 == 0x40 || b == 0x64 || b == 0x63 {
		// A value type: a one-byte code, or a reference type.
		return nil, []module.ValueType{r.valueType()}
	}
	ft := m.Types[r.s64()].Func
	return ft.Params, ft.Results
}

// memarg reads a memory access's immediate: the memory, the offset.
func (r *reader) memarg() (mem uint32, offset uint64) {
	align := r.u32()
	if align&0x40 != 0 {
		mem = r.u32()
	}
	return mem, r.u64()
}

// skipDead skips instructions up to the end of the construct being
// compiled -- its end, an if's else, a try's catch or delegate -- leaving
// that instruction to be read.
func (r *reader) skipDead() {
	depth := 0
	for {
		at := r.pc
		op := r.opcode()
		switch op {
		case opcode.Block, opcode.Loop, opcode.If, opcode.Try, opcode.TryTable:
			depth++
		case opcode.End:
			if depth == 0 {
				r.pc = at
				return
			}
			depth--
			continue
		case opcode.Else, opcode.Catch, opcode.CatchAll:
			if depth == 0 {
				r.pc = at
				return
			}
		case opcode.Delegate:
			if depth == 0 {
				r.pc = at
				return
			}
			depth--
		}
		r.skipImmediates(op)
	}
}

// skipImmediates skips op's immediates.
func (r *reader) skipImmediates(op opcode.Opcode) {
	switch op {
	case opcode.Block, opcode.Loop, opcode.If, opcode.Try:
		r.s64()
	case opcode.TryTable:
		r.s64()
		for n := r.u32(); n > 0; n-- {
			if k := r.byte(); k < 2 {
				r.u32()
			}
			r.u32()
		}
	case opcode.Br, opcode.BrIf, opcode.BrOnNull, opcode.BrOnNonNull,
		opcode.CallFunction, opcode.ReturnCall, opcode.CallRef, opcode.ReturnCallRef,
		opcode.LocalGet, opcode.LocalSet, opcode.LocalTee, opcode.GlobalGet, opcode.GlobalSet,
		opcode.TableGet, opcode.TableSet, opcode.Throw, opcode.Rethrow, opcode.Catch,
		opcode.Delegate, opcode.RefFunc, opcode.MemorySize, opcode.MemoryGrow:
		r.u32()
	case opcode.BrTable:
		for n := r.u32() + 1; n > 0; n-- {
			r.u32()
		}
	case opcode.CallIndirect, opcode.ReturnCallIndirect:
		r.u32()
		r.u32()
	case opcode.SelectWithType:
		for n := r.u32(); n > 0; n-- {
			r.valueType()
		}
	case opcode.I32Const:
		r.s64()
	case opcode.I64Const:
		r.s64()
	case opcode.F32Const:
		r.pc += 4
	case opcode.F64Const:
		r.pc += 8
	case opcode.RefNull:
		r.heapType()
	}
	if op >= opcode.I32LoadMem && op <= opcode.I64StoreMem32 {
		r.memarg()
		return
	}
	switch op.Prefix() {
	case opcode.PrefixGC:
		r.skipGC(op)
	case opcode.PrefixNumeric:
		switch op {
		case opcode.MemoryInit, opcode.TableInit, opcode.MemoryCopy, opcode.TableCopy:
			r.u32()
			r.u32()
		case opcode.DataDrop, opcode.ElemDrop, opcode.MemoryFill,
			opcode.TableGrow, opcode.TableSize, opcode.TableFill:
			r.u32()
		}
	case opcode.PrefixSIMD:
		r.skipSIMD(op)
	case opcode.PrefixAtomic:
		if op == opcode.AtomicFence {
			r.pc++
		} else {
			r.memarg()
		}
	}
}

func (r *reader) skipGC(op opcode.Opcode) {
	switch op {
	case opcode.StructNew, opcode.StructNewDefault, opcode.ArrayNew, opcode.ArrayNewDefault,
		opcode.ArrayGet, opcode.ArrayGetS, opcode.ArrayGetU, opcode.ArraySet, opcode.ArrayFill:
		r.u32()
	case opcode.StructGet, opcode.StructGetS, opcode.StructGetU, opcode.StructSet,
		opcode.ArrayNewFixed, opcode.ArrayNewData, opcode.ArrayNewElem, opcode.ArrayCopy,
		opcode.ArrayInitData, opcode.ArrayInitElem:
		r.u32()
		r.u32()
	case opcode.RefTest, opcode.RefTestNull, opcode.RefCast, opcode.RefCastNull:
		r.heapType()
	case opcode.BrOnCast, opcode.BrOnCastFail:
		r.pc++
		r.u32()
		r.heapType()
		r.heapType()
	}
}

func (r *reader) skipSIMD(op opcode.Opcode) {
	switch {
	case op == opcode.S128Const || op == opcode.I8x16Shuffle:
		r.pc += 16
	case op >= opcode.S128LoadMem && op <= opcode.S128StoreMem,
		op == opcode.S128Load32Zero || op == opcode.S128Load64Zero:
		r.memarg()
	case op >= opcode.S128Load8Lane && op <= opcode.S128Store64Lane:
		r.memarg()
		r.pc++
	case op >= opcode.I8x16ExtractLaneS && op <= opcode.F64x2ReplaceLane:
		r.pc++
	}
}
