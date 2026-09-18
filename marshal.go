package quickjs

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"

	"github.com/go-quickjs/go-quickjs/internal/vm"
)

// Conversion between Go and JavaScript values.
//
// The rules follow encoding/json where the two overlap, because that is what a
// Go programmer already knows: a struct becomes an object keyed by field name,
// a map becomes an object, a slice becomes an array, and a nil pointer becomes
// null. The differences are the ones JavaScript forces: undefined exists and is
// distinct from null, and a Go function becomes a callable value rather than
// being rejected.

// Encode converts a Go value to a JavaScript value.
func (r *Runtime) Encode(v any) (Value, error) {
	if r.closed {
		return Value{}, ErrClosed
	}
	out, err := r.encode(v)
	if err != nil {
		return Value{}, err
	}
	return Value{v: out, rt: r.rt}, nil
}

func (r *Runtime) encode(v any) (vm.Value, error) { return encodeValue(r.rt, v) }

// encodeValue converts a Go value into the engine's representation.
func encodeValue(rt *vm.Runtime, v any) (vm.Value, error) {
	switch x := v.(type) {
	case nil:
		return vm.Null, nil
	case Value:
		return x.v, nil
	case bool:
		return vm.Bool(x), nil
	case string:
		return vm.Str(vm.NewString(x)), nil
	case int:
		return vm.Float(float64(x)), nil
	case int8:
		return vm.Float(float64(x)), nil
	case int16:
		return vm.Float(float64(x)), nil
	case int32:
		return vm.Float(float64(x)), nil
	case int64:
		return vm.Float(float64(x)), nil
	case uint:
		return vm.Float(float64(x)), nil
	case uint8:
		return vm.Float(float64(x)), nil
	case uint16:
		return vm.Float(float64(x)), nil
	case uint32:
		return vm.Float(float64(x)), nil
	case uint64:
		return vm.Float(float64(x)), nil
	case float32:
		return vm.Float(float64(x)), nil
	case float64:
		return vm.Float(x), nil
	case []byte:
		// A byte slice becomes an array of numbers; a typed array would be
		// better but is not implemented yet.
		vals := make([]vm.Value, len(x))
		for i, b := range x {
			vals[i] = vm.Float(float64(b))
		}
		return vm.Obj(rt.NewArray(vals)), nil
	case error:
		return rt.NewError("Error", x.Error()), nil
	}

	rv := reflect.ValueOf(v)
	return encodeReflect(rt, rv)
}

var (
	valueType   = reflect.TypeOf(Value{})
	promiseType = reflect.TypeOf((*Promise)(nil))
)

func encodeReflect(rt *vm.Runtime, rv reflect.Value) (vm.Value, error) {
	if !rv.IsValid() {
		return vm.Null, nil
	}
	// A value the host already built is passed through rather than reflected
	// over: a Value is a JavaScript value, not a struct with fields, and a
	// Promise is the promise it stands for.
	switch rv.Type() {
	case valueType:
		return rv.Interface().(Value).v, nil
	case promiseType:
		if rv.IsNil() {
			return vm.Null, nil
		}
		return rv.Interface().(*Promise).Value().v, nil
	}
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			return vm.Null, nil
		}
		return encodeReflect(rt, rv.Elem())

	case reflect.Bool:
		return vm.Bool(rv.Bool()), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return vm.Float(float64(rv.Int())), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return vm.Float(float64(rv.Uint())), nil
	case reflect.Float32, reflect.Float64:
		return vm.Float(rv.Float()), nil
	case reflect.String:
		return vm.Str(vm.NewString(rv.String())), nil

	case reflect.Slice, reflect.Array:
		if rv.Kind() == reflect.Slice && rv.IsNil() {
			return vm.Null, nil
		}
		vals := make([]vm.Value, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			el, err := encodeReflect(rt, rv.Index(i))
			if err != nil {
				return vm.Undefined, err
			}
			vals[i] = el
		}
		return vm.Obj(rt.NewArray(vals)), nil

	case reflect.Map:
		if rv.IsNil() {
			return vm.Null, nil
		}
		o := rt.NewObject()
		iter := rv.MapRange()
		for iter.Next() {
			// Keys are stringified, since a JavaScript property key is a
			// string or a symbol.
			key := fmt.Sprint(iter.Key().Interface())
			val, err := encodeReflect(rt, iter.Value())
			if err != nil {
				return vm.Undefined, err
			}
			if err := rt.DefineProp(o, rt.Intern(key), val); err != nil {
				return vm.Undefined, err
			}
		}
		return vm.Obj(o), nil

	case reflect.Struct:
		o := rt.NewObject()
		t := rv.Type()
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name, omitEmpty, skip := fieldName(f)
			if skip {
				continue
			}
			fv := rv.Field(i)
			if omitEmpty && fv.IsZero() {
				continue
			}
			val, err := encodeReflect(rt, fv)
			if err != nil {
				return vm.Undefined, err
			}
			if err := rt.DefineProp(o, rt.Intern(name), val); err != nil {
				return vm.Undefined, err
			}
		}
		return vm.Obj(o), nil

	case reflect.Func:
		return wrapGoFunc(rt, rv)
	}
	return vm.Undefined, fmt.Errorf("quickjs: cannot convert %s to a JavaScript value", rv.Type())
}

// fieldName resolves a struct field's JavaScript name from its tags.
//
// The `js` tag wins, then `json`, so a struct already annotated for
// encoding/json needs no changes.
func fieldName(f reflect.StructField) (name string, omitEmpty, skip bool) {
	tag, ok := f.Tag.Lookup("js")
	if !ok {
		tag = f.Tag.Get("json")
	}
	if tag == "-" {
		return "", false, true
	}
	parts := strings.Split(tag, ",")
	name = parts[0]
	if name == "" {
		name = f.Name
	}
	for _, opt := range parts[1:] {
		if opt == "omitempty" {
			omitEmpty = true
		}
	}
	return name, omitEmpty, false
}

// throwGoError turns the error a Go function returned into a thrown value.
//
// An error that already carries a JavaScript value is rethrown as that value: a
// callback that let one through is passing it on rather than reporting a
// failure of its own, and wrapping it would lose which kind it was. Source that
// failed to compile is a SyntaxError for the same reason.
func throwGoError(rt *vm.Runtime, err error) error {
	var jsErr *Error
	if errors.As(err, &jsErr) {
		return rt.ThrowValue(jsErr.value.v)
	}
	var synErr *SyntaxError
	if errors.As(err, &synErr) {
		return rt.ThrowSyntaxError("%s", synErr.Error())
	}
	return rt.ThrowError(err)
}

// wrapGoFunc makes a Go function callable from JavaScript.
//
// Two shapes get special treatment. A first parameter of type *Runtime receives
// the runtime so the function can call back into the engine, and a final result
// of type error becomes a thrown exception rather than a returned value.
func wrapGoFunc(rt *vm.Runtime, fv reflect.Value) (vm.Value, error) {
	t := fv.Type()
	if t.IsVariadic() {
		// Supporting variadics would require splitting the argument list at the
		// right point for every call; it is not needed yet.
		return vm.Undefined, fmt.Errorf("quickjs: variadic functions are not supported")
	}

	wantsRuntime := t.NumIn() > 0 && t.In(0) == reflect.TypeOf((*Runtime)(nil))
	firstArg := 0
	if wantsRuntime {
		firstArg = 1
	}
	returnsError := t.NumOut() > 0 && t.Out(t.NumOut()-1) == reflect.TypeOf((*error)(nil)).Elem()

	arity := t.NumIn() - firstArg
	name := "" // a Go function has no name the engine can see

	native := func(callRT *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
		in := make([]reflect.Value, t.NumIn())
		if wantsRuntime {
			in[0] = reflect.ValueOf(&Runtime{rt: callRT})
		}
		for i := 0; i < arity; i++ {
			pt := t.In(firstArg + i)
			var av vm.Value
			if i < len(args) {
				av = args[i]
			} else {
				av = vm.Undefined
			}
			dst := reflect.New(pt).Elem()
			if err := decodeInto(callRT, av, dst); err != nil {
				return vm.Undefined, callRT.ThrowTypeError(
					"argument %d: %s", i+1, err.Error())
			}
			in[firstArg+i] = dst
		}

		out := fv.Call(in)

		if returnsError {
			if e := out[len(out)-1]; !e.IsNil() {
				// A returned error becomes a thrown exception.
				return vm.Undefined, throwGoError(callRT, e.Interface().(error))
			}
			out = out[:len(out)-1]
		}
		switch len(out) {
		case 0:
			return vm.Undefined, nil
		case 1:
			return encodeReflect(callRT, out[0])
		}
		// Multiple results become an array, which is the only sensible mapping.
		vals := make([]vm.Value, len(out))
		for i, o := range out {
			v, err := encodeReflect(callRT, o)
			if err != nil {
				return vm.Undefined, err
			}
			vals[i] = v
		}
		return vm.Obj(callRT.NewArray(vals)), nil
	}

	return rt.NewFunction(name, arity, native), nil
}

// ---------------------------------------------------------------------------
// Decoding
// ---------------------------------------------------------------------------

// Decode converts a JavaScript value into a Go value, following the conventions
// of encoding/json.
//
// dst must be a non-nil pointer. A *any receives the natural Go form: nil for
// null and undefined, bool, float64, string, []any or map[string]any.
func (v Value) Decode(dst any) error {
	if v.rt == nil {
		return ErrClosed
	}
	rv := reflect.ValueOf(dst)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return fmt.Errorf("quickjs: Decode requires a non-nil pointer, got %T", dst)
	}
	return decodeInto(v.rt, v.v, rv.Elem())
}

// decodeInto converts one value into a settable destination.
func decodeInto(rt *vm.Runtime, val vm.Value, dst reflect.Value) error {
	// A destination of type Value or any takes the value as-is.
	switch dst.Type() {
	case reflect.TypeOf(Value{}):
		dst.Set(reflect.ValueOf(Value{v: val, rt: rt}))
		return nil
	}
	if dst.Kind() == reflect.Interface && dst.NumMethod() == 0 {
		natural, err := toNatural(rt, val)
		if err != nil {
			return err
		}
		if natural == nil {
			dst.Set(reflect.Zero(dst.Type()))
			return nil
		}
		dst.Set(reflect.ValueOf(natural))
		return nil
	}

	switch dst.Kind() {
	case reflect.Pointer:
		if val.IsNullish() {
			dst.Set(reflect.Zero(dst.Type()))
			return nil
		}
		p := reflect.New(dst.Type().Elem())
		if err := decodeInto(rt, val, p.Elem()); err != nil {
			return err
		}
		dst.Set(p)
		return nil

	case reflect.Bool:
		dst.SetBool(val.Truthy())
		return nil

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := rt.ToNumber(val)
		if err != nil {
			return err
		}
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return fmt.Errorf("cannot convert %v to %s", n, dst.Type())
		}
		if dst.OverflowInt(int64(n)) {
			return fmt.Errorf("%v overflows %s", n, dst.Type())
		}
		dst.SetInt(int64(n))
		return nil

	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := rt.ToNumber(val)
		if err != nil {
			return err
		}
		if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
			return fmt.Errorf("cannot convert %v to %s", n, dst.Type())
		}
		if dst.OverflowUint(uint64(n)) {
			return fmt.Errorf("%v overflows %s", n, dst.Type())
		}
		dst.SetUint(uint64(n))
		return nil

	case reflect.Float32, reflect.Float64:
		n, err := rt.ToNumber(val)
		if err != nil {
			return err
		}
		dst.SetFloat(n)
		return nil

	case reflect.String:
		s, err := rt.ToString(val)
		if err != nil {
			return err
		}
		dst.SetString(s.Go())
		return nil

	case reflect.Slice:
		if val.IsNullish() {
			dst.Set(reflect.Zero(dst.Type()))
			return nil
		}
		if !vm.IsArray(val) {
			return fmt.Errorf("cannot decode %s into %s", describeKind(val), dst.Type())
		}
		elems := vm.ArrayElements(val.Object())
		out := reflect.MakeSlice(dst.Type(), len(elems), len(elems))
		for i, el := range elems {
			if vm.IsHole(el) {
				continue
			}
			if err := decodeInto(rt, el, out.Index(i)); err != nil {
				return fmt.Errorf("index %d: %w", i, err)
			}
		}
		dst.Set(out)
		return nil

	case reflect.Map:
		if val.IsNullish() {
			dst.Set(reflect.Zero(dst.Type()))
			return nil
		}
		if !val.IsObject() {
			return fmt.Errorf("cannot decode %s into %s", describeKind(val), dst.Type())
		}
		if dst.Type().Key().Kind() != reflect.String {
			return fmt.Errorf("quickjs: a map key must be a string, not %s", dst.Type().Key())
		}
		out := reflect.MakeMap(dst.Type())
		for _, k := range rt.OwnKeys(val.Object()) {
			pv, err := rt.GetProp(val, k)
			if err != nil {
				return err
			}
			ev := reflect.New(dst.Type().Elem()).Elem()
			if err := decodeInto(rt, pv, ev); err != nil {
				return fmt.Errorf("key %q: %w", rt.AtomName(k), err)
			}
			out.SetMapIndex(reflect.ValueOf(rt.AtomName(k)).Convert(dst.Type().Key()), ev)
		}
		dst.Set(out)
		return nil

	case reflect.Struct:
		if val.IsNullish() {
			return nil
		}
		if !val.IsObject() {
			return fmt.Errorf("cannot decode %s into %s", describeKind(val), dst.Type())
		}
		t := dst.Type()
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name, _, skip := fieldName(f)
			if skip {
				continue
			}
			pv, err := rt.GetProp(val, rt.Intern(name))
			if err != nil {
				return err
			}
			// An absent property leaves the field at its zero value, as
			// encoding/json does.
			if pv.IsUndefined() {
				continue
			}
			if err := decodeInto(rt, pv, dst.Field(i)); err != nil {
				return fmt.Errorf("field %s: %w", f.Name, err)
			}
		}
		return nil
	}
	return fmt.Errorf("quickjs: cannot decode into %s", dst.Type())
}

// toNatural converts a value to the Go form a *any destination receives.
func toNatural(rt *vm.Runtime, val vm.Value) (any, error) {
	switch val.Kind() {
	case vm.KindUndefined, vm.KindNull:
		return nil, nil
	case vm.KindBool:
		return val.BoolValue(), nil
	case vm.KindNumber:
		return val.Number(), nil
	case vm.KindString:
		return val.String().Go(), nil
	case vm.KindBigInt:
		return val.BigInt().String(), nil
	case vm.KindSymbol:
		return val.Symbol().String(), nil
	}
	if vm.IsArray(val) {
		elems := vm.ArrayElements(val.Object())
		out := make([]any, len(elems))
		for i, el := range elems {
			if vm.IsHole(el) {
				continue
			}
			nv, err := toNatural(rt, el)
			if err != nil {
				return nil, err
			}
			out[i] = nv
		}
		return out, nil
	}
	if vm.IsCallable(val) {
		// A function has no natural Go form, so it stays a Value.
		return Value{v: val, rt: rt}, nil
	}
	out := make(map[string]any)
	for _, k := range rt.OwnKeys(val.Object()) {
		pv, err := rt.GetProp(val, k)
		if err != nil {
			return nil, err
		}
		nv, err := toNatural(rt, pv)
		if err != nil {
			return nil, err
		}
		out[rt.AtomName(k)] = nv
	}
	return out, nil
}

func describeKind(v vm.Value) string {
	switch {
	case vm.IsArray(v):
		return "array"
	case vm.IsCallable(v):
		return "function"
	}
	return v.Kind().String()
}
