// Command v8bench runs the V8 version 7 benchmark suite, and QuickJS's
// micro-benchmarks, on go-quickjs, for measuring and profiling the engine;
// see package v8bench for its modes. internal/cmd/v8bench/goja runs the same
// suite, the same way, on goja.
//
//	go run ./internal/cmd/v8bench -dir /tmp/v8-v7 -fetch -mode fixed
//	go run ./internal/cmd/v8bench -mode micro -suite empty_loop,func_call
//
// V8BENCH_STACKPAD=n runs every script n frames of 64 bytes deeper in the
// goroutine's stack, which moves where the engine's frames and spill slots
// fall against the heap; V8BENCH_JITPAD=n, in a JIT build, has the JIT
// place its first code n bytes into its arena (jit.FirstOffset), and
// V8BENCH_JITSEED=s has it vary where the rest falls, each later chunk's
// start and the gaps between functions (jit.LayoutSeed): together they
// move where its code falls against the op cache's sets.
// internal/cmd/v8bench/placements varies both from placement to placement.
package main

import (
	"errors"
	"flag"
	"os"
	"strconv"

	quickjs "github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/internal/jit"
	"github.com/go-quickjs/go-quickjs/internal/v8bench"
)

func main() { v8bench.Main("go-quickjs", engine{}) }

type engine struct{}

var (
	jitFlag       = flag.Bool("jit", false, "enable the optional native numeric tier")
	jitReportFlag = flag.Bool("jitreport", false, "after each benchmark, print what the JIT compiled, refused and skipped (fixed and micro modes, with -jit)")
	jitMemFlag    = flag.Int("jitmem", 32, "the JIT's memory budget in MiB, with -jit; 0 for the runtime's default (8 MiB)")
)

func (engine) Compile(name, src string) error {
	_, err := quickjs.Compile(name, src)
	return err
}

func (engine) NewRuntime(host v8bench.Host) (v8bench.Runtime, error) {
	if *jitFlag && !jit.Supported() {
		return nil, errors.New("JIT benchmarking requires a supported build with -tags quickjs_jit")
	}
	var opts []quickjs.Option
	if *jitFlag {
		opts = append(opts, quickjs.WithJIT())
		if *jitMemFlag > 0 {
			opts = append(opts, quickjs.WithJITMemoryLimit(int64(*jitMemFlag)<<20))
		}
	}
	rt := quickjs.New(opts...)
	if err := rt.Set("print", host.Print); err != nil {
		return nil, err
	}
	if err := rt.Set("now", host.Now); err != nil {
		return nil, err
	}
	if err := rt.Set("read", host.Load); err != nil {
		return nil, err
	}
	err := rt.Set("load", func(r *quickjs.Runtime, name string) error {
		src, err := host.Load(name)
		if err != nil {
			return err
		}
		_, err = r.EvalFile(name, src)
		return err
	})
	reportStart(runtimeOf{rt})
	return runtimeOf{rt}, err
}

type runtimeOf struct{ rt *quickjs.Runtime }

func (r runtimeOf) Run(name, src string) error {
	return padded(stackPad, func() error {
		_, err := r.rt.EvalFile(name, src)
		return err
	})
}

func (r runtimeOf) EvalString(src string) (string, error) {
	var v quickjs.Value
	err := padded(stackPad, func() error {
		var err error
		v, err = r.rt.Eval(src)
		return err
	})
	if err != nil {
		return "", err
	}
	return v.String(), nil
}

// stackPad is V8BENCH_STACKPAD, the frames padded calls f under.
var stackPad, _ = strconv.Atoi(os.Getenv("V8BENCH_STACKPAD"))

var padSink byte

// padded calls f n frames deeper, each with 64 bytes of its own.
//
//go:noinline
func padded(n int, f func() error) error {
	var pad [64]byte
	if n > 0 {
		pad[n%64] = byte(n)
		err := padded(n-1, f)
		padSink += pad[n%64]
		return err
	}
	return f()
}
