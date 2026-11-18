package engine

import (
	"unsafe"

	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// Register code's tables and the calls of a function a table or a
// reference names.

// The table and indirect call operations of the second table.
const (
	xTableGet           regOp = 96 + iota // reference A = table[B], the table in the word after
	xTableSet                             // table[A] = reference B
	xTableSize                            // A = table BC's size
	xTableGrow                            // A = table grown by B of reference C
	xTableFill                            // table.fill: destination A, reference B, count C
	xTableCopy                            // table.copy: destination A, source B, count C
	xTableInit                            // table.init: destination A, source B, count C
	xElemDrop                             // elem.drop of segment BC
	xCallIndirect                         // the call of sites[BC] of table entry A
	xCallRef                              // the call of sites[BC] of reference A
	xReturnCallIndirect                   // the tail call of sites[BC] of table entry A
	xReturnCallRef                        // the tail call of sites[BC] of reference A
)

// tableOp compiles a table instruction or an indirect call, reporting
// whether op is one.
func (c *regCompiler) tableOp(op opcode.Opcode) bool {
	switch op {
	case opcode.TableGet:
		table := c.r.u32()
		i := c.pop()
		h := len(c.stack)
		is := c.read(i, h)
		t := c.m.Tables[table].Type
		c.def(t, h, c.emit(rExt, xTableGet, c.dst(t, h), is, 0, uint64(table)))
	case opcode.TableSet:
		table := c.r.u32()
		v := c.pop()
		i := c.pop()
		h := len(c.stack)
		is := c.read(i, h)
		vs := c.read(v, h+1)
		c.emit(rExt, xTableSet, is, vs, 0, uint64(table))
	case opcode.TableSize:
		table := c.r.u32()
		h := len(c.stack)
		c.def(c.tableIndex(table), h, c.emitBC(rExt, xTableSize, ns(c.numSlot(h)), table))
	case opcode.TableGrow:
		table := c.r.u32()
		n := c.pop()
		v := c.pop()
		h := len(c.stack)
		vs := c.read(v, h)
		nsl := c.read(n, h+1)
		c.def(c.tableIndex(table), h, c.emit(rExt, xTableGrow, ns(c.numSlot(h)), nsl, vs, uint64(table)))
	case opcode.TableFill:
		c.bulk(xTableFill, uint64(c.r.u32()))
	case opcode.TableCopy:
		dt := c.r.u32()
		st := c.r.u32()
		c.bulk(xTableCopy, uint64(dt)|uint64(st)<<32)
	case opcode.TableInit:
		seg := c.r.u32()
		table := c.r.u32()
		c.bulk(xTableInit, uint64(seg)|uint64(table)<<32)
	case opcode.ElemDrop:
		c.emitBC(rExt, xElemDrop, 0, c.r.u32())
	case opcode.CallIndirect, opcode.ReturnCallIndirect:
		ti := c.r.u32()
		table := c.r.u32()
		s := regSite{at: c.at, typeIndex: ti, table: table, checked: indirectChecksType(c.m, ti, table)}
		x := xCallIndirect
		if op == opcode.ReturnCallIndirect {
			x = xReturnCallIndirect
		}
		c.callBy(x, op == opcode.ReturnCallIndirect, c.m.Types[ti].Func, s)
	case opcode.CallRef, opcode.ReturnCallRef:
		ti := c.r.u32()
		x := xCallRef
		if op == opcode.ReturnCallRef {
			x = xReturnCallRef
		}
		c.callBy(x, op == opcode.ReturnCallRef, c.m.Types[ti].Func, regSite{at: c.at})
	default:
		return false
	}
	return true
}

// tableIndex returns the type of table's indices and sizes.
func (c *regCompiler) tableIndex(table uint32) module.ValueType {
	if c.m.Tables[table].Limits.Is64 {
		return module.I64
	}
	return module.I32
}

// callBy compiles a call, or a tail call, of signature sig of the function
// the operand on top of the stack names, a table index or a reference.
func (c *regCompiler) callBy(x uint8, tail bool, sig *module.FuncType, s regSite) {
	target := c.pop()
	s.target = uint32(c.slotOf(target, len(c.stack)))
	var base int
	s.args, base = c.siteArgs(sig)
	if !tail {
		s.rets = c.siteRets(sig, base)
	}
	si := len(c.out.sites)
	c.out.sites = append(c.out.sites, s)
	c.emitBC(rExt, x, 0, uint32(si))
	if tail {
		c.markDead()
		return
	}
	for i, t := range sig.Results {
		c.pushSlot(t, base+i)
	}
}

// regCallFn calls fn from site s of the frame f at pc.
func regCallFn(l unsafe.Pointer, f *frame, s *regSite, fn *Function) {
	x := f.x
	x.calls[len(x.calls)-1].offset = s.at
	if fn.Host != nil {
		regHostCall(l, f, fn, s)
		return
	}
	cd := fn.Inst.Code.get(fn.Index)
	nc, nsp, rc, rsp := x.nc, x.nsp, x.rc, x.rsp
	nl, _, _ := x.takeNum(cd.nslots)
	nr, _, _ := x.takeRef(cd.rslots)
	regArgs(l, f, s.args, nl, nr)
	x.run(fn, cd, nl, nr, nc, nsp, rc, rsp)
	regRets(l, f, s.rets, nl, nr)
	x.nc, x.nsp, x.rc, x.rsp = nc, nsp, rc, rsp
}

// regTailFn makes the tail call of fn from site s.
func regTailFn(l unsafe.Pointer, f *frame, s *regSite, fn *Function) int32 {
	x := f.x
	x.tailArgs = x.tailArgs[:0]
	for _, m := range s.args {
		x.tailArgs = append(x.tailArgs, getVal(l, f, m.k, m.from))
	}
	x.tail = fn
	f.sig = sigTail
	return regDone
}

// regIndirect returns the function the call_indirect of site s finds at
// the table index in its target's slot.
func regIndirect(l unsafe.Pointer, f *frame, s *regSite) *Function {
	return indirectCallee(f.inst, s.typeIndex, s.table, num(l, s.target), s.checked, s.at)
}

// regReferenced returns the function the reference in site s's target's
// slot names, trapping on null at its call.
func regReferenced(f *frame, s *regSite) *Function {
	r := f.r[s.target]
	if r.IsNull() {
		trapAt(TrapNullDereference, s.at)
	}
	return r.Func()
}

func rhCallIndirect(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	s := &f.rc.sites[uint32(w>>32)]
	regCallFn(l, f, s, regIndirect(l, f, s))
	return pc + 1
}

func rhCallRef(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	s := &f.rc.sites[uint32(w>>32)]
	regCallFn(l, f, s, regReferenced(f, s))
	return pc + 1
}

func rhReturnCallIndirect(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	s := &f.rc.sites[uint32(w>>32)]
	return regTailFn(l, f, s, regIndirect(l, f, s))
}

func rhReturnCallRef(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	s := &f.rc.sites[uint32(w>>32)]
	return regTailFn(l, f, s, regReferenced(f, s))
}

func rhTableGet(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	e := f.inst.Tables[uint32(f.rc.code[pc+1])].Elems
	i := *rs(l, w>>32)
	if i >= uint64(len(e)) {
		trapAt(TrapTableOutOfBounds, regAt(pc))
	}
	*f.rr(w >> 16) = e[i]
	return pc + 2
}

func rhTableSet(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	e := f.inst.Tables[uint32(f.rc.code[pc+1])].Elems
	i := *rs(l, w>>16)
	if i >= uint64(len(e)) {
		trapAt(TrapTableOutOfBounds, regAt(pc))
	}
	e[i] = *f.rr(w >> 32)
	return pc + 2
}

func rhTableSize(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	*rs(l, w>>16) = f.inst.Tables[uint32(w>>32)].Size()
	return pc + 1
}

func rhTableGrow(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	t := f.inst.Tables[uint32(f.rc.code[pc+1])]
	r := uint64(t.Grow(*rs(l, w>>32), *f.rr(w >> 48)))
	if !t.Is64 {
		r = uint64(uint32(r))
	}
	*rs(l, w>>16) = r
	return pc + 2
}

func rhTableFill(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	dst, val, cnt := *rs(l, w>>16), *f.rr(w >> 32), *rs(l, w>>48)
	e := f.inst.Tables[uint32(f.rc.code[pc+1])].Elems
	checkRange(dst, cnt, uint64(len(e)), TrapTableOutOfBounds, regAt(pc))
	s := e[dst : dst+cnt]
	for i := range s {
		s[i] = val
	}
	return pc + 2
}

func rhTableCopy(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	imm := f.rc.code[pc+1]
	dst, src, cnt := *rs(l, w>>16), *rs(l, w>>32), *rs(l, w>>48)
	de := f.inst.Tables[uint32(imm)].Elems
	se := f.inst.Tables[uint32(imm>>32)].Elems
	checkRange(src, cnt, uint64(len(se)), TrapTableOutOfBounds, regAt(pc))
	checkRange(dst, cnt, uint64(len(de)), TrapTableOutOfBounds, regAt(pc))
	copy(de[dst:dst+cnt], se[src:src+cnt])
	return pc + 2
}

func rhTableInit(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	imm := f.rc.code[pc+1]
	dst, src, cnt := *rs(l, w>>16), uint64(uint32(*rs(l, w>>32))), uint64(uint32(*rs(l, w>>48)))
	elems := f.inst.elems[uint32(imm)]
	t := f.inst.Tables[uint32(imm>>32)]
	checkRange(dst, cnt, uint64(len(t.Elems)), TrapTableOutOfBounds, regAt(pc))
	checkRange(src, cnt, uint64(len(elems)), TrapElementSegmentOutOfBounds, regAt(pc))
	copy(t.Elems[dst:dst+cnt], elems[src:src+cnt])
	return pc + 2
}

func rhElemDrop(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	f.inst.elems[uint32(w>>32)] = nil
	return pc + 1
}

func init() {
	for x, h := range map[regOp]regHandler{
		xTableGet: rhTableGet, xTableSet: rhTableSet, xTableSize: rhTableSize, xTableGrow: rhTableGrow,
		xTableFill: rhTableFill, xTableCopy: rhTableCopy, xTableInit: rhTableInit, xElemDrop: rhElemDrop,
		xCallIndirect: rhCallIndirect, xCallRef: rhCallRef,
		xReturnCallIndirect: rhReturnCallIndirect, xReturnCallRef: rhReturnCallRef,
	} {
		regExtTable[x] = h
	}
}
