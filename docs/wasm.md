# WebAssembly

go-quickjs has a WebAssembly engine of its own, in pure Go: a decoder and
validator, a register interpreter, and an optional JIT for amd64 and
arm64: a baseline tier and an optimizing one above it. It implements WebAssembly 3.0 and the threads proposal, and it
reports invalid modules, failed links and traps with V8's messages, word for
word.

It is reached two ways, and both are opt-in:

- from Go, through the `wasm` package, whose API is shaped like wazero's;
- from JavaScript, through the `WebAssembly` namespace, which a host installs
  in a runtime with `wasm/wasmjs`, built only with `-tags quickjs_wasm`, as
  the JIT is with `quickjs_jit`. `qjs` built so installs it, as Node has it.

The JavaScript engine does not import the wasm engine, and neither does the
standard library: a program that imports no `wasm` package links none of it
(`TestEnginesApart`).

| Package | |
|---|---|
| `wasm` | the runtime: compiling, instantiating and linking modules, host modules, calls, errors |
| `wasm/api` | the types modules, functions, memories, tables and globals are handed out as, and the values' encodings |
| `wasm/sys` | `ExitError` and the clock function types |
| `wasm/wasi` | WASI preview 1, `wasi_snapshot_preview1` |
| `wasm/wat` | the text format compiled to the binary format |
| `wasm/wasmjs` | the `WebAssembly` namespace for a `quickjs.Runtime` |

Inside, `wasm/internal/decoder` decodes and validates (a port of V8 14.6's
module and function-body decoders), `module` is the module model and its type
canonicalizer, `opcode` the opcodes, `engine` the interpreter, memories,
tables and instantiation, `jit` the baseline compiler, `text` and `wast` the
text and script formats, `spectest` the spec suite's runner, and `sysctx` and
`sysfs` what a module is given of the system.

## Features

Everything below is on in every runtime; `RuntimeConfig.WithCoreFeatures`
records a set but turns nothing off.

| Proposal | Interpreter | JIT |
|---|---|---|
| WebAssembly 1.0, mutable globals | yes | yes |
| Sign extension, non-trapping float-to-int, multi-value, bulk memory | yes | yes; `memory.init`, `data.drop`, `table.init`, `table.copy` and `elem.drop` in Go |
| Reference types | yes | yes; `table.grow` in Go |
| Fixed-width SIMD | yes | yes; on amd64 with SSSE3, SSE4.1 and SSE4.2 |
| Tail calls | yes | yes |
| Extended constant expressions | yes | -- |
| Multiple memories | yes | yes |
| Memory64 and table64 | yes | refused for loads and stores of an i64 memory |
| Exception handling (`exnref`), and the legacy `try` | yes | yes; throwing and catching through Go |
| Typed function references | yes | yes |
| Garbage collection: structs, arrays, i31, casts | yes | yes; what allocates, and `array.fill`, `array.copy` and `array.init_*`, in Go; refused for a v128 field or element read or written |
| Relaxed SIMD | yes, with the host architecture's choice for each operator | yes, with the interpreter's choices; a fused `relaxed_madd` and `relaxed_nmadd` in Go |
| Threads: shared memories, atomics, `wait` and `notify` | yes | refused for the atomics |

A refused function stays in the interpreter; the rest of its module may run
natively. The relaxed SIMD operators give the result the host's vector
instructions give -- `pshufb`, `cvttps2dq`, `blendvps` and the rest on amd64
and 386, NEON's on arm64 and every other architecture -- so that the
interpreter, the JIT and Node agree on one machine (`TestRelaxedMatchesNode`).

The proposals Node 26 leaves off -- stringref, shared-everything threads,
custom descriptors, stack switching, half-precision floats, acquire-release
atomics -- are rejected when a module is validated, with V8's message naming
the flag that would enable each.

The `WebAssembly` namespace adds the JavaScript interface's own proposals: the
JS Promise Integration and the JS String Builtins.

## The Go API

```go
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
	return err
}

mod, err := r.Instantiate(ctx, binary) // the bytes of a .wasm file
if err != nil {
	return err
}
results, err := mod.ExportedFunction("add").Call(ctx, api.EncodeI32(40), api.EncodeI32(2))
```

A `Runtime` compiles modules, instantiates them under names, and links a
module's imports against the modules it holds, host modules included.
`Function.Call` passes and returns numbers as `uint64`s, a v128 as two;
`Function.CallValues` passes `api.Value`s, which carry references of every
type -- functions, host values as externrefs, and the structs, arrays and
exceptions wasm makes, as opaque `api.Reference`s. `api.Memory` reads and
writes a memory's bytes; `Size64`, `Grow64`, `Read64` and `Write64` reach a
memory64's past 4 GiB.

`Memory.Read` returns a view of the bytes, not a copy, and on 64-bit hosts
those bytes are the OS's, reserved outside the Go heap, so a view does not
keep them alive. A module holds the memories it defines and imports while it
is open: their bytes stay, whatever the collector does, until the module and
every other module holding them are closed. After that they go back to the
OS once nothing reaches the memory -- the `api.Memory`, a running call, a
reference to the module's functions -- and a view used then ends the
process with a fault that `recover` cannot catch. So copy what must outlive
`Close`, or keep the `api.Memory` reachable while its views are used. A
runtime dropped without `Close` keeps its modules' memories until the
process exits: close runtimes. The JavaScript namespace's `ArrayBuffer`s
and `SharedArrayBuffer`s over a memory keep it alive themselves. `wasm/wat.Compile` turns the text format into a binary,
which is how the package's examples (`wasm/example_test.go`) write their
modules.

`NewRuntimeWithConfig` takes a `RuntimeConfig`:

- `NewRuntimeConfig()` and `NewRuntimeConfigInterpreter()` run everything in
  the interpreter; `NewRuntimeConfigCompiler()` compiles hot functions to
  native code where the JIT runs (see [The JIT](#the-jit)).
- `WithMemoryLimitPages` caps every memory; a module declaring more fails to
  compile.
- `WithMemoryLimit(bytes)` bounds what the runtime's modules hold together,
  and `WithJITMemoryLimit(bytes)` what their native code holds (64 MiB by
  default): see [Memory limits](#memory-limits).
- `WithMemoryCapacityFromMax` allocates a memory's maximum up front where the
  OS cannot reserve it (32-bit platforms), so that it never moves.
- `WithCompilationCache` shares compiled modules between runtimes, keyed by
  the bytes' SHA-256, the memory limits, whether the JIT is on and its
  memory limit.
- `WithCloseOnContextDone` makes a call whose context is done return a
  `*sys.ExitError` and close its module; without it, the call returns the
  context's error and the module stays open. The engine checks the context
  every 4,096 loop back edges and calls either way, so a canceled call ends
  soon after.
- `WithDebugInfoEnabled(false)` drops function names from stack traces;
  `WithCustomSections(true)` keeps custom sections for
  `CompiledModule.CustomSections`.

`ModuleConfig` says what an instance sees of the system -- arguments,
environment, streams, clocks, sleep, randomness and files -- for WASI and
other host modules that implement system interfaces. Nothing reaches the
process unless it is configured: see [WASI](#wasi).

A host function that panics fails its call, with the panic's value as the
error if it is one. A module's start function runs once its memories are
bounded by the runtime's limit.

## WASI

`wasm/wasi` implements WASI preview 1, the `wasi_snapshot_preview1` module,
for Go hosts of the `wasm` package, with wazero's API. A host instantiates it
once in a runtime; each module's `ModuleConfig` says what the module sees:

```go
r := wasm.NewRuntime(ctx)
defer r.Close(ctx)
wasi.MustInstantiate(ctx, r)

cfg := wasm.NewModuleConfig().
	WithArgs("prog", "-v").
	WithEnv("HOME", "/home/gopher").
	WithStdin(os.Stdin).WithStdout(os.Stdout).WithStderr(os.Stderr).
	WithSysWalltime().WithSysNanotime().WithSysNanosleep().
	WithRandSource(rand.Reader).
	WithFSConfig(wasm.NewFSConfig().
		WithDirMount("/srv/data", "/data").
		WithReadOnlyDirMount("/srv/assets", "/assets"))
_, err := r.InstantiateWithConfig(ctx, binary, cfg)
```

Nothing reaches the process unless it is configured: arguments and
environment are empty, stdin reads EOF and stdout and stderr discard, the
clocks are fakes that advance a millisecond each reading, sleeping returns at
once, and randomness is a fixed sequence.

A program's `proc_exit` closes its module and ends the call with a
`*sys.ExitError` of its code, which `InstantiateModule` returns, or no error
for code zero. Closing a module closes the files it opened.

### Files

`FSConfig` mounts host directories, read-write (`WithDirMount`) or read-only
(`WithReadOnlyDirMount`), and `fs.FS` trees, read-only (`WithFSMount`), each
at a guest path; `ModuleConfig.WithFS(fsys)` is an `fs.FS` mounted at `/`.
Descriptors 0 to 2 are the standard streams, and each mount follows as a
preopened directory named by its guest path ("", "." and "/" all being "/").
A directory is opened when a module is instantiated, which fails if it cannot
be.

Every mount is a sandbox, on Windows as on Unix:

- A path is relative to a directory descriptor and cleaned as a string,
  `a/../b` being `b`. An absolute path, or one leading above the directory it
  is relative to, fails with `EPERM`.
- Symbolic links are followed within the mount only: an absolute link, or
  one whose target is outside the mount, fails with `EPERM`, and a module
  cannot make an absolute one.
- A mounted directory is an `os.Root`, which keeps opens, stats, `mkdir` and
  removals in it. What `os.Root` lacks in Go 1.24 -- rename, link, symlink,
  readlink and setting times -- runs as the `*at` system calls on a parent
  directory the root opened, on Linux, macOS and the BSDs; on Windows and
  other systems it resolves the path within the mount, then acts on the host
  path.

What the host answers is mapped to the closest WASI errno. On Windows, the
package checks what POSIX would before renaming and removing, so that a
module sees the errnos it would on Unix, and lists directories with their
file IDs, so that `d_ino` matches the inode a stat reports.

### Choices

- `poll_oneoff` waits on the real-time and monotonic clocks, by relative or
  absolute timeouts, sleeping with the module's `Nanosleep` only when no
  subscription is ready, until the earliest clock or until the call's
  context is done, which ends the call: a sleeper of the host's is given
  the context, and is cut off if it does not return when it is done. A subscription to reading
  or writing a descriptor is ready at once; one of a descriptor that is not
  open is ready with `EBADF`. A call reports every subscription ready when
  it is made, a clock already due beside a ready descriptor. Go programs'
  goroutines and timers run on it.
- Rights are reported, and enforced once a module drops some with
  `fd_fdstat_set_rights`: what needs a dropped right fails with
  `ENOTCAPABLE`.
- The clocks of process and thread CPU time fail with `EINVAL`, and
  `proc_raise` with `ENOSYS`.
- There are no sockets: the `sock_*` functions fail with `EBADF` for a
  descriptor that is not open and `ENOTSOCK` for one that is.
- `fd_pwrite` on a file in append mode writes at the offset it is given.
- A read or a write takes as many iovecs as its memory holds, with no
  `IOV_MAX`, and `poll_oneoff` as many subscriptions: each is read where it
  lies in the module's memory, so that their count costs the host no memory
  of its own.
- `path_link` links a symbolic link itself, and fails with `EINVAL` if asked
  to follow it.

`wasm/wasi` tests each function's errnos, and builds a Go program for
`GOOS=wasip1` (`wasm/wasi/testdata/prog`) and runs it: arguments,
environment, streams, exit codes, files on writable and read-only mounts,
the sandbox, clocks and sleeping, `crypto/rand`, and goroutines. `-short`
skips the program, as does a toolchain that cannot build it. wasi-testsuite
runs too: see [Conformance](#conformance).

## The JavaScript namespace

```go
// go build -tags quickjs_wasm
rt := quickjs.New()
if err := wasmjs.Install(rt); err != nil {
	return err
}
```

The namespace is built only with the `quickjs_wasm` build tag: without it,
`Install` returns `wasmjs.ErrNotBuilt`, defines nothing, and nothing of the
wasm engine is linked. `Install` defines the global `WebAssembly`, which is
made the first time a script reads it, so a runtime that never uses
WebAssembly pays nothing for it; it is a data property from the start,
writable, configurable and not enumerable, as in Node -- asking for its
descriptor builds it, as `Intl`'s and `Temporal`'s do. A `qjs` built with the tag installs it in
every runtime it makes, workers' included; the root package and the
standard library do not. `wasmjs.WithoutJSPI()` leaves out the
JS Promise Integration.

The namespace is the WebAssembly JavaScript interface, with the error types
and messages of V8 (Node 26), word for word, prefixes included
(`WebAssembly.Instance(): Import #0 "m" "f": ...`):

- `validate`, `compile`, `instantiate`, and `compileStreaming` and
  `instantiateStreaming` over the runtime's `Response` (the standard
  library's `Network` installs it), with Node's checks of the response
  (see [Streaming](#streaming));
- `Module` (`imports`, `exports`, `customSections`), `Instance`, `Memory`
  (shared memories as `SharedArrayBuffer`, memory64 sizes as BigInts,
  `toResizableBuffer` and `toFixedLengthBuffer`: a shared memory's
  resizable buffer is a growable `SharedArrayBuffer`, of the memory's
  maximum as its `maxByteLength`, whose length follows every grow of the
  memory -- by wasm, by `Memory.prototype.grow` or by its own `grow`, which
  takes a whole number of pages, in any runtime or thread), `Table` (table64),
  `Global`, `Tag`, `Exception`, `JSTag`, and `CompileError`, `LinkError` and
  `RuntimeError`;
- the conversion of every value type: i64 as BigInt; numbers that fit an
  i31 as i31refs; externref and anyref holding any value; GC structs and
  arrays as opaque objects wasm gets back as themselves -- of no prototype
  and no properties, refusing every change, left out by `JSON.stringify`
  as a function is (`null` in an array) and not cloneable
  (`[object Object] could not be cloned.`), as in Node; v128 and exnref
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
  A call suspended for good ends its goroutine once nothing holds what would
  resume it, and `Close` ends every suspended call's before it returns;
- structured cloning, by `postMessage` to a worker and by `structuredClone`:
  a `Module` clones as a `Module` of the same compiled code, which the
  receiving runtime instantiates without compiling it again (its native
  code, and the JIT memory budget that code is charged to, stay the
  compiling runtime's), and a shared `Memory` as a `Memory` of the same
  memory, its buffer a `SharedArrayBuffer` of the same bytes, a grow
  through either seen by both. Anything else of the namespace -- an
  unshared `Memory`, an `Instance`, a `Table`, a `Global`, a `Tag`, an
  `Exception`, an exported function -- is a `DataCloneError` with Node's
  message, and so is any of them in a transfer list. A runtime the
  namespace is not installed in cannot deserialize a `Module` or a
  `Memory`. A shared memory's buffers clone as `SharedArrayBuffer`s of the
  same memory, its growable one as a growable one -- but under
  `WithNodeQuirks`, as in Node, that one cannot be deserialized: an
  `Error`, "Unable to deserialize cloned data.", or a worker's
  `messageerror`;
- one list of waiters for each address of a shared memory, whichever engine
  waits and whichever notifies: `Atomics.wait` and `Atomics.waitAsync` on any
  of the memory's buffers -- in any runtime, a clone's included -- and wasm's
  `memory.atomic.wait32` and `wait64` are woken, longest waiting first, by
  `Atomics.notify` and `memory.atomic.notify` alike, and a notify counts
  every waiter it wakes.

Constructors read `new.target`'s prototype before their arguments, as Node's
do. A context that ends -- `EvalContext`'s deadline, say -- stops wasm as it
stops script, in a call made directly or from a promising call.

A runtime made `WithJIT` runs the WebAssembly it compiles natively too, where
the JavaScript JIT runs: see [The JIT](#the-jit).

### Streaming

`compileStreaming` and `instantiateStreaming` read the response's body as
it arrives, chunk by chunk (`for await` over `response.body`, each chunk an
`ArrayBuffer` or a view of one, anything else the `TypeError` Node throws),
and hand each chunk to a streaming decoder (`decoder.Stream`) on a
goroutine of its own, which decodes each section once all of it has
arrived and, in the code section, validates each function body as soon as
its bytes are there. The interpreter compiles a body to register code at
its first call and the JIT at its first call too, so there is no machine
code to make ahead: what streaming overlaps with the download is the
decoding and the validation, which is all of the compile but the last
section's. A module of 3.6 MB arriving in 64 KB chunks a millisecond apart
is ready less than half a millisecond (the clock's resolution) after its
last byte, where reading the whole body and then compiling it took 20 to 23
ms more (`TestStreamingTiming`).

The result is the one-shot compile's, error for error: the decoder runs the
same checks in the same order, and wherever it finds an error -- or the
bytes it has do not settle a check -- it decodes and validates the whole
module again once the body has ended, so that the error reported is the
one a compile of the whole would report, at the same offset (a malformed
section after the code section wins over an invalid body before it, and a
function's error names it from the name section after the bodies).
`TestStreamMatchesDecode` holds it to `Decode` and `ValidateFunctions` over
the decoder's corpus, cut into chunks of 1, 3, 17 and 256 bytes and whole. A
response with a `url` gives the module's frames in stack traces that URL.

### WebAssembly modules as ES modules

`Install` also makes a WebAssembly module importable from JavaScript, as Node
26 does without a flag (the WebAssembly ESM integration):

```js
import { add, memory } from "./m.wasm";  // its instance's exports
import source mod from "./m.wasm";       // its WebAssembly.Module
const again = await import.source("./m.wasm");
```

It works through the runtime's module loading, so any embedder that installs
the namespace has it: the source a module loader returns for a name that
ends in `.wasm`, or that begins with the binary format's magic number, is
the module's bytes, and is compiled as WebAssembly -- with the JS String
builtins and `wasm:js/string-constants` as its string constants' module, as
Node compiles it. `qjs`'s loader returns a `.wasm` file's bytes.

- Each module name its imports use (but the builtins' and the constants')
  is a module it imports from, resolved against its own name, loaded,
  linked and evaluated before it as an `import * as` of it would be.
  Evaluating it instantiates it with those modules' namespaces as its
  imports object, so a JavaScript module gives it functions, numbers,
  `WebAssembly.Memory`s and the rest by its exports, with the link errors of
  `new WebAssembly.Instance` (`WebAssembly.Instance(): Import #0 "./js.mjs"
  "f": function import requires a callable`). A global it imports from
  another WebAssembly module is that module's `WebAssembly.Global`, so a
  mutable one is shared.
- Its exports are its namespace's bindings: functions, memories, tables and
  tags as the instance's exports objects are, and a global as its value when
  the module was evaluated (`undefined` for a v128), not as the
  `WebAssembly.Global`. They are in their temporal dead zone until it is
  evaluated: a cycle that reaches one sooner -- a JavaScript module reading
  one at its top level, a WebAssembly module importing from one being
  evaluated -- throws a `ReferenceError`.
- A module is compiled once, instantiated once however many modules import
  it, and its source phase is the same `WebAssembly.Module` each time. A
  source phase import loads the module alone: what its imports name is
  neither loaded nor linked. `%AbstractModuleSource%.prototype`'s
  `Symbol.toStringTag` getter names its class, `WebAssembly.Module`.
- A module that does not compile fails to load with the `CompileError` of
  `new WebAssembly.Module`, its message prefixed with the module's name
  (`/app/m.wasm: WebAssembly.Module(): expected magic word ...`); one whose
  imports name a `wasm-js:` module, or whose imports or exports use a name
  beginning with `wasm:` or `wasm-js:`, with a `LinkError` naming its URL
  (`Invalid Wasm import "wasm-js:x" in file:///app/m.wasm`), which
  `wasmjs.WithModuleURL` gives from its name -- `qjs` gives a file's
  `file:` URL -- and is its name by default.

Node prints an `ExperimentalWarning` when it first evaluates such a module;
go-quickjs prints nothing. The source phase of a JavaScript module is the
runtime's `SyntaxError`, worded its own way, where Node's loader throws
`ERR_SOURCE_PHASE_NOT_DEFINED`.

## Limits

A module is validated against one of three sets of limits, the presets of
`wasm/internal/decoder`, and a module over one is rejected with the message
that names the limit and its value:

| | `CoreLimits` | `JSAPILimits` | `V8Limits` |
|---|---|---|---|
| Used by | the `wasm` package | `WebAssembly`, in standards mode | `WebAssembly`, in a runtime made `WithNodeQuirks` |
| Types, functions, imports, exports, globals, tags | 2^27 each | 1,000,000 each | 1,000,000 each |
| Tables, data segments | 2^27 each | 100,000 each | 100,000 each |
| Memories | 2^27 | 100 | 100,000 |
| A memory's declared size, i32 addresses | 65,536 pages | 65,536 pages | 65,536 pages |
| A memory's declared size, i64 addresses | 2^48 pages | 2^37-1 pages | 262,144 pages |
| A table's declared size, i32 / i64 addresses | 2^32-1 / 2^64-1 elements | 10,000,000 elements | 10,000,000 elements |
| Table initializer entries | 2^27 | 10,000,000 | 10,000,000 |
| Struct fields; subtyping depth | 2^27; 2^27 | 10,000; 63 | 10,000; 63 |
| `br_table` targets; `array.new_fixed` length | 2^27; 2^27 | 65,520; 10,000 | 65,520; 10,000 |
| Module size | 2^32-1 bytes, what its 32-bit offsets address | 1 GiB | 1 GiB |

The bounds on a function, which also bound what validating it costs, are the
same in all three: a body of 7,654,321 bytes, 50,000 locals, 1,000 parameters
and 1,000 results.

`JSAPILimits` are the interface's. `V8Limits` differ from them in two rows,
where V8 is narrower or wider than the interface: a memory64 of at most
262,144 pages, and up to 100,000 memories. `quickjs.WithNodeQuirks` documents
both, and `wasm/wasmjs` tests both answers.

What the engine can make bounds every preset:

- A memory holds at most 65,536 pages (4 GiB) with i32 addresses and 262,144
  (16 GiB) with i64 ones on 64-bit platforms, and 32,767 pages either way on
  32-bit ones. A declared maximum above that is accepted where the preset
  allows it, and the memory grows to the engine's limit at most. The `wasm`
  package lowers `CoreLimits`'s memory sizes to these, or to
  `WithMemoryLimitPages`, so a module declaring more fails to compile.
- A table holds at most 10,000,000 elements; a larger initial size fails at
  instantiation.
- Calls nest 30,000 deep (`engine.MaxCallDepth`), and their frames take 64
  MiB at most, a function's locals and operand stack included, past either
  of which a call traps with "Maximum call stack size exceeded". A call back
  into wasm from a host function counts against what the call that made it
  has left, 16 calls deep a round trip. Under a smaller memory limit the
  frames take that limit at most (below).
- In the text format (`wasm/wat`, and a script's modules), instructions
  nest 10,000 deep -- a block, loop, if, try or try_table in another, or a
  folded instruction an operand of another, is a level -- and a script's
  commands and results as deep. Deeper text is a `wat.Error`, "instructions
  nest more than 10000 deep", rather than a parse whose recursion overflows
  the Go stack; the binary format has no such bound.
- A GC array may be as large as 2^30-1 bytes with its header.

### Memory limits

`RuntimeConfig.WithMemoryLimit(bytes)` bounds the memory a runtime's modules
hold together, and `quickjs.WithMemoryLimit` bounds a JavaScript runtime's
heap with what its WebAssembly holds in it.

- Linear memories and tables are counted exactly, as they are made and
  grown, and charged before they are allocated: one that does not fit is
  refused before any of it is allocated or reserved. A `memory.grow` or
  `table.grow` past the limit returns -1, and
  `Memory.prototype.grow` throws its RangeError, "Unable to grow instance
  memory", as when the memory cannot be had. A module whose memories do not
  fit fails to instantiate with "Out of memory: Cannot allocate Wasm memory
  for new instance", which unwraps to `wasm.ErrMemoryLimit` in Go; a
  `WebAssembly.Memory` that does not fit throws "could not allocate memory".
- Element segments are counted as their references are made at
  instantiation, 16 bytes each, and given back as each is dropped; a module
  whose segments do not fit fails to instantiate with `ErrMemoryLimit`.
- A call's frames are not counted with what the modules hold, but take no
  more than the limit, if it is under 64 MiB: a call past it traps with
  "Maximum call stack size exceeded"
- GC structs and arrays are counted as they are allocated. When the count
  would pass the limit, what is still reachable is counted again -- from the
  modules' globals, tables and segments and the running call's frames -- and
  a program that still does not fit stops with `ErrMemoryLimit`, which no
  wasm handler or JavaScript `try` catches. A program that lets go of what it
  allocates runs on. Native code makes structs, and arrays of up to 480
  bytes of elements, from pools the engine fills ahead, up to 128 objects
  and 16 KiB at a time: a fill is counted whole as it is made, and what a
  pool still holds when Go collects it is given back unless what is
  reachable was counted again since. An array is counted as its 32-byte
  header and its elements. Native code has Go fill a pool, or make an array
  past the pools', through a stub it calls without leaving (the JIT's
  README, GC objects); an allocation that would trap, or that the budget
  holds only once what is reachable is counted again, leaves native code
  for the engine to make it.
- What a closed module held is given back once Go collects it.
- Under `quickjs.WithMemoryLimit`, the runtime's measurement of its heap asks
  each WebAssembly object it reaches what it holds, and a memory's buffer is
  counted by the memory's size, not the address space reserved for it. A
  `WebAssembly.Table` that does not fit stops the script, as an allocation of
  a size the script chose does. A shared memory another runtime can grow --
  one cloned to it, or one whose growable buffer was made, which can be --
  is no longer charged as it grows, since its grows run on that runtime's
  goroutine too: each runtime's measurement counts it at its size.

The count of GC objects is an estimate: the Go API's may pass the limit by a
sixteenth of it before it is found, and what only a call running on another
goroutine holds is counted at that call's own next count. A count never
reads a module another goroutine may be changing: while another goroutine
runs a call, instantiates a module or changes a global or a table through
the API, a count walks only the modules of its own goroutine's calls, and
counts each other module by what it held when a count last walked it.

The JIT's memory -- the modules' native code and what describes it -- is
never counted by those limits, so a program fails with `ErrMemoryLimit` the
same with the JIT on and off. `RuntimeConfig.WithJITMemoryLimit(bytes)`
bounds it for the modules of a runtime of `NewRuntimeConfigCompiler`:
`wasm.DefaultJITMemoryLimit`, 64 MiB, by default, which holds every function
of a 3,028-function Go program compiled by both tiers (23 MB by the
baseline, 34 MB by the optimizing tier), while a run of one of the
benchmarks' Go programs holds 3.9 to 8.2 MB. Runtimes sharing a
compilation cache share the limit too. A `quickjs.Runtime` made `WithJIT`
bounds its WebAssembly's code by its `WithJITMemoryLimit` -- 8 MB by
default, or an eighth of `WithMemoryLimit` -- in a budget of that size of
its own, apart from its JavaScript code's, so that the two together hold at
most twice it.

When a compile does not fit, the JIT first drops code no call is running:
code the optimizing tier replaced, and the code of functions not called for
100 ms or more, whose next call compiles them again. Where that is not
enough, the function runs in the interpreter, and is compiled once there is
room, from the JIT clock's next 50 ms epoch; a tier-up so refused is tried again after
as many calls again. The limit bounds what a compile works in as it runs
too: a tier does not compile a function whose working set, estimated at 64
bytes a byte of body for the baseline and 128 for the optimizing tier,
would pass the limit -- under the JavaScript runtime's default 8 MB, the
optimizing tier compiles bodies of up to about 64 KB, the baseline up to
about 128 KB -- and the optimizing tier's liveness sets, which grow with
the square of a function of many blocks, get what the code held leaves of
the limit, the compile refused where they need more. So what a compile
works in is of the order of the limit, beside the code the budget holds.
`engine.ReadJITStats` counts the refusals as `budget` and the functions
dropped. Results are the same whatever the limit, but a
limit below what a program's hot code needs makes it slower, down to the
interpreter's speed: on amd64, with three runs each, under 4 MiB sort's
steady runs take 118 ms against 108 unbounded, gzip's 95 against 21 and
json's (8.2 MB unbounded) about 500 against 42; under 2 MiB sha256's 156
against 48. Native code is dropped only when no frame of any goroutine is
in it, and a goroutine never enters code dropped since it found it.

## Error messages

The decoder is a port of V8 14.6's, the V8 Node 26 ships, and keeps its order
of checks: which error a module reports first, its text and its offset are
V8's. The rest of the engine words its errors as V8 does too:

- A module that does not decode or validate is a `*wasm.CompileError`,
  `<message> @+<offset>`: "Compiling function #0 failed: i32.add[1] expected
  type i32, found i64.const of type i64 @+37". A function is named from the
  name section, cut at 50 bytes.
- An import that does not resolve or match is a `*wasm.LinkError`:
  `Import #0 "env" "f": imported function does not match the expected type`.
  Memories are linked before the other imports, so the first error is the
  one V8 reports.
- A trap is a `*wasm.RuntimeError` with V8's message -- "divide by zero",
  "memory access out of bounds", "function signature mismatch", "operation
  does not support unaligned accesses", "requested new array is too large".
  Its `Stack` prints ten frames as V8 does,
  `    at add (wasm://wasm/8c1f3e2a:wasm-function[1]:0x3a)`, the hash being
  V8's of the module's bytes and the name the module's and the function's;
  the innermost frame is at the instruction that trapped, in the interpreter
  and in native code alike, and each frame below it at its call. A stack
  overflow's innermost frame is the function that could not be entered, at
  the start of its body.
- In JavaScript the same texts come with the interface's prefixes, from
  `CompileError`, `LinkError` and `RuntimeError`. An error's `stack` shows
  the wasm frames of each exported function's call running, in the place of
  the function's own frame -- a trap's, and an error an import threw while
  wasm called it -- each frame at the call it is making, or a trap's
  innermost at the instruction that trapped, and
  `Error.prepareStackTrace`'s CallSites answer for them as V8's do: the
  function's index from `getFunction`, line 1 and the offset plus one as the
  column, the module's URL as the file.

The match is tested against Node, not assumed. With
`QUICKJS_COMPARE_NODE_WASM=1` and Node 26 as `node` on `PATH`:

- `wasm/internal/decoder`'s `TestCompareNode` validates a corpus here and in
  Node and compares each verdict and message: hand-written modules for every
  message the module decoder has, seed modules using most opcodes, and
  random mutations of them -- 24,437 modules by default, none differing.
  `QUICKJS_WASM_MUTATIONS` and `QUICKJS_WASM_SEED` set the mutations per
  seed and the random seed: 15,000 with seed 2 is 120,437 modules, and
  25,000 with seed 3 is 200,437, and neither differs either.
  `TestHandCases` checks the hand-written cases' verdicts without Node, from
  `testdata/hand.golden`.
- `wasm/internal/wast`'s `TestNodeAcceptsCompiledModules` has Node compile
  each of the spec scripts' 5,321 text modules: it accepts the 2,492 valid
  ones and rejects the 2,825 invalid ones; the 4 others are over V8's
  limits, which the test lists.
- `wasm/wasmjs`'s `TestCompareNode` runs the scripts of
  `wasm/wasmjs/testdata/node` in both and compares, line by line, what each
  operation returns or throws: 1,717 lines over 11 scripts, all equal but 26
  that the test lists with why, each a message of the JavaScript runtime's or
  a gap listed under [Known gaps](#known-gaps).
- `wasm/internal/engine`'s `TestRelaxedMatchesNode` compares the relaxed SIMD
  operators with Node's Liftoff and TurboFan code, 20,000 calls each.

## The JIT

The interpreter compiles a function body, on its first call, to register
code: fixed-width instructions over the frame's slots, run by native
threaded handlers on amd64 and arm64 and a loop over a table of Go
handlers elsewhere (`wasm/internal/engine/README.md`). The baseline JIT
(`wasm/internal/jit`)
compiles a function to native code in one pass over its validated body, with
a register cache of the operand stack over the interpreter's own frames, so a
call crosses between the tiers in either direction. Where the JIT runs, it
compiles a function at its first call, its frame sized by validation's
bound, and the interpreter's compile of the body is never made unless a
call runs there. An optimizing tier (`wasm/internal/jit/opt`) recompiles a
function whose baseline code was called 30,000 times, or whose loops ran
long, from SSA with a register allocator, in the baseline code's place;
`WASM_JIT_OPT=off` turns it off. Its compiles run on background workers
shared by the process, at most a quarter of GOMAXPROCS, the program running
the baseline code until the optimized code is placed -- on pages no running
code is in, so that nothing waits -- and published; with GOMAXPROCS 1 they
are made on the spot, the worker otherwise waiting for the program's
goroutine to be preempted. It compiles references -- locals, globals,
tables' elements, the null tests and branches -- calls through tables and
references, tail calls, the accesses of every memory, shared ones and
those of i64 addresses included, the atomics, the bulk and table
instructions -- those the baseline leaves to Go, and table.fill and the
copies and fills of memories other than an unshared i32 memory 0, through
Go too -- v128s and every SIMD and relaxed SIMD instruction, each the
baseline's instruction sequence over registers the allocator gives (a
fused `relaxed_madd` and `relaxed_nmadd` in Go, as in the baseline), and
the GC instructions and the exceptions, as the baseline compiles them.
It takes 15 to 47% off the Go-built programs' time from their second run
on (interp 22.8 ms to 12.0, json 35.8 to 28.9), and makes their first runs
as fast as the baseline's or faster (sha256 81 ms to 64, sort 147 to 129),
but interp's, 36 to 38 ms for the baseline code's counting of calls; see
[Benchmarks](#benchmarks). Their designs are in
[the JIT's README](../wasm/internal/jit/README.md) and
[the optimizing tier's](../wasm/internal/jit/opt/README.md).

It is built only with `-tags quickjs_jit`, on amd64 (Linux and Windows) and
arm64 (macOS and Linux); without the tag, on 386 and on other platforms,
nothing of it is linked and the interpreter runs everything. Native code is
opt-in:

```go
// go build -tags quickjs_jit
r := wasm.NewRuntimeWithConfig(ctx, wasm.NewRuntimeConfigCompiler())
```

A `quickjs.Runtime` made `WithJIT` (`qjs --jit`) compiles the WebAssembly its
scripts load as well, on the platforms where the JavaScript JIT runs:
linux/amd64, windows/amd64, darwin/arm64 and linux/arm64. A runtime made `WithDebugger` runs wasm in the
interpreter.

A function is compiled to native code at its first call
(`engine.DefaultJITThreshold`, 0), without the interpreter's compile: the
interpreter compiles the body only of a function the JIT refuses, or of
one a call runs in the interpreter for a test (`WASM_JIT=eager`, `osr`). A
Code given a threshold above 0 (`EnableJIT(n)`, `WASM_JIT=N`) runs a
function in the interpreter for its first n calls, and compiles it then,
or once a call of it has run 1,000 back edges of its loops in the
interpreter (`engine.DefaultOSRThreshold`): the call then goes on in native
code from the loop's head, on-stack replacement. Against compiling after 10
calls, with the interpreter's code built for every function called,
compiling at the first call made the Go-built programs' first runs as fast
or up to 16% faster (json 100 ms to 84), their second runs up to 13%
faster, and allocated a twelfth of the objects in the first run, as
measured at its commit (de6821e7), before the optimizing tier. Each loop
has an entry of its own in the function's code, which loads what resuming
after an exit to Go does; both tiers keep everything in the frame's slots at
a loop's head, so nothing is translated. A function the JIT refuses is tried
once, and its loops stop counting.
Native code is shared by the instances of a compiled module and lives in an
arena of the module's own, released when the module is collected; what it
holds is bounded by a JIT memory limit (see [Memory limits](#memory-limits)).
Loops, calls and tail calls count toward the same context poll the
interpreter's do.

Native code computes what the interpreter does, bit for bit where the
specification fixes the bits: NaN quieting, the minimum and maximum of zeros,
trapping and saturating truncations, one rounding for unsigned 64-bit
conversions. Calls between native functions of an instance are native,
through a table or a reference too where the callee is the instance's own
function of the call's type exactly, and so are tail calls; calls of
imports, of host functions, other calls through tables and references,
`memory.grow`, `table.grow`, `memory.init`, `table.init`, `table.copy`,
`data.drop`, `elem.drop`, and `memory.atomic.wait` and `notify` return to Go
and resume; `memory.size`, `memory.copy`, `memory.fill`, `table.size` and
`table.fill` are native, the copies and fills checking both ends before
writing as the interpreter does. References are
native -- locals, globals, tables' elements, the null tests and branches,
`ref.func`, `ref.eq`, `select` -- and so are the GC instructions that read
and write structs' fields and arrays' elements, i31s, tests and casts; what
allocates runs in Go, charged to the instance's budget. References are
kept only where the collector sees
them, in the frame's reference slots, never in a register across a return
to Go; a store of a reference while the collector marks goes through the
Go runtime's write barrier, as compiled Go's does, without leaving native
code. `throw`, `throw_ref` and `rethrow` return to Go, which throws, and
an exception native code's `try_table` or legacy `try` catches, from Go or
from native code, resumes native code at the catch's label. Loads and
stores of every memory, i64 ones included, and the
atomic loads, stores and read-modify-writes are native; an atomic
read-modify-write is the processor's own locked or exclusive instruction.

A v128 lives in an XMM or a V register, or in two of the frame's slots, as
the interpreter keeps it. Native code compiles v128.const, the SIMD loads
and stores of every memory, splat, extract_lane, replace_lane, the
bitwise instructions, every integer and float lane instruction, shuffle
and swizzle included, the conversions and the relaxed instructions, with
the interpreter's choices for the host. relaxed_madd and relaxed_nmadd
are FMLA on arm64; on amd64 with FMA3, where the interpreter fuses them,
they return to Go, which runs the interpreter's own code for them, so that
the result is the interpreter's bit for bit, and resumes.

A function is refused whole, and stays in the interpreter, when it has:

| Reason | What |
|---|---|
| `simd` | `struct.get`, `struct.set`, `array.get` or `array.set` of a v128 field or element |
| `atomics` | the struct and array atomic instructions |
| `cpu` | on amd64, `popcnt` without POPCNT, rounding without SSE4.1, a v128 or a SIMD instruction without SSSE3, SSE4.1 and SSE4.2 |
| `frame`, `size` | an operand stack deeper than the interpreter's frame, or code past the JIT's size limit |

With every function compiled at its first call, the spec suite compiles 6,840
functions and refuses 2, for reading v128 fields.

On `BenchmarkFib25` and `BenchmarkLoop1M` (`wasm/internal/engine`), the
interpreter takes 7.2 ms and 9.7 ms and native code 0.40 ms and 0.355 ms on
amd64 (commit 82c30728); on GitHub's arm64 runners native code runs them 10
to 15 times as fast as the interpreter (commit 35a8ead9). For whole programs
see [Benchmarks](#benchmarks).

## Concurrency

A `wasm.Runtime`, its compiled modules and their code may be used from any
number of goroutines: compiling, instantiating, looking up and closing modules
are synchronized. An instance's functions may be called from any goroutine,
but two calls of one instance must not overlap unless the program
synchronizes what they share -- its memories, globals and tables, and what its
host functions keep. Separate instances, of one compiled module or of
several, run at once. A shared memory is used from several goroutines at
once, as the threads proposal intends: its bytes never move, its atomics are
atomic, and `memory.atomic.wait` is woken by a `notify` from another
goroutine. The waiters of every shared memory are in one table of the
process's (`internal/atomicwait`), which JavaScript's `Atomics` wait in too.

Native code is shared by every instance and goroutine running a compiled
module. Placing new code takes a write lock and running code a read lock, so
no goroutine runs in pages being written.

A `quickjs.Runtime` is not safe for concurrent use, and its `WebAssembly`
namespace runs wasm on the runtime's goroutine, but for a promising call's,
whose goroutine and the runtime's take turns.

## Conformance

| Suite | Result | Pinned at |
|---|---|---|
| The spec's core and legacy-exception tests, and the threads proposal's | 65,824 of 65,824 assertions, interpreted and with every function compiled, on amd64 and arm64; 65,824 interpreted on 386 | WebAssembly/spec [`f3268fa9`](https://github.com/WebAssembly/spec/commit/f3268fa9dcb7d3a60d09546e743e7281e75cddb0), WebAssembly/threads [`cc535ada`](https://github.com/WebAssembly/threads/commit/cc535ada1aa21cfaa3cabf3ac73b89acef78a0a0) |
| The spec scripts, parsed | 276 of 276 scripts; 5,321 text modules compiled and 1,240 malformed ones rejected | the same |
| The JavaScript API tests (`test/js-api`) | 1,205 of 1,223 subtests; the 18 others fail in Node 26 too, with the same messages | WebAssembly/spec `f3268fa9` |
| Validation against Node 26 (V8 14.6) | no difference over 345,311 modules, in three corpora | -- |
| wasi-testsuite, `wasm32-wasip1` | 72 of 72 on Linux; 67 on Windows, the 5 that make symbolic links skipped without the privilege to | `prod/testsuite-base` |

CI's `wasm-spec` job runs the spec scripts on Linux, Windows and macOS (arm64),
on GitHub's linux/arm64 runner, and for 386; where the JIT runs, it runs them
again with every function compiled at its first call, with the engine's tests,
and runs `TestJITDifferential`. It runs the `WebAssembly` namespace's tests
(`-tags quickjs_wasm`) with the JavaScript API scripts on 64-bit platforms,
under the JIT too, and without them on 386, whose address space does not
hold the modules of a million types, functions and imports that
`limits.any.js` builds.

The suites are not vendored. Check them out at the pinned commits:

```sh
git clone https://github.com/WebAssembly/spec /tmp/wasm-spec
git -C /tmp/wasm-spec checkout f3268fa9dcb7d3a60d09546e743e7281e75cddb0
git clone https://github.com/WebAssembly/threads /tmp/wasm-threads
git -C /tmp/wasm-threads checkout cc535ada1aa21cfaa3cabf3ac73b89acef78a0a0
git clone --depth 1 -b prod/testsuite-base https://github.com/WebAssembly/wasi-testsuite /tmp/wasi-testsuite
```

The environment names them:

| Variable | |
|---|---|
| `WASM_SPEC_DIR` | a WebAssembly/spec checkout: `test/core`, `test/legacy` and `test/js-api` |
| `WASM_THREADS_DIR` | a WebAssembly/threads checkout, for `test/core/threads` |
| `WASM_SPEC_ONLY` | runs only the scripts whose path contains it |
| `WASM_NONATIVE` | `1` runs register code by the Go table's loop rather than the native handlers (amd64, arm64) |
| `WASM_REG_VERIFY` | `1` verifies every body of register code as it is compiled |
| `WASM_JIT` | in a JIT build, compiles every function: `eager` at its first call, a call with loops entering native code at its first back edge; `osr` so for every call Go or the interpreter makes; `on` or `1` at its first call, the interpreter compiling no body it does not run; a number after that many calls in the interpreter |
| `WASM_JIT_OSR` | the back edges a call runs before its loop enters native code, or `off` |
| `WASM_JIT_OPT` | the calls of a function's baseline code before the optimizing tier compiles it: a number, `on` for the default, `eager` at its first compile, or `off` |
| `WASM_JIT_OPT_BACKGROUND` | `on` compiles the optimizing tier's tier-ups on the process's workers even at GOMAXPROCS 1, `off` on the goroutine that ran the count out; by default the workers where GOMAXPROCS is above 1, and never under `WASM_JIT=eager`, `osr` or `opt` |
| `QUICKJS_COMPARE_NODE_WASM=1` | runs the comparisons with the `node` on `PATH` |
| `WASI_TESTSUITE_DIR` | wasi-testsuite's `tests` directory |

```sh
# The spec scripts, interpreted, and the script parser over every module
WASM_SPEC_DIR=/tmp/wasm-spec WASM_THREADS_DIR=/tmp/wasm-threads \
  go test ./wasm/internal/spectest ./wasm/internal/wast -count=1 -v

# The same on 386, where an int is 32 bits and an unshared memory is a Go slice
GOARCH=386 WASM_SPEC_DIR=/tmp/wasm-spec WASM_THREADS_DIR=/tmp/wasm-threads \
  go test ./wasm/internal/spectest -count=1 -v

# Every function compiled at its first call, and the engine's tests so;
# then every call with loops entering native code from its first back edge
WASM_JIT=eager WASM_SPEC_DIR=/tmp/wasm-spec WASM_THREADS_DIR=/tmp/wasm-threads \
  go test -tags quickjs_jit ./wasm/internal/spectest ./wasm/internal/engine -count=1 -v
WASM_JIT=osr WASM_SPEC_DIR=/tmp/wasm-spec WASM_THREADS_DIR=/tmp/wasm-threads \
  go test -tags quickjs_jit ./wasm/internal/spectest ./wasm/internal/engine -count=1 -v

# Every spec module's exports called interpreted and native, and compared
WASM_SPEC_DIR=/tmp/wasm-spec WASM_THREADS_DIR=/tmp/wasm-threads go test -tags quickjs_jit ./wasm/internal/spectest -run TestJITDifferential -count=1 -v

# The JavaScript API tests
WASM_SPEC_DIR=/tmp/wasm-spec go test -tags quickjs_wasm ./wasm/wasmjs -run TestWasmSpecJSAPI -count=1 -v

# The comparisons with Node
QUICKJS_COMPARE_NODE_WASM=1 WASM_SPEC_DIR=/tmp/wasm-spec WASM_THREADS_DIR=/tmp/wasm-threads \
  go test -tags quickjs_wasm ./wasm/internal/decoder ./wasm/internal/wast ./wasm/internal/engine ./wasm/wasmjs \
  -run 'TestCompareNode|TestNodeAcceptsCompiledModules|TestRelaxedMatchesNode' -count=1 -v

# wasi-testsuite
WASI_TESTSUITE_DIR=/tmp/wasi-testsuite/tests go test ./wasm/wasi -run TestWASITestsuite -count=1 -v
```

`TestSpec` logs `spec: N of M assertions passed`, and under `WASM_JIT` how
many functions were compiled and refused, by reason. `TestJITDifferential`
runs each spec module that links with the spectest module alone -- and each
threads module, with `WASM_THREADS_DIR` -- twice, interpreted and with every
function compiled at its first call, calls every export whose signature is
numbers with the scripts' constants, each type's edges and random values, and
compares the results, the traps, and every memory and global after each
call: 2,106 modules, 4,963 functions and 59,421 calls on amd64, with no
difference.

## Benchmarks

`internal/cmd/wasmbench` runs the same workloads on the engine's interpreter
and its JIT's tiers, on wazero's interpreter and compiler, and on Node, and
reports for each the compile and instantiate times, the first run, the best
and the median run, the CPU time, the Go heap's allocations per run and the
peak RSS. Run it from the checkout's root:

```sh
go run ./internal/cmd/wasmbench -n 3                              # interp
go run -tags quickjs_jit ./internal/cmd/wasmbench -suite micro    # interp and jit
go run -tags quickjs_jit ./internal/cmd/wasmbench -engine jit,wazero-compiler,node -suite fib,sort
go run -tags quickjs_jit ./internal/cmd/wasmbench -engine jit-base,jit-tiered,jit-opt -suite programs
go run -tags quickjs_jit ./internal/cmd/wasmbench -matrix -n 5 > matrix.md
```

Flags:

- `-engine`: any of `interp`; `jit`, the JIT as a runtime of
  `NewRuntimeConfigCompiler` runs it -- the baseline at a function's first
  call, the optimizing tier at 30,000 calls, in the background -- which
  needs a build with `-tags quickjs_jit` on amd64 Linux or Windows or arm64
  macOS or Linux; `jit-base`, the baseline alone (`WASM_JIT_OPT=off`);
  `jit-tiered`, the default named (`WASM_JIT_OPT=on`); `jit-opt`, every
  function the optimizing tier accepts optimized at its first call
  (`WASM_JIT_OPT=eager`); `wazero-interp`, `wazero-compiler`, `node` (its
  default tiering), `node-liftoff` (`--liftoff --no-wasm-tier-up`) and
  `node-turbofan` (`--no-liftoff`). The default is `interp`, and `interp`
  and `jit` in a JIT build; `-matrix` takes `interp`, `jit` and the wazero
  and Node engines unless `-engine` names others. The worker inherits the
  environment, so `GOMAXPROCS` and the `WASM_JIT` variables reach the Go
  engines.
- `-suite`: comma-separated prefixes of workload names (`fib`) or of
  suite/name (`micro`, `programs`).
- `-n`: fresh processes per workload and engine, the engines taking turns.
  Each process compiles, instantiates, makes `-warmup` runs (default 1) and
  `-reps` timed runs (default 1); the first run is reported apart, and the
  best and the median are over every timed run.
- `-json` prints every figure and run as JSON; `-matrix` prints a Markdown
  table for each suite of the median runs, the engines side by side, with
  go-quickjs's ratios to Node and to wazero's compiler and their geometric
  mean, and a table of compile times.
- `-cpuprofile PREFIX` has each Go worker write a CPU profile of its timed
  runs, the warmup left out, to `PREFIX.<suite-name>.<engine>.pprof`.
- `-node` names the node command, `-wazero` a built wazero runner,
  `-cache` the directory for built modules, `-timeout` a worker's limit.

The workloads are in `internal/cmd/wasmbench/testdata`, and each run's
result is checked, so that an engine computing the wrong thing fails rather
than wins:

- `micro`, in the text format: `fib` (recursive calls), `loop` (i32 on
  locals), `i64` (splitmix64 with a remainder and a rotate), `mandelbrot`
  and `nbody` (f64), `bytes` (byte and word loads and stores), `bulk`
  (`memory.fill`, `memory.copy` of 1 MiB and of 64 bytes), `indirect`
  (`call_indirect`), `brtable` (unpredictable `br_table`), `globals`,
  `gc` (binary trees of structs, arrays of i32), `arrays` (short arrays
  of f64s, i32s, i16s and references, as a GC language's compiler makes
  them, and a list's backing array doubled by copies) and `simd`
  (i32x4). A
  kernel is a function called many times in a run.
- `programs`, a Go program built for `GOOS=wasip1` when first needed and
  cached in the user cache directory by its source's hash and the Go
  version, run through WASI: `sha256` of 8 MiB, `sort` (`sort.Ints` of a
  million ints), `json` (`encoding/json` round trips), `regexp`, `gzip`
  (`compress/gzip` of 512 KiB and back) and `interp` (a bytecode
  interpreter's loop). `TestProgramsNative` holds its expected output to a
  native build of the same source.

wazero's runner, `internal/cmd/wasmbench/wazero`, is a module of its own,
which `wasmbench` builds into its cache directory, so that go-quickjs never
depends on wazero (`TestNoWazeroDependency`). Node runs `node_worker.mjs`,
the programs on `node:wasi`. The Go runners time with the performance
counter on Windows and take CPU time from the process's cycles; Node's CPU
time on Windows moves in ticks of 15.6 ms, so its column is coarse there.
wazero has no GC proposal, so its `gc` and `arrays` cells are n/a.

On the machine below, `-n 5` over all nine engines takes eight minutes, most
of it the two interpreters on the programs, and the time grows with `-n` and
with `-warmup` plus `-reps`.

### Measured

The tables below were made at commit ed2c6ec9, on 2026-11-24, on an AMD
Ryzen 9 9950X3D2 (16 cores, 32 threads), Windows 11 Pro 26200, with Go
1.27.3, Node 26.12.1 (V8 14.6) and wazero 1.9.0, the machine otherwise
idle, from the `-json` output of

```sh
go run -tags quickjs_jit ./internal/cmd/wasmbench -n 5 -json \
  -engine interp,jit,jit-base,jit-opt,wazero-interp,wazero-compiler,node,node-liftoff,node-turbofan
```

run for `-suite micro` and `-suite programs` apart. Each cell is the median
of five fresh processes, in ms; a process compiled the module, made one
warmup run, reported as the first run, and one timed run. "tiered" is the
`jit` engine, the JIT as it runs by default; the ratios are its time over
Node's default and over wazero's compiler. `-matrix` prints the median and
compile tables of such a run; the first runs are in `-json` and the text
output.

The median run, the second in a process:

| micro | go-quickjs interp | go-quickjs baseline | go-quickjs tiered | go-quickjs optimizing | wazero interp | wazero compiler | Node | Node Liftoff | Node TurboFan | tiered/Node | tiered/wazero-c |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| fib | 113 | 11.7 | 4.63 | 4.65 | 395 | 7.15 | 3.21 | 3.15 | 4.14 | 1.44x | 0.65x |
| loop | 68.0 | 14.1 | 14.4 | 14.1 | 908 | 14.1 | 14.2 | 14.1 | 14.1 | 1.02x | 1.03x |
| i64 | 96.9 | 20.2 | 12.8 | 12.9 | 1095 | 19.7 | 11.1 | 11.1 | 11.1 | 1.16x | 0.65x |
| mandelbrot | 93.6 | 11.3 | 10.5 | 10.5 | 641 | 12.6 | 9.67 | 9.65 | 9.66 | 1.09x | 0.84x |
| nbody | 64.7 | 6.34 | 3.43 | 3.41 | 452 | 4.09 | 3.27 | 3.28 | 3.26 | 1.05x | 0.84x |
| bytes | 81.6 | 8.76 | 5.52 | 5.52 | 816 | 12.6 | 5.05 | 5.02 | 5.03 | 1.09x | 0.44x |
| bulk | 102 | 17.2 | 11.0 | 10.9 | 277 | 28.8 | 10.5 | 10.4 | 10.3 | 1.04x | 0.38x |
| indirect | 189 | 20.6 | 16.8 | 17.1 | 804 | 16.8 | 17.0 | 16.9 | 18.0 | 0.99x | 1.00x |
| brtable | 113 | 42.3 | 42.2 | 42.4 | 647 | 46.7 | 40.1 | 40.3 | 40.0 | 1.05x | 0.90x |
| globals | 76.7 | 26.7 | 10.7 | 11.1 | 657 | 27.0 | 14.9 | 14.8 | 14.8 | 0.71x | 0.39x |
| gc | 228 | 83.8 | 96.6 | 97.2 | n/a | n/a | 9.40 | 9.29 | 9.83 | 10x | |
| arrays | 61.5 | 27.7 | 32.5 | 33.0 | n/a | n/a | 3.99 | 3.97 | 3.93 | 8.13x | |
| simd | 18.6 | 2.95 | 2.98 | 2.95 | 197 | 4.22 | 2.89 | 2.91 | 2.90 | 1.03x | 0.70x |
| geomean | | | | | | | | | | 1.46x | 0.67x |

| programs | go-quickjs interp | go-quickjs baseline | go-quickjs tiered | go-quickjs optimizing | wazero interp | wazero compiler | Node | Node Liftoff | Node TurboFan | tiered/Node | tiered/wazero-c |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| sha256 | 791 | 67.6 | 44.1 | 43.3 | 8070 | 99.4 | 50.3 | 50.2 | 46.7 | 0.88x | 0.44x |
| sort | 690 | 128 | 89.2 | 88.8 | 5514 | 151 | 91.8 | 91.6 | 92.4 | 0.97x | 0.59x |
| json | 463 | 34.5 | 26.9 | 21.0 | 1921 | 47.3 | 20.6 | 20.7 | 19.1 | 1.30x | 0.57x |
| regexp | 305 | 35.0 | 25.2 | 24.2 | 2287 | 69.5 | 23.1 | 23.3 | 22.1 | 1.09x | 0.36x |
| gzip | 189 | 23.6 | 19.1 | 17.8 | 1534 | 32.2 | 20.0 | 20.0 | 18.8 | 0.95x | 0.59x |
| interp | 167 | 23.1 | 9.79 | 10.2 | 1560 | 23.3 | 13.6 | 13.2 | 12.0 | 0.72x | 0.42x |
| geomean | | | | | | | | | | 0.97x | 0.49x |

The programs' first runs, which pay for every compile made during the run
(a module's compile, below, is apart):

| programs | go-quickjs interp | go-quickjs baseline | go-quickjs tiered | go-quickjs optimizing | wazero interp | wazero compiler | Node | Node Liftoff | Node TurboFan | tiered/Node | tiered/wazero-c |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| sha256 | 789 | 83.0 | 62.2 | 134 | 7302 | 101 | 68.1 | 64.3 | 780 | 0.91x | 0.62x |
| sort | 706 | 147 | 113 | 197 | 5436 | 152 | 114 | 114 | 853 | 0.99x | 0.74x |
| json | 478 | 58.6 | 58.7 | 179 | 1893 | 48.7 | 67.8 | 67.7 | 1128 | 0.87x | 1.21x |
| regexp | 317 | 52.9 | 45.2 | 137 | 2272 | 69.7 | 43.5 | 43.1 | 835 | 1.04x | 0.65x |
| gzip | 205 | 46.0 | 46.5 | 160 | 1536 | 33.2 | 44.2 | 44.3 | 1011 | 1.05x | 1.40x |
| interp | 177 | 36.9 | 37.6 | 93.7 | 1565 | 24.3 | 37.7 | 37.3 | 618 | 1.00x | 1.54x |
| geomean | | | | | | | | | | 0.97x | 0.96x |

Compiling the program's module (`CompileModule`, `WebAssembly.compile`)
took 4.5 to 4.8 ms on go-quickjs whatever the tier, which decodes and
validates it and compiles a function at its first call; 91 to 100 ms on
wazero's interpreter and 909 to 1025 ms on its compiler, which compiles
every function ahead of time; and 3.6 to 5.6 ms on Node, which compiles
lazily too. A micro-benchmark's module compiled in under 0.1 ms on
go-quickjs, 0.15 to 0.2 ms on Node and 0.16 to 0.43 ms on wazero's
compiler.

What they show:

- The interpreter takes 0.10 to 0.24 of wazero's interpreter's time on the
  programs (4 to 10 times as fast) and 0.08 to 0.37 on the
  micro-benchmarks, and 8 to 18 times the default tiering's on the
  programs.
- Steady state, the default tiering is 0.97x Node's time on the programs'
  geometric mean -- under it for sha256, sort, gzip and interp, 1.30x for
  json -- and 0.49x wazero's compiler's. On the micro-benchmarks it is 1.46x
  Node and 0.67x wazero's compiler: level with Node on loop, nbody, bulk,
  indirect, brtable and simd, under it on globals; 1.1 times on i64,
  mandelbrot and bytes, where wazero's compiler is slower; fib, a call per
  node, 1.44 times, its small callees inlined. `gc` is 10 times Node's and
  `arrays` 8 times, though native code takes their objects from
  pools Go fills ahead and makes the rest through an allocation stub (The
  JIT's README, GC objects): what is left is Go's -- its allocation of each
  object, its collector's marking, and the write barrier native code's
  stores into reference slots take while it marks. The collector's
  workers show in the CPU time, 182 ms of it for `gc`'s 97 tiered; at
  GOMAXPROCS=1, where they share the program's thread, `gc` takes 259 ms
  in the baseline and 210 tiered, `arrays` 86 and 78. `arrays`' processes
  vary more than the others', 26 to 42 ms, with how much of a run the
  collector marks.
- The optimizing tier takes 25 to 56% off the baseline's time on the
  programs, and 36 to 60% on the kernels whose values it keeps in registers
  across a loop or whose calls it inlines (fib, i64, nbody, bytes, bulk,
  globals); on loop, mandelbrot, brtable and simd the two tiers are level.
  The baseline makes calls through tables natively, so `indirect` gains
  least, 17%.
- First runs: the tiered first run is the baseline's or faster, within 2%
  of it for json, gzip and interp, whose tier-ups compile during the run.
  Against the engines it is 0.97x Node's on the geometric mean -- under it
  for sha256, sort and json -- and level with wazero's compiler, which
  compiled for 0.9 to 1 s before the run. Optimizing every function at its
  first call (`jit-opt`) makes the first run 1.7 to 3.4 times as long as the
  tiered one (94 to 197 ms), as TurboFan alone does Node's (0.6 to 1.1 s):
  that is why the optimizing tier waits for 30,000 calls.
- The second run is not quite steady for json, whose tier-ups are still
  being compiled in the background: its CPU time, 36.6 ms against 26.9 of
  wall time, counts the workers', and with 15 warmup runs and 10 timed ones
  its median is 21.9 ms, the `jit-opt` column's.
- Peak RSS of a program's process: go-quickjs's interpreter 42 to 59 MiB,
  the baseline 45 to 62, tiered 47 to 85 (json, its compiles in flight),
  optimizing 68 to 91; Node 73 to 91; wazero's compiler 122 to 167 and its
  interpreter 160 to 299.

At `GOMAXPROCS=1` the optimizing tier compiles on the spot, on the program's
goroutine, rather than on workers (see [The JIT](#the-jit)). The same
command with `GOMAXPROCS=1` set, `-engine jit,jit-base -suite programs`,
the first and the second run, ms:

| program | baseline first | tiered first | baseline second | tiered second |
|---|---:|---:|---:|---:|
| sha256 | 81.9 | 62.9 | 66.6 | 42.6 |
| sort | 148 | 117 | 130 | 89.1 |
| json | 61.2 | 78.0 | 34.3 | 34.4 |
| regexp | 56.0 | 51.4 | 36.3 | 24.5 |
| gzip | 50.4 | 61.3 | 24.2 | 19.9 |
| interp | 38.5 | 41.5 | 22.6 | 10.6 |

There the tier-ups' compiles are in the program's time: the first runs of
json, gzip and interp take 8 to 27% longer than the baseline's, and json's
second run, still compiling, is the baseline's; sha256, sort and regexp
gain in their first run as they do with workers.

## Known gaps

The engine:

- The JIT refuses the functions listed under [The JIT](#the-jit).

The `WebAssembly` namespace:

- A suspended promising call is a goroutine's stack, which keeps reachable
  what its wasm and its imports reach -- the runtime among it -- until it
  ends: when the collector finds nothing holds the reactions that would
  resume it, or when the runtime is closed. A promise only the suspending
  function's closure or the runtime's globals hold, that will never settle,
  keeps its stack until `Close`, and a runtime dropped without `Close` with
  such a stack is not collected.
- Type reflection (`WebAssembly.Function`, the `type()` methods), which Node
  does not ship either.

WASI:

- `fd_filestat_set_times` sets the times of the path a file was opened by,
  so a file renamed or removed since is not reached.
- Where the platform has no `*at` calls -- Windows among them -- rename,
  link, symlink, readlink and setting times check a path and then act on it:
  a directory swapped for a symbolic link between the two, by another
  thread or process, is not seen.
- `fd_readdir` reads a directory's whole listing when a module reads it from
  its start, and serves later cookies from it.
- On systems other than Windows without `*at` calls, setting a symbolic
  link's own times fails with `ENOTSUP`.

## Follow-ups

- Native code for the v128 fields and elements of GC objects, which the
  JIT refuses.
- array.new_fixed of more than 480 bytes of elements, array.new_data and
  array.new_elem still exit to Go; array.new of any length is native, its
  value stored in a loop (20 ns an array of 8 i32s, where the exit took
  56).
