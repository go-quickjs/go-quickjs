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
| `bulk.go`, `bulk_GOARCH.go` | memory.size, memory.copy, memory.fill, table.size and table.fill |
| `indirect.go`, `indirect_GOARCH.go` | calls through a table or a reference, and their tail calls |
| `refs.go`, `refs_GOARCH.go` | references: frames' reference slots, the reference instructions, the stores of pointer words and the barrier routine |
| `gc.go`, `gc_GOARCH.go` | the GC instructions: fields, elements, i31s, tests and casts |
| `simd.go`, `simd_GOARCH.go` | the SIMD instructions: v128.const, their types and the exit through the interpreter, and a backend's code for them |
| `engine/jit_on.go` | tiering, placement, entry, the exits' handling |
| `opt/` | the optimizing tier (`opt/README.md`) |
| `engine/jit_gc.go` | the GC instructions Go runs for native code |
| `exn.go`, `engine/jit_exn.go` | exceptions: the handlers' records, and Go's search of the native frames for the clause that takes one |

## Tiers

A function is compiled by the **baseline** compiler at its first call
(`DefaultJITThreshold`, 0): one pass over the validated body, emitting a
fixed sequence for each instruction over a register cache of the operand
stack. Its record (`compiled`) is then made from validation's bound of the
frame (`module.FrameBound`, `Code.boundRecord`) -- the slots of the locals
and of the deepest operand stack, the reference slots and the legacy trys'
caught slots -- without the interpreter's compile of the body, which
`Code.interp` makes only when a call of the function first runs in the
interpreter, into the frame the record already has: the bound holds the
frame the interpreter's compile makes (`TestFrameBound`), and the stack
positions' slots are the same in both. Calls from the interpreter or from
native code enter the native code. A function the compiler refuses (see
Refusals) is marked, its body is built, and it stays in the interpreter for
good; so does one whose code cannot be placed. A compiler panic is a
refusal (`internal`), never a crash.

A Code given a threshold above 0 (`EnableJIT(n)`, `WASM_JIT=N`) runs a
function in the interpreter first, its body compiled at its first call and
its frame the interpreter's: `run` counts its calls (`fnJIT.hot`) and past
the threshold the baseline compiles it. The JIT's state of a function
(`fnJIT`) is shared by its records: building the body stores a new record,
with the old one's frame and state, so that a record never changes once
stored but for that state. Concurrent first calls make one record (a
compare-and-swap) and one compile (`codeJIT.mu`, `Code.mu` for bodies).
Without the JIT -- a build without it, a Code that never asked for it (the
Go API's default, a debugger's runtime) -- every function's body is
compiled for the interpreter at its first call, with its own frame.

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
interpreter, its body built, and goes on natively at its first back edge,
and under `WASM_JIT=osr` every call Go or the interpreter makes does, so
that the spec scripts run the loops' entries.

The **optimizing** tier (`opt/README.md`, package `opt`) builds SSA from
the body, optimizes it, allocates registers with a linear scan and lowers
it, on amd64 and arm64, to code that keeps this tier's frames, context, entries,
exits and calls. It is on by default where it is built (`DefaultOptThreshold`,
30,000 calls; `Code.SetOptThreshold`, `WASM_JIT_OPT=N` and `WASM_JIT=opt` set
another threshold, `WASM_JIT_OPT=off` turns it off): a function's baseline
code counts its calls at its entry, and its poll exits, and past the
threshold exits (`ExitTierUp`). The engine queues the function for the
process's compile workers and the call goes on in the baseline code, as
later calls do until the optimizing tier's code is placed and takes the
baseline's place in the link table (`opt/README.md`, In the background);
where GOMAXPROCS is 1, or the Code compiles on the spot
(`Code.SetOptBackground(false)`), the engine compiles it there and the call
enters the new code. Either way the frames that began in the baseline code
run on in it. A function the optimizing tier refuses keeps its baseline
code.

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

A function the compiler refuses is never entered at a loop, nor is a loop
in the body of a try_table or a legacy try, or in a legacy catch: the
interpreter runs those under a recover of their own, which would outlive
the construct while native code ran the function on to its return. The engine narrows what enters a frame from the host -- the
arguments of an `Invoke`, a host function's results -- to the halves native
code keeps clear, which the interpreter alone did not need: a value left in
a slot under a loop is read by native code after the loop's entry.

## Frames

A frame is the interpreter's (see `engine/compile.go`): numeric slots on the
exec's chunked stack, and reference slots of two words each on its chunked
stack of references, which the collector scans; neither moves. Numeric
local k is the numeric slot of its place among the numeric locals, the
parameters first, a v128 taking two -- its low half, then its high half --
and a reference local the reference slot of its place among the reference
locals; stack position h is numeric slot `nlocals +
2h` and reference slot `rlocals + h`, `nlocals` and `rlocals` being the
numbers of numeric and reference locals or of results, whichever is more;
a function's results are left in the slots its parameters would take if
they were of the result types. A frame has the slots the interpreter's
compile of the function counted (`nslots`, `rslots`); the compiler refuses
a function whose stack needs more (`frame`).

Every i32 and f32 in a slot or a general register has its upper half clear,
as the interpreter leaves them, so a slot is read as the interpreter reads
it and copied whole.

Native code addresses the frame through a register of its own (`L`: BX on
amd64, X19 on arm64), and a function that holds references its reference
slots through another (`R`: SI on amd64, X12 on arm64), which the cache
then never hands out. A function that holds none -- the engine's compile
saw no reference among its locals, results and stack entries -- neither
reads nor writes reference slots, and keeps no `R`.

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
loaded again; the prologue loads them first. References never have homes,
and in a function that holds references amd64 gives integer locals two
homes, not three, `R` having taken one of the cache's registers.

**The operand stack** is a list of entries, each a constant, a register
-- general for integers, SSE or FP for floats and vectors, a v128 taking the
whole register -- or its position's slot, or
the flags of a comparison that the very next br_if or if consumes: an
integer comparison's on both, a float comparison's too on arm64. An
instruction pops its operands, owning their registers, and pushes its
result; when no register is free, the lowest entry holding one is stored
into its slot. Every entry is stored into its slot when control merges --
entering a block, a loop or an if, and so at every branch's target -- and
at a call and an exit, so that at a label the whole stack is in slots, as
in the interpreter, and no register is live.

A reference on the operand stack is always in its position's reference
slot: what makes one -- ref.null, ref.func, local.get, global.get,
table.get, a call -- stores it there at once, and what reads one reads the
slot, so no reference is ever in a register past the instruction that
moves it, nor at a call, an exit or a poll.

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
| SI | `R`, the frame's reference slots, in a function that holds references; else the cache's |
| R13, R9, R8 | homes of integer locals, else the cache's |
| X14-X11 | homes of float and v128 locals, else the cache's |
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
| X12 | `R`, the frame's reference slots, in a function that holds references; else the cache's |
| X26, X25, X24, X15, X14, X13 | homes of integer locals, else the cache's |
| V29-V22 | homes of float and v128 locals, else the cache's |
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
- `Globals`: the instance's table of its globals' addresses: a number's
  word, or a reference global's `Ref`.
- `Funcs`, `Tables`: the instance's tables of its functions' and its
  tables' addresses, which ref.func and table.get and table.set read.
- `R`, `RChunkEnd`: the running frame's reference slots and the end of the
  chunk they are in. Go sets `R` at entry; a function that holds references
  moves it past its frame for a native call and back after, and one that
  holds none leaves it as it is.
- `WBSlot`, `WBPtr`, `WBRet`, `Save`, `SaveV`: the barrier routine's
  arguments and register save areas, the float registers' whole in `SaveV`
  (see References and the collector).
- `ChunkEnd`: the end of the stack chunk the frames are in.
- `Inst`, `Frames`, `RFrames`: the address of the instance the code runs
  for, and the Code's tables of each function's frame, set when its native
  code is placed: what a native call through a table or a reference checks
  its callee by (Calls). `Frames` holds the numeric slots' bytes, or, for
  a function that holds references, a number past any chunk's end;
  `RFrames` the numeric slots' bytes in its low word and the reference
  slots' in its high one.
- `RSP`, `RSEnd`: the native return stack, shared by an exec's levels: its
  next entry, which a register holds in native code and an exit stores,
  and its end -- or, if that is before, the entry at which a call would
  pass `MaxCallDepth`.
- `Poll`: the poll counter, the exec's, which a register holds in native
  code: copied in at entry and out at every exit.
- The exit record: `Exit`, `Arg0`-`Arg4`, `ExitFn`, `ExitAddr`, and
  `RArgs`: where it is not 0, the references of the exit's operands and
  results are not in the slots of the positions their numeric slots are,
  but from the reference slot it says (`RArgsBias` above it), one for
  every two numeric slots; the optimizing tier, whose numeric operands go
  through a call area of their own while its references stay in their
  positions' slots, sets it, and the engine takes and clears it when the
  exit's work begins.

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

The exits most sites have -- each loop's and each call's poll, each
call's `ExitCall`, and each call through a table or a reference's
`ExitCallIndirect` or `ExitCallRef` -- are a function's own, shared by its
sites: a site
stores the exit's arguments, leaves where it goes on in the scratch
register and jumps to the function's exit of that kind, which stores it
in the context (`Ctx.Resume`) and exits to resume at the function's shared
resumption; that reloads the registers and the homes and jumps to
`Ctx.Resume`. A site so takes some 12 bytes for a poll and 45 for a call's
exit, where its own exit and resumption took about a hundred each, a sixth
of a Go program's code.

| Exit | Go |
|---|---|
| `ExitReturn` | the frame Go entered -- at its start or at a loop's head -- returned |
| `ExitTrap` | panics with `engine.Trap`: the trap's code, its frame's offset |
| `ExitCall` | calls a function -- imported, the host's, without native code, or with no room -- with `callGeneric`, the arguments and results in the frame's slots |
| `ExitCallIndirect` | a call_indirect not made natively (Calls): finds the function as the interpreter does, its traps included, and calls it |
| `ExitTailCall`, `ExitTailIndirect` | a tail call made otherwise than natively: see Calls |
| `ExitPoll` | `pollContext`, the context's cancellation |
| `ExitCallRef` | a call_ref not made natively (Calls): calls the reference's function |
| `ExitTailRef` | a return_call_ref not made natively, as `ExitTailIndirect` |
| `ExitOp` | memory.grow, memory.init, data.drop, table.grow, table.init, table.copy, elem.drop, struct.new, struct.new_default, the array.new instructions, array.fill, array.copy, array.init_data, array.init_elem, throw, throw_ref, rethrow (which do not resume: see Exceptions), memory.atomic.wait32, memory.atomic.wait64, memory.atomic.notify, on the frame's slots; an atomic instruction's offset is the 64-bit `Arg2` |
| `ExitSIMD` | a SIMD instruction the backend does not compile (see SIMD): the interpreter's own code for the instruction at offset `Arg0`, made once and kept by the Code (`codeJIT.simd`), run on the operands in the frame's slots from `Arg1` |
| `ExitTierUp` | the function's count of calls toward the optimizing tier ran out, at its entry (where the request gives a `TierCounter`): the engine compiles it there and enters the optimized code with the frame as the call made it, or resumes the baseline code |

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

A callee that holds references has its reference slots past its caller's
too: a caller that holds references copies the reference arguments to
`R + rslots`, moves the context's `R` there for the call and back after,
and loads its `R` again; one that holds none leaves `R` where it is, its
own reference slots unused. Before the call the caller checks that the
callee's reference slots fit in `R`'s chunk, else the call is an
`ExitCall`. A function that holds references makes every reference slot of
its frame but its parameters' null when it begins, as the engine's frames
are made, so that what the frames hold is what the interpreter's would.

The room a call checks for is the callee's frame as validation bounds it
(`module.FrameBound`, `engine.frameBound`): the most operands its stack
holds after an instruction that can be reached, whether it pushes a
reference, and its legacy trys' nesting, from which the engine sizes a
frame as its compile of the body would. So compiling a caller compiles none
of its callees, for the interpreter or natively. The bound is the frame
for nearly every function -- above it only where code that validation
reads follows a branch, unreachable code the interpreter skips -- and
never below it: a function whose frame would be is refused (`frame`), its
native callers calling it through Go.

The return stack is Go memory, words, one for each exec, grown by Go (every
level's pointers moved) when a native call finds it full and calls through
Go.

A `return_call` of a defined function with native code is made in place:
the arguments into the frame's parameters, the return stack's top entry
renamed, a jump to the callee. Otherwise it exits, and Go makes the call
natively itself if it can (a function of the same instance with native code
and room), else ends the level with the interpreter's tail call (`x.tail`,
`tailChain`) if the frame is the one Go entered, else calls the function
and resumes the frame at its return. A `return_call_indirect` or
`return_call_ref` not made natively (below) exits to the same handling.
The interpreter's `tailChain` enters native code too, so a chain of tail
calls across the tiers does not grow the stack.

Calls of imports and of host functions are exits. A call through a table
(`call_indirect`) or a reference (`call_ref`), and their tail calls, are
made natively (`indirect.go`) where the callee -- the table's element,
within the table and not null, or the reference -- is a function of the
call's type exactly (its canonical index, `Layout.FuncType`), defined by
the instance the code runs for (`Layout.FuncInst` against `Ctx.Inst`),
with native code (the link table at its index, `Layout.FuncIndex`) and
room for its frame (`Ctx.Frames`) and, for the call, an entry of the
return stack. A callee that holds references fails the frame's check,
whose `Frames` entry is past any chunk's end, and is checked again out of
line by both its sizes (`Ctx.RFrames`): its numeric slots' and, past the
caller's `R` -- moved past the caller's frame for a call, as a direct
call's is -- its reference slots'. A null reference traps
(`TrapNullDereference`) at the instruction before the poll is counted,
as Go's search for the callee would. Anything else -- null in a table,
past the table, another type or a subtype, a host function, another
instance's, no native code or no room -- exits (`ExitCallIndirect`,
`ExitCallRef`, `ExitTailIndirect`, `ExitTailRef`), and Go finds the callee
or traps as the interpreter does. Every entry is in its slot, so the checks
have the cache's registers: on amd64 the callee's index waits in DX and
its entry in AX, on arm64 in X4 and X3. The call is then a direct call's,
the return stack entry's third word made of the index the checks found,
and a tail call a direct tail call's, the frame's return stack entry
renamed with it.

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
is `[base + that]`.

memory.size, memory.copy and memory.fill of any memory are the code's own
(`bulk.go`), as are table.size and table.fill; memory.grow and table.grow,
which allocate and charge the budget, memory.init and table.init, which
read a segment, table.copy, data.drop and elem.drop exit (`ExitOp`).
memory.size reads the size an access reads -- a shared memory's where its
grows store it, by a load-acquire on arm64 -- and shifts it; table.size
reads the table's number of elements. A copy, a fill and a table.fill
first store every entry, which frees the cache's registers, then read
their operands from the slots: on amd64 into AX, CX, DX and SI (SI being
R in a function that holds references, loaded again after), on arm64
into X1-X3. Both ends -- the start plus the count, which for an i64
memory or table traps where it carries -- are checked against the size
before a byte is written, as the interpreter checks them, with the
interpreter's trap. Then on amd64 a fill is REP STOSB and a copy REP
MOVSB, DI, the context, waiting in R11 meanwhile; on arm64 they loop over
blocks of 32 bytes in Q31 and Q30 (LDP and STP, the address moving past
each block), then of 8 in X16, then bytes, a fill's byte DUPed into each
of Q31's. A copy within one memory to a higher address that overlaps its
source goes from its end down -- on amd64 words then bytes, on arm64 the
same blocks pre-indexed -- each read before the bytes above it are
written. table.fill stores its reference into each element as any
reference store is, through the barrier routine while the collector marks
(References and the collector), which keeps the loop's registers. None
makes an exit or a poll: the count is bounded by the memory or the table,
as Go's copy of it in the interpreter is.

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

The numerics are the interpreter's, bit for bit, NaNs included. The spec
lets an operation with NaN operands return any of their NaNs, and the
interpreter does not leave the choice to the order Go puts an operator's
operands in: it picks by a rule (`engine/nan.go`), the one the host's
instructions follow when their operands are in the operation's order, so
that native code emits the operation's instruction and nothing more:

- add, sub, mul, div, min and max, scalar and lane by lane, return one of
  their NaN operands quieted. On amd64 it is the first operand's if that
  is a NaN, else the second's -- SSE's choice, the first source operand
  being the destination; on arm64 a signaling NaN comes ahead of a quiet
  one, and of two of a kind the first operand's -- A64's with the default
  NaN off.
- Of operands that are not NaNs (inf-inf, 0*inf, 0/0) they return the
  host's default NaN: negative on amd64, positive on arm64.
- A fused multiply-add takes, on amd64, the first NaN of the first factor,
  the second and the addend, quieted, nmadd's negation leaving its sign --
  what FMA3 gives; on arm64 FMLA's: a signaling NaN of the addend, the
  first factor (negated for nmadd) or the second ahead of a quiet one, in
  that order, and the default NaN of a quiet addend and an infinity times
  a zero. Unfused, relaxed_madd and nmadd are a multiplication and an
  addition (p + c) or subtraction (c - p), each by the rule.
- sqrt, the roundings, demote and promote return their NaN operand
  quieted; abs, neg and copysign are operations on the sign bit.

`TestNaNRule` holds the interpreter to the rule, and `TestJITFloatBits`
and `TestJITSIMDBits` native code too, of every pair of NaNs in both
orders and every triple for the multiply-adds.

On amd64:
- add, sub, mul, div, sqrt, and the conversions between f32 and f64: the
  SSE instruction, its first operand the destination.
- ceil, floor, trunc, nearest: ROUNDSS and ROUNDSD, which quiet a NaN as the
  interpreter's `a + a` does; SSE4.1 is checked (`x/sys/cpu`).
- min, max: a NaN operand gives the sum, the rule's NaN; equal operands the
  OR (min) or AND (max) of their bits, so that -0 is below +0.
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
  FP instruction, its first source the first operand.
- ceil, floor, trunc, nearest: FRINTP, FRINTM, FRINTZ and FRINTN, which
  quiet a NaN as `a + a` does.
- min, max: FMIN and FMAX. Their NaN is the one FADD of the same operands
  gives, the rule's; and they order -0 below +0, as the interpreter's OR
  and AND of equal operands' bits do.
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

## SIMD

A v128 lives where a float does: in an XMM register (amd64) or a V
register (arm64) of the cache or of a local's home, the whole register, or
in the two slots of its stack position or its local. A constant entry
carries both halves, and a constant loaded into a register comes from the
code's pool of them, after the code (`vconst`), RIP-relative on amd64 and
PC-relative (LDR literal) on arm64; zeros are made. A v128 moves between
slots, frames and globals 16 bytes at a time (MOVDQU, LDR and STR Q);
a call copies a v128 argument or result as two slots.

The loads and stores, of every memory, are the scalar ones' address and
bounds check (see Memory) with the access's width: v128.load and store
are MOVDQU and LDR/STR Q at the base plus the address; the extending
loads read 8 bytes and widen (PMOVSX, PMOVZX; SXTL, UXTL); the splatting
loads, the loads of a lane and the stores of one go through a general
register (PINSR and PEXTR; INS and UMOV).

On amd64 the code uses SSE2, SSSE3, SSE4.1 and SSE4.2, none VEX-encoded:
a function with a v128 or a SIMD instruction is refused (`cpu`) on a CPU
without all three. arm64's ASIMD is every A64 processor's.

A SIMD instruction the backend does not compile is an exit (`ExitSIMD`)
that runs the interpreter's own code for it: the engine compiles the one
instruction as the interpreter compiles it, over operands read from the
frame's slots, and keeps that code by the instruction's offset, so that its
result is the interpreter's by construction. The backends compile natively:

| | |
|---|---|
| v128.const, the loads and stores | all |
| splat, extract_lane, replace_lane | all |
| bitwise | not, and, andnot, or, xor, bitselect, any_true |
| float lanes | add, sub, mul, div, sqrt, abs, neg, min, max, pmin, pmax, ceil, floor, trunc, nearest, the comparisons |
| conversions | convert, trunc_sat, demote, promote |
| integer lanes | add, sub, saturating add and sub, mul, neg, abs, min, max, avgr_u, popcnt, all_true, bitmask, shl, shr_s, shr_u, the comparisons, narrow, extend, extmul, extadd_pairwise, dot_i16x8_s, q15mulr_sat_s, swizzle, shuffle |
| relaxed SIMD | all but relaxed_madd and relaxed_nmadd where they fuse on amd64 (below) |

The exit runs what is left: relaxed_madd and relaxed_nmadd on amd64 when
the CPU has FMA3 and AVX.

Where the instruction sets have no one instruction for an operation, the
code composes it. On amd64: i64x2.mul from PMULUDQ's products of 32-bit
halves; the byte shifts as 16-bit shifts masked, or, by a count not known,
the bytes widened to 16-bit lanes, shifted and narrowed; i64x2.shr_s as
`(x >>u n ^ m) - m`, m the sign bit shifted alike; i64x2.abs by the sign
spread through each lane; the unsigned comparisons by PMAXU/PMINU and
equality; ne, le and ge by complementing the other comparison; popcnt by
PSHUFB from a table of each nibble's count; q15mulr_sat_s as PMULHRSW with
its one result past the range, 0x8000, complemented; swizzle by PSHUFB of
the indexes saturated past 15 to ones with their top bit set; shuffle as
PSHUFB of each operand, the other's lanes selecting 0, ORed. On arm64:
i64x2.mul lane by lane in general registers; shifts by SSHL and USHL of
the count, negated to shift right; bitmask by each negative lane's bit,
summed; shuffle by TBL of a two-register table, the operands copied into
V30 and V31.

The float lanes are the scalar operations' lane by lane (see Floats): the
packed forms of the scalar instructions, whose NaNs and zeros are the
rule's in each lane. abs and neg are masks of the sign bits. On amd64 min
and max are MINPS and MAXPS both ways round, ORed (min) or ANDed (max) so
that -0 is below +0, and the lanes' sum, the rule's NaN, where a NaN is;
pmin and pmax are MINPS and MAXPS of the second operand and the first;
the roundings ROUNDPS and ROUNDPD. The unsigned conversion of i32 lanes
to f32 converts the high 16 bits, scaled exactly, and the low 16, and
rounds their sum once; to f64, each lane is the low word of 2**52 + u, less
2**52. trunc_sat of f32 lanes makes NaN 0 and complements CVTTPS2DQ's
0x80000000 where a lane is at or past 2**31; unsigned, the lanes at or past
2**31 are converted less 2**31 with the top bit set after, those at or past
2**32 all ones; of f64 lanes, clamped first, the unsigned ones by the low
word of 2**52 plus the truncated lane. On arm64 every float lane
instruction is ASIMD's own -- FMIN and FMAX for min and max, FCVTZS and
FCVTZU, narrowed by SQXTN and UQXTN, for trunc_sat -- but pmin and pmax,
an FCMGT mask selecting with BSL.

The relaxed instructions give what the interpreter gives on the host
(`engine/relaxed_x86.go`, `relaxed_other.go`), whose choices the backend
reads from the same CPU features (`Features`: FMA3 with AVX, AVX-VNNI,
AVX-VNNI-INT8). On amd64: relaxed_swizzle is PSHUFB alone; the signed
truncations CVTTPS2DQ and CVTTPD2DQ alone, the unsigned ones trunc_sat's;
relaxed_min and relaxed_max MINPS and MAXPS; relaxed_q15mulr_s PMULHRSW;
relaxed_laneselect the top bit of each byte, or of each 32- or 64-bit
lane, of the mask spread through it (PCMPGTB of 0, PSRAD), then a
bitselect, as PBLENDVB, BLENDVPS and BLENDVPD select, without their
implicit XMM0; relaxed_dot_i8x16_i7x16_s PMADDUBSW, and its add, with
AVX-VNNI (unsigned second operand) or AVX-VNNI-INT8 (signed), PMADDWD of
the even and the odd bytes widened to 16 bits, summed, else PMADDUBSW,
PMADDWD of ones and PADDD. relaxed_madd and nmadd unfused are MULPS (MULPD)
of the factors in order, then ADDPS of the product and the addend or SUBPS
of the product from the addend, each operation's first operand its
destination; fused they exit, FMA3 being VEX-encoded. On arm64 every
relaxed instruction is the deterministic one's but relaxed_madd and
nmadd, FMLA into the addend of the factors in order (the interpreter's
single precision is a rounding to odd of its own, which rounds as FMLA
does), nmadd's first factor negated by FNEG first.

`TestJITSIMDBits` (engine) runs every SIMD instruction that is not a load,
a store or a constant, relaxed ones included, on the edges of each lane
shape and on random bits, interpreted and natively, requires the same
bits, and requires the exit for no instruction but those above;
`TestJITRelaxedChoices` (amd64) does it for the relaxed instructions under
each choice amd64's CPUs make, the interpreter's made alike;
`TestJITSIMDValues` holds many v128s live at once, in homes across calls,
through globals, select, blocks, br_table and a loop.

## Refusals

A function is refused whole when it has:

| Reason | What |
|---|---|
| `simd` | struct.get, struct.set, array.get or array.set of a v128 field or element |
| `gc` | a GC instruction past the GC proposal's: custom descriptors, strings, ref.i31_shared |
| `atomics` | the struct and array atomic instructions |
| `asmjs` | V8's asm.js opcodes |
| `f16` | f32.load_f16, f32.store_f16 |
| `cpu` | popcnt without POPCNT, rounding without SSE4.1, a v128 value or a SIMD instruction without SSSE3, SSE4.1 and SSE4.2 (amd64) |
| `frame` | an operand stack deeper than the interpreter's frame for it |
| `size` | code past `MaxCodeBytes`, a megabyte |
| `internal` | a panic of the compiler's |
| `unplaced` | code the arena could not place |

`engine.ReadJITStats` counts compiles and refusals by reason; `TestSpec`
logs them when `WASM_JIT` is set.

## GC objects

The engine's objects (`engine/gc.go`) are read in place, at the offsets
the engine gives (`Layout`): a struct is cells of two words, its header
first, a field's number in its cell's second word, zero-extended, a
reference the whole cell; an array is a header, its length, and its
elements in memory of their own, numbers packed, references two words
each. struct.get, struct.set, array.get, array.set and array.len are
native: the reference's pointer word tested for null (a trap), an array's
index against its length (a trap), the field or element loaded or stored
as its storage type, a reference through the barrier. ref.i31 stores the
i31 mark's address and the value's low 31 bits; i31.get reads the
payload. any.convert_extern and extern.convert_any are nothing.

ref.test, ref.cast, br_on_cast and br_on_cast_fail are native too: null
matches a nullable type; a top type (any, extern, func, exn) any
reference; a bottom type none; i31 the mark; eq an i31, a struct or an
array, and struct and array the header's kind byte; a defined type an
object whose header's runtime information has, at the type's depth among
its supertypes, the type's canonical index -- the depth and the index
known when the code is compiled.

What allocates -- struct.new, struct.new_default, array.new,
array.new_default, array.new_fixed, array.new_data, array.new_elem -- and
array.fill, array.copy, array.init_data and array.init_elem run in Go
(`ExitOp`): the engine's routines make the objects, charging the
instance's budget as the interpreter's do, with the native frames'
reference slots counted among what is live, and leave the result in its
stack position's reference slot.

## Exceptions

throw, throw_ref and rethrow exit to Go (`ExitOp`), which makes the
exception -- or takes the one an exnref holds, or a legacy catch caught --
and throws it as the interpreter does, a Go panic. try_table and the legacy
try compile to blocks, and to records for Go (`Handler`): the offsets of
the body's instructions, the construct's depth among the function's
constructs, and its catch clauses, each with its tag, where its values go
(its label's stack positions) and a landing pad, out of line, which loads
what a resumption loads and branches to the label -- a try_table's
clause's target, or a legacy catch's body. A legacy try ending in a
delegate records its target's depth instead. A function's records are in
the order the bodies end, so that the first holding an instruction is the
innermost construct around it.

Where the Code has catch clauses, Go does the work of every exit under a
recover (`jitExitCatching`) that takes exceptions -- thrown there or out
of what Go called, other levels of native code and the interpreter
included -- to the level's native frames, the innermost first: the exiting
frame at the exit's instruction (not a tail call's, whose frame is gone),
each caller at its call, the offset its callee's return stack entry
records -- each by the records of the code it is in, its function's or a
code that one replaced (`nativeFn.holding` of its return address, or of
the exit's), so that a frame of baseline code the optimizing tier's
replaced catches by the baseline code's. The first handler around the
offset whose clause takes the exception's tag wins, a delegate letting it
pass the handlers of the constructs within its target. Go drops the
frames above that one (the return stack's top, the context's L and R, R
walked up from the frame Go entered), stores the exception's values --
the references in the reference slots of the clause's positions, the
numbers from the numeric slot the clause names (`Clause.Vals`: the
positions' own here, the call area in the optimizing tier's code) --
the exnref after them for a `_ref` clause, and in the reference slot of
its try's depth for a legacy catch, which rethrow reads -- and enters the
pad. An exception no frame takes goes on out of the
level. Every entry beneath a construct and every local is in its slot at a
call and at an exit, so a pad finds the frame as the label wants it. The
engine's frame of a function with legacy trys has a reference slot past
the interpreter's for each depth of them (`Request.CaughtSlot`).

## Compile cost

A compile takes a workspace from a pool (`workspace`, compile.go): the
assembler's buffers, the compiler's lists and tables, and its constructs,
kept from one compile to the next, so that a compile allocates little but
its code's copy, its handlers and its loops' entries. What is emitted after
the body -- the slow paths of calls, polls and barriers, the trap sites'
stubs and each trap code's common stub -- is a list of items in the order
they were asked for, a trap's kept as data rather than a closure, as most
memory accesses have one.

`TestCompileCost` (engine) times what a module costs before its code runs,
phase by phase, over a corpus of .wasm files and the spec's modules
(`WASM_COMPILE_COST`), and `WASM_COMPILE_HASHES` writes a hash of each
function's baseline code; a change meant to make compiles cheaper leaves
the code as it was, which two builds' files show. `TestFirstRun` (wasi)
breaks a WASI program's first run down by phase.

```sh
WASM_SPEC_DIR=/path/to/spec WASM_COMPILE_COST=prog.wasm,spec WASM_COMPILE_HASHES=/tmp/a.txt \
  go test -tags quickjs_jit ./wasm/internal/engine -run TestCompileCost -count=1 -v
WASM_FIRSTRUN=prog.wasm WASM_FIRSTRUN_ARGS="json 200" \
  go test -tags quickjs_jit ./wasm/wasi -run TestFirstRun -count=1 -v
```

## Concurrency and memory

A Code's native code is shared by its instances and the goroutines running
them. It lives in an arena of the Code's own (`internal/jit`'s reusing
arena, `NewReusingArena`), released by a cleanup when the Code is
collected. Placing code makes pages of the arena writable, and
no thread runs code in pages being written, in one of two ways. A compile
made by a goroutine running the Code -- a function's first, a tier-up on
the spot -- places its code after the last placed, perhaps on a page
holding code that runs: native code runs under a read lock of the Code's
(`codeJIT.run`), and such a placement takes the write lock. A compile in
the background places its code apart (`internal/jit.PlaceRawApart`): from a
page boundary past every code placed in the arena's last chunk, or in a
new chunk, on pages no code is in and none ever ran from, which it seals
executable before the code is published; it takes no lock running code
holds, and so waits for none. The arena's own lock orders the two, so that
a placement on the spot that shares a page with code placed apart writes
it once that code is sealed, and under the write lock. Compiles are
serialized per Code (`codeJIT.mu`), but those in the background, which
take it only to publish. The link table is written atomically, and read by
native code.

What the JIT holds for a Code -- each code's bytes in the arena, its record
(`nativeFn` and its loops' entries and catch clauses), the SIMD
instructions' Go code, and the link, frames and counters tables -- is
charged to a budget (`engine.JITBudget`), which several Codes may share:
a `wasm.Runtime`'s (`WithJITMemoryLimit`, 64 MiB by default) or a
JavaScript runtime's (`quickjs.WithJITMemoryLimit`); `Code.JITMemory`
reports it. A compile takes room in the budget before it places its code
(`codeJIT.reserve`, `engine/jit_mem.go`). Where there is none, a pass over
the Code's functions -- at most one each epoch of the JIT's clock, 50 ms,
and then over the other Codes of the budget that are not compiling --
makes it:

- code the optimizing tier replaced, or a loop's entry was compiled for
  (`loopsFor`), that no frame is in is dropped;
- a function whose code no frame is in has its link cleared: its next call,
  native code's or Go's, comes through Go (`ExitCall` and `runNative`),
  which notes the call's epoch and links the code again;
- a function whose link a pass two epochs before cleared, and that has not
  been called since, is dropped, the least recently called first, until
  the room is made; its link stays clear, and its next call compiles it
  again as its first did.

Where that makes no room the compile is refused (`budget`, `engine.RefuseBudget`): the function
runs in the interpreter, its body compiled for it then, until a call in the
next epoch, or a loop's OSR check a threshold of back edges on, compiles
it; a tier-up refused so tries again after as many calls again, and is not
queued for a worker meanwhile. A compile in the background takes its room
when it is placed, the batch's codes each, under the compile lock, from a
worker's goroutine; one that finds none is refused so, and one finished
for a Code given back gives back its room. Before
compiling, a compile takes room for the least it could need -- a byte of
code for each byte of body, or what the tier's code needed when it last
found no room -- so that one bound to be refused is mostly not made again.

No frame can be in code a pass drops. The pass holds the run lock for
writing, so every native frame of the Code is in a level (`jitLevel`) that
exited to Go, and goes on at an address the level holds: its frames'
return addresses on the exec's return stack, and its exit's address. The
pass reads every level of the Code's in every exec (`jitExecs`, which holds
the execs made, weakly), and keeps each code holding one of those
addresses with its function's current code. An exec keeps the levels of
each Code it runs in a chain of their own (`codeLevels`, each level linked
to the one of the same Code's below it), and changes a Code's chain --
beginning a level, ending one by its return, a tail call the interpreter
makes or a panic out of it -- only holding that Code's run lock for
reading; a level's exit address and its frames' entries are written
holding it too, by its native code or by Go entering, resuming or catching
in it. The rest the pass reads -- the exec's list of chains, which it lets
go of those with no level running from once it has 16, and where the
return stack is, which a level of any Code's may move -- the exec changes
holding a lock of its own (`jitExec.mu`), which the pass takes as it reads
the exec. The write lock waits for every reader, so each of those writes
is done before the pass reads, or waits for it to end: what the pass reads
is every level of the Code's, as it is. Entering native code and
returning from it take no lock but the run lock, and no atomic write. Go
enters code -- a call, a loop's entry, a tier-up, a tail call made
natively, a catch's landing pad -- only after taking the read lock and
checking that the code was not dropped; where it was, the call runs in
the interpreter, or goes on as it would without the code. A compile in
the background publishes its code under the compile lock, which a pass
holds, so the two never overlap: a function whose code a pass dropped
while its optimizing compile was queued or running gets the optimized
code when it is published, as its only code. A dropped code's space goes
back to the arena, which places later code there -- a placement on the
spot, under the write lock, never one apart, which takes only pages past
every code placed -- and unmaps a chunk once nothing in it is held.

`WASM_JIT_MEMLIMIT`, read when the engine starts, gives each Code a budget
of its own of that many bytes, for tests, and `WASM_JIT_CLOCK=step` makes
the clock advance at each reading, so that every compile that finds its
budget full makes a pass and every function not called since the last is
old: the spec suite and `TestJITDifferential` run so (Validation).

## References and the collector

A reference is two words, a pointer and a payload (an i31's value), and
native code keeps them only in Go memory the collector sees: frames'
reference slots, globals' `Ref`s, tables' elements. Every store of a
pointer word there -- a slot's, a table element's, a global's, null
included -- tests the runtime's write-barrier flag (`internal/jit`'s
`WriteBarrier`, the address compiled in): clear, it is a plain store; set,
the collector marks, and the store goes to the function's barrier routine,
out of line, the slot's address and the pointer in the context's `WBSlot`
and `WBPtr` and where to go on in a scratch register. The payload word is
always a plain store. The routine returns at once for a nil stored over a
nil, which has nothing to write or to shade; else it saves every register
the code may keep a value in into the context's `Save` -- the float
registers whole, 16 bytes each, into `SaveV`, since a v128 may be in one --
jumps to
`internal/jit`'s `StoreRef` -- to the Go runtime a function the Go function
that entered the code called, which stores with Go's barrier, shading the
pointer it overwrites and the one it writes -- with the address in AX (X15)
and the pointer in CX (X17), restores the registers and goes on. Native
code so does the same work whether the collector marks or not: it never
leaves for the marking's sake, never calls Go but through `StoreRef`, and
its exits are the same. The flag cannot change between the test and the
store: Go sets it only with the world stopped, which native code, never a
safe point, does not let happen. No reference is in a register across an
exit, a poll or a call; a pointer is in one only between its load from Go
memory and its store into Go memory, within one instruction's code.

`StoreRef` is built wherever the WebAssembly JIT is, with the JavaScript
JIT.

`WASM_JIT_BARRIER=marking`, read when the engine starts, compiles code
that sees the collector always marking, so that every store takes the
routine; `clobber` stores through `StoreRefClobber`, which first fills
every register native code may keep with junk, so that a register the
routine does not save goes wrong. `TestJITSameWorkWhileMarking` holds
native code to the same results and the same exits idle and marking, and
`TestJITReferencesUnderCollection` moves references while the collector
runs, again under `GODEBUG=gccheckmark=1`.

## Validation

```sh
WASM_SPEC_DIR=/path/to/spec WASM_THREADS_DIR=/path/to/threads WASM_JIT=eager \
  go test -tags quickjs_jit ./wasm/internal/spectest -count=1 -run TestSpec -v
WASM_SPEC_DIR=/path/to/spec WASM_THREADS_DIR=/path/to/threads WASM_JIT=osr \
  go test -tags quickjs_jit ./wasm/internal/spectest -count=1 -run TestSpec -v
WASM_SPEC_DIR=/path/to/spec WASM_THREADS_DIR=/path/to/threads WASM_JIT=eager WASM_JIT_BARRIER=clobber \
  go test -tags quickjs_jit ./wasm/internal/spectest -count=1 -run TestSpec -v
WASM_SPEC_DIR=/path/to/spec WASM_THREADS_DIR=/path/to/threads go test -tags quickjs_jit ./wasm/internal/spectest -run TestJITDifferential -v
WASM_SPEC_DIR=/path/to/spec WASM_THREADS_DIR=/path/to/threads WASM_JIT=eager WASM_JIT_MEMLIMIT=8192 WASM_JIT_CLOCK=step \
  go test -tags quickjs_jit ./wasm/internal/spectest -count=1 -run 'TestSpec$|TestJITDifferential' -v
go test -tags quickjs_jit ./wasm/internal/engine -run 'JIT|OSR'
(cd internal/jit/verify && go test -run 'TestAMD64|TestARM64' -tags quickjs_jit .)
```

`WASM_JIT_CODE=FILE` writes each code the engine places, with the
compiler's marks, for `jitprof` to read a CPU profile's native samples by
(`opt/README.md`, Validation); the baseline's code has no marks of its
own, its samples counted by function and instruction.

`TestJITDifferential` runs every spec module that links with the spectest
module alone -- the threads proposal's too, with `WASM_THREADS_DIR` -- both
interpreted and compiled at its first call, calls each
export whose parameters are numbers, vectors or nullable references with
the scripts' constants, each type's edges -- for a v128, vectors of each
lane shape's -- and random values, null and i31s, and compares the results
(any NaN for a scalar NaN, a v128's bits exactly, references by what they
are),
the traps, and the memories and globals after every call; then again with every call of a
function with loops entering its native code at its first back edge
(`osr`). The spec runs log how many calls went on natively from a loop
(`ReadJITStats().OSR`), and under a memory limit how many functions' code
was dropped (`Dropped`). Under one, compiles are refused and code dropped
by design, so the engine's tests that check a function was compiled fail
there; its results are what they check, as the spec's and the
differential's are. `TestJITDropKeepsFrames`, `TestJITDropKeepsCatchingFrames`
and `TestJITDropConcurrent` drop code below suspended frames and on many
goroutines at once; run them with the race detector after a change to what
drops code or enters it.
