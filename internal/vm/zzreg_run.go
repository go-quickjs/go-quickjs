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

func init() { regTier.Store(os.Getenv("QJS_REG") != "") }

// SetRegTier turns the register interpreter on or off for functions not yet
// run, for tests that compare it with the other tiers.
func SetRegTier(on bool) { regTier.Store(on) }

// regsBuilt counts the functions given register code.
var regsBuilt atomic.Int64

// RegsBuilt is how many functions have been given register code, for tests
// that check it ran.
func RegsBuilt() int64 { return regsBuilt.Load() }

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
	// reuse is the sites of the code's call_reuse instructions whose
	// arrays may be made again, by pc (regReuseSites).
	reuse map[int32]*execReuseSite
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

// firstForm is what firstTree builds for a function: its register code,
// run by a tree of one node, where the register interpreter is on and the
// form covers it, and its tree otherwise. With the tree tier off there is
// neither: the stack interpreter runs everything, as it does without the
// register interpreter.
func firstForm(fn *bytecode.Function) *tree {
	if regTier.Load() && treeTier.Load() {
		if t := regTreeOf(fn); t != nil {
			return t
		}
	}
	return buildTree(fn)
}

// regTreeOf translates fn into register code and returns the tree that runs
// it, or nil.
func regTreeOf(fn *bytecode.Function) *tree {
	if rc := fn.Reg; rc != nil && rc.Wide {
		return regTreeWide(fn, rc)
	}
	if regTrace {
		println("reg: not", fn.Name, "(no register code)")
	}
	return nil
}

// regTreeWide is the tree that runs fn's register code rc, which the
// register backend compiled from its AST.
func regTreeWide(fn *bytecode.Function, rc *bytecode.RegCode) *tree {
	regsBuilt.Add(1)
	if regDump {
		println("reg:", fn.Name)
		println(rc.String())
	}
	ncap := 0
	for _, l := range fn.Locals {
		if l.Captured {
			ncap++
		}
	}
	c := &regCode{
		code: unsafe.Pointer(unsafe.SliceData(rc.Code)), src: rc.SrcPC, siteName: rc.SiteName,
		numRegs: int32(rc.NumRegs), params: int32(rc.Params), inits: int32(rc.Inits),
		coerceThis: fn.CoerceThis, rc: rc, fn: fn, wide: true, ncap: int32(ncap), newTarget: fn.UsesNewTarget, args: rc.UsesArguments || rc.ForwardsArguments,
	}
	if rc.LocalReg != nil {
		c.reuse = regReuseSites(fn, rc)
	}
	switch {
	case fn.Leaf == bytecode.LeafPure && !fn.PureCalls && regLeafRunnable(rc) && regLeafOps(fn, rc) != nil:
		// A short straight body of numbers and upvalues is run as its
		// register code without a frame (regLeafRun).
		c.leaf, c.leafOps = regLeafCode, regLeafOps(fn, rc)
	case fn.Leaf == bytecode.LeafPure && !fn.PureCalls && !pureByTree(fn):
		// A longer body runs faster as code than in the frameless
		// evaluator, as the tree tier finds (pureByTree).
		c.leaf = regLeafPure
	case fn.Leaf != bytecode.LeafNone && fn.Leaf != bytecode.LeafPure:
		c.leaf = regLeafThis
	}
	if len(rc.Code) > 0 {
		// A body that returns a parameter, a constant or nothing first
		// thing -- what follows is unreachable: a call's answer is read
		// from its window or the code, with no frame and no evaluator,
		// whatever its this, which the body never reads.
		switch w := rc.Code[0]; w.Op() {
		case bytecode.RRet:
			if a := int(w.A()); a >= 2 && a < 2+int(rc.Params) {
				c.leaf, c.leafArg = regLeafArg, int32(a-2)
			}
		case bytecode.RRetUndef:
			c.leaf, c.leafVal = regLeafConst, Undefined
		default:
			if v, ok := regConstOf(fn, w); ok && len(rc.Code) > 1 {
				if ret := rc.Code[1]; ret.Op() == bytecode.RRet && ret.A() == w.A() {
					c.leaf, c.leafVal = regLeafConst, v
				}
			}
		}
	}
	for _, cases := range rc.Switches {
		c.switches = append(c.switches, newRegSwitchTable(cases))
	}
	t := &tree{reg: c}
	t.blocks = []tblock{{body: []tstmt{regStmt(c)}, next: regNext}}
	return t
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

// regConstOf is the value instruction w loads, where it loads a constant
// that is the same in every realm and runtime: a number, a boolean, null or
// undefined.
func regConstOf(fn *bytecode.Function, w bytecode.RInstr) (Value, bool) {
	switch w.Op() {
	case bytecode.RLoadInt:
		return Int32(int32(w.BC())), true
	case bytecode.RLoadUndef:
		return Undefined, true
	case bytecode.RLoadNull:
		return Null, true
	case bytecode.RLoadTrue:
		return True, true
	case bytecode.RLoadFalse:
		return False, true
	case bytecode.RLoadConst:
		if k := int(w.BC()); k < len(fn.Constants) && fn.Constants[k].Kind == bytecode.ConstNumber {
			return Float(fn.Constants[k].Num), true
		}
	}
	return Undefined, false
}

// regTrace prints why a function has no register code.
var regTrace = os.Getenv("QJS_REGTRACE") != ""

// regDump prints each function's register code as it is made.
var regDump = os.Getenv("QJS_REGDUMP") != ""

// regStmt is the one node of a register function's tree: it runs the frame
// a call through runFD or callTree has made as register code. A frame of
// the few calls the code cannot run in -- an arrow sharing a derived
// constructor's this, a function made inside a with or beside an eval -- is
// run by the stack interpreter instead.
func regStmt(c *regCode) tstmt {
	return func(t *tctx) {
		r, f := t.r, t.f
		if c.wide && f.thisRef == nil && f.withScopes == nil && f.evalVars == nil {
			if f.cl.reg == nil {
				f.cl.reg = c
			}
			if f.callee != nil {
				if fd := f.callee.fn(); fd != nil {
					regPlan(fd)
				}
			}
			v, err := r.regEnterFrame(f, c)
			if err != nil {
				t.throw(err)
			}
			t.ret = v
			return
		}
		if c.wide || f.thisRef != nil || f.withScopes != nil || f.evalVars != nil {
			v, err := r.execute(f)
			if err != nil {
				t.throw(err)
			}
			t.ret = v
			return
		}
		// The frame was made for the stack form's slots; the register form
		// has a few more of its own.
		base := f.base - int(c.locals)
		if top := base + int(c.numRegs); top > r.stackTop {
			if top > len(r.stack) {
				t.throw(r.throwRangeError("maximum call stack size exceeded"))
			}
			r.stackTop = top
			if top > r.stackHigh {
				r.stackHigh = top
			}
		}
		v, err := r.runReg(f, c)
		if err != nil {
			t.throw(err)
		}
		t.ret = v
	}
}

func regNext(*tctx) int { return -1 }

// regEnterFrame runs the frame f, which runFD or callTree made for the stack
// form, as register code of the form's own layout: its window is made at the
// top of the stack, the function, this and the arguments copied in.
func (r *Runtime) regEnterFrame(f *frame, c *regCode) (Value, error) {
	if regReportOn {
		regNoteCross(regCrossEnter, c.fn, "")
	}
	base := r.stackTop
	top := base + int(c.numRegs)
	if top > len(r.stack) {
		return Undefined, r.throwRangeError("maximum call stack size exceeded")
	}
	r.stackTop = top
	if top > r.stackHigh {
		r.stackHigh = top
	}
	w := r.stack[base:top:top]
	w[0] = Undefined
	w[1] = f.this
	if f.callee != nil {
		w[0] = Obj(f.callee)
		// An arrow's this is the one it captured, whichever way it is
		// called (regPlan).
		if fd := f.callee.fn(); fd != nil && fd.arrow {
			w[1] = fd.extra.lexThis
		}
	}
	p := int(c.params)
	m := min(p, len(f.args))
	for i, a := range f.args[:m] {
		w[2+i] = a
	}
	for i := 2 + m; i < 2+p+int(c.inits); i++ {
		w[i] = Undefined
	}
	saved, savedLocals := f.base, f.locals
	f.base = base
	if c.args {
		f.locals = w[2 : 2+p : 2+p]
	}
	v, err := r.runReg(f, c)
	f.base, f.locals = saved, savedLocals
	return v, err
}

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

// runReg runs frame f as register code c and returns what it returns.
//
// A case that finishes its instruction continues the loop; one that can
// fail sets the frame's pc to the stack form's instruction first, which is
// what a stack trace or an error's position reads, and returns the error.
// A frame has no handler yet: the backend compiles no function with a try
// statement.
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
				return Undefined, r.regThrowDeadZone(f, c, pc)
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
				return Undefined, err
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
				return Undefined, err
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
				return Undefined, err
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
				return Undefined, err
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
				return Undefined, err
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
			return Undefined, r.regThrow(f, c, pc, regs)
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
				return Undefined, err
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
				return Undefined, err
			}
			pc = kept.pc
		// Every value of the switch is a case, so that it is a jump table
		// that needs no range check: Go makes one only of a switch whose
		// cases are dense, and the mask tells it the value is below 128. A
		// new operation takes its value out of this list, or the switch does
		// not compile: the values the translation reference's operations
		// had, and 127, invalid's.
		case 24, 29, 36, 37, 40, 41, 42, 43:
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
					return Undefined, err
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
			return Undefined, err
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
			return Undefined, err
		}
		pc = kept.pc
		continue
	invalid:
		panic("register code: invalid instruction")
	}
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
