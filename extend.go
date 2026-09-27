package quickjs

import (
	"fmt"
	"reflect"
	"sort"

	"github.com/go-quickjs/go-quickjs/internal/compiler"
	"github.com/go-quickjs/go-quickjs/internal/parser"
	"github.com/go-quickjs/go-quickjs/internal/vm"
)

// Extending the runtime.
//
// A Runtime starts with the language and nothing else: no console, no clock to
// speak of, no filesystem, no network. Everything beyond the language is
// something the host puts there, which is what makes a runtime safe to hand
// untrusted code and what makes it worth embedding at all -- the host decides
// exactly what the script can reach.
//
// There are two ways to put something there. A global is what a script finds
// without asking:
//
//	rt.Set("readFile", os.ReadFile)
//
// A module is what a script has to import, which keeps the global object clean
// and makes the dependency visible in the source that uses it:
//
//	rt.SetModule("fs", map[string]any{"readFile": os.ReadFile})
//	rt.EvalModule("main.js", `import {readFile} from "fs"; ...`)
//
// The stdlib package builds both kinds out of these, for the hosts that want
// something more familiar than a bare engine.

// SetModule installs a module whose exports come from Go.
//
// The module answers to this exact name, ahead of any module loader: a script
// that writes `import {x} from "fs"` reaches what the host registered under
// "fs" rather than a file of that name. Each value is converted as Set
// converts one, so a Go function becomes a callable export.
//
// An entry named "default" becomes the default export, so both of these work:
//
//	import fs, {readFile} from "fs"
//
// Registering a name twice replaces what it exports. A module already imported
// keeps the bindings it resolved, so a host that means to replace one should do
// it before running the code that imports it.
func (r *Runtime) SetModule(name string, exports map[string]any) error {
	if r.closed {
		return ErrClosed
	}
	names := make([]string, 0, len(exports))
	for k := range exports {
		names = append(names, k)
	}
	sort.Strings(names)

	out := make([]vm.NativeExport, 0, len(names))
	for _, k := range names {
		v, err := r.encode(exports[k])
		if err != nil {
			return err
		}
		out = append(out, vm.NativeExport{Name: k, Value: v})
	}
	r.rt.DefineNativeModule(name, out)
	return nil
}

// SetModuleValues is SetModule for exports that are already JavaScript values,
// which is what a host assembling them with NewObject and friends has.
func (r *Runtime) SetModuleValues(name string, exports map[string]Value) error {
	if r.closed {
		return ErrClosed
	}
	names := make([]string, 0, len(exports))
	for k := range exports {
		names = append(names, k)
	}
	sort.Strings(names)

	out := make([]vm.NativeExport, 0, len(names))
	for _, k := range names {
		out = append(out, vm.NativeExport{Name: k, Value: exports[k].v})
	}
	r.rt.DefineNativeModule(name, out)
	return nil
}

// CheckSyntax parses and compiles source without running it, reporting the
// first error as one.
//
// It is what a tool that checks a file does, and what a prompt uses to tell an
// unfinished line from a wrong one: the error carries the position of the
// trouble, so a caller can say whether more input would help.
func (r *Runtime) CheckSyntax(src string) error {
	_, err := r.compile(src, "<check>")
	return err
}

// CheckModuleSyntax is CheckSyntax for module source, where import and export
// are allowed and the code is strict whether it says so or not.
func (r *Runtime) CheckModuleSyntax(src string) error {
	prog, err := parser.Parse(src, parser.Options{Module: true, NodeQuirks: r.nodeQuirks})
	if err != nil {
		return &SyntaxError{err: err}
	}
	if _, _, err := compiler.CompileModule(prog, compiler.Options{
		Source: "<check>", Text: src, NodeQuirks: r.nodeQuirks,
	}); err != nil {
		return &SyntaxError{err: err}
	}
	return nil
}

// NewObject returns a new empty object.
func (r *Runtime) NewObject() Value {
	if r.closed {
		return Value{}
	}
	return Value{v: vmObj(r.rt.NewPlainObject()), rt: r.rt}
}

// NewHTMLDDA returns an object with Annex B's [[IsHTMLDDA]] slot, as the web's
// document.all has: typeof gives "undefined", it is falsy, and it is == to
// null and undefined, while it remains an object everywhere else -- to ===,
// ??, optional chaining, destructuring defaults and every built-in. It exists
// for hosts that emulate a browser's document.
//
// If call is nil the object is not callable. Otherwise call must be a Go
// function, which is converted as Set converts one and called when the object
// is. Members are added afterwards as on any other object.
func (r *Runtime) NewHTMLDDA(call any) (Value, error) {
	if r.closed {
		return Value{}, ErrClosed
	}
	if call == nil {
		o := r.rt.NewPlainObject()
		o.MarkHTMLDDA()
		return Value{v: vmObj(o), rt: r.rt}, nil
	}
	// Only a Go function makes a new object; a Value would be one a script
	// may already have seen, whose typeof must not change under it.
	fv := reflect.ValueOf(call)
	if fv.Kind() != reflect.Func || fv.IsNil() {
		return Value{}, fmt.Errorf("quickjs: NewHTMLDDA needs a Go function, not %T", call)
	}
	v, err := wrapGoFunc(r.rt, fv)
	if err != nil {
		return Value{}, err
	}
	v.Object().MarkHTMLDDA()
	return Value{v: v, rt: r.rt}, nil
}

// NewArray returns a new array holding the given values, each converted as Set
// converts one.
func (r *Runtime) NewArray(items ...any) (Value, error) {
	if r.closed {
		return Value{}, ErrClosed
	}
	vals := make([]vm.Value, len(items))
	for i, it := range items {
		v, err := r.encode(it)
		if err != nil {
			return Value{}, err
		}
		vals[i] = v
	}
	return Value{v: vmObj(r.rt.NewArrayOf(vals)), rt: r.rt}, nil
}

// NewBytes returns a Uint8Array holding a copy of b.
//
// It is what a host hands back for the contents of a file or the body of a
// response. An ordinary Go byte slice converts to an array of numbers, which is
// the right answer for a small one and the wrong one for a megabyte.
func (r *Runtime) NewBytes(b []byte) Value {
	if r.closed {
		return Value{}
	}
	return Value{v: r.rt.NewUint8ArrayOf(b), rt: r.rt}
}

// Bytes returns the bytes behind a typed array, a DataView or an ArrayBuffer,
// and whether the value was one of those.
//
// The bytes are the ones the object is looking at rather than a copy, so a host
// that keeps them keeps what the script can still write to; copy them if they
// are to outlive the call. The bytes of an immutable ArrayBuffer, which a
// script is promised never change, must not be written, and those of a
// resizable one are only its bytes until the script next resizes it.
func (v Value) Bytes() ([]byte, bool) {
	if v.rt == nil {
		return nil, false
	}
	return v.rt.Bytes(v.v)
}

// Promise is a promise a host settles, which is how a host operation that
// finishes later is handed to script.
//
// A promise cannot be cancelled, and has no lifetime of its own: the work
// that settles it belongs to the runtime, and is started with the runtime's
// Context, which Close cancels.
//
// Settling it queues the reactions rather than running them, exactly as
// settling a promise from script does: they run when the job queue is next
// drained, which Eval does before returning and which RunJobs does on demand.
// Settling twice does nothing, as in script.
type Promise struct {
	rt *Runtime
	p  *vm.HostPromise
}

// NewPromise returns a pending promise for the host to settle.
func (r *Runtime) NewPromise() *Promise {
	if r.closed {
		return &Promise{rt: r}
	}
	return &Promise{rt: r, p: r.rt.NewHostPromise()}
}

// Value is the promise itself, which is what the host hands to script.
func (p *Promise) Value() Value {
	if p.p == nil {
		return Value{}
	}
	return Value{v: p.p.Value(), rt: p.rt.rt}
}

// Resolve settles the promise with a value, converted as Set converts one. A
// promise resolved with another promise follows it.
func (p *Promise) Resolve(v any) error {
	if p.p == nil {
		return ErrClosed
	}
	val, err := p.rt.encode(v)
	if err != nil {
		return err
	}
	p.p.Resolve(val)
	return nil
}

// Reject settles the promise with a reason, converted as Set converts one.
func (p *Promise) Reject(v any) error {
	if p.p == nil {
		return ErrClosed
	}
	val, err := p.rt.encode(v)
	if err != nil {
		return err
	}
	p.p.Reject(val)
	return nil
}

// RejectError rejects the promise with an Error carrying a Go error's message,
// which is what a host operation that failed usually has to offer.
func (p *Promise) RejectError(err error) {
	if p.p == nil {
		return
	}
	p.p.RejectError(err)
}

// RunJobs runs the queued microtasks until none remain.
//
// Eval and its relatives do this before returning, so a host only needs it when
// something outside a call settles a promise -- a timer firing, a request
// finishing -- and the reactions to it have to run.
func (r *Runtime) RunJobs() (err error) {
	if r.closed {
		return ErrClosed
	}
	defer r.guard(&err)
	if err := r.rt.DrainJobs(); err != nil {
		return r.wrapError(err)
	}
	return nil
}

// EnqueueJob queues a microtask, which runs when the queue is next drained.
//
// It is what a host needs to make something happen after the current job and
// before anything else: queueMicrotask is this, and so is the callback of an
// operation that finished while script was running.
func (r *Runtime) EnqueueJob(fn func()) {
	if r.closed {
		return
	}
	r.rt.EnqueueJob(fn)
}

// HasPendingJobs reports whether any microtask is waiting to run.
func (r *Runtime) HasPendingJobs() bool {
	if r.closed {
		return false
	}
	return r.rt.HasPendingJobs()
}

// OnUnhandledRejection installs what to call for a promise that was rejected
// and that nothing was waiting for.
//
// A rejection is reported at the end of the turn it happened in, because a
// handler attached later in that turn is still in time: what makes a rejection
// unhandled is that nobody took it, not that nobody had taken it yet. A
// reported rejection is not reported again.
//
// Without a handler, an unhandled rejection is silent, which is what a host
// that means to ignore them gets by doing nothing.
func (r *Runtime) OnUnhandledRejection(fn func(reason Value)) {
	if r.closed {
		return
	}
	if fn == nil {
		r.rt.OnUnhandledRejection(nil)
		return
	}
	rt := r.rt
	r.rt.OnUnhandledRejection(func(reason vm.Value, _ vm.Value) {
		fn(Value{v: reason, rt: rt})
	})
}

// OnImportMeta installs what fills in a module's import.meta.
//
// It is called once per module, the first time that module mentions
// import.meta, with the specifier the module was loaded under and the object to
// fill in. What belongs there is the host's to decide -- the specification says
// nothing about it -- and a host that serves modules over HTTP has a different
// answer from one that reads them off a disk:
//
//	rt.OnImportMeta(func(specifier string, meta quickjs.Value) {
//	    meta.Set("url", "file://"+specifier)
//	})
func (r *Runtime) OnImportMeta(fn func(specifier string, meta Value)) {
	if r.closed {
		return
	}
	if fn == nil {
		r.rt.OnImportMeta(nil)
		return
	}
	rt := r.rt
	r.rt.OnImportMeta(func(specifier string, meta *vm.Object) {
		fn(specifier, Value{v: vmObj(meta), rt: rt})
	})
}

// Throw returns an error that raises v as a JavaScript exception when returned
// from a Go function called by script.
//
// A Go function that returns an ordinary error raises an Error carrying its
// message, which is usually what is wanted; this is for a host that needs to
// throw something in particular -- a TypeError, or a value of its own.
func (r *Runtime) Throw(v Value) error {
	if r.closed {
		return ErrClosed
	}
	return r.rt.ThrowValue(v.v)
}

// NewError builds an error object of one of the standard kinds: "Error",
// "TypeError", "RangeError" and the rest. An unknown name gives a plain Error.
func (r *Runtime) NewError(kind, message string) Value {
	if r.closed {
		return Value{}
	}
	return Value{v: r.rt.NewError(kind, message), rt: r.rt}
}
