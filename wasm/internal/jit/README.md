# WebAssembly JIT

Native code for WebAssembly functions, built only with the `quickjs_jit` tag
on amd64 (linux and windows) and used only for code that asks for it
(`engine.Code.EnableJIT`, or `WASM_JIT` in tests). Elsewhere -- without the
tag, on 386, on darwin/arm64 until its backend exists -- the engine's
`jit_off.go` makes every hook a constant or an empty type, and the
closure-tree interpreter (`wasm/internal/engine`) runs everything.

The JavaScript and WebAssembly JITs share the lowest layer and nothing else:
the instruction encoders (`internal/jit/asm/amd64`, checked against
`golang.org/x/arch` by `internal/jit/verify`), the executable memory
(`internal/jit.Arena`, `PlaceRaw`, `RawCode.EnterAt`) and the rules of the
execution boundary (`internal/jit/README.md`, "The execution boundary"):
native code never moves SP, never touches BP or R14 (g), never executes a
call instruction, and is never a safe point. Each JIT has its own front end,
tiers and Go entry points.

This package compiles; the engine places, enters and handles exits:

| File | |
|---|---|
| `ctx.go` | the context block and the exits: the contract between the two |
| `scan.go` | the refusals: what the compiler leaves to the interpreter |
| `reader.go` | the interpreter's body reader, which this package cannot import |
| `compile_amd64.go` | control flow, calls, exits, traps |
| `stack_amd64.go` | the operand stack and its register cache |
| `ops_amd64.go` | the numeric and memory instructions |
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

There is no entry into a loop already running (OSR): a function called once
that loops for long runs interpreted. An optimizing tier, later, would build
SSA from the body and replace the baseline code of the hottest functions.

The tiers share the interpreter's frame, so a call may cross between them in
either direction.

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

Native code addresses the frame through BX (`L`).

**Locals.** The most used locals live in registers of their own for the
whole function -- three integers in general registers, four floats in SSE
ones, chosen by their uses, a use in a loop counting eight times one out of
it -- and the rest in their slots. A local's home is the same at every
point of the function, so control merges need nothing of it. A local.get
copies the home into a register of the operand stack's, so that a later
local.set does not change an entry. Before a call and an exit, the homes of
the locals the function ever writes are stored into their slots (the
others' slots hold their values already: a parameter's the caller's, a
declared local's the prologue's zero), and after them every home is
loaded again; the prologue loads them first.

**The operand stack** is a list of entries, each a constant, a register
-- general for integers, SSE for floats -- or its position's slot, or the
flags of a comparison that the very next br_if or if consumes. An
instruction pops its operands, owning their registers, and pushes its
result; when no register is free, the lowest entry holding one is stored
into its slot. Every entry is stored into its slot when control merges --
entering a block, a loop or an if, and so at every branch's target -- and
at a call and an exit, so that at a label the whole stack is in slots, as
in the interpreter, and no register is live.

## Registers

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
| SI, R8, R9, R13, AX, CX, DX | the cache's general registers, AX, CX and DX last (division and shifts take them) |
| X0-X14 | the cache's SSE registers |
| SP, BP, R14 | never written |

R15, R12 and R10 hold the same thing in every native frame of a level: a
native call leaves them as they are, and an exit stores R12 and R10 into
the context and the resumption loads all three again.

## The context

`Ctx` is a Go struct of words, none a Go pointer, one for each level of
native code an exec has entered (`jitLevel`), its address in DI:

- `L`: the running frame's slots, set before entry and stored at every exit.
- `Links`: the Code's link table, each function index's native entry or 0.
- `MemBase`, `MemSize`: memory 0's first byte and size, set before every
  entry; a shared memory's size is read through `MemSizePtr` at each
  access, as other threads grow it. Only Go grows a memory, so the code
  reloads R15 after every exit and reads the size from the context.
- `Globals`: the instance's table of its globals' addresses.
- `ChunkEnd`: the end of the stack chunk the frames are in.
- `RSP`, `RSEnd`: the native return stack, shared by an exec's levels: its
  next entry, which R10 holds in native code and an exit stores, and its
  end -- or, if that is before, the entry at which a call would pass
  `MaxCallDepth`.
- `Poll`: the poll counter, the exec's, which R12 holds in native code:
  copied in at entry and out at every exit.
- The exit record: `Exit`, `Arg0`-`Arg4`, `ExitFn`, `ExitAddr`.

## Entry, exits and resumption

Go enters a function through `RawCode.EnterAt` at its Go entry, which loads
BX, R15, R12 and R10 from the context; native callers jump to its entry, past that.
Each function's code starts with its return stub, which stores `ExitReturn`
and returns. Before entering, Go pushes a return stack entry whose return
address is that stub: the function returns as every frame does, and its
return goes to the stub, and the stub to Go.

Native code never calls. Anything it does not do itself is an exit: it
stores L, the exit's kind and arguments, its own function index and the
address to resume at, and returns to `enterNative`'s loop, which does the
work and enters the code again at that address, as an offset in the
function's code (`RawCode.EnterAt`). At the resume address the code reloads
BX, R15, R12, R10 and the locals' homes. Every entry that lives past an exit
is in its slot.

| Exit | Go |
|---|---|
| `ExitReturn` | the frame Go entered returned |
| `ExitTrap` | panics with `engine.Trap`: the trap's code, its frame's offset |
| `ExitCall` | calls a function -- imported, the host's, without native code, or with no room -- with `callGeneric`, the arguments and results in the frame's slots |
| `ExitCallIndirect` | finds the function as the interpreter does, its traps included, and calls it |
| `ExitTailCall`, `ExitTailIndirect` | a tail call made otherwise than natively: see Calls |
| `ExitPoll` | `pollContext`, the context's cancellation |
| `ExitOp` | memory.size, memory.grow, memory.init, memory.copy, memory.fill, data.drop, table.size, table.init, table.copy, elem.drop, on the frame's slots |

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

A load or a store of memory 0 checks the effective address -- the i32
address, zero-extended, plus the offset, plus the access's size, computed in
64 bits -- against the memory's size, and goes to a trap stub past it. The
access is `[R15 + address + offset]`. Loads and stores of memories other
than 0, and of 64-bit memories, are refused; the bulk instructions of any
memory are exits.

## Polls

Every loop back edge decrements the poll counter and exits when it runs out
(`ExitPoll`), as the interpreter's loops do, and so does every native call
and tail call, for recursion without a loop; Go counts the tail calls it
makes for native code, as the interpreter's `tailChain` counts its own. Native code is not a safe point: the polls
bound how long a goroutine in it delays preemption and a stop-the-world
pause.

## Floats

The numerics are the interpreter's, bit for bit where the spec fixes the
bits:
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

## Refusals

A function is refused whole when it has:

| Reason | What |
|---|---|
| `simd` | a v128 value anywhere -- local, parameter, result, block type, callee's signature, global -- or any SIMD instruction |
| `references` | a reference value anywhere, or ref.null, ref.is_null, ref.func, ref.as_non_null, ref.eq, br_on_null, br_on_non_null, call_ref, return_call_ref, table.get, table.set, table.grow, table.fill |
| `gc` | any GC instruction |
| `exceptions` | try_table, throw, throw_ref, and the legacy try, catch, catch_all, delegate, rethrow |
| `atomics` | any atomic instruction |
| `memory64` | a load or a store of a 64-bit memory |
| `multi-memory` | a load or a store of a memory other than 0 |
| `asmjs` | V8's asm.js opcodes |
| `f16` | f32.load_f16, f32.store_f16 |
| `cpu` | popcnt without POPCNT, rounding without SSE4.1 |
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
WASM_SPEC_DIR=/path/to/spec go test -tags quickjs_jit ./wasm/internal/spectest -run TestJITDifferential -v
go test -tags quickjs_jit ./wasm/internal/engine -run JIT
(cd internal/jit/verify && go test -tags quickjs_jit -run TestAMD64 .)
```

`TestJITDifferential` runs every spec module that links with the spectest
module alone both interpreted and compiled at its first call, calls each
numeric export with the scripts' constants, each type's edges and random
values, and compares the results (any NaN for a NaN), the traps, and the
memories and globals after every call.
