package quickjs

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
	"github.com/go-quickjs/go-quickjs/internal/parser"
	"github.com/go-quickjs/go-quickjs/internal/vm"
)

// bytecodeFunc is the compiled form Eval hands to the interpreter. It is
// aliased so that the public package does not expose the internal type.
type bytecodeFunc = bytecode.Function

// vmObj is a small helper so this file does not repeat the conversion.
func vmObj(o *vm.Object) vm.Value { return vm.Obj(o) }

// Kind classifies a Value.
type Kind uint8

const (
	KindUndefined Kind = iota
	KindNull
	KindBool
	KindNumber
	KindString
	KindSymbol
	KindBigInt
	KindObject
	KindArray
	KindFunction
)

func (k Kind) String() string {
	switch k {
	case KindUndefined:
		return "undefined"
	case KindNull:
		return "null"
	case KindBool:
		return "boolean"
	case KindNumber:
		return "number"
	case KindString:
		return "string"
	case KindSymbol:
		return "symbol"
	case KindBigInt:
		return "bigint"
	case KindArray:
		return "array"
	case KindFunction:
		return "function"
	}
	return "object"
}

// Value is a JavaScript value.
//
// A Value is only meaningful while the Runtime that produced it is open. Values
// are compared with Equal and StrictEqual rather than ==, because JavaScript
// equality is not Go equality.
//
// The zero Value is the number zero, wherever it goes: handed to the engine --
// Set, a call's arguments, a Go function's result, a struct's field -- it is 0,
// and Kind, String, Float, Decode and the comparisons say so. It belongs to no
// runtime, so what needs one -- Get, Set, Call, New -- returns ErrClosed.
type Value struct {
	v  vm.Value
	rt *vm.Runtime
}

// Kind returns the value's type, distinguishing arrays and functions from plain
// objects because that is usually what a caller wants to branch on.
func (v Value) Kind() Kind {
	switch v.v.Kind() {
	case vm.KindUndefined:
		return KindUndefined
	case vm.KindNull:
		return KindNull
	case vm.KindBool:
		return KindBool
	case vm.KindNumber:
		return KindNumber
	case vm.KindString:
		return KindString
	case vm.KindSymbol:
		return KindSymbol
	case vm.KindBigInt:
		return KindBigInt
	}
	switch {
	case vm.IsCallable(v.v):
		return KindFunction
	case vm.IsArray(v.v):
		return KindArray
	}
	return KindObject
}

// IsUndefined reports whether the value is undefined.
func (v Value) IsUndefined() bool { return v.v.IsUndefined() }

// IsNull reports whether the value is null.
func (v Value) IsNull() bool { return v.v.IsNull() }

// IsNullish reports whether the value is null or undefined.
func (v Value) IsNullish() bool { return v.v.IsNullish() }

// IsString reports whether the value is a string.
func (v Value) IsString() bool { return v.v.Kind() == vm.KindString }

// IsObject reports whether the value is an object, including arrays and
// functions.
func (v Value) IsObject() bool { return v.v.IsObject() }

// IsFunction reports whether the value can be called.
func (v Value) IsFunction() bool { return vm.IsCallable(v.v) }

// IsArray reports whether the value is an array.
func (v Value) IsArray() bool { return vm.IsArray(v.v) }

// String returns the value's string form, as String(v) would in script.
//
// It satisfies fmt.Stringer, so a Value can be printed directly. A conversion
// that would throw -- a symbol, or an object whose toString throws -- yields a
// placeholder rather than panicking, because a String method must not fail.
func (v Value) String() string {
	if v.rt == nil {
		return "0" // the zero Value
	}
	s, err := v.rt.ToString(v.v)
	if err != nil {
		return "<" + v.Kind().String() + ">"
	}
	return s.Go()
}

// Bool returns the value's truthiness, as a condition would treat it.
func (v Value) Bool() bool { return v.v.Truthy() }

// Float returns the value as a float64, converting if necessary. A value that
// cannot be converted yields NaN.
func (v Value) Float() float64 {
	if v.rt == nil {
		return 0 // the zero Value
	}
	n, err := v.rt.ToNumber(v.v)
	if err != nil {
		return math.NaN()
	}
	return n
}

// Int returns the value as an int, truncating toward zero.
func (v Value) Int() int {
	f := v.Float()
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return int(f)
}

// StrictEqual reports whether two values are === to each other.
func (v Value) StrictEqual(other Value) bool { return v.v.StrictEquals(other.v) }

// Equal reports whether two values are == to each other, applying the coercion
// rules that the loose equality operator uses.
func (v Value) Equal(other Value) (bool, error) {
	rt := v.rt
	if rt == nil {
		// The zero Value is 0, compared in the other's runtime -- or with
		// another zero Value, equal to it.
		if rt = other.rt; rt == nil {
			return true, nil
		}
	}
	// Loose equality can call user code through valueOf, so it can fail.
	res, err := rt.Call(rt.NewFunction("", 2, looseEqualsNative), vm.Undefined,
		[]vm.Value{v.v, other.v})
	if err != nil {
		return false, wrapThrown(rt, err)
	}
	return res.Truthy(), nil
}

// looseEqualsNative exposes the interpreter's == so that Equal can reuse it
// rather than reimplementing the coercion table.
func looseEqualsNative(rt *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
	return rt.LooseEquals(args[0], args[1])
}

// Get reads a property.
func (v Value) Get(name string) (Value, error) {
	if v.rt == nil {
		return Value{}, ErrClosed
	}
	res, err := v.rt.GetProp(v.v, v.rt.Intern(name))
	if err != nil {
		return Value{}, wrapThrown(v.rt, err)
	}
	return Value{v: res, rt: v.rt}, nil
}

// Set writes a property, converting the Go value with Encode's rules.
func (v Value) Set(name string, val any) error {
	if v.rt == nil {
		return ErrClosed
	}
	encoded, err := encodeValue(v.rt, val)
	if err != nil {
		return err
	}
	return wrapThrown(v.rt, v.rt.SetProp(v.v, v.rt.Intern(name), encoded))
}

// Index reads an array element.
func (v Value) Index(i int) (Value, error) {
	if v.rt == nil {
		return Value{}, ErrClosed
	}
	res, err := v.rt.GetProp(v.v, v.rt.Intern(fmt.Sprint(i)))
	if err != nil {
		return Value{}, wrapThrown(v.rt, err)
	}
	return Value{v: res, rt: v.rt}, nil
}

// Len returns an array's length, or 0 for a value that is not array-like.
func (v Value) Len() int {
	if v.rt == nil || !v.v.IsObject() {
		return 0
	}
	n, err := v.rt.GetProp(v.v, v.rt.Intern("length"))
	if err != nil {
		return 0
	}
	f, err := v.rt.ToNumber(n)
	if err != nil || math.IsNaN(f) {
		return 0
	}
	return int(f)
}

// Keys returns an object's own enumerable string keys, in specification order.
func (v Value) Keys() []string {
	if v.rt == nil || !v.v.IsObject() {
		return nil
	}
	atoms := v.rt.OwnKeys(v.v.Object())
	out := make([]string, len(atoms))
	for i, a := range atoms {
		out[i] = v.rt.AtomName(a)
	}
	return out
}

// Call invokes the value as a function with the given arguments.
//
// Arguments are converted with Encode's rules, so ordinary Go values may be
// passed directly.
func (v Value) Call(args ...any) (Value, error) {
	return v.CallWithThis(Value{v: vm.Undefined, rt: v.rt}, args...)
}

// CallWithThis invokes the value as a function with an explicit receiver.
func (v Value) CallWithThis(this Value, args ...any) (Value, error) {
	if v.rt == nil {
		return Value{}, ErrClosed
	}
	host, _ := v.rt.Host.(*Runtime)
	if host != nil && host.closed {
		return Value{}, ErrClosed
	}
	if !vm.IsCallable(v.v) {
		return Value{}, fmt.Errorf("quickjs: %s is not a function", v.Kind())
	}
	vals := make([]vm.Value, len(args))
	for i, a := range args {
		enc, err := encodeValue(v.rt, a)
		if err != nil {
			return Value{}, err
		}
		vals[i] = enc
	}
	v.rt.ClearStop()
	res, err := v.rt.Call(v.v, this.v, vals)
	if host != nil && host.closed {
		// The function closed its Runtime, which stopped it.
		v.rt.ReleaseClosed()
		return Value{}, ErrClosed
	}
	if err != nil {
		return Value{}, wrapThrown(v.rt, err)
	}
	return Value{v: res, rt: v.rt}, nil
}

// CallContext is Call with cancellation, as EvalContext is Eval with it: the
// function stops when ctx is cancelled or its deadline passes, and the error
// then wraps ctx.Err(), so errors.Is(err, context.DeadlineExceeded) identifies
// a timeout. Called from inside a running script -- by a Go function the
// script called -- it is bounded by the script's context as well.
func (v Value) CallContext(ctx context.Context, args ...any) (Value, error) {
	return v.CallWithThisContext(ctx, Value{v: vm.Undefined, rt: v.rt}, args...)
}

// CallWithThisContext is CallWithThis with cancellation, as CallContext is
// Call with it.
func (v Value) CallWithThisContext(ctx context.Context, this Value, args ...any) (Value, error) {
	if v.rt == nil {
		return Value{}, ErrClosed
	}
	r, _ := v.rt.Host.(*Runtime)
	if r == nil || r.closed {
		return Value{}, ErrClosed
	}
	outer := v.rt.Context()
	nested, leave := r.enter(ctx)
	defer leave()
	res, err := v.CallWithThis(this, args...)
	var jsErr *Error
	if err != nil && !errors.As(err, &jsErr) &&
		(errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		endNested(v.rt, nested, ctx, outer)
		return Value{}, fmt.Errorf("quickjs: execution interrupted: %w", err)
	}
	return res, err
}

// New invokes the value as a constructor.
func (v Value) New(args ...any) (Value, error) {
	if v.rt == nil {
		return Value{}, ErrClosed
	}
	vals := make([]vm.Value, len(args))
	for i, a := range args {
		enc, err := encodeValue(v.rt, a)
		if err != nil {
			return Value{}, err
		}
		vals[i] = enc
	}
	v.rt.ClearStop()
	res, err := v.rt.Construct(v.v, vals)
	if err != nil {
		return Value{}, wrapThrown(v.rt, err)
	}
	return Value{v: res, rt: v.rt}, nil
}

// Error is a JavaScript exception that reached Go.
//
// The thrown value is preserved rather than formatted, so a script that throws
// a custom object can have it inspected here.
type Error struct {
	value Value
	stack []vm.StackEntry
	// thrown is the exception as the engine threw it, which a Go function
	// returning the error throws on unchanged.
	thrown *vm.Thrown
}

// wrapThrown is an error from the engine as the API reports it: an exception
// is an *Error.
func wrapThrown(rt *vm.Runtime, err error) error {
	var thrown *vm.Thrown
	if errors.As(err, &thrown) {
		return &Error{value: Value{v: thrown.Value, rt: rt}, stack: thrown.Stack, thrown: thrown}
	}
	return err
}

func (e *Error) Error() string {
	return e.value.String()
}

// Value returns the thrown value.
func (e *Error) Value() Value { return e.value }

// Unwrap returns the Go error the exception was made from: what a Go function
// the script called returned, which the script threw on -- uncaught, or
// caught and rethrown as the same object. errors.Is and errors.As reach it
// through the *Error. It is nil for an exception the script made itself.
func (e *Error) Unwrap() error {
	if e.thrown == nil {
		return nil
	}
	return e.thrown.Cause()
}

// Stack renders the JavaScript stack trace at the point of the throw.
//
// A thrown Error carries its own trace, written when it was built, so that is
// what is reported for one; anything else thrown gets the frames recorded at
// the throw.
func (e *Error) Stack() string {
	if len(e.stack) == 0 {
		if v, err := e.value.Get("stack"); err == nil && !v.IsUndefined() {
			return v.String()
		}
	}
	var sb strings.Builder
	sb.WriteString(e.Error())
	for _, f := range e.stack {
		name := f.Function
		if name == "" {
			name = "<anonymous>"
		}
		fmt.Fprintf(&sb, "\n    at %s", name)
		switch {
		case f.Source != "" && f.Column > 0:
			fmt.Fprintf(&sb, " (%s:%d:%d)", f.Source, f.Line, f.Column)
		case f.Source != "":
			fmt.Fprintf(&sb, " (%s:%d)", f.Source, f.Line)
		}
	}
	return sb.String()
}

// SyntaxError reports source that failed to parse or compile, and where: the
// name the source was compiled under, and the line and column in it, which
// Position reports and the message ends with -- "(main.js:3:9)", or "(line 3,
// column 9)" for source with no name.
type SyntaxError struct {
	err          error
	msg          string
	file         string
	line, column int
}

// newSyntaxError is err, from the parser or the compiler, as source named
// file reports it: with its position placed as WithOffset places the source,
// lineOffset lines down and, on its first line, columnOffset columns in. eval
// code, and source that is only checked, has no name.
func newSyntaxError(err error, file string, lineOffset, columnOffset int) *SyntaxError {
	if file == "<eval>" || file == "<check>" {
		file = ""
	}
	e := &SyntaxError{err: err, msg: strings.TrimPrefix(err.Error(), "SyntaxError: "), file: file}
	var pe *parser.Error
	var ce *compiler.Error
	switch {
	case errors.As(err, &pe):
		e.msg, e.line, e.column = pe.Msg, pe.Line, pe.Col
		if e.line > 0 {
			if e.line == 1 && e.column > 0 {
				e.column += columnOffset
			}
			e.line += lineOffset
		}
	case errors.As(err, &ce):
		// The compiler's positions are placed in the file already.
		e.msg, e.line, e.column = ce.Msg, ce.Line, ce.Col
	}
	return e
}

func (e *SyntaxError) Error() string {
	var where string
	switch {
	case e.line <= 0:
		where = e.file
	case e.file != "" && e.column > 0:
		where = fmt.Sprintf("%s:%d:%d", e.file, e.line, e.column)
	case e.file != "":
		where = fmt.Sprintf("%s:%d", e.file, e.line)
	case e.column > 0:
		where = fmt.Sprintf("line %d, column %d", e.line, e.column)
	default:
		where = fmt.Sprintf("line %d", e.line)
	}
	if where == "" {
		return "quickjs: SyntaxError: " + e.msg
	}
	return "quickjs: SyntaxError: " + e.msg + " (" + where + ")"
}

func (e *SyntaxError) Unwrap() error { return e.err }

// Position reports where the source failed: the name it was compiled under --
// empty for code with none, as Eval's and eval's -- and the line and column,
// counting from 1 and placed as WithOffset placed the source. A line or column
// the compiler does not know is 0.
func (e *SyntaxError) Position() (file string, line, column int) {
	return e.file, e.line, e.column
}
