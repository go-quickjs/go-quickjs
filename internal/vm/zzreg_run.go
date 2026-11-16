package vm

import (
	"encoding/binary"
	"math"
	"os"
	"sync/atomic"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jsnum"
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
	wide  bool
	inits int32
	// ncap is how many of the function's stack slots a closure captures,
	// the cells a closure it makes is given (regClosure).
	ncap int32
	// newTarget says the function reads new.target, which its frame keeps.
	newTarget bool
	// args says a frame keeps its arguments as passed, for RArguments.
	args bool
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
}

// runReg runs frame f as register code c and returns what it returns.
//
// A case that finishes its instruction continues the loop; one that can
// fail sets the frame's pc to the stack form's instruction first, which is
// what a stack trace or an error's position reads, keeps the pc, and goes
// to the fail tail with the error, where a handler of the frame's may
// catch it (regCatch).
func (r *Runtime) runReg(f *frame, c *regCode) (Value, error) {
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
	pc := 0
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
			regs[w.A()] = Str(emptyString)
		case bytecode.RLoadUninit:
			regs[w.A()] = uninitialized
		case bytecode.RLoadThis:
			regs[w.A()] = f.this
		case bytecode.RCheckRef:
			if regs[w.A()].IsUninitialized() {
				kept.pc = pc
				failed = r.regThrowDeadZone(f, c, pc)
				goto fail
			}
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
				// A prototype's data property the site remembers: a method.
				if h := s.holder(obj); h != nil && !s.getter {
					regs[w.A()] = h.valAt(int(s.idx))
					continue
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
				if h := s.holder(obj); h != nil && !s.getter {
					p[0], p[1] = h.valAt(int(s.idx)), o
					continue
				}
			}
			goto slow
		case bytecode.RSetProp:
			if o := regs[w.A()]; o.IsObject() {
				obj := o.Object()
				if s := &cl.ic[w.C()]; obj.shape == s.ownShape {
					obj.vals()[s.idx] = regs[w.B()]
					continue
				}
			}
			goto slow
		case bytecode.RGetIndex:
			obj, key := regs[w.B()], regs[w.C()]
			if obj.IsObject() && key.IsNumber() {
				o := obj.Object()
				if i := uint32(key.Number()); float64(i) == key.Number() &&
					uint(i) < uint(len(o.elemsOf())) && o.flags&objMappedArguments == 0 {
					if v := o.elemsOf()[i]; !isHole(v) {
						regs[w.A()] = v
						continue
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
				if i := uint32(k); float64(i) == k &&
					uint(i) < uint(len(o.elemsOf())) && o.flags&objMappedArguments == 0 {
					if v := o.elemsOf()[i]; !isHole(v) {
						regs[w.C()] = Value{num: k}
						regs[w.A()] = v
						continue
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
					uint(i) < uint(len(o.elemsOf())) && o.flags&objMappedArguments == 0 {
					if v := o.elemsOf()[i]; !isHole(v) {
						regs[w.C()] = Value{num: key.Number() + float64(int8(w.X()))}
						regs[w.A()] = v
						continue
					}
				}
			}
			goto slow
		case bytecode.RSetIndex:
			obj, key := regs[w.A()], regs[w.B()]
			if obj.IsObject() && key.IsNumber() {
				o := obj.Object()
				if i := uint32(key.Number()); float64(i) == key.Number() &&
					uint(i) < uint(len(o.elemsOf())) && o.flags&(objMappedArguments|objSharedElems) == 0 && !isHole(o.elemsOf()[i]) {
					o.extOf().elems[i] = regs[w.C()]
					continue
				}
			}
			kept.pc = pc
			if regSetMiss(c, &kept, regs) {
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
			bytecode.RNewArray, bytecode.RCold:
			// What the loop has no fast path of its own for, and cold's
			// operations, which may throw (regColdSub).
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
			// nothing of the instruction is live across one.
			if t, ok := regTruth(regs[w.A()]); !ok {
				goto cmpSlow
			} else if !t {
				off = int32(w.BC())
				goto jump
			}
		case bytecode.RJmpT:
			if t, ok := regTruth(regs[w.A()]); !ok {
				goto cmpSlow
			} else if t {
				off = int32(w.BC())
				goto jump
			}
		case bytecode.RRet:
			return regs[w.A()], nil
		case bytecode.RRetUndef:
			return Undefined, nil
		case bytecode.RAndI:
			if x, ok := regInt32(regs[w.B()]); ok {
				regs[w.A()] = Int32(x & w.SC())
				continue
			}
			goto slow
		case bytecode.ROrI:
			if x, ok := regInt32(regs[w.B()]); ok {
				regs[w.A()] = Int32(x | w.SC())
				continue
			}
			goto slow
		case bytecode.RXorI:
			if x, ok := regInt32(regs[w.B()]); ok {
				regs[w.A()] = Int32(x ^ w.SC())
				continue
			}
			goto slow
		case bytecode.RShlI:
			if x, ok := regInt32(regs[w.B()]); ok {
				regs[w.A()] = Int32(x << (uint32(w.SC()) & 31))
				continue
			}
			goto slow
		case bytecode.RShrI:
			if x, ok := regInt32(regs[w.B()]); ok {
				regs[w.A()] = Int32(x >> (uint32(w.SC()) & 31))
				continue
			}
			goto slow
		case bytecode.RUShrI:
			if x, ok := regInt32(regs[w.B()]); ok {
				regs[w.A()] = Uint32(uint32(x) >> (uint32(w.SC()) & 31))
				continue
			}
			goto slow
		case bytecode.RBitAnd:
			if x, ok := regInt32(regs[w.B()]); ok {
				if y, ok := regInt32(regs[w.C()]); ok {
					regs[w.A()] = Int32(x & y)
					continue
				}
			}
			goto slow
		case bytecode.RBitOr:
			if x, ok := regInt32(regs[w.B()]); ok {
				if y, ok := regInt32(regs[w.C()]); ok {
					regs[w.A()] = Int32(x | y)
					continue
				}
			}
			goto slow
		case bytecode.RBitXor:
			if x, ok := regInt32(regs[w.B()]); ok {
				if y, ok := regInt32(regs[w.C()]); ok {
					regs[w.A()] = Int32(x ^ y)
					continue
				}
			}
			goto slow
		case bytecode.RShl:
			if x, ok := regInt32(regs[w.B()]); ok {
				if y, ok := regInt32(regs[w.C()]); ok {
					regs[w.A()] = Int32(x << (uint32(y) & 31))
					continue
				}
			}
			goto slow
		case bytecode.RShr:
			if x, ok := regInt32(regs[w.B()]); ok {
				if y, ok := regInt32(regs[w.C()]); ok {
					regs[w.A()] = Int32(x >> (uint32(y) & 31))
					continue
				}
			}
			goto slow
		case bytecode.RUShr:
			if x, ok := regInt32(regs[w.B()]); ok {
				if y, ok := regInt32(regs[w.C()]); ok {
					regs[w.A()] = Uint32(uint32(x) >> (uint32(y) & 31))
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
		case bytecode.RJNLtU:
			if a, b := regs[w.A()], cl.upvalues[w.B()].get(); a.IsNumber() && b.IsNumber() {
				if !(a.Number() < b.Number()) {
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
		case bytecode.RJNLeU:
			if a, b := regs[w.A()], cl.upvalues[w.B()].get(); a.IsNumber() && b.IsNumber() {
				if !(a.Number() <= b.Number()) {
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
		case bytecode.RJGe:
			if a, b := regs[w.A()], regs[w.B()]; a.IsNumber() && b.IsNumber() {
				if a.Number() >= b.Number() {
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
			if a, b := regs[w.A()], regs[w.B()]; w.Op() == bytecode.RJEqI {
				if a.IsNumber() {
					if a.Number() == float64(w.SB()) {
						goto taken
					}
					continue
				}
			} else if a.IsNumber() && b.IsNumber() {
				if a.Number() == b.Number() {
					goto taken
				}
				continue
			}
			goto cmpSlow
		case bytecode.RJNe, bytecode.RJNeI:
			if a, b := regs[w.A()], regs[w.B()]; w.Op() == bytecode.RJNeI {
				if a.IsNumber() {
					if a.Number() != float64(w.SB()) {
						goto taken
					}
					continue
				}
			} else if a.IsNumber() && b.IsNumber() {
				if a.Number() != b.Number() {
					goto taken
				}
				continue
			}
			goto cmpSlow
		case bytecode.RJNull, bytecode.RJNotNull:
			// An object other than document.all's stand-in is not == null.
			if a := regs[w.A()]; !a.IsObject() || a.object().flags&objHTMLDDA == 0 {
				if a.IsNullish() == (w.Op() == bytecode.RJNull) {
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
		case bytecode.RJSEqI:
			// Only a number is === a number.
			if a := regs[w.A()]; a.IsNumber() && a.Number() == float64(w.SB()) {
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
				regs[w.A()] = Uint32(o.object().arrayLength())
				continue
			} else if o.IsString() {
				regs[w.A()] = Int(o.String().Len())
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
		case bytecode.RAddIndex:
			// A dense element's number and a number added in place, as
			// set_index's fast path writes an element. Only the number's
			// word is written, as the tree's updates write it: a number's
			// pointer word is nil already, and the word alone needs no
			// write barrier's check.
			if o, k, b := regs[w.A()], regs[w.B()], regs[w.C()]; o.IsObject() && k.IsNumber() && b.IsNumber() {
				obj := o.Object()
				if i, es := uint32(k.Number()), obj.elemsOf(); float64(i) == k.Number() && uint(i) < uint(len(es)) && obj.flags&(objMappedArguments|objSharedElems) == 0 {
					if p := &es[i]; p.IsNumber() {
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
		case bytecode.RAddIndexI:
			if o, k := regs[w.A()], regs[w.B()]; o.IsObject() && k.IsNumber() {
				obj := o.Object()
				if i, es := uint32(k.Number()), obj.elemsOf(); float64(i) == k.Number() && uint(i) < uint(len(es)) && obj.flags&(objMappedArguments|objSharedElems) == 0 {
					if p := &es[i]; p.IsNumber() {
						p.num = regNum(p.Number() + float64(w.SC())).num
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
			t := regTypeOf(v.Kind())
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
		case bytecode.RThrow:
			kept.pc = pc
			failed = r.regThrow(f, c, pc, regs)
			goto fail
		case bytecode.RNewW, bytecode.RClosureW, bytecode.RInstanceOf, bytecode.RIn, bytecode.RDelete, bytecode.RArguments, bytecode.RNewArrayK,
			bytecode.RApplyArgs, bytecode.RArgsLen, bytecode.RArgsIndex, bytecode.RConcat:
			// What the loop has no fast path of its own for.
			goto slow
		case bytecode.RKeyOfBase:
			// A key whose conversion runs nothing is left as it is, and a
			// null or undefined object is the read's to report.
			if v := regs[w.C()]; v.IsString() || v.IsNumber() || v.IsSymbol() {
				regs[w.A()] = v
				continue
			}
			goto slow
		case bytecode.RStrPart:
			// A string or a number is joined as it is.
			if v := regs[w.A()]; v.IsString() || v.IsNumber() {
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
			if v := regs[w.A()]; v.IsString() {
				if s := v.String(); s.building {
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
		// Every value of the switch is a case, so that it is a jump table
		// that needs no range check: Go makes one only of a switch whose
		// cases are dense, and the mask tells it the value is below 128. A
		// new operation takes its value out of this list, or the switch does
		// not compile: the values the translation reference's operations
		// had, and 127, invalid's.
		case bytecode.RCallWT, bytecode.RCallMWT:
			goto tail
		case 40, 41, 42, 43:
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
				off = regJumpOff(regAt(code, pc-1))
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
		} else if pc = kept.pc; t.sameBits(True) {
			w = regAt(code, pc-1)
			off = regJumpOff(w)
			goto jump
		}
		continue
	typed:
		// An element get_index and the immediate forms have not found
		// dense: a typed array's, out of line (regElemMiss).
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

// regCallW is call_w and call_method_w: a call whose callee's frame
// starts at the caller's window, R[A], its parameters the arguments where
// they lie. It writes the call's value to R[B] itself, so that the loop
// keeps nothing of the instruction across the call.
//
// A callee the site called last, of the form's own layout and with nothing
// of its surroundings to establish but a this its cache holds, is entered
// here: its frame is the window, the frame record the few fields a stack
// trace and a closure read, and runReg is the only call between the two
// loops. What the frame needs is read from the callee's register code,
// whose facts are the function's, precomputed. Anything else is
// regCallMiss's.
func (r *Runtime) regCallW(f *frame, c *regCode, k *regKept, wb int) error {
	pc := k.pc - 1
	w := regAt(c.code, pc)
	base := wb + int(w.A())
	win := regWindow(r.stack, base)
	if calls := f.cl.regCalls; uint(w.C()) < uint(len(calls)) {
		e := &calls[w.C()]
		// The callee's tag and pointer read as words: read as a Value, the
		// tag goes through a float register to be compared.
		if regIsObject(&win[0]) && (*Object)(win[0].ref) == e.callee {
			// A leaf is answered first, before the frame's pc is written:
			// its body reads nothing of the frame and throws nothing.
			if e.kind&regCallLeaf != 0 {
				if leaf := e.rc.leaf; leaf >= regLeafArg {
					v := e.rc.leafVal
					if p := int(e.rc.leafArg); leaf == regLeafArg {
						v = Undefined
						if p < int(w.X()) {
							v = win[2+p]
						}
					}
					regWindow(r.stack, wb)[w.B()] = v
					return nil
				}
				if e.rc.leaf == regLeafCode {
					if v, ok := r.regLeafRun(e.cl, e.rc, base, int(w.X())); ok {
						regWindow(r.stack, wb)[w.B()] = v
						return nil
					}
					if e.cl.pureMiss >= pureMissLimit {
						// A body that keeps meeting what it cannot run
						// without a frame is entered with one from now on.
						e.kind &^= regCallOut | regCallLeaf
					}
					f.pc = regSrc(c, pc) + 1
					return r.regCallMiss(f, c, w, wb, pc)
				}
				// The this is given to the body, not written to the
				// window: a miss writes it (regCallSlow).
				this, ok := win[1], true
				switch e.kind &^ (regCallOut | regCallLeaf) {
				case regCallThis:
					this = e.this
				case regCallObjThis:
					ok = this.IsObject()
				}
				if ok {
					var v Value
					argc := int(w.X())
					if e.rc.leaf == regLeafPure {
						// pureCallAt checks the closure's misses.
						r.pureRefused = false
						v, ok = r.pureCallAt(e.callee.fn(), this, win[2:2+argc], 0, true)
					} else if ok = this.IsObject(); ok {
						v, ok = r.leafCall(e.cl, this.Object(), win[2:2+argc])
					}
					if ok {
						regWindow(r.stack, wb)[w.B()] = v
						return nil
					}
				}
				f.pc = regSrc(c, pc) + 1
				return r.regCallMiss(f, c, w, wb, pc)
			}
			f.pc = regSrc(c, pc) + 1
			if e.kind == regCallNative {
				return r.regNative(e.callee, w, wb)
			}
			if e.kind == regCallMarked {
				v, err := r.regMarkedCall(f, c, base, int(w.X()), pc, c.rc.Marks[pc], regAuxAt(c, pc))
				if err == nil {
					regWindow(r.stack, wb)[w.B()] = v
				}
				return err
			}
			ok := e.kind == regCallEnter
			if e.kind == regCallThis {
				win[1] = e.this
				ok = true
			} else if e.kind == regCallObjThis {
				ok = win[1].IsObject()
			}
			if ok {
				o, cl, rc := e.callee, e.cl, e.rc
				argc := int(w.X())
				top := base + int(rc.numRegs)
				if top > len(r.stack) || r.frameDepth >= r.maxFrames {
					return r.throwRangeError("maximum call stack size exceeded")
				}
				if r.interruptCounter--; r.interruptCounter <= 0 {
					if err := r.checkInterruptNow(); err != nil {
						return err
					}
				}
				saved := r.stackTop
				if top > saved {
					r.stackTop = top
					if top > r.stackHigh {
						r.stackHigh = top
					}
				}
				if argc > int(rc.params) {
					// The arguments past the parameters, which the
					// bindings overwrite: f.arguments reads them.
					r.regKeepWindow(base, int(rc.params), argc)
				}
				// Parameters no argument was given for, and the bindings
				// past them.
				for i := min(argc, int(rc.params)); i < int(rc.params+rc.inits); i++ {
					win[2+i] = Undefined
				}
				off := r.frameDepth - r.frameBase
				if uint(off) >= uint(len(r.cur)) {
					r.seekFrameBlock()
					off = r.frameDepth - r.frameBase
				}
				r.frameDepth++
				if r.frameDepth > r.frameHigh {
					r.frameHigh = r.frameDepth
				}
				nf := &r.cur[off]
				nf.cl = cl
				nf.callee = o
				nf.base = base
				nf.pc = 0
				if len(nf.openUpvalues) != 0 {
					nf.openUpvalues = nf.openUpvalues[:0]
				}
				if nf.thisRef != nil || nf.withScopes != nil || nf.evalVars != nil || nf.native != "" || nf.args != nil {
					regClearFrame(nf)
				}
				if rc.newTarget {
					nf.newTarget = Undefined
				}
				nf.regKind = regFrameCall
				v, err := r.runReg(nf, rc)
				nf.regKind = 0
				if rc.ncap != 0 && len(nf.openUpvalues) != 0 {
					regCloseUpvalues(nf)
				}
				r.frameDepth--
				r.stackTop = saved
				if err != nil {
					return r.regCallWEnd(nf, regFrameCall, err, saved, wb+int(w.B()))
				}
				regWindow(r.stack, wb)[w.B()] = v
				return nil
			}
		}
	}
	f.pc = regSrc(c, pc) + 1
	return r.regCallMiss(f, c, w, wb, pc)
}

// regEnterW runs a register function of the form's own layout in the
// window at base, which holds the function, its this and its argc
// arguments.
//
// The frame record is written only where a reader needs it: the function,
// the window's base and the pc, which stack traces read, and the fields a
// frame's last user may have left set, cleared where they are. Its this is
// the window's register 1, which regClosure gives the frame when it makes
// an arrow that captures it.
func (r *Runtime) regEnterW(o *Object, cl *closure, rc *regCode, base, argc int, args []Value, newTarget Value) (Value, error) {
	top := base + int(rc.numRegs)
	if top > len(r.stack) || r.frameDepth >= r.maxFrames {
		return Undefined, r.throwRangeError("maximum call stack size exceeded")
	}
	if r.interruptCounter--; r.interruptCounter <= 0 {
		if err := r.checkInterruptNow(); err != nil {
			return Undefined, err
		}
	}
	saved := r.stackTop
	if top > saved {
		r.stackTop = top
		if top > r.stackHigh {
			r.stackHigh = top
		}
	}
	var kept []Value
	if rc.args {
		ka := len(r.argStack)
		// Before the bindings take the window past the parameters, which
		// args may be; kept on the runtime's argument stack until the call
		// returns.
		r.argStack = append(r.argStack, args...)
		kept = r.argStack[ka:len(r.argStack):len(r.argStack)]
	}
	// Parameters no argument was given for, and the bindings past them.
	for i := base + 2 + min(argc, int(rc.params)); i < base+2+int(rc.params)+int(rc.inits); i++ {
		r.stack[i] = Undefined
	}
	off := r.frameDepth - r.frameBase
	if uint(off) >= uint(len(r.cur)) {
		r.seekFrameBlock()
		off = r.frameDepth - r.frameBase
	}
	r.frameDepth++
	if r.frameDepth > r.frameHigh {
		r.frameHigh = r.frameDepth
	}
	f := &r.cur[off]
	f.cl = cl
	f.callee = o
	f.base = base
	f.pc = 0
	if len(f.openUpvalues) != 0 {
		f.openUpvalues = f.openUpvalues[:0]
	}
	if f.thisRef != nil || f.withScopes != nil || f.evalVars != nil || f.native != "" || f.args != nil {
		regClearFrame(f)
	}
	// What an arrow made here captures of new.target, and what a stack
	// trace reads.
	f.newTarget = newTarget
	f.regKind = regFrameEnter
	if rc.args {
		f.args = kept
		p := int(rc.params)
		f.locals = r.stack[base+2 : base+2+p : base+2+p]
	}
	v, err := r.runReg(f, rc)
	f.regKind = 0
	if len(f.openUpvalues) != 0 {
		regCloseUpvalues(f)
	}
	r.frameDepth--
	r.stackTop = saved
	if a := f.args; a != nil {
		// The arguments kept for it, the last on the argument stack.
		r.argStack = r.argStack[:len(r.argStack)-len(a)]
	}
	return v, err
}

// regDirect is callDirect's call of a function whose tree runs register
// code of the form's own layout: its window is made at the top of the stack
// and run there, without callTree's frame, runTree's recover and the copy
// regEnterFrame makes. callDirect has counted the call.
func (r *Runtime) regDirect(o *Object, fd *funcData, c *regCode, this Value, args []Value) (Value, error) {
	if regReportOn {
		regNoteCross(regCrossDirect, c.fn, "")
	}
	// A body answered without a frame is answered here, as callDirect
	// answers one without register code and regCallW one a register call
	// makes: a built-in's callback is most often such a body.
	switch c.leaf {
	case regLeafArg:
		if p := int(c.leafArg); p < len(args) {
			return args[p], nil
		}
		return Undefined, nil
	case regLeafConst:
		return c.leafVal, nil
	case regLeafCode:
		// The parameters' arguments in a window at the top of the stack,
		// the rest of them unread; the body reads neither its callee's
		// register nor its this's (regLeafRunnable).
		m := min(int(c.params), len(args))
		if base := r.stackTop; base+2+m <= len(r.stack) {
			w := r.stack[base+2 : base+2+m : base+2+m]
			for i, a := range args[:m] {
				w[i] = a
			}
			if v, ok := r.regLeafRun(fd.closure, c, base, m); ok {
				return v, nil
			}
		}
	case regLeafPure:
		if fd.closure.pureMiss < pureMissLimit {
			if v, ok := r.pureCall(fd, this, args); ok {
				return v, nil
			}
		}
	case regLeafThis:
		if this.IsObject() {
			if v, ok := r.leafCall(fd.closure, this.Object(), args); ok {
				return v, nil
			}
		}
	}
	if fd.arrow {
		this = fd.extra.lexThis
	} else if c.coerceThis && !this.IsObject() {
		if !this.IsNullish() {
			return r.callTree(o, fd, this, args, false)
		}
		this = r.globalThis
	}
	base := r.stackTop
	if base+int(c.numRegs) > len(r.stack) {
		return Undefined, r.throwRangeError("maximum call stack size exceeded")
	}
	m := min(int(c.params), len(args))
	w := r.stack[base : base+2+m : base+2+m]
	w[0] = Obj(o)
	w[1] = this
	for i, a := range args[:m] {
		w[2+i] = a
	}
	return r.regEnterW(o, fd.closure, c, base, m, args, Undefined)
}

// regNative is call_w's call of the built-in o a site has cached, made as
// callDirect makes one: Math's functions, push and pop and the native
// leaves answered without a frame where their checks pass, anything else
// with the built-in's frame. Its value goes to R[B].
func (r *Runtime) regNative(o *Object, w bytecode.RInstr, wb int) error {
	base := wb + int(w.A())
	args := r.stack[base+2 : base+2+int(w.X())]
	this := Undefined
	if w.Op().MethodCall() {
		this = r.stack[base+1]
	}
	fd := o.fn()
	v, ok := Undefined, false
	if fd.mathOp != 0 {
		v, ok = mathCall(fd.mathOp, args)
	} else if op := fd.elemOp; op >= elemNativeFirst {
		if r.meter == nil && leafChecks[op](r, this, args) {
			var err error
			v, err = fd.native(r, this, args)
			ok = err == nil
		}
	} else if op != 0 {
		v, ok = r.pureElemOp(op, this, args, true)
	}
	if !ok {
		if err := r.tick(); err != nil {
			return err
		}
		if err := r.nativeFrame(o, fd, this, args, Undefined); err != nil {
			return err
		}
		var err error
		v, err = fd.native(r, this, args)
		r.frameDepth--
		if r.meter != nil && err == nil {
			err = r.chargeResult(v)
		}
		if err != nil {
			return err
		}
	}
	r.stack[wb+int(w.B())] = v
	return nil
}

// regLeafRun runs cl's register code c, a regLeafCode body, in the window
// at base, where the callee, its this and argc arguments are, without a
// frame: an arithmetic operation of what is no number gives up, before any
// store, and the call is then made with one. It is the frameless evaluator
// of a pure body (pureCallAt) for the register form, whose instructions
// are fewer, dispatched by a switch of their own (regLeafOp).
func (r *Runtime) regLeafRun(cl *closure, c *regCode, base, argc int) (Value, bool) {
	if cl.pureMiss >= pureMissLimit {
		return Undefined, false
	}
	top := base + int(c.numRegs)
	if top > len(r.stack) {
		return Undefined, false
	}
	if top > r.stackHigh {
		r.stackHigh = top
	}
	regs := regWindow(r.stack, base)
	for i := argc; i < int(c.params+c.inits); i++ {
		regs[2+i] = Undefined
	}
	// The stores to upvalues, made when the body returns.
	var held [regLeafStores]regHeld
	n := 0
	for i := range c.leafOps {
		o := &c.leafOps[i]
		switch o.op {
		case rlMove:
			regs[o.a] = regs[o.b]
		case rlLoad:
			regs[o.a] = o.k
		case rlGetUp:
			regs[o.a] = cl.upvalues[o.b].get()
		case rlSetUp:
			held[n] = regHeld{cl.upvalues[o.b].slot, regs[o.a]}
			n++
		case rlAdd:
			a, b := regs[o.b], regs[o.c]
			if !a.IsNumber() || !b.IsNumber() {
				cl.pureMiss++
				return Undefined, false
			}
			regs[o.a] = regNum(a.Number() + b.Number())
		case rlSub:
			a, b := regs[o.b], regs[o.c]
			if !a.IsNumber() || !b.IsNumber() {
				cl.pureMiss++
				return Undefined, false
			}
			regs[o.a] = regNum(a.Number() - b.Number())
		case rlMul:
			a, b := regs[o.b], regs[o.c]
			if !a.IsNumber() || !b.IsNumber() {
				cl.pureMiss++
				return Undefined, false
			}
			regs[o.a] = regNum(a.Number() * b.Number())
		case rlAddK:
			a := regs[o.b]
			if !a.IsNumber() {
				cl.pureMiss++
				return Undefined, false
			}
			regs[o.a] = regNum(a.Number() + o.k.num)
		case rlRet:
			regLeafStore(held[:n])
			return regs[o.a], true
		case rlRetUndef:
			regLeafStore(held[:n])
			return Undefined, true
		}
	}
	return Undefined, true
}

// regCloseUpvalues closes the upvalues a frame's closures left open on its
// registers.
//
//go:noinline
func regCloseUpvalues(f *frame) {
	for _, u := range f.openUpvalues {
		u.close()
	}
}

// regSlow is the slow path of the instruction at pc, whatever the loop's
// fast path for it did not answer: it sets the frame's pc to the stack
// form's instruction, does what the stack interpreter does, and stores the
// result in its register. A comparison's jump is left to the loop, which is
// given the comparison's result.
//
// It reads the instruction from the code rather than being given it, so that
// the loop keeps nothing of the instruction across the call (see runReg).
func (r *Runtime) regSlow(f *frame, c *regCode, k *regKept, regs *[1 << 16]Value) (Value, error) {
	pc := k.pc - 1
	w := regAt(c.code, pc)
	f.pc = c.src[pc] + 1
	cl := f.cl
	wb := f.base - int(c.locals)
	var v Value
	var err error
	switch w.Op() {
	case bytecode.RCold:
		return Undefined, r.regColdSub(f, c, k, regs)
	case bytecode.RGetGlobal:
		v, err = r.regGetGlobal(f, cl, w)
	case bytecode.RSetGlobal:
		return Undefined, r.regSetGlobal(f, cl, w, regs[w.A()])
	case bytecode.RGetProp:
		v, err = r.regGetProp(cl, c, w, regs[w.B()])
	case bytecode.RGetMethod:
		v, err = r.regGetMethod(cl, c, w, regs[w.B()])
	case bytecode.RGetMethodW:
		o := regs[w.B()]
		v, err = r.regGetMethod(cl, c, w, o)
		(*[2]Value)(unsafe.Pointer(&regs[w.A()]))[1] = o
	case bytecode.RSetProp:
		return Undefined, r.regSetProp(cl, c, w, regs[w.A()], regs[w.B()])
	case bytecode.RGetIndex:
		// A key string the runtime handed out -- a for-in's -- read as
		// the tree's element read reads it (stringKeyRead): a[k] in for
		// (k in a) an element by the key's index.
		if o, key := regs[w.B()], regs[w.C()]; o.IsObject() && key.IsString() {
			if x, ok := r.stringKeyRead(o.Object(), key.String()); ok {
				v = x
				break
			}
		}
		v, err = r.getIndexed(regs[w.B()], regs[w.C()])
	case bytecode.RSetIndex:
		return Undefined, r.setIndexed(regs[w.A()], regs[w.B()], regs[w.C()], cl.fn.Strict)
	case bytecode.RAdd:
		v, err = r.add(regs[w.B()], regs[w.C()])
	case bytecode.RSub:
		v, err = r.arith(bytecode.OpSub, regs[w.B()], regs[w.C()])
	case bytecode.RMul:
		v, err = r.arith(bytecode.OpMul, regs[w.B()], regs[w.C()])
	case bytecode.RBin:
		v, err = r.regBinary(bytecode.Op(w.X()), regs[w.B()], regs[w.C()])
	case bytecode.RAndI, bytecode.ROrI, bytecode.RXorI, bytecode.RShlI, bytecode.RShrI, bytecode.RUShrI:
		v, err = r.regBinImm(regStackOp[w.Op()], regs[w.B()], w.SC())
	case bytecode.RBitAnd, bytecode.RBitOr, bytecode.RBitXor, bytecode.RShl, bytecode.RShr, bytecode.RUShr:
		v, err = r.regBinary(regStackOp[w.Op()], regs[w.B()], regs[w.C()])
	case bytecode.RInc:
		v, err = r.regStep(regs[w.B()], 1)
	case bytecode.RDec:
		v, err = r.regStep(regs[w.B()], -1)
	case bytecode.RUnary:
		v, err = r.regUnary(bytecode.Op(w.X()), regs[w.B()])
	case bytecode.RDefineField:
		return Undefined, r.regDefineField(cl, w, regs[w.A()], regs[w.B()])
	case bytecode.RNewObject:
		v = Obj(r.regNewObject(cl, w))
	case bytecode.RAddIndex:
		return Undefined, r.regAddIndex(f, c.rc.Aux[pc], regs[w.A()], regs[w.B()], regs[w.C()], false)
	case bytecode.RAddIndexK:
		return Undefined, r.regAddIndex(f, c.rc.Aux[pc], regs[w.A()], Int32(int32(w.C())), regs[w.B()], w.X() != 0)
	case bytecode.RAddIndexI:
		return Undefined, r.regAddIndex(f, c.rc.Aux[pc], regs[w.A()], regs[w.B()], Int32(w.SC()), w.X() != 0)
	case bytecode.RNewArray:
		b := wb + int(w.B())
		v = Obj(r.newArrayFrom(r.stack[b : b+int(w.C())]))
	case bytecode.RGetIndexPre:
		// x[++i]: the step, then the read, each as the stack form makes it.
		// Assigned, not declared: the switch's err is what the call returns.
		var n Value
		if n, err = r.regStep(regs[w.C()], float64(int8(w.X()))); err != nil {
			return Undefined, err
		}
		regs[w.C()] = n
		v, err = r.getIndexed(regs[w.B()], n)
	case bytecode.RGetIndexPost:
		// x[i++]: the step, its old value the key, then the read, as the
		// stack form makes them.
		var old, n Value
		if old, err = r.toNumeric(regs[w.C()]); err != nil {
			return Undefined, err
		}
		if n, err = r.regStep(old, float64(int8(w.X()))); err != nil {
			return Undefined, err
		}
		o := regs[w.B()]
		regs[w.C()] = n
		v, err = r.getIndexed(o, old)
	case bytecode.RGetIndexI:
		v, err = r.getIndexed(regs[w.B()], Int32(int32(w.C())))
	case bytecode.RSetIndexI:
		return Undefined, r.setIndexed(regs[w.A()], Int32(int32(w.B())), regs[w.C()], cl.fn.Strict)
	case bytecode.RNewW:
		if regReportOn {
			regCrossOf(regCrossNew, regs[w.A()])
		}
		b := wb + int(w.A())
		if v, err = r.regConstruct(regs[w.A()], b, int(w.X())); err == nil {
			regs[w.B()] = v
		}
		return v, err
	case bytecode.RClosureW:
		v = Obj(r.regClosure(f, c, w, wb))
	case bytecode.RPostInc, bytecode.RPostDec:
		old, err := r.toNumeric(regs[w.B()])
		if err != nil {
			return Undefined, err
		}
		delta := 1.0
		if w.Op() == bytecode.RPostDec {
			delta = -1
		}
		next, err := r.regStep(old, delta)
		if err != nil {
			return Undefined, err
		}
		regs[w.A()] = old
		regs[w.B()] = next
		return old, nil
	case bytecode.RGetLength:
		if o := regs[w.B()]; o.IsString() {
			v = Int(o.String().Len())
		} else {
			v, err = r.getValueProp(o, atomLength)
		}
	case bytecode.RInstanceOf:
		var ok bool
		ok, err = r.instanceOfAt(&cl.ic[c.rc.Aux[pc]], regs[w.B()], regs[w.C()])
		v = Bool(ok)
	case bytecode.RIn:
		v, err = r.regIn(regs[w.B()], regs[w.C()])
	case bytecode.RDelete:
		v, err = r.regDelete(cl, regs[w.B()], regs[w.C()])
	case bytecode.RArguments:
		v = Obj(r.newArgumentsObject(f))
	case bytecode.RArgsLen:
		if a := regs[w.B()]; a.IsObject() {
			v, err = r.getValueProp(a, atomLength)
		} else {
			v = Int(len(f.args))
		}
	case bytecode.RConcat:
		// The registers are the parts' own, which concatParts may convert
		// in place: nothing reads them after.
		b := int(w.B())
		v, err = concatParts(r, regs[b:b+int(w.C())])
	case bytecode.RKeyOfBase:
		if base := regs[w.B()]; base.IsNullish() {
			return Undefined, r.throwTypeError("cannot read property of %s", r.describe(base))
		}
		var k Atom
		if k, err = r.toPropertyKey(regs[w.C()]); err == nil {
			// An object's Symbol.toPrimitive may give a symbol: the key
			// keeps whichever kind it turned out to be.
			v = r.keyToValue(k)
		}
	case bytecode.RStrPart:
		v, err = r.add(Str(emptyString), regs[w.A()])
	case bytecode.RArgsIndex:
		key := regs[w.C()]
		if !regs[w.B()].IsObject() {
			if i := uint32(key.num); key.IsNumber() && float64(i) == key.num && uint(i) < uint(len(f.args)) &&
				!(f.cl.fn.MappedArguments && uint(i) < uint(f.cl.fn.ParamCount)) {
				v = f.args[i]
				break
			}
			regs[w.B()] = Obj(r.newArgumentsObject(f))
		}
		v, err = r.getIndexed(regs[w.B()], key)
	case bytecode.RApplyArgs:
		b := int(w.B())
		v, err = r.regApplyArgs(f, regs[b], regs[b+1], regs[b+2], &regs[w.C()])
	case bytecode.RAddProp, bytecode.RAddPropI:
		b := Int32(w.SB())
		if w.Op() == bytecode.RAddProp {
			b = regs[w.B()]
		}
		return Undefined, r.regAddProp(cl, c, w, regs[w.A()], b)
	case bytecode.RNewArrayK:
		// The tree's shared literal (ztree_a0shared.go): the site keeps the
		// array of the constants, and each array shares its elements until
		// one is written.
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
		v = Obj(sharedShare(r, s, t))
	case bytecode.RAddI:
		v, err = r.regBinImm(bytecode.OpAdd, regs[w.B()], w.SC())
	case bytecode.RSubI:
		v, err = r.regBinImm(bytecode.OpSub, regs[w.B()], w.SC())
	case bytecode.RJLt, bytecode.RJNLt, bytecode.RJLe, bytecode.RJNLe, bytecode.RJGt, bytecode.RJNGt, bytecode.RJGe, bytecode.RJNGe,
		bytecode.RJLtI, bytecode.RJNLtI, bytecode.RJLeI, bytecode.RJNLeI, bytecode.RJGtI, bytecode.RJNGtI, bytecode.RJGeI, bytecode.RJNGeI,
		bytecode.RJEq, bytecode.RJNe, bytecode.RJEqI, bytecode.RJNeI, bytecode.RJNull, bytecode.RJNotNull,
		bytecode.RJLtU, bytecode.RJNLtU, bytecode.RJLeU, bytecode.RJNLeU:
		k := regJumpCmp[w.Op()]
		b := Int32(w.SB())
		if k.reg {
			b = regs[w.B()]
		} else if w.Op() == bytecode.RJNull || w.Op() == bytecode.RJNotNull {
			b = Null
		} else if k.upval {
			b = cl.upvalues[w.B()].get()
		}
		res, err := r.regCompare(k.op, regs[w.A()], b)
		return Bool(res != k.not), err
	case bytecode.RJmpF, bytecode.RJmpT:
		// A truthiness the loop's regTruth does not answer.
		return Bool(regs[w.A()].Truthy() == (w.Op() == bytecode.RJmpT)), nil
	case bytecode.RJTypeof:
		t := typeofKind[struct{}](regs[w.A()])
		return Bool((t == int(w.X()&^bytecode.RCmpTrue)) == (w.X()&bytecode.RCmpTrue != 0)), nil
	case bytecode.RJSEq, bytecode.RJSNe:
		// A === the loop's regStrictEq does not answer: strings, BigInts.
		return Bool(regs[w.A()].StrictEquals(regs[w.B()]) == (w.Op() == bytecode.RJSEq)), nil
	default:
		panic("register code: no slow path for " + w.Op().String())
	}
	if err == nil {
		regs[w.A()] = v
	}
	return v, err
}

// regBinImm is bin_imm's slow path.
func (r *Runtime) regBinImm(op bytecode.Op, a Value, imm int32) (Value, error) {
	if a.IsNumber() {
		x, y := a.Number(), imm
		switch op {
		case bytecode.OpAdd:
			return Float(x + float64(y)), nil
		case bytecode.OpSub:
			return Float(x - float64(y)), nil
		case bytecode.OpMul:
			return Float(x * float64(y)), nil
		}
		xi := int32(x)
		if float64(xi) != x {
			xi = jsnum.ToInt32(x)
		}
		switch op {
		case bytecode.OpBitAnd:
			return Int32(xi & y), nil
		case bytecode.OpBitOr:
			return Int32(xi | y), nil
		case bytecode.OpBitXor:
			return Int32(xi ^ y), nil
		case bytecode.OpShl:
			return Int32(xi << (uint32(y) & 31)), nil
		case bytecode.OpShr:
			return Int32(xi >> (uint32(y) & 31)), nil
		default:
			return Uint32(uint32(xi) >> (uint32(y) & 31)), nil
		}
	}
	return r.binImm(op, a, Int32(imm))
}

// regBinary is a binary operator the loop has no case of its own for.
func (r *Runtime) regBinary(op bytecode.Op, a, b Value) (Value, error) {
	switch op {
	case bytecode.OpDiv, bytecode.OpMod, bytecode.OpPow:
		if a.IsNumber() && b.IsNumber() {
			if op == bytecode.OpDiv {
				return Float(a.Number() / b.Number()), nil
			}
			return Float(numericOp(op, a.Number(), b.Number())), nil
		}
		return r.arith(op, a, b)
	case bytecode.OpBitAnd, bytecode.OpBitOr, bytecode.OpBitXor, bytecode.OpShl, bytecode.OpShr, bytecode.OpUShr:
		if a.IsNumber() && b.IsNumber() {
			fa, fb := a.Number(), b.Number()
			x, y := int32(fa), int32(fb)
			if float64(x) != fa {
				x = jsnum.ToInt32(fa)
			}
			if float64(y) != fb {
				y = jsnum.ToInt32(fb)
			}
			switch op {
			case bytecode.OpBitAnd:
				return Int32(x & y), nil
			case bytecode.OpBitOr:
				return Int32(x | y), nil
			case bytecode.OpBitXor:
				return Int32(x ^ y), nil
			case bytecode.OpShl:
				return Int32(x << (uint32(y) & 31)), nil
			case bytecode.OpShr:
				return Int32(x >> (uint32(y) & 31)), nil
			}
			return Uint32(uint32(x) >> (uint32(y) & 31)), nil
		}
		return r.bitwise(op, a, b)
	}
	res, err := r.regCompare(op, a, b)
	return Bool(res), err
}

// regCompare is a comparison: an equality or a relational operator.
func (r *Runtime) regCompare(op bytecode.Op, a, b Value) (bool, error) {
	switch op {
	case bytecode.OpStrictEq:
		return a.StrictEquals(b), nil
	case bytecode.OpStrictNe:
		return !a.StrictEquals(b), nil
	case bytecode.OpEq, bytecode.OpNe:
		eq, err := r.looseEquals(a, b)
		return eq == (op == bytecode.OpEq), err
	}
	if a.IsNumber() && b.IsNumber() {
		return compareFloats(op, a.Number(), b.Number()), nil
	}
	c, err := r.compare(a, b)
	if err != nil {
		return false, err
	}
	return relationalResult(op, c), nil
}

// regStep is ++ or -- of a value that is not a number.
func (r *Runtime) regStep(a Value, delta float64) (Value, error) {
	n, err := r.toNumeric(a)
	if err != nil {
		return Undefined, err
	}
	if n.IsBigInt() {
		return r.arith(bytecode.OpAdd, n, shortBig(int64(delta)))
	}
	return Float(n.Number() + delta), nil
}

// regUnary is a unary operator.
func (r *Runtime) regUnary(op bytecode.Op, a Value) (Value, error) {
	switch op {
	case bytecode.OpNeg:
		if a.IsNumber() {
			return Float(-a.Number()), nil
		}
		return r.negate(a)
	case bytecode.OpPos:
		n, err := r.toNumber(a)
		return Float(n), err
	case bytecode.OpNot:
		return Bool(!regTruthy(a)), nil
	case bytecode.OpTypeOf:
		return Str(r.typeofString(a)), nil
	case bytecode.OpToNumeric:
		return r.toNumeric(a)
	}
	// bit_not.
	if !a.IsNumber() {
		n, err := r.toNumeric(a)
		if err != nil {
			return Undefined, err
		}
		if n.IsBigInt() {
			return bigNot(n), nil
		}
		a = n
	}
	x, err := r.toInt32(a)
	return Int32(^x), err
}

// regElemMiss is the element get_index, set_index and their immediate
// forms read or write where their fast path has found no dense element, as
// the tree's getIndexMiss and setIndexMiss do: a typed array's, from or to
// its buffer as elemAt and setElem read and write it, typedRead and
// typedStore the rest they can; an element appended at an array's length,
// or a hole filled (appendElem, fillHole). False sends what is left -- a
// hole read, a value a typed array converts with code, a getter, a proxy
// -- to the slow path. None of it can throw or run code.
//
// It is the one call of a label get_index and the immediate forms share,
// so that the loop grows by a jump each, and set_index's before its slow
// path, which keeps the code after that case where it was: with the case
// shorter, crypto_am3 was 5.5% slower at every placement. It switches on
// the operation again, as regSlow does, out of the loop.
//
//go:noinline
func regElemMiss(c *regCode, k *regKept, regs *[1 << 16]Value) bool {
	w := regAt(c.code, k.pc-1)
	var obj, key Value
	switch w.Op() {
	case bytecode.RGetIndex, bytecode.RGetIndexI:
		obj, key = regs[w.B()], Value{num: float64(w.C())}
		if w.Op() == bytecode.RGetIndex {
			key = regs[w.C()]
		}
		if obj.IsObject() && obj.object().class == ClassTypedArray {
			// The element read here, as elemAt reads it, rather than
			// through a second call.
			t, ok := obj.object().dataOf().(*typedArrayData)
			i := uint32(key.num)
			if ok && float64(i) == key.num && uint(i) < uint(t.fixedLength) {
				if st := t.st; st != nil && t.end <= len(st.bytes) {
					b, j := st.bytes[t.byteOffset:t.end], int(i)
					var v Value
					switch t.kind {
					case elemInt8:
						v = Int(int(int8(b[j])))
					case elemUint8, elemUint8Clamped:
						v = Int(int(b[j]))
					case elemInt16:
						v = Int(int(int16(binary.LittleEndian.Uint16(b[j<<1:]))))
					case elemUint16:
						v = Int(int(binary.LittleEndian.Uint16(b[j<<1:])))
					case elemInt32:
						v = Int32(int32(binary.LittleEndian.Uint32(b[j<<2:])))
					case elemUint32:
						v = Uint32(binary.LittleEndian.Uint32(b[j<<2:]))
					case elemFloat32:
						v = Float(float64(math.Float32frombits(binary.LittleEndian.Uint32(b[j<<2:]))))
					case elemFloat64:
						v = Float(math.Float64frombits(binary.LittleEndian.Uint64(b[j<<3:])))
					default:
						goto long
					}
					regs[w.A()] = v
					return true
				}
			}
		}
	long:
		if v, ok := elemAt(obj, key); ok {
			regs[w.A()] = v
			return true
		}
		if v, ok := typedRead(obj, key.num); ok {
			regs[w.A()] = v
			return true
		}
		return false
	case bytecode.RSetIndex, bytecode.RSetIndexI:
		obj, key = regs[w.A()], Value{num: float64(w.B())}
		if w.Op() == bytecode.RSetIndex {
			key = regs[w.B()]
		}
		v := regs[w.C()]
		if obj.IsObject() && obj.object().class == ClassTypedArray && v.IsNumber() {
			// A number stored here, as setElem stores it.
			t, ok := obj.object().dataOf().(*typedArrayData)
			i := uint32(key.num)
			if ok && float64(i) == key.num && uint(i) < uint(t.fixedLength) {
				if st := t.st; st != nil && t.end <= len(st.bytes) && !st.immutable {
					b, n, j := st.bytes[t.byteOffset:t.end], v.num, int(i)
					switch t.kind {
					case elemInt8, elemUint8:
						b[j] = byte(toInt32Wrap(n))
					case elemUint8Clamped:
						b[j] = clampUint8(n)
					case elemInt16, elemUint16:
						binary.LittleEndian.PutUint16(b[j<<1:], uint16(toInt32Wrap(n)))
					case elemInt32, elemUint32:
						binary.LittleEndian.PutUint32(b[j<<2:], uint32(toInt32Wrap(n)))
					case elemFloat32:
						binary.LittleEndian.PutUint32(b[j<<2:], math.Float32bits(float32(n)))
					case elemFloat64:
						binary.LittleEndian.PutUint64(b[j<<3:], math.Float64bits(n))
					default:
						return typedStore(obj, key.num, v)
					}
					return true
				}
			}
		}
		if setElem(obj, key, v) || typedStore(obj, key.num, v) {
			return true
		}
		// An element appended at an array's length, or a hole filled,
		// as the tree's setIndexMiss stores them.
		return obj.IsObject() && (appendElem(obj.object(), key.num, v) || fillHole(obj.object(), key.num, v))
	}
	return false
}

// regSetMiss is set_index's miss: on an array, an element appended at its
// length or a hole filled, as the tree's setIndexMiss stores them, where
// setElem would store nothing the loop's fast path has not; on anything
// else, regElemMiss's typed array's element. False sends what is left --
// a shared literal's element, a setter, a proxy -- to the slow path. None
// of it can throw or run code.
//
//go:noinline
func regSetMiss(c *regCode, k *regKept, regs *[1 << 16]Value) bool {
	w := regAt(c.code, k.pc-1)
	if obj := regs[w.A()]; obj.IsObject() && obj.object().class == ClassArray {
		key, v := regs[w.B()], regs[w.C()]
		o := obj.object()
		return appendElem(o, key.num, v) || fillHole(o, key.num, v)
	}
	return regElemMiss(c, k, regs)
}

// regBackEdge is a backward jump's interrupt check, when the budget is
// spent.
func (r *Runtime) regBackEdge(f *frame, c *regCode, k *regKept) error {
	r.backEdges = backEdgeCheckInterval
	f.pc = c.src[k.pc-1] + 1
	if err := r.checkInterruptNow(); err != nil {
		return err
	}
	r.sweepStaleSlots(f.base - int(c.locals) + int(c.numRegs))
	return nil
}

// regAppend is append, a case of runReg's: an append to a local, its
// string grown in place where it alone holds it, as the tree's
// strAppendNode grows it. The case keeps nothing of the instruction across
// the call; this reads it back.
//
//go:noinline
func (r *Runtime) regAppend(f *frame, c *regCode, k *regKept, regs *[1 << 16]Value) error {
	pc := k.pc - 1
	w := regAt(c.code, pc)
	f.pc = c.src[pc] + 1
	v, err := strAppendOp[struct{}](r, regs[w.A()], regs[w.B()])
	if err == nil {
		regs[w.A()] = v
	}
	return err
}

// regCharCodeAt is char_code_at, a case of runReg's: s.charCodeAt(i) of a
// string, its method the built-in read from the cache primProp fills and
// its argument a number, read here as the tree's charCodeAtCall reads it,
// without the method's read on the slow path, the call's site and the
// built-in's conversions; anything else is regCharCodeAtSlow's. The case
// keeps nothing of the instruction across the call; this reads it back.
//
//go:noinline
func (r *Runtime) regCharCodeAt(f *frame, c *regCode, k *regKept, regs *[1 << 16]Value) error {
	pc := k.pc - 1
	w := regAt(c.code, pc)
	if o := regs[w.B()]; o.IsString() {
		key := f.cl.names[c.siteName[w.C()]]
		p := r.proto.str
		if e := &r.primProps[key&63]; e.key == key && e.proto == p && e.shape == p.shape {
			if m := p.vals()[e.idx]; m.IsObject() {
				if fd := m.Object().fn(); fd != nil && fd.elemOp == elemStrCharCodeAt && !fd.bound {
					if a := regs[w.X()]; a.IsNumber() {
						x := a.Number()
						if x != x {
							x = 0
						}
						// ToIntegerOrInfinity truncates toward zero, so any
						// position above -1 and below the length is a code
						// unit's.
						v := Float(math.NaN())
						if s := o.String(); x > -1 && x < float64(s.Len()) {
							v = Int(s.CharCodeAt(int(x)))
						}
						regs[w.A()] = v
						return nil
					}
				}
			}
		}
	}
	return r.regCharCodeAtSlow(f, c, pc, regs)
}

// regIterNext is iter_next: R[A] = the next value of cursor R[B], or a
// jump by SC where it has none, the step at the head of each turn of a
// for-of or a for-in loop. An array's element in its dense storage is
// read here, as the tree's iterNextTree reads it; a generator's cursor is
// stepped as the tree steps it (regIterNextTree); anything else as
// iterNext steps it.
//
//go:noinline
func (r *Runtime) regIterNext(f *frame, c *regCode, k *regKept, regs *[1 << 16]Value) error {
	pc := k.pc - 1
	w := regAt(c.code, pc)
	cur := regs[w.B()]
	var v Value
	var more bool
	var err error
	if st := cursorOf(cur); st != nil && !st.done {
		if a := st.arr; a != nil {
			if i := st.idx; i < int(a.arrayLength()) && i < len(a.elemsOf()) {
				if x := a.elemsOf()[i]; !isHole(x) {
					st.idx++
					regs[w.A()] = x
					return nil
				}
			}
		} else if !st.forIn && r.genNextFn != nil && st.next.ref == unsafe.Pointer(r.genNextFn) {
			f.pc = c.src[pc] + 1
			v, more, err = r.regIterNextTree(f, cur)
			goto stepped
		} else if !st.forIn {
			f.pc = c.src[pc] + 1
			v, more, err = r.regStepNext(st)
			goto stepped
		}
	}
	f.pc = c.src[pc] + 1
	v, more, err = r.iterNext(cur)
stepped:
	if err != nil {
		return err
	}
	if !more {
		k.pc += int(w.SC())
		return nil
	}
	regs[w.A()] = v
	return nil
}

// regLit is regexp_lit: the literal's RegExp made or given back in the
// window's receiver and its method in the callee's register -- the
// built-in, unread, where reading it would find it running nothing to
// find it (regexpLitMethod), as the tree's literal call does, and read
// otherwise, at the instruction's position or, for one that calls, at the
// stack form's instruction of the literal (Aux) -- and, with RLitCall,
// the method called with the window's one argument as a call_method_w
// marked RMarkLitRecv calls it.
//
//go:noinline
func (r *Runtime) regLit(f *frame, c *regCode, k *regKept, regs *[1 << 16]Value) error {
	pc := k.pc - 1
	w := regAt(c.code, pc)
	f.pc = c.src[pc] + 1
	a, x, kc := w.A(), w.X(), uint32(w.C())
	lit, err := r.newRegExpLiteral(&f.cl.fn.Constants[kc])
	if err != nil {
		return err
	}
	regs[a+1] = lit
	test := x&bytecode.RLitTest != 0
	if m := r.regexpLitMethod(test); m != nil {
		if x&(bytecode.RLitCall|bytecode.RLitDead) == bytecode.RLitCall && !regs[a+2].IsObject() {
			// The built-in on the literal given a primitive, called
			// at once, and the RegExp given back (RMarkLitRecv).
			v, err := r.callRest(m, m.fn(), lit, regs[a+2:a+3])
			if err != nil {
				return err
			}
			if !test || r.regexpLitMethod(false) != nil {
				// test calls exec, which must be the built-in too.
				r.regexpLiteralDone(&f.cl.fn.Constants[kc], lit.Object())
			}
			regs[w.B()] = v
			return nil
		}
		regs[a] = Obj(m)
	} else {
		name := atomExec
		if test {
			name = r.atoms.intern("test")
		}
		if x&bytecode.RLitCall != 0 {
			f.pc = c.rc.Aux[pc] + 1
		}
		v, err := r.getValueProp(lit, name)
		if err != nil {
			return err
		}
		regs[a] = v
		f.pc = c.src[pc] + 1
	}
	if x&bytecode.RLitCall == 0 {
		return nil
	}
	m := uint8(bytecode.RMarkLitRecv)
	if x&bytecode.RLitDead != 0 {
		m |= bytecode.RMarkDead
	}
	v, err := r.regMarkedCall(f, c, f.base-int(c.locals)+int(a), 1, pc, m, kc)
	if err != nil {
		return err
	}
	regs[w.B()] = v
	return nil
}

// regCallSlow is the call of instruction w at pc, its window at base,
// that its site's cache does not answer.
// regMarkedCall is the call of call_method_w pc, its window at base, of
// argc arguments, which its Aux, aux, marks, and what the tree's nodes of
// such calls do around it, x its operand (Aux). RMarkLitArg makes the
// call's first argument,
// the literal; a literal's RegExp is given back where the call keeps
// nothing of it and runs nothing of the script's (regexpLiteralDone):
// exec or test, the realm's own, given a primitive, of the literal's
// RegExp (RMarkLitRecv); replace, split or match searching with it
// directly (regexpArgCall). A call of the realm's exec or replace whose
// value nothing reads but its truth (RMarkDead) tells the function so, as
// deadExec's node does (regDeadCall); one of exec whose value goes to a
// local that keeps it only as execReuseOf says (RMarkReuse) has its array
// made in the memory of the last (reuseCallRest).
func (r *Runtime) regMarkedCall(f *frame, c *regCode, base, argc, pc int, m uint8, x uint32) (Value, error) {
	k := x
	args := r.stack[base+2 : base+2+argc]
	if m&bytecode.RMarkLitArg != 0 {
		lit, err := r.newRegExpLiteral(&f.cl.fn.Constants[k])
		if err != nil {
			return Undefined, err
		}
		args[0] = lit
	}
	fv, recv := r.stack[base], r.stack[base+1]
	if !fv.IsObject() {
		return r.regCall(fv, recv, args)
	}
	o := fv.Object()
	var rx *Object
	if m&bytecode.RMarkLitArg != 0 {
		if r.regexpArgCall(o, recv, args) {
			rx = args[0].Object()
		}
	} else if m&bytecode.RMarkLitRecv != 0 && recv.IsObject() && (argc == 0 || !args[0].IsObject()) {
		if exec := r.regexpLitMethod(false); exec != nil && (o == exec || o == r.regexpLitMethod(true)) {
			rx = recv.Object()
		}
	}
	var v Value
	var err error
	switch {
	case m&bytecode.RMarkReuse != 0:
		if site := regReuseAt(c, pc); site != nil && o == r.regexpExecFn {
			wb := f.base - int(c.locals)
			v, err = reuseCallRest[struct{}](r, site, r.stack[wb:wb+int(c.numRegs)], o, o.fn(), recv, args)
		} else {
			v, err = r.regMarkedRest(o, recv, args)
		}
	case m&bytecode.RMarkDead != 0 && (o == r.regexpExecFn || o == r.strReplaceFn):
		v, err = r.regDeadCall(o, recv, args)
	default:
		v, err = r.regMarkedRest(o, recv, args)
	}
	if err != nil {
		return Undefined, err
	}
	if rx != nil {
		r.regexpLiteralDone(&f.cl.fn.Constants[k], rx)
	}
	return v, nil
}

// regApplyArgs is apply_args: target.apply(this, the frame's arguments),
// which the built-in apply makes with the arguments as passed, no object
// made; any other apply is given one, made once and kept in the arguments
// binding's register, slot, so that every call is given the same one.
func (r *Runtime) regApplyArgs(f *frame, target, apply, this Value, slot *Value) (Value, error) {
	if !apply.IsObject() || apply.Object() != r.applyFn {
		o := *slot
		if !o.IsObject() {
			o = Obj(r.newArgumentsObject(f))
			*slot = o
		}
		return r.call(apply, target, []Value{this, o})
	}
	args := f.args
	if f.cl.fn.MappedArguments {
		n := min(f.cl.fn.ParamCount, len(args), len(f.locals))
		for i := 0; i < n; i++ {
			if !f.locals[i].sameBits(args[i]) {
				// A parameter has been assigned, which a mapped object
				// would show.
				args = append([]Value(nil), args...)
				copy(args, f.locals[:n])
				break
			}
		}
	}
	return r.applyCall(target, this, args)
}

// regWindow is the frame's registers from base. The register form's
// verifier proves that every register an instruction names is below the
// frame's count, which the frame has, so the array's length is never
// reached past them.
//
//go:nocheckptr
func regWindow(stack []Value, base int) *[1 << 16]Value {
	return (*[1 << 16]Value)(unsafe.Add(unsafe.Pointer(unsafe.SliceData(stack)), uintptr(base)*unsafe.Sizeof(Value{})))
}

// regAt is the instruction at pc, which the verifier proves is in the code.
func regAt(code unsafe.Pointer, pc int) bytecode.RInstr {
	return *(*bytecode.RInstr)(unsafe.Add(code, uintptr(pc)*8))
}

// The kinds of regCode's leaf.
const (
	regLeafNone = iota
	regLeafPure
	regLeafThis
	// regLeafCode is run as its register code by regLeafRun, with no
	// frame, at the call site.
	regLeafCode
	// regLeafArg returns its parameter leafArg, and regLeafConst the
	// constant leafVal, undefined for nothing: regCallW answers them from
	// the window and the code.
	regLeafArg
	regLeafConst
)

// regTrace prints why a function has no register code.
var regTrace = os.Getenv("QJS_REGTRACE") != ""

// regDump prints each function's register code as it is made.
var regDump = os.Getenv("QJS_REGDUMP") != ""

// regTruth is ToBoolean of what needs no call -- a boolean, an object, a
// number -- and ok false for anything else.
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

// regWordKind reports whether a value of tag bits is one that is === only
// to a value of the same words: undefined, null, a boolean, a symbol or an
// object.
func regWordKind(bits uint64) bool {
	return bits-tagBase < heapBigBits-tagBase &&
		1<<(bits&0xFF)&(1<<KindUndefined|1<<KindNull|1<<KindBool|1<<KindSymbol|1<<KindObject) != 0
}

// regTruthy is ToBoolean, a boolean's without the call.
func regTruthy(v Value) bool {
	return math.Float64bits(v.num) == trueBits || math.Float64bits(v.num) != falseBits && v.Truthy()
}

// regJumpOff is a jump's offset: BC for a jump of a register's
// truthiness or none, SC for a compare-and-branch.
func regJumpOff(w bytecode.RInstr) int32 {
	switch w.Op() {
	case bytecode.RJmp, bytecode.RJmpF, bytecode.RJmpT:
		return int32(w.BC())
	}
	return w.SC()
}

// regNaN is the canonical NaN as a Value, in a variable rather than a
// constant: Go's loop-invariant pass takes a constant out of the loop into a
// register of its own, one for each use, which the loop then saves and
// restores around every case that calls anything.
var regNaN = Value{num: math.Float64frombits(canonicalNaN)}

// regNum is Float, its NaN read from regNaN.
func regNum(f float64) Value {
	if f != f {
		return regNaN
	}
	return Value{num: f}
}

// regInt32 is v as an int32, where it is a number that is one exactly:
// what a bitwise operator's fast path takes without converting.
func regInt32(v Value) (int32, bool) {
	if v.IsNumber() {
		f := v.Number()
		if x := int32(f); float64(x) == f {
			return x, true
		}
	}
	return 0, false
}

// regKept is what the loop keeps in memory across a call: a struct Go does
// not keep in registers (it has an array of more than one element), whose
// address every call the loop makes is given, so that Go cannot take a load
// of it after the call for the value stored before: the loop's pc is then
// not live across the call, and is not saved at every dispatch.
type regKept struct {
	pc  int
	pad [2]int
}
