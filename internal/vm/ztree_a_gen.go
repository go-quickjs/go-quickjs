package vm

import (
	"sync/atomic"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// A tree's loop stepping a generator, with neither the layers of a step
// nor a recover of the resumption's own.
//
// A for...of loop in a tree steps its cursor at every value
// (iterNextOrJump). Over a generator that had been iterNext, stepIter,
// resumeTree, runGeneratorTree and runTreeProtected, each of them
// checking what the one before had, the last setting a recover so that
// an exception the body throws finishes the generator before it goes on
// out. Where the body has no handler and no loop's cursor of its own,
// nothing in it catches an exception or has an iterator to close, and
// the exception goes from the body straight to the loop's: the step runs
// the body's blocks with no recover of its own, as runTreeNested runs a
// callee's, and lets the exception go on to the recover of the tree whose
// loop it is (runTree, runTreeProtected), which unwinds the frames the
// step left -- next's and the generator's, whose captures it closes, as
// finishing the generator does (unwindTreeFrames). What else finishing it
// does is left for the next resumption to find: the cursor is marked done
// before the body runs, so that the loop's unwinding does not close its
// iterator, which threw; and the generator, executing in a tree's step
// (generator.treeRun), is executing while its frame is on the stack and
// finished once it is not (stillRunning).
//
// Its functions are variables, each assigned a closure in init, as the
// tree tier's are (see ztree_zyield.go). The file sorts before every other
// file of the tier with an init, so its closures, made by the init that
// runs first of theirs, are laid out after all of theirs, and move none.

var (
	// iterNextTree is iterNext for a tree's loop, c's, stepping cursor:
	// a dense array's walk and, where its body allows (tgen.plain), a
	// generator's resumption, made here; anything else is iterNext's. An
	// exception a generator's body throws leaves through the loop's
	// recover.
	iterNextTree func(c *tctx, cursor Value) (Value, bool, error)

	// stepGenTree resumes g, the generator st steps, in t, its tree, as
	// stepIter and resumeTree would, with no recover of its own (see
	// above), and reports the value it yields, or false once it is done.
	// false, having done nothing, where it cannot: the step is
	// iterNext's.
	stepGenTree func(c *tctx, st *iterState, g *generator, t *tree) (Value, bool, bool)

	// stillRunning is the TypeError a resumption of g, executing, throws,
	// or nil where g is a generator a tree's step resumed whose body threw
	// past it (see above): its frame is not on the stack. It finishes such
	// a generator, as finishResume would have, but for its captures, which
	// were closed with its frame.
	stillRunning func(r *Runtime, g *generator) error

	// genPlain is t.gen's plain, for t, fn's tree, settled the first time
	// a step asks: whether fn has neither a for...of loop nor a try
	// statement. The tree is the function's, which every runtime shares,
	// so the answer, the same from each, is stored atomically.
	genPlain func(t *tree, fn *bytecode.Function) uint32
)

// What tgen.plain says.
const (
	genPlainYes = 1 + iota
	genPlainNo
)

func init() {
	genPlain = func(t *tree, fn *bytecode.Function) uint32 {
		p := uint32(genPlainYes)
		if t.gen.iters {
			p = genPlainNo
		}
		for _, in := range fn.Code {
			if in.Op == bytecode.OpPushCatch || in.Op == bytecode.OpPushFinally {
				p = genPlainNo
				break
			}
		}
		t.gen.plain.Store(p)
		return p
	}

	iterNextTree = func(c *tctx, cursor Value) (Value, bool, error) {
		st := cursorOf(cursor)
		if st == nil || st.done {
			return Undefined, false, nil
		}
		if a := st.arr; a != nil {
			// As iterNext walks an array: an element in its dense storage
			// that is no hole, here, and anything else there.
			if i := st.idx; i < int(a.arrayLength()) && i < len(a.elemsOf()) {
				if v := a.elemsOf()[i]; !isHole(v) {
					st.idx++
					return v, true, nil
				}
			}
			return c.r.iterNext(cursor)
		}
		if !st.forIn && st.iter.IsObject() {
			if it := st.iter.Object(); it.class == ClassGenerator {
				g := genOf(it)
				if g.state <= genSuspendedYield {
					fn := g.cl.fn
					if t := (*tree)(atomic.LoadPointer(&fn.VMCode)); t != nil && t.gen != nil {
						p := t.gen.plain.Load()
						if p == 0 {
							p = genPlain(t, fn)
						}
						if p == genPlainYes {
							if v, more, ok := stepGenTree(c, st, g, t); ok {
								return v, more, nil
							}
						}
					}
				}
			}
		}
		return c.r.iterNext(cursor)
	}

	stepGenTree = func(c *tctx, st *iterState, g *generator, t *tree) (Value, bool, bool) {
		r := c.r
		gn := r.genNextFn
		cl := g.cl
		fn := cl.fn
		if gn == nil || st.next.ref != unsafe.Pointer(gn) || g.async || g.delegating ||
			(cl.realm != nil && cl.realm != r.Realm) || r.stopped != nil || r.frameDepth+1 >= r.maxFrames {
			return Undefined, false, false
		}
		base := r.stackTop
		top := base + fn.MaxStack
		if top > len(r.stack) {
			return Undefined, false, false
		}
		if err := r.tick(); err != nil {
			c.throw(err)
		}

		// next's frame, as stepIter makes it.
		off := r.frameDepth - r.frameBase
		if uint(off) >= uint(len(r.cur)) {
			r.seekFrameBlock()
			off = r.frameDepth - r.frameBase
		}
		r.frameDepth++
		if r.frameDepth > r.frameHigh {
			r.frameHigh = r.frameDepth
		}
		nf := &r.cur[off]
		nf.cl = nil
		nf.native = gn.fn().name
		nf.this = st.iter
		nf.newTarget = Undefined
		nf.callee = gn
		nf.args = nil
		if len(nf.handlers) != 0 {
			nf.handlers = nf.handlers[:0]
		}
		if len(nf.openUpvalues) != 0 {
			nf.openUpvalues = nf.openUpvalues[:0]
		}

		// The generator's, as resumeTree makes it, its operand stack as
		// the suspension left it and on it undefined, the value a loop
		// sends.
		r.stackTop = top
		r.stackHigh = max(r.stackHigh, top)
		sp := base
		saved := g.saved
		if saved != nil && len(saved.stack) != 0 {
			copy(r.stack[base:], saved.stack)
			sp += len(saved.stack)
		}
		if g.started {
			r.stack[sp] = Undefined
			sp++
		}
		off = r.frameDepth - r.frameBase
		if uint(off) >= uint(len(r.cur)) {
			r.seekFrameBlock()
			off = r.frameDepth - r.frameBase
		}
		r.frameDepth++
		if r.frameDepth > r.frameHigh {
			r.frameHigh = r.frameDepth
		}
		f := &r.cur[off]
		locals := g.vals[g.nargs:]
		f.cl = cl
		f.locals = locals
		f.base = base
		f.pc = g.pc
		f.this = g.this
		f.callee = g.callee
		f.args = g.vals[:g.nargs:g.nargs]
		if saved != nil {
			f.handlers, f.openUpvalues = saved.handlers, saved.openUpvalues
		} else {
			f.handlers, f.openUpvalues = nil, nil
		}
		if x := g.rare; x != nil {
			f.newTarget, f.thisRef, f.evalVars, f.withScopes = x.newTarget, x.thisRef, x.evalVars, x.withScopes
		} else {
			f.newTarget, f.thisRef, f.evalVars, f.withScopes = Undefined, nil, nil, nil
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
		tc := &f.tc
		tc.r, tc.f, tc.cl, tc.locals = r, f, cl, locals
		tc.closureCode = cl.closureCode
		tc.stack = r.stack[base:top]

		var v Value
		var err error
		if r.jitOn() {
			// The JIT may run the body natively from here, or from a back
			// edge of its loop, whose run ends with a panic the protected
			// run of the tree is told it by (tryJITTreeLoop): the body runs
			// as resumeTree runs it, under runGeneratorTree's recover.
			g.state = genExecuting
			v, err = runGeneratorTree(r, f, t, t.gen.index[f.pc])
		} else {
			// Running: an exception leaving the body leaves the loop's
			// cursor done and the generator executing in a step, with its
			// frame gone (stillRunning).
			g.state, g.treeRun = genExecuting, true
			st.done = true
			for b := t.gen.index[f.pc]; ; {
				blk := &t.blocks[b]
				for _, s := range blk.body {
					s(tc)
				}
				if b = blk.next(tc); b < 0 {
					break
				}
			}
			if s := r.treeSuspend; s != nil {
				r.treeSuspend = nil
				err = s
			} else {
				v = tc.ret
			}
		}
		g.treeRun = false
		if s, ok := err.(*suspendSignal); ok {
			// Suspended at a yield, the body having no yield* (a loop's
			// cursor): what resumeTree saves, and the frames let go.
			g.pc = f.pc
			if saved != nil || f.savedSP > base || len(f.handlers) != 0 || len(f.openUpvalues) != 0 {
				g.keep(r.stack[base:max(f.savedSP, base)], f.handlers, f.openUpvalues)
			}
			g.state = genSuspendedYield
			g.started = true
			f.detachSuspended()
			r.frameDepth -= 2
			r.stackTop = base
			v = s.value
			s.value = Undefined
			st.done = false
			return v, true, true
		}
		// Finished, by its return or, under the JIT, an exception: as
		// finishResume finishes it, and the cursor with it.
		st.done = true
		g.state = genCompleted
		g.saved = nil
		for _, u := range f.openUpvalues {
			u.close()
		}
		f.detachSuspended()
		r.frameDepth -= 2
		r.stackTop = base
		if err != nil {
			c.throw(err)
		}
		return Undefined, false, true
	}

	stillRunning = func(r *Runtime, g *generator) error {
		if g.treeRun {
			locals := unsafe.SliceData(g.vals[g.nargs:])
			live := false
			for d := r.frameDepth - 1; d >= 0 && !live; d-- {
				f := r.frameAt(d)
				live = f.cl == g.cl && unsafe.SliceData(f.locals) == locals
			}
			if !live {
				g.state, g.treeRun, g.saved = genCompleted, false, nil
				return nil
			}
		}
		return r.throwTypeError("the generator is already running")
	}
}
