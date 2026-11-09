package wasmjs

import (
	"context"
	"errors"
	"runtime"
	"strconv"
	"time"

	"github.com/go-quickjs/go-quickjs/internal/vm"
	"github.com/go-quickjs/go-quickjs/wasm/internal/engine"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
)

// The JS Promise Integration: WebAssembly.promising runs an export on a
// stack of its own, which an import WebAssembly.Suspending wraps can
// suspend until a promise settles, the promising call returning a promise
// of the export's result meanwhile.
//
// A promising call's wasm runs on a goroutine of its own. Script runs only
// on the runtime's goroutine, which services the wasm goroutine's calls out
// -- every import it calls, every suspension -- over channels, and is
// blocked while wasm runs: exactly one of the two runs at a time. A
// suspension leaves the wasm goroutine waiting for its reply, and the
// runtime's goroutine free to return the promise and run other script; a
// reaction to the promise the suspension waits for replies, and services
// the stack again until it next suspends or ends.

// jspiState is the namespace's state of the promising calls.
type jspiState struct {
	// current is the stack whose calls out the runtime's goroutine is
	// servicing now, nil if none.
	current *jspiStack
}

// jspiStack is a promising call's stack, as the runtime's goroutine sees
// it.
type jspiStack struct {
	a      *api
	sig    *module.FuncType
	result *vm.HostPromise
	// toJS carries the wasm goroutine's calls out and its end; toWasm the
	// replies to them.
	toJS   chan stackMsg
	toWasm chan error
	// jsFrames counts the calls into wasm script running on this stack's
	// behalf has made and that are running, which a suspension cannot
	// suspend through.
	jsFrames int
	// handle is what the reactions waiting on a suspension hold: once
	// nothing does, the stack can never resume, and its goroutine is ended.
	handle *stackHandle
}

// stackHandle is held by what may resume a stack; its collection cancels
// the stack.
type stackHandle struct{ s *jspiStack }

// stackMsg is a message from a stack's wasm goroutine.
type stackMsg struct {
	// run is an import to call on the runtime's goroutine.
	run func() error
	// suspend is a call of a suspending import.
	suspend *suspension
	// done is the end of the call, with its results or what stopped it.
	done    bool
	results []engine.Val
	err     error
}

// suspension is a call of a suspending import: the function, wasm's
// arguments and where its results go.
type suspension struct {
	callable vm.Value
	sig      *module.FuncType
	args     []engine.Val
	results  []engine.Val
}

// stackContext is the context wasm runs under on a stack's goroutine. Its
// stack's runtime is the runtime of the goroutine blocked servicing it.
type stackContext struct {
	r        *vm.Runtime
	toJS     chan stackMsg
	toWasm   chan error
	canceled chan struct{}
}

func (*stackContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *stackContext) Done() <-chan struct{}     { return c.canceled }
func (*stackContext) Value(any) any               { return nil }

// Err reports why the runtime has been stopped, while its goroutine is
// blocked servicing this one.
func (c *stackContext) Err() error {
	select {
	case <-c.canceled:
		return errStackCanceled
	default:
	}
	return vm.HostCheckInterrupt(c.r)
}

var errStackCanceled = errors.New("wasmjs: the suspended call can never resume")

// callOut has the runtime's goroutine handle m, and returns its reply.
func (c *stackContext) callOut(m stackMsg) error {
	select {
	case c.toJS <- m:
	case <-c.canceled:
		return errStackCanceled
	}
	select {
	case err := <-c.toWasm:
		return err
	case <-c.canceled:
		return errStackCanceled
	}
}

// suspendingData is what a WebAssembly.Suspending carries.
type suspendingData struct {
	fn vm.Value
}

func (a *api) suspendingOf(v vm.Value) *suspendingData {
	d, _ := vm.HostData(v).(*suspendingData)
	return d
}

// newSuspending is new WebAssembly.Suspending(function).
func (a *api) newSuspending(rt *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
	t := a.thrower("WebAssembly.Suspending()")
	if !rt.Constructing() {
		return vm.Undefined, t.typeError("WebAssembly.Suspending must be invoked with 'new'")
	}
	// The prototype is new.target's to read, but the object is a
	// Suspending's whatever it is.
	if _, err := a.newTargetProto(rt); err != nil {
		return vm.Undefined, err
	}
	fn := arg(args, 0)
	if !vm.IsCallable(fn) || !fn.IsObject() {
		return vm.Undefined, t.typeError("Argument 0 must be a function")
	}
	if s, ok := vm.HostSlot(rt, fn).(*funcSlot); ok && s != nil {
		return vm.Undefined, t.typeError("Argument 0 must not be a WebAssembly function")
	}
	return vm.Obj(vm.HostObject(a.suspendingProto, &suspendingData{fn: fn})), nil
}

// promising is WebAssembly.promising(function).
func (a *api) promising(rt *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
	t := a.thrower("WebAssembly.promising()")
	fn := arg(args, 0)
	if !vm.IsCallable(fn) || !fn.IsObject() {
		return vm.Undefined, t.typeError("Argument 0 must be a function")
	}
	s, ok := vm.HostSlot(rt, fn).(*funcSlot)
	if !ok {
		return vm.Undefined, t.typeError("Argument 0 must be a WebAssembly exported function")
	}
	f := s.fn
	o := rt.NewFunc(strconv.FormatUint(uint64(f.Index), 10), len(f.Sig.Params),
		func(rt *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
			return a.callPromising(f, args)
		})
	vm.HostSetSlot(rt, o, &funcSlot{fn: f, promising: true, a: a})
	return vm.Obj(o), nil
}

// callPromising calls f on a stack of its own and returns the promise of
// its result.
func (a *api) callPromising(f *engine.Function, args []vm.Value) (vm.Value, error) {
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
	s := &jspiStack{a: a, sig: sig, result: a.rt.NewHostPromise(),
		toJS: make(chan stackMsg), toWasm: make(chan error)}
	ctx := &stackContext{r: a.rt, toJS: s.toJS, toWasm: s.toWasm, canceled: make(chan struct{})}
	s.handle = &stackHandle{s: s}
	// Once nothing can resume the stack, its goroutine is told so and ends.
	runtime.AddCleanup(s.handle, func(c chan struct{}) { close(c) }, ctx.canceled)
	go func() {
		res, err := engine.Invoke(ctx, f, vals)
		select {
		case ctx.toJS <- stackMsg{done: true, results: res, err: err}:
		case <-ctx.canceled:
		}
	}()
	promise := s.result.Value()
	if err := a.service(s); err != nil {
		return vm.Undefined, err
	}
	return promise, nil
}

// service handles stack s's calls out until it suspends or ends. It
// returns a failure that is not an exception: the host stopping the script.
func (a *api) service(s *jspiStack) error {
	prev := a.jspi.current
	a.jspi.current = s
	defer func() { a.jspi.current = prev }()
	for {
		m := <-s.toJS
		switch {
		case m.run != nil:
			s.toWasm <- m.run()
		case m.suspend != nil:
			ret, err := a.rt.Call(m.suspend.callable, vm.Undefined, a.suspendArgs(m.suspend))
			if err != nil {
				s.toWasm <- a.wasmError(err)
				continue
			}
			if err := a.await(s, m.suspend, ret); err != nil {
				// Awaiting failed: the stack is resumed with the error.
				s.toWasm <- a.wasmError(err)
				continue
			}
			return nil
		default:
			if m.err != nil {
				err := a.runError(m.err)
				if v, ok := thrownValue(err); ok {
					s.result.Reject(v)
					return nil
				}
				if errors.Is(err, errStackCanceled) {
					return nil
				}
				return err
			}
			v, err := a.results(m.results, s.sig.Results)
			if err != nil {
				if tv, ok := thrownValue(err); ok {
					s.result.Reject(tv)
					return nil
				}
				return err
			}
			s.result.Resolve(v)
			return nil
		}
	}
}

// suspendArgs converts a suspending import's arguments to JavaScript; its
// signature is compatible, so they convert.
func (a *api) suspendArgs(su *suspension) []vm.Value {
	out := make([]vm.Value, len(su.args))
	for i, t := range su.sig.Params {
		out[i], _ = a.toJS(su.args[i], t)
	}
	return out
}

// await waits for the promise of ret, resuming s with its value or its
// rejection.
func (a *api) await(s *jspiStack, su *suspension, ret vm.Value) error {
	r := a.rt
	h := s.handle
	p := r.NewHostPromise()
	p.Resolve(ret)
	resume := func(err error) (vm.Value, error) {
		st := h.s
		st.toWasm <- err
		return vm.Undefined, a.service(st)
	}
	onFulfilled := r.NewFunc("", 1, func(rt *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
		return resume(a.wasmError(a.returned(arg(args, 0), su.sig.Results, su.results)))
	})
	onRejected := r.NewFunc("", 1, func(rt *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
		return resume(a.wasmError(rt.ThrowValue(arg(args, 0))))
	})
	then, err := r.GetProp(p.Value(), r.Intern("then"))
	if err != nil {
		return err
	}
	_, err = r.Call(then, p.Value(), []vm.Value{vm.Obj(onFulfilled), vm.Obj(onRejected)})
	return err
}

// suspendingFunction returns the host function wasm calls a suspending
// import through, of canonical type c.
func (a *api) suspendingFunction(d *suspendingData, c module.CanonicalIndex, index uint32) *engine.Function {
	sig := module.Canonical(c).Func
	fn := engine.NewHostFunction(c, "", func(ctx context.Context, caller *engine.Instance, args, results []engine.Val) error {
		sc, ok := ctx.(*stackContext)
		if !ok {
			msg := "trying to suspend without WebAssembly.promising"
			if cur := a.jspi.current; cur != nil && cur.jsFrames > 0 {
				msg = "trying to suspend JS frames"
			}
			return a.wasmError(a.throwError(a.suspendErrorProto, msg))
		}
		if !jsCompatible(sig) {
			return sc.callOut(stackMsg{run: func() error {
				return a.wasmError(a.rt.ThrowTypeError("%s", msgJSType))
			}})
		}
		return sc.callOut(stackMsg{suspend: &suspension{callable: d.fn, sig: sig, args: args, results: results}})
	})
	fn.Index = index
	return fn
}
