//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"math/bits"
	"slices"
	"sync/atomic"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// regStringAddOf records a string addition at register pc of c for the
// first compile (regCode's stringAdds). Only the slow paths' marking forms
// call it (addMark, stringMark, addImmMark, addPropMark), which they call
// where the JIT runs.
func regStringAddOf(c *regCode, pc int) {
	marks := c.stringAdds.Load()
	if marks == nil {
		bits := make([]atomic.Uint32, (len(c.rc.Code)+31)/32)
		marks = &bits
		if !c.stringAdds.CompareAndSwap(nil, marks) {
			marks = c.stringAdds.Load()
		}
	}
	word, bit := &(*marks)[pc>>5], uint32(1)<<(pc&31)
	if word.Load()&bit == 0 {
		word.Or(bit)
	}
}

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
	key, l, h := r.jitRegLowering(cl, rc)
	if l == nil {
		return jitRegLowerWith(rc, cl.fn, kind, h)
	}
	if !l.done[kind] {
		s := r.jit
		// Lowered ahead by a worker, if it was (jit_lowerahead.go): the same
		// programs.
		got, ok := s.regTakeAhead(jitAheadKey{key, jitAheadKind(kind)}, rc, &h)
		if !ok {
			got = jitRegLowerKinds(rc, cl.fn, kind, h)
		}
		s.regKeepLowered(l, &got)
	}
	return l.progs[kind], l.errs[kind]
}

// regKeepLowered keeps in l, a function's lowering (regLowering), what got
// lowered of the kinds l has none of, made of the function's register code
// with l's hints, and counts it in what the JIT holds once for each program
// (jitLowering's footprint): a lowering made on the runtime's goroutine
// (jitRegLower), taken of the workers' ahead (regTakeAhead), or made on a
// worker for a snapshot's derivation and kept at the install
// (regKeepLoweredAt). It reports whether it kept any.
func (s *jitState) regKeepLowered(l *jitLowering, got *jitLowered) bool {
	n := l.footprint()
	kept := false
	for k := range got.done {
		if got.done[k] && !l.done[k] {
			l.progs[k], l.errs[k], l.done[k] = got.progs[k], got.errs[k], true
			kept = true
		}
	}
	n = l.footprint() - n
	l.bytes += n
	s.lowerBytes += n
	return kept
}

// regKeepLoweredAt is regKeepLowered for cl's function, its register code
// rc, of got lowered on a worker with hints h (jitRegHintsNow's, copied as
// they were) at the install of the compile that lowered it there: kept
// where the hints taken now (jitRegLowering) are h still, the kinds the
// function's lowering has none of -- one lowered meanwhile on the
// runtime's goroutine stays -- and nothing kept where they have grown
// since, which the next ask lowers anew.
func (r *Runtime) regKeepLoweredAt(cl *closure, rc *bytecode.RegCode, h *jitcompile.Hints, got *jitLowered) bool {
	_, l, now := r.jitRegLowering(cl, rc)
	if l == nil || !jitHintsSame(&now, h) {
		return false
	}
	return r.jit.regKeepLowered(l, got)
}

// jitRegHintsCopy is the hints cl's function's register code rc would be
// lowered with now -- what jitRegLowering would take, KeepParams among
// them, taken by the same code (jitHintsTake) -- in a copy of their own,
// the function's key, and its lowering's programs (cached) where they were
// lowered with those hints, nil where not: nothing of the runtime's
// written -- no state made for the function, nothing lowered again or
// anew -- for a snapshot's gather to hand a worker.
func (r *Runtime) jitRegHintsCopy(cl *closure, rc *bytecode.RegCode) (key jitKey, h jitcompile.Hints, cached *jitLowered) {
	h.KeepParams = r.nodeQuirks && !cl.fn.Strict
	s := r.jit
	if s == nil || rc == nil || rc != cl.fn.RegCompiled() {
		return jitKey{}, h, nil
	}
	key = r.jitRegKeyOf(cl)
	if e := s.regLearned[key]; e != nil {
		x := &e.regHints
		h.StringAdds, h.ElementReads = slices.Clone(x.StringAdds), slices.Clone(x.ElementReads)
		h.StringReads, h.WholeOps = slices.Clone(x.StringReads), slices.Clone(x.WholeOps)
	}
	jitHintsTake(&h, cl, rc)
	if l := s.regLowered[key]; l != nil {
		lh := l.hints
		lh.KeepParams = h.KeepParams
		if jitHintsSame(&lh, &h) {
			cached = &jitLowered{done: l.done, progs: l.progs, errs: l.errs}
		}
	}
	return key, h, cached
}

// jitHintsTake takes into h what the interpreter has met since of cl's
// function's register code rc: the additions that met a string, its marks
// (regStringAddOf), and what its operand lattice met (jitLatticeHints),
// each pc appended once, in order: into what the function learned
// (jitRegLowering), or into a copy (jitRegHintsCopy).
func jitHintsTake(h *jitcompile.Hints, cl *closure, rc *bytecode.RegCode) {
	if cl.reg == nil || cl.reg.rc != rc {
		return
	}
	if marks := cl.reg.stringAdds.Load(); marks != nil {
		for i := range *marks {
			for word := (*marks)[i].Load(); word != 0; word &= word - 1 {
				pc := int32(i*32 + bits.TrailingZeros32(word))
				if !slices.Contains(h.StringAdds, pc) {
					h.StringAdds = append(h.StringAdds, pc)
				}
			}
		}
	}
	if cl.jitRegOps != nil {
		// What the operand lattice met, taken as the marks are: an
		// addition that met a string Go's, an operator that met an object
		// run whole, from the first lowering on.
		jitLatticeHints(h, rc, cl.jitRegOps)
	}
}

// jitHintsTaken reports whether jitHintsTake has anything of cl's to take
// for its register code rc: marks or a lattice.
func jitHintsTaken(cl *closure, rc *bytecode.RegCode) bool {
	return cl.reg != nil && cl.reg.rc == rc && (cl.reg.stringAdds.Load() != nil || cl.jitRegOps != nil)
}

// jitRegLowering is the register lowering of cl's function, its key key,
// for its register code rc (regLowering), with the hints its code has
// learned taken into it, and those hints, which its programs are lowered
// with; or, before the runtime has any state, which no compile is, or for
// code not the function's own register code, which no key names, no
// lowering, and the hints such code is lowered with.
func (r *Runtime) jitRegLowering(cl *closure, rc *bytecode.RegCode) (jitKey, *jitLowering, jitcompile.Hints) {
	s := r.jit
	if s == nil || rc == nil || rc != cl.fn.RegCompiled() {
		return jitKey{}, nil, jitcompile.Hints{KeepParams: r.nodeQuirks && !cl.fn.Strict}
	}
	key := r.jitRegKeyOf(cl)
	if s.hintsDepth != 0 {
		// Taken once a compile (jitState's hintsTaken): nothing runs
		// between its asks that marks the code.
		if slices.Contains(s.hintsTaken, cl) {
			return r.jitRegLowered(key, cl)
		}
		s.hintsTaken = append(s.hintsTaken, cl)
	}
	if jitHintsTaken(cl, rc) {
		// Take interpreter feedback before asking for a cached lowering:
		// new hints invalidate it, including an inlined callee's.
		jitHintsTake(&s.regLearnedOf(key).regHints, cl, rc)
	}
	return r.jitRegLowered(key, cl)
}

// jitRegLowered is jitRegLowering of the function whose key is key, cl's,
// its hints taken already.
func (r *Runtime) jitRegLowered(key jitKey, cl *closure) (jitKey, *jitLowering, jitcompile.Hints) {
	l := r.jit.regLowering(key)
	h := l.hints
	h.KeepParams = r.nodeQuirks && !cl.fn.Strict
	return key, l, h
}

// jitHintsOnce has the hints of each function a compile asks the lowering
// of taken at its first ask alone (jitRegLowering), until the function it
// returns is called, as the compile ends. Nothing runs between a compile's
// asks that marks the code -- the interpreter's marks and its operand
// lattice, which the hints are taken from, are written as the code runs --
// so the asks after the first, each a scan of both, the register code's
// length, would take nothing: the root's twice a compile in the
// background, at its ask (jitRegLowerAhead) and at its lowering, and a
// callee's at each of its calls the snapshot asks to inline.
func (s *jitState) jitHintsOnce() func() {
	s.hintsDepth++
	return func() {
		if s.hintsDepth--; s.hintsDepth == 0 {
			clear(s.hintsTaken)
			s.hintsTaken = s.hintsTaken[:0]
		}
	}
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
		len(l.hints.StringReads) != len(h.StringReads) || len(l.hints.WholeOps) != len(h.WholeOps) {
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

// jitLowered is a lowering's programs and their errors by kind, done the
// kinds it made.
type jitLowered struct {
	done  [jitLowerKinds]bool
	progs [jitLowerKinds]*ir.Program
	errs  [jitLowerKinds]error
}

// jitLowerShares reports whether a program of kind is lowered with those
// of the other kinds that share it (jitRegLowerKinds): an entry's, a hot
// function's and a callee's, which differ only in what they refuse
// (jitcompile.LowerRegKindsWith), but not a forwarding callee's.
func jitLowerShares(kind jitLowerKind) bool { return kind != jitLowerForward }

// jitRegLowerKinds is fn's register code rc lowered as kind says, with
// hints, and checked, as jitRegLowerWith lowers it, and, where kind shares
// its program (jitLowerShares), lowered so for the other kinds that do,
// at once: one program for those that take it, each the one it would have
// been lowered to alone, checked once. A function compiled and inlined had
// been lowered twice, about half of the lowerings Octane's Typescript made
// in the background.
func jitRegLowerKinds(rc *bytecode.RegCode, fn *bytecode.Function, kind jitLowerKind, h jitcompile.Hints) (out jitLowered) {
	if !jitLowerShares(kind) {
		p, err := jitRegLowerWith(rc, fn, kind, h)
		out.done[kind], out.progs[kind], out.errs[kind] = true, p, err
		return out
	}
	var p *ir.Program
	var errs [3]error
	if rc.Typed {
		// Typed code's, a twin's (asmTwin): one lowering for every kind.
		p, errs[0] = jitTypedLower(rc, fn)
		errs[1], errs[2] = errs[0], errs[0]
	} else {
		p, errs[0], errs[1], errs[2] = jitcompile.LowerRegKindsWith(rc, fn, h)
		if p != nil {
			// Checked once, here, as jitRegLowerWith checks it.
			err := p.Validate()
			p.Checked = err == nil
			for i := range errs {
				if errs[i] == nil {
					errs[i] = err
				}
			}
		}
	}
	for i, k := range [3]jitLowerKind{jitLowerSSA, jitLowerHot, jitLowerInline} {
		out.done[k], out.errs[k] = true, errs[i]
		if errs[i] == nil {
			out.progs[k] = p
		}
	}
	return out
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
	case bytecode.RAddProp:
		// o.name += x's addition, of a string x.
		if !str(w.B()) {
			return false
		}
	default:
		return false
	}
	l.regHints.StringAdds = append(l.regHints.StringAdds, int32(rpc))
	return true
}

// jitRegObjectArithSeen learns, at a deoptimization or a host exit of
// register code at register pc rpc of c, whose frame's window at wb holds
// the state at the instruction's start, an arithmetic operator that met an
// object -- add's, sub's, mul's, their immediates', the other binary
// operators', a local's append -- whose conversion may run script: every lowering from here
// has Go run it whole on the register frame (Hints' WholeOps), native code
// going on after it. It reports whether it learned one.
func (r *Runtime) jitRegObjectArithSeen(l *jitEntry, c *regCode, wb, rpc int) bool {
	if rpc < 0 || rpc >= len(c.rc.Code) || slices.Contains(l.regHints.WholeOps, int32(rpc)) {
		return false
	}
	w := c.rc.Code[rpc]
	object := func(n uint16) bool { return wb+int(n) < len(r.stack) && r.stack[wb+int(n)].IsObject() }
	switch w.Op() {
	case bytecode.RAdd, bytecode.RSub, bytecode.RMul, bytecode.RBin,
		bytecode.RBitAnd, bytecode.RBitOr, bytecode.RBitXor, bytecode.RShl, bytecode.RShr, bytecode.RUShr:
		if !object(w.B()) && !object(w.C()) {
			return false
		}
	case bytecode.RAddI, bytecode.RSubI, bytecode.RMulI,
		bytecode.RAndI, bytecode.ROrI, bytecode.RXorI, bytecode.RShlI, bytecode.RShrI, bytecode.RUShrI:
		if !object(w.B()) {
			return false
		}
	case bytecode.RAppend:
		// A local's s += x, a string's addition from the start.
		if !object(w.A()) && !object(w.B()) {
			return false
		}
	default:
		return false
	}
	l.regHints.WholeOps = append(l.regHints.WholeOps, int32(rpc))
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
	d := f.native()
	return d.elemIn(pc, ir.ArrayRead, d.stringReads)
}

// TypedRead is ssa.ElementFeedback's (jitRegIndexSeen).
func (f *jitRegFeedback) TypedRead(pc int) bool {
	d := f.native()
	return d.elemIn(pc, ir.ArrayRead, d.typedReads)
}

// TypedBigRead is ssa.BigElementFeedback's (jitRegIndexSeen).
func (f *jitRegFeedback) TypedBigRead(pc int) (unsigned, ok bool) {
	return f.native().typedBigRead(pc)
}

// KeyedRead is ssa.KeyedFeedback's (jitRegIndexSeen).
func (f *jitRegFeedback) KeyedRead(pc int) bool {
	d := f.native()
	return d.elemIn(pc, ir.ArrayRead, d.keyedReads)
}

// KeyedWrite is ssa.KeyedWriteFeedback's (jitRegElemSeen).
func (f *jitRegFeedback) KeyedWrite(pc int) bool {
	d := f.native()
	return d.elemIn(pc, ir.ArrayWrite, d.keyedWrites)
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

// KeyedStore is ssa.KeyedStoreFeedback's: the site of set_index's write
// (ir.ArrayWrite) or define_index's define (its whole host) at the
// program's instruction pc, as its keyed store cache (reg_table_keyed.go)
// holds it -- its entry and its poly's -- grouped by key: for each, the
// slots objects of its shapes have (an assignment's only: a define's
// entry for one is left to Go) and the additions it made, as a named
// write's site has them (siteOf, add), a define's marked so. A site that
// gave up, or holds nothing native code is compiled for, has none.
func (f *jitRegFeedback) KeyedStore(pc int) (ssa.KeyedStoreSite, bool) {
	fb := f.fb
	w, op, ok := f.at(pc)
	if !ok || fb.cl == nil {
		return ssa.KeyedStoreSite{}, false
	}
	define := w.Op() == bytecode.RDefineIndex
	if !(w.Op() == bytecode.RSetIndex && op == ir.ArrayWrite || define && op == ir.Host) {
		return ssa.KeyedStoreSite{}, false
	}
	rpc := int(f.p.Origin[pc])
	if rpc >= len(f.rc.Aux) {
		return ssa.KeyedStoreSite{}, false
	}
	s := int(f.rc.Aux[rpc])
	if s == 0 || s >= len(fb.cl.ic) {
		return ssa.KeyedStoreSite{}, false
	}
	c := &fb.cl.ic[s]
	if c.fills >= maxCacheFills {
		return ssa.KeyedStoreSite{}, false
	}
	site := ssa.KeyedStoreSite{Define: define}
	keyOf := func(k Atom) *ssa.PropertySite {
		for i := range site.Keys {
			if site.Keys[i].Key == uint32(k) {
				return &site.Keys[i]
			}
		}
		site.Keys = append(site.Keys, ssa.PropertySite{Key: uint32(k)})
		return &site.Keys[len(site.Keys)-1]
	}
	note := func(e *propCache) {
		sh := e.shape
		if sh == nil || sh == noShape || sh == dictShape || e.getter || e.next == setterNext {
			return
		}
		if e.next != nil {
			add := fb.add(e, jitPropLive{r: fb.r})
			if add == nil {
				return
			}
			k := e.next.key
			if define {
				add.Define, add.Key = true, uint32(k)
			}
			ks := keyOf(k)
			if ks.Add == nil {
				ks.Add = add
			} else {
				ks.Adds = append(ks.Adds, add)
			}
			return
		}
		if define || e.cell != nil || e.p1 != nil || e.idx < 0 || e.idx >= sh.n || sh.loose {
			return
		}
		k := sh.keys[e.idx].key
		fb.keep().shapes = append(fb.keep().shapes, remember(sh))
		ks := keyOf(k)
		if ks.Shape == 0 {
			ks.Shape, ks.Index = uintptr(unsafe.Pointer(sh)), e.idx
		} else {
			ks.Cases = append(ks.Cases, ssa.PropertyCase{Shape: uintptr(unsafe.Pointer(sh)), Index: e.idx})
		}
	}
	note(c)
	if p := c.poly; p != nil {
		for i := range p.n {
			note(&p.entries[i])
		}
	}
	return site, len(site.Keys) != 0
}

// addMark, stringMark, addImmMark and addPropMark are the slow paths'
// additions' marking forms (reg_runs.go's stringSeen), which they call
// where the JIT runs: here, with the JIT's files, so that none lies among
// the interpreter's code in a build with the JIT. addMark is a + b,
// marked: a string's result, and, if big, a BigInt's (regBigOf), add's;
// add_i32's marks no BigInt, which its | 0 throws for.
//
//go:noinline
func (st *regState) addMark(pc int, a, b Value, big bool) (Value, error) {
	v, err := st.r.add(a, b)
	if err == nil {
		if v.IsString() {
			regStringAddOf(st.c, pc)
		} else if big && v.IsBigInt() {
			st.r.regBigOf(st.c, pc, a, b, v)
		}
	}
	return v, err
}

//go:noinline
func (st *regState) stringMark(pc int, v Value) {
	if v.IsString() {
		regStringAddOf(st.c, pc)
	}
}

//go:noinline
func (st *regState) addImmMark(pc int, op bytecode.Op, a Value, imm int32) (Value, error) {
	v, err := st.r.regBinImm(op, a, imm)
	if err == nil && v.IsString() {
		regStringAddOf(st.c, pc)
	}
	return v, err
}

//go:noinline
func (st *regState) addPropMark(pc int, w bytecode.RInstr, obj, b Value) error {
	if w.Op() == bytecode.RAddProp && b.IsString() {
		// o.name += x of a string x: a string's addition.
		regStringAddOf(st.c, pc)
	}
	return st.r.regAddProp(st.cl, st.c, w, obj, b)
}
