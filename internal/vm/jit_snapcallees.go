//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// The callees a compile in the background may inline, found on the
// runtime's goroutine before any is decided (jitSnapCalleeWalk), so that
// a snapshot may copy their state for the worker's inliner to decide from
// (jit_snapderive.go): every closure a function's state names as one it
// calls or makes, and theirs in turn, to the snapshot's depth, of those
// whose register code is short enough to be inlined at all
// (jitSnapMayInline). The inliner takes no other: what names a callee to
// inline -- a call's target, a closure the function makes, an accessor --
// names it from the caches and the learned state the walk reads, and every
// inlining of a callee is bounded by its program's size, which is no less
// than its register code's (TestJITLowerRegAnInstructionAPC).

// jitSnapMayInline reports whether cl's function may be inlined as far
// as its register code's length says (ssa.MayInlineSize): one with none
// yet, made at its first call or as a call of it is asked, may. Its code
// is not made here (RegCompiled): made for every closure the walk names,
// it had been compiled for functions no call ever runs.
func jitSnapMayInline(cl *closure) bool {
	if cl.reg != nil && cl.reg.rc != nil {
		return ssa.MayInlineSize(len(cl.reg.rc.Code))
	}
	if rc := cl.fn.RegCompiled(); rc != nil {
		return ssa.MayInlineSize(len(rc.Code))
	}
	return true
}

// jitSnapCalleeWalk is the closures a compile of root's function may
// inline, root's own excepted: each its function's state names
// (jitSnapCalleesOf) that may be inlined (jitSnapMayInline), and those
// theirs name, to jitSnapDepth.
func (r *Runtime) jitSnapCalleeWalk(root *closure) map[*closure]int {
	set := map[*closure]int{}
	level := []*closure{root}
	for d := 0; d < jitSnapDepth && len(level) != 0; d++ {
		var next []*closure
		var add func(c *closure, o *Object)
		add = func(c *closure, o *Object) {
			if c == nil || !jitSnapMayInline(c) {
				return
			}
			if o != nil {
				// A forwarder's (jitRegForwards): what it hands its
				// arguments to, inlined with them where it is constructed
				// with o, or called (forwardInline).
				r.jitSnapForwardsTo(c, o, add)
			}
			if c == root || set[c] != 0 {
				return
			}
			set[c] = d + 1
			next = append(next, c)
		}
		for _, cl := range level {
			r.jitSnapCalleesOf(cl, add)
		}
		level = next
	}
	return set
}

// jitSnapForwardsTo calls add with the functions the forwarder cl hands
// its arguments to, constructed with o or called (jitRegForwardTarget).
func (r *Runtime) jitSnapForwardsTo(cl *closure, o *Object, add func(*closure, *Object)) {
	if cl.reg == nil || !jitRegForwards(cl) {
		return
	}
	for rpc, w := range cl.reg.rc.Code {
		if w.Op() != bytecode.RApplyArgs {
			continue
		}
		for _, ctor := range [2]*Object{o, nil} {
			if t := r.jitRegForwardTarget(cl, ctor, rpc, int(w.X())); t != nil {
				if fd := t.fn(); fd != nil && fd.closure != nil {
					add(fd.closure, nil)
				}
			}
		}
	}
}

// jitSnapCalleesOf calls add with each closure cl's state names as one it
// calls or makes, with the function object it is where known: its calls'
// caches' (regCalls, as a snapshot copies them), the functions its calls'
// bindings hold now and the methods its method calls' reads found on
// prototypes, those its code learned it calls natively or inlines, the
// templates of the closures it makes (its constants), and the getters and
// setters its property caches found on prototypes, now and before, and
// those its code met at its exits.
func (r *Runtime) jitSnapCalleesOf(cl *closure, add func(*closure, *Object)) {
	rc := cl.fn.RegCompiled()
	if rc == nil {
		// Never called, nor made for a call's asking: it has no caches,
		// and names nothing it calls.
		return
	}
	// As a snapshot copies them (jitCallsCopy), restored where a turn's
	// end let them go.
	for _, c := range jitCallsCopy(r, cl) {
		add(c.cl, c.callee)
	}
	for i := range cl.consts {
		if t := cl.consts[i].template(); t != nil {
			add(t, nil)
		}
	}
	fnOf := func(o *Object) {
		if o == nil {
			return
		}
		if fd := o.fn(); fd != nil && !fd.bound && fd.native == nil && fd.closure != nil {
			add(fd.closure, o)
		}
	}
	for i := range cl.ic {
		c := &cl.ic[i]
		for _, write := range [2]bool{false, true} {
			if a, ok := jitCacheAccessor(c, write); ok {
				fnOf(a.fn)
			}
			if seen := c.seenOf(); seen != nil {
				for j := range seen.entries[:seen.n] {
					if a, ok := jitCacheAccessor(&seen.entries[j], write); ok {
						fnOf(a.fn)
					}
				}
			}
		}
	}
	// The functions its calls' bindings hold now, which the calls' first
	// compile guesses (regConstructorNow, regViaNow), and the methods its
	// method calls' reads found on prototypes (jitRegSeenCallees).
	{
		f := &jitRegFeedback{fb: &jitFeedback{r: r, fn: cl.fn, cl: cl}, rc: rc}
		for rpc, w := range rc.Code {
			switch jitRegCallOp(w.Op()) {
			case bytecode.RNewW, bytecode.RCallW:
				if o := f.regConstructorNow(rpc, int(w.A())); o != nil {
					if fd := o.fn(); fd != nil && !fd.bound && fd.native == nil && fd.closure != nil {
						// Its register code made, as the call's target is
						// asked (regCallTargets): a function not yet called
						// has none, and names nothing it calls.
						r.regOf(fd.closure)
						add(fd.closure, o)
					}
				}
			case bytecode.RCallMW:
				if g, c := f.regViaNow(rpc, int(w.A())); c != nil {
					r.regOf(c)
					add(c, g)
				}
				for _, c := range r.jitRegSeenCallees(cl, rc, rpc, w) {
					add(c.cl, c.o)
				}
			}
		}
	}
	s := r.jit
	if s == nil {
		return
	}
	e := s.regLearned[r.jitRegKeyOf(cl)]
	if e == nil {
		return
	}
	for _, list := range [...][]jitInline{e.inlines, e.nativeCalls} {
		for _, x := range list {
			add(x.cl, x.obj)
		}
	}
	for i := range e.poly {
		for _, a := range e.poly[i].accessors {
			fnOf(a.fn)
		}
	}
}
