//go:build quickjs_jit && !android && !ios && ((amd64 && (linux || windows)) || (arm64 && (darwin || linux)))

package engine

import (
	"unsafe"

	wjit "github.com/go-quickjs/go-quickjs/wasm/internal/jit"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// An instruction native code leaves to Go (wjit.ExitInstr) runs the
// interpreter's own code for it: the instruction compiled to register code
// as the interpreter compiles it, over operands in the frame's slots -- its
// numbers from the numeric slot the exit names, two apart, its references
// in the reference slots of their stack positions, or where Ctx.RArgs
// says -- its result stored into the first operand's slot, and run by the
// Go table in the frame that exited. So its result, and its trap, are the
// interpreter's by construction.

// instrExit is the code of one instruction native code leaves to Go.
type instrExit struct {
	rc regCode
	// pure says the instruction neither traps nor allocates -- a SIMD
	// instruction that does not access memory, popcnt, a rounding -- so
	// that Go runs it without making the exec's call stack the native
	// frames'.
	pure bool
}

// instrExitKey is what an instruction's exit code is kept by: the
// instruction's offset in the module and where its operands are, which
// the tiers' code of one instruction stage in slots of their own.
type instrExitKey struct {
	at          uint32
	base, rbase int
}

// instrExit returns the code of the instruction at offset at of the
// module, its numeric operands in the frame's slots from base, two apart,
// its references in the reference slots from rbase, making it the first
// time.
func (cj *codeJIT) instrExit(c *Code, at uint32, base, rbase int) *instrExit {
	k := instrExitKey{at, base, rbase}
	if e, ok := cj.instrs.Load(k); ok {
		return e.(*instrExit)
	}
	e := buildInstrExit(c, at, base, rbase)
	if prev, ok := cj.instrs.LoadOrStore(k, e); ok {
		return prev.(*instrExit)
	}
	jitStats.instrExits.Add(1)
	// What running code needs: charged, never refused.
	cj.mu.Lock()
	cj.recordBytes.Add(instrExitBytes)
	cj.budget.charge(instrExitBytes)
	cj.mu.Unlock()
	return e
}

func buildInstrExit(code *Code, at uint32, base, rbase int) *instrExit {
	m := code.Module
	e, ok := wjit.Effect(m, at, nil)
	if !ok {
		panic("wasm jit: no exit for " + e.Op.Name())
	}
	c := &regCompiler{code: code, m: m, last: -1, farPCs: map[int]bool{}, maxCode: maxRegCode}
	c.out.loops = map[int32]uint32{}
	c.r = reader{b: m.Bytes, pc: int(at)}
	// Stack position h is numeric slot base + 2h and reference slot
	// rbase + h.
	c.nlocals, c.rlocals = base, rbase
	for h, t := range e.Params {
		c.push(rentry{t: t, slot: c.posSlot(t, h), local: -1, def: -1})
	}
	c.instr()
	want := 0
	if e.HasResult() {
		want = 1
	}
	if len(c.stack) != want || c.r.pc != e.End {
		panic("wasm jit: no exit for " + e.Op.Name())
	}
	if want == 1 {
		c.materialize(0)
	}
	return &instrExit{rc: c.out, pure: pureInstr(e.Op)}
}

// pureInstr reports whether instruction op neither traps nor allocates.
func pureInstr(op opcode.Opcode) bool {
	switch op {
	case opcode.I32Popcnt, opcode.I64Popcnt, opcode.F32Ceil, opcode.F32Floor, opcode.F32Trunc, opcode.F32NearestInt,
		opcode.F64Ceil, opcode.F64Floor, opcode.F64Trunc, opcode.F64NearestInt:
		return true
	}
	if op.Prefix() != opcode.PrefixSIMD {
		return false
	}
	switch {
	case op >= opcode.S128LoadMem && op <= opcode.S128StoreMem,
		op == opcode.S128Load32Zero || op == opcode.S128Load64Zero,
		op >= opcode.S128Load8Lane && op <= opcode.S128Store64Lane:
		return false
	}
	return true
}

// run runs the instruction's code in frame f, a trap of it reporting
// offset at, the instruction's in its function.
func (e *instrExit) run(f *frame, at uint32) {
	if !e.pure {
		defer func() {
			if p := recover(); p != nil {
				f.rc = nil
				// A trap of the code's says its pc: the instruction's
				// offset stands for it.
				if t, ok := p.(trapSite); ok && t.at >= regPC {
					t.at = uint64(at)
					p = t
				}
				panic(p)
			}
		}()
	}
	f.rc = &e.rc
	l := unsafe.Pointer(&f.l[0])
	code := e.rc.code
	for pc := int32(0); int(pc) < len(code); {
		w := code[pc]
		pc = regTable[uint8(w)](l, w, pc, f)
	}
	f.rc = nil
}

// jitInstr runs the instruction native code of level lv left to Go
// (wjit.ExitInstr): at offset Arg0, its numeric operands from slot Arg1.
func (x *exec) jitInstr(lv *jitLevel) {
	ctx := &lv.ctx
	inst := lv.inst
	f := x.exitFrame(lv)
	at := uint32(ctx.Arg0)
	leaf := lv.cj.inline.leaf(at)
	base := int(ctx.Arg1)
	e := lv.cj.instrExit(inst.Code, leaf, base, f.rslot(base))
	fr := &lv.frame
	fr.x, fr.l, fr.r, fr.inst = x, f.l, f.r, inst
	if e.pure {
		e.run(fr, leaf)
	} else {
		x.jitSync(lv, at)
		x.takeFrom(lv, &f)
		e.run(fr, leaf)
		x.jitUnsync(lv)
	}
	fr.l, fr.r = nil, nil
}
