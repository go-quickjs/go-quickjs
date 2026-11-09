package decoder

import (
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	op "github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// corpusCase is a module of the corpus the tests validate, by name.
type corpusCase struct {
	name string
	b    []byte
}

// opBytes encodes opcode o with its prefix, if any.
func opBytes(o op.Opcode) []byte {
	if p := o.Prefix(); p != 0 {
		idx := uint64(o) & 0xff
		if o > 0xffff {
			idx = uint64(o) & 0xfff
		}
		return cat(p, uleb(idx))
	}
	return []byte{byte(o)}
}

// typeCode encodes a numeric or abstract reference value type.
func typeCode(t module.ValueType) byte {
	switch t.Kind {
	case module.KindI32:
		return tI32
	case module.KindI64:
		return tI64
	case module.KindF32:
		return tF32
	case module.KindF64:
		return tF64
	case module.KindV128:
		return tV128
	}
	return t.Heap.Code()
}

// pushOf is code pushing a zero of numeric type t, or a null of reference
// type t.
func pushOf(t module.ValueType) []byte {
	switch t.Kind {
	case module.KindI32:
		return bs(0x41, 0)
	case module.KindI64:
		return bs(0x42, 0)
	case module.KindF32:
		return bs(0x43, 0, 0, 0, 0)
	case module.KindF64:
		return bs(0x44, 0, 0, 0, 0, 0, 0, 0, 0)
	case module.KindV128:
		return cat(0xfd, 0x0c, make([]byte, 16))
	}
	return bs(0xd0, t.Heap.Code())
}

// sigCode is code running operator o of signature sig on zeros and
// dropping its result.
func sigCode(o op.Opcode, sig *op.Sig) []byte {
	var b []byte
	for _, p := range sig.Params {
		b = append(b, pushOf(p)...)
	}
	b = append(b, opBytes(o)...)
	for range sig.Results {
		b = append(b, 0x1a)
	}
	return b
}

// numericSeed is a module running every operator of fixed signature, the
// SIMD lane operators and constants.
func numericSeed() []byte {
	var code []byte
	for o := op.Opcode(0); o < 0x100; o++ {
		if s := op.Signature(o); s != nil {
			code = append(code, sigCode(o, s)...)
		}
	}
	for i := op.Opcode(0); i < 0x100; i++ {
		for _, o := range []op.Opcode{0xfc00 | i, 0xfd00 | i, 0xfd100 | i} {
			if op.IsFP16SIMD(o) {
				continue
			}
			if s := op.Signature(o); s != nil && o.Prefix() != 0 && o != op.S128LoadMem && o != op.S128StoreMem &&
				!(o >= op.S128Load8x8S && o <= op.S128Load64Splat) && !(o >= op.S128Load8Lane && o <= op.S128Load64Zero) &&
				o != op.DataDrop && o != op.TableInit && o != op.ElemDrop && o != op.TableCopy && o != op.TableSize {
				code = append(code, sigCode(o, s)...)
			}
		}
	}
	v128 := pushOf(module.V128)
	lanes := []struct {
		o op.Opcode
		t module.ValueType
		n byte
	}{
		{op.I8x16ExtractLaneS, module.I32, 15}, {op.I8x16ExtractLaneU, module.I32, 1},
		{op.I16x8ExtractLaneS, module.I32, 7}, {op.I16x8ExtractLaneU, module.I32, 0},
		{op.I32x4ExtractLane, module.I32, 3}, {op.I64x2ExtractLane, module.I64, 1},
		{op.F32x4ExtractLane, module.F32, 2}, {op.F64x2ExtractLane, module.F64, 1},
	}
	for _, l := range lanes {
		code = append(code, cat(v128, opBytes(l.o), l.n, 0x1a)...)
	}
	replace := []struct {
		o op.Opcode
		t module.ValueType
	}{
		{op.I8x16ReplaceLane, module.I32}, {op.I16x8ReplaceLane, module.I32}, {op.I32x4ReplaceLane, module.I32},
		{op.I64x2ReplaceLane, module.I64}, {op.F32x4ReplaceLane, module.F32}, {op.F64x2ReplaceLane, module.F64},
	}
	for _, r := range replace {
		code = append(code, cat(v128, pushOf(r.t), opBytes(r.o), 1, 0x1a)...)
	}
	code = append(code, cat(v128, v128, opBytes(op.I8x16Shuffle), bs(0, 1, 2, 3, 4, 5, 6, 7, 16, 17, 18, 19, 20, 21, 22, 31), 0x1a)...)
	code = append(code, cat(0x42, sleb(-1), 0x43, bs(0, 0, 0x80, 0x7f), 0x1a, 0x1a)...)
	return oneFunc(voidFn, body(noLocals, code, 0x0b))
}

// memorySeed is a module accessing two memories, one of them shared, and
// a memory64.
func memorySeed() []byte {
	var code []byte
	for o := byte(0x28); o <= 0x35; o++ {
		a := loadAccess[o-0x28]
		code = append(code, cat(0x41, 0, o, a.sizeLog2, 4, 0x1a)...)
	}
	for o := byte(0x36); o <= 0x3e; o++ {
		a := storeAccess[o-0x36]
		code = append(code, cat(0x41, 0, pushOf(a.t), o, a.sizeLog2, 8)...)
	}
	// Memory 1 is i64-addressed.
	code = append(code, cat(0x42, 0, 0x28, 0x42, 1, 0, 0x1a)...)
	code = append(code, cat(0x3f, 0, 0x1a, 0x41, 1, 0x40, 0, 0x1a, 0x3f, 1, 0x1a)...)
	code = append(code, cat(0x41, 0, 0x41, 0, 0x41, 0, 0xfc, 0x08, 0, 0)...) // memory.init 0 0
	code = append(code, cat(0xfc, 0x09, 0)...)                               // data.drop 0
	code = append(code, cat(0x41, 0, 0x41, 0, 0x41, 0, 0xfc, 0x0a, 0, 0)...)
	code = append(code, cat(0x41, 0, 0x42, 0, 0x41, 0, 0xfc, 0x0a, 0, 1)...) // copy from i64 memory
	code = append(code, cat(0x41, 0, 0x41, 0, 0x41, 0, 0xfc, 0x0b, 0)...)
	v128 := pushOf(module.V128)
	for _, l := range []struct {
		o     op.Opcode
		align byte
	}{{op.S128LoadMem, 4}, {op.S128Load8x8S, 3}, {op.S128Load16x4U, 3}, {op.S128Load32x2S, 3},
		{op.S128Load8Splat, 0}, {op.S128Load16Splat, 1}, {op.S128Load32Splat, 2}, {op.S128Load64Splat, 3},
		{op.S128Load32Zero, 2}, {op.S128Load64Zero, 3}} {
		code = append(code, cat(0x41, 0, opBytes(l.o), l.align, 0, 0x1a)...)
	}
	code = append(code, cat(0x41, 0, v128, opBytes(op.S128StoreMem), 4, 0)...)
	for _, l := range []struct {
		o     op.Opcode
		align byte
		lane  byte
		load  bool
	}{{op.S128Load8Lane, 0, 15, true}, {op.S128Load16Lane, 1, 7, true}, {op.S128Load32Lane, 2, 3, true},
		{op.S128Load64Lane, 3, 1, true}, {op.S128Store8Lane, 0, 0, false}, {op.S128Store64Lane, 3, 1, false}} {
		code = append(code, cat(0x41, 0, v128, opBytes(l.o), l.align, 0, l.lane)...)
		if l.load {
			code = append(code, 0x1a)
		}
	}
	// Atomics on the shared memory 0, and one on memory64.
	for o := op.AtomicNotify; o <= op.I64AtomicCompareExchange32U; o++ {
		size, ok := atomicSizeLog2(o)
		if !ok {
			continue
		}
		sig := op.AtomicSignature(o, false)
		var b []byte
		for _, p := range sig.Params {
			b = append(b, pushOf(p)...)
		}
		b = append(b, opBytes(o)...)
		b = append(b, byte(size), 0)
		for range sig.Results {
			b = append(b, 0x1a)
		}
		code = append(code, b...)
	}
	code = append(code, cat(0xfe, 0x03, 0)...)
	code = append(code, cat(0x42, 0, 0xfe, 0x10, 0x42, 1, 0, 0x1a)...)
	return modSpec{
		types:     [][]byte{voidFn},
		memories:  [][]byte{bs(3, 1, 2), bs(4, 1)},
		dataCount: uleb(1),
		bodies:    [][]byte{body(noLocals, code, 0x0b)},
		datas:     [][]byte{cat(1, "abc")},
	}.bytes()
}

// controlSeed is a module of blocks, branches, calls, locals and globals.
func controlSeed() []byte {
	types := [][]byte{
		voidFn,
		functype(bs(tI32), bs(tI32)),
		functype(bs(tI32, tI64), bs(tI64, tI32)),
		functype(nil, bs(tI32)),
	}
	f1 := body(bs(1, 2, tI64), // (param i32) (result i32), locals i64 i64
		0x02, 0x40, 0x0b, // block end
		0x03, 0x7f, 0x41, 1, 0x0b, 0x1a, // loop (result i32)
		0x20, 0, 0x04, 0x7f, 0x41, 1, 0x05, 0x41, 2, 0x0b, 0x1a, // if else
		0x20, 0, 0x04, 0x40, 0x0b,
		0x41, 0, 0x42, 0, 0x02, 2, 0x1a, 0x1a, 0x42, 0, 0x41, 0, 0x41, 1, 0x0d, 0, 0x0b, 0x1a, 0x1a, // block (type 2), br_if
		0x02, 0x40, 0x02, 0x40, 0x20, 0, 0x0e, 2, 0, 1, 1, 0x0b, 0x0b, // br_table
		0x02, 0x7f, 0x41, 3, 0x0c, 0, 0x0b, 0x1a,
		0x41, 1, 0x41, 2, 0x20, 0, 0x1b, 0x1a, // select
		0x42, 1, 0x42, 2, 0x20, 0, 0x1c, 1, tI64, 0x21, 1, // select t, local.set
		0x20, 1, 0x22, 2, 0x1a,
		0x23, 0, 0x24, 1, // global.get 0 global.set 1
		0x20, 0, 0x10, 4, 0x1a, // call 4
		0x10, 1, // call 1
		0x41, 0, 0x41, 0, 0x11, 1, 0, 0x1a, // call_indirect type 1 table 0
		0x01,
		0x20, 0, 0x0f, // return
		0x0b,
	)
	f0 := body(noLocals, 0x00, 0x0b)
	f2 := body(noLocals, 0x20, 1, 0x20, 0, 0x0b)
	f3 := body(noLocals, 0x41, 7, 0x12, 4, 0x0b) // return_call 4
	f4 := body(noLocals, 0x20, 0, 0x0b)
	f5 := body(noLocals, 0x41, 1, 0x41, 0, 0x13, 1, 0, 0x0b) // return_call_indirect
	return modSpec{
		types:     types,
		funcTypes: []uint32{1, 0, 2, 3, 1, 1},
		tables:    [][]byte{bs(tFuncRef, 0, 2)},
		globals:   [][]byte{bs(tI32, 0, 0x41, 1, 0x0b), bs(tI32, 1, 0x41, 2, 0x0b)},
		exports:   [][]byte{cat("f", 0, 0)},
		elems:     [][]byte{cat(0, 0x41, 0, 0x0b, vec(uleb(0), uleb(4)))},
		bodies:    [][]byte{f1, f0, f2, f3, f4, f5},
	}.bytes()
}
