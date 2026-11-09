package wasmjs

import (
	"context"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/vm"
	"github.com/go-quickjs/go-quickjs/wasm/internal/engine"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
)

// Functions crossing between wasm and JavaScript: exported functions, which
// script calls into wasm through, and host functions, which wasm calls
// script through.

// funcSlot is what an exported function object carries.
type funcSlot struct {
	fn *engine.Function
	// promising marks a function WebAssembly.promising made, which runs fn
	// on a stack of its own and returns a promise.
	promising bool
}

// exportedFunction returns the wasm function v is the exported function of,
// or nil.
func (a *api) exportedFunction(v vm.Value) *engine.Function {
	if s, ok := vm.HostSlot(a.rt, v).(*funcSlot); ok && !s.promising {
		return s.fn
	}
	return nil
}

// functionObject returns the exported function of f, made the first time it
// is asked for: a function named by f's index, of length its parameter
// count, which is not a constructor.
func (a *api) functionObject(f *engine.Function) *vm.Object {
	if o := a.wrappers.get(unsafe.Pointer(f)); o != nil {
		return o
	}
	fn := a.rt.NewFunc(strconv.FormatUint(uint64(f.Index), 10), len(f.Sig.Params),
		func(rt *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
			return a.callExport(f, args)
		})
	vm.HostSetSlot(a.rt, fn, &funcSlot{fn: f})
	a.wrappers.put(unsafe.Pointer(f), fn)
	return fn
}

// callExport calls wasm function f with JavaScript arguments, returning its
// result: undefined for none, the value of one, an array of several.
func (a *api) callExport(f *engine.Function, args []vm.Value) (vm.Value, error) {
	sig := f.Sig
	if !jsCompatible(sig) {
		return vm.Undefined, a.rt.ThrowTypeError("%s", msgJSType)
	}
	vals := make([]engine.Val, len(sig.Params))
	for i, t := range sig.Params {
		v, err := a.toWasm(arg(args, i), t)
		if err != nil {
			return vm.Undefined, err
		}
		vals[i] = v
	}
	res, err := a.invoke(f, vals)
	if err != nil {
		return vm.Undefined, err
	}
	return a.results(res, sig.Results)
}

// results converts a call's results to JavaScript.
func (a *api) results(res []engine.Val, types []module.ValueType) (vm.Value, error) {
	switch len(types) {
	case 0:
		return vm.Undefined, nil
	case 1:
		return a.toJS(res[0], types[0])
	}
	out := make([]vm.Value, len(types))
	for i, t := range types {
		v, err := a.toJS(res[i], t)
		if err != nil {
			return vm.Undefined, err
		}
		out[i] = v
	}
	return vm.Obj(a.rt.NewArrayOf(out)), nil
}

// invoke runs f, turning what stopped it into what script sees.
func (a *api) invoke(f *engine.Function, args []engine.Val) ([]engine.Val, error) {
	if a.jspi != nil && a.jspi.current != nil {
		c := a.jspi.current
		// A call from JavaScript a suspendable stack's import made: it
		// runs here, on the JavaScript side, with nothing of the
		// suspendable stack under it.
		c.jsFrames++
		defer func() { c.jsFrames-- }()
	}
	res, err := engine.Invoke(runtimeContext{a.rt}, f, args)
	if err != nil {
		return nil, a.runError(err)
	}
	return res, nil
}

// runtimeContext is the context wasm runs under: done when the host has
// stopped the runtime, which the interpreter asks every few thousand
// instructions.
type runtimeContext struct{ r *vm.Runtime }

func (runtimeContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (runtimeContext) Done() <-chan struct{}       { return nil }
func (c runtimeContext) Err() error                { return vm.HostCheckInterrupt(c.r) }
func (runtimeContext) Value(any) any               { return nil }

var _ context.Context = runtimeContext{}

// uncatchable marks an error a trap or a stack overflow made, which a wasm
// handler does not catch, even when it reaches wasm through JavaScript.
type uncatchable struct{}

// runError is what an error wasm stopped with throws: a trap a
// RuntimeError, exhausting the stack a RangeError, a wasm exception the
// value it stands for, and anything else -- an exception of a JavaScript
// function wasm called, the host stopping the script -- itself.
func (a *api) runError(err error) error {
	switch e := err.(type) {
	case *engine.Trap:
		return a.trapError("", e)
	case *engine.Exception:
		return a.throwException(e)
	}
	return err
}

// trapError throws the error of a trap: a RuntimeError of its message, or
// the RangeError of the stack's exhaustion, prefixed as t names if a trap
// of the API's own.
func (a *api) trapError(prefix string, e *engine.Trap) error {
	var o *vm.Object
	if e.Code == engine.TrapStackOverflow {
		o = a.rt.NewError("RangeError", "Maximum call stack size exceeded").Object()
	} else {
		o = vm.HostNewError(a.rt, a.runtimeErrorProto, prefix+e.Error())
	}
	vm.HostSetSlot(a.rt, o, uncatchable{})
	return a.rt.ThrowValue(vm.Obj(o))
}

// throwException throws the JavaScript value of a wasm exception: what
// JavaScript threw, for one that came from it; the payload of one thrown
// with JSTag; otherwise its WebAssembly.Exception.
func (a *api) throwException(e *engine.Exception) error {
	if err, ok := e.Host.(error); ok {
		return err
	}
	if e.Tag == a.jsTag {
		return a.rt.ThrowValue(a.refToJS(e.Vals[0].R))
	}
	return a.rt.ThrowValue(vm.Obj(a.exceptionObject(e)))
}

// catchable reports whether a wasm handler may catch v, a value JavaScript
// threw: anything but the error of a trap or of the stack's exhaustion.
func (a *api) catchable(v vm.Value) bool {
	if !v.IsObject() {
		return true
	}
	if _, ok := vm.HostSlot(a.rt, v).(uncatchable); ok {
		return false
	}
	// The runtime's own stack overflow is a RangeError of this message.
	if name, err := a.rt.GetProp(v, a.rt.Intern("name")); err == nil && name.IsString() && name.String().Go() == "RangeError" {
		if msg, err := a.rt.GetProp(v, a.rt.Intern("message")); err == nil && msg.IsString() &&
			strings.EqualFold(msg.String().Go(), "Maximum call stack size exceeded") {
			return false
		}
	}
	return true
}

// wasmError is what a JavaScript function wasm called throws into wasm: a
// WebAssembly.Exception its exception, an error wasm cannot catch itself,
// and any other value an exception of JSTag carrying it.
func (a *api) wasmError(err error) error {
	v, ok := thrownValue(err)
	if !ok {
		return err
	}
	if e := a.exceptionOf(v); e != nil {
		return e
	}
	if !a.catchable(v) {
		return err
	}
	return engine.NewException(a.jsTag, []engine.Val{{R: a.hostRef(v)}}, err)
}

// hostFunction returns the host function wasm calls callable through, of
// canonical type c; index is its index in the importing module, which an
// exported function of it is named by.
func (a *api) hostFunction(callable vm.Value, c module.CanonicalIndex, index uint32) *engine.Function {
	sig := module.Canonical(c).Func
	fn := engine.NewHostFunction(c, "", func(ctx context.Context, caller *engine.Instance, args, results []engine.Val) error {
		if sc, ok := ctx.(*stackContext); ok {
			// Wasm running on a suspendable stack calls out through the
			// runtime's goroutine, which alone runs script.
			return sc.callOut(stackMsg{run: func() error { return a.callImport(callable, sig, args, results) }})
		}
		return a.callImport(callable, sig, args, results)
	})
	fn.Index = index
	return fn
}

// callImport calls callable with wasm's arguments and converts what it
// returns to results.
func (a *api) callImport(callable vm.Value, sig *module.FuncType, args, results []engine.Val) error {
	if !jsCompatible(sig) {
		return a.wasmError(a.rt.ThrowTypeError("%s", msgJSType))
	}
	jsArgs := make([]vm.Value, len(args))
	for i, t := range sig.Params {
		v, err := a.toJS(args[i], t)
		if err != nil {
			return a.wasmError(err)
		}
		jsArgs[i] = v
	}
	ret, err := a.rt.Call(callable, vm.Undefined, jsArgs)
	if err != nil {
		return a.wasmError(err)
	}
	return a.wasmError(a.returned(ret, sig.Results, results))
}

// returned converts what a JavaScript function returned to wasm's results:
// the value itself for one result, an iterable of them for several.
func (a *api) returned(ret vm.Value, types []module.ValueType, results []engine.Val) error {
	switch len(types) {
	case 0:
		return nil
	case 1:
		v, err := a.toWasm(ret, types[0])
		if err != nil {
			return err
		}
		results[0] = v
		return nil
	}
	var vals []vm.Value
	if err := a.rt.Iterate(ret, func(v vm.Value) error {
		vals = append(vals, v)
		return nil
	}); err != nil {
		return err
	}
	if len(vals) != len(types) {
		return a.rt.ThrowTypeError("multi-return length mismatch")
	}
	for i, t := range types {
		v, err := a.toWasm(vals[i], t)
		if err != nil {
			return err
		}
		results[i] = v
	}
	return nil
}
