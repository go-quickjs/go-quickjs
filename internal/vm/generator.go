package vm

import (
	"sync/atomic"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

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
//
// One is made at every call of a generator or an async function, so what
// few of them need is kept apart, in rare, and the arguments and the locals
// share one slice: the object and its generator then fit a smaller size
// class, with room for a small function's values beside them (see
// newGenerator).
type generator struct {
	cl     *closure
	this   Value
	callee *Object

	// vals is the arguments, nargs of them, and then the locals: see args
	// and locals. It is heap-allocated so that upvalues captured by closures
	// created inside the generator survive suspension.
	vals []Value
	// stack holds the operand stack contents at the point of suspension.
	stack    []Value
	pc       uint32
	nargs    uint32
	handlers []handler
	// openUpvalues carries the captures across a suspension, so that two
	// closures made in different resumptions still share a binding.
	openUpvalues []*upvalue
	// withScopes carries the enclosing `with` objects across a suspension: a
	// generator may yield from inside a with body, and the body still has to
	// resolve names against the object when it resumes.
	withScopes []*Object
	// rare is what only some generators need, or nil.
	rare *genRare

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
}

// genRare is what a generator needs only where it is an async generator,
// an async arrow or has a direct eval: any other has an undefined
// new.target and neither a this binding nor eval vars.
type genRare struct {
	newTarget Value
	// thisRef and evalVars are what run gives a frame: an async arrow's
	// share of the derived constructor's `this`, and where a direct eval's
	// vars go.
	thisRef  *thisBinding
	evalVars *Object

	// queue holds the async generator requests still to be serviced.
	//
	// next, return and throw may all be called again before the previous call
	// has settled. The specification queues them rather than interleaving
	// them: there is one body, and letting a second call re-enter it while the
	// first is suspended at an await would scramble both.
	queue []asyncRequest
	// pumping marks a pumpAsyncGenerator running, which a request finishing
	// meanwhile leaves the next one to.
	pumping bool
	// draining marks a request being serviced, so that one arriving meanwhile
	// joins the queue instead of re-entering the body.
	draining bool
}

// args is the arguments g was called with, a full slice, so that nothing
// appended to it runs into the locals.
func (g *generator) args() []Value { return g.vals[:g.nargs:g.nargs] }

// locals is g's locals.
func (g *generator) locals() []Value { return g.vals[g.nargs:] }

// rareOf is g's rare part, made if it has none.
func (g *generator) rareOf() *genRare {
	if g.rare == nil {
		g.rare = &genRare{newTarget: Undefined}
	}
	return g.rare
}

// frameRare gives f the new.target, this binding and eval vars of g.
func (g *generator) frameRare(f *frame) {
	if x := g.rare; x != nil {
		f.newTarget, f.thisRef, f.evalVars = x.newTarget, x.thisRef, x.evalVars
	} else {
		f.newTarget, f.thisRef, f.evalVars = Undefined, nil, nil
	}
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

// The values are the ones the compiler writes into a delegation's kind
// binding, so the two have to agree.
const (
	resumeNext   resumeMode = bytecode.ResumeNext
	resumeThrow  resumeMode = bytecode.ResumeThrow
	resumeReturn resumeMode = bytecode.ResumeReturn
)

// newGenerator builds the generator object a call to a generator function
// returns.
func (r *Runtime) newGenerator(cl *closure, this Value, args []Value, callee *Object, async bool, newTarget Value) (*Object, error) {
	// The object, its generator, and the arguments and the locals are one
	// allocation where the function has few of them: a generator or an
	// async function is made at every call.
	n := len(args) + cl.fn.LocalCount
	var gobj *generatorObject
	var vals []Value
	switch {
	case n <= 2:
		o := &generatorVals[[2]Value]{}
		gobj, vals = &o.generatorObject, o.vals[:n]
	case n <= 4:
		o := &generatorVals[[4]Value]{}
		gobj, vals = &o.generatorObject, o.vals[:n]
	case n <= 8:
		o := &generatorVals[[8]Value]{}
		gobj, vals = &o.generatorObject, o.vals[:n]
	default:
		gobj, vals = &generatorObject{}, make([]Value, n)
	}
	copy(vals, args)
	gobj.Object = Object{class: ClassGenerator, flags: objExtensible}
	g := &gobj.g
	*g = generator{
		cl:     cl,
		this:   this,
		callee: callee,
		vals:   vals,
		nargs:  uint32(len(args)),
		async:  async,
	}
	// Nothing constructs a generator or an async function, so new.target
	// there is undefined, but for an async arrow's, which is where it was
	// written. An async generator queues its requests.
	if !newTarget.IsUndefined() {
		g.rareOf().newTarget = newTarget
	}
	if async && cl.fn.Generator {
		g.rareOf()
	}
	locals := g.locals()
	// Parameters are bound now rather than on first resumption, because the
	// specification evaluates them when the generator is created. A default
	// value or a destructuring pattern therefore runs -- and can throw -- at
	// the call rather than at the first next().
	n = cl.fn.ParamCount
	if n > len(locals) {
		n = len(locals)
	}
	for i := 0; i < n; i++ {
		if i < len(args) {
			locals[i] = args[i]
		} else {
			locals[i] = Undefined
		}
	}

	// A generator written inside a `with` body resolves the names in its own
	// body against the objects too. An ordinary call picks that up when the
	// frame is set up; a generator builds its frames itself, so it records the
	// chain once here and restores it on every resumption.
	// The rest is what run sets up for a frame, which a generator's are
	// given on every resumption: an async arrow in a derived constructor
	// shares its `this` binding, and a body with a direct eval gets
	// somewhere for the vars the eval declares -- without which they went
	// onto the global object.
	if callee != nil {
		if fd := callee.fn(); fd != nil {
			if w := fd.extra.with(); len(w) > 0 {
				g.withScopes = w[:len(w):len(w)]
			}
			if fd.arrow && fd.extra.lexThisRef != nil {
				g.rareOf().thisRef = fd.extra.lexThisRef
			}
			if ev := fd.extra.evalVars(); ev != nil {
				g.rareOf().evalVars = ev
			}
		}
	}
	if cl.fn.HasDirectEval {
		ev := newObject(nil, ClassObject)
		ev.flags |= objEvalVars
		g.rareOf().evalVars = ev
		g.withScopes = append(g.withScopes[:len(g.withScopes):len(g.withScopes)], ev)
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
	// `g.prototype` and every generator g makes will have it. A generator
	// function's is a data property it cannot lose or make an accessor, which
	// is read where it is. An async function's generator is never seen, and
	// the function has no prototype to read: one a script gives it, a getter
	// perhaps, is not read at each call.
	if callee != nil && cl.fn.Generator {
		if p := callee.getOwn(atomPrototype); p.ok() && p.flags()&(propAccessor|propPrivate|propDeleted) == 0 {
			if p.value().IsObject() {
				proto = p.value().Object()
			}
		} else if custom, err := r.getValueProp(Obj(callee), atomPrototype); err == nil &&
			custom.IsObject() {
			proto = custom.Object()
		}
	}
	gobj.proto = proto
	gobj.embedExt(&gobj.side)
	gobj.side.data = g
	return &gobj.Object, nil
}

// generatorObject is a generator's object and its state, made together.
type generatorObject struct {
	Object
	side objectExt
	g    generator
}

// generatorVals is a generatorObject and the arguments and locals of a
// generator with few of them, V being an array of Values.
type generatorVals[V any] struct {
	generatorObject
	vals V
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
	if r.frameDepth >= r.maxFrames {
		return r.throwRangeError("maximum call stack size exceeded")
	}
	base := r.stackTop
	if base+fn.MaxStack > len(r.stack) {
		return r.throwRangeError("maximum call stack size exceeded")
	}
	if fn.ParamEnd == 1 && fn.Code[0].Op == bytecode.OpEndParams && r.debug == nil && r.stopped == nil {
		// A prologue of end_params alone -- parameters without defaults or
		// patterns -- binds nothing newGenerator has not: the body begins
		// after it. A debugger is left to see it run, and a stopped runtime
		// to refuse to.
		g.pc = 1
		return nil
	}
	r.stackTop = base + fn.MaxStack
	r.stackHigh = max(r.stackHigh, r.stackTop)

	f := r.pushFrame()
	*f = frame{
		cl:         g.cl,
		locals:     g.locals(),
		base:       base,
		this:       g.this,
		callee:     g.callee,
		args:       g.args(),
		withScopes: g.withScopes,
		paramsOnly: true,
	}
	g.frameRare(f)
	_, err := r.executeAt(f, base, nil)
	// The body resumes where the prologue stopped.
	g.pc = f.pc
	g.openUpvalues = f.openUpvalues
	g.withScopes = f.withScopes
	f.detachSuspended()

	r.frameDepth--
	r.stackTop = base
	return err
}

// generatorOf recovers a generator from a receiver.
func (r *Runtime) generatorOf(this Value, name string) (*generator, error) {
	if !this.IsObject() || this.Object().class != ClassGenerator {
		return nil, r.throwTypeError("%s called on an incompatible receiver", name)
	}
	g, ok := this.Object().dataOf().(*generator)
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
	// delegate marks a suspension inside a `yield*`, whose value has already
	// been through whatever awaiting it needed.
	delegate bool
	// realm is the realm a result object for the value is made in, where it
	// is not the running one's: a synchronous `yield*` that stepped a
	// generator itself yields what it gave as a value (see delegateNext),
	// and the generator's next it called would have made the object in the
	// realm the body runs in.
	realm *Realm
}

// result renders a resumption as the object a generator's next, return or
// throw hands back.
func (r *Runtime) result(res resumeResult) Value {
	if res.raw && res.value.IsObject() {
		return res.value
	}
	if re := res.realm; re != nil && re != r.Realm {
		prev := r.Realm
		r.Realm = re
		o := r.iterResult(res.value, res.done)
		r.Realm = prev
		return Obj(o)
	}
	return Obj(r.iterResult(res.value, res.done))
}

// resume runs a generator until its next suspension or completion.
func (r *Runtime) resume(g *generator, sent Value, mode resumeMode) (Value, bool, error) {
	res, err := r.resumeFull(g, sent, mode)
	return res.value, res.done, err
}

// nextTree is g's tree, where next() on g can be made by resumeTree: g has
// not finished and is not running, and its body is a tree. It is nil
// anywhere else.
func (g *generator) nextTree() *tree {
	if g.state > genSuspendedYield {
		return nil
	}
	if t := (*tree)(atomic.LoadPointer(&g.cl.fn.VMCode)); t != nil && t.gen != nil {
		return t
	}
	return nil
}

// resumeFull runs a generator and reports what kind of suspension stopped it.
func (r *Runtime) resumeFull(g *generator, sent Value, mode resumeMode) (resumeResult, error) {
	if mode == resumeNext {
		// The common case, a body its tree runs, is resumed there.
		if t := g.nextTree(); t != nil {
			if res, ran, err := resumeTree(r, g, t, sent); ran {
				return res, err
			}
		}
	}
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

	// The body runs in the realm it was written in, as a call of it does,
	// whichever realm's next resumes it. It is put back as a call puts it
	// back, without a defer: a panic that passes here closes the runtime.
	if re := g.cl.realm; re != nil && re != r.Realm {
		prev := r.Realm
		r.Realm = re
		res, err := r.resumeFrame(g, sent, mode)
		r.Realm = prev
		return res, err
	}
	return r.resumeFrame(g, sent, mode)
}

// resumeFrame is resumeFull for a generator that has not finished and is
// not running, in its own realm: it gives the generator a frame again and
// runs it from where it was suspended.
func (r *Runtime) resumeFrame(g *generator, sent Value, mode resumeMode) (resumeResult, error) {
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

	if r.frameDepth >= r.maxFrames {
		return resumeResult{value: Undefined, done: true}, r.throwRangeError("maximum call stack size exceeded")
	}

	fn := g.cl.fn
	// Only the operand stack needs a window; the locals live on the heap.
	base := r.stackTop
	if base+fn.MaxStack > len(r.stack) {
		return resumeResult{value: Undefined, done: true}, r.throwRangeError("maximum call stack size exceeded")
	}
	r.stackTop = base + fn.MaxStack
	// What endTurn clears back to, as for any other frame.
	r.stackHigh = max(r.stackHigh, r.stackTop)

	// Restore the operand stack from the previous suspension.
	sp := base
	if len(g.stack) != 0 {
		copy(r.stack[base:], g.stack)
		sp += len(g.stack)
	}

	// The frame is set field by field, as runFD sets a call's: a frame is
	// five cache lines, and building one whole and copying it in was most
	// of what a resumption cost. Its tree context is set by runGenerator.
	f := r.pushFrame()
	f.cl = g.cl
	f.locals = g.locals()
	f.base = base
	f.pc = g.pc
	f.this = g.this
	f.callee = g.callee
	f.args = g.args()
	f.handlers = g.handlers
	f.openUpvalues = g.openUpvalues
	f.withScopes = g.withScopes
	g.frameRare(f)
	if f.native != "" {
		f.native = ""
	}
	if f.savedSP != 0 {
		f.savedSP = 0
	}
	if f.paramsOnly {
		f.paramsOnly = false
	}

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
// value and the kind of resumption are already on the operand stack, as its
// tree where it has one.
func (r *Runtime) runDelegating(g *generator, f *frame, base, sp int) (resumeResult, error) {
	v, err := runGenerator(r, f, sp, nil)
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

	v, err := runGenerator(r, f, sp, pending)
	return r.finishResume(g, f, base, v, err)
}

// finishResume records what a resumption produced: a suspension to be resumed
// again, or the end of the generator.
func (r *Runtime) finishResume(g *generator, f *frame, base int,
	v Value, err error) (resumeResult, error) {
	if sig, ok := err.(*suspendSignal); ok {
		// Save everything the next resumption needs and release the window.
		g.pc = f.pc
		if g.stack = g.stack[:0]; f.savedSP > base {
			g.stack = append(g.stack, r.stack[base:f.savedSP]...)
		}
		g.handlers = f.handlers
		g.openUpvalues = f.openUpvalues
		g.withScopes = f.withScopes
		g.delegating = sig.delegate
		g.state = genSuspendedYield
		g.started = true
		r.releaseGeneratorFrame(g, base)
		res := resumeResult{
			value: sig.value, await: sig.await, raw: sig.raw, delegate: sig.delegate,
		}
		if sig.delegate && !sig.raw && !g.async {
			res.realm = g.cl.realm
		}
		// A tree's suspension is the runtime's one, which is not to keep
		// the value alive.
		sig.value = Undefined
		return res, nil
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
	if f := r.topFrame(); f != nil {
		if g.state == genCompleted {
			// Only now are the captures detached from the locals.
			for _, u := range f.openUpvalues {
				u.close()
			}
		}
		f.detachSuspended()
		r.frameDepth--
	}
	// The window is left as a returning call leaves its own, for endTurn
	// to clear up to where the turn reached.
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

	next := r.defMethod(p, "next", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
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
	next.fn().iterNext = iterNextGenerator
	r.genNextFn = next

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
	r.genIterFn = r.defSymbolMethod(p, r.wellKnown.iterator, "[Symbol.iterator]", 0,
		func(rt *Runtime, this Value, args []Value) (Value, error) {
			return this, nil
		})
	r.defToStringTag(p, "Generator")
}

// iterResult builds the { value, done } object the iteration protocol uses.
//
// Every one is made with room for its two properties, and every one has the
// same two, so after the first the rest copy its shape rather than finding
// each property's place: one allocation where there were three.
func (r *Runtime) iterResult(v Value, done bool) *Object {
	o := newLiteralObject(r.proto.object, ClassObject, 2)
	if s := r.iterResultShape; s != nil {
		o.vals = append(o.vals, v, Bool(done))
		o.shape = s
		return o
	}
	o.setOwnRaw(atomValue, v, propDefault)
	o.setOwnRaw(atomDone, Bool(done), propDefault)
	if s := o.shape; s != nil && !s.unique && s.n == 2 {
		r.iterResultShape = s
	}
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
	// %AsyncIteratorPrototype% carries the one method that makes an async
	// iterator its own iterable, so that `for await (x of it)` works on any of
	// them without each having to say so.
	r.defSymbolMethod(r.proto.asyncIterator, r.wellKnown.asyncIterator,
		"[Symbol.asyncIterator]", 0,
		func(rt *Runtime, this Value, args []Value) (Value, error) {
			return this, nil
		})

	r.proto.asyncGenerator = newObject(r.proto.asyncIterator, ClassObject)
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
			g.rare.queue = append(g.rare.queue, asyncRequest{
				result: result, sent: arg(args, 0), mode: mode,
			})
			rt.pumpAsyncGenerator(g)
			return Obj(result), nil
		}
	}
	r.defMethod(p, "next", 1, drive(resumeNext))
	r.defMethod(p, "return", 1, drive(resumeReturn))
	r.defMethod(p, "throw", 1, drive(resumeThrow))

	r.defToStringTag(p, "AsyncGenerator")
}

// pumpAsyncGenerator services the queued requests, one at a time. A request
// that finishes at once hands the next back to the loop here rather than
// starting it itself, so that a long queue on a generator that has ended is
// a loop and not a recursion as deep as the queue is long.
func (r *Runtime) pumpAsyncGenerator(g *generator) {
	x := g.rare
	if x.pumping {
		return
	}
	x.pumping = true
	defer func() { x.pumping = false }()
	for !x.draining && len(x.queue) > 0 {
		r.pumpOne(g)
	}
}

// pumpOne starts the request at the head of the queue.
func (r *Runtime) pumpOne(g *generator) {
	g.rare.draining = true
	req := g.rare.queue[0]
	if req.mode == resumeReturn {
		// The value a return injects is awaited before the generator sees it,
		// so returning a promise into one delivers what it settles to -- and a
		// broken promise becomes a throw at the resumption point rather than a
		// result nobody can use.
		r.awaitValue(req.sent, g, awaitReturn)
		return
	}
	r.stepAsyncGenerator(g, req.sent, req.mode)
}

// finishAsyncRequest settles the request being serviced and moves on to the
// next one.
func (r *Runtime) finishAsyncRequest(g *generator, reject bool, v Value) {
	x := g.rare
	if len(x.queue) == 0 {
		return
	}
	req := x.queue[0]
	x.queue = x.queue[1:]
	x.draining = false
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
		r.awaitValue(res.value, g, awaitGenerator)
		return
	}

	// A yield or a return settles the caller's promise with an iterator result.
	// The yielded value is awaited first, so that `yield somePromise` produces
	// the value rather than the promise.
	if res.done {
		r.finishAsyncRequest(g, false, Obj(r.iterResult(res.value, true)))
		return
	}
	if res.delegate {
		// `yield*` passes on what the delegate produced. Its result has
		// already been awaited as a whole, and awaiting the value inside it
		// again would unwrap a promise the delegate meant to hand over.
		r.finishAsyncRequest(g, false, Obj(r.iterResult(res.value, false)))
		return
	}
	r.awaitValue(res.value, g, awaitYield)
}
