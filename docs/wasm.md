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
