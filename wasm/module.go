package wasm

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/go-quickjs/go-quickjs/wasm/api"
	"github.com/go-quickjs/go-quickjs/wasm/internal/engine"
	"github.com/go-quickjs/go-quickjs/wasm/internal/internalapi"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/sysctx"
	"github.com/go-quickjs/go-quickjs/wasm/sys"
)

// moduleInstance is an api.Module: a wasm module's instance, or a host
// module's functions.
type moduleInstance struct {
	internalapi.WasmOnlyType
	r    *runtime
	name string

	// code and inst are a wasm module's; host is a host module's
	// functions by export name.
	code *compiledCode
	inst *engine.Instance
	host map[string]*engine.Function

	sys *sysctx.Context

	// closed is zero while the module is open, and one more than its exit
	// code shifted left by 32 once it is closed.
	closed atomic.Uint64

	// calls are the cancel functions of the calls running under
	// WithCloseOnContextDone, which closing the module cancels.
	mu    sync.Mutex
	calls map[*context.CancelFunc]struct{}
}

var _ sysctx.Provider = (*moduleInstance)(nil)

// SysContext returns the module's system configuration.
func (m *moduleInstance) SysContext() *sysctx.Context { return m.sys }

func (m *moduleInstance) String() string { return fmt.Sprintf("Module[%s]", m.name) }

func (m *moduleInstance) Name() string { return m.name }

// export returns the entity exported as name.
func (m *moduleInstance) export(name string) (engine.Extern, bool) {
	if m.host != nil {
		f, ok := m.host[name]
		return engine.Extern{Kind: module.ExternFunc, Func: f}, ok
	}
	return m.inst.Export(name)
}

// exportIndex returns the index of the entity of kind k exported as name.
func (m *moduleInstance) exportIndex(name string, k module.ExternKind) (uint32, bool) {
	if m.inst == nil {
		return 0, false
	}
	for _, e := range m.inst.Module.Exports {
		if e.Name == name && e.Kind == k {
			return e.Index, true
		}
	}
	return 0, false
}

func (m *moduleInstance) Memory() api.Memory {
	if m.inst == nil || len(m.inst.Memories) == 0 {
		return nil
	}
	return m.memory(0)
}

func (m *moduleInstance) memory(index uint32) api.Memory {
	return &memory{mem: m.inst.Memories[index], code: m.code, index: index}
}

func (m *moduleInstance) ExportedFunction(name string) api.Function {
	ext, ok := m.export(name)
	if !ok || ext.Kind != module.ExternFunc {
		return nil
	}
	f := m.r.wrapFunction(ext.Func)
	if f.mod == nil && (m.inst != nil && ext.Func.Inst == m.inst || m.host != nil && ext.Func.Inst == nil) {
		// The module is closed, and no longer the runtime's: its
		// functions still answer for it.
		f.mod = m
	}
	return f
}

func (m *moduleInstance) ExportedFunctionDefinitions() map[string]api.FunctionDefinition {
	if m.host != nil {
		out := map[string]api.FunctionDefinition{}
		for name, f := range m.host {
			out[name] = m.r.wrapFunction(f).Definition()
		}
		return out
	}
	return (&compiledModule{r: m.r, code: m.code}).ExportedFunctions()
}

func (m *moduleInstance) ExportedMemory(name string) api.Memory {
	i, ok := m.exportIndex(name, module.ExternMemory)
	if !ok {
		return nil
	}
	return m.memory(i)
}

func (m *moduleInstance) ExportedMemoryDefinitions() map[string]api.MemoryDefinition {
	if m.host != nil {
		return map[string]api.MemoryDefinition{}
	}
	return (&compiledModule{r: m.r, code: m.code}).ExportedMemories()
}

func (m *moduleInstance) ExportedGlobal(name string) api.Global {
	i, ok := m.exportIndex(name, module.ExternGlobal)
	if !ok {
		return nil
	}
	g := &global{r: m.r, g: m.inst.Globals[i]}
	if g.g.Mutable {
		return &mutableGlobal{g}
	}
	return g
}

func (m *moduleInstance) ExportedTable(name string) api.Table {
	i, ok := m.exportIndex(name, module.ExternTable)
	if !ok {
		return nil
	}
	return &table{r: m.r, t: m.inst.Tables[i]}
}

func (m *moduleInstance) IsClosed() bool { return m.closed.Load() != 0 }

// exitCode returns the code the module was closed with, and whether it was.
func (m *moduleInstance) exitCode() (uint32, bool) {
	c := m.closed.Load()
	return uint32(c >> 32), c != 0
}

func (m *moduleInstance) Close(ctx context.Context) error { return m.CloseWithExitCode(ctx, 0) }

func (m *moduleInstance) CloseWithExitCode(ctx context.Context, exitCode uint32) error {
	if !m.closed.CompareAndSwap(0, 1+uint64(exitCode)<<32) {
		return nil
	}
	m.r.unregister(m)
	if m.sys != nil {
		_ = m.sys.Close()
	}
	m.mu.Lock()
	for cancel := range m.calls {
		(*cancel)()
	}
	m.mu.Unlock()
	return nil
}

// enter begins a call of one of the module's functions under ctx, returning
// the context to run it under and what ends it.
func (m *moduleInstance) enter(ctx context.Context) (context.Context, func()) {
	if !m.r.cfg.closeOnContextDone {
		return ctx, func() {}
	}
	cctx, cancel := context.WithCancel(ctx)
	m.mu.Lock()
	if m.calls == nil {
		m.calls = map[*context.CancelFunc]struct{}{}
	}
	m.calls[&cancel] = struct{}{}
	m.mu.Unlock()
	if m.IsClosed() {
		cancel()
	}
	return cctx, func() {
		m.mu.Lock()
		delete(m.calls, &cancel)
		m.mu.Unlock()
		cancel()
	}
}

// call calls fn, a function of m or one m holds, and converts its error.
func (m *moduleInstance) call(ctx context.Context, fn *engine.Function, args []engine.Val) ([]engine.Val, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if code, closed := m.exitCode(); closed {
		return nil, sys.NewExitError(code)
	}
	runCtx, done := m.enter(ctx)
	res, err := engine.Invoke(runCtx, fn, args)
	done()
	if err != nil {
		return nil, m.r.convert(ctx, m, err)
	}
	return res, nil
}

// convert converts an error a call of one of m's functions returned; m is
// nil for a function of no module of r's.
func (r *runtime) convert(ctx context.Context, m *moduleInstance, err error) error {
	var exit *sys.ExitError
	if errors.As(err, &exit) {
		return exit
	}
	if m != nil && r.cfg.closeOnContextDone && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		if code, closed := m.exitCode(); closed {
			return sys.NewExitError(code)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			code := sys.ExitCodeContextCanceled
			if errors.Is(ctxErr, context.DeadlineExceeded) {
				code = sys.ExitCodeDeadlineExceeded
			}
			_ = m.CloseWithExitCode(ctx, code)
			return sys.NewExitError(code)
		}
	}
	switch e := err.(type) {
	case *engine.Trap:
		return r.runtimeError(e)
	case *engine.Exception:
		vals := make([]api.Value, len(e.Vals))
		for i, v := range e.Vals {
			vals[i] = r.fromVal(v, e.Tag.Sig.Params[i], nil)
		}
		return &Exception{Values: vals}
	}
	return err
}

// function is an api.Function.
type function struct {
	internalapi.WasmOnlyType
	r   *runtime
	mod *moduleInstance // nil for a function of no module of r's
	fn  *engine.Function
}

// wrapFunction returns fn as an api.Function.
func (r *runtime) wrapFunction(fn *engine.Function) *function {
	f := &function{r: r, fn: fn}
	if fn.Inst != nil {
		f.mod = r.moduleOf(fn.Inst)
	} else if h, ok := r.hostFuncs.Load(fn); ok {
		f.mod = h.(*hostFunc).mod
	}
	return f
}

func (f *function) Definition() api.FunctionDefinition {
	if f.fn.Inst == nil {
		if h, ok := f.r.hostFuncs.Load(f.fn); ok {
			return h.(*hostFunc).def
		}
		sig := f.fn.Sig
		return &functionDefinition{name: f.fn.Name, params: typeCodes(sig.Params, nil), results: typeCodes(sig.Results, nil)}
	}
	cc := f.r.codeOf(f.fn.Inst)
	return cc.funcDefinition(f.fn.Index, exportNamesOf(cc.m, module.ExternFunc))
}

// invoke calls the function, converting the error.
func (f *function) invoke(ctx context.Context, args []engine.Val) ([]engine.Val, error) {
	if f.mod == nil {
		// A function of another runtime's or of none.
		if ctx == nil {
			ctx = context.Background()
		}
		res, err := engine.Invoke(ctx, f.fn, args)
		if err != nil {
			return nil, f.r.convert(ctx, nil, err)
		}
		return res, nil
	}
	return f.mod.call(ctx, f.fn, args)
}

func (f *function) Call(ctx context.Context, params ...uint64) ([]uint64, error) {
	sig := f.fn.Sig
	if n := slots(sig.Params); len(params) != n {
		return nil, fmt.Errorf("expected %d params, but passed %d", n, len(params))
	}
	args := make([]engine.Val, len(sig.Params))
	if err := encodeArgs(sig, params, args); err != nil {
		return nil, err
	}
	res, err := f.invoke(ctx, args)
	if err != nil {
		return nil, err
	}
	out := make([]uint64, slots(sig.Results))
	if err := decodeResults(sig.Results, res, out); err != nil {
		return nil, err
	}
	return out, nil
}

func (f *function) CallWithStack(ctx context.Context, stack []uint64) error {
	sig := f.fn.Sig
	np, nr := slots(sig.Params), slots(sig.Results)
	if len(stack) < max(np, nr) {
		return fmt.Errorf("need %d params, but stack size is %d", max(np, nr), len(stack))
	}
	args := make([]engine.Val, len(sig.Params))
	if err := encodeArgs(sig, stack[:np], args); err != nil {
		return err
	}
	res, err := f.invoke(ctx, args)
	if err != nil {
		return err
	}
	return decodeResults(sig.Results, res, stack)
}

func (f *function) CallValues(ctx context.Context, params ...api.Value) ([]api.Value, error) {
	sig := f.fn.Sig
	if len(params) != len(sig.Params) {
		return nil, fmt.Errorf("expected %d params, but passed %d", len(sig.Params), len(params))
	}
	args := make([]engine.Val, len(params))
	for i, p := range params {
		v, err := f.r.toVal(p, sig.Params[i], nil)
		if err != nil {
			return nil, fmt.Errorf("param[%d]: %w", i, err)
		}
		args[i] = v
	}
	res, err := f.invoke(ctx, args)
	if err != nil {
		return nil, err
	}
	out := make([]api.Value, len(res))
	for i, v := range res {
		out[i] = f.r.fromVal(v, sig.Results[i], nil)
	}
	return out, nil
}

// global is an api.Global.
type global struct {
	internalapi.WasmOnlyType
	r *runtime
	g *engine.Global
}

func (g *global) Type() api.ValueType { return typeCode(g.g.Type, g.g.Module) }

func (g *global) Get() uint64 {
	t := g.g.Type
	switch t.Kind {
	case module.KindI32, module.KindF32:
		return uint64(uint32(g.g.Num))
	case module.KindRef:
		n, err := externBits(g.g.Ref, t)
		if err != nil {
			return 1
		}
		return n
	}
	return g.g.Num
}

func (g *global) GetValue() api.Value {
	return g.r.fromVal(engine.Val{N: g.g.Num, Hi: g.g.Hi, R: g.g.Ref}, g.g.Type, g.g.Module)
}

func (g *global) String() string {
	switch g.g.Type.Kind {
	case module.KindI32, module.KindI64:
		return fmt.Sprintf("global(%d)", g.Get())
	case module.KindF32:
		return fmt.Sprintf("global(%f)", api.DecodeF32(g.Get()))
	case module.KindF64:
		return fmt.Sprintf("global(%f)", api.DecodeF64(g.Get()))
	case module.KindV128:
		return fmt.Sprintf("global(%#x, %#x)", g.g.Num, g.g.Hi)
	}
	return fmt.Sprintf("global(%v)", g.GetValue().Ref)
}

// mutableGlobal is an api.MutableGlobal.
type mutableGlobal struct{ *global }

func (g *mutableGlobal) Set(v uint64) {
	t := g.g.Type
	switch t.Kind {
	case module.KindRef:
		if ref, err := externValue(v, t); err == nil {
			g.g.Ref = ref
		}
	default:
		val := fromNumber(t, v, 0)
		g.g.Num, g.g.Hi = val.N, 0
	}
}

func (g *mutableGlobal) SetValue(v api.Value) error {
	val, err := g.r.toVal(v, g.g.Type, g.g.Module)
	if err != nil {
		return err
	}
	g.g.Num, g.g.Hi, g.g.Ref = val.N, val.Hi, val.R
	return nil
}
