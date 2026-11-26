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
| `inline.go` | the calls inlined: which, their bodies built into the caller, their sites |
| `cfg.go` | the control flow graph: reachability, order, critical edges, dominators, natural loops, loop depths |
| `passes.go` | the optimizations |
| `bounds.go` | the bounds of addresses bce proves accesses within the memory by |
| `globals.go` | the globals' values forwarded from block to block and carried by loops |
| `divconst.go` | the divisions and remainders by constants, made of multiplications and shifts |
| `thread.go` | jump threading, and the lookups that repair SSA after it |
| `verify.go` | the IR's verifier, and what lowering requires of it |
| `compile.go` | `Compile`: build, optimize, lower |
| `workspace.go`, `arena.go`, `table.go` | what a compile works in, kept for the next (The compile's cost) |
| `timing.go` | the compiles' phases, timed for a measuring host |
| `lower.go` | the lowering's part both architectures share: the allocator's view of the function, where each value is, the listing |
| `lower_amd64.go`, `lower_arm64.go` | the lowering: each architecture's code |
| `refs_amd64.go`, `refs_arm64.go` | the references: the reference instructions, the barrier routine, a call's references, the tail calls |
| `memory_amd64.go`, `memory_arm64.go` | the accesses of any memory at any offset: memories other than 0, shared ones, those of i64 addresses |
| `simd.go` | the SIMD instructions' values: what the tier refuses of them and what the builder makes |
| `lower_simd_amd64.go`, `lower_simdf_amd64.go`, `lower_simd_arm64.go` | the SIMD instructions' lowering |
| `gc.go`, `gc_lower.go`, `gc_amd64.go`, `gc_arm64.go` | the GC instructions: their building, the instructions Go runs for them, their lowering |
| `interp.go`, `interp_lower.go` | the instructions the interpreter's code runs for the tier's (Instructions in Go) |
| `exn.go`, `exn_lower.go`, `exn_amd64.go`, `exn_arm64.go` | the exceptions: the sites, their landing pads and the records Go finds them by |
| `internal/jit/lsra` | the register allocator, which knows nothing of WebAssembly |

## The IR

A `Func` is blocks of `Value`s. A value has an operation, a type -- i32,
i64, f32, f64, v128, a call's tuple, or none -- its arguments, two immediates
(`Aux`, `Aux2`) and, for what traps or calls, the offset in the module of
its instruction (`At`), which a trap reports and a return stack entry
records. A block has its phis apart from its other values, and ends one
of five ways: a jump, an if on an i32, a switch (br_table: the distinct
targets as successors, a table of them by index, the last for an index
past the others), a return of values, or a trap (unreachable).

The operations are WebAssembly's numeric ones, one for each operation of
both widths (a comparison's width is its argument's), the high half of an
i64 product, unsigned and signed (`mulh_u`, `mulh_s`, which divisions by
constants become), `select`, loads and
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
notify; `goops.go`; or, marked `goInterp`, any instruction the
interpreter's code runs for the tier's, Instructions in Go) -- but
memory.copy and memory.fill of memory 0
where it is neither shared nor of i64 addresses, which the code does
itself and which are not calls (`InCode`, `BulkInCode`) --
`ref`, a reference instruction (below), the tail calls (`return_call`,
`return_call_indirect`, `return_call_ref`), the last value of a block that
ends with them (a sixth way, `tail`), the atomic accesses -- loads,
stores, the read-modify-writes and compare-and-exchange, each of a memory,
an offset and a width -- and `fence` (`atomics.go`), `simd`, a SIMD
instruction (below), and `exn.val`, a number of the exception a landing
pad takes (below). A block that ends with a call or a throw the handlers
around it may catch ends a seventh way, `invoke`: it goes on to its first
successor, and the others are the landing pads of the catch clauses that
may take what it throws.

A v128 is a value as a number is: a constant has its low half in `Aux` and
its high half in `Aux2`, and a load, a store, a global, a local, a phi, a
`select`, a parameter, a result and a call's argument may be one. A `simd`
value is the instruction whose opcode is its `Aux`'s low 24 bits, on its
`Args`, its lane in `Aux2` -- a shuffle's sixteen lanes packed into both
(`shuffleLanes`) -- and is pure. The loads and stores that are not of a
whole v128 are a scalar access and a `simd` value (`simd.go`): an
extending load an i64 load widened, a splatting load a load splatted,
load32_zero and load64_zero a load put in the low lane, a lane's load a
load replacing the lane, a lane's store the lane extracted and stored --
so that the builder forwards, and `bce` checks, them as any other access,
of any memory.

References are never values. A reference is in a reference slot of the
frame, as the baseline keeps it (its README's Frames): a reference local
in its own, a reference on the operand stack in its position's, at every
point. A `ref` value names the slots it reads and writes -- a copy from
one to another, a null or a function made in one, a global or a table's
element copied to or from one, the null test and `ref.eq` of slots (an i32
value), `ref.as_non_null`'s trap, `select`'s copy where its condition is
0 -- and has its numeric operands (a table's index, `select`'s condition)
as arguments; table.size is one too, and so are the GC instructions on
objects (below). Every `ref` value has an effect: none
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

The GC instructions on objects are `ref` values too (`gc.go`): a field's
or an element's read and write -- the object's slot, the field's cell
offset, its storage type and extension in the immediate, the index and a
numeric value as arguments, a reference value or result in its position's
slot -- array.len, ref.i31 and i31.get, ref.test (an i32) and ref.cast,
each trapping where the instruction does; br_on_cast and br_on_cast_fail
are a test and a branch on it, the reference staying in its slot both
ways; any.convert_extern and extern.convert_any are nothing. A number read
is a value like any load's, but never shared or moved: a `ref` value has
an effect. struct.new and struct.new_default are `ref` values too
(`refStructNew`): the numeric fields -- a v128 one's halves into its two
cells -- their
arguments, the reference fields in their positions' slots, the struct
taken from the level's pool of its type's (the baseline README's GC
objects) and left in the first operand's position's slot; so are
array.new_fixed, and array.new of a constant length and array.new_default,
of the arrays the baseline takes from pools (`refArrayNew`): the type,
the length and what the elements are -- the operands, the operand in
each, or zero -- in the immediate, the numbers their arguments, the
references in their positions' slots; array.new_default's length, where
it is not constant, its argument, and array.new's, where it is not or is
past 16, its last, the value in each element stored by a loop in two
temporaries of the instruction's (the baseline README's GC objects).
array.new_data and array.new_elem are `ref` values (`refArraySeg`), the
segment's offset and the length their arguments: the range checked
against the segment's length (`Ctx.Datas`, `Ctx.Elems`), the array taken
from a pool or made by the engine past them, and the segment copied in by
a loop in three temporaries of the instruction's. What else allocates --
array.new_fixed of other arrays -- and array.fill, array.copy,
array.init_data and array.init_elem are `goop`s, their references in
their positions' slots from `Pos` and the object made left in the
first's; Go makes the objects -- those that allocate from the allocation
stub -- and charges the instance's budget as the interpreter does, and
they forget nothing the block knows of memory and globals.

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
of a whole value (a v128's 16 bytes included) wrote, a later load of the
same memory, address value, offset, width and type reading it. A store
forgets what it may overlap: accesses of other memories, accesses at one
address value whose bytes are disjoint, and accesses at constant addresses
whose bytes are disjoint, survive it. A shared memory's loads are not
known: another thread may store between two. A call and an instruction Go
runs forget it all.

`globals` (`globals.go`) then carries what the globals hold across the
blocks, as the builder carries the locals, by the same construction: each
mutable global read in the function (up to 64) a variable, its value at a
block's start looked up through the predecessors, a write its value, a
read the value found or, where none is, itself; a call, an instruction Go
runs, an atomic access and a fence make it unknown. A loop's header knows
a global only where the loop -- the blocks the builder made between its
header and its end, which hold its natural loop -- makes none of those and
reads the global: it holds what the preheader brings, and, where the loop
writes it, a phi of that and what the back edges bring; where the
preheader knows nothing a load is put at its end, run once where the
loop's reads ran on each turn. At any other merge a value one predecessor
does not know is not known, so no load is put on a path that had none.
Writes stay: the global holds what the interpreter's does at every exit
and trap. Without a loop that could carry a global it reads the pass stops
after one look at the values (0.9 µs a function of the Go program, whose
loops all call: its code is the same). A function the values it keeps
live leave too few spill slots (`frame`) is compiled again without it.
micro/globals' kernel, whose i32, i64 and f64 globals each made a chain
of a load, an operation and a store across the loop's turns, went from
27.9 to 11.9 ms.

`chains` (`chains.go`) carries what the builder knows of the globals and
memory within a block from a block to each successor of which it is the
one predecessor, along such chains: a read of a global or a load whose
value the predecessor knew at its end -- what the last write or read of the
global was, the last load of the same memory, address value, offset, width
and type, or what a store of a whole value there wrote -- becomes that
value. It runs after jump threading, which takes Go's code's edges out of
its dispatch loop straight from a block to the one that goes on, so that a
block reads back the stack pointer its predecessor wrote to a global and
the words it stored into its frame. A call, an instruction Go runs, an
atomic access and a fence forget it all, a store what it may overlap, as
in the builder; a block of several predecessors, a loop's header among
them, knows nothing, so nothing is carried around a loop or past a merge,
and a load it takes away was checked as far by the access whose value it
takes. A block carries 16 globals and 32 accesses at most. A function the
values it keeps live leave too few spill slots is compiled again without
it, as without `globals`. It changes 1,651 of the Go program's 3,011
functions, its code level (1 KB smaller), and takes 3.4 µs a function
(2.2% of a compile), its walk resolving the values' arguments and
dropping the copies, gvn's and its own, for the copyelim after it. The steady state, optimized, with the layout and the
holes over calls (Register allocation) and against them alone (the mean
over two placements of the medians of 3 processes in 3 rounds in turns),
ms: interp 8.27 to 7.80, json 17.95 to 17.85, sort 85.75 to 85.30,
regexp, gzip and the micro-benchmarks level, sha256 38.75 to 39.05.

The phis a lookup made that turn out trivial -- every argument one value,
or the phi itself -- are removed by the first pass.

### Exceptions

throw, throw_ref and rethrow are `goop`s, Go throwing the exception as the
interpreter does (their operands staged as a GC instruction's). A
try_table and a legacy try are blocks, and a call or a throw within the
body of one or more of them -- a site (`exn.go`) -- ends its block
(`invoke`): its successors are the block after it, and a landing pad for
each catch clause of each construct around it that may take its
exceptions -- every try_table around it, and every legacy try whose body
it is in -- innermost first, each a block of its own whose one
predecessor is the site's (`Block.Pad` naming the site). A try_table's
pads are made with the site: each reads the tag's numbers (`exn.val`) and
goes to the clause's label with them, its references already in the
label's positions' slots, or returns them. A legacy try's catch, when it
begins, makes a pad for each site of the try's body, and its body is a
block of those pads, the tag's numbers its phis; a catch no site reaches
is dead, and a delegate's try makes no pads. A site whose trys had no
catch ends its block with a jump.

The pads are blocks like any other, an edge to one an edge of the control
flow graph: the dominators, the passes and liveness see them, so that what
a pad's label reads is live across the site's call -- and so in its slot
when the exception comes (Register allocation) -- and no pass moves a site,
a call or a throw having effects. Nothing jumps to a pad: Go enters it.

### Inlining

A direct call of a function of the module is built into its caller
(`inline.go`) where the callee is small -- its body, its locals'
declarations included, at most 64 bytes -- holds only numbers (its
parameters, results, locals, the globals it reads and writes and the
signatures of the calls it makes), and its instructions are control,
locals, globals, numbers, memory accesses, memory.size, memory.grow and
direct calls: nothing that holds a reference, throws or catches, makes a
tail call, or that Go runs by its offset (`ExitInstr`) but where the CPU
or a test has it so (Instructions in Go): the exit then names the inlined
site, and the engine reads the instruction's own offset back from it.
Calls are inlined
four deep at most (`wjit.InlineDepth`) -- a recursion inlines itself so,
its body sixteen times over -- and 1,024 bytes of bodies into a function
at most, and not where a
construct around the call may catch what it throws: Go finds a handler by
a call's offset in its function. The builder reads the callee's body where
the call is, its parameters variables holding the call's arguments, its
locals new variables holding zeros, its body a block whose label -- where
`return` and the body's end go -- is where the call returns, its results
that label's phis. The call made inlined is no call: it forgets nothing of
memory and the globals, and the passes see across it.

An inlined body makes no frame, and what its instructions report is the
frames the calls would have made. Each instruction of one that may report
an offset -- a trap, an exit, a call's return stack entry -- has, in place
of its offset, one past the module's bytes that the engine gives
(`wjit.Request.InlineSite`): an inlined site, which says the calls around
it, the outermost first, each a function and the offset of its call, and
the instruction's offset in the innermost. The builder numbers them as it
reads the bodies, and once the passes are done, what is left of them is
given the engine's (`resolveInline`). Where the engine makes the call
stack of native frames (`jitSync`) it adds the inlined frames, so that a
trap's frames are those the calls would have had.

The call depth is the calls' too. The engine runs the code only in a
frame whose calls `wjit.InlineDepth` deep are within the depth's limit --
a native call's return stack ends where its callee's would not be, and a
frame nearer runs in the interpreter (the baseline README's Calls) -- so
an inlined call is never one that would pass it, and checks nothing. Each
had checked it, three instructions an inlined call, until fib's code went
from 3.87 to 3.63 ms without them. A call made k calls deep pushes its
entry k entries past the next (Aux2 of `OpCall`), leaving the k frames' entries unwritten, and takes the
return stack's register back from the entry it returns with less those k:
the return stack's depth is the calls' depth, its end checked as any
call's. The engine walks a level's entries from the last, each entry's
offset saying how many unwritten ones are below it (`rsEntries`).

A function whose frame inlining leaves too few spill slots is compiled
again inlining one call less deep, down to none; one whose code it made
too large, without inlining. `WASM_JIT_INLINE=off` turns inlining off.

micro/fib, whose calls were a third of its instructions and whose returns,
indirect jumps, its mispredicted branches, went from 8.1 to 4.7 ms (median
of 6 processes; inlining two calls deep, 4.9, four, 4.3, its code twice as
large again). The Go programs' functions, Go's own compiler having inlined
what is small, have few such calls: json went from 24.0 to 22.8-23.9 ms,
sha256 from 49.2 to 47.7-48.1, the others level; their compiles took 2%
longer and their code 0.8% larger. Inlining four deep rather than three,
once the inlined calls no longer checked the depth, took fib from 3.87 to
3.20 ms tiered and from 3.72 to 3.47 optimized (the mean over four
placements of its code), its code 10 to 20 KB; five deep, which the
budget cuts short, ran slower again (3.55 tiered). It changes none of the
Go program's functions: their small callees are not recursive, and none
inlines a call four deep.

## The passes

`Optimize` runs, in order:

| Pass | |
|---|---|
| `globals` | the globals' values forwarded across blocks and carried by loops that change none (below) |
| `copyelim` | copy propagation: each use of a copy, and of a trivial phi, becomes the value copied |
| `gvn` | global value numbering: a pure value a dominating one computes the same way -- operation, type, arguments (commutative ones in order), immediates -- becomes a copy of it, by a walk of the dominator tree |
| `fold` | constant folding and identities (x+0, x*1, x&-1, x-x, x==x ...), the comparisons' inversion under eqz, an eqz of an eqz, or of its widening to an i64, made the comparison with zero (Go's code's test of a bool it made with eqz, two SETcc where a branch now fuses with one comparison), `i32.wrap_i64` of a 64-bit operation of extended i32s and constants narrowed to the 32-bit operation, the divisions and remainders by constants made of multiplications and shifts (below), a float's product with 2 made its sum with itself (below), select on a constant or of one value, and the branches and switches on constants, whose dead blocks go |
| `thread` | jump threading (below), then `copyelim` |
| `gvn` again, `copyelim` | |
| `chains` | what the builder knows within a block -- the globals' values, the last loads, the values stores wrote -- carried along the chains of blocks of one predecessor (below), then `copyelim` |
| `licm` | the pure values of a loop that makes no call whose arguments are defined outside it go to its preheader -- its header's one predecessor outside it; a loop threading made has others, and keeps its values -- inner loops first; a value hoisted out of a loop that calls would live across the calls, kept in a slot, where computing it again costs less |
| `dce` | values without uses or effects, and phis only themselves use; and a global.set another of the same global follows in its block, nothing between reading the global, having an effect or able to trap (a Go function's return moves its stack pointer twice) |
| `bce` | bounds-check elimination: an access is in bounds if a dominating access of the same memory and address value -- or of the i64 an i32 address wraps -- checked an end as far; a memory never shrinks, so the check holds across calls and grows; a shared memory's accesses each check its size; an access of memory 0 whose address's values keep it within the memory's least size is not checked, nor one whose address is a phi each of whose arguments is in bounds on its edge; and an access's check covers those of its address after it in its block that nothing between could tell apart (below) |
| `split` | every critical edge gets a block, where its phis' moves go |
| `loops` | natural loops, each block's depth (below), and the layout: reverse postorder again, of an if's two successors the deeper first (`layout`, Lowering), where an if's second successor is the deeper -- elsewhere the blocks are as `order` left them |

### Bounds from values

An access of memory 0, unshared and of i32 addresses, is in bounds
wherever it runs if its address's greatest value plus its offset and size
is within the memory's least size: the minimum the module declares, which
an imported memory has at least when it is linked and which no memory goes
below (`bounds.go`). The greatest value is a constant's, a mask's, an
unsigned byte's or half's load, a right shift's, a sum's where it cannot
wrap, a phi's greatest argument's -- and an induction variable's: a loop
header's phi whose back edge brings it plus a constant step, the way to
the back edge decided by a comparison of that sum with a constant, takes
only the values it enters with and those the comparison let through,
below the constant; where the sum cannot wrap and it enters with a
constant, only that constant plus steps, so that a loop over words to
the memory's end is within it. Signed comparisons bound it where it
enters with a constant that is not negative and nothing wraps. Nothing is
hoisted, so every access that may be out of bounds is checked where it
is, and traps where the interpreter's does. micro/bytes' loops, over a
memory of exactly the size they cover, went from 7.3 to 5.5 ms; the Go
program's code is 0.5% smaller, its accesses at constant addresses and of
masked indices within the minimum its memory declares.

### Checks across phis and through a block

Go's code runs a loop's every value through a phi of the dispatch
switch's blocks (Jump threading, below), and the switch enters each of
those blocks from the function's entry too, where the locals hold their
first values: a pointer the loop carries is a phi of a constant and of
the value the loop brings, block after block, and its accesses in one
block are not dominated by those of the same pointer in the block
before. An access of memory 0 whose address (or the i64 it wraps) is a
phi is in bounds where every argument is: a constant whose access is
within the memory's least size, or a value an access reached as far on
every path to the end of the argument's predecessor -- an access in that
block or one dominating it, back to the value's own block, or, where the
value is a phi again, its arguments in turn (`bcePaths`; six phis deep
and 64 blocks a search at most, a phi's farthest end found and nearest
failed kept). An access before it in time, on every path, reached as
far, and the memory never shrinks.

An access's check is widened (`widen`) to cover the accesses of its
address after it in its block, up to the first value with an effect or
that may trap -- but a load checked as far already -- and up to a store
among them, which is covered and ends the run: nothing between could
tell at which of them the check was made. The access checks the
farthest end, and where that fails its cold code takes the end back to
each covered access's in order, and traps at the first past the memory,
as the accesses one by one would have. Go's code reads a frame's
arguments and saved values at rising offsets from one stack pointer.

The two take 6,600 of the Go program's 177,800 checks away (4,290
checks widened), 20 KB of its 33 MB of code; json's steady state went from 21.3 to 21.0 ms optimized (22.0 to
21.7 tiered), regexp's from 23.9 to 23.3 (24.3 to 23.7), gzip's and
interp's from 18.4 and 10.2 to 17.9 and 9.6, sha256 and sort level; the
pass takes 4.3 µs a function where it took 3.0 (the compile 0.7% longer).

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

A division or a remainder by a constant that cannot trap -- any but 0,
and -1 for a signed division -- is made of operations that take a few
cycles where the division instruction takes tens (`divconst.go`): by a
power of 2 a shift or a mask, the signed ones rounding toward zero by a
bias of the sign; unsigned by 2^(w-1) or more a comparison; by any other
the high half of the dividend times a magic number, shifted (Granlund and
Montgomery's, an add where the number needs w+1 bits; Warren's for the
signed, which adds 1 to a negative quotient), an i32's product made in 64
bits and an i64's high half `mulh_u` or `mulh_s` (MUL or IMUL into DX on
amd64, which the allocator keeps free as a division's; UMULH or SMULH on
arm64); a remainder is the dividend less the quotient times the divisor.
micro/i64's kernel, a remainder by 1,000,003 in a loop that was 78% the
division, went from 20.0 to 13.1 ms.

A float times 2 -- either operand the constant -- is the float plus
itself: both exact, overflowing to the same infinity, and, where the float
is a NaN, the same NaN, its payload kept and made quiet, on amd64 and arm64
alike, so the result is the interpreter's bit for bit; an add's latency is
less than a multiply's. micro/mandelbrot, whose inner loop carries
`(2*zr)*zi + ci` from turn to turn, went from 11.0 to 10.6 ms optimized
(10.9 to 10.4 tiered; the mean over four placements of the code of the
medians of 3 processes in two rounds).

Folding is exact: integers as WebAssembly defines them, a division only
where it cannot trap; floats only where no NaN is involved (add, sub,
mul, div, promote, demote, the exact integer conversions), or on bits
(neg, abs, copysign), so that the interpreter's NaN payloads are never
the folder's to choose. The SIMD instructions are not folded -- a
`select` of v128s is, on its condition -- but they are pure: `gvn` shares
them by their opcode, lane and operands (a v128 constant by both halves)
and `licm` hoists them.

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

- At entry the parameters are in slots 0 up, as the caller left them, a
  v128 taking two -- its low half, then its high half.
- At a return the results are in slots 0 up, likewise.
- A native call writes the arguments into the callee's frame, at its
  frame's end (`L + 8*nslots`), one slot each and a v128 two, and reads
  the results there; the return stack entry is the baseline's.
- A call through Go -- of an import, of a callee with no native code or no
  room, a `call_indirect` not made natively -- and an instruction Go runs
  find their numeric operands, two slots apart, each at its place among
  the operands, in the call area at the frame's top (from `argBase`,
  `nslots` less twice the most operands or results of any of them), and
  leave their results there; their references stay in their positions'
  slots, where `Ctx.RArgs` says (below).
- A poll's exit keeps every register the allocator gave a value in the
  context's save area (`Ctx.Save`): nothing in the level runs while Go
  polls. A function with v128 values keeps its float registers whole, 16
  bytes each, in `Ctx.SaveV`, and its barrier routine likewise.
- A trap reads nothing of the frame.

Every other slot below `argBase` is the allocator's spill slot, a v128's
two. A parameter, a local and a result have the slots of the engine's
frames: a number's numeric slot is its place among the numbers, a v128
counting two, a reference's reference slot its place among the
references. A slot's
word holds an i32 or an f32 with its upper half clear, as the
interpreter's frames do, so that what Go reads of the call area is right
and a slot is copied whole: on amd64 an f32 is stored with MOVSS and a
zero upper word; on arm64 an f32 in a register has the rest of the register
clear, as the baseline keeps it -- every instruction that makes one writes
its S view, which clears the rest -- and is stored as its D view.

A function that holds references -- the engine's frame has reference
slots (`Request.FrameRefSlots`) -- makes every reference slot but its
parameters' null when it begins, as the baseline does -- the barrier's
flag tested once, the slots cleared by plain stores while the collector
does not mark, each through the barrier while it does -- and moves the
context's R past its own for a native call (`adv`) and back after: its
callees' references go past its own. Its reference instructions address
the slots from the context's R, loaded into a scratch register for each,
and a pointer word passes through the float scratch register (x0) within
the instruction's code: no reference is in a register the allocator has,
nor across a call, an exit or a poll. Every store of a pointer word tests
the write barrier's flag and, while the collector marks, goes to the
function's barrier routine, out of line, which saves every register the
code may keep a value in -- of the allocator's those the function's
values were given and those a sequence takes for its own while it may
store (a call's callee entry and index, the entry's clearing loop, the
register that keeps the return stack's entry), and the ones every native
frame keeps -- around `internal/jit.StoreRef`, as the baseline's does
(its README's References and the collector); the payload word is a plain
store. Saving only those took micro/gc from 266 to 232 ms at GOMAXPROCS=1,
where the collector marks for most of a run and its trees' pointers are
stored through the routine (micro/arrays 83 to 74), and nothing at 32
threads, where it marks a third of the time.

A GC instruction on an object loads the object's pointer word from its
slot into s1 (X17), tests it for null, and reads or writes the field or
element in place at the engine's offsets (`Layout`), as the baseline does
(its README's GC objects): a struct's field in its cell's second word,
zero-extended, an array's index compared with its length, a number loaded
into the value's register or stored from its location -- a packed one
sign- or zero-extended, an f32 as its word, a v128 field's halves from
and to its two cells' payload words, a v128 element's 16 bytes whole
(`loadV128`, `storeV128`) -- and a reference copied
between the object and its position's slot through the barrier, s0 (X16)
holding R, the index or a number on its way. ref.i31 stores the mark and
the low 31 bits; the tests compare the mark, the header's kind byte, and
for a defined type the canonical index at its depth among the
supertypes, a cast going to its trap. A new struct is taken from its pool
-- the context's table, the type's pool, its count, each tested, the
count decremented and the struct's pointer loaded into s1 -- its numbers
stored from their locations and its references copied plainly through x0
from their positions' slots (the baseline README's References and the
collector), its pointer stored into the first operand's position's slot
and the pool's word of it cleared, both through the barrier; on arm64,
whose barrier routine keeps no scratch register, the pool's word is found
again from the context. A new array is taken so through its type's entry
and its class's word, its length stored and its elements from their
locations or slots; of a length not constant, the class is looked up as
the baseline does, the length and the pool's word's address kept in the
context's Arg2 and Arg3 while s0 and s1 are taken, and an array past the
pools' goes to an exit site of its own (`AllocMake`), which goes on past
the instruction. An empty pool goes out of line to the function's
exit for it, which keeps every register the allocator has in the
context's save area, as a poll's, exits with `ExitAlloc` -- by way of the
allocation stub where the request has one, the exit taken only where the
engine's Go declines (the baseline README's GC objects) -- and goes back
to take again (a site leaves its type, an array's class, and its offset
in the exit's arguments and where to go back in s0). On arm64 an index not in a register
of its own is compared with the length while the array's address waits in
D31. An instruction Go runs for objects stages its numeric operands in the
call area, each at its place among the operands, and sets `RArgs` to its
first operand's position, where Go leaves an object it makes (`stageGC`).

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
| R10 | the return stack's next entry where it arrives and leaves |
| one of R13, R9, R8, SI, DX, CX, AX | the return stack's next entry in the code, where the allocator gave that register to no value and no instruction writes it; else `Ctx.RSP` holds it |
| R11, R10 | general scratch registers |
| X15, X14 | float scratch registers |
| SI, R8, R9, R13, AX, CX, DX | the allocator's general registers, AX, CX and DX last (division's and shifts'; memory.copy and memory.fill take AX, CX and SI, an atomic store, read-modify-write and compare-and-exchange AX, CX and DX) |
| X0-X13 | the allocator's float registers: f32s, f64s and v128s |
| SP, BP, R14 | never written |

Native callers enter with R10 the return stack's next entry, which the
entry keeps: in the first of R13, R9, R8, SI, DX, CX and AX that the
allocator gave to no value of the function and that none of its
instructions writes besides (`keepRS`): a division's AX and DX, a shift's
CX, memory.copy's and memory.fill's AX, CX and SI, an atomic's AX, CX and
DX -- the allocator's clobbers -- and AX, CX and DX where the entry
clears reference slots; or, where there is none, in the context. The
allocator gives AX, CX and DX last, so that most of a Go program's
functions, whose values take the other four, keep it in DX (503 of the
789 json optimizes, 33 when only R13, R9 and R8 could). The
return sequence takes it from there, pops the entry and jumps to its
address with BX still the function's L, as the baseline's does, and the
caller takes its own back by subtracting its frame's size (the baseline
README's Calls). A native call
pushes its entry from where the code keeps it and leaves R10 past it for
the callee, which returns with R10 as the call found it; the code keeps
it again, where an exit of the callee may have left another in the
context. A register that keeps it is stored into `Ctx.RSP` at every exit
and trap, as the baseline's R10 is, and loaded again by the exit's
resumption and a landing pad; the poll's exit does not save it. Kept in
the context, it cost each call a store at the callee's entry and another
after the return, and a load at the call and at the return: fib's
optimized code took 7.3 ms where it took 7.8 with the other changes to
calls (below) alone.

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
| one of X26, X25, X24, X12-X3 | memory 0's size less 4,096, where the function keeps it in a register the allocator gave nothing (Lowering) |
| X1-X12, X24-X26, X13-X15 | the allocator's general registers, X13-X15 last (memory.copy's and memory.fill's, and an atomic store's, read-modify-write's and compare-and-exchange's) |
| D0-D29 | the allocator's float registers: f32s, f64s and, whole, v128s |
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
slots, never). Webs are allocated by a linear scan in order of their
starts with active and inactive webs, as Wimmer and Mössenböck's, so that
a web takes a register in another's hole. Where a class's registers run
out, the web scanned takes the register whose webs in its way are least
dense -- their weight, their uses at eight times each loop around them,
over the positions they cover -- and each of those webs is split: it
gives the register up over a hole in its life (below). On arm64 that is
where each one's holes would cost less than its uses and the new holes
together less than spilling the web scanned; else, as before splitting,
they are spilled whole where they are less dense than it, and it is
spilled otherwise. On amd64 (`lsra.Config.Displace`) the web scanned
takes the register whatever their density, where no instruction's
temporaries are in the way, each of them split where its holes would cost
less than its uses and spilled whole otherwise. A register an instruction
clobbers (on amd64 AX and DX for a division, CX for a shift by a
variable, AX, CX and SI for memory.copy and memory.fill, AX, CX and DX
for an atomic store, read-modify-write and compare-and-exchange; on arm64
X13-X15 for memory.copy, memory.fill and those atomics) is not given to a
web live across it. A web in a register that lives across a call is
stored into its slot where each of its values is defined and loaded again
after each call it crosses in its register. One whose reloads would weigh
as much as its uses gives its register up over the calls instead
(`callHoles`): a hole over each run of calls with none of its uses
between them, from its last use or definition before the run to its first
after, where the hole -- a load where it ends, the web's slot holding it
already -- costs less than the reloads; it is then in its register where
it is used between calls, and loaded once before the first use after a
run, or past a loop that calls, rather than after each call. Where its
holes and the reloads after the other calls would still weigh as much as
its uses, it lives in its slot: of the Go program's 13,757 webs that live
across calls, 1,260 weighed so, and 274 of them are now in registers.
micro/fib's inlined calls' `n - 1`, read by the tests and the calls below
it, had lived in its slot, stored, reloaded and stored again by each; it
now stays in its register to its first call, and fib is level all the
same (3.43 ms): its time is its calls' returns, an indirect jump each. The
steady state, optimized, with the layout and `chains` and against them
alone (the mean over two placements of the medians of 3 processes in 3
rounds in turns), ms: interp 8.34 to 7.80, sha256 38.65 to 39.05 within
its noise, the others level. The scan takes 18.0 µs a function where it
took 16.4 (the events' table, once a function has such a web, and the
holes' costs), 1% of a compile; the code is 3 KB smaller. Holes over the
calls of every web that lives across them, each reloaded after its calls
before, changed the code of 253 functions, 2 KB, and nothing measured
but interp, for a scan of 23.3 µs. Slots are given by a
second scan over the ranges, holes included, a web with a parameter
keeping the slot it arrives in, and a register wider than a slot
(`Func.Width`, a v128's two) taking consecutive ones. An instruction's
temporaries (`Inst.Temps`), which a sequence works in, are registers live
from its reads to its writes, so apart from its operands', its results'
and those live across it, and heavier than anything they meet, which is
displaced for them. A phi prefers its arguments' registers and they its,
and a two-address operation's result its first operand's.

A web's weight is what its register saves it: its uses and definitions,
each eight times for each loop around it, but for those that cost the
same wherever the web is -- a phi's argument of the phi's own web, which
moves nowhere on its edge, a phi all of whose arguments are its web's,
and the definitions of a web that lives across a call, stored where its
registers are defined in a register or not -- which weigh nothing, in its
weight as in its holes' cost. A block in a loop that calls weighs as if a
loop less deep (`lsra.Block.Depth`, from the lowering): a loop calls, more
often than not, on a path most of its turns do not take -- a Go program's
calls of the runtime -- and a call costs more than reloading around it.
Before, a web carried through a loop by phis weighed eight for each of
its phis and arguments in the loop, and a value made and stored on every
turn of a loop for a read past it weighed its definitions: in the Go
program's sort, the loop counter gave its register up to such a flag,
and in sha256 and interp the values their loops read went to slots for
values the loops only carried. Weighed so, the steady state (`-warmup
15 -reps 10`, the mean of 8 fresh processes' medians, in turns with the
code before, the code placed at 4 offsets in its arena) took, ms: optimized, sha256 41.8
where it took 47.5 and interp 9.8 where it took 10.4; tiered, sha256 42.4
where 47.9 and interp 9.8 where 10.7; the other programs, the
micro-benchmarks and the compiles' cost level.

A hole covers where the split web and the one scanned are both live,
widened, between the split web's last use before and its first after, to
where a block of a lower loop depth begins or to a loop's header, or to a
hole the web has already, so that the moves into and out of it run least
often. It costs the web's uses in it, each now its slot's, and those
moves -- a store where the web goes in, none for a web stored where it is
defined, whose slot holds it already, and a load where it comes out --
before an instruction within a block, or on each edge that crosses its
bounds, each by its loop depth; the web's holes, the new one with them,
must cost less than all its uses, which spilling it whole would cost. A
split web keeps its one register, and a slot: over its holes alone where
it is not stored where defined. Before splitting, a web displaced was
spilled whole whatever its weight: in the Go program's sort, the hottest
function's loop counter, a web of 50 phis and arguments weighing 1,059,
went to its slot for a temporary of two positions in a block outside its
loops, and its loops read, added to and stored it there on every turn. A
function with a critical edge (the passes split them all) or allocated
with `WASM_OPT_SPLIT=off` is allocated without splitting: the web scanned
displaces those in its way only where they are less dense than it,
spilling them whole, and is spilled itself otherwise. Either way a web of
several registers that would be spilled -- displacing, where temporaries
take every register -- is taken apart instead, its registers
scanned again each on its own and hinted to the others'. Density, not
weight, chooses the register, so that a loop's short temporaries keep
registers and the long values around them, used less often for their
length, go to slots, now over holes around the temporaries. On amd64,
displacing gained regexp 5% and sort 4% where arm64's rule leaves them
level; on arm64 displacing took sha256 from 106 ms to 118, its hot loop's
code a fifth larger with the moves of holes where the old rule, with
arm64's eighteen registers, spilled almost nothing, and gained nothing
else. Other rules measured worse on amd64: the old density rule with
holes (sha256 48 ms to 54-62, its stack pointer losing R13 in a cascade
of evictions), the register chosen by the holes' cost (52), spilling the
web scanned wherever the holes cost more than its own uses (66-70, interp
and gzip 10-15% slower): weight does not see what a short value on a
chain of them, or a loop's address base, is worth in a register.

A phi in a register is stored into its slot where its block begins unless
every argument but a constant shares its register and its slot, which
each stored where it was made; a constant argument is then stored into
the phi's slot on its edge, beside its move into the register
(`phiMoves`). A Go program's function begins with a switch on the point
it resumes at, whose edges give every phi of a loop a zero beside the
value the loop carries: stored where their blocks began, those phis cost
sort's partition loop six to nine stores at each of its blocks, which
held its optimized code on arm64 a seventh slower than the baseline's
(backend stalls 89 million cycles of a run in the baseline code, 420
million in the optimized), and its steady state 268 ms against the
baseline's 237; stored on the switch's edges, they cost the loop nothing
(139 ms on arm64, where regexp went from 77.5 to 53.7, gzip from 59.1 to
41.2 and interp from 50.0 to 19.4; on amd64 sort from 106 to 94, regexp
from 28.8 to 27.0, interp from 12.1 to 11.2, the others level).

A landing pad's one predecessor ends with its site, so what the pad reads
of the function is live across the site's call: in its slot, where Go
finds it, the pad loading it again as the code after the call does. A pad
has no phi, the pad's own edge to its label taking the phis' moves; the
moves of split values after the site's call, and on its edge, it makes as
the code after the call does (below).

Lowering reads a spilled operand from its slot -- on amd64 as a memory
operand where the instruction takes one (`OpMem`, `ImulMem`), else, and on
arm64 always, into a scratch register -- and computes a spilled result in
a scratch register and stores it. The moves on an edge are parallel: each
waits for those that read its destination, and a cycle is broken through
R10 or X14 (X17 or D30 on arm64). Where a move is of a v128, whose two
slots may meet a word's or another v128's in part, a move waits for those
whose sources meet its destination at all, and where every move waits, the
source of one another waits for goes to a scratch register -- X14, X15 or
R10 (D30, D31 or X17) -- which its readers read (`parallelV`).

The lowering keeps where each value is as it goes (`lowering.where`),
which moves for a split value at its holes' bounds: as a block begins it
takes it from `lsra.Result.Entry`; before each instruction, as the
allocator numbers them, it makes the moves `Result.Moves` gives -- a load
into the register where a hole ends, a store into the slot where one
begins, none for a value stored where defined -- as a parallel move, and
learns where a value the instruction defines is; on an edge from a block
of one successor it makes `Result.Edge`'s moves in the parallel move of
the phis' arguments at the block's end, a phi going where it is as its
block begins, and on an edge from a block of several successors where the
successor, of one predecessor, begins. A landing pad, which Go enters,
loads what its site's call reloads, then makes the moves after the call
(`siteGap`) and its edge's. The out-of-line code of a call through Go,
made after the blocks, finds its operands and results where the call
left them (`keepWhere`).

v128s are the float class's, as wide as two slots (`lsra.Func.Width`):
they share the float registers with f32s and f64s, and a spilled v128, or
one kept across a call, has two slots in a row.

## Lowering

Each instruction compiles to the baseline's sequence for it where the
result's bits depend on the instruction chosen -- the floats (the
baseline README's Floats: the same SSE instructions, operand orders --
the first operand the destination, for the NaN rule's choice --
roundings, min/max, truncation bounds and unsigned conversions), so that
the results are the interpreter's bit for bit -- with the operands where
the allocator put them. copysign is done on the bits in general
registers. An integer comparison that is the last value of a block and
its branch's condition alone is fused with the branch, and so is a float
one whose truth is one condition of the flags (`fusesFloat`): on amd64
gt, ge, lt and le, UCOMISD then JA or JAE, an unordered result making
them false and their inverses true; on arm64 every one, after FCMP; eq
and ne on amd64, which need the parity flag besides, are made into a
register and tested.

On amd64 an and of a mask of a value's low byte, or of an i64's low half
-- Go's code's conversions to uint8 and uint32 -- is a zero-extending
move from the value's register or slot (MOVZX, MOV of 32 bits), where it
was a copy and an AND, 0xffffffff, no instruction's immediate, moved into
R10 first.

On amd64 a float constant other than zero, and fabs's and fneg's masks,
are loaded from the code's pool of constants after it, RIP-relative, as a
v128's are (below): one load, where a move of the bits into R11 and from
there into the register took two instructions and a general register.
micro/mandelbrot's inner loop multiplies by 2 and compares with 4: 11.0
to 10.6 ms (median of 7 processes).

On amd64 a switch's index, held to its table's last entry, picks a jump
of 5 bytes in a table after the code's jump to it: straight to its
target block, or, for a back edge, to the code that counts the poll
down. Each entry had been a jump of 5 bytes and a NOP of 3, to a jump
to the target: the Go program's switches went from 499 bytes each to
249, its code from 19.6 to 18.9 MB. A table of 4-byte displacements
from the end of each, loaded and added, made them 210 bytes, but
micro/brtable 13% slower (42.1 to 47.4 ms) at every placement: the
target is a load away from the index where it was arithmetic.

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
size is within 2^31-1. Where that end past the address -- the offset
plus the size, or a widened check's farthest -- is 4,096 bytes or less
(`wjit.MemLimitSlack`), the address itself is compared with the
context's `MemLimit`, the memory's size less 4,096, signed, so that a
memory smaller than that fails it: one comparison of a register with
the context, CMP and JG, 4 bytes and 6 (the field within a byte's
displacement of the context's start, where it had taken 7 and the Go
program's code 0.5 MB more); past the limit, out of line
(`stubCheck`), the end is made and checked exactly and the code goes
back to the access, or traps as the check does. Nearly every access of
the Go program's is within 4,096 bytes of its address; their checks were
a sixth of json's and regexp's samples, and with the end no longer made
in R11 before each the steady state went, optimized, ms: json 20.5 to
19.5, regexp 22.0 to 20.7, sha256 41.6 to 37.4, gzip 17.5 to 16.7,
interp 9.9 to 9.6, sort 88.9 to 87.0. The access is `[R15 + address + offset]`,
the address in a register (an i32 there has its upper half clear). On
arm64 the address plus the offset is made in X17, in 64 bits, where the
offset is not 0, the end in X16 and compared with X21, and the access is
`[X20 + X17]`. Where the function keeps memory 0's limit in a register
(`keepLimit`), an access whose end past its address is 4,096 bytes or less
compares the address plus the offset with it instead, signed, the exact
check out of line as on amd64 (`stubCheck`), the trap's site after it: the
end is no longer made in X16, one instruction fewer an access. The
register is one of the allocator's that it gave no value or temporary --
X26, X25, X24, then X12 down to X3; X1 and X2 are a call through Go's, X13
to X15 memory.copy's, memory.fill's and the atomics' -- made from X21, the
size less 4,096 (`SUB`), where the code begins and after every call,
exit, poll and landing pad, where Go or a callee may have changed the
register or the size, and kept by the barrier routine. A function keeps it
where its accesses so checked outweigh its calls, each weighed by its loop
depth. Of the Go program's 3,011 functions 2,935 have such a register and
2,838 keep the limit in it; 154,758 of their accesses compare with it, one
instruction fewer each, and the code makes the limit in 42,017 places.
Its arm64 code is 9.0% larger (15.65 to 17.05 MB), the exact checks out of
line, as amd64's was for its limit; with the trap's site its own jump away
it was 12.9%. Timing it needs an arm64 machine: CI's `ubuntu-24.04-arm`
runners or a Mac.

A trap's site, out of line, leaves its offset in R11 (X16) and jumps to
the function's code for the trap's code, which stores the offset in the
exit's record and exits. On amd64 that is a move of 6 bytes and a jump,
to the last such jump before it where that is within a byte's
displacement (`trapAt`), so that most sites' jumps are 2 bytes -- made so
(`amd64.Asm.JmpNear`), not 5 that Finish would shorten, whose rounds had
taken 15% longer for them; the
exact check out of line goes back on its one condition and is its
access's trap site from there. A site was the store of the offset into
the context, 11 bytes past a byte's displacement, and a jump, and the
exact check a jump back and a jump to its site: the Go program's code
went from 24.1 to 22.0 MB.

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

The blocks are laid out in reverse postorder, of an if's two successors
the one of the deeper loop first (`layout`): the test that leaves a loop
falls through into its body, and the code after the loop comes after the
body, so that a turn of the loop takes no branch but its back edge, where
an exit laid out before the body had the test jump over it into the body
on every turn (micro/mandelbrot's inner loop took three branches a turn).
The steady state (`-warmup 15 -reps 10`, optimized, the mean over two
placements of the code of the medians of 3 processes in 2 to 3 rounds in
turns with the code before) went, ms: interp 8.37 to 7.67, sort 87.3 to
85.1, sha256 37.95 to 37.32, micro/i64 13.12 to 12.90, micro/mandelbrot
10.53 to 10.28; json, regexp, bytes and fib level, gzip 16.38 to 16.60.
The Go program's code is 20 KB larger (18.87 MB) and the `loops` phase
took 3.96 µs a function where it took 3.00; laid out again only where an
if's second successor is the deeper, it takes 2.5. Starting
each loop's code at a 64-byte line besides, as the JavaScript JIT does,
gained bytes 5% at one placement and cost interp 25% at every one (8.36
to 10.43 ms), loops that call left unaligned and all: its padding is run
on every entry into a loop, and it was dropped.

A back edge -- a jump to a block at or before its own in the layout,
reverse postorder -- counts down the poll counter and exits for a poll
when it runs out. The exit is the function's own, which its sites share,
back edges' and calls' alike: a site leaves where to go on in R11 (X16)
and jumps to it, and it keeps in the context's save area only the
registers the allocator gave out, then jumps to where the site left
(`Ctx.Resume`). Each site had its own on amd64, every register saved and
loaded again: 490 bytes for each call of the Go program's, 12 now, and
its code went from 36.0 to 24.1 MB. A call counts it down too, then checks, as the baseline
does, that the callee has native code, that its frame and reference slots
fit their chunks, and that the return stack has room, else calls through
Go (`ExitCall`), out of line. A call of a function through Go, there or
of an import, leaves the callee's index and its offset in two of the
allocator's registers, which hold nothing across a call (RAX and RCX, or
RDX for the one that keeps the return stack's entry; X1 and X2), and
where to go on in R11 (X16), and jumps to the function's exit for it,
which stores them with the rest of the exit's record, and, resumed,
loads the registers the code keeps and jumps to where the site left
(`Ctx.Resume`, which the callee leaves alone: Go runs it in a level of
its own). Each site had made its exit whole: a call's out-of-line code,
its poll's site, its arguments' moves and its results' included, went
from 124 bytes to 54 on amd64. The callee's entry, read from the link
table for the check, waits for the jump in a register of the allocator's
that no argument is in -- at a call every other value is in its slot or
dead -- rather than being read again. A call of the function by itself
jumps to the code's own entry, and its tail call likewise, in the frame
it has, with no check of a link or of the frame's room: the code runs, so
it is placed and no pass of the memory limit drops it (the baseline
README's Concurrency and memory), and it is the function's last tier.
Such a recursion runs on in the code even where a pass cleared the
function's link, and a call from elsewhere then comes through Go as any
other does. fib's optimized code went from 8.9 ms to 7.8 for these two,
from 38 instructions a call, on average over its calls and leaves, to 30
with the register that keeps the return stack (Registers).

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
callee or traps as the interpreter does. The checks have s0 and s1 (on
arm64 X16 and X17, the table kept in D30 meanwhile). The callee's index
and native entry, read for the checks, wait for the arguments' moves and
the jump in two registers of the allocator's that no operand of the call
is in -- the table's index included, which a call through Go reads --
and the return stack entry's third word is stored as its two halves, the
call's offset and the index; where there are not two such registers the
index waits in X15 (D31), as s0 is the moves', and the entry is read
again from the link table. indirect's kernel went from 19.0 ms to 17.1 for
it: 49 instructions a call to 42, the entry no longer a chain of loads
just before the jump, and the index no longer through an SSE register.

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
read before the bytes above it are written. A memory.copy of a constant
count from 1 to 64 bytes is loaded whole then stored, so that any overlap
copies right: pieces of 16 bytes through X15, X14 and as many of the
instruction's temporaries as the rest takes (`copyTemps`), the last
ending at the count's end, or, below 16 bytes, two pieces of the widest
word within the count through CX and R11, one at each end -- the
operands still in AX and SI and both ends checked first. micro/bulk's
16,384 copies of 64 bytes a round took 43% of its samples on REP MOVSB's
start; the round went from 16.8 to 10.5 ms. A count not constant of 32
bytes or fewer, and a fill's of 16 or fewer, are done so too, after
both ends' checks (`copyShort`, `fillShort`): two pieces of the widest
width within the count, one at each end -- 16 bytes through X15 and X14,
8, 4 or 2 through R10 and R11, or a byte -- both loaded before either is
stored; a fill's byte is repeated through a word by a multiplication.
REP MOVSB was a third of regexp's and json's samples in Go's memmove,
whose copies are mostly of a few bytes: regexp's steady state went from
23.4 to 22.3 ms optimized (23.8 to 22.8 tiered), json's from 21.4 to
20.7 (22.0 to 21.7), interp's 1% slower, the others level. On arm64 the operands go into
X13, X14 and X15, and the code loops over blocks of 32 bytes in Q31 and
Q30 (LDP and STP, the address moving past each block), then of 8 in X16,
then bytes -- a copy to a higher address that overlaps its source from
its end down, each block read before it is written; a fill DUPs its byte
into Q31's. They make no exit and no poll: the count is bounded by the
memory, as Go's copy of it in the engine is.

Go enters a landing pad (`exn_GOARCH.go`) when the handlers' records the
code returns (`wjit.Handler`) say one of its clauses takes an exception
that reached Go during an exit of the frame's level: one record for each
site and each construct around it, innermost first, over the site's
instruction alone, its clauses' pads' offsets, where Go leaves the
exception's numbers -- the call area (`Clause.Vals`, its size counting the
largest tag's) -- and a legacy catch's reference slot for its exception
(`Request.CaughtSlot` past the frame's own, which rethrow reads). Go drops
the frames above, stores the references in the label's positions' slots,
the numbers in the call area, the context's L, R and return stack's top
the frame's, and enters the pad at its offset: it loads what an exit's
resumption loads (L, memory 0's base, the poll counter; on arm64 the
memory's size and the return stack's next entry too), every value in a
register that lives across the site's call from its slot (`reload`), and
the exception's numbers from the call area, and goes on to its label.
Go searches each frame by the code it is in (`nativeFn.holding` of its
return address, or of the exit's): a frame that began in the baseline
code the optimized code replaced catches by the baseline code's records.

## SIMD

Each SIMD instruction is the baseline's sequence for it (the baseline
README's SIMD; `simd*_GOARCH.go` there): the same instructions, in the same
order, with the same operand orders -- the first operand the destination
on amd64, for the NaN rule's choice -- so that every result is the
interpreter's bit for bit, NaNs included. Only the registers differ: the
result's and the operands' are the allocator's, and the registers a
sequence works in -- the baseline's temporaries from its cache and its
scratch register -- are the instruction's temporaries (`lsra.Inst.Temps`,
`simdTemps`), which the allocator keeps apart from its operands and
result.

On amd64 most instructions are two-address: the result is computed in a
register holding the first operand (`vprep`) -- the result's own, X15 for
a spilled one, or X14 where another operand is in the result's -- the
others read from their own registers, or from X14, X15 or, for an
instruction of three v128 operands, a temporary of its own, where they are
spilled, constants, or in that register. On arm64 most are one instruction
from the operands' registers to the result's (`v3`, `v2`); a sequence that
writes its result before it reads an operand begins with a copy of one
(`vprep`: bitselect and laneselect from the mask, relaxed_madd from the
addend, a lane's replacement, narrow, i64x2.mul) or computes in a
temporary (pmin, pmax, the dot products). A v128 constant is made where it
is used: zeros and, on amd64, ones of registers alone, any other loaded
from the code's pool of them after the code, RIP-relative (LDR literal on
arm64, of a table of the assembler's that it places in a pool near the
loads in code longer than a literal reaches, `LoadQTable`). select of v128s is a branch around a move, as of floats on amd64;
arm64 has no vector CSEL. The loads' scalars move into the
vector register whole (`MOVQ`, `MOVD`; `FMOV` of the D or S view), which
clears the rest, then widen or splat.

relaxed_madd and relaxed_nmadd, where the interpreter fuses them (amd64
with FMA3 and AVX), would need VEX encodings: as in the baseline, Go runs
the interpreter's own code for them (`ExitInstr`, `simdExits`), their
operands staged in the call area two slots apart and the result read from
the first's, the instruction a call to the allocator. The engine keeps an
exit's code by the instruction's offset and its operands' slots, which the
two tiers' code of one instruction stage in places of their own.

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

The tier is on by default, at `DefaultOptThreshold`, 30,000 calls. When it
was made the default its compiles, once the cost of both tiers' compiles
was worked on, left a short program's first run faster than the baseline
alone for four of the six Go programs and slower for two (json 89 ms to
98, interp 37 to 39), and every later run about twice as fast (json 57 to
47, gzip 59 to 29); before that work they made json's first run 135 to 161
ms. Compiled in the background (b08051c5), every first run was the
baseline's or faster -- json 77 to 70, regexp 89 to 52, gzip 75 to 53 --
but interp's, 36 to 37. That is still so (Measurements: sha256 81 to 64,
regexp 54 to 50, interp 36 to 38), the baseline having since become faster
itself; interp's is the baseline code's counting of its calls: a threshold
no function reaches costs it as much (In the background).
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

A compile in the background is counted to `DefaultOptBackgroundThreshold`,
5,000 calls (20 polls), instead: the program does not wait for it, so what
the count weighs is the CPU the compile takes, which has fallen to a
quarter of what it was when 30,000 was chosen (0.17 ms a Go function, The
compile's cost). The countdown a function's baseline code starts from is
the one of the mode its Code compiles tier-ups in when it is compiled
(`codeJIT.optCount`); `SetOptThreshold` and `WASM_JIT_OPT=N` set both
counts. At 30,000, json's functions crossed the count run by run, from
the calls each run added -- 19, 17, 25, 20, 12 and 8 functions optimized
in its first six runs, each compiled within a few milliseconds of being
queued -- so that its second run was still 6 ms from its steady state
with no compile waiting for a worker. At 5,000 (the median of 7 rounds of
fresh processes in turns on CPUs 0-15 of the machine below, 16 runs each,
ms -- first run, its CPU time, second run, steady state, all the runs' CPU
time, peak RSS in MiB):

| program | first | CPU | second | steady | CPU in all | RSS |
|---|---:|---:|---:|---:|---:|---:|
| json | 58.9 -> 53.0 | 89.8 -> 130 | 26.0 -> 20.9 | 19.9 -> 19.1 | 482 -> 478 | 158 -> 178 |
| gzip | 45.8 -> 45.5 | 68.8 -> 72.7 | 17.7 -> 16.9 | 16.6 -> 16.5 | 333 -> 342 | 151 -> 152 |
| regexp | 42.4 -> 43.6 | 48.9 -> 67.7 | 21.8 -> 21.0 | 20.2 -> 19.7 | 389 -> 395 | 108 -> 118 |
| sha256 | 55.8 -> 54.9 | 58.3 -> 58.1 | 38.3 -> 37.5 | 37.7 -> 37.6 | 635 -> 629 | 98 -> 85 |
| sort | 110 -> 110 | 115 -> 116 | 86.1 -> 86.5 | 86.4 -> 86.0 | 1423 -> 1433 | 159 -> 169 |
| interp | 34.8 -> 35.4 | 37.6 -> 38.4 | 9.3 -> 8.7 | 8.9 -> 8.5 | 178 -> 180 | 86 -> 88 |

json's second run is Node's (20.6); its first run's CPU time is 40 ms
more, the compiles of later runs made in the first, and the CPU of all
its runs level. Counted to 10,000 its second run was 24.0 ms, to 3,000
21.6 and to 1,000 20.3, the first run's CPU time 118, 161 and 166 ms. On
the spot (GOMAXPROCS 1) nothing changed: sha256, regexp and gzip, 7
rounds, level within 1% but gzip's first run, 66.6 to 62.7 ms.

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

**Scheduling.** The exit -- the baseline code having counted
`DefaultOptBackgroundThreshold` calls (Tier-up) -- or Go's count of the
baseline code's polls and exits (`codeJIT.sample`), queues the function once (`tierUpLater`: its
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
would bring it: when the workers were added (b08051c5), compiled in the
background the Go programs' first runs took regexp 72 ms where on the spot
they took 58, gzip 82 and 66, json's second run 48 and 39. So by default a Code compiles on the spot where
GOMAXPROCS is 1 (asked at each tier-up), and in the background above; with
two the background wins (json's first run 83 to 69, gzip's 60 to 54).

**Measured** when the workers were added (b08051c5):
`internal/cmd/wasmbench -engine jit-base,jit-tiered -suite programs`,
median of 3 fresh processes, the tiered first run on the spot and in the
background, the baseline alone's in parentheses, then the second run, ms
(Measurements has the current figures):

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
threshold no function reaches costs it as much. sort's on arm64 was
neither the counting (459 ms with a threshold never reached, 455
without) nor the compiles on the program's goroutine (503 on the spot):
its optimized code was slower than its baseline code, by the stores of
the partition loop's phis (Register allocation). Since the baseline
made its calls through tables natively and the phis' constants were
stored on their edges, the arm64 first runs are, tiered in the
background (the baseline alone's in parentheses), ms: sha256 141 (211),
sort 186 (269), json 119 (121), regexp 93 (123), gzip 100 (106),
interp 79 (76).

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
a block's values, phis and edges, a switch's table, the constants
`narrow` makes, a copy's argument -- from arenas (`arena.go`), both
emptied for the next compile. What is by value or block is a table by
ID, and the lists a compile makes are copied an element at a time: a
copy of a slice of pointers, or an append of one, is a call of the
runtime's copy, which a list of a few costs more than its stores. The
locals' values at the blocks' ends are a `grid` (`table.go`): a
row of a column for each local for every block, its entries those marked
with the compile's stamp -- the Go program's functions have fewer than 32
locals and about 600 such pairs each; `repairPhis`' values of the
threaded block's phis at the blocks' starts are an array of an entry for
each phi and block, where they make 65,536 or fewer; the edges
`loopDepths` cuts are marks by edge; `bce`'s ends checked of memory 0's
addresses are an array by ID. A table a pass leaves as it found it --
GVN's heads, popped as its walk leaves their blocks, and `bce`'s ends,
put back as its walk does -- is not emptied again for the next compile.
The maps that are left -- those two where they would be too large, `bce`'s
ends of the other memories, the lowering's trap stubs, made for the
function's values at once, and poll exits -- are `u64map`s,
open-addressed by a uint64 key, a
slot an entry only while its stamp is the table's, so that emptying one
is a new stamp where a Go map's clear costs its capacity. The allocator
is an `lsra.Allocator` kept there, with its own tables and arenas, and
the assembler. A warm compile of the Go program makes 5 objects, its
code's copy and its record among them.

The passes walk what may change: the dominator tree is kept until the
graph changes (`order`), its dominance a test of its walk's numbers, and
its iteration stops after a first walk that found every predecessor's
dominator known; GVN's table is a hash table of chains whose entries are
popped as the walk leaves their blocks; `copyelim` stops after the phis
where no copy is left, and rewrites the uses and drops the copies in one
walk, of the values from the first block a value was made a copy in
(`Block.copied`) -- each block is after its dominators, and a value's
arguments but a phi's in its block or before it; after the second gvn it
leaves gvn's copies to `chains`, whose walk resolves the values'
arguments and drops the copies as it goes, so that what is left is the
phis', branches' and results' arguments; the uses are counted in one walk
where the blocks are as `order` left them, each value's count cleared
before any use of it is counted (`computeUsesInOrder`); `removeUnreachable`
removes what `order`'s walk did not reach; the walks' stacks are cleared
as they pop, not to their capacities, which a large function's compile
grows for every compile after it; `thread`
removes the predecessors it threads together, counts the uses only where
a block may thread and an edge into it would be threaded, and after a
threading counts them again only where a block's values are among those
it gave uses or took them from -- the arguments its edges brought into H's
phis and the target's phis read; `dce` drops values only from the blocks
it found dead ones in, and counts the uses again only after removing
phis that only themselves used; the last pass finds the natural loops --
over a dominator tree the split edges would have it compute again --
only for a listing, which alone prints their headers: the allocator
weighs uses by `loopDepths`' depths, which are all 0, without a
component found, where every edge goes forward in the blocks' order. The
lowering numbers only the
values the allocator sees, densely and in the order of their IDs
(`Value.vreg`), so that the allocator decides as it would by ID; finds
each phi's place once for the edges into its block (`phiDst`,
`phiStore`), and leaves out a phi's move of a value where it already is;
takes a block's split moves in order (`Result.GapsOf`); and its cold code
is data (`stub`), not closures. What is made many times a compile -- an
allocator's block and instruction (`lsra.Inst.Set`), a register's web, a
construct of the builder, an entry of GVN's table -- is set field by
field in place: a composite literal is built aside and copied in. An instruction the
allocator sees is its registers in one list -- uses, definitions,
temporaries -- a hint, a call flag and the index of its clobbers among
the function's few (`lsra.Inst`, 40 bytes). The allocator's liveness
follows the paths from the uses (Register allocation); what a register
lives across is counted by sums over the clobbers, a table of the
clobbers before each position giving each range's; a few ranges are
tested and merged against many by searches, and lists one of which ends
before the other begins found apart from their bounds; the webs and the
pairs to coalesce are ordered by counting sorts, the webs once where the
scan takes none apart and moves no start; what splitting knows of a web --
its ranges as they were, its holes, its events -- is apart from it
(`webSplit`), made for the webs it plans holes in; the events, each
register's uses and definitions by position, are found in one walk and
placed by a counting sort, and a web's merged from its members' runs.
The assembler writes each jump's displacement that keeps 32 bits as it
moves the code down over what the shortened ones remove, and a label's
place in the code made when it is asked for.

`TestOptCompileCorpus` measures it (Validation). On the Go program
`internal/cmd/wasmbench` builds -- 3,011 functions, 3.77 MB of body -- a
compile takes 168 µs a function, 134 ns a byte of body, and allocates
12.4 KB and 5 objects (over 40 repeated compiles; 11 KB of it the code's
copy); a first compile, in a workspace still growing, 169 µs, 27 KB and
8 objects. 7ca010e4, before splitting, took 167 and 170 µs and allocated
21 KB and 115 objects, 49 KB and 149; 1bf325a3, which split, 191 and 190
µs; before the compile's cost was first worked on it took 398 µs and
allocated 627 KB and 5,394 objects. By phase, µs a function (in turns, on
one machine):

| | build | passes | describe | liveness | ranges | webs | scan | slots | emit, encode |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| 7ca010e4 | 21 | 64 | 11 | 7 | 8 | 16 | 7 | 4 | 24 |
| 1bf325a3 | 22 | 65 | 12 | 6 | 8 | 19 | 18 | 6 | 28 |
| now | 20 | 55 | 9 | 6 | 9 | 15 | 17 | 6 | 27 |

-- the passes now: thread 15, gvn 12 (two runs), copyelim 10 (five),
dce 5, check 4, loops 3.5, bce 3, fold 3. The largest functions cost the
most a byte (342 ns for one of 16 KB of body).

The chains, the layout and the holes over calls made the compile 6%
slower, 163 to 173 µs a function: `chains` 4.0, `loops` 3.0 to 3.9, the
scan 16.4 to 18.1 -- the holes over calls find the events in functions
that evict nothing. The work on the walks above -- the values rewritten
from the first block a copy was made in, the chains' walk doing the
copies' rewrite, the layout and the loops' components skipped where
nothing needs them, the stacks cleared as they pop, and what is made many
times a compile set in place -- took it to 159 µs, below what it was
before them, the code the same, function by function, on amd64 and
arm64: copyelim 10.1 to 8.0, webs 15.4 to 13.7, describe 9.8 to 8.4,
slots 5.9 to 4.9, loops 3.9 to 2.5, gvn 12.9 to 11.6, thread 12.5 to
11.2, `chains` 4.0 to 3.4, the scan 18.1 to 17.2.

Splitting (Register allocation) cost the Go program's compiles 15%, 172
to 198 µs a function: the scan 7 to 19 µs -- the holes' planning, with
each register's uses and definitions by position found once, at a
function's first eviction, and each split web's, with the sums of their
weights, at its first -- the result's moves 4 to 8, and the emission of
them 22 to 27. 856 of the 3,011 functions change, their code 0.2% larger
in all; the spec scripts' functions do not split, and cost what they
did. The work above took the rest of the compile down by about as much,
the splitting kept and the code the same, function by function.

A compile in a workspace its function outgrows -- a large function's
first in a process, or one after the collector has emptied the pool --
allocates what the workspace grows by, and that, more than its work,
makes it dearer: gzip's function 2211 (17.7 KB of body) takes 11-13 ms at
its first compile in a new process and 5-8 at the next, allocating 20 MB
(23 before the locals' grid). `TestOptCompileCorpus` logs the first
compiles apart (with `WASM_OPT_ONLY` naming one function, its first
compile is cold, the repeated ones warm). The lists made a block at a
time come from arenas, each as long as it will be (the allocator's view
of the blocks: their edges, phis and instructions); the parts of a web
taken apart share one list of hints, where each had its own of all of
them; a split web's holes and ranges change in their arrays; and an
arena's or chunks' first buffer is a couple of kilobytes, not 256 values,
each chunk after it twice the one before. What is left of a large
function's growth is mostly its liveness sets -- a block's live-in and
live-out sets are bits for each register live across a block boundary,
so blocks times such registers -- its ranges and its values. Kept across
collections rather than in the pool, a workspace saves the Go program's
first compiles little (27.0 KB a function, against 27.0 to 31.8 as the
collections fall): their bytes are the workspace growing once to the
largest function's size.

The Go program's tiered first runs at GOMAXPROCS=1, which compile on the
program's goroutine, lose to the baseline's where they tier big
functions up: json 84 ms against 68, gzip 62 against 53, interp 40
against 39 (the others gain: sha256 63 against 85, sort 121 against 152,
regexp 55 against 59; the fastest of four rounds in turns; json 83, gzip
64 and interp 44 at 1bf325a3). That is the compiles: with the threshold
out of reach the first runs are the baseline's (json 65-71 ms, gzip
54-56, interp 41-42), so the counting of calls costs little, and json's
twenty tier-ups and gzip's fifteen take 12-18 ms of the run. A threshold
of 100,000 calls left json's and gzip's first runs level with the
baseline's but took the compiles into their second runs -- gzip's 24 ms
to 37-39, json's 33-40 to 45-56 -- so the threshold stays. At the default
GOMAXPROCS, compiling in the background, every program's first run is
the baseline's or faster (json 65 against 66, gzip 53 against 55, interp
38 against 40, regexp 47 against 55).

## Instructions in Go

What the tier does not build natively does not keep a function's baseline
code: the interpreter's code runs the instruction, as the baseline's exit
runs it (the baseline README's Instructions in Go), and the function is
optimized all the same. The builder makes it an `OpGoOp` marked
`goInterp` (`goInstr`), its numeric operands its arguments, its offset in
the module's bytes in `Aux2` -- an inlined body's instruction reports an
inlined site, which the engine reads the offset back from too (`leaf`) --
and it is lowered as the other instructions Go runs, a call to the
allocator: its numeric operands staged in the call area, each at its place
among its operands (`stageInterp`), `RArgs` set where it takes or leaves a
reference, which stays in its position's reference slot, then
`ExitInstr`, its number read back from the call area's first slot. What
it writes -- memory, a global, a table -- the builder forgets it knows.
`wjit.Effect` says what it takes and leaves. The tier runs so:

| Instruction | |
|---|---|
| array.fill of v128 elements | |
| popcnt | on an amd64 CPU without POPCNT (`Request.Features`) |
| ceil, floor, trunc, nearest | on an amd64 CPU without SSE4.1 |
| every instruction `Effect` takes | under `Request.ExitAll`, for tests (`WASM_JIT_EXITS=all`) |

## Refusals

A function is refused -- it keeps its baseline code -- when it has:

| Reason | What |
|---|---|
| `v128` | a v128 value, or a SIMD instruction, on an amd64 CPU without SSSE3, SSE4.1 and SSE4.2 (below) |
| `frame` | more spill slots than its frame has below the call area |
| `size` | an address the arm64 assembler cannot keep in reach (the baseline README's Refusals): none the tier makes |
| `other` | an instruction neither the tier nor an exit runs: none validation takes |
| `internal` | a panic of the compiler's |

A function with v128 values on a CPU without the three is the baseline's:
its vector registers' moves, constants and parallel copies are SSE2's, but
every SIMD instruction it has would be a call to Go, which the baseline
code runs as well, and the tier would gain only around them. `checkNoV128`
refuses it before it is built, by the request's features
(`WASM_JIT_CPU=base` has the spec run so on any machine); the process's CPU
is checked again wherever a v128 is typed (`typeOf`).

A function refused for its frame keeps its native baseline code; one
that inlined or forwarded globals is compiled again without (Compile).
The frame is the engine's, sized before any code calls the function: a
native caller checks its callee's room by the size it was compiled with,
and Go makes the frames of the calls it makes for the code past it. An
optimized function with spill slots of its own past it would need every
caller compiled before it, and Go, to know of them, so the tier takes the
frame as it is. The spec's functions and the programs' fit it; with every
instruction run in Go (`WASM_JIT_EXITS=all`), every value live across an
instruction spilled, three of the spec's do not.

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

Two knobs, read when the package starts, drive the allocator for tests
and comparisons: `WASM_OPT_SPLIT=off` allocates without splitting, the
code then the same as before splitting came, and `stress` splits
wherever the allocator can -- a web scanned takes a register whatever
the density of those in its way, which take holes whatever they cost;
`nodisplace` splits as on arm64 (`lsra.Config.Displace` off);
`WASM_OPT_REGS=g,f` gives the allocator its first g general and f float
registers alone. The spec scripts' functions are small and split little
(1,051 holes in all, under `stress`): the spec, the engine's tests and
the differential are run under `stress`, with 3 and 6 registers too.

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
written at its parent. Hashed, the compiles are given the same addresses
of the write barrier and its routine in every build, and the layout's
addresses the code holds (the array classes' table, the i31s' word) are
zeroed before the hash, so that two builds' hashes compare; built for
linux/arm64 and run under QEMU, the test compares arm64's code the same
way. `WASM_OPT_ONLY` limits the corpus to the functions
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
functions in SSA form, a third of them allocated with
`lsra.Config.Displace` and a third with `StressSplit`, by running each on its locations
(`checkResult`): what each register and slot holds at each point as the
lowering's code would leave it -- definitions, split values' moves,
phis' and edges' moves as if all at once, calls and their reloads,
clobbers and temporaries -- met over the paths into each block, each use
reading a location that holds its value; and an Allocator kept from one
to the next against a new one. `TestCheckCatchesMutations` has the
check find allocations made wrong on purpose (holes that leave a
position out, a load dropped, a block's entry wrong, a reload dropped),
`TestLivenessMethods` the two liveness methods against each other,
`TestRanges` the range lists' tests and merges, `TestCoalesce` a loop's
phi web; `TestWorkspaceReuse` compiles in
workspaces kept and from goroutines at once (with -race);
`TestOptTierUp` the counts, the replacement mid-recursion and the poll
samples, `TestOptTierUpByExits` the exits'; `TestOptCallIndirect` every
kind of element a native call_indirect meets, `TestOptGC` the baseline's
GC instructions' cases (`TestJITGC`) on this tier, idle and with the
collector marking, `TestOptGCUnderCollection` its list relinked while the
collector runs (again under `GODEBUG=gccheckmark=1`), `TestOptGCPressure`
fields and elements with more values live than registers, across the
allocations, `TestJITSameWorkWhileMarking` both tiers, `TestOptExceptions`
the baseline's exception tests (`TestJITExceptions`) on this tier, idle and
with the collector marking, `TestOptExceptionsLive` values more than the
registers live across throwing calls in nested try_tables and legacy
trys, `TestOptExceptionsAcrossTiers` a recursion whose frames in the
baseline code and in the optimized code each catch and throw again,
`TestOptExceptionsV128` a tag's v128 thrown and caught and v128s live
across throwing calls and allocations, every store through the barrier
routine too,
`TestOptRefs` references
in every place, calls and tail calls of every kind with them, idle and
with the collector marking; `TestJITSIMDBits`, `TestJITRelaxedChoices`
(amd64) and `TestJITSIMDValues` run every SIMD instruction and v128s in
every place in the optimizing tier as in the baseline, each instruction
alone and with its operands one value, its first operand live past it,
and its operands or its result spilled, and `TestOptSIMDMemoryRandom`
random SIMD loads and stores of four kinds of memory near their ends,
among more v128s live than registers; `TestParallelV` parallel moves
whose slots meet in part; `TestOptBulkMemory`
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

`internal/cmd/wasmbench` at 3e0c4fec, median of 5 fresh processes, each
one run after one warm-up run, ms (Ryzen 9 9950X3D2, Windows, 32 threads;
`jit-tiered`, wasmbench's default `jit` engine, is the baseline with the
tier at `DefaultOptThreshold` in the background, `jit-opt` every
function the tier accepts optimized at its first compile; the first run, in
parentheses, pays the compiles). docs/wasm.md's Benchmarks has the same
run beside wazero and Node, and the runs at GOMAXPROCS=1, where the
tier-ups are compiled on the spot.

| workload | interp | jit-base | jit-tiered | jit-opt |
|---|---:|---:|---:|---:|
| micro/fib | 114 | 9.95 | 3.17 | 3.45 |
| micro/loop | 67.2 | 14.2 | 14.1 | 14.1 |
| micro/i64 | 97.7 | 19.7 | 12.6 | 12.5 |
| micro/mandelbrot | 93.2 | 11.3 | 10.6 | 10.6 |
| micro/nbody | 64.3 | 6.39 | 3.37 | 3.40 |
| micro/bytes | 77.5 | 8.76 | 5.43 | 5.73 |
| micro/bulk | 101 | 16.9 | 11.2 | 10.9 |
| micro/indirect | 196 | 20.4 | 15.9 | 15.8 |
| micro/brtable | 113 | 42.8 | 42.6 | 42.1 |
| micro/globals | 77.0 | 26.5 | 11.0 | 10.9 |
| micro/gc | 217 | 80.1 | 90.9 | 89.7 |
| micro/arrays | 60.2 | 25.9 | 31.4 | 31.0 |
| micro/simd | 18.6 | 2.92 | 2.92 | 2.91 |
| programs/sha256 | 780 | 63.9 (79.4) | 38.1 (55.5) | 37.4 (121) |
| programs/sort | 679 | 127 (143) | 86.0 (109) | 85.2 (185) |
| programs/json | 461 | 34.6 (58.6) | 25.3 (58.2) | 18.4 (167) |
| programs/regexp | 303 | 33.9 (52.5) | 21.1 (41.3) | 19.7 (126) |
| programs/gzip | 191 | 23.8 (46.0) | 17.5 (45.5) | 16.3 (149) |
| programs/interp | 164 | 22.4 (35.9) | 8.32 (34.7) | 8.67 (87.7) |

The Go programs' steady state, once every function that will be compiled
is (`-warmup 15 -reps 10`, median of 3 processes, ms):

| program | jit-base | jit-tiered | jit-opt |
|---|---:|---:|---:|
| sha256 | 64.4 | 37.0 | 37.1 |
| sort | 126 | 85.7 | 85.1 |
| json | 34.3 | 19.2 | 17.9 |
| regexp | 33.7 | 19.9 | 19.5 |
| gzip | 23.5 | 16.4 | 16.0 |
| interp | 21.9 | 8.49 | 8.30 |

The tier takes 32 to 62% off the baseline's steady state. The baseline's
own column is half or less of what it was in this README's earlier table
for sha256, regexp and gzip (144, 70.8 and 53.0 ms), the baseline having
since come to make calls through tables and references and the bulk
memory instructions natively (1db2aac1, 5c074edf), as the tier did
first. json's second run, 25.3 ms tiered, is not
yet its steady state: tier-ups are still being compiled in the background
then.

What the Go programs gained, in the order it came (steady state,
optimized, ms): the calls through tables and memory.copy natively (gzip
52 to 24, sha256 145 to 69 -- a third to a half of their time had been
those exits, in both tiers); jump threading of the dispatch loops (regexp
64 to 29, json 42 to 30, its hot functions no longer refused for their
spills' slots); the allocator's coalescing and spills by density (interp
21 to 12, sha256 75 to 47). The kernels gain where values stay in
registers across a loop (nbody 1.8x, fib 1.3x, bytes 1.3x); loop, i64
and mandelbrot are bound by one multiply's or add's latency each
iteration, as the baseline is, and simd by its accumulator's chain of an
add, a shift, an xor and a shuffle, which both tiers keep in a register;
gc and brtable spent their time in Go and
in the switch: gc, binary trees of structs, now takes its structs from
pools Go fills (8d179ac8, the baseline README's GC objects), 247 to 99 ms
optimized and 240 to 82 in the baseline, the rest Go's allocation and
collection; arrays, as `arrays` makes them, from pools by class too
(9c416599, af732b61): 67 to 31 ms optimized, 65 to 26 in the baseline. The tiered first runs are the baseline's or better but
interp's and gzip's (+2% and +1%), the baseline code's counting of calls;
json's is level (58.6 to 58.7 ms). Compiled on the program's goroutine they were +10% and
+4%, and +19% and +17% before the compile's cost was worked on (see
Tier-up).

Splitting values' lives (Register allocation) gained, the steady state's
median runs (`-warmup 15 -reps 10`) in 6 fresh processes against as many
of the code before it, in turns, ms: optimized, interp 11.3 to 10.5,
regexp 25.6 to 24.2 and json 23.1 to 22.5; tiered, interp 11.5 to 10.6,
regexp 25.6 to 24.6 and json 24.0 to 23.4; sha256, sort and gzip level
in both. The micro-benchmarks are level: their loops have registers
enough. On arm64 (GitHub's `ubuntu-24.04-arm`, 3 rounds of 3 fresh
processes in turns, `-warmup 1`) every program and micro-benchmark was
level, both tiers, on the code splitting was first written on. The optimized first
runs, which pay every compile at once, are 4 to 9% slower (The compile's
cost); the tiered ones, compiling in the background, level.

The checks and code of Go's memory accesses (Checks across phis and
through a block, Lowering: the limit, the short copies, the masks, the
overwritten global.sets, eqz of eqz) took, against bed71e54, the median
of 3 rounds of 5 fresh processes in turns, on CPUs 0-15 of the machine
above, ms -- second run, tiered then optimized: json 27.4 to 26.4 and
21.2 to 19.1, regexp 25.1 to 21.8 and 23.8 to 20.2, sha256 41.9 to 37.9
and 42.3 to 37.4, sort 89.0 to 85.1 and 89.3 to 84.5, gzip 18.8 to 17.9
and 17.8 to 16.4, interp 10.1 to 8.5 and 9.8 to 8.6; steady state
(`-warmup 15 -reps 10`), json 21.8 to 19.8 and 20.9 to 18.9, regexp 24.0
to 20.4 and 23.6 to 19.9; first runs and the baseline's level, the
micro-benchmarks level. Node 26 on the same CPUs: json 20.4 (TurboFan
alone 18.6), steady 17.3; regexp 23.3 (21.9), steady 21.3. The Go
program's code is 9% larger (36.1 MB), most of it each check's exact
check out of line, and its compiles were 2% longer, bce's 4.35 µs a
function where it was 3.14, encode's 4.09 where it was 3.32. The compile's
cost worked on after (The compile's cost: the arenas' copies, bce's ends,
the threading's and dce's counts, the assembler's displacements), they
take 167.9 µs a function where bed71e54's took 174.0 (the median of 5
runs of each in turns, `TestOptCompileCorpus`, 10 compiles of each
function): build 21.0 to 19.1, thread 14.8 to 12.5, describe 10.5 to
9.8, loops 3.5 to 3.0, scan 17.0 to 16.5, bce 3.14 to 3.22, emit 24.9 to
25.4, encode 3.32 to 3.43, the others within 2%; the code is the same,
function by function.

`BenchmarkOptMemories` (`wasm/internal/engine`) times ten passes over
64 KiB of a memory -- a load, a store and a load at offset 4 every 8 bytes
-- in the baseline code and optimized, µs: memory 1 61 and 54, an i64
memory 2 75 and 66, a shared memory 88 and 89, and an atomic add and a
compare-and-exchange every 8 bytes of the shared one 583 and 583. A shared
memory's accesses each read its size through its address and are never
taken as checked, and the atomics are bound by their locked instructions,
as the baseline's are.

On the spec scripts (`WASM_JIT=opt`) the tier compiles 6,756 functions
and refuses none, where it compiled 6,754 and refused 2 (reads of v128
fields) before it ran the instructions it does not build in Go; it
compiled 6,547 before it took the GC instructions
(188 refused) and the exceptions (107); 5,048 before it took SIMD and
v128s (1,499 refused); 4,149 before it took every memory,
every offset, the atomics and the instructions Go runs for tables and
segments -- memories other than an i32 one 514, offsets 14, atomics 80,
the table instructions that allocate or copy 218, memory.init and
data.drop 73 were refused -- and 3,914 before it took references and
tail calls; in 32 to 54 ms, 5 to 8 µs a function (on a busy machine). The Go program's 3,011 functions
compile in 0.17 ms each (0.40 before the compile's cost was worked
on; the baseline's, 0.12 ms).

On arm64 (GitHub's `ubuntu-24.04-arm` runner, Linux), measured when the
tier was ported to it (954d9a4e; wasmbench's runs as above, but median
of 3, the first run in parentheses, ms), the tier gains as on amd64; the
micro-benchmarks were, baseline then tiered, fib 30.0 and 18.5, loop 25.9
and 17.9, mandelbrot 38.3 and 20.2, nbody 19.9 and 10.8, bytes 31.1 and
17.4, bulk 263 and 24.7, indirect 1,889 and 55.1, brtable 55.3 and 46.5,
simd 14.3 and 6.26 (medians of 5 processes of 3 runs, once the tier
compiled SIMD, 26dd564d; before, it refused the kernel), and i64, globals
and gc level. The Go programs, measured again once the baseline made its
calls through tables natively and the phis' constants were stored on
their edges (Register allocation; medians of three medians of
3; before the phis', sort 237 (270), 268 (306), 267 (465), and interp
50.0 (75.3), 50.0 (79.3), 49.9 (207)):

| program | jit-base | jit-tiered | jit-opt |
|---|---:|---:|---:|
| sha256 | 183 (211) | 108 (141) | 107 (274) |
| sort | 237 (269) | 139 (186) | 138 (338) |
| json | 75.7 (121) | 60.0 (119) | 44.1 (349) |
| regexp | 89.3 (123) | 53.7 (93.0) | 50.7 (267) |
| gzip | 64.3 (106) | 41.2 (100) | 37.3 (308) |
| interp | 50.0 (76.1) | 19.4 (79.2) | 19.0 (179) |

The 11,904 functions `TestOptCompileCorpus` compiles of the Go program
and the spec scripts lower to 20.6 MB of amd64 code and 17.2 MB of
arm64's, where they were 38.0 and 19.6 MB before the polls' and the
calls' exits were shared, the trap sites' offsets left in a register,
`MemLimit` within a byte's displacement and the switches' tables made
of jumps to their targets (each in Lowering); the Go program's 3,011
alone, 36.0 to 18.9 MB on amd64 (11,972 to 6,282 bytes a function) and
18.1 to 15.7 on arm64. The steady state and the first runs, tiered and
optimized, and the compile's every phase stayed level with them --
interp's steady state aside, which is bimodal by where its hot loop
falls in its 64-byte line (0.8x to 1.3x), the code's being no worse over
the placements. A longer scan for widening (`widenScan` 256) took 7 of
the Go program's 170,660 checks away, 100 bytes: what ends a run is an
effect, not the scan's length.

## Next

- OSR into optimized code: a function called once whose loop runs long
  (sha256's block function) runs its first call in the baseline code,
  and is optimized only for its next.
- Splitting into another register: a split web keeps its one register
  and gives it up over holes, and a web spilled whole is in its slot
  everywhere. Over the Go program and the micro-benchmarks, of the
  stretches in slots that hold uses weighing 8 or more (2,321, their uses
  weighing 750,000), 85 have a register free over all of them in the
  allocation as made (2.8% of the weight); within each block, where a
  free register would take a load and save the uses after the first, it
  is 12.8% of the weight. A part re-allocated from where its hole ends, as
  Wimmer and Mössenböck's splitting re-scans the part after a split, would
  reach those; the holes' cost weighs a use from a slot as a move, where
  on amd64 most such uses are memory operands.
- The compile's cost, 0.17 ms a Go function, spread over its phases
  (The compile's cost), none above 15%: the holes' planning (the scan,
  17 µs where it was 7 before splitting), copyelim's five runs, the
  trivial phis the builder makes and the first pass removes (about a
  fifth of the values built), and a range for every block a register is
  live through -- the position between two blocks is in neither, so that
  a register live across 100 blocks has 100 ranges, 42% of all, where
  merged they would change the density the allocator decides by. A large
  function's liveness sets are blocks times registers, most of its first
  compile's growth. In the background the compile costs the first run
  little, but it is CPU an embedder running a runtime on each core pays,
  and at GOMAXPROCS=1 it is what json's and gzip's first runs lose.
- Inlining of callees that hold references, as micro/gc's tree walk's
  are, and of calls through tables and references whose callee is known.
- v128 constants as values: a shuffle's mask and a constant operand are
  loaded from the pool where they are used, in every turn of a loop.
- Bounds checks: hoisting a loop's checks of an induction variable whose
  bound is not a constant (a guarded loop without checks, the checked
  one its fallback). The Go program's code still checks 171,000 of its
  221,000 accesses of memory 0 after bce, a sixth of json's and regexp's samples: most are the first
  access of a value in a block, a stack pointer read again after each
  call or a pointer loaded from memory.
- In the code what Go runs for it now, as the baseline makes them:
  memory.size, memory.copy and memory.fill of memories other than an
  unshared i32 memory 0, and table.fill, through the barrier.
