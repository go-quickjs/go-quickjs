package vm

import (
	"slices"
	"sync/atomic"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// The register interpreter's calls and slow paths: what its loop does out
// of line, so that the loop's cases stay small (see zzreg_run.go).

// regCall calls callee with this and args from the register interpreter. A
// function of this realm that has register code, and that a call runs with
// nothing to establish but its frame, is run here directly: its frame made
// here, its registers run by runReg, with no recover and no other Go call
// between the two loops. Anything else is called by callDirect. A frame
// that ended in a tail
// call (errTailCall) has its callee called in its place, in this same call
// of regCall, as run does: a chain of tail calls runs in constant space.
func (r *Runtime) regCall(callee, this Value, args []Value) (Value, error) {
	if !callee.IsObject() {
		return r.callDirect(callee, this, args)
	}
	o := callee.Object()
	fd := o.fn()
	if fd == nil || fd.closure == nil || fd.bound {
		return r.callDirect(callee, this, args)
	}
	cl := fd.closure
	c := cl.reg
	if c == nil {
		c = r.regOf(cl)
	}
	if c == nil || c.wide || cl.realm != r.Realm {
		return r.callDirect(callee, this, args)
	}
	if fd.extra != nil {
		// An arrow whose surroundings are its this alone; anything else a
		// function carries of its surroundings is runFD's to establish.
		x := fd.extra
		if !fd.arrow || x.lexThisRef != nil || x.rare != nil {
			return r.callDirect(callee, this, args)
		}
		this = x.lexThis
	}
	if c.coerceThis && !this.IsObject() {
		if !this.IsNullish() {
			return r.callDirect(callee, this, args)
		}
		this = r.globalThis
	}
	if err := r.tick(); err != nil {
		return Undefined, err
	}
	if r.frameDepth >= r.maxFrames {
		return Undefined, r.throwRangeError("maximum call stack size exceeded")
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
	lc := int(c.locals)
	locals := r.stack[base : base+lc : base+lc]
	m := min(int(c.params), len(locals), len(args))
	//lint:ignore S1001 a loop, not copy
	for i, a := range args[:m] {
		locals[i] = a
	}
	for i := m; i < len(locals); i++ {
		locals[i] = Undefined
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
	f.locals = locals
	f.base = base + lc
	f.pc = 0
	f.this = this
	f.newTarget = Undefined
	f.callee = o
	f.args = args
	if len(f.openUpvalues) != 0 {
		f.openUpvalues = f.openUpvalues[:0]
	}
	if f.thisRef != nil {
		f.thisRef = nil
	}
	if f.withScopes != nil {
		f.withScopes = nil
	}
	if f.evalVars != nil {
		f.evalVars = nil
	}
	if len(f.handlers) != 0 {
		f.handlers = f.handlers[:0]
	}
	if f.native != "" {
		f.native = ""
	}
	if f.savedSP != 0 {
		f.savedSP = 0
	}
	v, err := r.runReg(f, c, 0)
	r.popFrameOf(f, base)
	if err != nil {
		return r.regCallEnd(v, err)
	}
	return v, err
}

// regSrc is the stack form's instruction of register instruction pc, which
// the verifier proves is in the code.
func regSrc(c *regCode, pc int) uint32 {
	return *(*uint32)(unsafe.Add(unsafe.Pointer(unsafe.SliceData(c.src)), uintptr(pc)*4))
}

// regCallMiss is a call rhCallW's cache does not answer: a site's first
// call or a new callee, a body answered without a frame, a frame that keeps
// its arguments, a this to coerce, or a callee of no register code.
//
//go:noinline
func (r *Runtime) regCallMiss(f *frame, c *regCode, w bytecode.RInstr, wb, pc int) error {
	v, err := r.regCallSlow(f, c, w, wb+int(w.A()), pc)
	if err != nil {
		return r.regMissEnd(err, wb+int(w.B()))
	}
	r.stack[wb+int(w.B())] = v
	return nil
}

// regGenCall is a call of o, a generator function or an async function of
// this realm with closure cl, whose window is at base: its object made as
// callClosure makes it, the receiver coerced as there, with no frame, since
// the body does not run until the object is stepped; an async function's
// first step is made at once (runAsync), an error binding its parameters
// rejecting its promise.
func (r *Runtime) regGenCall(o *Object, cl *closure, method bool, base, argc int) (Value, error) {
	if err := r.tick(); err != nil {
		return Undefined, err
	}
	this := Undefined
	if method {
		this = r.stack[base+1]
	}
	if fn := cl.fn; fn.UsesThis && !fn.Strict && !this.IsObject() {
		if !this.IsNullish() {
			w, err := r.toObject(this)
			if err != nil {
				return Undefined, err
			}
			this = Obj(w)
		} else {
			this = r.globalThis
		}
	}
	async := cl.fn.Async
	if async && !cl.fn.Generator && !cl.fn.HasDirectEval {
		// Its first run made as a call, the generator made only where it
		// suspends (regAsyncRun).
		if c := r.regGenOf(cl, o); c != nil && !c.args {
			if fd := o.fn(); fd != nil && fd.extra == nil {
				return r.regAsyncRun(o, cl, c, this, base, argc), nil
			}
		}
	}
	gen, err := r.newGenerator(cl, this, r.stack[base+2:base+2+argc], o, async, Undefined)
	if err != nil {
		if async {
			p := r.newPromise()
			r.rejectPromise(p, thrownValue(err))
			return Obj(p), nil
		}
		return Undefined, err
	}
	if async {
		return r.runAsync(genOf(gen)), nil
	}
	return Obj(gen), nil
}

// regMarkedRest is regMarkedCall's call of o on recv with args: a
// built-in's as callRest makes it, without callDirect's search; anything
// else's as register code calls (regCall).
func (r *Runtime) regMarkedRest(o *Object, recv Value, args []Value) (Value, error) {
	if fd := o.fn(); fd != nil && fd.native != nil {
		return r.callRest(o, fd, recv, args)
	}
	return r.regCall(Obj(o), recv, args)
}

// regMarked reports whether call_method_w pc's Aux, aux, marks a call of o
// that regMarkedCall makes: of anything where the call makes its literal
// argument; of the realm's exec or test on a literal; of its exec or
// replace whose value is dead, or exec's reused at a site execReuseOf has
// (regReuseSites). What regMarkedCall does otherwise is the call
// regNative or rhCallW makes, slower.
func (r *Runtime) regMarked(c *regCode, o *Object, pc int, m uint8) bool {
	switch {
	case m&bytecode.RMarkLitArg != 0:
		return true
	case m&bytecode.RMarkLitRecv != 0:
		return o == r.regexpExecFn || o == r.regexpTestFn
	case m&bytecode.RMarkReuse != 0:
		return o == r.regexpExecFn && regReuseAt(c, pc) != nil
	case m&bytecode.RMarkDead != 0:
		return o == r.regexpExecFn || o == r.strReplaceFn
	}
	return false
}

// regAuxAt is the Aux of the instruction at pc, or 0.
func regAuxAt(c *regCode, pc int) uint32 {
	if pc < len(c.rc.Aux) {
		return c.rc.Aux[pc]
	}
	return 0
}

// regNameIdx is the index in the function's names of op, the name
// operand of the instruction at pc: op itself, or, where it is RNameFar,
// the index past 16 bits FarNames has for the instruction. Only cold
// operations and miss paths read a name.
func regNameIdx(c *regCode, pc int, op uint16) uint32 {
	if op == bytecode.RNameFar {
		return c.rc.FarNames[pc]
	}
	return uint32(op)
}

// regName is the name regNameIdx finds, of closure cl's names.
func regName(cl *closure, c *regCode, pc int, op uint16) Atom {
	return cl.names[regNameIdx(c, pc, op)]
}

// regReuseAt is the reuse site of call_method_w pc, or nil.
func regReuseAt(c *regCode, pc int) *execReuseSite {
	if s := regAt(c.code, pc).C(); int(s) < len(c.reuse) {
		return c.reuse[s]
	}
	return nil
}

// regCallKind is how a call site of register code enters fd's closure, a
// method's call or not: of the form's own layout, not suspending, of this
// realm, neither a class's constructor nor with a direct eval (enter),
// the kind its cache gives it and the this that kind writes -- 0 where
// its this is not one a cache can give, an arrow's of a derived
// constructor, say.
func (r *Runtime) regCallKind(fd *funcData, method bool) (cl *closure, rc *regCode, kind uint8, this Value, enter bool) {
	cl = fd.closure
	if rc = cl.reg; rc == nil {
		rc = r.regOf(cl)
	}
	if rc == nil || !rc.wide || rc.suspends || cl.realm != r.Realm || isClassConstructorKind(cl.fn.Kind) || cl.fn.HasDirectEval {
		return cl, rc, 0, Undefined, false
	}
	if x := fd.extra; x == nil {
		switch {
		case !method:
			kind, this = regCallThis, Undefined
			if rc.coerceThis {
				this = r.globalThis
			}
		case rc.coerceThis:
			kind = regCallObjThis
		default:
			kind = regCallEnter
		}
	} else if fd.arrow && x.lexThisRef == nil && x.rare == nil {
		kind, this = regCallThis, x.lexThis
	}
	return cl, rc, kind, this, true
}

// regCacheCallee makes site e's cache callee o's, of closure cl and code
// rc, entered as kind with this, marked where rhCallW leaves it to
// regCallMiss: a leaf, or a frame that keeps its arguments.
func regCacheCallee(e *regCallCache, o *Object, cl *closure, rc *regCode, this Value, kind uint8) {
	e.callee, e.cl, e.rc, e.this, e.kind = o, cl, rc, this, kind
	if rc.leaf != regLeafNone {
		e.kind |= regCallOut | regCallLeaf
	} else if rc.args {
		e.kind |= regCallOut
	}
}

func (r *Runtime) regCallSlow(f *frame, c *regCode, w bytecode.RInstr, base, pc int) (Value, error) {
	argc := int(w.X())
	method := w.Op().MethodCall()
	callee := r.stack[base]
	if callee.IsObject() {
		o := callee.Object()
		if f.cl.regCalls == nil && c.rc.CallSites > 0 {
			r.regCallsMake(f.cl, c)
		}
		var e *regCallCache
		if calls := f.cl.regCalls; uint(w.C()) < uint(len(calls)) {
			e = &calls[w.C()]
			if !f.cl.regCallsHeld {
				r.regCallsHold(f.cl.closureCode)
			}
		}
		var (
			cl   *closure
			rc   *regCode
			kind uint8
			this Value
		)
		if e != nil && e.callee == o && e.kind == regCallGen {
			return r.regGenCall(o, e.cl, method, base, argc)
		}
		if method && pc < len(c.rc.Marks) {
			// A call its marks say regMarkedCall makes where they are the
			// callee's, and the site then sends it there.
			if m := c.rc.Marks[pc]; m != 0 && r.regMarked(c, o, pc, m) {
				if e != nil {
					*e = regCallCache{callee: o, kind: regCallMarked}
				}
				return r.regMarkedCall(f, c, base, argc, pc, m, regAuxAt(c, pc))
			}
		}
		if e != nil && e.callee == o && e.kind != 0 && e.kind != regCallNative {
			// A callee the site knows, whose call is not rhCallW's alone.
			cl, rc, kind, this = e.cl, e.rc, e.kind&^regCallMarks, e.this
		} else if fd := o.fn(); fd != nil && fd.closure != nil && !fd.bound {
			var enter bool
			cl, rc, kind, this, enter = r.regCallKind(fd, method)
			if fn := cl.fn; !enter && e != nil && fn.Generator != fn.Async && fd.extra == nil && cl.realm == r.Realm {
				// A generator or an async function of this realm: the site
				// makes its object as callClosure makes one (regCallGen),
				// without the generic call's search.
				e.callee, e.cl, e.rc, e.this, e.kind = o, cl, nil, Undefined, regCallGen
				return r.regGenCall(o, cl, method, base, argc)
			}
			if kind != 0 && e != nil {
				regCacheCallee(e, o, cl, rc, this, kind)
			}
		}
		switch kind {
		case regCallThis:
			r.stack[base+1] = this
		case regCallObjThis:
			if !r.stack[base+1].IsObject() {
				// A this to coerce: the generic call's.
				kind = 0
			}
		}
		if kind != 0 {
			// A body that only reads or writes a property of its this, or
			// computes a value calling nothing, is answered without a frame,
			// as callDirect answers it (leafCall, pureCall): its frame would
			// cost more than its body. A pure body that calls is run as
			// register code, faster than the frameless evaluator's calls.
			if leaf := rc.leaf; leaf != regLeafNone {
				if leaf == regLeafPure {
					if cl.pureMiss < pureMissLimit {
						if v, ok := r.pureCall(o.fn(), r.stack[base+1], r.stack[base+2:base+2+argc]); ok {
							return v, nil
						}
					}
					if cl.pureMiss >= pureMissLimit && e != nil && e.callee == o && !rc.args {
						// A body the frameless evaluator has given up on,
						// for good: its calls are entered as frames.
						e.kind &^= regCallOut | regCallLeaf
					}
				} else if this := r.stack[base+1]; leaf == regLeafThis && this.IsObject() {
					if v, ok := r.leafCall(cl, this.Object(), r.stack[base+2:base+2+argc]); ok {
						return v, nil
					}
				}
			}
			if argc > int(rc.keep) && !rc.args {
				// The arguments past the parameters, which the bindings
				// overwrite: f.arguments reads them. A frame that keeps
				// its arguments has them in f.args.
				r.regKeepWindow(base, int(rc.keep), argc)
			}
			return r.regEnterW(o, cl, rc, base, argc, r.stack[base+2:base+2+argc], Undefined)
		}
	}
	if method && callee.IsObject() && callee.Object() == r.callFn {
		// f.call(t, ...): f called with this t in call's frame, without
		// the generic call's search for call and then for f.
		if v, err, ok := r.regCallThrough(f, c, w, base, argc); ok {
			return v, err
		}
		return r.callThrough(r.callFn, r.stack[base+1], r.stack[base+2:base+2+argc])
	}
	if calls := f.cl.regCalls; callee.IsObject() && uint(w.C()) < uint(len(calls)) {
		// A built-in of this realm: the site calls it as callDirect does,
		// without the generic call's search (regNative).
		o := callee.Object()
		if fd := o.fn(); fd != nil && fd.native != nil && !fd.bound && (fd.realm == nil || fd.realm == r.Realm) && o != r.callFn {
			calls[w.C()] = regCallCache{callee: o, kind: regCallNative}
		}
	}
	if regReportOn {
		regCrossOf(regCrossOut, callee)
	}
	this := Undefined
	if method {
		this = r.stack[base+1]
	}
	return r.regCall(callee, this, r.stack[base+2:base+2+argc])
}

// regCallsMake makes the call caches of cl's register code c, at its
// first call or construction.
//
//go:noinline
func (r *Runtime) regCallsMake(cl *closure, c *regCode) {
	cl.regCalls = r.newRegCalls(cl.closureCode, c.rc.CallSites)
	if jitBuilt {
		r.jitRegCountsMake(cl.closureCode, c.rc.CallSites)
	}
}

// regCallThrough is call_method_w's call of the realm's
// Function.prototype.call, f.call(t, ...), its window at base: f, a
// function the site's cache holds marked regCallThrough, entered in the
// window one register on -- f its callee, t its this, the arguments after
// -- as call_w enters the callee its cache holds (regEnterW), without
// call's frame, which a stack trace and a caller passes over (V8 has
// none); and the cache made f's where f is one the call handler enters
// so, the native handler's to enter from then on -- a leaf with a frame,
// as the handler enters one Go answers frameless otherwise, and one that
// keeps its arguments as regEnterW enters it. A built-in of this realm is
// called as the site would call it (regNative), one register on. ok is
// false where the call is callThrough's: no this given, a callee of
// another kind, a this to coerce.
func (r *Runtime) regCallThrough(f *frame, c *regCode, w bytecode.RInstr, base, argc int) (Value, error, bool) {
	calls := f.cl.regCalls
	if argc < 1 || uint(w.C()) >= uint(len(calls)) || !regIsObject(&r.stack[base+1]) {
		return Undefined, nil, false
	}
	e, o := &calls[w.C()], (*Object)(r.stack[base+1].ref)
	if e.callee != o || e.kind&regCallThrough == 0 {
		fd := o.fn()
		switch {
		case fd == nil || fd.bound:
			return Undefined, nil, false
		case fd.native != nil:
			// A built-in of this realm: called as the site would call it
			// (regNative), one register on.
			if fd.realm != nil && fd.realm != r.Realm || o == r.callFn {
				return Undefined, nil, false
			}
			*e = regCallCache{callee: o, kind: regCallNative | regCallThrough}
		case fd.closure == nil || r.nodeQuirks:
			// With Node's quirks, f.arguments reads the arguments as call
			// passed them (regDirectLegacy): callThrough's.
			return Undefined, nil, false
		default:
			cl, rc, kind, this, enter := r.regCallKind(fd, true)
			if !enter || kind == 0 {
				return Undefined, nil, false
			}
			e.callee, e.cl, e.rc, e.this, e.kind = o, cl, rc, this, kind|regCallThrough
		}
	}
	if e.kind == regCallNative|regCallThrough {
		// The built-in's call of the instruction one register on, the
		// arguments one fewer (regNative), its value R[B].
		wb := base - int(w.A())
		ws := bytecode.MakeR(w.Op(), w.X()-1, w.A()+1, w.B(), w.C())
		if err := r.regNative(o, ws, wb); err != nil {
			return Undefined, err, true
		}
		return r.stack[wb+int(w.B())], nil, true
	}
	nb := base + 1
	switch e.kind &^ regCallThrough {
	case regCallThis:
		r.stack[nb+1] = e.this
	case regCallObjThis:
		if !r.stack[nb+1].IsObject() {
			return Undefined, nil, false
		}
	}
	n := argc - 1
	if n > int(e.rc.keep) {
		// The arguments past the parameters, which the bindings
		// overwrite: f.arguments reads them.
		r.regKeepWindow(nb, int(e.rc.keep), n)
	}
	v, err := r.regEnterW(o, e.cl, e.rc, nb, n, r.stack[nb+2:nb+2+n], Undefined)
	return v, err, true
}

// regCallCache is a call site's cache: the function it called last, its
// closure and register code, how it is entered, and the this a call that
// is not a method's gives it.
type regCallCache struct {
	callee *Object
	cl     *closure
	rc     *regCode
	this   Value
	kind   uint8
}

// The kinds of regCallCache: what entering the callee takes of its this.
// regCallOut marks one rhCallW leaves to regCallMiss: a body answered
// without a frame, or a frame that keeps its arguments.
const (
	// regCallEnter is a method's call of a function whose this is the
	// method's receiver as it is.
	regCallEnter = 1 + iota
	// regCallThis writes the cache's this: a plain call's undefined or
	// global object, or an arrow's this.
	regCallThis
	// regCallObjThis is a method's call of a function that coerces its
	// this, entered as it is for an object receiver.
	regCallObjThis

	// regCallNative is a built-in of this realm, which regNative calls.
	regCallNative = 4
	// regCallGen is a generator function of this realm, whose object
	// regGenCall makes.
	regCallGen = 5
	// regCallMarked is the realm's exec or replace at a call_method_w
	// whose Aux marks its value dead or reused, which regMarkedCall
	// calls: the mark costs the site's other callees nothing.
	regCallMarked = 6
	// regCallConstruct is new_w's: a base constructor of this realm with
	// register code, whose objects' room is settled, and that gives its
	// objects nothing but what its body stores (regConstruct), its object
	// made as regCtorThis makes it.
	regCallConstruct = 7

	regCallOut = 8
	// regCallLeaf marks a callee whose body rhCallW may answer without a
	// frame (leafCall, pureCall).
	regCallLeaf = 16
	// regCallThrough marks the cache of a call of the realm's
	// Function.prototype.call, f.call(t, ...), whose callee, closure, code
	// and kind are f's, entered in the window one register on
	// (regCallThrough): no kind's, so that neither rhCallW nor the call
	// handler's hit takes it for a call of f, nor the JIT's feedback, and
	// none a turn's end restores (regCallsRestored).
	regCallThrough = 32
)

// regCallMarks are the marks of a call cache's kind that say how a call
// enters its callee, not what it gives it as this.
const regCallMarks = regCallOut | regCallLeaf

// regIsObject reports whether *v is an object, its tag read as a word.
func regIsObject(v *Value) bool {
	return *(*uint64)(unsafe.Pointer(v)) == objectBits
}

// regClearFrame clears what a frame's last user left that a register
// frame has none of.
//
//go:noinline
func regClearFrame(f *frame) {
	f.thisRef = nil
	f.withScopes = nil
	f.evalVars = nil
	f.native = ""
	f.args = nil
}

// regClosure is closure_w: a closure of the nested function constant BC,
// each upvalue it takes from this frame open on the register the code's
// capture table names for it.
func (r *Runtime) regClosure(f *frame, c *regCode, w bytecode.RInstr, wb int) *Object {
	k := w.BC()
	tmpl := f.cl.consts[k]
	fn := f.cl.fn
	caps := c.rc.Caps[k]
	if c.wide {
		// What an arrow captures of this is the frame's register 1, which
		// the frame record does not keep (regEnterW).
		f.this = r.stack[wb+1]
	}
	if t := tmpl.template(); t.fn.CapturesByValue {
		// What it takes by value copied from its registers, as
		// newClosureIn's newClosureByValue copies it from the slots.
		return regMadeClosure(newClosureByValue[struct{}](r, f, t, caps, wb))
	}
	// The cells newClosureIn reads, kept in the template's code for the next
	// closure; the upvalues this closure is the first to open, one
	// allocation, as newClosureIn's room.
	cells := f.cl.regCells
	if len(cells) < int(c.ncap) {
		cells = make([]*upvalue, c.ncap)
		f.cl.regCells = cells
	}
	var room []upvalue
	ups := tmpl.template().fn.Upvalues
	for i, u := range ups {
		if !u.FromParent {
			continue
		}
		slot := &r.stack[wb+int(caps[i])]
		var up *upvalue
		for _, o := range f.openUpvalues {
			if o.slot == slot {
				up = o
				break
			}
		}
		if up == nil {
			if len(room) == 0 {
				room = make([]upvalue, len(ups)-i)
			}
			up, room = &room[0], room[1:]
			up.slot = slot
			f.openUpvalues = append(f.openUpvalues, up)
		}
		cells[cellOfLocal(fn, int(u.Index))] = up
	}
	o := r.newClosureIn(f, tmpl, cells)
	clear(cells)
	return regMadeClosure(o)
}

// regMadeClosure is closure_w's closure o, its function's code planned
// where it has been built, as makeClosure plans: a function's code is built
// at its first call, which plans the closure called (planCall), so that one
// never called is never compiled (bytecode.RegLater). A leaf's is built as
// it is made: its calls are answered by the stack form's leaf evaluators
// (pureCall, leafCall), which build nothing, until a plan sends them to its
// register code (regPlan, regDirect, regLeafRun).
func regMadeClosure(o *Object) *Object {
	if fd := o.fn(); fd != nil {
		if fn := fd.closure.fn; fn.Leaf != bytecode.LeafNone && fn.Reg != nil &&
			atomic.LoadPointer(&fn.VMCode) == nil {
			firstReg(fn)
		}
		if atomic.LoadPointer(&fd.closure.fn.VMCode) != nil {
			planCall(fd)
			// A pure body's too, which planCall leaves undecided for
			// pureCall: a closure register code makes enters its code at
			// once (regPlan), whose callers -- a built-in's callback above
			// all -- then run the code the JIT learns from and compiles.
			regPlan(fd)
		}
	}
	return o
}

// regRunFD runs what runFD would run -- a call of cl, the callee's
// function data fd, or top-level code, whose callee is nil -- as register
// code entered directly, its frame given what runFD gives one of its
// surroundings: the with objects and an eval's variables of the functions
// around it, a scope for the variables of its own direct eval, and the
// this binding of a derived constructor's construction or an arrow
// sharing one. It reports false, having done nothing, where cl has no
// register code to enter so: a generator's body, and a function with no
// register code (errNoRegCode). A chain of tail calls the frame ends in is made
// here, in its place.
//
//go:noinline
func (r *Runtime) regRunFD(cl *closure, this Value, args []Value, newTarget Value, callee *Object, fd *funcData) (Value, error, bool) {
	c := cl.reg
	if c == nil {
		if c = r.regOf(cl); c == nil {
			return Undefined, nil, false
		}
	}
	if c.suspends {
		return Undefined, nil, false
	}
	fn := cl.fn
	// The this binding super() binds: a derived constructor's
	// construction makes one, holding the object made for it until then,
	// and an arrow written inside one shares it, as runFD's frames do.
	var thisRef *thisBinding
	cell := false
	if fd != nil {
		if fd.arrow {
			if fd.extra != nil {
				thisRef = fd.extra.lexThisRef
			}
		} else if fd.ctorKind == ctorDerived && !newTarget.IsUndefined() {
			if c.cellThis {
				// The runtime's cell for the frame's depth, which nothing
				// keeps past the frame: the construction allocates its
				// object alone.
				d := r.frameDepth
				if d >= len(r.thisCells) {
					r.thisCells = make([]thisBinding, d+d/2+16)
				}
				thisRef = &r.thisCells[d]
				*thisRef = thisBinding{value: this}
				cell = true
			} else {
				thisRef = &thisBinding{value: this}
			}
		}
	}
	if regReportOn {
		regNoteCross(regCrossEnter, fn, "")
	}
	if fn.CoerceThis && !this.IsObject() && newTarget.IsUndefined() {
		if this.IsNullish() {
			this = r.globalThis
		} else {
			o, err := r.toObject(this)
			if err != nil {
				return Undefined, err, true
			}
			this = Obj(o)
		}
	}
	var with []*Object
	var evalVars *Object
	if fd != nil && fd.extra != nil {
		if w := fd.extra.with(); len(w) > 0 {
			with = w[:len(w):len(w)]
		}
		evalVars = fd.extra.evalVars()
	}
	if fn.HasDirectEval {
		// A scope of its own for the variables the eval declares, inside
		// what the functions around it put on the chain, as runFD's.
		evalVars = &Object{class: ClassObject, flags: objExtensible | objEvalVars}
		with = append(with[:len(with):len(with)], evalVars)
	}
	base := r.stackTop
	if base+int(c.numRegs) > len(r.stack) {
		return Undefined, r.throwRangeError("maximum call stack size exceeded"), true
	}
	m := min(int(c.params), len(args))
	w := r.stack[base : base+2+m : base+2+m]
	w[0] = Undefined
	if callee != nil {
		w[0] = Obj(callee)
	}
	w[1] = this
	for i, a := range args[:m] {
		w[2+i] = a
	}
	var v Value
	var err error
	if r.nodeQuirks && !fn.Strict && !c.args {
		// f.arguments reads the arguments as they were passed.
		v, err = r.regScopedLegacy(callee, cl, c, base, m, args, newTarget, thisRef, with, evalVars)
	} else {
		v, err = r.regEnterScoped(callee, cl, c, base, m, args, newTarget, thisRef, with, evalVars)
	}
	if cell {
		// The object is the construction's now, not the cell's to keep.
		thisRef.value = Undefined
	}
	if err == errTailCall && r.tailLoopDepth != r.frameDepth {
		// The chain made here, in constant space; inside regTailLoop's
		// call at this depth, by its loop (regCall).
		v, err = r.regTailLoop()
	}
	return v, err, true
}

// regEnterScoped is regEnterW's entry of a frame regRunFD has made the
// window of, given the surroundings regRunFD found for it.
func (r *Runtime) regEnterScoped(o *Object, cl *closure, rc *regCode, base, argc int, args []Value, newTarget Value, thisRef *thisBinding, with []*Object, evalVars *Object) (Value, error) {
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
		r.argStack = append(r.argStack, args...)
		kept = r.argStack[ka:len(r.argStack):len(r.argStack)]
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
	regClearFrame(f)
	if len(f.handlers) != 0 {
		f.handlers = f.handlers[:0]
	}
	f.withScopes, f.evalVars = with, evalVars
	f.newTarget = newTarget
	// The frame record's this is what super() reads and rebinds
	// (superCall), and what a direct eval's code shares: register 1
	// otherwise, which regSyncThis writes back before either.
	f.this, f.thisRef = r.stack[base+1], thisRef
	f.regKind = regFrameEnter
	if rc.args {
		f.args = kept
		p := int(rc.params)
		f.locals = r.stack[base+2 : base+2+p : base+2+p]
	}
	var v Value
	var err error
	if jitBuilt && cl.jitEnters && thisRef == nil && with == nil && evalVars == nil {
		// Its function's calls go to the JIT, as regEnterW's (jitRegEnter):
		// a frame of nothing more of its surroundings than regEnterW's --
		// no this binding a derived constructor shares, no with or eval
		// scope, which native code does not look in.
		v, err = r.jitRegEnter(f, cl, rc, base, 0)
	} else {
		v, err = r.runReg(f, rc, 0)
	}
	if err == errRegRestart {
		v, err = r.regRestartRun(f)
	}
	f.regKind = 0
	if len(f.openUpvalues) != 0 {
		regCloseUpvalues(f)
	}
	f.withScopes, f.evalVars, f.thisRef = nil, nil, nil
	r.frameDepth--
	r.stackTop = saved
	if a := f.args; a != nil {
		r.argStack = r.argStack[:len(r.argStack)-len(a)]
	}
	return v, err
}

// regLeafRunnable reports whether rc is a body regLeafRun runs: straight
// code to its first return, of at most regLeafMax instructions, each a
// load, a move, an upvalue's read or store or an arithmetic operation of
// numbers, reading neither the callee's register nor its this's, writing
// no parameter's register, making at most regLeafStores stores to
// upvalues and reading none it has stored to. The stores are held until
// the return, so a run that gives up has changed nothing the frame that
// then runs the body could see: the arguments are where they were, and no
// store has been made.
func regLeafRunnable(rc *bytecode.RegCode) bool {
	var stored []uint32
	for i, w := range rc.Code {
		if i >= regLeafMax {
			return false
		}
		switch w.Op() {
		case bytecode.RRet:
			return w.A() >= 2
		case bytecode.RRetUndef:
			return true
		case bytecode.RSetUpval:
			if len(stored) == regLeafStores || w.A() < 2 {
				return false
			}
			stored = append(stored, uint32(w.BC()))
			continue
		case bytecode.RGetUpval:
			if slices.Contains(stored, uint32(w.BC())) {
				return false
			}
		case bytecode.RAdd, bytecode.RSub, bytecode.RMul:
			if w.B() < 2 || w.C() < 2 {
				return false
			}
		case bytecode.RAddI, bytecode.RSubI, bytecode.RMulI, bytecode.RInc, bytecode.RDec, bytecode.RMove:
			if w.B() < 2 {
				return false
			}
		case bytecode.RLoadInt, bytecode.RLoadConst, bytecode.RLoadUndef, bytecode.RLoadNull,
			bytecode.RLoadTrue, bytecode.RLoadFalse:
		default:
			return false
		}
		if int(w.A()) < 2+rc.Params {
			return false
		}
	}
	return false
}

// regLeafStores is how many stores to upvalues a body regLeafRun runs may
// make: they are held until it returns.
const regLeafStores = 4

// regLeafMax is the longest body regLeafRun runs: past it, a frame costs
// less than the run's dispatch, as pureByTree finds for pure bodies.
const regLeafMax = 12

// regLeafOp is an instruction of a body regLeafRun runs: its operation
// one of a few numbered densely, so that the run's switch is a jump table,
// and a load's value or an immediate's number made once.
type regLeafOp struct {
	op      uint8
	a, b, c uint16
	k       Value
}

// The operations of regLeafOp.
const (
	rlMove = iota
	rlLoad
	rlGetUp
	rlSetUp
	rlAdd
	rlSub
	rlMul
	// rlAddK adds k: add_imm, sub_imm of the negated immediate (x - y is
	// x + -y, -0 and NaN alike), inc and dec.
	rlAddK
	// rlMulK multiplies by k: mul_imm.
	rlMulK
	rlRet
	rlRetUndef
)

// regLeafOps is rc's code, which regLeafRunnable has passed, as regLeafRun
// runs it, to its first return.
func regLeafOps(fn *bytecode.Function, rc *bytecode.RegCode) []regLeafOp {
	var ops []regLeafOp
	for _, w := range rc.Code {
		o := regLeafOp{a: w.A(), b: w.B(), c: w.C()}
		switch w.Op() {
		case bytecode.RMove:
			o.op = rlMove
		case bytecode.RLoadInt, bytecode.RLoadConst, bytecode.RLoadUndef, bytecode.RLoadNull, bytecode.RLoadTrue, bytecode.RLoadFalse:
			v, ok := regConstOf(regConsts(fn, rc), w)
			if !ok {
				return nil
			}
			o.op, o.k = rlLoad, v
		case bytecode.RGetUpval:
			o.op, o.b = rlGetUp, uint16(w.BC())
		case bytecode.RSetUpval:
			o.op, o.b = rlSetUp, uint16(w.BC())
		case bytecode.RAdd:
			o.op = rlAdd
		case bytecode.RSub:
			o.op = rlSub
		case bytecode.RMul:
			o.op = rlMul
		case bytecode.RAddI:
			o.op, o.k = rlAddK, Float(float64(w.SC()))
		case bytecode.RSubI:
			o.op, o.k = rlAddK, Float(-float64(w.SC()))
		case bytecode.RMulI:
			o.op, o.k = rlMulK, Float(float64(w.SC()))
		case bytecode.RInc:
			o.op, o.k = rlAddK, Float(1)
		case bytecode.RDec:
			o.op, o.k = rlAddK, Float(-1)
		case bytecode.RRet:
			o.op = rlRet
		default:
			o.op = rlRetUndef
		}
		ops = append(ops, o)
		if o.op >= rlRet {
			return ops
		}
	}
	return nil
}

// regHeld is a store regLeafRun holds until its body returns: the slot of
// an upvalue and the value.
type regHeld struct {
	p *Value
	v Value
}

// regLeafStore makes regLeafRun's held stores, in their order. A number
// over a number writes its word alone, with no write barrier's check.
func regLeafStore(held []regHeld) {
	for _, h := range held {
		if h.v.IsNumber() && h.p.IsNumber() {
			h.p.num = h.v.num
		} else {
			*h.p = h.v
		}
	}
}

// regOf is the register code of cl's function, built if the function has
// not been run yet, and kept in its template for regCall's next call of it;
// or nil.
func (r *Runtime) regOf(cl *closure) *regCode {
	c := firstReg(cl.fn)
	if c != nil && cl.reg == nil {
		r.regExtend(cl, c)
	}
	cl.reg = c
	return c
}

// regStackOp is the stack form's operator of each specialised operation,
// whose slow path is the stack form's.
var regStackOp = [256]bytecode.Op{
	bytecode.RAndI: bytecode.OpBitAnd, bytecode.ROrI: bytecode.OpBitOr, bytecode.RXorI: bytecode.OpBitXor,
	bytecode.RShlI: bytecode.OpShl, bytecode.RShrI: bytecode.OpShr, bytecode.RUShrI: bytecode.OpUShr,
	bytecode.RBitAnd: bytecode.OpBitAnd, bytecode.RBitOr: bytecode.OpBitOr, bytecode.RBitXor: bytecode.OpBitXor,
	bytecode.RShl: bytecode.OpShl, bytecode.RShr: bytecode.OpShr, bytecode.RUShr: bytecode.OpUShr,
	bytecode.RAdd: bytecode.OpAdd, bytecode.RSub: bytecode.OpSub, bytecode.RMul: bytecode.OpMul,
	bytecode.RMulI: bytecode.OpMul, bytecode.RAddI32: bytecode.OpAdd, bytecode.RSubI32: bytecode.OpSub,
	bytecode.RAddII32: bytecode.OpAdd,
}

// regJumpCmp is each compare-and-branch operation's comparison: its stack
// form operator, whether it jumps when the comparison does not hold, and
// whether its right operand is a register.
var regJumpCmp = [256]struct {
	op              bytecode.Op
	not, reg, upval bool
}{
	bytecode.RJLtU: {bytecode.OpLt, false, false, true}, bytecode.RJNLtU: {bytecode.OpLt, true, false, true},
	bytecode.RJLeU: {bytecode.OpLe, false, false, true}, bytecode.RJNLeU: {bytecode.OpLe, true, false, true},
	bytecode.RJLt: {bytecode.OpLt, false, true, false}, bytecode.RJNLt: {bytecode.OpLt, true, true, false},
	bytecode.RJLe: {bytecode.OpLe, false, true, false}, bytecode.RJNLe: {bytecode.OpLe, true, true, false},
	bytecode.RJGt: {bytecode.OpGt, false, true, false}, bytecode.RJNGt: {bytecode.OpGt, true, true, false},
	bytecode.RJGe: {bytecode.OpGe, false, true, false}, bytecode.RJNGe: {bytecode.OpGe, true, true, false},
	bytecode.RJLtI: {bytecode.OpLt, false, false, false}, bytecode.RJNLtI: {bytecode.OpLt, true, false, false},
	bytecode.RJLeI: {bytecode.OpLe, false, false, false}, bytecode.RJNLeI: {bytecode.OpLe, true, false, false},
	bytecode.RJGtI: {bytecode.OpGt, false, false, false}, bytecode.RJNGtI: {bytecode.OpGt, true, false, false},
	bytecode.RJGeI: {bytecode.OpGe, false, false, false}, bytecode.RJNGeI: {bytecode.OpGe, true, false, false},
	bytecode.RJEq: {bytecode.OpEq, false, true, false}, bytecode.RJNe: {bytecode.OpEq, true, true, false},
	bytecode.RJEqI: {bytecode.OpEq, false, false, false}, bytecode.RJNeI: {bytecode.OpEq, true, false, false},
	bytecode.RJNull: {bytecode.OpEq, false, false, false}, bytecode.RJNotNull: {bytecode.OpEq, true, false, false},
	// A step-and-branch's, its partner's.
	bytecode.RStepJLt: {bytecode.OpLt, false, true, false}, bytecode.RStepJLe: {bytecode.OpLe, false, true, false},
	bytecode.RStepJLtI: {bytecode.OpLt, false, false, false}, bytecode.RStepJGtI: {bytecode.OpGt, false, false, false},
	bytecode.RStepJGeI: {bytecode.OpGe, false, false, false}, bytecode.RStepJLeU: {bytecode.OpLe, false, false, true},
}

// regConstruct is new_w: new callee with the argc arguments of the window
// at base, whose site's cache is calls[site] (none where the site is past
// calls). A base constructor of this realm with register code of the
// form's own layout -- a function's or a class's -- runs in the window as
// a register frame, its object made as constructWithTarget makes it -- its
// prototype, its room, the root its objects start from -- and the room it
// came to need noted. Once its objects' room is settled (the root
// tracked), the site's cache is made the constructor's
// (regCallConstruct), which the native handler enters itself, its object
// made by Go with no exit (nativeNew). Anything else is constructed as
// construct constructs it.
func (r *Runtime) regConstruct(calls []regCallCache, site uint16, callee Value, base, argc int) (Value, error) {
	args := r.stack[base+2 : base+2+argc : base+2+argc]
	if !callee.IsObject() {
		return r.construct(callee, args)
	}
	o := callee.Object()
	fd := o.fn()
	// Whether the site's cache holds this constructor already, from an
	// earlier construction: then none of the work below that fills it
	// is done again, as none was before the native handler had a cache.
	noted := false
	if uint(site) < uint(len(calls)) {
		e := &calls[site]
		noted = e.callee == o && e.kind == regCallConstruct && fd != nil && fd.closure != nil && e.rc == fd.closure.reg
	}
	// regCtorBase, in line: asked at every construction of the Go table's.
	if fd == nil || fd.closure == nil || fd.bound || fd.ctorKind != ctorBase || fd.closure.realm != r.Realm ||
		proxyOf(o) != nil || fd.extra != nil && !regCtorScopesOK(fd) {
		if !noted && fd != nil && fd.ctorKind == ctorDerived && uint(site) < uint(len(calls)) {
			if cl, rc, ok := r.regCtorEntered(o, fd); ok {
				// A derived class's, which the native handler enters with
				// its this binding (nativeNew); its construction here is
				// construct's, as before.
				regCtorNote(&calls[site], o, cl, rc)
			}
		}
		return r.construct(callee, args)
	}
	cl := fd.closure
	if cl.fn.Leaf == bytecode.LeafForward {
		// What hands its arguments on, the construction answers without a
		// frame (leafForward); the native handler enters it with one,
		// whose apply_args it makes too, once its objects' room is
		// settled.
		if !noted && uint(site) < uint(len(calls)) && fd.fieldInit() == nil {
			if p := o.getOwnVisible(atomPrototype); p.ok() && p.flags()&propAccessor == 0 && p.value().IsObject() {
				if root := r.shapes.ctorRoot(fd, p.value().Object()); root == nil || root.tracked() {
					if ecl, rc, ok := r.regCtorEntered(o, fd); ok {
						regCtorNote(&calls[site], o, ecl, rc)
					}
				}
			}
		}
		return r.construct(callee, args)
	}
	rc := cl.reg
	if rc == nil {
		rc = r.regOf(cl)
	}
	if rc == nil || !rc.wide || rc.suspends || cl.fn.HasDirectEval {
		return r.construct(callee, args)
	}
	var protoVal Value
	var err error
	if p := o.getOwnVisible(atomPrototype); p.ok() && p.flags()&propAccessor == 0 {
		protoVal = p.value()
	} else if protoVal, err = r.getProp(o, atomPrototype, Obj(o)); err != nil {
		return Undefined, err
	}
	var proto *Object
	if protoVal.IsObject() {
		proto = protoVal.Object()
	} else if proto, err = r.protoForNewTarget(o, r.proto.object); err != nil {
		return Undefined, err
	}
	root := r.shapes.ctorRoot(fd, proto)
	if !noted && uint(site) < uint(len(calls)) && (root == nil || root.tracked()) && fd.fieldInit() == nil {
		// Its objects' room settled and nothing to give them but what its
		// body stores: the site's to construct with from now on.
		regCtorNote(&calls[site], o, cl, rc)
	}
	obj := newOrdinary[struct{}](fd, root, proto, root.roomFor(int(cl.fn.ThisProps)))
	this := Obj(obj)
	if err := r.initInstanceElements(o, this); err != nil {
		return Undefined, err
	}
	if cl.fn.Leaf != bytecode.LeafNone && cl.fn.Leaf != bytecode.LeafPure {
		// A body that only stores its parameters in this, without a frame
		// where leafCall can.
		if _, done := r.leafCall(cl, obj, args); done {
			root.noteBuilt(int(obj.valn))
			return this, nil
		}
	}
	r.stack[base+1] = this
	res, err := r.regEnterW(o, cl, rc, base, argc, args, Obj(o))
	if err != nil {
		return Undefined, err
	}
	root.noteBuilt(int(obj.valn))
	if res.IsObject() {
		return res, nil
	}
	return this, nil
}

// regCtorNote makes e, a new_w site's cache, the construction's with o, of
// closure cl and register code rc (regCallConstruct), written only where
// it holds something else: a construction the native handler leaves to Go
// passes here each time, and the three pointers' stores, barriers and all,
// had been made at every one.
func regCtorNote(e *regCallCache, o *Object, cl *closure, rc *regCode) {
	if e.callee != o || e.cl != cl || e.rc != rc || e.kind != regCallConstruct {
		*e = regCallCache{callee: o, cl: cl, rc: rc, kind: regCallConstruct}
	}
}

// regCtorBase reports whether o, of funcData fd with a closure, is a base
// constructor regConstruct runs as a register frame: a function's or a
// base class's of this realm, neither bound nor a proxy's target, with no
// with or eval scope of its surroundings to establish, which runFD's frames
// would have. regConstruct, which the Go table's loop calls for every
// construction, spells the same test out in line.
func (r *Runtime) regCtorBase(o *Object, fd *funcData) bool {
	return !fd.bound && fd.ctorKind == ctorBase && fd.closure.realm == r.Realm && proxyOf(o) == nil &&
		(fd.extra == nil || regCtorScopesOK(fd))
}

// regCtorScopesOK reports whether fd's side fields leave a constructor of
// it nothing to establish but its this (regCtorBase): no arrow's this, no
// with or eval scope of its surroundings.
func regCtorScopesOK(fd *funcData) bool {
	x := fd.extra
	return !fd.arrow && x.lexThisRef == nil && len(x.with()) == 0 && x.evalVars() == nil
}

// regCtorEntered is the closure and register code of o, of funcData fd,
// where a construction with it, or a super() calling it, is entered as a
// register frame with nothing to establish but its this, its this binding
// for a derived class's, and its new.target: a constructor of this realm,
// a base one (regCtorBase) or a derived class's, with no with or eval
// scope, of the form's own layout, neither suspending nor with a direct
// eval, that gives its objects no fields of its own (no instance
// initializer to run). ok is false for anything else.
func (r *Runtime) regCtorEntered(o *Object, fd *funcData) (cl *closure, rc *regCode, ok bool) {
	if fd.closure == nil || fd.fieldInit() != nil {
		return nil, nil, false
	}
	switch fd.ctorKind {
	case ctorBase:
		if !r.regCtorBase(o, fd) {
			return nil, nil, false
		}
	case ctorDerived:
		if fd.bound || fd.closure.realm != r.Realm || proxyOf(o) != nil || fd.extra != nil {
			return nil, nil, false
		}
	default:
		return nil, nil, false
	}
	cl = fd.closure
	if rc = cl.reg; rc == nil {
		rc = r.regOf(cl)
	}
	if rc == nil || !rc.wide || rc.suspends || cl.fn.HasDirectEval {
		return nil, nil, false
	}
	return cl, rc, true
}

// regCtorThis is the object a construction with o, of funcData fd, a
// constructor its site's cache holds (regCallConstruct), is made: of o's
// prototype, its own data property holding an object, and of its root,
// whose room is settled (tracked) where it is a base constructor's, as
// regConstruct and constructWithTarget make it; or nil, the construction
// then regConstruct's, which reads the prototype however it is and notes
// the room an object came to need.
func (r *Runtime) regCtorThis(o *Object, fd *funcData) *Object {
	p := o.getOwnVisible(atomPrototype)
	if !p.ok() || p.flags()&propAccessor != 0 || !p.value().IsObject() {
		return nil
	}
	proto := p.value().Object()
	root := r.shapes.ctorRoot(fd, proto)
	if root != nil && !root.tracked() && fd.ctorKind != ctorDerived {
		return nil
	}
	return newOrdinary[struct{}](fd, root, proto, root.roomFor(int(fd.closure.fn.ThisProps)))
}

// regThisBinding is the this binding of a derived constructor's frame,
// cl's with register code rc, that a construction or a super() enters at
// the runtime's depth now, holding this, as regRunFD makes it: the
// runtime's cell for the depth where nothing keeps it past the frame
// (cellThis), which the frame's end clears (regEndThisRef).
func (r *Runtime) regThisBinding(rc *regCode, this Value) *thisBinding {
	if !rc.cellThis {
		return &thisBinding{value: this}
	}
	d := r.frameDepth
	if d >= len(r.thisCells) {
		r.thisCells = make([]thisBinding, d+d/2+16)
	}
	b := &r.thisCells[d]
	*b = thisBinding{value: this}
	return b
}

// regEndThisRef ends the this binding of nf, a derived constructor's frame
// of register code rc the native code entered, as regRunFD ends one: the
// runtime's cell lets go of the object, and the frame of the binding.
func regEndThisRef(nf *frame, rc *regCode) {
	if rc.cellThis {
		nf.thisRef.value = Undefined
	}
	nf.thisRef = nil
}

// regAddProp is add_prop's slow path: the property read as get_prop
// reads it, b added, and the sum stored as set_prop stores it, through the
// site's entry, which the store fills.
func (r *Runtime) regAddProp(cl *closure, c *regCode, w bytecode.RInstr, obj, b Value) error {
	name := cl.names[c.siteName[w.C()]]
	cur, err := r.getValueProp(obj, name)
	if err != nil {
		return err
	}
	var sum Value
	if w.X() != 0 {
		sum, err = r.regStep(cur, b.Number())
	} else {
		sum, err = r.add(cur, b)
	}
	if err != nil {
		return err
	}
	if obj.IsObject() {
		return r.setPropCached(&cl.ic[w.C()], obj.Object(), name, sum, cl.fn.Strict)
	}
	return r.setValueProp(obj, name, sum, cl.fn.Strict)
}

// regAddIndex is add_index's slow path: the key converted once, as
// key_of_base converts it, the element read, b added as + adds or, for an
// update, stepped as ++ steps, and the sum stored, at store, the stack
// instruction whose position the store's errors take.
func (r *Runtime) regAddIndex(f *frame, store uint32, obj, key, b Value, update bool) error {
	if !key.IsString() && !key.IsNumber() && !key.IsSymbol() {
		if obj.IsNullish() {
			return r.throwTypeError("cannot read property of %s", r.describe(obj))
		}
		k, err := r.toPropertyKey(key)
		if err != nil {
			return err
		}
		key = r.keyToValue(k)
	}
	cur, err := r.getIndexed(obj, key)
	if err != nil {
		return err
	}
	var sum Value
	if update {
		sum, err = r.regStep(cur, b.Number())
	} else {
		sum, err = r.add(cur, b)
	}
	if err != nil {
		return err
	}
	f.pc = store + 1
	return r.setIndexed(obj, key, sum, f.cl.fn.Strict)
}

// regThrow is throw: the value thrown, from the instruction at pc.
//
// next is the pc after the throw's, which the loop holds: the throw's own,
// next-1 there, is the value before the loop's increment, which Go would keep
// for it at every dispatch.
func (r *Runtime) regThrow(f *frame, c *regCode, next int, regs *[1 << 16]Value) error {
	pc := next - 1
	f.pc = c.src[pc] + 1
	return r.throw(regs[regAt(c.code, pc).A()])
}

// regIn is key in obj.
func (r *Runtime) regIn(key, obj Value) (Value, error) {
	if !obj.IsObject() {
		return Undefined, r.throwTypeError("the right operand of \"in\" must be an object")
	}
	// An index straight to its walk; a name to the cache, then to its walk,
	// which remembers the answer there (hasCached).
	o := obj.Object()
	if i := uint32(key.num); float64(i) == key.num && i < atomIndexTag {
		if has, ok := hasIndexQuick(o, i); ok {
			return Bool(has), nil
		}
	} else if has, ok := hasCached(r, key, o); ok {
		return Bool(has), nil
	} else if has, ok := hasQuick(r, key, o); ok {
		return Bool(has), nil
	}
	n, err := r.toPropertyName(key)
	if err != nil {
		return Undefined, err
	}
	k, known := r.keyFor(obj.Object(), n)
	if !known {
		return False, nil
	}
	has, err := r.hasPropErr(obj.Object(), k)
	return Bool(has), err
}

// regDelete is delete obj[key], delete_prop.
func (r *Runtime) regDelete(cl *closure, obj, key Value) (Value, error) {
	if obj.IsNullish() {
		return Undefined, r.throwTypeError("cannot delete a property of %s", r.describe(obj))
	}
	n, err := r.toPropertyName(key)
	if err != nil {
		return Undefined, err
	}
	if !obj.IsObject() {
		if obj, err = toObjectValue(r, obj); err != nil {
			return Undefined, err
		}
	}
	k, has := r.keyFor(obj.Object(), n)
	if !has {
		return True, nil
	}
	ok, err := r.deleteProp(obj.Object(), k, cl.fn.Strict)
	return Bool(ok), err
}

// regThrowDeadZone is the ReferenceError of a read of a binding in its dead
// zone, at the check at pc.
// next is the pc after the read's, as regThrow's is.
func (r *Runtime) regThrowDeadZone(f *frame, c *regCode, next int) error {
	pc := next - 1
	f.pc = c.src[pc] + 1
	return r.throwReferenceError("cannot access %q before initialization", f.cl.fn.Locals[regNameIdx(c, pc, regAt(c.code, pc).B())].Name)
}

// regGetGlobal is get_global's slow path: the global's read, then the
// layout of the global object the site found the slot in noted,
// for the loop's check.
func (r *Runtime) regGetGlobal(f *frame, cl *closure, c *regCode, pc int, w bytecode.RInstr) (Value, error) {
	in := bytecode.Instr{Op: bytecode.OpGetGlobal, A: regNameIdx(c, pc, w.B()), B: uint32(w.C())}
	v, err := r.getGlobalAt(f, cl, in)
	if err == nil {
		if name := cl.names[in.A]; r.lexShadows(name) {
			r.regNoteGlobalLex(f, cl, w.C(), name, false)
		} else {
			r.regNoteGlobal(cl, w.C(), name, false)
		}
	}
	return v, err
}

// regSetGlobal is set_global's, or set_global_strict's, slow path.
func (r *Runtime) regSetGlobal(f *frame, cl *closure, c *regCode, pc int, w bytecode.RInstr, v Value) error {
	in := bytecode.Instr{Op: bytecode.OpSetGlobal, A: regNameIdx(c, pc, w.B()), B: uint32(w.C())}
	var err error
	if w.X() != 0 {
		in.Op = bytecode.OpSetGlobalStrict
		err = r.setGlobalStrict(f, cl, in, v)
	} else {
		err = r.setGlobalIn(f, cl, in, v)
	}
	if err == nil {
		if name := cl.names[in.A]; r.lexShadows(name) {
			r.regNoteGlobalLex(f, cl, w.C(), name, true)
		} else {
			r.regNoteGlobal(cl, w.C(), name, true)
		}
	}
	return err
}

// regNoteGlobalLex is regNoteGlobal's of a name a script-level lexical
// binding has (lexShadows), which getGlobalAt reads in globalLex: the
// site's slot the binding's, at the global epoch, where it is
// initialized -- out of its dead zone for good -- and, for a write, not a
// constant, in a frame whose scope is the realm's global one with no
// variables of a direct eval in front of it. globalLex's table is only
// appended to, and a binding declared bumps the epoch, so that while the
// epoch is what it was the slot is the binding's, as for the global
// object's (rhGetGlobal reads either); otherwise the site stays Go's.
func (r *Runtime) regNoteGlobalLex(f *frame, cl *closure, site uint16, name Atom, write bool) {
	if cl.regEpochs == nil {
		cl.regEpochs = make([]uint64, len(cl.ic))
	}
	cl.regEpochs[site] = 0
	lex := r.globalLex
	if f.evalVars != nil || !r.isGlobalScope(cl.scope()) || lex.shape == nil {
		return
	}
	i := lex.findOwn(name)
	if i < 0 {
		return
	}
	mask := propAccessor | propPrivate | propDeleted | propUninit
	want := propFlags(0)
	if write {
		mask |= propWritable
		want = propWritable
	}
	if lex.shape.keys[i].flags&mask != want || lex.vals()[i].IsUninitialized() {
		return
	}
	s := &cl.ic[site]
	s.p1, s.idx = lex, i
	cl.regEpochs[site] = r.shapes.globalEpoch + 1
}

// regNoteGlobal remembers, at global site site of cl's code, the global
// epoch it found name's binding it may read or write in place at: a plain data
// property of the global object -- a writable one for a write -- of a
// layout of its own, which no script-level lexical binding shadows. While
// the epoch is what it was, nothing of that has changed (shapeTree's
// globalEpoch): the global object's table has only been appended to and no
// lexical binding declared. The epoch is kept as one more than it is, so
// that a site never filled is never current.
func (r *Runtime) regNoteGlobal(cl *closure, site uint16, name Atom, write bool) {
	if cl.regEpochs == nil {
		cl.regEpochs = make([]uint64, len(cl.ic))
	}
	cl.regEpochs[site] = 0
	s := &cl.ic[site]
	env := s.p1
	if env == nil || s.idx < 0 || int(s.idx) >= int(env.valn) || env.shape == nil {
		return
	}
	sh := env.shape
	if env.flags&objGlobalBindings == 0 || !sh.unique || sh.loose || sh.tree != r.shapes {
		return
	}
	p := sh.keys[s.idx]
	if p.key != name || r.lexShadows(name) {
		return
	}
	mask := propAccessor | propPrivate | propDeleted | propUninit
	want := propFlags(0)
	if write {
		mask |= propWritable
		want = propWritable
	}
	if p.flags&mask == want {
		cl.regEpochs[site] = r.shapes.globalEpoch + 1
	}
}

// regGetProp is get_prop's slow path.
func (r *Runtime) regGetProp(cl *closure, c *regCode, w bytecode.RInstr, obj Value) (Value, error) {
	site := w.C()
	name := cl.names[c.siteName[site]]
	if obj.IsObject() {
		o := obj.Object()
		// A plain own property of a small table, remembered for the loop's
		// check of the next object of its layout (ownScan).
		if v, ok := ownScan(&cl.ic[site], o, name); ok {
			return v, nil
		}
		if v, ok, err := r.cachedGet(&cl.ic[site], o, name); ok {
			return v, err
		}
	}
	return r.getValueProp(obj, name)
}

// regGetMethod is get_prop_this's slow path.
func (r *Runtime) regGetMethod(cl *closure, c *regCode, w bytecode.RInstr, obj Value) (Value, error) {
	site := w.C()
	name := cl.names[c.siteName[site]]
	if obj.IsObject() {
		if v, ok := ownScan(&cl.ic[site], obj.Object(), name); ok {
			return v, nil
		}
		if v, ok := r.cachedProp(&cl.ic[site], obj.Object(), name); ok {
			return v, nil
		}
	} else {
		// A string's or a number's method, from the cache of its
		// prototype's data properties that primProp fills: s.indexOf,
		// n.toString.
		var p *Object
		if obj.IsString() {
			p = r.proto.str
		} else if obj.IsNumber() {
			p = r.proto.number
		}
		if e := &r.primProps[name&63]; p != nil && e.key == name && e.proto == p && e.shape == p.shape {
			if verifyShapes {
				checkShape(p)
			}
			return p.vals()[e.idx], nil
		}
	}
	return r.getValueProp(obj, name)
}

// regSetProp is set_prop's slow path.
func (r *Runtime) regSetProp(cl *closure, c *regCode, w bytecode.RInstr, obj, v Value) error {
	site := w.C()
	name := cl.names[c.siteName[site]]
	if obj.IsObject() {
		return r.setPropCached(&cl.ic[site], obj.Object(), name, v, cl.fn.Strict)
	}
	return r.setValueProp(obj, name, v, cl.fn.Strict)
}

// regDefineField is define_field.
func (r *Runtime) regDefineField(cl *closure, name Atom, obj, v Value) error {
	if !obj.IsObject() {
		return nil
	}
	if o := obj.Object(); o.class == ClassObject && o.flags&objExtensible != 0 {
		if next := o.transition(name, propDefault); next != nil {
			o.appendTransition(v, next)
			return nil
		}
	}
	return r.defineOwnProp(obj.Object(), name, v, propDefault)
}

// regNewObject is new_object.
func (r *Runtime) regNewObject(cl *closure, w bytecode.RInstr) *Object {
	// With the room the site's objects came to need (outgrowProps learns
	// it), where the literal says less.
	root := r.shapes.siteRoot(&cl.ic[w.C()], r.proto.object)
	m := int(w.B())
	if root != nil && int(root.slack) > m {
		m = int(root.slack)
	}
	return newShapedLiteral(root.orRoot(ClassObject, r.proto.object), ClassObject, m)
}

// A register frame's regKind: what a stack trace reads of it where the heap
// frame's this and newTarget are not written.
const (
	// regFrameCall is rhCallW's frame: its this is its window's second
	// register, and it is no construct.
	regFrameCall = 1 + iota
	// regFrameEnter is regEnterW's: its this is its window's, its
	// new.target the frame's.
	regFrameEnter
	// regFrameAwaitJob is an async function's, resumed by its await's
	// job from the job queue, its this and new.target the frame's
	// (regColdGen).
	regFrameAwaitJob
	// regFrameGenStep is a generator's, a for-of's step the native code
	// entered itself (the iter_next handler), its this and new.target the
	// frame's: a yield, a return or a throw ends the step
	// (nativeGenEnd), not its caller's call.
	regFrameGenStep
	// regFrameConstruct is a construction's the native code entered
	// itself (new_w's handler): regEnterW's frame, its new.target the
	// frame's, whose return gives its this, register 1, where it returns
	// what is not an object; a derived class's has its this binding too,
	// which its end lets go of.
	regFrameConstruct
	// regFrameSuper is a super()'s call of the parent constructor the
	// native code entered itself (super_call's handler): a construction's
	// of the derived constructor's object and new.target, whose return
	// binds the caller's this (superBind), which Go makes.
	regFrameSuper
)

// regFrameThis is the this and new.target of register frame f, for a stack
// trace.
func (r *Runtime) regFrameThis(f *frame) (this, newTarget Value) {
	if f.regKind == regFrameAwaitJob || f.regKind == regFrameGenStep {
		return f.this, f.newTarget
	}
	this = r.stack[f.base+1]
	newTarget = Undefined
	if f.regKind == regFrameEnter || f.regKind == regFrameConstruct || f.regKind == regFrameSuper {
		newTarget = f.newTarget
	}
	return this, newTarget
}
