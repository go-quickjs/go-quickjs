//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"math"
	"math/bits"
	"slices"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// jitRegFeedback is the JIT's feedback for a function's register code, the
// program compile.LowerReg makes of it (docs/register-interpreter.md,
// "Inline caches and feedback"): asked by the program's instructions, as
// the builder asks, and read through each one's origin, the register pc,
// from the caches the register interpreter fills -- a property site by its
// instruction's C, a global's by its C and name, a call's by its C
// (regCalls).
//
// What the code made from the function's register code learned at its
// exits, by register pc (jitState's regLearned), is its jitFeedback's
// entry: the property sites' cases, primitives and megamorphic marks it
// met, and the register pcs whose guards failed (Generic). jitFeedback
// holds the state its answers share; the methods the builder asks are
// jitRegFeedback's, and the snapshot of a background compile
// (jit_snapshot_gen.go) is generated from them.
type jitRegFeedback struct {
	fb *jitFeedback
	rc *bytecode.RegCode
	p  *ir.Program
	// fwd, for a callee inlined with its call's arguments, which it hands
	// on (ssa.InlineSite's Arguments), is that call: its apply_args are
	// the calls inlined with them (forwardInline).
	fwd *jitForwardFrame
	// asked are the answers Inline and NativeCalls gave, askedAt their
	// indexes by the program's instruction (askedOf).
	asked   []jitRegAsked
	askedAt []int32
	// nat is the record of its function's learning and the interpreter's
	// marks the methods that read them answer from (native).
	nat *jitNativeData
	// calls is the record of its closure's call caches and the realm's
	// built-ins the calls' methods answer from (callsData).
	calls *jitCallsData
}

// jitRegFeedbackFor is cl's register code's feedback for p, the program
// LowerReg made of it.
func (r *Runtime) jitRegFeedbackFor(cl *closure, p *ir.Program) *jitRegFeedback {
	if cl == nil || cl.fn == nil || cl.fn.RegCode() == nil || p == nil || p.Origin == nil {
		return nil
	}
	fb := &jitFeedback{r: r, fn: cl.fn, cl: cl, e: r.jitState().regLearnedOf(r.jitRegKeyOf(cl)), keepLimit: r.jitTest.keepLimit}
	return &jitRegFeedback{fb: fb, rc: cl.fn.RegCode(), p: p}
}

// at is the register instruction of the program's instruction pc, and what
// that instruction is.
func (f *jitRegFeedback) at(pc int) (bytecode.RInstr, ir.Op, bool) {
	return jitRegAt(f.p, f.rc, pc)
}

// jitRegAt is the register instruction of the program p's instruction pc,
// p lowered from rc, and what that instruction is (jitRegFeedback's at).
func jitRegAt(p *ir.Program, rc *bytecode.RegCode, pc int) (bytecode.RInstr, ir.Op, bool) {
	if pc < 0 || pc >= len(p.Code) || int(p.Origin[pc]) >= len(rc.Code) {
		return 0, 0, false
	}
	return rc.Code[p.Origin[pc]], p.Code[pc].Op, true
}

// name is the atom of the register instruction w's name operand n, at
// register pc rpc (bytecode.RNameFar).
func (f *jitRegFeedback) name(rpc int, n uint16) (Atom, bool) {
	i := uint32(n)
	if n == bytecode.RNameFar {
		if rpc >= len(f.rc.FarNames) {
			return 0, false
		}
		i = f.rc.FarNames[rpc]
	}
	if int(i) >= len(f.fb.cl.names) {
		return 0, false
	}
	return f.fb.cl.names[i], true
}

// propertyAt is the property site the program's instruction pc reads or
// writes, of the heap as it is (jitPropCode's propertyAt).
func (f *jitRegFeedback) propertyAt(pc int) (rpc int, key Atom, c *propCache, write, method, plain, ok bool) {
	code := f.propCode()
	return code.propertyAt(pc, jitPropLive{f: f, r: f.fb.r})
}

// propCode is f's function as a property site's question is decoded from
// it (jitPropCode).
func (f *jitRegFeedback) propCode() jitPropCode {
	code := jitPropCode{rc: f.rc, p: f.p}
	if cl := f.fb.cl; cl != nil {
		code.names, code.cl = cl.names, true
	}
	return code
}

// jitPropCode is what of a function a property site's question is decoded
// from: its register code, the program LowerReg made of it, and its
// closure's names, none of which changes once made, so that a worker may
// decode it as the runtime's goroutine does (jit_snapprop.go).
type jitPropCode struct {
	rc    *bytecode.RegCode
	p     *ir.Program
	names []Atom
	// cl says the feedback has a closure, whose names these are.
	cl bool
}

// at is the register instruction of the program's instruction pc, and what
// that instruction is.
func (c *jitPropCode) at(pc int) (bytecode.RInstr, ir.Op, bool) {
	if pc < 0 || pc >= len(c.p.Code) || int(c.p.Origin[pc]) >= len(c.rc.Code) {
		return 0, 0, false
	}
	return c.rc.Code[c.p.Origin[pc]], c.p.Code[pc].Op, true
}

// propertyAt is the property site the program's instruction pc reads or
// writes -- get_prop's, get_method's, get_method_w's, set_prop's, or
// add_prop's read or store, which share the store's site -- its register
// pc, its key, its cache by the register instruction's C, as src has it
// (nil past the closure's caches), and whether it is a write, a method's
// read, or a plain read (get_prop's).
func (c *jitPropCode) propertyAt(pc int, src jitPropSrc) (rpc int, key Atom, cache *propCache, write, method, plain, ok bool) {
	w, op, ok := c.at(pc)
	if !ok || !c.cl {
		return
	}
	if w.Op() == bytecode.RLit {
		// regexp_lit's read of its method (regLitCache).
		if op != ir.ReferenceRead {
			return 0, 0, nil, false, false, false, false
		}
		rpc = int(c.p.Origin[pc])
		key, cache, ok = src.litRead(rpc, w)
		if !ok {
			return 0, 0, nil, false, false, false, false
		}
		return rpc, key, cache, false, true, false, true
	}
	switch w.Op() {
	case bytecode.RGetProp, bytecode.RGetPropNext, bytecode.RGetMethod, bytecode.RGetMethodW, bytecode.RSetProp, bytecode.RAddProp, bytecode.RAddPropI:
	default:
		return 0, 0, nil, false, false, false, false
	}
	if op != ir.PropertyRead && op != ir.ReferenceRead && op != ir.PropertyWrite {
		return 0, 0, nil, false, false, false, false
	}
	site := int(w.C())
	if site >= len(c.rc.SiteName) || int(c.rc.SiteName[site]) >= len(c.names) {
		return 0, 0, nil, false, false, false, false
	}
	key = c.names[c.rc.SiteName[site]]
	cache = src.cache(site)
	write = op == ir.PropertyWrite
	method = w.Op() == bytecode.RGetMethod || w.Op() == bytecode.RGetMethodW
	plain = w.Op() == bytecode.RGetProp || w.Op() == bytecode.RGetPropNext
	return int(c.p.Origin[pc]), key, cache, write, method, plain, true
}

// regLitCache is the cache of regexp_lit's read of its method, exec or
// test, of its literal's RegExp (compile's regexpLit), the register
// instruction w at register pc rpc, which no cache of the interpreter's
// has: a cache of the code's own, filled by a read of the method off a
// RegExp of the literal -- or none where it is found an accessor, which a
// read would call before the call it ends in (ir's windowLast) -- and the
// method's key.
func (f *jitRegFeedback) regLitCache(rpc int, w bytecode.RInstr) (key Atom, c *propCache, ok bool) {
	fb, r := f.fb, f.fb.r
	consts, k := regConsts(fb.cl.fn, f.rc), int(w.C())
	if fb.e == nil || !f.regLitFedAt(rpc) || k >= len(consts) {
		return 0, nil, false
	}
	key = atomExec
	if w.X()&bytecode.RLitTest != 0 {
		key = r.atoms.intern("test")
	}
	if fb.e.regLitReads == nil {
		fb.e.regLitReads = map[int32]*propCache{}
	}
	c = fb.e.regLitReads[int32(rpc)]
	if c == nil {
		lit, err := r.newRegExpLiteral(&consts[k])
		if err != nil || !lit.IsObject() {
			return 0, nil, false
		}
		// Filled by the first read, answered by the second.
		c = new(propCache)
		r.cachedProp(c, lit.Object(), key)
		if _, found := r.cachedProp(c, lit.Object(), key); !found || c.getter {
			return 0, nil, false
		}
		fb.e.regLitReads[int32(rpc)] = c
	}
	return key, c, true
}

// regKnownRead is a cache for the read of key at register pc rpc, whose own
// cache never filled -- its instruction not reached when the function was
// compiled -- filled from the object a captured binding holds now, which
// the register instruction before it reads into the read's receiver
// register (regDefined). The read's checks catch a receiver of another
// shape, as a filled cache's would. It is kept with regexp_lit's caches
// while the function's code lives (regLitReads). Without it, RegExp's
// runBlock0, compiled in the first of its two dozen loops, had each later
// loop's re.exec deoptimize until it had feedback, and was compiled again
// once a loop. A global's, or a property's of one, is left to the read's
// first run, as a site with no feedback is in V8: the reads of a branch
// that opened after the compile are compiled for at once (feedReopt).
func (f *jitRegFeedback) regKnownRead(rpc int, key Atom) (*propCache, bool) {
	fb := f.fb
	if fb.e == nil || !jitKnownReadAt(f.rc, rpc) {
		return nil, false
	}
	v, ok := f.regDefined(rpc, int(f.rc.Code[rpc].B()))
	if !ok || !v.IsObject() {
		return nil, false
	}
	o := v.Object()
	// Filled by the first read, answered by the second.
	c := new(propCache)
	fb.r.cachedProp(c, o, key)
	if _, found := fb.r.cachedProp(c, o, key); !found || c.getter {
		return nil, false
	}
	if fb.e.regLitReads == nil {
		fb.e.regLitReads = map[int32]*propCache{}
	}
	fb.e.regLitReads[int32(rpc)] = c
	return c, true
}

// jitKnownReadAt reports whether the read at register pc rpc of rc reads
// what the register instruction before it reads of a captured binding, as
// regKnownRead asks.
func jitKnownReadAt(rc *bytecode.RegCode, rpc int) bool {
	if rpc < 1 || rpc >= len(rc.Code) {
		return false
	}
	op := rc.Code[rpc-1].Op()
	return op == bytecode.RGetUpval || op == bytecode.RGetUpvalCheck
}

// jitKnownReadWanted reports whether a read whose cache is c, a write's
// where write says, is answered by a cache of its own (regKnownRead): a
// read whose cache never filled.
func jitKnownReadWanted(c *propCache, write bool) bool {
	return !write && (c.shape == nil || c.shape == noShape) && c.fills == 0
}

// Property is ssa.Feedback's: the property site of a read or a write the
// program's instruction pc makes (jitPropertyOf), of the heap as it is.
func (f *jitRegFeedback) Property(pc int) (ssa.PropertySite, bool) {
	code := f.propCode()
	return jitPropertyOf(&code, pc, jitPropLive{f: f, r: f.fb.r}, f.fb)
}

// jitPropertyOf is Property's answer at the program's instruction pc of
// code, from what src has of the caches, of what the code learned and of
// the heap, what it keeps appended to fb's lists: the property site of a
// read or a write (propertyAt), as its cache knows it, with the cases the
// code met at its exits (jitRegDeoptSeen, jitRegHostSeen), a read's past
// what a cache holds, or a write's additions. A cache that gave up, or
// holds dictionary objects or a chain's cell, or a site that met more than
// its cases hold, is megamorphic; a read whose cache never filled searches
// the receiver's own table (Scan). The feedback derives it so from the
// live caches, a snapshot's worker from their copy (jit_snapprop.go).
func jitPropertyOf(code *jitPropCode, pc int, src jitPropSrc, fb *jitFeedback) (ssa.PropertySite, bool) {
	if w, op, ok := code.at(pc); ok && (w.Op() == bytecode.RGetPrivate || w.Op() == bytecode.RSetPrivate) && code.p.Code[pc].Private {
		return jitPrivatePropertyOf(code, pc, op, src, fb)
	}
	rpc, key, cache, write, _, plain, ok := code.propertyAt(pc, src)
	if !ok {
		return ssa.PropertySite{}, false
	}
	if !write {
		if site, ok := src.forward(rpc, key); ok {
			return site, true
		}
	}
	if cache == nil {
		return ssa.PropertySite{Key: uint32(key)}, true
	}
	if jitKnownReadWanted(cache, write) {
		if c, ok := src.knownRead(rpc, key); ok {
			cache = c
		}
	}
	x := src.ctx()
	site, ok := fb.siteFrom(key, write, cache, src)
	if !ok {
		return site, false
	}
	p := src.polyAt(rpc)
	deep := false
	if !write && site.Shape == 0 && site.Holders[0].Object == 0 && (cache.shape == nil || cache.shape == noShape || cache.cell != nil) {
		// A read its cache could not hold, the property past the two
		// prototypes it holds: compiled for the first of the shapes it
		// met, the others its cases.
		if cases := fb.deepCasesOf(&x, p, cache); len(cases) != 0 {
			k := cases[0]
			site.Shape, site.Index, site.Holders, site.More, site.Cases = k.Shape, k.Index, k.Holders, k.More, cases[1:]
			deep = true
		}
	}
	if write {
		site.Cases, site.Adds = fb.writeCases(p, site, src)
	} else if site.Shape != 0 && !deep {
		site.Cases = fb.cases(p, site.Shape)
	}
	site.Megamorphic = megamorphicOf(&x, p, cache, write, site)
	site.Primitives = p != nil && p.prim
	if write && cache.jitPrimWriteSeen() && !x.primLeave {
		// A write the interpreter has seen make of a primitive's property
		// (jitNotePrimWrite): Go's from native code, as one the code met
		// at an exit is (jitRegHostSeen). Learned only at exits, each site
		// had deoptimized once: carried_exit_loops' 24 loops, a number's
		// write in each, had their function compiled 24 times more, the
		// interpreter running the rest of each invocation that failed.
		site.Megamorphic, site.Primitives = true, true
	}
	if plain && !site.Megamorphic && cache.fills == 0 {
		site.Scan = true
	}
	return site, true
}

// jitPrivatePropertyOf is Property's site for get_private's or
// set_private's read or write, the program's instruction pc of code,
// lowered to op: the property of the key its site's cache (Aux's high 16
// bits) met it under -- a symbol each evaluation of the class mints, which
// the code checks the key's register holds (ssa.PropertySite's Private) --
// in the objects of the shapes the cache holds of it, each an own data
// property, a write's writable: the entry's first, the rest its cases. A
// cache that holds none -- or whose first is another evaluation's key's --
// answers nothing, and the site is Go's, whose read or write fills the
// cache (regGetPrivate, regSetPrivate) for the code compiled next.
func jitPrivatePropertyOf(code *jitPropCode, pc int, op ir.Op, src jitPropSrc, fb *jitFeedback) (ssa.PropertySite, bool) {
	rpc := int(code.p.Origin[pc])
	if !code.cl || rpc >= len(code.rc.Aux) {
		return ssa.PropertySite{}, false
	}
	site := int(code.rc.Aux[rpc] >> 16)
	c := src.cache(site)
	if site == 0 || c == nil {
		return ssa.PropertySite{}, false
	}
	cases, atom, ok := jitPrivateCases(c, op == ir.PropertyWrite)
	if !ok {
		return ssa.PropertySite{}, false
	}
	sym := src.symbol(atom)
	if sym == nil {
		return ssa.PropertySite{}, false
	}
	ps := ssa.PropertySite{Key: uint32(atom), Private: uintptr(unsafe.Pointer(sym))}
	k := fb.keep()
	for i, c := range cases {
		k.shapes = append(k.shapes, src.keep(c.shape))
		if i == 0 {
			ps.Shape, ps.Index = uintptr(unsafe.Pointer(c.shape)), c.idx
			continue
		}
		ps.Cases = append(ps.Cases, ssa.PropertyCase{Shape: uintptr(unsafe.Pointer(c.shape)), Index: c.idx})
	}
	return ps, true
}

// jitPrivateCases are the entries of a private member's site, its cache c,
// a write's or a read's, that jitPrivatePropertyOf compiles for, and the
// key of the first: those of an own data property, a write's writable, of
// that key -- an object of another evaluation's shape is Go's, whose brand
// check throws.
func jitPrivateCases(c *propCache, write bool) ([]propCache, Atom, bool) {
	var cases []propCache
	own := func(e *propCache) {
		sh := e.shape
		if sh == nil || sh == noShape || sh == dictShape || e.ownShape != sh || e.getter || e.idx < 0 || e.idx >= sh.n || sh.loose {
			return
		}
		if p := sh.keys[e.idx]; p.isAccessor() || p.flags&(propDeleted|propUninit) != 0 || write && p.flags&propWritable == 0 {
			return
		}
		cases = append(cases, propCache{shape: sh, idx: e.idx})
	}
	own(c)
	if p := c.poly; p != nil {
		for i := range p.n {
			own(&p.entries[i])
		}
	}
	if len(cases) == 0 {
		return nil, 0, false
	}
	atom := cases[0].shape.keys[cases[0].idx].key
	cases = slices.DeleteFunc(cases, func(c propCache) bool { return c.shape.keys[c.idx].key != atom })
	return cases, atom, true
}

// StringProperty is ssa.StringPropertyFeedback's: a method's read whose
// cache holds no shape, of a method of the prototype of the primitive its
// receivers have been -- a string's, or the BigInt's, number's or boolean's
// the code met at its exits -- read off that prototype (jitFeedback's
// stringPropertyOf).
func (f *jitRegFeedback) StringProperty(pc int) (ssa.PropertySite, bool) {
	rpc, key, cache, _, method, _, ok := f.propertyAt(pc)
	if !ok || !method || cache == nil {
		return ssa.PropertySite{}, false
	}
	return f.stringPropertyAt(rpc, key, cache)
}

// stringPropertyAt is StringProperty's answer for the method's read of key
// at register pc rpc, its cache cache, as accessorAt is Accessor's.
func (f *jitRegFeedback) stringPropertyAt(rpc int, key Atom, cache *propCache) (ssa.PropertySite, bool) {
	if f.fb.e == nil {
		return ssa.PropertySite{}, false
	}
	return f.fb.stringPropertyOf(rpc, cache, key, false)
}

// Accessor is ssa.AccessorFeedback's: the getter a plain read found on its
// receivers' prototypes, or the setter a write found (jitFeedback's
// accessorOf): from its cache, the entries it held before, and what the
// code met at its exits. Go calls it from native code, or the code inlines
// it (accessorInline).
func (f *jitRegFeedback) Accessor(pc int) (ssa.AccessorSite, bool) {
	rpc, key, cache, write, _, plain, ok := f.propertyAt(pc)
	if !ok || cache == nil || !write && !plain {
		return ssa.AccessorSite{}, false
	}
	return f.accessorAt(rpc, key, cache, write)
}

// accessorAt is Accessor's answer for the read or write of key at register
// pc rpc, its cache cache: of the register code, which a snapshot's
// gather asks it by (gatherPropertyAsked).
func (f *jitRegFeedback) accessorAt(rpc int, key Atom, cache *propCache, write bool) (ssa.AccessorSite, bool) {
	a, callee, cl, ok := f.fb.accessorOf(rpc, cache, key, write)
	if !ok || cl == nil {
		return a, ok
	}
	if site, ok := f.accessorInline(callee, cl, write); ok {
		a.Inline = &site
	}
	// Its native call, where the builder does not inline it -- past the
	// inlining's budget, a getter's loop among them.
	if site, ok := f.accessorCall(callee, cl, write); ok {
		a.Call = &site
	}
	return a, true
}

// accessorCall is the native call of the getter or setter callee, cl's
// function object, a register read or write makes where it is not inlined
// (AccessorSite's Call), through its function's link, as a method's call
// of it is (regCallSite): register code native callers may call, not an
// arrow. The read or write has no window of its registers; the call makes
// its callee's past the VM's stack's top, as every register call does --
// the function in its register 0 where it reads it, the receiver in 1, a
// setter's value in 2 (mir's windowAccessor) -- Window marking it register
// code's. A callee with no code native callers may call yet Go runs from
// native code at each call (the link's entry 0) and compiles for them
// (jitRegKnownCallee), as it does a call's callee.
func (f *jitRegFeedback) accessorCall(callee *Object, cl *closure, write bool) (ssa.CallSite, bool) {
	r := f.fb.r
	argc := jitB2i(write)
	if cl.reg == nil || !r.jitRegCallable(callee, cl, cl.reg, argc) || callee.fn().arrow {
		return ssa.CallSite{}, false
	}
	rc := cl.reg
	l := r.jitState().regLinkOf(cl, rc)
	k := f.fb.keep()
	k.links = append(k.links, l)
	k.holders = append(k.holders, callee)
	site := ssa.CallSite{
		Callee: uintptr(unsafe.Pointer(callee)), Closure: uintptr(unsafe.Pointer(cl)),
		Entry: uintptr(unsafe.Pointer(&l.entry)), Count: uintptr(unsafe.Pointer(&l.in)),
		Argc: argc, Method: true, Params: 2 + int(rc.params), LocalCount: int(rc.numRegs), ThisSlot: -1,
		Window: 1, FrameArgs: rc.args, ReadsFunction: jitFrameCallee(cl.fn),
	}
	if cl.fn.HasTailCall {
		// As a method call's: its calls in tail position may give its
		// frame up.
		site.Tail, site.TailFrame = true, true
	}
	return site, true
}

// accessorInline is the getter or setter callee, cl's function object, of
// a register read or write, to inline (AccessorSite's Inline), as a
// method's call of it is (Inline): register code native callers may call,
// not the compiled function's, nor an arrow, nor one that keeps its
// arguments (regCode's args): the builder leaves such a callee's
// arguments.length and arguments[k] to an exit, whose frame of the
// inlined level is given the arguments of a call's window alone
// (jitRegInlineArgs) -- a setter's, inlined, counted none. Its window the
// function, from a cell (InlineSite's FunctionCell), the receiver, a
// setter's value.
func (f *jitRegFeedback) accessorInline(callee *Object, cl *closure, write bool) (ssa.InlineSite, bool) {
	r := f.fb.r
	argc := jitB2i(write)
	if cl.reg == nil || !r.jitRegCallable(callee, cl, cl.reg, argc) || callee.fn().arrow || cl.fn == f.fb.fn || cl.fn == f.fb.keep().fn ||
		r.jitNoInlined(cl.fn) || cl.reg.args {
		return ssa.InlineSite{}, false
	}
	p, err := f.fb.r.jitRegLower(cl, cl.reg.rc, jitLowerInline)
	if err != nil {
		return ssa.InlineSite{}, false
	}
	r.jitRegSeenInlined(f.fb.keep(), cl)
	k := f.fb.keep()
	cell := new(Value)
	*cell = Obj(callee)
	k.cells = append(k.cells, cell)
	k.holders = append(k.holders, callee)
	k.inlined = append(k.inlined, cl)
	return ssa.InlineSite{
		Program: p, Feedback: r.jitRegCalleeOf(k, cl, p),
		Callee: uintptr(unsafe.Pointer(callee)), Closure: uintptr(unsafe.Pointer(cl)),
		Argc: argc, Method: true, Params: 2 + int(cl.reg.params), ThisSlot: -1, Window: true,
		Upvalues: jitUpvalueCells(cl), FunctionCell: uintptr(unsafe.Pointer(cell)),
	}, true
}

// Global is ssa.Feedback's: the global a read, a write or a strict
// assignment's check at the program's instruction pc names, found where
// the global object or a script's lexical bindings have it, starting from
// the slot its cache, by the register instruction's C, remembers.
func (f *jitRegFeedback) Global(pc int) (ssa.GlobalSite, bool) {
	w, op, ok := f.at(pc)
	if !ok || f.fb.cl == nil {
		return ssa.GlobalSite{}, false
	}
	rpc := int(f.p.Origin[pc])
	site, write, check := int(w.C()), false, false
	switch w.Op() {
	case bytecode.RGetGlobal, bytecode.RGetGlobalInstanceOf:
	case bytecode.RSetGlobal:
		write = true
	case bytecode.RGetGlobalOpt:
		// typeof's read, which has no site of its own.
		site = -1
	case bytecode.RCheckGlobalRef:
		check = true
	default:
		return ssa.GlobalSite{}, false
	}
	if op != ir.BindingRead && op != ir.BindingWrite && op != ir.BindingCheck {
		return ssa.GlobalSite{}, false
	}
	name, ok := f.name(rpc, w.B())
	if !ok {
		return ssa.GlobalSite{}, false
	}
	return f.fb.globalSite(name, site, write, check)
}

// CallShape is ssa.CalledClosureFeedback's: the argument count of the call
// at the program's instruction pc, and whether it is a method's -- call_w's
// and call_method_w's, not one in tail position, nor, in the function's own
// code, one whose inlining left native code too often (jitInlineLeft).
func (f *jitRegFeedback) CallShape(pc int) (int, bool, bool) {
	return f.native().callShape(pc)
}

// CalledClosure is ssa.CalledClosureFeedback's: the closure closure_w makes
// at the program's instruction pc, which the code's calls of it -- its own,
// or a callee's it is given to -- may inline, leaving it unmade (ssa's
// calledClosures): its template, which the code keeps, as the inlined
// frames' closure an exit names, and what it captures -- a register of its
// maker's (the register code's Caps), or one of the maker's own captured
// bindings -- and its function's InlineSite (madeInline); and the
// instructions of the maker's that close what it captures at a scope's end
// (close_regs, which the register form's lowering leaves to Go whole,
// naming no register the builder sees), past which a call inlined would
// read the binding the scope's next entry makes, the closure the one it
// took (MadeCallback's Closes). Not an arrow that reads its maker's
// new.target or super (jitNeedsMaker). In the function's own code alone.
func (f *jitRegFeedback) CalledClosure(pc int) (ssa.MadeCallback, ssa.InlineSite, bool) {
	fb := f.fb
	w, op, ok := f.at(pc)
	if !ok || op != ir.Closure || w.Op() != bytecode.RClosureW || fb.e == nil || fb.root != nil || fb.cl == nil {
		return ssa.MadeCallback{}, ssa.InlineSite{}, false
	}
	k := int(w.BC())
	if k >= len(fb.cl.consts) || k >= len(f.rc.Caps) {
		return ssa.MadeCallback{}, ssa.InlineSite{}, false
	}
	c := jitTemplateCell[struct{}](fb, &fb.cl.consts[k])
	t := c.template()
	if t == nil || jitNeedsMaker(*c) {
		return ssa.MadeCallback{}, ssa.InlineSite{}, false
	}
	caps, ok := f.madeCaptures(t, k)
	if !ok {
		return ssa.MadeCallback{}, ssa.InlineSite{}, false
	}
	site, ok := f.madeInline(t)
	if !ok {
		return ssa.MadeCallback{}, ssa.InlineSite{}, false
	}
	k2 := fb.keep()
	if !slices.Contains(k2.made, t) {
		k2.inlined = append(k2.inlined, t)
		k2.made = append(k2.made, t)
	}
	return ssa.MadeCallback{PC: pc, Template: uintptr(unsafe.Pointer(c)), Closure: uintptr(unsafe.Pointer(t)), Captures: caps}, site, true
}

// madeCaptures are what t, the template of the maker's constant k, captures
// (ssa.MadeCallback's Captures): a register of the maker's, by its code's
// Caps, index<<1|1, or one of the maker's own captured bindings, index<<1.
func (f *jitRegFeedback) madeCaptures(t *closure, k int) ([]int32, bool) {
	caps := make([]int32, len(t.fn.Upvalues))
	for i, desc := range t.fn.Upvalues {
		if !desc.FromParent {
			caps[i] = int32(desc.Index) << 1
			continue
		}
		if k >= len(f.rc.Caps) || i >= len(f.rc.Caps[k]) {
			return nil, false
		}
		caps[i] = int32(f.rc.Caps[k][i])<<1 | 1
	}
	return caps, true
}

// madeInline is the InlineSite of t's function, a closure the function
// makes (CalledClosure), to inline as a plain call of it, under the rules
// a register callee's inlining follows (Inline): register code of the
// form's own layout a call enters, its arguments kept only to be read
// (ReadsArguments), not the function's own; its window the call's -- the
// function, a plain call's receiver, which a callee reading it is given
// (InlineSite's ThisCell), the arguments -- and no closure object: the
// builder's to know. An arrow that
// reads its this reads the maker's, its register 1, as closure_w gives it
// the frame's register 1 (InlineSite's LexThis), as V8's inlined arrow
// reads a context slot of its maker's -- not in a derived constructor,
// whose this may not be bound yet.
func (f *jitRegFeedback) madeInline(t *closure) (ssa.InlineSite, bool) {
	fb := f.fb
	r := fb.r
	fn := t.fn
	rc := fn.RegCode()
	switch fn.Kind {
	case bytecode.KindNormal, bytecode.KindArrow, bytecode.KindMethod:
	default:
		return ssa.InlineSite{}, false
	}
	if rc == nil || !rc.Wide || fn.Async || fn.Generator || fn.HasDirectEval ||
		fn == fb.fn || fn == fb.keep().fn || fb.r.jitNoInlined(fn) {
		return ssa.InlineSite{}, false
	}
	// One that keeps its arguments only to read them by index and count
	// them (jitRegReadsArguments) reads the call's (InlineSite's
	// ReadsArguments), as Inline's callees do; any other that keeps them is
	// called.
	reads := false
	if rc.UsesArguments || rc.ForwardsArguments || rc.KeepsArgs {
		if !jitRegReadsArguments(t) {
			return ssa.InlineSite{}, false
		}
		reads = true
	}
	lexThis := fn.Kind == bytecode.KindArrow && fn.UsesThis
	if lexThis && fb.fn.Kind == bytecode.KindDerivedConstructor {
		return ssa.InlineSite{}, false
	}
	p, err := r.jitRegLower(t, rc, jitLowerInline)
	if err != nil {
		return ssa.InlineSite{}, false
	}
	if t.reg != nil && t.reg.rc == rc {
		// What its sites' caches have seen besides their entries, as an
		// inlined callee's (Inline).
		r.jitRegSeenInlined(fb.keep(), t)
	}
	k := fb.keep()
	in := ssa.InlineSite{
		Program: p, Feedback: &jitRegFeedback{fb: &jitFeedback{r: r, fn: fn, cl: t, e: r.jitState().regLearnedOf(r.jitRegKeyOf(t)), root: k}, rc: rc, p: p},
		Closure: uintptr(unsafe.Pointer(t)), Params: 2 + rc.Params, ThisSlot: -1, Window: true, ReadsArguments: reads,
	}
	switch {
	case lexThis:
		in.LexThis, in.LexThisSlot = true, 1
	case fn.UsesThis && fn.Kind != bytecode.KindArrow:
		// A plain call's receiver: the global object a sloppy callee's is,
		// or undefined.
		cell := new(Value)
		*cell = Undefined
		if fn.CoerceThis {
			*cell = r.globalThis
		}
		k.cells = append(k.cells, cell)
		in.ThisCell = uintptr(unsafe.Pointer(cell))
	}
	return in, true
}

// callTarget is the function the call at the program's instruction pc last
// called, as its site's cache (regCalls, by the register instruction's C)
// holds it: a monomorphic target from the first compile. Its closure is nil
// for a built-in.
func (f *jitRegFeedback) callTarget(pc int) (*Object, *closure, bool) {
	return f.callsData().callTarget(pc)
}

// constant is the constant a StringConst or a BigIntConst at the
// program's instruction pc reads -- load_const's, a switch's case's, an
// element of new_array_k's, or load_empty's empty string -- and whether it
// is one of op's.
func (f *jitRegFeedback) constant(pc int, op ir.Op) (Value, bool) {
	w, iop, ok := f.at(pc)
	if !ok || iop != op || f.fb.cl == nil {
		return Value{}, false
	}
	if w.Op() == bytecode.RLoadEmpty {
		return Str(emptyString), op == ir.StringConst
	}
	k := f.p.Code[pc].Key
	if w.Op() != bytecode.RLoadConst && w.Op() != bytecode.RSwitch && w.Op() != bytecode.RNewArrayK || int(k) >= len(f.fb.cl.consts) {
		return Value{}, false
	}
	v := f.fb.cl.consts[k]
	if op == ir.StringConst && !v.IsString() || op == ir.BigIntConst && !v.IsBigInt() {
		return Value{}, false
	}
	return v, true
}

// StringCell is ssa.ConstantFeedback's: a cell holding the string constant
// the program's instruction pc reads, as jitFeedback's.
func (f *jitRegFeedback) StringCell(pc int) (uintptr, bool) {
	v, ok := f.constant(pc, ir.StringConst)
	if !ok {
		return 0, false
	}
	cell := new(Value)
	*cell = v
	k := f.fb.keep()
	k.cells = append(k.cells, cell)
	return uintptr(unsafe.Pointer(cell)), true
}

// StringContent is ssa.StringContentFeedback's: the string the constant at
// the program's instruction pc is, flat.
func (f *jitRegFeedback) StringContent(pc int) (string, bool) {
	v, ok := f.constant(pc, ir.StringConst)
	if !ok {
		return "", false
	}
	if s := v.String(); s.left == nil {
		return s.s, true
	}
	return "", false
}

// BigIntCell is ssa.BigIntConstFeedback's: the cell a copy of the BigInt
// constant at the program's instruction pc is in, as jitFeedback's.
func (f *jitRegFeedback) BigIntCell(pc int) (uintptr, uint64, int, bool) {
	v, ok := f.constant(pc, ir.BigIntConst)
	if !ok {
		return 0, 0, ssa.BigConstHeap, false
	}
	cell := new(Value)
	*cell = v
	k := f.fb.keep()
	k.cells = append(k.cells, cell)
	if cell.isInt64Big() {
		return uintptr(unsafe.Pointer(cell)), uint64(cell.int64Big()), ssa.BigConstInt64, true
	}
	if cell.isShortBig() {
		if neg, mag := cell.shortBigParts(); !neg {
			return uintptr(unsafe.Pointer(cell)), mag, ssa.BigConstUint64, true
		}
	}
	return uintptr(unsafe.Pointer(cell)), 0, ssa.BigConstHeap, true
}

// Generic is ssa.Feedback's: the register instruction of the program's
// instruction pc is one a guard of the function's code failed at before
// (jitState's regLearned): built generic, what it did not expect is Go's to
// make.
func (f *jitRegFeedback) Generic(pc int) bool {
	return f.native().generic(pc)
}

// regLitFed reports whether the program's instruction pc, of regexp_lit's
// expansion (compile's regexpLit), is fed: not after a guard of it failed,
// from which on the builder leaves the instruction to Go whole at its
// start -- a literal, a read or a call it is not fed for is an exit there.
func (f *jitRegFeedback) regLitFed(pc int) bool {
	return f.regLitFedAt(int(f.p.Origin[pc]))
}

// regLitFedAt is regLitFed's of register pc rpc.
func (f *jitRegFeedback) regLitFedAt(rpc int) bool {
	return f.fb.e == nil || !slices.Contains(f.fb.e.failed, int32(rpc))
}

// Resumed is ssa.ResumeFeedback's: Go has gone on at register pc rpc, after
// a call the code had no entry after (jitRegResumeSeen), which the next
// compile has one at. The builder asks it by the state's PC, a register
// pc. After a call of the function itself, which its recursion's outer
// runs are all suspended at, it has one too: one leaving deep in the
// recursion has Go go on after the call in each, which had run the rest of
// each outer level in the interpreter.
func (f *jitRegFeedback) Resumed(rpc int) bool {
	fb := f.fb
	e := fb.e
	if e == nil {
		return false
	}
	if slices.Contains(e.resumed, uint32(rpc)) {
		return true
	}
	return slices.Contains(fb.selfCallPCs(), int32(rpc-1))
}

// selfCallPCs caches the native and inlined calls of this function itself.
func (fb *jitFeedback) selfCallPCs() []int32 {
	if !fb.selfKnown {
		fb.selfKnown = true
		for _, x := range jitTargets(fb.e) {
			if x.cl != nil && x.cl.fn == fb.fn {
				fb.selfCalls = append(fb.selfCalls, x.pc)
			}
		}
	}
	return fb.selfCalls
}

// snapshotStatePCs visits the sparse lists backing the state questions.
// Unlisted PCs already have false answers in jitRegSnapshot.get.
func (fb *jitRegFeedback) snapshotStatePCs(s *jitSnapshot) {
	if t := &s.d.calls.tg; t.gathered {
		// Derived on the worker, from the learning as it is now and what
		// the derivations append to it (jitTargetsDerive).
		t.late(fb)
		return
	}
	e := fb.fb.e
	if e == nil {
		return
	}
	n := int64(len(fb.rc.Code))
	for _, pc := range e.resumed {
		if int64(pc) < n {
			jitRegSnapAskResumed(s, fb, int(pc))
		}
	}
	for _, call := range fb.fb.selfCallPCs() {
		pc := int64(call) + 1
		if pc >= 0 && pc < n {
			jitRegSnapAskResumed(s, fb, int(pc))
		}
	}
	for _, pc := range e.failedEntries {
		if int64(pc) < n {
			jitRegSnapAskEntryGeneric(s, fb, int(pc))
		}
	}
}

// EntryGeneric is ssa.Feedback's: an entry at register pc pc whose
// speculation failed before (jitRegEntrySeen), which the builder asks by
// the state's PC, a register pc.
func (f *jitRegFeedback) EntryGeneric(pc int) bool {
	return f.fb.e != nil && slices.Contains(f.fb.e.failedEntries, uint32(pc))
}

// Inline is ssa.Feedback's: a plain call, a method's or a construction of a
// base constructor -- call_w's, call_method_w's, new_w's, or a call in tail
// position, tail_call_w's and tail_call_method_w's -- of one function, the
// one its site's cache holds and its code learned it calls or constructs
// with (regCallSites), no other, that runs register code native callers may
// call, not the caller's own nor the compiled function's, nor an arrow, is
// the callee to inline, its program its register code lowered for an
// inlined frame (LowerRegInline), asked by its own register feedback, the
// root's keep holding what its sites name; the builder decides whether its
// program may be (ssa's InlineCalls) and fits, and how deep. An inlined
// callee's calls are asked so too, of its own caches and what its code
// learned, as V8 inlines a callee's callees. A call whose inlined callee
// left native code too often is not inlined again (jitInlineLeft's
// notInline). A callee that keeps its arguments is inlined too: one that
// reads them, its reads the call's (ReadsArguments); one that hands them
// on, with them, its apply_args the inlined calls of what it forwards to
// (Arguments, forwardInline).
//
// Asked once a compile for each instruction (askedOf): the builder asks it
// of each of the root's calls in each of its builds, and of each call of a
// callee it plans to inline, and the answer, a search of the VM's caches
// and a callee's feedback made, does not change while it builds.
func (f *jitRegFeedback) Inline(pc int) (ssa.InlineSite, bool) {
	a := f.askedOf(pc)
	if a == nil {
		return f.inline(pc)
	}
	if a.did&askedInline == 0 {
		// Found before a is written: the finding may grow the answers.
		site, ok := f.inline(pc)
		a = f.askedOf(pc)
		a.did |= askedInline
		if ok {
			// Taking &site would allocate it even for a rejected inline.
			a.site = new(ssa.InlineSite)
			*a.site = site
		}
	}
	if a.site == nil {
		return ssa.InlineSite{}, false
	}
	return *a.site, true
}

// jitRegAsked is what a register compile's feedback answered for one of
// its program's instructions, asked once (jitRegFeedback's askedOf): its
// Inline's answer, site, nil for none, and its NativeCalls', calls, where
// did says they were asked.
type jitRegAsked struct {
	site  *ssa.InlineSite
	calls []ssa.CallSite
	did   uint8
}

// What a jitRegAsked holds (did).
const (
	askedInline uint8 = 1 << iota
	askedCalls
)

// askedOf is the answers f gave for its program's instruction pc, made
// empty at its first ask, or nil past the program. The answers are by
// index, which askedAt keeps by pc, one past it: most of a program's
// instructions are no calls.
func (f *jitRegFeedback) askedOf(pc int) *jitRegAsked {
	if pc < 0 || pc >= len(f.p.Code) {
		return nil
	}
	if f.askedAt == nil {
		f.askedAt = make([]int32, len(f.p.Code))
	}
	if i := f.askedAt[pc]; i != 0 {
		return &f.asked[i-1]
	}
	f.asked = append(f.asked, jitRegAsked{})
	f.askedAt[pc] = int32(len(f.asked))
	return &f.asked[len(f.asked)-1]
}

// jitRegCalleeOf is the feedback of cl's register code lowered as p for a
// callee that the compile whose root's feedback is k asks to inline: one
// for all of its calls the compile asks of (k's regCallees), as its
// answers are its code's and its caches', the same at each -- but a
// forwarding callee's, whose frame is its call's (jitRegFeedback's fwd).
// Made at each ask, they had been most of what the asks allocated, a
// large function asking of every call it makes, and inlining a few.
func (r *Runtime) jitRegCalleeOf(k *jitFeedback, cl *closure, p *ir.Program) *jitRegFeedback {
	for _, c := range k.regCallees {
		if c.fb.cl == cl && c.p == p {
			return c
		}
	}
	s, key := r.jitState(), r.jitRegKeyOf(cl)
	if e := s.regParked[key]; e != nil {
		// A callee whose code was parked as the runtime returned to its
		// host: back with what it learned, which its parked state holds,
		// before the caller is compiled from it (jitRegUnpark).
		s.jitRegUnpark(e)
	}
	c := &jitRegFeedback{fb: &jitFeedback{r: r, fn: cl.fn, cl: cl, e: s.regLearnedOf(key), root: k}, rc: cl.fn.RegCode(), p: p}
	k.regCallees = append(k.regCallees, c)
	return c
}

// inline is Inline's answer, found, of the live state (jitInlineOf).
func (f *jitRegFeedback) inline(pc int) (ssa.InlineSite, bool) {
	q := jitCallCtx{p: f.p, rc: f.rc, src: jitCallLive{f}}
	if f.fb != nil {
		// A test's feedback of a program alone has none.
		q = f.callCtx()
	}
	return jitInlineOf(&q, pc, jitInlineLive{f})
}

// jitRegCallOp is op with a call in tail position's opcode as the
// ordinary call's, as native code makes it where the stack is shallow.
func jitRegCallOp(op bytecode.ROp) bytecode.ROp {
	switch op {
	case bytecode.RCallWT:
		return bytecode.RCallW
	case bytecode.RCallMWT:
		return bytecode.RCallMW
	}
	return op
}

// GoCalls is ssa.GoFeedback's: native code made from register code calls
// Go, which makes what needs no frame of the VM's from what the code passes
// it, and a call on the register frame (jitRegGoCall); what needs more, a
// call of Go refuses, and the code leaves there.
func (f *jitRegFeedback) GoCalls() bool { return f.native().goCalls() }

// NativeCalls is ssa.Feedback's: a call -- call_w's, call_method_w's or
// new_w's, of any number of operands -- of the function
// its site's cache holds (regCalls), or of one its code learned it calls,
// a construction's with a pool of its objects, where that runs register
// code native callers may call, is a native call of it (regCallSites); any
// other of no more operands than a call of Go takes is a call of Go
// (ssa.GoCall), which makes it on the register frame without leaving
// native code (jitRegGoCall), and one of more leaves. A call in tail
// position is
// neither, which the interpreter makes in constant space at a host exit.
//
// Asked once a compile for each instruction, as Inline is.
func (f *jitRegFeedback) NativeCalls(pc int) []ssa.CallSite {
	a := f.askedOf(pc)
	if a == nil {
		return f.nativeCalls(pc)
	}
	if a.did&askedCalls == 0 {
		calls := f.nativeCalls(pc)
		a = f.askedOf(pc)
		a.calls, a.did = calls, a.did|askedCalls
		return calls
	}
	return a.calls
}

// nativeCalls is NativeCalls' answer, found.
func (f *jitRegFeedback) nativeCalls(pc int) []ssa.CallSite {
	q := f.callCtx()
	return q.nativeCalls(pc)
}

// nativeCalls is NativeCalls' answer, of q's source. What it answers
// reading the heap, or making what the runtime keeps, it asks of the live
// feedback (mustLive), at the PCs a snapshot's gather asks on the
// runtime's goroutine (callsLive).
func (q *jitCallCtx) nativeCalls(pc int) []ssa.CallSite {
	w, op, ok := jitRegAt(q.p, q.rc, pc)
	if ok && op == ir.IterStep && w.Op() == bytecode.RIterNext && !q.p.Code[pc].When && !q.p.Code[pc].Rest {
		// A loop's step's native call of its iterator's next
		// (regStepSites).
		return q.mustLive().regStepSites(pc)
	}
	if ok && (op == ir.Host && q.p.Code[pc].Whole || op == ir.FieldDefine && w.Op() == bytecode.RDefineField) {
		// An instruction Go runs whole, its fast paths native code's
		// (regWholeSite).
		return q.mustLive().regWholeSite(pc, w)
	}
	if !ok || op != ir.Host || !q.p.Code[pc].Window {
		return nil
	}
	switch w.Op() {
	case bytecode.RTypeof:
		// Its name, which Go makes from native code.
		return []ssa.CallSite{{Go: ssa.GoCall, TypeOf: true, ThisSlot: -1}}
	case bytecode.RInstanceOf, bytecode.RInstanceOfNext:
		// Go's, from native code, by the site's cache, as the interpreter
		// makes it: it may run code, a Symbol.hasInstance method or a
		// proxy's trap.
		return []ssa.CallSite{{Go: ssa.GoCall, InstanceOf: true, Argc: 1, ThisSlot: -1}}
	}
	rpc := int(q.p.Origin[pc])
	if w.Op() == bytecode.RSuperCall {
		// A derived constructor's super(...) (regSuperSites).
		return q.mustLive().regSuperSites(pc, w)
	}
	if w.Op() == bytecode.RLit {
		// regexp_lit's call of the method its expansion read, of its one
		// argument: Go's from native code, as call_method_w's marked to
		// give the literal back (jitRegGoCall), or the leaf's (Leaf), its
		// receiver the literal's RegExp from its pool (regLitPool), which
		// the leaf gives back to it (ssa.CallSite's Give,
		// jitRegExpLeafGive).
		f := q.mustLive()
		if !f.regLitFed(pc) {
			return nil
		}
		return []ssa.CallSite{{Go: ssa.GoCall, Argc: 1, Method: true, ThisSlot: -1, Give: f.regLitPool(pc, w)}}
	}
	if rpc < len(q.rc.Marks) && q.rc.Marks[rpc]&bytecode.RMarkTail != 0 {
		return nil
	}
	// A call in tail position (tail_call_w, tail_call_method_w) is made as
	// the ordinary call, where the stack is shallow (ir's Tail).
	argc, method := int(w.X()), w.Op().MethodCall()
	if w.Op() == bytecode.RApplyArgs {
		// f.apply(x, arguments): a native call of the functions its code
		// learned it forwards to (regCallSite's Forward), or else a call of
		// Go, which makes it from the frame's arguments in the context --
		// Array.apply(null, arguments) without a frame (jitForwardPlain) --
		// the realm's apply checked by the code.
		f := q.mustLive()
		if !q.p.Forwards || f.fb.r.applyFn == nil {
			return nil
		}
		if sites := q.callSites(pc, w, 2, false); len(sites) != 0 {
			if l := f.fb.e; l != nil && jitCountAt(l.nativeCalls, rpc) >= jitCallTargets {
				// As many as it calls natively: any other Go's.
				sites[0].Generic = true
			}
			return sites
		}
		k := f.fb.keep()
		k.holders = append(k.holders, f.fb.r.applyFn)
		return []ssa.CallSite{{Go: ssa.GoCall, Forward: true, Apply: uintptr(unsafe.Pointer(f.fb.r.applyFn)), Argc: 2, ThisSlot: -1}}
	}
	n := argc + 1
	if method {
		n++
	}
	// More operands than a call of Go takes in the context: a native call
	// all the same, its window holding them, where Go reads them when it
	// makes the call (mir's windowGoCall) -- not a call of Go alone, which
	// has nowhere else to take them from, nor a built-in's.
	wide := n > abi.MaxGoArgs
	if w.Op() == bytecode.RNewW {
		f := q.mustLive()
		if site, ok := f.regAllocSite(rpc, w, argc); ok && !wide {
			return []ssa.CallSite{site}
		}
		if l := f.fb.e; !wide && l != nil && slices.Contains(l.genericCalls, int32(rpc)) {
			// A construction that met more functions than it constructs
			// with natively (jitRegGenericNew) -- classes a loop makes
			// anew, each with a prototype of its own -- is Go's from native
			// code, whatever it constructs with, as V8's code constructs
			// through its generic Construct builtin.
			return []ssa.CallSite{{Go: ssa.GoCall, Argc: argc, New: true, ThisSlot: -1}}
		}
	}
	if sites := q.callSites(pc, w, argc, method); len(sites) != 0 {
		if l := q.src.learned(); w.Op() != bytecode.RNewW && l != nil && (jitCountAt(l.nativeCalls, rpc) >= jitCallTargets || slices.Contains(l.genericCalls, int32(rpc))) {
			// As many as it calls natively: any other function it calls
			// through its link (ssa.CallSite's Generic) -- where Go had
			// made every call past them, AWFY's Richards' tasks' functions,
			// which each run makes anew. So too a call that met a function
			// native callers never call (jitRegGenericSeen), which Go calls
			// from native code: x.toString() of objects and numbers, where
			// each number's call had left native code.
			sites[0].Generic = true
			sites[0].GenericEnv, sites[0].GenericRealm = uintptr(unsafe.Pointer(q.cl.env)), uintptr(unsafe.Pointer(q.cl.realm))
			if q.rc.Typed {
				// Typed code's call through a function table: any of its
				// functions by the call's index (regTableEntries).
				sites[0].Table, sites[0].TableLen = q.mustLive().regTableEntries(pc, w, argc)
			}
		}
		return sites
	}
	if q.builtinAt(w, method) {
		if site, ok := q.mustLive().regBuiltinSite(w, argc, method); ok {
			// Made natively of its operands, however many (mir's push).
			return []ssa.CallSite{site}
		}
	}
	if wide {
		return nil
	}
	site := ssa.CallSite{Go: ssa.GoCall, Argc: argc, Method: method, New: w.Op() == bytecode.RNewW, ThisSlot: -1}
	if method && q.litAt(rpc, w) {
		// A RegExp literal's call, whose RegExp a leaf's call gives back
		// to the literal's pool (ssa.CallSite's Give).
		site.Give = q.mustLive().regLitPool(pc, w)
	}
	// A call of the realm's next on generators the code resumes natively
	// (regGenNextSite), or a generator function's call, its generator made
	// natively (regGenMakeSite).
	if q.genAt(rpc) {
		f := q.mustLive()
		if f.regGenNextSite(rpc, &site); site.Pool == 0 {
			f.regGenMakeSite(rpc, &site)
		}
	}
	return []ssa.CallSite{site}
}

// callSites is regCallSites' answer, of q's source.
func (q *jitCallCtx) callSites(pc int, w bytecode.RInstr, argc int, method bool) []ssa.CallSite {
	var t jitRegTargets
	q.callTargets(&t, pc, w, argc, method)
	if t.n == 0 {
		return nil
	}
	return slices.Clone(t.sites[:t.n])
}

// builtinAt reports whether the call w's site's cache holds a built-in the
// call may make natively (regBuiltinSite): a method call's of a native
// function.
func (q *jitCallCtx) builtinAt(w bytecode.RInstr, method bool) bool {
	if !method {
		return false
	}
	calls := q.src.calls(int(w.C()))
	if uint(w.C()) >= uint(len(calls)) {
		return false
	}
	e := &calls[w.C()]
	return e.kind&^regCallMarks == regCallNative && e.callee != nil
}

// litAt reports whether the method call w at register pc rpc is marked to
// give a RegExp literal back (regLitPool).
func (q *jitCallCtx) litAt(rpc int, w bytecode.RInstr) bool {
	return w.Op() == bytecode.RCallMW && rpc < len(q.rc.Marks) && q.rc.Marks[rpc]&(bytecode.RMarkLitArg|bytecode.RMarkLitRecv) != 0
}

// genAt reports whether the code learned generators resumed or made at
// register pc rpc (regGenNextSite, regGenMakeSite).
func (q *jitCallCtx) genAt(rpc int) bool {
	l := q.src.learned()
	if l == nil {
		return false
	}
	for i := range l.genSteps {
		if int(l.genSteps[i].pc) == rpc {
			return true
		}
	}
	for i := range l.genMakes {
		// Its pc alone: its pool is made in place at its first ask
		// (jitGenMakePool), on the runtime's goroutine.
		if int(l.genMakes[i].pc) == rpc {
			return true
		}
	}
	return false
}

// callsLive reports whether NativeCalls' answer at the program's
// instruction pc asks the live feedback (mustLive): what a snapshot's
// gather asks on the runtime's goroutine, where the asking reads the heap
// -- a binding's value, a typed table, the prototypes a built-in's call
// checks -- or makes what the runtime keeps -- a pool, a literal, register
// code. It answers from the copy as nativeCalls would, and says yes
// wherever that may come to mustLive: never too seldom, a derivation that
// came to one would panic.
func (q *jitCallCtx) callsLive(pc int) bool {
	w, op, ok := jitRegAt(q.p, q.rc, pc)
	if !ok {
		return false
	}
	if q.rc.Typed {
		// Typed code's calls read its tables (regTableTargets).
		return true
	}
	c := &q.p.Code[pc]
	if op == ir.IterStep && w.Op() == bytecode.RIterNext && !c.When && !c.Rest ||
		op == ir.Host && c.Whole || op == ir.FieldDefine && w.Op() == bytecode.RDefineField {
		return true
	}
	if op != ir.Host || !c.Window {
		return false
	}
	switch w.Op() {
	case bytecode.RTypeof, bytecode.RInstanceOf, bytecode.RInstanceOfNext:
		return false
	case bytecode.RSuperCall, bytecode.RLit, bytecode.RApplyArgs, bytecode.RNewW:
		return true
	}
	rpc := int(q.p.Origin[pc])
	if rpc < len(q.rc.Marks) && q.rc.Marks[rpc]&bytecode.RMarkTail != 0 {
		return false
	}
	calls := q.src.calls(int(w.C()))
	if uint(w.C()) >= uint(len(calls)) || calls[w.C()].callee == nil {
		// Unfilled: the binding read before it holds the function now
		// (regViaNow, regConstructorNow).
		return true
	}
	method := w.Op().MethodCall()
	return q.builtinAt(w, method) || method && q.litAt(rpc, w) || q.genAt(rpc)
}

// regBuiltinSite is the call of a built-in native code makes itself whose
// target call_method_w w's site's cache holds (regCallNative): the realm's
// Array.prototype.pop, of no argument, or push, of any, whose fast path the
// call's checks keep to -- the receiver an array of the realm's prototype,
// and, for push, the prototypes as they are now, with no index in their
// tables. The call leaves anything else to Go.
func (f *jitRegFeedback) regBuiltinSite(w bytecode.RInstr, argc int, method bool) (ssa.CallSite, bool) {
	r := f.fb.r
	calls := r.regCallsRestored(f.fb.cl, int(w.C()))
	if !method || uint(w.C()) >= uint(len(calls)) {
		return ssa.CallSite{}, false
	}
	e := &calls[w.C()]
	o := e.callee
	if e.kind&^regCallMarks != regCallNative || o == nil {
		return ssa.CallSite{}, false
	}
	fd := o.fn()
	if fd == nil || fd.realm != r.Realm {
		return ssa.CallSite{}, false
	}
	k := f.fb.keep()
	switch {
	case argc == 0 && fd.elemOp == elemPop:
		k.holders = append(k.holders, o)
		return ssa.CallSite{Callee: uintptr(unsafe.Pointer(o)), ThisSlot: -1, Method: true, Pop: true,
			Protos: [2]ssa.Holder{{Object: uintptr(unsafe.Pointer(r.proto.array))}}}, true
	case argc >= 1 && fd.elemOp == elemPush:
		ap, op := r.proto.array, r.proto.object
		if ap.shape == nil || op.shape == nil || ap.shape.loose || op.shape.loose || ap.proto() != op || op.proto() != nil ||
			!ap.noIndexKeys() || !op.noIndexKeys() {
			return ssa.CallSite{}, false
		}
		k.shapes = append(k.shapes, remember(ap.shape), remember(op.shape))
		k.holders = append(k.holders, o, ap, op)
		return ssa.CallSite{Callee: uintptr(unsafe.Pointer(o)), ThisSlot: -1, Argc: argc, Method: true, Push: true,
			Protos: [2]ssa.Holder{{Object: uintptr(unsafe.Pointer(ap)), Shape: uintptr(unsafe.Pointer(ap.shape))},
				{Object: uintptr(unsafe.Pointer(op)), Shape: uintptr(unsafe.Pointer(op.shape))}}}, true
	}
	return ssa.CallSite{}, false
}

// BigIntSole is ssa.BigIntRecycleFeedback's: the registers of the first 64
// that hold their values alone (regSoleRegisters), and whether the code may
// give BigInts' blocks back at all: a function's that does not suspend.
func (f *jitRegFeedback) BigIntSole() (uint64, bool) {
	return f.native().bigIntSole()
}

// regSoleRegisters is the registers of rc, fn's register code lowered to p,
// of the first 64, that hold their values alone: its bindings past the
// function, this and the parameters -- not its temporaries, whose values an
// operator's call gives back as it takes them (ssa's regBigFresh) -- none a
// closure captures (ir.Program's Captured), each read only as an operand of
// an operator that makes a value of its own -- arithmetic, bitwise, a
// comparison, a test, a branch -- in a function that is no generator, no
// async function, has no direct eval and no arguments object, of
// instructions this knows: any other has the function none. A move from
// one, a call's window or a return of it, a store of it to a global, a read
// of its property or a postfix update of it, which leaves its value in
// another register, has it hold its value no more. What an operator stores
// to one is the register's alone (the VM's bigSoleBit), and its old value
// given back where it was so (ssa's ReleaseOld).
func regSoleRegisters(fn *bytecode.Function, rc *bytecode.RegCode, p *ir.Program) uint64 {
	if bits.UintSize != 64 || fn.Generator || fn.Async || fn.HasDirectEval || fn.UsesArguments || rc.UsesArguments || rc.LocalReg != nil {
		return 0
	}
	var cand uint64
	for k := 2 + fn.ParamCount; k < 2+fn.ParamCount+rc.Inits && k < rc.NumRegs && k < 64; k++ {
		if !p.IsCaptured(k) {
			cand |= 1 << uint(k)
		}
	}
	escaped := uint64(0)
	escape := func(k int) {
		if k >= 0 && k < 64 {
			escaped |= 1 << uint(k)
		}
	}
	for _, w := range rc.Code {
		a, b := int(w.A()), int(w.B())
		switch w.Op() {
		case bytecode.RNop, bytecode.RLoadInt, bytecode.RLoadConst, bytecode.RLoadUndef, bytecode.RLoadNull,
			bytecode.RLoadTrue, bytecode.RLoadFalse, bytecode.RLoadEmpty, bytecode.RLoadUninit, bytecode.RGetGlobal, bytecode.RGetGlobalInstanceOf,
			bytecode.RAdd, bytecode.RSub, bytecode.RMul, bytecode.RBin, bytecode.RAddI, bytecode.RSubI,
			bytecode.RBitAnd, bytecode.RBitOr, bytecode.RBitXor, bytecode.RShl, bytecode.RShr, bytecode.RUShr,
			bytecode.RAndI, bytecode.ROrI, bytecode.RXorI, bytecode.RShlI, bytecode.RShrI, bytecode.RUShrI,
			bytecode.RInc, bytecode.RDec, bytecode.RUnary,
			bytecode.RMulI, bytecode.RAddI32, bytecode.RSubI32, bytecode.RAddII32,
			bytecode.RJmp, bytecode.RJmpF, bytecode.RJmpT,
			bytecode.RJLt, bytecode.RJNLt, bytecode.RJLe, bytecode.RJNLe, bytecode.RJGt, bytecode.RJNGt, bytecode.RJGe, bytecode.RJNGe,
			bytecode.RJLtI, bytecode.RJNLtI, bytecode.RJLeI, bytecode.RJNLeI, bytecode.RJGtI, bytecode.RJNGtI, bytecode.RJGeI, bytecode.RJNGeI,
			bytecode.RJSEq, bytecode.RJSNe, bytecode.RJSEqI, bytecode.RJSNeI, bytecode.RJBool,
			bytecode.RJEq, bytecode.RJNe, bytecode.RJEqI, bytecode.RJNeI, bytecode.RJNull, bytecode.RJNotNull,
			bytecode.RStepJLt, bytecode.RStepJLe, bytecode.RStepJLtI, bytecode.RStepJGtI, bytecode.RStepJGeI,
			bytecode.RRetUndef, bytecode.RAppend, bytecode.RStrFreeze:
			// Operands, results of their own, tests and branches; an
			// append's addition, of a result of its own, and a read of the
			// string it grows that leaves the value where it is.
		case bytecode.RMove, bytecode.RPostInc, bytecode.RPostDec, bytecode.RGetLength, bytecode.RGetMethod, bytecode.RGetMethodW, bytecode.RGetProp, bytecode.RGetPropNext:
			// Its value left in another register, or read a property of.
			escape(b)
		case bytecode.RRet, bytecode.RSetGlobal:
			escape(a)
		case bytecode.RCallW, bytecode.RCallMW, bytecode.RNewW, bytecode.RCallWT, bytecode.RCallMWT:
			// The window, the callee's frame.
			for k := a; k < a+2+int(w.X()); k++ {
				escape(k)
			}
		default:
			return 0
		}
	}
	return cand &^ escaped
}

// Leaf is ssa.LeafFeedback's: the built-in the call at the program's
// instruction pc -- an ordinary call or one in tail position -- last
// called, as its site's cache holds it (regCalls), where that is a
// leaf of the realm's that native code calls without a frame (jitLeafOp):
// answered from the interpreter's cache at the first compile.
func (f *jitRegFeedback) Leaf(pc int) (uintptr, bool) {
	w, op, ok := f.at(pc)
	if ok && op == ir.Host && f.p.Code[pc].Window && w.Op() == bytecode.RLit {
		// regexp_lit's call: the realm's exec or test, read where it is the
		// built-in (regexpLitMethod), a leaf; the literal's RegExp, from
		// the pool, nothing keeps.
		m := f.fb.r.regexpLitMethod(w.X()&bytecode.RLitTest != 0)
		if m == nil || !f.regLitFed(pc) {
			return 0, false
		}
		f.fb.keep().holders = append(f.fb.keep().holders, m)
		return uintptr(unsafe.Pointer(m)), true
	}
	call := jitRegCallOp(w.Op())
	if !ok || op != ir.Host || !f.p.Code[pc].Window || call != bytecode.RCallW && call != bytecode.RCallMW {
		return 0, false
	}
	var marks uint8
	if rpc := int(f.p.Origin[pc]); rpc < len(f.rc.Marks) {
		marks = f.rc.Marks[rpc]
	}
	if marks&bytecode.RMarkTail != 0 {
		return 0, false
	}
	callee, cl, ok := f.callTarget(pc)
	if !ok || cl != nil {
		return 0, false
	}
	fd := callee.fn()
	if fd == nil || fd.native == nil || fd.bound || fd.realm != nil && fd.realm != f.fb.r.Realm {
		return 0, false
	}
	// RegExp's exec and test too -- a literal's call among them
	// (RMarkLitRecv), its RegExp then from the pool (regexp_lit's lowering,
	// compile's regexpLit), one whose value nothing reads but its truth
	// (RMarkDead), which the leaf makes whole, and an exec whose array the
	// interpreter may make again in the memory of the last (RMarkReuse),
	// which the leaf makes anew: the old array, which only the local the
	// new one replaces could reach, the interpreter's next call may make
	// again, its slots' check finding the local holds it no more.
	regexp := fd.elemOp == elemRegExpExec || fd.elemOp == elemRegExpTest
	// String.prototype.replace of two arguments, s.replace(re, ""), whose
	// leaf checks at each call that they are a RegExp it replaces with
	// directly and a string (jitReplaceLeaf) -- a literal's too, which the
	// call makes its first argument (RMarkLitArg), natively from the
	// literal's pool (lowerreg's literal before the call), the leaf giving
	// it back (ssa.CallSite's Give, jitLeafForm's giveArg), where its
	// template is a string constant: a function's, which the leaf's check
	// refuses, would leave native code at every call the call of Go makes
	// now.
	replace := callee == f.fb.r.strReplaceFn && call == bytecode.RCallMW && w.X() == 2 &&
		(marks&bytecode.RMarkLitArg == 0 || f.regStringBefore(int(f.p.Origin[pc]), int(w.A())+3))
	// Array.prototype.push of other than one value: one value's push native
	// code appends itself.
	push := fd.elemOp == elemPush && w.X() != 1
	if !jitLeafOp(fd.elemOp) && !regexp && !replace && !push {
		return 0, false
	}
	f.fb.keep().holders = append(f.fb.keep().holders, callee)
	return uintptr(unsafe.Pointer(callee)), true
}

// ViaLeaf is ssa.ViaLeafFeedback's: for the method call at the program's
// instruction pc, the realm's Function.prototype.call and the built-in G
// the call was seen to call through it, G.call(x, ...) --
// hasOwnProperty.call(o, k) -- where G is a leaf native code calls without
// a frame (jitLeafOp), as its plain call learned it (jitViaLeafSeen), and
// no function of the script's it calls natively.
func (f *jitRegFeedback) ViaLeaf(pc int) (uintptr, uintptr, bool) {
	w, op, ok := f.at(pc)
	l, r := f.fb.e, f.fb.r
	if !ok || op != ir.Host || !f.p.Code[pc].Window || l == nil || r.callFn == nil || jitRegCallOp(w.Op()) != bytecode.RCallMW || w.X() < 1 {
		return 0, 0, false
	}
	rpc := int32(f.p.Origin[pc])
	if int(rpc) < len(f.rc.Marks) && f.rc.Marks[rpc]&bytecode.RMarkTail != 0 {
		return 0, 0, false
	}
	for _, x := range l.nativeCalls {
		if x.pc == rpc {
			// A call that met a function of the script's too, which its
			// exits taught it: the general way, which makes both.
			return 0, 0, false
		}
	}
	for _, x := range l.viaLeaves {
		if x.pc != rpc {
			continue
		}
		fd := x.obj.fn()
		if fd == nil || fd.native == nil || fd.bound || !jitLeafOp(fd.elemOp) || fd.realm != nil && fd.realm != r.Realm {
			return 0, 0, false
		}
		f.fb.keep().holders = append(f.fb.keep().holders, r.callFn, x.obj)
		return uintptr(unsafe.Pointer(r.callFn)), uintptr(unsafe.Pointer(x.obj)), true
	}
	return 0, 0, false
}

// regStringBefore reports whether the register instruction before
// register pc rpc loads a string constant into register reg: a call's
// last argument, made right before it.
func (f *jitRegFeedback) regStringBefore(rpc, reg int) bool {
	if rpc < 1 || rpc > len(f.rc.Code) {
		return false
	}
	w := f.rc.Code[rpc-1]
	switch {
	case int(w.A()) != reg:
		return false
	case w.Op() == bytecode.RLoadEmpty:
		return true
	case w.Op() == bytecode.RLoadConst:
		consts := regConsts(f.fb.fn, f.rc)
		return int(w.BC()) < len(consts) && consts[w.BC()].Kind == bytecode.ConstString
	}
	return false
}

// methodCall is the built-in the call at the program's instruction pc --
// call_w's, or call_method_w's where method says, including tail position
// -- last called, as its site's cache holds it (regCalls), and its argument
// count.
func (f *jitRegFeedback) methodCall(pc int, method bool) (*Object, int, bool) {
	return f.callsData().methodCall(pc, method)
}

// CharCode is ssa.CharCodeFeedback's: the method call of one argument at
// the program's instruction pc calls String.prototype.charCodeAt, the
// realm's own (methodCall).
func (f *jitRegFeedback) CharCode(pc int) bool {
	return f.callsData().charCode(pc)
}

// Substring is ssa.SubstringFeedback's: String.prototype's substring,
// slice, substr or charAt, the realm's own, which the method call at the
// program's instruction pc calls (methodCall), and which it is.
func (f *jitRegFeedback) Substring(pc int) (uintptr, int, bool) {
	return f.callsData().substring(pc)
}

// FromCharCode is ssa.FromCharCodeFeedback's: String.fromCharCode, the
// realm's own, which the method call of one argument at the program's
// instruction pc calls (methodCall).
func (f *jitRegFeedback) FromCharCode(pc int) (uintptr, bool) {
	return f.callsData().fromCharCode(pc)
}

// Intrinsic is ssa.IntrinsicFeedback's: the realm's Math function a call at
// the program's instruction pc -- a method's, Math.sqrt(x), or a plain one
// of the function read before -- calls (methodCall), made natively or by Go
// called from native code (jitFeedback's mathIntrinsic).
func (f *jitRegFeedback) Intrinsic(pc int) (ssa.Intrinsic, bool) {
	return f.callsData().intrinsic(pc, f.fb.keep())
}

// regConstOperand is the number p's instruction pc, a window's call, finds
// in slot, its window's register: a constant the program copies there
// before it, with nothing between that may write the slot -- what writes
// only its own register, an operation, a call's window or an instruction
// Go runs whole, writes another -- nor jumps or is jumped to, nor an entry,
// nor a handler: what every path to the call reads there. It reports
// whether it is one.
func regConstOperand(p *ir.Program, pc, slot int) (float64, bool) {
	q := pc - 1
	for ; q >= 0; q-- {
		in := &p.Code[q]
		switch {
		case in.Op == ir.Jump, in.Op == ir.Branch, in.Op == ir.IterStep, in.Op == ir.Call,
			in.Op == ir.Host && !in.Window && !in.Whole:
			return 0, false
		}
		if in.Dest == slot {
			break
		}
	}
	if q < 0 {
		return 0, false
	}
	in := &p.Code[q]
	if in.Op != ir.Copy || in.Left.Slot >= 0 || in.Left.Literal.Kind != ir.Number {
		return 0, false
	}
	into := func(t int) bool { return t > q && t <= pc }
	for k := range p.Code {
		if t := &p.Code[k]; (t.Op == ir.Jump || t.Op == ir.Branch || t.Op == ir.IterStep) && into(t.Target) {
			return 0, false
		}
	}
	for _, h := range p.Handlers {
		if into(int(h.PC)) {
			return 0, false
		}
	}
	if slices.ContainsFunc(p.Entries, into) {
		return 0, false
	}
	return math.Float64frombits(in.Left.Literal.Bits), true
}

// Literal is ssa.LiteralFeedback's: the literal the program's instruction
// pc makes natively from its pool -- new_object's and new_object_lit's
// object, new_array's and new_array_empty's array, new_regexp's RegExp --
// kept, by register pc, in what the function's code learned (regLearned),
// across its compiles.
func (f *jitRegFeedback) Literal(pc int) (ssa.LiteralSite, bool) {
	l := f.literalAt(pc)
	if l == nil {
		return ssa.LiteralSite{}, false
	}
	k := f.fb.keep()
	k.pools = append(k.pools, l.pool)
	if l.root != nil {
		k.shapes = append(k.shapes, remember(l.root))
	}
	return ssa.LiteralSite{Pool: uintptr(unsafe.Pointer(l.pool)), Shared: l.shared}, true
}

// regLitPool is the address of the pool of the RegExp literal whose RegExp
// is the receiver of the call at the program's instruction pc, w's -- a
// regexp_lit's that calls (compile's regexpLit), or a call_method_w marked
// to give its receiver back (RMarkLitRecv), the regexp_lit before it that
// does not call having put the RegExp in its window -- or its first
// argument, a call_method_w's that makes it (RMarkLitArg), which nothing
// but the call reaches: the literal literalAt finds or makes for that
// regexp_lit's ObjectLiteral, or the call's own, by the register pc they
// share, kept with the code. 0 for any other call, or where it has none.
func (f *jitRegFeedback) regLitPool(pc int, w bytecode.RInstr) uintptr {
	e, cl, r := f.fb.e, f.fb.cl, f.fb.r
	if e == nil || cl == nil || pc >= len(f.p.Origin) {
		return 0
	}
	rpc := int(f.p.Origin[pc])
	k := int(w.C())
	switch {
	case w.Op() == bytecode.RLit && w.X()&bytecode.RLitCall != 0:
	case w.Op() == bytecode.RCallMW && rpc < len(f.rc.Marks) && f.rc.Marks[rpc]&bytecode.RMarkLitArg != 0 && rpc < len(f.rc.Aux):
		k = int(f.rc.Aux[rpc])
	case w.Op() == bytecode.RCallMW && rpc < len(f.rc.Marks) && f.rc.Marks[rpc]&bytecode.RMarkLitRecv != 0 && rpc < len(f.rc.Aux):
		// Its literal's regexp_lit, of its window and its constant
		// (regcomp's regexpLitCall): the instructions between make the
		// arguments.
		k = int(f.rc.Aux[rpc])
		at := -1
		for q := rpc - 1; q >= 0; q-- {
			if x := f.rc.Code[q]; x.Op() == bytecode.RLit && x.X()&bytecode.RLitCall == 0 && x.A() == w.A() && int(x.C()) == k {
				at = q
				break
			}
		}
		if at < 0 {
			return 0
		}
		rpc = at
	default:
		return 0
	}
	var l *jitLiteral
	for i := range e.literals {
		if int(e.literals[i].pc) == rpc && e.literals[i].fn == cl.fn {
			l = &e.literals[i]
			break
		}
	}
	if l == nil {
		consts := regConsts(cl.fn, f.rc)
		if k >= len(consts) || consts[k].Kind != bytecode.ConstRegExp {
			return 0
		}
		l = r.jitRegExpLiteral(e, cl, rpc, &consts[k])
	}
	if l == nil || l.lit == nil {
		return 0
	}
	f.fb.keep().pools = append(f.fb.keep().pools, l.pool)
	return uintptr(unsafe.Pointer(l.pool))
}

// literalAt is the literal of the register instruction of the program's
// instruction pc, an ObjectLiteral or an ArrayLiteral, made if it has none.
func (f *jitRegFeedback) literalAt(pc int) *jitLiteral {
	w, op, ok := f.at(pc)
	e, cl, r := f.fb.e, f.fb.cl, f.fb.r
	if !ok || e == nil || cl == nil || op != ir.ObjectLiteral && op != ir.ArrayLiteral {
		return nil
	}
	rpc := int(f.p.Origin[pc])
	for i := range e.literals {
		if int(e.literals[i].pc) == rpc && e.literals[i].fn == cl.fn {
			return &e.literals[i]
		}
	}
	switch w.Op() {
	case bytecode.RNewObject, bytecode.RNewObjectLit:
		n := int(w.B())
		if w.Op() == bytecode.RNewObjectLit {
			n = int(w.X())
		}
		if int(w.C()) >= len(cl.ic) {
			return nil
		}
		if w.Op() == bytecode.RNewObjectLit && cl.reg != nil {
			// Its root the realm's for literals of its keys, as the
			// interpreter's first object takes it (regLiteralKeyedRoot),
			// where the code makes the site's first.
			r.regLiteralKeyedRoot(cl.reg, rpc, &cl.ic[w.C()])
		}
		return r.jitObjectLiteral(e, cl, rpc, uint32(w.C()), n)
	case bytecode.RNewArray:
		if int(w.C()) > ir.MaxArrayLiteral {
			return nil
		}
		return r.jitArrayLiteral(e, cl, rpc, int(w.C()))
	case bytecode.RNewArrayE:
		return r.jitArrayLiteral(e, cl, rpc, 0)
	case bytecode.RNewArrayK:
		// Its arrays sharing its constants where the site does still, the
		// interpreter's; where it has given sharing up, of their own, the
		// constants written to them (jitArrayLiteral).
		if l := r.jitRegSharedLiteral(e, cl, f.rc, rpc, w); l != nil {
			return l
		}
		if rpc < len(f.rc.Aux) && int(f.rc.Aux[rpc]) < len(f.rc.Literals) {
			return r.jitArrayLiteral(e, cl, rpc, len(f.rc.Literals[f.rc.Aux[rpc]]))
		}
	case bytecode.RNewRegExp:
		// The register code's constant (regConsts), its own table where
		// it has one.
		if consts, k := regConsts(cl.fn, f.rc), int(w.BC()); k < len(consts) && consts[k].Kind == bytecode.ConstRegExp {
			return r.jitRegExpLiteral(e, cl, rpc, &consts[k])
		}
	case bytecode.RLit:
		// regexp_lit's literal (compile's regexpLit).
		if consts, k := regConsts(cl.fn, f.rc), int(w.C()); f.regLitFed(pc) && k < len(consts) && consts[k].Kind == bytecode.ConstRegExp {
			return r.jitRegExpLiteral(e, cl, rpc, &consts[k])
		}
	case bytecode.RCallMW:
		// The literal a call makes its first argument (RMarkLitArg), of
		// its constant Aux (lowerreg's literal before the call).
		if k, ok := regLitArgConst(f.rc, rpc); ok {
			if consts := regConsts(cl.fn, f.rc); k < len(consts) && consts[k].Kind == bytecode.ConstRegExp {
				return r.jitRegExpLiteral(e, cl, rpc, &consts[k])
			}
		}
	}
	return nil
}

// regLitArgConst is the constant of the RegExp literal the call_method_w at
// register pc rpc of rc makes its first argument (RMarkLitArg, its Aux),
// false where it makes none.
func regLitArgConst(rc *bytecode.RegCode, rpc int) (int, bool) {
	if rpc >= len(rc.Marks) || rc.Marks[rpc]&bytecode.RMarkLitArg == 0 || rpc >= len(rc.Aux) || rc.Code[rpc].Op() != bytecode.RCallMW {
		return 0, false
	}
	return int(rc.Aux[rpc]), true
}

// jitRegSharedLiteral is the literal of new_array_k w at register pc rpc of
// cl's register code rc, made in e: an array of the constants of
// Literals[Aux[rpc]] whose arrays share them, natively from a pool of
// arrays made as the interpreter makes them (rsNewArrayK, sharedShare),
// over its site's storage, made here where the interpreter has not made it.
// nil where the site has given sharing up, its arrays then Go's.
func (r *Runtime) jitRegSharedLiteral(e *jitEntry, cl *closure, rc *bytecode.RegCode, rpc int, w bytecode.RInstr) *jitLiteral {
	if int(w.C()) >= len(cl.ic) || rpc >= len(rc.Aux) || int(rc.Aux[rpc]) >= len(rc.Literals) {
		return nil
	}
	ic := &cl.ic[w.C()]
	if ic.p1 == nil {
		lit := rc.Literals[rc.Aux[rpc]]
		vals := make([]Value, len(lit))
		for i, x := range lit {
			switch x.Kind {
			case bytecode.LitConst:
				if int(x.K) >= len(cl.consts) {
					return nil
				}
				vals[i] = cl.consts[x.K]
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
		ic.p1 = sharedMade(r, vals)
	}
	if ic.idx < 0 || sharedCount(ic.p1.extAt().elems).num >= sharedGiveUp {
		return nil
	}
	n := len(ic.p1.extAt().elems)
	e.literals = append(e.literals, jitLiteral{pc: int32(rpc), pool: new(abi.ObjectPool), array: true, n: n, fn: cl.fn, shared: true})
	l := &e.literals[len(e.literals)-1]
	l.pool.Source = unsafe.Pointer(&jitPoolSource{array: true, n: n, sharedOf: cl, sharedPC: int32(w.C())})
	r.jitFillLiteral(l)
	return l
}

// Define is ssa.LiteralFeedback's: what the field the program's
// instruction pc defines on a new_object_lit's object adds, along the
// shapes' transitions from the shape its pool's objects have -- the
// fields before it, in the literal's order, each a step -- as the stack
// form's define_field (jitFeedback's Define). Go defines a field whose
// transition no object has taken yet.
func (f *jitRegFeedback) Define(pc int) (*ssa.PropertyAdd, bool) {
	w, op, ok := f.at(pc)
	cl := f.fb.cl
	if !ok || op != ir.FieldDefine || w.Op() != bytecode.RNewObjectLit || cl == nil {
		return nil, false
	}
	// The literal's object, the origin's first instruction, and the field's
	// place among its definitions after it.
	first := pc
	for first > 0 && f.p.Origin[first-1] == f.p.Origin[pc] {
		first--
	}
	l := f.literalAt(first)
	if l == nil || l.root == nil {
		return nil, false
	}
	keys := f.rc.ObjKeys[f.rc.Aux[f.p.Origin[pc]]]
	s, key, ok := regLiteralField(cl, keys, l.root, pc-first-1)
	if !ok {
		return nil, false
	}
	next := s.next
	k := f.fb.keep()
	k.shapes = append(k.shapes, remember(s), remember(next))
	return &ssa.PropertyAdd{From: uintptr(unsafe.Pointer(s)), Next: uintptr(unsafe.Pointer(next)), Flags: uint8(next.flags), Define: true, Key: uint32(key)}, true
}

// regLiteralField is the shape from which field i of an object literal of
// cl's keys, its objects of root, adds its key -- the fields before it, in
// the literal's order, each a step along the shapes' first transitions --
// and that key, which Define plans the field's definition along: ok only
// where an object has taken every transition to the field's, the field's
// own among them.
func regLiteralField(cl *closure, keys []uint32, root *shape, i int) (s *shape, key Atom, ok bool) {
	if root == nil || i < 0 || i >= len(keys) {
		return nil, 0, false
	}
	for _, k := range keys[:i+1] {
		if int(k) >= len(cl.names) {
			return nil, 0, false
		}
	}
	s = root
	for _, k := range keys[:i] {
		if s.next == nil || s.nextEdge() != (shapeEdge{cl.names[k], propDefault}) {
			return nil, 0, false
		}
		s = s.next
	}
	key = cl.names[keys[i]]
	if s.next == nil || s.nextEdge() != (shapeEdge{key, propDefault}) {
		return nil, 0, false
	}
	if next := s.next; next.index == nil && int(next.n) > linearScanLimit {
		return nil, 0, false
	}
	return s, key, true
}

// jitRegLiteralSeen learns, at a host exit of e's code at site of its
// program, a field of an object literal (new_object_lit) whose definition
// the code was compiled without, no object having taken its transition
// then (Define), and which Go, running the literal whole at the exit, has
// now defined: it reports whether the code compiled again plans that
// definition, which leaves native code no more. Not learned, Splay's
// GeneratePayloadTree, compiled at its first call before any of its
// inner literals had made an object, had left at every call, and was then
// found not to pay for its exits.
func (r *Runtime) jitRegLiteralSeen(f *frame, e *jitRegEntry, site int) bool {
	p := e.p
	if site < 0 || p == nil || e.life == nil || site >= len(p.Code) || site >= len(p.Origin) || p.Code[site].Op != ir.FieldDefine {
		return false
	}
	rpc := int(p.Origin[site])
	rc := e.rc
	if rc == nil || rpc >= len(rc.Code) || rc.Code[rpc].Op() != bytecode.RNewObjectLit || rpc >= len(rc.Aux) || int(rc.Aux[rpc]) >= len(rc.ObjKeys) {
		return false
	}
	first := site
	for first > 0 && p.Origin[first-1] == p.Origin[site] {
		first--
	}
	for i := range e.life.literals {
		if l := &e.life.literals[i]; int(l.pc) == rpc && l.fn == f.cl.fn {
			_, _, ok := regLiteralField(f.cl, rc.ObjKeys[rc.Aux[rpc]], l.root, site-first-1)
			return ok
		}
	}
	return false
}

// LeafKind is ssa.LeafKindFeedback's, jitFeedback's.
func (f *jitRegFeedback) LeafKind(fn uintptr) ssa.LeafKind { return f.fb.LeafKind(fn) }

// The runtime's addresses the builder asks once a function, the same for
// either form's code: jitFeedback's answers.

// ArrayProtos is ssa.AppendFeedback's, jitFeedback's.
func (f *jitRegFeedback) ArrayProtos() ([2]ssa.Holder, bool) { return f.fb.ArrayProtos() }

// GenNext is ssa.GenStepFeedback's half that is the realm's, jitFeedback's.
func (f *jitRegFeedback) GenNext() uintptr { return f.fb.GenNext() }

// GenSteps is ssa.GenStepFeedback's other half: the generators the step
// of the program's instruction pc has met (jitRegGenSeen), which it
// resumes natively (regGenSteps).
func (f *jitRegFeedback) GenSteps(pc int) []ssa.GenStep {
	if pc < 0 || pc >= len(f.p.Code) {
		return nil
	}
	return f.regGenSteps(int(f.p.Origin[pc]))
}

// HasCache is ssa.HasCacheFeedback's, jitFeedback's.
func (f *jitRegFeedback) HasCache() uintptr { return f.fb.HasCache() }

// KeyAtoms is ssa.KeyAtomsFeedback's, jitFeedback's.
func (f *jitRegFeedback) KeyAtoms() (uintptr, uintptr) { return f.fb.KeyAtoms() }

// MapNexts is ssa.MapStepFeedback's, jitFeedback's.
func (f *jitRegFeedback) MapNexts() [2]uintptr { return f.fb.MapNexts() }

// TypeofCells is ssa.TypeofFeedback's, jitFeedback's.
func (f *jitRegFeedback) TypeofCells() (uintptr, bool) { return f.fb.TypeofCells() }

// WalkedCloseCell is ssa.WalkedCloseFeedback's, jitFeedback's.
func (f *jitRegFeedback) WalkedCloseCell() uintptr { return f.fb.WalkedCloseCell() }

// TypedKind is ssa.TypedKindFeedback's: the cell the element kinds native
// code met at the program's instruction pc gather in, as jitFeedback's, by
// the register instruction's pc -- kept in the function's lowering, under
// keys of their own (-1-pc).
func (f *jitRegFeedback) TypedKind(pc int) (ssa.TypedSite, bool) {
	s := f.fb.r.jit
	if s == nil || pc < 0 || pc >= len(f.p.Origin) {
		return ssa.TypedSite{}, false
	}
	l := s.lowering(f.fb.fn)
	key := -1 - int32(f.p.Origin[pc])
	cell := l.typedCells[key]
	if cell == nil {
		if l.typedCells == nil {
			l.typedCells = map[int32]*Value{}
		}
		cell = new(Value)
		l.typedCells[key] = cell
		l.bytes += jitTypedCellBytes
		s.lowerBytes += jitTypedCellBytes
	}
	k := f.fb.keep()
	k.cells = append(k.cells, cell)
	return ssa.TypedSite{Kinds: uint16(math.Float64bits(cell.num)), Cell: uintptr(unsafe.Pointer(cell))}, true
}

// jitMegaGetter reports whether a read of cl's register code, at a site
// that is megamorphic -- its cache gave up, or the code met more shapes
// than its cases hold (jitRegSiteMegamorphic) -- has met a getter on its
// receivers' prototypes and receivers of more than one shape: an entry of
// its cache's, or of those it held before (poly, seen), is an accessor's
// (propCache's getter), and their shapes are not all one. Such a callee
// is not inlined (Inline). A site of one receiver shape, whose cache gave
// up on a getter it cannot hold -- a function's legacy arguments, read
// by method.arguments -- is compiled for the getter, which checks that
// shape, and inlined.
func (r *Runtime) jitMegaGetter(cl *closure) bool {
	c := cl.reg
	if c == nil {
		return false
	}
	l := r.jitState().regLearnedOf(r.jitRegKeyOf(cl))
	for rpc, w := range c.rc.Code {
		if op := w.Op(); op != bytecode.RGetProp {
			continue
		}
		site := int(w.C())
		if site >= len(cl.ic) || !r.jitRegSiteMegamorphic(cl, c, l, rpc) {
			continue
		}
		var one *shape
		getter, shapes := false, 0
		note := func(e *propCache) {
			if e.shape == nil || e.shape == noShape {
				return
			}
			getter = getter || e.getter
			if shapes == 0 || e.shape != one && shapes == 1 {
				one = e.shape
				shapes++
			}
		}
		e := &cl.ic[site]
		note(e)
		if p := e.poly; p != nil {
			for i := range p.n {
				note(&p.entries[i])
			}
		}
		if s := e.seenOf(); s != nil {
			for i := range s.n {
				note(&s.entries[i])
			}
		}
		if getter && shapes > 1 {
			return true
		}
	}
	return false
}
