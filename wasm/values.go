package wasm

import (
	"errors"
	"fmt"

	"github.com/go-quickjs/go-quickjs/wasm/api"
	"github.com/go-quickjs/go-quickjs/wasm/internal/engine"
	"github.com/go-quickjs/go-quickjs/wasm/internal/internalapi"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
)

// A type with indices is read against a module, or, with a nil module, as a
// canonical type, whose indices are canonical indices: a function's Sig.

// typeKindOf returns what indexed heap type h of m, or canonical, defines.
func typeKindOf(h module.HeapType, m *module.Module) module.TypeKind {
	if m == nil {
		return module.Canonical(module.CanonicalIndex(h.Index())).Kind
	}
	return m.Types[h.Index()].Kind
}

// canonicalOf returns the canonical index of indexed heap type h of m.
func canonicalOf(h module.HeapType, m *module.Module) module.CanonicalIndex {
	if m == nil {
		return module.CanonicalIndex(h.Index())
	}
	return m.Canonical[h.Index()]
}

// typeCode returns t as the api reports it.
func typeCode(t module.ValueType, m *module.Module) api.ValueType {
	switch t.Kind {
	case module.KindI32:
		return api.ValueTypeI32
	case module.KindI64:
		return api.ValueTypeI64
	case module.KindF32:
		return api.ValueTypeF32
	case module.KindF64:
		return api.ValueTypeF64
	case module.KindV128:
		return api.ValueTypeV128
	}
	if !t.Heap.IsIndex() {
		return t.Heap.Code()
	}
	switch typeKindOf(t.Heap, m) {
	case module.TypeStruct:
		return api.ValueTypeStructref
	case module.TypeArray:
		return api.ValueTypeArrayref
	}
	return api.ValueTypeFuncref
}

func typeCodes(ts []module.ValueType, m *module.Module) []api.ValueType {
	out := make([]api.ValueType, len(ts))
	for i, t := range ts {
		out[i] = typeCode(t, m)
	}
	return out
}

// isExtern reports whether t is a reference in the extern hierarchy.
func isExtern(t module.ValueType) bool {
	return t.Kind == module.KindRef && (t.Heap == module.HeapExtern || t.Heap == module.HeapNoExtern)
}

// slots returns how many uint64s Call takes for values of types ts.
func slots(ts []module.ValueType) int {
	n := len(ts)
	for _, t := range ts {
		if t.Kind == module.KindV128 {
			n++
		}
	}
	return n
}

// errIncompatible is a value of the wrong type for where it is passed.
var errIncompatible = errors.New("type incompatibility: the value is not of the type expected")

// reference is a reference the host holds without seeing into it.
type reference struct {
	internalapi.WasmOnlyType
	r engine.Ref
}

func (r *reference) Kind() string {
	if r.r.IsI31() {
		return "i31"
	}
	switch r.r.Kind() {
	case engine.ObjFunc:
		return "func"
	case engine.ObjStruct:
		return "struct"
	case engine.ObjArray:
		return "array"
	case engine.ObjExn:
		return "exn"
	}
	return "extern"
}

// fromNumber returns a number or vector of type t encoded as Call encodes it:
// lo, and hi for a v128.
func fromNumber(t module.ValueType, lo, hi uint64) engine.Val {
	switch t.Kind {
	case module.KindI32, module.KindF32:
		return engine.Val{N: uint64(uint32(lo))}
	case module.KindV128:
		return engine.Val{N: lo, Hi: hi}
	}
	return engine.Val{N: lo}
}

// toRef returns the engine's reference for v, a Value's Ref, where a
// reference of type t is expected.
func (r *runtime) toRef(v any, t module.ValueType, m *module.Module) (engine.Ref, error) {
	if v == nil {
		if !t.Nullable {
			return engine.Null, errIncompatible
		}
		return engine.Null, nil
	}
	var ref engine.Ref
	opaque := false
	switch x := v.(type) {
	case *reference:
		ref, opaque = x.r, true
	case *function:
		ref = engine.FuncRef(x.fn)
	}
	h := t.Heap
	if h.IsIndex() {
		if k := typeKindOf(h, m); k != module.TypeFunc {
			// The engine does not tell the host which defined type a
			// struct or an array has, so none is passed where one is
			// expected.
			return engine.Null, fmt.Errorf("a reference to a %s of a defined type cannot be passed from the host", k.Name())
		}
		f := ref.Func()
		if f == nil || !module.IsCanonicalSubtype(f.Type, canonicalOf(h, m)) {
			return engine.Null, errIncompatible
		}
		return ref, nil
	}
	switch h {
	case module.HeapFunc:
		if ref.Func() == nil {
			return engine.Null, errIncompatible
		}
		return ref, nil
	case module.HeapExtern, module.HeapAny:
		if !opaque {
			// Any Go value, a Function too, is the host's.
			return engine.HostRef(v), nil
		}
	}
	if !opaque || ref.IsNull() || !engine.RefMatchesAbstract(ref, h.Name()) {
		return engine.Null, errIncompatible
	}
	return ref, nil
}

// fromRef returns reference ref as a Value's Ref.
func (r *runtime) fromRef(ref engine.Ref) any {
	if ref.IsNull() {
		return nil
	}
	if v, ok := ref.Host(); ok {
		return v
	}
	if f := ref.Func(); f != nil {
		return r.wrapFunction(f)
	}
	return &reference{r: ref}
}

// toVal returns v as a value of type t.
func (r *runtime) toVal(v api.Value, t module.ValueType, m *module.Module) (engine.Val, error) {
	if t.Kind != module.KindRef {
		return fromNumber(t, v.Lo, v.Hi), nil
	}
	ref, err := r.toRef(v.Ref, t, m)
	return engine.Val{R: ref}, err
}

// fromVal returns v, of type t, as a Value.
func (r *runtime) fromVal(v engine.Val, t module.ValueType, m *module.Module) api.Value {
	code := typeCode(t, m)
	switch t.Kind {
	case module.KindRef:
		return api.Value{Type: code, Ref: r.fromRef(v.R)}
	case module.KindI32, module.KindF32:
		return api.Value{Type: code, Lo: uint64(uint32(v.N))}
	case module.KindV128:
		return api.Value{Type: code, Lo: v.N, Hi: v.Hi}
	}
	return api.Value{Type: code, Lo: v.N}
}

// encodeArgs converts Call's params for a function of type ft.
func encodeArgs(ft *module.FuncType, params []uint64, args []engine.Val) error {
	j := 0
	for i, t := range ft.Params {
		switch {
		case t.Kind == module.KindV128:
			args[i] = engine.Val{N: params[j], Hi: params[j+1]}
			j += 2
			continue
		case t.Kind != module.KindRef:
			args[i] = fromNumber(t, params[j], 0)
		case params[j] == 0:
			if !t.Nullable {
				return fmt.Errorf("param[%d]: null for a non-nullable %s", i, t)
			}
			args[i] = engine.Val{}
		case t.Heap == module.HeapExtern:
			args[i] = engine.Val{R: engine.HostRef(uintptr(params[j]))}
		default:
			return fmt.Errorf("param[%d]: %s value other than null cannot be passed as a uint64: use CallValues", i, t)
		}
		j++
	}
	return nil
}

// decodeResults converts a function's results of types ts for Call.
func decodeResults(ts []module.ValueType, vals []engine.Val, out []uint64) error {
	j := 0
	for i, t := range ts {
		v := vals[i]
		switch {
		case t.Kind == module.KindV128:
			out[j], out[j+1] = v.N, v.Hi
			j += 2
			continue
		case t.Kind == module.KindI32 || t.Kind == module.KindF32:
			out[j] = uint64(uint32(v.N))
		case t.Kind != module.KindRef:
			out[j] = v.N
		default:
			n, err := externBits(v.R, t)
			if err != nil {
				return fmt.Errorf("result[%d]: %w", i, err)
			}
			out[j] = n
		}
		j++
	}
	return nil
}

// externBits encodes reference ref of type t as a uint64.
func externBits(ref engine.Ref, t module.ValueType) (uint64, error) {
	if ref.IsNull() {
		return 0, nil
	}
	if isExtern(t) {
		if v, ok := ref.Host(); ok {
			if p, ok := v.(uintptr); ok {
				return uint64(p), nil
			}
		}
	}
	return 0, fmt.Errorf("%s value that is not a uintptr cannot be returned as a uint64: use CallValues", t)
}

// externValue decodes uint64 n as a reference of type t.
func externValue(n uint64, t module.ValueType) (engine.Ref, error) {
	if n == 0 {
		if !t.Nullable {
			return engine.Null, fmt.Errorf("null for a non-nullable %s", t)
		}
		return engine.Null, nil
	}
	if t.Heap != module.HeapExtern {
		return engine.Null, fmt.Errorf("%s value other than null cannot be passed as a uint64", t)
	}
	return engine.HostRef(uintptr(n)), nil
}
