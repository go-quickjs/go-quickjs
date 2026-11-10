package opt

import (
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// reader reads the instructions of a validated function body, checking
// nothing: the baseline's reader (jit/reader.go), for what this tier reads.
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

func (r *reader) fixed32() uint32 {
	b := r.b[r.pc:]
	r.pc += 4
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

func (r *reader) fixed64() uint64 {
	lo := r.fixed32()
	return uint64(lo) | uint64(r.fixed32())<<32
}

func (r *reader) opcode() opcode.Opcode {
	b := r.byte()
	if opcode.IsPrefix(b) {
		return opcode.Prefixed(b, r.u32())
	}
	return opcode.Opcode(b)
}

func (r *reader) heapType() {
	if v := r.s64(); v >= 0 {
		return
	}
}

// valueType reads a value type; a reference type's heap type is skipped:
// a reference is in its slot, whatever its type.
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
		r.heapType()
	}
	return module.ValueType{Kind: module.KindRef}
}

func (r *reader) blockType(m *module.Module) (params, results []module.ValueType) {
	b := r.b[r.pc]
	if b == 0x40 {
		r.pc++
		return nil, nil
	}
	if b&0xc0 == 0x40 || b == 0x64 || b == 0x63 {
		switch b {
		case 0x7f, 0x7e, 0x7d, 0x7c, 0x7b:
			r.pc++
			return nil, oneType[0x7f-b : 0x7f-b+1]
		}
		return nil, []module.ValueType{r.valueType()}
	}
	ft := m.Types[r.s64()].Func
	return ft.Params, ft.Results
}

func (r *reader) memarg() (mem uint32, offset uint64) {
	align := r.u32()
	if align&0x40 != 0 {
		mem = r.u32()
	}
	return mem, r.u64()
}

// oneType are the block types of one numeric or v128 result, by 0x7f less
// their byte.
var oneType = []module.ValueType{module.I32, module.I64, module.F32, module.F64, module.V128}

// skipDead skips instructions up to the end of the construct being built:
// its end, an if's else, a legacy try's catch or catch_all, or its
// delegate, which it leaves to be read.
func (r *reader) skipDead() {
	depth := 0
	for {
		at := r.pc
		op := r.opcode()
		switch op {
		case opcode.Block, opcode.Loop, opcode.If, opcode.Try, opcode.TryTable:
			depth++
		case opcode.End, opcode.Delegate:
			if depth == 0 {
				r.pc = at
				return
			}
			depth--
			if op == opcode.End {
				continue
			}
		case opcode.Else, opcode.Catch, opcode.CatchAll:
			if depth == 0 {
				r.pc = at
				return
			}
		}
		r.skipImmediates(op)
	}
}

// skipImmediates skips the immediates of an instruction a refusal check
// let through (check).
func (r *reader) skipImmediates(op opcode.Opcode) {
	switch op {
	case opcode.Block, opcode.Loop, opcode.If, opcode.Try, opcode.TryTable:
		if b := r.b[r.pc]; b == 0x64 || b == 0x63 {
			// A reference type's heap type follows.
			r.pc++
			r.heapType()
			break
		}
		r.s64()
	case opcode.Br, opcode.BrIf, opcode.CallFunction, opcode.LocalGet, opcode.LocalSet, opcode.LocalTee,
		opcode.GlobalGet, opcode.GlobalSet, opcode.MemorySize, opcode.MemoryGrow, opcode.ReturnCall,
		opcode.CallRef, opcode.ReturnCallRef, opcode.RefFunc, opcode.BrOnNull, opcode.BrOnNonNull,
		opcode.TableGet, opcode.TableSet, opcode.TableSize, opcode.Throw, opcode.Rethrow, opcode.Catch,
		opcode.Delegate:
		r.u32()
	case opcode.RefNull:
		r.heapType()
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
	case opcode.I32Const, opcode.I64Const:
		r.s64()
	case opcode.F32Const:
		r.pc += 4
	case opcode.F64Const:
		r.pc += 8
	case opcode.MemoryCopy:
		r.u32()
		r.u32()
	case opcode.MemoryFill, opcode.DataDrop, opcode.ElemDrop, opcode.TableGrow, opcode.TableFill:
		r.u32()
	case opcode.MemoryInit, opcode.TableInit, opcode.TableCopy:
		r.u32()
		r.u32()
	}
	if op >= opcode.I32LoadMem && op <= opcode.I64StoreMem32 {
		r.memarg()
	}
	if op == opcode.TryTable {
		r.clauses(nil)
	}
	if op.Prefix() == opcode.PrefixGC {
		r.skipGC(op)
	}
	if op.Prefix() == opcode.PrefixAtomic {
		switch op {
		case opcode.AtomicFence:
			r.byte()
		case opcode.Pause:
		default:
			r.memarg()
		}
	}
	if op.Prefix() == opcode.PrefixSIMD {
		r.skipSIMD(op)
	}
}

// skipSIMD skips a SIMD instruction's immediates: a constant's or a
// shuffle's 16 bytes, a memory argument, a lane, or both.
func (r *reader) skipSIMD(op opcode.Opcode) {
	switch {
	case op == opcode.S128Const || op == opcode.I8x16Shuffle:
		r.pc += 16
	case isSIMDMem(op):
		r.memarg()
		if isSIMDLaneMem(op) {
			r.pc++
		}
	case isLaneOp(op):
		r.pc++
	}
}
