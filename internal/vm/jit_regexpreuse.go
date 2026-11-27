//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"sync/atomic"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/regexp"
)

// Native code's calls of exec at a site whose array may be made again in
// the memory of its last (regexpreuse.go) tell exec's leaf so, as the
// interpreter's do (reuseCallRest): a leaf's call (ssa.GoLeaf) of the
// realm's exec by its description (abi.GoCallState's Reuse), the site the
// register compiler marked (bytecode.RMarkReuse), of the function's own
// code -- not an inlined callee's, whose locals are not its frame's own
// (GoCallState's Inline). What native code holds of a frame's locals its
// slots cannot say, so a native frame's call reads none: it makes the
// array again only where the frame made the last itself -- the table's
// entry held in this frame's slot of the call's local, which the call's
// result is about to replace -- and the pieces' Strings only where nothing
// reads the local they are kept in past the call before it is written,
// which the code's liveness says at its compile (ExecReuse). Its calls
// are told a site of their own (execReuseSite's native), so that the
// arrays native frames made and those the interpreter's made are kept
// apart, each made again only by calls of its own kind.
//
// The functions are generic, each instantiated with struct{} alone.

// ExecReuse is ssa.ExecReuseFeedback's: the call at the program's
// instruction pc of the leaf at fn, the realm's exec, where the register
// compiler marked its array to be made again (bytecode.RMarkReuse), as its
// register pc, plus one, over a low bit set where the pieces' local, if
// any, is read by nothing past the call before it is written, nor
// captured; 0 for any other (jitExecReuse).
func (f *jitRegFeedback) ExecReuse(pc int, fn uintptr) uint32 {
	return jitExecReuse[struct{}](f, pc, fn)
}

// jitExecReuse is ExecReuse's answer.
func jitExecReuse[_ any](f *jitRegFeedback, pc int, fn uintptr) uint32 {
	cl := f.fb.cl
	if cl == nil {
		return 0
	}
	c := cl.reg
	return jitExecReuseOf(c, c != nil && c.rc == f.rc && jitReuseOwn[struct{}](c), f.p, f.fb.r.regexpExecFn, pc, fn)
}

// jitExecReuseOf is ExecReuse's answer at the program's instruction pc of
// p, the closure's register code lowered, of fn, the leaf its Leaf
// answered: c is that register code, own whether it is the program's and
// its function's (jitReuseOwn), execFn the realm's exec -- as the feedback
// reads them live, or a snapshot's gather took them.
func jitExecReuseOf(c *regCode, own bool, p *ir.Program, execFn *Object, pc int, fn uintptr) uint32 {
	if c == nil || pc >= len(p.Origin) || execFn == nil || uintptr(unsafe.Pointer(execFn)) != fn || !own {
		return 0
	}
	rpc := int(p.Origin[pc])
	site := jitReuseSite[struct{}](c, rpc)
	if site == nil || rpc >= 1<<30 {
		return 0
	}
	a := uint32(rpc+1) << 1
	if site.strs && (site.s < 0 || !p.IsCaptured(site.s) && !p.LiveAt(pc+1, site.s)) {
		a |= 1
	}
	return a
}

// jitReuseOwn reports whether c is its function's register code
// (bytecode.Function's VMCode), whose sites a call's description names
// by their register pcs (ExecReuse), for its compile and its frames alike.
func jitReuseOwn[_ any](c *regCode) bool {
	return !c.typed && atomic.LoadPointer(&c.fn.VMCode) == unsafe.Pointer(c)
}

// jitReuseSite is the site of c's call at register pc rpc, a call of exec
// whose array may be made again (bytecode.RMarkReuse), or nil for none.
func jitReuseSite[_ any](c *regCode, rpc int) *execReuseSite {
	if rpc < 0 || rpc >= len(c.rc.Marks) || c.rc.Marks[rpc]&bytecode.RMarkReuse == 0 || regAt(c.code, rpc).Op() != bytecode.RCallMW {
		return nil
	}
	return regReuseAt(c, rpc)
}

// jitRegExpResultForm has form, the leaf's call st describes, make its
// result as the call's description says: none where nothing reads it
// (abi.GoCallState's Unused, jitRegExpDeadNative), exec's array again in
// the memory of its last where the call's Reuse says it may
// (jitReuseForm). It is not inlined, which would lay the making of their
// closures in jitLeafFormOf, in the range of the layout lock's pad N
// (layoutlock.txt).
//
//go:noinline
func jitRegExpResultForm[_ any](r *Runtime, form *jitLeafForm, st *abi.GoCallState) {
	if st.Unused {
		form.native = jitRegExpDeadNative[struct{}](r, (*Object)(*(*unsafe.Pointer)(unsafe.Pointer(&st.Leaf))), form.native)
	} else if fd := (*Object)(*(*unsafe.Pointer)(unsafe.Pointer(&st.Leaf))).fn(); fd.elemOp == elemRegExpExec {
		jitReuseForm[struct{}](form, st)
	}
}

// jitReuseForm has form, the leaf's call of exec st describes, at a site
// whose array the leaf may make again (abi.GoCallState's Reuse), tell the
// leaf of each call, where the call is of the function's own code: the
// leaf told of the call, which it takes as it begins, and untold after,
// as reuseCallRest untells it, where it did not. The site is found at the
// call's first, in the code the frame runs; a call that finds none is
// made plain from then on, one whose RegExp's array would be new however
// it is told (jitReuseFits) tells nothing, and one whose site's last
// array another frame made forgets it first. The leaf is told a site of
// the form's own, whose arrays it keeps apart from the interpreter's
// frames'.
func jitReuseForm[_ any](form *jitLeafForm, st *abi.GoCallState) {
	if st.Inline != 0 {
		return
	}
	native, at, first := form.native, st.Reuse, form.first
	var site *execReuseSite
	// unfit is the address of the RegExp the form's last call found its
	// array would be new of however the leaf were told (jitReuseFits): the
	// calls after on it are made at once, at one comparison. It is an
	// address, so as not to keep the RegExp alive: one freed, its memory
	// another's, is that other's made untold until a call on a third.
	var unfit uintptr
	form.native = func(r *Runtime, this Value, args []Value) (Value, error) {
		if uintptr(this.ref) == unfit {
			return native(r, this, args)
		}
		// The call's context, whose GoArgs from first are args: jitLeafGo
		// alone calls a form's function.
		ctx := (*abi.Context)(unsafe.Add(unsafe.Pointer(unsafe.SliceData(args)), -int(abi.OffGoArgs)-first*int(unsafe.Sizeof(abi.GoArg{}))))
		if site == nil {
			c := jitReuseSiteOf[struct{}](r, ctx, at)
			if c == nil || c.strs && at&1 == 0 {
				// None, or one whose pieces' local the code reads past
				// the call (ExecReuse): plain from now.
				form.native = native
				return native(r, this, args)
			}
			// The site native code's calls are told, of their own: the
			// arrays native frames made are kept apart from the
			// interpreter's frames'.
			site = &execReuseSite{l: c.l, s: c.s, strs: c.strs}
		}
		if r.regexpExecDead == nil || !this.IsObject() || r.meter != nil || r.debug != nil || ctx.Locals == nil {
			return native(r, this, args)
		}
		d, ok := this.Object().dataOf().(*regexpData)
		if ok && !jitReuseFits[struct{}](site, d.re) {
			// Nothing to tell: the array would be new. A site may call
			// exec on several RegExps, a function's parameter's, some
			// of which fit: asked again at a call on another.
			unfit = uintptr(this.ref)
			return native(r, this, args)
		}
		l := execReuseLeaf[struct{}](r)
		if tb := l.reuseTable; ok && tb != nil {
			// The site's last array of as many elements, held where
			// another frame made it, which may still read it from a
			// register: the slot it was held in says nothing. Forgotten,
			// and left as it is: the call makes a new one, this frame's.
			// execReuseAt's entry, its index written out: an instance of
			// it called from here would be laid out among the generics
			// before the RegExp leaf's own, moving them.
			n := d.re.GroupCount() + 1
			e := &tb.e[(uintptr(unsafe.Pointer(site))>>4^uintptr(n))%uintptr(len(tb.e))]
			if e.site == site && e.held != (*Value)(unsafe.Add(ctx.Locals, site.l*int(unsafe.Sizeof(Value{})))) {
				*e = execReuseEntry{}
			}
		}
		n := max(site.l, site.s) + 1
		l.reuse = execReuseCall{site: site, recv: this.Object(), locals: unsafe.Slice((*Value)(ctx.Locals), n)}
		v, err := native(r, this, args)
		if l.reuse.site != nil {
			l.reuse = execReuseCall{}
		}
		return v, err
	}
}

// jitReuseFits reports whether the site's array of a match of re may be
// made again, as matchResultReused asks before it looks: one of no more
// elements than a match block holds, of no indices, whose pieces the
// groups' object does not hold too -- re has no named groups -- and, where
// its pieces are not made again, of more than four elements.
func jitReuseFits[_ any](site *execReuseSite, re *regexp.Regexp) bool {
	n := re.GroupCount() + 1
	return n <= matchBlockMax && re.Flags()&regexp.FlagHasIndices == 0 && (site.strs || n > 4) && len(re.GroupNames()) == 0
}

// jitReuseSiteOf is the site a call's description at names (ExecReuse) in
// the code ctx's frame runs -- the run's at its own level, its closure's
// at a native callee's (jitRegCtxEntry) -- or nil for none.
func jitReuseSiteOf[_ any](r *Runtime, ctx *abi.Context, at uint32) *execReuseSite {
	s := r.jit
	idx := int(ctx.Level)
	if idx >= len(s.ssaCtxs) || s.ssaCtxs[idx] != ctx {
		return nil
	}
	var c *regCode
	if ctx.Live == 0 {
		if idx < len(s.regRuns) && s.regRuns[idx].f != nil {
			c = s.regRuns[idx].c
		}
	} else if cl := (*closure)(ctx.Closure); cl != nil {
		c = cl.reg
	}
	if c == nil || !jitReuseOwn[struct{}](c) {
		return nil
	}
	return jitReuseSite[struct{}](c, int(at>>1)-1)
}
