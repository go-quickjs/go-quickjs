// Package quickjs embeds a JavaScript engine in a Go program.
//
// The engine is a pure-Go reimplementation of QuickJS: no cgo, no wasm, and no
// C toolchain. It cross-compiles anywhere Go does.
//
// # Running code
//
// A Runtime is one isolated JavaScript world. Create one, evaluate source, and
// close it when finished:
//
//	rt := quickjs.New()
//	defer rt.Close()
//
//	v, err := rt.Eval(`[1, 2, 3].map(x => x * 2).join("-")`)
//	if err != nil {
//	    return err
//	}
//	fmt.Println(v) // 2-4-6
//
// # Calling Go from JavaScript
//
// Set an ordinary Go function as a global and it becomes callable from script.
// Arguments and results are converted automatically:
//
//	rt.Set("add", func(a, b int) int { return a + b })
//	rt.Eval(`add(1, 2)`) // 3
//
// A function may return an error, which becomes a thrown JavaScript exception,
// and may take a *Runtime as its first parameter to call back into the engine.
//
// # Reading values back
//
// Decode follows the conventions of encoding/json, so a result can be read into
// an ordinary Go value:
//
//	var out struct {
//	    Name string `js:"name"`
//	    Age  int    `js:"age"`
//	}
//	v, _ := rt.Eval(`({name: "Ada", age: 36})`)
//	err := v.Decode(&out)
//
// # Untrusted code
//
// A Runtime has no I/O, no network access and no timers unless the host adds
// them, so a script cannot reach outside the engine on its own. Bound resources
// are set at construction:
//
//	rt := quickjs.New(
//	    quickjs.WithMemoryLimit(64<<20),
//	    quickjs.WithStackSize(1<<16),
//	    quickjs.WithMaxCallDepth(1000),
//	)
//
// Wall-clock bounds come from a context, which the interpreter checks as it
// runs, so an infinite loop is interrupted rather than hanging the process:
//
//	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
//	defer cancel()
//	_, err := rt.EvalContext(ctx, `while (true) {}`)
//	// err wraps context.DeadlineExceeded
//
// # Concurrency
//
// A Runtime is not safe for concurrent use. Give each goroutine its own, which
// also isolates untrusted scripts from one another.
package quickjs

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync/atomic"
	"time"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
	"github.com/go-quickjs/go-quickjs/internal/parser"
	"github.com/go-quickjs/go-quickjs/internal/vm"
)

// Runtime is an isolated JavaScript world: its own global object, intrinsics
// and interpreter stack.
//
// A Runtime is not safe for concurrent use.
type Runtime struct {
	rt     *vm.Runtime
	closed bool
	// posting is the engine that work from other goroutines is posted to,
	// until the runtime is closed: the one field another goroutine reads.
	posting atomic.Pointer[vm.Runtime]
	// ctx is the runtime's lifetime, which Close ends.
	ctx    context.Context
	cancel context.CancelFunc
	// nodeQuirks is WithNodeQuirks, which the parser and compiler are told.
	nodeQuirks bool
	// noCodeGeneration is WithoutCodeGeneration, which node:vm respects too.
	noCodeGeneration bool
}

// Option configures a Runtime.
type Option func(*config)

type config struct {
	memoryLimit      int64
	stackSize        int
	maxCallDepth     int
	locale           string
	nodeQuirks       bool
	noCodeGeneration bool
}

// WithMemoryLimit caps the memory a script may hold, beyond what the
// runtime's built-ins take. A script that exceeds it is stopped, and the
// Eval, call or job running it returns ErrMemoryLimit: like a cancelled
// context, it is not an exception the script can catch.
//
// The measure is the runtime's own heap -- its objects, strings and buffers
// -- which it walks from its roots when the process has allocated a quarter
// of the limit since it last did, and before any single allocation large
// enough to cross the limit at once. It is an estimate: what a host holds
// for the script, and what Go closures inside the engine capture, is not
// counted. A runtime with a limit is a little slower to allocate.
func WithMemoryLimit(bytes int64) Option {
	return func(c *config) { c.memoryLimit = bytes }
}

// WithStackSize sets the number of value slots shared by all call frames.
//
// This bounds recursion depth: exhausting it raises "maximum call stack size
// exceeded" in the script, exactly as a browser engine does, rather than
// overflowing the goroutine stack.
func WithStackSize(slots int) Option {
	return func(c *config) { c.stackSize = slots }
}

// WithMaxCallDepth bounds recursion independently of the stack size, so that a
// function with very few locals still cannot recurse without bound.
func WithMaxCallDepth(frames int) Option {
	return func(c *config) { c.maxCallDepth = frames }
}

// WithLocale sets the language a script means when it formats something
// without saying which language to format it in: what Intl answers with when
// it is given no locale, and what Date's toString and toLocaleString methods
// write in.
//
// The tag is written the way a tag is written -- "de-DE", "zh-Hant-TW". Left
// unset, the runtime takes the language the machine is set to: the user's
// locale on Windows, LC_ALL, LC_MESSAGES or LANG on Unix, and English where
// none of them says. That is what every other engine does, so that a program
// run twice in the same environment is not given two different answers.
func WithLocale(tag string) Option {
	return func(c *config) { c.locale = tag }
}

// WithNodeQuirks enables observable Node.js behavior where it intentionally or
// temporarily differs from the JavaScript and internationalization standards.
//
// In Intl and Temporal it reproduces Node 26's proleptic Islamic era names,
// Japanese h12 preference, and Temporal locale-formatting behavior for
// standalone era and hour-cycle options, among go-intl's named divergences;
// and Intl.NumberFormat refuses, as ICU does, a numeric string whose first
// digit is more than 999,999,999 places below the point, which the standard
// formats as the number it is. In the language it follows V8: strict code may assign to a call, which
// throws a ReferenceError when it runs; and Annex B hoists a function declared
// in a block over the arguments object, and over a function an enclosing
// block declares with the same name; and a script's global functions and vars
// are created in the order they are written, where the standard creates the
// functions first.
//
// It is useful for hosts that prioritize Node compatibility over conformance.
func WithNodeQuirks() Option {
	return func(c *config) { c.nodeQuirks = true }
}

// New creates a Runtime with the standard globals installed.
func New(opts ...Option) *Runtime {
	var c config
	for _, o := range opts {
		o(&c)
	}
	r := &Runtime{rt: vm.New(vm.Config{
		MemoryLimit:  c.memoryLimit,
		StackSize:    c.stackSize,
		MaxCallDepth: c.maxCallDepth,
		Locale:       c.locale,
		NodeQuirks:   c.nodeQuirks,
	}), nodeQuirks: c.nodeQuirks, noCodeGeneration: c.noCodeGeneration}
	r.rt.Host = r
	r.posting.Store(r.rt)
	r.ctx, r.cancel = context.WithCancel(context.Background())
	if !c.noCodeGeneration {
		r.installCodeGeneration()
	}
	return r
}

// Close releases the runtime. Using a Runtime after Close returns ErrClosed
// from every method.
func (r *Runtime) Close() error {
	r.closed = true
	r.posting.Store(nil)
	r.rt = nil
	if r.cancel != nil {
		r.cancel()
	}
	return nil
}

// Context is the runtime's lifetime: it is cancelled when the runtime is
// closed. A host function that starts work for the script -- a request, a
// program, anything that settles a Promise later -- starts it with this
// context, so that closing the runtime stops the work rather than leaving it
// to finish for no one. It is safe to call from any goroutine.
func (r *Runtime) Context() context.Context {
	if r.ctx == nil {
		return context.Background()
	}
	return r.ctx
}

// ErrMemoryLimit is returned when a script has exceeded the memory limit
// WithMemoryLimit set.
var ErrMemoryLimit = vm.ErrMemoryLimit

// ErrClosed is returned by a Runtime that has been closed.
var ErrClosed = errors.New("quickjs: runtime is closed")

// ErrInternal reports a bug in the engine itself, reached by running a script.
//
// A host embedding this package to run untrusted code must not be taken down by
// the code it is sandboxing, so a panic anywhere inside the engine is caught at
// the boundary and returned as an error instead. It is deliberately not a
// JavaScript exception: a script cannot catch one, because after an internal
// failure the runtime's invariants are no longer known to hold.
//
// Seeing one is always a bug worth reporting.
var ErrInternal = errors.New("quickjs: internal error")

// guard converts a panic inside the engine into an error.
//
// The runtime is marked closed, since continuing to use one whose invariants
// may have been broken is worse than refusing to.
func (r *Runtime) guard(err *error) {
	p := recover()
	if p == nil {
		return
	}
	r.closed = true
	r.posting.Store(nil)
	if r.cancel != nil {
		r.cancel()
	}
	*err = fmt.Errorf("%w: %v\n%s", ErrInternal, p, debug.Stack())
}

// Eval compiles and runs src, returning its completion value.
func (r *Runtime) Eval(src string) (Value, error) {
	return r.EvalContext(context.Background(), src)
}

// EvalContext is Eval with cancellation.
//
// The interpreter checks ctx periodically, so a script that loops forever stops
// when the context is cancelled or its deadline passes. The returned error
// wraps ctx.Err() in that case, so errors.Is(err, context.DeadlineExceeded)
// identifies a timeout.
func (r *Runtime) EvalContext(ctx context.Context, src string) (Value, error) {
	return r.EvalFileContext(ctx, "<eval>", src)
}

// EvalFile compiles and runs src, using name in stack traces.
func (r *Runtime) EvalFile(name, src string) (Value, error) {
	return r.EvalFileContext(context.Background(), name, src)
}

// EvalFileContext is EvalFile with cancellation, as EvalContext is Eval with
// it: the script is called name in stack traces, and stops when ctx is done.
func (r *Runtime) EvalFileContext(ctx context.Context, name, src string) (Value, error) {
	return r.evalIn(ctx, nil, name, src)
}

// evalIn runs a script in a realm, or in the runtime's own when re is nil, and
// then the jobs it queued.
func (r *Runtime) evalIn(ctx context.Context, re *vm.Realm, name, src string) (result Value, err error) {
	if r.closed {
		return Value{}, ErrClosed
	}
	defer r.guard(&err)
	fn, err := r.compile(src, name)
	if err != nil {
		return Value{}, err
	}
	r.rt.SetContext(ctx)
	defer r.rt.SetContext(nil)

	var v vm.Value
	if re == nil {
		v, err = r.rt.Run(fn)
	} else {
		v, err = r.rt.RunIn(re, fn)
	}
	if err != nil {
		return Value{}, r.wrapError(err)
	}
	// Promise reactions are queued rather than run synchronously, so the queue
	// is drained before returning; otherwise a then callback registered by the
	// script would never run.
	if err := r.rt.DrainJobs(); err != nil {
		return Value{}, r.wrapError(err)
	}
	return Value{v: v, rt: r.rt}, nil
}

// compile parses and compiles source text.
func (r *Runtime) compile(src, name string) (*bytecodeFunc, error) {
	return r.compileAt(src, name, 0, 0)
}

// compileAt is compile for source placed within a larger file, whose first
// line is lineOffset lines down and columnOffset columns in.
func (r *Runtime) compileAt(src, name string, lineOffset, columnOffset int) (*bytecodeFunc, error) {
	prog, err := parser.Parse(src, parser.Options{NodeQuirks: r.nodeQuirks})
	if err != nil {
		return nil, &SyntaxError{err: err}
	}
	fn, err := compiler.Compile(prog, compiler.Options{
		Source: name, Text: src, NodeQuirks: r.nodeQuirks,
		LineOffset: lineOffset, ColumnOffset: columnOffset,
	})
	if err != nil {
		return nil, &SyntaxError{err: err}
	}
	return fn, nil
}

// Global returns the global object.
func (r *Runtime) Global() Value {
	if r.closed {
		return Value{}
	}
	return Value{v: vmObj(r.rt.Global()), rt: r.rt}
}

// Get reads a global by name.
func (r *Runtime) Get(name string) (Value, error) {
	if r.closed {
		return Value{}, ErrClosed
	}
	v, err := r.rt.GetProp(vmObj(r.rt.Global()), r.rt.Intern(name))
	if err != nil {
		return Value{}, r.wrapError(err)
	}
	return Value{v: v, rt: r.rt}, nil
}

// Set defines a global.
//
// The value is converted with the same rules as Encode, so an ordinary Go
// function, map, slice or struct can be handed to script directly.
func (r *Runtime) Set(name string, v any) error {
	if r.closed {
		return ErrClosed
	}
	val, err := r.encode(v)
	if err != nil {
		return err
	}
	return r.wrapError(r.rt.DefineProp(r.rt.Global(), r.rt.Intern(name), val))
}

// DetachArrayBuffer releases an ArrayBuffer's storage, as transferring it to
// another owner does.
//
// Every view over the buffer then throws on access, which is what makes the
// transfer safe: the previous owner cannot keep reading through a view it made
// earlier. A host implementing structured cloning needs exactly this.
func (r *Runtime) DetachArrayBuffer(v Value) error {
	if r.closed {
		return ErrClosed
	}
	return r.wrapError(r.rt.DetachArrayBuffer(v.v))
}

// AbstractModuleSource returns %AbstractModuleSource%, the abstract
// constructor that module source objects inherit from. It is not a property
// of the global object, so a host that defines a kind of module with a source
// -- WebAssembly, say -- reaches it here to extend it. JavaScript modules have
// no source: importing one's is a SyntaxError.
func (r *Runtime) AbstractModuleSource() Value {
	if r.closed {
		return Value{}
	}
	return Value{v: vmObj(r.rt.AbstractModuleSource()), rt: r.rt}
}

// wrapError converts an engine error into the public form.
func (r *Runtime) wrapError(err error) error {
	if err == nil {
		return nil
	}
	// A cancelled context surfaces as itself so that errors.Is works.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("quickjs: execution interrupted: %w", err)
	}
	return wrapThrown(r.rt, err)
}

// SetClock installs the source of the current time that Date and Date.now read.
//
// A sandboxed runtime should not be able to read the wall clock unless the host
// allows it, and a test should be able to pin time, so the clock is injectable.
// Passing nil restores the process clock.
func (r *Runtime) SetClock(fn func() time.Time) {
	if r.closed {
		return
	}
	r.rt.SetClock(fn)
}

// SetTimeZone installs the zone that local-time Date methods use. A location
// is taken by its name from the time zone data bundled with Intl, rather
// than from the host's possibly different copy, and one whose name that data
// does not know by its offset. Passing nil restores the host's zone.
func (r *Runtime) SetTimeZone(loc *time.Location) {
	if r.closed {
		return
	}
	r.rt.SetTimeZone(loc)
}

// SetLocale installs the language a script means when it formats something
// without saying which, as WithLocale does. Passing an empty tag restores the
// language the machine is set to.
func (r *Runtime) SetLocale(tag string) {
	if r.closed {
		return
	}
	r.rt.SetLocale(tag)
}

// Locale reports the language this runtime formats in when a script does not
// say which.
func (r *Runtime) Locale() string {
	if r.closed {
		return ""
	}
	return r.rt.Locale()
}

// ModuleLoader resolves a module specifier to its source.
//
// specifier is the text of the import, and referrer is the module that
// contains it, empty for the entry point. The returned name is what the module
// is cached under, so a loader that resolves two specifiers to the same module
// must return the same name for both.
//
// A runtime with no loader rejects every import. That is the default, because
// the engine has no filesystem access of its own and should not acquire any
// implicitly.
type ModuleLoader func(specifier, referrer string) (source string, resolved string, err error)

// SetModuleLoader installs the loader used to resolve imports.
func (r *Runtime) SetModuleLoader(fn ModuleLoader) {
	if r.closed {
		return
	}
	r.rt.SetModuleLoader(vm.ModuleLoader(fn))
	// The compiler lives outside the vm package, so the runtime is given a
	// callback rather than importing it.
	r.rt.SetModuleCompiler(func(specifier, source string) (*vm.Module, error) {
		return r.compileAndRegisterModule(specifier, source)
	})
}

// compileAndRegisterModule parses, compiles and registers module source.
func (r *Runtime) compileAndRegisterModule(specifier, source string) (*vm.Module, error) {
	prog, err := parser.Parse(source, parser.Options{Module: true, NodeQuirks: r.nodeQuirks})
	if err != nil {
		return nil, &SyntaxError{err: err}
	}
	fn, info, err := compiler.CompileModule(prog, compiler.Options{
		Source: specifier, Text: source, NodeQuirks: r.nodeQuirks,
	})
	if err != nil {
		return nil, &SyntaxError{err: err}
	}
	reqs := make([]vm.ModuleImportRequest, len(info.Imports))
	for i, imp := range info.Imports {
		reqs[i] = vm.ModuleImportRequest{
			Specifier: imp.Specifier,
			Local:     imp.Local,
			Imported:  imp.Imported,
			Namespace: imp.Namespace,
			IsDefault: imp.IsDefault,
		}
	}
	return r.rt.LoadModule(specifier, vm.ModuleShape{
		Body:        fn,
		Init:        info.Init,
		Imports:     reqs,
		Exports:     info.Exports,
		StarExports: info.StarExports,
		Requests:    info.Requests,
	})
}

// EvalModule compiles and runs source as an ECMAScript module.
//
// Imports are resolved through the loader installed with SetModuleLoader; a
// runtime without one rejects any import. The returned value is the module's
// namespace, through which its exports can be read.
func (r *Runtime) EvalModule(specifier, source string) (Value, error) {
	return r.EvalModuleContext(context.Background(), specifier, source)
}

// EvalModuleContext is EvalModule with cancellation.
//
// A module can loop forever just as a script can, so a host running untrusted
// modules needs the same bound EvalContext gives it.
func (r *Runtime) EvalModuleContext(ctx context.Context, specifier, source string) (v Value, err error) {
	if r.closed {
		return Value{}, ErrClosed
	}
	if r.rt == nil {
		return Value{}, ErrClosed
	}
	defer r.guard(&err)
	r.rt.SetContext(ctx)
	defer r.rt.SetContext(nil)

	// The compiler callback is needed even without a loader, so that the entry
	// point itself can be compiled.
	r.rt.SetModuleCompiler(func(spec, src string) (*vm.Module, error) {
		return r.compileAndRegisterModule(spec, src)
	})

	mod, err := r.compileAndRegisterModule(specifier, source)
	if err != nil {
		return Value{}, err
	}
	if err := r.rt.Link(mod); err != nil {
		return Value{}, r.wrapError(err)
	}
	// Evaluation hands back a promise for the graph. The jobs are drained
	// here, which is what makes a module that awaits something already settled
	// finish before this returns; one waiting on the host stays pending, and
	// its failure -- if any -- surfaces as a rejection rather than being lost.
	done, err := r.rt.EvaluateModule(mod)
	if err != nil {
		return Value{}, r.wrapError(err)
	}
	if err := r.rt.DrainJobs(); err != nil {
		return Value{}, r.wrapError(err)
	}
	if err := r.rt.ModuleResult(done); err != nil {
		return Value{}, r.wrapError(err)
	}
	ns, err := r.rt.ModuleNamespace(mod)
	if err != nil {
		return Value{}, r.wrapError(err)
	}
	return Value{v: vmObj(ns), rt: r.rt}, nil
}

// WithoutCodeGeneration disables eval and the Function constructor.
//
// Neither grants a script any capability it does not already have — code it
// could eval, it could also write inline — but both defeat review of the source
// a host is about to run, which matters when the source is audited before use.
func WithoutCodeGeneration() Option {
	return func(c *config) { c.noCodeGeneration = true }
}

// installCodeGeneration gives the runtime eval and the Function constructor.
//
// They live here rather than in the vm package because they need the parser and
// compiler, which that package deliberately does not import.
func (r *Runtime) installCodeGeneration() {
	r.rt.SetEvaluator(func(source string, req vm.EvalRequest) (*bytecode.Function, error) {
		popts := parser.Options{NodeQuirks: r.nodeQuirks}
		copts := compiler.Options{
			Source: "<eval>", Text: source, NodeQuirks: r.nodeQuirks,
			// Whatever eval declares on the global object is configurable,
			// unlike what a script declares: the evaluated code could have
			// declared it anywhere, so nothing should be able to rely on it.
			EvalConfigurable: true,
			EvalOwnVarScope:  true,
		}
		if req.Direct {
			// A direct eval is inside its caller: it inherits the strictness,
			// may use the caller's `super` and `new.target`, and resolves the
			// caller's bindings.
			popts.Strict = req.Scope.Strict
			popts.AllowSuperProp = req.Scope.AllowSuperProp
			popts.AllowSuperCall = req.Scope.AllowSuperCall
			popts.AllowNewTarget = req.Scope.AllowNewTarget
			copts.EvalScope = req.Scope.Bindings
			copts.EvalWithDepth = req.Scope.WithDepth
			copts.PrivateNames = req.Scope.PrivateNames
			copts.ArgumentNames = req.Scope.ArgumentNames
			copts.InFieldInit = req.Scope.InFieldInit
			copts.AllowSuperProp = req.Scope.AllowSuperProp
			copts.AllowSuperCall = req.Scope.AllowSuperCall
			copts.AllowNewTarget = req.Scope.AllowNewTarget
			copts.EvalVarScopeIsGlobal = req.Scope.VarScopeIsGlobal
		}
		prog, err := parser.Parse(source, popts)
		if err != nil {
			return nil, &SyntaxError{err: err}
		}
		fn, err := compiler.Compile(prog, copts)
		if err != nil {
			return nil, &SyntaxError{err: err}
		}
		return fn, nil
	})
}
