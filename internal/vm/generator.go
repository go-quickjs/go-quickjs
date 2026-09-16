package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// Generators.
//
// A generator suspends by saving its frame and resumes by restoring it. The
// alternative -- running the body on its own goroutine and handing values
// across a channel -- is simpler to write but leaks a goroutine for every
// generator that is never exhausted, which for an engine embedded in a server
// is unacceptable.
//
// Saving the frame is possible because `yield` is only valid syntactically
// inside the generator's own body, never inside a function it calls. So at the
// moment of suspension the generator's frame is the top of the call stack and
// nothing below it needs preserving.
//
// The one wrinkle is upvalues: a closure created inside a generator holds a
// pointer into the frame's locals. A generator's locals are therefore allocated
// on the heap rather than carved out of the shared stack, so that those
// pointers stay valid across a suspension.

// genState is where a generator is in its lifetime.
type genState uint8

const (
	// genSuspendedStart is a generator that has been created but whose body has
	// not begun.
	genSuspendedStart genState = iota
	genSuspendedYield
	genExecuting
	genCompleted
)

// generator holds a suspended generator's state.
type generator struct {
	cl        *closure
	this      Value
	newTarget Value
	args      []Value
	callee    *Object

	// locals is heap-allocated so that upvalues captured by closures created
	// inside the generator survive suspension.
	locals []Value
	// stack holds the operand stack contents at the point of suspension.
	stack    []Value
	pc       uint32
	handlers []handler
	// openUpvalues carries the captures across a suspension, so that two
	// closures made in different resumptions still share a binding.
	openUpvalues []*upvalue

	state genState
	// async marks a generator that backs an async function, whose suspensions
	// are awaits rather than yields.
	async bool
	// promise is the async function's result promise.
	promise *Object
}

// suspendSignal is returned by the interpreter when a generator yields.
//
// It travels as an error so that the existing return path carries it, but it is
// deliberately not a *Thrown, so the handler search in unwindToHandler ignores
// it and a try/catch inside a generator cannot swallow a yield.
type suspendSignal struct {
	value Value
	// await marks a suspension caused by `await` rather than `yield`.
	await bool
	// delegate marks `yield*`, whose value is an iterable to drain.
	delegate bool
}

func (*suspendSignal) Error() string { return "generator suspended" }

// resumeMode says how a generator is being re-entered.
type resumeMode uint8

const (
	resumeNext resumeMode = iota
	resumeThrow
	resumeReturn
)

// newGenerator builds the generator object a call to a generator function
// returns.
func (r *Runtime) newGenerator(cl *closure, this Value, args []Value, callee *Object, async bool) *Object {
	g := &generator{
		cl:     cl,
		this:   this,
		args:   append([]Value(nil), args...),
		callee: callee,
		locals: make([]Value, cl.fn.LocalCount),
		async:  async,
	}
	// Parameters are bound now rather than on first resumption, because the
	// specification evaluates them when the generator is created.
	n := cl.fn.ParamCount
	if n > len(g.locals) {
		n = len(g.locals)
	}
	for i := 0; i < n; i++ {
		if i < len(args) {
			g.locals[i] = args[i]
		} else {
			g.locals[i] = Undefined
		}
	}

	proto := r.proto.generator
	if async {
		// An async generator has its own prototype, whose methods return
		// promises.
		proto = r.proto.asyncGenerator
	}
	o := newObject(proto, ClassGenerator)
	o.data = g
	return o
}

// generatorOf recovers a generator from a receiver.
func (r *Runtime) generatorOf(this Value, name string) (*generator, error) {
	if !this.IsObject() || this.Object().class != ClassGenerator {
		return nil, r.throwTypeError("%s called on an incompatible receiver", name)
	}
	g, ok := this.Object().data.(*generator)
	if !ok {
		return nil, r.throwTypeError("%s called on an uninitialized generator", name)
	}
	return g, nil
}

// resumeResult says how a resumption ended.
type resumeResult struct {
	value Value
	// done marks the generator finishing rather than suspending.
	done bool
	// await marks a suspension caused by `await`, which an async generator
	// must service before producing anything.
	await bool
}

// resume runs a generator until its next suspension or completion.
func (r *Runtime) resume(g *generator, sent Value, mode resumeMode) (Value, bool, error) {
	res, err := r.resumeFull(g, sent, mode)
	return res.value, res.done, err
}

// resumeFull runs a generator and reports what kind of suspension stopped it.
func (r *Runtime) resumeFull(g *generator, sent Value, mode resumeMode) (resumeResult, error) {
	switch g.state {
	case genExecuting:
		return resumeResult{done: true}, r.throwTypeError("the generator is already running")
	case genCompleted:
		// A completed generator answers every request the same way.
		switch mode {
		case resumeThrow:
			return resumeResult{done: true}, r.throw(sent)
		case resumeReturn:
			return resumeResult{value: sent, done: true}, nil
		}
		return resumeResult{done: true}, nil
	}

	// A return or throw before the body starts finishes it without running.
	if g.state == genSuspendedStart {
		switch mode {
		case resumeReturn:
			g.state = genCompleted
			return resumeResult{value: sent, done: true}, nil
		case resumeThrow:
			g.state = genCompleted
			return resumeResult{done: true}, r.throw(sent)
		}
	}

	if len(r.frames) >= r.maxFrames {
		return resumeResult{done: true}, r.throwRangeError("maximum call stack size exceeded")
	}

	fn := g.cl.fn
	// Only the operand stack needs a window; the locals live on the heap.
	base := r.stackTop
	if base+fn.MaxStack > len(r.stack) {
		return resumeResult{done: true}, r.throwRangeError("maximum call stack size exceeded")
	}
	r.stackTop = base + fn.MaxStack

	// Restore the operand stack from the previous suspension.
	copy(r.stack[base:], g.stack)
	sp := base + len(g.stack)

	gf := r.pushFrame()
	*gf = frame{
		cl:           g.cl,
		locals:       g.locals,
		base:         base,
		pc:           g.pc,
		this:         g.this,
		newTarget:    g.newTarget,
		callee:       g.callee,
		args:         g.args,
		handlers:     g.handlers,
		openUpvalues: g.openUpvalues,
	}
	f := gf

	if g.state == genSuspendedYield {
		// The value sent in becomes the result of the yield expression that
		// suspended the generator.
		switch mode {
		case resumeThrow:
			// A throw at the suspension point behaves as if the yield itself
			// had thrown, so it can be caught by a try inside the body.
			g.state = genExecuting
			return r.runGeneratorFrom(g, f, base, sp, Undefined, r.throw(sent))
		case resumeReturn:
			g.state = genCompleted
			r.releaseGeneratorFrame(g, base)
			return resumeResult{value: sent, done: true}, nil
		}
	}
	g.state = genExecuting
	return r.runGeneratorFrom(g, f, base, sp, sent, nil)
}

// runGeneratorFrom drives the interpreter for one resumption.
//
// pending, when non-nil, is an exception injected at the suspension point,
// which is how generator.throw() works.
func (r *Runtime) runGeneratorFrom(g *generator, f *frame, base, sp int, sent Value, pending error) (resumeResult, error) {
	if pending == nil && g.pc > 0 {
		// Deliver the sent value as the yield expression's result.
		r.stack[sp] = sent
		sp++
	}

	v, err := r.executeAt(f, sp, pending)

	if sig, ok := err.(*suspendSignal); ok {
		// Save everything the next resumption needs and release the window.
		g.pc = f.pc
		g.stack = append(g.stack[:0], r.stack[base:f.savedSP]...)
		g.handlers = f.handlers
		g.openUpvalues = f.openUpvalues
		g.state = genSuspendedYield
		r.releaseGeneratorFrame(g, base)
		return resumeResult{value: sig.value, await: sig.await}, nil
	}

	g.state = genCompleted
	g.stack = g.stack[:0]
	r.releaseGeneratorFrame(g, base)
	if err != nil {
		return resumeResult{done: true}, err
	}
	return resumeResult{value: v, done: true}, nil
}

// releaseGeneratorFrame pops a generator's frame without closing its upvalues,
// which must survive until the generator completes.
func (r *Runtime) releaseGeneratorFrame(g *generator, base int) {
	if len(r.frames) > 0 {
		f := &r.frames[len(r.frames)-1]
		if g.state == genCompleted {
			// Only now are the captures detached from the locals.
			for _, u := range f.openUpvalues {
				u.close()
			}
		}
		r.frames = r.frames[:len(r.frames)-1]
	}
	clear(r.stack[base:r.stackTop])
	r.stackTop = base
}

func (r *Runtime) initGeneratorBuiltins() {
	r.proto.generator = newObject(r.proto.iterator, ClassObject)
	p := r.proto.generator

	r.defMethod(p, "next", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		g, err := rt.generatorOf(this, "Generator.prototype.next")
		if err != nil {
			return Undefined, err
		}
		v, done, err := rt.resume(g, arg(args, 0), resumeNext)
		if err != nil {
			return Undefined, err
		}
		return Obj(rt.iterResult(v, done)), nil
	})

	r.defMethod(p, "return", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		g, err := rt.generatorOf(this, "Generator.prototype.return")
		if err != nil {
			return Undefined, err
		}
		v, done, err := rt.resume(g, arg(args, 0), resumeReturn)
		if err != nil {
			return Undefined, err
		}
		return Obj(rt.iterResult(v, done)), nil
	})

	r.defMethod(p, "throw", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		g, err := rt.generatorOf(this, "Generator.prototype.throw")
		if err != nil {
			return Undefined, err
		}
		v, done, err := rt.resume(g, arg(args, 0), resumeThrow)
		if err != nil {
			return Undefined, err
		}
		return Obj(rt.iterResult(v, done)), nil
	})

	// A generator is its own iterator.
	r.defSymbolMethod(p, r.wellKnown.iterator, "[Symbol.iterator]", 0,
		func(rt *Runtime, this Value, args []Value) (Value, error) {
			return this, nil
		})
	r.defToStringTag(p, "Generator")
}

// iterResult builds the { value, done } object the iteration protocol uses.
func (r *Runtime) iterResult(v Value, done bool) *Object {
	o := newObject(r.proto.object, ClassObject)
	o.setOwnRaw(atomValue, v, propDefault)
	o.setOwnRaw(atomDone, Bool(done), propDefault)
	return o
}

// isGeneratorTemplate reports whether a compiled function suspends.
func isGeneratorTemplate(fn *bytecode.Function) bool {
	return fn.Generator || fn.Async
}

// ---------------------------------------------------------------------------
// Async generators
// ---------------------------------------------------------------------------

// An async generator is a generator whose next, return and throw methods return
// promises, and whose body may suspend on `await` as well as on `yield`.
//
// Driving one means distinguishing the two kinds of suspension. An await is
// serviced internally -- the awaited value is settled and the body resumed --
// and is invisible to the caller. A yield settles the promise the caller is
// holding. That is why resumeFull reports which kind stopped it.

func (r *Runtime) initAsyncGeneratorBuiltins() {
	r.proto.asyncGenerator = newObject(r.proto.object, ClassObject)
	p := r.proto.asyncGenerator

	drive := func(mode resumeMode) NativeFunc {
		return func(rt *Runtime, this Value, args []Value) (Value, error) {
			g, err := rt.generatorOf(this, "AsyncGenerator.prototype.next")
			if err != nil {
				// A method on the wrong receiver rejects rather than throws,
				// because every async generator method returns a promise.
				p := rt.newPromise()
				rt.rejectPromise(p, thrownValue(err))
				return Obj(p), nil
			}
			result := rt.newPromise()
			rt.stepAsyncGenerator(g, result, arg(args, 0), mode)
			return Obj(result), nil
		}
	}
	r.defMethod(p, "next", 1, drive(resumeNext))
	r.defMethod(p, "return", 1, drive(resumeReturn))
	r.defMethod(p, "throw", 1, drive(resumeThrow))

	r.defSymbolMethod(p, r.wellKnown.asyncIterator, "[Symbol.asyncIterator]", 0,
		func(rt *Runtime, this Value, args []Value) (Value, error) {
			return this, nil
		})
	r.defToStringTag(p, "AsyncGenerator")
}

// stepAsyncGenerator advances an async generator until it yields or finishes,
// settling the promise the caller holds.
func (r *Runtime) stepAsyncGenerator(g *generator, result *Object, sent Value, mode resumeMode) {
	res, err := r.resumeFull(g, sent, mode)
	if err != nil {
		r.rejectPromise(result, thrownValue(err))
		return
	}

	if res.await {
		// An await is the generator's own business: settle the awaited value
		// and resume, without the caller seeing anything.
		awaited := r.toPromise(res.value)
		onFulfilled := r.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, error) {
			rt.stepAsyncGenerator(g, result, arg(a, 0), resumeNext)
			return Undefined, nil
		})
		onRejected := r.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, error) {
			rt.stepAsyncGenerator(g, result, arg(a, 0), resumeThrow)
			return Undefined, nil
		})
		r.promiseThen(awaited, Obj(onFulfilled), Obj(onRejected))
		return
	}

	// A yield or a return settles the caller's promise with an iterator result.
	// The yielded value is awaited first, so that `yield somePromise` produces
	// the value rather than the promise.
	value := res.value
	done := res.done
	if !done {
		awaited := r.toPromise(value)
		onFulfilled := r.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, error) {
			rt.resolvePromise(result, Obj(rt.iterResult(arg(a, 0), false)))
			return Undefined, nil
		})
		onRejected := r.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, error) {
			rt.rejectPromise(result, arg(a, 0))
			return Undefined, nil
		})
		r.promiseThen(awaited, Obj(onFulfilled), Obj(onRejected))
		return
	}
	r.resolvePromise(result, Obj(r.iterResult(value, true)))
}
