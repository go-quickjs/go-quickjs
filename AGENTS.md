# AGENTS.md

This file applies to the entire repository.

## Project overview

`go-quickjs` is a pure-Go JavaScript engine, with a pure-Go WebAssembly
engine beside it. It began as a reimplementation of QuickJS-NG and keeps
its name, its `qjs` command and its embedding model, but its execution is
its own design: register code, run by an interpreter whose handlers are
native code generated for each architecture (docs/register-interpreter.md),
and a JIT modeled on V8's. The stack bytecode interpreter it started with,
after QuickJS's, remains only until the other architectures and the JIT
have moved over. Keep the engines portable: production code must not
require cgo, a C engine compiled to WebAssembly, a C compiler, or
platform-specific runtime dependencies.

The main areas are:

- `quickjs.go` and other root files: the public embedding API and public
  behavior tests.
- `internal/lexer`, `internal/parser`, `internal/ast`: source processing.
- `internal/compiler`, `internal/bytecode`: compilation and VM instructions.
- `internal/ucd`: the Unicode properties and case data the language asks
  about, generated at the regexp's Unicode version. Production code asks it,
  not Go's `unicode` package, anything a script can observe.
- `internal/jsonscan`: the scan of long JSON strings, AVX2 on amd64 and NEON on
  arm64, nothing needing it on the rest.
- `internal/vm`: JavaScript runtime semantics and built-ins. `Intl`, `Date`'s
  local time and `Temporal` are bindings over
  [go-intl](https://github.com/go-quickjs/go-intl), which holds the locale
  data, the calendars and the time zones. The register interpreter
  (docs/register-interpreter.md) runs every runtime's code: on native
  handlers on amd64 and arm64, and on the Go handler table elsewhere and
  with `QJS_NONATIVE=1`; test with both. The JIT compiles register code,
  the one form a function is compiled to.
- `internal/compiler/regcomp`: the register backend, which compiles a
  function's AST to register code over what `internal/compiler`'s walk,
  which emits no code, records of its scopes;
  `internal/vm/internal/nativegen` generates the register interpreter's
  native handlers.
- `stdlib`: opt-in host functionality.
- `cmd/qjs`: the command-line host.
- `conformance`: the test262 runner. The test262 checkout is external.
- `wasm`: the WebAssembly engine, its Go API, WASI and the `WebAssembly`
  namespace; see [WebAssembly](#webassembly).

The module targets Go 1.24. A `Runtime` is not safe for concurrent use; callers
give each goroutine its own runtime. Process-wide immutable data and caches,
go-intl's included, must still be safe when separate runtimes use them
concurrently.

## Working rules

- Preserve existing behavior unless the task explicitly changes it. This is a
  language engine, so apparently small coercion, property-order, error-type,
  locale, or rounding changes can affect many unrelated tests.
- Prefer focused changes in the owning layer. Do not work around a parser or
  compiler bug in the VM, or a VM bug in the public wrapper.
- Keep the public API small and document exported identifiers.
- Run `gofmt` on every changed Go file.
- `internal/vm` is sensitive to code layout. Go lays out a package's
  functions in file-name order, then all its closures, each aligned to 32
  bytes, so adding, moving or renaming code there -- even code a benchmark
  never runs -- can move the V8 suite by 5 to 10%. The layout lock (below),
  not file order, keeps the hot code in place, so files are named by
  topic -- `jit.go` and `jit_*.go` for the JIT's side, `reg_*.go` for the
  register interpreter, `native_*.go` and `.s` for the native handlers --
  and a new file goes where its topic is. Do not rename or reorder files
  or move code between them without comparing builds with
  `internal/cmd/v8bench/placements` (see Validation): what lies between
  the lock's anchors still moves. Names still decide four things. The
  lock's pads sort right before their anchors (`bigi_layoutpad.go`
  before `bigint.go`, `builtin_layoutpad.go` before `builtins.go`,
  `calls_layoutpad.go` before `callsite.go`, `generato_layoutpad.go`
  before `generator.go`, `regexp_a_layoutpad.go` before
  `regexp_abstract.go`, `strh_layoutpad.go` before `string.go`,
  `jit_layoutpad.go` before `jit_leaffast.go`, `native_r_layoutpad.go`
  before `native_run.go`, `reg_cal_layoutpad.go` before `reg_call.go`,
  `reg_ru_layoutpad.go` and `reg_run_a_layoutpad.go` around
  `reg_run.go`, `reg_runr_layoutpad.go` before `reg_runs.go`,
  `reg_sw_layoutpad.go` before `reg_switch.go`, `vl_layoutpad.go` before
  `vm.go`, `vm_b_layoutpad.go` before `vm_call.go`, `z_layoutpad.go`
  after every file), and the assembly pad,
  `native_a_layoutpad_*.s`, after the JIT's assembly and before
  `native_<arch>.s`, the first of the native handlers' block. The
  linker lays out the packages linked before vm in the order vm's files
  first import them, which `aa_imports.go` and `aa_imports_jit.go`,
  sorted first, hold. Go runs a
  package's inits in file-name order, and the handler tables are written
  and read by several: the Go handler tables are written by
  `reg_table.go` and each `reg_table_<name>.go`, then copied by
  `reg_tables.go`'s init into the native exit table, then by
  `reg_typed.go`'s into the typed tables, an order those names keep
  (`TestHandlerTables` pins every table's entries). And go generate runs
  in file-name order: snapgen (`generate.go`), nativegen
  (`native_run.go`), then layoutpad (`z_layoutpad.go`).
- The register interpreter's hot code is its loop, `runReg`, its call path,
  `rhCallW`, `regEnterW`, `regCallSlow` and `regRunFD`, the native
  dispatcher's Go side, `nativeLoop` and its loop `nativeDrive`, and the
  native handlers' block.
  `runReg` is a fixed loop over a table of handlers, the same for every
  operation (`TestRegLoopIsSmall`): an operation is a handler of its own,
  never a case added to it. A Go handler added to `reg_table.go` moves
  every handler after it, so a new operation's handlers go in a file of
  their own, `reg_table_<name>.go`, which sorts after it, and replace
  their table entries in its `init`; its value is appended before `bytecode.ROpCount`, never
  inserted, and its native handler is appended to nativegen's
  `handlerOps`, with any path its handler takes rarely laid after the last
  handler, so that no handler before it moves. A new operation lands with
  its regcomp pattern, its Go handler and slow function, its amd64 and
  arm64 native handlers and its JIT lowering (`compile.LowerReg`) in one
  change, so that it opens no refusal. What changes nothing of an
  instruction's dispatch is a mark or a flag of an operation that exists,
  not a new one (docs/register-interpreter.md, "Opcode values").
  `internal/cmd/hotdiff` shows a change leaves the hot code, the native
  handlers' block included, alone.
- What decides the speed is how much of the hot path AMD Zen 2's op cache
  holds: 64 sets of 8 entries, each set taking the code at one offset in
  every 4 KB. Hot code that a change moves onto the same sets -- even by
  moving it, its instructions unchanged -- is decoded again on every pass,
  and a loop can run a quarter slower. So keep the interpreter's hot
  functions (above, and `callObject`) as they are, and the native
  handlers, which `internal/vm/internal/nativegen` generates, the same: an
  instruction the interpreter must run is a handler of its own in both
  emitters and the Go table, and a slow path a call out of line.
- The layout lock keeps the hot code where it measured best whatever a
  change moves around it: generated pads, dead code and data, put the
  anchors of `internal/vm/layoutlock.txt` -- the built-ins, the call path,
  `runReg` and `rhCallW`, the JIT's leaf calls, the regular expression
  matcher (a pad of its own in internal/regexp), the register
  interpreter's native handlers' block (`regNativeHandlers`, by an
  assembly pad sorted before it: package vm's assembly is laid out after
  its closures, so that any closure's growth had moved it), and the Go
  runtime's `mheap_`, `writeBarrier` and `gcController` -- at fixed offsets modulo
  4 KB and 64 bytes in a `v8bench` build. After a change to package
  vm, run `go generate ./internal/vm` (or `go run ./internal/cmd/layoutpad`)
  and commit the pads; `TestLayoutLock` (internal/vm/layoutlock_test.go,
  skipped under `-short` and with another toolchain than the targets')
  fails until then. What the lock cannot hold -- code between two of its
  positions against each other, generics added among others, closures
  against each other, read-only data -- docs/optimizing.md ("The layout
  lock") lists, with how to add an anchor or a position for code that needs
  one. A pad only grows, so a change that moves an anchor back costs up to
  4 KB of pad.
- The small functions the hot paths call are worth what the inliner makes
  of them: Go inlines a function of cost 80 or less, and a change that
  grows one past it -- or grows what it inlines -- turns a few instructions
  into a call in every caller, which no test fails and the benchmarks show
  as a few per cent across unrelated tests. Moving the prototype into the
  shape made `Object.proto` bigger, and `plainArray`, `chainHasProxy`,
  `protoValue` and `ctorRoot` went over; the side fields moving after the
  object did it to `arrayLike.dense` (array_reduce up to 1.5x main's).
  `TestIntendedInlining` (internal/vm/inline_test.go, skipped under
  `-short`) builds the package with `-gcflags=-m`, plain and with the JIT,
  and fails when a function of its list is no longer inlinable; add what a
  new hot path relies on. To see what a change lost, diff the sorted
  `can inline` lines of
  `go build -gcflags=github.com/go-quickjs/go-quickjs/internal/vm=-m ./internal/vm`
  between the two builds; `-m=2`, which prints each cost, runs the go
  command out of memory on this package.
- What a newer Go adds -- generic methods, in Go 1.27 -- may be offered in a
  file built only with it, `//go:build go1.27`, which gives that file the
  newer language whatever go.mod says, as long as what it offers is there for
  the older Go too: `buffers_go127.go`'s method is `NewTypedArray`'s function.
  An older gofmt cannot parse such a file, so CI formats with the newest Go
  and tests with both.
- Native code does the same work whether the collector marks or not
  (internal/jit/README.md, "The write barrier and the marking rule"): a native path stores its
  pointers through the barrier's routine while the collector marks, and never
  leaves native code or calls Go for the marking's sake, or it is not
  compiled natively. `TestJITSameWorkWhileMarking` runs the micro-benchmarks
  with native code seeing the collector idle and marking and fails on any
  difference; its list of known breaches only shrinks. Test a native path
  under `jitMarkingForTest` too; holding the collector off in a test hides a
  breach.
- internal/jit/README.md is the JIT's design. A change to that design -- what
  the inliner takes, how registers are allocated and spilled, what a frame
  state or an exit holds, a new kind of native path -- updates it in the same
  commit, saying how the JIT works now rather than how it came to.
- A path the native interpreter adds to run a function's body without its
  frame -- a leaf, an initializer, a forwarder -- is native handler code
  that nativegen generates, from a plan the register compiler decides
  (regcomp's leaf classification). Go only says whether the handler may
  try, from runtime state such as the JIT, a due interrupt and the frame
  limit; it does not evaluate the body for the native handlers. Such code
  shares with the ordinary path the one implementation of what both do, as
  `appendTransition`'s bookkeeping and `regCtorThis`, rather than a copy.
  The Go table (other architectures, `QJS_NONATIVE`) keeps its existing
  leaf paths (`leafCall`, `pureCall`) and gains no new Go-side evaluator.
- Add a regression test for every semantic bug. Prefer an exact result and
  error type over a broad smoke test.
- Do not weaken conformance expectations, add skips, or change expected
  failures merely to make a suite green. Establish the behavior in the
  applicable specification and, for compatibility work, in current engines.
- Run a file the same way in every engine you compare. `qjs file.js` loads
  a `.js` file as Node does, as a CommonJS module: its top-level `var` and
  function declarations belong to the module, not to the global object, and
  `this` is `module.exports`. C QuickJS's `qjs` runs a file as a classic
  script. So for script semantics -- globals, `globalThis` properties a
  `var` made, test262's global code, QuickJS's `microbench.js`, which finds
  its tests through `this[name]` -- run go-quickjs's `qjs` with `-s`
  (`--script`), and Node with `vm.runInThisContext`. A difference that comes
  down to a file's top-level bindings is usually this, not an engine bug:
  check it before reporting one. Go tests that call `Runtime.Eval` already
  run scripts.
- Preserve unrelated worktree changes. Do not rewrite history or use
  destructive Git commands unless explicitly requested.

## Generated files

Do not manually edit generated files or their binary companions:

- `internal/regexp/unicodetables.go` and `internal/ucd/tables.go`, the
  Unicode data, at one version: 17.0.0, the one the test262 at CI's pin
  expects. The first is the regexp's property escapes; the second is
  package ucd's identifier, space and case tables, which the lexer, the
  regexp's case folding and group names, the compiler and the string case
  conversions read in place of Go's `unicode` package, whose tables are at
  the Unicode version of the Go that builds the program (15.0 in Go 1.24).
  `internal/regexp/internal/unicodegen` writes both in one run (below): the
  property sets from test262's generated property-escape tests, the case
  mappings and foldings from the Unicode Character Database of the version
  those tests name, checked against the SHA-256 sums pinned in its `ucd.go`.
  `TestMatchesGo` (internal/ucd) compares every code point with Go's tables
  when a Go's are at the same version.
- `internal/regexp/exec_ascii.go` and `internal/regexp/exec_utf8.go`, the
  regexp matchers over an ASCII string's bytes and over UTF-8 by code point,
  made from `exec.go`'s matcher over UTF-16 code units so that they stay the
  same code; the UTF-8 one is patched where `internal/asciigen` says. After
  changing `exec.go`, run `go generate ./internal/regexp`.
  `TestASCIIMatcherGenerated` fails while either file is out of date.
- `internal/vm/jit_snapshot_gen.go`, the JIT's snapshot of a build's
  feedback -- its answer to and its asker of each of `jitRegFeedback`'s
  methods -- written by `internal/vm/internal/snapgen` from those methods,
  which it reads from the package's `jit.go` and `jit_*.go` files only.
  After changing a method of `jitRegFeedback`, run `go generate ./internal/vm`;
  `TestSnapshotGenerated` fails while the file is out of date.
- `internal/vm/native_handlers_amd64.s`, `native_handlers_arm64.s`,
  `native_typed_amd64.s`, `native_typed_arm64.s`,
  `native_feedback_amd64.s` and `native_feedback_arm64.s`, the register
  interpreter's native handlers on each architecture with their plain
  table, the typed code's table, and the feedback dispatcher's table and
  own handlers, built with the JIT alone: machine
  code made with the JIT's assemblers by `internal/vm/internal/nativegen`
  (docs/register-interpreter.md, "Native handlers"). The generator reads
  package vm's private layout -- the context, a frame, the Runtime fields
  and the constants the handlers use -- as plain numbers from
  `NativeLayout` (`native_layout.go`), which only its helper's
  build tag, `quickjs_nativelayout`, builds. After changing the emitter, or
  a layout its offsets come from, run `go generate ./internal/vm` on amd64
  or arm64 (`go run ./internal/nativegen/cmd` in `internal/vm`
  alone; a field or a constant the emitter newly reads goes in
  `NativeLayout` first). It reads
  the layout of a build without tags and with `quickjs_jit`, one per
  variant, from the host's own build: any 64-bit host gives amd64's and
  arm64's layout, but only one whose build has the JIT (linux or windows
  amd64, darwin or linux arm64) gives the JIT's. `TestRegNativeGenerated`
  fails while a file is out of date; on a host with no JIT (darwin amd64,
  say), it checks every file but the feedback dispatchers'.
- `internal/vm/*_layoutpad.go`, `internal/vm/layoutpad_*.go`,
  `internal/vm/native_a_layoutpad_*.s`,
  `internal/regexp/aa_layoutpad.go` and `internal/regexp/layoutpad_*.go`,
  the layout lock's pads and the constants that size them for each
  configuration,
  written by `internal/cmd/layoutpad` from `layoutlock.txt`'s targets.
  `go generate ./internal/vm` runs it last, after the generators above,
  since it measures what they write; `TestLayoutLock` fails while an anchor
  is off its target.
- `wasm/internal/engine/ops_num_gen.go`, the wasm interpreter's numeric
  operators, a node for each pairing of a tree, a slot and a constant, and
  the loads and stores of memory 0 at an i32 address (`memFast`), written by
  `wasm/internal/engine/internal/numgen`; and
  `wasm/internal/engine/ops_simd_gen.go`, the SIMD operators without
  immediates, written by `internal/simdgen` beside it. After changing either
  generator, run `go generate ./wasm/internal/engine`; `TestNumericGenerated`
  and `TestSIMDGenerated` (skipped under `-short`) fail while a file is out
  of date.
- `wasm/internal/opcode/tables.go`, every opcode's constant, name and
  signature, written by `wasm/internal/opcode/internal/opgen` from the
  opcode lists of V8's `src/wasm/wasm-opcodes.h` (see
  [WebAssembly](#webassembly) for the checkout). No test regenerates it,
  since it needs that file; regenerate it when the V8 that Node ships
  changes:

  ```sh
  generated=$(mktemp /tmp/wasm-opcodes.XXXXXX.go)
  go run ./wasm/internal/opcode/internal/opgen /path/to/v8/src/wasm/wasm-opcodes.h > "$generated" && \
    mv "$generated" wasm/internal/opcode/tables.go
  ```

go-intl's data is generated in go-intl, and changes there; a new go-intl is
taken with `go get` and `go mod tidy`, never by copying its files here.

Run generators from the repository root. Use a temporary output file for
generators that write Go source to stdout so a failed run cannot truncate the
tracked file.

The Unicode tables require a test262 checkout, and fetch UnicodeData.txt,
CaseFolding.txt and SpecialCasing.txt of its Unicode version from
unicode.org (`-ucddata DIR` reads them from a directory instead). The
generator writes both files itself, formatted, each through a temporary
file and only once both are made. A test262 at a new Unicode version needs
that version's sums in `ucd.go` first:

```sh
go run ./internal/regexp/internal/unicodegen /path/to/test262
```

## Intl and data-loading invariants

- Ordinary runtime construction must not read Intl data. A service reads
  go-intl's data when it is first used: `Intl` is built the first time the
  global is read, and Temporal's calendars and zones are loaded when a
  Temporal value first needs them.
- go-intl's formatters are immutable and safe to share. What the engine keeps
  per runtime -- its locale, its zone, the `Date` environment, Temporal's
  data -- lives on the `Runtime`, never in package variables.
- A difference from Node is one of go-intl's named divergences, chosen in
  `intlCompat`: standards mode takes the standard's side, `WithNodeQuirks`
  Node's.
- `WithNodeQuirks` covers the language too. Where V8 departs from the
  standard, standards mode follows the standard and test262, and
  `WithNodeQuirks` follows V8, which the parser and compiler are told through
  their `NodeQuirks` options. Each such difference gets a test pinning both
  answers, as `TestNodeQuirksLanguage` does, and a line in the option's
  documentation.
- Error messages Intl and Temporal throw are V8's, word for word.
- Keep `new Date().toString()`, `new Date().toTimeString()`, and Intl time-zone
  names aligned with the selected Node/ICU data across locales, zones, seasons,
  and historical transitions.
- When Test262 disagrees with current Node and Chrome, retain a focused
  regression test documenting the observed engine behavior rather than
  silently changing the result to satisfy one stale expectation.

## WebAssembly

[docs/wasm.md](docs/wasm.md) describes the engine for its users;
[wasm/internal/jit/README.md](wasm/internal/jit/README.md) is its JIT's
design, which a change to that design updates in the same commit.

- The packages: `wasm` (the runtime, shaped like wazero's root package),
  `wasm/api`, `wasm/sys`, `wasm/wasi` (WASI preview 1), `wasm/wat` (the text
  format), `wasm/wasmjs` (the `WebAssembly` namespace), and under
  `wasm/internal` the decoder and validator (`decoder`), the module model
  (`module`), the opcodes (`opcode`), the interpreter, memories and
  instantiation (`engine`), the baseline JIT (`jit`), the text and script
  parsers (`text`, `wast`), the spec runner (`spectest`), and what a module
  is given of the system (`sysctx`, `sysfs`).
- WebAssembly is opt-in. The root package, `internal/vm` and `stdlib` never
  import a `wasm` package; `wasm/wasmjs` binds the two engines from outside,
  and `cmd/qjs` is the only host that installs it. The namespace is built
  only with the `quickjs_wasm` tag, as the JIT is with `quickjs_jit`: without
  it `wasmjs.Install` returns `ErrNotBuilt` and links nothing of the wasm
  engine, so its tests run with `-tags quickjs_wasm`. `TestEnginesApart` and
  `TestWasmBuildTag` (wasm/wasmjs, built either way) fail when the root
  package or `stdlib` depends on one, or when the tag no longer decides
  whether `qjs` links the wasm engine.
  What the bridge needs of the VM is a host hook, a package variable
  assigned in the `init` of `internal/vm/zzzz_host.go`, the last file, so
  that no function moves (see the layout rules above); `HostJITEnabled` is
  set in `zzzz_host_jit.go`, built with the JIT. wazero is a benchmark
  baseline only, in a module of its own: `TestNoWazeroDependency` fails if
  it reaches `go.mod` or `go.sum`.
- What a module is told is V8's, word for word: the decoder's and the
  validator's errors, in V8's order of checks and at its offsets, the link
  errors, the trap messages, and the namespace's errors with their
  prefixes -- V8 14.6's, the V8 Node 26 ships (`node -p
  process.versions.v8`). Port a message or a check from that V8's source
  rather than from memory or another engine, and confirm it with the Node
  comparisons below. A sparse checkout of the parts the engine follows is
  32 MB:

  ```sh
  git clone --depth 1 --branch 14.6.202.34 --filter=blob:none --sparse \
    https://chromium.googlesource.com/v8/v8.git /tmp/v8
  git -C /tmp/v8 sparse-checkout set src/common src/wasm test/mjsunit/wasm
  ```

  `src/wasm/module-decoder-impl.h` and `function-body-decoder-impl.h` are
  the decoder's, `src/common/message-template.h` the traps' and
  `src/wasm/wasm-js.cc` the namespace's. A limit Node's V8 has and the
  interface does not is `V8Limits` against `JSAPILimits`, chosen by
  `WithNodeQuirks`, with both answers tested and a line in that option's
  documentation, as for the language.
- Code comments say what the code does and why, in its own terms, not by
  comparison with V8 or another engine ("as V8 does"). That V8 is the
  reference belongs in the tests that compare with Node and in
  docs/wasm.md; a test may name a known difference from Node with why. The
  decoder's BSD attribution, which V8's license requires, stays.
- The JIT's frames are the interpreter's (`engine/compile.go`): local k is
  slot k, stack position h slot `nlocals + 2h`, the results from slot 0, and
  an i32 or f32 in a slot has its upper half clear. A function the JIT runs
  from its first call has a frame of validation's bound and no body for the
  interpreter until a call runs there (`Code.boundRecord`, `Code.interp`). A call crosses between
  the tiers in either direction and a native function resumes after any
  exit, so a change to the interpreter's frames, its call path or its poll
  is a change to the JIT's too. Native code computes the interpreter's
  results bit for bit where the specification fixes them, on each
  architecture: `TestJITFloatBits` holds every float operation to it, and
  `TestJITDifferential` every spec module's exports.
- Native code keeps references only in Go memory the collector sees --
  frames' reference slots, tables, globals -- never in a register across an
  exit, and stores every pointer word through the write barrier's routine
  while the collector marks, as the JavaScript JIT does
  (internal/jit/README.md, "The write barrier": `jit.StoreRef`, the flag
  at `jit.WriteBarrier`), never leaving native code for the marking's sake
  (wasm/internal/jit/README.md, "References and the collector").
  `StoreRef` is built with the JavaScript JIT. Run the spec with `WASM_JIT_BARRIER=clobber` after a change to what native code
  stores. Like the rest of
  internal/jit's native code, the wasm JIT's never moves SP, never writes
  the registers Go keeps, never executes a call instruction, and is never a
  safe point: loops and calls count toward the poll.

Validation, with the suites checked out as docs/wasm.md's Conformance says.
After a change to the engine, the interpreter or the decoder:

```sh
go test ./wasm/...
go test -tags quickjs_jit ./wasm/...
GOARCH=386 go vet ./wasm/...
WASM_SPEC_DIR=/tmp/wasm-spec WASM_THREADS_DIR=/tmp/wasm-threads \
  go test ./wasm/internal/spectest ./wasm/internal/wast -count=1 -v
GOARCH=386 WASM_SPEC_DIR=/tmp/wasm-spec WASM_THREADS_DIR=/tmp/wasm-threads \
  go test ./wasm/internal/spectest -count=1 -v
```

Every assertion passes: `TestSpec` logs `spec: 65824 of 65824 assertions
passed` at the pinned commits, and a run that does not is a regression,
never a new expectation. After a change to the JIT, or to anything native
code meets -- frames, calls, polls, memories, traps -- run the spec with every
function compiled at its first call, the differential test, and the
encoders' tests:

```sh
WASM_JIT=eager WASM_SPEC_DIR=/tmp/wasm-spec WASM_THREADS_DIR=/tmp/wasm-threads \
  go test -tags quickjs_jit ./wasm/internal/spectest ./wasm/internal/engine -count=1 -v
WASM_JIT=osr WASM_SPEC_DIR=/tmp/wasm-spec WASM_THREADS_DIR=/tmp/wasm-threads \
  go test -tags quickjs_jit ./wasm/internal/spectest ./wasm/internal/engine -count=1 -v
WASM_SPEC_DIR=/tmp/wasm-spec WASM_THREADS_DIR=/tmp/wasm-threads go test -tags quickjs_jit ./wasm/internal/spectest -run TestJITDifferential -count=1 -v
(cd internal/jit/verify && go test -tags quickjs_jit -run 'TestAMD64|TestARM64' .)
GOOS=darwin GOARCH=arm64 go vet -tags quickjs_jit ./wasm/...
GOOS=linux GOARCH=arm64 go vet -tags quickjs_jit ./wasm/...
```

The eager run logs how many functions were compiled and refused, by
reason; a refusal that was not there before is a regression too. arm64's
backend runs in CI's `wasm-spec` job on `ubuntu-24.04-arm` and `macos-15`;
before that, run it on Linux under QEMU's user-mode emulation -- in WSL,
with the Go there, the package unpacked where `sudo` is not at hand:

```sh
(cd /tmp && apt-get download qemu-user && dpkg-deb -x qemu-user_*.deb /tmp/qemu)
GOOS=linux GOARCH=arm64 go test -c -tags quickjs_jit -o /tmp/spectest.test ./wasm/internal/spectest
WASM_JIT=eager WASM_SPEC_DIR=/tmp/wasm-spec WASM_THREADS_DIR=/tmp/wasm-threads \
  /tmp/qemu/usr/bin/qemu-aarch64 /tmp/spectest.test -test.run 'TestSpec$' -test.v
WASM_SPEC_DIR=/tmp/wasm-spec /tmp/qemu/usr/bin/qemu-aarch64 /tmp/spectest.test -test.run TestJITDifferential -test.v
GOOS=linux GOARCH=arm64 go test -c -tags quickjs_jit -o /tmp/engine.test ./wasm/internal/engine
(cd wasm/internal/engine && WASM_JIT=eager /tmp/qemu/usr/bin/qemu-aarch64 /tmp/engine.test -test.short)
```

After a change to a message, a check or a limit, and to the namespace,
compare with Node 26 as `node` on `PATH`; each test fails on a difference
it does not list with why:

```sh
QUICKJS_COMPARE_NODE_WASM=1 WASM_SPEC_DIR=/tmp/wasm-spec WASM_THREADS_DIR=/tmp/wasm-threads \
  go test -tags quickjs_wasm ./wasm/internal/decoder ./wasm/internal/wast ./wasm/internal/engine ./wasm/wasmjs \
  -run 'TestCompareNode|TestNodeAcceptsCompiledModules|TestRelaxedMatchesNode' -count=1 -v
WASM_SPEC_DIR=/tmp/wasm-spec go test -tags quickjs_wasm ./wasm/wasmjs -run TestWasmSpecJSAPI -count=1 -v
```

The decoder's corpus is 24,437 modules; a change to the decoder runs it
larger too, with `QUICKJS_WASM_MUTATIONS=15000 QUICKJS_WASM_SEED=2`
(120,437 modules, about a minute and a half). `TestHandCases` checks the
hand-written cases' verdicts without Node, from `testdata/hand.golden`,
which the comparison rewrites from Node's verdicts with
`QUICKJS_WASM_UPDATE_GOLDEN=1`. A change to WASI runs wasi-testsuite on Linux,
where all 72 pass (`WASI_TESTSUITE_DIR`, docs/wasm.md).

## Validation

Start with the smallest relevant package while iterating, then run the full
suite before handing off a substantial change:

```sh
go test ./...
git diff --check
```

The long checks run once, on the final code: the full suite with and without
the JIT's tag, test262's modes and the timing across suites. A change to
the register interpreter or its JIT path also runs, on amd64 or arm64, the
suite with `QJS_NONATIVE=1`, which runs the Go handler table; and the 386
build and `TestLengthsPastAnInt32` there, where the Go table alone runs.
While a change
is iterated, test the packages it touches and time only the benchmarks it is
for, at two placements (`compare -k 2 -suite List`, `-micro -suite
bigint64_fnv1a`); `placements build -k N` makes only the placements a `-k N`
compare runs. Before timing a change that touches few functions,
`compilediff native` says which compiles changed, and a benchmark whose code
is the same need not be timed. A branch whose code is final goes to CI before
it is timed, so that the two run at once.

Time a change to package vm with its layout lock regenerated
(`go generate ./internal/vm`), so that it is compared at the base's layout
rather than at wherever it moved the hot code. The placements tool builds as
the lock measures, with `CGO_ENABLED=0` and `-buildvcs=false`, whose data
does not move with a tree's changes; build both sides with the same tool.

For concurrency or shared-cache changes:

```sh
go test -race -count=1 -run TestIntlConcurrentRuntimes .
```

For portability-sensitive changes:

```sh
GOOS=windows GOARCH=amd64 go build ./...
GOOS=windows GOARCH=arm64 go build ./...
GOARCH=386 go build ./...
GOARCH=386 go test . -run TestLengthsPastAnInt32
```

An int is 32 bits on 386 and arm: convert an index, a length or an offset
from a uint32 or an int64 only once it is known to fit.

Go 1.27's loop invariant pass moves an `unsafe.Add` sum out of the branch
that guards it, to the loop's preheader, where a nil base leaves nil+32 in
a pointer slot live across the loop's calls: a collection then throws
"invalid pointer found on stack" (golang/go#82865; once a Go release
fixes it and go.mod's Go has the fix, the check can go). Reach memory past
an object as a field's
address where the code allows it (`arrayExt`); `extAt`, `dataExtAt` and a
BigInt's words are still sums, called only where their object exists
wherever the loop is entered. For a change that adds or moves such
arithmetic, or a loop around it, run the check CI runs, with Go 1.27 (about
ten minutes, the standard library rebuilt):

```sh
go run ./internal/cmd/hoistcheck
```

A test that turns the collector off restores it in its cleanup, as
`gc := debug.SetGCPercent(-1); t.Cleanup(func() { debug.SetGCPercent(gc) })`.
`defer debug.SetGCPercent(debug.SetGCPercent(-1))` is right in a defer,
whose arguments are evaluated at once, but wrapped in a closure for
`t.Cleanup` it turns the collector off again at the end: every JIT test after
one such cleanup ran without a collection, which kept the crash above out of
CI for as long as it was there.

Some tests fail when they share the machine with other test processes, and
such a failure says nothing about the change. A failure is load if the test
passes alone at the same commit and at the base, and its message is about
time or about code that is missing, not a wrong answer. There are three
kinds:

- Tests that compare elapsed times or have a deadline:
  `TestJITCompileLinear/time`, `TestRegExpLeafSpentOnceNested`,
  `TestNativeLoopsAreInterruptible`, and the JIT's cancellation tests under
  qemu. Run them as a step of their own (`-run '^Name$' -count=3`), not
  beside other heavy tests.
- Tests that look at the JIT's cache after a run. The time-based flush lets
  code go after about a second of program time without use, which a slow
  build (checkptr, qemu) reaches. The vm tests therefore hold every
  runtime's JIT clocks by default, per runtime, so that parallel tests keep
  theirs (`jit_clocks_test.go`). A test about aging or flushing sets the
  clocks itself (`jitEpochNow`, `jitFlushNow`) or lets its runtime's run
  (`jitRunClocksForTest`), and says why. `QUICKJS_TEST_FAST_CLOCKS=1` runs
  the clocks it lets run fast.
- Tests that set a process-wide switch (`jitSSAOptimized`, `mir.Timing`,
  `JITCompileRepeat`, `regNativeOn`, the clocks). They change what every
  other runtime in the process does, so a parallel one makes other tests
  fail at random: `jitSSAOptimized` keeps compiles out of the background.
  They are never top-level parallel.

Under qemu, a test that runs its own binary as a child
(`TestStoreRefShades`, `TestRegNativeInDroppedRealm`) fails with
"exec format error", because the child is not started through qemu. Run
its child form directly: set the environment variable that marks the
child, and the `GODEBUG` the parent would give it.

For Intl changes, also run the exact golden tests:

```sh
go test . -run '^(TestIntlFormats|TestIntlMatchesICU)$' -count=1 -v
```

For changes to dates, zones or Temporal, or a new go-intl, compare with Node
across every locale DateTimeFormat has and every zone Node lists. It needs a
Node with Temporal, runs shards in parallel, one per CPU unless
`QUICKJS_NODE_TEMPORAL_PARALLEL` says otherwise, and takes about ten minutes:

```sh
QUICKJS_COMPARE_NODE_TEMPORAL=1 go test . \
  -run '^TestTemporalMatchesNodeAcrossLocalesAndTimeZones$' -count=1 -timeout=60m -v
```

If `/tmp/test262` is available, run the relevant conformance area. For Intl:

```sh
TEST262_DIR=/tmp/test262 go test ./conformance \
  -run TestConformance -count=1 -timeout 30m -v \
  -args -conformance.dir=intl402 -conformance.max-failures=20
```

For parser, compiler, VM, or built-in changes, use the narrowest applicable
`-conformance.dir` while iterating, then broaden coverage in proportion to the
change. A compiler change runs test262 a second time with
`-conformance.debugger`, every runtime made `WithDebugger`: code compiled for
a debugger has an instruction at each statement no other code has, and must
do exactly what other code does. A passing Go test command is not enough if
its log reports newly introduced conformance failures.

Performance-sensitive work should compare fresh processes, not just repeated
operations in one warmed process. Report bundle size, first-use latency,
allocations, and peak RSS. Distinguish work moved from package startup to first
feature use.

An optimization follows [docs/optimizing.md](docs/optimizing.md): what it
must show before it lands -- a measured net gain, no test much slower at
every placement, totals as well as minimums -- the order of work, and how
to tell a slower test's cost from where the linker put code and data. Read
it before measuring, not after.

`internal/cmd/v8bench` runs the V8 v7 suite: `-mode fixed` does the same
work on every run, which is what compares two builds (build each to a binary
and alternate them), `-mode compile` is the parser and compiler alone, and
`-mode score` gives the suite's own scores. `-cpuprofile` and `-memprofile`
write profiles.

```sh
go run ./internal/cmd/v8bench -dir /tmp/v8-v7 -fetch -mode fixed -n 5
```

`-bench` picks another suite, fetched the same way into a directory of its
own: `octane`, Octane 2.0 -- the V8 suite's benchmarks revised, and larger
programs: TypeScript, pdf.js, a Game Boy emulator, Box2D, zlib, Mandreel --
and `awfy`, Are We Fast Yet's fourteen benchmarks, written to compare
language implementations alike -- and `ssr`, a shop's page React 18
renders on the server (`internal/v8bench/ssr.js`, React's UMD builds
fetched, pinned by SHA-256): `FirstRender` is a fresh runtime's load and
first render, what a host making a runtime for each request pays, and
`Render` the steady state -- and `fresh`, the same files as code run on a
kept runtime (`internal/v8bench/fresh.go`): a bundle run again as a Program
and evaluated anew, a request's data as a script of literals over code the
runtime keeps, and a small script run thousands of times, each new and as
one Program. The goja and external runners below take `-bench` too; an
external engine runs `Render` alone, and `fresh` needs a runtime kept from
Go. They reach far more of the JIT than the V8 suite does. With
`-jit`, `-jitreport` prints after each benchmark what the
JIT did: what it compiled, by how much each function ran natively, and what
it refused or never tried, with why. The runner gives the JIT a 32 MiB
budget (`-jitmem`), not the runtime's default 8 MiB, which Typescript's code
does not fit; `-jitmem 0` runs with the default.

```sh
go run -tags quickjs_jit ./internal/cmd/v8bench -jit -jitreport -bench awfy -dir /tmp/awfy -fetch -mode fixed -n 1
```

What native code spends its time on, pprof cannot say: its samples there
have no symbol. `-jitcode FILE` writes every function's code as it is
compiled, with what the backend emitted each stretch of it for (a value
and its operation, a block's end, an edge, a cold path, an exit), and
`jitprof`, in the encoder verifier's module, joins it with a CPU
profile: the native samples by operation, by function, and the hottest
functions' instructions, disassembled (`-full` for all of them,
`-listing` for their SSA). It decodes amd64 and arm64; operation marks are
amd64's only. This needs a profile preserving native PCs: Go on darwin/arm64
collapses them to `runtime._ExternalCode`, losing instruction attribution.
Profile runs of ten seconds or more: the profiler takes a hundred samples
a second.

```sh
go run -tags quickjs_jit ./internal/cmd/v8bench -jit -bench awfy -dir /tmp/awfy -suite List -mode fixed -n 40 \
  -jitcode /tmp/list.jsonl -cpuprofile /tmp/list.cpu
(cd internal/jit/verify && go run ./cmd/jitprof -code /tmp/list.jsonl /tmp/list.cpu)
```

What the JIT's compiles cost, `-jitcompile N` says: every compile a run
makes is run N times, each as the program made it, the ones after the
first thrown away, and after each benchmark the runner prints each
phase's time per run of them all -- the SSA's build and optimization,
the backend's analyses, its allocation's uses, liveness, intervals,
hints and scan, the calls' saves, the instruction selection and the
encoding (`mir.Times`) -- and what the repeated compiles allocated. That
is the compiler's own CPU and garbage, apart from the program's; with
`-cpuprofile` or `-memprofile` the profile is mostly the compiler's, and
`pprof -focus=jitCompileAgain` is nothing else.

```sh
go run -tags quickjs_jit ./internal/cmd/v8bench -jit -bench octane -dir /tmp/octane -suite Typescript -mode fixed -n 1 -jitcompile 10
```

`-mode micro` runs QuickJS's micro-benchmarks, `internal/v8bench/microbench.js`
(QuickJS's `tests/microbench.js`, kept as it is there), and ours beside it,
`microbench_extra.js`, of what it has none of -- generators, async
functions, try statements, iterators, using declarations, closures
made and called anew, calls in tail position, method calls with a frame
and without, loops of a few simple instructions, which time the register
interpreter's dispatch (the Go table's with `QJS_NONATIVE=1`), private class members, Math.random, the in operator, a delete of an object's last property,
object and array spread, object rest, computed keys and spread calls,
Array.prototype.forEach, reduce, map, filter and some, and their callbacks
varied -- made once or anew, a function expression or an arrow, reading
this, writing what they captured, called by a loop of the script's own --
a closure's appends to a string it captures and what growing one in place
does worst -- reads between them, reads that keep the string, a few to a new
binding -- a template's render into a local, with escaping and numbers, for-in
loops the last cursor cannot serve, arrays past 16,384 elements, a typed
array's callback methods and its element reads and writes over every element
type, a DataView's getters and setters, Map's and Set's forEach,
String.prototype.indexOf and Array.prototype.indexOf, String's methods that
run no script -- at, codePointAt, toUpperCase, trim, startsWith, padStart,
repeat, slice, concat, String.fromCodePoint -- Math.imul, clz32, pow, atan2
and hypot, a template and a chain of calls joined, BigInts' division,
shifts, hashing, comparisons, powers, modular exponentiation, digits of
pi and decimal strings, regular expressions as programs use them --
validating, tokenizing with a global and a sticky pattern, replacing by a
template and by a function, splitting, matching every number, named
groups, the u and i flags, a subject made just before, objects a loop
carries from turn to turn and what keeping them out of the heap may cost --
functions run once, loops left early or on most turns -- JSON.parse and
JSON.stringify, over records, numbers, strings, dictionaries, layouts
that vary, with a reviver, a replacer and toJSON --
written as
its tests are: each test is given
the same work every run -- what QuickJS takes about 150 ms for, the work
the comparison with QuickJS in docs/benchmarks.md is made with -- `-n`
times, and the fastest time of one operation is printed. It needs no
`-dir`; `-suite` names tests by the start of their names. Every runner
below has it, so the four engines run the same driver.

```sh
go run ./internal/cmd/v8bench -mode micro -n 1 -suite empty_loop,func_call
```

To compare two builds, use `internal/cmd/v8bench/placements`: one build of
each side compares where the linker put package vm's hot code as much as
the code. `build` makes eight builds of the working tree, each placing
`callObject`, the JIT's leaf calls, `runReg`, `rhCallW`, `nativeDrive`
and the assembly -- the native handlers and the JIT's leaf stubs --
differently in a balanced design, and checks every one with `go tool
nm`; `-tags quickjs_jit` builds them with the JIT. `compare` runs two
labels' placements in turn and reports each side's mean. Build both sides in
the same checkout, and leave the machine idle while they run.

```sh
git checkout base   && go run ./internal/cmd/v8bench/placements build -label base
git checkout change && go run ./internal/cmd/v8bench/placements build -label change
go run ./internal/cmd/v8bench/placements compare -dir /tmp/v8-v7 base change
```

`-v` prints each placement's figure: a change that is slower at some
placements and level at the others is placement, not the change.

`-parallel` runs both labels at once, each on a group of CPUs of its own
-- the fastest cores', a level 3 cache's, one NUMA node's, found by
`-cpus auto` or given as `-cpus "0-5;6-11"` -- the groups swapped from one
run to the next, and `-k 4` takes four of the eight placements. Screen a
change so, in minutes, and confirm a batch of them with the eight before
pushing; compare a label with itself in parallel first on a machine new to
it, to see that its groups measure alike.

```sh
go run ./internal/cmd/v8bench/placements compare -parallel -k 4 -bench octane -dir /tmp/octane base change
```

`-micro` compares the micro-benchmarks the same way, in parallel too:
each placement's binary in micro mode, each test's best round, and beside
each test at how many placements the second label is more than 2% slower,
then the tests 3% or more slower at every placement. A micro-benchmark
moves with code placement more than a suite does; one slower at some
placements and level at others is placement.

With `-jit`, of builds made with `-tags quickjs_jit`, each placement also moves the JIT's code: where its first
code goes in its arena (`V8BENCH_JITPAD`), where its later chunks and
functions go (`V8BENCH_JITSEED`), both by whole 64-byte lines, and how far
into its line each function's code starts (`V8BENCH_JITLINE`, 0 to 56 bytes
by 8, `-k 4`'s 0, 16, 32 and 48), which moves its loops, which the backend
starts at a line, and its branches within their lines and 32-byte fetch
windows. Each is 0, and production code unchanged, outside the tool. A JIT
change that moves code within its lines is judged over those offsets: one
slower at offset 0 alone, where every placement had put each loop, is
placement. `-jitbackground` runs the code the way `qjs --jit` does, its
compiles in the background; a change to how a compile is asked for is
compared both ways.

```sh
go run ./internal/cmd/v8bench/placements compare -parallel -k 4 -micro base change
```

`internal/cmd/v8bench/goja` runs the same suite the same way on goja, for
comparing the two engines. It is a module of its own, run from its
directory, so that go-quickjs never depends on goja; `TestNoGojaDependency`
fails if goja reaches go-quickjs's `go.mod` or `go.sum`.

```sh
(cd internal/cmd/v8bench/goja && go run . -dir /tmp/v8-v7 -mode fixed -n 5)
```

`internal/cmd/v8bench/external` runs it on an engine that is a program of
its own -- QuickJS's `qjs`, or `node` -- from a driver script that does the
same work, and reports the same times without the allocations Go counts.

```sh
go run ./internal/cmd/v8bench/external -engine qjs -cmd /path/to/qjs -dir /tmp/v8-v7 -mode fixed -n 5
```

`-arg` passes the program an argument of its own, once for each:
`-engine node -arg --jitless` runs Node with its JIT compilers off.

`internal/cmd/v8bench/matrix` makes the tables comparing the engines --
go-quickjs without and with the JIT, QuickJS, Node without and with its
JIT, goja -- from every runner above, built from the checkout: the V8
suite, Octane and Are We Fast Yet as milliseconds for the fixed work, each
the median of three fresh-process runs, with go-quickjs's time over
QuickJS's; the V8 suite's own scores; the micro-benchmarks' largest gaps.
Where the OS gives a process CPUs of its own it runs two at once, on groups
as `placements -parallel` finds them (`-parallel=false` one at a time).
It keeps every run's output, what the machine and the engines were, and
the tables, which `-report DIR` prints again without running anything.
Use it rather than a script of one's own.

```sh
go run ./internal/cmd/v8bench/matrix -qjs /path/to/qjs -v8 /tmp/v8-v7 -octane /tmp/octane -awfy /tmp/awfy -fetch
```

Each Go runner ends with the live heap after a collection, with the runtime
still alive: one that grows with `-n` is a leak.

The fixed mode prints, beside each benchmark's time, the CPU time the
process spent on it (`cpu`), the collector's on other threads included,
and the external runner its engine's whole: on a machine with cores to
spare the time does not show what collecting costs, which an embedder
running a runtime on each core pays. A change that moves what is
allocated is compared by both, and at `GOMAXPROCS=1` too: there the
collector shares the thread, a cycle marks eight times as long, and the
script runs under the write barrier for much of it. An item worth 3.7% of a
render on 32 cores was worth 10.5% at one thread, and a literal the JIT made
in 39 ns took 95, slower than the interpreter's 69, which no run with cores
to spare showed. QuickJS has no helper threads, so one thread is the fair
comparison with it.

`internal/cmd/disasm` prints the bytecode a script or module compiles to,
each function's register code under the source lines it comes from;
`-func` picks functions by name, and `-jit` says whether the JIT lowers
each function. Look there
first when a micro-benchmark moves: it shows which instructions a loop
runs as.

```sh
go run ./internal/cmd/disasm -func global_read bench.js
```

`internal/cmd/regcount` counts, over directories of code, the register
code's patterns the fused instructions take and those they leave -- a
step before a compare-and-branch, `x * k`, `(x ± y) | 0` -- the evidence
an operation's value is spent on; `internal/cmd/regrefusals` counts the
register backend's refusals by reason.

`internal/cmd/compilediff` compares what the front end makes at two
revisions, for every `.js` file of the directories it is given, as a script,
strict, a module, for a debugger, with Node's quirks and as an eval's code:
every field of every function, its constants, its register code an
instruction a line with each instruction's position, the line table's
answers, and a rejected program's exact error. `-mutants N` adds N mutations
of each file. `-reg` compares the register code alone, each operand as what
it names -- a name, a constant, a site by its kind and name -- so that a
change that numbers a function's tables anew compares by its code
(`-nopos` without positions, `-noleaf` without leaf kinds). It prints the first difference by file, mode,
function and line. The same output means a change to the lexer, the parser or
the compiler changes nothing a program that compiles does, nor what the JIT is
given, so it needs only what the comparison cannot see, not all of the above:
the packages' tests and a plain test262 run for programs that must be rejected
and that its directories do not have, and the race detector for what is made
lazily, as the line table is. It says nothing of concurrency, of what the
output keeps alive, or of time.

```sh
go run ./internal/cmd/compilediff -base origin/main -mutants 8 /tmp/octane /tmp/test262/test
```

`compilediff native` does the same for the JIT, over two builds' runs of a
suite with `-jitheld`, which makes a program's compiles the same from one
build to the next: it holds the clocks the JIT's cache ages code by and turns
the collector off, and without it some 20 of 1,300 compiles differ between two
runs of one build. The collector off, a heap grows without bound -- Octane's
Game Boy to 6.75 GB, 159 MB of it live -- so it runs at a memory limit of 3
GiB, where the collector does run, unless `GOMEMLIMIT` gives one; two runs of
one build still make the same compiles. Each compile's code is compared but for addresses, with
its marks, SSA listing, spills and the descriptions of its calls and exits;
and what arm64's backend makes of each compile, on any machine, not run. The
same code means a change to the JIT's compile path changes nothing a program
does with the JIT, so it needs that package's tests and test262's JIT mode, not
the benchmarks; it says nothing of what the compile keeps alive, or of
compiling in the background. Math.random is seeded anew for each run and a
program's clock is the machine's, so a program they steer -- Octane's Crypto
and Typescript, the micro-benchmarks -- need `native -any`, which compares
function by function in no order, and a control of the base against itself.

```sh
go build -tags quickjs_jit -o /tmp/base ./internal/cmd/v8bench    # at the base
go build -tags quickjs_jit -o /tmp/change ./internal/cmd/v8bench  # at the change
/tmp/base -jit -jitheld -jitcode /tmp/base.jsonl -bench awfy -dir /tmp/awfy -mode fixed -n 1
/tmp/change -jit -jitheld -jitcode /tmp/change.jsonl -bench awfy -dir /tmp/awfy -mode fixed -n 1
go run ./internal/cmd/compilediff native /tmp/base.jsonl /tmp/change.jsonl
```

A change that keeps memory longer shows in neither: compare the conformance
run's peak memory as well, which is how a cache that outlived its objects was
caught.

## Releases

Every release is named the same way, in go-quickjs and in go-intl alike:

| | Form | Example |
|---|---|---|
| Tag | `vX.Y.Z`, annotated | `v0.9.1` |
| Tag message | `<module> vX.Y.Z` | `go-quickjs v0.9.1` |
| GitHub release title | `<module> vX.Y.Z` | `go-quickjs v0.9.1` |

`<module>` is the repository's name: `go-quickjs` or `go-intl`. Nothing else
goes in a title, such as a date, a codename or a summary; what a release
contains belongs in its notes.

```sh
git tag -a v0.9.1 -m "go-quickjs v0.9.1"
git push origin v0.9.1
gh release create v0.9.1 --repo go-quickjs/go-quickjs --verify-tag \
  --title "go-quickjs v0.9.1" --notes-file notes.md
```

Versions follow semantic versioning. Before 1.0, a new exported identifier or
a change in behavior bumps the minor version, and a release with only fixes
bumps the patch.

A go-quickjs release requires a tagged go-intl, never a pseudo-version: tag
and release go-intl first, then take it with `go get` and `go mod tidy`.

Before tagging, run the full suite and the checks in [Validation](#validation)
that apply to what changed since the last release. After publishing, confirm
that `proxy.golang.org` serves the new version.
