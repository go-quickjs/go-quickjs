# WebAssembly

go-quickjs runs WebAssembly with a wasm engine of its own, in pure Go
(`wasm/internal/engine`). The JavaScript engine does not depend on it: a host
gives a runtime the `WebAssembly` namespace by installing it, from
`wasm/wasmjs`:

```go
rt := quickjs.New()
if err := wasmjs.Install(rt); err != nil {
	return err
}
```

The global is made the first time a script reads it, so a runtime that never
uses WebAssembly pays nothing for it. `qjs` installs it, as Node has it; the
standard library does not. `wasmjs.WithoutJSPI()` leaves out the JS Promise
Integration.

## What it has

The WebAssembly JavaScript interface, with the error types and messages of
V8 (Node 26), word for word:

- `validate`, `compile`, `instantiate`, and `compileStreaming` and
  `instantiateStreaming` over the runtime's `Response` (the standard
  library's `Network` installs it), with Node's checks of the response;
- `Module` (`imports`, `exports`, `customSections`), `Instance`, `Memory`
  (shared memories as `SharedArrayBuffer`, memory64 sizes as BigInts,
  `toResizableBuffer` and `toFixedLengthBuffer`), `Table` (table64),
  `Global`, `Tag`, `Exception`, `JSTag`, and `CompileError`, `LinkError` and
  `RuntimeError`;
- the conversion of every value type: i64 as BigInt; numbers that fit an
  i31 as i31refs; externref and anyref holding any value; GC structs and
  arrays as opaque objects wasm gets back as themselves; v128 and exnref
  refused;
- exceptions both ways: a JavaScript exception crossing wasm is caught by
  `catch_all` and by a `catch` of `WebAssembly.JSTag`, and comes out the
  same value; a wasm exception in JavaScript is a `WebAssembly.Exception`.
  A trap is a `RuntimeError`, the stack's exhaustion a `RangeError`, and
  neither is caught by wasm, even through JavaScript;
- the JS String Builtins (`builtins: ["js-string"]`) and
  `importedStringConstants`;
- the JS Promise Integration, `WebAssembly.Suspending` and
  `WebAssembly.promising`. A promising call's wasm runs on a goroutine of its
  own; script runs only on the runtime's goroutine, which serves the wasm
  goroutine's imports and suspensions, so that one of the two runs at a time.

A runtime made `WithNodeQuirks` validates modules against V8's limits; in
standards mode they are the interface's. The two differ for memory64, whose
declared size may be up to 2^37-1 pages by the interface and 262,144 by V8,
and for the count of memories, at most 100 by the interface and 100,000 by
V8.

## Tests

`TestWasmSpecJSAPI` runs the specification's JavaScript API tests
(`test/js-api` of a checkout of github.com/WebAssembly/spec, named by
`WASM_SPEC_DIR`) with a small stand-in for WPT's testharness:

```sh
WASM_SPEC_DIR=/path/to/wasm-spec go test ./wasm/wasmjs -run TestWasmSpecJSAPI -v
```

`TestCompareNode` runs the scripts of `wasm/wasmjs/testdata/node` in Node
and in go-quickjs and compares, line by line, what each operation returns or
throws:

```sh
QUICKJS_COMPARE_NODE_WASM=1 WASM_SPEC_DIR=/path/to/wasm-spec go test ./wasm/wasmjs -run TestCompareNode -v
```

## Known gaps

- `Object.getOwnPropertyDescriptor(globalThis, "WebAssembly")` is the
  accessor the global is until first read, as Intl's is.
- `toResizableBuffer` of a shared memory returns its fixed-length
  `SharedArrayBuffer`, not a growable one.
- A memory that wasm grows by zero pages keeps its buffer, which the
  interface would detach and make anew.
- A trap's `stack` has the JavaScript frames, not wasm's.
- `JSON.stringify` writes a GC object as an empty object, where V8 writes
  `null`.
- A promising call suspended on a promise that never settles keeps its
  goroutine until nothing holds the promise's reactions.
- Type reflection (`WebAssembly.Function`, the `type()` methods), which Node
  does not ship either.

## Follow-ups

- ESM integration: importing a `.wasm` file as a module, and its source
  phase (`import source m from "./m.wasm"`).
- `postMessage` and structured clone of a `Module`, and of a shared
  `Memory`, between workers.
