# The WebAssembly interpreter

The engine interprets a function as *register code*: its validated body
compiled in one pass to fixed-width instructions that name the frame's
slots as their operands, run by a small loop over a table of handlers. It
follows the JavaScript engine's register interpreter
([docs/register-interpreter.md](../../../docs/register-interpreter.md)) and
replaced the closure tree that interpreted every body before it
(compile.go's header), which `WASM_INTERP=tree` still selects for
comparison. The JIT (wasm/internal/jit,
[wasm/internal/jit/README.md](../jit/README.md)) compiles from the module's
bytes, not from either form, and shares the interpreter's frames.

This document is the design: what is built, where it departs from the
JavaScript design and why, and what is left.

## Contents

- [Where it runs](#where-it-runs)
- [Why registers](#why-registers)
- [Instructions](#instructions)
- [The opcode space](#the-opcode-space)
- [The frame](#the-frame)
- [The compiler](#the-compiler)
- [The loop](#the-loop)
- [Slots past 16 bits](#slots-past-16-bits)
- [Traps](#traps)
- [Exceptions](#exceptions)
- [Calls and returns](#calls-and-returns)
- [Polls, OSR and the JIT](#polls-osr-and-the-jit)
- [Offsets](#offsets)
- [The switch](#the-switch)
- [Where it departs from the JavaScript design](#where-it-departs-from-the-javascript-design)
- [Measurements](#measurements)
- [Native handlers](#native-handlers)
- [Open work](#open-work)

## Where it runs

`Code.interp` compiles a body when the interpreter first needs it: at the
first call, or where native code ran the function from its first call
(`nativeFirst`, `boundRecord`), when a call first reaches the interpreter.
`compileFunction` compiles it to register code (`regCompile`) unless the
Code asks for the closure tree (`Code.reg`, `WASM_INTERP=tree`); the
record holds one or the other (`compiled.reg`, or `compiled.body` and
`epi`), and `exec.interpret` runs whichever it holds. Calls cross between
the two forms, and between either and native code, in both directions.

Every validated body compiles to register code: every instruction of
WebAssembly 3.0 and the threads proposal, every value kind, any number of
locals, any offset and memory. `ReadInterpStats` counts the bodies of each
form, and of the tree's by the reason the register compiler gave, which
the spec test logs: 6,842 bodies, none left to the tree. The one check
left is a module whose offsets do not fit the 32 bits every part of the
engine reports them in ([Traps](#traps)).

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

An instruction is one 64-bit word (reg.go), and some take words after it:

```
bits  0- 7  Op  the operation, the index of its handler in regTable
bits  8-15  X   a small operand: an escaped operation's index (rExt, rSIMD)
bits 16-31  A   a slot, most often the destination
bits 32-47  B   a slot, or a 16-bit immediate (SB)
bits 48-63  C   a slot, a 16-bit immediate or a branch's offset (SC)
```

B and C read together are BC, a 32-bit immediate, index or offset. Every
field is decoded with one shift and one extension, the same for every
operation; no operand's kind is decided at run time. A slot field is a
16-bit index, `*rs(l, w>>16)`: a shift, a zero extension and an address; a
reference slot is the frame's reference slot of that index (`f.rr`), a
v128 the numeric slot and the next. A word after an instruction holds what
does not fit its fields: a 64-bit or v128 constant, a memory and its
offset, a shuffle's lanes, a lane's index, a fourth operand, a record's
index.

The forms, by family:

- **Binary** `A = B op C`, and **immediate** `A = B op SC` for the
  integer operators a constant is commonly the right operand of (add, sub,
  mul, and, or, xor, the shifts, i32's rotations, every comparison). A
  constant on the left of a commutative operator is swapped to the right;
  of a comparison, the comparison is swapped (`lt_s(5, x)` is
  `gt_s(x, 5)`).
- **Unary** `A = op B`: the tests, counts, extensions, roundings and
  conversions. An operator that changes no bit -- the reinterpretations,
  `i64.extend_i32_u` of an i32 whose upper half is clear, the extern/any
  conversions -- is no instruction: the compiler retypes the entry.
- **Compare-and-branch** `if A op B: pc += SC` and `if A op SB: pc += SC`
  for every integer comparison, i32 and i64; `br_z` and `br_nz`
  (`if A == 0: pc += BC`) for eqz and for a plain condition; `br_null`,
  `br_non_null`, `br_on_cast` and `br_on_cast_fail` of reference A; `jmp`
  (`pc += BC`); `br_table` (`pc = tables[BC][min(A, last)]`).
- **Moves and constants**: `mov`, `mov2` (a v128), `ref.mov`, `const32`
  (zero-extended BC), `const32s` (sign-extended), `const64`, `const128`,
  `ref.null`.
- **Memory**: the loads and stores of an i32 memory 0 at an offset below
  2^16, `A = load[B + C]` and `store[A + C] = B`; every other access by
  rExt's generic form, the memory and opcode in the word after and the
  64-bit offset in the next (`regEA`); `memory.size`, `memory.grow` and
  the bulk operations.
- **Globals**: `A = global[BC]` (32- and 64-bit, so that an i32's upper
  half is clear however the host set the global), `global[BC] = A`, and
  the v128 and reference globals' forms.
- **Control**: `select` (`if C == 0: A = B`, A holding the first operand,
  of each kind), `call` and `return_call` (`sites[BC]`), `ret`, and `ret1`
  (`slot 0 = A` and return), the common return of one value in one
  dispatch.
- **Records and sites**: a call, a throw, a GC instruction and an atomic
  name their operands through a site, a record or the words after them, as
  32-bit slot indices, since they have more operands, or more kinds, than
  fields.

The numeric operations, their handlers and what the compiler needs to know
of each (`regNumeric`, `regMemOp`, `regLoadAny`, `regStoreAny`) are
generated (reg_num_gen.go, internal/numgen) from the same table of
operators as the tree's nodes, and the SIMD operators (reg_simd_gen.go,
internal/simdgen) from the tree's SIMD table, calling the same lane
functions: the two forms' semantics -- NaN payloads, the truncations'
ranges, the relaxed operators' host choices -- are one source.

## The opcode space

Wasm has about 550 instructions and the dispatch is 8 bits. The first table
holds what a hot loop runs, each operation its value; the rest go behind
escapes, each a second table of 256 indexed by X:

| First table | What |
|---|---|
| 0 | `rInvalid`, which panics; every unused value of every table is it |
| 1-23 | hand-written: `rExt`, moves, constants, branches, `br_table`, returns, calls, `select`, globals, the v128 and reference moves, `rSIMD`, `rFar` (reg.go) |
| 24-240 | 217 numeric: every i32/i64/f32/f64 operator but the cold ones, 40 immediate forms, 40 compare-and-branches, 23 loads and stores of memory 0 |
| 241-255 | free |

| `rExt`'s table | What |
|---|---|
| 0-15 | `unreachable`, memory 0's size and growth, the reference and v128 globals and selects, `ref.is_null`, `ref.func`, `ref.eq`, `ref.as_non_null`, `br_null`, `br_non_null`, the caught-exception drop |
| 32-48 | the cold numeric operators: the saturating truncations, popcnt, copysign, f32's roundings, f64's nearest |
| 64-71 | the generic loads and stores, any memory's size and growth, `memory.init`, `memory.copy`, `memory.fill`, `data.drop` |
| 80-83 | the vector loads, stores and lane accesses |
| 96-107 | the table operations, `elem.drop`, `call_indirect`, `call_ref` and their tail calls |
| 112-131 | the GC instructions |
| 160 | the atomics, one operation whose handler switches on the opcode it carries |
| 176-178 | `throw`, `throw_ref`, `rethrow` |

`rSIMD`'s table holds the 212 generated SIMD operators -- every operator
without immediates, the relaxed ones included -- and 15 hand-written: the
shuffle, the lane extracts and replaces, the splats (227 of 256). A v128
operation is heavy enough that the second dispatch is a small part of it.

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
numeric slots, and the reference slots and the `refs` flag counted as the
tree counts them (a reference pushed, reference locals, legacy try depth)
-- since the compiler pushes and pops as validation does, so
`boundRecord`, `frameBound`, `withinBound` and the JIT's callee frames hold
for either form. An i32 or f32 in a slot has its upper half clear: every
handler writing one writes `uint64(uint32(...))`, and what comes from the
host is narrowed as it enters a frame (`narrow`). So an index of an i32 or
an i64 table or memory is the slot's whole word either way, and one
handler serves both.

## The compiler

reg_compile.go, one pass over the body, after validation, with a symbolic
operand stack. An entry is a constant, or the slot that holds the value: a
local's, read in place, or its own position's. Unlike the tree's entries no
entry is pending work -- every instruction is emitted as it is read -- so
there is no evaluation order to keep, and the only hazard is a local read
in place and then written: `local.set` and `local.tee` first copy the
stack's reads of the local to their positions (`flushLocal`). A slot field
carries its kind -- number, v128, reference -- as marks until it is
emitted, so that a slot past 16 bits is caught in one place
([Slots past 16 bits](#slots-past-16-bits)).

**Copy elision.** An operation writes its result to its position's slot,
and its entry remembers the instruction (`def`). A `local.set` or
`local.tee` of that value straight after -- nothing emitted between, no
label bound between, no read of the local on the stack -- rewrites the
instruction's destination to the local (`retarget`), or a GC record's
destination, and so does a return of it, to slot 0. `local.get` is no
instruction, and neither is a constant: it is the immediate of the
operation that takes it where a 16-bit form exists, and is loaded into its
position's slot where one does not (`read`).

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
- `br_if`, `br_null`, `br_non_null`, `br_on_cast` and `br_on_cast_fail`
  with no values to move are one branch; with values, the opposite branch
  skips the moves and a `jmp`. Such a branch to the function's label is a
  conditional return.
- `br_table` is one instruction over a table of absolute pcs; a target the
  values must be moved for, or the function's, or one out of a legacy
  catch, is reached through a stub laid after the table, one stub a target
  construct.
- `return` moves one result to slot 0 in the instruction that computes it
  or by `ret1`; several go to their positions' slots first, since a result
  slot is a local another result may be read from.
- `try_table` and `try` begin a region ([Exceptions](#exceptions)); a
  catch clause's landing is patched as a branch to its label is.
- Code after `br`, `br_table`, `return`, `unreachable`, a throw and a tail
  call is skipped to the construct's end, as the tree does.

**Branches that do not reach.** A compare-and-branch's offset is 16 bits.
Where one does not reach, the body is compiled again with every
conditional branch a 32-bit `br_nz`/`br_z` of a computed condition
(`far`); unconditional branches and tables are 32 bits always.

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
([Polls](#polls-osr-and-the-jit)); an escaped branch's slow function is
its table's (`regExtSlowTable`). An escape (`rExt`, `rSIMD`) calls its
table's handler, a second dispatch the cold operations pay.

Slots are reached by `unsafe.Add` from the frame's first: the compiler
named only slots of the frame, which is given that many before it runs. A
pointer to an array of 65,536 slots, as the JavaScript loop uses, would
not do: a frame's slots are in chunks of 16,384 (`chunkSlots`), and a
conversion to a larger array fails `checkptr`. The code is read at a pc
the compiler made, past which no branch goes. The spec suite runs clean
under `-d=checkptr=2`.

## Slots past 16 bits

A body with tens of thousands of v128 locals puts its later locals and its
stack positions past what a field holds. An instruction whose slot field
would name such a slot is emitted behind a far prefix (`rFar`): its fields
name the frame's scratch slots instead -- numeric 0, 2, 4 and 6 and
reference 0 to 3 for A, B, C and a fourth operand -- and the prefix's
three words hold each operand's kind and its 32-bit slot. `rhFar` copies
the operands in, runs the instruction (and its slow function, where a back
edge asks for it) on the scratch slots, and copies A, its destination if
it has one, out, unless the frame has returned. A branch behind the prefix
is relative to its own pc, which is what the compiler's fixes name, and
nothing rewrites an instruction behind the prefix after it is emitted. The
operations that name their slots through a site, a record or the words
after them take 32-bit indices and need no prefix. Such bodies are
generated code, run rarely; the prefix costs them a copy of each operand,
and costs every other body nothing.

## Traps

A trap keeps the engine's panic, recovered at the call's entry (`invoke`,
`recovered`). It is the cheapest form in Go: the path that does not trap
pays nothing -- a handler that returned an error would test it on every
instruction, and the loop would test the handler's answer. To keep even
the trap's offset off the hot path, a handler traps with its pc's
complement (`regAt`), which is past every offset of its module, and the
recovery makes it the module offset of the instruction the pc was compiled
from, through the innermost frame's register code (`offs`,
`exec.regOffset`); an offset the tree or native code reports is below the
module's size and stays as it is. So a division's handler passes `^pc`
where the tree's node passed a captured offset, at no cost. A module whose
offsets with its pcs would pass 32 bits is the register compiler's one
refusal; no part of the engine reports such a module's offsets.

## Exceptions

A throw is a Go panic of an `*Exception`, as the tree's: a frame that never
catches pays nothing. The body of each `try_table` and legacy `try` is a
region of the code's (`regRegion`), by the module offsets of its
instructions, with its catch clauses and their landings. A frame whose
code has any regions runs its loop under `runRegCatching`, which recovers
an exception (or a tree frame's delegation, which is its exception),
restores the exec as the frame had it (`tryState`, saved once at the
frame's entry), and finds the innermost region holding the offset the
frame was at -- the call or the throw it was making, which its entry in
`exec.calls` holds -- then delivers the clause's values and exception
reference and enters the loop again at its landing: a block's end, a
loop's head, or a return stub for the function's label. A region that does
not take the exception passes it to the regions around it; a `delegate`
passes it to the regions at or outside its target; a frame with none left
panics it on.

A legacy catch's exception is on the exec's caught stack while its body
runs, at the frame's base plus the catches around it, for `rethrow`; a
branch out of the body, and its fall-through, drop it (`xCaughtTo`), and
the frame drops what is left as it returns.

## Calls and returns

`call` names a site (`regSite`): the function's index, the call's offset,
and the moves of the arguments from the caller's slots -- a local's in
place, or a position's, each a word, two words or a reference -- to the
callee's parameter slots, and of the results from the callee's slots to
the caller's positions. The handler takes the callee's frame from the
exec's chunks, makes the moves, and runs the callee by `exec.run`, which
keeps what it did for the tree: the depth check and the stack overflow's
frame, the call's poll, the record in `exec.calls`, native code where the
callee has it, and the interpreter in whichever form the callee's record
holds. A host function, or another instance's, is the same site: the
handler calls the host through `hostCall` with the arguments in `Val`s.
`call_indirect` and `call_ref` are sites with the slot of their target,
the callee found after the arguments' moves are read (`indirectCallee`,
the reference's function).

A tail call stores its arguments in the exec's tail call and ends the
frame with `sigTail`; `exec.run`'s `tailChain` makes the call in place of
the frame, as for the tree's. A frame that returns leaves its results in
slots from 0, where its caller and native code look for them.

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
enters native code from a loop as many times with either form.

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
stack trace, a host's `Trace` and the catching loop read; a throw records
its own there too. A stack overflow's frame is made by `exec.run`,
unchanged. Moves and branches the compiler adds take the offset of the
instruction that caused them; none of them traps.

## The switch

`WASM_INTERP`, read once, picks the form of the Codes made after: register
code by default, and the closure tree with `WASM_INTERP=tree`, kept for
side-by-side comparison until the native handlers land; then the tree is
deleted. Tests pick a form per Code (`Code.reg`, `instantiateMode`, whose
Codes run the interpreter alone whatever `WASM_JIT` asks), and
`regMatchesTree` compares the two over each instruction family.

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
  of it. The atomics go further: one handler switching on the opcode, their
  cost being the compare-and-swap's.
- **Records.** An instruction of more operands than fields names them in a
  record or the words after it, as 32-bit slots, rather than through
  moves into a call window.
- **Far slots by a prefix.** The JavaScript design moves a far register
  through near ones; a wasm frame has no near slot to spare -- its size is
  validation's bound -- so the instruction runs on the frame's scratch
  slots behind a prefix instead.
- **Traps by a marked pc.** JavaScript handlers keep the pc before they
  can throw (`st.at(pc)`) and end in a fail tail that finds the catch.
  Wasm traps cannot be caught by wasm code and exceptions are rare, so the
  panic stays and the pc travels in the panic's value; the catching loop
  finds a region by the offset the frame already records.
- **No verifier yet.** The JavaScript loop's trust is proved by a verifier
  for every function; here the compiler is the only producer and the tests
  and the spec check it. A verifier of the same rules (slots below the
  frame's count, branches onto instructions, tables' entries, sites' and
  records' slots) comes with the native handlers, which trust the code as
  the Go handlers do.
- **The tick is the frame's.** JavaScript's back edges count the runtime's
  interrupt budget; here a frame's tick stands for the exec's poll and the
  frame's OSR budget together, so that the handler decrements one counter.
- **No caches, no feedback, no typed twin.** Every wasm operand's type is
  static, so the ordinary code is already what asm.js's typed code is in
  the JavaScript engine: raw words in slots, operations of one type each.

## Measurements

Interpreter only, wasmbench (`-engine interp`), best run in ms; Ryzen 9
9950X3D2, Windows. The micro workloads: three fresh processes of three
timed runs, two alternating rounds; the programs (Go-compiled wasm, their
results checked): one process, one timed run.

| Micro | Tree | Register | Register, `regLoop` at 32 mod 64 |
|---|---:|---:|---:|
| fib | 209 - 211 | 201 - 202 | 198 - 199 |
| loop | 229 - 232 | 178 - 180 | 154 - 156 |
| i64 | 274 - 281 | 267 - 270 | 231 - 233 |
| globals | 181 - 182 | 208 - 210 | 181 - 183 |
| brtable | 240 - 242 | 177 - 178 | 161 - 162 |
| bytes | 205 - 207 | 175 - 179 | 155 - 157 |
| mandelbrot | 155 - 156 | 158 | 129 - 131 |
| nbody | 105 | 104 | 98.6 - 99.8 |

| Program | Tree | Register | Change |
|---|---:|---:|---:|
| sha256 | 1,955 | 1,772 | -9% |
| sort | 1,801 | 1,168 | -35% |
| json | 956 | 597 | -38% |
| regexp | 806 | 484 | -40% |
| gzip | 478 | 325 | -32% |
| interp | 494 | 312 | -37% |

The last column is the same code with `regLoop` 32 bytes later: Go aligns
a function to 32 bytes, and `regLoop`'s 45-byte loop fits one 64-byte line
where the function starts at 32 mod 64 and straddles two where it starts at
0 mod 64, which is where this build put it. Its instructions are the same
either way, and so is every handler's; a quarter of what a turn of a tight
loop costs is that line. The native handlers end the question, since
`regLoop` then runs only where they do not; until then, the placement is
noise of up to 15% on the tightest loops. `globals` is a global's read, an
operation and its write, three dispatches where the tree's statement was
one closure reading the global in place; `fib` is mostly the call: the
site's moves and `exec.run`.

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

1. A verifier of register code, before native handlers trust it.
2. The native handlers (stage 2), then the closure tree's deletion with
   `WASM_INTERP`.
3. Forms with a 32-bit immediate in the word after the instruction for the
   hot ones -- compare-and-branch, add, sub, and, or, xor, mul -- where a
   constant past 16 bits is reloaded every turn today (`i32.lt_u i
   1048576` in bytes.wat).
4. Fused forms of a global's read, operation and write, once a profile
   with the native handlers shows they matter.
5. Fused float compare-and-branches, with their negations.
