package quickjs

import (
	"fmt"
	"math"
	"strings"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
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
		return "<invalid>"
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
		return math.NaN()
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
	if v.rt == nil {
		return false, ErrClosed
	}
	// Loose equality can call user code through valueOf, so it can fail.
	res, err := v.rt.Call(v.rt.NewFunction("", 2, looseEqualsNative), vm.Undefined,
		[]vm.Value{v.v, other.v})
	if err != nil {
		return false, err
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
		return Value{}, err
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
	return v.rt.SetProp(v.v, v.rt.Intern(name), encoded)
}

// Index reads an array element.
func (v Value) Index(i int) (Value, error) {
	if v.rt == nil {
		return Value{}, ErrClosed
	}
	res, err := v.rt.GetProp(v.v, v.rt.Intern(fmt.Sprint(i)))
	if err != nil {
		return Value{}, err
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
	res, err := v.rt.Call(v.v, this.v, vals)
	if err != nil {
		return Value{}, err
	}
	return Value{v: res, rt: v.rt}, nil
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
	res, err := v.rt.Construct(v.v, vals)
	if err != nil {
		return Value{}, err
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
}

func (e *Error) Error() string {
	return e.value.String()
}

// Value returns the thrown value.
func (e *Error) Value() Value { return e.value }

// Stack renders the JavaScript stack trace at the point of the throw.
func (e *Error) Stack() string {
	var sb strings.Builder
	sb.WriteString(e.Error())
	for _, f := range e.stack {
		name := f.Function
		if name == "" {
			name = "<anonymous>"
		}
		fmt.Fprintf(&sb, "\n    at %s", name)
		if f.Source != "" {
			fmt.Fprintf(&sb, " (%s:%d)", f.Source, f.Line)
		}
	}
	return sb.String()
}

// SyntaxError reports source that failed to parse or compile.
type SyntaxError struct{ err error }

func (e *SyntaxError) Error() string { return "quickjs: " + e.err.Error() }
func (e *SyntaxError) Unwrap() error { return e.err }
