//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
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
// It answers what register code records today. What the register
// interpreter does not record yet -- what a fast path did not expect
// (Generic), call counts at a site (Frequency), the JIT's own tables
// learned at its exits, which the stack form's code keys by its stack pcs
// -- it answers with nothing, and the builder then speculates nothing
// there. It is the stack form's jitFeedback's other face, of its own type
// while the stack form's JIT stays the default: no method of jitFeedback
// asks which form it serves, and the snapshot of a background compile
// (zzjit_snapshot_gen.go) is of jitFeedback alone.
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
	return &jitRegFeedback{fb: &jitFeedback{r: r, fn: cl.fn, cl: cl}, rc: cl.fn.RegCode(), p: p}
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

// Property is ssa.Feedback's: the property site of a read or a write the
// program's instruction pc makes -- get_prop's, get_method's,
// get_method_w's, set_prop's, or add_prop's read or store, which share the
// store's site -- as its cache, by the register instruction's C, knows it.
// A cache that gave up, or holds dictionary objects or a chain's cell, is
// megamorphic; a read whose cache never filled searches the receiver's own
// table (Scan), as the stack form's.
func (f *jitRegFeedback) Property(pc int) (ssa.PropertySite, bool) {
	w, op, ok := f.at(pc)
	if !ok || f.fb.cl == nil {
		return ssa.PropertySite{}, false
	}
	switch w.Op() {
	case bytecode.RGetProp, bytecode.RGetMethod, bytecode.RGetMethodW, bytecode.RSetProp, bytecode.RAddProp, bytecode.RAddPropI:
	default:
		return ssa.PropertySite{}, false
	}
	if op != ir.PropertyRead && op != ir.ReferenceRead && op != ir.PropertyWrite {
		return ssa.PropertySite{}, false
	}
	c := int(w.C())
	if c >= len(f.rc.SiteName) || int(f.rc.SiteName[c]) >= len(f.fb.cl.names) {
		return ssa.PropertySite{}, false
	}
	key := f.fb.cl.names[f.rc.SiteName[c]]
	if c >= len(f.fb.cl.ic) {
		return ssa.PropertySite{Key: uint32(key)}, true
	}
	cache := &f.fb.cl.ic[c]
	write := op == ir.PropertyWrite
	site, ok := f.fb.siteOf(key, write, cache)
	if !ok {
		return site, false
	}
	site.Megamorphic = cache.fills >= maxCacheFills || cache.shape == dictShape || cache.cell != nil
	if !write && !site.Megamorphic && w.Op() == bytecode.RGetProp && cache.fills == 0 {
		site.Scan = true
	}
	return site, true
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
// program's instruction pc reads -- load_const's, a switch's case's, or
// load_empty's empty string -- and whether it is one of op's.
func (f *jitRegFeedback) constant(pc int, op ir.Op) (Value, bool) {
	w, iop, ok := f.at(pc)
	if !ok || iop != op || f.fb.cl == nil {
		return Value{}, false
	}
	if w.Op() == bytecode.RLoadEmpty {
		return Str(emptyString), op == ir.StringConst
	}
	k := f.p.Code[pc].Key
	if w.Op() != bytecode.RLoadConst && w.Op() != bytecode.RSwitch || int(k) >= len(f.fb.cl.consts) {
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

// Generic is ssa.Feedback's: nothing, until the register interpreter
// records what its fast paths did not expect (Stage 3a-1's regSeen).
func (f *jitRegFeedback) Generic(pc int) bool { return false }

// EntryGeneric is ssa.Feedback's: nothing, as Generic.
func (f *jitRegFeedback) EntryGeneric(pc int) bool { return false }

// Inline is ssa.Feedback's: nothing yet. The call's target is known
// (callTarget); making it an InlineSite takes the callee's program, which
// the stack form's JIT lowers from its stack code (Stage 3a-3).
func (f *jitRegFeedback) Inline(pc int) (ssa.InlineSite, bool) { return ssa.InlineSite{}, false }

// NativeCalls is ssa.Feedback's: nothing yet, as Inline: a native call
// links the callee's code, compiled from its stack form.
func (f *jitRegFeedback) NativeCalls(pc int) []ssa.CallSite { return nil }
