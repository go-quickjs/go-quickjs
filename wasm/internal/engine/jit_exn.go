//go:build quickjs_jit && !android && !ios && ((amd64 && (linux || windows)) || (arm64 && (darwin || linux)))

package engine

import (
	"math"

	ijit "github.com/go-quickjs/go-quickjs/internal/jit"
	wjit "github.com/go-quickjs/go-quickjs/wasm/internal/jit"
)

// Exceptions in native code. throw and throw_ref exit, and Go throws the
// exception as the interpreter does, a panic; so does what Go calls for
// native code. Where the level's code has catch clauses, an exit's work
// runs under a recover that takes an exception to the innermost native
// frame of the level with a try_table around the instruction it is at --
// the exit's, or the call its callee was made by -- and resumes that
// frame at the clause's landing pad, the frames above it dropped. An
// exception no frame of the level catches goes on out of it.

// exitAt returns the offset of the instruction whose exit Go is handling,
// and whether its frame is gone: a tail call's, whose callee's exceptions
// its try_tables do not take.
func exitAt(ctx *wjit.Ctx) (at uint32, gone bool) {
	switch ctx.Exit {
	case wjit.ExitCall, wjit.ExitCallRef:
		return uint32(ctx.Arg2), false
	case wjit.ExitCallIndirect:
		return uint32(ctx.Arg3), false
	case wjit.ExitOp:
		return uint32(ctx.Arg4), false
	}
	return 0, true
}

// jitExitCatching is jitExit taking the exceptions out of its work to the
// level's catch clauses: it returns the landing pad to enter where one
// takes it.
func (x *exec) jitExitCatching(lv *jitLevel) (raw *ijit.RawCode, off int, tail bool) {
	at, gone := exitAt(&lv.ctx)
	st := saveTry(x)
	defer func() {
		p := recover()
		if p == nil {
			return
		}
		var exn *Exception
		switch e := p.(type) {
		case *Exception:
			exn = e
		case *delegation:
			// A delegation out of the frame it was made in is its
			// exception.
			exn = e.exn
		default:
			panic(p)
		}
		st.restore(x)
		if r, o, ok := x.jitCatch(lv, exn, at, gone); ok {
			raw, off, tail = r, o, false
			return
		}
		panic(p)
	}()
	return x.jitExit(lv)
}

// jitCatch finds the catch clause of the level's native frames that takes
// exn, the innermost frame at offset at -- or, gone, not taking it -- and
// the others at their calls. It drops the frames above the clause's,
// stores the exception's values in the slots of the clause's label, and
// returns the clause's landing pad.
func (x *exec) jitCatch(lv *jitLevel, exn *Exception, at uint32, gone bool) (*ijit.RawCode, int, bool) {
	ctx := &lv.ctx
	code := lv.inst.Code
	rs := x.jit.rs
	first := x.rsIndex(lv.rsBase)
	n := (x.rsIndex(ctx.RSP) - first) / wjit.RSEntryWords
	entry := func(k int) int { return first + k*wjit.RSEntryWords }
	// Each frame's reference slots: the first's where Go entered it, and
	// each one's past its caller's if that holds references.
	rbase := make([]uintptr, n)
	r := lv.r0
	for k := 0; k < n; k++ {
		rbase[k] = r
		r += uintptr(code.get(uint32(rs[entry(k)+2]>>32)).nativeRefSlots()) * 16
	}
	for k := n - 1; k >= 0; k-- {
		if k == n-1 && gone {
			continue
		}
		fn := uint32(rs[entry(k)+2] >> 32)
		off, l := at, ctx.L
		if k < n-1 {
			off, l = uint32(rs[entry(k+1)+2]), uintptr(rs[entry(k+1)+1])
		}
		cd := code.get(fn)
		nat := cd.native.Load()
		// A delegate passes its body's exceptions to the constructs at its
		// target and out: the handlers of those within it let them pass.
		limit := math.MaxInt
		for _, h := range nat.handlers {
			if off < h.Start || off >= h.End || h.Index > limit {
				continue
			}
			if h.Delegate {
				limit = min(limit, h.Target)
				continue
			}
			for _, cl := range h.Clauses {
				if !cl.All && exn.Tag != lv.inst.Tags[cl.Tag] {
					continue
				}
				li := int((l - lv.chunkBase) / 8)
				ri := int((rbase[k] - lv.rchunkBase) / 16)
				nr := cd.nativeRefSlots()
				f := exitFrame{cd: cd, l: x.nchunks[lv.nc][li : li+cd.nslots], r: x.rchunks[lv.rc][ri : ri+nr]}
				pos := cl.Base
				if !cl.All {
					for i, t := range lv.inst.Module.TagType(cl.Tag).Params {
						f.put(t, cd.nlocals+2*(pos+i), exn.Vals[i])
					}
					pos += len(exn.Vals)
				}
				if cl.Ref {
					f.r[cd.rlocals+pos] = ExnRef(exn)
				}
				if cl.Caught >= 0 {
					f.r[cl.Caught] = ExnRef(exn)
				}
				ctx.L, ctx.R = l, rbase[k]
				ctx.RSP = lv.rsBase + uintptr(k+1)*rsEntryBytes
				return nat.raw, cl.Pad, true
			}
		}
	}
	return nil, 0, false
}
