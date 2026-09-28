// Command goja runs the V8 version 7 benchmark suite on goja, the same way
// internal/cmd/v8bench runs it on go-quickjs, for comparing the two; see
// package v8bench for its modes.
//
// It is a module of its own, so that go-quickjs does not depend on goja: it
// is run from its directory, and builds against the go-quickjs beside it.
//
//	cd internal/cmd/v8bench/goja
//	go run . -dir /tmp/v8-v7 -fetch -mode fixed
package main

import (
	"github.com/dop251/goja"
	"github.com/go-quickjs/go-quickjs/internal/v8bench"
)

func main() { v8bench.Main("goja", engine{}) }

type engine struct{}

func (engine) Compile(name, src string) error {
	_, err := goja.Compile(name, src, false)
	return err
}

func (engine) NewRuntime(print func(string), load func(string) (string, error)) (v8bench.Runtime, error) {
	vm := goja.New()
	if err := vm.Set("print", func(call goja.FunctionCall) goja.Value {
		print(call.Argument(0).String())
		return goja.Undefined()
	}); err != nil {
		return nil, err
	}
	err := vm.Set("load", func(call goja.FunctionCall) goja.Value {
		name := call.Argument(0).String()
		src, err := load(name)
		if err == nil {
			_, err = vm.RunScript(name, src)
		}
		if err != nil {
			// A script's exception goes on as itself; anything else is thrown
			// as goja throws what a Go function reports.
			if ex, ok := err.(*goja.Exception); ok {
				panic(ex)
			}
			panic(vm.NewGoError(err))
		}
		return goja.Undefined()
	})
	return runtimeOf{vm}, err
}

type runtimeOf struct{ vm *goja.Runtime }

func (r runtimeOf) Run(name, src string) error {
	_, err := r.vm.RunScript(name, src)
	return err
}

func (r runtimeOf) EvalString(src string) (string, error) {
	v, err := r.vm.RunString(src)
	if err != nil {
		return "", err
	}
	return v.String(), nil
}
