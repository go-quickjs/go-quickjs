package wasm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/go-quickjs/go-quickjs/wasm/api"
	"github.com/go-quickjs/go-quickjs/wasm/internal/decoder"
	"github.com/go-quickjs/go-quickjs/wasm/internal/engine"
	"github.com/go-quickjs/go-quickjs/wasm/internal/sysctx"
	"github.com/go-quickjs/go-quickjs/wasm/sys"
)

// Runtime compiles and instantiates modules, and holds the instantiated
// ones by name, which other modules import them by.
//
//	ctx := context.Background()
//	r := wasm.NewRuntime(ctx)
//	defer r.Close(ctx) // closes everything r made
//
//	mod, _ := r.Instantiate(ctx, binary)
//
// A Runtime and its CompiledModules may be used from several goroutines at
// once.
type Runtime interface {
	// Instantiate compiles binary and instantiates it with the default
	// ModuleConfig, which calls its exported "_start" function, if it has
	// one, after its start function.
	Instantiate(ctx context.Context, binary []byte) (api.Module, error)

	// InstantiateWithConfig compiles binary and instantiates it with
	// config. To instantiate one module several times, CompileModule it
	// once and InstantiateModule the result.
	InstantiateWithConfig(ctx context.Context, binary []byte, config ModuleConfig) (api.Module, error)

	// NewHostModuleBuilder begins a module of functions written in Go,
	// which wasm modules import by moduleName.
	NewHostModuleBuilder(moduleName string) HostModuleBuilder

	// CompileModule decodes and validates binary. A module that does not
	// decode or validate is a *CompileError. The runtime keeps a copy of
	// binary; the caller may change it afterwards.
	CompileModule(ctx context.Context, binary []byte) (CompiledModule, error)

	// InstantiateModule instantiates compiled under the name config gives,
	// resolving its imports against the modules the runtime holds by their
	// module names, runs its start function and then the start functions
	// config names.
	//
	// It fails with
	//   - an error if the name is in use;
	//   - a *LinkError if an import's module or name is not there, or what
	//     is there does not match the import;
	//   - a *RuntimeError if an active segment does not fit its table or
	//     memory, or the start function traps;
	//   - the error of a start function config names, wrapped, or its
	//     *sys.ExitError as it is; one of exit code zero is not a failure.
	InstantiateModule(ctx context.Context, compiled CompiledModule, config ModuleConfig) (api.Module, error)

	// CloseWithExitCode closes every module the runtime instantiated, with
	// exitCode, and the runtime: it compiles and instantiates nothing more.
	CloseWithExitCode(ctx context.Context, exitCode uint32) error

	// Module returns the module instantiated under moduleName, or nil.
	Module(moduleName string) api.Module

	// Closer closes the runtime with exit code zero.
	api.Closer
}

// NewRuntime returns a runtime of the default configuration,
// NewRuntimeConfig's.
func NewRuntime(ctx context.Context) Runtime {
	return NewRuntimeWithConfig(ctx, NewRuntimeConfig())
}

// NewRuntimeWithConfig returns a runtime of config.
func NewRuntimeWithConfig(ctx context.Context, config RuntimeConfig) Runtime {
	c, _ := config.(*runtimeConfig)
	if c == nil {
		c = defaultRuntimeConfig
	}
	return &runtime{
		cfg:     c.clone(),
		modules: map[string]*moduleInstance{},
		all:     map[*moduleInstance]struct{}{},
	}
}

type runtime struct {
	cfg *runtimeConfig

	mu sync.Mutex
	// modules are the named modules; a name being instantiated maps to nil.
	modules map[string]*moduleInstance
	// all are the open modules, named or not.
	all map[*moduleInstance]struct{}

	// instances maps the engine's instances to their modules, for the
	// caller a host function is given.
	instances sync.Map // *engine.Instance -> *moduleInstance
	// hostFuncs maps host functions to their definitions.
	hostFuncs sync.Map // *engine.Function -> *hostFunc

	// closed is zero while the runtime is open, and one more than its exit
	// code shifted left by 32 once it is closed.
	closed atomic.Uint64
}

func (r *runtime) failIfClosed() error {
	if c := r.closed.Load(); c != 0 {
		return fmt.Errorf("runtime closed with exit_code(%d)", uint32(c>>32))
	}
	return nil
}

func (r *runtime) Module(moduleName string) api.Module {
	if moduleName == "" {
		return nil
	}
	r.mu.Lock()
	m := r.modules[moduleName]
	r.mu.Unlock()
	if m == nil {
		return nil
	}
	return m
}

// moduleOf returns the module of instance inst, or nil.
func (r *runtime) moduleOf(inst *engine.Instance) *moduleInstance {
	if inst == nil {
		return nil
	}
	m, _ := r.instances.Load(inst)
	mi, _ := m.(*moduleInstance)
	return mi
}

// codeOf returns the compiled code instance inst runs, an instance of this
// runtime or of another.
func (r *runtime) codeOf(inst *engine.Instance) *compiledCode {
	if mi := r.moduleOf(inst); mi != nil && mi.code != nil {
		return mi.code
	}
	return &compiledCode{m: inst.Module}
}

func (r *runtime) CompileModule(ctx context.Context, binary []byte) (CompiledModule, error) {
	if err := r.failIfClosed(); err != nil {
		return nil, err
	}
	mem32, mem64 := r.cfg.memoryLimits()
	var key cacheKey
	if r.cfg.cache != nil {
		key = cacheKey{sum: sha256.Sum256(binary), mem32: mem32, mem64: mem64, compiler: r.cfg.compiler}
		if cc := r.cfg.cache.get(key); cc != nil {
			return &compiledModule{r: r, code: cc, custom: r.cfg.customSections}, nil
		}
	}
	lim := decoder.CoreLimits()
	lim.Memory32Pages = min(lim.Memory32Pages, mem32)
	lim.Memory64Pages = min(lim.Memory64Pages, mem64)
	b := bytes.Clone(binary)
	m, derr := decoder.Decode(b, lim)
	if derr != nil {
		return nil, compileError(derr)
	}
	if derr := decoder.ValidateFunctions(m, lim); derr != nil {
		return nil, compileError(derr)
	}
	// The runtime calls the start function itself, once the instance's
	// memories are bounded by the runtime's limit.
	run := *m
	run.HasStart = false
	cc := &compiledCode{m: &run, code: engine.NewCode(&run), start: m.Start, hasStart: m.HasStart}
	if r.cfg.compiler {
		cc.code.EnableJIT(engine.DefaultJITThreshold)
	}
	if r.cfg.cache != nil {
		cc = r.cfg.cache.put(key, cc)
	}
	return &compiledModule{r: r, code: cc, custom: r.cfg.customSections}, nil
}

func (r *runtime) Instantiate(ctx context.Context, binary []byte) (api.Module, error) {
	return r.InstantiateWithConfig(ctx, binary, NewModuleConfig())
}

func (r *runtime) InstantiateWithConfig(ctx context.Context, binary []byte, config ModuleConfig) (api.Module, error) {
	c, err := r.CompileModule(ctx, binary)
	if err != nil {
		return nil, err
	}
	return r.InstantiateModule(ctx, c, config)
}

// reserveName claims name for a module being instantiated.
func (r *runtime) reserveName(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if name == "" {
		return nil
	}
	if _, ok := r.modules[name]; ok {
		return fmt.Errorf("module[%s] has already been instantiated", name)
	}
	r.modules[name] = nil
	return nil
}

// releaseName frees name, claimed by a module whose instantiation failed.
func (r *runtime) releaseName(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if m, ok := r.modules[name]; ok && m == nil {
		delete(r.modules, name)
	}
}

// register adds mod to the runtime's modules.
func (r *runtime) register(mod *moduleInstance) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if mod.name != "" {
		r.modules[mod.name] = mod
	}
	r.all[mod] = struct{}{}
	if mod.inst != nil {
		r.instances.Store(mod.inst, mod)
	}
}

// unregister removes mod from the runtime's modules.
func (r *runtime) unregister(mod *moduleInstance) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if mod.name != "" && r.modules[mod.name] == mod {
		delete(r.modules, mod.name)
	}
	delete(r.all, mod)
	if mod.inst != nil {
		r.instances.Delete(mod.inst)
	}
	for _, fn := range mod.host {
		r.hostFuncs.Delete(fn)
	}
}

func (r *runtime) InstantiateModule(ctx context.Context, compiled CompiledModule, config ModuleConfig) (api.Module, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := r.failIfClosed(); err != nil {
		return nil, err
	}
	c, ok := compiled.(*compiledModule)
	if !ok {
		return nil, errors.New("wasm: CompiledModule of another package")
	}
	if c.r != r {
		return nil, errors.New("wasm: CompiledModule of another Runtime")
	}
	cfg, _ := config.(*moduleConfig)
	if cfg == nil {
		cfg = NewModuleConfig().(*moduleConfig)
	}
	name := c.Name()
	if cfg.nameSet {
		name = cfg.name
	}
	if c.host != nil {
		return r.instantiateHost(c.host, name)
	}
	sc, err := sysctx.New(&cfg.sys)
	if err != nil {
		return nil, err
	}
	if err := r.reserveName(name); err != nil {
		_ = sc.Close()
		return nil, err
	}
	mod, err := r.instantiate(ctx, c.code, name, sc)
	if err != nil {
		_ = sc.Close()
		r.releaseName(name)
		return nil, err
	}
	for _, fn := range cfg.startFunctions {
		f := mod.ExportedFunction(fn)
		if f == nil {
			continue
		}
		if _, err := f.Call(ctx); err != nil {
			_ = mod.Close(ctx)
			var exit *sys.ExitError
			if errors.As(err, &exit) {
				if exit.ExitCode() == 0 {
					return mod, nil
				}
				return nil, exit
			}
			return nil, fmt.Errorf("module[%s] function[%s] failed: %w", name, fn, err)
		}
	}
	return mod, nil
}

// instantiate instantiates code as name: links its imports, bounds its
// memories, registers it and runs its start function.
func (r *runtime) instantiate(ctx context.Context, cc *compiledCode, name string, sc *sysctx.Context) (*moduleInstance, error) {
	m := cc.m
	imports := make([]engine.Extern, len(m.Imports))
	for i, imp := range m.Imports {
		r.mu.Lock()
		dep := r.modules[imp.Module]
		r.mu.Unlock()
		if dep == nil {
			return nil, &LinkError{fmt.Sprintf("Import #%d %q %q: module not found", i, imp.Module, imp.Name)}
		}
		ext, ok := dep.export(imp.Name)
		if !ok {
			return nil, &LinkError{fmt.Sprintf("Import #%d %q %q: import not found", i, imp.Module, imp.Name)}
		}
		imports[i] = ext
	}
	inst, err := engine.Instantiate(ctx, cc.code, imports)
	if inst == nil && err != nil {
		switch e := err.(type) {
		case *engine.LinkError:
			return nil, &LinkError{e.Msg}
		case *engine.AllocError:
			return nil, errors.New(e.Msg)
		case *engine.Trap:
			return nil, r.runtimeError(e)
		}
		return nil, err
	}
	inst.Name = name
	mod := &moduleInstance{r: r, name: name, code: cc, inst: inst, sys: sc}
	if err != nil {
		// What the instance initialized is seen through its imports, but
		// the instance itself is not kept.
		if t, ok := err.(*engine.Trap); ok {
			return nil, r.runtimeError(t)
		}
		return nil, err
	}
	mem32, mem64 := r.cfg.memoryLimits()
	for i, decl := range m.Memories {
		if decl.Imported {
			continue
		}
		mem := inst.Memories[i]
		limit := mem32
		if mem.Is64 {
			limit = mem64
		}
		mem.Max = min(mem.Max, limit)
		if r.cfg.capacityFromMax {
			mem.ReserveCapacity(mem.Max)
		}
	}
	r.register(mod)
	if cc.hasStart {
		if _, err := mod.call(ctx, inst.Funcs[cc.start], nil); err != nil {
			r.unregister(mod)
			mod.closed.Store(1)
			return nil, err
		}
	}
	return mod, nil
}

func (r *runtime) NewHostModuleBuilder(moduleName string) HostModuleBuilder {
	return &hostModuleBuilder{r: r, name: moduleName}
}

func (r *runtime) Close(ctx context.Context) error { return r.CloseWithExitCode(ctx, 0) }

func (r *runtime) CloseWithExitCode(ctx context.Context, exitCode uint32) error {
	if !r.closed.CompareAndSwap(0, 1+uint64(exitCode)<<32) {
		return nil
	}
	r.mu.Lock()
	mods := make([]*moduleInstance, 0, len(r.all))
	for m := range r.all {
		mods = append(mods, m)
	}
	r.mu.Unlock()
	for _, m := range mods {
		_ = m.CloseWithExitCode(ctx, exitCode)
	}
	return nil
}
