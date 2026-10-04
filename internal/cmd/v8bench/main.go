// Command v8bench runs the V8 version 7 benchmark suite on go-quickjs, for
// measuring and profiling the engine; see package v8bench for its modes.
// internal/cmd/v8bench/goja runs the same suite, the same way, on goja.
//
//	go run ./internal/cmd/v8bench -dir /tmp/v8-v7 -fetch -mode fixed
//
// V8BENCH_STACKPAD=n runs every script n frames of 64 bytes deeper in the
// goroutine's stack, which moves where the engine's frames and spill slots
// fall against the heap; internal/cmd/v8bench/placements varies it from
// placement to placement.
package main

import (
	"os"
	"strconv"

	quickjs "github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/internal/v8bench"
)

func main() { v8bench.Main("go-quickjs", engine{}) }

type engine struct{}

func (engine) Compile(name, src string) error {
	_, err := quickjs.Compile(name, src)
	return err
}

func (engine) NewRuntime(print func(string), load func(string) (string, error)) (v8bench.Runtime, error) {
	rt := quickjs.New()
	if err := rt.Set("print", print); err != nil {
		return nil, err
	}
	err := rt.Set("load", func(r *quickjs.Runtime, name string) error {
		src, err := load(name)
		if err != nil {
			return err
		}
		_, err = r.EvalFile(name, src)
		return err
	})
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
