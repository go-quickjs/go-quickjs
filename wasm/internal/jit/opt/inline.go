package opt

import (
	wjit "github.com/go-quickjs/go-quickjs/wasm/internal/jit"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// Inlining. A direct call of a small function of numbers is built into its
// caller: the builder reads the callee's body where the call is, its
// parameters the call's arguments, its locals variables of their own, its
// body a block whose label is where it returns, the results that label's
// phis. What the callee's instructions report -- a trap's offset, an
// exit's, a call's return stack entry -- is the offset the engine gives the
// frames the calls would have made (wjit.Request.InlineSite), so that a
// trap's frames and the call depth are those of the calls.

// The limits of inlining: a callee's body -- its locals' declarations and
// its instructions -- of inlineBodyBytes at most, inlineDepth calls deep
// at most, and inlineBudget bytes of bodies in a function.
const (
	inlineBodyBytes = 64
	inlineDepth     = 3
	inlineBudget    = 1024
)

// inlFrame is a call being inlined: the function called, the offset of the
// call in the function it is in, the frame it is in (nil for the function
// compiled's), how many calls deep it is (1 for a call the function
// compiled makes), where its locals' variables begin, and the construct of
// its body.
type inlFrame struct {
	parent    *inlFrame
	fn, call  uint32
	level     int32
	locals    int32
	body      int
	maxHeight int
}

// inlSite is an instruction of an inlined body that may report its offset:
// its frame, and its offset in the callee.
type inlSite struct {
	fr *inlFrame
	at uint32
}

// inliner is the builder's state of inlining.
type inliner struct {
	// on says the compile may inline, to depth calls deep at most;
	// budget is the bytes of bodies it may still inline, used says it
	// inlined a call, and maxLevel how deep the deepest is.
	on       bool
	depth    int32
	budget   int
	used     bool
	maxLevel int32
	// cur is the call whose body is being built, nil in the function's
	// own.
	cur *inlFrame
	// base is the offset of the first site: the instructions of inlined
	// bodies have base plus their index in sites as their offset until
	// resolveInline gives them the engine's.
	base   uint32
	frames chunks[inlFrame]
	sites  []inlSite
	// verdicts is each function's: 0 not yet asked, 1 inlinable, 2 not.
	verdicts []uint8
	// resolved and chain are resolveInline's.
	resolved []uint32
	chain    []wjit.InlineFrame
}

// reset empties the inliner for the next function, keeping its memory.
func (in *inliner) reset() {
	in.frames.reset()
	clear(in.sites[:cap(in.sites)])
	*in = inliner{frames: in.frames, sites: in.sites[:0], verdicts: in.verdicts[:0], resolved: in.resolved[:0],
		chain: in.chain[:0]}
}

// level is how many calls deep the body being built is.
func (in *inliner) level() int32 {
	if in.cur == nil {
		return 0
	}
	return in.cur.level
}

// at is the offset an instruction at offset pc of the body being built
// reports: pc itself in the function's own body, else a site's.
func (bd *builder) at(pc uint32) uint32 {
	in := &bd.inl
	if in.cur == nil {
		return pc
	}
	in.sites = append(in.sites, inlSite{in.cur, pc})
	return in.base + uint32(len(in.sites)-1)
}

// inlines reports whether a call of function idx is to be inlined where
// the builder is.
func (bd *builder) inlines(idx uint32) bool {
	in := &bd.inl
	if !in.on || in.level() >= in.depth {
		return false
	}
	fn := &bd.m.Functions[idx]
	if fn.Imported || int(fn.Code.Len) > inlineBodyBytes || int(fn.Code.Len) > in.budget {
		return false
	}
	// The offsets of its instructions must stay below 2^31, as the code's
	// immediates hold them.
	if uint64(in.base)+uint64(len(in.sites))+uint64(fn.Code.Len) >= 1<<31-1 {
		return false
	}
	// Nothing may catch what the call throws: Go finds a handler by the
	// offset of a call in its function, which an inlined body has none of.
	for _, t := range bd.ctrl {
		if t.try != nil && !t.try.inCatch {
			return false
		}
	}
	if len(in.verdicts) == 0 {
		in.verdicts = grown(in.verdicts, len(bd.m.Functions))
		clear(in.verdicts)
	}
	if in.verdicts[idx] == 0 {
		in.verdicts[idx] = 2
		if inlinable(bd.m, idx) {
			in.verdicts[idx] = 1
		}
	}
	return in.verdicts[idx] == 1
}

// numeric reports whether t is a number the inliner takes: i32, i64, f32 or
// f64.
func numericType(t module.ValueType) bool {
	switch t.Kind {
	case module.KindI32, module.KindI64, module.KindF32, module.KindF64:
		return true
	}
	return false
}

func numericTypes(ts []module.ValueType) bool {
	for _, t := range ts {
		if !numericType(t) {
			return false
		}
	}
	return true
}

// inlinable reports whether function idx's body may be inlined: its
// parameters, results and locals numbers, and its instructions those of
// control, locals, globals of numbers, numbers, memory accesses,
// memory.size and memory.grow, and direct calls of functions of numbers --
// nothing that holds a reference, throws, catches, makes a tail call or
// has Go run it by its offset.
func inlinable(m *module.Module, idx uint32) bool {
	ft := m.FuncType(idx)
	if !numericTypes(ft.Params) || !numericTypes(ft.Results) {
		return false
	}
	code := m.Functions[idx].Code
	r := reader{b: m.Bytes, pc: int(code.Offset)}
	for n := r.u32(); n > 0; n-- {
		r.u32()
		if !numericType(r.valueType()) {
			return false
		}
	}
	end := int(code.End())
	for r.pc < end {
		op := r.opcode()
		switch op {
		case opcode.Block, opcode.Loop, opcode.If:
			params, results := r.blockType(m)
			if !numericTypes(params) || !numericTypes(results) {
				return false
			}
			continue
		case opcode.CallFunction:
			callee := m.FuncType(r.u32())
			if !numericTypes(callee.Params) || !numericTypes(callee.Results) {
				return false
			}
			continue
		case opcode.GlobalGet, opcode.GlobalSet:
			if !numericType(m.Globals[r.u32()].Type) {
				return false
			}
			continue
		case opcode.SelectWithType:
			for n := r.u32(); n > 0; n-- {
				if !numericType(r.valueType()) {
					return false
				}
			}
			continue
		case opcode.Unreachable, opcode.Nop, opcode.Else, opcode.End, opcode.Br, opcode.BrIf, opcode.BrTable,
			opcode.Return, opcode.Drop, opcode.Select, opcode.LocalGet, opcode.LocalSet, opcode.LocalTee,
			opcode.I32Const, opcode.I64Const, opcode.F32Const, opcode.F64Const, opcode.MemorySize,
			opcode.MemoryGrow:
		default:
			switch {
			case op >= opcode.I32LoadMem && op <= opcode.I64StoreMem32:
			case op < 256 && numerics1[op].arity != 0:
			case op >= 256 && numerics[op].arity != 0:
			default:
				return false
			}
		}
		r.skipImmediates(op)
	}
	return true
}

// inlineCall builds the call of function idx at offset pc, which reports
// at, into the function: a trap where the call would pass the call depth's
// limit, then the callee's body on the call's arguments.
func (bd *builder) inlineCall(idx, pc uint32) {
	m, in := bd.m, &bd.inl
	ft := m.FuncType(idx)
	code := m.Functions[idx].Code
	in.used = true
	in.budget -= int(code.Len)
	fr := in.frames.new()
	*fr = inlFrame{parent: in.cur, fn: idx, call: pc, level: in.level() + 1, locals: int32(len(bd.locals)),
		maxHeight: bd.f.MaxHeight}
	in.cur = fr
	in.maxLevel = max(in.maxLevel, fr.level)
	d := bd.value(OpDepth, TNone)
	d.Aux = uint64(fr.level)
	// The call that cannot be made traps at its callee's start.
	d.At = bd.at(code.Offset)

	n := len(ft.Params)
	args := bd.stack[len(bd.stack)-n:]
	for i, t := range ft.Params {
		bd.locals = append(bd.locals, typeOf(t, "parameter"))
		bd.writeVar(fr.locals+int32(i), bd.b, args[i])
	}
	bd.stack = bd.stack[:len(bd.stack)-n]
	saved := bd.r
	bd.r = reader{b: m.Bytes, pc: int(code.Offset)}
	for c := bd.r.u32(); c > 0; c-- {
		count := bd.r.u32()
		t := typeOf(bd.r.valueType(), "local")
		z := bd.zero[t]
		if z == nil {
			z = bd.f.newValue(bd.f.Entry, OpConst, t)
			bd.zero[t] = z
		}
		for range count {
			k := int32(len(bd.locals))
			bd.locals = append(bd.locals, t)
			bd.writeVar(k, bd.b, z)
		}
	}
	body := bd.enter(opcode.Block, nil, bd.typesOf(ft.Results, "result"), bd.f.newBlock())
	body.inl = true
	fr.body = len(bd.ctrl) - 1
	for len(bd.ctrl) > fr.body {
		bd.instr()
	}
	bd.r = saved
	in.cur = fr.parent
	// The callee's operands are numbers, in no reference slot.
	bd.f.MaxHeight = fr.maxHeight
	if bd.b == nil {
		// The callee never returns.
		bd.markDead()
	}
}

// bodyCtl is the index among the constructs of the body being built's.
func (bd *builder) bodyCtl() int {
	if c := bd.inl.cur; c != nil {
		return c.body
	}
	return 0
}

// resolveInline gives each instruction of an inlined body the offset the
// engine reports its frames by, through site, reporting false where site
// has none.
func (bd *builder) resolveInline(f *Func, site func([]wjit.InlineFrame, uint32) (uint32, bool)) bool {
	in := &bd.inl
	if !in.used {
		return true
	}
	in.resolved = grown(in.resolved, len(in.sites))
	clear(in.resolved)
	ok := true
	get := func(at uint32) uint32 {
		i := at - in.base
		if r := in.resolved[i]; r != 0 {
			return r
		}
		s := in.sites[i]
		ch := in.chain[:0]
		for fr := s.fr; fr != nil; fr = fr.parent {
			ch = append(ch, wjit.InlineFrame{Fn: fr.fn, Call: fr.call})
		}
		for i, j := 0, len(ch)-1; i < j; i, j = i+1, j-1 {
			ch[i], ch[j] = ch[j], ch[i]
		}
		in.chain = ch
		r, good := site(ch, s.at)
		if !good || r >= 1<<31 {
			ok = false
			return at
		}
		in.resolved[i] = r
		return r
	}
	for _, b := range f.Blocks {
		if b.Kind == BTrap && b.At >= in.base {
			b.At = get(b.At)
		}
		for _, v := range b.Values {
			if v.At >= in.base {
				v.At = get(v.At)
			}
		}
	}
	return ok
}

// localBase is the variable of local 0 of the body being built.
func (bd *builder) localBase() int32 {
	if c := bd.inl.cur; c != nil {
		return c.locals
	}
	return 0
}
