package wasm_test

import (
	"bytes"
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-quickjs/go-quickjs/wasm"
	"github.com/go-quickjs/go-quickjs/wasm/api"
	"github.com/go-quickjs/go-quickjs/wasm/internal/sysctx"
	"github.com/go-quickjs/go-quickjs/wasm/sys"
	"github.com/go-quickjs/go-quickjs/wasm/wat"
)

// compile returns the binary of text module src.
func compile(t testing.TB, src string) []byte {
	t.Helper()
	b, err := wat.Compile([]byte(src))
	if err != nil {
		t.Fatalf("wat: %v", err)
	}
	return b
}

// newRuntime returns a runtime of config, or the default one, closed when
// the test ends.
func newRuntime(t testing.TB, configs ...wasm.RuntimeConfig) (context.Context, wasm.Runtime) {
	t.Helper()
	ctx := context.Background()
	cfg := wasm.NewRuntimeConfig()
	if len(configs) > 0 {
		cfg = configs[0]
	}
	r := wasm.NewRuntimeWithConfig(ctx, cfg)
	t.Cleanup(func() { r.Close(ctx) })
	return ctx, r
}

func instantiate(t testing.TB, ctx context.Context, r wasm.Runtime, src string) api.Module {
	t.Helper()
	mod, err := r.Instantiate(ctx, compile(t, src))
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}
	return mod
}

func TestInstantiateAndCall(t *testing.T) {
	ctx, r := newRuntime(t)
	mod := instantiate(t, ctx, r, `(module
		(func (export "add") (param i32 i32) (result i32)
			local.get 0 local.get 1 i32.add)
		(func (export "swap") (param i64 f64) (result f64 i64)
			local.get 1 local.get 0))`)
	add := mod.ExportedFunction("add")
	res, err := add.Call(ctx, api.EncodeI32(-5), api.EncodeI32(3))
	if err != nil {
		t.Fatal(err)
	}
	if got := api.DecodeI32(res[0]); got != -2 || res[0] != 0xffff_fffe {
		t.Errorf("add = %d (%#x), want -2 with the upper bits clear", got, res[0])
	}
	res, err = mod.ExportedFunction("swap").Call(ctx, 7, api.EncodeF64(1.5))
	if err != nil {
		t.Fatal(err)
	}
	if api.DecodeF64(res[0]) != 1.5 || res[1] != 7 {
		t.Errorf("swap = %v", res)
	}
	if _, err := add.Call(ctx, 1); err == nil || err.Error() != "expected 2 params, but passed 1" {
		t.Errorf("short call: %v", err)
	}
	def := add.Definition()
	if got := def.ParamTypes(); len(got) != 2 || got[0] != api.ValueTypeI32 || got[1] != api.ValueTypeI32 {
		t.Errorf("params %v", got)
	}
	if got := def.ExportNames(); len(got) != 1 || got[0] != "add" {
		t.Errorf("export names %v", got)
	}
	if got := def.DebugName(); got != ".$0" {
		t.Errorf("debug name %q", got)
	}
	if mod.ExportedFunction("missing") != nil {
		t.Error("missing export found")
	}
	stack := []uint64{2, 40}
	if err := add.CallWithStack(ctx, stack); err != nil || stack[0] != 42 {
		t.Errorf("CallWithStack: %v %v", stack, err)
	}
	if got := mod.String(); got != "Module[]" {
		t.Errorf("String() = %q", got)
	}
}

func TestHostFunctions(t *testing.T) {
	ctx, r := newRuntime(t)
	var logged []string
	var total uint64
	_, err := r.NewHostModuleBuilder("env").
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, m api.Module, offset, length uint32) {
			b, ok := m.Memory().Read(offset, length)
			if !ok {
				panic("out of range")
			}
			logged = append(logged, string(b))
		}).
		WithParameterNames("offset", "length").
		Export("log").
		NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, m api.Module, stack []uint64) {
			// Write a greeting where the caller asks and return its length.
			msg := "written by the host"
			if !m.Memory().WriteString(api.DecodeU32(stack[0]), msg) {
				panic(errors.New("cannot write"))
			}
			stack[0] = api.EncodeU32(uint32(len(msg)))
		}), []api.ValueType{api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).
		WithName("greet").
		Export("greet").
		NewFunctionBuilder().
		WithGoFunction(api.GoFunc(func(ctx context.Context, stack []uint64) {
			total += stack[0]
			stack[0] = total
		}), []api.ValueType{api.ValueTypeI64}, []api.ValueType{api.ValueTypeI64}).
		Export("sum").
		NewFunctionBuilder().
		WithFunc(func(a int32, b float32, c float64) (float64, int64) {
			return float64(a) + float64(b) + c, int64(a) * 2
		}).
		Export("mix").
		Instantiate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mod := instantiate(t, ctx, r, `(module
		(import "env" "log" (func $log (param i32 i32)))
		(import "env" "greet" (func $greet (param i32) (result i32)))
		(import "env" "sum" (func $sum (param i64) (result i64)))
		(import "env" "mix" (func $mix (param i32 f32 f64) (result f64 i64)))
		(memory (export "memory") 1)
		(data (i32.const 16) "hello")
		(func (export "run") (result i64)
			(call $log (i32.const 16) (i32.const 5))
			(call $log (i32.const 100) (call $greet (i32.const 100)))
			(drop (call $sum (i64.const 40)))
			(call $sum (i64.const 2)))
		(func (export "mix") (result f64 i64)
			(call $mix (i32.const -3) (f32.const 0.5) (f64.const 10))))`)
	res, err := mod.ExportedFunction("run").Call(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res[0] != 42 {
		t.Errorf("sum = %d, want 42", res[0])
	}
	if want := []string{"hello", "written by the host"}; strings.Join(logged, "|") != strings.Join(want, "|") {
		t.Errorf("logged %q, want %q", logged, want)
	}
	res, err = mod.ExportedFunction("mix").Call(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if api.DecodeF64(res[0]) != 7.5 || int64(res[1]) != -6 {
		t.Errorf("mix = %v, %d", api.DecodeF64(res[0]), int64(res[1]))
	}

	env := r.Module("env")
	defs := env.ExportedFunctionDefinitions()
	log := defs["log"]
	if log == nil || log.DebugName() != "env.log" || strings.Join(log.ParamNames(), ",") != "offset,length" {
		t.Errorf("log definition: %v", log)
	}
	if greet := defs["greet"]; greet.Name() != "greet" || greet.GoFunction() == nil {
		t.Errorf("greet definition: %v", greet)
	}

	// A host function's panic is the call's error.
	_, err = r.NewHostModuleBuilder("bad").
		NewFunctionBuilder().WithFunc(func() { panic(errors.New("host failure")) }).Export("f").
		Instantiate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	bad := instantiate(t, ctx, r, `(module (import "bad" "f" (func $f)) (func (export "g") call $f))`)
	if _, err := bad.ExportedFunction("g").Call(ctx); err == nil || err.Error() != "host failure" {
		t.Errorf("host panic: %v", err)
	}
}

func TestHostFunctionErrors(t *testing.T) {
	ctx, r := newRuntime(t)
	for _, tc := range []struct {
		fn   any
		want string
	}{
		{func(string) {}, "func[env.f] param[0] is unsupported: string"},
		{func() string { return "" }, "func[env.f] result[0] is unsupported: string"},
		{func(uint32, context.Context) {}, "func[env.f] param[1] is a context.Context, which must be the first parameter"},
		{42, "func[env.f] kind != func: int"},
	} {
		_, err := r.NewHostModuleBuilder("env").NewFunctionBuilder().WithFunc(tc.fn).Export("f").Compile(ctx)
		if err == nil || err.Error() != tc.want {
			t.Errorf("%T: %v, want %q", tc.fn, err, tc.want)
		}
	}
	_, err := r.NewHostModuleBuilder("").Compile(ctx)
	if err == nil {
		t.Error("empty host module name compiled")
	}
}

func TestMemoryFromHost(t *testing.T) {
	ctx, r := newRuntime(t)
	mod := instantiate(t, ctx, r, `(module
		(memory (export "mem") 1 3)
		(func (export "load") (param i32) (result i64) local.get 0 i64.load)
		(func (export "size") (result i32) memory.size))`)
	mem := mod.ExportedMemory("mem")
	if mem == nil || mod.Memory() == nil {
		t.Fatal("no memory")
	}
	if mem.Size() != 65536 || mem.Size64() != 65536 {
		t.Errorf("size %d", mem.Size())
	}
	if !mem.WriteUint64Le(8, 0x0102030405060708) || !mem.WriteFloat32Le(100, 1.25) || !mem.WriteUint8(200, 7) {
		t.Fatal("write failed")
	}
	res, err := mod.ExportedFunction("load").Call(ctx, 8)
	if err != nil || res[0] != 0x0102030405060708 {
		t.Errorf("load = %#x, %v", res, err)
	}
	if v, ok := mem.ReadFloat32Le(100); !ok || v != 1.25 {
		t.Errorf("ReadFloat32Le = %v %v", v, ok)
	}
	if v, ok := mem.ReadUint16Le(8); !ok || v != 0x0708 {
		t.Errorf("ReadUint16Le = %#x %v", v, ok)
	}
	if _, ok := mem.ReadUint32Le(65533); ok {
		t.Error("read past the end")
	}
	if mem.Write(65535, []byte{1, 2}) {
		t.Error("write past the end")
	}
	view, _ := mem.Read(8, 8)
	if prev, ok := mem.Grow(2); !ok || prev != 1 {
		t.Errorf("Grow = %d %v", prev, ok)
	}
	if _, ok := mem.Grow(1); ok {
		t.Error("grew past the maximum")
	}
	res, _ = mod.ExportedFunction("size").Call(ctx)
	if res[0] != 3 {
		t.Errorf("memory.size = %d", res[0])
	}
	// The view taken before the grow still shows the memory where the
	// platform reserves it; it always shows what it showed.
	if view[0] != 0x08 {
		t.Errorf("view %x", view)
	}
	def := mem.Definition()
	if max, ok := def.Max(); def.Min() != 1 || !ok || max != 3 || def.Is64() {
		t.Errorf("definition min %d max %d %v", def.Min(), max, ok)
	}
	defs := mod.ExportedMemoryDefinitions()
	if len(defs) != 1 || defs["mem"].ExportNames()[0] != "mem" {
		t.Errorf("memory definitions %v", defs)
	}
}

func TestMemory64(t *testing.T) {
	ctx, r := newRuntime(t)
	mod := instantiate(t, ctx, r, `(module
		(memory (export "mem") i64 1)
		(func (export "store") (param i64 i64) local.get 0 local.get 1 i64.store)
		(func (export "load") (param i64) (result i64) local.get 0 i64.load)
		(func (export "grow") (param i64) (result i64) local.get 0 memory.grow)
		(func (export "size") (result i64) memory.size))`)
	mem := mod.ExportedMemory("mem")
	if !mem.Definition().Is64() {
		t.Error("memory not 64-bit")
	}
	if _, err := mod.ExportedFunction("store").Call(ctx, 65528, 0xdeadbeef); err != nil {
		t.Fatal(err)
	}
	if v, ok := mem.ReadUint64Le(65528); !ok || v != 0xdeadbeef {
		t.Errorf("read %#x %v", v, ok)
	}
	if prev, ok := mem.Grow64(1); !ok || prev != 1 {
		t.Errorf("Grow64 = %d %v", prev, ok)
	}
	if mem.Size64() != 2*65536 {
		t.Errorf("Size64 = %d", mem.Size64())
	}
	if !mem.Write64(131072-3, []byte{1, 2, 3}) || mem.Write64(131072-2, []byte{1, 2, 3}) {
		t.Error("Write64 bounds")
	}
	if b, ok := mem.Read64(131072-3, 3); !ok || !bytes.Equal(b, []byte{1, 2, 3}) {
		t.Errorf("Read64 = %v %v", b, ok)
	}
	res, err := mod.ExportedFunction("load").Call(ctx, 1<<40)
	var re *wasm.RuntimeError
	if !errors.As(err, &re) || re.Error() != "memory access out of bounds" {
		t.Errorf("load past 4 GiB: %v %v", res, err)
	}
	res, _ = mod.ExportedFunction("size").Call(ctx)
	if res[0] != 2 {
		t.Errorf("memory.size = %d", res[0])
	}
}

func TestMemoryLimit(t *testing.T) {
	ctx, r := newRuntime(t, wasm.NewRuntimeConfig().WithMemoryLimitPages(2))
	_, err := r.CompileModule(ctx, compile(t, `(module (memory 3))`))
	var ce *wasm.CompileError
	if !errors.As(err, &ce) || ce.Msg != "initial memory size (3 pages) is larger than implementation limit (2 pages)" {
		t.Errorf("initial over the limit: %v", err)
	}
	_, err = r.CompileModule(ctx, compile(t, `(module (memory 1 4))`))
	if !errors.As(err, &ce) || ce.Msg != "maximum memory size (4 pages) is larger than implementation limit (2 pages)" {
		t.Errorf("maximum over the limit: %v", err)
	}
	_, err = r.CompileModule(ctx, compile(t, `(module (memory i64 3))`))
	if !errors.As(err, &ce) {
		t.Errorf("memory64 over the limit: %v", err)
	}
	mod := instantiate(t, ctx, r, `(module
		(memory 1)
		(func (export "grow") (param i32) (result i32) local.get 0 memory.grow))`)
	grow := mod.ExportedFunction("grow")
	if res, _ := grow.Call(ctx, 1); res[0] != 1 {
		t.Errorf("grow to the limit = %d", int32(res[0]))
	}
	if res, _ := grow.Call(ctx, 1); int32(res[0]) != -1 {
		t.Errorf("grow past the limit = %d", int32(res[0]))
	}

	// A memory made at its maximum does not move as it grows.
	ctx, r = newRuntime(t, wasm.NewRuntimeConfig().WithMemoryCapacityFromMax(true))
	mod = instantiate(t, ctx, r, `(module (memory (export "m") 1 4))`)
	mem := mod.Memory()
	view, _ := mem.Read(0, 4)
	mem.Grow(3)
	mem.WriteUint32Le(0, 0xcafe)
	if view[0] != 0xfe {
		t.Errorf("view moved: %x", view)
	}
}

func TestGlobals(t *testing.T) {
	ctx, r := newRuntime(t)
	mod := instantiate(t, ctx, r, `(module
		(global (export "const") i32 (i32.const -7))
		(global $counter (export "counter") (mut i64) (i64.const 1))
		(global (export "pi") f64 (f64.const 3.5))
		(global (export "vec") (mut v128) (v128.const i64x2 1 2))
		(func (export "bump") (result i64)
			(global.set $counter (i64.add (global.get $counter) (i64.const 1)))
			global.get $counter))`)
	c := mod.ExportedGlobal("const")
	if _, mutable := c.(api.MutableGlobal); mutable {
		t.Error("const is mutable")
	}
	if c.Type() != api.ValueTypeI32 || api.DecodeI32(c.Get()) != -7 || c.String() != "global(4294967289)" {
		t.Errorf("const = %d %s", c.Get(), c)
	}
	counter := mod.ExportedGlobal("counter").(api.MutableGlobal)
	counter.Set(40)
	res, _ := mod.ExportedFunction("bump").Call(ctx)
	if res[0] != 41 || counter.Get() != 41 {
		t.Errorf("bump = %d, counter %d", res[0], counter.Get())
	}
	if pi := mod.ExportedGlobal("pi"); api.DecodeF64(pi.Get()) != 3.5 || pi.String() != "global(3.500000)" {
		t.Errorf("pi = %s", pi)
	}
	vec := mod.ExportedGlobal("vec").(api.MutableGlobal)
	if v := vec.GetValue(); v.Type != api.ValueTypeV128 || v.Lo != 1 || v.Hi != 2 {
		t.Errorf("vec = %+v", v)
	}
	if err := vec.SetValue(api.ValueV128(3, 4)); err != nil {
		t.Fatal(err)
	}
	if lo, hi := vec.GetValue().V128(); lo != 3 || hi != 4 {
		t.Errorf("vec = %d %d", lo, hi)
	}
	if mod.ExportedGlobal("missing") != nil {
		t.Error("missing global found")
	}
}

func TestLinkingByName(t *testing.T) {
	ctx, r := newRuntime(t)
	_, err := r.InstantiateWithConfig(ctx, compile(t, `(module
		(memory (export "memory") 1)
		(global (export "base") i32 (i32.const 1000))
		(func (export "double") (param i32) (result i32) local.get 0 i32.const 2 i32.mul))`),
		wasm.NewModuleConfig().WithName("lib"))
	if err != nil {
		t.Fatal(err)
	}
	app := instantiate(t, ctx, r, `(module
		(import "lib" "double" (func $double (param i32) (result i32)))
		(import "lib" "memory" (memory 1))
		(import "lib" "base" (global $base i32))
		(func (export "run") (param i32) (result i32)
			(i32.store (i32.const 0) (call $double (local.get 0)))
			(i32.add (global.get $base) (i32.load (i32.const 0)))))`)
	res, err := app.ExportedFunction("run").Call(ctx, 21)
	if err != nil || res[0] != 1042 {
		t.Fatalf("run = %v, %v", res, err)
	}
	// The memory is the library's.
	if v, _ := r.Module("lib").Memory().ReadUint32Le(0); v != 42 {
		t.Errorf("lib memory holds %d", v)
	}
	// One compiled module, two names.
	c, err := r.CompileModule(ctx, compile(t, `(module (func (export "f") (result i32) i32.const 1))`))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b", "", ""} {
		if _, err := r.InstantiateModule(ctx, c, wasm.NewModuleConfig().WithName(name)); err != nil {
			t.Fatalf("%q: %v", name, err)
		}
	}
	if _, err := r.InstantiateModule(ctx, c, wasm.NewModuleConfig().WithName("a")); err == nil || err.Error() != "module[a] has already been instantiated" {
		t.Errorf("duplicate name: %v", err)
	}
	// A module of the name section's name is instantiated under it.
	named := instantiate(t, ctx, r, `(module $calc (func (export "f")))`)
	if named.Name() != "calc" || r.Module("calc") == nil {
		t.Errorf("name %q", named.Name())
	}
}

func TestLinkErrors(t *testing.T) {
	ctx, r := newRuntime(t)
	importer := compile(t, `(module (import "env" "f" (func (param i32))) (func (export "g") i32.const 1 call 0))`)
	for _, tc := range []struct {
		setup func()
		want  string
	}{
		{func() {}, `Import #0 "env" "f": module not found`},
		{func() {
			instantiate(t, ctx, r, `(module $env (func (export "other")))`)
		}, `Import #0 "env" "f": import not found`},
		{func() {
			r.Module("env").Close(ctx)
			instantiate(t, ctx, r, `(module $env (func (export "f")))`)
		}, `Import #0 "env" "f": imported function does not match the expected type`},
		{func() {
			r.Module("env").Close(ctx)
			instantiate(t, ctx, r, `(module $env (global (export "f") i32 (i32.const 0)))`)
		}, `Import #0 "env" "f": function import requires a callable`},
	} {
		tc.setup()
		_, err := r.Instantiate(ctx, importer)
		var le *wasm.LinkError
		if !errors.As(err, &le) || le.Error() != tc.want {
			t.Errorf("err = %v, want %q", err, tc.want)
		}
	}
	// A failed instantiation leaves its name free.
	if r.Module("") != nil {
		t.Error("anonymous module registered")
	}
}

func TestCompileErrors(t *testing.T) {
	ctx, r := newRuntime(t)
	for _, tc := range []struct {
		bin  []byte
		want string
	}{
		{compile(t, `(module (func (export "f") (result i32) i64.const 1 i32.const 1 i32.add))`),
			"Compiling function #0 failed: i32.add[0] expected type i32, found i64.const of type i64 @+31"},
		{compile(t, `(module (func (export "f") local.get 5 drop))`),
			"Compiling function #0 failed: invalid local index: 5 @+31"},
		{compile(t, `(module (memory 1) (func (export "f") (param i32) (result i32) local.get 0 i32.load offset=4 align=8))`),
			"Compiling function #0 failed: invalid alignment; expected maximum alignment is 2, actual alignment is 3 @+40"},
		{[]byte{0, 0x61, 0x73, 0x6e, 1, 0, 0, 0}, "expected magic word 00 61 73 6d, found 00 61 73 6e @+0"},
		{[]byte{0, 0x61, 0x73, 0x6d, 2, 0, 0, 0}, "expected version 01 00 00 00, found 02 00 00 00 @+4"},
	} {
		_, err := r.CompileModule(ctx, tc.bin)
		var ce *wasm.CompileError
		if !errors.As(err, &ce) || ce.Error() != tc.want {
			t.Errorf("err = %v, want %q", err, tc.want)
		}
	}
}

// traps is the module the stack trace tests compare with Node's: its error
// stacks there are
//
//	RuntimeError: divide by zero
//	    at inner (wasm://wasm/91c08aaa:wasm-function[0]:0x3a)
//	    at wasm://wasm/91c08aaa:wasm-function[1]:0x41
const traps = `(module
	(func $inner (param i32) (result i32) local.get 0 i32.const 0 i32.div_s)
	(func (export "outer") (param i32) (result i32) nop local.get 0 call $inner)
	(func (export "unr") unreachable)
	(func (export "rec") call 3))`

func TestTraps(t *testing.T) {
	ctx, r := newRuntime(t)
	mod := instantiate(t, ctx, r, `(module
		(memory 1)
		(table 1 funcref)
		(type $t (func))
		(func (export "unreachable") unreachable)
		(func (export "div") (param i32 i32) (result i32) local.get 0 local.get 1 i32.div_s)
		(func (export "rem") (param i32 i32) (result i32) local.get 0 local.get 1 i32.rem_u)
		(func (export "overflow") (result i32) i32.const 0x80000000 i32.const -1 i32.div_s)
		(func (export "trunc") (result i32) f64.const nan i32.trunc_f64_s)
		(func (export "oob") (result i32) i32.const 65535 i32.load)
		(func (export "null") i32.const 0 call_indirect (type $t))
		(func (export "table") i32.const 5 call_indirect (type $t))
		(func $rec (export "recurse") call $rec))`)
	for name, want := range map[string]string{
		"unreachable": "unreachable",
		"div":         "divide by zero",
		"rem":         "remainder by zero",
		"overflow":    "divide result unrepresentable",
		"trunc":       "float unrepresentable in integer range",
		"oob":         "memory access out of bounds",
		"null":        "function signature mismatch",
		"table":       "table index is out of bounds",
		"recurse":     "Maximum call stack size exceeded",
	} {
		params := make([]uint64, len(mod.ExportedFunction(name).Definition().ParamTypes()))
		_, err := mod.ExportedFunction(name).Call(ctx, params...)
		var re *wasm.RuntimeError
		if !errors.As(err, &re) || re.Error() != want {
			t.Errorf("%s: %v, want %q", name, err, want)
		}
	}
}

func TestTrapStack(t *testing.T) {
	ctx, r := newRuntime(t)
	mod := instantiate(t, ctx, r, traps)
	_, err := mod.ExportedFunction("outer").Call(ctx, 5)
	var re *wasm.RuntimeError
	if !errors.As(err, &re) {
		t.Fatalf("err = %v", err)
	}
	lines := strings.Split(re.Stack(), "\n")
	// The innermost frame is at the trapping instruction, its caller at
	// the call, as in Node.
	if len(lines) != 2 || lines[0] != "    at inner (wasm://wasm/91c08aaa:wasm-function[0]:0x3a)" ||
		lines[1] != "    at wasm://wasm/91c08aaa:wasm-function[1]:0x41" {
		t.Errorf("stack:\n%s", re.Stack())
	}
	if f := re.Frames()[1]; f.FunctionIndex != 1 || f.Offset != 0x41 || f.Name != "" {
		t.Errorf("frame %+v", f)
	}

	_, err = mod.ExportedFunction("rec").Call(ctx)
	if !errors.As(err, &re) {
		t.Fatalf("err = %v", err)
	}
	lines = strings.Split(re.Stack(), "\n")
	// The function that could not be entered, at the start of its body,
	// then its callers at their calls, as in Node.
	if len(lines) != 10 || lines[0] != "    at wasm://wasm/91c08aaa:wasm-function[3]:0x49" ||
		lines[1] != "    at wasm://wasm/91c08aaa:wasm-function[3]:0x4a" {
		t.Errorf("stack overflow's stack: %d lines\n%s", len(lines), re.Stack())
	}

	// Named after the module's name and its functions'. In Node:
	//	    at mymod.inner (wasm://wasm/mymod-de152af2:wasm-function[1]:0x43)
	//	    at mymod (wasm://wasm/mymod-de152af2:wasm-function[2]:0x4a)
	if _, err := r.NewHostModuleBuilder("env").NewFunctionBuilder().WithFunc(func() {}).Export("f").Instantiate(ctx); err != nil {
		t.Fatal(err)
	}
	named := instantiate(t, ctx, r, `(module $mymod
		(import "env" "f" (func $hostf))
		(func $inner (param i32) (result i32) local.get 0 i32.const 0 i32.div_s)
		(func (export "outer") (param i32) (result i32) nop local.get 0 call $inner)
		(func (export "callhost") call $hostf)
		(data "BIGDATA"))`)
	_, err = named.ExportedFunction("outer").Call(ctx, 1)
	if !errors.As(err, &re) {
		t.Fatalf("err = %v", err)
	}
	lines = strings.Split(re.Stack(), "\n")
	if len(lines) != 2 || lines[0] != "    at mymod.inner (wasm://wasm/mymod-de152af2:wasm-function[1]:0x43)" ||
		lines[1] != "    at mymod (wasm://wasm/mymod-de152af2:wasm-function[2]:0x4a)" {
		t.Errorf("named stack:\n%s", re.Stack())
	}

	// A module over 16,383 bytes is named by its length: the module above
	// with a custom section of 20,000 zeros, 0x4e86 bytes long.
	big := append(compile(t, traps), 0, 0xa4, 0x9c, 0x01, 3, 'p', 'a', 'd')
	big = append(big, make([]byte, 20000)...)
	bm, err := r.Instantiate(ctx, big)
	if err != nil {
		t.Fatal(err)
	}
	_, err = bm.ExportedFunction("unr").Call(ctx)
	if !errors.As(err, &re) || re.Stack() != "    at wasm://wasm/00013a1a:wasm-function[2]:0x46" {
		t.Errorf("big module: %v\n%s", err, re.Stack())
	}

	// Without debug information, frames are not named.
	ctx, r = newRuntime(t, wasm.NewRuntimeConfig().WithDebugInfoEnabled(false))
	mod = instantiate(t, ctx, r, traps)
	_, err = mod.ExportedFunction("outer").Call(ctx, 5)
	if !errors.As(err, &re) || !strings.HasPrefix(re.Stack(), "    at wasm://wasm/91c08aaa:wasm-function[0]:0x3a\n") {
		t.Errorf("stack without names:\n%s", re.Stack())
	}
}

func TestStartFunctions(t *testing.T) {
	ctx, r := newRuntime(t)
	src := `(module
		(global $g (export "g") (mut i32) (i32.const 1))
		(func $start (global.set $g (i32.mul (global.get $g) (i32.const 10))))
		(start $start)
		(func (export "_start") (global.set $g (i32.add (global.get $g) (i32.const 2))))
		(func (export "init") (global.set $g (i32.add (global.get $g) (i32.const 5)))))`
	mod := instantiate(t, ctx, r, src)
	if got := mod.ExportedGlobal("g").Get(); got != 12 {
		t.Errorf("start then _start: g = %d, want 12", got)
	}
	mod, err := r.InstantiateWithConfig(ctx, compile(t, src), wasm.NewModuleConfig().WithName("two").WithStartFunctions("init", "missing", "init"))
	if err != nil {
		t.Fatal(err)
	}
	if got := mod.ExportedGlobal("g").Get(); got != 20 {
		t.Errorf("start then init twice: g = %d, want 20", got)
	}

	// A trapping start function fails the instantiation and frees the
	// name.
	_, err = r.InstantiateWithConfig(ctx, compile(t, `(module (func $s unreachable) (start $s))`), wasm.NewModuleConfig().WithName("s"))
	var re *wasm.RuntimeError
	if !errors.As(err, &re) || re.Error() != "unreachable" {
		t.Errorf("start trap: %v", err)
	}
	if r.Module("s") != nil {
		t.Error("failed module registered")
	}
	_, err = r.InstantiateWithConfig(ctx, compile(t, `(module (func (export "_start") unreachable))`), wasm.NewModuleConfig().WithName("s"))
	if err == nil || err.Error() != "module[s] function[_start] failed: unreachable" || !errors.As(err, &re) {
		t.Errorf("_start trap: %v", err)
	}

	// An exit from _start: zero is success, the module closed.
	_, err = r.NewHostModuleBuilder("wasi").NewFunctionBuilder().
		WithFunc(func(ctx context.Context, m api.Module, code uint32) {
			_ = m.CloseWithExitCode(ctx, code)
			panic(sys.NewExitError(code))
		}).Export("proc_exit").Instantiate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	exiting := compile(t, `(module (import "wasi" "proc_exit" (func $exit (param i32)))
		(global $code (import "env" "code") i32)
		(func (export "_start") (call $exit (global.get $code))))`)
	instantiate(t, ctx, r, `(module $env (global (export "code") i32 (i32.const 0)))`)
	mod, err = r.InstantiateWithConfig(ctx, exiting, wasm.NewModuleConfig().WithName("ok"))
	if err != nil || mod == nil || !mod.IsClosed() {
		t.Errorf("exit 0: %v %v", mod, err)
	}
	r.Module("env").Close(ctx)
	instantiate(t, ctx, r, `(module $env (global (export "code") i32 (i32.const 3)))`)
	_, err = r.InstantiateWithConfig(ctx, exiting, wasm.NewModuleConfig().WithName("fail"))
	var exit *sys.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 3 || err.Error() != "module closed with exit_code(3)" {
		t.Errorf("exit 3: %v", err)
	}
}

func TestCloseOnContextDone(t *testing.T) {
	const loop = `(module (func (export "loop") (loop br 0)))`
	ctx, r := newRuntime(t, wasm.NewRuntimeConfig().WithCloseOnContextDone(true))
	mod := instantiate(t, ctx, r, loop)
	tctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	_, err := mod.ExportedFunction("loop").Call(tctx)
	var exit *sys.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != sys.ExitCodeDeadlineExceeded || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline: %v", err)
	}
	if !mod.IsClosed() {
		t.Error("module not closed")
	}
	if _, err := mod.ExportedFunction("loop").Call(ctx); !errors.As(err, &exit) || exit.ExitCode() != sys.ExitCodeDeadlineExceeded {
		t.Errorf("call after close: %v", err)
	}

	mod = instantiate(t, ctx, r, loop)
	cctx, cancel2 := context.WithCancel(ctx)
	time.AfterFunc(10*time.Millisecond, cancel2)
	_, err = mod.ExportedFunction("loop").Call(cctx)
	if !errors.As(err, &exit) || exit.ExitCode() != sys.ExitCodeContextCanceled || err.Error() != "module closed with context canceled" {
		t.Errorf("cancel: %v", err)
	}

	// Closing the module ends its running calls.
	mod = instantiate(t, ctx, r, loop)
	time.AfterFunc(10*time.Millisecond, func() { mod.CloseWithExitCode(ctx, 9) })
	_, err = mod.ExportedFunction("loop").Call(ctx)
	if !errors.As(err, &exit) || exit.ExitCode() != 9 {
		t.Errorf("close: %v", err)
	}

	// Without the option, the call returns the context's error and the
	// module stays open.
	ctx, r = newRuntime(t)
	mod = instantiate(t, ctx, r, loop)
	tctx, cancel3 := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel3()
	_, err = mod.ExportedFunction("loop").Call(tctx)
	if err != context.DeadlineExceeded || mod.IsClosed() {
		t.Errorf("without the option: %v, closed %v", err, mod.IsClosed())
	}
}

func TestClose(t *testing.T) {
	ctx := context.Background()
	r := wasm.NewRuntime(ctx)
	src := compile(t, `(module $m (func (export "f") (result i32) i32.const 1))`)
	mod, err := r.Instantiate(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	f := mod.ExportedFunction("f")
	if err := mod.CloseWithExitCode(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if err := mod.Close(ctx); err != nil {
		t.Errorf("second close: %v", err)
	}
	_, err = f.Call(ctx)
	var exit *sys.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 {
		t.Errorf("call of a closed module: %v", err)
	}
	if r.Module("m") != nil {
		t.Error("closed module still registered")
	}
	// Its name is free again.
	mod, err = r.Instantiate(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.CloseWithExitCode(ctx, 5); err != nil {
		t.Fatal(err)
	}
	if !mod.IsClosed() {
		t.Error("runtime close left a module open")
	}
	if _, err := mod.ExportedFunction("f").Call(ctx); !errors.As(err, &exit) || exit.ExitCode() != 5 {
		t.Errorf("call after runtime close: %v", err)
	}
	if _, err := r.CompileModule(ctx, src); err == nil || err.Error() != "runtime closed with exit_code(5)" {
		t.Errorf("compile after close: %v", err)
	}
}

func TestTables(t *testing.T) {
	ctx, r := newRuntime(t)
	mod := instantiate(t, ctx, r, `(module
		(type $ii (func (param i32) (result i32)))
		(table $t (export "funcs") 2 10 funcref)
		(table (export "externs") 1 externref)
		(func $inc (export "inc") (type $ii) local.get 0 i32.const 1 i32.add)
		(func $dbl (type $ii) local.get 0 i32.const 2 i32.mul)
		(elem (i32.const 0) $inc $dbl)
		(func (export "call") (param i32 i32) (result i32)
			local.get 1 local.get 0 call_indirect $t (type $ii)))`)
	funcs := mod.ExportedTable("funcs")
	if funcs.Type() != api.ValueTypeFuncref || funcs.Size() != 2 || funcs.Is64() {
		t.Fatalf("table %v size %d", funcs.Type(), funcs.Size())
	}
	if max, ok := funcs.Max(); !ok || max != 10 {
		t.Errorf("max %d %v", max, ok)
	}
	v, ok := funcs.Get(1)
	if !ok {
		t.Fatal("no element 1")
	}
	dbl, isFunc := v.Ref.(api.Function)
	if !isFunc {
		t.Fatalf("element 1 is %T", v.Ref)
	}
	if res, err := dbl.Call(ctx, 21); err != nil || res[0] != 42 {
		t.Errorf("element 1 called: %v %v", res, err)
	}
	// Swap the elements through the API.
	inc, _ := funcs.Get(0)
	if err := funcs.Set(0, v); err != nil {
		t.Fatal(err)
	}
	if err := funcs.Set(1, inc); err != nil {
		t.Fatal(err)
	}
	call := mod.ExportedFunction("call")
	if res, _ := call.Call(ctx, 0, 5); res[0] != 10 {
		t.Errorf("call 0 = %d", res[0])
	}
	if res, _ := call.Call(ctx, 1, 5); res[0] != 6 {
		t.Errorf("call 1 = %d", res[0])
	}
	if prev, err := funcs.Grow(2, api.ValueFuncref(mod.ExportedFunction("inc"))); err != nil || prev != 2 {
		t.Errorf("grow = %d %v", prev, err)
	}
	if res, _ := call.Call(ctx, 3, 5); res[0] != 6 {
		t.Errorf("call 3 = %d", res[0])
	}
	if _, err := funcs.Grow(7, api.Value{}); err == nil {
		t.Error("grew past the maximum")
	}
	if err := funcs.Set(0, api.ValueExternref("not a function")); err == nil {
		t.Error("set a host value in a funcref table")
	}
	if err := funcs.Set(10, api.Value{}); err == nil {
		t.Error("set out of range")
	}
	if _, ok := funcs.Get(4); ok {
		t.Error("got out of range")
	}

	externs := mod.ExportedTable("externs")
	if err := externs.Set(0, api.ValueExternref(map[string]int{"a": 1})); err != nil {
		t.Fatal(err)
	}
	got, _ := externs.Get(0)
	if m, ok := got.Ref.(map[string]int); !ok || m["a"] != 1 || got.Type != api.ValueTypeExternref {
		t.Errorf("externref element %#v", got)
	}
}

func TestCallValues(t *testing.T) {
	ctx, r := newRuntime(t)
	mod := instantiate(t, ctx, r, `(module
		(type $ii (func (param i32) (result i32)))
		(func (export "vec") (param v128 i32) (result v128 i32) local.get 0 local.get 1)
		(func (export "ext") (param externref) (result externref) local.get 0)
		(func (export "isnull") (param externref) (result i32) local.get 0 ref.is_null)
		(func (export "fn") (param funcref) (result funcref) local.get 0)
		(func (export "typed") (param (ref null $ii)) (result i32)
			local.get 0 ref.is_null)
		(func (export "inc") (type $ii) local.get 0 i32.const 1 i32.add)
		(func (export "noargs") (result f32) f32.const 2.5)
		(global $kept (mut externref) (ref.null extern))
		(func (export "keep") (param externref) local.get 0 global.set $kept)
		(func (export "kept") (result externref) global.get $kept))`)
	vec := mod.ExportedFunction("vec")
	res, err := vec.CallValues(ctx, api.ValueV128(0x0102030405060708, 0x1112131415161718), api.ValueI32(-1))
	if err != nil {
		t.Fatal(err)
	}
	if lo, hi := res[0].V128(); res[0].Type != api.ValueTypeV128 || lo != 0x0102030405060708 || hi != 0x1112131415161718 || res[1].I32() != -1 {
		t.Errorf("vec = %+v", res)
	}
	// Call takes a v128 as two uint64s.
	raw, err := vec.Call(ctx, 1, 2, 3)
	if err != nil || len(raw) != 3 || raw[0] != 1 || raw[1] != 2 || raw[2] != 3 {
		t.Errorf("vec Call = %v %v", raw, err)
	}

	type token struct{ name string }
	tok := &token{"x"}
	ext := mod.ExportedFunction("ext")
	res, err = ext.CallValues(ctx, api.ValueExternref(tok))
	if err != nil || res[0].Ref != tok || res[0].Type != api.ValueTypeExternref {
		t.Errorf("ext = %+v %v", res, err)
	}
	res, _ = ext.CallValues(ctx, api.ValueExternref(nil))
	if !res[0].IsNull() {
		t.Errorf("null ext = %+v", res)
	}
	if res, _ := mod.ExportedFunction("isnull").CallValues(ctx, api.ValueExternref(nil)); res[0].I32() != 1 {
		t.Errorf("ref.is_null(null) = %d", res[0].I32())
	}
	// Call passes an externref as a uintptr.
	if raw, err := ext.Call(ctx, 1234); err != nil || raw[0] != 1234 {
		t.Errorf("ext Call = %v %v", raw, err)
	}
	if _, err := ext.Call(ctx, 0); err != nil {
		t.Errorf("null ext Call: %v", err)
	}
	// An externref Call cannot encode it refuses.
	keep := mod.ExportedFunction("keep")
	if _, err := keep.CallValues(ctx, api.ValueExternref(tok)); err != nil {
		t.Fatal(err)
	}
	if _, err := mod.ExportedFunction("kept").Call(ctx); err == nil || err.Error() != "result[0]: externref value that is not a uintptr cannot be returned as a uint64: use CallValues" {
		t.Errorf("kept: %v", err)
	}

	inc := mod.ExportedFunction("inc")
	fn := mod.ExportedFunction("fn")
	res, err = fn.CallValues(ctx, api.ValueFuncref(inc))
	if err != nil {
		t.Fatal(err)
	}
	back, ok := res[0].Ref.(api.Function)
	if !ok {
		t.Fatalf("fn returned %T", res[0].Ref)
	}
	if r, err := back.Call(ctx, 1); err != nil || r[0] != 2 {
		t.Errorf("returned function = %v %v", r, err)
	}
	if _, err := fn.Call(ctx, 0); err != nil {
		t.Errorf("null funcref Call: %v", err)
	}
	if _, err := fn.CallValues(ctx, api.ValueExternref("x")); err == nil || !strings.Contains(err.Error(), "type incompatibility") {
		t.Errorf("host value as funcref: %v", err)
	}
	if _, err := fn.Call(ctx, 1); err == nil || !strings.Contains(err.Error(), "CallValues") {
		t.Errorf("non-null funcref Call: %v", err)
	}
	typed := mod.ExportedFunction("typed")
	if res, err := typed.CallValues(ctx, api.ValueFuncref(inc)); err != nil || res[0].I32() != 0 {
		t.Errorf("typed = %v %v", res, err)
	}
	if _, err := typed.CallValues(ctx, api.ValueFuncref(mod.ExportedFunction("noargs"))); err == nil {
		t.Error("function of another type passed as (ref null $ii)")
	}
	if _, err := typed.CallValues(ctx); err == nil || err.Error() != "expected 1 params, but passed 0" {
		t.Errorf("missing param: %v", err)
	}
	if res, _ := mod.ExportedFunction("noargs").CallValues(ctx); res[0].F32() != 2.5 || res[0].Type != api.ValueTypeF32 {
		t.Errorf("noargs = %+v", res)
	}
}

func TestCompiledModule(t *testing.T) {
	ctx, r := newRuntime(t, wasm.NewRuntimeConfig().WithCustomSections(true))
	c, err := r.CompileModule(ctx, compile(t, `(module $lib
		(import "env" "log" (func $log (param i32)))
		(import "env" "mem" (memory 1 2))
		(func $add (export "add") (export "plus") (param $a i32) (param $b i32) (result i32)
			local.get $a local.get $b i32.add)
		(memory (export "own") i64 1)
		(@custom "meta" "data"))`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Name() != "lib" {
		t.Errorf("name %q", c.Name())
	}
	imports := c.ImportedFunctions()
	if len(imports) != 1 {
		t.Fatalf("imports %v", imports)
	}
	if mod, name, ok := imports[0].Import(); !ok || mod != "env" || name != "log" || imports[0].Name() != "log" || imports[0].DebugName() != "lib.log" {
		t.Errorf("import %s %s %v name %q", mod, name, ok, imports[0].Name())
	}
	exports := c.ExportedFunctions()
	add := exports["add"]
	if add == nil || exports["plus"] != nil && exports["plus"].Index() != 1 || add.Index() != 1 || add.Name() != "add" {
		t.Fatalf("exports %v", exports)
	}
	if got := strings.Join(add.ParamNames(), ","); got != "a,b" {
		t.Errorf("param names %q", got)
	}
	if got := add.ExportNames(); len(got) != 2 {
		t.Errorf("export names %v", got)
	}
	mems := c.ImportedMemories()
	if len(mems) != 1 || mems[0].Min() != 1 {
		t.Errorf("imported memories %v", mems)
	}
	if max, ok := mems[0].Max(); !ok || max != 2 {
		t.Errorf("imported memory max %d %v", max, ok)
	}
	if own := c.ExportedMemories()["own"]; own == nil || !own.Is64() || own.Min64() != 1 {
		t.Errorf("exported memories %v", c.ExportedMemories())
	}
	found := false
	for _, s := range c.CustomSections() {
		if s.Name() == "meta" && string(s.Data()) == "data" {
			found = true
		}
	}
	if !found {
		t.Errorf("custom section not found in %v", c.CustomSections())
	}
	if err := c.Close(ctx); err != nil {
		t.Error(err)
	}

	// Without WithCustomSections there are none.
	ctx, r = newRuntime(t)
	c, _ = r.CompileModule(ctx, compile(t, `(module (@custom "meta" "data"))`))
	if c.CustomSections() != nil {
		t.Error("custom sections kept")
	}
}

func TestCompilationCache(t *testing.T) {
	cache := wasm.NewCompilationCache()
	defer cache.Close(context.Background())
	cfg := wasm.NewRuntimeConfigCompiler().WithCompilationCache(cache)
	bin := compile(t, `(module (func (export "f") (result i32) i32.const 7))`)
	for i := 0; i < 2; i++ {
		ctx, r := newRuntime(t, cfg)
		mod, err := r.Instantiate(ctx, bin)
		if err != nil {
			t.Fatal(err)
		}
		if res, err := mod.ExportedFunction("f").Call(ctx); err != nil || res[0] != 7 {
			t.Errorf("run %d: %v %v", i, res, err)
		}
	}
}

func TestSysContext(t *testing.T) {
	ctx, r := newRuntime(t)
	var out bytes.Buffer
	var seen *sysctx.Context
	_, err := r.NewHostModuleBuilder("sys").NewFunctionBuilder().
		WithFunc(func(ctx context.Context, m api.Module) {
			seen = sysctx.FromModule(m)
			seen.Stdout.Write([]byte(strings.Join(seen.Args, " ")))
		}).Export("hello").Instantiate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sysctx.FromModule(r.Module("sys")) != nil {
		t.Error("host module has a system context")
	}
	_, err = r.InstantiateWithConfig(ctx, compile(t, `(module (import "sys" "hello" (func $h)) (func (export "_start") call $h))`),
		wasm.NewModuleConfig().WithArgs("prog", "arg").WithEnv("A", "1").WithEnv("B", "2").WithEnv("A", "3").WithStdout(&out))
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != "prog arg" {
		t.Errorf("stdout %q", out.String())
	}
	if got := strings.Join(seen.Environ, ","); got != "A=3,B=2" {
		t.Errorf("environ %q", got)
	}
	s1, _ := seen.Walltime()
	if s1 != 1640995200 {
		t.Errorf("fake walltime %d", s1)
	}
	if a, b := seen.Nanotime(), seen.Nanotime(); b-a != int64(time.Millisecond) {
		t.Errorf("fake nanotime %d %d", a, b)
	}
	if n, err := seen.Stdin.Read(make([]byte, 1)); n != 0 || err == nil {
		t.Errorf("stdin %d %v", n, err)
	}
	_, err = r.InstantiateWithConfig(ctx, compile(t, `(module)`), wasm.NewModuleConfig().WithEnv("A=B", "1"))
	if err == nil || err.Error() != "environ invalid: key contains '=' character" {
		t.Errorf("bad env: %v", err)
	}
}

func TestConcurrentInstances(t *testing.T) {
	ctx, r := newRuntime(t)
	c, err := r.CompileModule(ctx, compile(t, `(module
		(global $n (mut i64) (i64.const 0))
		(func (export "count") (param i64) (result i64)
			(loop $l
				(global.set $n (i64.add (global.get $n) (i64.const 1)))
				(br_if $l (i64.lt_u (global.get $n) (local.get 0))))
			global.get $n))`))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mod, err := r.InstantiateModule(ctx, c, wasm.NewModuleConfig().WithName(""))
			if err != nil {
				errs <- err
				return
			}
			res, err := mod.ExportedFunction("count").Call(ctx, 100000)
			if err != nil || res[0] != 100000 {
				errs <- errors.New("bad count")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestFeatures(t *testing.T) {
	cfg := wasm.NewRuntimeConfig()
	if cfg.CoreFeatures() != api.CoreFeaturesAll {
		t.Errorf("default features %s", cfg.CoreFeatures())
	}
	v1 := cfg.WithCoreFeatures(api.CoreFeaturesV1)
	if v1.CoreFeatures() != api.CoreFeaturesV1 || cfg.CoreFeatures() != api.CoreFeaturesAll {
		t.Error("WithCoreFeatures changed the original")
	}
	// Every feature stays on: a module using tail calls compiles.
	ctx, r := newRuntime(t, v1)
	mod := instantiate(t, ctx, r, `(module
		(func $f (param i32) (result i32) local.get 0)
		(func (export "g") (result i32) i32.const 3 return_call $f))`)
	if res, err := mod.ExportedFunction("g").Call(ctx); err != nil || res[0] != 3 {
		t.Errorf("g = %v %v", res, err)
	}
	if got := api.CoreFeaturesV2.String(); got != "bulk-memory-operations|multi-value|mutable-global|nontrapping-float-to-int-conversion|reference-types|sign-extension-ops|simd" {
		t.Errorf("V2 = %s", got)
	}
	if !api.CoreFeaturesV3.IsEnabled(api.CoreFeatureGC) || api.CoreFeaturesV2.IsEnabled(api.CoreFeatureGC) {
		t.Error("IsEnabled")
	}
	if err := api.CoreFeaturesV1.RequireEnabled(api.CoreFeatureSIMD); err == nil || err.Error() != `feature "simd" is disabled` {
		t.Errorf("RequireEnabled: %v", err)
	}
}

func TestMemoryLimitPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("no panic")
		}
	}()
	wasm.NewRuntimeConfig().WithMemoryLimitPages(math.MaxUint32)
}
