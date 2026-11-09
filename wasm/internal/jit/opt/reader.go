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

// valueType reads a value type; a reference type's heap type is skipped,
// as this tier refuses references.
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

// skipDead skips instructions up to the end of the construct being built:
// its end or an if's else, which it leaves to be read. The constructs this
// tier builds are blocks, loops and ifs; a function with any other is
// refused before it is built.
func (r *reader) skipDead() {
	depth := 0
	for {
		at := r.pc
		op := r.opcode()
		switch op {
		case opcode.Block, opcode.Loop, opcode.If:
			depth++
		case opcode.End:
			if depth == 0 {
				r.pc = at
				return
			}
			depth--
			continue
		case opcode.Else:
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
	case opcode.Block, opcode.Loop, opcode.If:
		r.s64()
	case opcode.Br, opcode.BrIf, opcode.CallFunction, opcode.LocalGet, opcode.LocalSet, opcode.LocalTee,
		opcode.GlobalGet, opcode.GlobalSet, opcode.MemorySize, opcode.MemoryGrow:
		r.u32()
	case opcode.BrTable:
		for n := r.u32() + 1; n > 0; n-- {
			r.u32()
		}
	case opcode.CallIndirect:
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
	case opcode.MemoryFill:
		r.u32()
	}
	if op >= opcode.I32LoadMem && op <= opcode.I64StoreMem32 {
		r.memarg()
	}
}
