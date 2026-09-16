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
	"time"

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
}

// Option configures a Runtime.
type Option func(*config)

type config struct {
	memoryLimit  int64
	stackSize    int
	maxCallDepth int
}

// WithMemoryLimit caps the memory the runtime will account for. Exceeding it
// raises a RangeError in the script rather than exhausting the process.
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

// New creates a Runtime with the standard globals installed.
func New(opts ...Option) *Runtime {
	var c config
	for _, o := range opts {
		o(&c)
	}
	return &Runtime{rt: vm.New(vm.Config{
		MemoryLimit:  c.memoryLimit,
		StackSize:    c.stackSize,
		MaxCallDepth: c.maxCallDepth,
	})}
}

// Close releases the runtime. Using a Runtime after Close returns ErrClosed
// from every method.
func (r *Runtime) Close() error {
	r.closed = true
	r.rt = nil
	return nil
}

// ErrClosed is returned by a Runtime that has been closed.
var ErrClosed = errors.New("quickjs: runtime is closed")

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
	if r.closed {
		return Value{}, ErrClosed
	}
	fn, err := compile(src, "<eval>")
	if err != nil {
		return Value{}, err
	}
	r.rt.SetContext(ctx)
	defer r.rt.SetContext(nil)

	v, err := r.rt.Run(fn)
	if err != nil {
		return Value{}, r.wrapError(err)
	}
	return Value{v: v, rt: r.rt}, nil
}

// EvalFile compiles and runs src, using name in stack traces.
func (r *Runtime) EvalFile(name, src string) (Value, error) {
	if r.closed {
		return Value{}, ErrClosed
	}
	fn, err := compile(src, name)
	if err != nil {
		return Value{}, err
	}
	v, err := r.rt.Run(fn)
	if err != nil {
		return Value{}, r.wrapError(err)
	}
	return Value{v: v, rt: r.rt}, nil
}

// compile parses and compiles source text.
func compile(src, name string) (*bytecodeFunc, error) {
	prog, err := parser.Parse(src, parser.Options{})
	if err != nil {
		return nil, &SyntaxError{err: err}
	}
	fn, err := compiler.Compile(prog, compiler.Options{Source: name, Text: src})
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

// wrapError converts an engine error into the public form.
func (r *Runtime) wrapError(err error) error {
	if err == nil {
		return nil
	}
	// A cancelled context surfaces as itself so that errors.Is works.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("quickjs: execution interrupted: %w", err)
	}
	var thrown *vm.Thrown
	if errors.As(err, &thrown) {
		return &Error{
			value: Value{v: thrown.Value, rt: r.rt},
			stack: thrown.Stack,
		}
	}
	return err
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

// SetTimeZone installs the zone that local-time Date methods use. Passing nil
// restores the process zone.
func (r *Runtime) SetTimeZone(loc *time.Location) {
	if r.closed {
		return
	}
	r.rt.SetTimeZone(loc)
}
