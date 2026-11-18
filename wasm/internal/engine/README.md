# The WebAssembly interpreter

The engine interprets a function as *register code*: its validated body
compiled in one pass to fixed-width instructions that name the frame's
slots as their operands, run by a small loop over a table of handlers. It
follows the JavaScript engine's register interpreter
([docs/register-interpreter.md](../../../docs/register-interpreter.md)) and
replaces the closure tree that interpreted every body before it
(compile.go's header), which stays for the bodies register code does not
cover yet and for comparison. The JIT (internal/jit and
[internal/jit/README.md](../jit/README.md)) compiles from the module's
bytes, not from either form, and shares the interpreter's frames.

This document is the design: what is built, what is decided for the rest,
and where it departs from the JavaScript design and why.

## Contents

- [Where it runs](#where-it-runs)
- [Why registers](#why-registers)
- [Instructions](#instructions)
- [The opcode space](#the-opcode-space)
- [The frame](#the-frame)
- [The compiler](#the-compiler)
- [The loop](#the-loop)
- [Traps and exceptions](#traps-and-exceptions)
- [Calls and returns](#calls-and-returns)
- [Polls, OSR and the JIT](#polls-osr-and-the-jit)
- [Offsets](#offsets)
- [The switch](#the-switch)
- [Where it departs from the JavaScript design](#where-it-departs-from-the-javascript-design)
- [Measurements](#measurements)
- [Native handlers](#native-handlers)
- [Open work](#open-work)

## Where it runs

`Code.interp` compiles a body when the interpreter first needs it, as
before: at the first call, or where native code ran the function from its
first call (`nativeFirst`, `boundRecord`), when a call first reaches the
interpreter. `compileFunction` tries the register compiler first
(`regCompile`) where the Code says so (`Code.reg`), and the closure tree
where the register compiler does not take the body; the record holds one
or the other (`compiled.reg`, or `compiled.body` and `epi`), and
`exec.interpret` runs whichever it holds. A body is taken whole or not at
all: a function never mixes the two forms, and calls cross between them,
and between either and native code, in both directions.

Today the register compiler takes the MVP's numeric core (below); with
`WASM_INTERP=reg` the spec suite runs 3,612 of its 6,842 interpreted bodies
on register code, every assertion passing. `ReadInterpStats` counts the
bodies of each form, the tree's by the register compiler's reason, and the
spec test logs them.

## Why registers

The tree runs an expression as nested closures: each operand that is not a
slot or a constant is an indirect call, every statement returns a signal
its block tests, and each node reloads what its caller had live, since Go
keeps nothing in registers across a call. A register instruction reads its
operands where they lie and writes its result to a slot, and the loop's
one indirect call is the only dispatch. `local.get`, `local.set` and
constants mostly vanish into operands, so the instruction count is close to
the number of operations: wasmbench's loop.wat kernel is seven
instructions a turn for its nineteen wasm instructions -- the fused
compare-and-branch, five operations each writing its local or temporary,
the branch back.

Wasm makes the design simpler than JavaScript's. Every operand's type is
static, so no handler has a slow path for another kind of value and every
numeric handler is a leaf: it reads two words, computes, stores one, with
no call, no frame and no write barrier (numbers are never pointers). There
are no caches to fill and no feedback to record: the JIT reads the module.

## Instructions

An instruction is one 64-bit word (reg.go):

```
bits  0- 7  Op  the operation, the index of its handler in regTable
bits  8-15  X   a small operand: an escaped operation's index (rExt)
bits 16-31  A   a slot, most often the destination
bits 32-47  B   a slot, or a 16-bit immediate (SB)
bits 48-63  C   a slot, a 16-bit immediate or a branch's offset (SC)
```

B and C read together are BC, a 32-bit immediate, index or offset. Every
field is decoded with one shift and one extension, the same for every
operation; no operand's kind is decided at run time. A slot is a 16-bit
index, `*rs(l, w>>16)`: a shift, a zero extension and an address. A
64-bit constant that does not fit BC takes the word after its instruction
(`rConst64`), the only two-word instruction.

The forms, by family:

- **Binary** `A = B op C`, and **immediate** `A = B op SC` for the
  integer operators a constant is commonly the right operand of (add, sub,
  mul, and, or, xor, the shifts, i32's rotations, every comparison). A
  constant on the left of a commutative operator is swapped to the right;
  of a comparison, the comparison is swapped (`lt_s(5, x)` is
  `gt_s(x, 5)`).
- **Unary** `A = op B`: the tests, counts, extensions, roundings and
  conversions. An operator that changes no bit -- the reinterpretations,
  `i64.extend_i32_u` of an i32 whose upper half is clear -- is no
  instruction: the compiler retypes the entry.
- **Compare-and-branch** `if A op B: pc += SC` and `if A op SB: pc += SC`
  for every integer comparison, i32 and i64; `br_z` and `br_nz`
  (`if A == 0: pc += BC`) for eqz and for a plain condition; `jmp`
  (`pc += BC`); `br_table` (`pc = tables[BC][min(A, last)]`).
- **Moves and constants**: `mov`, `const32` (zero-extended BC),
  `const32s` (sign-extended), `const64`.
- **Memory 0**, an i32 memory: `A = load[B + C]`, `store[A + C] = B`, C the
  memarg's offset; `memory.size` and `memory.grow`.
- **Globals**: `A = global[BC]` (32- and 64-bit, so that an i32's upper
  half is clear however the host set the global), `global[BC] = A`.
- **Control**: `select` (`if C == 0: A = B`, A holding the first operand),
  `call` and `return_call` (`sites[BC]`), `ret`, and `ret1` (`slot 0 = A`
  and return), the common return of one value in one dispatch.

The numeric operations, their handlers and what the compiler needs to know
of each (`regNumeric`, `regMemOp`) are generated (reg_num_gen.go,
internal/numgen) from the same table of operators as the tree's nodes, so
that the two forms' semantics -- NaN payloads, the truncations' ranges --
are one source.

## The opcode space

Wasm has about 550 instructions and the dispatch is 8 bits. The first table
holds what a hot loop runs, each operation its value; the rest go behind
escapes, each a second table of 256 indexed by X:

| Values | What |
|---|---|
| 0 | `rInvalid`, which panics; every unused value of every table is it |
| 1-17 | hand-written: `rExt`, moves, constants, branches, `br_table`, returns, calls, `select`, globals (reg.go) |
| 18-23 | free, for the hand-written operations to come |
| 24-240 | 217 numeric: every i32/i64/f32/f64 operator but the cold ones, 40 immediate forms, 40 compare-and-branches, 23 loads and stores of memory 0 |
| 241-255 | free |

`rExt`'s table holds `unreachable`, `memory.size`, `memory.grow` and the
numeric operators a hot loop rarely runs: the saturating truncations,
popcnt, copysign, f32's roundings and f64's nearest (20 of 256 so far).

The rest of the instruction set is planned so:

- **First table** (the free values): `call_indirect`, `call_ref`, the
  moves of a reference slot and of a v128's two slots, `ref.is_null`,
  `br_on_null`, `local`-to-reference moves, `throw`, and loads and stores
  with a 32-bit offset, by a second word.
- **`rExt`**: the bulk memory and table operations, the segments' drops,
  the other memories' and 64-bit memories' accesses (the memory in the
  word after), the reference operations, `rethrow` and `throw_ref`.
- **`rSIMD`**, an escape of its own: the ~250 SIMD and relaxed operations,
  two tables of 128 by X's low bit and seven bits -- a v128 operation is
  heavy enough that the second dispatch is a small part of it, and a
  ternary one (bitselect, relaxed madd) takes its fourth operand in a
  second word.
- **`rGC`** and **`rAtomic`**: struct, array, i31 and cast operations; the
  atomics, each table's operation in X.

Values are appended, never renumbered, once native handlers depend on
them; an operation goes in the first table only where a loop that runs it
is common enough to pay for a value, as the JavaScript design allots them
by frequency.

## The frame

Register code's registers are the frame's existing slots, which the JIT
shares, so nothing about frames changes:

```
numeric slot k            numeric local k (parameters first, a v128 two)
numeric slots nlocals+2h  operand stack position h (two each, a v128 both)
reference slot k          reference local k
reference slot rlocals+h  stack position h's reference
numeric slots from 0      the results, as layout() places them
```

The frame's size is the tree's exactly -- `nlocals + 2(maxHeight+1)`
numeric slots, the same reference slots -- since the compiler pushes and
pops as validation does, so `boundRecord`, `frameBound` and the JIT's
callee frames hold for either form. An i32 or f32 in a slot has its upper
half clear: every handler writing one writes `uint64(uint32(...))`, and
what comes from the host is narrowed as it enters a frame (`narrow`), as
before.

## The compiler

reg_compile.go, one pass over the body, after validation, with a symbolic
operand stack. An entry is a constant, or the slot that holds the value: a
local's, read in place, or its own position's. Unlike the tree's entries no
entry is pending work -- every instruction is emitted as it is read -- so
there is no evaluation order to keep, and the only hazard is a local read
in place and then written: `local.set` and `local.tee` first copy the
stack's reads of the local to their positions (`flushLocal`).

**Copy elision.** An operation writes its result to its position's slot,
and its entry remembers the instruction (`def`). A `local.set` or
`local.tee` of that value straight after -- nothing emitted between, no
label bound between, no read of the local on the stack -- rewrites the
instruction's destination to the local (`retarget`), and so does a return
of it, to slot 0. `local.get` is no instruction, and neither is a
constant: it is the immediate of the operation that takes it where a
16-bit form exists, and is loaded into its position's slot where one does
not (`read`).

**Fusion.** An integer comparison taken straight after by `br_if` becomes
a compare-and-branch in place: its word is rewritten, its operands moved to
A and B, the offset patched into C. An `if` takes the negated comparison to
its else. An `eqz` becomes `br_z` or `br_nz` of its operand. A float
comparison makes a boolean and `br_nz` tests it (its negation differs on
NaN; fused float branches come with their negations when a workload asks
for them).

**Constructs.** Each begins with every entry in its position's slot
(`flushAll`), so that a branch's values go to fixed slots and a loop's head
holds what native code's entry there expects. An `if`'s condition, where
the last instruction computed it, is moved past those stores so it can
still fuse: they write only positions below its own.

- `block` and `if` keep their branches to be patched (`fixes`) and patch
  them at `end`; a loop's head is a pc, so a branch to it is patched as it
  is emitted, backward.
- `br` moves the carried values to the target's slots, in increasing
  order, which never overwrites a value still to be moved, then `jmp`s.
- `br_if` with no values to move is one branch; with values, the negated
  branch skips the moves and a `jmp`. A `br_if` to the function's label is
  a conditional return.
- `br_table` is one instruction over a table of absolute pcs; a target the
  values must be moved for, or the function's, is reached through a stub
  laid after the table, one stub a target construct.
- `return` moves one result to slot 0 in the instruction that computes it
  or by `ret1`; several go to their positions' slots first, since a result
  slot is a local another result may be read from.
- Code after `br`, `br_table`, `return`, `unreachable` and `return_call` is
  skipped to the construct's end, as the tree does.

**Branches that do not reach.** A compare-and-branch's offset is 16 bits.
Where one does not reach, the body is compiled again with every
conditional branch a 32-bit `br_nz`/`br_z` of a computed condition
(`far`); unconditional branches and tables are 32 bits always.

**What is refused, for now.** `regBail` names it and the body goes to the
tree: a reference or a v128 anywhere, memories other than an i32 memory 0,
offsets past 16 bits, slots past 65,535, and the instructions not listed
above ([Open work](#open-work)). None of these is to stay a refusal: the
end state compiles every validated body, as the JavaScript design never
refuses one.

## The loop

```go
func regLoop(l, code unsafe.Pointer, pc int32, f *frame) {
	for {
		w := *(*uint64)(unsafe.Add(code, uintptr(pc)<<3))
		if pc = regTable[uint8(w)](l, w, pc, f); pc < 0 {
			if pc == regDone {
				return
			}
			pc = ^pc
			w = *(*uint64)(unsafe.Add(code, uintptr(pc)<<3))
			if pc = regSlowTable[uint8(w)](l, w, pc, f); pc < 0 {
				return
			}
		}
	}
}
```

A handler is given the frame's numeric slots, the instruction, its pc and
the frame, and returns the next pc, a branch's target, `^pc` for the
operation's slow function, or `regDone` (`math.MinInt32`) once the frame
has returned. Only the branches have slow functions: a branch back that
spends the frame's tick returns `^pc`, and the slow function checks the
context and native code and takes the branch
([Polls](#polls-osr-and-the-jit)). An escape (`rExt`) calls its table's
handler, a second dispatch the cold operations pay.

Slots are reached by `unsafe.Add` from the frame's first: the compiler
named only slots of the frame, which is given that many before it runs. A
pointer to an array of 65,536 slots, as the JavaScript loop uses, would
not do: a frame's slots are in chunks of 16,384 (`chunkSlots`), and a
conversion to a larger array fails `checkptr`. The code is read at a pc
the compiler made, past which no branch goes. The spec suite runs clean
under `-d=checkptr=2`.

## Traps and exceptions

A trap keeps the engine's panic, recovered at the call's entry (`invoke`,
`recovered`). It is the cheapest form in Go: the path that does not trap
pays nothing -- a handler that returned an error would test it on every
instruction, and the loop would test the handler's answer. To keep even
the trap's offset off the hot path, a handler traps with its pc marked
(`regAt`: the pc with bit 31 set; a module past 2 GiB, whose offsets
could have it, is left to the tree),
and the recovery makes it the module offset of the instruction the pc was
compiled from, through the innermost frame's register code (`offs`,
`exec.regOffset`). So a division's handler passes `pc | 1<<31` where the
tree's node passed a captured offset, at no cost.

Wasm exceptions will keep the panic too (`*Exception`, `delegation`), for
the same reason: a throw is rare and a handler that never throws pays
nothing. A body with a `try_table` or a legacy `try` will be run by a
catching entry of the loop: `regLoop` inside a function with a deferred
recover that, for an exception, finds the innermost handler whose range
holds the pc the frame was at (a table by pc, the try's catch clauses and
their label), restores the exec as the construct entered it (`tryState`),
stores the exception's values, and enters the loop again at the clause's
label. A body with no try runs the plain loop and pays nothing for any of
this.

## Calls and returns

`call` names a site (`regSite`): the function's index, the call's offset,
and the moves of the arguments from the caller's slots -- a local's in
place, or a position's -- to the callee's parameter slots, and of the
results from the callee's slots to the caller's positions. The handler
takes the callee's frame from the exec's chunks, makes the moves, and runs
the callee by `exec.run`, which keeps what it did for the tree: the depth
check and the stack overflow's frame, the call's poll, the record in
`exec.calls`, native code where the callee has it, and the interpreter in
whichever form the callee's record holds. A host function, or another
instance's, is the same site: the handler calls the host through
`hostCall` with the arguments in `Val`s.

`return_call` stores its arguments in the exec's tail call and ends the
frame with `sigTail`; `exec.run`'s `tailChain` makes the call in place of
the frame, as for the tree's. `call_indirect`, `call_ref` and their tail
calls will be sites with the callee found by the existing checks
(`indirectCallee`), after the arguments' moves are read.

A frame that returns leaves its results in slots from 0, where its caller
and native code look for them.

## Polls, OSR and the JIT

**The tick.** A branch back counts down the frame's `tick` in its handler
-- a decrement and a compare, nothing else -- from `tickSet`, the lesser
of the exec's poll and the frame's OSR budget (`osr`, from `osrBudget`).
Where it is spent the slow function (`backEdge`) counts the back edges
since the last check toward both: the poll, which checks the context at
zero, and the budget, which at zero goes on in native code from the loop's
head (`exec.osr`) and otherwise is set past reach, as the tree's loops do.
Calls count toward the poll in `exec.run`, as before; a frame's tick does
not see them until its next check, so a context's end is seen at most one
tick later, and the OSR budget is exact. Under `WASM_JIT=osr` the spec
enters native code from a loop 587 times with either form.

**OSR.** A loop's head is a pc; its offset in the module, which names its
entry in native code, is in the code's `loops`. At a back edge every entry
below the loop's parameters is in its position's slot (constructs begin
so), the parameters have been moved to theirs by the branch, and the locals
are in theirs: the frame is the loop head's, which `exec.osr` hands to
native code. Native code runs the call to its return, leaving the results
in place, and the register frame ends.

**The JIT's contracts**, all kept, since what the JIT shares is the frame
and the record, not the body:

- the slot mapping above, an i32's or f32's upper half clear;
- `Code.interp` builds the body only when the interpreter needs it, and a
  record made from validation's bound (`boundRecord`) keeps its frame and
  native state when the body is built (`q.reg` is copied as `q.body` is);
- a call crosses between the tiers in either direction (`exec.run`);
- native code never resumes the interpreter in the middle of a function:
  `enterNative` runs the frame to `ExitReturn`, and every exit resumes
  native code (`jitExit`'s answers are a code and an offset in it, or a
  tail call made in the frame's place). So the interpreter's pcs are never
  a native exit's target, and register code needs no deoptimization
  states.

## Offsets

`offs` holds, for each instruction, the module offset of the wasm
instruction it was compiled from: what a trap's innermost frame reports
(`TestTrapOffsets`' cases, `TestRegTraps`). A call records its offset in
the caller's `exec.calls` entry from its site, as the tree's did, which a
stack trace and a host's `Trace` read. A stack overflow's frame is made by
`exec.run`, unchanged. Moves and branches the compiler adds take the offset
of the instruction that caused them; none of them traps.

## The switch

`WASM_INTERP`, read once, picks the form of the Codes made after:
`WASM_INTERP=reg` compiles each body to register code where the register
compiler takes it, the tree otherwise; `WASM_INTERP=tree`, the default
while the register compiler covers only part of the instruction set,
compiles every body to the tree. Once register code takes every body and
passes everything, it becomes the default and `WASM_INTERP=tree` keeps the
tree for side-by-side comparison until the native handlers land; then the
tree is deleted. Tests pick a form per Code (`Code.reg`,
`instantiateMode`).

## Where it departs from the JavaScript design

- **No overlapping call windows.** A JavaScript call's window is the
  callee's frame, so arguments are never copied. A wasm frame's layout is
  fixed by what the JIT shares -- parameters packed one or two slots each,
  stack positions two slots each, in chunks -- so the caller's positions
  cannot be the callee's parameters; a site's moves copy them, a few words
  a call.
- **Escapes.** The JavaScript design forbids sub-operations, since a
  second dispatch is what the table removed. Wasm's instruction set is
  twice the table's size; what goes behind an escape is cold, or heavy
  enough (SIMD, GC, atomics, bulk) that the second dispatch is a small part
  of it.
- **Traps by a marked pc.** JavaScript handlers keep the pc before they
  can throw (`st.at(pc)`) and end in a fail tail that finds the catch.
  Wasm traps cannot be caught by wasm code and exceptions are rare, so the
  panic stays and the pc travels in the panic's value.
- **No verifier yet.** The JavaScript loop's trust is proved by a verifier
  for every function; here the compiler is the only producer and the tests
  and the spec check it. A verifier of the same rules (slots below the
  frame's count, branches onto instructions, tables' entries, sites' moves)
  comes with the native handlers, which trust the code as the Go handlers
  do.
- **The tick is the frame's.** JavaScript's back edges count the runtime's
  interrupt budget; here a frame's tick stands for the exec's poll and the
  frame's OSR budget together, so that the handler decrements one counter.
- **No caches, no feedback, no typed twin.** Every wasm operand's type is
  static, so the ordinary code is already what asm.js's typed code is in
  the JavaScript engine: raw words in slots, operations of one type each.

## Measurements

First, interpreter only, on wasmbench's micro workloads (`wasmbench
-engine interp`, three fresh processes of three timed runs each, best run,
ms, two alternating rounds; Ryzen 9 9950X3D2, Windows):

| Workload | Tree | Register | Change |
|---|---:|---:|---:|
| fib | 209 / 211 | 194 / 196 | -7% |
| loop | 229 / 232 | 154 / 155 | -33% |
| i64 | 275 / 271 | 231 / 234 | -15% |
| globals | 181 / 181 | 185 / 185 | +2% |
| brtable | 241 / 242 | 162 / 163 | -33% |
| bytes | 206 / 201 | 152 / 153 | -25% |
| mandelbrot | 154 / 157 | 138 / 138 | -11% |
| nbody | 105 / 105 | 99.8 / 99.8 | -5% |

`globals` is a global's read, an operation and its write, three dispatches
where the tree's statement was one closure reading the global in place; a
read-modify-write of a global as one instruction, or the operation's
operand read from the global, would close it. `fib` is mostly the call:
the site's moves and `exec.run`.

## Native handlers

Stage 2 generates machine code for the first table's hot operations on
amd64 and arm64, over the same instructions and frames, with a generator of
the wasm engine's own (the engines stay apart: nothing of internal/vm or
its nativegen is imported). Each native handler ends in its own dispatch
-- the next word loaded, its handler's address from the table, an indirect
jump -- with the slots' base, the code's address, the table and the tick
pinned in machine registers. An operation with no native handler, and
every miss, exits to the Go handler through a stub that calls it in Go's
convention and goes on at the pc it returns, so the Go table stays the
whole semantics. A trap is an exit too: the stub calls the Go handler,
which panics in Go. The escapes' tables get native entries where an
operation earns one. Since the numeric handlers are leaves of one load,
one operation and one store, the native form removes almost all that is
left of an instruction's cost: the return to the loop and the indirect
call.

## Open work

In order, toward the tree's deletion:

1. References and v128 values in slots: the reference moves, `ref.null`,
   `ref.is_null`, `ref.func`, `ref.eq`, `ref.as_non_null`, `br_on_null`,
   typed `select`; v128 locals, constants and moves; then every SIMD
   operation behind `rSIMD`, generated with simdgen's table.
2. `call_indirect`, `call_ref` and their tail calls.
3. Bulk memory and tables, segments, other memories and i64 memories,
   offsets past 16 bits.
4. Exceptions: `throw`, `throw_ref`, `try_table`, the legacy `try`,
   `catch`, `rethrow`, `delegate`, by the catching loop above.
5. GC: structs, arrays, i31, casts, `br_on_cast`; atomics and the threads
   proposal's waits.
6. Slots past 65,535: a function that has them gets far moves, as the
   JavaScript design's far registers, never the tree.
7. A verifier, then register code as the default, `WASM_INTERP=tree` for
   comparison, and the tree's deletion once native handlers land.
