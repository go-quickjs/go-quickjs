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
- [The verifier](#the-verifier)
- [Native handlers](#native-handlers)
- [Measurements](#measurements)
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
Register code runs by native threaded handlers on amd64 and arm64
([Native handlers](#native-handlers)), and by the Go table's loop ([The
loop](#the-loop)) elsewhere and with `WASM_NONATIVE=1`.

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
  fields -- but for the GC accesses native code runs (reg_gc_fast.go): a
  struct's field and an array's element by storage kind, an array's
  length, `ref.i31`, `i31.get`, `ref.test` and `ref.cast`, whose operands
  are the instruction's fields (a field's cell in C, a test's record in
  C), the record forms kept for a cell or a record past 16 bits.

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
| 112-131 | the GC instructions, by their records |
| 160 | the atomics, one operation whose handler switches on the opcode it carries |
| 176-178 | `throw`, `throw_ref`, `rethrow` |
| 192-225 | the GC accesses by their fields: struct get and set and array get and set of each storage kind, `array.len`, `ref.i31`, `i31.get_s` and `_u`, `ref.test`, `ref.cast` |

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
- **The verifier off by default.** The JavaScript engine verifies every
  function as it is compiled; here the verifier runs in the engine's tests
  and where `WASM_REG_VERIFY=1` asks ([The verifier](#the-verifier)): it
  adds a third to a large program's compile and up to 3% to its first run.
- **The tick is the frame's.** JavaScript's back edges count the runtime's
  interrupt budget; here a frame's tick stands for the exec's poll and the
  frame's OSR budget together, so that the handler decrements one counter.
- **No caches, no feedback, no typed twin.** Every wasm operand's type is
  static, so the ordinary code is already what asm.js's typed code is in
  the JavaScript engine: raw words in slots, operations of one type each.

## The verifier

The handlers, Go's and native, read slots and code without bounds checks,
so what they take on trust is proved for a body (`regCode.verify`,
reg_verify.go): every slot an instruction names is in the frame -- a
v128's two, a reference among the reference slots, the scratch slots for
an instruction behind a far prefix and the frame's for the prefix itself
-- every branch, table entry, loop head and catch landing is an
instruction's start, the words an instruction takes after it are there,
every index into the code's sites, branch tables and GC records and into
the module's globals (of the operation's kind), functions, memories,
tables, segments and tags is in range, sites' and records' slots are in
the frame, every operation and escaped operation has a handler, and the
last instruction does not go on past the end. What each operation's
fields and words are is a descriptor (`rd`): the generators write the
numeric and SIMD operations' beside their handlers.

It runs on every body as it is compiled in the engine's tests and where
`WASM_REG_VERIFY=1` asks; the spec runs pass it in every JIT mode.
What it costs is why it is not on by default. On the spec's 6,842 bodies it
takes the interpreter's compile time from 3.5 ms to 7.7 - 11 ms, about
0.6 us a body. On wasmbench's programs, one Go-compiled module of 5.7 MB
and 3,011 bodies, compiling every body takes 46 ms, 60 ms verified (+29%;
the module decodes in 1.5 ms and validates in 1 ms), and the first run of
each program, which compiles the bodies it reaches, is 0.7 - 3% slower
(best of 15 fresh processes: sha256 784 / 789 ms, sort 706 / 711, json
483 / 487, regexp 316 / 324, gzip 207 / 210, interp 175 / 180). `TestRegVerifierRejects` shows it refusing each kind of
broken code it guards against.

## Native handlers

On amd64 and arm64 register code runs by native threaded handlers
(native_handlers_<arch>.s): machine code for the hot operations, over the
same instructions and frames, each handler ending in its own dispatch --
the next word loaded, its handler's address from the native table, an
indirect jump. There is no second interpreter: what the native code does
not do, the exit stub does by calling the instruction's Go handler in
place, so the Go table stays the whole semantics and every operation
works natively from the start. Elsewhere, and with `WASM_NONATIVE=1` (or
`SetRegNative` in a test), the Go table's loop runs it.

**Generated, position-independent, one for every build.** The handlers are
written with the JIT's assemblers by the engine's own generator,
internal/nativegen (nothing of internal/vm or its generator is imported),
as one TEXT symbol of BYTE directives (WORD directives on arm64, whose
assembler takes nothing else) and the native tables as DATA, every
address the code needs given by the context: no JIT is needed at run
time, and no tag. The generator reads the operations' values and the
context's layout from internal/regdef, which the engine aliases and
`TestNativeLayout` checks against its structures; `TestNativeGenerated`
fails while the file is stale. The block is padded to whole 4 KB pages, so
that a change to the handlers moves none of what follows within its page:
the package's closures, among them some of the Go handlers' hot paths. A
64-byte shift of them made the arrays and bulk workloads 35% slower, the
native code unchanged.

**Pinned registers.** `RBX` the instruction's address, `R12` the frame's
numeric slots, `R13` the native table, `R11` the frame's tick, `RDI` the
context (`nativeCtx`, a frame's own: the code, the slots, the frame, the
Go tables, the trampoline, memory 0 where it is unshared, the globals,
the branch tables, the native second table, the barrier routine and the
marking flag's address, and for a frame native code called, the way back
to its caller). `RAX` holds the instruction word; `RCX`, `RDX`, `RSI`,
`R8` to `R10` and `X0` to `X5` are a handler's scratch. The code never
moves SP, never writes `R14`, `RBP` or `X15`, never executes a call
instruction, and is never a safe point.

**arm64.** The same handlers, tables, context and exit protocol in arm64's
instructions (internal/nativegen's arm64*.go, each the amd64 file's of its
name). `R19` the instruction's address, `R20` the slots, `R21` the native
table, `R23` the tick, `R25` the context; `R0` the instruction word, `R1`
to `R7`, `R16`, `R17` and `V0` to `V5` a handler's scratch. The code
never moves SP and never writes `R18` (the platform's), `R26` but as the
func value an exit gives Go, `R28` (g), `R29` or `R30`, which holds
`wasmNativeEnter`'s return to `nativeRun` throughout, so that a traceback
taken in the code or in the trampolines' frames reads it. The exit stub
calls the Go handler through `wasmNativeCallH` (native_arm64.s) by Go's
internal ABI on arm64: the arguments in `R0` to `R3`, the func value in
`R26`, the result in `R0`, the spill room at `8(RSP)` to `40(RSP)`, the
word below the stack pointer left alone (it holds `nativeRun`'s caller's
frame pointer); `wasmNativeStoreRef` saves every register the code keeps
and calls the store by ABI0. The block is in the binary, so no instruction
cache is flushed. What differs from amd64 is the machine's: an operand is
decoded to its slot's address (arm64 has no base-plus-index addressing
with a displacement); a truncation is FCVTZS, which saturates, its NaN
and its range's edges left to Go; FMIN and FMAX are min and max whole,
and the conversions from unsigned 64 bits are UCVTF, so neither leaves to
Go; the floats' NaNs are arm64's (a signaling NaN ahead of a quiet one),
as the Go handlers give there. The SIMD handlers are ASIMD, which every
arm64 CPU has, and cover more than amd64's: min, max, i64x2's mul, abs
and shr_s, i8x16's shifts, popcnt, q15mulr, the extending multiplies and
pairwise adds, the conversions and saturating truncations, and the
relaxed operators with the results the Go handlers give on arm64
(relaxed_other.go). Darwin and Linux take the same block.

**The exit stub.** It stores the tick into the frame and calls the Go
handler of the instruction at `RBX` -- or, for a branch back that spent
the tick, its slow function -- through `wasmNativeCallH`
(native_amd64.s). The handler it calls is `nativeExitRun`, which runs the
instruction's Go handler and goes on as the Go table's loop does while the
next instruction has no native handler either, so that a run of them --
SIMD, GC operations -- costs one exit, not one each. The trampoline writes
Go's internal ABI by hand: the four
arguments in `AX`, `BX`, `CX` and `DI`, the func value in `DX`, the
goroutine in `R14`, `X15` zero, the arguments' spill room in its 40-byte
frame, whose caller to the unwinder and the collector is the Go that
entered the code. Back from Go it reloads the pinned registers and goes on
at the pc Go answered, runs the slow function on `^pc`, or returns to Go
once the frame has returned. A trap is an exit: the Go handler panics in
Go, through the trampoline's frame. `TestNativeCallHConvention` checks
each part of the convention and says which differs.

**What is native.** Moves and constants (64-bit and v128 ones too),
select, the numeric globals, `jmp`, `br_nz`, `br_z`, `br_table`, `ret` and
`ret1`; every i32 and i64 operator of the first table -- arithmetic,
bitwise, shifts and rotations with their immediate forms, divisions and
remainders (a divisor of 0 or -1 left to Go), comparisons, every
compare-and-branch, `eqz`, `clz`, `ctz`, the sign extensions, the wrap;
the f32 and f64 operators -- arithmetic, `sqrt`, `abs`, `neg`, `min`,
`max`, comparisons and the fused compare-and-branches with their
negations; `copysign` and the roundings are Go's -- and the conversions between
floats and integers (a truncation out of range, NaN or infinite left to Go,
which traps or saturates; an unsigned 64-bit source past 2^63 too); the
23 loads and stores of memory 0, an unshared one (a shared memory's size
another thread may change, so its accesses are Go's; an access out of
bounds is Go's, which traps); `call`, `call_indirect` and `call_ref`, the
reference moves; the escapes to the second table, whose i64 compares with
a constant, `v128.load` and `v128.store`, the reference tests, selects and
branches and the GC accesses are native (**GC objects** below), and to
rSIMD's table, 169 of whose SIMD operators are on amd64 and 227 on arm64
(**SIMD** below).
245 of the first table's values, 57 of the second's. A branch back counts down
the tick in `R11` and leaves to the slow function where it is spent, so the
poll, OSR and preemption happen as in the Go loop; native code is not
preemptible, and Go reaches it at the exits.

**Floats.** SSE's scalar instructions, the operands in the operation's
order, give what the Go handlers give on amd64: of NaN operands the
first's quieted, else the second's; of none, the default NaN. `min` and
`max` order the zeros' signs and NaNs explicitly, `abs` and `neg` touch
the sign bit alone and keep a NaN's payload, and an f32
is stored with its slot's upper half clear.

**SIMD.** rSIMD's handler dispatches through the context's SIMD table:
SSE's packed instructions over a v128's two slots, for the operators whose
instruction gives exactly what the operator's Go function does -- the
bitwise ones and `bitselect`; every integer lane's add and sub, the
saturating ones, min, max, avgr, mul (but i64x2's), dot, the narrowings;
eq and the signed comparisons (PCMPGT swapped and negated), the unsigned
ones by min or max and eq; abs, neg, the extensions, the shifts but
i8x16's and i64x2.shr_s; any_true, all_true, bitmask; the
floats' arithmetic, sqrt, the roundings, pmin, pmax, abs, neg and
comparisons; swizzle, shuffle (two PSHUFBs, the lanes' masks made with a
saturating add), extract_lane, replace_lane and splat. The floats' NaNs
are the scalar operators', lane by lane. The engine gives these handlers
only where the CPU has SSSE3, SSE4.1 and SSE4.2 (golang.org/x/sys/cpu);
elsewhere the table is the exit stub's alone. The floats' min and max,
the conversions, popcnt, q15mulr, the extending multiplies and the
relaxed operators are Go's.

**GC objects.** An object is read and written in place, by the layout
gc.go gives it: a struct's field in its cell's payload (a v128 in two
cells', a reference the whole cell), an array's element at its elements'
first (`Array.e`) plus the index times its size, below its length
(`Array.n`). A reference stored, to a slot or into an object, goes by
storeRef, under the marking rule. `ref.test`, `ref.cast` and
`br_on_cast` test by the record's mode (`regGC.mode`): a type index by
the object's type's supertype at the target's depth (`rtt.supers`), i31,
struct, array and eq by what the reference points to, a top type as
always true; a bottom type is Go's. A null where an object is needed, an
index out of bounds and a failed cast exit to Go, which traps;
allocations (`struct.new`, `array.new*`) stay in Go, an exit each.
`TestNativeGC` holds every kind, the traps and the casts to the Go table
with the marking flag read set and clear, and fails where a loop of them
leaves to Go but for its allocations.

**Calls.** `call` enters its callee natively where Go's call would do no
more than native code can: a function of the same instance with register
code that may be called so (`regCode.callee`: no try region, no tail
call), a Code that runs no JIT, depth below `MaxCallDepth` and below the
frames made, room on the exec's calls and in its chunks, and the poll not
due. `call_indirect` finds its callee in the table first -- the index in
bounds, the entry not null, and, where the site checks the type, the
callee's canonical type the one the site holds (`regSite.want`) -- and
leaves anything else to Go, which traps or checks a subtype. Then it does
what Go's call does -- the caller's tick and call offset
kept, the callee's record pushed, depth and poll counted, its numeric slots
and, where its body holds a reference (`compiled.refs`), its reference
slots taken from the chunks and cleared, the site's arguments moved, a
reference's pointer word under the marking rule below, its frame and
context made -- and jumps into the callee, whose return
(`retNative`) moves the results by the site, gives back the slots of both
kinds and the record, and goes on in the caller. Host functions, other instances'
functions, callees with native JIT code, the stack's exhaustion, a due
poll and anything else exit to the Go handler; a trap or an exception in
a natively called frame unwinds through Go as any exit's does, its frames
recorded.

**References and the collector.** The reference moves, and a native
call's clearing of its callee's reference slots and moves of references in
and out, store their pointer words under the JavaScript engine's marking rule: plainly while the
collector does not mark, through `wasmNativeStoreRef`, a barrier routine
of the engine's own, while it does -- the flag read from the runtime, the
same work either way but the barrier, and no leaving native code for the
marking's sake. Every other operation that writes a reference slot or a
reference global goes to its Go handler, whose stores have Go's write
barrier. `TestNativeRefsMarking` runs the moves, and `TestNativeCalls`
calls with references, with the flag read set and clear, and each fails
unless the routine stored exactly while it was set.

**Correctness.** `TestNativeMatchesGoTable` runs every handled operation
over its edge cases -- for the floats quiet and signalling NaNs with payloads
and either sign, the zeros, infinities, the smallest subnormal and the
truncations' bounds on both sides --
natively and on the Go table and compares results bit for bit, traps and
trap offsets, and fails on an exit the operation should not make.
`TestNativeCalls` compares recursion, arguments and results of every
kind, references among them, GC structs made and walked by recursive
callees with reference slots (and checks that none of those calls leaves
to Go), the stack's exhaustion, traps and exceptions in callees and the
callees that must exit; the spec and `TestJITDifferential` pass with the
native handlers on.

## Measurements

Interpreter only, wasmbench (`-engine interp`), best run in ms; Ryzen 9
9950X3D2, Windows. Two fresh processes of three timed runs each. The Go
table and native code at three placements: the build as it is, and with
1,024 and 2,112 bytes after the handlers' block, which move the code after
it (the range is theirs). The programs are Go-compiled wasm, their results
checked.

| Micro | Tree | Go table | Native | Native / Go |
|---|---:|---:|---:|---:|
| fib | 210 | 217 - 219 | 107 - 111 | 0.49 - 0.51 |
| loop | 222 | 153 - 156 | 66.7 - 67.3 | 0.43 - 0.44 |
| i64 | 263 | 230 - 232 | 90.2 - 97.6 | 0.39 - 0.42 |
| mandelbrot | 145 | 123 | 91.8 - 93.3 | 0.75 - 0.76 |
| nbody | 101 | 98.9 - 99.6 | 71.1 - 71.6 | 0.72 |
| bytes | 194 | 155 - 156 | 66.2 - 67.0 | 0.43 |
| bulk | 125 | 122 - 123 | 100 - 101 | 0.82 - 0.83 |
| indirect | 384 | 340 - 344 | 172 - 188 | 0.50 - 0.55 |
| brtable | 231 | 161 - 164 | 113 | 0.69 - 0.70 |
| globals | 171 | 175 - 176 | 74.6 - 77.0 | 0.42 - 0.44 |
| gc | 344 | 347 - 352 | 282 - 288 | 0.81 - 0.83 |
| arrays | 124 | 101 | 101 - 104 | 1.00 - 1.03 |
| simd | 74.6 | 89.9 - 90.7 | 17.7 - 18.3 | 0.20 |

| Program | Tree | Go table | Native | Native / Go |
|---|---:|---:|---:|---:|
| sha256 | 1,885 | 1,515 - 1,536 | 752 - 758 | 0.49 - 0.50 |
| sort | 1,726 | 1,046 - 1,055 | 681 - 693 | 0.65 - 0.66 |
| json | 937 | 631 - 633 | 457 - 458 | 0.72 - 0.73 |
| regexp | 794 | 452 - 455 | 295 - 296 | 0.65 |
| gzip | 478 | 299 - 305 | 186 - 187 | 0.61 - 0.63 |
| interp | 490 | 290 - 293 | 165 | 0.56 - 0.57 |

Every workload but arrays runs faster natively than on the Go table, by
1.2 to 5 times. arrays is level: its loops' array.get, array.set and
array.len are Go's, an exit each. The integer kernels and the calls gain
most; simd runs its loop's operators natively, five times the Go table's
speed and four times the tree's. The Go table's own figures move with
where the linker puts `regLoop` and the Go handlers: its 45-byte loop fits
one 64-byte line where the function starts at 32 mod 64 and straddles two
at 0 mod 64, up to 15% on the tightest loops, and loop, i64 and mandelbrot
ran 13 - 19% faster on it in this build than in the one before, the Go
code unchanged; native code does not run that loop. An exit-heavy
workload moves with the code after the handlers' block too: arrays and
bulk by 35% for a 64-byte shift, before the block was padded to whole
pages, and indirect, while its calls still left to Go, by 21 - 27% on
both the Go table and native code for a 1 KB shift of what follows.

## Open work

1. The closure tree's deletion with `WASM_INTERP`, once arm64's handlers
   have run on hardware.
2. Native allocation of small structs and arrays, from a pool of the
   context's, once the arrays and gc workloads show it worth more than
   the exit each costs now.
3. Forms with a 32-bit immediate in the word after the instruction for the
   hot ones -- compare-and-branch, add, sub, and, or, xor, mul -- where a
   constant past 16 bits is reloaded every turn today (`i32.lt_u i
   1048576` in bytes.wat).
4. Fused forms of a global's read, operation and write, once a profile
   with the native handlers shows they matter.
5. The verifier always on, should its cost on first runs come down: it
   adds 0.7 - 3% to a large program's first run today.
