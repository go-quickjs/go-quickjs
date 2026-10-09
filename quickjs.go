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
	"time"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
	"github.com/go-quickjs/go-quickjs/internal/hostaccess"
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
	// ctx is the runtime's lifetime, which Close ends.
	ctx    context.Context
	cancel context.CancelFunc
	// onClose is what Close runs, newest last; see OnClose.
	onClose []*func()
	// nodeQuirks is WithNodeQuirks, which the parser and compiler are told.
	nodeQuirks bool
	// moduleFetchLimit is WithModuleFetchLimit, which an AsyncModuleLoader
	// is installed with.
	moduleFetchLimit int
	// noCodeGeneration is WithoutCodeGeneration, which node:vm respects too.
	noCodeGeneration bool
	// debug is WithDebugger: everything the runtime compiles is compiled for
	// a debugger.
	debug bool
	// seqs are the Go sequences script has iterators over that have not
	// ended, which Close stops; seqDropped carries the stop of one whose
	// iterator was collected to the runtime's goroutine. See seq.go.
	seqs       map[*seqIter]struct{}
	seqDropped *AsyncWork
}

// Option configures a Runtime.
type Option func(*config)

type config struct {
	moduleFetchLimit int
	memoryLimit      int64
	stackSize        int
	maxCallDepth     int
	locale           string
	nodeQuirks       bool
	noCodeGeneration bool
	debug            bool
	sourceMaps       bool
	sourceMapLoader  SourceMapLoader
}

// WithMemoryLimit caps the memory a script may hold, beyond what the
// runtime's built-ins take. A script that exceeds it is stopped, and the
// Eval, call or job running it returns ErrMemoryLimit: like a cancelled
// context, it is not an exception the script can catch.
//
// The measure is the runtime's own heap -- its objects, strings and buffers
// -- which it walks from its roots when the process has allocated a quarter
// of the limit since it last did, and whenever what the engine has made
// for the script since would take it over: an allocation whose size the
// script chose is charged before it is made, a join, a template, JSON text
// or a spread as it is written, and what a built-in returns once it is
// made.
//
// What is limited is what the script holds -- what it can still reach --
// not what it has allocated over its life, which may be any amount, nor what
// the process takes from the system. Memory the script has let go of is not
// counted, though Go has not yet collected it, and Go lets its heap grow
// past what is live before it collects, so a process running a script near
// its limit uses more than the limit: about one and a half to two times it
// with Go's default collector settings. Size a host for that, or bound the
// process as a whole with GOMEMLIMIT (runtime/debug.SetMemoryLimit).
//
// It is an estimate, within 15% of Go's live heap for each kind of value a
// script can hold: what a host holds for the script, and what Go closures
// inside the engine capture, is not counted. A runtime with a limit is a
// little slower to allocate.
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

// WithModuleFetchLimit bounds how many modules an AsyncModuleLoader is asked
// for at once: across every import the runtime is loading, not for each
// graph. A request beyond the limit waits its turn, in the order it was made,
// and goes out when a call comes back. It is 8 unless set, and a limit below
// 1 is 1 -- one module at a time. A ModuleLoader answers one request at a
// time, and is not bounded.
func WithModuleFetchLimit(n int) Option {
	return func(c *config) { c.moduleFetchLimit = max(n, 1) }
}

// defaultModuleFetchLimit is WithModuleFetchLimit's limit unless set.
const defaultModuleFetchLimit = 8

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
// functions first; and strict code assigning to a global name asks a proxy on
// the global object's chain nothing first, taking it to have the name, where
// the standard asks its has trap; and Iterator.prototype.take and drop take a
// finite count past 2^53 - 1, which the standard refuses; and a sloppy
// function's caller and arguments say what called it and with what, as every
// engine's accessors on Function.prototype do, where the standard has its
// %ThrowTypeError%, which throws; and Date.prototype.setYear reads the date's
// time value after converting the year, where the standard reads it first.
// Intl.DateTimeFormat
// refuses, with V8's RangeError, a Temporal plain date whose midnight, or a
// plain date-time, is past the instants a Date can hold, which the standard
// formats; and Intl.Locale answers a firstDayOfWeek keyword with no value as
// "true", where the standard answers ""; and Intl.Locale's maximize keeps a
// locale that names a language, a script and a region as it is, as ICU
// does, though the script is "Zzzz" or the region "ZZ", which the standard
// fills in: "en-Zzzz-US" rather than "en-Latn-US".
//
// Under it, as ICU does, Intl.NumberFormat writes an accounting amount with
// signDisplay "never" by the plain currency pattern, Norwegian -1 euro
// "1,00 €" where the standard writes "€ 1,00"; writes a currency in the
// format of the currency of the locale's region where that has one of its
// own, US dollars in en-DE "-US$1,234.50" rather than "-1.234,50 US$";
// and rounds a number to an increment other than 1 or 5 from ICU's fast
// reading of the double, which past about sixteen digits is not the double's
// own decimal -- though not a numeric string, which was never a double, nor
// formatRange's ends, which ICU's range formatter reads accurately; and
// writes a range whose ends are the same double but not the same number, a
// BigInt or a numeric string past an int64 among them, as the first marked
// approximate, "~0.1" where the standard writes "0.1–0.10000000000000000001".
// Intl.RelativeTimeFormat with numeric "auto" names in words any
// offset within 0.005 of a whole one from -2 to 2, 1.004 days "tomorrow"
// where the standard writes "in 1.004 days". Intl.ListFormat's formatToParts
// leaves an empty item out, joining the literals either side of it.
// Intl.DateTimeFormat makes a time style in a locale's -u-hc cycle even where
// hour12 overrode it, "02:12:47 PM" rather than "2:12:47 PM". Intl.Collator
// compares two strings from after the prefix they share, as ICU's does, so
// that with numeric Arabic-Indic 15 sorts after 100. And the host's zone, where
// nothing names it, is ICU's guess from the C library's abbreviations, or
// Etc/Unknown on Windows, where the standard takes the host's offset.
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
		Debug:        c.debug,
	}), nodeQuirks: c.nodeQuirks, noCodeGeneration: c.noCodeGeneration,
		moduleFetchLimit: c.moduleFetchLimit, debug: c.debug}
	if c.moduleFetchLimit == 0 {
		r.moduleFetchLimit = defaultModuleFetchLimit
	}
	r.rt.Host = r
	r.ctx, r.cancel = context.WithCancel(context.Background())
	if !c.noCodeGeneration {
		r.installCodeGeneration()
	}
	if c.debug {
		// A debugger evaluates code in a paused frame whether or not the
		// script may generate code from strings.
		r.rt.SetDebugEvaluator(r.compileEval)
	}
	if c.sourceMaps {
		r.installSourceMaps(c.sourceMapLoader)
	}
	return r
}

// Close releases the runtime. Using a Runtime after Close returns ErrClosed
// from every method.
//
// It may be called from inside the runtime's own script -- by a Go function
// the script called, as process.exit is -- and then stops that script: it
// runs no further than its next call or loop iteration, nothing it has can
// catch that, and the call that ran it returns ErrClosed. Like every other
// method, Close is for the runtime's own goroutine; a host that must end a
// runtime from another cancels the context it runs under.
func (r *Runtime) Close() error {
	r.closed = true
	rt := r.rt
	if rt != nil {
		if rt.Running() {
			rt.Halt(ErrClosed)
		}
		rt.Close()
		// What was posted to an AsyncWork and has not run is told now.
		rt.CloseHostJobs()
	}
	// The runtime's work stops, and then what the host holds is released,
	// as a node worker's handles are closed and its cleanup hooks run.
	if r.cancel != nil {
		r.cancel()
	}
	hooks := r.onClose
	r.onClose = nil
	for i := len(hooks) - 1; i >= 0; i-- {
		if hooks[i] != nil {
			(*hooks[i])()
		}
	}
	if rt != nil {
		rt.ReleaseClosed()
	}
	r.rt = nil
	return nil
}

// OnClose has fn run when the runtime is closed, which is where a host
// releases what it holds for the script: a file, a connection, a goroutine
// to wait for. Close runs the hooks on its own goroutine before it returns,
// the newest first, after the script has stopped, what was posted to an
// AsyncWork has been told, and the runtime's Context has been cancelled -- as
// a node worker runs its cleanup hooks when it ends, process.exit included.
// The runtime is closed when they run: they touch nothing of it.
//
// It returns what removes the hook, for a resource released before then. On
// a closed runtime, fn runs at once. Both are called on the runtime's
// goroutine, as every Runtime method is.
func (r *Runtime) OnClose(fn func()) (remove func()) {
	if r.closed {
		fn()
		return func() {}
	}
	p := &fn
	r.onClose = append(r.onClose, p)
	return func() {
		for i, h := range r.onClose {
			if h == p {
				r.onClose[i] = nil
				return
			}
		}
	}
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
	if r.cancel != nil {
		r.cancel()
	}
	if r.rt != nil {
		// What was posted to an AsyncWork and has not run -- behind the
		// function that panicked, perhaps -- is told the runtime is closed.
		r.rt.CloseHostJobs()
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

// evalInternal evaluates a script of this module's own -- the standard
// library's, qjs's -- compiled as though the runtime had no debugger, so
// that one neither lists it nor stops in it.
func (r *Runtime) evalInternal(name, src string) (result Value, err error) {
	if r.closed {
		return Value{}, ErrClosed
	}
	defer r.guard(&err)
	fn, err := compileScript(src, name, 0, 0, false, r.nodeQuirks, false)
	if err != nil {
		return Value{}, err
	}
	return r.runIn(context.Background(), nil, fn)
}

func init() {
	hostaccess.VM = func(rt any) *vm.Runtime { return rt.(*Runtime).rt }
	hostaccess.Wrap = func(rt any, v vm.Value) any { return Value{v: v, rt: rt.(*Runtime).rt} }
	hostaccess.Unwrap = func(v any) vm.Value { return v.(Value).v }
	hostaccess.EvalInternal = func(rt any, name, src string) (any, error) {
		return rt.(*Runtime).evalInternal(name, src)
	}
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
	return r.runIn(ctx, re, fn)
}

// runIn runs compiled code in a realm, or in the runtime's own when re is
// nil, and then the jobs it queued.
func (r *Runtime) runIn(ctx context.Context, re *vm.Realm, fn *bytecodeFunc) (result Value, err error) {
	rt := r.rt
	outer := rt.Context()
	nested, leave := r.enter(ctx)
	defer leave()

	var v vm.Value
	if re == nil {
		v, err = rt.Run(fn)
	} else {
		v, err = rt.RunIn(re, fn)
	}
	if r.closed {
		// A Go function the script called closed the Runtime, which has
		// nothing left to run and nothing to hand back. Its stack, which the
		// script was still running on when it was closed, is free now.
		if !nested {
			rt.ReleaseClosed()
		}
		return Value{}, ErrClosed
	}
	if err != nil {
		endNested(rt, nested, ctx, outer)
		return Value{}, r.wrapError(err)
	}
	// Promise reactions are queued rather than run synchronously, so the queue
	// is drained before returning; otherwise a then callback registered by the
	// script would never run. From inside a running script it is left for
	// that script's turn to drain, as its own reactions are.
	if !nested {
		if err := r.rt.DrainJobs(); err != nil {
			return Value{}, r.wrapError(err)
		}
	}
	return Value{v: v, rt: r.rt}, nil
}

// endNested ends the stop of a call made from inside a running script --
// a host function evaluating more, or calling back -- that its own context
// stopped: the call stops, with an error wrapping that context's, and the
// script it was made from runs on, as node:vm's timeout leaves the code that
// set it running. A stop of the script's own -- its context, an abort --
// stops everything, as it always does.
func endNested(rt *vm.Runtime, nested bool, ctx, outer context.Context) {
	if nested && ctx != nil && ctx.Err() != nil && (outer == nil || outer.Err() == nil) {
		rt.EndNestedStop()
	}
}

// enter sets the context a call runs under, and returns whether the call is
// made from inside a running script and what restores things after it.
//
// A call from inside a script -- a host function evaluating more -- is part
// of that script: it runs under the script's context as well as its own, so
// the script's deadline still bounds it, and the script's is what is in
// force again after it.
func (r *Runtime) enter(ctx context.Context) (nested bool, leave func()) {
	// The engine is held here rather than read again on the way out: a Go
	// function the script called may have closed the Runtime, which lets go
	// of it.
	rt := r.rt
	if !rt.Running() {
		// Whatever was in force before -- a loop's, between its tasks -- is
		// again after, and so is the async context: a script that set one
		// for the rest of its turn leaves it there.
		prev, prevAsync := rt.Context(), rt.AsyncContext()
		rt.SetContext(ctx)
		return false, func() {
			rt.SetContext(prev)
			rt.SetAsyncContext(prevAsync)
		}
	}
	outer := rt.Context()
	run, cancel := ctx, func() {}
	switch {
	case outer == nil:
	case ctx == nil || ctx.Done() == nil:
		run = outer
	default:
		var c context.Context
		c, cancel = context.WithCancel(ctx)
		stop := context.AfterFunc(outer, cancel)
		run = c
		prev := cancel
		cancel = func() { stop(); prev() }
	}
	rt.SetContext(run)
	return true, func() {
		cancel()
		rt.SetContext(outer)
	}
}

// compile parses and compiles source text.
func (r *Runtime) compile(src, name string) (*bytecodeFunc, error) {
	return r.compileAt(src, name, 0, 0)
}

// compileAt is compile for source placed within a larger file, whose first
// line is lineOffset lines down and columnOffset columns in.
func (r *Runtime) compileAt(src, name string, lineOffset, columnOffset int) (*bytecodeFunc, error) {
	return compileScript(src, name, lineOffset, columnOffset, false, r.nodeQuirks, r.debug)
}

// compileScript parses and compiles a script, strict throughout if strict is
// set, and as V8 has it where it departs from the standard if nodeQuirks is.
// It depends on no runtime: what it returns any runtime may run, but for
// code compiled for a debugger, which only the runtime it is for may.
func compileScript(src, name string, lineOffset, columnOffset int, strict, nodeQuirks, debug bool) (*bytecodeFunc, error) {
	prog, err := parser.Parse(src, parser.Options{Strict: strict, NodeQuirks: nodeQuirks})
	if err != nil {
		return nil, newSyntaxError(err, name, lineOffset, columnOffset)
	}
	fn, err := compiler.Compile(prog, compiler.Options{
		Source: name, Text: src, NodeQuirks: nodeQuirks, Debug: debug,
		LineOffset: lineOffset, ColumnOffset: columnOffset,
	})
	if err != nil {
		return nil, newSyntaxError(err, name, lineOffset, columnOffset)
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
// specifier is the text of the import, and referrer is the module or script
// the import is written in, for a static import and an import() alike: the
// name a module was loaded under, the name a script was compiled under
// (EvalFile, Compile), or empty for code that has none (Eval). An import() in
// code an eval or the Function constructor compiled has the referrer of the
// code that called them. The returned name is what the module is cached under,
// so a loader that resolves two specifiers to the same module must return the
// same name for both.
//
// A loader's error is what the import throws, if it is one a Go function
// would throw: what Throw, ThrowError and the like return -- an error with a
// code of node's, say -- or a *Error or *SyntaxError the loader passes on. Any
// other error is a TypeError saying the module cannot be resolved, through
// which errors.Is and errors.As find the loader's error.
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
	r.installModuleHooks()
}

// ModuleRequest is what an AsyncModuleLoader is asked for.
type ModuleRequest struct {
	// Specifier is the text of the import.
	Specifier string
	// Referrer is the name of the module or script the import is written in,
	// as a ModuleLoader's referrer is: empty for code that has none.
	Referrer string
}

// LoadedModule is what an AsyncModuleLoader answers with.
type LoadedModule struct {
	// Source is the module's source: JavaScript, or the text of a module of
	// a type -- JSON, say -- that the import's attributes asked for. It is
	// ignored for a name a module is defined under already, a synthetic one
	// the loader defined, say.
	Source string
	// Resolved is the name the module is cached under, and what imports in
	// it are resolved against: a loader that resolves two requests to the
	// same module answers with the same name for both.
	Resolved string
}

// AsyncModuleLoader is a ModuleLoader that need not answer before it returns:
// it calls done once -- from any goroutine, then or later -- with the module,
// or an error, and a second call does nothing. Its request and its answer
// are structs, so that what a loader is told and may say can grow without
// changing what it is. It is called on the
// runtime's goroutine, so what it does with the runtime -- define a synthetic
// module, say -- it does before it returns; done must not wait for the
// runtime, which may be waiting for it.
//
// ctx ends when the module is no longer wanted: the runtime has closed,
// another module of the graph failed to load, or the call waiting for the
// graph -- EvalModuleContext, RequireModule -- gave up as its context ended. A
// loader that stops then need not call done; its place under
// WithModuleFetchLimit is given back either way. An import() outlives the
// call that ran it, and so does its fetch: ctx is cut from the runtime's
// lifetime, not from that call's context.
//
// Its error is what a ModuleLoader's is. One made for script -- by Throw,
// ThrowError and the like -- is made on the runtime's goroutine too, before
// the loader returns, and may be handed to done later; a Go error may come
// from anywhere.
type AsyncModuleLoader func(ctx context.Context, req ModuleRequest, done func(LoadedModule, error))

// SetAsyncModuleLoader installs an asynchronous loader, in place of any
// loader. The modules a graph needs are asked for at once -- as many at a
// time as WithModuleFetchLimit allows -- and each one's imports as it
// arrives. An import() fetches without holding up the runtime: the script,
// its jobs and the host's work run on, and the import goes on when the last of
// its graph has arrived. EvalModule and RequireModule return their module,
// and so wait for its graph, running nothing else meanwhile, as they do while
// a ModuleLoader reads; so does linking a graph whose modules have not been
// fetched. A nil loader removes it.
func (r *Runtime) SetAsyncModuleLoader(fn AsyncModuleLoader) {
	if r.closed {
		return
	}
	var loader vm.AsyncModuleLoader
	if fn != nil {
		loader = func(ctx context.Context, specifier, referrer string, done func(source, resolved string, err error)) {
			fn(ctx, ModuleRequest{Specifier: specifier, Referrer: referrer}, func(m LoadedModule, err error) {
				done(m.Source, m.Resolved, err)
			})
		}
	}
	r.rt.SetAsyncModuleLoader(loader, r.moduleFetchLimit, r.Context())
	r.installModuleHooks()
}

// installModuleHooks gives the engine what a loader needs of this package:
// the compiler, which lives outside the vm package, and what makes a loader's
// error the exception it stands for.
func (r *Runtime) installModuleHooks() {
	r.rt.SetModuleCompiler(func(specifier, source string) (*vm.Module, error) {
		return r.compileAndRegisterModule(specifier, source)
	})
	rt := r.rt
	rt.SetLoaderErrorHook(func(err error) error {
		// An exception the loader passes on -- from script it ran, or a
		// SyntaxError from source it compiled -- is thrown as it is.
		var jsErr *Error
		var synErr *SyntaxError
		if errors.As(err, &jsErr) || errors.As(err, &synErr) {
			return thrownGoError(rt, err)
		}
		return err
	})
}

// compileAndRegisterModule parses, compiles and registers module source.
func (r *Runtime) compileAndRegisterModule(specifier, source string) (*vm.Module, error) {
	if r.closed {
		// A loader closed the runtime as it was asked for a module.
		return nil, ErrClosed
	}
	prog, err := parser.Parse(source, parser.Options{Module: true, NodeQuirks: r.nodeQuirks})
	if err != nil {
		return nil, newSyntaxError(err, specifier, 0, 0)
	}
	fn, info, err := compiler.CompileModule(prog, compiler.Options{
		Source: specifier, Text: source, NodeQuirks: r.nodeQuirks, Debug: r.debug,
	})
	if err != nil {
		return nil, newSyntaxError(err, specifier, 0, 0)
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
	_, leave := r.enter(ctx)
	defer leave()

	// The compiler callback is needed even without a loader, so that the entry
	// point itself can be compiled.
	r.rt.SetModuleCompiler(func(spec, src string) (*vm.Module, error) {
		return r.compileAndRegisterModule(spec, src)
	})

	// The engine is held here: a loader, or a module, may close the Runtime,
	// which lets go of it.
	rt := r.rt
	mod, err := r.compileAndRegisterModule(specifier, source)
	if err != nil {
		return Value{}, err
	}
	err = rt.Link(mod)
	if r.closed {
		// A loader closed the Runtime, which halted the linking.
		rt.ReleaseClosed()
		return Value{}, ErrClosed
	}
	if err != nil {
		return Value{}, r.wrapError(err)
	}
	// Evaluation hands back a promise for the graph. The jobs are drained
	// here, which is what makes a module that awaits something already settled
	// finish before this returns; one waiting on the host stays pending, and
	// its failure -- if any -- surfaces as a rejection rather than being lost.
	done, err := rt.EvaluateModule(mod)
	if r.closed {
		// The module closed the Runtime, which stopped it; the stack it ran
		// on is free now.
		rt.ReleaseClosed()
		return Value{}, ErrClosed
	}
	if err != nil {
		return Value{}, r.wrapError(err)
	}
	err = rt.DrainJobs()
	if r.closed {
		rt.ReleaseClosed()
		return Value{}, ErrClosed
	}
	if err != nil {
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

// WithDebugger makes a runtime a debugger can attach to. Everything it
// compiles -- scripts, modules, a Program it runs, the code of an eval or a
// Function call -- is compiled for one: each statement is a place the code
// can stop, at a breakpoint or a step, and records the bindings in scope
// there, which a debugger shows and evaluates code against. A debugger
// statement stops there too, once a debugger is attached.
//
// It is for development. The runtime runs its code in the interpreter,
// without the tier that turns a function's bytecode into closures, and a
// statement costs a test even with no debugger attached; and a debugger can
// read and change anything the code can. A runtime made without it compiles
// none of this, and pays nothing for it.
func WithDebugger() Option {
	return func(c *config) { c.debug = true }
}

// installCodeGeneration gives the runtime eval and the Function constructor.
//
// They live here rather than in the vm package because they need the parser and
// compiler, which that package deliberately does not import.
func (r *Runtime) installCodeGeneration() {
	r.rt.SetEvaluator(r.compileEval)
}

// compileEval compiles the code of an eval or a Function call, or code a
// debugger evaluates in a frame.
func (r *Runtime) compileEval(source string, req vm.EvalRequest) (*bytecode.Function, error) {
	popts := parser.Options{NodeQuirks: r.nodeQuirks}
	copts := compiler.Options{
		Source: "<eval>", Text: source, NodeQuirks: r.nodeQuirks, Debug: r.debug,
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
		return nil, newSyntaxError(err, "", 0, 0)
	}
	fn, err := compiler.Compile(prog, copts)
	if err != nil {
		return nil, newSyntaxError(err, "", 0, 0)
	}
	return fn, nil
}
