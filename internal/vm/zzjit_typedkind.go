//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"math"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// The element types of the typed arrays an element read or write of a
// number has met, as V8's feedback keeps a keyed access's elements kind,
// are learned as native code runs, which is where most such reads run: the
// existing tiers keep nothing of a site's. Each site has a cell, its
// function's lowering's (jitLowering's typedCells), whose number word has
// bit 1<<k for each element type k it met. Code compiled with the cell
// reads it as it reads a typed array's element the site's code was not
// made for -- of another type, or at a site that met none, or several --
// and calls Go (ssa.GoTypedSeen) to set the type's bit where it has none,
// once for each type: Go marks the code to be compiled again (jitEntry's
// reopt), for the type it met if that is the only one, inline, with no
// other type's case (ssa.TypedKindFeedback).

// jitTypedCellBytes is what a site's cell costs its lowering: the cell
// and its slot in the lowering's table.
const jitTypedCellBytes = int(unsafe.Sizeof(Value{})) + jitMapSlot

// TypedKind is ssa.TypedKindFeedback's: the element types the typed arrays
// the element read or write at pc met, as its cell has them, the cell made
// if the site has none, which the code keeps alive.
func (fb *jitFeedback) TypedKind(pc int) (ssa.TypedSite, bool) {
	s := fb.r.jit
	if s == nil || pc < 0 || pc >= len(fb.fn.Code) {
		return ssa.TypedSite{}, false
	}
	l := s.lowering(fb.fn)
	cell := l.typedCells[int32(pc)]
	if cell == nil {
		if l.typedCells == nil {
			l.typedCells = map[int32]*Value{}
		}
		cell = new(Value)
		l.typedCells[int32(pc)] = cell
		l.bytes += jitTypedCellBytes
		s.lowerBytes += jitTypedCellBytes
	}
	k := fb.keep()
	k.cells = append(k.cells, cell)
	return ssa.TypedSite{Kinds: uint16(math.Float64bits(cell.num)), Cell: uintptr(unsafe.Pointer(cell))}, true
}

// jitTypedSeen is ssa.GoTypedSeen: the typed array in the first operand,
// the site's cell in the second's pointer word. It sets the bit of the
// array's element type in the cell, and marks the code the call was made
// from to be compiled again (jitRunEntry). It refuses only what native
// code never asks, which exits.
func (r *Runtime) jitTypedSeen(ctx *abi.Context) bool {
	o := *(**Object)(unsafe.Pointer(&ctx.GoArgs[0].Ref))
	cell := *(**Value)(unsafe.Pointer(&ctx.GoArgs[1].Ref))
	if o == nil || cell == nil || o.class != ClassTypedArray {
		return false
	}
	t, ok := o.dataOf().(*typedArrayData)
	if !ok || t.kind > abi.ElemBigUint64 {
		return false
	}
	bits := math.Float64bits(cell.num)
	if bit := uint64(1) << t.kind; bits&bit == 0 {
		cell.num = math.Float64frombits(bits | bit)
		if e := r.jitRunEntry(ctx); e != nil && e.ssa != nil {
			e.reopt = true
		}
	}
	return true
}

// jitRunEntry is the entry of the code a call of Go was made from, with
// ctx: the run's, at its own level, which may be a loop's (osrOf), or
// else, at a native callee's level, its function's.
func (r *Runtime) jitRunEntry(ctx *abi.Context) *jitEntry {
	s := r.jit
	if idx := int(ctx.Level); ctx.Live == 0 && idx < len(s.goRuns) && idx < len(s.ssaCtxs) && s.goRuns[idx].f != nil && s.ssaCtxs[idx] == ctx {
		return s.goRuns[idx].e
	}
	if cl := (*closure)(ctx.Closure); cl != nil {
		return s.entryFor(cl.fn)
	}
	return nil
}
