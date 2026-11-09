//go:build amd64 || arm64

package jit

import (
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// References. A reference is two words, a pointer and a payload, kept in
// the frame's reference slots (R), which the collector scans: a function's
// reference locals, then one slot for each stack position, as the engine's
// frames have them. A reference on the operand stack is always in its own
// position's slot: an instruction that makes one stores it there at once,
// and one that reads one reads the slot. A store of a pointer word into
// memory the collector sees -- a slot, a table's element, a global, an
// object's field -- tests the write barrier's flag and, while the
// collector marks, is made by the runtime's barrier (wbRoutine): the
// payload word is a plain store.

// slotPos is where a value is in a frame: reference slot i, or numeric
// slot i.
type slotPos struct {
	ref bool
	i   int
}

// layoutOf returns where values of types ts go as a function's parameters,
// locals or results, and the numeric and reference slots they take: a
// v128 takes two numeric slots, its low half first.
func layoutOf(ts []module.ValueType) (pos []slotPos, nnum, nref int) {
	pos = make([]slotPos, len(ts))
	for i, t := range ts {
		if t.IsRef() {
			pos[i] = slotPos{ref: true, i: nref}
			nref++
			continue
		}
		pos[i] = slotPos{i: nnum}
		nnum++
		if t.Kind == module.KindV128 {
			nnum++
		}
	}
	return pos, nnum, nref
}

// layout places the function's locals and results in its frame, as the
// engine does.
func (c *compiler) layout() {
	var nref int
	c.slots, c.numLocals, nref = layoutOf(c.locals)
	_, c.numParams, c.refParams = layoutOf(c.sig.Params)
	var rn, rr int
	c.results, rn, rr = layoutOf(c.sig.Results)
	c.nlocals = max(c.numLocals, rn)
	c.rlocals = max(nref, rr)
	c.refs = c.req.FrameRefSlots > 0
	if !c.refs && c.rlocals > 0 {
		panic("wasm jit: reference locals in a frame without reference slots")
	}
	if c.refs {
		c.adv = int32(16 * c.req.FrameRefSlots)
		c.gused |= 1 << rR
	}
}

// localOff is the offset of numeric local k's slot from L, localRefOff of
// reference local k's from R.
func (c *compiler) localOff(k int) int32    { return int32(8 * c.slots[k].i) }
func (c *compiler) localRefOff(k int) int32 { return int32(16 * c.slots[k].i) }

// mem is a memory operand: base plus off.
type mem struct {
	base reg
	off  int32
}

// wordKind is where a word to store is.
type wordKind uint8

const (
	wImm wordKind = iota
	wReg
	wMem
)

// word is a word to store: the constant c, register r or memory m.
type word struct {
	k wordKind
	r reg
	m mem
	c uint64
}

func immWord(v uint64) word { return word{k: wImm, c: v} }
func regWord(r reg) word    { return word{k: wReg, r: r} }
func memWord(m mem) word    { return word{k: wMem, m: m} }

// refPos is stack position h's reference slot.
func (c *compiler) refPos(h int) mem { return mem{rR, c.refSlotOff(h)} }

// refOf is reference entry v's slot.
func (c *compiler) refOf(v value) mem {
	if v.t != module.KindRef || v.k != kSlot {
		panic("wasm jit: a reference out of its slot")
	}
	return mem{rR, v.off}
}

// storeAt stores entry v into stack position h's slot.
func (c *compiler) storeAt(v value, h int) {
	if v.t != module.KindRef {
		c.store(v, rL, c.slotOff(h))
		return
	}
	if dst := c.refPos(h); dst != c.refOf(v) {
		c.refCopy(dst, c.refOf(v))
	}
}

// storeResult stores entry v into the slot of the function's result i.
func (c *compiler) storeResult(v value, i int) {
	s := c.results[i]
	if !s.ref {
		c.store(v, rL, int32(8*s.i))
		return
	}
	c.refCopy(mem{rR, int32(16 * s.i)}, c.refOf(v))
}

// copyPos copies the value of type t at stack position from into the
// slot of position to, whatever is in registers.
func (c *compiler) copyPos(t module.Kind, from, to int) {
	if t == module.KindRef {
		if from != to {
			c.refCopy(c.refPos(to), c.refPos(from))
		}
		return
	}
	if from != to {
		c.copyWord(mem{rL, c.slotOff(to)}, mem{rL, c.slotOff(from)})
		if t == module.KindV128 {
			c.copyWord(mem{rL, c.slotOff(to) + 8}, mem{rL, c.slotOff(from) + 8})
		}
	}
}

// copyPosResult copies the value of type t at stack position from into
// the slot of the function's result i.
func (c *compiler) copyPosResult(t module.Kind, from, i int) {
	s := c.results[i]
	if s.ref {
		c.refCopy(mem{rR, int32(16 * s.i)}, c.refPos(from))
		return
	}
	c.copyWord(mem{rL, int32(8 * s.i)}, mem{rL, c.slotOff(from)})
	if t == module.KindV128 {
		c.copyWord(mem{rL, int32(8*s.i) + 8}, mem{rL, c.slotOff(from) + 8})
	}
}

// argsTo copies a call's arguments, of types ts, from the stack positions
// from base into the parameters of a frame whose slots are at L plus num
// and R plus ref bytes.
func (c *compiler) argsTo(ts []module.ValueType, base int, num, ref int32) {
	pos, _, _ := layoutOf(ts)
	for i, p := range pos {
		if p.ref {
			c.refCopy(mem{rR, ref + int32(16*p.i)}, c.refPos(base+i))
			continue
		}
		c.copyWord(mem{rL, num + int32(8*p.i)}, mem{rL, c.slotOff(base + i)})
		if ts[i].Kind == module.KindV128 {
			c.copyWord(mem{rL, num + int32(8*p.i) + 8}, mem{rL, c.slotOff(base+i) + 8})
		}
	}
}

// resultsFrom copies a call's results, of types ts, from a callee frame
// whose slots are at L plus num and R plus ref bytes into the stack
// positions from base.
func (c *compiler) resultsFrom(ts []module.ValueType, base int, num, ref int32) {
	pos, _, _ := layoutOf(ts)
	for i, p := range pos {
		if p.ref {
			c.refCopy(c.refPos(base+i), mem{rR, ref + int32(16*p.i)})
			continue
		}
		c.copyWord(mem{rL, c.slotOff(base + i)}, mem{rL, num + int32(8*p.i)})
		if ts[i].Kind == module.KindV128 {
			c.copyWord(mem{rL, c.slotOff(base+i) + 8}, mem{rL, num + int32(8*p.i) + 8})
		}
	}
}

// hasRef reports whether any of ts is a reference.
func hasRef(ts []module.ValueType) bool {
	for _, t := range ts {
		if t.IsRef() {
			return true
		}
	}
	return false
}

// pushRef pushes the reference just stored into the slot of the position
// it takes.
func (c *compiler) pushRef() { c.pushSlot(module.KindRef) }

// refOp compiles the reference instructions, reporting whether op is one.
func (c *compiler) refOp(op opcode.Opcode, at uint32) bool {
	switch op {
	case opcode.RefNull:
		c.r.heapType()
		c.refPut(c.refPos(len(c.stack)), immWord(0), immWord(0))
		c.pushRef()
	case opcode.RefIsNull:
		v := c.pop()
		c.setCond(c.refNull(c.refOf(v)))
	case opcode.RefFunc:
		idx := c.r.u32()
		r := c.allocG(0)
		c.tableEntry(r, offFuncs, idx)
		c.refPut(c.refPos(len(c.stack)), regWord(r), immWord(0))
		c.freeG(r)
		c.pushRef()
	case opcode.RefAsNonNull:
		v := c.stack[len(c.stack)-1]
		c.jcc(c.refNull(c.refOf(v)), c.trap(TrapNullDereference, at))
	case opcode.RefEq:
		y := c.pop()
		x := c.pop()
		c.setCond(c.refEqual(c.refOf(x), c.refOf(y)))
	case opcode.BrOnNull:
		depth := int(c.r.u32())
		v := c.pop()
		c.brOn(depth, c.refNull(c.refOf(v)))
		c.push(v)
	case opcode.BrOnNonNull:
		depth := int(c.r.u32())
		v := c.stack[len(c.stack)-1]
		c.brOn(depth, c.refNull(c.refOf(v)).Invert())
		c.pop()
	case opcode.CallRef, opcode.ReturnCallRef:
		c.callRef(c.r.u32(), at, op == opcode.ReturnCallRef)
	case opcode.TableGet:
		c.tableGet(c.r.u32(), at)
	case opcode.TableSet:
		c.tableSet(c.r.u32(), at)
	default:
		return false
	}
	return true
}

// localGetRef pushes reference local k.
func (c *compiler) localGetRef(k int) {
	c.refCopy(c.refPos(len(c.stack)), mem{rR, c.localRefOff(k)})
	c.pushRef()
}

// localSetRef stores the reference on top of the stack into local k;
// local.tee leaves it.
func (c *compiler) localSetRef(k int, tee bool) {
	v := c.pop()
	c.refCopy(mem{rR, c.localRefOff(k)}, c.refOf(v))
	if tee {
		c.push(v)
	}
}

// globalGetRef pushes reference global g.
func (c *compiler) globalGetRef(g uint32) {
	r := c.allocG(0)
	c.tableEntry(r, offGlobals, g)
	c.refCopy(c.refPos(len(c.stack)), mem{r, 0})
	c.freeG(r)
	c.pushRef()
}

// globalSetRef stores the reference on top of the stack into global g.
func (c *compiler) globalSetRef(g uint32) {
	v := c.pop()
	r := c.allocG(0)
	c.tableEntry(r, offGlobals, g)
	c.refCopy(mem{r, 0}, c.refOf(v))
	c.freeG(r)
}

// selectRef compiles select of references: the first operand, in the
// result's slot already, unless the condition is false.
func (c *compiler) selectRef() {
	cond := c.pop()
	b := c.pop()
	x := c.pop()
	if cond.k == kConst {
		if uint32(cond.c) == 0 {
			c.refCopy(c.refOf(x), c.refOf(b))
		}
		c.push(x)
		return
	}
	keep := c.a.NewLabel()
	c.testCond(cond)
	c.jcc(condNonZero, keep)
	c.refCopy(c.refOf(x), c.refOf(b))
	c.a.Bind(keep)
	c.push(x)
}

// brOn compiles a branch to depth taken where cond holds, the flags just
// set, its values the top of the stack: br_if's, for a condition other
// than an i32's.
func (c *compiler) brOn(depth int, cond condCode) {
	c.push(value{t: module.KindI32, k: kFlags, c: uint64(cond)})
	c.brIf(depth)
}

// callRef compiles call_ref and return_call_ref of type typeIndex, which
// the engine makes.
func (c *compiler) callRef(typeIndex uint32, at uint32, tail bool) {
	ft := c.m.Types[typeIndex].Func
	n := len(ft.Params)
	c.spillAll()
	base := len(c.stack) - n - 1
	c.truncate(base)
	resume := c.a.NewLabel()
	if tail {
		c.exit(ExitTailRef, resume, uint64(typeIndex), c.slotIndex(base), uint64(at))
		c.resumeAt(resume)
		c.jmp(c.retL)
		c.markDead()
		return
	}
	c.exit(ExitCallRef, resume, uint64(typeIndex), c.slotIndex(base), uint64(at))
	c.resumeAt(resume)
	c.pushResults(ft.Results)
}

// tableElem leaves in a register the address of the element of table
// whose index is on top of the stack, popped, trapping past the table's
// end.
func (c *compiler) tableElem(table uint32, at uint32) reg {
	is64 := c.m.Tables[table].Limits.Is64
	idx := c.pop()
	if idx.k == kConst && !is64 {
		idx.c = uint64(uint32(idx.c))
	}
	r := c.intoG(idx, 0)
	t := c.allocG(1 << r)
	c.tableEntry(t, offTables, table)
	c.elemAddr(t, r, c.req.Layout.TableElems, c.trap(TrapTableOutOfBounds, at))
	c.freeG(r)
	return t
}

func (c *compiler) tableGet(table uint32, at uint32) {
	e := c.tableElem(table, at)
	c.refCopy(c.refPos(len(c.stack)), mem{e, 0})
	c.freeG(e)
	c.pushRef()
}

func (c *compiler) tableSet(table uint32, at uint32) {
	v := c.pop()
	e := c.tableElem(table, at)
	c.refCopy(mem{e, 0}, c.refOf(v))
	c.freeG(e)
}

// wbRoutine returns the function's barrier routine, made out of line with
// the first store that needs it.
func (c *compiler) wbRoutine() asmLabel {
	if !c.hasWB {
		c.hasWB = true
		c.wb = c.a.NewLabel()
		l := c.wb
		c.ool = append(c.ool, func() { c.emitWBRoutine(l) })
	}
	return c.wb
}
