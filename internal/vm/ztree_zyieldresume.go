package vm

// The common resumption of a generator, an async function or an async
// generator whose body is a tree: next() on one suspended at a yield, a
// yield* or an await's promise settling, where nothing is injected at the
// suspension point. It is what resumeFull, resumeFrame,
// runGenerator and finishResume do for it, in one place: a for...of loop
// over a generator resumes it once a value, and passing the frame and the
// result through each of them was most of what a value cost.
//
// It is a variable assigned a closure, as ztree_zyield.go's functions are,
// so that it is laid out after the closures already there.

var (
	// resumeTree resumes g, not yet started or suspended at a yield, a
	// yield* or an await, in t, its tree (see generator.nextTree), with sent as that
	// expression's value, and runs it to its next suspension or its end,
	// as resumeFull(g, sent, resumeNext) does. It reports false, having
	// done nothing, where resumeFull's general path is needed: the body
	// runs in another realm or cannot be given a frame, or the runtime has
	// been stopped.
	resumeTree func(r *Runtime, g *generator, t *tree, sent Value) (resumeResult, bool, error)
)

func init() {
	resumeTree = func(r *Runtime, g *generator, t *tree, sent Value) (resumeResult, bool, error) {
		cl := g.cl
		fn := cl.fn
		if (cl.realm != nil && cl.realm != r.Realm) || r.stopped != nil || r.frameDepth >= r.maxFrames {
			return resumeResult{}, false, nil
		}
		base := r.stackTop
		top := base + fn.MaxStack
		if top > len(r.stack) {
			return resumeResult{}, false, nil
		}
		r.stackTop = top
		r.stackHigh = max(r.stackHigh, top)

		// The operand stack as it was at the suspension, and on it the
		// value sent in, which is the yield's or the await's. What starts
		// the body is sent nowhere.
		sp := base
		if len(g.stack) != 0 {
			copy(r.stack[base:], g.stack)
			sp += len(g.stack)
		}
		if g.started {
			r.stack[sp] = sent
			sp++
		}
		if g.delegating {
			// Suspended inside a yield*, whose resumption is given the kind
			// of resumption above the value, to forward to the delegate
			// (see runDelegating).
			g.delegating = false
			r.stack[sp] = Int(int(resumeNext))
			sp++
		}

		f := r.pushFrame()
		locals := g.vals[g.nargs:]
		f.cl = cl
		f.locals = locals
		f.base = base
		f.pc = g.pc
		f.this = g.this
		f.callee = g.callee
		f.args = g.vals[:g.nargs:g.nargs]
		f.handlers = g.handlers
		f.openUpvalues = g.openUpvalues
		f.withScopes = g.withScopes
		if x := g.rare; x != nil {
			f.newTarget, f.thisRef, f.evalVars = x.newTarget, x.thisRef, x.evalVars
		} else {
			f.newTarget, f.thisRef, f.evalVars = Undefined, nil, nil
		}
		if f.native != "" {
			f.native = ""
		}
		if f.savedSP != 0 {
			f.savedSP = 0
		}
		if f.paramsOnly {
			f.paramsOnly = false
		}
		g.state = genExecuting

		c := &f.tc
		c.r, c.f, c.cl, c.locals = r, f, cl, locals
		c.closureCode = cl.closureCode
		c.stack = r.stack[base:top]
		if t.gen.iters {
			// As runGenerator: an exception looks for cursors in every
			// slot of the frame's.
			clear(r.stack[sp:top])
		}
		v, err := runGeneratorTree(r, f, t, t.gen.index[f.pc])
		s, ok := err.(*suspendSignal)
		if !ok {
			res, err := r.finishResume(g, f, base, v, err)
			return res, true, err
		}
		// Suspended again: what finishResume saves, and the frame let go.
		g.pc = f.pc
		if g.stack = g.stack[:0]; f.savedSP > base {
			g.stack = append(g.stack, r.stack[base:f.savedSP]...)
		}
		g.handlers = f.handlers
		g.openUpvalues = f.openUpvalues
		g.withScopes = f.withScopes
		g.delegating = s.delegate
		g.state = genSuspendedYield
		g.started = true
		f.detachSuspended()
		r.frameDepth--
		r.stackTop = base
		res := resumeResult{value: s.value, await: s.await, raw: s.raw, delegate: s.delegate}
		if s.delegate && !s.raw && !g.async {
			// A value a yield* stepped a generator to (see delegateNext).
			res.realm = cl.realm
		}
		// The suspension is the runtime's one, which is not to keep the
		// value alive.
		s.value = Undefined
		return res, true, nil
	}
}
