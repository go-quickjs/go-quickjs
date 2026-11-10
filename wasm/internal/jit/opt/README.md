# The optimizing tier

The WebAssembly JIT's second tier (`wasm/internal/jit/README.md` is the
first's, the baseline's, and the rules both keep). It builds SSA from a
validated function body, optimizes it, allocates registers over it with a
linear scan and lowers it to native code that keeps the baseline's frame,
context, entries, exits and calls, so that its code takes the baseline
code's place in the link table and the tiers -- and the interpreter --
call each other in every direction.

It lowers to amd64 and arm64, wherever the baseline runs. It is on by
default (see Tier-up).

| File | |
|---|---|
| `ir.go` | the IR: types, operations, values, blocks |
| `build.go` | what the tier refuses, and the SSA builder |
| `cfg.go` | the control flow graph: reachability, order, critical edges, dominators, natural loops, loop depths |
| `passes.go` | the optimizations |
| `thread.go` | jump threading, and the lookups that repair SSA after it |
| `verify.go` | the IR's verifier, and what lowering requires of it |
| `compile.go` | `Compile`: build, optimize, lower |
| `workspace.go`, `arena.go`, `table.go` | what a compile works in, kept for the next (The compile's cost) |
| `timing.go` | the compiles' phases, timed for a measuring host |
| `lower.go` | the lowering's part both architectures share: the allocator's view of the function, where each value is, the listing |
| `lower_amd64.go`, `lower_arm64.go` | the lowering: each architecture's code |
| `refs_amd64.go`, `refs_arm64.go` | the references: the reference instructions, the barrier routine, a call's references, the tail calls |
| `memory_amd64.go`, `memory_arm64.go` | the accesses of any memory at any offset: memories other than 0, shared ones, those of i64 addresses |
| `internal/jit/lsra` | the register allocator, which knows nothing of WebAssembly |

## The IR

A `Func` is blocks of `Value`s. A value has an operation, a type -- i32,
i64, f32, f64, a call's tuple, or none -- its arguments, two immediates
(`Aux`, `Aux2`) and, for what traps or calls, the offset in the module of
its instruction (`At`), which a trap reports and a return stack entry
records. A block has its phis apart from its other values, and ends one
of five ways: a jump, an if on an i32, a switch (br_table: the distinct
targets as successors, a table of them by index, the last for an index
past the others), a return of values, or a trap (unreachable).

The operations are WebAssembly's numeric ones, one for each operation of
both widths (a comparison's width is its argument's), `select`, loads and
stores of any memory (their width, sign extension, offset, their memory --
its index, and whether it is shared and of i64 addresses, in `Aux2` -- and
`NoCheck` once a dominating access checked as far), the globals -- `global.addr`,
the address of a global's value, made once in the entry block, which
`global.get` and `global.set` read and write through -- calls of a
function of the module (`call`), through a table (`call_indirect`) and
through a reference (`call_ref`), whose numeric results `result` values
read, `goop`, an instruction Go runs (memory.size, memory.grow,
memory.copy, memory.fill, memory.init, data.drop, table.init, table.copy,
elem.drop, table.grow, table.fill, memory.atomic.wait32, wait64 and
notify; `goops.go`) -- but memory.copy and memory.fill of memory 0
where it is neither shared nor of i64 addresses, which the code does
itself and which are not calls (`InCode`, `BulkInCode`) --
`ref`, a reference instruction (below), the tail calls (`return_call`,
`return_call_indirect`, `return_call_ref`), the last value of a block that
ends with them (a sixth way, `tail`), and the atomic accesses -- loads,
stores, the read-modify-writes and compare-and-exchange, each of a memory,
an offset and a width -- and `fence` (`atomics.go`).

References are never values. A reference is in a reference slot of the
frame, as the baseline keeps it (its README's Frames): a reference local
in its own, a reference on the operand stack in its position's, at every
point. A `ref` value names the slots it reads and writes -- a copy from
one to another, a null or a function made in one, a global or a table's
element copied to or from one, the null test and `ref.eq` of slots (an i32
value), `ref.as_non_null`'s trap, `select`'s copy where its condition is
0 -- and has its numeric operands (a table's index, `select`'s condition)
as arguments; table.size is one too. Every `ref` value has an effect: none
is shared, moved or removed, and they keep their order among themselves
and the calls, which read and write the same slots. A call's reference
arguments and results are in its arguments' positions' slots, from the
position its value records (`Pos`); its `Args` are its numbers. So are
table.grow's and table.fill's, which Go runs: the reference in its
position's slot, `Pos` the first operand's position.

An operation is pure -- no effect, no trap, so shared, moved and removed
at will -- or not: a division may trap unless its divisor is a constant
other than 0 (and, signed, -1); a truncation of a float may trap; a load
traps past the memory's end; a mutable global's read depends on the stores
before it; stores, global.set, calls, the atomic accesses and the fence
have effects. An atomic access is never shared, moved or removed, keeps
its order among the other effects, and is neither checked by nor taken as
checked by BCE; the builder forgets what it knew of memory at each, as at
a fence, and leaves memory.atomic.wait32, wait64 and notify to Go.

## Building SSA

`check` first refuses a function with anything the tier does not build
(see Refusals), so that the builder meets only what it builds.

The builder reads the body once, as Braun et al.'s "Simple and Efficient
Construction of Static Single Assignment Form" does: each local is a
variable, its value in a block the last one written there, else looked up
through the block's predecessors, a phi made where they are several. A
block is sealed once all its predecessors are known, and a lookup in a
block not yet sealed makes a phi completed at the seal. WebAssembly's
structured control knows when that is: a loop's header is sealed at the
loop's end, every back edge being inside the loop; the block after a
`block` or an `if` -- the construct's label, where its branches go -- at
its end; every other block when it is made.

The operand stack is a stack of values, and of a mark for each reference,
which is in its position's slot. A label's values are phis made with the
label: a loop's parameters in its header, the results of anything else in
the block after it, each edge to the label appending its values -- but
its references, which an edge copies from their positions' slots into the
label's, where the two differ: in the block a branch leaves, or, for a
conditional branch, in a block of its own on the edge. A return copies
its references into the result slots. A `br_if` ends its block with an if; `br_table` with a switch;
`br`, `return` and `unreachable` end it and the rest of the construct is
skipped, as the baseline skips it. A branch to the function's body is a
return block. The code after a loop begins a block of its own, and a
construct whose end nothing reaches makes the rest of its parent dead.

Within a block the builder knows what a global holds -- the last value
read or written -- and what memory holds: the last loads, and what a store
of a whole value wrote, a later load of the same memory, address value,
offset, width and type reading it. A store forgets what it may overlap:
accesses of other memories, accesses at one address value whose bytes are
disjoint, and accesses at constant addresses whose bytes are disjoint,
survive it. A shared memory's loads are not known: another thread may
store between two. A call and an instruction Go runs forget it all.

The phis a lookup made that turn out trivial -- every argument one value,
or the phi itself -- are removed by the first pass.

## The passes

`Optimize` runs, in order:

| Pass | |
|---|---|
| `copyelim` | copy propagation: each use of a copy, and of a trivial phi, becomes the value copied |
| `gvn` | global value numbering: a pure value a dominating one computes the same way -- operation, type, arguments (commutative ones in order), immediates -- becomes a copy of it, by a walk of the dominator tree |
| `fold` | constant folding and identities (x+0, x*1, x&-1, x-x, x==x ...), the comparisons' inversion under eqz, `i32.wrap_i64` of a 64-bit operation of extended i32s and constants narrowed to the 32-bit operation, select on a constant or of one value, and the branches and switches on constants, whose dead blocks go |
| `thread` | jump threading (below), then `copyelim` |
| `gvn` again, `copyelim` | |
| `licm` | the pure values of a loop that makes no call whose arguments are defined outside it go to its preheader -- its header's one predecessor outside it; a loop threading made has others, and keeps its values -- inner loops first; a value hoisted out of a loop that calls would live across the calls, kept in a slot, where computing it again costs less |
| `dce` | values without uses or effects, and phis only themselves use |
| `bce` | bounds-check elimination: an access is in bounds if a dominating access of the same memory and address value checked an end as far; a memory never shrinks, so the check holds across calls and grows; a shared memory's accesses each check its size |
| `split` | every critical edge gets a block, where its phis' moves go |
| `loops` | natural loops, and each block's depth (below) |

### Jump threading

Go's code for WebAssembly runs every loop of a function through one loop
whose header switches (br_table) on a local -- the block to run next --
that each of its back edges sets to a constant. `thread` takes such edges
straight to their cases: where a block H ends with an if or a switch on a
value its phis decide -- a phi, or H's pure values of its phis, which
nothing outside H uses -- and the arguments an edge from a predecessor p
brings make it a constant, p's edge goes to the successor H would go to.
The target's phis read on the new edge what they read on H's.

H's phis then no longer hold on every path to the blocks H dominated.
Each is a variable, defined by H and on each threaded edge by the
argument p brought there (another of H's phis being that variable's value
at p's end; p's other edges define nothing), and every use of it is
looked up again as the builder looks up a local: a use's value at its
block's start, a phi's argument on its edge, a phi made where
predecessors bring different values, and one that turns out trivial
forwarded to the value it is (`repairPhis`). Nothing is copied: no block
is duplicated, and the code does not grow.

The cases' loops are loops after it, but irreducible ones -- the switch
still enters every case, for the calls that resume one -- which natural
loops do not see. A block's depth, which the allocator weighs its uses by,
is the number of loops of a nesting forest as Steensgaard's it is in
(`loopDepths`): a loop is a strongly connected component, its entries the
blocks an edge from outside reaches -- but a switch's of more than two
targets, which enters a loop only where nothing else does -- and the loops
within it those of the component without its edges to the entries.

Folding is exact: integers as WebAssembly defines them, a division only
where it cannot trap; floats only where no NaN is involved (add, sub,
mul, div, promote, demote, the exact integer conversions), or on bits
(neg, abs, copysign), so that the interpreter's NaN payloads are never
the folder's to choose.

`Verify` checks the IR's invariants -- edges agree with block kinds and
each other, a phi has an argument for each predecessor, a value's
arguments dominate it, types agree -- and `checkLowerable` what lowering
needs besides: no critical edge, no phi in a block of one predecessor, a
call's results right after it. `TestBuildSpec` builds every function of
the spec scripts the tier accepts and verifies the IR after each pass.

## Frames

The optimized code runs in the interpreter's frame, as the baseline's
does (its README's Frames), with the frame's numeric slots its own but at
the points where something else reads them:

- At entry the parameters are in slots 0 up, as the caller left them.
- At a return the results are in slots 0 up.
- A native call writes the arguments into the callee's frame, at its
  frame's end (`L + 8*nslots`), one slot each, and reads the results
  there; the return stack entry is the baseline's.
- A call through Go -- of an import, of a callee with no native code or no
  room, a `call_indirect` not made natively -- and an instruction Go runs
  find their numeric operands, two slots apart, each at its place among
  the operands, in the call area at the frame's top (from `argBase`,
  `nslots` less twice the most operands or results of any of them), and
  leave their results there; their references stay in their positions'
  slots, where `Ctx.RArgs` says (below).
- A poll's exit keeps every register in the context's save area
  (`Ctx.Save`): nothing in the level runs while Go polls.
- A trap reads nothing of the frame.

Every other slot below `argBase` is the allocator's spill slot. A
parameter, a local and a result have the slots of the engine's frames: a
number's numeric slot is its place among the numbers, a reference's
reference slot its place among the references. A slot's
word holds an i32 or an f32 with its upper half clear, as the
interpreter's frames do, so that what Go reads of the call area is right
and a slot is copied whole: on amd64 an f32 is stored with MOVSS and a
zero upper word; on arm64 an f32 in a register has the rest of the register
clear, as the baseline keeps it -- every instruction that makes one writes
its S view, which clears the rest -- and is stored as its D view.

A function that holds references -- the engine's frame has reference
slots (`Request.FrameRefSlots`) -- makes every reference slot but its
parameters' null when it begins, as the baseline does, and moves the
context's R past its own for a native call (`adv`) and back after: its
callees' references go past its own. Its reference instructions address
the slots from the context's R, loaded into a scratch register for each,
and a pointer word passes through the float scratch register (x0) within
the instruction's code: no reference is in a register the allocator has,
nor across a call, an exit or a poll. Every store of a pointer word tests
the write barrier's flag and, while the collector marks, goes to the
function's barrier routine, out of line, which saves every register the
code may keep a value in -- the allocator's and the ones every native
frame keeps -- around `internal/jit.StoreRef`, as the baseline's does
(its README's References and the collector); the payload word is a plain
store.

A call's reference arguments are copied from their positions' slots into
the callee's parameters past the frame's (adv bytes past R), and its
reference results back after. A call through Go stages its numeric
operands in the call area, each at its place among the signature's
parameters, while its references stay where they are: it sets the
context's `RArgs`, by which the engine reads and writes the references of
the exit's operands in their positions' slots, not in the call area's.
table.grow and table.fill, which Go runs, do the same (`stageGo`).

## Registers

| Register | |
|---|---|
| DI | the context |
| BX | L, the frame's slots |
| R15 | memory 0's base, loaded again after every exit |
| R12 | the poll counter |
| R10 | the return stack's next entry where it arrives and leaves; in the code, `Ctx.RSP` holds it |
| R11, R10 | general scratch registers |
| X15, X14 | float scratch registers |
| SI, R8, R9, R13, AX, CX, DX | the allocator's general registers, AX, CX and DX last (division's and shifts'; memory.copy and memory.fill take AX, CX and SI, an atomic store, read-modify-write and compare-and-exchange AX, CX and DX) |
| X0-X13 | the allocator's float registers |
| SP, BP, R14 | never written |

Native callers enter with R10 the return stack's next entry, which the
entry stores into the context; the return sequence loads it, pops the
entry and jumps to its address with BX the caller's L, as the baseline's
does. After a native call the callee leaves R10 as the call found it, and
the code stores it into the context again, where an exit of the callee
may have left another.

On arm64:

| Register | |
|---|---|
| X0 | the context |
| X19 | L, the frame's slots |
| X20, X21 | memory 0's base and size, loaded again after every exit |
| X22 | the poll counter |
| X23 | the return stack's next entry, kept there throughout, stored at every exit |
| X16, X17 | general scratch registers |
| D31, D30 | float scratch registers; Q31 and Q30, whole, memory.copy's and memory.fill's |
| X27 | the encoder's temporary, and a remainder's quotient |
| X1-X12, X24-X26, X13-X15 | the allocator's general registers, X13-X15 last (memory.copy's and memory.fill's, and an atomic store's, read-modify-write's and compare-and-exchange's) |
| D0-D29 | the allocator's float registers |
| SP, X18, X28, X29, X30 | never written |

These are the baseline's: every register a native frame of a level keeps
-- L, memory 0's, the poll counter, the return stack's -- is in the same
place in both tiers, and a native call leaves them as they are.

## Register allocation

`internal/jit/lsra` is a linear-scan allocator over an abstract function:
blocks of instructions that use and define virtual registers of classes,
phis with an argument per predecessor, an instruction's fixed clobbers
and calls. It knows nothing of WebAssembly or of the target's
instructions; the lowering describes the SSA to it -- a virtual register
for each value but constants, which are made where they are used, a
two-address operation's first operand as its result's hint -- and lowers
with the locations it gives.

A virtual register's lifetime is a list of ranges, from liveness, its
holes left out: the paths from each use back to the definition, where
every register is defined once, as the lowering's are (after Boissinot et
al.'s "Computing Liveness Sets for SSA-Form Programs"), else sweeps of the
blocks until their sets change no more. A phi and an argument whose lifetimes do not meet are
coalesced into a web -- one location, no move on their edge -- the
edges of the deepest loops first (two parameters, arriving in different
slots, never). Each web has one location for its whole life, given by a
linear scan of the webs in order of their starts with active and
inactive webs, as Wimmer and Mössenböck's, so that a web takes a
register in another's hole. Where a class's registers run out, the web
scanned displaces those in its way on the register where they are least
dense -- their weight, their uses at eight times each loop around them,
over the positions they cover -- if they are less dense than it, and is
spilled otherwise; a web of several registers it would spill is taken
apart instead, its registers scanned again each on its own and hinted to
the others'. Density, not weight, decides, so that a loop's short
temporaries keep registers and the long values around them, used less
often for their length, go to slots. A register an instruction clobbers
(on amd64 AX and DX for a division, CX for a shift by a variable, AX, CX
and SI for memory.copy and memory.fill, AX, CX and DX for an atomic store,
read-modify-write and compare-and-exchange; on arm64 X13-X15 for
memory.copy, memory.fill and those atomics) is not given to a web live
across it. A
web in a register that lives across a call is stored into its slot where
each of its values is defined and loaded again after each call it
crosses; one whose reloads would weigh as much as its uses lives in its
slot instead. Slots are given by a second scan over the ranges, holes
included, a web with a parameter keeping the slot it arrives in. A phi
prefers its arguments' registers and they its, and a two-address
operation's result its first operand's. There is no splitting within a
value's life: a web is in its register or its slot throughout.

A phi in a register is stored into its slot where its block begins unless
every argument shares its register and its slot, which each stored
where it was made.

Lowering reads a spilled operand from its slot -- on amd64 as a memory
operand where the instruction takes one (`OpMem`, `ImulMem`), else, and on
arm64 always, into a scratch register -- and computes a spilled result in
a scratch register and stores it. The moves on an edge are parallel: each
waits for those that read its destination, and a cycle is broken through
R10 or X14 (X17 or D30 on arm64).

## Lowering

Each instruction compiles to the baseline's sequence for it where the
result's bits depend on the instruction chosen -- the floats (the
baseline README's Floats: the same SSE instructions, operand orders --
the first operand the destination, for the NaN rule's choice --
roundings, min/max, truncation bounds and unsigned conversions), so that
the results are the interpreter's bit for bit -- with the operands where
the allocator put them. copysign is done on the bits in general
registers. An integer comparison that is the last value of a block and
its branch's condition alone is fused with the branch.

On arm64 the instructions are three-address, and a result is computed in
its register from its operands' wherever they are. The floats are the
baseline's arm64 sequences: FMIN and FMAX, FRINT's roundings, FCVTZS and
FCVTZU after comparisons with the bounds in the operand's type for the
trapping truncations and alone for the saturating ones, SCVTF and UCVTF,
the conditions MI, LS, GT and GE after FCMP, abs, neg and copysign on the
bits in general registers. A division never faults: the zero divisor and
the least integer over -1 trap first, and a remainder is the dividend less
the quotient's product (MSUB), the quotient in X27. A branch on a value not
fused is CBZ or CBNZ, and a switch's table is of branches straight to its
targets, or, for a back edge, to its poll's count. A constant is made where
it is used, the zero register for 0 where the instruction reads its
register forms.

An access of memory 0, unshared and of i32 addresses (`plainAccess`),
computes its end, `address + offset + size`, in R11 and compares it with
the context's memory size, unless `NoCheck`, where the offset plus the
size is within 2^31-1. The access is `[R15 + address + offset]`,
the address in a register (an i32 there has its upper half clear). On
arm64 the address plus the offset is made in X17, in 64 bits, where the
offset is not 0, the end in X16 and compared with X21, and the access is
`[X20 + X17]`.

Any other access (`memory_GOARCH.go`) reads its memory's size where the
baseline reads it (its README's Memory), at each access: memory 0's from
the context (X21 on arm64), a shared one's through the address of its
size, which other threads' grows store, another's from the instance's
table of memories (`Ctx.Mems`), whose address the context holds and whose
entries the engine fills at every entry, a grow by the function, by a
callee or by Go included: nothing of a memory is kept in a register or a
value across anything. An i64 memory's address plus its offset, and an
i32 one's plus an offset past 2^31-1, is made in R10 (X17), in 64 bits, as
the baseline makes it, and an i64 memory's traps where it carries; the check is then that the size
less the rest of the access -- the offset and the size, or the size where
the offset is in the register -- does not borrow and is not below the
address, in R11 (X16), so that no sum carries. A constant address is
checked against a constant end, and one whose sums carry jumps to the
trap. Memory 0's base is R15 (X20). Where the address is in a register of
its own -- the allocator's, or the instruction's -- R10 (X17) takes the
memory's entry in the table once, for the check and the base, which it
ends holding, `[R10 + address + offset]` (`checkFromS1`): on amd64 an i32
address's end is made by LEA and compared with the size in memory, on
arm64 the size less the access's size is compared with the address; R11
(X16) is then left to the instruction. Where the address is in R10
(spilled, or made in 64 bits there), another memory's base is loaded into
R11 (X16) after the check, `[R11 + address + offset]` -- or, for a store
whose integer value R11 must hold, added to the address in R10, the access
`[R10 + offset]` (on arm64 an add, the access `[X17]`). A float store's
value is in X15 (D31) first, a constant being made through R11.

An atomic access is the baseline's sequence for it (its README's Memory),
its alignment checked after an i64 address's carry and before the bounds,
in the interpreter's order. On amd64 a load is a plain load, zero-extended;
a store XCHG, add and sub LOCK XADD (of the negated operand for sub),
exchange XCHG, and, or and xor a loop of LOCK CMPXCHG from the old value
in AX, compare-and-exchange LOCK CMPXCHG with the expected value in AX: the
operands are moved into CX (the address), DX and AX at once, and the
result, its bits above the width cleared, from DX or AX; another memory's
base is in R10. On arm64 the address in memory -- the base plus the
effective address, in X13, or X17 for a load -- is what LDAR, STLR and the
loop of LDAXR and STLXR take, the operand in X14, the old value of a
read-modify-write in X15 and of a compare-and-exchange in X16, the
replacement in X15, the expected value masked to 8 or 16 bits, the
exclusive store's status in X17; the loop holds no other access. A fence
is MFENCE or DMB ISH.

A back edge -- a jump to a block at or before its own in the layout,
reverse postorder -- counts down the poll counter and exits for a poll
when it runs out. On arm64 the exit is the function's own, which its sites
share: a site leaves where to go on in X16 and jumps to it, and it keeps
in the context's save area only the registers the allocator gave out. A call counts it down too, then checks, as the baseline
does, that the callee has native code, that its frame and reference slots
fit their chunks, and that the return stack has room, else calls through
Go (`ExitCall`), out of line.

A `call_indirect` or a `call_ref` counts it down and calls natively too
where the table's element -- within the table, not null -- or the
reference is a function of the call's type exactly (its canonical index,
`Layout.FuncType`), defined by the instance the code runs for
(`Layout.FuncInst` against `Ctx.Inst`), with native code (the link table
at its index, `Layout.FuncIndex`) and room for its frame, which the
engine's table of each function's frame says (`Ctx.Frames`). A function
that holds references has a frame there past any chunk's end, and fails
the check: out of line, both its sizes (`Ctx.RFrames`) are checked, its
reference slots' past the caller's. A call of a type with a reference
parameter or result goes through Go, as its callee holds references. A
null reference traps (`TrapNullDereference`) before the poll is counted.
Anything else -- null in a table, past the table, another type or a
subtype, a host function, another instance's, no native code or no room
-- goes through Go (`ExitCallIndirect`, `ExitCallRef`), which finds the
callee or traps as the interpreter does. The callee's index waits in X15 while the
arguments are moved, as s0 is the moves'; on arm64 the checks have X16
and X17, and keep the table and the callee's index in D30 and D31.

A tail call (`return_call`, and `return_call_indirect` and
`return_call_ref` of a type without references) counts the poll down and
is made in place natively where the callee has native code and room for
its frame from L -- the same checks as a call's, but for the return
stack's -- its reference arguments copied into the frame's parameters'
reference slots, its numeric arguments into the parameters' slots by a
parallel move, the frame's return stack entry renamed for the callee, and
a jump to it with R10 the return stack's next entry. Otherwise it exits
(`ExitTailCall`, `ExitTailIndirect`, `ExitTailRef`) as the baseline's
does, and its resumption returns, the results in their slots.

memory.copy and memory.fill of memory 0 are the code's own: the operands
into AX, CX and SI, which the allocator keeps free of intervals live
across them, both ends checked against the memory's size before a byte
is written, then REP MOVSB or REP STOSB with DI the destination's
address, the context kept in R11 meanwhile; a copy to a higher address
that overlaps its source goes from its end down, words then bytes, each
read before the bytes above it are written. On arm64 the operands go into
X13, X14 and X15, and the code loops over blocks of 32 bytes in Q31 and
Q30 (LDP and STP, the address moving past each block), then of 8 in X16,
then bytes -- a copy to a higher address that overlaps its source from
its end down, each block read before it is written; a fill DUPs its byte
into Q31's. They make no exit and no poll: the count is bounded by the
memory, as Go's copy of it in the engine is.

## Tier-up

The baseline code of a function the optimizing tier may compile counts
its calls: at its entry it decrements the function's countdown
(`Request.TierCounter`, the engine's `codeJIT.counters`) and exits
(`ExitTierUp`) when it goes below zero, before its prologue, with the
frame as the call made it. The engine queues the function for a compile
in the background (In the background) and the call resumes in the
baseline code; once a worker has placed the optimized code, it is stored
as the function's native code and in the link table, and native callers,
the interpreter and Go call it from then on. Compiling on the spot
(GOMAXPROCS 1, `Code.SetOptBackground(false)`), the engine compiles the
function there, places and stores the code, and enters it at its Go entry
for that call. A poll that baseline
code exits for -- its loops or calls ran the poll counter out -- counts
250 calls, so that a function whose loops run long is optimized for its
next call, and each call and instruction Go makes for the baseline code
(`ExitCall`, `ExitCallIndirect`, `ExitOp`) counts one: a loop that calls
through Go leaves its polls to Go, and the optimized code makes some of
those calls natively. The frames that began in the baseline code run on in it: the
replaced code stays placed (`nativeFn.prev`), and an exit resumes in the
code that holds its address.

The tier is on by default, at `DefaultOptThreshold`, 30,000 calls. Its
compiles, once the cost of both tiers' compiles was worked on, left a short
program's first run faster than the baseline alone for four of the six Go
programs and slower for two (json 89 ms to 98, interp 37 to 39), and every
later run about twice as fast (json 57 to 47, gzip 59 to 29); before that
work they made json's first run 135 to 161 ms. Compiled in the background,
every first run is the baseline's or faster -- json 77 to 70, regexp 89 to
52, gzip 75 to 53 -- but interp's, 36 to 37, which is the baseline code's
counting of its calls: a threshold no function reaches costs it as much
(In the background).
`Code.SetOptThreshold(n)` sets another: 0 compiles every function the
tier accepts with it at once, the baseline only those it refuses, and a
negative n turns the tier off. `WASM_JIT_OPT` sets it for tests and
benchmarks (a number, `on` for the default, `eager` for 0, `off`), and
`WASM_JIT=opt` is `eager` with the JIT on at every first call.

The count weighs the compile against what it saves, as measured on the
Go programs of `internal/cmd/wasmbench`, when compiling cost about 0.4 to
0.7 µs a byte of a function's body -- a third of that since (The
compile's cost) -- 0.76 ms for each of the 182 functions json compiles
in its first run (138 ms in all), and the optimized code
saves json 27 ms a run over the 2.5 million calls a run makes of its 789
natively compiled functions: about 11 ns a call, so that an average function
repays its compile after some 70,000 calls, the hottest far sooner. Taking
a function's calls so far for those to come, the count is where the
programs' first runs, which pay the compiles, cost least -- json's first
two runs took 282 ms at 1,000 calls, 251 at 10,000 and 222 at 30,000 (193
in the baseline alone), regexp's 153, 141 and 131 (194) -- while every
workload's steady state is that of compiling everything at once, and the
functions whose baseline code is slowest, a micro-benchmark's calls
through a table and memory.copy, are still compiled within their first
run (62 and 36 ms where 1,000 calls gives 29 and 18, against the
baseline's 840 and 139). A function whose loops run long is counted by
its polls: 30,000 calls are 120 polls, about half a million back edges.

On-stack replacement enters baseline code: the optimized code has no
loop entries. A call running in the interpreter of a function whose
native code is optimized enters the loops of the baseline code it
replaced, or of baseline code compiled then for its loops alone
(`loopsFor`) where it replaced none.

## In the background

A function whose baseline code runs its count out is compiled by the
process's workers (`engine/jit_bg.go`), the program running its baseline
code meanwhile; the baseline's first-call compile stays on the calling
goroutine, which needs the code at once.

**Scheduling.** The exit, or Go's count of the baseline code's polls and
exits (`codeJIT.sample`), queues the function once (`tierUpLater`: its
`optDone` set by a compare-and-swap) and sets its countdown to
`math.MaxInt64`, so that its baseline code exits no more; every later ask
stores it again, since a call's decrement racing with the store may undo
it. The workers (`optWorkers`) are at most a quarter of GOMAXPROCS, at
least one, each started when a request is queued and none is free, each
ending when none is queued, as the JavaScript JIT's are. The queue holds
the Codes with requests in a ring, served in turn, so that one module's
many requests do not hold up another's; of a Code's, a worker takes the
function whose baseline code was called most since it was queued -- its
countdown, from `math.MaxInt64` down -- so that one no longer called waits
behind those that are. A request holds the Code's `codeJIT`, never the
Code: it is dropped when the Code is collected.

**What a compile reads.** Only what no running call changes: the module
-- its bytes, types and functions, fixed once validated -- the
function's body in it, its record's frame (`nslots`, the locals' slots,
`refs`, `caught`), set before the record is stored and never changed, its
callees' frames, validation's bounds or their records (`Code.get`, which
makes a record with atomic stores and `Code.mu`), and the process's
constants (the write barrier's addresses, `jitLayout`). Not the frames,
instances, memories, tables or globals, nor the exec; the countdowns only
for the queue's order, atomically. A compile works in a workspace of its
own from the tier's pool, and a panic is a refusal (`Compile`). While a
host times the compiles (`Timing`), which it does on one goroutine, they
are made on the spot.

**Placement.** Running native code holds the Code's run lock for reading
for as long as it runs, so a placement taking the write lock would wait
for the longest stretch between two exits of every goroutine running the
Code, and stop each at its next entry meanwhile. A worker places apart
(`internal/jit.PlaceRawApart`) instead, taking no lock running code holds:
from a page boundary past every code placed in the arena's last chunk, or
in a new chunk, on pages no code is in and none ran from, written, sealed
executable and flushed from the instruction cache before the code is
published. No core has fetched from those pages, which were never
executable, and other threads reach the code only through the link or the
function's native code, stored after (atomically, sequentially
consistent). The compiles a Code finished meanwhile are placed together,
one worker at a time (`placeReady`): the rest of the page before a batch
(16 KiB pages on darwin/arm64; where pages are 64 KiB, as large as a
chunk, each batch maps a chunk of its own) is left to code placed on the
spot later, which the Code's reusing arena places in such space, and what
is placed on the spot after it packs after its end too. Each code takes
its room in the Code's JIT budget as the batch is placed (the JIT's
README, Concurrency and memory). On json's first run 38 batches of
one function were placed, and the arena mapped 8.85 MB with 7.40 MB of
code in it, the chunks' ends included.

**Publishing.** Under the Code's compile lock (`codeJIT.mu`), held for the
stores alone, so that the first-call compiles of the program's goroutines
wait for no compile: the code joins the Code's, the function's native
code becomes the optimized code with the baseline code as its `prev`, its
frame is stored, then its link. A call entering after it reads the link,
or the native code, and runs the optimized code; a call already in the
baseline code finishes there: an exit resumes in the code that holds its
address (`nativeFn.holding`, along `prev`), and a call running in the
interpreter enters the baseline code's loops (`loopCode`). An `ExitTierUp`
that raced with the publishing finds the optimized code and enters it for
its call.

**GOMAXPROCS 1.** A worker then runs only when the program's goroutine is
preempted, and the optimized code comes later than a compile on the spot
would bring it: compiled in the background the Go programs' first runs
took regexp 72 ms where on the spot they took 58, gzip 82 and 66, json's
second run 48 and 39. So by default a Code compiles on the spot where
GOMAXPROCS is 1 (asked at each tier-up), and in the background above; with
two the background wins (json's first run 83 to 69, gzip's 60 to 54).

**Measured.** `internal/cmd/wasmbench -engine jit-base,jit-tiered -suite
programs`, median of 3 fresh processes, the tiered first run on the spot
and in the background, the baseline alone's in parentheses, then the
second run, ms:

| program | amd64 first | amd64 second | arm64 first | arm64 second |
|---|---:|---:|---:|---:|
| sha256 | 146 -> 147 (158) | 49 -> 48 | 341 -> 330 (408) | 108 -> 108 |
| sort | 210 -> 205 (220) | 109 -> 106 | 516 -> 500 (448) | 270 -> 269 |
| json | 90 -> 69 (77) | 43 -> 35 | 176 -> 149 (160) | 103 -> 82 |
| regexp | 54 -> 52 (89) | 31 -> 30 | 129 -> 125 (192) | 81 -> 81 |
| gzip | 62 -> 53 (75) | 23 -> 22 | 140 -> 115 (178) | 64 -> 62 |
| interp | 38 -> 37 (36) | 12 -> 12 | 81 -> 79 (78) | 50 -> 50 |

(amd64: Ryzen 9 9950X3D2, Windows, 32 threads, 8 workers; arm64: GitHub's
`ubuntu-24.04-arm`, 4 CPUs, one worker.) Peak RSS is level but json's,
82 to 90 MB on amd64, which varied from 74 to 88 MB with one to eight
workers. interp's first run is the baseline code's counting of calls: a
threshold no function reaches costs it as much. sort's on arm64 is not
the counting (459 ms with a threshold never reached, 455 without) nor the
compiles on the program's goroutine (503 on the spot).

**Shutdown.** A worker makes the request's weak pointer to the Code
strong for the compile, its placement and its publishing, so the Code's
cleanup, which closes its code and gives its arena back, cannot run
meanwhile; a Code collected while its requests are queued has them
dropped by its cleanup (`optPool.drop`), and a worker finding the weak
pointer nil drops its own. The cleanup marks the code closed under the
compile lock, and a placement that finds it closed releases what it
placed: nothing is published into an arena given back
(`TestOptBackgroundCodeCollected`).

**The collector.** A compile is Go code like any other, on a goroutine
of its own: it allocates, is preempted and marks through Go's barriers.
Native code gains no pointer from it: the links are code addresses
outside Go's heap, and the native code's record is a Go pointer stored
atomically.

**On the spot, for tests.** `Code.SetOptBackground(false)` compiles on the
goroutine that ran the count out, the tiering then the same on every run,
and `true` in the background even at GOMAXPROCS 1;
`WASM_JIT_OPT_BACKGROUND` (`on`, `off`) sets it for tests and benchmarks.
`WASM_JIT=eager`, `osr` and `opt` turn it off, so that the spec scripts
run as they did, and the tier's compiles at a function's first compile
(`WASM_JIT_OPT=eager`, `SetOptThreshold(0)`) are made there, being that
compile. `TestOptBackgroundStress` crosses the threshold for 96 functions
of two Codes on six goroutines while the collector runs, and
`TestJITDifferential`'s `tiered` mode compares every spec function with
the tier compiling after its third call in the background.

## The compile's cost

A compile works in a workspace (`workspace.go`) that a `sync.Pool` gives
it and that it puts back once its code is copied out, so that once the
workspaces have grown to the functions' sizes a compile allocates little
but its code; nothing of the workspace is in the code it returns, and
compiles at once each have their own. Values, blocks and the builder's
constructs come from chunks, and the small lists -- a value's arguments,
a block's values, phis and edges, a switch's table -- from arenas
(`arena.go`), both emptied for the next compile. What is by value or
block is a table by ID; the maps that are left -- the locals' values at
the blocks' ends, the values `repairPhis` finds, the edges `loopDepths`
cuts, the lowering's trap stubs and poll exits -- are `u64map`s
(`table.go`), open-addressed by a uint64 key, a slot an entry only while
its stamp is the table's, so that emptying one is a new stamp where a Go
map's clear costs its capacity. The allocator is an `lsra.Allocator`
kept there, with its own tables and arenas, and the assembler.

The passes walk what may change: the dominator tree is kept until the
graph changes (`order`), its dominance a test of its walk's numbers;
GVN's table is a hash table of chains whose entries are popped as the
walk leaves their blocks; `copyelim` stops after the phis where no copy
is left; `thread` removes the predecessors it threads together and
counts the uses only where a block may thread; `dce` counts them again
only after removing phis that only themselves used. The lowering numbers
only the values the allocator sees, densely and in the order of their
IDs (`Value.vreg`), so that the allocator decides as it would by ID, and
its cold code is data (`stub`), not closures. The allocator's liveness
follows the paths from the uses (Register allocation); what a register
lives across is counted by sums over the clobbers, a few ranges are
tested and merged against many by searches, and the webs and the pairs
to coalesce are ordered by counting sorts.

`TestOptCompileCorpus` measures it (Validation). On the Go program
`internal/cmd/wasmbench` builds -- 3,011 functions, 3.77 MB of body -- a
compile takes 158 µs a function, 126 ns a byte of body (196 µs at
GOMAXPROCS=1), and allocates 25 KB and 121 objects, 11 KB of it the
code's copy; it took 398 µs (589 at GOMAXPROCS=1) and allocated 627 KB
and 5,394 objects before. By phase, µs a function:

| build | passes | describe | liveness | ranges | webs | scan, slots | emit, encode |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 23 | 63 | 10 | 7 | 8 | 16 | 10 | 19 |

-- the passes: copyelim 13 (five runs), gvn 13 (two), thread 16, loops
6, dce 5, fold 4, bce 3. The largest functions cost the most a byte (225
ns for one of 17 KB of body and 1,110 blocks).

## Refusals

A function is refused -- it keeps its baseline code -- when it has:

| Reason | What |
|---|---|
| `v128` | a v128 value |
| `simd` | a SIMD instruction |
| `atomics` | a struct or array atomic instruction |
| `gc` | a GC instruction |
| `exceptions` | try, try_table, throw, the legacy instructions |
| `frame` | more spill slots than its frame has below the call area |
| `size` | code past `internal/jit.MaxCodeBytes`, or, on arm64, past a branch's reach |
| `internal` | a panic of the compiler's |

`engine.ReadJITStats` counts what the tier compiled and refused, by
reason, and the time each tier spent compiling; `TestSpec` logs them.

## Validation

```sh
WASM_JIT=opt WASM_SPEC_DIR=/path/to/spec WASM_THREADS_DIR=/path/to/threads \
  go test -tags quickjs_jit ./wasm/internal/spectest ./wasm/internal/engine -count=1 -v
WASM_SPEC_DIR=/path/to/spec WASM_THREADS_DIR=/path/to/threads \
  go test -tags quickjs_jit ./wasm/internal/spectest -run 'TestJITDifferential/opt' -v
WASM_SPEC_DIR=/path/to/spec go test ./wasm/internal/jit/opt -run TestBuildSpec -v
go test ./internal/jit/lsra
go test -tags quickjs_jit ./wasm/internal/engine -run 'TestOpt'
(cd internal/jit/verify && go test -tags quickjs_jit -run 'TestAMD64WasmOpt|TestARM64WasmOpt' .)
go run -tags quickjs_jit ./internal/cmd/wasmbench -engine jit-base,jit-tiered,jit-opt -suite micro,programs
```

A profile's samples in native code have no symbol. `WASM_JIT_CODE=FILE`
has the engine write every code it places to FILE, a line of JSON each --
the function and its tier (`base`, `opt`, or `loops` for baseline code made
for an optimized function's loop entries), the code's address and bytes --
and asks the compilers for their marks (`Request.Marks`): this tier's say
what each stretch of its code was emitted for (a block's start, a value
and its operation, a spill's store, a call's reloads, an edge's moves, a
back edge's poll, a block's end, cold code) and come with a listing of the
function as lowered, each value with its location. `jitprof`, in the
encoder verifier's module, joins the file with a CPU profile of one
process:

```sh
WASM_JIT_CODE=/tmp/gzip.jsonl go run -tags quickjs_jit ./internal/cmd/wasmbench \
  -engine jit-opt -suite gzip -n 1 -warmup 15 -reps 40 -cpuprofile /tmp/p
(cd internal/jit/verify && go run ./cmd/jitprof -listing -code /tmp/gzip.jsonl /tmp/p.programs-gzip.jit-opt.pprof)
```

What the compiles cost, `TestOptCompileCorpus` measures: it compiles
every function of the modules `WASM_OPT_CORPUS` names -- `.wasm` files,
such as the Go program `internal/cmd/wasmbench` builds into its cache, and
`spec` for the spec scripts' -- once, then `WASM_OPT_REPS`-1 more times, and
logs the repeated compiles' time per function and per byte of body, each
phase's (`Times`, the passes, the allocator's phases, the emission and the
encoding), and the bytes and objects they allocated per function.
`WASM_OPT_HASHES=FILE` writes each function's code's hash, or its
refusal, and `WASM_OPT_BASE=FILE` compares with a file so written: a change
that should not change the code is checked by both, the base's hashes
written at its parent. `WASM_OPT_ONLY` limits the corpus to the functions
it names, by the keys the test logs (`goprog.wasm#2211`), and the ten
slowest are logged with their size. `-cpuprofile` and `-memprofile`
profile the test.

```sh
WASM_OPT_CORPUS=/path/goprog.wasm,spec WASM_SPEC_DIR=/path/to/spec WASM_OPT_HASHES=/tmp/base.hashes \
  go test -tags quickjs_jit ./wasm/internal/engine -run TestOptCompileCorpus -count=1 -v
```

`TestJITDifferential`'s `opt` pass runs the spec modules with every
function the tier accepts optimized and compares them with the
interpreter; `TestAllocateRandom` checks the allocator's results on random
functions by a liveness of its own, and an Allocator kept from one to
the next against a new one, `TestLivenessMethods` the two liveness
methods against each other, `TestRanges` the range lists' tests and
merges, `TestCoalesce` a loop's phi web; `TestWorkspaceReuse` compiles in
workspaces kept and from goroutines at once (with -race);
`TestOptTierUp` the counts, the replacement mid-recursion and the poll
samples, `TestOptTierUpByExits` the exits'; `TestOptCallIndirect` every
kind of element a native call_indirect meets, `TestOptRefs` references
in every place, calls and tail calls of every kind with them, idle and
with the collector marking, `TestOptBulkMemory`
memory.copy and memory.fill at every overlap and end; `TestOptMemory` the
baseline's tests of every kind of memory (`TestJITMultiMemory` and the
others, and `TestJITAtomicsMatchInterpreter` and `TestJITAtomicTraps`) on
this tier, `TestOptMemoryRandom` random accesses, atomic ones too, of four
kinds of memory near their ends, with values spilled and constant, and
`TestOptAtomicsConcurrent` goroutines updating one shared memory
atomically; `TestOptBulk` `TestJITBulkBaseline`'s copies, fills and
table.grow and table.fill of every kind of memory and table on this tier,
idle and with the collector marking, and `TestOptGoOps` the segments' and
tables' instructions Go runs, with references and numbers live around
them (`BenchmarkOptMemories` times the accesses, below); `TestThreadDispatch`
and `TestOptThreadDispatch` jump threading, the latter on 1,500 random
dispatch loops against the interpreter; `TestMarks` the profile's marks.

## Measurements

`internal/cmd/wasmbench`, median of 3 fresh processes, each one run after
one warm-up run, ms (Ryzen 9 9950X3D2, Windows; `jit-tiered` is the
baseline with the tier at `DefaultOptThreshold`, `jit-opt` every function
the tier accepts optimized at its first compile; the first run, in
parentheses, pays the compiles; the programs measured again after the
compile's cost was worked on):

| workload | interp | jit-base | jit-tiered | jit-opt |
|---|---:|---:|---:|---:|
| micro/fib | 208 | 11.0 | 9.43 | 8.86 |
| micro/loop | 230 | 14.2 | 14.5 | 14.2 |
| micro/i64 | 277 | 19.8 | 20.1 | 19.7 |
| micro/mandelbrot | 153 | 11.3 | 11.0 | 11.0 |
| micro/nbody | 102 | 6.34 | 3.49 | 3.44 |
| micro/bytes | 207 | 8.79 | 6.87 | 6.97 |
| micro/bulk | 129 | 139 | 16.9 | 16.8 |
| micro/indirect | 387 | 846 | 20.7 | 20.7 |
| micro/brtable | 238 | 42.3 | 41.9 | 41.8 |
| micro/globals | 179 | 26.5 | 27.1 | 26.5 |
| micro/gc | 354 | 234 | 242 | 237 |
| micro/simd | 74.9 | 3.02 | 3.11 | 3.01 |
| programs/sha256 | 1956 | 150 (176) | 52.4 (162) | 54.6 (179) |
| programs/sort | 1760 | 208 (243) | 114 (231) | 120 (249) |
| programs/json | 874 | 59.3 (132) | 49.3 (145) | 37.6 (178) |
| programs/regexp | 822 | 70.8 (117) | 34.6 (87.0) | 36.3 (114) |
| programs/gzip | 464 | 65.4 (105) | 31.2 (92.5) | 43.7 (116) |
| programs/interp | 544 | 24.3 (50.2) | 15.3 (52.3) | 17.7 (70.8) |

The Go programs' steady state, once every function that will be compiled
is (`-warmup 15 -reps 10`, ms):

| program | jit-base | jit-tiered | jit-opt |
|---|---:|---:|---:|
| sha256 | 144 | 47.7 | 47.5 |
| sort | 199 | 106 | 106 |
| json | 54.2 | 24.7 | 24.1 |
| regexp | 70.8 | 28.7 | 27.6 |
| gzip | 53.0 | 20.6 | 20.2 |
| interp | 22.0 | 11.7 | 11.6 |

What the Go programs gained, in the order it came (steady state,
optimized, ms): the calls through tables and memory.copy natively (gzip
52 to 24, sha256 145 to 69 -- a third to a half of their time had been
those exits, in both tiers); jump threading of the dispatch loops (regexp
64 to 29, json 42 to 30, its hot functions no longer refused for their
spills' slots); the allocator's coalescing and spills by density (interp
21 to 12, sha256 75 to 47). The kernels gain where values stay in
registers across a loop (nbody 1.8x, fib 1.2x, bytes 1.3x); loop, i64
and mandelbrot are bound by one multiply's or add's latency each
iteration, as the baseline is; gc and brtable spend their time in Go and
in the switch. The tiered first runs are the baseline's or better but
json's and interp's, which compile their hot functions within a run of
about 60 ms (json +10%, interp +4%, where they were +19% and +17%
before the compile's cost was worked on; see Tier-up).

`BenchmarkOptMemories` (`wasm/internal/engine`) times ten passes over
64 KiB of a memory -- a load, a store and a load at offset 4 every 8 bytes
-- in the baseline code and optimized, µs: memory 1 61 and 54, an i64
memory 2 75 and 66, a shared memory 88 and 89, and an atomic add and a
compare-and-exchange every 8 bytes of the shared one 583 and 583. A shared
memory's accesses each read its size through its address and are never
taken as checked, and the atomics are bound by their locked instructions,
as the baseline's are.

On the spec scripts the tier compiles 5,048 functions and refuses 1,794
(SIMD and v128s 1,499, GC instructions 188, exceptions 107), where it
compiled 4,149 before it took every memory, every offset, the atomics and
the instructions Go runs for tables and segments -- memories other than
an i32 one 514, offsets 14, atomics 80, the table instructions that
allocate or copy 218, memory.init and data.drop 73 were refused -- and
3,914 before it took references and tail calls; in 24 to 34 ms, 5 to 7
µs a function (43 to 63 ms before references), where the
baseline's 2,926 take 11 to 23 ms. The Go program's 3,011 functions
compile in 0.16 ms each (0.40 before; the baseline's, 0.12 ms).

On arm64 (GitHub's `ubuntu-24.04-arm` runner, Linux; the same runs as the
first table, median of 3, the first run in parentheses, ms) the tier gains
as on amd64; the micro-benchmarks are, baseline then tiered, fib 30.0 and
18.5, loop 25.9 and 17.9, mandelbrot 38.3 and 20.2, nbody 19.9 and 10.8,
bytes 31.1 and 17.4, bulk 263 and 24.7, indirect 1,889 and 55.1, brtable
55.3 and 46.5, and i64, globals, gc and simd level:

| program | jit-base | jit-tiered | jit-opt |
|---|---:|---:|---:|
| sha256 | 387 (408) | 108 (355) | 108 (268) |
| sort | 416 (462) | 270 (498) | 268 (460) |
| json | 114 (162) | 97.2 (184) | 51.5 (349) |
| regexp | 161 (195) | 81.0 (130) | 75.7 (283) |
| gzip | 138 (180) | 66.4 (140) | 56.6 (313) |
| interp | 53.5 (79.6) | 50.5 (79.5) | 50.1 (204) |

The 8,764 functions `TestOptCompileCorpus` compiles of the Go program and
the spec scripts lower to 19.3 MB of arm64 code, where amd64's are 34.9
MB: on arm64 a function's polls share one exit, which keeps only the
registers the allocator gave out.

## Next

- OSR into optimized code: a function called once whose loop runs long
  (sha256's block function) runs its first call in the baseline code,
  and is optimized only for its next.
- Splitting within a value's life in the allocator: a web is in its
  register or its slot throughout, so a long value is spilled for its
  whole loop where a short stretch of pressure needs its register.
- The compile's cost, 0.16 ms a Go function, now spread over its
  phases (The compile's cost): fewer passes over the IR -- copyelim
  runs five times, the dominator tree up to three -- and trivial phis
  the builder need not make. In the background it costs the first run
  little, but it is CPU an embedder running a runtime on each core pays.
- Inlining of small callees.
- Bounds checks: hoisting a loop's checks of an induction variable, and a
  block's checks of one base at its widest where nothing between can be
  observed.
- In the code what Go runs for it now, as the baseline makes them:
  memory.size, memory.copy and memory.fill of memories other than an
  unshared i32 memory 0, and table.fill, through the barrier.
