package vm

import (
	"errors"
	"sync/atomic"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// The register interpreter's tail calls (tail_call_w and
// tail_call_method_w; docs/register-interpreter.md, "Stage 2"), in a file
// that sorts after the package's others but the layout lock's last pad:
// what only a tail call runs moves none of the call path's code. regPlan,
// which refuses a treeCall to a function with a tail call, is here too:
// with the call path grown by the tail calls' checks of its errors, 192
// bytes, its 192 bytes leave regSlow, regIn, regNative and runReg where
// they were before tail calls (weak_map_set, has_own_method_hit and
// array_in 5 to 9% slower at every placement there).

// regCallEnd is regCall's end in an error: where its frame ended in a tail
// call (errTailCall), the chain the call starts made by regTailLoop,
// unless the loop called it (tailLoopDepth). Only regCall's own frame can
// end so: a function with a tail call has no treeCall (regPlan,
// planTreeCall), so callDirect makes its tail calls itself.
//
//go:noinline
func (r *Runtime) regCallEnd(v Value, err error) (Value, error) {
	if err == errTailCall && r.tailLoopDepth != r.frameDepth {
		return r.regTailLoop()
	}
	return v, err
}

// regTailLoop is the loop over the calls a chain of frames ended in
// (errTailCall), each made in place of the last at this depth.
//
//go:noinline
func (r *Runtime) regTailLoop() (Value, error) {
	saved := r.tailLoopDepth
	r.tailLoopDepth = r.frameDepth
	defer func() { r.tailLoopDepth = saved }()
	v, err := Undefined, errTailCall
	for err == errTailCall {
		tc := r.pendingTail
		r.pendingTail = tailCall{}
		// A bound function is resolved to its target here, as run's
		// tailTarget resolves it: called through the built-in, each would
		// make its call a level deeper.
		_, this, args, o, ok, terr := r.tailTarget(tc)
		switch {
		case terr != nil:
			return Undefined, terr
		case ok:
			v, err = r.regCall(Obj(o), this, args)
		default:
			v, err = r.regCall(tc.callee, tc.this, tc.args)
		}
	}
	return v, err
}

// regMissEnd is regCallMiss's end of a call that ended in an error, which
// it returns: where the callee's frame, entered by regEnterW, was given to
// a callee in place (errRegRestart), the frame -- the one past the top, as
// regEnterW left it -- run again, and where it ended in a tail call
// (errTailCall), the call made, its value written to the stack at dst.
//
//go:noinline
func (r *Runtime) regMissEnd(err error, dst int) error {
	if err == errRegRestart {
		u := uint(r.frameDepth)
		return r.regCallWEnd(&r.frames[u/frameBlockSize][u%frameBlockSize], regFrameEnter, err, r.stackTop, dst)
	}
	return r.regCallWEnd(nil, 0, err, 0, dst)
}

// regTailRest is the call a register frame, gone, ended in (errTailCall,
// r.pendingTail), made where the frame's caller would have had its value:
// by regCall, whose loop makes every later call of the chain in place.
func (r *Runtime) regTailRest() (Value, error) {
	tc := r.pendingTail
	r.pendingTail = tailCall{}
	return r.regCall(tc.callee, tc.this, tc.args)
}

// regTailCallW is tail_call_w and tail_call_method_w: call_w and
// call_method_w in tail position, as the stack form makes tail_call. Once
// the stack is tailCallDepth deep, the frame f, an ordinary call's, is
// given to the callee in place (regTailInPlace, errRegRestart) or given up
// (regTail, errTailCall), as the tree's restarts and tailCallNode's frame
// are; otherwise, or where neither can be, the call is regCallW's.
//
//go:noinline
func (r *Runtime) regTailCallW(f *frame, c *regCode, k *regKept, wb int) error {
	if r.frameDepth >= tailCallDepth {
		pc := k.pc - 1
		w := regAt(c.code, pc)
		f.pc = regSrc(c, pc) + 1
		if r.regTailInPlace(f, c, w, wb) {
			return errRegRestart
		}
		if r.regTail(f, w, wb) {
			return errTailCall
		}
	}
	return r.regCallW(f, c, k, wb)
}

// regCallWEnd is regCallW's end of a frame nf, of kind kind, that ended
// in an error,
// which it returns, the frame closed and the stack's top saved again;
// where the frame was given to a callee in place (errRegRestart), the
// frame run again, and where it ended in a tail call (errTailCall), the
// call made, its value written to the stack at dst.
//
//go:noinline
func (r *Runtime) regCallWEnd(nf *frame, kind uint8, err error, saved, dst int) error {
	var v Value
	if err == errRegRestart {
		v, err = r.regRerun(nf, kind, saved)
	}
	if err == errTailCall {
		v, err = r.regTailRest()
	}
	if err != nil {
		return err
	}
	r.stack[dst] = v
	return nil
}

// regRerun runs again frame f, of kind kind, which its caller has closed
// and whose stack top was saved, where a tail call has given it to a callee
// in place (regTailTo): opened again at the same depth with the callee's
// registers, run by regRestartRun, and closed.
func (r *Runtime) regRerun(f *frame, kind uint8, saved int) (Value, error) {
	r.frameDepth++
	if top := f.base + int(r.regRestart.numRegs); top > r.stackTop {
		r.stackTop = top
		if top > r.stackHigh {
			r.stackHigh = top
		}
	}
	f.regKind = kind
	v, err := r.regRestartRun(f)
	f.regKind = 0
	r.frameDepth--
	r.stackTop = saved
	return v, err
}

// errRegRestart is how a call in tail position tells runReg's caller that
// its frame is now the callee's (regTailInPlace), to run from its first
// instruction with r.regRestart's code (regRestartRun): the loop leaves
// through its fail tail, which regCatch leaves to the caller.
//
// regRestartRun runs frame f again with the code a tail call gave it, as
// many times as its tail calls give it another. Each is a call, counted
// for the host's interrupt as regEnterW counts one: a loop of calls in
// tail position is stopped as a loop is.
//
//go:noinline
func (r *Runtime) regRestartRun(f *frame) (Value, error) {
	v, err := Undefined, errRegRestart
	for err == errRegRestart {
		rc := r.regRestart
		r.regRestart = nil
		if r.interruptCounter--; r.interruptCounter <= 0 {
			if err = r.checkInterruptNow(); err != nil {
				break
			}
		}
		v, err = r.runReg(f, rc, 0)
	}
	// The captures of the code the frame was given, which its caller
	// closes only by the first code's.
	if len(f.openUpvalues) != 0 {
		regCloseUpvalues(f)
	}
	return v, err
}

var errRegRestart = errors.New("register frame restarted")

// regTailInPlace is a call in tail position, the stack deep, of a function
// of this realm whose register code is of the form's own layout and needs
// nothing of its surroundings: the frame f, regCallW's or regEnterW's, an
// ordinary call's, its arguments kept nowhere, becomes the callee's -- its
// captured bindings closed, the arguments moved to the parameters, the
// rest of the bindings undefined -- as the tree's restarts and swaps of
// frames do (selfRestart, tailSwap), with no unwinding and no frame made.
// It reports whether it did.
func (r *Runtime) regTailInPlace(f *frame, c *regCode, w bytecode.RInstr, wb int) bool {
	win := regWindow(r.stack, wb+int(w.A()))
	if !win[0].IsObject() {
		return false
	}
	o := win[0].Object()
	fd := o.fn()
	if fd == nil || fd.closure == nil || fd.bound || fd.native != nil || fd.closure.realm != r.Realm {
		return false
	}
	cl := fd.closure
	rc := cl.reg
	if rc == nil {
		rc = r.regOf(cl)
	}
	if rc == nil || isClassConstructorKind(cl.fn.Kind) {
		return false
	}
	this := Undefined
	if w.Op().MethodCall() {
		this = win[1]
	}
	if x := fd.extra; x != nil {
		if !fd.arrow || x.lexThisRef != nil || x.rare != nil {
			return false
		}
		this = x.lexThis
	} else if rc.coerceThis && !this.IsObject() {
		if !this.IsNullish() {
			return false
		}
		this = r.globalThis
	}
	return r.regTailTo(f, w, wb, o, cl, rc, this)
}

// regTailTo is regTailInPlace's frame given to o, of closure cl and
// register code rc, called with this, which its caller has found.
func (r *Runtime) regTailTo(f *frame, w bytecode.RInstr, wb int, o *Object, cl *closure, rc *regCode, this Value) bool {
	if f.regKind != regFrameCall && f.regKind != regFrameEnter || !f.newTarget.IsUndefined() ||
		f.thisRef != nil || f.args != nil || f.withScopes != nil || f.evalVars != nil ||
		!rc.wide || rc.suspends || rc.args || rc.locals != 0 || !cl.fn.Strict {
		// A sloppy callee is given a frame of its own: its f.arguments
		// reads its count from its caller's instruction (regLegacyArgs).
		return false
	}
	base := f.base
	if base+int(rc.numRegs) > len(r.stack) {
		return false
	}
	if len(f.openUpvalues) != 0 {
		regCloseUpvalues(f)
	}
	argc := min(int(w.X()), int(rc.params))
	src := wb + int(w.A()) + 2
	nw := regWindow(r.stack, base)
	copy(r.stack[base+2:base+2+argc], r.stack[src:src+argc])
	nw[0], nw[1] = Obj(o), this
	for i := min(argc, int(rc.params)); i < int(rc.params+rc.inits); i++ {
		nw[2+i] = Undefined
	}
	if top := base + int(rc.numRegs); top > r.stackTop {
		r.stackTop = top
		if top > r.stackHigh {
			r.stackHigh = top
		}
	}
	f.cl, f.callee, f.this, f.pc = cl, o, this, 0
	r.regRestart = rc
	return true
}

// regTail is call_w or call_method_w of a call in tail position, as the
// stack form's tail_call makes it once the stack is tailCallDepth deep:
// the frame f, an ordinary call's, is given up, its loop leaving with
// errTailCall and the call pending (r.pendingTail), its arguments copied
// out of the window the callee's frame may reuse. It reports false where
// f is a construct's or shares a derived constructor's this, whose call
// is made the ordinary way.
func (r *Runtime) regTail(f *frame, w bytecode.RInstr, wb int) bool {
	switch f.regKind {
	case regFrameCall:
	default:
		// regEnterW's, regCall's, or a frame the tree or run made, whose
		// runTree leaves with errTailCall (regStmt sets treeTail), as
		// tailCallNode's frame does.
		if !f.newTarget.IsUndefined() || f.thisRef != nil {
			return false
		}
	}
	win := regWindow(r.stack, wb+int(w.A()))
	this := Undefined
	if w.Op().MethodCall() {
		this = win[1]
	}
	r.pendingTail = tailCall{callee: win[0], this: this, args: append([]Value(nil), win[2:2+int(w.X())]...)}
	return true
}

// regPlan gives a closure of a template with register code of the form's
// own layout the tree that runs it as its tree's call, where nothing of its
// surroundings but an arrow's this is to establish, so that callDirect --
// a built-in's callback, a tree's call -- enters the register code at once.
// An arrow's this is the one it captured, which regEnterFrame and
// regDirect give it.
func regPlan(fd *funcData) {
	if fd.treeCall != nil || fd.bound || fd.native != nil || fd.closure == nil {
		return
	}
	t := (*tree)(atomic.LoadPointer(&fd.closure.fn.VMCode))
	if t == nil || t.reg == nil || !t.reg.wide || t.reg.suspends {
		return
	}
	if fd.closure.fn.HasTailCall {
		// A frame that may end in a tail call, whose callers must make
		// it: planTreeCall's treeRest, not a treeCall, whose callers
		// expect none.
		return
	}
	if isClassConstructorKind(fd.closure.fn.Kind) {
		// A class's constructor is only constructed, which construct and
		// runFD make: a call throws, and a derived one's this is bound.
		return
	}
	if fd.closure.fn.HasDirectEval {
		// A frame with a scope of its own for the eval's variables,
		// which runFD makes.
		return
	}
	if x := fd.extra; x != nil && (!fd.arrow || x.lexThisRef != nil || x.rare != nil) {
		return
	}
	regBindPlan(fd, t)
	fd.treeCall = t
}

// regKeepWindow keeps the arguments from parameter params (regCode.keep)
// of the register frame about to be made at base with argc arguments,
// which its bindings or a closure's stores may overwrite in its window, for
// f.arguments (regLegacyArgs), which only WithNodeQuirks has: at the
// frame's depth, with the frame's base and callee, which regLegacyArgs
// checks, in a slice of the runtime's that the next such call at that
// depth reuses. Only a call with more arguments than parameters pays for
// it: regCallW and regCallSlow call it past one compare. A built-in's
// callback (regDirect) keeps all of them, the built-in's
// (regDirectLegacy).
//
//go:noinline
func (r *Runtime) regKeepWindow(base, params, argc int) {
	if !r.nodeQuirks {
		// No f.arguments to read them.
		return
	}
	d := r.frameDepth
	for len(r.regExtras) <= d {
		r.regExtras = append(r.regExtras, regExtra{})
	}
	e := &r.regExtras[d]
	e.base, e.callee, e.all = base, nil, false
	if v := r.stack[base]; v.IsObject() {
		e.callee = v.Object()
	}
	e.vals = append(e.vals[:0], r.stack[base+2+params:base+2+argc]...)
}

// A regExtra is what regKeepWindow kept of a call: the arguments past the
// parameters of the frame at base with callee; or, all set, every argument
// regDirectLegacy's call passed.
type regExtra struct {
	base   int
	callee *Object
	vals   []Value
	all    bool
}

// regDirectLegacy is regDirect's entry of a frame under WithNodeQuirks,
// where f.arguments reads a sloppy function's arguments: a built-in's
// callback, a call through call, apply or a bound function, a tail call
// made by a frame given up -- whose caller's instruction, if any, is not
// the call's. It keeps every argument the call passed at the frame's depth,
// with its base and callee, which regLegacyArgs checks, until the frame
// returns.
//
//go:noinline
func (r *Runtime) regDirectLegacy(o *Object, cl *closure, c *regCode, base, m int, args []Value) (Value, error) {
	if cl.fn.Strict || c.args {
		// No f.arguments, or the arguments kept in the frame's args.
		return r.regEnterW(o, cl, c, base, m, args, Undefined)
	}
	d := r.frameDepth
	for len(r.regExtras) <= d {
		r.regExtras = append(r.regExtras, regExtra{})
	}
	e := &r.regExtras[d]
	e.base, e.callee, e.all = base, o, true
	e.vals = append(e.vals[:0], args...)
	v, err := r.regEnterW(o, cl, c, base, m, args, Undefined)
	if e := &r.regExtras[d]; e.all && e.base == base && e.callee == o {
		clear(e.vals)
		e.vals, e.callee, e.all = e.vals[:0], nil, false
	}
	return v, err
}

// regScopedLegacy is regDirectLegacy for a frame regRunFD enters: every
// argument kept for f.arguments (regLegacyArgs) while the frame runs.
func (r *Runtime) regScopedLegacy(o *Object, cl *closure, c *regCode, base, m int, args []Value, newTarget Value, thisRef *thisBinding, with []*Object, evalVars *Object) (Value, error) {
	d := r.frameDepth
	for len(r.regExtras) <= d {
		r.regExtras = append(r.regExtras, regExtra{})
	}
	e := &r.regExtras[d]
	e.base, e.callee, e.all = base, o, true
	e.vals = append(e.vals[:0], args...)
	v, err := r.regEnterScoped(o, cl, c, base, m, args, newTarget, thisRef, with, evalVars)
	if e := &r.regExtras[d]; e.all && e.base == base && e.callee == o {
		clear(e.vals)
		e.vals, e.callee, e.all = e.vals[:0], nil, false
	}
	return v, err
}

// regExtrasLetGo lets go of the arguments regKeepWindow kept for calls that
// have returned, deeper than any frame now (clearReturnedFrames), keeping
// the slices' room.
func (r *Runtime) regExtrasLetGo() {
	for i := r.frameDepth; i < len(r.regExtras); i++ {
		e := &r.regExtras[i]
		clear(e.vals[:cap(e.vals)])
		e.vals, e.callee, e.all = e.vals[:0], nil, false
	}
}

// regLegacyArgs is the arguments f.arguments reads of register frame f, at
// depth i, made by regCallW or regEnterW, which keep no slice of them: the
// count from its caller's call instruction -- the one call of the caller's
// register code at the stack form's instruction its pc names, which is
// where a call sets it -- the parameters from the window, as they are now,
// and the arguments past them from what regKeepWindow kept. A frame
// regDirectLegacy entered has every argument kept, its parameters read
// from the window as well. It reports false where the caller runs no
// register code or names no one call: a construct.
func (r *Runtime) regLegacyArgs(i int, f *frame) ([]Value, bool) {
	if i < len(r.regExtras) && f.cl.reg != nil {
		if e := &r.regExtras[i]; e.all && e.base == f.base && e.callee == f.callee {
			args := append([]Value(nil), e.vals...)
			for k := 0; k < len(args) && k < int(f.cl.reg.keep); k++ {
				args[k] = r.stack[f.base+2+k]
			}
			return args, true
		}
	}
	if i == 0 {
		return nil, false
	}
	caller := r.frameAt(i - 1)
	if caller.cl == nil || caller.cl.reg == nil || caller.pc == 0 || f.cl.reg == nil {
		return nil, false
	}
	cc := caller.cl.reg
	argc := -1
	for pc, s := range cc.src {
		if s != caller.pc-1 {
			continue
		}
		switch w := regAt(cc.code, pc); w.Op() {
		case bytecode.RCallW, bytecode.RCallMW, bytecode.RCallWT, bytecode.RCallMWT:
			if argc >= 0 {
				return nil, false
			}
			argc = int(w.X())
		}
	}
	if argc < 0 {
		return nil, false
	}
	params := int(f.cl.reg.keep)
	args := make([]Value, argc)
	for k := 0; k < argc && k < params; k++ {
		args[k] = r.stack[f.base+2+k]
	}
	if argc > params {
		var extra []Value
		if i < len(r.regExtras) {
			if e := &r.regExtras[i]; e.base == f.base && e.callee == f.callee {
				extra = e.vals
			}
		}
		for k := params; k < argc; k++ {
			args[k] = Undefined
			if k-params < len(extra) {
				args[k] = extra[k-params]
			}
		}
	}
	return args, true
}
