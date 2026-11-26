package vm

import (
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// The typed table (docs/register-interpreter.md, "The typed table"): the
// loop that runs typed code, its table and its call handler. The file
// sorts after the ordinary table's (reg_table*.go) and reg_tables.go:
// its init copies the ordinary handlers once their inits have written
// them (TestHandlerTables).
//
// Typed code is a template's twin of its ordinary code (closureCode's
// regTyped), entered by call sites alone: a site of typed code caches only
// a callee whose code to run is typed, and any other callee -- and, at a
// site of ordinary code, a typed one -- with regCallCross, which none of
// the hit paths' compares lets through. regCallSlow then enters it by the
// table of its kind, in a code entry of its own (regEnterWTyped,
// regEnterW). Tables switch only there: a code entry runs one kind of
// code, whose native context holds its table throughout, so that a call
// between two typed functions is the ordinary call handler's, its frame
// decoded by the same table, and a return goes back to code of its kind.

// regTypedTable is each operation's handler in typed code, and
// regTypedSlowTable each one's slow function: the ordinary operations
// typed code may hold keep their handlers (bytecode.TypedTableOps) but
// call_w, whose handler enters a typed callee by runRegTyped (rhTCallW);
// every other value is invalid, the verifier proving it absent.
var regTypedTable, regTypedSlowTable [256]regHandler

func init() {
	for op := range regTypedTable {
		regTypedTable[op], regTypedSlowTable[op] = rhInvalid, rhInvalid
	}
	for _, op := range bytecode.TypedTableOps() {
		regTypedTable[op], regTypedSlowTable[op] = regTable[op], regSlowTable[op]
	}
	regTypedTable[bytecode.RCallW] = rhTCallW
}

// runRegTyped is runReg for typed code c: the same loop over the typed
// table, and its native handlers where they run (nativeRunTyped), with
// states of their own (regEnterTyped), whose contexts keep the typed
// table.
func (r *Runtime) runRegTyped(f *frame, c *regCode, start int) (Value, error) {
	if r.stopped != nil {
		return Undefined, r.stopped
	}
	if regNativeBuilt && r.runNative {
		return r.nativeRunTyped(f, c, start)
	}
	st := r.regEnterTyped(f, c)
	regs, code, pc := st.regs, c.code, start
	var w bytecode.RInstr
	for {
		w = regAt(code, pc)
		if pc = regTypedTable[w.Op()](regs, w, pc, st); pc < 0 {
			if pc == regDone {
				break
			}
			pc = ^pc
			w = regAt(code, pc)
			if pc = regTypedSlowTable[w.Op()](regs, w, pc, st); pc < 0 {
				break
			}
		}
	}
	err := st.err
	r.regLeaveTyped(st)
	if err != nil {
		return Undefined, err
	}
	if w.Op() == bytecode.RRet {
		return regs[w.A()], nil
	}
	return Undefined, nil
}

// typedRun is a code entry running typed code: the depth of its frame,
// and the native context running it, whose frames entered natively are
// the depths past it (nativeCtx's depth); nil for the Go table's, which
// runs that frame alone.
type typedRun struct {
	depth int
	ctx   *nativeCtx
}

// typedFrame reports whether the frame at depth i runs typed code: one of
// a typed code entry's. A typed entry runs typed code alone, and a call
// out of it is an entry of its own, so its frames are its depth and those
// its context entered past it.
func (r *Runtime) typedFrame(i int) bool {
	for _, t := range r.typedRuns {
		if i == t.depth || t.ctx != nil && i > t.depth && i <= t.depth+int(t.ctx.depth) {
			return true
		}
	}
	return false
}

// framePC is the pc a stack trace keeps of frame f at depth i: the
// instruction it is at, its saved pc being past it, marked TypedPC where
// the frame runs typed code, whose line table gives its position.
func (r *Runtime) framePC(f *frame, i int) uint32 {
	pc := f.pc
	if pc > 0 {
		pc--
	}
	if f.cl.regTyped != nil && r.typedFrame(i) {
		pc |= bytecode.TypedPC
	}
	return pc
}

// regEnterTyped is regEnter's state for frame f running typed code c,
// from the typed states' list.
func (r *Runtime) regEnterTyped(f *frame, c *regCode) *regState {
	st := r.regFreeTyped
	if st == nil {
		st = &regState{r: r}
	} else {
		r.regFreeTyped = st.next
	}
	st.f, st.c, st.cl = f, c, f.cl
	st.wb = f.base - int(c.locals)
	st.regs = regWindow(r.stack, st.wb)
	r.typedRunStart()
	return st
}

// typedRunStart starts a typed code entry, its frame the runtime's top
// (typedFrame); nativeRunTyped gives it its context. Out of line, so that
// runRegTyped's loop stays as small as runReg's.
//
//go:noinline
func (r *Runtime) typedRunStart() {
	r.typedRuns = append(r.typedRuns, typedRun{depth: r.frameDepth - 1})
}

// regLeaveTyped gives a typed state back, as regLeave gives one, and ends
// its code entry.
func (r *Runtime) regLeaveTyped(st *regState) {
	r.typedRuns = r.typedRuns[:len(r.typedRuns)-1]
	st.f, st.c, st.cl, st.err = nil, nil, nil, nil
	st.next = r.regFreeTyped
	r.regFreeTyped = st
}

// rhTCallW is call_w's handler in typed code: rhCallW's, whose entered
// callee -- typed, as every callee a typed site enters is -- runs by
// runRegTyped. A JIT never runs where typed code does.
func rhTCallW(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	r, f, c, wb := st.r, st.f, st.c, st.wb
	st.kept.pc = pc + 1
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
					return pc + 1
				}
				if e.rc.leaf == regLeafCode {
					if v, ok := r.regLeafRun(e.cl, e.rc, base, int(w.X())); ok {
						regWindow(r.stack, wb)[w.B()] = v
						return pc + 1
					}
					if e.cl.pureMiss >= pureMissLimit {
						// A body that keeps meeting what it cannot run
						// without a frame is entered with one from now on.
						e.kind &^= regCallOut | regCallLeaf
					}
					f.pc = uint32(pc) + 1
					return st.done(r.regCallMiss(f, c, w, wb, pc))
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
						return pc + 1
					}
				}
				f.pc = uint32(pc) + 1
				return st.done(r.regCallMiss(f, c, w, wb, pc))
			}
			f.pc = uint32(pc) + 1
			if e.kind == regCallNative {
				return st.done(r.regNative(e.callee, w, wb))
			}
			if e.kind == regCallMarked {
				v, err := r.regMarkedCall(f, c, base, int(w.X()), pc, c.rc.Marks[pc], regAuxAt(c, pc))
				if err == nil {
					regWindow(r.stack, wb)[w.B()] = v
				}
				return st.done(err)
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
					return st.done(r.throwRangeError("maximum call stack size exceeded"))
				}
				if r.interruptCounter--; r.interruptCounter <= 0 {
					if err := r.checkInterruptNow(); err != nil {
						return st.done(err)
					}
				}
				saved := r.stackTop
				if top > saved {
					r.stackTop = top
					if top > r.stackHigh {
						r.stackHigh = top
					}
				}
				if argc > int(rc.keep) {
					// The arguments past the parameters, which the
					// bindings overwrite: f.arguments reads them.
					r.regKeepWindow(base, int(rc.keep), argc)
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
				var v Value
				var err error
				if jitBuilt && cl.jitEnters {
					// Native code made from the typed code (asmTwin).
					v, err = r.jitRegEnter(nf, cl, rc, base, 0)
				} else {
					v, err = r.runRegTyped(nf, rc, 0)
				}
				nf.regKind = 0
				if rc.ncap != 0 && len(nf.openUpvalues) != 0 {
					regCloseUpvalues(nf)
				}
				r.frameDepth--
				r.stackTop = saved
				if err != nil {
					return st.done(r.regCallWEnd(nf, regFrameCall, err, saved, wb+int(w.B())))
				}
				regWindow(r.stack, wb)[w.B()] = v
				return pc + 1
			}
		} else if regIsObject(&win[0]) && regSameTemplate(e, (*Object)(win[0].ref)) {
			// Another closure of the callee's template, which the cache is
			// now made: the call made again, which the cache answers.
			return pc
		}
	}
	f.pc = uint32(pc) + 1
	return st.done(r.regCallMiss(f, c, w, wb, pc))
}

// regEnterWTyped is regEnterW for typed code rc: a frame that keeps no
// arguments of its own and reads no new.target, run by runRegTyped in a
// code entry of the typed table.
func (r *Runtime) regEnterWTyped(o *Object, cl *closure, rc *regCode, base, argc int) (Value, error) {
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
	f.newTarget = Undefined
	f.regKind = regFrameEnter
	var v Value
	var err error
	if jitBuilt && cl.jitEnters {
		v, err = r.jitRegEnter(f, cl, rc, base, 0)
	} else {
		v, err = r.runRegTyped(f, rc, 0)
	}
	f.regKind = 0
	if len(f.openUpvalues) != 0 {
		regCloseUpvalues(f)
	}
	r.frameDepth--
	r.stackTop = saved
	return v, err
}

// regTypedCode is typed code as the VM runs it: a regCode, its typed
// set, and what only typed code has, past it, so that every other
// function's regCode stays the size it was.
type regTypedCode struct {
	regCode
	// heap is a linked asm.js function's heap, nil where it has no view;
	// asmCl the closure it was linked for, whose template's typed code it
	// is (closureCode.regTyped), nil for the prototype's twins, which any
	// closure of the template enters.
	heap  *asmHeap
	asmCl *closure
	// twin is, in a runtime with the JIT, asmCl's twin whose register
	// code is this typed code (asmTwin): what typed call sites enter and
	// the JIT compiles; nil elsewhere.
	twin *closure
	// floats is the first of rc.Floats, which TFConst's native handler
	// reads, or nil where the code has none.
	floats unsafe.Pointer
}

// typedExt is typed code c's regTypedCode, c.typed set.
func (c *regCode) typedExt() *regTypedCode { return (*regTypedCode)(unsafe.Pointer(c)) }

// newRegTypedCode is the VM's typed code rc of function fn, whose
// ordinary code is g: g's facts of the frame, rc's instructions, no leaf
// -- a typed call is entered with a frame -- and no call in tail position
// taking a frame in place.
func newRegTypedCode(fn *bytecode.Function, rc *bytecode.RegCode, g *regCode) *regCode {
	x := &regTypedCode{}
	x.regCode = regCode{
		code: unsafe.Pointer(unsafe.SliceData(rc.Code)), siteName: rc.SiteName,
		numRegs: int32(rc.NumRegs), locals: g.locals, params: int32(rc.Params), inits: int32(rc.Inits - int(rc.SkipInit)),
		coerceThis: g.coerceThis, wide: true, keep: g.keep, ncap: g.ncap, newTarget: g.newTarget,
		args: g.args, rc: rc, fn: fn, typed: true,
	}
	c := &x.regCode
	if len(rc.Aux) > 0 {
		c.aux = unsafe.Pointer(&rc.Aux[0])
	}
	if len(rc.Floats) > 0 {
		x.floats = unsafe.Pointer(&rc.Floats[0])
	}
	for _, cases := range rc.Switches {
		c.switches = append(c.switches, newRegSwitchTable(cases))
	}
	return c
}
