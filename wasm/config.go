package wasm

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"sync"
	"time"

	"github.com/go-quickjs/go-quickjs/wasm/api"
	"github.com/go-quickjs/go-quickjs/wasm/internal/engine"
	"github.com/go-quickjs/go-quickjs/wasm/internal/sysctx"
	"github.com/go-quickjs/go-quickjs/wasm/sys"
)

// RuntimeConfig configures a Runtime. It is immutable: each With method
// returns a new RuntimeConfig with the change.
type RuntimeConfig interface {
	// WithCoreFeatures records the features the embedder asks for. The
	// engine implements every feature of WebAssembly 3.0 and the threads
	// proposal and turns none off, so a module using a feature outside
	// features still compiles; CoreFeatures reports the set. The default is
	// api.CoreFeaturesAll.
	WithCoreFeatures(features api.CoreFeatures) RuntimeConfig

	// CoreFeatures returns the features WithCoreFeatures recorded.
	CoreFeatures() api.CoreFeatures

	// WithMemoryLimitPages caps every memory of the runtime's modules at
	// memoryLimitPages pages of 64 KiB, for memories with i32 and i64
	// addresses alike. A module that declares a memory, defined or
	// imported, with an initial size or a maximum over the limit fails to
	// compile with a *CompileError; a memory declared without a maximum
	// grows to the limit at most.
	//
	// The default is the engine's own limit: 65,536 pages (4 GiB) for an
	// i32 memory and 262,144 (16 GiB) for an i64 one on 64-bit platforms,
	// 32,767 for either on 32-bit ones. A limit above the larger of them
	// panics.
	WithMemoryLimitPages(memoryLimitPages uint32) RuntimeConfig

	// WithMemoryLimit bounds what the runtime's modules hold together, in
	// bytes: their linear memories and tables, counted exactly, and the GC
	// objects they can reach, counted as they are allocated and counted
	// again, from the modules' globals, tables and running frames, when the
	// total would pass the limit. A memory.grow or table.grow past it
	// returns -1, as a grow that cannot be had does; instantiating a module
	// whose memories do not fit fails; a GC allocation that does not fit
	// stops the call with ErrMemoryLimit. What a closed module held is given
	// back once Go collects it. The count of GC objects is an estimate: it
	// may pass the limit by a sixteenth of it before it is found, and what
	// only a call running on another goroutine holds is counted at that
	// call's own next count. 0, the default, is no limit.
	WithMemoryLimit(bytes int64) RuntimeConfig

	// WithJITMemoryLimit bounds what the compiler (NewRuntimeConfigCompiler)
	// holds for the runtime's modules together, in bytes: their functions'
	// native code and what describes it. It is apart from WithMemoryLimit,
	// which never counts it. When a compile would pass the limit, the
	// compiler first drops code no call is running -- code the optimizing
	// tier replaced, and the code of functions not called for 100 ms or
	// more, which their next call compiles again -- and where that is not
	// enough runs the function in the interpreter for now, compiling it
	// once there is room. Results are the same whatever the limit; a limit
	// smaller than what a program's hot functions need makes it slower, as
	// far as the interpreter's speed.
	//
	// The default, DefaultJITMemoryLimit, is 64 MiB, which holds every
	// function of a large program compiled by both tiers: a Go program of
	// 3,028 functions holds 23 MB with each compiled by the baseline, 34 MB
	// by the optimizing tier, and a run of one compiles some 550 to 800 of
	// them, 3.9 to 8.2 MB on amd64. 0 keeps the default; a negative limit
	// panics. Runtimes that share a compilation cache share their modules'
	// code, and so the limit: those of the same limit share one budget of
	// it. It does nothing for an interpreter's configuration.
	WithJITMemoryLimit(bytes int64) RuntimeConfig

	// WithMemoryCapacityFromMax makes each memory hold its maximum, or the
	// runtime's limit for a memory without one, from its instantiation, so
	// that a grow never moves it and a Memory.Read view stays valid. Where
	// the platform lets the engine reserve a memory's address space (64-bit
	// Linux, macOS, the BSDs and Windows) memories never move anyway and
	// this changes nothing; elsewhere it allocates the maximum up front.
	WithMemoryCapacityFromMax(memoryCapacityFromMax bool) RuntimeConfig

	// WithDebugInfoEnabled says whether RuntimeError stack traces name
	// functions from the modules' name sections. The default is true; off,
	// frames show their locations alone.
	WithDebugInfoEnabled(enabled bool) RuntimeConfig

	// WithCompilationCache shares compiled modules between the runtimes
	// configured with cache: compiling the same bytes under the same
	// memory limit again returns the module compiled before. The cache is
	// in memory.
	WithCompilationCache(cache CompilationCache) RuntimeConfig

	// WithCustomSections keeps modules' custom sections, which
	// CompiledModule.CustomSections then returns. The default is false.
	WithCustomSections(storeCustomSections bool) RuntimeConfig

	// WithCloseOnContextDone ends calls whose context is done, and calls of
	// a module that is closed while they run, returning a *sys.ExitError:
	// sys.ExitCodeContextCanceled or sys.ExitCodeDeadlineExceeded for the
	// context, closing the module, or the code the module was closed with.
	//
	// The engine checks the context at loop back edges and calls whether or
	// not this is set, so a done context ends a call either way; without
	// it, the call returns the context's error and its module stays open,
	// and closing a module does not end the calls running in it.
	WithCloseOnContextDone(closeOnContextDone bool) RuntimeConfig
}

type runtimeConfig struct {
	features           api.CoreFeatures
	memoryLimitPages   uint32 // 0: the engine's limits
	capacityFromMax    bool
	debugInfo          bool
	cache              *cache
	customSections     bool
	closeOnContextDone bool
	compiler           bool
	memoryLimit        int64 // 0: none
	jitMemoryLimit     int64 // 0: DefaultJITMemoryLimit
}

// DefaultJITMemoryLimit is what the compiler holds for a runtime's modules
// at most, unless WithJITMemoryLimit says otherwise.
const DefaultJITMemoryLimit = 64 << 20

var defaultRuntimeConfig = &runtimeConfig{features: api.CoreFeaturesAll, debugInfo: true}

// NewRuntimeConfig returns the default configuration, which runs modules in
// the interpreter. Native code is opt-in: see NewRuntimeConfigCompiler.
func NewRuntimeConfig() RuntimeConfig { return defaultRuntimeConfig.clone() }

// NewRuntimeConfigInterpreter returns a configuration that runs modules in
// the interpreter.
func NewRuntimeConfigInterpreter() RuntimeConfig { return defaultRuntimeConfig.clone() }

// NewRuntimeConfigCompiler returns a configuration that compiles a module's
// functions to native code once each has been called a few times, in a
// build with the quickjs_jit tag on a platform the compiler supports (amd64
// on Linux and Windows, arm64 on Linux and macOS). Elsewhere, and for a function the compiler does not
// handle, the interpreter runs the code, as NewRuntimeConfigInterpreter's
// does.
func NewRuntimeConfigCompiler() RuntimeConfig {
	c := defaultRuntimeConfig.clone()
	c.compiler = true
	return c
}

func (c *runtimeConfig) clone() *runtimeConfig {
	r := *c
	return &r
}

func (c *runtimeConfig) WithCoreFeatures(features api.CoreFeatures) RuntimeConfig {
	r := c.clone()
	r.features = features
	return r
}

func (c *runtimeConfig) CoreFeatures() api.CoreFeatures { return c.features }

func (c *runtimeConfig) WithMemoryLimitPages(memoryLimitPages uint32) RuntimeConfig {
	if most := max(engine.MaxMemory32Pages, engine.MaxMemory64Pages); uint64(memoryLimitPages) > most {
		panic(fmt.Errorf("memoryLimitPages invalid: %d > %d", memoryLimitPages, most))
	}
	r := c.clone()
	r.memoryLimitPages = memoryLimitPages
	return r
}

func (c *runtimeConfig) WithMemoryLimit(bytes int64) RuntimeConfig {
	if bytes < 0 {
		panic(fmt.Errorf("memory limit invalid: %d < 0", bytes))
	}
	r := c.clone()
	r.memoryLimit = bytes
	return r
}

func (c *runtimeConfig) WithJITMemoryLimit(bytes int64) RuntimeConfig {
	if bytes < 0 {
		panic(fmt.Errorf("JIT memory limit invalid: %d < 0", bytes))
	}
	r := c.clone()
	r.jitMemoryLimit = bytes
	return r
}

// jitLimit is the compiler's memory limit in bytes.
func (c *runtimeConfig) jitLimit() int64 {
	if c.jitMemoryLimit > 0 {
		return c.jitMemoryLimit
	}
	return DefaultJITMemoryLimit
}

func (c *runtimeConfig) WithMemoryCapacityFromMax(memoryCapacityFromMax bool) RuntimeConfig {
	r := c.clone()
	r.capacityFromMax = memoryCapacityFromMax
	return r
}

func (c *runtimeConfig) WithDebugInfoEnabled(enabled bool) RuntimeConfig {
	r := c.clone()
	r.debugInfo = enabled
	return r
}

func (c *runtimeConfig) WithCompilationCache(ca CompilationCache) RuntimeConfig {
	r := c.clone()
	r.cache, _ = ca.(*cache)
	return r
}

func (c *runtimeConfig) WithCustomSections(storeCustomSections bool) RuntimeConfig {
	r := c.clone()
	r.customSections = storeCustomSections
	return r
}

func (c *runtimeConfig) WithCloseOnContextDone(closeOnContextDone bool) RuntimeConfig {
	r := c.clone()
	r.closeOnContextDone = closeOnContextDone
	return r
}

// memoryLimits returns the most pages an i32 and an i64 memory may have.
func (c *runtimeConfig) memoryLimits() (mem32, mem64 uint64) {
	mem32, mem64 = engine.MaxMemory32Pages, engine.MaxMemory64Pages
	if c.memoryLimitPages != 0 {
		mem32 = min(mem32, uint64(c.memoryLimitPages))
		mem64 = min(mem64, uint64(c.memoryLimitPages))
	}
	return
}

// CompilationCache holds compiled modules for the runtimes configured with
// it to share. It is safe for concurrent use. Close empties it.
type CompilationCache interface{ api.Closer }

// NewCompilationCache returns an empty in-memory cache.
func NewCompilationCache() CompilationCache { return &cache{entries: map[cacheKey]*compiledCode{}} }

type cacheKey struct {
	sum          [sha256.Size]byte
	mem32, mem64 uint64
	// compiler keeps code a compiler configuration compiles to native code
	// apart from the interpreter's, and jitLimit code charged to a budget of
	// one limit apart from another's.
	compiler bool
	jitLimit int64
}

type cache struct {
	mu      sync.Mutex
	entries map[cacheKey]*compiledCode
	// jit are the budgets of the native code of the runtimes sharing the
	// cache, by their limit.
	jit map[int64]*engine.JITBudget
}

// jitBudget returns the budget of the runtimes sharing the cache whose
// limit is limit.
func (c *cache) jitBudget(limit int64) *engine.JITBudget {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.jit == nil {
		c.jit = map[int64]*engine.JITBudget{}
	}
	b := c.jit[limit]
	if b == nil {
		b = engine.NewJITBudget(limit)
		c.jit[limit] = b
	}
	return b
}

func (c *cache) get(k cacheKey) *compiledCode {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.entries[k]
}

func (c *cache) put(k cacheKey, cc *compiledCode) *compiledCode {
	c.mu.Lock()
	defer c.mu.Unlock()
	if old := c.entries[k]; old != nil {
		return old
	}
	c.entries[k] = cc
	return cc
}

func (c *cache) Close(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	clear(c.entries)
	return nil
}

// ModuleConfig configures an instantiation: the module's name, the functions
// to call once it is instantiated, and what host functions that implement
// system interfaces, such as WASI, give it. It is immutable: each With
// method returns a new ModuleConfig with the change.
//
// The system settings sandbox the module: nothing reaches the process's
// arguments, environment, streams, files, clocks or randomness unless it is
// configured to.
type ModuleConfig interface {
	// WithArgs sets the arguments, argv; the default is none. An argument
	// holding a NUL makes the instantiation fail.
	WithArgs(args ...string) ModuleConfig

	// WithEnv sets an environment variable, replacing an earlier value of
	// key; the default is none. An empty key, or one holding '=' or NUL,
	// makes the instantiation fail.
	WithEnv(key, value string) ModuleConfig

	// WithFS sets the file system to fsys, mounted read-only at "/"; the
	// default is none. It is WithFSConfig(NewFSConfig().WithFSMount(fsys,
	// "/")).
	WithFS(fsys fs.FS) ModuleConfig

	// WithFSConfig sets the file systems, at the guest paths config gives;
	// the default is none. It replaces what WithFS set.
	WithFSConfig(config FSConfig) ModuleConfig

	// WithName sets the name the module is instantiated under, which
	// modules import it by. The default is the name in the module's name
	// section, or the host module's name. The empty name makes the module
	// anonymous, so that the same CompiledModule can be instantiated many
	// times; two modules of one runtime cannot have the same other name.
	WithName(name string) ModuleConfig

	// WithStartFunctions sets the exported functions called, in order,
	// once the module is instantiated and its start function has run. The
	// default is "_start"; a name the module does not export is skipped.
	WithStartFunctions(names ...string) ModuleConfig

	// WithStderr sets standard error; the default discards. It is not
	// closed with the module.
	WithStderr(w io.Writer) ModuleConfig

	// WithStdin sets standard input; the default reads io.EOF. It is not
	// closed with the module.
	WithStdin(r io.Reader) ModuleConfig

	// WithStdout sets standard output; the default discards. It is not
	// closed with the module.
	WithStdout(w io.Writer) ModuleConfig

	// WithWalltime sets the wall clock. The default is a fake that starts
	// at 2022-01-01T00:00:00Z and advances a millisecond each reading.
	WithWalltime(walltime sys.Walltime, resolution sys.ClockResolution) ModuleConfig

	// WithSysWalltime sets the wall clock to the system's, time.Now, at a
	// resolution of a microsecond.
	WithSysWalltime() ModuleConfig

	// WithNanotime sets the monotonic clock. The default is a fake that
	// starts at zero and advances a millisecond each reading.
	WithNanotime(nanotime sys.Nanotime, resolution sys.ClockResolution) ModuleConfig

	// WithSysNanotime sets the monotonic clock to the system's, at a
	// resolution of a nanosecond.
	WithSysNanotime() ModuleConfig

	// WithNanosleep sets how to sleep. The default returns at once.
	WithNanosleep(nanosleep sys.Nanosleep) ModuleConfig

	// WithSysNanosleep sleeps with time.Sleep.
	WithSysNanosleep() ModuleConfig

	// WithOsyield sets how to yield the processor. The default returns at
	// once.
	WithOsyield(osyield sys.Osyield) ModuleConfig

	// WithRandSource sets the source of random bytes. The default is a
	// fixed, deterministic sequence; crypto/rand.Reader is the system's. It
	// is not closed with the module.
	WithRandSource(source io.Reader) ModuleConfig
}

type moduleConfig struct {
	name           string
	nameSet        bool
	startFunctions []string
	sys            sysctx.Config
}

// NewModuleConfig returns the default configuration: the module's own
// name, "_start" called after instantiation, and no access to the system.
func NewModuleConfig() ModuleConfig {
	return &moduleConfig{startFunctions: []string{"_start"}}
}

func (c *moduleConfig) clone() *moduleConfig {
	r := *c
	r.startFunctions = append([]string(nil), c.startFunctions...)
	r.sys.Args = append([]string(nil), c.sys.Args...)
	r.sys.EnvKeys = append([]string(nil), c.sys.EnvKeys...)
	r.sys.EnvValues = append([]string(nil), c.sys.EnvValues...)
	return &r
}

func (c *moduleConfig) WithArgs(args ...string) ModuleConfig {
	r := c.clone()
	r.sys.Args = append([]string(nil), args...)
	return r
}

func (c *moduleConfig) WithEnv(key, value string) ModuleConfig {
	r := c.clone()
	for i, k := range r.sys.EnvKeys {
		if k == key {
			r.sys.EnvValues[i] = value
			return r
		}
	}
	r.sys.EnvKeys = append(r.sys.EnvKeys, key)
	r.sys.EnvValues = append(r.sys.EnvValues, value)
	return r
}

func (c *moduleConfig) WithFS(fsys fs.FS) ModuleConfig {
	r := c.WithFSConfig(NewFSConfig().WithFSMount(fsys, "/")).(*moduleConfig)
	r.sys.FS = fsys
	return r
}

func (c *moduleConfig) WithName(name string) ModuleConfig {
	r := c.clone()
	r.name, r.nameSet = name, true
	return r
}

func (c *moduleConfig) WithStartFunctions(names ...string) ModuleConfig {
	r := c.clone()
	r.startFunctions = append([]string(nil), names...)
	return r
}

func (c *moduleConfig) WithStderr(w io.Writer) ModuleConfig {
	r := c.clone()
	r.sys.Stderr = w
	return r
}

func (c *moduleConfig) WithStdin(rd io.Reader) ModuleConfig {
	r := c.clone()
	r.sys.Stdin = rd
	return r
}

func (c *moduleConfig) WithStdout(w io.Writer) ModuleConfig {
	r := c.clone()
	r.sys.Stdout = w
	return r
}

func (c *moduleConfig) WithWalltime(walltime sys.Walltime, resolution sys.ClockResolution) ModuleConfig {
	r := c.clone()
	r.sys.Walltime, r.sys.WalltimeRe = walltime, resolution
	return r
}

func (c *moduleConfig) WithSysWalltime() ModuleConfig {
	return c.WithWalltime(func() (int64, int32) {
		t := time.Now()
		return t.Unix(), int32(t.Nanosecond())
	}, sys.ClockResolution(time.Microsecond))
}

// processStart is what the system monotonic clock counts from.
var processStart = time.Now()

func (c *moduleConfig) WithNanotime(nanotime sys.Nanotime, resolution sys.ClockResolution) ModuleConfig {
	r := c.clone()
	r.sys.Nanotime, r.sys.NanotimeRe = nanotime, resolution
	return r
}

func (c *moduleConfig) WithSysNanotime() ModuleConfig {
	return c.WithNanotime(func() int64 { return int64(time.Since(processStart)) }, 1)
}

func (c *moduleConfig) WithNanosleep(nanosleep sys.Nanosleep) ModuleConfig {
	r := c.clone()
	r.sys.Nanosleep = nanosleep
	return r
}

func (c *moduleConfig) WithSysNanosleep() ModuleConfig {
	return c.WithNanosleep(func(ns int64) { time.Sleep(time.Duration(ns)) })
}

func (c *moduleConfig) WithOsyield(osyield sys.Osyield) ModuleConfig {
	r := c.clone()
	r.sys.Osyield = osyield
	return r
}

func (c *moduleConfig) WithRandSource(source io.Reader) ModuleConfig {
	r := c.clone()
	r.sys.RandSource = source
	return r
}
