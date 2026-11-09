// Package wasm runs WebAssembly modules from Go, in pure Go.
//
// A Runtime compiles modules, instantiates them under names, and holds them
// for other modules to import by those names:
//
//	ctx := context.Background()
//	r := wasm.NewRuntime(ctx)
//	defer r.Close(ctx)
//
//	_, err := r.NewHostModuleBuilder("env").
//		NewFunctionBuilder().
//		WithFunc(func(v uint32) { fmt.Println(v) }).
//		Export("log").
//		Instantiate(ctx)
//
//	mod, err := r.Instantiate(ctx, binary)
//	results, err := mod.ExportedFunction("add").Call(ctx, 1, 2)
//
// The engine implements WebAssembly 3.0 -- multiple memories, memories and
// tables with i64 addresses, exceptions, tail calls, typed references and
// garbage-collected structs and arrays, SIMD and relaxed SIMD -- and the
// threads proposal's shared memories.
//
// # Values
//
// Function.Call passes and returns numbers as uint64s, as the api package's
// ValueType describes, and a v128 as two of them. Function.CallValues passes
// and returns api.Values, which carry references of every type: functions,
// host values, and the structs, arrays and exceptions wasm makes, which the
// host holds as opaque api.References.
//
// # Errors
//
// Each kind of failure has an error type of its own, whose message says what
// went wrong:
//
//   - a module that does not decode or validate is a *CompileError:
//     "Compiling function #0 failed: i32.add[1] expected type i32, found
//     i64.const of type i64 @+37";
//   - an import that does not resolve or match is a *LinkError:
//     `Import #0 "env" "f": imported function does not match the expected
//     type`;
//   - a trap is a *RuntimeError: "divide by zero", "unreachable", "Maximum
//     call stack size exceeded".
//
// A RuntimeError's Stack method gives the wasm call stack, a line for each
// frame:
//
//	at add (wasm://wasm/8c1f3e2a:wasm-function[1]:0x3a)
//
// # Memory
//
// A Memory gives the host the bytes of a module's memory. Its methods of
// uint32 offsets reach the first 4 GiB; Size64, Grow64, Read64 and Write64
// reach every byte of a memory with i64 addresses, which may be larger.
//
// # Concurrency
//
// A Runtime, its CompiledModules and the compiled code they share may be
// used from any number of goroutines at once: compiling, instantiating,
// looking up and closing modules are synchronized.
//
// The functions of an instantiated module may be called from any
// goroutine, but two calls of one module's functions must not overlap
// unless the program synchronizes what they share: the module's memories,
// globals and tables, and whatever its host functions keep. Calls of
// different modules' functions may run at once, as may calls of one
// compiled module's separate instances. A shared memory may be used from
// several goroutines at once, as the threads proposal intends.
//
// # Ending calls
//
// The engine checks a call's context at every loop back edge and call,
// every few thousand of them, so a call whose context is canceled or passes
// its deadline ends soon after. RuntimeConfig.WithCloseOnContextDone says
// how: with it, the call returns a *sys.ExitError and its module is closed;
// without it, the call returns the context's error.
package wasm
