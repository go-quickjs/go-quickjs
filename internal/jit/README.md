# Native JIT tier

This package is the optional native tier's machine-code side. The VM side
lives in `internal/vm/zzjit_*.go`.

The tier is **experimental and opt-in**, and it needs two things:

1. a build with `-tags quickjs_jit` on `linux/amd64`, `windows/amd64` or
   `darwin/arm64`;
2. a runtime made with `quickjs.WithJIT()` (or `qjs --jit`,
   `-conformance.jit`, `v8bench -jit`).

Without either, every function runs in the interpreter and the tree tier,
exactly as before (see [Zero cost when off](#zero-cost-when-off)).

This file states the design and the contracts the code keeps.

## Layout

| Package or file | Role |
|---|---|
| `ir` | The slot IR, one instruction per bytecode PC, with a state map at each. `Validate` checks a program, and `Evaluate`, a Go interpreter of the IR, is the oracle the SSA tests compare with. |
| `compile` | Lowers bytecode into the IR (`LowerSSA`, and `LowerSSAInline` for a callee to inline), or refuses it (`Refusal`). It never imports the VM. |
| `ssa` | Builds SSA from a program and the VM's feedback (`Feedback`), and optimizes it. `eval.go` runs SSA over a heap model, for tests. |
| `mir` | Selects instructions, allocates registers and encodes amd64 and arm64 machine code (`CompileAMD64`, `CompileARM64`), both on every target. |
| `abi` | The contract between the code and the VM: the context block (`Context`), the exits, and the value encoding (`Encoding`), given as data. |
| `asm` | The two assemblers. |
| `ssacode.go`, `arena.go`, `exit.go` | Placing code (`SSACode`, `Arena`), entering it, and reading its exits. |
| `verify` | A module of its own that checks the code `mir` makes for both targets against `golang.org/x/arch`'s disassemblers. |
| `entry_*.s` | Entry from Go (`enterSSA`), and the calls of Go native code makes (`callGo`). |
| `memory_*.go`, `policy_darwin.go` | Executable memory. |
| `internal/vm/zzjit_native.go` | The VM's side: selection, the code cache, budgets, host exits. |
| `internal/vm/zzjit_ssa.go` | Feedback for the builder, entering and resuming code, the calls of Go, native calls and their frames. |
| `internal/vm/zzjit_disabled.go` | The stubs for builds without the tier. |

The VM files are named `zzjit_` so that they sort after `ztree_*.go`.
Package vm's code is laid out in file-name order, so this placement moves
nothing the interpreter or the tree tier runs.

## Design

The tier follows V8's optimizing compilers where it can: it compiles what
the interpreter and the tree tier have seen run, speculates on what their
caches recorded, and deoptimizes when a speculation fails.

**Pipeline.** A hot function's bytecode is lowered to the slot IR
(`compile.LowerSSA`), one instruction per bytecode PC with the frame's
state at each. `ssa.Build` turns it into typed SSA (Braun et al.), asking
the VM's feedback (`ssa.Feedback`) at each site: a property's shape and
index from the site's inline cache, up to four shapes (polymorphic), the
prototypes a method was found on, a global's or a script lexical binding's
cell, the functions a call has called, a literal's pool. `ssa.Optimize`
unboxes numbers, merges guards a dominating one already made, removes dead
code and keeps references alive across stores and calls (`keep.go`).
`mir` allocates registers by linear scan and encodes amd64 or arm64; the
code is placed in the runtime's arena (`SSACode`).

**Speculation and deoptimization.** A guard checks what the code was
compiled for -- a number, a shape, a callee -- and exits to the interpreter
at the guard's bytecode PC if it fails. The failure is noted, and the
function is compiled again with that site generic, as V8 deoptimizes and
reoptimizes. Code compiled before a site had feedback is compiled again
once its cache knows the site, at the function's next invocation.

**Calls.** A call the VM has seen call a compiled function is made
natively: the callee's frame is made in the VM's stack, its context is the
next one, and its code is entered through a link (`jitLink`) that holds the
function's current code, so callers keep working when the callee is
compiled again or its code is dropped. Small callees are inlined, with
their own frames made at an exit. A call of a built-in, or of anything the
code does not call natively, is a call of Go (`ssa.GoCall`): native code
calls into Go and goes on after it, without leaving.

**Allocation.** Object, array and regular expression literals, and the
receivers of constructions, are taken from pools of objects the VM makes
ahead (`abi.ObjectPool`), as V8's code allocates inline; Go refills a pool
that runs out.

**Exceptions.** The compiler emits structured code, so the handlers in
force at each instruction are known when lowering (`ir.Program`'s
`Handlers`), and native code registers none: wherever Go takes the frame
at an exit it gives it those handlers. A throw from what Go does there --
a call, an operation, a throw statement -- is unwound to its handler as
the interpreter unwinds it, and native code goes on at the handler, an
entry of its own, as V8's code goes on at a handler (`jitCatch`). A
finally clause's normal completion goes on natively; any other (a return,
a throw) deoptimizes, the interpreter reproducing it. A using declaration's
scope is such a try statement, its resources added and disposed of by Go.

**Iteration.** A for...of or for...in loop's cursor, and `yield*`'s
delegate, are Go's. Native code steps an array's for...of in place; any
other step is a call of Go, which answers the next value or, done, the
uninitialized value native code branches on. A step's iterator is closed
however the loop is left, as the interpreter closes it. `yield*`, an
`await using` scope's disposal steps, `import()` and `import.meta` leave
for Go and go on past it.

**Closures.** A local an inner closure captures stays in the frame's slot,
which the closure's upvalue points at until the frame returns, as in the
interpreter. Native code treats that slot as the local's cell
(`ssa.OpLocalCell`), as it does a captured binding's: it assigns the local
there as it assigns it, and reads it there again after anything that may
have run a closure -- a native call, a call of Go that runs script, an
entry -- and frame states leave it out, the frame holding it already. A
closure is made, and a scope's end closes what closures captured, by a
call of Go on the frame Go entered the code with (`ssa.GoClosure`,
`GoClose`); a native callee, which has no frame of the VM's, leaves for Go
to make it. A call of a closure that captures bindings checks the
function the callee runs, in its environment and realm, not the object
(`ssa.CallSite`'s `ByCode`), and gives the callee the closure it finds, as
V8 checks a call's code: a callback made anew at each call of its maker is
a new object, the same function. Such a closure is not inlined: an inlined
callee's captured bindings are Go's.

**Generators and async functions.** `yield` and `await` are exits at which
Go suspends the frame as the interpreter does, and each resumption enters
native code at the frame's pc -- the entry after the yield or await, or,
the first time, past the parameters -- as the tree tier resumes a tree at
the block there (`jitResume`, `runGeneratorTree`). A call of one makes its
object, so none is called natively or inlined.

**Top-level code.** A script's, a module's or an indirect eval's code runs
once, so it is entered only at its loops, as V8 enters top-level code by
on-stack replacement: never at its start, and no refusal of it is kept in
the cache, which a loop evaluating thousands of scripts would fill. Its
declarations, which come before its loops, are left to the interpreter,
but for a `let`, `const` or `class` given its first value between two
loops, which Go gives it and native code goes on. A script's globals and
its top-level lexical bindings are read and assigned where they are, as a
function reads them (`ssa.OpGlobalCell`, `OpLexCell`). A module's bindings
are fixed once it is linked, so its code reads each one in the object that
has it, at that object's address (`ssa.OpBindingCell`), as V8 reads a
module's variables in its context and an import in its exporter's cell:
its own bindings in its environment, an import where the binding it names
is, found again from the modules already loaded, and a name the module
does not declare in the global object. An imported `const` is a constant.
A direct eval's code, which reads its caller's scopes, stays in the
interpreter.

**Tail calls.** A call in tail position gives up its frame for the
callee's only deep in the stack, as the interpreter's does
(`tailCallDepth`): a shallow one keeps its frame, as a stack trace shows,
and native code makes it as any call (`ssa.OpTailRoom`, `OpTailSelf`, by
`abi.Context`'s `TailLevel`). Deeper, a call of the closure the frame runs
writes its arguments to the frame's parameters and enters the code at its
start again, a loop in the frame, as a compiler turns tail recursion into
one; in a native callee's frame, a call of another function the call has
puts that function's frame in this one's place, its context this one's,
and goes on in its code, returning where this frame would have. A frame Go
entered the code with, which the VM's frame describes, is given up by Go
(`errTailCall`), as the interpreter gives it up. Frames the interpreter
keeps for such a call -- a construction's, an arrow's sharing a derived
constructor's this -- native code keeps too (`TailFrame`).

**Not compiled yet.** Destructuring from an iterator and code using
private fields run in the tree tier, which handles both; the JIT is to
follow it there.

**Profitability.** Each native stretch's work is estimated, exits and
calls of Go counted against it; code whose stretches do too little between
them is left to the tree tier (`jitSSAProfit`).

### Reading the heap in place

Native code reads the VM's objects where they are, through the offsets
`abi.Encoding` gives, rather than through copies: an array's elements, a
property table, a binding's cell. A reference it reads keeps the address of
the cell it came from as its source, and Go reads the reference back from
there at an exit. Reading an address back as a pointer is outside what
`unsafe.Pointer`'s rules promise; it is sound while Go's heap does not move
and nothing else changes the object graph while native code runs, which a
`Runtime` (not safe for concurrent use) guarantees. It is checked with each
new Go: `TestJITObjectLayout` pins the fields native code reads, and the
JIT's tests run under `-d=checkptr=2` and `GODEBUG=gccheckmark=1`.

### The write barrier

Native code tests the runtime's write-barrier flag before it writes a
pointer, as compiled Go does. While the collector marks, the write is made
by a call of Go -- a store, a pool's object taken, a property added along a
transition, a native call's frame -- so Go's own barrier records it, and
native code goes on after it. The flag cannot change between the test and
the write: Go sets it only with the world stopped, which native code,
never a preemption point, is not. Emulating the barrier's buffer in native
code was rejected: its contents would have to reach the collector before
any safepoint Go could stop the goroutine at.

## The execution boundary

**Entry.** `enterSSA` is `NOSPLIT|NOFRAME`. It tail-jumps into the code with
the context block in a register, and the code returns with a plain `RET` to
`enterSSA`'s Go caller. Native code:
- never moves SP or touches the frame pointer;
- never executes a call instruction: it calls another function's native
  code by jumping there, with that level's context, and Go by jumping to
  `callGo` (`abi.Context.GoOp`);
- uses Go's ABI0, not the platform C convention, on every OS.

**Calls of Go.** To the Go runtime, `callGo` is a function the Go function
that entered the code called: its frame and return address are all a
traceback, a stack scan or the stack's growth meets. Go may do anything
there, run JavaScript included, and the code goes on where Go says.

**Registers.**

| | amd64 | arm64 |
|---|---|---|
| Never written | SP, BP, R14 (g) | SP, FP (R29), LR (R30), R18 (platform), R28 (g) |
| Clobbered | the rest, X15 included | the rest, R19-R25 included |

ABI0 callers expect clobbering: on amd64, Go restores X15 and R14 after any
ABI0 call. `verify` checks these rules for every function its corpora
compile.

**Polls.** Every back edge decrements the interpreter's back-edge counter
(`Context.BackEdges`). When it runs out, the code calls Go (or, under a
stress budget, exits to it), which checks for cancellation, `Halt` and the
memory limit, gives the loop a new budget, and lets the code go on.

**Not a safe point.** Generated code is never a safe point. SIGURG, Windows'
`SuspendThread` and SIGPROF see a PC outside Go's text:
- preemption retries until Go code runs again;
- a profile sample counts as external code.

The back-edge budget, and the calls of Go, bound how long the goroutine can
delay a stop-the-world pause.

**Faults kill the process.** Go cannot recover a fault at a PC outside its
text:
- on Windows the exception goes unhandled;
- on Unix it is fatal, except a nil-page access, which `Runtime`'s recover
  turns into an error.

So a backend bug can take down the host. The defences are verification and
differential testing: the SSA evaluator and the IR oracle, the VM's tests
under every stress setting (`QJS_JIT_STRESS`), the differential fuzzer, and
`verify`.

**Compiler panics are refusals.** Lowering and the backend recover a panic
into a refusal, and the function runs in the existing tiers.

## Values and the garbage collector

The code uses the VM's frame and objects in place (see
[Reading the heap in place](#reading-the-heap-in-place)):
- It holds a slot's number word, never its pointer word. A reference stays
  in the slot that held it at entry, which native code does not write before
  it exits; an exit has Go copy it where it goes, and store a primitive over
  a reference (`abi.Record`). An ambiguous phi's shadow tells Go, at run
  time, which slot a value came from.
- It reads an object through the pointer word of the slot that holds it,
  at the offsets `abi.Encoding` gives: an array's class, flags, length
  and elements; a property table, by its shape; a script's global and
  lexical bindings (`Context.Global`, `Context.GlobalLex`).
- A store of a pointer tests the runtime's write-barrier flag first, as
  compiled Go does, and while the collector marks is made by a call of Go
  (see [The write barrier](#the-write-barrier)).
- References it must keep across calls of Go are kept in the context
  (`Context.Keep`), where the collector sees them.
- It keeps no address between entries.

## Exits and deoptimization

**State maps.** Each reachable PC has a state map: the bytecode PC and the
live operand depth. An exit names one, and its records say what Go writes
into the frame.

**Exits.**
- A guard (`ExitDeopt`) fails before its instruction commits anything. Go
  writes the frame at that PC and the interpreter runs on from there. The
  failure is noted, and the function is compiled again with the site
  generic, at most four times (`jitReoptimizations`).
- A host exit (`ExitHost`) leaves one instruction to Go, which runs it, and
  the code goes on at the next entry.
- A native callee that cannot go on leaves its frame to Go (`ExitTable`),
  which finishes the call in the interpreter, and its caller's code goes on
  after the call.
- Work already committed, earlier loop iterations included, is never
  replayed.

## Selection and the code cache

**Function entry.** A closure counts framed calls. The eighth may compile its
function (`jitHotCalls`).

**Loops.** A loop may also promote at a back-edge check: on-stack
replacement at the loop's header, from the interpreter or the tree tier.

**Native calls.** A call the VM has seen call a function it may compile is
made natively, or inlined. A callee whose code Go never enters on its own is
compiled for native callers only.

**The cache.** Each runtime keeps its own cache, weakly keyed by the shared
bytecode function. Shared bytecode carries no JIT state. It holds as many
functions' code as the JIT's budget has room for (see [Memory](#memory)):
no count of entries bounds it.

**Closure hints.** A closure remembers its code by a hint, a slot in the
cache's hint table (not a pointer), which holds the entry and its
function's address. The hint finds the entry only for a closure of that
function, so a stale hint misses however often its slot was reused. The
closure stays 128 bytes; the table grows to the most entries the cache has
held at once, its free slots reused.

**Refusals.**
- Permanent: lowering or the backend refused it. These are cached, and the
  closure is marked refused.
- Deferred: the budget had no room for the code. These are cached until
  the cache releases code.
- Temporary (no budget at all): retried after another warm-up. When the OS
  denies executable memory, the runtime marks the JIT unavailable for good.

**Giving up.** Code whose native work per exit stays low is entered by Go no
more (`entrySlow`, `jitSSAProfit`), and runs only for its native callers.

**Debugger.** Runtimes made `WithDebugger` never run native code.

## Memory

**Allocation.**
- A runtime's code lives in its `Arena`: chunks of 64 KiB (a larger
  function gets a chunk of its own), each function placed after the last
  at 64-byte alignment. A runtime compiling 101 functions holds them in 2
  mappings (`TestJITArenaMappings`).
- Placing a function makes the pages it touches writable, copies it, and
  seals them read/execute again. Pages are never writable and executable at
  once, and published code is never patched. Other code in those pages does
  not run meanwhile: an arena is one runtime's, and its native code always
  returns to Go before Go compiles anything.
- A chunk is unmapped when the last function in it is released. The arena
  locks, since a finalizer can release code on another goroutine.
- Windows flushes the instruction cache with `FlushInstructionCache`.
- Darwin uses `ic ivau` with barriers.

**Darwin policy.** Darwin refuses executable memory to hardened or
code-signing-enforced processes (`ErrUnavailable`). There is no `MAP_JIT`
path yet.

**Not the script's memory.** The memory meter never counts the JIT, so a
script's measure, and whether it fails with `ErrMemoryLimit`, is the same with
the JIT on and off. The JIT has a budget of its own (`jitBudget`):
`WithJITMemoryLimit`'s, or 8 MB, or an eighth of the memory limit.

**The budget bounds all the JIT holds** (`jitState.held`), as V8 bounds its
code and what describes it by memory, not by a count of functions:

- each entry: its machine code as placed, its entries' offsets and its
  exits' descriptions (`SSACode.Footprint`), what it was compiled with and
  what it has learned, and its object pools with the objects made for them;
- the programs it lowered, one lowering a function (`ir.Program.Footprint`);
- the contexts native code runs in (4 at first, as many more each time they
  run out, up to 96: some 17 KB each), the hint table, the links, the
  compiles' workspaces between compiles, and code a suspended call of Go
  still holds.

Entries are measured at each compile and once an epoch; the rest as it
changes. Not counted: the arena's free space between live functions, which
it does not reuse until a chunk empties.

**Making room** (`makeRoom`). A compile asks for room for what its entry is
likely to hold (bytes per bytecode instruction, from the last compiles),
then, compiled, for exactly what its entry will hold before its code is
placed. The cache reclaims, in this order, each least recently used first:
lowerings of functions it has no entry for; refusals remembered; code not
used in the last two epochs (old, as V8 flushes code by age); the
workspaces. Code used within two epochs, code that runs, and an entry being
compiled are never reclaimed. If all it may reclaim would not make the room,
it reclaims nothing and the compile is refused: the function runs in the
existing tiers until some code is old. An epoch is a collector cycle, or
50 ms.

**Release.**
- `ReleaseClosed` releases code once native frames have unwound; code a
  suspended run still uses stays until the run leaves it.
- A failed release keeps ownership for a retry.
- A finalizer on `SSACode` reclaims what a host abandoned.

Runtime construction allocates nothing for the JIT.

## Zero cost when off

**Without the tag.**
- `jitBuilt` is false, and every hook in the interpreter and the tree tier is
  either an empty stub or inside `if jitBuilt`, so it is compiled out.
- `internal/cmd/hotdiff` checks the result. Compare an untagged `cmd/qjs` at
  main with one at the change: `executeAt`, `runTree`, `runTreeNested`,
  `callObject`, `callTree`, `runFD` and `backEdgeCheck` must be the same,
  instruction by instruction.

**With the tag, JIT off.**
- The JIT's fields fit existing padding: `closure` stays 128 bytes.
- `Runtime`'s and `Realm`'s JIT fields sit at the end of their structs, so no
  offset the interpreter reads moves. `TestJITFieldLayout` checks both builds.
- Measure the remaining flag tests with `placements`.

**With the JIT on.** Code the JIT never runs keeps the tree tier's fast nested
calls: only a frame whose loop may still promote keeps `runTree`'s recover
(`jitTreeRecovery`).

## Validation

```sh
go test ./internal/jit/... ./internal/vm                      # untagged
QUICKJS_REQUIRE_JIT=1 CGO_ENABLED=0 go test -tags quickjs_jit ./internal/jit/... ./internal/vm .
go test -tags quickjs_jit -race -count=1 ./internal/jit/...
go test -tags quickjs_jit -gcflags=all=-d=checkptr=2 ./internal/jit/... ./internal/vm
go test -tags quickjs_jit ./internal/jit/compile -run '^$' -fuzz '^FuzzLower$' -fuzztime=30s
(cd internal/jit/verify && go test -tags quickjs_jit ./...)   # encodings, both arches
TEST262_DIR=/path/to/test262 go test -tags quickjs_jit ./conformance \
  -run TestConformance -count=1 -timeout 60m -args -conformance.jit
```

- `QUICKJS_REQUIRE_JIT=1` fails a native test job that would otherwise pass
  by falling back.
- The VM's JIT tests force promotion and assert native entry, so they can't
  pass in the existing tiers.
- `-conformance.jit` keeps the normal thresholds, under which few test262
  tests reach native code. `QJS_JIT_STRESS=threshold,budget=3,deopt=7`
  compiles every function at its first call, polls every three back edges,
  and finishes every seventh poll's invocation in the interpreter.

`disasm -jit` reports a function's eligibility and IR without native support:

```sh
go run ./internal/cmd/disasm -jit -func sum -e \
  'function sum(n) { let s=0; for(let i=0;i<n;i++) s+=i; return s }'
```
