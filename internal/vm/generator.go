package vm

import (
	"unsafe"

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
// few of them need is kept apart, in rare, what a suspension leaves but
// for its place in saved, and the arguments and the locals share one
// slice: the object and its generator then fit a smaller size class, with
// room for a small function's values beside them (see newGenerator).
type generator struct {
	cl     *closure
	this   Value
	callee *Object

	// vals is the frame's window, the code's registers, and then the
	// arguments the code keeps, nargs of them (newRegGenerator). It is
	// heap-allocated so that what the frame holds survives suspension.
	vals  []Value
	pc    uint32
	nargs uint32
	// saved is what its suspensions leave besides its place, the
	// captures of its registers (see keep), nil where none ever left any:
	// a generator native code may resume (abi's GenSaved).
	saved *genSaved
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
	// room marks a generator made with room for the cursor of the first
	// loop over it, which no loop has taken yet (see genAlloc).
	room bool
	// reg marks a generator whose body runs as register code, its vals
	// the frame's window and its pc a register pc (reg_generator.go): every
	// generator newGenerator makes.
	reg bool
	// awaitJob marks an async function its await's job is resuming, from
	// the job queue itself (resumeAwait): an await of a settled value
	// there may go on in place (reg_generator.go).
	awaitJob bool
}

// genSaved is what a generator's suspension leaves for its next
// resumption besides its place, made by the first that leaves any: most
// suspend with nothing captured.
type genSaved struct {
	// openUpvalues carries the captures across a suspension, so that two
	// closures made in different resumptions still share a binding.
	openUpvalues []*upvalue
}

// savedUpvalues is the captures of g's locals its frames have made.
func (g *generator) savedUpvalues() []*upvalue {
	if s := g.saved; s != nil {
		return s.openUpvalues
	}
	return nil
}

// keep saves what a suspension of g leaves for its next resumption: the
// captures its frame had, ups, which g takes over (see detachSuspended).
// g's saved part is made where they are the first thing left.
//
//go:noinline
func (g *generator) keep(ups []*upvalue) {
	s := g.saved
	if s == nil {
		if len(ups) == 0 {
			return
		}
		s = &genSaved{}
		g.saved = s
	}
	s.openUpvalues = ups
}

// object is g's object, in whose generatorObject it is.
func (g *generator) object() *Object {
	return &(*generatorObject)(unsafe.Add(unsafe.Pointer(g), -int(unsafe.Offsetof(generatorObject{}.g)))).Object
}

// genOf is the generator of o, an object of class ClassGenerator: the
// generator follows it in its generatorObject.
func genOf(o *Object) *generator {
	return &(*generatorObject)(unsafe.Pointer(o)).g
}

// genRare is what a generator needs only where it is an async generator,
// an async arrow, has a direct eval or is written in or suspends in a
// `with` body: any other has an undefined new.target and neither a this
// binding, eval vars nor with scopes.
type genRare struct {
	newTarget Value
	// thisRef and evalVars are what run gives a frame: an async arrow's
	// share of the derived constructor's `this`, and where a direct eval's
	// vars go.
	thisRef  *thisBinding
	evalVars *Object
	// withScopes carries the enclosing `with` objects across a suspension:
	// a generator may yield from inside a with body, and the body still has
	// to resolve names against the object when it resumes.
	withScopes []*Object

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

// rareOf is g's rare part, made if it has none.
func (g *generator) rareOf() *genRare {
	if g.rare == nil {
		g.rare = &genRare{newTarget: Undefined}
	}
	return g.rare
}

// frameRare gives f the new.target, this binding, eval vars and with
// scopes of g. The eval's variables are the innermost of the with
// scopes that hold them: the body's own scope for them, where its
// prologue pushed one past the parameters' (with_push's mark), which a
// suspension keeps among the with scopes.
func (g *generator) frameRare(f *frame) {
	if x := g.rare; x != nil {
		f.newTarget, f.thisRef, f.evalVars, f.withScopes = x.newTarget, x.thisRef, x.evalVars, x.withScopes
		if f.evalVars != nil {
			for i := len(f.withScopes) - 1; i >= 0; i-- {
				if o := f.withScopes[i]; o.flags&objEvalVars != 0 {
					f.evalVars = o
					break
				}
			}
		}
	} else {
		f.newTarget, f.thisRef, f.evalVars, f.withScopes = Undefined, nil, nil, nil
	}
}

// withScopes is the `with` objects g's body is inside.
func (g *generator) withScopes() []*Object {
	if x := g.rare; x != nil {
		return x.withScopes
	}
	return nil
}

// saveWith keeps w, the `with` objects a frame of g's was inside when it
// suspended, for its next resumption.
func (g *generator) saveWith(w []*Object) {
	if x := g.rare; x != nil {
		x.withScopes = w
	} else if len(w) != 0 {
		g.rareOf().withScopes = w
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
// deliberately not a *Thrown, so the handler search (regCatch) ignores it
// and a try/catch inside a generator cannot swallow a yield.
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
	// async function is made at every call. The arguments are kept only
	// where something reads them once the parameters are bound: an
	// arguments object, a direct eval, a prologue run in the interpreter
	// -- a default, a pattern, a rest parameter -- or a debugger, which
	// sees even a prologue of end_params run. The parameters are bound
	// below, from the call's arguments.
	c := r.regGenOf(cl, callee)
	if c == nil {
		return nil, errNoRegCode(cl.fn)
	}
	return r.newRegGenerator(cl, c, this, args, callee, async, newTarget)
}

// generatorObject is a generator's object and its state, made together.
// The object has no ext, its class saying where the state is (genOf):
// one of a generator would be an ext of its own, as an ordinary object's
// is (extOf).
type generatorObject struct {
	Object
	g generator
}

// generatorVals is a generatorObject and the arguments and locals of a
// generator with few of them, V being an array of Values.
type generatorVals[V any] struct {
	generatorObject
	vals V
}

// generatorLoop is a generatorObject with room for the cursor of the
// first loop over it (see genAlloc), and generatorLoopVals one with its
// arguments and locals too.
type generatorLoop struct {
	generatorObject
	cur iterObject
}

type generatorLoopVals[V any] struct {
	generatorLoop
	vals V
}

// asyncObject is the generatorObject of an async function, which no loop
// steps and nothing sees, with the promise of the function's result
// (promiseOf), and asyncVals one with its arguments and locals too.
type asyncObject struct {
	generatorObject
	promise *Object
}

type asyncVals[V any] struct {
	asyncObject
	vals V
}

// promiseOf is the promise of the result of the async function g backs,
// in its asyncObject.
func (g *generator) promiseOf() **Object {
	o := (*asyncObject)(unsafe.Add(unsafe.Pointer(g), -int(unsafe.Offsetof(generatorObject{}.g))))
	return &o.promise
}

// The kinds of generator genAlloc makes.
const (
	genPlainKind = iota
	// genLoopKind is a generator with room for the cursor of the first
	// loop over it.
	genLoopKind
	// genAsyncKind is an async function's, with its promise.
	genAsyncKind
)

// generatorOf recovers a generator from a receiver.
func (r *Runtime) generatorOf(this Value, name string) (*generator, error) {
	if !this.IsObject() || this.Object().class != ClassGenerator {
		return nil, r.throwTypeError("%s called on an incompatible receiver", name)
	}
	return genOf(this.Object()), nil
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
//
// It has three fields, the kinds of suspension one set of bits, so that the
// compiler keeps one in registers: a struct of more than four fields lives
// in memory, and each resumption had stored this one field by field and
// read it back whole, a load the stores could not be forwarded to.
type resumeResult struct {
	value Value
	how   resumeHow
	// realm is the realm a result object for the value is made in, where it
	// is not the running one's: a synchronous `yield*` that stepped a
	// generator itself yields what it gave as a value (see delegateNext),
	// and the generator's next it called would have made the object in the
	// realm the body runs in.
	realm *Realm
}

// resumeHow is how a resumption ended, as bits.
type resumeHow uint8

const (
	// resumeDone marks the generator finishing rather than suspending.
	resumeDone resumeHow = 1 << iota
	// resumeRaw marks a value that is already the result object to hand
	// back, which a synchronous `yield*` produces: what the delegate said is
	// what the caller sees, down to the identity of the object.
	resumeRaw
	// resumeAwait marks a suspension caused by `await`, which an async
	// generator must service before producing anything.
	resumeAwait
	// resumeDelegate marks a suspension inside a `yield*`, whose value has
	// already been through whatever awaiting it needed.
	resumeDelegate
)

func (res resumeResult) done() bool     { return res.how&resumeDone != 0 }
func (res resumeResult) raw() bool      { return res.how&resumeRaw != 0 }
func (res resumeResult) await() bool    { return res.how&resumeAwait != 0 }
func (res resumeResult) delegate() bool { return res.how&resumeDelegate != 0 }

// suspendHow is how a suspension s ended a resumption.
func suspendHow(s *suspendSignal) resumeHow {
	var how resumeHow
	if s.raw {
		how |= resumeRaw
	}
	if s.await {
		how |= resumeAwait
	}
	if s.delegate {
		how |= resumeDelegate
	}
	return how
}

// result renders a resumption as the object a generator's next, return or
// throw hands back.
func (r *Runtime) result(res resumeResult) Value {
	if res.raw() && res.value.IsObject() {
		return res.value
	}
	if re := res.realm; re != nil && re != r.Realm {
		prev := r.Realm
		r.Realm = re
		o := r.iterResult(res.value, res.done())
		r.Realm = prev
		return Obj(o)
	}
	return Obj(r.iterResult(res.value, res.done()))
}

// resume runs a generator until its next suspension or completion.
func (r *Runtime) resume(g *generator, sent Value, mode resumeMode) (Value, bool, error) {
	res, err := r.resumeFull(g, sent, mode)
	return res.value, res.done(), err
}

// resumeFull runs a generator and reports what kind of suspension stopped it.
func (r *Runtime) resumeFull(g *generator, sent Value, mode resumeMode) (resumeResult, error) {
	if mode == resumeNext {
		// The common case, suspended and of this realm, is resumed at
		// once.
		if g.state <= genSuspendedYield && (g.cl.realm == nil || g.cl.realm == r.Realm) {
			return r.regGenResume(g, sent, mode)
		}
	}
	switch g.state {
	case genExecuting:
		return resumeResult{value: Undefined, how: resumeDone}, r.throwTypeError("the generator is already running")
	case genCompleted:
		// A completed generator answers every request the same way.
		switch mode {
		case resumeThrow:
			return resumeResult{value: Undefined, how: resumeDone}, r.throw(sent)
		case resumeReturn:
			return resumeResult{value: sent, how: resumeDone}, nil
		}
		return resumeResult{value: Undefined, how: resumeDone}, nil
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
			return resumeResult{value: sent, how: resumeDone}, nil
		case resumeThrow:
			g.state = genCompleted
			return resumeResult{value: Undefined, how: resumeDone}, r.throw(sent)
		}
	}

	if r.frameDepth >= r.maxFrames {
		return resumeResult{value: Undefined, how: resumeDone}, r.throwRangeError("maximum call stack size exceeded")
	}
	return r.regGenResume(g, sent, mode)
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
	keyOrder[struct{}](r, p, nil, "%GeneratorPrototype%")

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

	r.genReturnFn = r.defMethod(p, "return", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
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

	// A generator is its own iterator through %IteratorPrototype%'s
	// Symbol.iterator, which it inherits: %GeneratorPrototype% has none of
	// its own, so that one a script gives %IteratorPrototype% is a
	// generator's too.
	r.defToStringTag(p, "Generator")
}

// iterResult builds the { value, done } object the iteration protocol uses.
//
// Every one is made with room for its two properties, and every one has the
// same two, so after the first the rest copy its shape rather than finding
// each property's place: one allocation where there were three.
func (r *Runtime) iterResult(v Value, done bool) *Object {
	if s := r.iterResultShape; s != nil && s.proto == r.proto.object {
		o := newShapedLiteral(s, ClassObject, 2)
		o.setVals(append(o.vals(), v, Bool(done)))
		return o
	}
	o := newLiteralObject(r.proto.object, ClassObject, 2)
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
	keyOrder[struct{}](r, p, nil, "%AsyncGeneratorPrototype%")

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

	if res.await() {
		// An await is the generator's own business: settle the awaited value
		// and resume, without the caller seeing anything.
		r.awaitValue(res.value, g, awaitGenerator)
		return
	}

	// A yield or a return settles the caller's promise with an iterator result.
	// The yielded value is awaited first, so that `yield somePromise` produces
	// the value rather than the promise.
	if res.done() {
		r.finishAsyncRequest(g, false, Obj(r.iterResult(res.value, true)))
		return
	}
	if res.delegate() {
		// `yield*` passes on what the delegate produced. Its result has
		// already been awaited as a whole, and awaiting the value inside it
		// again would unwrap a promise the delegate meant to hand over.
		r.finishAsyncRequest(g, false, Obj(r.iterResult(res.value, false)))
		return
	}
	r.awaitValue(res.value, g, awaitYield)
}
