# WebAssembly JIT

Native code for WebAssembly functions, built only with the `quickjs_jit` tag
on amd64 (linux and windows) and arm64 (darwin and linux) and used only for
code that asks for it (`engine.Code.EnableJIT`, or `WASM_JIT` in tests). On
linux/arm64 it is the only native code: the JavaScript JIT is not built
there, and the engine checks `internal/jit.RawSupported`, not `Supported`.
Elsewhere -- without the tag, on 386, on other systems -- the engine's
`jit_off.go` makes every hook a constant or an empty type, and the
closure-tree interpreter (`wasm/internal/engine`) runs everything.

The JavaScript and WebAssembly JITs share the lowest layer and nothing else:
the instruction encoders (`internal/jit/asm/amd64` and `asm/arm64`, checked
against `golang.org/x/arch` by `internal/jit/verify`), the executable memory
(`internal/jit.Arena`, `PlaceRaw`, `RawCode.EnterAt`) and the rules of the
execution boundary (`internal/jit/README.md`, "The execution boundary"):
native code never moves SP, never writes the registers Go keeps (BP and R14,
g, on amd64; FP X29, LR X30, X18 and X28, g, on arm64), never executes a
call instruction, and is never a safe point. Each JIT has its own front end,
tiers and Go entry points.

This package compiles; the engine places, enters and handles exits:

| File | |
|---|---|
| `ctx.go` | the context block and the exits: the contract between the two |
| `scan.go` | the refusals: what the compiler leaves to the interpreter |
| `reader.go` | the interpreter's body reader, which this package cannot import |
| `compile.go` | the front end every backend shares: constructs, branches, locals, calls through the engine |
| `stack.go` | the operand stack's entries and its register cache |
| `compile_GOARCH.go` | a backend's entries, exits, traps, branches, native calls |
| `stack_GOARCH.go` | its registers, and the moves between registers, slots and constants |
| `ops_GOARCH.go` | its numeric and memory instructions |
| `atomic.go`, `atomic_GOARCH.go` | the atomic instructions: what they are, and a backend's code for them |
| `engine/jit_on.go` | tiering, placement, entry, the exits' handling |

## Tiers

A function runs first in the interpreter. `run` counts its calls
(`compiled.hot`); past the Code's threshold (`DefaultJITThreshold`, 10; 0
compiles at the first call, which `WASM_JIT=eager` asks for) the
**baseline** compiler compiles it: one pass over the validated body,
emitting a fixed sequence for each instruction over a register cache of the
operand stack. Later calls, from the interpreter or from native code, enter
the native code. A function the compiler refuses (see Refusals) is marked
and stays in the interpreter for good; so does one whose code cannot be
placed. A compiler panic is a refusal (`internal`), never a crash.

A call that runs in the interpreter counts its loops' back edges (the
frame's `osr`, set when the call begins); past the Code's OSR threshold
(`DefaultOSRThreshold`, 1,000 back edges; `SetOSRThreshold`,
`WASM_JIT_OSR`) the function is compiled, if it is not, and the call goes
on in its native code from the head of the loop it is in: **on-stack
replacement**, for a function called too few times to be compiled whose
loops run long -- a program's main loop, a decompressor's, an
interpreter's dispatch. A call of a function that has native code but runs
in the interpreter -- one that began before the function was compiled, a
copy of it deeper on the stack being the one that compiled it -- goes on
natively at its first back edge. A function the compiler refuses has its
calls' loops stop counting, for the call and for later ones. Under
`WASM_JIT=eager` the first call of each function with loops runs in the
interpreter and goes on natively at its first back edge, and under
`WASM_JIT=osr` every call Go or the interpreter makes does, so that the
spec scripts run the loops' entries.

An optimizing tier, later, would build SSA from the body and replace the
baseline code of the hottest functions.

The tiers share the interpreter's frame, so a call may cross between them in
either direction, and a call may cross from the interpreter to native code
at a loop's head.

## On-stack replacement

At a loop's head both tiers have the frame in the same state: every operand
stack entry in its position's slot, the loop's parameters at its base, every
local in its own slot (native code's homes are stored at every exit and
loaded at every entry). The interpreter's loop statement stores the branch's
values into the loop's slots before it counts the back edge, so its frame
there is what native code's loop label wants, with no translation.

Each loop has an entry (`Code.Loops`, by the loop instruction's offset in
the module, which both tiers read the body from): out of line, it loads
what a resumption loads -- L, memory 0's base (and size, on arm64), the
poll counter, the return stack's entry, the locals' homes -- and jumps to
the loop's head. The engine enters it as it enters the Go entry, as a new
level with the frame's return stack entry (`enterNative`, `exec.osr`), with
the interpreter's frame, depth and call stack as they are at the loop.

From the loop's head the native code runs the rest of the function, the
loop's exits and the constructs around it included, to the function's
return: it leaves the results in slots 0 up, where the interpreter's return
does, and the loop statement returns `sigReturn`, which every enclosing
statement passes out and `run` takes as a return already made (the
epilogue's copies are not run). A tail call the native code leaves to Go
for the frame comes back as `sigTail`, which `run` and `tailChain` make as
the interpreter's own. A trap's frames, the polls and the depth are those
of any level.

A function the compiler refuses -- one with exception handlers among them,
which the interpreter runs under a recover of their own -- is never entered
at a loop. The engine narrows what enters a frame from the host -- the
arguments of an `Invoke`, a host function's results -- to the halves native
code keeps clear, which the interpreter alone did not need: a value left in
a slot under a loop is read by native code after the loop's entry.

## Frames

A frame is the interpreter's (see `engine/compile.go`): numeric slots on the
exec's chunked stack, which never moves. With no references or vectors in
compiled code, numeric local k is slot k, the parameters first; stack
position h is slot `nlocals + 2h`, `nlocals` being the number of locals or
of results, whichever is more; a function's results are left in slots 0 up.
A frame has the slots the interpreter's compile of the function counted
(`nslots`); the compiler refuses a function whose stack needs more
(`frame`).

Every i32 and f32 in a slot or a general register has its upper half clear,
as the interpreter leaves them, so a slot is read as the interpreter reads
it and copied whole.

Native code addresses the frame through a register of its own (`L`: BX on
amd64, X19 on arm64).

**Locals.** The most used locals live in registers of their own for the
whole function -- on amd64 three integers in general registers and four
floats in SSE ones, on arm64 six and eight -- chosen by their uses, a use
in a loop counting eight times one out of it -- and the rest in their
slots. A local's home is the same at every point of the function, so
control merges need nothing of it. A local.get copies the home into a
register of the operand stack's, so that a later local.set does not change
an entry. Before a call and an exit, the homes of
the locals the function ever writes are stored into their slots (the
others' slots hold their values already: a parameter's the caller's, a
declared local's the prologue's zero), and after them every home is
loaded again; the prologue loads them first.

**The operand stack** is a list of entries, each a constant, a register
-- general for integers, SSE or FP for floats -- or its position's slot, or
the flags of a comparison that the very next br_if or if consumes: an
integer comparison's on both, a float comparison's too on arm64. An
instruction pops its operands, owning their registers, and pushes its
result; when no register is free, the lowest entry holding one is stored
into its slot. Every entry is stored into its slot when control merges --
entering a block, a loop or an if, and so at every branch's target -- and
at a call and an exit, so that at a label the whole stack is in slots, as
in the interpreter, and no register is live.

## Registers

On amd64:

| Register | |
|---|---|
| DI | the context |
| BX | L, the frame's slots |
| R15 | memory 0's base |
| R12 | the poll counter |
| R10 | the return stack's next entry |
| R11, X15 | scratch, never cached |
| R13, R9, R8 | homes of integer locals, else the cache's |
| X14-X11 | homes of float locals, else the cache's |
| SI, R8, R9, R13, AX, CX, DX | the cache's general registers, AX, CX and DX last (division and shifts take them, and LOCK CMPXCHG AX) |
| X0-X14 | the cache's SSE registers |
| SP, BP, R14 | never written |

On arm64:

| Register | |
|---|---|
| X0 | the context, where `RawCode.EnterAt` leaves it |
| X19 | L, the frame's slots |
| X20 | memory 0's base |
| X21 | memory 0's size |
| X22 | the poll counter |
| X23 | the return stack's next entry |
| X16, X17, D30, D31 | scratch, never cached |
| X27 | the encoder's temporary, for offsets and immediates an instruction cannot hold |
| X26, X25, X24, X15, X14, X13 | homes of integer locals, else the cache's |
| D29-D22 | homes of float locals, else the cache's |
| X1-X15, X24-X26 | the cache's general registers, the homes last; at a call every entry is in its slot, and the call sequence uses X1 and X2 |
| D0-D29 | the cache's FP registers |
| SP, X29, X30, X18, X28 | never written |

The registers of memory 0, the poll counter and the return stack hold the
same thing in every native frame of a level: a native call leaves them as
they are, and an exit stores the poll counter and the return stack's entry
into the context and the resumption loads them all again. On arm64 memory
0's size is one of them, as only Go grows a memory and native code
resumes after it; a shared memory's is still read at each access.

The code enters and leaves Go with a plain jump and return: on arm64 LR
holds the return to `RawCode.EnterAt` for the whole of a level, every exit
returns through it, and native calls jump through registers (BR) with
return addresses made by ADR, never BL.

## The context

`Ctx` is a Go struct of words, none a Go pointer, one for each level of
native code an exec has entered (`jitLevel`), its address in DI on amd64
and X0 on arm64:

- `L`: the running frame's slots, set before entry and stored at every exit.
- `Links`: the Code's link table, each function index's native entry or 0.
- `MemBase`, `MemSize`: memory 0's first byte and size, set before every
  entry; a shared memory's size is read through `MemSizePtr` at each
  access, as other threads grow it. Only Go grows a memory, so the code
  reloads the base after every exit and reads the size from the context
  (amd64) or reloads it with the base (arm64).
- `Mems`: the instance's table of its memories, three words each -- the
  first byte, the size, the address of the size -- filled before every
  entry when the instance has more than one memory (see Memory).
- `Globals`: the instance's table of its globals' addresses.
- `ChunkEnd`: the end of the stack chunk the frames are in.
- `RSP`, `RSEnd`: the native return stack, shared by an exec's levels: its
  next entry, which a register holds in native code and an exit stores,
  and its end -- or, if that is before, the entry at which a call would
  pass `MaxCallDepth`.
- `Poll`: the poll counter, the exec's, which a register holds in native
  code: copied in at entry and out at every exit.
- The exit record: `Exit`, `Arg0`-`Arg4`, `ExitFn`, `ExitAddr`.

## Entry, exits and resumption

Go enters a function through `RawCode.EnterAt` at its Go entry, which loads
L, memory 0's base (and size, on arm64), the poll counter and the return
stack's entry from the context; native callers jump to its entry, past
that. A loop's entry loads them too, and the locals' homes (see On-stack
replacement).
Each function's code starts with its return stub, which stores `ExitReturn`
and returns. Before entering, Go pushes a return stack entry whose return
address is that stub: the function returns as every frame does, and its
return goes to the stub, and the stub to Go.

Native code never calls. Anything it does not do itself is an exit: it
stores L, the exit's kind and arguments, its own function index and the
address to resume at, and returns to `enterNative`'s loop, which does the
work and enters the code again at that address, as an offset in the
function's code (`RawCode.EnterAt`). At the resume address the code reloads
the registers it keeps and the locals' homes. Every entry that lives past an exit
is in its slot.

| Exit | Go |
|---|---|
| `ExitReturn` | the frame Go entered -- at its start or at a loop's head -- returned |
| `ExitTrap` | panics with `engine.Trap`: the trap's code, its frame's offset |
| `ExitCall` | calls a function -- imported, the host's, without native code, or with no room -- with `callGeneric`, the arguments and results in the frame's slots |
| `ExitCallIndirect` | finds the function as the interpreter does, its traps included, and calls it |
| `ExitTailCall`, `ExitTailIndirect` | a tail call made otherwise than natively: see Calls |
| `ExitPoll` | `pollContext`, the context's cancellation |
| `ExitOp` | memory.size, memory.grow, memory.init, memory.copy, memory.fill, data.drop, table.size, table.init, table.copy, elem.drop, memory.atomic.wait32, memory.atomic.wait64, memory.atomic.notify, on the frame's slots; an atomic instruction's offset is the 64-bit `Arg2` |

A trap's site stores its offset and jumps to the function's stub for the
trap's code; there is no resumption.

Before Go does anything for native frames, `jitSync` rebuilds the exec's
call stack and depth from the level's return stack entries -- each names its
callee and the offset of the call in its caller -- so that a trap's frames,
a host function's caller and the depth limit see them; `jitUnsync` puts
them back. A panic out of Go's work unwinds through `enterNative` without a
defer: the exec's `tryState`, and `Invoke`, restore the number of levels
with the rest.

## Calls

A call of a function defined in the module is made natively when the callee
has native code (its link entry is not 0), its frame has room in the chunk,
and the return stack has room for an entry; otherwise it is an `ExitCall`.
Native code keeps no depth: the return stack ends, for it, where the next
call would pass the limit, so that call goes through Go, whose depth check
traps, and the depth Go sees is the level's at entry plus its entries. The
call first counts down the poll counter, as the interpreter's `run` does.
With every entry and the written locals' homes stored, the caller copies
the arguments into the parameters of the callee's frame, at its own
frame's end (`L + nslots`), pushes the return stack entry -- the return
address, its own L, the callee's index above the call's offset -- moves L
and jumps through the link table. The callee's return sequence pops the
entry, restores L and jumps back; the caller loads its locals' homes and
copies the results from the callee's frame into its stack positions.

The return stack is Go memory, words, one for each exec, grown by Go (every
level's pointers moved) when a native call finds it full and calls through
Go.

A `return_call` of a defined function with native code is made in place:
the arguments into the frame's parameters, the return stack's top entry
renamed, a jump to the callee. Otherwise it exits, and Go makes the call
natively itself if it can (a function of the same instance with native code
and room), else ends the level with the interpreter's tail call (`x.tail`,
`tailChain`) if the frame is the one Go entered, else calls the function
and resumes the frame at its return. `return_call_indirect` always exits,
to the same handling. The interpreter's `tailChain` enters native code too,
so a chain of tail calls across the tiers does not grow the stack.

Calls through a table, of imports and of host functions are exits.

## Memory

A load or a store checks the end of its access -- the effective address
plus the access's size -- against the memory's size, and goes to a trap
stub past it. For an i32 memory the effective address is the address,
zero-extended, plus the offset, computed in 64 bits, where neither sum
wraps. For an i64 memory the address plus the offset is computed first and
traps where it carries out of 64 bits, as the interpreter's does; then the
sum plus the size, which traps where it carries too. A constant address
whose sums carry is a jump to the trap.

Memory 0's base is in a register the code keeps (R15, X20) and its size in
the context (amd64) or a register (X21, arm64). Another memory's base and
size are read from the instance's table of its memories (`Ctx.Mems`) at
each access: the table's address from the context, then the memory's entry
-- its base, its size, and the address of its size -- into a register of
the access's own, which ends holding the base. Only Go grows a memory, in
an exit or in a call that went through Go, and the engine fills the table
before every entry, so a grow by the function itself, by a function it
calls or by the host is seen at the next access, and a memory that is a Go
slice and moved when it grew is read where it is now. A shared memory's
size is read through the address of its size at each access, as other
threads grow it; its bytes never move.

The access is `[base + address + offset]` on amd64 -- the offset added to
the address first for an i64 memory, or where it is past 2^31-1 -- and on
arm64 the offset is added to the address first, in 64 bits, and the access
is `[base + that]`. The bulk instructions of any memory are exits.

An atomic access checks, after an i64 memory's carry and before the
bounds, that the effective address is a multiple of its width, and traps
with `TrapUnalignedAccess` if not, in the interpreter's order. Its
instructions are the processor's own, at the access's width, sequentially
consistent:

| | amd64 | arm64 |
|---|---|---|
| load | MOV, MOVZX | LDAR, LDARB, LDARH |
| store | XCHG | STLR, STLRB, STLRH |
| add, sub | LOCK XADD, of the negated operand for sub | a loop of LDAXR, the operation, STLXR, round again while the exclusive store fails |
| and, or, xor | a loop of LOCK CMPXCHG from the old value in AX | the same loop |
| xchg | XCHG | the same loop |
| cmpxchg | LOCK CMPXCHG, the expected value in AX | the loop, left at the first unequal comparison |
| atomic.fence | MFENCE | DMB ISH |

A narrower read-modify-write's result is its old value zero-extended,
whatever the operand's or the expected value's upper bits: amd64 clears
the register's bits above the width after the instruction, which leaves
them; arm64's exclusive loads clear them, and an expected value of 8 or 16
bits is masked before its comparison. On arm64 the exclusive loop holds no
other access, so that nothing but another thread breaks the pair. The
address is the memory's base plus the effective address, in a register,
as those instructions take it. memory.atomic.wait32, wait64 and notify are
exits (`ExitOp`), which check the address as the interpreter's do.

## Polls

Every loop back edge decrements the poll counter and exits when it runs out
(`ExitPoll`), as the interpreter's loops do, and so does every native call
and tail call, for recursion without a loop; Go counts the tail calls it
makes for native code, as the interpreter's `tailChain` counts its own. Native code is not a safe point: the polls
bound how long a goroutine in it delays preemption and a stop-the-world
pause.

## Floats

The numerics are the interpreter's, bit for bit where the spec fixes the
bits, on each architecture: the interpreter's operators compile to its own
instructions, whose NaNs differ from the other's (an arm64 operation with a
signaling NaN operand returns that NaN quieted, ahead of a quiet one; x86's
returns its first operand's).

On amd64:
- add, sub, mul, div, sqrt, and the conversions between f32 and f64: the
  SSE instruction Go compiles the interpreter's operator to.
- ceil, floor, trunc, nearest: ROUNDSS and ROUNDSD, which quiet a NaN as the
  interpreter's `a + a` does; SSE4.1 is checked (`x/sys/cpu`).
- min, max: a NaN operand gives the sum, a quiet NaN; equal operands the OR
  (min) or AND (max) of their bits, so that -0 is below +0.
- abs, neg, copysign: masks of the sign bit, never arithmetic.
- The trapping truncations compare with the interpreter's bounds, as
  doubles -- an f32 widened first, exactly -- and trap on NaN; an unsigned
  64-bit result at 2^63 or above is converted less 2^63, the bit set after.
- The saturating truncations clamp by the same bounds, NaN to 0.
- An unsigned 64-bit integer at 2^63 or above converts halved, its lowest
  bit kept as a sticky bit, then doubled: one rounding, as the interpreter's
  `u64ToF32` and `u64ToF64`.

clz and ctz are BSR and BSF with a CMOV for 0; popcnt is POPCNT, checked.

On arm64, with the FPCR Go leaves (no default NaN, no flush to zero, no
alternate handling):
- add, sub, mul, div, sqrt and the conversions between f32 and f64: the
  FP instruction Go compiles the interpreter's operator to.
- ceil, floor, trunc, nearest: FRINTP, FRINTM, FRINTZ and FRINTN, which
  quiet a NaN as `a + a` does.
- min, max: FMIN and FMAX. Their NaN is the one FADD of the same operands
  gives -- both process NaNs alike, a signaling NaN ahead of a quiet one,
  the first operand ahead of the second -- which is the interpreter's
  `a + b`; and they order -0 below +0, as the interpreter's OR and AND of
  equal operands' bits do.
- abs, neg, copysign: AND, EOR and ORR of the sign bit in a general
  register, never FABS or FNEG.
- The trapping truncations compare with the interpreter's bounds in the
  operand's own type, in which each is exact, trapping on NaN with the
  conditions an unordered comparison makes true (LT, LE), then FCVTZS or
  FCVTZU, exact in range.
- The saturating truncations are FCVTZS and FCVTZU alone: they clamp to the
  result's range and give 0 for NaN, as the interpreter's do.
- The conversions of integers are SCVTF and UCVTF, one rounding, the 64-bit
  unsigned ones included.
- A float comparison leaves its flags, as an integer one does, for a br_if
  or if: MI (lt), LS (le), GT, GE and EQ are false when unordered, NE true.

clz is CLZ, ctz CLZ of the bits reversed (RBIT), popcnt the sum of each
byte's CNT (CNT, ADDV). An f32 in an FP register has the rest of the
register clear, as every instruction that writes its S view leaves it, so it
is stored and loaded as a word, as an integer is.

## Refusals

A function is refused whole when it has:

| Reason | What |
|---|---|
| `simd` | a v128 value anywhere -- local, parameter, result, block type, callee's signature, global -- or any SIMD instruction |
| `references` | a reference value anywhere, or ref.null, ref.is_null, ref.func, ref.as_non_null, ref.eq, br_on_null, br_on_non_null, call_ref, return_call_ref, table.get, table.set, table.grow, table.fill |
| `gc` | any GC instruction |
| `exceptions` | try_table, throw, throw_ref, and the legacy try, catch, catch_all, delegate, rethrow |
| `atomics` | the struct and array atomic instructions |
| `asmjs` | V8's asm.js opcodes |
| `f16` | f32.load_f16, f32.store_f16 |
| `cpu` | popcnt without POPCNT, rounding without SSE4.1 (amd64) |
| `frame` | an operand stack deeper than the interpreter's frame for it |
| `size` | code past `MaxCodeBytes`, a megabyte |
| `internal` | a panic of the compiler's |
| `unplaced` | code the arena could not place |

`engine.ReadJITStats` counts compiles and refusals by reason; `TestSpec`
logs them when `WASM_JIT` is set.

## Concurrency and memory

A Code's native code is shared by its instances and the goroutines running
them. It lives in an arena of the Code's own, released by a cleanup when
the Code is collected. Placing code makes pages of the arena writable, so
native code runs under a read lock of the Code's (`codeJIT.run`), and
placing takes the write lock: no thread runs code in pages being written.
Compiles are serialized per Code. The link table is written atomically,
and read by native code.

## References and the collector

Compiled code holds no references. Native code reads and writes only words
that are not Go pointers: slots, the context, the return stack, globals'
numbers, linear memory. When references come to native code, they will be
stored only through the write barrier's routine while the collector marks,
as the JavaScript JIT stores them (`internal/jit/README.md`, "The write
barrier").

## Validation

```sh
WASM_SPEC_DIR=/path/to/spec WASM_THREADS_DIR=/path/to/threads WASM_JIT=eager \
  go test -tags quickjs_jit ./wasm/internal/spectest -count=1 -run TestSpec -v
WASM_SPEC_DIR=/path/to/spec WASM_THREADS_DIR=/path/to/threads WASM_JIT=osr \
  go test -tags quickjs_jit ./wasm/internal/spectest -count=1 -run TestSpec -v
WASM_SPEC_DIR=/path/to/spec WASM_THREADS_DIR=/path/to/threads go test -tags quickjs_jit ./wasm/internal/spectest -run TestJITDifferential -v
go test -tags quickjs_jit ./wasm/internal/engine -run 'JIT|OSR'
(cd internal/jit/verify && go test -run 'TestAMD64|TestARM64' -tags quickjs_jit .)
```

`TestJITDifferential` runs every spec module that links with the spectest
module alone -- the threads proposal's too, with `WASM_THREADS_DIR` -- both
interpreted and compiled at its first call, calls each
numeric export with the scripts' constants, each type's edges and random
values, and compares the results (any NaN for a NaN), the traps, and the
memories and globals after every call; then again with every call of a
function with loops entering its native code at its first back edge
(`osr`). The spec runs log how many calls went on natively from a loop
(`ReadJITStats().OSR`).
