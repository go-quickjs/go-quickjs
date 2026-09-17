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
	// withScopes carries the enclosing `with` objects across a suspension: a
	// generator may yield from inside a with body, and the body still has to
	// resolve names against the object when it resumes.
	withScopes []*Object

	state genState
	// started marks a generator whose body has begun, which decides whether a
	// resumption delivers a sent value. The pc cannot answer that any more,
	// since a generator starts partway in -- its parameter prologue runs when
	// it is created.
	started bool
	// delegating marks a suspension inside a `yield*`, where a throw or a
	// return injected at the resumption point is forwarded to the inner
	// iterator instead of unwinding this generator.
	delegating bool
	// async marks a generator that backs an async function, whose suspensions
	// are awaits rather than yields.
	async bool
	// promise is the async function's result promise.
	promise *Object

	// queue holds the async generator requests still to be serviced.
	//
	// next, return and throw may all be called again before the previous call
	// has settled. The specification queues them rather than interleaving
	// them: there is one body, and letting a second call re-enter it while the
	// first is suspended at an await would scramble both.
	queue []asyncRequest
	// draining marks a request being serviced, so that one arriving meanwhile
	// joins the queue instead of re-entering the body.
	draining bool
}

// asyncRequest is one pending call to an async generator's next, return or
// throw.
type asyncRequest struct {
	result *Object
	sent   Value
	mode   resumeMode
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
	// raw marks a value that is already an iterator result object, which a
	// synchronous `yield*` yields verbatim.
	raw bool
}

func (*suspendSignal) Error() string { return "generator suspended" }

// returnSignal is a forced return injected at a generator's suspension point by
// generator.return().
//
// Like suspendSignal it travels as an error to reuse the return path, and like
// it is deliberately not a *Thrown, so a catch inside the body ignores it. Only
// a finally clause sees it, which is exactly what a return statement at that
// position would do.
type returnSignal struct{ value Value }

func (*returnSignal) Error() string { return "generator returned" }

// resumeMode says how a generator is being re-entered.
type resumeMode uint8

const (
	resumeNext resumeMode = iota
	resumeThrow
	resumeReturn
)

// newGenerator builds the generator object a call to a generator function
// returns.
func (r *Runtime) newGenerator(cl *closure, this Value, args []Value, callee *Object, async bool) (*Object, error) {
	g := &generator{
		cl:     cl,
		this:   this,
		args:   append([]Value(nil), args...),
		callee: callee,
		locals: make([]Value, cl.fn.LocalCount),
		async:  async,
		// Nothing constructs a generator or an async function, so new.target
		// there is undefined -- which has to be said, since a zero Value is
		// the number zero rather than undefined.
		newTarget: Undefined,
	}
	// Parameters are bound now rather than on first resumption, because the
	// specification evaluates them when the generator is created. A default
	// value or a destructuring pattern therefore runs -- and can throw -- at
	// the call rather than at the first next().
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

	// A generator written inside a `with` body resolves the names in its own
	// body against the objects too. An ordinary call picks that up when the
	// frame is set up; a generator builds its frames itself, so it records the
	// chain once here and restores it on every resumption.
	if callee != nil {
		if fd := callee.fn(); fd != nil && len(fd.lexWith) > 0 {
			g.withScopes = fd.lexWith[:len(fd.lexWith):len(fd.lexWith)]
		}
	}

	if err := r.bindGeneratorParams(g); err != nil {
		return nil, err
	}

	proto := r.proto.generator
	if async {
		// An async generator has its own prototype, whose methods return
		// promises.
		proto = r.proto.asyncGenerator
	}
	// The instance inherits from the function's own prototype object, which
	// inherits in turn from the shared one -- so a script can put something on
	// `g.prototype` and every generator g makes will have it.
	if callee != nil {
		if custom, err := r.getValueProp(Obj(callee), atomPrototype); err == nil &&
			custom.IsObject() {
			proto = custom.Object()
		}
	}
	o := newObject(proto, ClassGenerator)
	o.data = g
	return o, nil
}

// bindGeneratorParams runs a generator's parameter prologue, which evaluates
// defaults and unpacks patterns.
//
// It runs on the caller's turn because the specification binds a generator's
// parameters when the generator object is created: `function* g([x]) {}` called
// with a non-iterable throws there, not at the first next(). The prologue
// cannot yield, so it is safe to run to completion in a borrowed frame.
func (r *Runtime) bindGeneratorParams(g *generator) error {
	fn := g.cl.fn
	if fn.ParamEnd == 0 {
		return nil
	}
	if len(r.frames) >= r.maxFrames {
		return r.throwRangeError("maximum call stack size exceeded")
	}
	base := r.stackTop
	if base+fn.MaxStack > len(r.stack) {
		return r.throwRangeError("maximum call stack size exceeded")
	}
	r.stackTop = base + fn.MaxStack

	f := r.pushFrame()
	*f = frame{
		cl:         g.cl,
		locals:     g.locals,
		base:       base,
		this:       g.this,
		newTarget:  g.newTarget,
		callee:     g.callee,
		args:       g.args,
		withScopes: g.withScopes,
		paramsOnly: true,
	}
	_, err := r.executeAt(f, base, nil)
	// The body resumes where the prologue stopped.
	g.pc = f.pc
	g.openUpvalues = f.openUpvalues
	g.withScopes = f.withScopes

	r.frames = r.frames[:len(r.frames)-1]
	clear(r.stack[base:r.stackTop])
	r.stackTop = base
	return err
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

// asyncGeneratorOf is generatorOf for the asynchronous methods, which a
// synchronous generator is not a receiver for: the two protocols differ in what
// their methods hand back, so neither prototype's methods work on the other.
func (r *Runtime) asyncGeneratorOf(this Value, name string) (*generator, error) {
	g, err := r.generatorOf(this, name)
	if err != nil {
		return nil, err
	}
	if !g.async {
		return nil, r.throwTypeError("%s called on a synchronous generator", name)
	}
	return g, nil
}

// resumeResult says how a resumption ended.
//
// value is Undefined rather than the zero Value wherever nothing was produced:
// a zero Value is not undefined, and a completed generator has to keep
// answering { value: undefined, done: true } however many times it is asked.
type resumeResult struct {
	value Value
	// done marks the generator finishing rather than suspending.
	done bool
	// raw marks a value that is already the result object to hand back, which
	// a synchronous `yield*` produces: what the delegate said is what the
	// caller sees, down to the identity of the object.
	raw bool
	// await marks a suspension caused by `await`, which an async generator
	// must service before producing anything.
	await bool
}

// result renders a resumption as the object a generator's next, return or
// throw hands back.
func (r *Runtime) result(res resumeResult) Value {
	if res.raw && res.value.IsObject() {
		return res.value
	}
	return Obj(r.iterResult(res.value, res.done))
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
		return resumeResult{value: Undefined, done: true}, r.throwTypeError("the generator is already running")
	case genCompleted:
		// A completed generator answers every request the same way.
		switch mode {
		case resumeThrow:
			return resumeResult{value: Undefined, done: true}, r.throw(sent)
		case resumeReturn:
			return resumeResult{value: sent, done: true}, nil
		}
		return resumeResult{value: Undefined, done: true}, nil
	}

	// A return or throw before the body starts finishes it without running.
	if g.state == genSuspendedStart {
		switch mode {
		case resumeReturn:
			g.state = genCompleted
			return resumeResult{value: sent, done: true}, nil
		case resumeThrow:
			g.state = genCompleted
			return resumeResult{value: Undefined, done: true}, r.throw(sent)
		}
	}

	if len(r.frames) >= r.maxFrames {
		return resumeResult{value: Undefined, done: true}, r.throwRangeError("maximum call stack size exceeded")
	}

	fn := g.cl.fn
	// Only the operand stack needs a window; the locals live on the heap.
	base := r.stackTop
	if base+fn.MaxStack > len(r.stack) {
		return resumeResult{value: Undefined, done: true}, r.throwRangeError("maximum call stack size exceeded")
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
		withScopes:   g.withScopes,
	}
	f := gf

	if g.state == genSuspendedYield && g.delegating {
		// Suspended inside a `yield*`: every way of resuming is forwarded to
		// the inner iterator, so the kind is delivered as a value rather than
		// acted on here.
		g.delegating = false
		g.state = genExecuting
		r.stack[sp] = sent
		r.stack[sp+1] = Int(int(mode))
		return r.runDelegating(g, f, base, sp+2)
	}
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
			// A return at the suspension point is not simply the end of the
			// generator: the try blocks the body is suspended inside still owe
			// their finally clauses, and one of them may yield again or
			// override the returned value.
			g.state = genExecuting
			return r.runGeneratorFrom(g, f, base, sp, Undefined, &returnSignal{value: sent})
		}
	}
	g.state = genExecuting
	return r.runGeneratorFrom(g, f, base, sp, sent, nil)
}

// runDelegating resumes a generator suspended inside a `yield*`, where the
// value and the kind of resumption are already on the operand stack.
func (r *Runtime) runDelegating(g *generator, f *frame, base, sp int) (resumeResult, error) {
	v, err := r.executeAt(f, sp, nil)
	return r.finishResume(g, f, base, v, err)
}

// runGeneratorFrom drives the interpreter for one resumption.
//
// pending, when non-nil, is an exception injected at the suspension point,
// which is how generator.throw() works.
func (r *Runtime) runGeneratorFrom(g *generator, f *frame, base, sp int, sent Value, pending error) (resumeResult, error) {
	if pending == nil && g.started {
		// Deliver the sent value as the yield expression's result.
		r.stack[sp] = sent
		sp++
	}

	v, err := r.executeAt(f, sp, pending)
	return r.finishResume(g, f, base, v, err)
}

// finishResume records what a resumption produced: a suspension to be resumed
// again, or the end of the generator.
func (r *Runtime) finishResume(g *generator, f *frame, base int,
	v Value, err error) (resumeResult, error) {
	if sig, ok := err.(*suspendSignal); ok {
		// Save everything the next resumption needs and release the window.
		g.pc = f.pc
		g.stack = append(g.stack[:0], r.stack[base:f.savedSP]...)
		g.handlers = f.handlers
		g.openUpvalues = f.openUpvalues
		g.withScopes = f.withScopes
		g.delegating = sig.delegate
		g.state = genSuspendedYield
		g.started = true
		r.releaseGeneratorFrame(g, base)
		return resumeResult{value: sig.value, await: sig.await, raw: sig.raw}, nil
	}

	g.state = genCompleted
	g.stack = g.stack[:0]
	r.releaseGeneratorFrame(g, base)
	if err != nil {
		return resumeResult{value: Undefined, done: true}, err
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

// initGeneratorFunctionIntrinsics builds the prototype chain a generator, async
// or async generator function sits on.
//
// The three are ordinary functions as far as calling goes, but each has its own
// intrinsic prototype so that Object.prototype.toString names it and so that a
// generator's .prototype inherits next, return and throw. Without them every
// one of these reports itself as a plain Function.
func (r *Runtime) initGeneratorFunctionIntrinsics() {
	// %GeneratorFunction.prototype%, which every generator function inherits.
	r.genFuncProto = newObject(r.proto.function, ClassObject)
	r.defToStringTag(r.genFuncProto, "GeneratorFunction")
	r.genFuncProto.setOwnRaw(atomPrototype, Obj(r.proto.generator), propConfigurable)
	// The constructor of a generator object is the intrinsic every generator
	// function inherits from, not the function that made it: there is no way
	// to construct a generator object, so the name points at the shape rather
	// than at a callable. It is read-only, which is what tells it apart from
	// an ordinary prototype's.
	r.proto.generator.setOwnRaw(atomConstructor, Obj(r.genFuncProto), propConfigurable)

	r.asyncFuncProto = newObject(r.proto.function, ClassObject)
	r.defToStringTag(r.asyncFuncProto, "AsyncFunction")

	r.asyncGenFuncProto = newObject(r.proto.function, ClassObject)
	r.defToStringTag(r.asyncGenFuncProto, "AsyncGeneratorFunction")
	r.asyncGenFuncProto.setOwnRaw(atomPrototype, Obj(r.proto.asyncGenerator), propConfigurable)
	r.proto.asyncGenerator.setOwnRaw(atomConstructor, Obj(r.asyncGenFuncProto), propConfigurable)
}

// funcProtoFor returns the intrinsic prototype a compiled function object
// should have.
func (r *Runtime) funcProtoFor(fn *bytecode.Function) *Object {
	switch {
	case fn.Generator && fn.Async:
		return r.asyncGenFuncProto
	case fn.Generator:
		return r.genFuncProto
	case fn.Async:
		return r.asyncFuncProto
	}
	return r.proto.function
}

// instanceProtoFor returns what a generator function's .prototype should
// inherit from, or nil for a function whose .prototype is ordinary.
func (r *Runtime) instanceProtoFor(fn *bytecode.Function) *Object {
	switch {
	case fn.Generator && fn.Async:
		return r.proto.asyncGenerator
	case fn.Generator:
		return r.proto.generator
	}
	return nil
}

func (r *Runtime) initGeneratorBuiltins() {
	r.proto.generator = newObject(r.proto.iterator, ClassObject)
	p := r.proto.generator

	r.defMethod(p, "next", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		g, err := rt.generatorOf(this, "Generator.prototype.next")
		if err != nil {
			return Undefined, err
		}
		res, err := rt.resumeFull(g, arg(args, 0), resumeNext)
		if err != nil {
			return Undefined, err
		}
		return rt.result(res), nil
	})

	r.defMethod(p, "return", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		g, err := rt.generatorOf(this, "Generator.prototype.return")
		if err != nil {
			return Undefined, err
		}
		res, err := rt.resumeFull(g, arg(args, 0), resumeReturn)
		if err != nil {
			return Undefined, err
		}
		return rt.result(res), nil
	})

	r.defMethod(p, "throw", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		g, err := rt.generatorOf(this, "Generator.prototype.throw")
		if err != nil {
			return Undefined, err
		}
		res, err := rt.resumeFull(g, arg(args, 0), resumeThrow)
		if err != nil {
			return Undefined, err
		}
		return rt.result(res), nil
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
			g, err := rt.asyncGeneratorOf(this, "AsyncGenerator.prototype.next")
			if err != nil {
				// A method on the wrong receiver rejects rather than throws,
				// because every async generator method returns a promise.
				p := rt.newPromise()
				rt.rejectPromise(p, thrownValue(err))
				return Obj(p), nil
			}
			result := rt.newPromise()
			g.queue = append(g.queue, asyncRequest{
				result: result, sent: arg(args, 0), mode: mode,
			})
			rt.pumpAsyncGenerator(g)
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

// pumpAsyncGenerator services the queued requests, one at a time.
func (r *Runtime) pumpAsyncGenerator(g *generator) {
	if g.draining || len(g.queue) == 0 {
		return
	}
	g.draining = true
	req := g.queue[0]
	if req.mode == resumeReturn {
		// The value a return injects is awaited before the generator sees it,
		// so returning a promise into one delivers what it settles to -- and a
		// broken promise becomes a throw at the resumption point rather than a
		// result nobody can use.
		awaited := r.toPromise(req.sent)
		onFulfilled := r.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, error) {
			rt.stepAsyncGenerator(g, arg(a, 0), resumeReturn)
			return Undefined, nil
		})
		onRejected := r.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, error) {
			rt.stepAsyncGenerator(g, arg(a, 0), resumeThrow)
			return Undefined, nil
		})
		r.promiseThen(awaited, Obj(onFulfilled), Obj(onRejected))
		return
	}
	r.stepAsyncGenerator(g, req.sent, req.mode)
}

// finishAsyncRequest settles the request being serviced and moves on to the
// next one.
func (r *Runtime) finishAsyncRequest(g *generator, reject bool, v Value) {
	if len(g.queue) == 0 {
		return
	}
	req := g.queue[0]
	g.queue = g.queue[1:]
	g.draining = false
	if reject {
		r.rejectPromise(req.result, v)
	} else {
		r.resolvePromise(req.result, v)
	}
	r.pumpAsyncGenerator(g)
}

// stepAsyncGenerator advances an async generator until it yields or finishes,
// settling the promise the caller holds.
func (r *Runtime) stepAsyncGenerator(g *generator, sent Value, mode resumeMode) {
	res, err := r.resumeFull(g, sent, mode)
	if err != nil {
		r.finishAsyncRequest(g, true, thrownValue(err))
		return
	}

	if res.await {
		// An await is the generator's own business: settle the awaited value
		// and resume, without the caller seeing anything.
		awaited := r.toPromise(res.value)
		onFulfilled := r.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, error) {
			rt.stepAsyncGenerator(g, arg(a, 0), resumeNext)
			return Undefined, nil
		})
		onRejected := r.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, error) {
			rt.stepAsyncGenerator(g, arg(a, 0), resumeThrow)
			return Undefined, nil
		})
		r.promiseThen(awaited, Obj(onFulfilled), Obj(onRejected))
		return
	}

	// A yield or a return settles the caller's promise with an iterator result.
	// The yielded value is awaited first, so that `yield somePromise` produces
	// the value rather than the promise.
	if res.done {
		r.finishAsyncRequest(g, false, Obj(r.iterResult(res.value, true)))
		return
	}
	awaited := r.toPromise(res.value)
	onFulfilled := r.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, error) {
		rt.finishAsyncRequest(g, false, Obj(rt.iterResult(arg(a, 0), false)))
		return Undefined, nil
	})
	onRejected := r.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, error) {
		// The await happens inside the generator, at the yield, so a
		// rejection is a throw there rather than merely a rejected result:
		// a try round the yield can catch it, and an uncaught one finishes
		// the generator instead of leaving it suspended.
		rt.stepAsyncGenerator(g, arg(a, 0), resumeThrow)
		return Undefined, nil
	})
	r.promiseThen(awaited, Obj(onFulfilled), Obj(onRejected))
}
