package vm

import (
	"math"
	"math/bits"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// The register interpreter's handlers (docs/register-interpreter.md, "The
// loop"): a function an operation, which runReg calls through regTable.
// Each is given the frame's registers, the instruction, its pc and the
// frame's state, and returns the pc to go on at:
//
//   - pc+1, or a jump's target, where it has done the instruction;
//   - ^pc where its fast path has not -- an operand of another kind, a
//     cache that missed, a store of a number into a register that holds a
//     pointer, a back edge that spent the interrupt budget -- for runReg to
//     run the operation's slow function, from regSlowTable, which does
//     the whole instruction as the stack form does;
//   - regDone where the frame has returned, by ret or ret_undef, whose
//     value runReg reads from the instruction, or has thrown what no
//     handler of its own catches, with st.err.
//
// A handler whose fast path makes no call and stores no pointer -- a number
// stored by its num word alone, into a register whose ref is nil -- is a
// leaf: Go gives it no frame and no stack check. That is why a fast path
// gives back ^pc rather than calling its slow function.
//
// A handler or a slow function that can throw or run code keeps the pc
// past its instruction in st.kept first (st.at), as runReg's cases kept it:
// the fail tail (regState.fail) finds the instruction that threw by it,
// and a call it makes reads it rather than being given it.

// regHandler is a handler of an operation, or its slow function.
type regHandler func(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int

// regTable is each operation's handler; regSlowTable the slow function a
// handler's ^pc runs. Every value an ROp can hold has an entry.
var regTable, regSlowTable [256]regHandler

func init() {
	for i := range regTable {
		regTable[i], regSlowTable[i] = rhInvalid, rhInvalid
	}
	// An operation with a fast path: its handler, and its slow function.
	for _, e := range []struct {
		op         bytecode.ROp
		fast, slow regHandler
	}{
		{bytecode.RLoadInt, rhLoadInt, rsLoadInt},
		{bytecode.RLoadUndef, rhLoadUndef, rsLoadUndef},
		{bytecode.RLoadNull, rhLoadNull, rsLoadNull},
		{bytecode.RLoadTrue, rhLoadTrue, rsLoadTrue},
		{bytecode.RLoadFalse, rhLoadFalse, rsLoadFalse},
		{bytecode.RLoadUninit, rhLoadUninit, rsLoadUninit},
		{bytecode.RGetGlobal, rhGetGlobal, rsGetGlobal},
		{bytecode.RSetGlobal, rhSetGlobal, rsSetGlobal},
		{bytecode.RGetProp, rhGetProp, rsGetProp},
		{bytecode.RGetMethod, rhGetProp, rsGetMethod},
		{bytecode.RGetMethodW, rhGetMethodW, rsGetMethodW},
		{bytecode.RSetProp, rhSetProp, rsSetProp},
		{bytecode.RGetIndex, rhGetIndex, rsGetIndex},
		{bytecode.RGetIndexPre, rhGetIndexPre, rsGetIndexPre},
		{bytecode.RGetIndexPost, rhGetIndexPost, rsGetIndexPost},
		{bytecode.RSetIndex, rhSetIndex, rsSetIndex},
		{bytecode.RGetIndexI, rhGetIndexI, rsGetIndexI},
		{bytecode.RSetIndexI, rhSetIndexI, rsSetIndexI},
		{bytecode.RAdd, rhAdd, rsAdd},
		{bytecode.RSub, rhSub, rsSub},
		{bytecode.RMul, rhMul, rsMul},
		{bytecode.RInc, rhInc, rsInc},
		{bytecode.RDec, rhDec, rsDec},
		{bytecode.RAddI, rhAddI, rsAddI},
		{bytecode.RSubI, rhSubI, rsSubI},
		{bytecode.RAndI, rhAndI, rsAndI},
		{bytecode.ROrI, rhOrI, rsOrI},
		{bytecode.RXorI, rhXorI, rsXorI},
		{bytecode.RShlI, rhShlI, rsShlI},
		{bytecode.RShrI, rhShrI, rsShrI},
		{bytecode.RUShrI, rhUShrI, rsUShrI},
		{bytecode.RBitAnd, rhBitAnd, rsBitAnd},
		{bytecode.RBitOr, rhBitOr, rsBitOr},
		{bytecode.RBitXor, rhBitXor, rsBitXor},
		{bytecode.RShl, rhShl, rsShl},
		{bytecode.RShr, rhShr, rsShr},
		{bytecode.RUShr, rhUShr, rsUShr},
		{bytecode.RPostInc, rhPostInc, rsPostStep},
		{bytecode.RPostDec, rhPostDec, rsPostStep},
		{bytecode.RGetLength, rhGetLength, rsGetLength},
		{bytecode.RAddProp, rhAddProp, rsAddProp},
		{bytecode.RAddPropI, rhAddPropI, rsAddProp},
		{bytecode.RAddIndexK, rhAddIndexK, rsAddIndexK},
		{bytecode.RStrPart, rhStrPart, rsStrPart},
		{bytecode.RKeyOfBase, rhKeyOfBase, rsKeyOfBase},
		{bytecode.RJmp, rhJmp, rsJmp},
		{bytecode.RJmpF, rhJmpF, rsTruth},
		{bytecode.RJmpT, rhJmpT, rsTruth},
		{bytecode.RJLt, rhJLt, rsCompare},
		{bytecode.RJNLt, rhJNLt, rsCompare},
		{bytecode.RJLe, rhJLe, rsCompare},
		{bytecode.RJNLe, rhJNLe, rsCompare},
		{bytecode.RJGt, rhJGt, rsCompare},
		{bytecode.RJNGt, rhJNGt, rsCompare},
		{bytecode.RJGe, rhJGe, rsCompare},
		{bytecode.RJNGe, rhJNGe, rsCompare},
		{bytecode.RJLtI, rhJLtI, rsCompare},
		{bytecode.RJNLtI, rhJNLtI, rsCompare},
		{bytecode.RJLeI, rhJLeI, rsCompare},
		{bytecode.RJNLeI, rhJNLeI, rsCompare},
		{bytecode.RJGtI, rhJGtI, rsCompare},
		{bytecode.RJNGtI, rhJNGtI, rsCompare},
		{bytecode.RJGeI, rhJGeI, rsCompare},
		{bytecode.RJNGeI, rhJNGeI, rsCompare},
		{bytecode.RJEq, rhJEq, rsCompare},
		{bytecode.RJNe, rhJNe, rsCompare},
		{bytecode.RJEqI, rhJEqI, rsCompare},
		{bytecode.RJNeI, rhJNeI, rsCompare},
		{bytecode.RJNull, rhJNull, rsCompare},
		{bytecode.RJNotNull, rhJNotNull, rsCompare},
		{bytecode.RJLtU, rhJLtU, rsCompare},
		{bytecode.RJNLtU, rhJNLtU, rsCompare},
		{bytecode.RJLeU, rhJLeU, rsCompare},
		{bytecode.RJNLeU, rhJNLeU, rsCompare},
		{bytecode.RJSEq, rhJSEq, rsStrictEq},
		{bytecode.RJSNe, rhJSNe, rsStrictEq},
		{bytecode.RJSEqI, rhJSEqI, rsJump},
		{bytecode.RJSNeI, rhJSNeI, rsJump},
		{bytecode.RJTypeof, rhJTypeof, rsJTypeof},
	} {
		regTable[e.op], regSlowTable[e.op] = e.fast, e.slow
	}
	// An operation of no fast path: its handler does it all.
	for op, h := range map[bytecode.ROp]regHandler{
		bytecode.RNop:          rhNop,
		bytecode.RMove:         rhMove,
		bytecode.RLoadConst:    rhLoadConst,
		bytecode.RLoadEmpty:    rhLoadEmpty,
		bytecode.RLoadThis:     rhLoadThis,
		bytecode.RCheckRef:     rhCheckRef,
		bytecode.RGetUpval:     rhGetUpval,
		bytecode.RSetUpval:     rhSetUpval,
		bytecode.RDefineField:  rsDefineField,
		bytecode.RNewObject:    rsNewObject,
		bytecode.RNewArray:     rsNewArray,
		bytecode.RIterNext:     rhIterNext,
		bytecode.RBin:          rsBin,
		bytecode.RLit:          rhLit,
		bytecode.RUnary:        rsUnary,
		bytecode.RCallWT:       rhCallWT,
		bytecode.RCallMWT:      rhCallWT,
		bytecode.RRet:          rhRet,
		bytecode.RRetUndef:     rhRetUndef,
		bytecode.RGetPrivate:   rhGetPrivate,
		bytecode.RSetPrivate:   rhSetPrivate,
		bytecode.RYield:        rhYield,
		bytecode.RCallW:        rhCallW,
		bytecode.RCallMW:       rhCallW,
		bytecode.RNewW:         rsNewW,
		bytecode.RClosureW:     rsClosureW,
		bytecode.RThrow:        rhThrow,
		bytecode.RInstanceOf:   rsInstanceOf,
		bytecode.RIn:           rsIn,
		bytecode.RDelete:       rsDelete,
		bytecode.RArguments:    rsArguments,
		bytecode.RNewArrayK:    rsNewArrayK,
		bytecode.RApplyArgs:    rsApplyArgs,
		bytecode.RArgsLen:      rsArgsLen,
		bytecode.RArgsIndex:    rsArgsIndex,
		bytecode.RConcat:       rsConcat,
		bytecode.RNewArrayE:    rhNewArrayE,
		bytecode.RAddIndex:     rsAddIndex,
		bytecode.RAddIndexI:    rsAddIndexI,
		bytecode.RSwitch:       rhSwitch,
		bytecode.RTypeof:       rhTypeof,
		bytecode.RSwitchTypeof: rhSwitchTypeof,
		bytecode.RNewObjectLit: rhNewObjectLit,
		bytecode.RAppend:       rhAppend,
		bytecode.RStrFreeze:    rhStrFreeze,
		bytecode.RCharCodeAt:   rhCharCodeAt,
	} {
		regTable[op] = h
	}
	regColdInit()
}

// rhInvalid is the handler of a value no operation has, which the
// verifier proves no code holds.
func rhInvalid(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	panic("register code: invalid instruction " + w.Op().String())
}

// regJump is a jump from pc by off, taken: one back counts the interrupt
// budget, whose end leaves the instruction to its slow function.
func regJump(st *regState, pc int, off int32) int {
	if off < 0 {
		r := st.r
		if r.backEdges--; r.backEdges <= 0 {
			return ^pc
		}
	}
	return pc + 1 + int(off)
}

// regJumpSlow is a jump from pc by off, taken, in a slow function: the
// interrupt check where the budget is spent.
func regJumpSlow(st *regState, pc int, off int32) int {
	if off < 0 {
		if r := st.r; r.backEdges <= 1 {
			return r.regBackEdge(st, pc, pc+1+int(off))
		} else {
			r.backEdges--
		}
	}
	return pc + 1 + int(off)
}

func rhNop(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return pc + 1
}

func rhMove(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	regs[w.A()] = regs[w.B()]
	return pc + 1
}

func rhLoadInt(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if p := &regs[w.A()]; p.ref == nil {
		p.num = float64(int32(w.BC()))
		return pc + 1
	}
	return ^pc
}

func rsLoadInt(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	regs[w.A()] = Int32(int32(w.BC()))
	return pc + 1
}

func rhLoadConst(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	regs[w.A()] = st.cl.consts[w.BC()]
	return pc + 1
}

func rhLoadUndef(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if p := &regs[w.A()]; p.ref == nil {
		p.num = Undefined.num
		return pc + 1
	}
	return ^pc
}

func rsLoadUndef(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	regs[w.A()] = Undefined
	return pc + 1
}

func rhLoadNull(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if p := &regs[w.A()]; p.ref == nil {
		p.num = Null.num
		return pc + 1
	}
	return ^pc
}

func rsLoadNull(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	regs[w.A()] = Null
	return pc + 1
}

func rhLoadTrue(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if p := &regs[w.A()]; p.ref == nil {
		p.num = True.num
		return pc + 1
	}
	return ^pc
}

func rsLoadTrue(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	regs[w.A()] = True
	return pc + 1
}

func rhLoadFalse(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if p := &regs[w.A()]; p.ref == nil {
		p.num = False.num
		return pc + 1
	}
	return ^pc
}

func rsLoadFalse(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	regs[w.A()] = False
	return pc + 1
}

func rhLoadEmpty(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	regs[w.A()] = Value{num: mkTag(KindString, 0), ref: unsafe.Pointer(emptyString)}
	return pc + 1
}

func rhLoadUninit(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if p := &regs[w.A()]; p.ref == nil {
		p.num = uninitialized.num
		return pc + 1
	}
	return ^pc
}

func rsLoadUninit(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	regs[w.A()] = uninitialized
	return pc + 1
}

func rhLoadThis(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	regs[w.A()] = st.f.this
	return pc + 1
}

func rhCheckRef(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if regTagged(math.Float64bits(regs[w.A()].num), KindUninitialized) {
		st.kept.pc = pc + 1
		return st.fail(st.r.regThrowDeadZone(st.f, st.c, pc+1))
	}
	return pc + 1
}

func rhGetUpval(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	regs[w.A()] = st.cl.upvalues[w.BC()].get()
	return pc + 1
}

func rhSetUpval(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	st.cl.upvalues[w.BC()].set(regs[w.A()])
	return pc + 1
}

// rhGetGlobal reads the global object's slot the site remembers, while
// the global epoch is the one it was filled at (regNoteGlobal): no binding
// of the global object has moved, gone or changed its attributes since,
// and no script-level lexical binding shadows it, nor an eval's variable,
// which a frame of the register form never has.
func rhGetGlobal(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	cl := st.cl
	if ep := cl.regEpochs; uint(w.C()) < uint(len(ep)) && ep[w.C()] == st.r.shapes.globalEpoch+1 {
		s := &cl.ic[w.C()]
		if env, i := s.p1, uint(s.idx); i < uint(env.valn) {
			regs[w.A()] = env.valAt(int(i))
			return pc + 1
		}
	}
	return ^pc
}

func rsGetGlobal(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	f := st.at(pc)
	v, err := st.r.regGetGlobal(f, st.cl, st.c, pc, w)
	return st.put(regs, w, v, err)
}

func rhSetGlobal(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	cl := st.cl
	if ep := cl.regEpochs; uint(w.C()) < uint(len(ep)) && ep[w.C()] == st.r.shapes.globalEpoch+1 {
		s := &cl.ic[w.C()]
		if env, i := s.p1, uint(s.idx); i < uint(env.valn) {
			env.vals()[i] = regs[w.A()]
			return pc + 1
		}
	}
	return ^pc
}

func rsSetGlobal(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	f := st.at(pc)
	return st.done(st.r.regSetGlobal(f, st.cl, st.c, pc, w, regs[w.A()]))
}

// rhGetProp is get_prop's and get_method's: an own data property the
// site remembers for the object's shape, or a prototype's data property,
// a method, in the holder propCache.holder finds, written out.
func rhGetProp(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if o := regs[w.B()]; o.IsObject() {
		obj := o.Object()
		s := &st.cl.ic[w.C()]
		if obj.shape == s.ownShape {
			regs[w.A()] = obj.valAt(int(s.idx))
			return pc + 1
		}
		if obj.shape == s.shape {
			h := obj
			if s.cell != nil {
				h = nil
				if s.cell.valid {
					h = s.p2
				}
			} else if p1 := s.p1; p1 != nil {
				h = nil
				if p1.shape == s.s1 {
					h = p1
					if q := s.p2; q != nil {
						h = nil
						if q.shape == s.s2 {
							h = q
						}
					}
				}
			}
			if h != nil && !s.getter {
				regs[w.A()] = h.valAt(int(s.idx))
				return pc + 1
			}
		}
	}
	return ^pc
}

func rsGetProp(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	st.at(pc)
	v, err := st.r.regGetProp(st.cl, st.c, w, regs[w.B()])
	return st.put(regs, w, v, err)
}

func rsGetMethod(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	st.at(pc)
	v, err := st.r.regGetMethod(st.cl, st.c, w, regs[w.B()])
	return st.put(regs, w, v, err)
}

// rhGetMethodW is get_method_w: the receiver put in the call's window
// beside its method.
func rhGetMethodW(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if o := regs[w.B()]; o.IsObject() {
		obj := o.Object()
		s := &st.cl.ic[w.C()]
		p := (*[2]Value)(unsafe.Pointer(&regs[w.A()]))
		if obj.shape == s.ownShape {
			p[0], p[1] = obj.valAt(int(s.idx)), o
			return pc + 1
		}
		if obj.shape == s.shape {
			h := obj
			if s.cell != nil {
				h = nil
				if s.cell.valid {
					h = s.p2
				}
			} else if p1 := s.p1; p1 != nil {
				h = nil
				if p1.shape == s.s1 {
					h = p1
					if q := s.p2; q != nil {
						h = nil
						if q.shape == s.s2 {
							h = q
						}
					}
				}
			}
			if h != nil && !s.getter {
				p[0], p[1] = h.valAt(int(s.idx)), o
				return pc + 1
			}
		}
	}
	return ^pc
}

func rsGetMethodW(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	st.at(pc)
	o := regs[w.B()]
	v, err := st.r.regGetMethod(st.cl, st.c, w, o)
	(*[2]Value)(unsafe.Pointer(&regs[w.A()]))[1] = o
	return st.put(regs, w, v, err)
}

func rhSetProp(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if o := regs[w.A()]; o.IsObject() {
		obj := o.Object()
		s := &st.cl.ic[w.C()]
		if obj.shape == s.ownShape {
			obj.vals()[s.idx] = regs[w.B()]
			return pc + 1
		}
	}
	return ^pc
}

// rsSetProp is set_prop's slow path: a property it adds by the transition
// the site remembers, where nothing up the prototype chain the site found
// intercepts it -- storeCached's addition, written out -- and the store's
// slow path otherwise.
func rsSetProp(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if o := regs[w.A()]; o.IsObject() {
		obj := o.Object()
		cl := st.cl
		s := &cl.ic[w.C()]
		if obj.shape == s.shape && s.next != nil && s.next != setterNext && obj.flags&objExtensible != 0 &&
			(s.p1 == nil || s.p1.shape == s.s1 || s.cell != nil && s.cell.valid) && (s.p2 == nil || s.p2.shape == s.s2) {
			if verifyShapes {
				regVerifyAdd(st.r, s, obj, cl, st.c, w)
			}
			obj.appendTransition(regs[w.B()], s.next)
			return pc + 1
		}
	}
	st.at(pc)
	return st.done(st.r.regSetProp(st.cl, st.c, w, regs[w.A()], regs[w.B()]))
}

// rhGetIndex is get_index: an element of an object's dense elements, or a
// typed array's, read as native code reads it (regTypedAt, regTypedLoad,
// written out).
func rhGetIndex(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	obj, key := regs[w.B()], regs[w.C()]
	if obj.IsObject() && key.IsNumber() {
		o := obj.Object()
		// One test of the flags for both: o has elements, and is no
		// mapped arguments object, whose elements are its frame's.
		if i := uint32(key.Number()); float64(i) == key.Number() &&
			o.flags&(objHasExt|objMappedArguments) == objHasExt {
			if es := o.extAt().elems; uint(i) < uint(len(es)) {
				if v := es[i]; !isHole(v) {
					regs[w.A()] = v
					return pc + 1
				}
			}
		}
		if o.class == ClassTypedArray {
			if p := &regs[w.A()]; p.ref == nil {
				if n, ok := regTypedRead(st.r, o, key.num); ok {
					p.num = n
					return pc + 1
				}
			}
		}
	}
	return ^pc
}

// regTypedRead is a typed array's element at k as a number, in the
// register form's own number: false where o's view is stale, k no index
// in it, or its kind a BigInt's.
func regTypedRead(r *Runtime, o *Object, k float64) (float64, bool) {
	if t, ok := o.dataOf().(*typedArrayData); ok && t.epoch == r.typedEpoch && t.kind <= elemFloat64 {
		if i := uint32(k); float64(i) == k && uint(i) < uint(t.length) {
			p := unsafe.Add(t.data, uintptr(i)<<elemShifts[t.kind&15])
			var n float64
			switch t.kind {
			case elemInt32:
				n = float64(*(*int32)(p))
			case elemUint8, elemUint8Clamped:
				n = float64(*(*uint8)(p))
			case elemUint16:
				n = float64(*(*uint16)(p))
			case elemInt16:
				n = float64(*(*int16)(p))
			case elemInt8:
				n = float64(*(*int8)(p))
			case elemUint32:
				n = float64(*(*uint32)(p))
			case elemFloat64:
				n = *(*float64)(p)
			case elemFloat32:
				n = float64(*(*float32)(p))
			default:
				return 0, false
			}
			return regNum(n).num, true
		}
	}
	return 0, false
}

// rsGetIndex is get_index's slow path: a miss path's element (regElemMiss),
// a key string the runtime handed out -- a for-in's -- read as the tree's
// element read reads it (stringKeyRead), a[k] in for (k in a) an element by
// the key's index, and the read as the stack form makes it otherwise.
func rsGetIndex(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	st.kept.pc = pc + 1
	r := st.r
	if o, key := regs[w.B()], regs[w.C()]; o.IsObject() && key.IsString() {
		// A string key is no miss path's: it is looked for first.
		if x, ok := r.stringKeyRead(o.Object(), key.String()); ok {
			regs[w.A()] = x
			return pc + 1
		}
	} else if regElemMiss(st.c, &st.kept, regs) {
		return pc + 1
	}
	st.at(pc)
	v, err := r.getIndexed(regs[w.B()], regs[w.C()])
	return st.put(regs, w, v, err)
}

// rhGetIndexPre is x[++i]: the key stepped, then the element read; neither
// is written until both are known, so that the slow path does both.
func rhGetIndexPre(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	obj, key := regs[w.B()], regs[w.C()]
	if obj.IsObject() && key.IsNumber() {
		k := key.Number() + float64(int8(w.X()))
		o := obj.Object()
		if i := uint32(k); float64(i) == k && o.flags&(objHasExt|objMappedArguments) == objHasExt {
			if es := o.extAt().elems; uint(i) < uint(len(es)) {
				if v := es[i]; !isHole(v) {
					regs[w.C()] = Value{num: k}
					regs[w.A()] = v
					return pc + 1
				}
			}
		}
	}
	return ^pc
}

// rsGetIndexPre is x[++i]'s slow path: the step, then the read, each as the
// stack form makes it.
func rsGetIndexPre(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	r := st.r
	st.at(pc)
	n, err := r.regStep(regs[w.C()], float64(int8(w.X())))
	if err != nil {
		return st.fail(err)
	}
	regs[w.C()] = n
	v, err := r.getIndexed(regs[w.B()], n)
	return st.put(regs, w, v, err)
}

// rhGetIndexPost is x[i++]: the element read at the key, then the key
// stepped; neither is written until both are known.
func rhGetIndexPost(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	obj, key := regs[w.B()], regs[w.C()]
	if obj.IsObject() && key.IsNumber() {
		o := obj.Object()
		if i := uint32(key.Number()); float64(i) == key.Number() &&
			o.flags&(objHasExt|objMappedArguments) == objHasExt {
			if es := o.extAt().elems; uint(i) < uint(len(es)) {
				if v := es[i]; !isHole(v) {
					regs[w.C()] = Value{num: key.Number() + float64(int8(w.X()))}
					regs[w.A()] = v
					return pc + 1
				}
			}
		}
	}
	return ^pc
}

// rsGetIndexPost is x[i++]'s slow path: the step, its old value the key,
// then the read, as the stack form makes them.
func rsGetIndexPost(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	r := st.r
	st.at(pc)
	old, err := r.toNumeric(regs[w.C()])
	if err != nil {
		return st.fail(err)
	}
	n, err := r.regStep(old, float64(int8(w.X())))
	if err != nil {
		return st.fail(err)
	}
	o := regs[w.B()]
	regs[w.C()] = n
	v, err := r.getIndexed(o, old)
	return st.put(regs, w, v, err)
}

func rhSetIndex(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	obj, key := regs[w.A()], regs[w.B()]
	if obj.IsObject() && key.IsNumber() {
		o := obj.Object()
		if i := uint32(key.Number()); float64(i) == key.Number() &&
			o.flags&(objHasExt|objMappedArguments|objSharedElems) == objHasExt {
			if es := o.extAt().elems; uint(i) < uint(len(es)) && !isHole(es[i]) {
				es[i] = regs[w.C()]
				return pc + 1
			}
		}
	}
	return ^pc
}

// rsSetIndex is set_index's slow path: an append or a typed array's
// element (regSetMiss), and the store as the stack form makes it.
func rsSetIndex(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	r := st.r
	st.kept.pc = pc + 1
	if r.regSetMiss(st.c, &st.kept, regs) {
		return pc + 1
	}
	st.at(pc)
	return st.done(r.setIndexed(regs[w.A()], regs[w.B()], regs[w.C()], st.cl.fn.Strict))
}

func rhGetIndexI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if obj := regs[w.B()]; obj.IsObject() {
		o := obj.Object()
		if i := uint(w.C()); i < uint(len(o.elemsOf())) && o.flags&objMappedArguments == 0 {
			if v := o.elemsOf()[i]; !isHole(v) {
				regs[w.A()] = v
				return pc + 1
			}
		}
		if o.class == ClassTypedArray {
			if p := &regs[w.A()]; p.ref == nil {
				if n, ok := regTypedRead(st.r, o, float64(w.C())); ok {
					p.num = n
					return pc + 1
				}
			}
		}
	}
	return ^pc
}

func rsGetIndexI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	st.kept.pc = pc + 1
	if regElemMiss(st.c, &st.kept, regs) {
		return pc + 1
	}
	st.at(pc)
	v, err := st.r.getIndexed(regs[w.B()], Int32(int32(w.C())))
	return st.put(regs, w, v, err)
}

func rhSetIndexI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if obj := regs[w.A()]; obj.IsObject() {
		o := obj.Object()
		if i := uint(w.B()); i < uint(len(o.elemsOf())) && o.flags&(objMappedArguments|objSharedElems) == 0 && !isHole(o.elemsOf()[i]) {
			o.extOf().elems[i] = regs[w.C()]
			return pc + 1
		}
	}
	return ^pc
}

func rsSetIndexI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	st.kept.pc = pc + 1
	if regElemMiss(st.c, &st.kept, regs) {
		return pc + 1
	}
	st.at(pc)
	return st.done(st.r.setIndexed(regs[w.A()], Int32(int32(w.B())), regs[w.C()], st.cl.fn.Strict))
}

func rhAdd(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a, b := regs[w.B()], regs[w.C()]; a.IsNumber() && b.IsNumber() {
		if p := &regs[w.A()]; p.ref == nil {
			p.num = regNum(a.Number() + b.Number()).num
			return pc + 1
		}
	}
	return ^pc
}

// regBigMarks are the marks of an add, a sub or a mul whose BigInt result
// may let a block go back for the next result to be made in.
const regBigMarks = bytecode.RArithFreshB | bytecode.RArithFreshC | bytecode.RArithOwnA

// regBigGives reports whether regBigRelease may give anything back for
// v, an operator's BigInt result of a room the list keeps: a marked
// operand that is a BigInt on the heap, or a register A that holds its
// values alone. A result a sum of fresh numbers too small for the heap
// made, or of a local that does not hold its values alone, costs only
// this.
func regBigGives(regs *[1 << 16]Value, w bytecode.RInstr, v Value) bool {
	x := w.X()
	return bigGiveable((*BigInt)(v.ref)) && (x&bytecode.RArithOwnA != 0 ||
		x&bytecode.RArithFreshB != 0 && regs[w.B()].isHeapBig() || x&bytecode.RArithFreshC != 0 && regs[w.C()].isHeapBig())
}

// regBigRelease gives back what of an arithmetic operator's operands,
// in registers B and C, and of the value its register A held, its marks say nothing
// else has (regcomp's fresh and RArithOwnA), now that it has made v, a
// BigInt on the heap, as the tree's operator nodes give theirs back
// (bigRelease): `sum += a * a` and `a += 1n` in a loop make their results
// in the blocks of the ones before, which nothing else saw. It is
// bigRelease's work, with a block too small or too large for the list's
// rooms (bigGiveable) not given at all; and the operators call it only
// for a result of such a room, which the next result of its size is made
// in: a BigInt of a few words, made anew whatever is given back, pays
// only the operator's check of its room.
//
//go:noinline
func (r *Runtime) regBigRelease(regs *[1 << 16]Value, w bytecode.RInstr, v Value) {
	if bits.UintSize != 64 {
		return
	}
	// The operands are in their registers still, and A holds what it
	// held: the operator stores its result after this.
	a, b, x := regs[w.B()], regs[w.C()], w.X()
	if x&bytecode.RArithFreshB != 0 && a.isHeapBig() && a.ref != v.ref && bigGiveable((*BigInt)(a.ref)) {
		r.bigGive((*BigInt)(a.ref))
	}
	if x&bytecode.RArithFreshC != 0 && b.isHeapBig() && b.ref != v.ref && b.ref != a.ref && bigGiveable((*BigInt)(b.ref)) {
		r.bigGive((*BigInt)(b.ref))
	}
	if x&bytecode.RArithOwnA != 0 {
		old := regs[w.A()]
		if old.isHeapBig() && old.ref != v.ref && (*BigInt)(old.ref).hdr&bigSoleBit != 0 &&
			(x&bytecode.RArithFreshB == 0 || old.ref != a.ref) && (x&bytecode.RArithFreshC == 0 || old.ref != b.ref) &&
			bigGiveable((*BigInt)(old.ref)) {
			r.bigGive((*BigInt)(old.ref))
		}
		(*BigInt)(v.ref).hdr |= bigSoleBit
	}
}

func rsAdd(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a, b := regs[w.B()], regs[w.C()]; a.IsNumber() && b.IsNumber() {
		regs[w.A()] = regNum(a.Number() + b.Number())
		return pc + 1
	}
	st.at(pc)
	if w.X()&bytecode.RAddAppend != 0 {
		// An append to an upvalue: R[B] the string the binding alone
		// holds, grown in place where it can be, as the tree's upvalueAdd
		// grows it (strAppend).
		v, err := strAppendOp[struct{}](st.r, regs[w.B()], regs[w.C()])
		return st.put(regs, w, v, err)
	}
	v, err := st.r.add(regs[w.B()], regs[w.C()])
	if err == nil && v.isHeapBig() && w.X()&regBigMarks != 0 && regBigGives(regs, w, v) {
		st.r.regBigRelease(regs, w, v)
	}
	return st.put(regs, w, v, err)
}

func rhSub(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a, b := regs[w.B()], regs[w.C()]; a.IsNumber() && b.IsNumber() {
		if p := &regs[w.A()]; p.ref == nil {
			p.num = regNum(a.Number() - b.Number()).num
			return pc + 1
		}
	}
	return ^pc
}

func rsSub(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a, b := regs[w.B()], regs[w.C()]; a.IsNumber() && b.IsNumber() {
		regs[w.A()] = regNum(a.Number() - b.Number())
		return pc + 1
	}
	st.at(pc)
	v, err := st.r.arith(bytecode.OpSub, regs[w.B()], regs[w.C()])
	if err == nil && v.isHeapBig() && w.X()&regBigMarks != 0 && regBigGives(regs, w, v) {
		st.r.regBigRelease(regs, w, v)
	}
	return st.put(regs, w, v, err)
}

func rhMul(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a, b := regs[w.B()], regs[w.C()]; a.IsNumber() && b.IsNumber() {
		if p := &regs[w.A()]; p.ref == nil {
			p.num = regNum(a.Number() * b.Number()).num
			return pc + 1
		}
	}
	return ^pc
}

func rsMul(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a, b := regs[w.B()], regs[w.C()]; a.IsNumber() && b.IsNumber() {
		regs[w.A()] = regNum(a.Number() * b.Number())
		return pc + 1
	}
	st.at(pc)
	v, err := st.r.arith(bytecode.OpMul, regs[w.B()], regs[w.C()])
	if err == nil && v.isHeapBig() && w.X()&regBigMarks != 0 && regBigGives(regs, w, v) {
		st.r.regBigRelease(regs, w, v)
	}
	return st.put(regs, w, v, err)
}

func rhInc(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a := regs[w.B()]; a.IsNumber() {
		if p := &regs[w.A()]; p.ref == nil {
			p.num = regNum(a.Number() + 1).num
			return pc + 1
		}
	}
	return ^pc
}

func rsInc(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a := regs[w.B()]; a.IsNumber() {
		regs[w.A()] = regNum(a.Number() + 1)
		return pc + 1
	}
	st.at(pc)
	v, err := st.r.regStep(regs[w.B()], 1)
	return st.put(regs, w, v, err)
}

func rhDec(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a := regs[w.B()]; a.IsNumber() {
		if p := &regs[w.A()]; p.ref == nil {
			p.num = regNum(a.Number() - 1).num
			return pc + 1
		}
	}
	return ^pc
}

func rsDec(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a := regs[w.B()]; a.IsNumber() {
		regs[w.A()] = regNum(a.Number() - 1)
		return pc + 1
	}
	st.at(pc)
	v, err := st.r.regStep(regs[w.B()], -1)
	return st.put(regs, w, v, err)
}

func rhAddI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a := regs[w.B()]; a.IsNumber() {
		if p := &regs[w.A()]; p.ref == nil {
			p.num = regNum(a.Number() + float64(w.SC())).num
			return pc + 1
		}
	}
	return ^pc
}

func rsAddI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a := regs[w.B()]; a.IsNumber() {
		regs[w.A()] = regNum(a.Number() + float64(w.SC()))
		return pc + 1
	}
	st.at(pc)
	v, err := st.r.regBinImm(bytecode.OpAdd, regs[w.B()], w.SC())
	return st.put(regs, w, v, err)
}

func rhSubI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a := regs[w.B()]; a.IsNumber() {
		if p := &regs[w.A()]; p.ref == nil {
			p.num = regNum(a.Number() - float64(w.SC())).num
			return pc + 1
		}
	}
	return ^pc
}

func rsSubI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a := regs[w.B()]; a.IsNumber() {
		regs[w.A()] = regNum(a.Number() - float64(w.SC()))
		return pc + 1
	}
	st.at(pc)
	v, err := st.r.regBinImm(bytecode.OpSub, regs[w.B()], w.SC())
	return st.put(regs, w, v, err)
}

// The bitwise operators of an immediate, each a number's where the
// operand is an int32 exactly (regIsInt32).

func rhAndI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a := regs[w.B()]; a.IsNumber() && regIsInt32(a.num) {
		if p := &regs[w.A()]; p.ref == nil {
			p.num = float64(int32(a.num) & w.SC())
			return pc + 1
		}
	}
	return ^pc
}

func rhOrI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a := regs[w.B()]; a.IsNumber() && regIsInt32(a.num) {
		if p := &regs[w.A()]; p.ref == nil {
			p.num = float64(int32(a.num) | w.SC())
			return pc + 1
		}
	}
	return ^pc
}

func rhXorI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a := regs[w.B()]; a.IsNumber() && regIsInt32(a.num) {
		if p := &regs[w.A()]; p.ref == nil {
			p.num = float64(int32(a.num) ^ w.SC())
			return pc + 1
		}
	}
	return ^pc
}

func rhShlI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a := regs[w.B()]; a.IsNumber() && regIsInt32(a.num) {
		if p := &regs[w.A()]; p.ref == nil {
			p.num = float64(int32(a.num) << (uint32(w.SC()) & 31))
			return pc + 1
		}
	}
	return ^pc
}

func rhShrI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a := regs[w.B()]; a.IsNumber() && regIsInt32(a.num) {
		if p := &regs[w.A()]; p.ref == nil {
			p.num = float64(int32(a.num) >> (uint32(w.SC()) & 31))
			return pc + 1
		}
	}
	return ^pc
}

func rhUShrI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a := regs[w.B()]; a.IsNumber() && regIsInt32(a.num) {
		if p := &regs[w.A()]; p.ref == nil {
			p.num = float64(uint32(int32(a.num)) >> (uint32(w.SC()) & 31))
			return pc + 1
		}
	}
	return ^pc
}

// The bitwise operators' slow functions: an operand that is an int32,
// stored by a register that held a pointer, and the conversion otherwise.

func rsAndI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a := regs[w.B()]; a.IsNumber() && regIsInt32(a.num) {
		x := int32(a.num)
		regs[w.A()] = Int32(x & w.SC())
		return pc + 1
	}
	return rsBinImm(regs, w, pc, st)
}

func rsOrI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a := regs[w.B()]; a.IsNumber() && regIsInt32(a.num) {
		x := int32(a.num)
		regs[w.A()] = Int32(x | w.SC())
		return pc + 1
	}
	return rsBinImm(regs, w, pc, st)
}

func rsXorI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a := regs[w.B()]; a.IsNumber() && regIsInt32(a.num) {
		x := int32(a.num)
		regs[w.A()] = Int32(x ^ w.SC())
		return pc + 1
	}
	return rsBinImm(regs, w, pc, st)
}

func rsShlI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a := regs[w.B()]; a.IsNumber() && regIsInt32(a.num) {
		x := int32(a.num)
		regs[w.A()] = Int32(x << (uint32(w.SC()) & 31))
		return pc + 1
	}
	return rsBinImm(regs, w, pc, st)
}

func rsShrI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a := regs[w.B()]; a.IsNumber() && regIsInt32(a.num) {
		x := int32(a.num)
		regs[w.A()] = Int32(x >> (uint32(w.SC()) & 31))
		return pc + 1
	}
	return rsBinImm(regs, w, pc, st)
}

func rsUShrI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a := regs[w.B()]; a.IsNumber() && regIsInt32(a.num) {
		x := int32(a.num)
		regs[w.A()] = Uint32(uint32(x) >> (uint32(w.SC()) & 31))
		return pc + 1
	}
	return rsBinImm(regs, w, pc, st)
}

func rsBitAnd(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a, b := regs[w.B()], regs[w.C()]; a.IsNumber() && regIsInt32(a.num) && b.IsNumber() && regIsInt32(b.num) {
		x, y := int32(a.num), int32(b.num)
		regs[w.A()] = Int32(x & y)
		return pc + 1
	}
	return rsBitwise(regs, w, pc, st)
}

func rsBitOr(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a, b := regs[w.B()], regs[w.C()]; a.IsNumber() && regIsInt32(a.num) && b.IsNumber() && regIsInt32(b.num) {
		x, y := int32(a.num), int32(b.num)
		regs[w.A()] = Int32(x | y)
		return pc + 1
	}
	return rsBitwise(regs, w, pc, st)
}

func rsBitXor(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a, b := regs[w.B()], regs[w.C()]; a.IsNumber() && regIsInt32(a.num) && b.IsNumber() && regIsInt32(b.num) {
		x, y := int32(a.num), int32(b.num)
		regs[w.A()] = Int32(x ^ y)
		return pc + 1
	}
	return rsBitwise(regs, w, pc, st)
}

func rsShl(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a, b := regs[w.B()], regs[w.C()]; a.IsNumber() && regIsInt32(a.num) && b.IsNumber() && regIsInt32(b.num) {
		x, y := int32(a.num), int32(b.num)
		regs[w.A()] = Int32(x << (uint32(y) & 31))
		return pc + 1
	}
	return rsBitwise(regs, w, pc, st)
}

func rsShr(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a, b := regs[w.B()], regs[w.C()]; a.IsNumber() && regIsInt32(a.num) && b.IsNumber() && regIsInt32(b.num) {
		x, y := int32(a.num), int32(b.num)
		regs[w.A()] = Int32(x >> (uint32(y) & 31))
		return pc + 1
	}
	return rsBitwise(regs, w, pc, st)
}

func rsUShr(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a, b := regs[w.B()], regs[w.C()]; a.IsNumber() && regIsInt32(a.num) && b.IsNumber() && regIsInt32(b.num) {
		x, y := int32(a.num), int32(b.num)
		regs[w.A()] = Uint32(uint32(x) >> (uint32(y) & 31))
		return pc + 1
	}
	return rsBitwise(regs, w, pc, st)
}

func rsBinImm(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	st.at(pc)
	v, err := st.r.regBinImm(regStackOp[w.Op()], regs[w.B()], w.SC())
	return st.put(regs, w, v, err)
}

// The bitwise operators of two registers, each a number's where both
// operands are int32s exactly.

func rhBitAnd(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a, b := regs[w.B()], regs[w.C()]; a.IsNumber() && regIsInt32(a.num) && b.IsNumber() && regIsInt32(b.num) {
		if p := &regs[w.A()]; p.ref == nil {
			p.num = float64(int32(a.num) & int32(b.num))
			return pc + 1
		}
	}
	return ^pc
}

func rhBitOr(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a, b := regs[w.B()], regs[w.C()]; a.IsNumber() && regIsInt32(a.num) && b.IsNumber() && regIsInt32(b.num) {
		if p := &regs[w.A()]; p.ref == nil {
			p.num = float64(int32(a.num) | int32(b.num))
			return pc + 1
		}
	}
	return ^pc
}

func rhBitXor(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a, b := regs[w.B()], regs[w.C()]; a.IsNumber() && regIsInt32(a.num) && b.IsNumber() && regIsInt32(b.num) {
		if p := &regs[w.A()]; p.ref == nil {
			p.num = float64(int32(a.num) ^ int32(b.num))
			return pc + 1
		}
	}
	return ^pc
}

func rhShl(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a, b := regs[w.B()], regs[w.C()]; a.IsNumber() && regIsInt32(a.num) && b.IsNumber() && regIsInt32(b.num) {
		if p := &regs[w.A()]; p.ref == nil {
			p.num = float64(int32(a.num) << (uint32(int32(b.num)) & 31))
			return pc + 1
		}
	}
	return ^pc
}

func rhShr(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a, b := regs[w.B()], regs[w.C()]; a.IsNumber() && regIsInt32(a.num) && b.IsNumber() && regIsInt32(b.num) {
		if p := &regs[w.A()]; p.ref == nil {
			p.num = float64(int32(a.num) >> (uint32(int32(b.num)) & 31))
			return pc + 1
		}
	}
	return ^pc
}

func rhUShr(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a, b := regs[w.B()], regs[w.C()]; a.IsNumber() && regIsInt32(a.num) && b.IsNumber() && regIsInt32(b.num) {
		if p := &regs[w.A()]; p.ref == nil {
			p.num = float64(uint32(int32(a.num)) >> (uint32(int32(b.num)) & 31))
			return pc + 1
		}
	}
	return ^pc
}

func rsBitwise(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	st.at(pc)
	v, err := st.r.regBinary(regStackOp[w.Op()], regs[w.B()], regs[w.C()])
	return st.put(regs, w, v, err)
}

// rhPostInc and rhPostDec are post_inc and post_dec: R[A] = R[B], a
// number, and R[B] stepped, both stored by their num words.
func rhPostInc(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a := regs[w.B()]; a.IsNumber() {
		if p := &regs[w.A()]; p.ref == nil {
			p.num = a.num
			regs[w.B()].num = regNum(a.Number() + 1).num
			return pc + 1
		}
	}
	return ^pc
}

func rhPostDec(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a := regs[w.B()]; a.IsNumber() {
		if p := &regs[w.A()]; p.ref == nil {
			p.num = a.num
			regs[w.B()].num = regNum(a.Number() - 1).num
			return pc + 1
		}
	}
	return ^pc
}

func rsPostStep(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a := regs[w.B()]; a.IsNumber() {
		d := 1.0
		if w.Op() == bytecode.RPostDec {
			d = -1
		}
		regs[w.A()] = a
		regs[w.B()] = regNum(a.Number() + d)
		return pc + 1
	}
	r := st.r
	st.at(pc)
	old, err := r.toNumeric(regs[w.B()])
	if err != nil {
		return st.fail(err)
	}
	delta := 1.0
	if w.Op() == bytecode.RPostDec {
		delta = -1
	}
	next, err := r.regStep(old, delta)
	if err != nil {
		return st.fail(err)
	}
	regs[w.A()] = old
	regs[w.B()] = next
	return pc + 1
}

// rhGetLength is get_length: an array's length, Object.arrayLength
// written out, and a string's.
func rhGetLength(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if p := &regs[w.A()]; p.ref == nil {
		if o := regs[w.B()]; o.IsObject() && o.object().class == ClassArray {
			obj := o.object()
			n := uint32(len(obj.elemsOf()))
			if obj.flags&objHasSparseElements != 0 && obj.arrayExt().arrayLen > n {
				n = obj.arrayExt().arrayLen
			}
			p.num = float64(n)
			return pc + 1
		} else if regTagged(math.Float64bits(o.num), KindString) {
			p.num = float64((*String)(o.ref).Len())
			return pc + 1
		}
	}
	return ^pc
}

func rsGetLength(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if o := regs[w.B()]; o.IsObject() && o.object().class == ClassArray {
		obj := o.object()
		n := uint32(len(obj.elemsOf()))
		if obj.flags&objHasSparseElements != 0 && obj.arrayExt().arrayLen > n {
			n = obj.arrayExt().arrayLen
		}
		regs[w.A()] = Uint32(n)
		return pc + 1
	}
	r := st.r
	st.at(pc)
	if o := regs[w.B()]; o.IsString() {
		regs[w.A()] = Int(o.String().Len())
		return pc + 1
	}
	v, err := r.getValueProp(regs[w.B()], atomLength)
	return st.put(regs, w, v, err)
}

// rhAddProp is add_prop, o.a += j: an own writable data property the
// site's entry for the store says where to find, a number added in place.
func rhAddProp(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if o := regs[w.A()]; o.IsObject() {
		obj := o.Object()
		if s := &st.cl.ic[w.C()]; obj.shape == s.ownShape {
			p := &obj.vals()[s.idx]
			if b := regs[w.B()]; p.IsNumber() && b.IsNumber() {
				p.num = regNum(p.Number() + b.Number()).num
				return pc + 1
			}
		}
	}
	return ^pc
}

// rhAddPropI is add_prop_imm, o.n++.
func rhAddPropI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if o := regs[w.A()]; o.IsObject() {
		obj := o.Object()
		if s := &st.cl.ic[w.C()]; obj.shape == s.ownShape {
			p := &obj.vals()[s.idx]
			if p.IsNumber() {
				p.num = regNum(p.Number() + float64(w.SB())).num
				return pc + 1
			}
		}
	}
	return ^pc
}

func rsAddProp(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	st.at(pc)
	b := Int32(w.SB())
	if w.Op() == bytecode.RAddProp {
		b = regs[w.B()]
	}
	return st.done(st.r.regAddProp(st.cl, st.c, w, regs[w.A()], b))
}

func rhAddIndexK(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if o, b := regs[w.A()], regs[w.B()]; o.IsObject() && b.IsNumber() {
		obj := o.Object()
		if i, es := uint(w.C()), obj.elemsOf(); i < uint(len(es)) && obj.flags&(objMappedArguments|objSharedElems) == 0 {
			if p := &es[i]; p.IsNumber() {
				p.num = regNum(p.Number() + b.Number()).num
				return pc + 1
			}
		}
	}
	return ^pc
}

func rsAddIndexK(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	f := st.at(pc)
	c := st.c
	return st.done(st.r.regAddIndex(f, c.rc.Aux[pc], regs[w.A()], Int32(int32(w.C())), regs[w.B()], w.X() != 0))
}

func rsAddIndex(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	f := st.at(pc)
	c := st.c
	return st.done(st.r.regAddIndex(f, c.rc.Aux[pc], regs[w.A()], regs[w.B()], regs[w.C()], false))
}

func rsAddIndexI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	f := st.at(pc)
	c := st.c
	return st.done(st.r.regAddIndex(f, c.rc.Aux[pc], regs[w.A()], regs[w.B()], Int32(w.SC()), w.X() != 0))
}

// rhStrPart is str_part: a string or a number is joined as it is.
func rhStrPart(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if v := regs[w.A()]; regTagged(math.Float64bits(v.num), KindString) || v.IsNumber() {
		return pc + 1
	}
	return ^pc
}

func rsStrPart(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	st.at(pc)
	v, err := st.r.add(Str(emptyString), regs[w.A()])
	return st.put(regs, w, v, err)
}

// rhKeyOfBase is key_of_base: a key whose conversion runs nothing is left
// as it is, and a null or undefined object is the read's to report.
func rhKeyOfBase(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if v := regs[w.C()]; regTagged(math.Float64bits(v.num), KindString) || v.IsNumber() || regTagged(math.Float64bits(v.num), KindSymbol) {
		regs[w.A()] = v
		return pc + 1
	}
	return ^pc
}

func rsKeyOfBase(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	r := st.r
	st.at(pc)
	if base := regs[w.B()]; base.IsNullish() {
		return st.fail(r.throwTypeError("cannot read property of %s", r.describe(base)))
	}
	k, err := r.toPropertyKey(regs[w.C()])
	if err != nil {
		return st.fail(err)
	}
	// An object's Symbol.toPrimitive may give a symbol: the key keeps
	// whichever kind it turned out to be.
	regs[w.A()] = r.keyToValue(k)
	return pc + 1
}

// The jumps. Each that may go back counts the interrupt budget
// (regJump), a jump of no fast path too (rsJump).

func rhJmp(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return regJump(st, pc, int32(w.BC()))
}

func rsJmp(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return regJumpSlow(st, pc, int32(w.BC()))
}

// rsJump is the slow function of a compare-and-branch whose fast path
// left only its jump, the budget spent: jsne_imm, jseq_imm.
func rsJump(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return regJumpSlow(st, pc, w.SC())
}

// regTruth is ToBoolean of what needs no call -- a boolean, an object, a
// number -- and false for anything else, which the slow function answers.
func regTruth(v Value) (t, ok bool) {
	switch b := math.Float64bits(v.num); {
	case b == trueBits:
		return true, true
	case b == falseBits:
		return false, true
	case b == objectBits:
		return (*Object)(v.ref).flags&objHTMLDDA == 0, true
	case v.IsNumber():
		return v.num != 0 && v.num == v.num, true
	}
	return false, false
}

func rhJmpF(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if t, ok := regTruth(regs[w.A()]); ok {
		if !t {
			return regJump(st, pc, int32(w.BC()))
		}
		return pc + 1
	}
	return ^pc
}

func rhJmpT(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if t, ok := regTruth(regs[w.A()]); ok {
		if t {
			return regJump(st, pc, int32(w.BC()))
		}
		return pc + 1
	}
	return ^pc
}

// rsTruth is jmp_false's and jmp_true's slow function: a truthiness
// regTruth does not answer, or a jump back the budget has spent.
func rsTruth(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if regs[w.A()].Truthy() == (w.Op() == bytecode.RJmpT) {
		return regJumpSlow(st, pc, int32(w.BC()))
	}
	return pc + 1
}

// The compare-and-branches of two numbers. What is not two numbers is
// rsCompare's, by regJumpCmp's comparison.

func rhJLt(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a, b := regs[w.A()], regs[w.B()]; a.IsNumber() && b.IsNumber() {
		if a.Number() < b.Number() {
			return regJump(st, pc, w.SC())
		}
		return pc + 1
	}
	return ^pc
}

func rhJNLt(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a, b := regs[w.A()], regs[w.B()]; a.IsNumber() && b.IsNumber() {
		if !(a.Number() < b.Number()) {
			return regJump(st, pc, w.SC())
		}
		return pc + 1
	}
	return ^pc
}

func rhJLe(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a, b := regs[w.A()], regs[w.B()]; a.IsNumber() && b.IsNumber() {
		if a.Number() <= b.Number() {
			return regJump(st, pc, w.SC())
		}
		return pc + 1
	}
	return ^pc
}

func rhJNLe(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a, b := regs[w.A()], regs[w.B()]; a.IsNumber() && b.IsNumber() {
		if !(a.Number() <= b.Number()) {
			return regJump(st, pc, w.SC())
		}
		return pc + 1
	}
	return ^pc
}

func rhJGt(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a, b := regs[w.A()], regs[w.B()]; a.IsNumber() && b.IsNumber() {
		if a.Number() > b.Number() {
			return regJump(st, pc, w.SC())
		}
		return pc + 1
	}
	return ^pc
}

func rhJNGt(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a, b := regs[w.A()], regs[w.B()]; a.IsNumber() && b.IsNumber() {
		if !(a.Number() > b.Number()) {
			return regJump(st, pc, w.SC())
		}
		return pc + 1
	}
	return ^pc
}

func rhJGe(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a, b := regs[w.A()], regs[w.B()]; a.IsNumber() && b.IsNumber() {
		if a.Number() >= b.Number() {
			return regJump(st, pc, w.SC())
		}
		return pc + 1
	}
	return ^pc
}

func rhJNGe(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a, b := regs[w.A()], regs[w.B()]; a.IsNumber() && b.IsNumber() {
		if !(a.Number() >= b.Number()) {
			return regJump(st, pc, w.SC())
		}
		return pc + 1
	}
	return ^pc
}

func rhJEq(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a, b := regs[w.A()], regs[w.B()]; a.IsNumber() && b.IsNumber() {
		if a.Number() == b.Number() {
			return regJump(st, pc, w.SC())
		}
		return pc + 1
	}
	return ^pc
}

func rhJNe(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a, b := regs[w.A()], regs[w.B()]; a.IsNumber() && b.IsNumber() {
		if a.Number() != b.Number() {
			return regJump(st, pc, w.SC())
		}
		return pc + 1
	}
	return ^pc
}

// The compare-and-branches of a number and the immediate SB.

func rhJLtI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a := regs[w.A()]; a.IsNumber() {
		if a.Number() < float64(w.SB()) {
			return regJump(st, pc, w.SC())
		}
		return pc + 1
	}
	return ^pc
}

func rhJNLtI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a := regs[w.A()]; a.IsNumber() {
		if !(a.Number() < float64(w.SB())) {
			return regJump(st, pc, w.SC())
		}
		return pc + 1
	}
	return ^pc
}

func rhJLeI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a := regs[w.A()]; a.IsNumber() {
		if a.Number() <= float64(w.SB()) {
			return regJump(st, pc, w.SC())
		}
		return pc + 1
	}
	return ^pc
}

func rhJNLeI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a := regs[w.A()]; a.IsNumber() {
		if !(a.Number() <= float64(w.SB())) {
			return regJump(st, pc, w.SC())
		}
		return pc + 1
	}
	return ^pc
}

func rhJGtI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a := regs[w.A()]; a.IsNumber() {
		if a.Number() > float64(w.SB()) {
			return regJump(st, pc, w.SC())
		}
		return pc + 1
	}
	return ^pc
}

func rhJNGtI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a := regs[w.A()]; a.IsNumber() {
		if !(a.Number() > float64(w.SB())) {
			return regJump(st, pc, w.SC())
		}
		return pc + 1
	}
	return ^pc
}

func rhJGeI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a := regs[w.A()]; a.IsNumber() {
		if a.Number() >= float64(w.SB()) {
			return regJump(st, pc, w.SC())
		}
		return pc + 1
	}
	return ^pc
}

func rhJNGeI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a := regs[w.A()]; a.IsNumber() {
		if !(a.Number() >= float64(w.SB())) {
			return regJump(st, pc, w.SC())
		}
		return pc + 1
	}
	return ^pc
}

func rhJEqI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a := regs[w.A()]; a.IsNumber() {
		if a.Number() == float64(w.SB()) {
			return regJump(st, pc, w.SC())
		}
		return pc + 1
	}
	return ^pc
}

func rhJNeI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a := regs[w.A()]; a.IsNumber() {
		if a.Number() != float64(w.SB()) {
			return regJump(st, pc, w.SC())
		}
		return pc + 1
	}
	return ^pc
}

// The compare-and-branches of a number and an upvalue B: a loop's bound
// that a closure captures.

func rhJLtU(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a, b := regs[w.A()], st.cl.upvalues[w.B()].get(); a.IsNumber() && b.IsNumber() {
		if a.Number() < b.Number() {
			return regJump(st, pc, w.SC())
		}
		return pc + 1
	}
	return ^pc
}

func rhJNLtU(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a, b := regs[w.A()], st.cl.upvalues[w.B()].get(); a.IsNumber() && b.IsNumber() {
		if !(a.Number() < b.Number()) {
			return regJump(st, pc, w.SC())
		}
		return pc + 1
	}
	return ^pc
}

func rhJLeU(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a, b := regs[w.A()], st.cl.upvalues[w.B()].get(); a.IsNumber() && b.IsNumber() {
		if a.Number() <= b.Number() {
			return regJump(st, pc, w.SC())
		}
		return pc + 1
	}
	return ^pc
}

func rhJNLeU(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a, b := regs[w.A()], st.cl.upvalues[w.B()].get(); a.IsNumber() && b.IsNumber() {
		if !(a.Number() <= b.Number()) {
			return regJump(st, pc, w.SC())
		}
		return pc + 1
	}
	return ^pc
}

// rhJNull and rhJNotNull are == null and != null: an object other than
// document.all's stand-in is not == null; Value.IsNullish, written out.
func rhJNull(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a := regs[w.A()]; !a.IsObject() || a.object().flags&objHTMLDDA == 0 {
		b := math.Float64bits(a.num)
		k := Kind(b & 0xFF)
		if b&tagMask == tagBase && (k == KindNull || k == KindUndefined) {
			return regJump(st, pc, w.SC())
		}
		return pc + 1
	}
	return ^pc
}

func rhJNotNull(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a := regs[w.A()]; !a.IsObject() || a.object().flags&objHTMLDDA == 0 {
		b := math.Float64bits(a.num)
		k := Kind(b & 0xFF)
		if !(b&tagMask == tagBase && (k == KindNull || k == KindUndefined)) {
			return regJump(st, pc, w.SC())
		}
		return pc + 1
	}
	return ^pc
}

// rsCompare is a compare-and-branch's slow function, by regJumpCmp's
// comparison: anything but two numbers, or a jump back the budget has
// spent.
func rsCompare(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	r := st.r
	st.at(pc)
	k := regJumpCmp[w.Op()]
	b := Int32(w.SB())
	if k.reg {
		b = regs[w.B()]
	} else if w.Op() == bytecode.RJNull || w.Op() == bytecode.RJNotNull {
		b = Null
	} else if k.upval {
		b = st.cl.upvalues[w.B()].get()
	}
	res, err := r.regCompare(k.op, regs[w.A()], b)
	if err != nil {
		return st.fail(err)
	}
	if res != k.not {
		return regJumpSlow(st, pc, w.SC())
	}
	return pc + 1
}

// rhJSEq and rhJSNe are === and !==: two numbers, or two values one of
// which is undefined, null, a boolean, a symbol or an object, equal only
// if their words are; two strings or BigInts are rsStrictEq's.
func rhJSEq(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	a, b := regs[w.A()], regs[w.B()]
	var eq bool
	if a.IsNumber() && b.IsNumber() {
		eq = a.num == b.num
	} else if x, y := math.Float64bits(a.num), math.Float64bits(b.num); regWordKind(x) || regWordKind(y) {
		eq = x == y && a.ref == b.ref
	} else {
		return ^pc
	}
	if eq {
		return regJump(st, pc, w.SC())
	}
	return pc + 1
}

func rhJSNe(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	a, b := regs[w.A()], regs[w.B()]
	var eq bool
	if a.IsNumber() && b.IsNumber() {
		eq = a.num == b.num
	} else if x, y := math.Float64bits(a.num), math.Float64bits(b.num); regWordKind(x) || regWordKind(y) {
		eq = x == y && a.ref == b.ref
	} else {
		return ^pc
	}
	if !eq {
		return regJump(st, pc, w.SC())
	}
	return pc + 1
}

func rsStrictEq(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if regs[w.A()].StrictEquals(regs[w.B()]) == (w.Op() == bytecode.RJSEq) {
		return regJumpSlow(st, pc, w.SC())
	}
	return pc + 1
}

// rhJSEqI is === of the immediate SB: only a number is === a number.
func rhJSEqI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a := regs[w.A()]; a.IsNumber() && a.Number() == float64(w.SB()) {
		return regJump(st, pc, w.SC())
	}
	return pc + 1
}

func rhJSNeI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a := regs[w.A()]; !a.IsNumber() || a.Number() != float64(w.SB()) {
		return regJump(st, pc, w.SC())
	}
	return pc + 1
}

// rhJTypeof is jtypeof: an object's type asked inline where it is a
// function's or an ordinary object's; a proxy and document.all's kind are
// the slow function's.
func rhJTypeof(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	v := regs[w.A()]
	// typeof's answer of v's kind (Value.Kind, regTypeOf), written out.
	var k Kind
	switch bits := math.Float64bits(v.num); {
	case bits>>51 != tagBase>>51:
		k = KindNumber
	case bits >= heapBigBits:
		k = KindBigInt
	default:
		k = Kind(bits & 0xFF)
	}
	t := uint8(typeUndefined)
	switch k {
	case KindNumber:
		t = typeNumber
	case KindNull, KindObject:
		t = typeObject
	case KindBool:
		t = typeBoolean
	case KindString:
		t = typeString
	case KindSymbol:
		t = typeSymbol
	case KindBigInt:
		t = typeBigInt
	}
	if v.IsObject() {
		o := v.object()
		d := o.dataOf()
		if _, isProxy := d.(*proxyData); isProxy || o.flags&objHTMLDDA != 0 {
			return ^pc
		}
		if _, isFn := d.(*funcData); isFn {
			t = typeFunction
		}
	}
	if (t == w.X()&^bytecode.RCmpTrue) == (w.X()&bytecode.RCmpTrue != 0) {
		return regJump(st, pc, w.SC())
	}
	return pc + 1
}

func rsJTypeof(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	t := typeofKind[struct{}](regs[w.A()])
	if (t == int(w.X()&^bytecode.RCmpTrue)) == (w.X()&bytecode.RCmpTrue != 0) {
		return regJumpSlow(st, pc, w.SC())
	}
	return pc + 1
}

// The operations of no fast path, each its handler and the whole of it.

// rhRet and rhRetUndef end the frame: runReg reads what ret returns from
// the instruction it ended at.
func rhRet(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return regDone
}

func rhRetUndef(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return regDone
}

// rhCallWT is tail_call_w and tail_call_method_w: once the stack is deep,
// the frame given to the callee in place or given up (regTailCallW), and
// call_w's call otherwise.
func rhCallWT(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if r := st.r; r.frameDepth >= tailCallDepth {
		st.kept.pc = pc + 1
		if err, ok := r.regTailCallW(st.f, st.c, pc, w, st.wb); ok {
			return st.fail(err)
		}
	}
	return rhCallW(regs, w, pc, st)
}

func rhThrow(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	st.kept.pc = pc + 1
	return st.fail(st.r.regThrow(st.f, st.c, pc+1, regs))
}

// rhYield is a generator's step, suspended with what it yields: the
// frame's suspension, which no handler of its own takes (regCatch), as no
// other error leaves without the fail tail.
func rhYield(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	st.kept.pc = pc + 1
	return st.fail(st.r.regYield(st.f, regs[w.B()], pc+1))
}

func rhIterNext(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	st.kept.pc = pc + 1
	return st.done(st.r.regIterNext(st.f, st.c, &st.kept, regs))
}

func rhLit(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	st.kept.pc = pc + 1
	return st.done(st.r.regLit(st.f, st.c, &st.kept, regs))
}

func rhAppend(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	st.kept.pc = pc + 1
	return st.done(st.r.regAppend(st.f, st.c, &st.kept, regs))
}

func rhCharCodeAt(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	st.kept.pc = pc + 1
	return st.done(st.r.regCharCodeAt(st.f, st.c, &st.kept, regs))
}

func rhGetPrivate(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	st.at(pc)
	return st.done(st.r.regGetPrivate(st.f, st.c, &st.kept, regs))
}

func rhSetPrivate(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	st.at(pc)
	return st.done(st.r.regSetPrivate(st.f, st.c, &st.kept, regs))
}

// rhStrFreeze is str_freeze: a read of a binding an append grows, which
// what read it may keep: only a string an append owns is written.
func rhStrFreeze(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if v := regs[w.A()]; regTagged(math.Float64bits(v.num), KindString) {
		if s := (*String)(v.ref); s.building {
			s.building = false
		}
	}
	return pc + 1
}

func rhNewArrayE(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	st.kept.pc = pc + 1
	st.r.regNewArrayE(st.f, st.c, &st.kept, regs)
	return pc + 1
}

func rhNewObjectLit(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	st.kept.pc = pc + 1
	st.r.regNewObjectLit(st.f, st.c, &st.kept, regs)
	return pc + 1
}

// rhSwitch is switch: the body of the first case R[A] strictly equals, or
// the next instruction.
func rhSwitch(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if to := st.c.switches[w.BC()].lookup(regs[w.A()], st.cl.consts); to >= 0 {
		return int(to)
	}
	return pc + 1
}

// rhSwitchTypeof is switch_typeof: the body of R[A]'s type's name, where a
// case has it.
func rhSwitchTypeof(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if to := st.c.switches[w.BC()].typeTo[typeofKind[struct{}](regs[w.A()])]; to >= 0 {
		return int(to)
	}
	return pc + 1
}

// rhTypeof is typeof: the type's name the runtime keeps, as typeofString
// answers it.
func rhTypeof(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	regs[w.A()] = Str(st.r.typeofString(regs[w.B()]))
	return pc + 1
}

func rsDefineField(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	st.at(pc)
	cl := st.cl
	return st.done(st.r.regDefineField(cl, regName(cl, st.c, pc, w.C()), regs[w.A()], regs[w.B()]))
}

func rsNewObject(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	st.at(pc)
	regs[w.A()] = Obj(st.r.regNewObject(st.cl, w))
	return pc + 1
}

func rsNewArray(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	r := st.r
	st.at(pc)
	b := st.wb + int(w.B())
	regs[w.A()] = Obj(r.newArrayFrom(r.stack[b : b+int(w.C())]))
	return pc + 1
}

func rsBin(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	st.at(pc)
	v, err := st.r.regBinary(bytecode.Op(w.X()), regs[w.B()], regs[w.C()])
	return st.put(regs, w, v, err)
}

func rsUnary(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	st.at(pc)
	v, err := st.r.regUnary(bytecode.Op(w.X()), regs[w.B()])
	return st.put(regs, w, v, err)
}

func rsNewW(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if regReportOn {
		regCrossOf(regCrossNew, regs[w.A()])
	}
	st.at(pc)
	v, err := st.r.regConstruct(regs[w.A()], st.wb+int(w.A()), int(w.X()))
	if err != nil {
		return st.fail(err)
	}
	regs[w.B()] = v
	return pc + 1
}

func rsClosureW(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	f := st.at(pc)
	regs[w.A()] = Obj(st.r.regClosure(f, st.c, w, st.wb))
	return pc + 1
}

func rsInstanceOf(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	st.at(pc)
	ok, err := st.r.instanceOfAt(&st.cl.ic[st.c.rc.Aux[pc]], regs[w.B()], regs[w.C()])
	return st.put(regs, w, Bool(ok), err)
}

func rsIn(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	st.at(pc)
	v, err := st.r.regIn(regs[w.B()], regs[w.C()])
	return st.put(regs, w, v, err)
}

func rsDelete(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	st.at(pc)
	v, err := st.r.regDelete(st.cl, regs[w.B()], regs[w.C()])
	return st.put(regs, w, v, err)
}

func rsArguments(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	f := st.at(pc)
	regs[w.A()] = Obj(st.r.newArgumentsObject(f))
	return pc + 1
}

// rsNewArrayK is new_array_k: the tree's shared literal
// (ztree_a0shared.go), the site keeping the array of the constants, and
// each array sharing its elements until one is written.
func rsNewArrayK(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	r, c, cl := st.r, st.c, st.cl
	st.at(pc)
	s := &cl.ic[w.C()]
	t := s.p1
	if t == nil {
		lit := c.rc.Literals[c.rc.Aux[pc]]
		vals := make([]Value, len(lit))
		for i, e := range lit {
			switch e.Kind {
			case bytecode.LitConst:
				vals[i] = cl.consts[e.K]
			case bytecode.LitTrue:
				vals[i] = True
			case bytecode.LitFalse:
				vals[i] = False
			case bytecode.LitNull:
				vals[i] = Null
			default:
				vals[i] = Undefined
			}
		}
		t = sharedMade(r, vals)
		s.p1 = t
	}
	regs[w.A()] = Obj(sharedShare(r, s, t))
	return pc + 1
}

func rsApplyArgs(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	f := st.at(pc)
	b := int(w.B())
	v, err := st.r.regApplyArgs(f, regs[b], regs[b+1], regs[b+2], &regs[w.C()])
	return st.put(regs, w, v, err)
}

func rsArgsLen(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	f := st.at(pc)
	if a := regs[w.B()]; a.IsObject() {
		v, err := st.r.getValueProp(a, atomLength)
		return st.put(regs, w, v, err)
	}
	regs[w.A()] = Int(len(f.args))
	return pc + 1
}

func rsArgsIndex(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	r := st.r
	f := st.at(pc)
	key := regs[w.C()]
	if !regs[w.B()].IsObject() {
		if i := uint32(key.num); key.IsNumber() && float64(i) == key.num && uint(i) < uint(len(f.args)) &&
			!(f.cl.fn.MappedArguments && uint(i) < uint(f.cl.fn.ParamCount)) {
			regs[w.A()] = f.args[i]
			return pc + 1
		}
		regs[w.B()] = Obj(r.newArgumentsObject(f))
	}
	v, err := r.getIndexed(regs[w.B()], key)
	return st.put(regs, w, v, err)
}

// rsConcat is concat: the registers are the parts' own, which concatParts
// may convert in place, as nothing reads them after.
func rsConcat(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	st.at(pc)
	b := int(w.B())
	v, err := concatParts(st.r, regs[b:b+int(w.C())])
	return st.put(regs, w, v, err)
}
