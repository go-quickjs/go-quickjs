//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"math"
	"math/bits"
	"slices"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// jitRegFeedback is the new pipeline's feedback for a function's register
// code, the program compile.LowerReg makes of it (docs/register-
// interpreter.md, Stage 3, section 3, item 3): asked by the program's
// instructions, as the builder asks, and read through each one's origin,
// the register pc, from the caches the register interpreter fills -- a
// property site by its instruction's C, a global's by its C and name, a
// call's by its C (regCalls).
//
// What the code made from the function's register code learned at its
// exits, by register pc (jitState's regLearned), is its jitFeedback's
// entry: the property sites' cases, primitives and megamorphic marks it
// met, and the register pcs whose guards failed (Generic), which the stack
// form's code learns by stack pc in its own entry. What neither records
// yet -- call counts at a site (Frequency) -- it answers with nothing, and
// the builder then speculates nothing there. It is the stack form's
// jitFeedback's other face, of its own type while the stack form's JIT
// stays the default: no method of jitFeedback asks which form it serves,
// and the snapshot of a background compile (zzjit_snapshot_gen.go) is of
// jitFeedback alone.
type jitRegFeedback struct {
	fb *jitFeedback
	rc *bytecode.RegCode
	p  *ir.Program
}

// jitRegFeedbackFor is cl's register code's feedback for p, the program
// LowerReg made of it.
func (r *Runtime) jitRegFeedbackFor(cl *closure, p *ir.Program) *jitRegFeedback {
	if cl == nil || cl.fn == nil || cl.fn.RegCode() == nil || p == nil || p.Origin == nil {
		return nil
	}
	fb := &jitFeedback{r: r, fn: cl.fn, cl: cl, e: r.jitState().regLearnedOf(r.jitKeyOf(cl.fn))}
	return &jitRegFeedback{fb: fb, rc: cl.fn.RegCode(), p: p}
}

// at is the register instruction of the program's instruction pc, and what
// that instruction is.
func (f *jitRegFeedback) at(pc int) (bytecode.RInstr, ir.Op, bool) {
	if pc < 0 || pc >= len(f.p.Code) || int(f.p.Origin[pc]) >= len(f.rc.Code) {
		return 0, 0, false
	}
	return f.rc.Code[f.p.Origin[pc]], f.p.Code[pc].Op, true
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
// writes -- get_prop's, get_method's, get_method_w's, set_prop's, or
// add_prop's read or store, which share the store's site -- its register
// pc, its key, its cache by the register instruction's C (nil past the
// closure's caches), and whether it is a write, a method's read, or a
// plain read (get_prop's).
func (f *jitRegFeedback) propertyAt(pc int) (rpc int, key Atom, c *propCache, write, method, plain, ok bool) {
	w, op, ok := f.at(pc)
	if !ok || f.fb.cl == nil {
		return
	}
	if w.Op() == bytecode.RLit {
		return f.regLitProperty(pc, w, op)
	}
	switch w.Op() {
	case bytecode.RGetProp, bytecode.RGetMethod, bytecode.RGetMethodW, bytecode.RSetProp, bytecode.RAddProp, bytecode.RAddPropI:
	default:
		return 0, 0, nil, false, false, false, false
	}
	if op != ir.PropertyRead && op != ir.ReferenceRead && op != ir.PropertyWrite {
		return 0, 0, nil, false, false, false, false
	}
	site := int(w.C())
	if site >= len(f.rc.SiteName) || int(f.rc.SiteName[site]) >= len(f.fb.cl.names) {
		return 0, 0, nil, false, false, false, false
	}
	key = f.fb.cl.names[f.rc.SiteName[site]]
	if site < len(f.fb.cl.ic) {
		c = &f.fb.cl.ic[site]
	}
	write = op == ir.PropertyWrite
	method = w.Op() == bytecode.RGetMethod || w.Op() == bytecode.RGetMethodW
	plain = w.Op() == bytecode.RGetProp
	return int(f.p.Origin[pc]), key, c, write, method, plain, true
}

// regLitProperty is propertyAt's for regexp_lit's read of its method,
// exec or test, of its literal's RegExp (compile's regexpLit), which no
// cache of the interpreter's has: a cache of the code's own, filled by a
// read of the method off a RegExp of the literal, as the stack form's
// get_prop_this's -- or none where it is found an accessor, which a read
// would call before the call it ends in (ir's windowLast).
func (f *jitRegFeedback) regLitProperty(pc int, w bytecode.RInstr, op ir.Op) (rpc int, key Atom, c *propCache, write, method, plain, ok bool) {
	fb, r := f.fb, f.fb.r
	consts, k := regConsts(fb.cl.fn, f.rc), int(w.C())
	if op != ir.ReferenceRead || fb.e == nil || !f.regLitFed(pc) || k >= len(consts) {
		return 0, 0, nil, false, false, false, false
	}
	rpc = int(f.p.Origin[pc])
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
			return 0, 0, nil, false, false, false, false
		}
		// Filled by the first read, answered by the second.
		c = new(propCache)
		r.cachedProp(c, lit.Object(), key)
		if _, found := r.cachedProp(c, lit.Object(), key); !found || c.getter {
			return 0, 0, nil, false, false, false, false
		}
		fb.e.regLitReads[int32(rpc)] = c
	}
	return rpc, key, c, false, true, false, true
}

// Property is ssa.Feedback's: the property site of a read or a write the
// program's instruction pc makes (propertyAt), as the stack form's
// (jitFeedback's): as its cache knows it, with the cases the code met
// at its exits (jitRegDeoptSeen, jitRegHostSeen), a read's past what a
// cache holds, or a write's additions. A cache that gave up, or holds
// dictionary objects or a chain's cell, or a site that met more than its
// cases hold, is megamorphic; a read whose cache never filled searches the
// receiver's own table (Scan).
func (f *jitRegFeedback) Property(pc int) (ssa.PropertySite, bool) {
	if w, op, ok := f.at(pc); ok && (w.Op() == bytecode.RGetPrivate || w.Op() == bytecode.RSetPrivate) && f.p.Code[pc].Private {
		return f.privateProperty(pc, op)
	}
	rpc, key, cache, write, _, plain, ok := f.propertyAt(pc)
	if !ok {
		return ssa.PropertySite{}, false
	}
	if cache == nil {
		return ssa.PropertySite{Key: uint32(key)}, true
	}
	fb := f.fb
	site, ok := fb.siteOf(key, write, cache)
	if !ok {
		return site, false
	}
	deep := false
	if !write && site.Shape == 0 && site.Holders[0].Object == 0 && (cache.shape == nil || cache.shape == noShape || cache.cell != nil) {
		// A read its cache could not hold, the property past the two
		// prototypes it holds: compiled for the first of the shapes it
		// met, the others its cases.
		if cases := fb.deepCasesOf(rpc, cache); len(cases) != 0 {
			k := cases[0]
			site.Shape, site.Index, site.Holders, site.More, site.Cases = k.Shape, k.Index, k.Holders, k.More, cases[1:]
			deep = true
		}
	}
	if write {
		site.Cases, site.Adds = fb.writeCases(rpc, site)
	} else if site.Shape != 0 && !deep {
		site.Cases = fb.cases(rpc, site.Shape)
	}
	site.Megamorphic = fb.megamorphicOf(rpc, cache, write, site)
	site.Primitives = jitPolyPrim(fb.e, rpc)
	if plain && !site.Megamorphic && cache.fills == 0 {
		site.Scan = true
	}
	return site, true
}

// privateProperty is Property's site for get_private's or set_private's
// read or write, the program's instruction pc, lowered to op, as the stack form's (jitFeedback's
// privateProperty): the property of the key its site's cache (Aux's high
// 16 bits) met it under -- a symbol each evaluation of the class mints,
// which the code checks the key's register holds (ssa.PropertySite's
// Private) -- in the objects of the shapes the cache holds of it, each an
// own data property, a write's writable: the entry's first, the rest its
// cases. A cache that holds none -- or whose first is another evaluation's
// key's -- answers nothing, and the site is Go's, whose read or write
// fills the cache (regGetPrivate, regSetPrivate) for the code compiled
// next.
func (f *jitRegFeedback) privateProperty(pc int, op ir.Op) (ssa.PropertySite, bool) {
	fb := f.fb
	rpc := int(f.p.Origin[pc])
	if fb.cl == nil || rpc >= len(f.rc.Aux) {
		return ssa.PropertySite{}, false
	}
	site := int(f.rc.Aux[rpc] >> 16)
	if site == 0 || site >= len(fb.cl.ic) {
		return ssa.PropertySite{}, false
	}
	write := op == ir.PropertyWrite
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
	c := &fb.cl.ic[site]
	own(c)
	if p := c.poly; p != nil {
		for i := range p.n {
			own(&p.entries[i])
		}
	}
	if len(cases) == 0 {
		return ssa.PropertySite{}, false
	}
	atom := cases[0].shape.keys[cases[0].idx].key
	sym := fb.r.atoms.symbol(atom)
	if sym == nil {
		return ssa.PropertySite{}, false
	}
	// Of the first's key alone: an object of another evaluation's shape is
	// Go's, whose brand check throws.
	cases = slices.DeleteFunc(cases, func(c propCache) bool { return c.shape.keys[c.idx].key != atom })
	ps := ssa.PropertySite{Key: uint32(atom), Private: uintptr(unsafe.Pointer(sym))}
	k := fb.keep()
	for i, c := range cases {
		k.shapes = append(k.shapes, remember(c.shape))
		if i == 0 {
			ps.Shape, ps.Index = uintptr(unsafe.Pointer(c.shape)), c.idx
			continue
		}
		ps.Cases = append(ps.Cases, ssa.PropertyCase{Shape: uintptr(unsafe.Pointer(c.shape)), Index: c.idx})
	}
	return ps, true
}

// StringProperty is ssa.StringPropertyFeedback's: a method's read whose
// cache holds no shape, of a method of the prototype of the primitive its
// receivers have been -- a string's, or the BigInt's, number's or
// boolean's the code met at its exits -- read off that prototype, as the
// stack form's (jitFeedback's stringPropertyOf).
func (f *jitRegFeedback) StringProperty(pc int) (ssa.PropertySite, bool) {
	rpc, key, cache, _, method, _, ok := f.propertyAt(pc)
	if !ok || !method || cache == nil || f.fb.e == nil {
		return ssa.PropertySite{}, false
	}
	return f.fb.stringPropertyOf(rpc, cache, key, false)
}

// Accessor is ssa.AccessorFeedback's: the getter a plain read found on its
// receivers' prototypes, or the setter a write found, as the stack form's
// (jitFeedback's accessorOf): from its cache, the entries it held before,
// and what the code met at its exits. Go calls it from native code: native
// calls of register code's callees, and inlining them, are not made yet.
func (f *jitRegFeedback) Accessor(pc int) (ssa.AccessorSite, bool) {
	rpc, key, cache, write, _, plain, ok := f.propertyAt(pc)
	if !ok || cache == nil || !write && !plain {
		return ssa.AccessorSite{}, false
	}
	a, callee, cl, ok := f.fb.accessorOf(rpc, cache, key, write)
	if !ok || cl == nil {
		return a, ok
	}
	if site, ok := f.accessorInline(callee, cl, write); ok {
		a.Inline = &site
	}
	return a, true
}

// accessorInline is the getter or setter callee, cl's function object, of
// a register read or write, to inline (AccessorSite's Inline), as a
// method's call of it is (Inline): register code native callers may call,
// not the compiled function's, nor an arrow; its window the function, from
// a cell (InlineSite's FunctionCell), the receiver, a setter's value.
func (f *jitRegFeedback) accessorInline(callee *Object, cl *closure, write bool) (ssa.InlineSite, bool) {
	r := f.fb.r
	argc := jitB2i(write)
	if cl.reg == nil || !r.jitRegCallable(callee, cl, cl.reg, argc) || callee.fn().arrow || cl.fn == f.fb.fn || cl.fn == f.fb.keep().fn ||
		jitNoInline != nil && jitNoInline(cl.fn) {
		return ssa.InlineSite{}, false
	}
	p, err := jitcompile.LowerRegInline(cl.reg.rc, cl.fn)
	if err != nil || p.Validate() != nil {
		return ssa.InlineSite{}, false
	}
	r.jitRegSeenFeedback(cl, cl.reg)
	k := f.fb.keep()
	cell := new(Value)
	*cell = Obj(callee)
	k.cells = append(k.cells, cell)
	k.holders = append(k.holders, callee)
	k.inlined = append(k.inlined, cl)
	return ssa.InlineSite{
		Program: p, Feedback: &jitRegFeedback{fb: &jitFeedback{r: r, fn: cl.fn, cl: cl, e: r.jitState().regLearnedOf(r.jitKeyOf(cl.fn)), root: k}, rc: cl.fn.RegCode(), p: p},
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
	case bytecode.RGetGlobal:
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

// CallShape is ssa.CalledFeedback's: the argument count of the call at the
// program's instruction pc, and whether it is a method's -- call_w's and
// call_method_w's, not one in tail position.
func (f *jitRegFeedback) CallShape(pc int) (int, bool, bool) {
	w, _, ok := f.at(pc)
	if !ok {
		return 0, false, false
	}
	rpc := int(f.p.Origin[pc])
	if rpc < len(f.rc.Marks) && f.rc.Marks[rpc]&bytecode.RMarkTail != 0 {
		return 0, false, false
	}
	switch w.Op() {
	case bytecode.RCallW:
		return int(w.X()), false, true
	case bytecode.RCallMW:
		return int(w.X()), true, true
	}
	return 0, false, false
}

// CalledClosure is ssa.CalledClosureFeedback's: nothing yet -- the closure
// made at pc inlined where a call calls it takes its program from its
// register code, which Stage 3a-3's inliner lowers.
func (f *jitRegFeedback) CalledClosure(pc int) (ssa.MadeCallback, ssa.InlineSite, bool) {
	return ssa.MadeCallback{}, ssa.InlineSite{}, false
}

// callTarget is the function the call at the program's instruction pc
// last called, as its site's cache (regCalls, by the register
// instruction's C) holds it: a monomorphic target from the first compile,
// which the stack form's JIT learns at its own exits. Its closure is nil
// for a built-in.
func (f *jitRegFeedback) callTarget(pc int) (*Object, *closure, bool) {
	w, _, ok := f.at(pc)
	if !ok || f.fb.cl == nil {
		return nil, nil, false
	}
	switch w.Op() {
	case bytecode.RCallW, bytecode.RCallMW, bytecode.RNewW:
	default:
		return nil, nil, false
	}
	calls := f.fb.cl.regCalls
	if int(w.C()) >= len(calls) || calls[w.C()].callee == nil {
		return nil, nil, false
	}
	k := &calls[w.C()]
	return k.callee, k.cl, true
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
// (jitState's regLearned), as the stack form's code learns at its exits:
// built generic, what it did not expect is Go's to make.
func (f *jitRegFeedback) Generic(pc int) bool {
	if f.fb.e == nil || pc < 0 || pc >= len(f.p.Origin) {
		return false
	}
	if w, _, ok := f.at(pc); ok && w.Op() == bytecode.RLit {
		// Its expansion, the call's last, which a read built generic
		// would commit before (regLitFed): fed no more instead.
		return false
	}
	return slices.Contains(f.fb.e.failed, int32(f.p.Origin[pc]))
}

// regLitFed reports whether the program's instruction pc, of regexp_lit's
// expansion (compile's regexpLit), is fed: not after a guard of it failed,
// from which on the builder leaves the instruction to Go whole at its
// start -- a literal, a read or a call it is not fed for is an exit there.
func (f *jitRegFeedback) regLitFed(pc int) bool {
	return f.fb.e == nil || !slices.Contains(f.fb.e.failed, int32(f.p.Origin[pc]))
}

// Resumed is ssa.ResumeFeedback's: Go has gone on at register pc rpc, after
// a call the code had no entry after (jitRegResumeSeen), which the next
// compile has one at. The builder asks it by the state's PC, a register
// pc.
func (f *jitRegFeedback) Resumed(rpc int) bool {
	return f.fb.e != nil && slices.Contains(f.fb.e.resumed, uint32(rpc))
}

// EntryGeneric is ssa.Feedback's: an entry at register pc pc whose
// speculation failed before (jitRegEntrySeen), which the builder asks by
// the state's PC, a register pc.
func (f *jitRegFeedback) EntryGeneric(pc int) bool {
	return f.fb.e != nil && slices.Contains(f.fb.e.failedEntries, uint32(pc))
}

// Inline is ssa.Feedback's: a plain call, a method's or a construction of
// a base constructor -- call_w's, call_method_w's, new_w's -- of one
// function, the one its site's cache holds and its code learned it calls
// or constructs with (regCallSites), no other, that runs register code
// native callers may call, not the caller's own nor the compiled
// function's, nor an arrow, is the callee to inline, its program its
// register code lowered for an inlined frame (LowerRegInline), asked by
// its own register feedback, the root's keep holding what its sites name;
// the builder decides whether its program may be (ssa's InlineCalls) and
// fits, and how deep. An inlined callee's calls are asked so too, of its
// own caches and what its code learned, as V8 inlines a callee's callees.
// A call whose inlined callee left native code too often is not inlined
// again (jitInlineLeft's notInline), as the stack form's.
func (f *jitRegFeedback) Inline(pc int) (ssa.InlineSite, bool) {
	w, op, ok := f.at(pc)
	if !ok || op != ir.Host || !f.p.Code[pc].Window {
		return ssa.InlineSite{}, false
	}
	method, construct := w.Op() == bytecode.RCallMW, w.Op() == bytecode.RNewW
	if w.Op() != bytecode.RCallW && !method && !construct {
		return ssa.InlineSite{}, false
	}
	rpc := int32(f.p.Origin[pc])
	if int(rpc) < len(f.rc.Marks) && f.rc.Marks[rpc]&bytecode.RMarkTail != 0 {
		return ssa.InlineSite{}, false
	}
	l := f.fb.e
	if l != nil && slices.Contains(l.notInline, rpc) {
		return ssa.InlineSite{}, false
	}
	argc := int(w.X())
	sites, objs, cls := f.regCallTargets(pc, w, argc, method)
	if len(sites) != 1 || sites[0].ByCode != 0 || sites[0].ArrowThis {
		return ssa.InlineSite{}, false
	}
	site, o, cl := sites[0], objs[0], cls[0]
	if cl.fn == f.fb.fn || cl.fn == f.fb.keep().fn || jitNoInline != nil && jitNoInline(cl.fn) {
		return ssa.InlineSite{}, false
	}
	p, err := jitcompile.LowerRegInline(cl.reg.rc, cl.fn)
	if err != nil || p.Validate() != nil {
		return ssa.InlineSite{}, false
	}
	r := f.fb.r
	// What its sites' caches have seen besides their entries, as its own
	// compiles take it (jitRegSeenFeedback).
	r.jitRegSeenFeedback(cl, cl.reg)
	k := f.fb.keep()
	callee := &jitRegFeedback{fb: &jitFeedback{r: r, fn: cl.fn, cl: cl, e: r.jitState().regLearnedOf(r.jitKeyOf(cl.fn)), root: k}, rc: cl.fn.RegCode(), p: p}
	in := ssa.InlineSite{
		Program: p, Feedback: callee, Callee: site.Callee, Closure: site.Closure,
		Argc: argc, Method: method, Params: 2 + int(cl.reg.params), ThisSlot: -1, Window: true,
		Coerce: site.Coerce, Upvalues: jitUpvalueCells(cl),
	}
	if construct {
		// A construction's (new_w's) of a base constructor, its receiver
		// from the pool of the objects its code learned it makes
		// (regCallSites, ssa.CallSite's Pool), as the stack form inlines
		// one (jitInlineTarget).
		in.Construct, in.Coerce = true, false
		in.Pool, in.ProtoIndex, in.ProtoKey = site.Pool, site.ProtoIndex, site.ProtoKey
	}
	if site.PlainThis {
		// A plain call's receiver, which the callee reads: the global
		// object a sloppy one's is, or undefined.
		cell := new(Value)
		*cell = Undefined
		if site.Global != 0 {
			*cell = r.globalThis
		}
		k.cells = append(k.cells, cell)
		in.ThisCell = uintptr(unsafe.Pointer(cell))
	}
	k.holders = append(k.holders, o)
	k.inlined = append(k.inlined, cl)
	if l != nil && !slices.ContainsFunc(l.inlines, func(x jitInline) bool { return x.pc == rpc && x.obj == o }) {
		// Where its exits are counted (jitInlineLeft).
		l.inlines = append(l.inlines, jitInline{pc: rpc, cl: cl, obj: o})
	}
	return in, true
}

// GoCalls is ssa.GoFeedback's: native code made from register code calls
// Go, which makes what needs no frame of the VM's from what the code
// passes it, and a call on the register frame (jitRegGoCall); what needs
// the stack form's frame, a call of Go refuses, and the code leaves there.
func (f *jitRegFeedback) GoCalls() bool { return true }

// NativeCalls is ssa.Feedback's: a call -- call_w's, call_method_w's or
// new_w's, its operands no more than a call of Go takes -- of the function
// its site's cache holds (regCalls), or of one its code learned it calls,
// a construction's with a pool of its objects, where that runs register
// code native callers may call, is a native call of it (regCallSites); any
// other is a call of Go (ssa.GoCall), which makes it on the register frame
// without leaving native code (jitRegGoCall). A call in tail position is
// neither, which the interpreter makes in constant space at a host exit.
func (f *jitRegFeedback) NativeCalls(pc int) []ssa.CallSite {
	w, op, ok := f.at(pc)
	if !ok || op != ir.Host || !f.p.Code[pc].Window {
		return nil
	}
	switch w.Op() {
	case bytecode.RTypeof:
		// Its name, which Go makes from native code.
		return []ssa.CallSite{{Go: ssa.GoCall, TypeOf: true, ThisSlot: -1}}
	case bytecode.RInstanceOf:
		// Go's, from native code, by the site's cache, as the interpreter
		// makes it: it may run code, a Symbol.hasInstance method or a
		// proxy's trap.
		return []ssa.CallSite{{Go: ssa.GoCall, InstanceOf: true, Argc: 1, ThisSlot: -1}}
	}
	rpc := int(f.p.Origin[pc])
	if w.Op() == bytecode.RLit {
		// regexp_lit's call of the method its expansion read, of its one
		// argument: Go's from native code, as call_method_w's marked to
		// give the literal back (jitRegGoCall), or the leaf's (Leaf).
		if !f.regLitFed(pc) {
			return nil
		}
		return []ssa.CallSite{{Go: ssa.GoCall, Argc: 1, Method: true, ThisSlot: -1}}
	}
	if rpc < len(f.rc.Marks) && f.rc.Marks[rpc]&bytecode.RMarkTail != 0 {
		return nil
	}
	// A call in tail position (tail_call_w, tail_call_method_w) is made as
	// the ordinary call, where the stack is shallow (ir's Tail).
	argc, method := int(w.X()), w.Op().MethodCall()
	n := argc + 1
	if method {
		n++
	}
	if n > abi.MaxGoArgs {
		return nil
	}
	if sites := f.regCallSites(pc, w, argc, method); len(sites) != 0 {
		return sites
	}
	if site, ok := f.regBuiltinSite(w, argc, method); ok {
		return []ssa.CallSite{site}
	}
	return []ssa.CallSite{{Go: ssa.GoCall, Argc: argc, Method: method, New: w.Op() == bytecode.RNewW, ThisSlot: -1}}
}

// regBuiltinSite is the call of a built-in native code makes itself whose
// target call_method_w w's site's cache holds (regCallNative), as the stack
// form's NativeCalls makes one jitSeedBuiltin or jitCallSeen found: the
// realm's Array.prototype.pop, of no argument, or push, of one, whose fast
// path the call's checks keep to -- the receiver an array of the realm's
// prototype, and, for push, the prototypes as they are now, with no index
// in their tables. The call leaves anything else to Go.
func (f *jitRegFeedback) regBuiltinSite(w bytecode.RInstr, argc int, method bool) (ssa.CallSite, bool) {
	r := f.fb.r
	calls := f.fb.cl.regCalls
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
	case argc == 1 && fd.elemOp == elemPush:
		ap, op := r.proto.array, r.proto.object
		if ap.shape == nil || op.shape == nil || ap.shape.loose || op.shape.loose || ap.proto() != op || op.proto() != nil ||
			!ap.noIndexKeys() || !op.noIndexKeys() {
			return ssa.CallSite{}, false
		}
		k.shapes = append(k.shapes, remember(ap.shape), remember(op.shape))
		k.holders = append(k.holders, o, ap, op)
		return ssa.CallSite{Callee: uintptr(unsafe.Pointer(o)), ThisSlot: -1, Argc: 1, Method: true, Push: true,
			Protos: [2]ssa.Holder{{Object: uintptr(unsafe.Pointer(ap)), Shape: uintptr(unsafe.Pointer(ap.shape))},
				{Object: uintptr(unsafe.Pointer(op)), Shape: uintptr(unsafe.Pointer(op.shape))}}}, true
	}
	return ssa.CallSite{}, false
}

// BigIntSole is ssa.BigIntRecycleFeedback's: the registers of the first
// 64 that hold their values alone (regSoleRegisters), as the stack form's
// locals do (soleLocals), and whether the code may give BigInts' blocks
// back at all, as the stack form's code of a function that does not
// suspend may (register code that suspends is not compiled).
func (f *jitRegFeedback) BigIntSole() (uint64, bool) {
	fn := f.fb.fn
	if bits.UintSize != 64 || fn.Generator || fn.Async {
		return 0, false
	}
	return regSoleRegisters(fn, f.rc, f.p), true
}

// regSoleRegisters is the registers of rc, fn's register code lowered to
// p, of the first 64, that hold their values alone: past the function,
// this and the parameters, none a closure captures (ir.Program's
// Captured), each read only as an operand of an operator that makes a
// value of its own -- arithmetic, bitwise, a comparison, a test, a
// branch -- in a function that is no generator, no async function, has no
// direct eval and no arguments object, of instructions this knows: any
// other has the function none. A move from one, a call's window or a
// return of it, a store of it to a global, a read of its property or a
// postfix update of it, which leaves its value in another register, has
// it hold its value no more. What an operator stores to one is the
// register's alone (the VM's bigSoleBit), and its old value given back
// where it was so (ssa's ReleaseOld), as the stack form's soleLocals says
// of its locals.
func regSoleRegisters(fn *bytecode.Function, rc *bytecode.RegCode, p *ir.Program) uint64 {
	if bits.UintSize != 64 || fn.Generator || fn.Async || fn.HasDirectEval || fn.UsesArguments || rc.UsesArguments || rc.LocalReg != nil {
		return 0
	}
	var cand uint64
	for k := 2 + fn.ParamCount; k < rc.NumRegs && k < 64; k++ {
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
			bytecode.RLoadTrue, bytecode.RLoadFalse, bytecode.RLoadEmpty, bytecode.RLoadUninit, bytecode.RGetGlobal,
			bytecode.RAdd, bytecode.RSub, bytecode.RMul, bytecode.RBin, bytecode.RAddI, bytecode.RSubI,
			bytecode.RBitAnd, bytecode.RBitOr, bytecode.RBitXor, bytecode.RShl, bytecode.RShr, bytecode.RUShr,
			bytecode.RAndI, bytecode.ROrI, bytecode.RXorI, bytecode.RShlI, bytecode.RShrI, bytecode.RUShrI,
			bytecode.RInc, bytecode.RDec, bytecode.RUnary,
			bytecode.RJmp, bytecode.RJmpF, bytecode.RJmpT,
			bytecode.RJLt, bytecode.RJNLt, bytecode.RJLe, bytecode.RJNLe, bytecode.RJGt, bytecode.RJNGt, bytecode.RJGe, bytecode.RJNGe,
			bytecode.RJLtI, bytecode.RJNLtI, bytecode.RJLeI, bytecode.RJNLeI, bytecode.RJGtI, bytecode.RJNGtI, bytecode.RJGeI, bytecode.RJNGeI,
			bytecode.RJSEq, bytecode.RJSNe, bytecode.RJSEqI, bytecode.RJSNeI,
			bytecode.RJEq, bytecode.RJNe, bytecode.RJEqI, bytecode.RJNeI, bytecode.RJNull, bytecode.RJNotNull,
			bytecode.RRetUndef:
			// Operands, results of their own, tests and branches.
		case bytecode.RMove, bytecode.RPostInc, bytecode.RPostDec, bytecode.RGetLength, bytecode.RGetMethod, bytecode.RGetMethodW, bytecode.RGetProp:
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
// instruction pc -- call_w's or call_method_w's, not one in tail position
// -- last called, as its site's cache holds it (regCalls), where that is a
// leaf of the realm's that native code calls without a frame
// (jitLeafOp): answered from the interpreter's cache at the first compile,
// where the stack form's code learns it at the calls of Go its native code
// makes (jitLeafSeen).
func (f *jitRegFeedback) Leaf(pc int) (uintptr, bool) {
	w, op, ok := f.at(pc)
	if ok && op == ir.Host && f.p.Code[pc].Window && w.Op() == bytecode.RLit {
		// regexp_lit's call: the realm's exec or test, read where it is
		// the built-in (regexpLitMethod), a leaf as the stack form's code
		// learns it at its calls of Go (jitRegExpLitSite); the literal's
		// RegExp, from the pool, nothing keeps.
		m := f.fb.r.regexpLitMethod(w.X()&bytecode.RLitTest != 0)
		if m == nil || !f.regLitFed(pc) {
			return 0, false
		}
		f.fb.keep().holders = append(f.fb.keep().holders, m)
		return uintptr(unsafe.Pointer(m)), true
	}
	if !ok || op != ir.Host || !f.p.Code[pc].Window || w.Op() != bytecode.RCallW && w.Op() != bytecode.RCallMW {
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
	// RegExp's exec and test too, as the stack form's code learns them at
	// its calls of Go (jitRegExpLitSite) -- a literal's call among them
	// (RMarkLitRecv), its RegExp then from the pool (regexp_lit's
	// lowering, compile's regexpLit), one whose value nothing reads but
	// its truth (RMarkDead), which the leaf makes whole, and an exec whose
	// array the interpreter may make again in the memory of the last
	// (RMarkReuse), which the leaf makes anew, as the stack form's native
	// code does (zzz_regexpreuse.go): the old array, which only the local
	// the new one replaces could reach, the interpreter's next call may
	// make again, its slots' check finding the local holds it no more --
	// but not where the call makes its first argument (RMarkLitArg),
	// which the interpreter's call does (regMarkedCall).
	regexp := (fd.elemOp == elemRegExpExec || fd.elemOp == elemRegExpTest) && marks&bytecode.RMarkLitArg == 0
	// String.prototype.replace of two arguments, s.replace(re, ""), whose
	// leaf checks at each call that they are a RegExp it replaces with
	// directly and a string (jitReplaceLeaf), as the stack form's code
	// learns it at its calls of Go (jitReplaceLeafSeen) or guesses it
	// (jitReplaceLeafGuess) -- but not where the call makes its RegExp, a
	// literal's (RMarkLitArg), whose call of Go gives the literal its
	// RegExp back, as the stack form's does.
	replace := callee == f.fb.r.strReplaceFn && w.Op() == bytecode.RCallMW && w.X() == 2 && marks&bytecode.RMarkLitArg == 0
	if !jitLeafOp(fd.elemOp) && !regexp && !replace {
		return 0, false
	}
	f.fb.keep().holders = append(f.fb.keep().holders, callee)
	return uintptr(unsafe.Pointer(callee)), true
}

// methodCall is the built-in the call at the program's instruction pc --
// call_w's, or call_method_w's where method says, not one in tail
// position -- last called, as its site's cache holds it (regCalls), and
// its argument count: the function the stack form's code finds read
// before its call, or learns at its calls of Go.
func (f *jitRegFeedback) methodCall(pc int, method bool) (*Object, int, bool) {
	w, op, ok := f.at(pc)
	want, tail := bytecode.RCallW, bytecode.RCallWT
	if method {
		want, tail = bytecode.RCallMW, bytecode.RCallMWT
	}
	if !ok || op != ir.Host || !f.p.Code[pc].Window || w.Op() != want && w.Op() != tail {
		return nil, 0, false
	}
	if rpc := int(f.p.Origin[pc]); rpc < len(f.rc.Marks) && f.rc.Marks[rpc]&bytecode.RMarkTail != 0 {
		return nil, 0, false
	}
	callee, cl, ok := f.callTarget(pc)
	if !ok || cl != nil {
		return nil, 0, false
	}
	return callee, int(w.X()), true
}

// CharCode is ssa.CharCodeFeedback's: the method call of one argument at
// the program's instruction pc calls String.prototype.charCodeAt, the
// realm's own (methodCall).
func (f *jitRegFeedback) CharCode(pc int) bool {
	o, n, ok := f.methodCall(pc, true)
	return ok && n == 1 && f.fb.r.jitCharCodeAt.IsObject() && o == f.fb.r.jitCharCodeAt.Object()
}

// Substring is ssa.SubstringFeedback's: String.prototype's substring,
// slice, substr or charAt, the realm's own, which the method call at the
// program's instruction pc calls (methodCall), and which it is.
func (f *jitRegFeedback) Substring(pc int) (uintptr, int, bool) {
	o, n, ok := f.methodCall(pc, true)
	if !ok || !f.fb.r.jitIsSubstring(o, n) {
		return 0, 0, false
	}
	kind, _ := f.fb.r.jitSubstringOf(o)
	return uintptr(unsafe.Pointer(o)), kind, true
}

// FromCharCode is ssa.FromCharCodeFeedback's: String.fromCharCode, the
// realm's own, which the method call of one argument at the program's
// instruction pc calls (methodCall).
func (f *jitRegFeedback) FromCharCode(pc int) (uintptr, bool) {
	o, n, ok := f.methodCall(pc, true)
	if !ok || n != 1 || !f.fb.r.jitFromCharCode.IsObject() || o != f.fb.r.jitFromCharCode.Object() {
		return 0, false
	}
	return uintptr(unsafe.Pointer(o)), true
}

// Intrinsic is ssa.IntrinsicFeedback's: the realm's Math function a call
// at the program's instruction pc -- a method's, Math.sqrt(x), or a plain
// one of the function read before -- calls (methodCall), made natively or
// by Go called from native code, as the stack form's (mathIntrinsic). The
// BigInt functions native code makes are the stack form's alone yet.
func (f *jitRegFeedback) Intrinsic(pc int) (ssa.Intrinsic, bool) {
	o, n, ok := f.methodCall(pc, true)
	plain := false
	if !ok {
		if o, n, ok = f.methodCall(pc, false); !ok {
			return ssa.Intrinsic{}, false
		}
		plain = true
	}
	if n > jitMathArgs {
		return ssa.Intrinsic{}, false
	}
	fd := o.fn()
	if fd == nil || fd.native == nil {
		return ssa.Intrinsic{}, false
	}
	if fd.mathOp == mathBigInt || fd.mathOp == mathAsIntN || fd.mathOp == mathAsUintN {
		// BigInt's that native code makes (bigint.go in ssa), as the stack
		// form's (jitBigIntIntrinsic): BigInt(x) of one argument, plain;
		// BigInt.asUintN(n, x) or asIntN's whose width n the window's
		// register is given as a constant from 0 to 64 before the call,
		// on the way to it alone (regConstOperand).
		k := ssa.Intrinsic{Callee: uintptr(unsafe.Pointer(o))}
		switch {
		case plain:
			if fd.mathOp != mathBigInt || n != 1 {
				return ssa.Intrinsic{}, false
			}
			k.Op, k.Plain = ssa.OpBigOfF64, true
		case n == 2 && fd.mathOp != mathBigInt:
			width, ok := regConstOperand(f.p, pc, f.p.Code[pc].Left.Slot+2)
			if !ok || width != math.Trunc(width) || width < 0 || width > 64 {
				return ssa.Intrinsic{}, false
			}
			k.Op, k.Fn = ssa.OpZextW, int(width)
			if fd.mathOp == mathAsIntN {
				k.Op = ssa.OpSextW
			}
		default:
			return ssa.Intrinsic{}, false
		}
		f.fb.keep().holders = append(f.fb.keep().holders, o)
		return k, true
	}
	return f.fb.mathIntrinsic(o, n, plain)
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
// across its compiles, as the stack form's entry keeps its own by stack
// pc (jitLiteralAt).
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
		// constants written to them (jitArrayLiteral), as the stack form's.
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
	}
	return nil
}

// jitRegSharedLiteral is the literal of new_array_k w at register pc rpc
// of cl's register code rc, made in e: an array of the constants of
// Literals[Aux[rpc]] whose arrays share them, natively from a pool of
// arrays made as the interpreter makes them (rsNewArrayK, sharedShare),
// over its site's storage, made here where the interpreter has not made
// it -- as the stack form's new_array of constants (jitLiteralAt,
// sharedLiteralSite). nil where the site has given sharing up, its arrays
// then Go's.
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
	l.pool.Source = unsafe.Pointer(&jitPoolSource{array: true, n: n, sharedOf: cl, sharedReg: true, sharedPC: int32(w.C())})
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
	i := pc - first - 1
	if i < 0 || i >= len(keys) {
		return nil, false
	}
	for _, k := range keys[:i+1] {
		if int(k) >= len(cl.names) {
			return nil, false
		}
	}
	s := l.root
	for _, k := range keys[:i] {
		if s.next == nil || s.nextEdge() != (shapeEdge{cl.names[k], propDefault}) {
			return nil, false
		}
		s = s.next
	}
	key := cl.names[keys[i]]
	if s.next == nil || s.nextEdge() != (shapeEdge{key, propDefault}) {
		return nil, false
	}
	next := s.next
	if next.index == nil && int(next.n) > linearScanLimit {
		return nil, false
	}
	k := f.fb.keep()
	k.shapes = append(k.shapes, remember(s), remember(next))
	return &ssa.PropertyAdd{From: uintptr(unsafe.Pointer(s)), Next: uintptr(unsafe.Pointer(next)), Flags: uint8(next.flags), Define: true, Key: uint32(key)}, true
}

// LeafKind is ssa.LeafKindFeedback's, jitFeedback's.
func (f *jitRegFeedback) LeafKind(fn uintptr) ssa.LeafKind { return f.fb.LeafKind(fn) }

// The runtime's addresses the builder asks once a function, the same for
// either form's code: jitFeedback's answers.

// ArrayProtos is ssa.AppendFeedback's, jitFeedback's.
func (f *jitRegFeedback) ArrayProtos() ([2]ssa.Holder, bool) { return f.fb.ArrayProtos() }

// GenNext is ssa.GenStepFeedback's half that is the realm's, jitFeedback's.
func (f *jitRegFeedback) GenNext() uintptr { return f.fb.GenNext() }

// GenSteps is ssa.GenStepFeedback's other half: no generator is known to
// a register loop's step yet (jitEntry's genSteps are the stack form's).
func (f *jitRegFeedback) GenSteps(pc int) []ssa.GenStep { return nil }

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
// code met at the program's instruction pc gather in, as jitFeedback's,
// by the register instruction's pc -- kept in the function's lowering
// beside the stack form's cells, under keys of their own (-1-pc).
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
