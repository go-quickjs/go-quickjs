// Command v8bench runs the V8 version 7 benchmark suite on go-quickjs, for
// measuring and profiling the engine; see package v8bench for its modes.
// internal/cmd/v8bench/goja runs the same suite, the same way, on goja.
//
//	go run ./internal/cmd/v8bench -dir /tmp/v8-v7 -fetch -mode fixed
package main

import (
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
	_, err := r.rt.EvalFile(name, src)
	return err
}

func (r runtimeOf) EvalString(src string) (string, error) {
	v, err := r.rt.Eval(src)
	if err != nil {
		return "", err
	}
	return v.String(), nil
}
