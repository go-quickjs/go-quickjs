package vm

import (
	"math"
	"sync/atomic"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// The register interpreter (docs/register-interpreter.md), stage 1: a
// function the register form covers runs as its register code, every other
// as before. QJS_REG in the environment turns it on.
//
// A frame's registers are a window of the runtime's value stack laid out as
// the stack form's frame is -- the locals, then the temporaries -- so that a
// closure captures a local, a stack trace reads a pc and the frame is pushed
// and popped as ever. The loop indexes the window with 16-bit register
// numbers through a pointer to an array of 65,536 values, which needs no
// bounds check: the register backend's verifier (regcomp.Verify)
// proves every register an instruction names is below the frame's count,
// and a frame is given that many slots before it runs. The code is read the
// same way, every jump proven to land in it.
//
// The files sort after every other of the package's, so that the functions
// they add come after every function there was: the interpreter's hot
// functions keep their addresses, and only the closures move.

// regTier turns the register interpreter on for functions not yet run.
var regTier atomic.Bool

// regsBuilt counts the functions given register code.
var regsBuilt atomic.Int64

// regCode is a function's register code as the VM runs it.
type regCode struct {
	// code is the first instruction, read with no bounds check.
	code unsafe.Pointer
	src  []uint32
	// siteName is each property site's name, by its cache's number.
	siteName []uint32
	// numRegs is how many registers a frame uses; locals and params are
	// the function's counts.
	numRegs, locals, params int32
	coerceThis              bool
	// wide marks code of the register form's own frame layout: register 0
	// the function, 1 its this, the parameters from 2, and inits bindings
	// after them that a call makes undefined. Only call_w enters it.
	wide bool
	// keep is the first parameter whose argument f.arguments reads as
	// the call passed it, which the call keeps if the frame may overwrite
	// it (regKeepWindow): params, but in a sloppy function whose
	// parameter list is not plain (0) or one of whose parameters a
	// closure captures (the first such). It fills the padding after wide.
	keep  uint16
	inits int32
	// ncap is how many of the function's stack slots a closure captures,
	// the cells a closure it makes is given (regClosure).
	ncap int32
	// newTarget says the function reads new.target, which its frame keeps.
	newTarget bool
	// args says a frame keeps its arguments as passed, for RArguments.
	args bool
	// suspends marks a generator's or an async function's code, which
	// only a generator's resumption runs (zzreg_zgen.go).
	suspends bool
	// cellThis marks a derived constructor whose this binding is the
	// runtime's cell for its frame's depth (thisCells): its body makes no
	// closure, no direct eval and no debugger's stop, the things that can
	// keep a binding past its frame, as planTreeNew finds for a tree.
	cellThis bool
	// leaf is what a call may answer without a frame: a pure body that
	// calls nothing, or one that only reads or writes its this's
	// properties; read here rather than in the function, a line away.
	leaf uint8
	// leafArg is the parameter a regLeafArg body returns, and leafVal the
	// value a regLeafConst body does.
	leafArg int32
	leafVal Value
	// leafOps is a regLeafCode body's code as regLeafRun runs it.
	leafOps []regLeafOp
	// switches is each switch instruction's table, made from the code's.
	switches []*regSwitchTable
	rc       *bytecode.RegCode
	fn       *bytecode.Function
	// reuse is the sites of the code's calls of exec marked RMarkReuse
	// whose arrays may be made again, by call site (regReuseSites).
	reuse []*execReuseSite
	// genFrom is the first register a generator's or an async function's
	// vals keep (newRegGenerator): 2 where nothing captures the callee's
	// register or this's and nothing reads a local by its register
	// (LocalReg), which a resumption makes again from the generator; 0
	// otherwise.
	genFrom int32
}

// runReg runs frame f as register code c, from instruction start, and
// returns what it returns. Only a generator's resumption starts past the
// first instruction (regGenRun).
//
// A case that finishes its instruction continues the loop; one that can
// fail sets the frame's pc to the stack form's instruction first, which is
// what a stack trace or an error's position reads, keeps the pc, and goes
// to the fail tail with the error, where a handler of the frame's may
// catch it (regCatch).
func (r *Runtime) runReg(f *frame, c *regCode, start int) (Value, error) {
	if r.stopped != nil {
		return Undefined, r.stopped
	}
	cl := f.cl
	wb := f.base - int(c.locals)
	regs := regWindow(r.stack, wb)
	code := c.code
	// The loop's pc stays in a register only between calls: a case that
	// calls out keeps it in kept across the call and reads it back, as it
	// reads its instruction back from the code, so that neither is live
	// across the call and Go does not store them at every dispatch.
	var kept regKept
	var failed error
	pc := start
	for {
		var off int32
		w := regAt(code, pc)
		pc++
		switch w.Op() & 127 {
		case bytecode.RNop:
		case bytecode.RMove:
			regs[w.A()] = regs[w.B()]
		case bytecode.RLoadInt:
			regs[w.A()] = Int32(int32(w.BC()))
		case bytecode.RLoadConst:
			regs[w.A()] = cl.consts[w.BC()]
		case bytecode.RLoadUndef:
			regs[w.A()] = Undefined
		case bytecode.RLoadNull:
			regs[w.A()] = Null
		case bytecode.RLoadTrue:
			regs[w.A()] = True
		case bytecode.RLoadFalse:
			regs[w.A()] = False
		case bytecode.RLoadEmpty:
			regs[w.A()] = Value{num: mkTag(KindString, 0), ref: unsafe.Pointer(emptyString)}
		case bytecode.RLoadUninit:
			regs[w.A()] = uninitialized
		case bytecode.RGetUpval:
			regs[w.A()] = cl.upvalues[w.BC()].get()
		case bytecode.RSetUpval:
			cl.upvalues[w.BC()].set(regs[w.A()])

		case bytecode.RGetGlobal:
			// The global object's slot the site remembers, while the global
			// epoch is the one it was filled at (regNoteGlobal): no binding
			// of the global object has moved, gone or changed its attributes
			// since, and no script-level lexical binding shadows it, nor an
			// eval's variable, which a frame of the register form never has.
			if ep := cl.regEpochs; uint(w.C()) < uint(len(ep)) && ep[w.C()] == r.shapes.globalEpoch+1 {
				s := &cl.ic[w.C()]
				if env, i := s.p1, uint(s.idx); i < uint(env.valn) {
					regs[w.A()] = env.valAt(int(i))
					continue
				}
			}
			goto slow
		case bytecode.RSetGlobal:
			if ep := cl.regEpochs; uint(w.C()) < uint(len(ep)) && ep[w.C()] == r.shapes.globalEpoch+1 {
				s := &cl.ic[w.C()]
				if env, i := s.p1, uint(s.idx); i < uint(env.valn) {
					env.vals()[i] = regs[w.A()]
					continue
				}
			}
			kept.pc = pc
			if _, err := r.regSlow(f, c, &kept, regs); err != nil {
				failed = err
				goto fail
			}
			pc = kept.pc

		case bytecode.RGetProp, bytecode.RGetMethod:
			if o := regs[w.B()]; o.IsObject() {
				obj := o.Object()
				s := &cl.ic[w.C()]
				if obj.shape == s.ownShape {
					regs[w.A()] = obj.valAt(int(s.idx))
					continue
				}
				// A prototype's data property the site remembers: a method,
				// in the holder propCache.holder finds, written out.
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
						continue
					}
				}
			}
			goto slow
		case bytecode.RGetMethodW:
			// The receiver put in the call's window beside its method.
			if o := regs[w.B()]; o.IsObject() {
				obj := o.Object()
				s := &cl.ic[w.C()]
				p := (*[2]Value)(unsafe.Pointer(&regs[w.A()]))
				if obj.shape == s.ownShape {
					p[0], p[1] = obj.valAt(int(s.idx)), o
					continue
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
						continue
					}
				}
			}
			goto slow
		case bytecode.RSetProp:
			if o := regs[w.A()]; o.IsObject() {
				obj := o.Object()
				s := &cl.ic[w.C()]
				if obj.shape == s.ownShape {
					obj.vals()[s.idx] = regs[w.B()]
					continue
				}
			}
			goto setAdd
		case bytecode.RGetIndex:
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
							continue
						}
					}
				}
			}
			goto typed
		case bytecode.RGetIndexPre:
			// x[++i]: the key stepped, then the element read; neither is
			// written until both are known, so that the slow path does both.
			obj, key := regs[w.B()], regs[w.C()]
			if obj.IsObject() && key.IsNumber() {
				k := key.Number() + float64(int8(w.X()))
				o := obj.Object()
				if i := uint32(k); float64(i) == k && o.flags&(objHasExt|objMappedArguments) == objHasExt {
					if es := o.extAt().elems; uint(i) < uint(len(es)) {
						if v := es[i]; !isHole(v) {
							regs[w.C()] = Value{num: k}
							regs[w.A()] = v
							continue
						}
					}
				}
			}
			goto slow
		case bytecode.RGetIndexPost:
			// x[i++]: the element read at the key, then the key stepped;
			// neither is written until both are known.
			obj, key := regs[w.B()], regs[w.C()]
			if obj.IsObject() && key.IsNumber() {
				o := obj.Object()
				if i := uint32(key.Number()); float64(i) == key.Number() &&
					o.flags&(objHasExt|objMappedArguments) == objHasExt {
					if es := o.extAt().elems; uint(i) < uint(len(es)) {
						if v := es[i]; !isHole(v) {
							regs[w.C()] = Value{num: key.Number() + float64(int8(w.X()))}
							regs[w.A()] = v
							continue
						}
					}
				}
			}
			goto slow
		case bytecode.RSetIndex:
			obj, key := regs[w.A()], regs[w.B()]
			if obj.IsObject() && key.IsNumber() {
				o := obj.Object()
				if i := uint32(key.Number()); float64(i) == key.Number() &&
					o.flags&(objHasExt|objMappedArguments|objSharedElems) == objHasExt {
					if es := o.extAt().elems; uint(i) < uint(len(es)) && !isHole(es[i]) {
						es[i] = regs[w.C()]
						continue
					}
				}
			}
			kept.pc = pc
			if r.regSetMiss(c, &kept, regs) {
				pc = kept.pc
				continue
			}
			if _, err := r.regSlow(f, c, &kept, regs); err != nil {
				failed = err
				goto fail
			}
			pc = kept.pc

		case bytecode.RAdd:
			a, b := regs[w.B()], regs[w.C()]
			if a.IsNumber() && b.IsNumber() {
				regs[w.A()] = regNum(a.Number() + b.Number())
				continue
			}
			goto slow
		case bytecode.RSub:
			a, b := regs[w.B()], regs[w.C()]
			if a.IsNumber() && b.IsNumber() {
				regs[w.A()] = regNum(a.Number() - b.Number())
				continue
			}
			goto slow
		case bytecode.RMul:
			a, b := regs[w.B()], regs[w.C()]
			if a.IsNumber() && b.IsNumber() {
				regs[w.A()] = regNum(a.Number() * b.Number())
				continue
			}
			goto slow
		case bytecode.RInc:
			if a := regs[w.B()]; a.IsNumber() {
				regs[w.A()] = regNum(a.Number() + 1)
				continue
			}
			goto slow
		case bytecode.RDec:
			if a := regs[w.B()]; a.IsNumber() {
				regs[w.A()] = regNum(a.Number() - 1)
				continue
			}
			goto slow
		case bytecode.RBin, bytecode.RUnary, bytecode.RDefineField, bytecode.RNewObject,
			bytecode.RNewArray, bytecode.RCold, bytecode.RGetPrivate, bytecode.RSetPrivate,
			bytecode.RAddIndex, bytecode.RAddIndexI:
			// What the loop has no fast path of its own for, cold's
			// operations, which may throw (regColdSub), and what runs
			// per turn of no loop the suites have: a[i] += v and a[i] += 1
			// of a key in a register (regAddIndex).
			kept.pc = pc
			if _, err := r.regSlow(f, c, &kept, regs); err != nil {
				failed = err
				goto fail
			}
			pc = kept.pc

		case bytecode.RJmp:
			off = int32(w.BC())
			goto jump
		case bytecode.RJmpF:
			// What needs a call is the compare's slow path's, so that
			// nothing of the instruction is live across one. ToBoolean of
			// what needs no call -- a boolean, an object, a number --
			// written out in each.
			v := regs[w.A()]
			var t bool
			switch b := math.Float64bits(v.num); {
			case b == trueBits:
				t = true
			case b == falseBits:
			case b == objectBits:
				t = (*Object)(v.ref).flags&objHTMLDDA == 0
			case v.IsNumber():
				t = v.num != 0 && v.num == v.num
			default:
				goto cmpSlow
			}
			if !t {
				off = int32(w.BC())
				goto jump
			}
		case bytecode.RJmpT:
			v := regs[w.A()]
			var t bool
			switch b := math.Float64bits(v.num); {
			case b == trueBits:
				t = true
			case b == falseBits:
			case b == objectBits:
				t = (*Object)(v.ref).flags&objHTMLDDA == 0
			case v.IsNumber():
				t = v.num != 0 && v.num == v.num
			default:
				goto cmpSlow
			}
			if t {
				off = int32(w.BC())
				goto jump
			}
		case bytecode.RRet:
			return regs[w.A()], nil
		case bytecode.RRetUndef:
			return Undefined, nil
		case bytecode.RAndI:
			if a := regs[w.B()]; a.IsNumber() && regIsInt32(a.num) {
				x := int32(a.num)
				regs[w.A()] = Int32(x & w.SC())
				continue
			}
			goto slow
		case bytecode.ROrI:
			if a := regs[w.B()]; a.IsNumber() && regIsInt32(a.num) {
				x := int32(a.num)
				regs[w.A()] = Int32(x | w.SC())
				continue
			}
			goto slow
		case bytecode.RXorI:
			if a := regs[w.B()]; a.IsNumber() && regIsInt32(a.num) {
				x := int32(a.num)
				regs[w.A()] = Int32(x ^ w.SC())
				continue
			}
			goto slow
		case bytecode.RShlI:
			if a := regs[w.B()]; a.IsNumber() && regIsInt32(a.num) {
				x := int32(a.num)
				regs[w.A()] = Int32(x << (uint32(w.SC()) & 31))
				continue
			}
			goto slow
		case bytecode.RShrI:
			if a := regs[w.B()]; a.IsNumber() && regIsInt32(a.num) {
				x := int32(a.num)
				regs[w.A()] = Int32(x >> (uint32(w.SC()) & 31))
				continue
			}
			goto slow
		case bytecode.RUShrI:
			if a := regs[w.B()]; a.IsNumber() && regIsInt32(a.num) {
				x := int32(a.num)
				regs[w.A()] = Uint32(uint32(x) >> (uint32(w.SC()) & 31))
				continue
			}
			goto slow
		case bytecode.RBitAnd:
			if a := regs[w.B()]; a.IsNumber() && regIsInt32(a.num) {
				if b := regs[w.C()]; b.IsNumber() && regIsInt32(b.num) {
					x, y := int32(a.num), int32(b.num)
					regs[w.A()] = Int32(x & y)
					continue
				}
			}
			goto slow
		case bytecode.RBitXor:
			if a := regs[w.B()]; a.IsNumber() && regIsInt32(a.num) {
				if b := regs[w.C()]; b.IsNumber() && regIsInt32(b.num) {
					x, y := int32(a.num), int32(b.num)
					regs[w.A()] = Int32(x ^ y)
					continue
				}
			}
			goto slow
		case bytecode.RShr:
			if a := regs[w.B()]; a.IsNumber() && regIsInt32(a.num) {
				if b := regs[w.C()]; b.IsNumber() && regIsInt32(b.num) {
					x, y := int32(a.num), int32(b.num)
					regs[w.A()] = Int32(x >> (uint32(y) & 31))
					continue
				}
			}
			goto slow
		case bytecode.RCallW:
			kept.pc = pc
			if err := r.regCallW(f, c, &kept, wb); err != nil {
				failed = err
				goto fail
			}
			pc = kept.pc
		case bytecode.RAddI:
			if a := regs[w.B()]; a.IsNumber() {
				regs[w.A()] = regNum(a.Number() + float64(w.SC()))
				continue
			}
			goto slow
		case bytecode.RSubI:
			if a := regs[w.B()]; a.IsNumber() {
				regs[w.A()] = regNum(a.Number() - float64(w.SC()))
				continue
			}
			goto slow
		case bytecode.RJLt:
			if a, b := regs[w.A()], regs[w.B()]; a.IsNumber() && b.IsNumber() {
				if a.Number() < b.Number() {
					goto taken
				}
				continue
			}
			goto cmpSlow
		case bytecode.RJLtU:
			if a, b := regs[w.A()], cl.upvalues[w.B()].get(); a.IsNumber() && b.IsNumber() {
				if a.Number() < b.Number() {
					goto taken
				}
				continue
			}
			goto cmpSlow
		case bytecode.RJLeU:
			if a, b := regs[w.A()], cl.upvalues[w.B()].get(); a.IsNumber() && b.IsNumber() {
				if a.Number() <= b.Number() {
					goto taken
				}
				continue
			}
			goto cmpSlow
		case bytecode.RJLtI:
			if a := regs[w.A()]; a.IsNumber() {
				if a.Number() < float64(w.SB()) {
					goto taken
				}
				continue
			}
			goto cmpSlow
		case bytecode.RJNLt:
			if a, b := regs[w.A()], regs[w.B()]; a.IsNumber() && b.IsNumber() {
				if !(a.Number() < b.Number()) {
					goto taken
				}
				continue
			}
			goto cmpSlow
		case bytecode.RJNLtI:
			if a := regs[w.A()]; a.IsNumber() {
				if !(a.Number() < float64(w.SB())) {
					goto taken
				}
				continue
			}
			goto cmpSlow
		case bytecode.RJLe:
			if a, b := regs[w.A()], regs[w.B()]; a.IsNumber() && b.IsNumber() {
				if a.Number() <= b.Number() {
					goto taken
				}
				continue
			}
			goto cmpSlow
		case bytecode.RJGt:
			if a, b := regs[w.A()], regs[w.B()]; a.IsNumber() && b.IsNumber() {
				if a.Number() > b.Number() {
					goto taken
				}
				continue
			}
			goto cmpSlow
		case bytecode.RJGtI:
			if a := regs[w.A()]; a.IsNumber() {
				if a.Number() > float64(w.SB()) {
					goto taken
				}
				continue
			}
			goto cmpSlow
		case bytecode.RJNGt:
			if a, b := regs[w.A()], regs[w.B()]; a.IsNumber() && b.IsNumber() {
				if !(a.Number() > b.Number()) {
					goto taken
				}
				continue
			}
			goto cmpSlow
		case bytecode.RJNGtI:
			if a := regs[w.A()]; a.IsNumber() {
				if !(a.Number() > float64(w.SB())) {
					goto taken
				}
				continue
			}
			goto cmpSlow
		case bytecode.RJGeI:
			if a := regs[w.A()]; a.IsNumber() {
				if a.Number() >= float64(w.SB()) {
					goto taken
				}
				continue
			}
			goto cmpSlow
		case bytecode.RJNe, bytecode.RJNeI:
			// B is the immediate form's operand, no register: read only
			// where it names one, which the verifier proves in the frame.
			if a := regs[w.A()]; w.Op() == bytecode.RJNeI {
				if a.IsNumber() {
					if a.Number() != float64(w.SB()) {
						goto taken
					}
					continue
				}
			} else if b := regs[w.B()]; a.IsNumber() && b.IsNumber() {
				if a.Number() != b.Number() {
					goto taken
				}
				continue
			}
			goto cmpSlow
		case bytecode.RJSEq, bytecode.RJSNe:
			// Two numbers, or two values one of which is undefined, null, a
			// boolean, a symbol or an object, equal only if their words
			// are; two strings or BigInts are the compare's slow path's.
			a, b := regs[w.A()], regs[w.B()]
			var eq bool
			if a.IsNumber() && b.IsNumber() {
				eq = a.num == b.num
			} else if x, y := math.Float64bits(a.num), math.Float64bits(b.num); regWordKind(x) || regWordKind(y) {
				eq = x == y && a.ref == b.ref
			} else {
				goto cmpSlow
			}
			if eq == (w.Op() == bytecode.RJSEq) {
				goto taken
			}
		case bytecode.RJSNeI:
			if a := regs[w.A()]; !a.IsNumber() || a.Number() != float64(w.SB()) {
				goto taken
			}
		case bytecode.RGetIndexI:
			if obj := regs[w.B()]; obj.IsObject() {
				o := obj.Object()
				if i := uint(w.C()); i < uint(len(o.elemsOf())) && o.flags&objMappedArguments == 0 {
					if v := o.elemsOf()[i]; !isHole(v) {
						regs[w.A()] = v
						continue
					}
				}
			}
			goto typed
		case bytecode.RSetIndexI:
			if obj := regs[w.A()]; obj.IsObject() {
				o := obj.Object()
				if i := uint(w.B()); i < uint(len(o.elemsOf())) && o.flags&(objMappedArguments|objSharedElems) == 0 && !isHole(o.elemsOf()[i]) {
					o.extOf().elems[i] = regs[w.C()]
					continue
				}
			}
			goto typed
		case bytecode.RCallMW:
			kept.pc = pc
			if err := r.regCallW(f, c, &kept, wb); err != nil {
				failed = err
				goto fail
			}
			pc = kept.pc
		case bytecode.RPostInc:
			if a := regs[w.B()]; a.IsNumber() {
				regs[w.A()] = a
				regs[w.B()] = regNum(a.Number() + 1)
				continue
			}
			goto slow
		case bytecode.RPostDec:
			if a := regs[w.B()]; a.IsNumber() {
				regs[w.A()] = a
				regs[w.B()] = regNum(a.Number() - 1)
				continue
			}
			goto slow
		case bytecode.RGetLength:
			if o := regs[w.B()]; o.IsObject() && o.object().class == ClassArray {
				// Object.arrayLength, written out.
				obj := o.object()
				n := uint32(len(obj.elemsOf()))
				if obj.flags&objHasSparseElements != 0 && obj.arrayExt().arrayLen > n {
					n = obj.arrayExt().arrayLen
				}
				regs[w.A()] = Uint32(n)
				continue
			} else if regTagged(math.Float64bits(o.num), KindString) {
				regs[w.A()] = Int((*String)(o.ref).Len())
				continue
			}
			goto slow
		case bytecode.RAddProp, bytecode.RAddPropI:
			if o := regs[w.A()]; o.IsObject() {
				obj := o.Object()
				if s := &cl.ic[w.C()]; obj.shape == s.ownShape {
					p := &obj.vals()[s.idx]
					var b Value
					if w.Op() == bytecode.RAddProp {
						b = regs[w.B()]
					} else {
						b = Int32(w.SB())
					}
					if p.IsNumber() && b.IsNumber() {
						p.num = regNum(p.Number() + b.Number()).num
						continue
					}
				}
			}
			goto slow
		case bytecode.RAddIndexK:
			if o, b := regs[w.A()], regs[w.B()]; o.IsObject() && b.IsNumber() {
				obj := o.Object()
				if i, es := uint(w.C()), obj.elemsOf(); i < uint(len(es)) && obj.flags&(objMappedArguments|objSharedElems) == 0 {
					if p := &es[i]; p.IsNumber() {
						p.num = regNum(p.Number() + b.Number()).num
						continue
					}
				}
			}
			goto slow
		case bytecode.RNewArrayE, bytecode.RSwitch, bytecode.RSwitchTypeof, bytecode.RTypeof, bytecode.RNewObjectLit:
			// Operations not run per turn of a hot loop: one case, which
			// moves none of the others' code, and a second dispatch.
			kept.pc = pc
			r.regCold(f, c, &kept, regs)
			pc = kept.pc
		case bytecode.RJTypeof:
			// An object's type asked inline where it is a function's or
			// an ordinary object's; a proxy and document.all's kind are
			// the slow path's.
			v := regs[w.A()]
			// typeof's answer of v's kind (Value.Kind, regTypeOf), written
			// out.
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
					goto cmpSlow
				}
				if _, isFn := d.(*funcData); isFn {
					t = typeFunction
				}
			}
			if (t == w.X()&^bytecode.RCmpTrue) == (w.X()&bytecode.RCmpTrue != 0) {
				goto taken
			}
		case bytecode.RNewW, bytecode.RClosureW, bytecode.RInstanceOf, bytecode.RIn, bytecode.RDelete, bytecode.RArguments, bytecode.RNewArrayK,
			bytecode.RApplyArgs, bytecode.RArgsLen, bytecode.RArgsIndex, bytecode.RConcat:
			// What the loop has no fast path of its own for.
			goto slow
		case bytecode.RStrPart:
			// A string or a number is joined as it is.
			if v := regs[w.A()]; regTagged(math.Float64bits(v.num), KindString) || v.IsNumber() {
				continue
			}
			goto slow
		case bytecode.RAppend:
			kept.pc = pc
			if err := r.regAppend(f, c, &kept, regs); err != nil {
				failed = err
				goto fail
			}
			pc = kept.pc
		case bytecode.RStrFreeze:
			// A read of a binding an append grows, which what read it may
			// keep: only a string an append owns is written.
			if v := regs[w.A()]; regTagged(math.Float64bits(v.num), KindString) {
				if s := (*String)(v.ref); s.building {
					s.building = false
				}
			}
		case bytecode.RCharCodeAt:
			kept.pc = pc
			if err := r.regCharCodeAt(f, c, &kept, regs); err != nil {
				failed = err
				goto fail
			}
			pc = kept.pc
		case bytecode.RIterNext:
			kept.pc = pc
			if err := r.regIterNext(f, c, &kept, regs); err != nil {
				failed = err
				goto fail
			}
			pc = kept.pc
		case bytecode.RLit:
			kept.pc = pc
			if err := r.regLit(f, c, &kept, regs); err != nil {
				failed = err
				goto fail
			}
			pc = kept.pc
		case bytecode.RYield:
			// A generator's step, suspended with what it yields: the
			// frame's suspension, which no handler of its own takes
			// (regCatch), as no other error leaves without the fail tail.
			kept.pc = pc
			failed = r.regYield(f, regs[w.B()], pc)
			goto fail
		case bytecode.RCallWT, bytecode.RCallMWT:
			goto tail
		// The hot cases are above, in their order; every other case is
		// below them, so that an edit to one moves no hot case's code.
		// The hot ones are layoutcases.txt's hot line, by dispatch counts
		// over the suites and the micro-benchmarks, and TestLayoutLock
		// fails where one has moved (docs/register-interpreter.md, "The
		// loop"). A new operation is a cold sub-operation or a case here.
		case bytecode.RLoadThis:
			regs[w.A()] = f.this
		case bytecode.RCheckRef:
			if regTagged(math.Float64bits(regs[w.A()].num), KindUninitialized) {
				kept.pc = pc
				failed = r.regThrowDeadZone(f, c, pc)
				goto fail
			}
		case bytecode.RBitOr:
			if a := regs[w.B()]; a.IsNumber() && regIsInt32(a.num) {
				if b := regs[w.C()]; b.IsNumber() && regIsInt32(b.num) {
					x, y := int32(a.num), int32(b.num)
					regs[w.A()] = Int32(x | y)
					continue
				}
			}
			goto slow
		case bytecode.RShl:
			if a := regs[w.B()]; a.IsNumber() && regIsInt32(a.num) {
				if b := regs[w.C()]; b.IsNumber() && regIsInt32(b.num) {
					x, y := int32(a.num), int32(b.num)
					regs[w.A()] = Int32(x << (uint32(y) & 31))
					continue
				}
			}
			goto slow
		case bytecode.RUShr:
			if a := regs[w.B()]; a.IsNumber() && regIsInt32(a.num) {
				if b := regs[w.C()]; b.IsNumber() && regIsInt32(b.num) {
					x, y := int32(a.num), int32(b.num)
					regs[w.A()] = Uint32(uint32(x) >> (uint32(y) & 31))
					continue
				}
			}
			goto slow
		case bytecode.RJNLtU:
			if a, b := regs[w.A()], cl.upvalues[w.B()].get(); a.IsNumber() && b.IsNumber() {
				if !(a.Number() < b.Number()) {
					goto taken
				}
				continue
			}
			goto cmpSlow
		case bytecode.RJNLeU:
			if a, b := regs[w.A()], cl.upvalues[w.B()].get(); a.IsNumber() && b.IsNumber() {
				if !(a.Number() <= b.Number()) {
					goto taken
				}
				continue
			}
			goto cmpSlow
		case bytecode.RJLeI:
			if a := regs[w.A()]; a.IsNumber() {
				if a.Number() <= float64(w.SB()) {
					goto taken
				}
				continue
			}
			goto cmpSlow
		case bytecode.RJNLe:
			if a, b := regs[w.A()], regs[w.B()]; a.IsNumber() && b.IsNumber() {
				if !(a.Number() <= b.Number()) {
					goto taken
				}
				continue
			}
			goto cmpSlow
		case bytecode.RJNLeI:
			if a := regs[w.A()]; a.IsNumber() {
				if !(a.Number() <= float64(w.SB())) {
					goto taken
				}
				continue
			}
			goto cmpSlow
		case bytecode.RJGe:
			if a, b := regs[w.A()], regs[w.B()]; a.IsNumber() && b.IsNumber() {
				if a.Number() >= b.Number() {
					goto taken
				}
				continue
			}
			goto cmpSlow
		case bytecode.RJNGe:
			if a, b := regs[w.A()], regs[w.B()]; a.IsNumber() && b.IsNumber() {
				if !(a.Number() >= b.Number()) {
					goto taken
				}
				continue
			}
			goto cmpSlow
		case bytecode.RJNGeI:
			if a := regs[w.A()]; a.IsNumber() {
				if !(a.Number() >= float64(w.SB())) {
					goto taken
				}
				continue
			}
			goto cmpSlow
		case bytecode.RJEq, bytecode.RJEqI:
			// B is the immediate form's operand, no register: read only
			// where it names one, which the verifier proves in the frame.
			if a := regs[w.A()]; w.Op() == bytecode.RJEqI {
				if a.IsNumber() {
					if a.Number() == float64(w.SB()) {
						goto taken
					}
					continue
				}
			} else if b := regs[w.B()]; a.IsNumber() && b.IsNumber() {
				if a.Number() == b.Number() {
					goto taken
				}
				continue
			}
			goto cmpSlow
		case bytecode.RJNull, bytecode.RJNotNull:
			// An object other than document.all's stand-in is not == null.
			if a := regs[w.A()]; !a.IsObject() || a.object().flags&objHTMLDDA == 0 {
				// Value.IsNullish, written out.
				b := math.Float64bits(a.num)
				k := Kind(b & 0xFF)
				if (b&tagMask == tagBase && (k == KindNull || k == KindUndefined)) == (w.Op() == bytecode.RJNull) {
					goto taken
				}
				continue
			}
			goto cmpSlow
		case bytecode.RJSEqI:
			// Only a number is === a number.
			if a := regs[w.A()]; a.IsNumber() && a.Number() == float64(w.SB()) {
				goto taken
			}
		case bytecode.RThrow:
			kept.pc = pc
			failed = r.regThrow(f, c, pc, regs)
			goto fail
		case bytecode.RKeyOfBase:
			// A key whose conversion runs nothing is left as it is, and a
			// null or undefined object is the read's to report.
			if v := regs[w.C()]; regTagged(math.Float64bits(v.num), KindString) || v.IsNumber() || regTagged(math.Float64bits(v.num), KindSymbol) {
				regs[w.A()] = v
				continue
			}
			goto slow
		// Every value of the switch is a case, so that it is a jump table
		// that needs no range check: Go makes one only of a switch whose
		// cases are dense, and the mask tells it the value is below 128. A
		// new operation takes its value out of this list, or the switch does
		// not compile: the values the translation reference's operations
		// had, and 127, invalid's.
		case 42:
			goto invalid
		case 127:
			goto invalid
		}
		continue
	taken:
		// A compare-and-branch taken: its offset is SC.
		off = w.SC()
	jump:
		// A jump taken, by off: one back counts the interrupt budget.
		if off < 0 {
			if r.backEdges--; r.backEdges <= 0 {
				kept.pc = pc
				if err := r.regBackEdge(f, c, &kept); err != nil {
					failed = err
					goto fail
				}
				pc = kept.pc
				// regJumpOff, written out.
				switch w := regAt(code, pc-1); w.Op() {
				case bytecode.RJmp, bytecode.RJmpF, bytecode.RJmpT:
					off = int32(w.BC())
				default:
					off = w.SC()
				}
			}
		}
		pc += int(off)
		continue
	cmpSlow:
		// A comparison of anything but two numbers, and its jump.
		kept.pc = pc
		if t, err := r.regSlow(f, c, &kept, regs); err != nil {
			failed = err
			goto fail
		} else if pc = kept.pc; math.Float64bits(t.num) == math.Float64bits(True.num) && t.ref == True.ref {
			w = regAt(code, pc-1)
			switch w.Op() {
			case bytecode.RJmp, bytecode.RJmpF, bytecode.RJmpT:
				off = int32(w.BC())
			default:
				off = w.SC()
			}
			goto jump
		}
		continue
	typed:
		// An element get_index and the immediate forms have not found
		// dense: a typed array's, read here (regTypedAt, regTypedLoad,
		// written out),
		// the operation and the key decoded again as a miss path may,
		// or out of line (regElemMiss), which set_index_imm's store
		// takes. Here rather than in get_index's case, whose code the
		// hot loops run: there it moved the loop's blocks, and empty
		// loops were 13% slower at every placement.
		if w.Op() != bytecode.RSetIndexI {
			if o := regs[w.B()]; o.IsObject() {
				k := float64(w.C())
				if w.Op() == bytecode.RGetIndex {
					k = regs[w.C()].num
				}
				if obj := o.Object(); obj.class == ClassTypedArray {
					if t, ok := obj.dataOf().(*typedArrayData); ok && t.epoch == r.typedEpoch && t.kind <= elemFloat64 {
						if i := uint32(k); float64(i) == k && uint(i) < uint(t.length) {
							p := unsafe.Add(t.data, uintptr(i)<<elemShifts[t.kind&15])
							var n float64
							ok := true
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
								ok = false
							}
							if ok {
								regs[w.A()] = regNum(n)
								continue
							}
						}
					}
				}
			}
		}
		kept.pc = pc
		if regElemMiss(c, &kept, regs) {
			pc = kept.pc
			continue
		}
		pc = kept.pc
	slow:
		// What a case's fast path did not answer.
		kept.pc = pc
		if _, err := r.regSlow(f, c, &kept, regs); err != nil {
			failed = err
			goto fail
		}
		pc = kept.pc
		continue
	setAdd:
		// A property set_prop adds, by the transition the site remembers,
		// where nothing up the prototype chain the site found intercepts
		// it: storeCached's addition, written out. A tail after the
		// others rather than set_prop's own code, so that it moves no hot
		// case (layoutcases.txt).
		if o := regs[w.A()]; o.IsObject() {
			obj := o.Object()
			s := &cl.ic[w.C()]
			if obj.shape == s.shape && s.next != nil && s.next != setterNext && obj.flags&objExtensible != 0 &&
				(s.p1 == nil || s.p1.shape == s.s1 || s.cell != nil && s.cell.valid) && (s.p2 == nil || s.p2.shape == s.s2) {
				if verifyShapes {
					regVerifyAdd(r, s, obj, cl, c, w)
				}
				// The pc kept across the call, as every case that calls
				// out keeps it (TestRegDispatchKeepsNoPC).
				kept.pc = pc
				obj.appendTransition(regs[w.B()], s.next)
				pc = kept.pc
				continue
			}
		}
		goto slow
	invalid:
		panic("register code: invalid instruction")
	fail:
		// An exception: the instruction at kept.pc-1 threw failed, which
		// a handler of the frame's may catch (regCatch).
		if pc = r.regCatch(f, c, &kept, regs, failed); pc < 0 {
			return Undefined, failed
		}
		continue
	tail:
		// tail_call_w and tail_call_method_w, after every other case and
		// tail: here they move none of the loop's code.
		kept.pc = pc
		if err := r.regTailCallW(f, c, &kept, wb); err != nil {
			failed = err
			goto fail
		}
		pc = kept.pc
	}
}
