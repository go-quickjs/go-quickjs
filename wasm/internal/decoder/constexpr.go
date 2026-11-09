package decoder

import "github.com/go-quickjs/go-quickjs/wasm/internal/module"

const (
	opEnd     = 0x0b
	opI32Cst  = 0x41
	opRefNull = 0xd0
	opRefFunc = 0xd2
)

// consumeInitExpr reads a constant expression whose value must be of type
// expected: the common single-instruction forms directly, anything else
// through the function body validator in its constant expression mode.
func (md *moduleDecoder) consumeInitExpr(expected module.ValueType) module.ConstExpr {
	m := md.m
	if !md.more() {
		md.errorAt(md.pc, "Beyond end of code")
		return module.ConstExpr{}
	}
	typeCheck := func(found module.ValueType) bool {
		if !module.IsSubtypeOf(found, expected, m) {
			md.errorf(md.pc+1, "type error in constant expression[0] (expected %s, got %s)", expected.Name(), found.Name())
			return false
		}
		return true
	}
	switch md.b[md.pc] {
	case opI32Cst:
		v, length := md.readI32v(md.pc+1, "i32.const")
		if md.failed() {
			return module.ConstExpr{}
		}
		if md.lookahead(1+length, opEnd) {
			if !typeCheck(module.I32) {
				return module.ConstExpr{}
			}
			md.consumeBytes(uint32(length + 2))
			return module.ConstExpr{Kind: module.ConstI32, Value: uint32(v)}
		}
	case opRefFunc:
		index, length := md.readU32v(md.pc+1, "ref.func")
		if md.failed() {
			return module.ConstExpr{}
		}
		if md.lookahead(1+length, opEnd) {
			if uint64(index) >= uint64(len(m.Functions)) {
				md.errorf(md.pc+1, "function index %d out of bounds", index)
				return module.ConstExpr{}
			}
			t := module.Ref(module.HeapIndex(m.Functions[index].TypeIndex))
			if !typeCheck(t) {
				return module.ConstExpr{}
			}
			m.Functions[index].Declared = true
			md.consumeBytes(uint32(length + 2))
			return module.ConstExpr{Kind: module.ConstRefFunc, Value: index}
		}
	case opRefNull:
		h, length := readHeapType(&md.Decoder, md.pc+1, &md.det, md.lim)
		validateHeapType(&md.Decoder, md.pc, m, h)
		if md.failed() {
			return module.ConstExpr{}
		}
		if md.lookahead(1+length, opEnd) {
			if !typeCheck(module.RefNull(h)) {
				return module.ConstExpr{}
			}
			md.consumeBytes(uint32(length + 2))
			return module.ConstExpr{Kind: module.ConstRefNull, Heap: h}
		}
	}
	start := md.pc
	stop, endFound, err := validateConstExpr(m, &md.det, md.b, start, md.end, expected, md.lim)
	md.pc = stop
	if err != nil {
		md.errorAt(int(err.Offset), err.Msg)
		return module.ConstExpr{}
	}
	if !endFound {
		md.errorAt(md.pc, "constant expression is missing 'end'")
		return module.ConstExpr{}
	}
	return module.ConstExpr{Kind: module.ConstCode, Code: module.Range{Offset: uint32(start), Len: uint32(stop - start)}}
}
