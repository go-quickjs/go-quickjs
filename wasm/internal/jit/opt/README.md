# The optimizing tier

The WebAssembly JIT's second tier (`wasm/internal/jit/README.md` is the
first's, the baseline's, and the rules both keep). It builds SSA from a
validated function body, optimizes it, allocates registers over it with a
linear scan and lowers it to native code that keeps the baseline's frame,
context, entries, exits and calls, so that its code takes the baseline
code's place in the link table and the tiers -- and the interpreter --
call each other in every direction.

It is built on amd64 alone (`lower_amd64.go`); on arm64 `Compile` refuses
every function (`arch`) and the baseline runs everything. It is on by
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
| `lower_amd64.go` | the lowering: the allocator's view of the function, the code |
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
stores of memory 0 (their width, sign extension, offset, and `NoCheck`
once a dominating access checked as far), the globals -- `global.addr`,
the address of a global's value, made once in the entry block, which
`global.get` and `global.set` read and write through -- calls of a
function of the module (`call`) and through a table (`call_indirect`),
whose results `result` values read, and `goop`, an instruction Go runs
(memory.size, memory.grow, memory.copy, memory.fill) -- but memory.copy
and memory.fill of memory 0, which the code does itself and which are
not calls (`BulkInCode`).

An operation is pure -- no effect, no trap, so shared, moved and removed
at will -- or not: a division may trap unless its divisor is a constant
other than 0 (and, signed, -1); a truncation of a float may trap; a load
traps past the memory's end; a mutable global's read depends on the stores
before it; stores, global.set and calls have effects.

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

The operand stack is a stack of values. A label's values are phis made
with the label: a loop's parameters in its header, the results of
anything else in the block after it, each edge to the label appending its
values. A `br_if` ends its block with an if; `br_table` with a switch;
`br`, `return` and `unreachable` end it and the rest of the construct is
skipped, as the baseline skips it. A branch to the function's body is a
return block. The code after a loop begins a block of its own, and a
construct whose end nothing reaches makes the rest of its parent dead.

Within a block the builder knows what a global holds -- the last value
read or written -- and what memory holds: the last loads, and what a store
of a whole value wrote, a later load of the same address value, offset,
width and type reading it. A store forgets what it may overlap: accesses
at one address value whose bytes are disjoint, and accesses at constant
addresses whose bytes are disjoint, survive it. A call and an instruction
Go runs forget it all.

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
| `bce` | bounds-check elimination: an access is in bounds if a dominating access of the same address value checked an end as far; a memory never shrinks, so the check holds across calls and grows |
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
  find their operands, two slots apart, in the call area at the frame's
  top (from `argBase`, `nslots` less twice the most arguments or results
  of any of them) and leave their results there.
- A poll's exit keeps every register in the context's save area
  (`Ctx.Save`): nothing in the level runs while Go polls.
- A trap reads nothing of the frame.

Every other slot below `argBase` is the allocator's spill slot. A slot's
word holds an i32 or an f32 with its upper half clear, as the
interpreter's frames do, so that what Go reads of the call area is right
and a slot is copied whole: an f32 is stored with MOVSS and a zero upper
word.

The optimized code holds no reference: a function with one is refused.

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
| SI, R8, R9, R13, AX, CX, DX | the allocator's general registers, AX, CX and DX last (division's and shifts'; memory.copy and memory.fill take AX, CX and SI) |
| X0-X13 | the allocator's float registers |
| SP, BP, R14 | never written |

Native callers enter with R10 the return stack's next entry, which the
entry stores into the context; the return sequence loads it, pops the
entry and jumps to its address with BX the caller's L, as the baseline's
does. After a native call the callee leaves R10 as the call found it, and
the code stores it into the context again, where an exit of the callee
may have left another.

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
holes left out. A phi and an argument whose lifetimes do not meet are
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
(AX and DX for a division, CX for a shift by a variable, AX, CX and SI
for memory.copy and memory.fill) is not given to a web live across it. A
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

Lowering reads a spilled operand from its slot -- as a memory operand
where the instruction takes one (`OpMem`, `ImulMem`), else into a scratch
register -- and computes a spilled result in a scratch register and
stores it. The moves on an edge are parallel: each waits for those that
read its destination, and a cycle is broken through R10 or X14.

## Lowering

Each instruction compiles to the baseline's sequence for it where the
result's bits depend on the instruction chosen -- the floats (the
baseline README's Floats: the same SSE instructions, operand orders,
roundings, min/max, truncation bounds and unsigned conversions), so that
the results are the interpreter's bit for bit -- with the operands where
the allocator put them. copysign is done on the bits in general
registers. An integer comparison that is the last value of a block and
its branch's condition alone is fused with the branch.

A memory access computes its end, `address + offset + size`, in R11 and
compares it with the context's memory size, unless `NoCheck`; an offset
plus a size past 2^31-1 is refused (`offset`). The access is `[R15 +
address + offset]`, the address in a register (an i32 there has its
upper half clear).

A back edge -- a jump to a block at or before its own in the layout,
reverse postorder -- counts down the poll counter and exits for a poll
when it runs out. A call counts it down too, then checks, as the baseline
does, that the callee has native code, that its frame and reference slots
fit their chunks, and that the return stack has room, else calls through
Go (`ExitCall`), out of line.

A `call_indirect` counts it down and calls natively too where the
table's element -- within the table, not null -- is a function of the
call's type exactly (its canonical index, `Layout.FuncType`), defined by
the instance the code runs for (`Layout.FuncInst` against `Ctx.Inst`),
with native code (the link table at its index, `Layout.FuncIndex`) and
room for its frame, which the engine's table of each function's frame
says (`Ctx.Frames`: a function that holds references has one past any
chunk's end). Anything else -- null, past the table, another type or a
subtype, a host function, another instance's, no native code or no room
-- goes through Go (`ExitCallIndirect`), which finds the callee or traps
as the interpreter does. The callee's index waits in X15 while the
arguments are moved, as s0 is the moves'.

memory.copy and memory.fill of memory 0 are the code's own: the operands
into AX, CX and SI, which the allocator keeps free of intervals live
across them, both ends checked against the memory's size before a byte
is written, then REP MOVSB or REP STOSB with DI the destination's
address, the context kept in R11 meanwhile; a copy to a higher address
that overlaps its source goes from its end down, words then bytes, each
read before the bytes above it are written. They make no exit and no
poll: the count is bounded by the memory, as Go's copy of it in the
engine is.

## Tier-up

The baseline code of a function the optimizing tier may compile counts
its calls: at its entry it decrements the function's countdown
(`Request.TierCounter`, the engine's `codeJIT.counters`) and exits
(`ExitTierUp`) when it goes below zero, before its prologue, with the
frame as the call made it. The engine compiles the function with the
optimizing tier, places the code, stores it as the function's native code
and in the link table, and enters it at its Go entry for that call; native
callers, the interpreter and Go call it from then on. A poll that baseline
code exits for -- its loops or calls ran the poll counter out -- counts
250 calls, so that a function whose loops run long is optimized for its
next call, and each call and instruction Go makes for the baseline code
(`ExitCall`, `ExitCallIndirect`, `ExitOp`) counts one: a loop that calls
through Go leaves its polls to Go, and the optimized code makes some of
those calls natively. The frames that began in the baseline code run on in it: the
replaced code stays placed (`nativeFn.prev`), and an exit resumes in the
code that holds its address.

The count is `DefaultOptThreshold`, 30,000 calls, by default;
`Code.SetOptThreshold(n)` sets another: 0 compiles every function the
tier accepts with it at once, the baseline only those it refuses, and a
negative n turns the tier off. `WASM_JIT_OPT` sets it for tests and
benchmarks (a number, `on` for the default, `eager` for 0, `off`), and
`WASM_JIT=opt` is `eager` with the JIT on at every first call.

The count weighs the compile against what it saves, as measured on the
Go programs of `internal/cmd/wasmbench`. Compiling costs about 0.4 to 0.7
µs a byte of a function's body, 0.76 ms for each of the 182 functions
json compiles in its first run (138 ms in all), and the optimized code
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

## Refusals

A function is refused -- it keeps its baseline code -- when it has:

| Reason | What |
|---|---|
| `ref` | a reference type anywhere, a reference instruction, call_ref |
| `v128` | a v128 value |
| `simd` | a SIMD instruction |
| `atomics` | an atomic instruction |
| `gc` | a GC instruction |
| `exceptions` | try, try_table, throw, the legacy instructions |
| `tail` | return_call, return_call_indirect, return_call_ref |
| `table` | a table instruction |
| `bulk` | memory.init, data.drop |
| `memory` | an access of a memory other than 0, of an i64 memory or of a shared one |
| `offset` | an access whose offset plus size is past 2^31-1 |
| `frame` | more spill slots than its frame has below the call area |
| `size` | code past `internal/jit.MaxCodeBytes` |
| `arch` | any function on arm64 |
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
(cd internal/jit/verify && go test -tags quickjs_jit -run TestAMD64WasmOpt .)
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

`TestJITDifferential`'s `opt` pass runs the spec modules with every
function the tier accepts optimized and compares them with the
interpreter; `TestAllocateRandom` checks the allocator's results on random
functions by a liveness of its own, `TestCoalesce` a loop's phi web;
`TestOptTierUp` the counts, the replacement mid-recursion and the poll
samples, `TestOptTierUpByExits` the exits'; `TestOptCallIndirect` every
kind of element a native call_indirect meets, `TestOptBulkMemory`
memory.copy and memory.fill at every overlap and end; `TestThreadDispatch`
and `TestOptThreadDispatch` jump threading, the latter on 1,500 random
dispatch loops against the interpreter; `TestMarks` the profile's marks.

## Measurements

`internal/cmd/wasmbench`, median of 3 fresh processes, each one run after
one warm-up run, ms (Ryzen 9 9950X3D2, Windows; `jit-tiered` is the
default, the baseline with the tier at `DefaultOptThreshold`, `jit-opt`
every function the tier accepts optimized at its first compile; the
first run, in parentheses, pays the compiles):

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
| programs/sha256 | 1956 | 148 (174) | 51.1 (160) | 61.4 (205) |
| programs/sort | 1760 | 213 (243) | 116 (234) | 136 (301) |
| programs/json | 874 | 59.9 (135) | 59.5 (161) | 58.0 (297) |
| programs/regexp | 822 | 75.8 (122) | 34.7 (92.4) | 47.1 (173) |
| programs/gzip | 464 | 65.5 (108) | 31.5 (106) | 69.8 (182) |
| programs/interp | 544 | 27.5 (57.0) | 17.8 (66.6) | 30.2 (115) |

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
about 60 ms (json +19%, interp +17%; see Tier-up).

On the spec scripts the tier compiles 3,914 functions and refuses 2,928
(SIMD, references, i64 and shared memories, tables, exceptions, tail
calls), in 43 to 63 ms, 11 to 16 µs a function, where the baseline's
2,926 take 11 to 22 ms. The Go program's 3,011 functions compile in 0.40
ms each (the baseline's, 0.12 ms), json's 182 hot ones in 138 ms within
its first run.

## Next

- OSR into optimized code: a function called once whose loop runs long
  (sha256's block function) runs its first call in the baseline code,
  and is optimized only for its next.
- Splitting within a value's life in the allocator: a web is in its
  register or its slot throughout, so a long value is spilled for its
  whole loop where a short stretch of pressure needs its register.
- The compile's cost, about 0.4 ms a Go function, half of it the
  collector's: fewer and smaller allocations, and compiles in the
  background.
- Inlining of small callees; tail calls; call_indirect and memory.copy
  natively in the baseline too, whose Go programs spend a third of their
  time in those exits.
- Bounds checks: hoisting a loop's checks of an induction variable, and a
  block's checks of one base at its widest where nothing between can be
  observed.
- arm64's lowering.
