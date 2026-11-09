// Command wazero is wasmbench's worker for wazero: given a request on its
// standard input, it measures the workload on wazero's interpreter
// ("wazero-interp") or its compiler ("wazero-compiler") with the worker
// package go-quickjs's own runner uses, and writes the result to its standard
// output. internal/cmd/wasmbench builds and runs it; it is a module of its
// own, so that go-quickjs never depends on wazero.
package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"

	"github.com/go-quickjs/go-quickjs/internal/cmd/wasmbench/internal/worker"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
)

func main() { worker.Serve(newEngine) }

func newEngine(name string) (worker.Engine, error) {
	var cfg wazero.RuntimeConfig
	switch name {
	case "wazero-interp":
		cfg = wazero.NewRuntimeConfigInterpreter()
	case "wazero-compiler":
		cfg = wazero.NewRuntimeConfigCompiler()
	default:
		return nil, fmt.Errorf("wazero has no engine %q", name)
	}
	ctx := context.Background()
	r := wazero.NewRuntimeWithConfig(ctx, cfg)
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, r); err != nil {
		return nil, err
	}
	return &wazeroEngine{r}, nil
}

type wazeroEngine struct{ r wazero.Runtime }

func (e *wazeroEngine) Compile(ctx context.Context, bin []byte) (worker.Module, error) {
	cm, err := e.r.CompileModule(ctx, bin)
	if err != nil {
		return nil, err
	}
	return &wazeroModule{e.r, cm}, nil
}

func (e *wazeroEngine) Close(ctx context.Context) error { return e.r.Close(ctx) }

type wazeroModule struct {
	r  wazero.Runtime
	cm wazero.CompiledModule
}

func (m *wazeroModule) Instantiate(ctx context.Context) (worker.Instance, error) {
	mod, err := m.r.InstantiateModule(ctx, m.cm, wazero.NewModuleConfig().WithName(""))
	if err != nil {
		return nil, err
	}
	return wazeroInstance{mod}, nil
}

func (m *wazeroModule) RunWASI(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	cfg := wazero.NewModuleConfig().WithName("").WithArgs(args...).
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

type wazeroInstance struct{ m api.Module }

func (i wazeroInstance) Call(ctx context.Context, name string, arg uint32) (uint64, error) {
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

func (i wazeroInstance) Close(ctx context.Context) error { return i.m.Close(ctx) }
