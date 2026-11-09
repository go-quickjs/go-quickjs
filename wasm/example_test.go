package wasm_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/go-quickjs/go-quickjs/wasm"
	"github.com/go-quickjs/go-quickjs/wasm/api"
	"github.com/go-quickjs/go-quickjs/wasm/sys"
	"github.com/go-quickjs/go-quickjs/wasm/wat"
)

// mustWat compiles a module in the text format; a program would read a
// .wasm file instead.
func mustWat(src string) []byte {
	b, err := wat.Compile([]byte(src))
	if err != nil {
		log.Fatal(err)
	}
	return b
}

// Instantiate a module and call one of its functions.
func Example() {
	ctx := context.Background()
	r := wasm.NewRuntime(ctx)
	defer r.Close(ctx)

	mod, err := r.Instantiate(ctx, mustWat(`(module
		(func (export "add") (param i32 i32) (result i32)
			local.get 0 local.get 1 i32.add))`))
	if err != nil {
		log.Fatal(err)
	}
	res, err := mod.ExportedFunction("add").Call(ctx, api.EncodeI32(40), api.EncodeI32(2))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(api.DecodeI32(res[0]))
	// Output: 42
}

// Give a module functions written in Go, which read its memory.
func Example_hostFunctions() {
	ctx := context.Background()
	r := wasm.NewRuntime(ctx)
	defer r.Close(ctx)

	_, err := r.NewHostModuleBuilder("env").
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, m api.Module, offset, length uint32) {
			b, _ := m.Memory().Read(offset, length)
			fmt.Println(string(b))
		}).
		Export("print").
		Instantiate(ctx)
	if err != nil {
		log.Fatal(err)
	}

	// The module's start function prints a greeting.
	_, err = r.Instantiate(ctx, mustWat(`(module
		(import "env" "print" (func $print (param i32 i32)))
		(memory 1)
		(data (i32.const 8) "hello, world")
		(func $main (call $print (i32.const 8) (i32.const 12)))
		(start $main))`))
	if err != nil {
		log.Fatal(err)
	}
	// Output: hello, world
}

// Link two modules: the second imports the first by the name it was
// instantiated under.
func Example_linking() {
	ctx := context.Background()
	r := wasm.NewRuntime(ctx)
	defer r.Close(ctx)

	_, err := r.InstantiateWithConfig(ctx, mustWat(`(module
		(func (export "square") (param i64) (result i64)
			local.get 0 local.get 0 i64.mul))`),
		wasm.NewModuleConfig().WithName("math"))
	if err != nil {
		log.Fatal(err)
	}
	app, err := r.Instantiate(ctx, mustWat(`(module
		(import "math" "square" (func $square (param i64) (result i64)))
		(func (export "fourth") (param i64) (result i64)
			(call $square (call $square (local.get 0)))))`))
	if err != nil {
		log.Fatal(err)
	}
	res, _ := app.ExportedFunction("fourth").Call(ctx, 3)
	fmt.Println(res[0])

	_, err = r.Instantiate(ctx, mustWat(`(module (import "physics" "g" (global f64)))`))
	fmt.Println(err)
	// Output:
	// 81
	// Import #0 "physics" "g": module not found
}

// A trap returns a RuntimeError, whose stack names the functions on the call
// stack and where in the module each was.
func Example_trap() {
	ctx := context.Background()
	r := wasm.NewRuntime(ctx)
	defer r.Close(ctx)

	mod, err := r.Instantiate(ctx, mustWat(`(module $calc
		(func $divide (param i32 i32) (result i32)
			local.get 0 local.get 1 i32.div_u)
		(func (export "ratio") (param i32 i32) (result i32)
			local.get 0 local.get 1 call $divide))`))
	if err != nil {
		log.Fatal(err)
	}
	_, err = mod.ExportedFunction("ratio").Call(ctx, 1, 0)
	var re *wasm.RuntimeError
	if errors.As(err, &re) {
		fmt.Println(re.Error())
		fmt.Println(re.Stack())
	}
	// Output:
	// divide by zero
	//     at calc.divide (wasm://wasm/calc-180f7c6a:wasm-function[0]:0x2a)
	//     at calc (wasm://wasm/calc-180f7c6a:wasm-function[1]:0x32)
}

// Pass values that Call's uint64s cannot hold: a v128, and a Go value as an
// externref.
func Example_callValues() {
	ctx := context.Background()
	r := wasm.NewRuntime(ctx)
	defer r.Close(ctx)

	mod, err := r.Instantiate(ctx, mustWat(`(module
		(func (export "pass") (param v128 externref) (result externref v128)
			local.get 1 local.get 0))`))
	if err != nil {
		log.Fatal(err)
	}
	res, err := mod.ExportedFunction("pass").CallValues(ctx,
		api.ValueV128(1, 2), api.ValueExternref("a Go string"))
	if err != nil {
		log.Fatal(err)
	}
	lo, hi := res[1].V128()
	fmt.Println(res[0].Ref, lo, hi)
	// Output: a Go string 1 2
}

// End a call that runs too long.
func Example_contextDone() {
	ctx := context.Background()
	r := wasm.NewRuntimeWithConfig(ctx, wasm.NewRuntimeConfig().WithCloseOnContextDone(true))
	defer r.Close(ctx)

	mod, err := r.Instantiate(ctx, mustWat(`(module (func (export "spin") (loop br 0)))`))
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	_, err = mod.ExportedFunction("spin").Call(ctx)
	var exit *sys.ExitError
	fmt.Println(errors.As(err, &exit), err, mod.IsClosed())
	// Output: true module closed with context deadline exceeded true
}
