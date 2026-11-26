package jit

import (
	"fmt"

	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// Refusal is why the compiler leaves a function to the interpreter: Reason
// is one of the README's list, Detail what in the function made it.
type Refusal struct {
	Reason string
	Detail string
	// Need is the bytes a compile refused with RefuseWork needed.
	Need int64
}

func (r *Refusal) Error() string {
	if r.Detail == "" {
		return "wasm jit: refused: " + r.Reason
	}
	return "wasm jit: refused: " + r.Reason + ": " + r.Detail
}

// The reasons a function is refused.
const (
	// RefuseFrame is the refusal of a function whose operand stack would
	// be deeper than its frame.
	RefuseFrame = "frame"
	// RefuseSize is the refusal of a function whose code would be past
	// Request.MaxCodeBytes.
	RefuseSize = "size"
	// RefuseOther is the refusal of an instruction neither the compiler
	// nor an exit (Effect) runs: one validation does not take.
	RefuseOther = "other"
	// RefuseWork is the refusal of a compile whose working sets would
	// take more than Request.MaxWorkBytes.
	RefuseWork = "work"
)

func refuse(reason, format string, args ...any) {
	panic(&Refusal{Reason: reason, Detail: fmt.Sprintf(format, args...)})
}

// Features are the CPU's optional instructions the code may use, and the
// relaxed SIMD results the interpreter gives on it.
type Features struct {
	// POPCNT, and SSE4.1's ROUNDSS and ROUNDSD.
	Popcnt, Round bool
	// SIMD is the vector instructions of SSSE3, SSE4.1 and SSE4.2 on
	// amd64; arm64 always has its own.
	SIMD bool
	// RelaxedFused says relaxed_madd and relaxed_nmadd are fused, one
	// rounding; RelaxedDotSigned and RelaxedDotVNNI say which products
	// relaxed_dot_i8x16_i7x16_add_s sums (see engine/relaxed_x86.go).
	RelaxedFused, RelaxedDotSigned, RelaxedDotVNNI bool
}

// localUse is what the body does with its locals: how much each is used,
// a use in a loop counting eight times one outside it, and which are ever
// written.
type localUse struct {
	weight  []uint64
	written []bool
	// loops is scan's stack of constructs, kept for the next scan.
	loops []bool
}

// scan reads function fn's body, past its locals. It returns the body's
// local types, the parameters first, and their uses, in the memory of
// locals and use where it has room.
func scan(m *module.Module, fn uint32, locals []module.ValueType, use localUse) ([]module.ValueType, localUse) {
	f := &m.Functions[fn]
	sig := m.Types[f.TypeIndex].Func
	r := reader{b: m.Bytes, pc: int(f.Code.Offset)}
	end := int(f.Code.End())
	locals = append(locals[:0], sig.Params...)
	for n := r.u32(); n > 0; n-- {
		count := r.u32()
		t := r.valueType()
		for j := uint32(0); j < count; j++ {
			locals = append(locals, t)
		}
	}
	use.weight = grow(use.weight, len(locals))
	use.written = grow(use.written, len(locals))
	// loops says, for each construct open, whether it is a loop.
	loops := append(use.loops[:0], false)
	defer func() { use.loops = loops }()
	depth := 0
	for r.pc < end {
		// The instructions scan only skips -- the numeric ones, constants,
		// memory accesses, branches -- and the locals' are read by their
		// first byte's kind.
		switch k := scanKinds[r.b[r.pc]]; k {
		case scanSlow:
		case scanLocal:
			get := r.b[r.pc] == byte(opcode.LocalGet)
			r.pc++
			k := r.u32()
			use.weight[k] += 1 << (3 * min(depth, 6))
			if !get {
				use.written[k] = true
			}
			continue
		default:
			r.pc++
			switch k {
			case scanLEB:
				for r.b[r.pc]&0x80 != 0 {
					r.pc++
				}
				r.pc++
			case scanMemarg:
				r.memarg()
			case scan4:
				r.pc += 4
			case scan8:
				r.pc += 8
			}
			continue
		}
		op := r.opcode()
		switch op {
		case opcode.Block, opcode.Loop, opcode.If, opcode.Try:
			r.blockType(m)
			loops = append(loops, op == opcode.Loop)
			if op == opcode.Loop {
				depth++
			}
			continue
		case opcode.TryTable:
			r.blockType(m)
			for n := r.u32(); n > 0; n-- {
				if kind := r.byte(); kind < 2 {
					r.u32()
				}
				r.u32()
			}
			loops = append(loops, false)
			continue
		case opcode.End, opcode.Delegate:
			if op == opcode.Delegate {
				r.u32()
			}
			if loops[len(loops)-1] {
				depth--
			}
			loops = loops[:len(loops)-1]
			continue
		case opcode.LocalGet, opcode.LocalSet, opcode.LocalTee:
			k := r.u32()
			use.weight[k] += 1 << (3 * min(depth, 6))
			if op != opcode.LocalGet {
				use.written[k] = true
			}
			continue
		}
		r.skipImmediates(op)
	}
	return locals, use
}

// The kinds of an instruction's first byte, for scan: scanSlow for one
// scan reads by its opcode, the others for one it skips -- with no
// immediate, a LEB128, a memory access's immediate, 4 bytes, 8 bytes --
// and a local's.
const (
	scanSlow = iota
	scanNone
	scanLEB
	scanMemarg
	scan4
	scan8
	scanLocal
)

// scanKinds is the kind of each first byte (scanSlow for a prefix's).
var scanKinds = func() (k [256]uint8) {
	for op := opcode.I32Eqz; op <= opcode.I64SExtendI32; op++ {
		k[op] = scanNone
	}
	for _, op := range []opcode.Opcode{opcode.Nop, opcode.Drop, opcode.Select, opcode.Return} {
		k[op] = scanNone
	}
	for _, op := range []opcode.Opcode{opcode.Br, opcode.BrIf, opcode.I32Const, opcode.I64Const} {
		k[op] = scanLEB
	}
	for op := opcode.I32LoadMem; op <= opcode.I64StoreMem32; op++ {
		k[op] = scanMemarg
	}
	k[opcode.F32Const], k[opcode.F64Const] = scan4, scan8
	k[opcode.LocalGet], k[opcode.LocalSet], k[opcode.LocalTee] = scanLocal, scanLocal, scanLocal
	return k
}()

// grow is s with n elements, cleared, from its memory where it has room.
func grow[T any](s []T, n int) []T {
	if cap(s) < n {
		return make([]T, n)
	}
	s = s[:n]
	clear(s)
	return s
}
