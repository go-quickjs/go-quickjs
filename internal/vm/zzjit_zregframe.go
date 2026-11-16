//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// The calls of Go native code made from register code makes on its frame
// (ssa.GoHost, GoArgument, GoCells, GoClosure, GoClose), on the register
// frame: the run's own, which Go entered the code with (jitState's
// regRuns), or a native callee's, which has none of the VM's, its window
// at its context's Base. Their stack form's (jitGoHost, jitArgument,
// jitMakeCells, jitCellsClosure) read the stack form's frame, its locals
// by local; these read the window, by register.
//
// The cells of code whose captured registers are in cells of their own
// (ir.Program's Cells; the k-th captured register's the k-th cell, as
// ssa's cellIndex counts them) are, as the stack form's: on the run's own
// frame, open on the window's registers -- the upvalues the frame's
// closures share, which the frame closes as it ends -- and for a native
// callee, closed cells holding the registers' values, which Go opens on
// its window if it makes the callee's frame (jitRegReopenCells).

// jitRegFrameRun is the register frame a call of Go on the frame made by
// the code running in ctx is made on: its frame, register code, code and
// window; ok false where ctx runs no register code.
func (r *Runtime) jitRegFrameRun(ctx *abi.Context) (run jitRegRun, level bool, ok bool) {
	s := r.jit
	idx := int(ctx.Level)
	if idx >= len(s.ssaCtxs) || s.ssaCtxs[idx] != ctx {
		return jitRegRun{}, false, false
	}
	switch ctx.Live {
	case 0:
		if idx < len(s.regRuns) && s.regRuns[idx].f != nil {
			return s.regRuns[idx], false, true
		}
	case abi.LiveCall:
		if len(s.regRuns) == 0 || r.jitRegRootOf(idx) < 0 {
			return jitRegRun{}, false, false
		}
		l, found := r.jitRegLevelAt(idx)
		if !found {
			return jitRegRun{}, false, false
		}
		l.e.levels--
		// A stand-in for the callee's frame, which has none of the VM's:
		// its closure, function object and receiver, its window.
		f := &s.calleeFrame
		*f = frame{}
		f.cl, f.base, f.this, f.newTarget = l.cl, l.wb+int(l.c.locals), r.stack[l.wb+1], Undefined
		if v := r.stack[l.wb]; v.IsObject() {
			f.callee = v.Object()
		}
		return jitRegRun{f: f, c: l.c, e: l.e, wb: l.wb}, true, true
	}
	return jitRegRun{}, false, false
}

// jitRegIsFrameOp reports whether ctx, making a call of Go on the frame,
// runs register code: the call is jitRegFrameOp's.
func (r *Runtime) jitRegIsFrameOp(ctx *abi.Context) bool {
	s := r.jit
	if len(s.regRuns) == 0 {
		return false
	}
	idx := int(ctx.Level)
	switch ctx.Live {
	case 0:
		return idx < len(s.regRuns) && s.regRuns[idx].f != nil && idx < len(s.ssaCtxs) && s.ssaCtxs[idx] == ctx
	case abi.LiveCall:
		return r.jitRegRootOf(idx) >= 0
	}
	return false
}

// jitRegFrameOp makes the call of Go on the frame ctx's register code
// makes (jitRegFrameOps), and reports whether it did: what it refuses the
// code leaves to its exit there, where Go runs the register instruction
// on the frame it makes.
func (r *Runtime) jitRegFrameOp(ctx *abi.Context) bool {
	run, level, ok := r.jitRegFrameRun(ctx)
	if !ok || run.e == nil || run.e.p == nil {
		return false
	}
	f, c, wb := run.f, run.c, run.wb
	keep := func(v Value) { *(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = v }
	switch ssa.GoOp(ctx.GoOp & 0xff) {
	case ssa.GoHost:
		rpc := int(ctx.ExitPC)
		if rpc >= len(c.rc.Code) {
			return false
		}
		switch w := regAt(c.code, rpc); w.Op() {
		case bytecode.RNewTarget:
			keep(f.newTarget)
		case bytecode.RRestParam:
			// The frame's arguments past the parameters, which a callee
			// native callers call has none of: its calls keep none.
			if level {
				return false
			}
			var rest []Value
			if i := int(w.B()); i < len(f.args) {
				rest = f.args[i:]
			}
			keep(Obj(r.newArrayFrom(rest)))
		case bytecode.RArguments:
			// An unmapped arguments object, of the arguments the frame
			// kept; a mapped one aliases the registers, which native code
			// may hold elsewhere.
			if level || f.cl.fn.MappedArguments {
				return false
			}
			keep(Obj(r.newArgumentsObject(f)))
		default:
			return false
		}
		return true
	case ssa.GoArgument:
		if level {
			return false
		}
		v, ok := jitArgumentOf(ctx, f.args, f.cl.fn)
		if ok {
			keep(v)
		}
		return ok
	case ssa.GoCells:
		if ctx.GoOp>>abi.GoOpKindShift&0xff == abi.GoClosurePool {
			jitRefillCells(*(**abi.ObjectPool)(unsafe.Pointer(&ctx.GoArgs[0].Ref)))
		}
		return r.jitRegMakeCells(ctx, f, run.e, wb, level)
	case ssa.GoClosure, ssa.GoClose:
		if ctx.GoOp>>abi.GoOpKindShift&0xff == abi.GoClosurePool {
			if pool := *(**abi.ObjectPool)(unsafe.Pointer(&ctx.GoArgs[0].Ref)); pool.Cells != 0 {
				jitRefillCells(pool)
			} else {
				r.jitRefillPool(pool)
			}
		}
		if ctx.Cells == nil {
			// Code whose captured registers are the frame's own slots,
			// which closures' upvalues are open on: the run's own frame's
			// closure_w or close_regs, as the interpreter makes it there.
			if level {
				return false
			}
			return r.jitRegFrameWhole(ctx, f, c, wb)
		}
		return r.jitRegCellsClosure(ctx, f, c, run.e, wb, level, int(ctx.GoOp>>abi.GoOpIndexShift))
	}
	return false
}

// jitRegFrameWhole makes closure_w or close_regs at ctx's register pc on
// f's registers, a window at wb, by the interpreter's handler (regWhole),
// for native code whose captured registers are the frame's slots: a
// closure captures the upvalues open on them, and a scope's end closes
// them, the values staying in the slots native code reads. The closure,
// which the handler writes to its register, goes to the call's keep cell,
// the register put back: native code keeps values in the frame's
// registers as it found them. Neither throws.
func (r *Runtime) jitRegFrameWhole(ctx *abi.Context, f *frame, c *regCode, wb int) bool {
	rpc := int(ctx.ExitPC)
	if rpc >= len(c.rc.Code) {
		return false
	}
	w := regAt(c.code, rpc)
	switch w.Op() {
	case bytecode.RClosureW:
		a := wb + int(w.A())
		saved := r.stack[a]
		if _, _, err := r.regWhole(f, c, wb, rpc); err != nil {
			return false
		}
		keepClosure(ctx, r.stack[a])
		r.stack[a] = saved
		ctx.Open = uint64(len(f.openUpvalues))
		return true
	case bytecode.RCloseRegs:
		if _, _, err := r.regWhole(f, c, wb, rpc); err != nil {
			return false
		}
		ctx.Open = uint64(len(f.openUpvalues))
		return true
	}
	return false
}

// jitRegCapturedRegs are the registers of p closures capture (ir.Program's
// Captured), in the order of their cells.
func jitRegCapturedRegs(p []bool) []int {
	var regs []int
	for reg, c := range p {
		if c {
			regs = append(regs, reg)
		}
	}
	return regs
}

// jitRegMakeCells makes, at an entry of e's code, whose captured registers
// are in cells of their own, those cells (abi.Context's Cells): on the
// run's own frame f, open on the window's registers at wb, those its
// closures opened before among them; for a native callee's, level, closed,
// each holding its register's value.
func (r *Runtime) jitRegMakeCells(ctx *abi.Context, f *frame, e *jitRegEntry, wb int, level bool) bool {
	regs := jitRegCapturedRegs(e.p.Captured)
	if len(regs) == 0 || wb+e.p.Regs > len(r.stack) {
		return false
	}
	cells, room := jitNewCells(len(regs))
	for k, reg := range regs {
		slot := &r.stack[wb+reg]
		if level {
			u := &room[k]
			u.closed = *slot
			u.slot = &u.closed
			cells[k] = u
			continue
		}
		var u *upvalue
		for _, o := range f.openUpvalues {
			if o.slot == slot {
				u = o
				break
			}
		}
		if u == nil {
			u = &room[k]
			u.slot = slot
			f.openUpvalues = append(f.openUpvalues, u)
		}
		cells[k] = u
	}
	ctx.Cells = unsafe.Pointer(unsafe.SliceData(cells))
	if !level {
		ctx.Open = uint64(len(f.openUpvalues))
	}
	return true
}

// jitRegCellsClosure is GoClosure and GoClose for register code whose
// captured registers are in cells of their own, as jitCellsClosure is the
// stack form's: closure_w's closure of constant i, capturing the cells, as
// regClosure makes it; or close_regs from register i on, whose captured
// registers take fresh cells holding what they hold -- open on the
// window's registers on the run's own frame, the old ones closed.
func (r *Runtime) jitRegCellsClosure(ctx *abi.Context, f *frame, c *regCode, e *jitRegEntry, wb int, level bool, i int) bool {
	regs := jitRegCapturedRegs(e.p.Captured)
	cells := unsafe.Slice((**upvalue)(ctx.Cells), len(regs))
	if ssa.GoOp(ctx.GoOp&0xff) == ssa.GoClose {
		if !level && len(f.openUpvalues) != 0 {
			regCloseRegs(f, regWindow(r.stack, wb), i, e.p.Regs-i)
		}
		for k, reg := range regs {
			if reg < i {
				continue
			}
			v := *cells[k].slot
			if level {
				u := &upvalue{closed: v}
				u.slot = &u.closed
				cells[k] = u
				continue
			}
			r.stack[wb+reg] = v
			u := &upvalue{slot: &r.stack[wb+reg]}
			f.openUpvalues = append(f.openUpvalues, u)
			cells[k] = u
		}
		if !level {
			ctx.Open = uint64(len(f.openUpvalues))
		}
		return true
	}
	rpc := int(ctx.ExitPC)
	if rpc >= len(c.rc.Code) || i >= len(f.cl.consts) || i >= len(c.rc.Caps) {
		return false
	}
	tmpl := f.cl.consts[i]
	t := tmpl.template()
	if t == nil {
		return false
	}
	// What an arrow captures of this is the window's register 1, as
	// regClosure has it.
	f.this = r.stack[wb+1]
	caps := c.rc.Caps[i]
	if t.fn.CapturesByValue {
		if level {
			// Its by-value captures read the window's registers, which a
			// native callee's cells hold: left to the exit.
			return false
		}
		keepClosure(ctx, Obj(regMadeClosure(newClosureByValue[struct{}](r, f, t, caps, wb))))
		return true
	}
	byLocal := make([]*upvalue, max(int(c.ncap), 1))
	for k, u := range t.fn.Upvalues {
		if !u.FromParent || k >= len(caps) {
			continue
		}
		at := -1
		for j, reg := range regs {
			if reg == int(caps[k]) {
				at = j
				break
			}
		}
		n := cellOfLocal(f.cl.fn, int(u.Index))
		if at < 0 || n >= len(byLocal) {
			return false
		}
		byLocal[n] = cells[at]
	}
	keepClosure(ctx, Obj(regMadeClosure(r.newClosureIn(f, tmpl, byLocal))))
	if !level {
		ctx.Open = uint64(len(f.openUpvalues))
	}
	return true
}

// keepClosure writes v, a closure made for native code, to the call's keep
// cell.
func keepClosure(ctx *abi.Context, v Value) {
	*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = v
}

// jitRegReopenCells gives f, the frame of a register function whose
// captured registers are in cells of their own (abi.Context's Cells), its
// window at wb, those cells, as jitReopenCells gives the stack form's: each
// register's value in the window, its cell open on it, the frame's to close
// as it ends; and, the run's own frame's (run), the upvalues it kept open
// that are no longer on its window's registers -- cells native code closed
// at a scope's end -- the frame's no more.
func (r *Runtime) jitRegReopenCells(f *frame, e *jitRegEntry, wb int, ctx *abi.Context, run bool) {
	if ctx.Cells == nil || e == nil || e.p == nil || wb+e.p.Regs > len(r.stack) {
		return
	}
	lo, hi := uintptr(unsafe.Pointer(&r.stack[wb])), uintptr(unsafe.Pointer(&r.stack[wb+e.p.Regs-1]))
	if run {
		kept := f.openUpvalues[:0]
		for _, u := range f.openUpvalues {
			if p := uintptr(unsafe.Pointer(u.slot)); p >= lo && p <= hi {
				kept = append(kept, u)
			}
		}
		clear(f.openUpvalues[len(kept):])
		f.openUpvalues = kept
	}
	regs := jitRegCapturedRegs(e.p.Captured)
	cells := unsafe.Slice((**upvalue)(ctx.Cells), len(regs))
	for k, reg := range regs {
		slot := &r.stack[wb+reg]
		if u := cells[k]; u.slot != slot {
			*slot = *u.slot
			u.slot = slot
			f.openUpvalues = append(f.openUpvalues, u)
		}
	}
	ctx.Open = uint64(len(f.openUpvalues))
}

// jitArgumentOf is GoArgument's answer of args, the arguments of a frame
// of fn: their count, or the argument the call's key names, where no
// arguments object is made and no parameter a mapped one would alias is in
// play -- or false.
func jitArgumentOf(ctx *abi.Context, args []Value, fn *bytecode.Function) (Value, bool) {
	if ctx.GoOp>>abi.GoOpArgcShift&0xff == 1 {
		if jitGoArg(ctx, 0).IsObject() {
			return Undefined, false
		}
		return Int(len(args)), true
	}
	k := jitGoArg(ctx, 0)
	if jitGoArg(ctx, 1).IsObject() || !k.IsNumber() {
		return Undefined, false
	}
	if i := uint32(k.num); float64(i) == k.num && uint(i) < uint(len(args)) {
		if n := int(ctx.GoOp>>abi.GoOpArgcShift&0xff) - 2; mapsArgument(fn, int(i)) {
			if uint(i) < uint(n) {
				return jitGoArg(ctx, 2+int(i)), true
			}
			return Undefined, false
		}
		return args[i], true
	}
	return Undefined, false
}
