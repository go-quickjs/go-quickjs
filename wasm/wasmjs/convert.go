package wasmjs

import (
	"math"

	"github.com/go-quickjs/go-quickjs/internal/vm"
	"github.com/go-quickjs/go-quickjs/wasm/internal/engine"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
)

// Converting values between JavaScript and wasm: ToWebAssemblyValue and
// ToJSValue.
//
// Types here are canonical: a type index in a reference type is a canonical
// index, as in a function's Sig, so that a value is checked against a type
// whatever module named it.

// msgJSType is the TypeError of a value that cannot cross between wasm and
// JavaScript where a function is called.
const msgJSType = "type incompatibility when transforming from/to JS"

// canon returns t, a type of module m, with its type index made canonical.
func canon(t module.ValueType, m *module.Module) module.ValueType {
	if t.HasIndex() && m != nil {
		t.Heap = module.HeapType(m.Canonical[t.Heap.Index()])
	}
	return t
}

// jsCompatible reports whether every type of sig can cross into JavaScript:
// none is a v128 or an exception reference.
func jsCompatible(sig *module.FuncType) bool {
	ok := func(t module.ValueType) bool {
		if t.Kind == module.KindV128 {
			return false
		}
		return !t.IsRef() || (t.Heap != module.HeapExn && t.Heap != module.HeapNoExn)
	}
	for _, t := range sig.Params {
		if !ok(t) {
			return false
		}
	}
	for _, t := range sig.Results {
		if !ok(t) {
			return false
		}
	}
	return true
}

// toInt32 is ToInt32 of a number.
func toInt32(f float64) int32 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	f = math.Trunc(f)
	if f >= -2147483648 && f <= 2147483647 {
		return int32(f)
	}
	m := math.Mod(f, 4294967296)
	if m < 0 {
		m += 4294967296
	}
	return int32(uint32(m))
}

// i31Of reports whether number f is an i31: an integer, not -0, in i31's
// range; such a number crosses into wasm as an i31ref.
func i31Of(f float64) (int32, bool) {
	if f != math.Trunc(f) || f < -(1<<30) || f >= 1<<30 || (f == 0 && math.Signbit(f)) {
		return 0, false
	}
	return int32(f), true
}

// toNumber is ToNumber, with V8's messages for the primitives it refuses.
func (a *api) toNumber(v vm.Value) (float64, error) {
	switch {
	case v.IsBigInt():
		return 0, a.rt.ThrowTypeError("Cannot convert a BigInt value to a number")
	case v.IsSymbol():
		return 0, a.rt.ThrowTypeError("Cannot convert a Symbol value to a number")
	}
	return a.rt.ToNumber(v)
}

// bigIntRefused returns the TypeError ToBigInt throws for a primitive it
// refuses -- a Number, undefined, null, a Symbol -- with V8's message, or
// nil for anything else.
func (a *api) bigIntRefused(v vm.Value) error {
	if !v.IsNumber() && !v.IsUndefined() && !v.IsNull() && !v.IsSymbol() {
		return nil
	}
	var s string
	if v.IsSymbol() {
		s = "Symbol()"
		if d, err := a.rt.GetProp(v, a.rt.Intern("description")); err == nil && d.IsString() {
			s = "Symbol(" + d.String().Go() + ")"
		}
	} else if str, err := a.rt.ToString(v); err == nil {
		s = str.Go()
	}
	return a.rt.ThrowTypeError("Cannot convert %s to a BigInt", s)
}

// toBigInt64 is ToBigInt64.
func (a *api) toBigInt64(v vm.Value) (int64, error) {
	if err := a.bigIntRefused(v); err != nil {
		return 0, err
	}
	return vm.HostToBigInt64(a.rt, v)
}

// toWasm converts v to a value of type t, which is numeric or a reference
// type; a reference that does not fit t is the TypeError of a call.
func (a *api) toWasm(v vm.Value, t module.ValueType) (engine.Val, error) {
	if t.IsRef() {
		ref, msg := a.toWasmRef(v, t)
		if msg != "" {
			return engine.Val{}, a.rt.ThrowTypeError("%s", msgJSType)
		}
		return engine.Val{R: ref}, nil
	}
	return a.toWasmNum(v, t)
}

// toWasmNum converts v to a number of type t, as the conversions a call
// makes; a v128 cannot be converted.
func (a *api) toWasmNum(v vm.Value, t module.ValueType) (engine.Val, error) {
	switch t.Kind {
	case module.KindI32:
		f, err := a.toNumber(v)
		if err != nil {
			return engine.Val{}, err
		}
		return engine.Val{N: uint64(uint32(toInt32(f)))}, nil
	case module.KindI64:
		n, err := a.toBigInt64(v)
		if err != nil {
			return engine.Val{}, err
		}
		return engine.Val{N: uint64(n)}, nil
	case module.KindF32:
		f, err := a.toNumber(v)
		if err != nil {
			return engine.Val{}, err
		}
		return engine.Val{N: uint64(math.Float32bits(float32(f)))}, nil
	case module.KindF64:
		f, err := a.toNumber(v)
		if err != nil {
			return engine.Val{}, err
		}
		return engine.Val{N: math.Float64bits(f)}, nil
	}
	return engine.Val{}, a.rt.ThrowTypeError("%s", msgJSType)
}

// toWasmRef converts v to a reference of type t, returning the message of
// why it cannot be one.
func (a *api) toWasmRef(v vm.Value, t module.ValueType) (engine.Ref, string) {
	if t.Nullable && v.IsNull() {
		switch t.Heap {
		case module.HeapExn:
			return engine.Null, "invalid type (ref null exn)"
		case module.HeapNoExn:
			return engine.Null, "invalid type (ref null noexn)"
		}
		return engine.Null, ""
	}
	if t.Heap.IsIndex() {
		c := module.CanonicalIndex(t.Heap)
		if f := a.exportedFunction(v); f != nil {
			if !module.IsCanonicalSubtype(f.Type, c) {
				return engine.Null, "assigned exported function has to be a subtype of the expected type"
			}
			return engine.FuncRef(f), ""
		}
		if ref, ok := a.gcRef(v); ok {
			if !engine.RefIsCanonicalSub(ref, c) {
				return engine.Null, "object is not a subtype of expected type"
			}
			return ref, ""
		}
		return engine.Null, "JS object does not match expected wasm type"
	}
	switch t.Heap {
	case module.HeapFunc:
		if f := a.exportedFunction(v); f != nil {
			return engine.FuncRef(f), ""
		}
		return engine.Null, "function-typed object must be null (if nullable) or a Wasm function object"
	case module.HeapExtern:
		if v.IsNull() {
			return engine.Null, "null is not allowed for (ref extern)"
		}
		return a.hostRef(v), ""
	case module.HeapAny:
		if v.IsNull() {
			return engine.Null, "null is not allowed for (ref any)"
		}
		return a.hostRef(v), ""
	case module.HeapExn:
		return engine.Null, "invalid type (ref exn)"
	case module.HeapStruct:
		if ref, ok := a.gcRef(v); ok && ref.Kind() == engine.ObjStruct {
			return ref, ""
		}
		return engine.Null, "structref object must be null (if nullable) or a wasm struct"
	case module.HeapArray:
		if ref, ok := a.gcRef(v); ok && ref.Kind() == engine.ObjArray {
			return ref, ""
		}
		return engine.Null, "arrayref object must be null (if nullable) or a wasm array"
	case module.HeapEq:
		if v.IsNumber() {
			if i, ok := i31Of(v.Number()); ok {
				return engine.I31(uint32(i)), ""
			}
		} else if ref, ok := a.gcRef(v); ok {
			return ref, ""
		}
		return engine.Null, "eqref object must be null (if nullable), or a wasm struct/array, or a Number that fits in i31ref range"
	case module.HeapI31:
		if v.IsNumber() {
			if i, ok := i31Of(v.Number()); ok {
				return engine.I31(uint32(i)), ""
			}
		}
		return engine.Null, "i31ref object must be null (if nullable) or a Number that fits in i31ref range"
	}
	return engine.Null, "only null allowed for null types"
}

// hostRef is the reference to a JavaScript value as externref and anyref
// hold one: a number that fits an i31 is one, a struct or an array that came
// from wasm is itself, and anything else is the value.
func (a *api) hostRef(v vm.Value) engine.Ref {
	if v.IsNull() {
		return engine.Null
	}
	if v.IsNumber() {
		if i, ok := i31Of(v.Number()); ok {
			return engine.I31(uint32(i))
		}
	}
	if ref, ok := a.gcRef(v); ok {
		return ref
	}
	return engine.HostRef(v)
}

// toJS converts a value of type t to JavaScript; a v128 or an exception
// reference cannot be converted.
func (a *api) toJS(v engine.Val, t module.ValueType) (vm.Value, error) {
	switch t.Kind {
	case module.KindI32:
		return vm.Float(float64(int32(uint32(v.N)))), nil
	case module.KindI64:
		return vm.HostBigInt64(int64(v.N)), nil
	case module.KindF32:
		return vm.Float(float64(math.Float32frombits(uint32(v.N)))), nil
	case module.KindF64:
		return vm.Float(math.Float64frombits(v.N)), nil
	case module.KindRef:
		if t.Heap == module.HeapExn || t.Heap == module.HeapNoExn {
			break
		}
		return a.refToJS(v.R), nil
	}
	return vm.Undefined, a.rt.ThrowTypeError("%s", msgJSType)
}

// refToJS returns the JavaScript value of a reference.
func (a *api) refToJS(r engine.Ref) vm.Value {
	switch {
	case r.IsNull():
		return vm.Null
	case r.IsI31():
		return vm.Float(float64(r.I31Signed()))
	}
	switch r.Kind() {
	case engine.ObjHost:
		if h, ok := r.Host(); ok {
			if v, ok := h.(vm.Value); ok {
				return v
			}
		}
		return vm.Undefined
	case engine.ObjFunc:
		return vm.Obj(a.functionObject(r.Func()))
	case engine.ObjStruct, engine.ObjArray:
		return vm.Obj(a.gcObject(r))
	case engine.ObjExn:
		return vm.Obj(a.exceptionObject(r.Exn()))
	}
	return vm.Undefined
}

// defaultRef is the value a reference of type t holds when none is given:
// undefined for an externref, null for the rest.
func defaultRef(t module.ValueType) vm.Value {
	if t.Heap == module.HeapExtern {
		return vm.Undefined
	}
	return vm.Null
}
