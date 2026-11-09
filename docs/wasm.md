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

### Tests

`wasm/wasi` tests each function's errnos, and builds a Go program for
`GOOS=wasip1` (`wasm/wasi/testdata/prog`) and runs it: arguments,
environment, streams, exit codes, files on writable and read-only mounts,
the sandbox, clocks and sleeping, `crypto/rand`, and goroutines. `-short`
skips the program, as does a toolchain that cannot build it.

`TestWASITestsuite` runs the prebuilt `wasm32-wasip1` tests of
[wasi-testsuite](https://github.com/WebAssembly/wasi-testsuite) (Rust, C and
AssemblyScript), named by `WASI_TESTSUITE_DIR`. All 72 pass on Linux; on
Windows, five need the privilege to make symbolic links.

```sh
git clone --depth 1 -b prod/testsuite-base https://github.com/WebAssembly/wasi-testsuite
WASI_TESTSUITE_DIR=$PWD/wasi-testsuite/tests go test ./wasm/wasi -run TestWASITestsuite -v
```

### Known gaps

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

A sample, one process per cell (`-matrix -n 1`), the median run in ms: AMD
Ryzen 9 9950X3D2, Windows 11, Go 1.27.3, Node 26.12.1, wazero 1.9.0, on
2026-11-09. Single runs are noisy; take figures from `-n 5` or more.

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
