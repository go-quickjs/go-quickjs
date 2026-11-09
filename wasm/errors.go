package wasm

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/go-quickjs/go-quickjs/wasm/api"
	"github.com/go-quickjs/go-quickjs/wasm/internal/decoder"
	"github.com/go-quickjs/go-quickjs/wasm/internal/engine"
)

// ErrMemoryLimit is the error a call or an instantiation stops with when the
// GC objects it allocates do not fit the runtime's WithMemoryLimit, and the
// error an instantiation fails with when its tables do not.
var ErrMemoryLimit = engine.ErrMemoryLimit

// wrappedError is an error with a message of its own that unwraps to err.
type wrappedError struct {
	msg string
	err error
}

func (e *wrappedError) Error() string { return e.msg }
func (e *wrappedError) Unwrap() error { return e.err }

// CompileError is a module that does not decode or validate: the first
// error found, and the offset in the module's bytes it was found at.
type CompileError struct {
	// Msg describes the error, e.g. "Compiling function #0 failed: i32.add[1]
	// expected type i32, found i64.const of type i64".
	Msg string
	// Offset is where in the module's bytes the error was found.
	Offset uint32
}

// Error returns "<message> @+<offset>".
func (e *CompileError) Error() string {
	return e.Msg + " @+" + strconv.FormatUint(uint64(e.Offset), 10)
}

func compileError(e *decoder.Error) *CompileError {
	return &CompileError{Msg: e.Msg, Offset: e.Offset}
}

// LinkError is an import that could not be resolved: a module or a name the
// runtime does not have, or an entity that does not match the import's
// declaration.
type LinkError struct {
	// Msg names the import and what is wrong, e.g. `Import #0 "env" "f":
	// imported function does not match the expected type`.
	Msg string
}

// Error returns the message.
func (e *LinkError) Error() string { return e.Msg }

// RuntimeError is a trap: an instruction that could not run, such as an
// integer division by zero, or the call stack's exhaustion.
type RuntimeError struct {
	msg    string
	frames []StackFrame
}

// StackTraceLimit is how many frames RuntimeError.Stack prints.
const StackTraceLimit = 10

// StackFrame is a wasm function on a RuntimeError's call stack.
type StackFrame struct {
	// URL identifies the module: wasm://wasm/ followed by the name in the
	// module's name section and a dash, if it has one, and eight hex
	// digits of a hash of its bytes.
	URL string
	// Name is the function as the stack trace names it: the module's name
	// and the function's, joined by a dot, either of them alone, or empty
	// when the module's name section names neither.
	Name string
	// FunctionIndex is the function's index in its module.
	FunctionIndex uint32
	// Offset is the module offset of the instruction executing: for a
	// frame that made a call, the call. For the frame that trapped it is
	// the offset of the last call that frame made, or zero, until the
	// interpreter records where each trap happens.
	Offset uint32
}

// Error returns the trap's message: "unreachable", "divide by zero",
// "memory access out of bounds", "Maximum call stack size exceeded", ...
func (e *RuntimeError) Error() string { return e.msg }

// Frames returns the wasm call stack where the trap happened, innermost
// first. Host functions are left out.
func (e *RuntimeError) Frames() []StackFrame { return e.frames }

// Stack returns the call stack, innermost first, one line for each of the
// innermost StackTraceLimit frames, each indented by four spaces:
//
//	at name (wasm://wasm/1a2b3c4d:wasm-function[1]:0x3a)
//
// or, for a function without a name,
//
//	at wasm://wasm/1a2b3c4d:wasm-function[1]:0x3a
//
// Frames returns every frame.
func (e *RuntimeError) Stack() string {
	var b strings.Builder
	for i, f := range e.frames {
		if i == StackTraceLimit {
			break
		}
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(f.String())
	}
	return b.String()
}

// String returns the frame's line of a stack trace.
func (f StackFrame) String() string {
	loc := fmt.Sprintf("%s:wasm-function[%d]:0x%x", f.URL, f.FunctionIndex, f.Offset)
	if f.Name == "" {
		return "    at " + loc
	}
	return "    at " + f.Name + " (" + loc + ")"
}

// Exception is a wasm exception that no wasm code caught.
type Exception struct {
	// Values are the exception's payload, as its tag's type gives them.
	Values []api.Value
}

// Error returns "uncaught wasm exception".
func (e *Exception) Error() string { return "uncaught wasm exception" }

// runtimeError converts a trap.
func (r *runtime) runtimeError(t *engine.Trap) *RuntimeError {
	e := &RuntimeError{msg: t.Error()}
	for _, f := range t.Frames {
		if f.Module == nil {
			continue
		}
		info := r.codeOf(f.Module).names()
		sf := StackFrame{URL: info.URL, FunctionIndex: f.Func, Offset: f.Offset}
		if r.cfg.debugInfo {
			sf.Name = info.FrameName(f.Func)
		}
		e.frames = append(e.frames, sf)
	}
	return e
}
