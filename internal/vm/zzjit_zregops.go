//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"math"
	"slices"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// Register code's operations native code makes from what the stack form's
// feedback answers of them, by register pc.

// regDefined is the value the register instruction just before register
// pc rpc leaves in register reg, where that instruction reads a binding --
// a global's (get_global), a captured one's (get_upval, get_upval_check)
// -- as it holds it now, as the stack form's jitChainValue finds what the
// code reads before an operation: what the code compiled for it checks it
// still reads.
func (f *jitRegFeedback) regDefined(rpc, reg int) (Value, bool) {
	cl := f.fb.cl
	if cl == nil || rpc < 1 || rpc > len(f.rc.Code) {
		return Undefined, false
	}
	w := f.rc.Code[rpc-1]
	if int(w.A()) != reg {
		return Undefined, false
	}
	switch w.Op() {
	case bytecode.RGetGlobal:
		name, ok := f.name(rpc-1, w.B())
		if !ok {
			return Undefined, false
		}
		return f.fb.r.jitGlobalValue(cl, name)
	case bytecode.RGetUpval, bytecode.RGetUpvalCheck:
		k := int(w.BC())
		if k >= len(cl.upvalues) || cl.upvalues[k] == nil {
			return Undefined, false
		}
		if v := cl.upvalues[k].get(); !v.IsUninitialized() {
			return v, true
		}
	}
	return Undefined, false
}

// InstanceOf is ssa.InstanceOfFeedback's: the instanceof at the program's
// instruction pc (instanceof's Window Host), of the constructor the
// register instruction before it reads (regDefined), made natively along
// the value's prototypes, as the stack form's (jitFeedback's InstanceOf):
// checked to have that constructor, its Symbol.hasInstance the realm's.
func (f *jitRegFeedback) InstanceOf(pc int) (ssa.InstanceOfSite, bool) {
	w, op, ok := f.at(pc)
	if !ok || op != ir.Host || w.Op() != bytecode.RInstanceOf || f.fb.r.hasInstanceFn == nil {
		return ssa.InstanceOfSite{}, false
	}
	v, ok := f.regDefined(int(f.p.Origin[pc]), int(w.C()))
	if !ok || !v.IsObject() {
		return ssa.InstanceOfSite{}, false
	}
	return f.fb.instanceOfSite(v.Object())
}

// IndexOf is ssa.IndexOfFeedback's: the search -- String.prototype's or
// Array.prototype's indexOf, lastIndexOf or includes, the realm's
// (jitSearchKind) -- the method call at the program's instruction pc last
// called, as its site's cache holds it (methodCall), which the stack form's
// code learns at its calls of Go (jitIndexOfs), and its kind. Go makes it
// from native code (ssa.GoIndexOf), and leaves what would run script to
// the call after an exit.
func (f *jitRegFeedback) IndexOf(pc int) (uintptr, int, bool) {
	o, n, ok := f.methodCall(pc, true)
	if !ok || n != 1 && n != 2 {
		return 0, 0, false
	}
	k := f.fb.r.jitSearchKind(o)
	if k < 0 {
		return 0, 0, false
	}
	f.fb.keep().holders = append(f.fb.keep().holders, o)
	return uintptr(unsafe.Pointer(o)), k, true
}

// ParseInt is ssa.ParseIntFeedback's: the realm's parseInt, which the plain
// call of one argument at the program's instruction pc last called
// (methodCall), made of a number natively, as the stack form's
// (jitParseInts).
func (f *jitRegFeedback) ParseInt(pc int) (uintptr, bool) {
	o, n, ok := f.methodCall(pc, false)
	if !ok || n != 1 || o == nil || o != f.fb.r.jitParseInt {
		return 0, false
	}
	f.fb.keep().holders = append(f.fb.keep().holders, o)
	return uintptr(unsafe.Pointer(o)), true
}

// BigString is ssa.BigStringFeedback's: BigInt.prototype.toString, the
// realm's own, which the method call at the program's instruction pc last
// called (methodCall), Go's from native code, as the stack form's
// (jitBigStrings).
func (f *jitRegFeedback) BigString(pc int) (uintptr, bool) {
	o, n, ok := f.methodCall(pc, true)
	if !ok || n > 1 || o == nil || o != f.fb.r.jitBigToString {
		return 0, false
	}
	f.fb.keep().holders = append(f.fb.keep().holders, o)
	return uintptr(unsafe.Pointer(o)), true
}

var (
	_ ssa.InstanceOfFeedback = (*jitRegFeedback)(nil)
	_ ssa.IndexOfFeedback    = (*jitRegFeedback)(nil)
	_ ssa.ParseIntFeedback   = (*jitRegFeedback)(nil)
	_ ssa.BigStringFeedback  = (*jitRegFeedback)(nil)
	_ ssa.ClosureFeedback    = (*jitRegFeedback)(nil)
	_ ssa.CellsFeedback      = (*jitRegFeedback)(nil)
	_ ssa.PureBodyFeedback   = (*jitRegFeedback)(nil)
)

// jitRegCodeIn reports whether the code making the call of Go in ctx, in
// context idx, is register code: a run of it (jitState's regRuns), or a
// native callee of one -- which calls only register code natively. Its
// exit PC is then a register pc, which no stack form's instruction is at.
func (r *Runtime) jitRegCodeIn(ctx *abi.Context, idx int) bool {
	s := r.jit
	switch ctx.Live {
	case 0:
		return idx < len(s.regRuns) && s.regRuns[idx].f != nil
	case abi.LiveCall:
		return r.jitRegRootOf(idx) >= 0
	}
	return false
}

// jitRegPlainSet is, for a call of Go register code makes in ctx, in
// context idx (reg), a set_prop's write jitGoCallPlain may make without a
// frame (jitPlainWrite): the code's closure -- the run's, or a native
// callee's -- and the write as the stack form's instruction names it, its
// site's name and its cache, of the register instruction at its register
// pc; a nil closure where it is none, or is an inlined callee's.
func (r *Runtime) jitRegPlainSet(ctx *abi.Context, idx int) (*closure, bytecode.Instr, bool) {
	if !r.jitRegCodeIn(ctx, idx) {
		return nil, bytecode.Instr{}, false
	}
	if st := jitGoCallState(ctx); st != nil && st.Inline != 0 {
		return nil, bytecode.Instr{}, true
	}
	var cl *closure
	var c *regCode
	if ctx.Live == 0 {
		run := r.jit.regRuns[idx]
		cl, c = run.f.cl, run.c
	} else {
		cl = (*closure)(ctx.Closure)
		if cl != nil {
			c = cl.reg
		}
	}
	rpc := int(ctx.ExitPC)
	if cl == nil || c == nil || rpc >= len(c.rc.Code) {
		return nil, bytecode.Instr{}, true
	}
	w := c.rc.Code[rpc]
	if w.Op() != bytecode.RSetProp || int(w.C()) >= len(c.rc.SiteName) {
		return nil, bytecode.Instr{}, true
	}
	return cl, bytecode.Instr{Op: bytecode.OpSetProp, A: c.rc.SiteName[w.C()], B: uint32(w.C())}, true
}

// jitRegAllocSeen learns, after Go made new_w w at register pc rpc of f's
// code e, a construction with o, the realm's Array, of no argument or of
// a length native code makes (jitArrayRoom), which needs nothing run: the
// next compile takes its arrays from a pool (regAllocSite), as the stack
// form's code learns it (jitAllocates), the code stale from here. It
// reports whether it learned it.
func (r *Runtime) jitRegAllocSeen(f *frame, c *regCode, e *jitRegEntry, rpc int, w bytecode.RInstr, o *Object) bool {
	// The length, in the window's register past the receiver's, the call
	// having written its result elsewhere: the frame's as an exit wrote it.
	n := Undefined
	if a := f.base - int(c.locals) + int(w.A()) + 2; w.X() == 1 && int(w.B()) != int(w.A())+2 && a >= 0 && a < len(r.stack) {
		n = r.stack[a]
	}
	return r.jitRegAllocLength(f, e, rpc, w, o, n)
}

// jitRegAllocLength is jitRegAllocSeen of the length n the construction was
// given: at a call of Go native code made, its operand, which the frame's
// register may not hold, native code keeping values in the frame's
// registers as it found them.
func (r *Runtime) jitRegAllocLength(f *frame, e *jitRegEntry, rpc int, w bytecode.RInstr, o *Object, n Value) bool {
	argc := int(w.X())
	if argc > 1 {
		return false
	}
	room := 0
	if argc == 1 {
		if !n.IsNumber() || n.num < 0 || n.num != math.Trunc(n.num) {
			return false
		}
		if room = jitArrayRoom(n.num); room == 0 {
			return false
		}
	}
	l := r.jit.regLearnedOf(r.jitRegKeyOf(f.cl))
	for _, x := range l.nativeCalls {
		if int(x.pc) == rpc && x.cl == nil && !x.forEach && x.pool != nil {
			return false
		}
	}
	pool := r.jitNewPool(o)
	if argc == 1 {
		pool = r.jitNewArrayPool(o, room)
	}
	l.nativeCalls = append(l.nativeCalls, jitInline{pc: int32(rpc), obj: o, pool: pool})
	r.jitRegReopt(f.cl, e)
	return true
}

// regAllocSite is the construction new_w w makes with the realm's Array
// natively from a pool (jitRegAllocSeen), as the stack form's NativeCalls
// makes one (ssa.CallSite's Alloc): new Array(), or new Array(n) of a
// length its pool's arrays have room for; any other, and any other
// function, Go's after an exit.
func (f *jitRegFeedback) regAllocSite(rpc int, w bytecode.RInstr, argc int) (ssa.CallSite, bool) {
	l, r := f.fb.e, f.fb.r
	if w.Op() != bytecode.RNewW || argc > 1 || l == nil {
		return ssa.CallSite{}, false
	}
	for _, x := range l.nativeCalls {
		if int(x.pc) != rpc || x.cl != nil || x.forEach || x.pool == nil || x.obj != r.proto.arrayCtor {
			continue
		}
		if argc == 1 && x.pool.Room == 0 {
			return ssa.CallSite{}, false
		}
		k := f.fb.keep()
		k.holders, k.pools = append(k.holders, x.obj), append(k.pools, x.pool)
		return ssa.CallSite{Callee: uintptr(unsafe.Pointer(x.obj)), ThisSlot: -1,
			Pool: uintptr(unsafe.Pointer(x.pool)), Alloc: true, Argc: argc}, true
	}
	return ssa.CallSite{}, false
}

// Closure is ssa.ClosureFeedback's: closure_w's closure at the program's
// instruction pc made natively from a pool of closures of its template, as
// the stack form's (jitFeedback's Closure): its captured registers' cells
// those of the code's (ir.Program's Cells), by their place among the
// captured registers (jitRegCapturedRegs), its maker's captured bindings'
// from the closure's own; an arrow's this its maker's register 1. Not
// one that reads its maker's new.target or super; nor in a derived
// constructor's code, nor an inlined callee's.
func (f *jitRegFeedback) Closure(pc int) (ssa.ClosureSite, bool) {
	fb := f.fb
	w, op, ok := f.at(pc)
	if !ok || op != ir.Closure || w.Op() != bytecode.RClosureW || fb.e == nil || fb.root != nil || fb.cl == nil ||
		fb.fn.Kind == bytecode.KindDerivedConstructor {
		return ssa.ClosureSite{}, false
	}
	k := int(w.BC())
	if k >= len(fb.cl.consts) || k >= len(f.rc.Caps) || jitNeedsMaker(fb.cl.consts[k]) {
		return ssa.ClosureSite{}, false
	}
	t := fb.cl.consts[k].template()
	if t == nil {
		return ssa.ClosureSite{}, false
	}
	regs := jitRegCapturedRegs(f.p.Captured)
	caps := make([]int32, len(t.fn.Upvalues))
	for i, desc := range t.fn.Upvalues {
		if !desc.FromParent {
			caps[i] = int32(desc.Index) << 1
			continue
		}
		if i >= len(f.rc.Caps[k]) {
			return ssa.ClosureSite{}, false
		}
		at := slices.Index(regs, int(f.rc.Caps[k][i]))
		if at < 0 {
			return ssa.ClosureSite{}, false
		}
		caps[i] = int32(at)<<1 | 1
	}
	l := fb.r.jitRegClosureLiteral(fb.e, fb.cl, int(f.p.Origin[pc]), k)
	if l == nil {
		return ssa.ClosureSite{}, false
	}
	fb.keep().pools = append(fb.keep().pools, l.pool)
	// An arrow's this is the maker's register 1, as closure_w takes it
	// (regClosure), not the context's This, which register code's frame
	// does not hold (ssa.ClosureSite's LexThisReg). An arrow an arrow
	// makes too, as the stack form's: where the maker shares a derived
	// constructor's binding (funcExtra's lexThisRef), its code runs only
	// once the binding is bound, which is then fixed -- a second super()
	// throws -- and its register 1 the value bound (jitRegThisBound), so
	// that capturing the value is exact.
	arrow := t.fn.Kind == bytecode.KindArrow
	return ssa.ClosureSite{Pool: uintptr(unsafe.Pointer(l.pool)), Captures: caps, LexThis: arrow, LexThisReg: arrow}, true
}

// jitRegClosureLiteral is the pool of closures of cl's constant k, its
// template, closure_w's at register pc rpc of cl's register code, in e,
// made if e has none, as the stack form's jitLiteralAt makes a closure's.
func (r *Runtime) jitRegClosureLiteral(e *jitEntry, cl *closure, rpc, k int) *jitLiteral {
	for i := range e.literals {
		if int(e.literals[i].pc) == rpc && e.literals[i].fn == cl.fn {
			return &e.literals[i]
		}
	}
	tmpl := cl.consts[k]
	e.literals = append(e.literals, jitLiteral{pc: int32(rpc), pool: new(abi.ObjectPool), fn: cl.fn, tmpl: tmpl, env: cl.env})
	l := &e.literals[len(e.literals)-1]
	l.pool.Source = unsafe.Pointer(&jitPoolSource{tmpl: tmpl, fn: cl.fn, env: cl.env})
	r.jitFillLiteral(l)
	return l
}

// CellsPool is ssa.CellsFeedback's: the pool the code takes its captured
// registers' cells from (ir.Program's Cells), at its entries and scopes'
// ends, as the stack form's (jitFeedback's CellsPool): the captured
// registers in their cells' order, each one's cell made holding the
// register's value at the entry -- an argument, or undefined past them.
func (f *jitRegFeedback) CellsPool() (uintptr, []int32, bool) {
	fb := f.fb
	if fb.e == nil || fb.root != nil || fb.cl == nil || !f.p.Cells {
		return 0, nil, false
	}
	regs := jitRegCapturedRegs(f.p.Captured)
	if n := len(regs); n == 0 || n > 4 {
		return 0, nil, false
	}
	cells := make([]int32, len(regs))
	for i, reg := range regs {
		cells[i] = int32(reg)<<1 | 1
	}
	e := fb.e
	if e.cellsPool == nil {
		e.cellsPool = &abi.ObjectPool{Size: abi.PoolSize, Cells: uint64(len(regs))}
		jitFillCells(e.cellsPool)
	}
	fb.keep().pools = append(fb.keep().pools, e.cellsPool)
	return uintptr(unsafe.Pointer(e.cellsPool)), cells, true
}

// jitRegFrameless makes register instruction op, whole, for native code
// in ctx without its frame where that runs nothing and throws nothing, as
// the stack form's operations are made (jitOpPlain, jitOpFrameless), each
// as the stack form's instruction of it: an object's spread -- learning
// the layouts' copy native code makes next (jitCopyLearn) -- an array's
// spread of a dense array's elements, an element appended, an object
// pattern's rest, a computed key converted or defined, a literal's
// __proto__. Its operands are the instruction's registers (compile's
// RegAccess), in their order, the stack form's operands'; its result goes
// to the call's keep cell, which the code reads where the instruction
// writes a register. It reports whether it made it; what it does not
// make, the instruction run whole on the frame (jitRegWhole) makes.
func (r *Runtime) jitRegFrameless(ctx *abi.Context, op bytecode.ROp) bool {
	var in bytecode.Instr
	n := int(ctx.GoOp >> abi.GoOpArgcShift & 0xff)
	switch op {
	case bytecode.RCopyDataProps:
		in.Op = bytecode.OpCopyDataProps
	case bytecode.RArraySpread:
		in.Op = bytecode.OpArraySpread
	case bytecode.RArrayPush:
		in.Op = bytecode.OpArrayPush
	case bytecode.RSetProtoOf:
		in.Op = bytecode.OpSetProtoOf
	case bytecode.RObjectRest:
		// The source, then the keys it leaves out.
		in.Op, in.A = bytecode.OpObjectRest, uint32(n-1)
	case bytecode.RDefineIndex:
		// Its attributes are a literal's, memberFlags(0), as
		// regDefineIndex defines it.
		in.Op = bytecode.OpDefineIndex
	case bytecode.RToPropertyKey:
		in.Op = bytecode.OpToPropertyKey
	default:
		return false
	}
	if n < 1 || n != jitcompile.GoOperands(in) {
		return false
	}
	args := unsafe.Slice((*Value)(unsafe.Pointer(&ctx.GoArgs[0])), n)
	v, ok := r.jitOpFrameless(nil, nil, in, args)
	if !ok {
		return false
	}
	*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = v
	return true
}

// regWholeSite is the call of Go a register instruction run whole makes
// (ssa.CallSite's Whole), with what native code makes of it itself first,
// as the stack form's sites of the same operations have it: a for...of's
// cursor taken from a generator's room (ForOf), a for...in's made without
// a frame (ForIn), an array spread by its elements (ArrayIter), an element
// appended (Append), an object spread by the layouts Go has copied
// (Copies), a literal's field defined along its shape's transition
// (DefineNext), a computed key that is a string its own (StringSelf).
func (f *jitRegFeedback) regWholeSite(pc int, w bytecode.RInstr) []ssa.CallSite {
	_, argc, _, ok := f.p.Code[pc].WholeOperands(f.p.Regs)
	if !ok {
		return nil
	}
	site := ssa.CallSite{Go: ssa.GoCall, Whole: true, Argc: argc, ThisSlot: -1}
	r := f.fb.r
	switch op := w.Op(); {
	case argc == 1 && op == bytecode.RForOfStart:
		// A for...of's cursor, of the iterable: one over a generator with
		// room for it native code takes from the generator, by the
		// runtime's cache of how generators iterate (mir's genForOf), as
		// the stack form's for_of_start's site; Go makes any other, an
		// array's without a frame (jitForOfPlain).
		site.ForOf = true
		if r.genNextFn != nil && r.genIterFn != nil {
			site.GenIter, site.GenIterFn, site.GenNext = uintptr(unsafe.Pointer(&r.genIteration)), uintptr(unsafe.Pointer(r.genIterFn)), f.GenNext()
		}
	case argc == 1 && op == bytecode.RForInStart:
		// A for...in's cursor, of the object, which Go makes without a
		// frame where that runs nothing (abi.GoCallForIn, jitForInPlain),
		// as the stack form's for_in_start's site; its steps native code
		// makes over the keys of its object's layout (mir's forInStep).
		site.ForIn = true
	case argc == 1 && op == bytecode.RToPropertyKey:
		// A computed key: a string is its own, which native code tells
		// (StringSelf), as the stack form's to_property_key's site.
		site.StringSelf = true
	case argc != 2:
		return nil
	case op == bytecode.RDefineField:
		// A literal's field along its shape's first transition by the
		// key (DefineNext), as the stack form's define_field's.
		key, ok := f.name(int(f.p.Origin[pc]), w.C())
		if !ok || key >= 1<<24 {
			return nil
		}
		site.Key, site.DefineNext = uint32(key), true
	case op == bytecode.RArraySpread:
		site.ArrayIter = uintptr(unsafe.Pointer(&r.jit.arrayIter))
	case op == bytecode.RArrayPush:
		site.Append = true
	case op == bytecode.RCopyDataProps:
		if r.jit.copies == nil {
			r.jit.copies = new([jitCopyEntries]jitCopyEntry)
		}
		site.Copies = uintptr(unsafe.Pointer(r.jit.copies))
	default:
		return nil
	}
	return []ssa.CallSite{site}
}

// jitRegReadSeen learns, after Go made a property's read native code did
// not find -- a megamorphic site's miss, a receiver no case of the site's
// is of -- at register pc rpc of run's code, of receiver recv, what the read
// met, as the stack form's code learns it at its calls of Go
// (jitPropertySeen, jitPolySeen): the megamorphic cache filled for the
// receiver's shape, a case its cache cannot hold (a typed array's method,
// found past the two prototypes its cache holds, or on objects no cache
// holds), which the code compiled again reads natively, stale from here.
// Its receiver is the call's operand: the frame's register need not hold
// it, native code keeping values in the frame's registers as it found them.
func (r *Runtime) jitRegReadSeen(f *frame, c *regCode, run jitRegRun, rpc int, recv Value) {
	op, ok := r.jitRegPropOp(f, c, run.wb, rpc)
	if !ok || op.write || run.e == nil || run.e.invalid {
		return
	}
	op.recv = recv
	l := r.jit.regLearnedOf(r.jitRegKeyOf(f.cl))
	r.jitPolySeenOp(l, rpc, op)
	if l.polyReopt {
		l.polyReopt = false
		r.jitRegReopt(f.cl, run.e)
	}
}

// PureBody is ssa.PureBodyFeedback's: the function is a pure body
// (bytecode.LeafPure, which the compiler finds of its code, no tier's
// marks), whose operators' sites register code learns only at its exits:
// an operand native code already holds as a BigInt makes the operator
// one of BigInts there, as the stack form's pure body does. The stack
// form's answer leaves out a body the tree tier runs, whose sites the
// tree marks; register code is run by no tier that marks its sites.
func (f *jitRegFeedback) PureBody() bool {
	fn := f.fb.fn
	return fn != nil && fn.Leaf == bytecode.LeafPure && !fn.Generator && !fn.Async
}

// jitRegGenMakeable reports whether register code of cl's may make the
// generators of o natively, as a call of it makes one (newRegGenerator),
// as jitGenMakeable asks of the stack form's: a synchronous generator
// function of register code, of cl's realm and scope, unbound, whose
// window binds its parameters with nothing run -- end_params right after
// gen_entry -- and keeps no arguments; no arguments object, no direct
// eval, no receiver coerced that its body reads, no rare part; its
// prototype its own data property holding an object.
func (r *Runtime) jitRegGenMakeable(o *Object, cl *closure) bool {
	fd := o.fn()
	if fd == nil || fd.native != nil || fd.bound || fd.arrow || fd.closure == nil || fd.closure.realm != r.Realm || cl == nil ||
		fd.closure.scope() != cl.scope() || r.debug != nil {
		return false
	}
	c := r.regGenOf(fd.closure, o)
	fn := fd.closure.fn
	if c == nil || c.args || len(c.rc.Code) < 2 || regAt(c.code, 1).Op() != bytecode.REndParams ||
		!fn.Generator || fn.Async || fn.UsesArguments || fn.HasDirectEval || fn.UsesThis && fn.CoerceThis {
		return false
	}
	if x := fd.extra; x != nil && (len(x.with()) != 0 || x.lexThisRef != nil || x.evalVars() != nil) {
		return false
	}
	p := o.getOwn(atomPrototype)
	return p.ok() && p.flags()&(propAccessor|propPrivate|propDeleted) == 0 && p.value().IsObject()
}

// jitRegGenMakeSeen learns, after Go made the call at register pc rpc of
// f's code e of callee, a generator function register code may make the
// generators of (jitRegGenMakeable), which the next compile makes from a
// pool (regGenMakeSite), as the stack form's code learns it
// (jitGenMakeSeen); or one it makes so already, whose pool ran out, which
// it fills again.
func (r *Runtime) jitRegGenMakeSeen(f *frame, e *jitRegEntry, rpc int, callee Value) {
	if !callee.IsObject() || e == nil || e.invalid {
		return
	}
	o := callee.Object()
	if fd := o.fn(); fd == nil || fd.closure == nil || !fd.closure.fn.Generator {
		return
	}
	l := r.jit.regLearnedOf(r.jitRegKeyOf(f.cl))
	for _, x := range l.genMakes {
		if int(x.pc) == rpc {
			if x.obj == o && x.pool != nil && x.pool.Count == 0 {
				r.jitRefillPool(x.pool)
			}
			return
		}
	}
	if r.jitRegGenMakeable(o, f.cl) {
		l.genMakes = append(l.genMakes, jitGenMake{pc: int32(rpc), obj: o})
		r.jitRegGenLearned(f.cl, e)
	}
}

// regGenMakeSite gives site, the call of Go of call_w or call_method_w at
// register pc rpc, the generators of the function it learned it calls
// (jitRegGenMakeSeen), from their pool, as the stack form's NativeCalls
// does (ssa.CallSite's GenMake): their arguments written to the
// generator's registers from the window's register 2 (GenArgAt), a
// method's receiver to its register 1 too (GenThisAt), where its window
// keeps it.
func (f *jitRegFeedback) regGenMakeSite(rpc int, site *ssa.CallSite) {
	fb := f.fb
	if fb.e == nil || site.New {
		return
	}
	for _, x := range fb.e.genMakes {
		if int(x.pc) != rpc || !fb.r.jitRegGenMakeable(x.obj, fb.cl) {
			continue
		}
		pool := fb.r.jitGenMakePool(fb.e, rpc, x.obj)
		i := x.obj.findOwn(atomPrototype)
		c := fb.r.regGenOf(x.obj.fn().closure, x.obj)
		if pool == nil || i < 0 || c == nil {
			continue
		}
		k := fb.keep()
		k.pools, k.holders = append(k.pools, pool), append(k.holders, x.obj)
		from := int(c.genFrom)
		site.GenMake, site.Pool, site.ProtoIndex, site.ProtoKey = uintptr(unsafe.Pointer(x.obj)), uintptr(unsafe.Pointer(pool)), int(i), uint32(atomPrototype)
		site.Params, site.LocalCount, site.GenArgAt = int(c.params), int(c.numRegs)-from, 2-from
		if from == 0 {
			site.GenThisAt = 2
		}
		return
	}
}

// jitRegExpLeafGive gives pool back recv, the RegExp of its literal, a
// regexp_lit's leaf's call of exec or test -- of test where test is set --
// on which, given args, has just returned (abi.GoCallState's Give), as the
// call of Go the leaf stands for gives the literal its RegExp back
// (regMarkedCall) and the stack form's gives it its pool's
// (jitRegExpLitDone): the call ran nothing of the script's and keeps
// nothing of the RegExp, which only ever was its receiver, where reading
// the method off it found the built-in and ran nothing -- RegExp.prototype
// as the realm made it (regexpLitMethod, its layout the realm's) -- and
// the argument is no object. lastIndex is 0 again, as the pool's RegExps
// are made (jitFillLiteral). Given back to its pool rather than to the
// literal's spare (regexpLiteralDone), which a pool's refill takes one of:
// a loop's /x/.exec(s), the leaf's call each time round, had a RegExp
// made by every pool's refill of all of its RegExps.
func (r *Runtime) jitRegExpLeafGive(pool *abi.ObjectPool, test bool, recv Value, args []Value) {
	if !recv.IsObject() || len(args) > 0 && args[0].IsObject() || pool.Count >= pool.Size || pool.Count >= abi.PoolCapacity ||
		r.regexpLitMethod(false) == nil || test && r.regexpLitMethod(true) == nil ||
		r.regexpProtoShape == nil || r.proto.regexp.shape != r.regexpProtoShape {
		return
	}
	src := (*jitPoolSource)(pool.Source)
	o := recv.Object()
	if src == nil || src.lit == nil || o.proto() != (*Object)(pool.Proto) || o.class != ClassRegExp || int(o.valn) != 1 {
		return
	}
	ro := (*regexpObject)(unsafe.Pointer(o))
	if ro.rd.re != &ro.re || ro.rd.realm != r.Realm || ro.side.data != any(&ro.rd) || ro.re.Source() != src.lit.Str {
		return
	}
	o.vals()[0] = Int(0)
	pool.Objects[pool.Count] = unsafe.Pointer(o)
	pool.Count++
}
