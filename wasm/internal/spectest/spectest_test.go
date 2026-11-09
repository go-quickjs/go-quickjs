// Package spectest runs the WebAssembly specification's test scripts against
// the engine. The scripts are not part of the repository: WASM_SPEC_DIR
// names a checkout of github.com/WebAssembly/spec, WASM_THREADS_DIR one of
// github.com/WebAssembly/threads, and without them the tests skip.
package spectest

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/wasm/internal/decoder"
	"github.com/go-quickjs/go-quickjs/wasm/internal/engine"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/wast"
)

// hostValue is the host value (ref.extern n) or (ref.host n) passes.
type hostValue struct {
	n    uint32
	host bool
}

type runner struct {
	t        *testing.T
	file     string
	lim      *decoder.Limits
	registry map[string]map[string]engine.Extern
	named    map[string]*engine.Instance
	defs     map[string]*engine.Code
	current  *engine.Instance

	passed, failed int
	failures       []string
	// threads are the script's running threads.
	threads map[string]*thread
}

// thread is a thread a script started: a runner of its own on a goroutine.
type thread struct {
	r    *runner
	done chan struct{}
}

func newRunner(t *testing.T, file string) *runner {
	r := &runner{
		t:        t,
		file:     file,
		lim:      decoder.CoreLimits(),
		registry: map[string]map[string]engine.Extern{"spectest": spectestModule()},
		named:    map[string]*engine.Instance{},
		defs:     map[string]*engine.Code{},
	}
	return r
}

func (r *runner) fail(line int, format string, args ...any) {
	r.failed++
	r.failures = append(r.failures, fmt.Sprintf("%s:%d: %s", filepath.Base(r.file), line, fmt.Sprintf(format, args...)))
}

func (r *runner) pass() { r.passed++ }

// spectestModule returns the exports of the scripts' host module.
func spectestModule() map[string]engine.Extern {
	fn := func(name string, params ...module.ValueType) engine.Extern {
		ft := &module.FuncType{Params: params}
		return engine.Extern{Kind: module.ExternFunc, Func: engine.NewHostFunction(module.CanonicalFunc(ft), name,
			func(ctx context.Context, caller *engine.Instance, args, results []engine.Val) error { return nil })}
	}
	global := func(t module.ValueType, n uint64) engine.Extern {
		return engine.Extern{Kind: module.ExternGlobal, Global: &engine.Global{Num: n, Type: t}}
	}
	return map[string]engine.Extern{
		"print":         fn("print"),
		"print_i32":     fn("print_i32", module.I32),
		"print_i64":     fn("print_i64", module.I64),
		"print_f32":     fn("print_f32", module.F32),
		"print_f64":     fn("print_f64", module.F64),
		"print_i32_f32": fn("print_i32_f32", module.I32, module.F32),
		"print_f64_f64": fn("print_f64_f64", module.F64, module.F64),
		"global_i32":    global(module.I32, 666),
		"global_i64":    global(module.I64, 666),
		"global_f32":    global(module.F32, uint64(math.Float32bits(666.6))),
		"global_f64":    global(module.F64, math.Float64bits(666.6)),
		"table":         {Kind: module.ExternTable, Table: engine.NewTable(module.FuncRef, nil, 10, 20, true, false, engine.Null)},
		"table64":       {Kind: module.ExternTable, Table: engine.NewTable(module.FuncRef, nil, 10, 20, true, true, engine.Null)},
		"memory":        {Kind: module.ExternMemory, Memory: engine.NewMemory(1, 2, true, false, false)},
	}
}

// compile decodes and validates a module's binary.
func (r *runner) compile(m *wast.Module) (*engine.Code, error) {
	b, err := m.Binary()
	if err != nil {
		return nil, err
	}
	mod, derr := decoder.Decode(b, r.lim)
	if derr != nil {
		return nil, derr
	}
	if derr := decoder.ValidateFunctions(mod, r.lim); derr != nil {
		return nil, derr
	}
	return engine.NewCode(mod), nil
}

// instantiate instantiates code, resolving its imports in the registry.
func (r *runner) instantiate(code *engine.Code) (*engine.Instance, error) {
	m := code.Module
	imports := make([]engine.Extern, len(m.Imports))
	for i, imp := range m.Imports {
		mod, ok := r.registry[imp.Module]
		if !ok {
			return nil, &engine.LinkError{Msg: fmt.Sprintf("unknown module %q", imp.Module)}
		}
		ext, ok := mod[imp.Name]
		if !ok {
			return nil, &engine.LinkError{Msg: fmt.Sprintf("unknown import %q %q", imp.Module, imp.Name)}
		}
		imports[i] = ext
	}
	return engine.Instantiate(context.Background(), code, imports)
}

func exportsOf(inst *engine.Instance) map[string]engine.Extern {
	out := map[string]engine.Extern{}
	for _, e := range inst.Module.Exports {
		ext, _ := inst.Export(e.Name)
		out[e.Name] = ext
	}
	return out
}

func (r *runner) instance(name string) *engine.Instance {
	if name == "" {
		return r.current
	}
	return r.named[name]
}

// toVal converts a script constant into a value.
func toVal(v wast.Value) engine.Val {
	switch v.Kind {
	case wast.V128:
		var lo, hi uint64
		for i := 7; i >= 0; i-- {
			lo = lo<<8 | uint64(v.V128[i])
			hi = hi<<8 | uint64(v.V128[8+i])
		}
		return engine.Val{N: lo, Hi: hi}
	case wast.RefNull:
		return engine.Val{}
	case wast.RefExtern:
		return engine.Val{R: engine.HostRef(hostValue{n: v.Ref})}
	case wast.RefHost:
		return engine.Val{R: engine.HostRef(hostValue{n: v.Ref, host: true})}
	}
	return engine.Val{N: v.Bits}
}

// do runs an action, returning its results.
func (r *runner) do(a wast.Action) ([]engine.Val, []module.ValueType, error) {
	inst := r.instance(a.Instance)
	if inst == nil {
		return nil, nil, fmt.Errorf("no instance %q", a.Instance)
	}
	ext, ok := inst.Export(a.Name)
	if !ok {
		return nil, nil, fmt.Errorf("no export %q", a.Name)
	}
	if a.Kind == wast.Get {
		g := ext.Global
		return []engine.Val{{N: g.Num, Hi: g.Hi, R: g.Ref}}, []module.ValueType{g.Type}, nil
	}
	args := make([]engine.Val, len(a.Args))
	for i, v := range a.Args {
		args[i] = toVal(v)
	}
	res, err := engine.Invoke(context.Background(), ext.Func, args)
	return res, ext.Func.Sig.Results, err
}

func (r *runner) run(cmds []wast.Command) {
	for _, cmd := range cmds {
		r.command(cmd)
	}
}

func (r *runner) command(cmd wast.Command) {
	defer func() {
		if p := recover(); p != nil {
			r.fail(cmd.Line(), "panic: %v", p)
		}
	}()
	switch c := cmd.(type) {
	case *wast.ModuleCommand:
		code, err := r.compile(c.Module)
		if err != nil {
			r.fail(c.Line(), "module: %v", err)
			r.current = nil
			return
		}
		if c.Definition {
			r.defs[c.Module.Name] = code
			r.pass()
			return
		}
		inst, err := r.instantiate(code)
		if err != nil {
			r.fail(c.Line(), "instantiate: %v", err)
			r.current = nil
			return
		}
		r.current = inst
		if c.Module.Name != "" {
			r.named[c.Module.Name] = inst
		}
		r.pass()
	case *wast.InstanceCommand:
		code := r.defs[c.Module]
		if code == nil {
			r.fail(c.Line(), "no module definition %q", c.Module)
			return
		}
		inst, err := r.instantiate(code)
		if err != nil {
			r.fail(c.Line(), "instantiate: %v", err)
			return
		}
		r.current = inst
		if c.Instance != "" {
			r.named[c.Instance] = inst
		}
		r.pass()
	case *wast.RegisterCommand:
		inst := r.instance(c.Instance)
		if inst == nil {
			r.fail(c.Line(), "register: no instance %q", c.Instance)
			return
		}
		r.registry[c.As] = exportsOf(inst)
	case *wast.ActionCommand:
		if _, _, err := r.do(c.Action); err != nil {
			r.fail(c.Line(), "action: %v", err)
			return
		}
		r.pass()
	case *wast.AssertReturn:
		res, types, err := r.do(c.Action)
		if err != nil {
			r.fail(c.Line(), "assert_return %s: %v", c.Action.Name, err)
			return
		}
		if len(res) != len(c.Results) {
			r.fail(c.Line(), "assert_return %s: %d results, want %d", c.Action.Name, len(res), len(c.Results))
			return
		}
		for i, want := range c.Results {
			if !match(res[i], types[i], want) {
				r.fail(c.Line(), "assert_return %s: result %d is %s, want %s", c.Action.Name, i, show(res[i], types[i]), showResult(want))
				return
			}
		}
		r.pass()
	case *wast.AssertTrap:
		_, _, err := r.do(c.Action)
		var tr *engine.Trap
		if !errors.As(err, &tr) {
			r.fail(c.Line(), "assert_trap %s: got %v, want trap %q", c.Action.Name, err, c.Message)
			return
		}
		if !trapMatches(tr, c.Message) {
			r.fail(c.Line(), "assert_trap %s: trapped with %q, want %q", c.Action.Name, tr.Error(), c.Message)
			return
		}
		r.pass()
	case *wast.AssertExhaustion:
		_, _, err := r.do(c.Action)
		var tr *engine.Trap
		if !errors.As(err, &tr) || tr.Code != engine.TrapStackOverflow {
			r.fail(c.Line(), "assert_exhaustion %s: got %v", c.Action.Name, err)
			return
		}
		r.pass()
	case *wast.AssertException:
		_, _, err := r.do(c.Action)
		var ex *engine.Exception
		if !errors.As(err, &ex) {
			r.fail(c.Line(), "assert_exception %s: got %v", c.Action.Name, err)
			return
		}
		r.pass()
	case *wast.AssertInvalid:
		if _, err := r.compile(c.Module); err == nil {
			r.fail(c.Line(), "assert_invalid: module is valid, want %q", c.Message)
			return
		}
		r.pass()
	case *wast.AssertMalformed:
		if _, err := r.compile(c.Module); err == nil {
			r.fail(c.Line(), "assert_malformed: module is well formed, want %q", c.Message)
			return
		}
		r.pass()
	case *wast.AssertUnlinkable:
		code, err := r.moduleOf(c.Module, c.Definition)
		if err != nil {
			r.fail(c.Line(), "assert_unlinkable: %v", err)
			return
		}
		_, err = r.instantiate(code)
		var le *engine.LinkError
		if !errors.As(err, &le) {
			r.fail(c.Line(), "assert_unlinkable: got %v, want link error %q", err, c.Message)
			return
		}
		r.pass()
	case *wast.AssertUninstantiable:
		code, err := r.moduleOf(c.Module, c.Definition)
		if err != nil {
			r.fail(c.Line(), "assert_uninstantiable: %v", err)
			return
		}
		_, err = r.instantiate(code)
		var tr *engine.Trap
		if !errors.As(err, &tr) {
			r.fail(c.Line(), "assert_uninstantiable: got %v, want trap %q", err, c.Message)
			return
		}
		r.pass()
	case *wast.ThreadCommand:
		tr := newRunner(r.t, r.file)
		for _, name := range c.Shared {
			tr.named[name] = r.named[name]
		}
		th := &thread{r: tr, done: make(chan struct{})}
		if r.threads == nil {
			r.threads = map[string]*thread{}
		}
		r.threads[c.Name] = th
		go func() {
			defer close(th.done)
			tr.run(c.Commands)
		}()
	case *wast.WaitCommand:
		th := r.threads[c.Thread]
		if th == nil {
			r.fail(c.Line(), "wait: no thread %q", c.Thread)
			return
		}
		<-th.done
		r.passed += th.r.passed
		r.failed += th.r.failed
		r.failures = append(r.failures, th.r.failures...)
	default:
		r.fail(cmd.Line(), "unknown command %T", cmd)
	}
}

func (r *runner) moduleOf(m *wast.Module, def string) (*engine.Code, error) {
	if m == nil {
		if code := r.defs[def]; code != nil {
			return code, nil
		}
		return nil, fmt.Errorf("no module definition %q", def)
	}
	return r.compile(m)
}

// trapMatches reports whether a trap is the one the script names.
func trapMatches(tr *engine.Trap, msg string) bool {
	want := map[string][]engine.TrapCode{
		"unreachable":                   {engine.TrapUnreachable},
		"integer divide by zero":        {engine.TrapDivByZero, engine.TrapRemByZero},
		"integer overflow":              {engine.TrapDivUnrepresentable, engine.TrapFloatUnrepresentable},
		"invalid conversion to integer": {engine.TrapFloatUnrepresentable},
		"out of bounds memory access":   {engine.TrapMemOutOfBounds, engine.TrapDataSegmentOutOfBounds},
		"out of bounds table access":    {engine.TrapTableOutOfBounds, engine.TrapElementSegmentOutOfBounds},
		"undefined element":             {engine.TrapTableOutOfBounds},
		"uninitialized element":         {engine.TrapNullFunc, engine.TrapFuncSigMismatch},
		"uninitialized element 2":       {engine.TrapNullFunc, engine.TrapFuncSigMismatch},
		"indirect call type mismatch":   {engine.TrapFuncSigMismatch},
		"null function reference":       {engine.TrapNullDereference, engine.TrapNullFunc},
		"null reference":                {engine.TrapNullDereference},
		"null structure reference":      {engine.TrapNullDereference},
		"null array reference":          {engine.TrapNullDereference},
		"null exception reference":      {engine.TrapRethrowNull, engine.TrapNullDereference},
		"null i31 reference":            {engine.TrapNullDereference},
		"cast failure":                  {engine.TrapIllegalCast},
		"out of bounds array access":    {engine.TrapArrayOutOfBounds},
		"array size too large":          {engine.TrapArrayTooLarge},
		"unaligned atomic":              {engine.TrapUnalignedAccess},
		"expected shared memory":        {engine.TrapAtomicsWaitNotAllowed},
		"out of bounds data segment":    {engine.TrapDataSegmentOutOfBounds},
		"out of bounds elem segment":    {engine.TrapElementSegmentOutOfBounds},
		"call stack exhausted":          {engine.TrapStackOverflow},
	}
	codes, ok := want[msg]
	if !ok {
		for k, v := range want {
			if strings.HasPrefix(msg, k) {
				codes, ok = v, true
				break
			}
		}
	}
	if !ok {
		return true
	}
	for _, c := range codes {
		if tr.Code == c {
			return true
		}
	}
	return false
}

func isNaN32(b uint32, k wast.NaNKind) bool {
	if b&0x7f800000 != 0x7f800000 || b&0x7fffff == 0 {
		return false
	}
	if k == wast.CanonicalNaN {
		return b&0x7fffff == 0x400000
	}
	return b&0x400000 != 0
}

func isNaN64(b uint64, k wast.NaNKind) bool {
	if b&0x7ff0000000000000 != 0x7ff0000000000000 || b&0xfffffffffffff == 0 {
		return false
	}
	if k == wast.CanonicalNaN {
		return b&0xfffffffffffff == 0x8000000000000
	}
	return b&0x8000000000000 != 0
}

// match reports whether v, of type t, matches pattern want.
func match(v engine.Val, t module.ValueType, want wast.Result) bool {
	switch want.Kind {
	case wast.ResultEither:
		for _, w := range want.Either {
			if match(v, t, w) {
				return true
			}
		}
		return false
	case wast.ResultNaN:
		if want.Value.Kind == wast.F32 {
			return isNaN32(uint32(v.N), want.NaN)
		}
		return isNaN64(v.N, want.NaN)
	case wast.ResultV128:
		bits := want.Value.Shape.LaneBits()
		for i, lane := range want.Lanes {
			var x uint64
			pos := i * bits
			if pos < 64 {
				x = v.N >> pos
			} else {
				x = v.Hi >> (pos - 64)
			}
			if bits < 64 {
				x &= 1<<bits - 1
			}
			switch {
			case lane.NaN != wast.NotNaN && bits == 32:
				if !isNaN32(uint32(x), lane.NaN) {
					return false
				}
			case lane.NaN != wast.NotNaN:
				if !isNaN64(x, lane.NaN) {
					return false
				}
			case x != lane.Bits:
				return false
			}
		}
		return true
	case wast.ResultRefNull:
		return v.R.IsNull()
	case wast.ResultRef:
		if v.R.IsNull() {
			return false
		}
		switch want.Heap {
		case "func":
			return v.R.Func() != nil
		case "extern":
			return true
		case "i31":
			return v.R.IsI31()
		case "exn":
			return v.R.Exn() != nil
		}
		return engine.RefMatchesAbstract(v.R, want.Heap)
	}
	switch want.Value.Kind {
	case wast.I32, wast.F32:
		return uint32(v.N) == uint32(want.Value.Bits)
	case wast.I64, wast.F64:
		return v.N == want.Value.Bits
	case wast.V128:
		w := toVal(want.Value)
		return v.N == w.N && v.Hi == w.Hi
	case wast.RefNull:
		return v.R.IsNull()
	case wast.RefExtern, wast.RefHost:
		h, ok := v.R.Host()
		hv, ok2 := h.(hostValue)
		return ok && ok2 && hv.n == want.Value.Ref
	}
	return false
}

func show(v engine.Val, t module.ValueType) string {
	switch t.Kind {
	case module.KindI32, module.KindF32:
		return fmt.Sprintf("%s:%#x", t.Name(), uint32(v.N))
	case module.KindV128:
		return fmt.Sprintf("v128:%#x:%#x", v.Hi, v.N)
	case module.KindRef:
		if v.R.IsNull() {
			return "null"
		}
		if h, ok := v.R.Host(); ok {
			return fmt.Sprintf("host %v", h)
		}
		return "ref"
	}
	return fmt.Sprintf("%s:%#x", t.Name(), v.N)
}

func showResult(r wast.Result) string {
	switch r.Kind {
	case wast.ResultNaN:
		return "nan"
	case wast.ResultRefNull:
		return "ref.null"
	case wast.ResultRef:
		return "ref." + r.Heap
	case wast.ResultEither:
		return "either"
	case wast.ResultV128:
		return fmt.Sprintf("v128 lanes %v", r.Lanes)
	}
	return fmt.Sprintf("%#x", r.Value.Bits)
}

// scripts returns the .wast files under dir, recursively.
func scripts(t *testing.T, dir string) []string {
	var files []string
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".wast") {
			files = append(files, p)
		}
		return nil
	})
	sort.Strings(files)
	return files
}

// logJIT logs what the JIT compiled and refused, when WASM_JIT turned it on.
func logJIT(t *testing.T) {
	st := engine.ReadJITStats()
	if st.Compiled == 0 && len(st.Refused) == 0 {
		return
	}
	refused := 0
	reasons := make([]string, 0, len(st.Refused))
	for r, n := range st.Refused {
		refused += n
		reasons = append(reasons, r)
	}
	sort.Slice(reasons, func(i, j int) bool { return st.Refused[reasons[i]] > st.Refused[reasons[j]] })
	t.Logf("jit: %d functions compiled, %d refused; %d calls went on natively from a loop (OSR)", st.Compiled, refused, st.OSR)
	for _, r := range reasons {
		t.Logf("jit: %6d refused: %s", st.Refused[r], r)
	}
}

func TestSpec(t *testing.T) {
	dir := os.Getenv("WASM_SPEC_DIR")
	if dir == "" {
		t.Skip("WASM_SPEC_DIR is not set")
	}
	only := os.Getenv("WASM_SPEC_ONLY")
	var files []string
	files = append(files, scripts(t, filepath.Join(dir, "test", "core"))...)
	files = append(files, scripts(t, filepath.Join(dir, "test", "legacy"))...)
	if td := os.Getenv("WASM_THREADS_DIR"); td != "" {
		files = append(files, scripts(t, filepath.Join(td, "test", "core", "threads"))...)
	}
	total, failed := 0, 0
	byFile := map[string]int{}
	var all []string
	for _, f := range files {
		rel, _ := filepath.Rel(dir, f)
		if only != "" && !strings.Contains(filepath.ToSlash(f), only) {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		cmds, err := wast.Parse(src)
		if err != nil {
			t.Errorf("%s: %v", rel, err)
			continue
		}
		r := newRunner(t, f)
		r.run(cmds)
		total += r.passed + r.failed
		failed += r.failed
		if r.failed > 0 {
			byFile[filepath.ToSlash(rel)] = r.failed
			all = append(all, r.failures...)
		}
	}
	keys := make([]string, 0, len(byFile))
	for k := range byFile {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return byFile[keys[i]] > byFile[keys[j]] })
	for _, k := range keys {
		t.Logf("%5d %s", byFile[k], k)
	}
	max := 60
	for i, f := range all {
		if i == max {
			t.Logf("... %d more", len(all)-max)
			break
		}
		t.Log(f)
	}
	t.Logf("spec: %d of %d assertions passed, %d failed", total-failed, total, failed)
	logJIT(t)
	if failed > 0 {
		t.Fail()
	}
}
