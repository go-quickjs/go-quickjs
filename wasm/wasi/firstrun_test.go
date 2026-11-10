package wasi_test

import (
	"context"
	"errors"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/go-quickjs/go-quickjs/wasm"
	"github.com/go-quickjs/go-quickjs/wasm/internal/engine"
	"github.com/go-quickjs/go-quickjs/wasm/sys"
	"github.com/go-quickjs/go-quickjs/wasm/wasi"
)

// TestFirstRun breaks a WASI program's first run down by what it spent:
// decoding and validating the module, the interpreter's compiles of its
// functions, the baseline JIT's compiles and their placement, and the
// rest, running; and times a second run, whose code is all compiled.
// WASM_FIRSTRUN names the module, WASM_FIRSTRUN_ARGS its arguments (split
// at spaces) and WASM_FIRSTRUN_ENGINE the engine, interp or jit (the
// default where the JIT runs). Run it in a fresh process for each program,
// as wasmbench measures.
func TestFirstRun(t *testing.T) {
	path := os.Getenv("WASM_FIRSTRUN")
	if path == "" {
		t.Skip("WASM_FIRSTRUN is not set")
	}
	bin, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	args := append([]string{"prog"}, strings.Fields(os.Getenv("WASM_FIRSTRUN_ARGS"))...)
	cfg := wasm.NewRuntimeConfigInterpreter()
	switch e := os.Getenv("WASM_FIRSTRUN_ENGINE"); {
	case e == "interp":
	case e == "jit" || e == "" && engine.JITAvailable():
		cfg = wasm.NewRuntimeConfigCompiler()
	}
	ctx := context.Background()
	r := wasm.NewRuntimeWithConfig(ctx, cfg)
	defer r.Close(ctx)
	wasi.MustInstantiate(ctx, r)

	var ms0, ms1 runtime.MemStats
	runtime.ReadMemStats(&ms0)
	t0 := time.Now()
	compiled, err := r.CompileModule(ctx, bin)
	if err != nil {
		t.Fatal(err)
	}
	decode := time.Since(t0)
	run := func() time.Duration {
		start := time.Now()
		mod, err := r.InstantiateModule(ctx, compiled, wasm.NewModuleConfig().WithName("").WithArgs(args...).
			WithStdout(io.Discard).WithStderr(io.Discard).WithSysWalltime().WithSysNanotime())
		if mod != nil {
			mod.Close(ctx)
		}
		var exit *sys.ExitError
		if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 0) {
			t.Fatal(err)
		}
		return time.Since(start)
	}
	st0 := engine.ReadJITStats()
	first := run()
	runtime.ReadMemStats(&ms1)
	st1 := engine.ReadJITStats()
	second := run()
	st2 := engine.ReadJITStats()

	interp, base, place := st1.InterpTime-st0.InterpTime, st1.BaselineTime-st0.BaselineTime, st1.PlaceTime-st0.PlaceTime
	t.Logf("decode+validate %v", decode.Round(10*time.Microsecond))
	t.Logf("first run %v: interp compile %v (%d fns), baseline compile %v (%d fns), place %v, rest %v; allocated %.1f MiB, %d objects",
		first.Round(10*time.Microsecond), interp.Round(10*time.Microsecond), st1.Interpreted-st0.Interpreted,
		base.Round(10*time.Microsecond), st1.Compiled-st0.Compiled, place.Round(10*time.Microsecond),
		(first - interp - base - place).Round(10*time.Microsecond),
		float64(ms1.TotalAlloc-ms0.TotalAlloc)/(1<<20), ms1.Mallocs-ms0.Mallocs)
	t.Logf("first run: optimizing tier %d fns, %d refused, compiling %v (in the background or not)",
		st1.Optimized-st0.Optimized, optRefused(st1)-optRefused(st0), (st1.OptTime - st0.OptTime).Round(10*time.Microsecond))
	t.Logf("second run %v: %d more interp compiles, %d more baseline compiles, %d more optimized", second.Round(10*time.Microsecond),
		st2.Interpreted-st1.Interpreted, st2.Compiled-st1.Compiled, st2.Optimized-st1.Optimized)
}

// optRefused is the number of functions the optimizing tier refused.
func optRefused(st engine.JITStats) int {
	n := 0
	for _, k := range st.OptRefused {
		n += k
	}
	return n
}
