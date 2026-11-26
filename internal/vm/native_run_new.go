//go:build amd64 || arm64

package vm

import (
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// native_run.go's, from the native dispatcher's constructions on, in a file
// of its own after the layout lock's pad CN (native_run_n_layoutpad.go), so
// that what the JIT's hooks add before it in native_run_builtin.go moves
// none of it (docs/optimizing.md, "The layout lock").

// nativeNew is new_w's object, made for the native handler through callH
// with no exit, as closure_w's calls nativeClosure: the construction's
// with the constructor its site's cache holds (regCallConstruct), which
// the handler has checked is R[A], made as regCtorThis makes it and put
// in R[A+1], the frame's this. It reports 1 for the handler to enter the
// constructor with it; 3 where the handler may first make the stores of
// the constructor's initializer plan, or of its forwarder's method's
// (regCode's initPlan, fwdKind), itself, without a frame -- the JIT not
// running, whose frames have feedback to give, an interrupt not due, and
// the frames the body would have had within the limit, so that its
// RangeError is the frame's -- and enter it where they do not all
// validate; 2 where the constructor is a body that only stores its
// parameters in this, which leafCall has done, R[B] the object; and 0
// where the construction is Go's (rsNewW): a prototype that is no data
// property holding an object, a root whose room is not settled, a leaf
// body leafCall does not answer. It throws nothing and runs no code.
//
//go:nocheckptr
func nativeNew(regs *[1 << 16]Value, w bytecode.RInstr, pc int, ctx *nativeCtx) int {
	f := (*frame)(nativePtr(&ctx.nf))
	r := ctx.st.r
	e := &f.cl.regCalls[w.C()]
	o := e.callee
	fd := o.fn()
	obj := r.regCtorThis(e, fd)
	if obj == nil {
		return 0
	}
	a := int(w.A())
	regs[a+1] = Obj(obj)
	ctx.scratch[sThisRef] = 0
	if fd.ctorKind == ctorDerived {
		// A derived class's frame, with its this binding, which the
		// handler gives the frame, holding the object until super()
		// binds it, as regRunFD makes one.
		ctx.thisRef = r.regThisBinding(e.rc, Obj(obj))
		ctx.scratch[sThisRef] = uint64(uintptr(unsafe.Pointer(ctx.thisRef)))
		return 1
	}
	if c := e.rc; (len(c.initPlan) > 0 && r.frameDepth < r.maxFrames || c.fwdKind != 0 && r.frameDepth+1 < r.maxFrames) &&
		r.interruptCounter > 1 && !r.jitOn() {
		if c.fwdKind != 0 {
			// The handler reads the method of obj's shape at the
			// forwarder's site, or in its table where the site has given
			// up, which leafForward would have filled (fwdCache).
			site := int(c.fwdSites & 0xffff)
			r.fwdCache(e.cl, site, obj, e.cl.names[c.siteName[site]])
		}
		return 3
	}
	if leaf := leafKind(e.cl); leaf != bytecode.LeafNone && leaf != bytecode.LeafPure && leaf != bytecode.LeafForward {
		if _, done := r.leafCall(e.cl, obj, regs[a+2:a+2+int(w.X())]); !done {
			return 0
		}
		regs[w.B()] = Obj(obj)
		return 2
	}
	return 1
}

// nativeSuper is super_call's and super_forward's entry of the parent
// constructor, made for the native handler through callH with no exit:
// in a derived class's constructor's own frame whose this super() has
// not bound yet, of a class with no fields of its own, the parent its
// constructor's prototype, a constructor the handler enters as a register
// frame (regCtorEntered), its window made at the stack's top -- the
// parent, the frame's this and the arguments, the registers' from C or
// the frame's own -- with its this binding where it is a derived class's,
// and its new.target the frame's, as superCall makes the call. It reports
// 1 for the handler to enter it, the context's super the entry, and 0
// where super() is Go's (rhSuperCall). It throws nothing and runs no
// code.
//
//go:nocheckptr
func nativeSuper(regs *[1 << 16]Value, w bytecode.RInstr, pc int, ctx *nativeCtx) int {
	f := (*frame)(nativePtr(&ctx.nf))
	r := ctx.st.r
	if f.cl.fn.Kind != bytecode.KindDerivedConstructor || f.thisRef == nil || f.thisRef.init || !f.this.IsObject() ||
		f.callee == nil || !f.newTarget.IsObject() {
		return 0
	}
	fd := f.callee.fn()
	if fd == nil || fd.fieldInit() != nil {
		return 0
	}
	if c := fd.superCtor(); c != nil && c != f.callee {
		if cfd := c.fn(); cfd == nil || cfd.fieldInit() != nil {
			return 0
		}
	}
	parent := r.parentConstructorOf(f)
	if parent == nil {
		return 0
	}
	pfd := parent.fn()
	if pfd == nil {
		return 0
	}
	cl, rc, ok := r.regCtorEntered(parent, pfd)
	if !ok {
		return 0
	}
	var args []Value
	if w.Op() == bytecode.RSuperForward {
		args = f.args
	} else {
		lo := int(w.C())
		args = regs[lo : lo+int(w.B()) : lo+int(w.B())]
	}
	wb := r.stackTop
	if wb+2+len(args) > len(r.stack) {
		return 0
	}
	regNextFrameClear(r)
	win := r.stack[wb : wb+2+len(args) : wb+2+len(args)]
	win[0], win[1] = Obj(parent), f.this
	copy(win[2:], args)
	ctx.scratch[sThisRef] = 0
	if pfd.ctorKind == ctorDerived {
		ctx.thisRef = r.regThisBinding(rc, f.this)
		ctx.scratch[sThisRef] = uint64(uintptr(unsafe.Pointer(ctx.thisRef)))
	}
	ctx.super = regCallCache{callee: parent, cl: cl, rc: rc, kind: regCallEnter}
	ctx.scratch[sE] = uint64(uintptr(unsafe.Pointer(&ctx.super)))
	ctx.scratch[sWin] = uint64(uintptr(unsafe.Pointer(&win[0])))
	ctx.scratch[sArgc] = uint64(len(args))
	ctx.scratch[sNewTgt] = uint64(uintptr(unsafe.Pointer(f.newTarget.Object())))
	return 1
}

// nativeApply is apply_args's entry of its target, f.apply(this,
// arguments) by the realm's Function.prototype.apply, made for the native
// handler through callH with no exit: f a function the handler enters as
// call_w enters the callee its cache holds (regCallKind), its window made
// at the stack's top -- f, the this, as its kind takes it, and the frame's
// arguments as regApplyArgs hands them on -- without apply's frame, which
// a stack trace and a caller pass over. It reports 1 for the handler to
// enter it, the context's super the entry, and 0 where the call is Go's
// (rsApplyArgs); 3 where the handler may first make the stores of f's
// initializer plan (regCode's initPlan) itself, on the this, an object,
// without f's frame, as nativeNew's 3 says, and enter f where they do not
// all validate. It throws nothing and runs no code.
//
//go:nocheckptr
func nativeApply(regs *[1 << 16]Value, w bytecode.RInstr, pc int, ctx *nativeCtx) int {
	f := (*frame)(nativePtr(&ctx.nf))
	r := ctx.st.r
	b := int(w.B())
	target, apply, this := regs[b], regs[b+1], regs[b+2]
	if !apply.IsObject() || apply.Object() != r.applyFn || !target.IsObject() {
		return 0
	}
	o := target.Object()
	fd := o.fn()
	if fd == nil || fd.closure == nil || fd.bound || r.nodeQuirks {
		// With Node's quirks, f.arguments reads the arguments as apply
		// passed them (regDirectLegacy): Go's.
		return 0
	}
	cl, rc, kind, ethis, enter := r.regCallKind(fd, true, ctx.st.c)
	switch {
	case !enter || regTypedCross(rc, ctx.st.c):
		return 0
	case kind == regCallThis:
		this = ethis
	case kind == regCallObjThis:
		if !this.IsObject() {
			return 0
		}
	case kind != regCallEnter:
		return 0
	}
	args := f.args
	if f.cl.fn.MappedArguments {
		n := min(f.cl.fn.ParamCount, len(args), len(f.locals))
		for i := 0; i < n; i++ {
			if !f.locals[i].sameBits(args[i]) {
				// A parameter has been assigned, which a mapped object
				// would show: regApplyArgs's.
				return 0
			}
		}
	}
	wb := r.stackTop
	if wb+2+len(args) > len(r.stack) {
		return 0
	}
	regNextFrameClear(r)
	win := r.stack[wb : wb+2+len(args) : wb+2+len(args)]
	win[0], win[1] = target, this
	copy(win[2:], args)
	ctx.super = regCallCache{callee: o, cl: cl, rc: rc, kind: regCallEnter}
	ctx.scratch[sE] = uint64(uintptr(unsafe.Pointer(&ctx.super)))
	ctx.scratch[sWin] = uint64(uintptr(unsafe.Pointer(&win[0])))
	ctx.scratch[sArgc] = uint64(len(args))
	if len(rc.initPlan) > 0 && rc.initNew == 0 && this.IsObject() && r.frameDepth < r.maxFrames && r.interruptCounter > 1 && !r.jitOn() {
		return 3
	}
	return 1
}

// regNextFrameClear clears what the last user of the frame record a call
// at the runtime's depth now takes left that a register frame has none of
// (regClearFrame): Go's call of the same callee, apply's or super()'s,
// leaves it the built-in's native frame, and the handler, which leaves a
// record so left to Go, would have left every call after it to Go.
func regNextFrameClear(r *Runtime) {
	if off := r.frameDepth - r.frameBase; uint(off) < uint(len(r.cur)) {
		if nf := &r.cur[off]; nf.thisRef != nil || nf.withScopes != nil || nf.evalVars != nil || nf.native != "" {
			regClearFrame(nf)
		}
	}
}

// nativeReflect is a call of the realm's Reflect.construct, its site's
// cached built-in, Reflect.construct(F, args, newTarget), made for the
// call handler through callH with no exit: a construction with F, a
// constructor new_w's handler enters (regCtorEntered), of a plain array
// of arguments with no holes, and a new.target, F where none is given, a
// function whose own prototype is a data property holding an object --
// its object made as constructWithTarget makes it, of new.target's
// prototype and of F's root, its
// window made at the stack's top, a derived class's frame given its this
// binding. It reports 1 for the handler to enter F, the context's super
// the entry, its new.target the scratch's sNewTgt; 2 where F is a body
// that only stores its parameters in this, which leafCall has done, R[B]
// the object; and 0 where the call is Go's (the built-in's). It throws
// nothing and runs no code.
//
//go:nocheckptr
func nativeReflect(regs *[1 << 16]Value, w bytecode.RInstr, pc int, ctx *nativeCtx) int {
	r := ctx.st.r
	a, argc := int(w.A()), int(w.X())
	if argc < 2 || !regs[a+2].IsObject() || !regs[a+3].IsObject() {
		return 0
	}
	o := regs[a+2].Object()
	fd := o.fn()
	if fd == nil {
		return 0
	}
	cl, rc, ok := r.regCtorEntered(o, fd)
	if !ok || r.nodeQuirks && !cl.fn.Strict && !rc.args {
		// With Node's quirks, a sloppy f.arguments reads the arguments as
		// passed (regScopedLegacy): Go's.
		return 0
	}
	nt := o
	if argc > 2 {
		if !regs[a+4].IsObject() {
			return 0
		}
		nt = regs[a+4].Object()
	}
	if nfd := nt.fn(); nfd == nil || nfd.ctorKind == ctorNone || nt.class != ClassFunction {
		return 0
	}
	arr := regs[a+3].Object()
	if arr.class != ClassArray || arr.flags&objHasSparseElements != 0 {
		return 0
	}
	args := arr.elemsOf()
	for _, v := range args {
		if isHole(v) {
			return 0
		}
	}
	p := nt.getOwnVisible(atomPrototype)
	if !p.ok() || p.flags()&propAccessor != 0 || !p.value().IsObject() {
		return 0
	}
	proto := p.value().Object()
	// Its root's room, settled or not: one construction of several
	// new.targets has a root made for each prototype in turn, which no
	// count of objects built would settle; the room is the body's count.
	root := r.shapes.ctorRoot(fd, proto)
	wb := r.stackTop
	if wb+2+len(args) > len(r.stack) {
		return 0
	}
	obj := newOrdinary[struct{}](fd, root, proto, root.roomFor(int(cl.fn.ThisProps)))
	if leaf := leafKind(cl); fd.ctorKind != ctorDerived && leaf != bytecode.LeafNone && leaf != bytecode.LeafPure && leaf != bytecode.LeafForward {
		if _, done := r.leafCall(cl, obj, args); done {
			regs[w.B()] = Obj(obj)
			return 2
		}
		return 0
	}
	regNextFrameClear(r)
	win := r.stack[wb : wb+2+len(args) : wb+2+len(args)]
	win[0], win[1] = Obj(o), Obj(obj)
	copy(win[2:], args)
	ctx.scratch[sThisRef] = 0
	if fd.ctorKind == ctorDerived {
		ctx.thisRef = r.regThisBinding(rc, Obj(obj))
		ctx.scratch[sThisRef] = uint64(uintptr(unsafe.Pointer(ctx.thisRef)))
	}
	ctx.super = regCallCache{callee: o, cl: cl, rc: rc, kind: regCallEnter}
	ctx.scratch[sE] = uint64(uintptr(unsafe.Pointer(&ctx.super)))
	ctx.scratch[sWin] = uint64(uintptr(unsafe.Pointer(&win[0])))
	ctx.scratch[sArgc] = uint64(len(args))
	ctx.scratch[sNewTgt] = uint64(uintptr(unsafe.Pointer(nt)))
	return 1
}

// nativeReflectFn is nativeReflect's func value, set by init.
var nativeReflectFn func(*[1 << 16]Value, bytecode.RInstr, int, *nativeCtx) int

// nativeApplyFn is nativeApply's func value, set by init.
var nativeApplyFn func(*[1 << 16]Value, bytecode.RInstr, int, *nativeCtx) int

// nativeSuperFn is nativeSuper's func value, set by init.
var nativeSuperFn func(*[1 << 16]Value, bytecode.RInstr, int, *nativeCtx) int

// nativeNewFn is nativeNew's func value, set by init.
var nativeNewFn func(*[1 << 16]Value, bytecode.RInstr, int, *nativeCtx) int

// nativeIterResFn is nativeIterRes's func value, set by init.
var nativeIterResFn func(*[1 << 16]Value, bytecode.RInstr, int, *nativeCtx)

// nativeFillPCs writes the pc of each caller on the native frame stack
// that has none written yet: its source position past its call, as
// rhCallW writes it before runReg, so that what Go runs -- a stack trace,
// the debugger -- reads the frames runReg would have made.
//
//go:nocheckptr
func nativeFillPCs(ctx *nativeCtx) {
	for d := ctx.filled; d < ctx.depth; d++ {
		rec := &ctx.recs[d]
		nf := (*frame)(nativePtr(&rec.nf))
		nf.pc = uint32((rec.retIP-rec.codeBase)/8) + 1
	}
	ctx.filled = ctx.depth
}

// nativePtr is the pointer the address at p holds: an object that is
// kept alive elsewhere, which the context and the frame stack hold as
// addresses.
func nativePtr(p *uintptr) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(p)) }

// nativePop makes the caller rec records the top frame again.
func nativePop(ctx *nativeCtx, rec *nativeRec) {
	ctx.regs, ctx.ic, ctx.codeBase = rec.regs, rec.ic, rec.codeBase
	ctx.rc, ctx.nf, ctx.cc, ctx.fv = rec.rc, rec.nf, rec.cc, rec.fv
	ctx.depth--
	ctx.filled = min(ctx.filled, ctx.depth)
	ctx.topOK = 0
}

// nativeBase is the handlers' block's address, where the code is entered.
var nativeBase = nativeHandlersAddr()

// newNativeTable is the dispatcher whose table is at addr.
func newNativeTable(addr uintptr) *nativeTable {
	d := &nativeTable{addr: addr}
	t := (*[256]uintptr)(nativePtr(&addr))
	for op := range t {
		d.has[op] = t[op] != nativeExitAddr
	}
	return d
}

// nativeExitAddr is the exit stub's address: plain's table's entry of the
// last operation, which has no handler.
var nativeExitAddr = func() uintptr {
	at := nativeTableAddr()
	return (*[256]uintptr)(nativePtr(&at))[255]
}()

// nativePlain is the plain dispatcher, the only one in builds without
// the JIT; nativeFeedback, in builds with it, a runtime's that runs the
// JIT, whose handlers record the JIT's feedback
// (native_feedback.go), and plain in builds without.
var nativePlain = newNativeTable(nativeTableAddr())

// nativeDispatch is the dispatcher r's contexts give the code.
func (r *Runtime) nativeDispatch() *nativeTable {
	if r.jitOn() {
		return nativeFeedback
	}
	return nativePlain
}

// nativeStoreRef is jumped to by native code to store a pointer while the
// collector marks: on amd64 the slot in AX, the pointer in CX, where to go
// on in R11, the context in DI, which alone it keeps; on arm64 the slot in
// R0, the pointer in R1, where to go on in R16, the context in R25, which
// it keeps with the link register.
func nativeStoreRef()

func nativeStoreRefAddr() uintptr

// nativeStoreRefGo is the store, with the write barrier.
//
//go:nosplit
//go:norace
//go:nocheckptr
func nativeStoreRefGo(slot *unsafe.Pointer, p unsafe.Pointer) { *slot = p }

// nativeCallH is where an exit calls the Go handler of its instruction:
// on amd64 its func value in DX, its arguments in AX, BX, CX and SI (the
// state), the context in DI; on arm64 its func value in R26, its arguments
// in R0 to R3, the context in R25. It is a function the Go function that
// entered the code called, as the JIT's callGo is, and goes on at the
// context's after with the handler's result in AX (R0) and the context
// in DI (R25).
func nativeCallH()

func nativeCallHAddr() uintptr

// nativeEnter jumps to code with the context in DI (R25 on arm64); the
// code returns to nativeEnter's caller.
//
//go:noescape
func nativeEnter(code uintptr, ctx unsafe.Pointer)

// regNativeHandlers is the assembled block of the handlers
// (native_handlers_<arch>.s); nativeHandlersAddr is its address and
// nativeTableAddr its table's, each operation's handler's address, the
// exit stub's where it has none.
func regNativeHandlers()

func nativeHandlersAddr() uintptr

func nativeTableAddr() uintptr
