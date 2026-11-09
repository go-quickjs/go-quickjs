# WebAssembly

go-quickjs has a WebAssembly engine of its own, in pure Go: a decoder and
validator, a closure-tree interpreter, and an optional baseline JIT for amd64
and arm64. It implements WebAssembly 3.0 and the threads proposal, and it
reports invalid modules, failed links and traps with V8's messages, word for
word.

It is reached two ways, and both are opt-in:

- from Go, through the `wasm` package, whose API is shaped like wazero's;
- from JavaScript, through the `WebAssembly` namespace, which a host installs
  in a runtime with `wasm/wasmjs`. `qjs` installs it, as Node has it.

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
| Sign extension, non-trapping float-to-int, multi-value, bulk memory | yes | yes; the bulk instructions run in Go |
| Reference types | yes | refused |
| Fixed-width SIMD | yes | refused |
| Tail calls | yes | yes; `return_call_indirect` through Go |
| Extended constant expressions | yes | -- |
| Multiple memories | yes | loads and stores of memory 0 only |
| Memory64 and table64 | yes | refused for loads and stores of an i64 memory |
| Exception handling (`exnref`), and the legacy `try` | yes | refused |
| Typed function references | yes | refused |
| Garbage collection: structs, arrays, i31, casts | yes | refused |
| Relaxed SIMD | yes, with the host architecture's choice for each operator | refused |
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
memory64's past 4 GiB. `wasm/wat.Compile` turns the text format into a binary,
which is how the package's examples (`wasm/example_test.go`) write their
modules.

`NewRuntimeWithConfig` takes a `RuntimeConfig`:

- `NewRuntimeConfig()` and `NewRuntimeConfigInterpreter()` run everything in
  the interpreter; `NewRuntimeConfigCompiler()` compiles hot functions to
  native code where the JIT runs (see [The JIT](#the-jit)).
- `WithMemoryLimitPages` caps every memory; a module declaring more fails to
  compile.
- `WithMemoryLimit(bytes)` bounds what the runtime's modules hold together:
  see [Memory limits](#memory-limits).
- `WithMemoryCapacityFromMax` allocates a memory's maximum up front where the
  OS cannot reserve it (32-bit platforms), so that it never moves.
- `WithCompilationCache` shares compiled modules between runtimes, keyed by
  the bytes' SHA-256, the memory limits and whether the JIT is on.
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
  subscription is ready, until the earliest clock. A subscription to reading
  or writing a descriptor is ready at once; one of a descriptor that is not
  open is ready with `EBADF`. Go programs' goroutines and timers run on it.
- Rights are reported, and enforced once a module drops some with
  `fd_fdstat_set_rights`: what needs a dropped right fails with
  `ENOTCAPABLE`.
- The clocks of process and thread CPU time fail with `EINVAL`, and
  `proc_raise` with `ENOSYS`.
- There are no sockets: the `sock_*` functions fail with `EBADF` for a
  descriptor that is not open and `ENOTSOCK` for one that is.
- `fd_pwrite` on a file in append mode writes at the offset it is given.
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
rt := quickjs.New()
if err := wasmjs.Install(rt); err != nil {
	return err
}
```

`Install` defines the global `WebAssembly`, which is made the first time a
script reads it, so a runtime that never uses WebAssembly pays nothing for it.
`qjs` installs it in every runtime it makes, workers' included; the root
package and the standard library do not. `wasmjs.WithoutJSPI()` leaves out the
JS Promise Integration.

The namespace is the WebAssembly JavaScript interface, with the error types
and messages of V8 (Node 26), word for word, prefixes included
(`WebAssembly.Instance(): Import #0 "m" "f": ...`):

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

Constructors read `new.target`'s prototype before their arguments, as Node's
do. A context that ends -- `EvalContext`'s deadline, say -- stops wasm as it
stops script, in a call made directly or from a promising call.

A runtime made `WithJIT` runs the WebAssembly it compiles natively too, where
the JavaScript JIT runs: see [The JIT](#the-jit).

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
| Module size | 2^63-1 bytes | 1 GiB | 1 GiB |

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
- Calls nest 30,000 deep (`engine.MaxCallDepth`), past which a call traps
  with "Maximum call stack size exceeded".
- A GC array may be as large as 2^30-1 bytes with its header.

### Memory limits

`RuntimeConfig.WithMemoryLimit(bytes)` bounds the memory a runtime's modules
hold together, and `quickjs.WithMemoryLimit` bounds a JavaScript runtime's
heap with what its WebAssembly holds in it.

- Linear memories and tables are counted exactly, as they are made and
  grown. A `memory.grow` or `table.grow` past the limit returns -1, and
  `Memory.prototype.grow` throws its RangeError, "Unable to grow instance
  memory", as when the memory cannot be had. A module whose memories do not
  fit fails to instantiate with "Out of memory: Cannot allocate Wasm memory
  for new instance", which unwraps to `wasm.ErrMemoryLimit` in Go; a
  `WebAssembly.Memory` that does not fit throws "could not allocate memory".
- GC structs and arrays are counted as they are allocated. When the count
  would pass the limit, what is still reachable is counted again -- from the
  modules' globals, tables and segments and the running call's frames -- and
  a program that still does not fit stops with `ErrMemoryLimit`, which no
  wasm handler or JavaScript `try` catches. A program that lets go of what it
  allocates runs on.
- What a closed module held is given back once Go collects it.
- Under `quickjs.WithMemoryLimit`, the runtime's measurement of its heap asks
  each WebAssembly object it reaches what it holds, and a memory's buffer is
  counted by the memory's size, not the address space reserved for it. A
  `WebAssembly.Table` that does not fit stops the script, as an allocation of
  a size the script chose does.

The count of GC objects is an estimate: the Go API's may pass the limit by a
sixteenth of it before it is found, and what only a call running on another
goroutine holds is counted at that call's own next count.

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
  V8's of the module's bytes and the name the module's and the function's.
- In JavaScript the same texts come with the interface's prefixes, from
  `CompileError`, `LinkError` and `RuntimeError`.

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
  operation returns or throws: 1,708 lines over 11 scripts, all equal but 26
  that the test lists with why, each a message of the JavaScript runtime's or
  a gap listed under [Known gaps](#known-gaps).
- `wasm/internal/engine`'s `TestRelaxedMatchesNode` compares the relaxed SIMD
  operators with Node's Liftoff and TurboFan code, 20,000 calls each.

## The JIT

The interpreter compiles a function body, on its first call, into trees of Go
closures: each expression a tree of typed nodes, each statement returning
whether to go on, branch or return. The baseline JIT (`wasm/internal/jit`)
compiles a function to native code in one pass over its validated body, with
a register cache of the operand stack over the interpreter's own frames, so a
call crosses between the tiers in either direction. Its design is in
[its README](../wasm/internal/jit/README.md).

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
linux/amd64, windows/amd64 and darwin/arm64. On linux/arm64 only the Go API
reaches the wasm JIT. A runtime made `WithDebugger` runs wasm in the
interpreter.

A function is compiled once it has been called 10 times
(`engine.DefaultJITThreshold`), or once a call of it has run 1,000 back
edges of its loops in the interpreter (`engine.DefaultOSRThreshold`): the
call then goes on in native code from the loop's head, on-stack replacement,
so a function called once that loops for long runs natively too. Each loop
has an entry of its own in the function's code, which loads what resuming
after an exit to Go does; both tiers keep everything in the frame's slots at
a loop's head, so nothing is translated. A function the JIT refuses is tried
once, and its loops stop counting.
Native code is shared by the instances of a compiled module and lives in an
arena of the module's own, released when the module is collected; it is not
counted by `quickjs.WithJITMemoryLimit`. Loops, calls and tail calls count
toward the same context poll the interpreter's do.

Native code computes what the interpreter does, bit for bit where the
specification fixes the bits: NaN quieting, the minimum and maximum of zeros,
trapping and saturating truncations, one rounding for unsigned 64-bit
conversions. Calls between native functions of an instance are native;
calls of imports, of host functions and through tables, and the bulk memory
and table instructions, return to Go and resume.

A function is refused whole, and stays in the interpreter, when it has:

| Reason | What |
|---|---|
| `simd` | a v128 value or any SIMD instruction |
| `references` | a reference value, or any reference or table-element instruction |
| `gc` | any GC instruction |
| `exceptions` | `try_table`, `throw`, `throw_ref`, or the legacy `try` and its clauses |
| `atomics` | any atomic instruction |
| `memory64` | a load or a store of an i64 memory |
| `multi-memory` | a load or a store of a memory other than 0 |
| `cpu` | on amd64, `popcnt` without POPCNT, rounding without SSE4.1 |
| `frame`, `size` | an operand stack deeper than the interpreter's frame, or code past the JIT's size limit |

With every function compiled at its first call, the spec suite compiles 4,320
functions and refuses 2,522: 1,499 for SIMD, 361 for references, 255 for
memory64, 189 for multiple memories, 89 for exceptions, 80 for atomics and 49
for GC.

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
goroutine.

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
| The JavaScript API tests (`test/js-api`) | 1,204 of 1,223 subtests; 18 of the others fail in Node 26 too, with the same messages, and the last is the lazy global's descriptor | WebAssembly/spec `f3268fa9` |
| Validation against Node 26 (V8 14.6) | no difference over 345,311 modules, in three corpora | -- |
| wasi-testsuite, `wasm32-wasip1` | 72 of 72 on Linux; 67 on Windows, the 5 that make symbolic links skipped without the privilege to | `prod/testsuite-base` |

CI's `wasm-spec` job runs the spec scripts on Linux, Windows and macOS (arm64),
on GitHub's linux/arm64 runner, and for 386; where the JIT runs, it runs them
again with every function compiled at its first call, with the engine's tests,
and runs `TestJITDifferential`.

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
| `WASM_JIT` | in a JIT build, compiles every function: `eager` at its first call, a call with loops entering native code at its first back edge; `osr` so for every call Go or the interpreter makes; `on` or `1` after 10 calls; a number after that many |
| `WASM_JIT_OSR` | the back edges a call runs before its loop enters native code, or `off` |
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
WASM_SPEC_DIR=/tmp/wasm-spec go test -tags quickjs_jit ./wasm/internal/spectest -run TestJITDifferential -count=1 -v

# The JavaScript API tests
WASM_SPEC_DIR=/tmp/wasm-spec go test ./wasm/wasmjs -run TestWasmSpecJSAPI -count=1 -v

# The comparisons with Node
QUICKJS_COMPARE_NODE_WASM=1 WASM_SPEC_DIR=/tmp/wasm-spec WASM_THREADS_DIR=/tmp/wasm-threads \
  go test ./wasm/internal/decoder ./wasm/internal/wast ./wasm/internal/engine ./wasm/wasmjs \
  -run 'TestCompareNode|TestNodeAcceptsCompiledModules|TestRelaxedMatchesNode' -count=1 -v

# wasi-testsuite
WASI_TESTSUITE_DIR=/tmp/wasi-testsuite/tests go test ./wasm/wasi -run TestWASITestsuite -count=1 -v
```

`TestSpec` logs `spec: N of M assertions passed`, and under `WASM_JIT` how
many functions were compiled and refused, by reason. `TestJITDifferential`
runs each spec module that links with the spectest module alone twice,
interpreted and with every function compiled at its first call, calls every
export whose signature is numbers with the scripts' constants, each type's
edges and random values, and compares the results, the traps, and every
memory and global after each call: 2,089 modules, 4,891 functions and 58,568
calls on amd64, with no difference.

## Benchmarks

`internal/cmd/wasmbench` runs the same workloads on the engine's interpreter
and its JIT, on wazero's interpreter and compiler, and on Node, and reports
for each the compile and instantiate times, the first run, the best and the
median run, the CPU time, the Go heap's allocations per run and the peak
RSS. Run it from the checkout's root:

```sh
go run ./internal/cmd/wasmbench -n 3                              # interp
go run -tags quickjs_jit ./internal/cmd/wasmbench -suite micro    # interp and jit
go run -tags quickjs_jit ./internal/cmd/wasmbench -engine jit,wazero-compiler,node -suite fib,sort
go run -tags quickjs_jit ./internal/cmd/wasmbench -matrix -n 5 > matrix.md
```

Flags:

- `-engine`: any of `interp`, `jit` (a build with `-tags quickjs_jit` on
  amd64 Linux or Windows), `wazero-interp`, `wazero-compiler`, `node` (its
  default tiering), `node-liftoff` (`--liftoff --no-wasm-tier-up`) and
  `node-turbofan` (`--no-liftoff`). The default is `interp`, and `jit` in a
  JIT build; `-matrix` takes them all.
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
  `gc` (binary trees of structs, arrays of i32) and `simd` (i32x4). A
  kernel is a function called many times, since the JIT compiles a
  function once it has been called ten times.
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
wazero has no GC proposal, so its `gc` cells are n/a.

On the machine below, `-matrix -n 1` takes two and a half minutes, most of
it the two interpreters on the programs, and the time grows with `-n` and
with `-warmup` plus `-reps`: about 12 minutes at `-n 5`, 25 at `-n 5 -reps
3`.

A sample, one process per cell (`-matrix -n 1`), the median run in ms, at
commit f564657d: AMD Ryzen 9 9950X3D2, Windows 11, Go 1.27.3, Node 26.12.1,
wazero 1.9.0, on 2026-11-09. Single runs are noisy; take figures from `-n 5`
or more.

| micro | go-quickjs interp | go-quickjs JIT | wazero interp | wazero compiler | Node | Node Liftoff | Node TurboFan |
|---|---:|---:|---:|---:|---:|---:|---:|
| fib | 219 | 11.3 | 368 | 7.35 | 3.13 | 3.19 | 4.31 |
| loop | 221 | 14.4 | 951 | 14.3 | 14.2 | 14.3 | 14.3 |
| i64 | 266 | 19.7 | 1096 | 19.9 | 11.2 | 11.3 | 11.3 |
| mandelbrot | 147 | 11.3 | 644 | 12.7 | 9.73 | 9.75 | 9.78 |
| nbody | 139 | 9.89 | 441 | 4.40 | 3.32 | 3.30 | 3.30 |
| bytes | 236 | 9.30 | 837 | 12.7 | 5.06 | 5.09 | 5.07 |
| bulk | 132 | 118 | 281 | 27.2 | 10.6 | 10.5 | 10.5 |
| indirect | 581 | 526 | 803 | 17.0 | 17.5 | 17.5 | 18.3 |
| brtable | 330 | 42.4 | 655 | 46.6 | 39.9 | 39.9 | 40.5 |
| globals | 177 | 26.7 | 692 | 27.3 | 15.0 | 14.9 | 14.8 |
| gc | 354 | 352 | n/a | n/a | 10.3 | 9.25 | 9.52 |
| simd | 75.0 | 75.8 | 188 | 4.09 | 2.89 | 2.89 | 2.89 |

| programs | go-quickjs interp | go-quickjs JIT | wazero interp | wazero compiler | Node | Node Liftoff | Node TurboFan |
|---|---:|---:|---:|---:|---:|---:|---:|
| sha256 | 2563 | 294 | 6794 | 94.9 | 49.7 | 48.2 | 47.0 |
| sort | 6999 | 633 | 5335 | 150 | 92.3 | 90.2 | 96.8 |
| json | 4172 | 62.6 | 1810 | 47.3 | 20.6 | 20.8 | 18.7 |
| regexp | 4341 | 81.0 | 2198 | 78.6 | 24.9 | 25.4 | 23.0 |
| gzip | 4365 | 713 | 1488 | 31.9 | 20.0 | 20.0 | 18.5 |
| interp | 3886 | 3863 | 1579 | 25.0 | 14.2 | 14.2 | 13.6 |

The program's compile took about 20 ms on go-quickjs, 100 ms on wazero's
interpreter, 1 s on its compiler (which compiles every function ahead of
time) and 4 ms on Node (which compiles lazily).

The interpreter has become faster since, by the single `-engine interp` runs
its commits report: a chain of nested blocks runs as one statement
(637d7726), the loads and stores of memory 0 read the memory and their
address in place (518d3d87), and calls through a table no longer allocate
(d0a4f5c6). After them the programs take 1,933 ms for sha256, 1,741 for
sort, 889 for json, 852 for regexp, 470 for gzip and 499 for interp, each
less than wazero's interpreter in the table, and `micro/indirect` 457 ms.

On-stack replacement (fa041de6) moved the JIT's column most where a hot loop
runs in a function called a few times. Its commit's runs, against those
interpreter changes and on a machine running other work, took the JIT's
median for interp from 531 ms to 26.6, gzip from 146 to 56.8, sort from 322
to 183, sha256 from 183 to 122, regexp from 69.6 to 64.7 and json from 61.8
to 55.0; `micro/bulk`, whose time is in the bulk instructions Go runs, did not
move.

## Known gaps

The engine:

- A trap's innermost frame carries the offset of the last call its function
  made, or 0, not the trapping instruction's.
- The JIT refuses the functions listed
  under [The JIT](#the-jit).

The `WebAssembly` namespace:

- `Object.getOwnPropertyDescriptor(globalThis, "WebAssembly")` is the
  accessor the global is until first read, as Intl's is.
- `toResizableBuffer` of a shared memory returns its fixed-length
  `SharedArrayBuffer`, not a growable one.
- A trap's `stack` has the JavaScript frames, not wasm's.
- `JSON.stringify` writes a GC object as an empty object, where V8 writes
  `null`.
- A promising call suspended on a promise that never settles keeps its
  goroutine until nothing holds the promise's reactions.
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

- ESM integration: importing a `.wasm` file as a module, and its source
  phase (`import source m from "./m.wasm"`).
- `postMessage` and structured clone of a `Module`, and of a shared
  `Memory`, between workers.
- Native code for what the JIT refuses -- references, SIMD, GC, exceptions,
  atomics, memory64 and multiple memories -- and an optimizing tier above the
  baseline one.
- `wasmbench`'s `jit` engine on arm64: the harness builds it on amd64 only.
