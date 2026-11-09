package wasm

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/go-quickjs/go-quickjs/wasm/api"
	"github.com/go-quickjs/go-quickjs/wasm/internal/engine"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
)

// HostFunctionBuilder defines a function written in Go for wasm modules to
// import.
//
//	builder.NewFunctionBuilder().
//		WithFunc(func(ctx context.Context, x, y uint32) uint32 {
//			return x + y
//		}).
//		Export("add")
//
// A host function acts on the module that calls it: the api.Module it is
// given is the caller, and its Memory the caller's memory.
type HostFunctionBuilder interface {
	// WithGoFunction implements the function with fn, which reads its
	// parameters from a stack of uint64s and writes its results there.
	// params and results are its signature, of the numeric types,
	// ValueTypeV128, which takes two of the stack's uint64s, and
	// ValueTypeExternref.
	WithGoFunction(fn api.GoFunction, params, results []api.ValueType) HostFunctionBuilder

	// WithGoModuleFunction is WithGoFunction for a function that is given
	// the module that calls it.
	WithGoModuleFunction(fn api.GoModuleFunction, params, results []api.ValueType) HostFunctionBuilder

	// WithFunc implements the function with a Go func, whose signature,
	// found by reflection, gives the function's: optionally a
	// context.Context and an api.Module first, in that order, then
	// parameters, and results, each a uint32 or int32 (i32), uint64 or
	// int64 (i64), float32 (f32), float64 (f64), or uintptr (externref).
	// Anything else makes Compile fail.
	//
	//	builder.WithFunc(func(ctx context.Context, m api.Module, offset uint32) uint32 {
	//		x, _ := m.Memory().ReadUint32Le(offset)
	//		return x
	//	})
	WithFunc(fn interface{}) HostFunctionBuilder

	// WithName sets the function's name, which stack traces and
	// definitions show; it need not be the export name.
	WithName(name string) HostFunctionBuilder

	// WithParameterNames names the parameters, all of them.
	WithParameterNames(names ...string) HostFunctionBuilder

	// WithResultNames names the results, all of them.
	WithResultNames(names ...string) HostFunctionBuilder

	// Export adds the function to the module as name, replacing a function
	// exported as name before, and returns the module's builder.
	Export(name string) HostModuleBuilder
}

// HostModuleBuilder defines a module of functions written in Go, for wasm
// modules to import by its name.
//
//	env, err := r.NewHostModuleBuilder("env").
//		NewFunctionBuilder().WithFunc(hello).Export("hello").
//		Instantiate(ctx)
//
// A builder is mutable: each method returns it, for chaining. Errors in the
// definitions are reported by Compile. Functions are numbered in the order
// they were first exported.
type HostModuleBuilder interface {
	// NewFunctionBuilder begins a function.
	NewFunctionBuilder() HostFunctionBuilder

	// Compile returns the module, which the runtime's InstantiateModule
	// instantiates, under another name if the ModuleConfig gives one.
	Compile(ctx context.Context) (CompiledModule, error)

	// Instantiate compiles the module and instantiates it under its name.
	// The functions are copied: later changes to the builder do not change
	// the module.
	Instantiate(ctx context.Context) (api.Module, error)
}

type hostModuleBuilder struct {
	r     *runtime
	name  string
	funcs []*hostFunctionBuilder
}

type hostFunctionBuilder struct {
	b               *hostModuleBuilder
	fn              interface{} // api.GoFunction, api.GoModuleFunction or a func for WithFunc
	params, results []api.ValueType
	typed           bool
	name            string
	paramNames      []string
	resultNames     []string
	exportName      string
}

func (b *hostModuleBuilder) NewFunctionBuilder() HostFunctionBuilder {
	return &hostFunctionBuilder{b: b}
}

func (h *hostFunctionBuilder) WithGoFunction(fn api.GoFunction, params, results []api.ValueType) HostFunctionBuilder {
	h.fn, h.params, h.results, h.typed = fn, params, results, true
	return h
}

func (h *hostFunctionBuilder) WithGoModuleFunction(fn api.GoModuleFunction, params, results []api.ValueType) HostFunctionBuilder {
	h.fn, h.params, h.results, h.typed = fn, params, results, true
	return h
}

func (h *hostFunctionBuilder) WithFunc(fn interface{}) HostFunctionBuilder {
	h.fn, h.params, h.results, h.typed = fn, nil, nil, false
	return h
}

func (h *hostFunctionBuilder) WithName(name string) HostFunctionBuilder {
	h.name = name
	return h
}

func (h *hostFunctionBuilder) WithParameterNames(names ...string) HostFunctionBuilder {
	h.paramNames = names
	return h
}

func (h *hostFunctionBuilder) WithResultNames(names ...string) HostFunctionBuilder {
	h.resultNames = names
	return h
}

func (h *hostFunctionBuilder) Export(name string) HostModuleBuilder {
	c := *h
	c.exportName = name
	b := h.b
	for i, f := range b.funcs {
		if f.exportName == name {
			b.funcs[i] = &c
			return b
		}
	}
	b.funcs = append(b.funcs, &c)
	return b
}

// hostModule is a compiled host module.
type hostModule struct {
	name  string
	funcs []*hostFunc
}

// hostFunc is a compiled host function.
type hostFunc struct {
	def  *functionDefinition
	ft   *module.FuncType
	call api.GoModuleFunction
	// mod is the host module of an instantiated function.
	mod *moduleInstance
}

func (b *hostModuleBuilder) Compile(ctx context.Context) (CompiledModule, error) {
	if err := b.r.failIfClosed(); err != nil {
		return nil, err
	}
	if b.name == "" {
		return nil, errors.New("host module name must not be empty")
	}
	hm := &hostModule{name: b.name}
	for i, h := range b.funcs {
		hf, err := h.compile()
		if err != nil {
			return nil, fmt.Errorf("func[%s.%s] %w", b.name, h.exportName, err)
		}
		hf.def.moduleName = b.name
		hf.def.index = uint32(i)
		hm.funcs = append(hm.funcs, hf)
	}
	return &compiledModule{r: b.r, host: hm}, nil
}

func (b *hostModuleBuilder) Instantiate(ctx context.Context) (api.Module, error) {
	c, err := b.Compile(ctx)
	if err != nil {
		return nil, err
	}
	return b.r.InstantiateModule(ctx, c, NewModuleConfig())
}

// moduleValueType returns the module's type of host type code t.
func moduleValueType(t api.ValueType) (module.ValueType, error) {
	switch t {
	case api.ValueTypeI32:
		return module.I32, nil
	case api.ValueTypeI64:
		return module.I64, nil
	case api.ValueTypeF32:
		return module.F32, nil
	case api.ValueTypeF64:
		return module.F64, nil
	case api.ValueTypeV128:
		return module.V128, nil
	case api.ValueTypeExternref:
		return module.ExternRef, nil
	}
	return module.ValueType{}, fmt.Errorf("value type %s (%#x) is not supported by host functions", api.ValueTypeName(t), t)
}

func (h *hostFunctionBuilder) compile() (*hostFunc, error) {
	hf := &hostFunc{}
	params, results := h.params, h.results
	switch fn := h.fn.(type) {
	case nil:
		return nil, errors.New("has no implementation")
	case api.GoModuleFunction:
		hf.call = fn
	case api.GoFunction:
		hf.call = api.GoModuleFunc(func(ctx context.Context, _ api.Module, stack []uint64) { fn.Call(ctx, stack) })
	default:
		if h.typed {
			return nil, fmt.Errorf("implementation %T is not a GoFunction", fn)
		}
		rf, err := reflectFunc(fn)
		if err != nil {
			return nil, err
		}
		hf.call, params, results = rf, rf.params, rf.results
	}
	ft := &module.FuncType{}
	for _, t := range params {
		mt, err := moduleValueType(t)
		if err != nil {
			return nil, err
		}
		ft.Params = append(ft.Params, mt)
	}
	for _, t := range results {
		mt, err := moduleValueType(t)
		if err != nil {
			return nil, err
		}
		ft.Results = append(ft.Results, mt)
	}
	if h.paramNames != nil && len(h.paramNames) != len(params) {
		return nil, fmt.Errorf("has %d params but %d param names", len(params), len(h.paramNames))
	}
	if h.resultNames != nil && len(h.resultNames) != len(results) {
		return nil, fmt.Errorf("has %d results but %d result names", len(results), len(h.resultNames))
	}
	hf.ft = ft
	hf.def = &functionDefinition{
		exportDefinition: exportDefinition{exportNames: []string{h.exportName}},
		name:             h.name,
		goFunc:           hf.call,
		params:           append([]api.ValueType{}, params...),
		results:          append([]api.ValueType{}, results...),
		paramNames:       h.paramNames,
		resultNames:      h.resultNames,
	}
	if hf.def.name == "" {
		hf.def.name = h.exportName
	}
	return hf, nil
}

// instantiateHost instantiates host module hm as name.
func (r *runtime) instantiateHost(hm *hostModule, name string) (api.Module, error) {
	if err := r.reserveName(name); err != nil {
		return nil, err
	}
	mod := &moduleInstance{r: r, name: name, host: map[string]*engine.Function{}}
	for _, hf := range hm.funcs {
		inst := *hf
		inst.mod = mod
		def := *hf.def
		def.moduleName = name
		inst.def = &def
		fn := engine.NewHostFunction(module.CanonicalFunc(hf.ft), name+"."+def.name, r.hostCall(&inst))
		r.hostFuncs.Store(fn, &inst)
		mod.host[def.exportNames[0]] = fn
	}
	r.register(mod)
	return mod, nil
}

// hostCall returns the engine's implementation of host function hf.
func (r *runtime) hostCall(hf *hostFunc) engine.HostFunc {
	ft := hf.ft
	n := max(slots(ft.Params), slots(ft.Results))
	return func(ctx context.Context, caller *engine.Instance, args, results []engine.Val) (err error) {
		stack := make([]uint64, n)
		j := 0
		for i, t := range ft.Params {
			v := args[i]
			switch t.Kind {
			case module.KindV128:
				stack[j], stack[j+1] = v.N, v.Hi
				j += 2
				continue
			case module.KindRef:
				bits, err := externBits(v.R, t)
				if err != nil {
					return fmt.Errorf("param[%d]: %w", i, err)
				}
				stack[j] = bits
			case module.KindI32, module.KindF32:
				stack[j] = uint64(uint32(v.N))
			default:
				stack[j] = v.N
			}
			j++
		}
		var mod api.Module
		if m := r.moduleOf(caller); m != nil {
			mod = m
		}
		defer func() {
			if p := recover(); p != nil {
				if e, ok := p.(error); ok {
					err = e
				} else {
					err = fmt.Errorf("%v", p)
				}
			}
		}()
		hf.call.Call(ctx, mod, stack)
		j = 0
		for i, t := range ft.Results {
			switch t.Kind {
			case module.KindV128:
				results[i] = engine.Val{N: stack[j], Hi: stack[j+1]}
				j += 2
				continue
			case module.KindRef:
				ref, err := externValue(stack[j], t)
				if err != nil {
					return fmt.Errorf("result[%d]: %w", i, err)
				}
				results[i] = engine.Val{R: ref}
			default:
				results[i] = fromNumber(t, stack[j], 0)
			}
			j++
		}
		return nil
	}
}

// reflectFunction is a Go func called by reflection.
type reflectFunction struct {
	fn              reflect.Value
	hasCtx, hasMod  bool
	params, results []api.ValueType
	kinds           []reflect.Type
}

var (
	contextType = reflect.TypeOf((*context.Context)(nil)).Elem()
	moduleType  = reflect.TypeOf((*api.Module)(nil)).Elem()
)

func reflectValueType(t reflect.Type) (api.ValueType, bool) {
	switch t.Kind() {
	case reflect.Uint32, reflect.Int32:
		return api.ValueTypeI32, true
	case reflect.Uint64, reflect.Int64:
		return api.ValueTypeI64, true
	case reflect.Float32:
		return api.ValueTypeF32, true
	case reflect.Float64:
		return api.ValueTypeF64, true
	case reflect.Uintptr:
		return api.ValueTypeExternref, true
	}
	return 0, false
}

func reflectFunc(fn interface{}) (*reflectFunction, error) {
	v := reflect.ValueOf(fn)
	t := v.Type()
	if t.Kind() != reflect.Func {
		return nil, fmt.Errorf("kind != func: %s", t.Kind())
	}
	rf := &reflectFunction{fn: v}
	i := 0
	if i < t.NumIn() && t.In(i) == contextType {
		rf.hasCtx = true
		i++
	}
	if i < t.NumIn() && t.In(i) == moduleType {
		rf.hasMod = true
		i++
	}
	for ; i < t.NumIn(); i++ {
		in := t.In(i)
		vt, ok := reflectValueType(in)
		if !ok {
			switch {
			case in == contextType:
				return nil, fmt.Errorf("param[%d] is a context.Context, which must be the first parameter", i)
			case in == moduleType:
				return nil, fmt.Errorf("param[%d] is an api.Module, which must come first or after the context.Context", i)
			}
			return nil, fmt.Errorf("param[%d] is unsupported: %s", i, in.Kind())
		}
		rf.params = append(rf.params, vt)
		rf.kinds = append(rf.kinds, in)
	}
	for i := 0; i < t.NumOut(); i++ {
		vt, ok := reflectValueType(t.Out(i))
		if !ok {
			return nil, fmt.Errorf("result[%d] is unsupported: %s", i, t.Out(i).Kind())
		}
		rf.results = append(rf.results, vt)
	}
	return rf, nil
}

// Call calls the func with the parameters on stack and writes its results
// there.
func (rf *reflectFunction) Call(ctx context.Context, mod api.Module, stack []uint64) {
	in := make([]reflect.Value, 0, len(rf.kinds)+2)
	if rf.hasCtx {
		if ctx == nil {
			ctx = context.Background()
		}
		in = append(in, reflect.ValueOf(&ctx).Elem())
	}
	if rf.hasMod {
		mv := reflect.New(moduleType).Elem()
		if mod != nil {
			mv.Set(reflect.ValueOf(mod))
		}
		in = append(in, mv)
	}
	for i, k := range rf.kinds {
		v := reflect.New(k).Elem()
		switch k.Kind() {
		case reflect.Uint32:
			v.SetUint(uint64(uint32(stack[i])))
		case reflect.Int32:
			v.SetInt(int64(int32(stack[i])))
		case reflect.Uint64, reflect.Uintptr:
			v.SetUint(stack[i])
		case reflect.Int64:
			v.SetInt(int64(stack[i]))
		case reflect.Float32:
			v.SetFloat(float64(api.DecodeF32(stack[i])))
		case reflect.Float64:
			v.SetFloat(api.DecodeF64(stack[i]))
		}
		in = append(in, v)
	}
	out := rf.fn.Call(in)
	for i, v := range out {
		switch v.Kind() {
		case reflect.Uint32:
			stack[i] = uint64(uint32(v.Uint()))
		case reflect.Int32:
			stack[i] = uint64(uint32(int32(v.Int())))
		case reflect.Uint64, reflect.Uintptr:
			stack[i] = v.Uint()
		case reflect.Int64:
			stack[i] = uint64(v.Int())
		case reflect.Float32:
			stack[i] = api.EncodeF32(float32(v.Float()))
		case reflect.Float64:
			stack[i] = api.EncodeF64(v.Float())
		}
	}
}
