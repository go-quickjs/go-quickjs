//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"slices"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// Elements' kinds learned at register code's exits (jitRegElemSeen,
// jitRegStringAddSeen): an element read that met a typed array, a BigInt
// typed array, a string or an object read by a name, an element write to an
// object native code does not store to, an addition that met a string --
// kept by register pc in what the function's register code learned
// (jitState's regLearned), its lists (typedReads, stringReads, keyedReads,
// keyedWrites), and the lowering's hints (jitLearned's regHints), with
// which every lowering of its register code is made (jitRegLower).

// jitRegLower is cl's register code rc lowered as kind says -- for an entry
// (jitLowerSSA), hot (jitLowerHot), or for native callers and inlining
// (jitLowerInline) -- with the hints its code learned at its exits, from
// the runtime's programs (regLowered): each kind is lowered once for the
// hints as they are, and again once its code has learned more
// (regLowering). A compile asks for its function's program and for each
// callee's it inlines, and a function is compiled again as it learns and
// inlined into each of its callers: lowered anew at every ask, Octane's
// Typescript lowered some 0.6 GB of programs a run, half its compiles'
// time. A program is checked (ir.Program's Validate) once, as it is
// lowered, its error the lowering's: checked at every ask, the checks of
// the callees inlined were a sixth of the build's time.
func (r *Runtime) jitRegLower(cl *closure, rc *bytecode.RegCode, kind jitLowerKind) (*ir.Program, error) {
	s := r.jit
	if s == nil || rc == nil || rc != cl.fn.RegCompiled() {
		// Before the runtime has any state, which no compile is, or code
		// not the function's own register code, which no key names.
		return jitRegLowerWith(rc, cl.fn, kind, jitcompile.Hints{KeepParams: r.nodeQuirks && !cl.fn.Strict})
	}
	l := s.regLowering(r.jitRegKeyOf(cl))
	if !l.done[kind] {
		h := l.hints
		h.KeepParams = r.nodeQuirks && !cl.fn.Strict
		l.progs[kind], l.errs[kind] = jitRegLowerWith(rc, cl.fn, kind, h)
		l.done[kind] = true
		n := l.progs[kind].Footprint()
		l.bytes += n
		s.lowerBytes += n
	}
	return l.progs[kind], l.errs[kind]
}

// regLowering is the register lowering of the function whose key is key,
// made if the runtime has none, and found used now: its programs lowered
// with the hints its code has learned (jitLearned's regHints), dropped
// (relower) once it has learned more. The hints only grow, each list
// appended to, so their lengths say whether the programs are of them all.
func (s *jitState) regLowering(key jitKey) *jitLowering {
	var h jitcompile.Hints
	if l := s.regLearned[key]; l != nil {
		h = l.regHints
	}
	l := s.regLowered[key]
	if l == nil {
		if s.regLowered == nil {
			s.regLowered = map[jitKey]*jitLowering{}
		}
		l = &jitLowering{bytes: jitLoweringFixed}
		s.regLowered[key] = l
		s.lowerBytes += l.bytes
	} else if len(l.hints.StringAdds) != len(h.StringAdds) || len(l.hints.ElementReads) != len(h.ElementReads) ||
		len(l.hints.StringReads) != len(h.StringReads) {
		s.relower(l)
	}
	// The learned lists themselves, which their entry owns and counts:
	// the lowering only reads them.
	l.hints = h
	l.used = s.agedAt
	return l
}

// dropRegLowering forgets the register lowering l of the function whose key
// is key.
func (s *jitState) dropRegLowering(key jitKey, l *jitLowering) {
	delete(s.regLowered, key)
	s.lowerBytes -= l.bytes
	s.floorAt = 0
}

// jitRegLowerWith is fn's register code rc lowered as kind says, with
// hints, and checked: a program Validate finds wrong is its error.
func jitRegLowerWith(rc *bytecode.RegCode, fn *bytecode.Function, kind jitLowerKind, h jitcompile.Hints) (*ir.Program, error) {
	var p *ir.Program
	var err error
	if rc.Typed {
		// Typed code's, a twin's (asmTwin): one lowering for every kind.
		return jitTypedLower(rc, fn)
	}
	switch kind {
	case jitLowerInline:
		p, err = jitcompile.LowerRegInlineWith(rc, fn, h)
	case jitLowerHot:
		p, err = jitcompile.LowerRegHotWith(rc, fn, h)
	case jitLowerForward:
		p, err = jitcompile.LowerRegForwardWith(rc, fn, h)
	default:
		p, err = jitcompile.LowerRegWith(rc, fn, h)
	}
	if err == nil {
		// Checked once, here: the lowering is kept, and built again at
		// each compile and each ask to inline it (ir.Program's Checked).
		err = p.Validate()
		p.Checked = err == nil
	}
	return p, err
}

// jitRegElemRead is the receiver and the key of the element read of the
// register instruction at rpc of c, on the frame whose window is at wb,
// as the instruction's start has them -- get_index's, get_index_imm's,
// get_index_pre's (its key stepped), get_index_post's, an add_index's --
// or false for any other instruction.
func (r *Runtime) jitRegElemRead(c *regCode, wb, rpc int) (o, k Value, ok bool) {
	if rpc < 0 || rpc >= len(c.rc.Code) {
		return Undefined, Undefined, false
	}
	w := c.rc.Code[rpc]
	at := func(n uint16) Value {
		if wb+int(n) < len(r.stack) {
			return r.stack[wb+int(n)]
		}
		return Undefined
	}
	switch w.Op() {
	case bytecode.RGetIndex, bytecode.RGetIndexPost:
		return at(w.B()), at(w.C()), true
	case bytecode.RGetIndexI:
		return at(w.B()), Float(float64(w.C())), true
	case bytecode.RGetIndexPre:
		k := at(w.C())
		if !k.IsNumber() {
			return Undefined, Undefined, false
		}
		return at(w.B()), Float(k.num + float64(int8(w.X()))), true
	case bytecode.RAddIndex, bytecode.RAddIndexI:
		return at(w.A()), at(w.B()), true
	case bytecode.RAddIndexK:
		return at(w.A()), Float(float64(w.C())), true
	}
	return Undefined, Undefined, false
}

// jitRegElemWrite is the receiver, the key and the value of the element
// write of the register instruction at rpc of c -- set_index's,
// set_index_imm's, an add_index's, whose value is its operand -- or false.
func (r *Runtime) jitRegElemWrite(c *regCode, wb, rpc int) (o, k, v Value, ok bool) {
	if rpc < 0 || rpc >= len(c.rc.Code) {
		return Undefined, Undefined, Undefined, false
	}
	w := c.rc.Code[rpc]
	at := func(n uint16) Value {
		if wb+int(n) < len(r.stack) {
			return r.stack[wb+int(n)]
		}
		return Undefined
	}
	switch w.Op() {
	case bytecode.RSetIndex:
		return at(w.A()), at(w.B()), at(w.C()), true
	case bytecode.RSetIndexI:
		return at(w.A()), Float(float64(w.B())), at(w.C()), true
	case bytecode.RAddIndex:
		return at(w.A()), at(w.B()), at(w.C()), true
	case bytecode.RAddIndexI:
		return at(w.A()), at(w.B()), Float(float64(w.SC())), true
	case bytecode.RAddIndexK:
		return at(w.A()), Float(float64(w.C())), at(w.B()), true
	}
	return Undefined, Undefined, Undefined, false
}

// jitRegElemSeen learns, in l, what the register code's element access at
// register pc rpc of c met, on the frame whose window is at wb, before Go
// runs it: at a host exit (reference), what a read made by its cell met --
// a string's, a typed array's or a BigInt typed array's element, or an
// object's property by its name, which the next compile reads as such, or
// Go from native code (ssa's StringRead, TypedRead, TypedBigRead,
// KeyedRead) -- and a write to an object native code does not store to,
// which Go stores from native code (KeyedWrite); at a deoptimization, a
// read made as a number that met what is none, read as it is from the next
// lowering on (Hints' ElementReads, StringReads). It reports whether it
// learned anything.
func (r *Runtime) jitRegElemSeen(l *jitEntry, c *regCode, wb, rpc int, reference bool) bool {
	learned := false
	if o, k, ok := r.jitRegElemRead(c, wb, rpc); ok {
		learned = r.jitRegIndexSeen(l, rpc, o, k, reference)
	}
	if !reference {
		return learned
	}
	if o, _, _, ok := r.jitRegElemWrite(c, wb, rpc); ok && r.jitTypedWriteSeen(c, rpc, o) {
		learned = true
	}
	if o, k, v, ok := r.jitRegElemWrite(c, wb, rpc); ok && !slices.Contains(l.keyedWrites, int32(rpc)) &&
		o.IsObject() && o.Object().class != ClassArray && o.Object().class != ClassTypedArray && r.jitKeyedWrite(o, k, v, false) {
		l.keyedWrites = append(l.keyedWrites, int32(rpc))
		learned = true
	}
	return learned
}

// jitRegIndexSeen learns the element read at register pc rpc, of receiver o
// at key k, in l.
func (r *Runtime) jitRegIndexSeen(l *jitEntry, rpc int, o, k Value, reference bool) bool {
	pc := int32(rpc)
	if slices.Contains(l.typedReads, pc) || jitBigReadAt(l.typedReads, rpc) != 0 || slices.Contains(l.stringReads, pc) || slices.Contains(l.keyedReads, pc) {
		return false
	}
	if !reference {
		keyed := o.IsObject() && (k.IsString() || k.IsNumber() && jitKeyedClass(o.Object()))
		chars := o.IsString() && k.IsNumber()
		big := int32(0)
		if !keyed && !chars && k.IsNumber() {
			big = jitBigTyped(o)
		}
		h := &l.regHints
		switch {
		case big != 0:
		case keyed:
			if _, ok := r.jitKeyedRead(o, k); !ok {
				return false
			}
		case o.IsObject() && k.IsNumber():
			// An array's element that is a string: read as it is, its
			// additions Go's (stringConcats).
			if v, ok := r.jitKeyedRead(o, k); !ok || !v.IsString() || slices.Contains(h.StringReads, pc) {
				return false
			}
		case !chars:
			return false
		}
		hint := &h.ElementReads
		if !keyed && big == 0 {
			hint = &h.StringReads
		}
		if !slices.Contains(*hint, pc) {
			*hint = append(*hint, pc)
		}
		switch {
		case big != 0:
			l.typedReads = append(l.typedReads, pc|big)
		case keyed:
			l.keyedReads = append(l.keyedReads, pc)
		case chars:
			l.stringReads = append(l.stringReads, pc)
		}
		return true
	}
	switch {
	case o.IsString() && k.IsNumber():
		l.stringReads = append(l.stringReads, pc)
	case k.IsNumber() && jitBigTyped(o) != 0:
		l.typedReads = append(l.typedReads, pc|jitBigTyped(o))
	case o.IsObject() && o.Object().class == ClassTypedArray:
		l.typedReads = append(l.typedReads, pc)
	case o.IsObject() && (k.IsString() || k.IsNumber() && jitKeyedClass(o.Object())):
		if _, ok := r.jitKeyedRead(o, k); !ok {
			return false
		}
		l.keyedReads = append(l.keyedReads, pc)
	default:
		return false
	}
	return true
}

// jitRegStringAddSeen learns, at a deoptimization of register code at
// register pc rpc of c, whose frame's window at wb holds the state at the
// instruction's start, an addition -- add's, add_imm's -- that met a
// string, which every lowering from here makes Go's (Hints' StringAdds),
// its operands read as they are. It reports whether it learned one.
func (r *Runtime) jitRegStringAddSeen(l *jitEntry, c *regCode, wb, rpc int) bool {
	if rpc < 0 || rpc >= len(c.rc.Code) || slices.Contains(l.regHints.StringAdds, int32(rpc)) {
		return false
	}
	w := c.rc.Code[rpc]
	str := func(n uint16) bool { return wb+int(n) < len(r.stack) && r.stack[wb+int(n)].IsString() }
	switch w.Op() {
	case bytecode.RAdd, bytecode.RAddI32:
		// add_i32's too, which the lowering makes an add and a ToInt32.
		if !str(w.B()) && !str(w.C()) {
			return false
		}
	case bytecode.RAddI, bytecode.RAddII32:
		// add_imm_i32's of x - K is a subtraction, which no string adds.
		if w.Op() == bytecode.RAddII32 && w.X() == bytecode.RAddII32Sub || !str(w.B()) {
			return false
		}
	case bytecode.RStepJLt, bytecode.RStepJLe, bytecode.RStepJLtI, bytecode.RStepJGtI, bytecode.RStepJGeI, bytecode.RStepJLeU:
		// A step-and-branch's step, add_imm's where it adds.
		if w.X()&bytecode.RStepAdd == 0 || bytecode.StepK(w.X()) < 0 || !str(w.A()) {
			return false
		}
	default:
		return false
	}
	l.regHints.StringAdds = append(l.regHints.StringAdds, int32(rpc))
	return true
}

// regElemPC is the register pc of the program's instruction pc, if it is
// of op.
func (f *jitRegFeedback) regElemPC(pc int, op ir.Op) (int32, bool) {
	if f.fb.e == nil || pc < 0 || pc >= len(f.p.Code) || f.p.Code[pc].Op != op {
		return 0, false
	}
	return int32(f.p.Origin[pc]), true
}

// StringRead is ssa.StringElementFeedback's (jitRegIndexSeen).
func (f *jitRegFeedback) StringRead(pc int) bool {
	rpc, ok := f.regElemPC(pc, ir.ArrayRead)
	return ok && slices.Contains(f.fb.e.stringReads, rpc)
}

// TypedRead is ssa.ElementFeedback's (jitRegIndexSeen).
func (f *jitRegFeedback) TypedRead(pc int) bool {
	rpc, ok := f.regElemPC(pc, ir.ArrayRead)
	return ok && slices.Contains(f.fb.e.typedReads, rpc)
}

// TypedBigRead is ssa.BigElementFeedback's (jitRegIndexSeen).
func (f *jitRegFeedback) TypedBigRead(pc int) (unsigned, ok bool) {
	rpc, ok := f.regElemPC(pc, ir.ArrayRead)
	if !ok {
		return false, false
	}
	m := jitBigReadAt(f.fb.e.typedReads, int(rpc))
	return m&jitBigReadUnsigned != 0, m != 0
}

// KeyedRead is ssa.KeyedFeedback's (jitRegIndexSeen).
func (f *jitRegFeedback) KeyedRead(pc int) bool {
	rpc, ok := f.regElemPC(pc, ir.ArrayRead)
	return ok && slices.Contains(f.fb.e.keyedReads, rpc)
}

// KeyedWrite is ssa.KeyedWriteFeedback's (jitRegElemSeen).
func (f *jitRegFeedback) KeyedWrite(pc int) bool {
	rpc, ok := f.regElemPC(pc, ir.ArrayWrite)
	return ok && slices.Contains(f.fb.e.keyedWrites, rpc)
}

// UpvalueTyped is ssa.UpvalueTypedFeedback's: whether the captured binding
// the read at the program's instruction pc reads (ir.UpvalueRead's Key)
// holds a typed array in the closure the code is compiled for.
func (f *jitRegFeedback) UpvalueTyped(pc int) (uint8, bool) {
	cl := f.fb.cl
	if cl == nil || pc < 0 || pc >= len(f.p.Code) || f.p.Code[pc].Op != ir.UpvalueRead {
		return 0, false
	}
	k := int(f.p.Code[pc].Key)
	if k >= len(cl.upvalues) || cl.upvalues[k] == nil {
		return 0, false
	}
	v := cl.upvalues[k].get()
	if !jitTypedArray(v) {
		return 0, false
	}
	return jitTypedKind(true, v), true
}

// jitRegCtxEntry is the register code a call of Go was made from, with ctx:
// the run's at its own level, or else, at a native callee's level, its
// closure's code; nil where ctx runs no register code.
func (r *Runtime) jitRegCtxEntry(ctx *abi.Context) *jitRegEntry {
	s := r.jit
	idx := int(ctx.Level)
	if idx >= len(s.ssaCtxs) || s.ssaCtxs[idx] != ctx {
		return nil
	}
	if ctx.Live == 0 {
		if idx < len(s.regRuns) && s.regRuns[idx].f != nil {
			return s.regRuns[idx].e
		}
		return nil
	}
	cl := (*closure)(ctx.Closure)
	if cl == nil || cl.reg == nil {
		return nil
	}
	if cc := cl.closureCode; cc != nil {
		if e := cc.jitReg; e != nil && e.cached && e.key.realm == r.jitRealmKey() {
			return e
		}
	}
	return s.regCache[r.jitRegKeyOf(cl)]
}

var (
	_ ssa.ElementFeedback       = (*jitRegFeedback)(nil)
	_ ssa.BigElementFeedback    = (*jitRegFeedback)(nil)
	_ ssa.KeyedFeedback         = (*jitRegFeedback)(nil)
	_ ssa.KeyedWriteFeedback    = (*jitRegFeedback)(nil)
	_ ssa.StringElementFeedback = (*jitRegFeedback)(nil)
	_ ssa.UpvalueTypedFeedback  = (*jitRegFeedback)(nil)
)
