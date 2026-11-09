package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"

	"github.com/go-quickjs/go-quickjs/internal/cmd/wasmbench/internal/worker"
	"github.com/go-quickjs/go-quickjs/wasm"
	"github.com/go-quickjs/go-quickjs/wasm/api"
	"github.com/go-quickjs/go-quickjs/wasm/sys"
	"github.com/go-quickjs/go-quickjs/wasm/wasi"
)

// newEngine is go-quickjs's wasm runtime for a worker: "interp" runs
// everything in the interpreter, "jit" compiles hot functions to native code.
func newEngine(name string) (worker.Engine, error) {
	var cfg wasm.RuntimeConfig
	switch name {
	case "interp":
		cfg = wasm.NewRuntimeConfigInterpreter()
	case "jit":
		if !jitBuilt {
			return nil, errors.New("this build has no JIT: build wasmbench with -tags quickjs_jit on amd64 Linux or Windows")
		}
		cfg = wasm.NewRuntimeConfigCompiler()
	default:
		return nil, fmt.Errorf("go-quickjs has no engine %q", name)
	}
	ctx := context.Background()
	r := wasm.NewRuntimeWithConfig(ctx, cfg)
	if _, err := wasi.Instantiate(ctx, r); err != nil {
		return nil, err
	}
	return &quickjsEngine{r}, nil
}

type quickjsEngine struct{ r wasm.Runtime }

func (e *quickjsEngine) Compile(ctx context.Context, bin []byte) (worker.Module, error) {
	cm, err := e.r.CompileModule(ctx, bin)
	if err != nil {
		return nil, err
	}
	return &quickjsModule{e.r, cm}, nil
}

func (e *quickjsEngine) Close(ctx context.Context) error { return e.r.Close(ctx) }

type quickjsModule struct {
	r  wasm.Runtime
	cm wasm.CompiledModule
}

func (m *quickjsModule) Instantiate(ctx context.Context) (worker.Instance, error) {
	mod, err := m.r.InstantiateModule(ctx, m.cm, wasm.NewModuleConfig().WithName(""))
	if err != nil {
		return nil, err
	}
	return quickjsInstance{mod}, nil
}

func (m *quickjsModule) RunWASI(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	cfg := wasm.NewModuleConfig().WithName("").WithArgs(args...).
		WithStdout(stdout).WithStderr(stderr).
		WithSysWalltime().WithSysNanotime().WithSysNanosleep().
		WithRandSource(rand.Reader)
	mod, err := m.r.InstantiateModule(ctx, m.cm, cfg)
	if mod != nil {
		mod.Close(ctx)
	}
	var exit *sys.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 0 {
		err = nil
	}
	return err
}

type quickjsInstance struct{ m api.Module }

func (i quickjsInstance) Call(ctx context.Context, name string, arg uint32) (uint64, error) {
	fn := i.m.ExportedFunction(name)
	if fn == nil {
		return 0, fmt.Errorf("no export %q", name)
	}
	res, err := fn.Call(ctx, uint64(arg))
	if err != nil {
		return 0, err
	}
	if len(res) != 1 {
		return 0, fmt.Errorf("%s returned %d results", name, len(res))
	}
	return res[0], nil
}

func (i quickjsInstance) Close(ctx context.Context) error { return i.m.Close(ctx) }
