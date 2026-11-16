# Native JIT tier

This package is the optional native tier's machine-code side; the VM's side
is `internal/vm/zzjit_*.go`. The tier compiles a hot function's register
code -- the register interpreter's instructions (`bytecode.RegCode`,
docs/register-interpreter.md) -- to amd64 or arm64 machine code that runs on
the register interpreter's own frame. As V8's optimizing compilers do over
Ignition, it compiles what the interpreter has seen run, speculates on what
the interpreter's caches and native code's own exits recorded, and leaves to
the interpreter, deoptimizing, where a speculation fails.

The tier is **experimental and opt-in**, and it needs two things:

1. a build with `-tags quickjs_jit` on `linux/amd64`, `windows/amd64`,
   `darwin/arm64` or `linux/arm64`;
2. a runtime made with `quickjs.WithJIT()` (or `qjs --jit`,
   `-conformance.jit`, `v8bench -jit`).

Without either, nothing of it runs: a function runs in the register
interpreter, the default on amd64 and arm64, and otherwise in the stack
interpreter and the tree tier (see [Zero cost when
off](#zero-cost-when-off)).

**One design, two front ends for now.** The register pipeline this file
describes is the design. Until the switch-over at the end of
docs/register-interpreter.md's Stage 3, a runtime with the JIT runs register
code only where `QJS_JITREG` is set in the environment (or
`vm.SetJITRegTier(true)`); by default it still runs the stack form under the
stack interpreter and the tree tier, and the JIT compiles that form through
a front end of its own. Everything past the lowering -- the IR, the SSA
builder, both backends, the ABI, the arena, the exits' mechanics, the write
barrier, the code cache -- serves both. What only the stack form's path has
is in one section, [The stack form's
path](#the-stack-forms-path-removed-at-the-switch-over), and goes with it.

This file states the design and the contracts the code keeps.

## Contents

- [The pipeline at a glance](#the-pipeline-at-a-glance)
- [Layout](#layout)
- [Register code and its lowering](#register-code-and-its-lowering)
- [Feedback](#feedback)
- [Tier-up and on-stack replacement](#tier-up-and-on-stack-replacement)
- [The SSA build](#the-ssa-build)
- [What the builder makes](#what-the-builder-makes)
- [The backend](#the-backend)
- [Native code on the register frame](#native-code-on-the-register-frame)
- [Calls of Go](#calls-of-go)
- [Native calls](#native-calls)
- [Inlining](#inlining)
- [Exits and deoptimization](#exits-and-deoptimization)
- [The execution boundary](#the-execution-boundary)
- [The code cache, budgets and memory](#the-code-cache-budgets-and-memory)
- [The write barrier and the marking
  rule](#the-write-barrier-and-the-marking-rule)
- [Regular expressions](#regular-expressions)
- [Zero cost when off](#zero-cost-when-off)
- [Validation](#validation)
- [The stack form's path (removed at the
  switch-over)](#the-stack-forms-path-removed-at-the-switch-over)

## The pipeline at a glance

```
register interpreter --(samples)--> jitRegFor
  ^    fills the caches                |
  |                                    v
  |                       compile.LowerReg --> ir.Program
  |                                    |
  |      jitRegFeedback --------> ssa.Build --> ssa.Optimize
  |      (the caches, what               |
  |       exits learned)                 v
  |             ^              mir: amd64 | arm64 --> arena
  |             |                        |
  +--------- exits <----- native code on the register frame
```

| Step | Where | What |
|---|---|---|
| Sample | `zzjit_zregtier.go` | A call or a back edge that spends a budget the interpreter spends anyway credits a function; its second sample compiles it. |
| Lower | `compile.LowerReg` | Register code to the slot IR: a register is a slot, a register instruction one or more IR instructions of its origin, under the atomicity rule. |
| Build | `ssa.BuildIn`, `jitRegFeedback` | Typed SSA, speculating at each site on the feedback its register pc has. |
| Optimize | `ssa.Optimize` | Numbers unboxed, int32s, guards merged, escape analysis, dead code removed, references kept. |
| Encode | `mir.CompileWith` | Linear-scan allocation; amd64's or arm64's code. |
| Place | `jit.NewSSACode` | The runtime's arena, written, then sealed read/execute. |
| Enter | `jitRegEnter`, `jitRegLoop` | At a call whose frame Go makes, or by on-stack replacement at a loop's header, on the register frame's window in place. |
| Exit | `jitRegRun` | Records write the live registers; Go runs the instruction whole and native code goes on, or the interpreter runs the rest of the invocation; what the exit taught is kept by register pc. |

## Layout

| Package or file | Role |
|---|---|
| `ir` | The slot IR: one or more instructions per register instruction, each with its register pc (`Origin`) and a state map. `Validate` checks a program, the atomicity rule included, and `Evaluate`, a Go interpreter of the IR, is the oracle the SSA tests and the VM's evaluator compare with. |
| `compile` | Lowers register code into the IR (`LowerReg`; `LowerRegHot`, `LowerRegInline`), or refuses it (`Refusal`), and lowers the stack form (`LowerSSA`, see [The stack form's path](#the-stack-forms-path-removed-at-the-switch-over)). It never imports the VM. |
| `ssa` | Builds SSA from a program and the VM's feedback (`Feedback` and its optional interfaces), and optimizes it. `eval.go` runs SSA over a heap model, for tests. |
| `mir` | Selects instructions, allocates registers and encodes amd64 and arm64 machine code (`CompileAMD64`, `CompileARM64`, `CompileWith`), both on every target. |
| `abi` | The contract between the code and the VM: the context block (`Context`), the exits, and the value encoding (`Encoding`), given as data. |
| `asm` | The two assemblers. |
| `ssacode.go`, `arena.go`, `exit.go`, `workspace.go` | Placing code (`SSACode`, `Arena`), entering it, reading its exits (`ExpandCall`, `ExpandGoCall`, `ApplyExit`), and the compiles' reused storage (`Workspace`). |
| `raw.go` | Placing and entering machine code another compiler made, a regular expression's (`RawCode`, `enterRaw`). |
| `barrier.go` | The runtime's write-barrier flag, which native code tests, and the Go side of its stores through the barrier (`storeRef`). |
| `verify` | A module of its own that checks the code `mir` makes for both targets against `golang.org/x/arch`'s disassemblers, and `jitprof`, which joins a CPU profile with the code. |
| `entry_amd64.s`, `entry_arm64.s` | Entry from Go (`enterSSA`, `enterRaw`), the calls of Go native code makes (`callGo`), and the barrier's store (`storeRef`). |
| `memory_unix.go`, `memory_windows.go`, `memory_windows_amd64.s` | Executable memory: `mmap` and `mprotect`, `VirtualAlloc` and `VirtualProtect`. |
| `cache_amd64.go`, `cache_arm64.go`, `flush_darwin_arm64.s`, `flush_linux_arm64.s` | The instruction cache's maintenance. |
| `policy_darwin.go`, `policy_linux_arm64.go` | Each OS's policy for executable memory. |
| `jit_disabled.go` | The stubs for builds and targets without the tier. |
| `internal/vm/zzjit_zregtier.go` | Tier-up from the register interpreter: sampling, entry at calls and by on-stack replacement, invalidation, what exits teach. |
| `internal/vm/zzjit_zregnative.go` | The compile (`jitRegFor`), the register code's cache, running code on a register frame and its exits (`jitRegRun`), the calls of Go made on it. |
| `internal/vm/zzjit_zregfeedback.go` | `jitRegFeedback`, the builder's feedback by register pc. |
| `internal/vm/zzjit_zregcall.go` | Native calls between register functions: links, call sites, levels, unwinding. |
| `internal/vm/zzjit_zreginline.go` | Finishing inlined register callees at an exit. |
| `internal/vm/zzjit_zregframe.go` | What Go makes on a register frame for native code: cells, closures, rest parameters, arguments. |
| `internal/vm/zzjit_zregrunner.go` | The IR's evaluator over a register frame, the VM its runner (`RegLowerCall`), and running a register instruction whole (`regWhole`). |
| `internal/vm/zzjit_zregparity.go` | The comparison of the two forms' feedback (`CompareFeedback`). |
| `internal/vm/zzjit_native.go`, `zzjit_ssa.go` | What both forms share -- the JIT's state, the code cache, budgets, links, contexts, entering and resuming code, calls of Go -- and the stack form's selection, feedback (`jitFeedback`) and host exits. |
| `internal/vm/zzjit_leaffast*`, `zzjit_bigleaf*` | The VM's routines native code jumps to for leaves and BigInts' operators (`LeafFast`, `BigFast`). |
| `internal/vm/zzjit_background.go`, `zzjit_snapshot*.go` | Compiling in the background, and its snapshot of the feedback (the stack form's). |
| `internal/vm/zzzyn_native_feedback*` | The native handlers' feedback dispatcher, which a runtime that runs the JIT is given (generated by `internal/vm/internal/nativegen`). |
| `internal/vm/zzjit_disabled.go` | The stubs for builds without the tier. |
| `internal/regexp/native_*.go` | Regular expressions' machine code: the generator (`native_gen.go`), its macro assembler for both machines (`native_masm.go`), character sets (`native_sets.go`), and entering and placing it (`native_jit.go`); `native_off.go` without the tag. See [Regular expressions](#regular-expressions). |
| `internal/vm/zzjit_regexpnative.go` | The VM's host for them: the arena and the budget. |

**Where the VM's files sit.** Package vm's functions are laid out in
file-name order, then all its closures (AGENTS.md). The JIT's `zzjit_` files
come after the stack interpreter's and the tree tier's (`vm*.go`,
`zcall_*.go`, `ztree_*.go`), so they move none of those functions; they come
before the register interpreter's Go code (`zzreg_*.go`, `zzzy*_*.go`),
whose functions they do move. The layout lock holds what matters in place
whatever moves: its pad J sits before the `zzjit_` files, pads Q and W hold
the register interpreter's loop (`runReg`) and its calls (`rhCallW`), and
the configurations with the JIT anchor the JIT's leaf calls (`jitLeafGo`,
`layoutlock.txt`). Every function comes before every closure, so a function
added to any of them moves the tree's closures, which the lock holds too. A
change to a `zzjit_` file is therefore followed by `go generate
./internal/vm`, as any change to package vm, and `TestLayoutLock` fails
until then. The register pipeline's files, `zzjit_zreg*.go`, sort last among
the JIT's, so that what is added there moves no JIT function before them.

## Register code and its lowering

A register function's frame is a window of registers in the VM's stack:
register 0 holds the function, 1 its this, and the parameters follow from 2.
Its instructions name registers as their operands
(docs/register-interpreter.md, "Instructions"), and a call's operands are a
run of registers, a window of the callee's, which the interpreter's call
makes the callee's frame. `compile.LowerReg` lowers a function's register
code into the slot IR; `LowerRegHot` lowers one its callers have shown to be
hot, and `LowerRegInline` one for native callers or for inlining. Code
compiled at a function's first call (`bytecode.RegLater`) keeps the names,
constants and property sites it adds in its `RegCode`, which the lowering
reads in place of the function's (`regView`).

**Slots.** A register is a slot: the program's first `Regs` slots are the
frame's window, register 0 the function, 1 its this and the parameters from
2 (`ParamBase`). The captured bindings' slots follow them (`UpvalueBase`),
and then the scratch slots the expansions below use, which no state holds.
There is no operand stack: every state's `Depth` is 0 and `StackSize` 0. An
exit's records write registers, and its PC is a register pc. `Live` is a
backward liveness over the registers -- each Host's by its register
instruction, a register a closure captures live throughout, a handler's live
registers live at every instruction it guards, a loop's step's register not
written at the loop's end, and every instruction of a register instruction's
lowering live as its first, where an exit inside it runs it again -- so an
exit writes only the registers the interpreter may read.

**One register instruction, one or more IR instructions.** Most register
instructions are one IR instruction with their operands named: `add r4, r2,
r3` is a `Binary`, a compare-and-branch a `Branch`, a property's read a
`PropertyRead` of its register. The fused ones are several, of the same
origin (`ir.Program.Origin`):

| Register instruction | Its expansion |
|---|---|
| `add_prop` | the property's read, the addition and the store |
| `add_index*` | the element's read, the addition and the store |
| `get_index_pre`, `get_index_post` | the step and the read into scratch slots, then both registers written |
| `get_method_w` | the method's read, then the receiver and the method copied into the call's window |
| `char_code_at` | the method's permission and the code unit |
| `new_object_lit` | the object and its fields |
| a compare with a captured bound | the bound's read and the compare |
| `jtypeof`, a switch's cases | the tests and their branches |

Where a register instruction's operands are a run of registers that is not
where its result goes -- `new_array`'s elements, `concat`'s parts -- they
are copied to scratch slots first, which the operation reads as a run of
operands. What the lowering leaves to Go whole -- a call, a cold operation
-- is one `Host`, alone in its origin: Go runs the whole register
instruction. Where Go can run it on the frame and the registers it reads
and writes are known (`compile.RegAccess`: up to three read, one written)
-- a for...in's or a for...of's cursor, an object's or an array's spread, a
spread call, a computed key's definition -- it is a whole Host (`ir.Instruction`'s `Whole`), its
operands the registers it reads and its result the one it writes, which
native code calls Go for and goes on after (see [Calls of
Go](#calls-of-go)). So is what the builder leaves to Go of an operation
whose operands are its register instruction's own, the only instruction
of its origin (`ir.Instruction`'s `WholeOperands`): a literal's field
whose definition the feedback does not plan, a destructuring's step or
rest, a private member's read or write its site's cache holds no own field
of. A private member's read or write (`get_private`, `set_private`) is a
property's read or write of the key its register holds (`ir.Instruction`'s
`Private`), made natively for the shapes its site's cache holds of the key,
as the stack form's. A call, `call_w`'s, `call_method_w`'s or `new_w`'s, and
a `typeof` are Hosts of their own kind (`ir.Instruction`'s `Window`), which
the builder makes natively from what the feedback says (see [Calls of
Go](#calls-of-go) and [Native calls](#native-calls)). So is a call in tail
position (`tail_call_w`, `tail_call_method_w`, `ir.Instruction`'s `Tail`):
made as the ordinary call while the stack is shallow, natively or by Go, as
the VM's `rhCallWT` makes it, native code going on after it; deeper, where
the interpreter gives the frame to the callee, the call leaves for it
(`ssa.OpTailRoom`), so that native code keeps no frame a tail call gives up,
and the frame ends after it. A `wrap` -- `x op y`, the second argument of
the `asUintN` or `asIntN` call right after it, which the interpreter makes
with the call at once -- is the operator alone, the call made after it as
any call, which native code makes of the operator's low bits (see
[BigInts](#bigints)). A return, a suspension and a throw the function does
not catch are plain Hosts.

`regexp_lit` that calls -- `/x/.exec(s)`, `/x/.test(s)` -- is the literal's
RegExp from its pool, its method read, both in scratch slots, the argument
copied past them, and the call of the method in that window of scratch
slots, a `Window` Host ending the origin (`compile`'s `regexpLit`); the
window's registers, R[A] and R[A+1], are not written, and where they are
live past the instruction it is Go's whole. One that does not call is the
literal and the read, copied to R[A+1] and R[A] last, for the
`call_method_w` after it, a leaf's call of the realm's `exec` or `test`.

**The atomicity rule.** An exit anywhere in an origin's instructions runs
the register instruction again from its start, in the interpreter. So
nothing may leave once something of it has committed (`Program`'s
`CommitsAt`): a register written, memory changed, code run -- a property's
read may call a getter, a store a setter. A field defined on, or a write to,
an object the origin itself made in a scratch slot commits nothing: an exit
before the object reaches a register makes another. An expansion puts what
may leave first and writes its registers last; where a commit cannot come
last, what follows it is `Generic`: it never leaves, what it does not expect
being Go's to make. `add_prop`'s addition and store are Generic after its
read. `ir.Validate` enforces the rule -- a jump only to an origin's first
instruction, a Host alone, every instruction that may leave before any that
commits but the Generic -- and `Evaluate` checks it as it runs: a Generic
instruction whose check fails after a commit is `ErrGeneric`, unless a
`Runner` makes it. The builder keeps the rule too, as it builds (see [The
SSA build](#the-ssa-build)).

**What the tier takes.** The VM compiles plain functions, methods, arrows,
getters and setters, and classes' base and derived constructors, whose
register code is of the form's own layout (`jitRegTakes`): their frames are
the call path's, an arrow's this, a constructor's object or a derived one's
uninitialized this in register 1, as the interpreter has them. A leaf's
body, which the interpreter's calls answer without a frame, is compiled for
the calls that make one. A generator's, an async function's and top-level
code's are compiled too, and entered only where they can be (see [Tier-up
and on-stack replacement](#tier-up-and-on-stack-replacement)).

**Refusals.** `LowerReg` refuses a function kind it does not take, direct
eval, an arguments object the frame cannot own (`compile`'s `argumentsOwn`),
an instruction it has no lowering for, a function of host operations alone
at its entry (`HostOnly`), and a program past the slot budget: registers,
captured bindings and scratch slots together within what a slot's number
names (`abi.MaxSlot`). A function of host operations alone is lowered all
the same for native callers (`LowerRegInline`) or where a call it makes
would be native (`LowerRegHot`, `jitRegCallsNatively`): compiled for that
alone, it is worth its code. A register function has no parameter prologue
-- its defaults are ordinary code over its registers -- so none is refused
for one. A panic in the lowering is a refusal (`lowerRegRecovered`).
`TestLowerRegCorpusRefusals` counts the refusals by reason over directories
of code (`QUICKJS_JIT_REG_REFUSALS`, a path list), beside the builder's; and
`TestJITRefusals` (internal/regdiff) counts, over the micro-benchmarks, the
V8 suite, Octane, Are We Fast Yet, the bundles and test262, why the JIT
would not compile each function from its register code -- what the tier
takes, the lowering's and the build's refusals (`vm.JITRefusals`) -- kind by
kind, and how many of each the stack form takes: a list that only shrinks.

**The evaluator on a register frame.** `vm.RegLowerCall`
(`zzjit_zregrunner.go`) runs a call's register frame by the IR's evaluator
over its program, `EvaluateHost` given a `Runner` backed by the VM
(`regRunner`): what the evaluator would leave at -- a host operation, a
guard that failed, a Generic instruction's slow path -- the VM runs on the
frame's real registers, the whole register instruction, by the interpreter's
own handlers (`regWhole`: its handler from `regTable`, and its slow function
from `regSlowTable` where the handler leaves it that), exceptions to the
frame's handlers by `regCatch`; where the evaluator leaves for good -- a
return it cannot make, the budget -- `runReg` goes on with the frame from
that register pc. Inside an expansion it runs the instruction whole only
where nothing of it has committed. That is what native code's exits do too
(see [Exits and deoptimization](#exits-and-deoptimization)), so
`TestRegLowerEvaluate` checks the lowering and the exits' contract without a
backend.

## Feedback

The builder asks its feedback (`ssa.Feedback` and the optional interfaces
beside it) at each site, by the program's PC; the VM's answer for register
code is `jitRegFeedback` (`zzjit_zregfeedback.go`), which reads each
instruction's origin, its register pc, and answers from two sources.

**The interpreter's caches.** The register interpreter fills them as it
runs: a property site by its register instruction's C (`add_prop`'s read and
store share the store's site), a global's by its name and C, a call's shape
by its operands and its target by its C (`regCalls`), a literal's site,
`instanceof`'s. With the JIT on, a property site's cache also keeps the
shapes it held before the one it holds (`propCache.seen`, `jitNoteSeen`),
which a compile takes for up to `jitPropertyCases` (4) cases
(`jitRegSeenFeedback`). `Property`, `Accessor` and `StringProperty` answer
from the cache and what the code learned (`megamorphicOf`, `deepCasesOf`,
`accessorOf`, `stringPropertyOf`). A call's site answers its callee from its
first compile, a leaf's (`Leaf`) included.

**What native code learns at its exits.** What a function's code learned is
kept by register pc across its compiles, in an entry of its own
(`jitState`'s `regLearned`, a `jitEntry` holding only a `jitLearned`), by
the function's key:

- the register pcs whose guards failed (`Generic`): the next compiles build
  the instructions of that register pc generic, but `regexp_lit`'s
  expansion, which Go makes whole from then on (`regLitFed`);
- the entries whose speculation failed -- at a loop's header or the start, a
  register loaded not of the kind compiled for -- which the next compiles
  enter generic (`jitRegEntrySeen`, `EntryGeneric`);
- a property site's cases, accessors, primitive receivers and megamorphic
  mark, learned at a guard's failure before the interpreter runs the
  instruction (`jitRegDeoptSeen`), and at a host exit that leaves a
  property's read or write to Go (`jitRegHostSeen`, `jitRegWriteSeen`), by
  the functions of either form's operation (`jitPropOp`: `jitDeoptSeenOp`,
  `jitPolySeenOp`, `jitPolyWriteSeenOp`): the site's cache learns the shape
  as the interpreter makes the operation, and what the cache cannot hold the
  entry notes;
- the functions a call was seen to call at its calls of Go and at its exits
  (`jitRegCallSeen`), and the constructions;
- the literals' pools by register pc (`Literal`: `new_object`'s,
  `new_object_lit`'s, `new_array`'s, `new_array_k`'s and `new_regexp`'s,
  made by `jitObjectLiteral`, `jitArrayLiteral`, `jitRegSharedLiteral` and
  `jitRegExpLiteral`), and `new_object_lit`'s fields defined along their
  shapes' transitions (`Define`). `new_array_k`'s arrays share its
  constants, made as the interpreter makes them, where its site does
  still, and are written from the constants loaded to scratch slots where
  it has given sharing up;
- the operators that met BigInts at a guard's failure or a generic one's
  host exit (`BigInt`, `BigIntWide`, `jitRegBigIntSeen`,
  `jitRegBigIntExit`, the stack form's `jitBigIntNote`), which the next
  compiles make natively or call Go for (see [BigInts](#bigints));
- the register pcs after calls Go went on at, which the code had been built
  with no entry at (`Resumed`, `jitRegResumeSeen`).

A guard's failure invalidates the code; anything else learned marks it
stale, to be compiled again without a deoptimization (see
[Tier-up](#tier-up-and-on-stack-replacement)).

**What it answers.** Where the feedback answers nothing, the builder
speculates nothing: it builds the site generically, or leaves it to Go.

| Answered | Methods |
|---|---|
| property sites, accessors, string receivers | `Property`, `Accessor`, `StringProperty` |
| globals | `Global` |
| calls: targets, inlining, calls of Go, leaves, built-ins it makes itself | `CallShape`, `CalledClosure`, `NativeCalls`, `Inline`, `GoCalls`, `Leaf`, `LeafKind`, `Intrinsic`, `CharCode`, `Substring`, `FromCharCode` |
| array loops | `ForEach` |
| objects escape analysis leaves unmade | `VirtualRoot` |
| literals, appends | `Literal`, `Define`, `ArrayProtos` |
| constants | `StringCell`, `StringContent`, `BigIntCell`, `BigIntSole` |
| call frequencies | `Frequency` |
| what exits taught | `Generic`, `EntryGeneric`, `Resumed` |
| steps, `in`, `typeof`, scopes' ends, typed arrays' kinds | `GenNext`, `MapNexts`, `KeyAtoms`, `HasCache`, `TypeofCells`, `WalkedCloseCell`, `TypedKind` |

| Not answered yet | Methods | So |
|---|---|---|
| generators a step resumes | `GenSteps` | the step a call of Go |
| `indexOf`, `parseInt`, `instanceof` | `IndexOf`, `ParseInt`, `InstanceOf` | Go's |
| elements' kinds learned at exits | `TypedRead`, `TypedBigRead`, `KeyedRead`, `KeyedWrite`, `StringRead`, `UpvalueTyped` | generic element access |
| closures and cells from pools | `Closure`, `CellsPool` | Go makes them on the register frame |
| an inlined callee's exits, keeps, BigInts' strings, pure bodies | `InlineExit`, `KeepLimit`, `BigString`, `PureBody` | nothing speculated |

**The feedback dispatcher.** The native handlers come in variants, each a
dispatch table over the same handlers (`internal/vm/internal/nativegen`,
docs/register-interpreter.md's "Native handlers"): plain, the only one in
builds without the JIT and the one a runtime with the JIT off runs; and, in
builds with the JIT alone, feedback, which a runtime that runs the JIT is
given (`nativeDispatch`, `nativeFeedback`). A feedback handler overrides a
plain one only where the JIT needs what no cache holds: it does its part and
goes on in plain's handler of its operation, through a slot of its table
that holds it (`nativegen`'s `slotPlain`), made by the variant's own emitter
(`nativegen`'s feedback.go), so that plain's emitter and handlers know
nothing of it; every other entry is plain's handler. Its calls -- `call_w`,
`call_method_w` and those in tail position -- count the calls made at each
call site, and its returns the function's runs, into the counts of the
function's closure's code (`jitRegCounts`: slot 0 the runs, 1+C the calls at
call site C), made with the call sites' caches where the runtime runs the
JIT (`jitRegCountsMake`). `Frequency` is a site's calls over the runs, once
`jitCountsEnough` runs are counted (zzjit_zregfreq.go); not a leaf's, whose
calls the call path answers without a frame and so without its returns, nor
a generator's or an async function's, whose runs end at suspensions too, nor
a construction's, which Go makes. Native code counts nothing, as V8's
optimized code counts nothing; a run that on-stack replacement took into
native code ends at a return of the interpreter's (`jitRegLoop`), and
counts. Nor does Go's dispatcher count, where it runs register frames in
place of the native handlers (`QJS_NONATIVE`, a verifying build): no
frequency is known there. None ORs the kinds of value it met into the function's feedback slot
yet (the native context's `fv`, kept in a register no handler uses: amd64's
R14, arm64's R15).
`TestRegNativeFeedbackSelected` checks a runtime picks its table, and
`TestRegNativePlainSameWithJIT` that a build with the JIT makes plain's as a
build without does.

**Feedback parity.** `TestJITFeedbackParity` (internal/regdiff) runs a
corpus in two runtimes with the JIT, one on the stack form's tiers and one
on the register tier, and compares, for every function both compiled, each
answer of the stack form's feedback with the register form's at the register
instruction of the same source position -- only where the builder asks the
method (`jitSnapOps`), keys by their names -- and what each form's build
decides (`vm.CompareFeedback`). If register code's feedback answers what the
stack form's does, the code is the same: it is the gate for the switch-over.
A register instruction's position is the first stack instruction's of its
source position, so an answer is looked for at the register instructions of
the same operation a few instructions on, then at those of the same
position, then -- object literals and their fields' definitions, which
register code places at the statement's position -- the k-th of the
operation with the k-th, where both forms have as many, and a field's
definition among those of its key, a nested literal's register code making
the inner literal first. Two shapes of the
same work are paired and listed apart: a RegExp literal that is a call's
first argument, which the register call makes in Go (`RMarkLitArg`) where
the stack form's call of Go is given the literal it made. `BigIntSole`, the
bindings that hold their values alone, each form numbering its bindings its
own way, is compared by what the builds make of it -- the source positions
of the operators that give back their binding's old value -- and only where
the build asks it, where an operator met BigInts. Call frequencies, which
each form counts over runs of its own, are alike where both are known and
on the same side of the inliner's cut (`ssa.MinInliningFrequency`).

## Tier-up and on-stack replacement

No handler of the register interpreter's dispatchers -- the Go handler
table's or the native handlers' -- does any work for tier-up
(`zzjit_zregtier.go`). Hotness is sampled at the two budgets they already
spend:

- a call that spends the interrupt budget -- `rhCallW`'s and `regEnterW`'s
  check, which the native call handler leaves to `rhCallW` -- credits its
  callee (`jitRegCallSample`, `jitRegSample`);
- a back edge that spends the back-edge budget -- `regBackEdge`, which every
  dispatcher's jump leaves to its Go slow function -- credits its frame's
  function (`jitRegLoop`), the budget varied from one expiry to the next
  (`backEdgeBudget`).

**Compiling.** A function is compiled (`jitRegFor`) at its
`jitRegSamples`-th sample (2), or at its first where the runtime compiles at
a first call (`QJS_JIT_STRESS`'s threshold, which makes every call a sample,
`jitRegStressSample`), from what its caches learned meanwhile; its code is
kept on its template's `closureCode` (`jitReg`). A callee native callers
call that has no code is compiled at their first call of it through Go
(`jitRegNativeCallee`): they make no sample of it.

**Compiling in the background.** A runtime made `WithJITBackground`, or
`QJS_JIT_STRESS`'s `background=K`, compiles register code as the stack
form's (see [Its tier-up](#its-tier-up)), in three steps
(`zzjit_zregbg.go`): the runtime's goroutine lowers the code and asks its
feedback ahead (`jitRegSnapshotOf`) -- every question the build may ask,
at each instruction the methods the build asks of its op (`jitSnapOps`),
those asked by a state's PC (`Resumed`, `EntryGeneric`) at each register
pc, of the function and of the callees it may inline, each a snapshot of
its own, to `jitSnapDepth` -- into a `jitRegSnapshot`, which has
`jitRegFeedback`'s methods alone, generated from them (`snapgen`,
`zzjit_snapshot_gen.go`), so that the build finds the same optional
interfaces it would; a worker builds, optimizes and compiles from it
(`jitRegJob`), touching nothing of the runtime's; and the runtime's
goroutine installs it at a later check -- a sample (`jitRegHot`), at a call
or a back edge -- putting back what the questions appended to the
feedback's lists (`replay`) and placing the code as the compile on the spot
(`jitRegPlace`). A question the snapshot was not asked is a miss
(`jitSnapMiss`): the compile is built at the install instead
(`regSnapMisses` counts them). Meanwhile the function runs in the
interpreter, or in the code the compile replaces, and is not compiled again
(`regPendingJobs`); its native callers call it through Go. Top-level code is
compiled on the spot, as the stack form's is.

**Compiling again.** Code is compiled again, in place, when:

- a guard failed: the code is invalidated (`jitRegInvalidate`), its link
  cleared, the function sampled anew and compiled again from what the
  interpreter learned, up to `jitRegDeoptLimit` (8) times, past which the
  function stays in the interpreter -- but a deoptimization that taught
  the code what it is compiled for next, an operator's BigInts or an entry
  after a call Go went on at, which no limit counts
  (`jitRegInvalidateLearned`): what each site learns so is bounded;
- its exits learned what it is to be compiled for -- a call's target, a
  site's case, a callee to inline: the code is stale (`jitRegReopt`), with
  no deoptimization, as V8 never invalidates code for feedback. Its runs go
  on in it until they leave it -- at an exit the code is compiled again and
  the run goes on in the new code (`jitRegGoOnAt`); at a loop's poll,
  which leaves for it rather than calling Go, the interpreter goes on,
  whose next sample compiles it anew -- and its link
  holds no entry meanwhile, so that native callers call it through Go,
  where it is sampled: stale code only native callers reach is compiled
  again too;
- its caches' feedback changed since it was compiled -- what they hold, not
  how often they were filled (`jitFeedbackMark`), as V8's `NotifyICChanged`
  finds it: a sample has it compiled again once it has run `jitICThreshold`
  invocations' worth more (`jitInvocationsAfterICUpdate`, 1 under
  `QJS_JIT_STRESS`'s threshold), a sample's worth where samples are the
  interrupt budget's, as many samples where every call is one. Code compiled
  before a site's cache knew a shape reads it by a scan, which never leaves
  native code to learn one.

**What is entered where.** Of the code compiled (`jitRegTakes`), what
`jitRegCalled` says -- neither a generator's nor an async function's code,
nor top-level code -- is entered at its start by calls, native callers'
among them, and inlined. Top-level code, which runs once, is entered only by
on-stack replacement at its loops, as V8 enters top-level code: its
declarations before them run in the interpreter. A generator's or an async
function's frame, which the interpreter suspends at each `yield` and `await`
and makes again at each resumption (`regGenRun`), is entered at its loops
and where it resumes (`jitRegEnter` from `regGenRun`): the lowering gives
the code an entry at each resumption -- past an `await`, past the jump after
a `yield` that a resumption by `next` goes on past, and at the return path
that jump goes over (`lowerReg`'s entries). The suspensions -- `yield`,
`await`, `yield*`, the parameters' end, a delegation's step -- and
`gen_entry`, which reads how the frame resumed, are the interpreter's
(`regWholeRuns`): native code leaves the frame to it there.

**Entering.** The code is entered:

- by a call whose frame Go makes (`regEnterW`, `jitRegEnter`): a call site
  sampled calling a function that has code is marked `regCallJIT`, which
  neither `rhCallW`'s fast path nor the native call handler enters, so its
  calls go to `regCallSlow` and `regEnterW`, whose test of the template's
  code (`jitRegHas`) is the one test a call pays outside the handlers;
- by native callers, through the function's link (see [Native
  calls](#native-calls));
- by on-stack replacement at the loop whose back edge took the sample
  (`jitRegLoop`), from inside the dispatcher that runs the frame.

**On-stack replacement.** The build's entries at a loop's header
(`ssa.Entry`'s `OSR`) are by the instruction its register pc lowers to first
(`p.At`), so a frame the interpreter runs goes on natively at a loop's
header with the registers live there loaded from the window: the code's OSR
entry map is its loops' headers by register pc (`jitRegEntry`'s `loops`,
`loopEntry`). The dispatcher goes on where the code leaves the frame
(`jitRegGoOn`): at the register pc of a deoptimization or of an instruction
with no entry after it; or, where the code returned, at a `ret_undef` of the
code's own, or at a `ret` whose register no closure captures, given the
value (`findReturns`). Code with neither, where it has a `ret`, is not
entered so. An exception no handler of the frame's takes ends the frame
(`regDone`).

## The SSA build

`ssa.Build` (`BuildIn`, in a workspace) turns a program into typed SSA
(Braun et al.), asking the feedback at each site: a property's shape and
index from the site's inline cache, up to four shapes (polymorphic), the
prototypes a method was found on, a global's cell, the functions a call has
called, a literal's pool. `ssa.Optimize` unboxes numbers, computes in int32s
where the uses truncate, merges guards a dominating one already made,
removes what escape analysis leaves unmade, removes dead code and keeps
references alive across stores and calls (`keep.go`). Its passes ask
dominance of the dominator tree (Cooper, Harvey and Kennedy's), each block
holding its interval of a walk of the tree, which an ancestor's encloses:
linear in the blocks, as a function of 65,600 statements needs. The builder
reads only the IR and the feedback: `ssa` and `mir` do not import
`internal/bytecode`.

**States and entries.** A state's PC is its origin's register pc, its depth
0, and it holds the registers live there: the builder leaves every slot past
`Regs` out of every frame state (`stateSlots`), so no entry loads a scratch
slot and no exit's record writes one, and the scratch slots' writes are no
captured binding's (`capturedUnchanged`). Code is entered only at a
function's start, at its loops' headers, and after what Go is sure to run --
an instruction left to it whole, a call -- as V8's code is entered only at a
function's start and by on-stack replacement at a loop.

**The atomicity rule, as built.** What an instruction commits depends on how
it is built (`atomicInstruction`): a property's read commits only where it
is built to call -- an accessor the feedback gives, or Go for a megamorphic
site's miss, which may run a getter (the builder's `readCalls`); an
arithmetic operation, a comparison or a string's addition, Generic or not,
has Go make only what runs nothing, leaving the rest to an exit. A build
that makes an exit after a commit in the same origin is refused, by its
register pc. The one call an origin may end in after other instructions
(`ir.Program`'s `windowLast`, `regexp_lit`'s) follows only writes of scratch
slots and a read whose feedback never makes it call -- the VM answers none
for an accessor -- so it commits nothing before the call, whose exit runs
the instruction whole again. A Generic instruction is built as one whose
speculation failed before (the builder's `generic`) only where what came
before it in its origin committed as built: after a read built to call
nothing, `add_prop`'s addition speculates on numbers, as the stack form's
`add` does, its exit running the instruction again from its start.

**Windows.** A call, `call_w`'s, `call_method_w`'s or `new_w`'s, and a
`typeof` read their operands from the window's registers, in order -- the
function, the receiver, the arguments -- and write the result to the
instruction's register (`windowCall`); the builder makes each a native call,
an inlined one, a built-in it makes itself, a leaf's call, or a call of Go,
as the feedback says. The calls the builder makes itself -- Math's functions
(`Intrinsic`), a string's `charCodeAt`, `substring`, `slice`, `substr` and
`charAt`, `String.fromCharCode` -- take their operands where the call's form
has them (`callSlots`), the feedback naming the built-in its site's cache
last called. A call of a leaf the feedback names (`Leaf`) is checked to call
that function and made natively where native code makes the leaf itself, a
call of Go of the leaf (`GoLeaf`) otherwise, its result to the call's
register (`windowLeaf`, `leafMade`). `BigInt(x)` and
`BigInt.asUintN(n, x)` or `asIntN`'s are made natively too (see
[BigInts](#bigints)), the width the constant the program copies to the
window's register on the way to the call alone (the VM's
`regConstOperand`). A call in tail position is made as any call while the
stack is shallow (`ssa.OpTailRoom` guarding it), and leaves for the
interpreter deeper, which makes it in constant space.

**Steps.** A loop's step (`iter_next`) writes its register only where the
loop goes on: the builder assigns the step's value in the block after it
(`stepOn`), so the done edge keeps what the register held, and the step is a
call of Go from native code; one whose next instruction something else jumps
to, or an inlined callee's, is left to Go.

**Speculation.** A guard checks what the code was compiled for -- a number,
a shape, a callee -- and exits at the guard's register pc if it fails,
before its instruction commits anything (see [Exits and
deoptimization](#exits-and-deoptimization)). Where the feedback says a
site's speculation failed before (`Generic`), the site is built so that what
it does not expect goes to Go, which goes on after it, rather than
deoptimizing: an element read by its cell, an arithmetic operation or a
comparison of what are not numbers by Go, a property read for a number that
met another value by its cell. An entry whose speculation failed
(`EntryGeneric`) loads its slots without taking them for numbers.

**Numbers.** A loop's phi of numbers is a float64 where a use unboxes it --
any use, where the phi merges nothing but numbers and unboxing it speculates
nothing, as V8's representation selection takes a phi of Float64 inputs --
its box made where a use outside the loop needs it, or by an exit
(`unboxPhis`, `sinkLoopBoxes`).

**Integers.** The slot IR computes every number as a double, a bitwise
operator's ToInt32 of its operands and the conversion of its result back
included; where the uses truncate, the SSA computes in int32s, as V8's
representation selection computes in Word32 (`ssa/ints.go`). A constant's
ToInt32 is an int32 constant (`OpConstI32`), which no instruction makes
unless a phi takes it: an instruction that reads one has it as an immediate
where it can, a shift's count or a mask. The ToInt32 of a sum, a difference,
a negation or a product of int32s, uint32s and integer constants, exact as a
double -- its magnitude's bound below 2^53 (`exactInt`) -- is the int32
operation (`truncInts`), which wraps as the exact result's ToInt32 does:
asm.js's `(a + b) | 0` is one add. A comparison of two such integers --
int32s' conversions, or one and an integer constant of the range -- compares
int32s (`OpCmpI32`), and one of two uint32s' conversions, or of one and a
constant of their range, compares them unsigned. An element's key that is an
int32's conversion is the int32 -- for a read or a write of numbers, a read
of any value as a reference, a store of any value and a BigInt typed array's
read or write: the access reads it sign-extended, an index where it is not
negative, and a call of Go its double.

A loop's phi of doubles that only ever merges int32s -- their conversions,
integer constants of the range, other such phis, and numbers an entry
unboxed, which a guard there checks are int32s and not -0 (`OpCheckI32`),
the entry's state speculating nothing there once one fails -- is an int32
phi where that pays (`intPhis`): an int32's conversion reaches it, its uses
want an int32 -- a ToInt32, a comparison, an element's key, truncated
arithmetic -- and none in its loop wants a double (`intPhiUses`), and its
loop's budget of five general registers holds it, a phi read after its loop
counting two, its interval having no holes (`intPhiBudgets`): amd64's code
has seven general registers for values against thirteen SSE ones. A box of
an int32's conversion, which a loop header's state names before the
conversion is made, is made of the int32: an exit reads the int32 and makes
its number (`abi.ExitI32Reg`, `ExitI32Spill`), and liveness keeps the int32
live wherever such a box is used (mir's `i32Boxed`). A conversion only such
boxes use is made by none, and one in a loop's header that only uses after
the loop read is made after it, as a phi's box is, and so is the box of one
a value after the loop takes -- a sum stored or returned (`sinkLoopBoxes`).

**Compiler panics are refusals.** The lowering, the build and the backend
recover a panic into a refusal, and the function runs in the interpreter.

## What the builder makes

What follows is what the builder and the backends make of the IR, given the
feedback. Each is made for register code where its feedback answers (see
[Feedback](#feedback)'s tables); what it does not answer yet is built
generically or left to Go.

### Properties

**Map checks.** A property read or write is compiled for the shapes its
site's inline cache has met -- the one it holds and those it held before, up
to four (`jitPropertyCases`). A receiver of any other shape, and any
receiver at a site whose cache knows none, deoptimizes (`ir.GuardExit`), as
V8's map checks do and its code does for insufficient feedback: the
interpreter makes the operation, the site's cache learning the shape -- the
deoptimization fills it where the interpreter's read of a small table would
not -- and the code is compiled again for it. A read whose cache is empty,
though the interpreter, which fills none there, may have run it, searches
its receiver's own small table, and deoptimizes only where that has not the
property (`PropertySite.Scan`). What a cache cannot hold -- a primitive
receiver, an object with no shape, an object's own accessor, more shapes
than the cases hold -- makes the site megamorphic, once. Inside an inlined
callee, whose exits all leave their operation to Go, such an exit has the
code that inlined it compiled again so, and is not counted as the callee's
leaving.

**Megamorphic reads.** Only at a site whose cache is megamorphic -- it gave
up (`maxCacheFills`), its cases are full, or it met what no case holds --
does a receiver of another shape have its own table searched and look in the
runtime's megamorphic cache (`abi.MegaCache`) before it leaves for Go, as
V8's megamorphic loads probe its stub cache: an entry, by the receiver's
shape and the key, names the receiver's own data property, or a prototype's
along a chain the code checks object by object and shape by shape
(`abi.MegaChain`), or that none of the prototypes has it, the read's value
undefined (`abi.MegaAbsent`). Go fills an entry at a read's exit. A read
whose receiver's prototypes are no longer its case's -- a method added to
one -- looks there too. A megamorphic site's write that neither the
receiver's own table nor the megamorphic cache has -- an addition, a shape
it never met -- is a call of Go (`ssa.GoCall`) on a branch the builder makes
within the instruction (`splitOn`), the write true where it stored nothing,
native code going on where the two ways meet, as V8's megamorphic stores
call the runtime on a miss; the analyses see the call on its branch alone. A
read of an object there is so too, its two ways each making a value
(`split`): its cell none where it finds no property (`OpCellNone`), the
value loaded from the cell on one branch and Go's on the other, which a phi
takes, the call filling the megamorphic cache as an exit would. A receiver
that is no object -- a string, a number, a boolean, a BigInt, which the site
met (`ssa.PropertySite`'s `Primitives`) -- is read so too, not left for: its
object is none (`OpObjectOf`'s `ObjectOfTry`), and Go reads the property of
the primitive on the call's branch, a getter called with it as this; a read
of undefined or null, which throws, still leaves. In an inlined callee,
whose frame Go cannot make, either leaves.

**Accessors.** A read whose site found a getter on its receivers'
prototypes, or a write a setter -- a class's accessor, or one
`Object.defineProperty` put on a prototype -- is a call of it, as V8's code
calls an accessor its feedback knows (`ssa.AccessorSite`): the receiver is
checked as a read found on a prototype is, the accessor in the holder's cell
checked to be the function compiled for (`ssa.OpAccessorIs`, since a
redefinition changes the pair in place), and the accessor inlined (see
[Inlining](#inlining)), or, where it is not, Go makes the read or the write
from native code (`abi.GoCallGet`, `GoCallSet`), through the site's cache as
the interpreter does, native code going on after it. The receiver is the
getter's this; a setter's result is dropped. Frames Go makes for such a call
show the accessor as the interpreter's do. Register code makes no native
call of an accessor: one not inlined -- a built-in's, a bound function's, a
callee past inlining's bounds -- is a call of Go on the register frame
(`jitRegGoCallOn`, `jitRegPropertyMade`).

**Private members.** A private member is a property under a key each
evaluation of its class mints, a symbol, which a binding the class's methods
capture holds. A read or a write of one is a property's (an
`ir.PropertyRead` or `PropertyWrite` marked `Private`), native in objects of
the shapes its site met, as V8's inline caches learn a private name's maps;
and `#x in o` is true natively where the read would find the member. Two
evaluations of a class share their methods' code, so the code checks that
the binding holds the key it was compiled for (`ssa.OpSameSymbol`). Go makes
the rest -- a member's definition, an accessor, a key in a local, and a
class's or an object literal's other members.

**The in operator.** `k in o` (`ir.HasProperty`) is a call of Go from native
code (`ssa.GoHas`), as V8's code calls its HasProperty builtin: Go searches
the object and its chain as the interpreter does, where that runs nothing
and throws nothing. A proxy on the chain, whose has trap would run, a module
namespace, a key that is an object, whose conversion calls its `toString` or
`Symbol.toPrimitive`, and a right operand that is no object, which throws,
Go refuses: the code leaves for Go to make it there, and goes on after it.

**The delete operator.** `delete o[k]` and `delete o.k`
(`ir.DeleteProperty`) are a call of Go from native code (`ssa.GoDelete`,
`GoDeleteStrict` in strict code), as V8's code calls its DeleteProperty
builtin: Go deletes the object's own property as the interpreter does, where
that runs nothing and throws nothing -- an ordinary object's, its shape
changed as the interpreter's deletion changes it, an array's element, which
becomes a hole, a property absent. A proxy, whose deleteProperty trap would
run, a module namespace, a typed array, a string wrapper, a mapped arguments
object, a key that is an object, a base that is no object, and in strict
code a property that cannot be deleted, which throws, Go refuses, having
changed nothing: the code leaves for Go to make it there, and goes on after
it. The call clears the deleted property's cell, so the analyses take it for
a store of any key (`ssa`'s allocOnly): a value read from that property and
used after it is kept where it is read (`keepAcrossStores`), as across a
call of a function; and every property operation after it checks the
object's shape again.

**Globals and bindings.** A script's globals and its top-level lexical
bindings are read and assigned where they are (`ssa.OpGlobalCell`,
`OpLexCell`). A global's read finds its binding at the position in the
global object's table it was compiled for while the runtime's count of
changes to that table other than appends -- a binding deleted, its
attributes changed -- and of script-level lexical bindings declared is what
it was then (`abi.Context`'s `GlobalEpoch`, the VM's `objGlobalBindings`): a
comparison of the whole count, whose low half comes round again, and of the
object, where it had checked that no lexical binding shadowed the name, that
the key at the position was the name's and that its binding was plain data,
at every read. Past 64 such changes a read is compiled to check them again,
so that a program that keeps deleting globals does not leave its code at
each. A module's bindings are fixed once it is linked, so its code reads
each one in the object that has it, at that object's address
(`ssa.OpBindingCell`), as V8 reads a module's variables in its context and
an import in its exporter's cell. An imported `const` is a constant.

### Built-ins and leaves

**Built-ins.** As V8 reduces calls of built-ins it knows, some are made by
the code itself, each call checking it calls the realm's function: `Math`'s
functions of numbers and `Math.random` (`ssa.Intrinsic`), called as methods
or kept in a global's or a closure's binding and called plainly (`P =
Math.clz32; P(x)`), a string's `charCodeAt`, `String.fromCharCode`, a
one-character `substring`, `slice`, `substr` or `charAt`, `parseInt` of a
number, an array's `push` and `pop` on their fast paths, and `new Array()`
or `new Array(n)` from a pool. Strings' and arrays' `indexOf`, `lastIndexOf`
and `includes` are a call of Go of their own (`ssa.GoIndexOf`), which
answers where the built-in runs nothing. Anything else such a call meets is
Go's.

**Leaves.** A built-in that runs no script where its check passes -- a leaf:
String's methods, Number's, a Map's or a Set's operations, `Date.now`,
`Object.prototype.hasOwnProperty` and `Array.isArray` of what is neither a
proxy nor a module namespace, an array's `push` of any number of values,
`RegExp.prototype.exec` and `test` -- that a call has been seen to call is a
call of Go of its own (`ssa.GoLeaf`), checked to call that function, as V8's
code calls a fast API function: native code jumps to the VM's routine for
leaves (`abi.Encoding`'s `LeafFast`), the call's description naming the leaf
(`abi.GoCallState`'s `Leaf`) and the receiver's and the arguments' words
written past it, and the routine applies the leaf as the form it made of the
call at its first says (the VM's `jitLeafForm`) -- no expansion of the call,
no dispatch of calls of Go -- and comes straight back, the result's number
word in a register. Some leaves native code makes itself, as V8's call
reducer reduces the builtins it knows (`ssa.LeafKind`): `codePointAt` and
`at` of a flat string's code units (`ssa.OpStringCodePointOr`,
`OpStringAtOr`), the leaf's call made on a branch of its own where native
code says nothing, and Number's `isNaN`, `isFinite`, `isInteger` and
`isSafeInteger` with no call at all (`OpNumberIs`). A method call of the
realm's `Function.prototype.call` of a leaf, `G.call(recv, ...args)`, is G's
call of recv, as V8 reduces `Function.prototype.call`:
`hasOwnProperty.call(o, k)` is the leaf's.

**Calls of Go through the routine.** Every other call of Go whose site saves
its live registers goes through the VM's routine for calls of Go
(`abi.Encoding`'s `GoFast`), which calls the VM's dispatch of them directly,
Math's functions Go computes (`ssa.OpMathCall`) among them, saving what
lives across them rather than every register. What only a call's refusal
reads after it, its state's, is restored on the refusal's way alone (mir's
cold saves).

### BigInts

As V8's code makes its BigInt64 values, an operator of BigInts the
interpreter saw held in their values (`abi.Encoding`'s `ShortBigTag`) -- or
one with an operand native code has unboxed already, a BigInt typed array's
element, whatever the site met -- is made on them unboxed (`ssa/bigint.go`),
in the root or in a callee inlined, whose own feedback the builder asks: as
Int64s, or, where the site met BigInts past the int64 range from 0 to 2^64-1
-- a 64-bit hash's or generator's -- as Uint64s. The arithmetic exits where
a result leaves its type's range, for a division by zero and for the most
negative int64 over -1; the bitwise operators exit nowhere; a shift is by a
constant. `BigInt(x)` of a number is made natively, and `BigInt.asUintN(n,
e)` and `asIntN`'s, of a width the call passes as a constant up to 64, check
the function they call and cut `e`'s low bits; where `e` is an operator that
only such operators and the call take, it is made on its operands' low 64
bits alone, as V8 truncates them to machine words, its guards and the call's
exiting to the state before the first of them, which nothing since has
changed. Register code's operator writes a register no closure captures,
which what takes it reads last (the program's `Live`); its call's width is
the constant the window's register is given on the way to it. A loop's BigInts stay in unboxed phis, and a box only frame states
name is made nowhere but at an exit, which makes the value of the unboxed
BigInt (`abi.ExitLoc`'s `IsBig`), as V8's deoptimizer materializes one.

The rest -- BigInts on the heap -- Go makes from native code
(`ssa.GoArith`), giving back what the interpreter would of its operands'
blocks (`ssa.CallSite`'s `Release`). A sum, a difference, a product, a
quotient or a remainder is made first by the VM's leaf (`abi.Encoding`'s
`BigFast`, `jitBigLeaf`), in fewer steps than Go's call: native code reaches
it as it reaches `callGo`, through a routine the Go runtime sees as
`callGo`, a function its caller called; it decodes the call itself, makes
the result in a block given back, the value before's, or a new one -- as
V8's code allocates a BigInt inline -- gives back as the call would, and
leaves what would throw to the call.

Register code's operator writes its register itself, so a result stored
over a value the register holds alone gives that value's block back
(`ReleaseOld`, `bigOwner`), and an operand is fresh where an operator wrote
it earlier in the block, nothing read it since and nothing reads it after
(`regBigFresh`, the program's `Live`) -- not a register of those that hold
their values alone, whose old values only their own stores give back. The
registers that do (`BigIntSole`, `regSoleRegisters`) are those past the
parameters no closure captures, read only as operators' operands, tests and
branches: a move, a call's window, a return, a global's store, a property's
read or a postfix update of one has it hold its value no more, and an
instruction the analysis does not know has the function none.

### Typed arrays and elements

An element read or write of a number whose site has met typed arrays of one
element type is compiled for that type, as V8's keyed loads and stores
follow their feedback's elements kind: the receiver's class checked to be a
typed array's, the key below its view's length, the type's byte compared
with the one expected, and the element loaded or stored at its width --
inline, an Array's element out of line (`ssa.TypedKindOf`). As V8's
JSTypedArray holds its data pointer and its length, a view holds where its
first element is and how many it has (`abi.Encoding`'s `TypedElems` and
`TypedLength`), a copy of its buffer's that holds while its epoch is the
runtime's (`abi.Context`'s `TypedEpoch`): a detach, a resize, a growth --
whatever moves a buffer's bytes or changes their length -- advances that,
and code that finds a view's copy of an epoch before has Go make it anew
(`ssa.GoTypedView`), then reads it again. A view detached, or out of its
buffer, has none, so that every key fails the bounds check and Go decides.
The byte after the type's is set for an immutable buffer's view, which
native code reads with it for a store, so that no type matches; the one
after that where the copy's length is no length to give -- a view detached
or out of its buffer, whose methods throw, or tracking a growable shared
buffer's length, which another agent may grow under it -- which native code
then leaves to Go. The element, the view, its copy's three words: two loads
before the element's, none of the buffer's.

A site that has met none, or several, tells them apart as it runs, a case
for each type, out of line; so does a site compiled for one type that meets
another, which keeps native code that only native callers run from leaving
for Go at every access until it is compiled again. The interpreter keeps
nothing of a site's types, so native code learns them, as V8's baseline code
fills a feedback vector: each site has a cell, its function's lowering's
(the VM's `typedCells`), with a bit for each type it met, which code reads
when it reads or writes an element of a type it was not compiled for; where
the cell has no bit of it, a call of Go sets it (`ssa.GoTypedSeen`) and
marks the code to be compiled again, as for what an exit taught it. A site
that has learned nothing, of what a global read found holding a typed array
(`ssa.GlobalSite`'s `TypedKind`), or the read of a captured binding holding
one as the code is compiled (`ssa.UpvalueTypedFeedback`), is compiled for
that array's type from the first -- Emscripten's heaps, globals or an asm.js
module's bindings its functions capture, read at every branch, would
otherwise teach a compile each. A store of any value native code does not
make Go makes from native code where it runs nothing (`ssa.GoElemStore`),
a BigInt's low 64 bits in a BigInt64Array's or a BigUint64Array's element
among them (the VM's `jitStoreBigElem`).

### Array loops

A call of `Array.prototype`'s `forEach`, `reduce`, `map`, `filter`, `some`,
`every`, `find` or `findIndex`, or a typed array's method of those names, is
made a loop of the code's own, as V8's JSCallReducer reduces it
(`ssa.ForEachSite`): the array checked and its length read once, then each
element given to the callback, called natively. `map`'s and `filter`'s
arrays Go makes (`ssa.GoNewArray`, where the array's species is `Array`),
and the code appends to them; a typed array's `map`'s is a typed array of
its type and length, where its species is its type's own constructor, which
the code stores each result in as a number, and its `filter`'s an `Array`
the code appends the elements it keeps to, of which Go makes the typed array
after the loop, as the method makes it then. A BigInt64Array's or a
BigUint64Array's element is its type's word (`ssa.OpBigElemRead`), boxed
where a use needs a value, and its `map`'s result's low 64 bits are stored
as one (`ssa.OpBigElemWrite`); its `sort` is Go's. A hole, or anything the
loop's checks find, leaves, and Go goes on with the loop from that index
(`abi.Context`'s `Loop`), then native code after the call.

The callback is inlined in the loop where any callee may be, under the
inliner's rules and bounds (`ssa.ForEachSite`'s `Inline`), as V8 inlines it:
the one closure the loop has been given, which the loop checks it is given,
or a closure the caller makes just before the call (`ssa.MadeCallback`),
which, given to nothing else, the code never makes, as V8's escape analysis
removes it: what it captures the inlined code reads and assigns as the
caller's own slots, in place, and a frame state names its template where the
closure would be, which Go makes the closure from at an exit, as V8's
deoptimizer materializes an object. An exit inside the callback has Go make
its frame past the loop's, finish it and go on with the loop after its
index; one that leaves too often has the callback called natively again.
Where every closure the code makes is such a callback, a native call's frame
gives its cells back to their pool as it returns (`ssa.Func`'s `CellsBack`),
and a scope's end gives it none fresh: nothing else can hold them, as V8's
escape analysis leaves a context only such closures read unmade.

Register code's loop is a `call_method_w` its code's calls of Go have made
of such a built-in on an array of the realm's (`jitRegForEachSeen`), the
code compiled again for it (`jitRegFeedback`'s `ForEach`): its operands
the call's window's registers (`callSlots`), its result the call's
register, its slots past the window and the scratch slots, where the
frame's homes and loops' slots are. Its callback is called natively
through its link, its window made from the loop's operands
(`ssa.CallSite`'s `Window`), or, the one closure the loop has been given,
inlined in the loop, its frame its window (`callbackInline`); one made
anew at each call is called by its code. A loop that leaves -- a check of
its own, its poll, a callback with no code to call or that left native
code -- Go goes on with on the register frame from its index
(`jitRegLoopLeft`, `jitLoopFinishAt`), the callback's answer first where
its call was made, and native code after the call.

### Allocation and escape analysis

**Allocation.** Object, array and regular expression literals, closures and
their captured cells (see Closures), and the receivers of constructions, are
taken from pools of objects the VM makes ahead (`abi.ObjectPool`), as V8's
code allocates inline; Go refills a pool that runs out. A pooled array has
storage for at most `abi.PoolRoomLimit` (16) elements: a `new Array(n)` of a
longer n is made by Go.

**Escape analysis.** As V8's TurboFan does, an object a pool would give the
code -- an object literal's, the receiver of a construction the code inlines
-- that nothing but the code sees is not made (`ssa/escape.go`), where the
feedback says how Go makes it at an exit (`ssa.VirtualFeedback`): its
properties are the values stored to them, held in SSA values, registers and
spill slots, and a read of one is that value, unboxed where a number is
read. What it does not make keeps the checks its making and its operations
made of the heap: the constructor's prototype (`ssa.OpProtoIs`, which
deoptimizes once it is replaced), an addition's prototypes intercepting
nothing (`OpChainCheck`), a method's holders on the object's prototypes
(`OpHolderCell`). An object escapes, and is made, where anything else may
see it and the code cannot make it there: a phi merging it with anything but
objects not made, and any operation whose shape escape analysis cannot
follow -- its stores change its shape along the transitions its sites'
caches know, the same on every path to a merge, where phis merge its
properties.

Beyond TurboFan's, as Graal's partial escape analysis has it, a phi merging
objects not made of one shape at the merge, none of them used past it, is an
object not made of its own, at a loop's header too, its properties phis: an
accumulator a loop replaces each turn (`sum = sum.add(v)`) is its properties
alone. A use that needs an object made -- a call's operand, a store into an
object that is made, a return -- has the code make it there, from its pool,
its properties defined along the additions that made its state, and it is
that object past there. An object made on every path anyway is made where it
is taken: left unmade it would save nothing, and its compile pays for each.
It is decided before the walk of the function (`pruneMade`), so that such an
object costs what one that escapes does. A phi merging it with the slot an
entry loads, at an entry there only for Go to go on at after an instruction
it ran (`ssa.Entry`'s `Resume`), or at a loop's header Go has never needed
(`OSR`), is the object: the entry is dropped (`syncEntries` keeps it dropped
only for an object left unmade in the end), and a compile asked for with a
frame waiting at a loop keeps that loop's entry and makes the objects it
would have been dropped for (`ssa.Func`'s `FrameAt`, `EntryKept`).

Where the code exits, as V8's deoptimization data names a materialized
object, the frame state names it by a word no value has (`abi.VirtualBase`),
with the first slot of its home past the frame's slots, where the state
holds its shape and its properties' values (`ssa.FrameState`'s `Virtual`):
Go makes the object of them, one for each home, which every slot naming it
is given, the words of others among its properties made too, as V8's
deoptimizer materializes them (on a register frame, `jitRegMaterialize`).
The frame takes the homes as it takes its loops' slots (`ssa.Func`'s
`HomesEnd`), its callees' frames starting past them, and checks the VM's
stack has room for them where it does not make the object (`OpFrameRoom`).

**Closures the code only calls.** A closure the code makes that nothing uses
but the code's own calls of it -- `const sq = (x) => x * x + n` called in a
loop, a function expression made and called in a loop's turn -- is a call
whose target the code knows, as V8's graph knows it once the closure is a
constant of it: the function is built twice (`ssa/called.go`), the first
build's SSA saying which closures they are -- its values used only as a
call's callee, the call's check of it, a frame state and phis merging it
with itself or with the slot an entry loads of the local it is kept in, out
of every loop, and no scope's end closing what it captured before a call --
and the second inlining their calls with no check of the callee and leaving
the closures unmade, as a callback the caller makes is (`MadeCallback`):
what they capture is read and assigned as the caller's slots, a frame state
names the template, Go makes the closure at an exit, and an exit inside the
inlined call has Go make its frame calling that closure. The entry after the
closure, there for Go to go on after making it, is dropped. Where it makes
no other closure, its cells go back to their pool too (`CellsBack`).

### Closures and cells

Code whose locals an inner closure captures keeps them in cells of its own
(`ir.Program`'s `Cells`), as V8 allocates a function's context: the
context's `Cells`, each an upvalue holding its value, which native code
reads and assigns through it, and frame states leave out. On a register
frame the k-th captured register's cell is the k-th cell: on the run's own
frame the cells are open on the window's registers, the upvalues its
closures share, which the frame closes as it ends; a native callee's are
closed cells holding the registers' values, which Go opens on its window
where it makes the callee's frame (`jitRegReopenCells`). A closure is made
of them as the interpreter's `regClosure` makes one, and a scope's end
(`close_regs`) gives its registers fresh cells holding what the old ones
held (`ssa.GoClose`).

A call of a closure that captures bindings checks the function the callee
runs, in its environment and realm, not the object (`ssa.CallSite`'s
`ByCode`), and gives the callee the closure it finds, as V8 checks a call's
code: a callback made anew at each call of its maker is a new object, the
same function. A closure is inlined where its call checks its object, which
fixes its closure and so the cells of its captured bindings: an inlined
frame reads and assigns them there (`ssa.InlineSite`'s `Upvalues`), as V8's
inlined code reads a context it knows, and an assignment has the caller read
its own captured bindings again, which may be the same cells.

### Control flow

**Exceptions.** The compiler emits structured code, so the handlers in force
at each instruction are known when lowering (`ir.Program`'s `Handlers`), and
native code registers none: wherever Go takes the frame at an exit it gives
it those handlers. A throw from what Go does there -- a call, an operation,
a throw statement -- is unwound to its handler as the interpreter unwinds it
(on a register frame, `regCatch`), and native code goes on at the handler,
an entry of its own, as V8's code goes on at a handler. A finally clause's
normal completion goes on natively, and so does a return, where no other
finally clause is in force around it and no loop's cursor is beneath the
completion record (`ir`'s `Rethrow`, `When`); any other (a throw)
deoptimizes, the interpreter reproducing it. A native callee may have
handlers: the frame Go makes where it leaves has those in force there, which
the interpreter's unwinding of a throw finds; an inlined callee may not
(`ssa.InlineCalls`). A using declaration's scope is such a try statement,
its resources added and disposed of by Go.

**Iteration.** A for...of or for...in loop's cursor, and `yield*`'s
delegate, are Go's. Native code steps an array's for...of in place, a Map's
or a Set's entries (mir's `mapStep`), and a for...in over the keys of its
object's layout or over an array's indices -- each the VM's string of its
index, which an element read by it finds again (mir's `forInIndex`,
`keyedRead`); any other step is a call of Go, which answers the next value
or, done, the uninitialized value native code branches on. A step's iterator
is closed however the loop is left, as the interpreter closes it.

**Generators' steps.** A for...of's or a destructuring's step over a
generator (`ssa.CallSite`'s `Gens`) resumes it by a native call, as V8's
code calls a generator's resumption: the step checks the cursor's generator
-- of a function it has met, suspended, not in a `yield*`, stepped by the
realm's `next` -- and jumps to the generator code's generator entry (mir's
`genEntry`), which makes the frame from the generator, its locals the
generator's own, and goes on at the entry for its place (`ssa.Func`'s
`GenResumes`). A yield there writes the frame, saves the generator's place,
operands and handlers as Go's suspension would and returns the value to the
step (`ssa.Block`'s `Yield`); a return finishes it and returns the step's
done. Anything else leaves as a native callee does, and Go finishes the
resumption from the generator. Only a synchronous generator without `yield*`
whose locals no closure captures is resumed so. Register code compiles no
generator yet, and its feedback names none a step resumes (`GenSteps`): this
serves the stack form's path until it does.

**Destructuring.** Destructuring from an iterator takes a cursor, as a
for...of does: Go makes it, called from native code (`abi.GoCallForOf`).
Each element is a step that goes on with undefined once the iterator is done
(`ir.IterStep`'s `When`, `abi.GoCallFill`), native over an array walked in
place and Go's otherwise, as a loop's step is. Its end closes the iterator
natively where there is nothing to close -- the cursor done, or an array's
-- and by Go where a return method must be called (`ssa.OpIterClosed`); a
for...of's break closes it so too. What is left, for a rest element, is
Go's.

## The backend

`mir` selects instructions, allocates registers by linear scan and encodes
amd64 or arm64 machine code (`CompileWith`, the target's; `CompileAMD64` and
`CompileARM64` make either on any machine). The two backends share one core
(`core.go`) and move together: a change to how the JIT compiles lands in
amd64's and arm64's in the same commit.

**Register allocation.** `mir`'s scan (`allocate`) gives each value an
interval from its definition to its last use, stretched over every block it
is live in or out of. A frame state's slot is a use, which keeps the value
live for the exits that read it, but not a read the code makes: a value's
cost in a spill slot counts only its definition and the uses that read it --
an operand, a branch's control, a phi's argument at its edge, whose move
reads the argument and writes the phi, where a phi that is its own argument
moves nothing -- each weighed by how often it runs, 16 to the power of the
loops holding it (at most three). A phi and its arguments are hinted to one
register, and joined into a group where their intervals do not meet, so that
a value carried round a loop keeps one. Where no register is free, the scan
spills the interval whose cost in a slot is least for what spilling it frees
-- the rest of it, from the new interval's start, each position weighed as
its block -- as LLVM's linear scan weighed intervals by their uses'
frequency over their length; of those alike, the one that ends last. A
budget loop's register goes first, its loop then counting in memory. A phi's
edge move to a slot its phi or argument already holds, and may share, costs
nothing, and so is taken off the cost of that value in a slot (`inSlot`):
spilled together, the two share the slot and the edge moves nothing.
Spilled, an interval is in its slot whole: stored where it is made, loaded
at each use.

An interval whose uses but frame states' are all behind the new interval's
start, each in its own block, none past a call that saves the registers nor
by a call that reads its operands again after Go had the registers
(`callsAgain`), is split instead when that costs less: it keeps its register
from its definition to its last such use, and the code stores it to its slot
as it is made; its register is the new interval's from there. Its cost is
that store, its uses reading the register. Every frame state reads a split
value from its slot, wherever it is -- an exit's description and its code, a
native call's state and records, the arguments a call copies from its state
-- as the backend reads locations there through the slot (`core`'s
`exitView`); the rest of the code reads the register, which its uses all
come before. So a value read once by the next instruction and then only by
the frame states up to a call -- a global's cell and the function read from
it, whose method the call is -- costs a store, not a store and a load
straight after it.

An entry block loads each slot the code reads (`OpLoadSlot`) and unboxes the
numbers among them, each unboxing a guard. A load whose only use but frame
states' -- which leave its slot as it is -- is an unboxing in its own block
is made by none (`findLazy`): the unboxing reads the slot, which nothing in
an entry block writes, so that an entry at a loop's header, which loads
every live register, holds no general register for the loads across its
guards.

An int32 operation's result takes the register of an operand whose interval
ends at it (`takesOperand`): each backend reads the operands before it
writes the result.

What lives across a native call in a register is saved to a slot of its own
and restored after (a split value is never live there in its register). A
call that takes an object from a pool is made again from its start once Go
has filled the pool (`ssa.GoRefill`), so its operands -- a register code
literal's elements, in scratch slots no frame state names -- live across
it too (`ssa.RefillsPools`); tests make every call of Go fill the registers
with junk first (`jit.CallGoClobber`).
`CheckAllocations` has every compile check its allocation against liveness
computed again by block sets and its uses listed: no two intervals meeting
in a register or a slot, a split value's register only to its last use but a
frame state's.

**Registers.**

| | amd64 | arm64 |
|---|---|---|
| Never written | SP, BP, R14 (g) | SP, R18 (platform), R28 (g), R29 (FP), R30 (LR) |
| Fixed | DI the context, SI the frame's first slot (`Locals`), DX its first operand (`Stack`) | R0 the context, R1 `Locals`, R2 `Stack` |
| Scratch | AX, CX (shift counts), R11; X0, X1, X15 | R15, R16, R17, R26; F0 to F2; R27 the encoder's |
| Clobbered | the rest, X15 included | the rest, R19 to R25 included |

ABI0 callers expect clobbering: on amd64, Go restores X15 and R14 after any
ABI0 call. A native call passes where it returns to, the callee's closure
and its link in fixed registers (amd64's `callRet`, `callClosure` and
`callLink`, arm64's `a64CallRet`, `a64CallClosure` and `a64CallLink`), and
the callee's return hands the result's number and pointer words back in
registers. `verify` checks these rules for every function its corpora
compile.

**Workspaces.** Compiles reuse their workspace's analysis tables and
assembler storage (`jit.Workspace`, the SSA's and `mir`'s). On arm64, that
includes the byte buffer the assembler's instruction words are encoded into
(`FinishInto`). A workspace compile's bytes hold until its next compile;
placing the code copies them into the executable arena before the workspace
is returned. The buffer counts toward the workspace's memory and is trimmed
with its other scratch storage when recent compiles need less. Compiling
without a workspace returns independent bytes.
The arm64 table used to patch Go calls' failure branches is scratch too;
it drops its pointers to the calls' descriptions when the compile ends.

## Native code on the register frame

**The frame.** The backend sees the register frame as its locals:
`FrameLocals` is `Regs`, so that a register is a word of the window, which
the context's `Locals` addresses in place -- the VM's stack never moves, so
native code reads and writes the frame's registers where the interpreter
does. The captured bindings' slots past them are read through their cells
(`Context.Upvalues`). The scratch slots past those live inside one register
instruction. Where one is in memory -- a run of operands an operation reads,
`new_array`'s elements, `concat`'s parts -- it is past the window: the
context's `Stack` is past the scratch slots (`ssa.Func`'s `Scratch`), and
what escape analysis leaves unmade has its homes past them, then its loops'
slots (`HomesEnd`, `LoopsEnd`). The VM's stack's top covers them while the
code runs.

**The context block** (`abi.Context`) is what native code reaches through
its context register. Go sets the frame's addresses before every entry;
native code writes the exit record and spills, and, for a native call, the
callee's context, the next. The fields most code reads or writes -- the
frame's, how an exit left, a return's and a native call's, and the constants
-- come first, within a byte's displacement of the start; `mir` reads every
field at its offset (`abi.Off*`), never by the VM's types.

| Fields | What |
|---|---|
| `Locals`, `Stack`, `Upvalues`, `This`, `Consts` | The frame: its first slot, its first operand past the window, its captured bindings' cells, its receiver, and constants native code compares and loads. |
| `ExitKind`, `ExitPC`, `ExitDepth`, `ExitSite`, `ExitDesc`, `Ret`, `RetFrom`, `RetValue` | How the code left: the exit's kind and state, or a return's result. |
| `Records`, `Record`, `RecordRef`, `RecordHigh`, `RecordAt` | An exit's records, room for `abi.MaxRecords` (256). |
| `Spill`, `Keep`, `Regs`, `XRegs` | Spill slots (`abi.SpillSlots`, 256), the references the code keeps where the collector sees them (`abi.MaxKeeps`, 256), and the registers a shared routine saves. |
| `BackEdges` | The interpreter's back-edge counter, which every back edge decrements. |
| `GoOp`, `GoArgs` and the call's words | A call of Go: the operation, its arguments' count and kind (see [Calls of Go](#calls-of-go)). |
| `Next`, `Prev`, `Live`, `Level`, `Base`, `Closure`, `Function`, `NewTarget`, `ReturnTo`, `Args` | Native calls: the callee's context, which runs a call's callee or describes an inlined frame (`LiveCall`, `LiveInline`), its window, closure and function object, where it returns to, the arguments its frame keeps. |
| `StackBase`, `StackEnd`, `StackTop`, `StackHigh`, `HighSeen` | The VM's stack, for the room a callee's frame needs. |
| `Global`, `GlobalLex`, `LexNames`, `GlobalEpoch` | A script's global and lexical bindings, and the count of changes to the global object's table. |
| `TypedEpoch`, `Mega`, `Hints`, `Loop`, `Shade`, `Bounds`, `TailFrame` | The typed arrays' epoch, the megamorphic cache, the hint table, an array loop's index, the marking's shading cell, the levels' limits (`abi.Bounds`' `LevelLimit`, `TailLevel`), and a frame a tail call gives up. |

The contexts are the runtime's, an array of them, a native call's callee in
the next: 4 at first, as many more each time a call finds none past its own,
up to `jitContextsMax` (96), some 17 KB each. A call past the deepest is a
call of Go.

**The value encoding** (`abi.Encoding`) is given to the backend as data, so
that `mir` never imports the VM. A VM value is `ValueSize` bytes, a number
word and a pointer word (`NumOffset`, `RefOffset`): the number word holds a
float64 below `abi.NumberLimit`, and every tag is at or above it --
undefined, null, booleans, the uninitialized value, a BigInt held in the
value (`ShortBigTag`) -- the pointer word the object, string or block a
reference points to. A word no value has, `abi.VirtualBase` with a home and
a count in its low bits, names an object escape analysis left unmade. The
encoding also gives the offsets of what native code reads of the heap -- an
array's class, flags, length and elements, a property table by its shape, a
typed array's view (`TypedElems`, `TypedLength`), a cell's value
(`UpvalueSlot`) -- and the addresses of the routines native code jumps to
(`CallGo`, `StoreRef`, `LeafFast`, `GoFast`, `BigFast`).

**Reading the heap in place.** Native code reads the VM's objects where they
are, through the offsets `abi.Encoding` gives, rather than through copies:
an array's elements, a property table, a binding's cell. A reference it
reads keeps the address of the cell it came from as its source, and Go reads
the reference back from there at an exit. Reading an address back as a
pointer is outside what `unsafe.Pointer`'s rules promise; it is sound while
Go's heap does not move and nothing else changes the object graph while
native code runs, which a `Runtime` (not safe for concurrent use)
guarantees. It is checked with each new Go: `TestJITObjectLayout` pins the
fields native code reads, and the JIT's tests run under `-d=checkptr=2` and
`GODEBUG=gccheckmark=1`.

**Values and the collector.**

- Native code holds a slot's number word, never its pointer word. A
  reference stays in the slot that held it at entry, which native code does
  not write before it exits; an exit has Go copy it where it goes, and store
  a primitive over a reference (`abi.Record`). An ambiguous phi's shadow
  tells Go, at run time, which slot a value came from.
- It reads an object through the pointer word of the slot that holds it, at
  the offsets `abi.Encoding` gives.
- A store of a pointer tests the runtime's write-barrier flag first, as
  compiled Go does, and while the collector marks is made with Go's barrier
  (see [The write barrier and the marking
  rule](#the-write-barrier-and-the-marking-rule)).
- References it must keep across calls of Go are kept in the context
  (`Context.Keep`), where the collector sees them.
- It keeps no address between entries.

## Calls of Go

What needs no frame -- an addition, a concat, a leaf, a comparison, a poll
-- Go makes from the words the code passes (`jitRegFeedback`'s `GoCalls`),
and native code goes on after it, without leaving: native code jumps to
`callGo` with the operation in `Context.GoOp`, and Go answers in the
context. As V8's deoptimization data describes a call's frame, a call of
Go's operands are a static description (`abi.GoCallState`) of where the code
keeps each value -- a register its shared routine saved, a spill slot, a
constant, the frame -- which Go reads only when it needs them
(`jit.ExpandGoCall`): a site writes little more than the description's
address.

**On the register frame.** A call Go makes for native code -- a call's site
that has no native target, a construction, a getter or setter not inlined --
Go makes on the run's register frame (`jitState`'s `regRuns`,
`jitRegGoCall`, `jitRegGoCallOn`), as the interpreter's slow path does
(`regCallSlow`, `regConstruct`): the function, the receiver and the
arguments put in a window of their own past the VM's stack's top -- never in
the frame's, whose registers the code may still hold values in as it found
them -- and the frame's pc past the call meanwhile. A call that threw, that
the runtime stopped in, or after which the code was found invalid, its exit
finishes (`regPending`, `regFinishCall`): the answer to the call's register,
or the exception to the frame's handlers, never the call made again.

**Instructions run whole.** A whole Host (`ir.Instruction`'s `Whole`) is
a call of Go (`ssa.CallSite`'s `Whole`, `abi.GoCallWhole`) that runs the
register instruction at its PC on the register frame, by the interpreter's
own handlers (`regWhole`), as V8's code calls a runtime function for what
it has no code of (`jitRegWhole`): the registers it reads are given the
values the code passed, its result, the register it writes, goes to the
call's keep cell, and what it wrote in the frame is put back -- the code
keeps values in the frame's registers as it found them -- but a captured
register of the run's own frame, where its cell holds it. On a native
callee's frame its level's frames are made first, as for any call of Go
that runs script (`jitRegGoCallNested`). What does not go on at the next
instruction -- a throw, a handler of the frame's that took one, the
runtime stopped, the code found invalid -- the exit at the call finishes
(`regPending`, `regFinishWhole`): the register the instruction wrote, or
the handler's, written over the exit's records, the instruction never run
again.

**On the frame's own state.** What reads only its operands and the frame --
a `new.target`, a rest parameter's array and an unmapped arguments object of
the arguments the frame kept, `arguments[k]` and `arguments.length` of them,
the cells of code whose captured registers are in cells, a closure, a
scope's end -- Go makes from native code on the register frame
(`ssa.GoHost`, `GoArgument`, `GoCells`, `GoClosure`, `GoClose`;
`zzjit_zregframe.go`, `jitRegFrameOp`): the run's own frame, or a native
callee's, which has none of the VM's -- a stand-in of its closure, function
object and receiver, its window at its context's `Base`. Left to an exit: a
mapped arguments object, which aliases the registers; a native callee's rest
parameters and arguments, which its calls keep none of; and a closure a
native callee makes that captures by value, which reads its window's
registers. Where the code's captured registers are the frame's own slots,
not in cells of their own, a closure and a scope's end on the run's own
frame are the interpreter's handler's (`jitRegFrameWhole`): the closure
captures the upvalues open on the slots, the scope's end closes them.

## Native calls

A call native code has seen call a function with native code is made
natively, as V8's code calls another function's: the callee's frame is made
in the VM's stack, its context is the next one, and its code is entered
through a link (`jitLink`) that holds the function's current code, so
callers keep working when the callee is compiled again or its code is
dropped.

**The call.** As V8's callee builds its own frame, the callee's code makes
its frame and context at its call entry (`mir`'s `Code.CallEntry`), once for
all its callers -- the VM stack's high water raised past the frame only
where the context last found it lower (`abi.Context`'s `HighSeen`, which Go
zeroes wherever it lowers the high water): a call writes only what it knows
-- the function, the receiver, the arguments -- and jumps there with where
it returns to, the callee's closure and its link in registers, and the
callee's return hands the result back in registers (`abi.Context`'s
`ReturnTo`). The call's state is a static description (`abi.CallState`),
every register the window does not hold as it is, which Go expands only
where it needs the caller's frame (`jit.ExpandCall`).

**Register functions' calls** (`zzjit_zregcall.go`). A register function's
link (the runtime's `regLinks`) holds its code's call entry while it has
code native callers may call -- set as the code is compiled, cleared as it
is invalidated, made stale or dropped -- and, where it holds none, Go makes
the call from native code (`jitRegGoCall`), as a call of Go. The callee's
window is made at the VM's stack's top, past the caller's window, scratch
slots and frame (`ssa.CallSite`'s `Window`): the call writes the function to
its register 0, a method call's receiver to 1 -- or a plain call's undefined
or global object, an arrow's captured this, for a callee that reads it --
and the arguments from 2, undefined in its parameters past them, and checks
the room its link says its code takes (`LinkFrame`). The callee's code makes
its frame at its call entry: its context the caller's next, its registers
past the parameters undefined, its stack's top past its window, scratch
slots and frame (`Func`'s `Scratch`). The result comes back in registers to
the call's keep cell and register.

**Which calls.** The feedback says which functions a call makes natively
(`jitRegFeedback`'s `NativeCalls`, `regCallSites`): the one its site's cache
holds (`regCalls`), and those the code learned it calls at its calls of Go
and at its exits (`jitRegCallSeen`), up to `jitCallTargets` (4), each
checked by its object, or, a closure its maker makes anew -- another object
of a function the call has called -- by its code (`ssa.CallSite`'s
`ByCode`), where the function runs register code of the form's own layout of
the runtime's realm, and its frame is one the call path makes as a call's
(not a class's constructor, nor one that keeps its arguments or shares a
derived constructor's this). A call that meets any other function leaves
native code for Go, which learns there what to call next. A construction
(`new_w`) of a plain function, a base constructor that does not read
`new.target`, is made so too, with the constructions its code learned: its
receiver taken from a pool of objects made as `regConstruct` makes one
(`jitNewPool`, `ssa.CallSite`'s `Pool`), in the callee's register 1 and its
context's `This`, which the result is where the callee returns no object. A
callee native callers call that has no code is compiled at their first call
of it through Go (`jitRegNativeCallee`): a function of host operations alone
is lowered for them (`LowerRegInline`).

**Polls.** A native call is no interpreter's, which spends the interrupt
budget: the callee's code spends the back-edge budget at its start
instead (`ssa.OpPoll`, see [Polls](#polls)), and calls Go when it runs
out, so that a native recursion meets the deadline and the memory limit.

**Levels.** No frame of the VM's is made for a native call. Where Go runs
while native callees are under way, it makes each level's frame as
`regEnterW` would -- its function, its window, past the call it makes --
from the context (`Closure`, `Base`) and the level's code, which it finds in
the cache:

- a call of Go a native callee makes (`jitRegGoCallNested`) is made on its
  level's frame, each level below it given its own for the call, as the
  interpreter's frames are during a call;
- a callee that leaves native code -- an exit, a call of Go that threw or
  found its code invalid -- has every native level finished by Go
  (`jitRegUnwind`): the innermost from where it left, as a run of a frame's
  code goes on from an exit (`jitRegLeft`); each caller from its call, with
  what its callee answered: on in its native code where the call returns to
  (`SSACode.Resume`, the result in the callee's context's `RetValue`), if
  its code is still valid, or, its call's state written to its window
  (`jit.ExpandCall`), by Go, the answer to the call's register or the
  exception to the level's handlers (`regPending`), on from there; and the
  run's own code goes on after the call the same way (`jitRegStretch`). A
  frame given to a callee by a call in tail position (`errRegRestart`,
  `errTailCall`) is finished as `regEnterW` finishes one.

While Go runs beside them, every level's code is pinned (`jitRegEntry`'s
`levels`): the cache neither drops it nor compiles the function again until
the level is done, so that the code a level runs is its function's in the
cache for as long as it runs. Both backends make such a call natively, the
same way (`mir`'s `call`, `windowSetup`).

## Inlining

**What is offered.** A plain call, a method's or a construction of a base
constructor (`call_w`, `call_method_w`, `new_w`) of one function -- the one
its site's cache holds and its code learned it calls, no other, not checked
by its code -- that runs register code native callers may call, not the
caller itself, nor an arrow, is offered with its register code lowered for
an inlined frame (`LowerRegInline`, `jitRegFeedback`'s `Inline`,
`ssa.InlineSite`'s `Window`), asked by its own register feedback, what its
sites' caches have seen added first (`jitRegSeenFeedback`), the root's keep
holding what its sites name. A getter or setter a register read or write
calls (`jitRegFeedback`'s `Accessor`, `accessorOf`) is offered the same way
(`AccessorSite`'s `Inline`), once the receiver and the accessor are checked
(`accessorOp`).

**What the builder takes.** What a function inlines is bounded by size
alone, as V8's JSInliningHeuristic bounds it, never by a count of calls: a
callee's instructions, a budget of all a function inlines, which the calls
made most often get first, and past it only small callees (`ssa`'s
`maxInlineTotal`, `maxInlineSmall`), as deep as `maxInlineDepth`. How often
is V8's call frequency where the feedback knows it (`Frequency`): a call
made less often than `ssa.MinInliningFrequency` (0.15, V8's
`min_inlining_frequency`) for each invocation of the function compiled is
not inlined, small or not, and is made a native call; the most frequent come
first, a call whose frequency is not known taken as made once outside the
loops and more often inside one (`ssa`'s `inlineOrder`). Register code's
frequencies are what the feedback dispatcher counted (see
[Feedback](#feedback)). A callee's
throw, or a call its own code has never been seen to make, does not keep it
from being inlined: native code leaves the inlined frame there, as V8's code
deoptimizes at a call with no feedback.

**Callees with loops.** A callee with a loop is inlined where it is small --
at most 24 instructions (`ssa`'s `maxInlineLoop`) -- and leaves nothing to
Go anywhere (`smallLoop`): a search of a short list, three property reads
and a compare a turn, a few turns a call, costs about what a native call's
sequence and entry do, and adds little code where it is inlined. That holds
for every frame an inlined callee may be -- a call, a getter or setter, a
construction, a closure the caller makes, a loop's callback -- and for the
build that inlines what callees are given (`ssa.InlineCalls`,
`InlineCallsGiven`). A larger loop, or one with a call, is inlined only
where the loop calls a closure its caller makes and gives it --
`forEach(fn)` given `(e) => e === obj` -- that closure inlined at the call
and not made (`ssa.InlineCallsGiven`). Such an arrow that reads `this` reads
its maker's receiver where it is inlined (`ssa.InlineSite`'s `LexThis`), as
V8's inlined arrow reads a context slot of its maker's; a built-in loop's
callback, which may come from any frame, and an arrow in a derived
constructor, whose this may not be bound yet, are not taken so. Elsewhere a
larger loop's call is made once for all its turns, so inlining it saves that
call spread over them, against the code it adds to its caller; what inlining
a loop can save at every turn is what the caller makes known to it, the call
of the closure it gives, and the closure, made at every call. The function
is built twice, as for a closure it only calls: a first build, where the
root makes a closure, inlines the callees that call what they are given,
leaving those calls to exits, whose states say which calls are of a closure
the root gave them, used by nothing else (`calledClosures`); a second
inlines the closure at those calls, by their paths through the frames, with
no check, and leaves it unmade -- and its own SSA checks what the first
could not see past its exits: that the closure is used by nothing but frame
states and that every such call calls it (`unmadeAlone`), the second build
refused otherwise. The first is never the code. A loop in an inlined frame
has its own header, whose state is the frame's: its poll and its exits make
the inlined frames.

**The inlined frame.** The callee's frame is its window, laid in the
builder's slot space past the caller's (`inlineWindowCall`): the function,
checked to be the callee, in register 0, a method call's receiver -- an
object where the callee would coerce it -- or a plain call's undefined or
global object (`ThisCell`) in 1, the arguments from 2, undefined in the
rest; its captured bindings read in their cells (`Upvalues`); its result to
the call's register. An inlined accessor's window is its function, from a
cell (`InlineSite`'s `FunctionCell`), the receiver and a setter's value, a
getter's result the read's register. A construction's receiver is taken from
the pool of the objects its code learned it makes, kept in a cell, in
register 1, which is the result: the callee returns nothing or it
(`returnsWindowReceiver`), and every state inside it holds register 1, live
or not, for Go to make the result of where it finishes the callee. The
atomicity rule holds in it as in the root: an exit inside it runs its
register instruction whole again (`atomicInstruction`, each frame's
`readCalls`). Its own calls are asked the same way, of its own caches and
what its code learned, and inlined in it in turn.

**Its exits.** An exit inside an inlined callee writes the caller's state at
the call -- a host exit there -- and the callee's window past the caller's
slots, each level in a context of its own (`abi.LiveInline`). Go finishes
them (`jitRegInlined`, from `jitRegStretch` for the run's own code and from
`jitRegUnwind` for a native callee's): each level's frame made as
`regEnterW` would at its window, the outermost first, a construction's with
its new.target, its result its receiver where the callee returns no object;
the innermost finished in the interpreter from its exit, learning there as a
frame's own exits do (`jitRegInlineLeft`); each other from its call with
what its callee answered (`regFinishCall`); and the caller's call given the
outermost's answer (`regPending`), as a native callee's. A guard's failure
there has the caller's code invalid, as one of its own does; an exit that
learned something else -- a site's case, a change of the callee's caches
(`jitFeedbackMark`), a read's value not a number (`jitNumberMissed`), which
the code compiled again reads generic -- has it compiled again for it
(`jitRegReopt`). One that learned nothing counts against inlining at the
call (`jitInlineLeft`, `jitInlineExits`, `notInline`): past it, the code
compiled next makes a native call there. None counts that left at a site
of the callee's -- a property's read or write whose site is not
megamorphic, a construction whose pool ran out (`jitRegLeftAtSite`) --
nor one of stale code, nor one whose operation threw, which the callee
called natively leaves native code for just the same.

## Exits and deoptimization

**State maps.** Each reachable instruction has a state map: its register pc,
and, for the stack form, its live operand depth. An exit names one, and its
records say what is written into the frame: the registers live at its
register pc.

**Exits** (`abi`'s exit kinds, in `Context.ExitKind`):

| Kind | What happened | What Go does on a register frame (`jitRegRun`) |
|---|---|---|
| `ExitDeopt` | A guard failed, before its instruction committed anything. | `runReg(f, c, pc)` runs the rest of the invocation from the exit's register pc; the failure is noted, the code invalidated, and the function compiled again at its next sample from what the interpreter learned. |
| `ExitHost` | The instruction is Go's. | Go runs the whole register instruction at the exit's pc by the interpreter's own handlers (`regWhole`: its handler from `regTable`, its slow function from `regSlowTable`, as `runReg` and the native handlers' exit stub run them), an exception to the frame's handlers by `regCatch`; native code goes on at the entry of the register pc the instruction went on to, and where the code has none, `runReg` runs the rest of the invocation from there. |
| `ExitPoll` | `BackEdges` ran out at a loop's header. | The interpreter's back-edge check; native code goes on at the loop's header. |
| `ExitReturn` | The code returned. | `Ret` holds the result's number word, or `RetFrom` where the result is. |
| `ExitEnter` | A native call's callee had no code native callers may call. | Its frame is made, its arguments in it, and Go runs it from its start; its caller's code goes on after the call. |
| `ExitTable` | The exit left its frame to Go to write. | Go writes it from the exit's description (`abi.ExitDescriptor`, `jit.ApplyExit`) and the registers it saved, then reads the exit's own kind. |

**Rebuilding the register frame.** An exit's records write the registers
live at its register pc, in the window, natively as the code leaves (or Go,
from its description, for an `ExitTable`); then Go makes the objects escape
analysis left unmade into the registers that name them
(`jitRegMaterialize`), and goes on by the exit's kind. A frame native code
called natively that Go finishes, or one where Go goes on at a register pc
with no entry, runs in the interpreter to the end of the invocation,
entering native code again only where a sample sends it there.

**Contracts.**

- A guard fails before its instruction commits anything, and an exit inside
  a register instruction's expansion leaves nothing of it committed (the
  atomicity rule), so the interpreter runs the instruction whole.
- Work already committed, earlier loop iterations included, is never
  replayed.
- Code is entered only at a function's start, at its loops' headers, and
  after what Go is sure to run.
- A native callee that cannot go on leaves its frame to Go, which finishes
  the call in the interpreter, and its caller's code goes on after the call.

**Learning at exits.** A property site's guard that fails -- a receiver of a
shape its cases do not have, a site that knew none -- has the site learn it
before the interpreter runs the instruction (`jitRegDeoptSeen`); an entry's
that fails notes the entry (`jitRegEntrySeen`); any other guard that fails
notes its register pc (`Generic`). A property read or write a host exit
leaves to Go is learned too (`jitRegHostSeen`, `jitRegWriteSeen`); where
that teaches the site something its code was not compiled for, the code is
stale from there. See [Feedback](#feedback) and
[Tier-up](#tier-up-and-on-stack-replacement) for what follows.

**How many slots.** Nothing bounds a function's slots -- its registers, its
captured bindings and its scratch slots -- but what names a slot by its
number: a record's or a run-time source's word below the code's slots is a
slot, and one past them an address, and an unmade object's home is 16 bits
of a word (`abi.MaxSlot`, 65,536). The context has room for `abi.MaxRecords`
records (256), which every state of a function of up to that many slots
fits: inlined callees' frames, native loops' slots and the homes of objects
escape analysis leaves unmade all stay below it. A function of more takes
none of those, and each of its exits and native calls is counted where its
code is made (mir's `recordRoom`, `callState`), the code refused if one
would write more. Its spills still have the context's 256
(`abi.SpillSlots`). Code of 300, 1,000 and 5,000 locals runs natively
(`TestJITManySlots`).

## The execution boundary

**Entry.** `enterSSA` is `NOSPLIT|NOFRAME`. It tail-jumps into the code with
the context block in a register, and the code returns with a plain `RET` to
`enterSSA`'s Go caller. Native code:

- never moves SP or touches the frame pointer;
- never executes a call instruction: it calls another function's native code
  by jumping there, with that level's context, and Go by jumping to `callGo`
  (`abi.Context.GoOp`);
- uses Go's ABI0, not the platform C convention, on every OS.

**Calls of Go.** To the Go runtime, `callGo` -- and so each of the VM's
routines native code jumps to as it jumps to `callGo`, for leaves, for other
calls of Go and for BigInts' operators (`LeafFast`, `GoFast`, `BigFast`),
which call the VM's Go directly, and the barrier's `storeRef` -- is a
function the Go function that entered the code called: its frame and return
address are all a traceback, a stack scan or the stack's growth meets. Go
may do anything there, run JavaScript included, and the code goes on where
Go says.

**Polls.** Every back edge decrements the interpreter's back-edge counter
(`Context.BackEdges`), and so does register code's start (`ssa.OpPoll`),
where every call enters it -- a native caller's, which no interpreter's
call counts, above all -- as V8's prologue checks its interrupt budget:
a recursion with no loop, run natively, is checked as the interpreter's
calls check one. When the counter runs out, the code calls Go (or, under a
stress budget, exits to it), which checks for cancellation, `Halt` and the
memory limit, gives a new budget, and lets the code go on. The call is the
budget's alone: the collector's marking changes nothing of it.

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
differential testing (see [Validation](#validation)).

## The code cache, budgets and memory

**The cache.** Each runtime keeps its own cache, weakly keyed by the shared
bytecode function and the realm the code is for (`jitKey`); shared bytecode
carries no JIT state. Register code's entries are the cache's `regCache`, by
those keys, with the links native callers call them through (`regLinks`) and
what their code learned (`regLearned`); a template's `closureCode` holds its
current code (`jitReg`) for the calls Go makes. An entry goes when its
function does (`jitKey`'s `gone`), when the runtime releases its JIT, and
when it is compiled again -- closed then unless a run is still in it.

**Realms.** Code is its realm's, as V8's optimized code is its native
context's (`jitKey`): its pools hold that realm's objects, and it is
compiled against that realm's prototypes and functions. A program run in two
realms warms up and compiles twice, and a call of another realm's function
leaves native code for Go to switch the realm. Code never meets a realm it
was not compiled for, so that nothing in it checks.

**Refusals.** A refusal is an entry with no code, and marks the template
refused (`closureCode`'s `jitRegRefused`), so that its samples ask nothing
more -- but a function of host operations alone, which is lowered again once
hot again (`hostOnly`). A compile the budget has no room for leaves no
entry, and is asked for again at a later sample. Past `jitRegDeoptLimit`
deoptimizations a function is refused too. When the OS denies executable
memory, the runtime marks the JIT unavailable for good.

**Debugger.** Runtimes made `WithDebugger` never run native code.

**Allocation.**

- A runtime's code lives in its `Arena`: chunks of 64 KiB (a larger function
  gets a chunk of its own), each function placed after the last at 64-byte
  alignment, so that a runtime's functions share a few mappings
  (`TestJITArenaMappings`).
- Placing a function makes the pages it touches writable, copies it, and
  seals them read/execute again. Pages are never writable and executable at
  once, and published code is never patched. Other code in those pages does
  not run meanwhile: an arena is one runtime's, and its native code always
  returns to Go before Go compiles anything.
- A chunk is unmapped when the last function in it is released. The arena
  locks, since a finalizer can release code on another goroutine.
- Windows flushes the instruction cache with `FlushInstructionCache`.
- Darwin uses `ic ivau` with barriers, over 64-byte lines: Apple's data
  caches are coherent with instruction fetch, and reading `CTR_EL0` traps
  there (`flush_darwin_arm64.s`).
- Linux on arm64 does what libgcc's `__clear_cache` does
  (`flush_linux_arm64.s`): `CTR_EL0`, which Linux lets user space read,
  gives the smallest data and instruction lines and whether either step is
  needed (IDC, DIC); each data line is cleaned to the point of unification
  (`dc cvau`), a barrier, each instruction line invalidated (`ic ivau`), a
  barrier, and an `isb`. Code is mapped read/write and sealed read/execute
  by `mmap` and `mprotect` as on linux/amd64 (`memory_unix.go`); the arena
  reads the page size from the OS, and its 64 KB chunks are whole pages on
  4, 16 and 64 KB kernels alike. `TestRewrittenCodeRuns` rewrites a function
  in place thousands of times and runs each, which a missing or short flush
  fails on hardware (an emulator such as qemu sees writes to code without
  one).

**Darwin policy.** Darwin refuses executable memory to hardened or
code-signing-enforced processes (`ErrUnavailable`, `policy_darwin.go`): the
tier has no `MAP_JIT` path. Linux has no policy of its own to ask
(`policy_linux_arm64.go`).

**Not the script's memory.** The memory meter never counts the JIT, so a
script's measure, and whether it fails with `ErrMemoryLimit`, is the same
with the JIT on and off. The JIT has a budget of its own (`jitBudget`):
`WithJITMemoryLimit`'s, or 8 MB, or an eighth of the memory limit.

**The budget bounds all the JIT holds** (`jitState.held`), as V8 bounds its
code and what describes it by memory, not by a count of functions:

- each entry: its machine code as placed, its entries' offsets and its
  exits' descriptions (`SSACode.Footprint`), what it was compiled with and
  what it has learned, and its object pools with the objects made for them;
- the programs it lowered, one lowering a function (`ir.Program.Footprint`);
- the contexts native code runs in, the hint table, the links, and code a
  suspended call of Go still holds.

Not counted: the arena's free space between live functions, which it does
not reuse until a chunk empties, and the workspace a compile is made in,
which is the process's, the runtime's only while it compiles. Runtime
construction allocates nothing for the JIT.

**Register code's share.** Register code takes at most half the budget
(`regBytes`), the stack form's code the rest while both exist; a register
compile that would pass it, or for which the cache cannot make room, is not
placed. Register code's entries are not aged out as the stack form's are:
they go with their function, as the runtime releases its JIT, or when
compiled again.

**Making room** (`makeRoom`). A compile asks for room for what its entry
will hold before its code is placed. The cache reclaims, in this order, each
least recently used first: lowerings of functions it has no entry for;
refusals remembered; code not used in the last two epochs (old, as V8
flushes code by age); the workspaces. Code used within two epochs, code that
runs, and an entry being compiled are never reclaimed. If all it may reclaim
would not make the room, it reclaims nothing and the compile is refused: the
function runs in the interpreter until some code is old. An epoch is a
collector cycle, or 50 ms.

**Release.**

- `ReleaseClosed` releases code once native frames have unwound; code a
  suspended run still uses stays until the run leaves it, and a level Go
  runs beside native callees pins its code (see [Native
  calls](#native-calls)).
- A failed release keeps ownership for a retry.
- A finalizer on `SSACode` reclaims what a host abandoned.

## The write barrier and the marking rule

**The rule: native code does the same work whether the collector marks or
not.** A path compiled natively stays native while the collector marks: it
stores its pointer words through `storeRef` (below), takes objects from its
pools as the paragraphs below say, and goes on. It never tests the
write-barrier flag to leave native code, call Go instead or make an object
by Go's hand, nor to leave to Go what it writes as it leaves, an exit's
records (`recordsTail`, below). The rule has no exception: it holds for
every new native path, and a path that cannot yet keep it is not compiled
natively.

The reason is more than the calls themselves. Work that leaves native code
only while the collector marks makes code look slow, or makes it exit and be
compiled again, at whatever moment a collection happens to run, and
allocation-heavy programs mark much of the time. A test of a native path
checks it while the collector marks (`jitMarkingForTest`) as well as
plainly. Holding the collector off in a test only hides a breach of the
rule.

**The barrier.** Native code tests the runtime's write-barrier flag before
it writes a pointer, as compiled Go does. While the collector marks, it
calls Go's barrier, as V8's code calls its own: a store of a pointer word --
a property's value, a property added along a transition and its shape, an
element, a keep -- jumps to `storeRef` (`entry_*.s`), which, as `callGo`, is
to the Go runtime a function the code's Go caller called, and calls
`storeRefGo`, a nosplit Go function that stores the word, so that the
compiler's barrier shades the pointer it overwrites and the one it writes. A
routine each code shares saves the registers the code allocates and restores
them after; `storeRef` keeps only the context's register (amd64: the word's
address in AX, the pointer in CX, where to go on in R11, the context in DI;
arm64: R15, R17, R16 and R0, with the link register). A nil over a nil, a
primitive's over a primitive's, has nothing to write or to shade, and the
routine goes straight back from it, nothing saved. Shading is skipped only
for what is known to be shaded in this cycle, and Go tells native code
nothing of that: a stale answer frees a live object. The flag cannot change
between the test and the write: Go sets it only with the world stopped,
which native code, never a preemption point, is not. Native code does not
emulate the barrier's buffer: its contents would have to reach the collector
before any safepoint Go could stop the goroutine at.

**Native calls.** A native call is made natively whether the collector marks
or not, with no barrier in its setup: what it writes -- the caller's
records, the callee's context and its frame past the VM's stack's top, the
callee's locals made undefined -- each outlives none of what it is written
from (the caller's frame, keeps and records, which last until the call
returns), and overwrites only what nothing reads again, an earlier call's.
The one store that outlives its source is the result's, into the caller's
keep cell (and a `super()`'s into its `This`), from the callee's frame,
which the next call overwrites: while the collector marks, its pointer word
goes through `storeRef`, after a return or after Go finished a callee that
left native code (`RetValue`). So Go resumes native callers, and they call
on natively, whether the collector marks or not.

**What moves a pointer into what outlives it** does so through `storeRef`
while the collector marks: an object taken from a pool has its pool's cell
cleared through it, which shades the object, or, a construction's receiver,
is shaded in its pool before the call takes it; what native code then writes
into it -- an array literal's elements, a closure's captured cells, the
arguments in an entry's cells -- and a value pushed or popped go through it
too, as do the pointer words of the calls of Go native code makes itself,
made alike whether the collector marks or not (mir's `byMarking`): an object
spread by layout and a literal's field along its shape's transition -- the
values, the shape, the result -- an array spread and an element appended --
the elements, the result -- and an array's length written shorter, whose
elements past it are cleared so, which shades what they held.

**A loop's step** copies a pointer into a keep cell with no barrier, by that
same rule read the other way: an array's element, a Map entry's, a
`for...in` key off the layout's keys the cursor walks, or the VM's string of
an array's index, which its table of them holds for as long as it lives
(mir's `iterStep`, `mapStep`, `forInStep`, `forInIndex`), and an element
read by a key (`keyedRead`). The cell does not outlive what the pointer came
from -- the array, the table, the keys -- so nothing moves into what
outlives it; the index's string recorded in the VM's table of key strings,
which does, goes there through `storeRef` while the collector marks. Two
things make that enough. The cell holds one value of the code's own, so the
one it overwrites is a definition of that value nothing reads again, never a
reference something else needs. And a loop body that takes the element out
from under the step removes it with a barriered store, which shades what it
overwrote, so the collector keeps it for the cycle in hand. A pointer no
live structure holds -- a string made for an index, a call's result -- has
no such cover: it goes through `storeRef`, as the result's store does, or it
is not written natively at all.

**Shading first.** What native code holds where the collector does not look
while it rewrites what the value came from it shades first, storing each
pointer through `storeRef` into the context's `Shade`, so that a collector
scanning the frame midway misses none: a call in tail position's operands,
read into `GoArgs` before the frame is written over; and an exit's records
(`recordsTail`), whose first pass reads every value they write into the
records themselves -- one record may read a slot another writes -- and whose
second writes them, each pointer word through `storeRef`, which shades the
one it overwrites too. `storeRef` keeps no scratch register, so the record
the passes are at is kept in the context (`RecordAt`). So an exit writes its
frame natively, and a yield suspends its generator natively, whether the
collector marks or not.

**What Go applies.** Go applies records only where native code never wrote
them, the same whether the collector marks or not: a native call's caller's
state, which the call describes rather than writes
(`abi.Context.CallState`), Go expands (`jit.ExpandCall`) and writes when the
callee leaves native code (on a register frame, `jitRegApplyRecords`); and
an exit that leaves its frame to a description (`abi.ExitDescriptor`) Go
writes from it (`jit.ApplyExit`). Nothing is Go's for the marking's sake,
and nothing leaves native code for it: code is compiled only with `storeRef`
(`abi.Encoding.StoreRef`).

`TestJITSameWorkWhileMarking` holds both forms' code to the rule: every test
of the micro-benchmarks is run with native code seeing the collector idle
and marking, by the stack form's code and by code made from register code
(named `reg:`), and must make as many calls of Go, leave as often, have the
interpreter finish as many runs and have Go apply as many records in both.
Its list of known breaches only shrinks.

## Regular expressions

A runtime made `WithJIT` compiles a regular expression's pattern to machine
code, on amd64 and arm64, as V8's Irregexp compiles one: the first match
over a kind of subject is the matcher's, and the second makes the code
(`nativeAfter`, V8's tier-up). The kinds are an ASCII string's bytes and a
string's UTF-16 code units, each with its own count, its own code and its
own refusal. Under `QJS_JIT_STRESS`'s threshold the first match makes it.
The code is placed and entered as `RawCode` (`raw.go`, `enterRaw`), with a
context of the regexp package's own, and keeps the execution boundary's
rules.

**The code** (`internal/regexp/native_gen.go`) is the whole search, not one
attempt: it resets the captures, counters and marks, passes over the
positions no match can begin at -- the pattern's first units a word at a
time, a class's a unit at a time -- and under each attempt pushes the
position and the address of the attempt's failure. Each of the program's
instructions is made as the matcher's run does it, in registers: the
position a pointer into the subject, a backtracking stack of its own whose
choice points are the addresses of the code that resumes them, and whose
capture, counter and mark writes push the code that undoes them, as V8's
code does, rather than a trail. A required literal is still found by Go
(`strings.Index`): the code returns when the search has passed where it was,
and Go finds the next and enters where the code left off. What a character
matches is worked out as runs of code points when the code is made
(`native_sets.go`), from the matcher's own tests: a few runs by comparisons,
ASCII by a 128-byte table, the rest of the BMP by a two-level bit table or a
compare tree. One generator writes both machines' code through a macro
assembler (`native_masm.go`), as Irregexp's writes through its
`RegExpMacroAssembler`, so every instruction's code changes on both at once.

**The same work as the matcher.** Steps are counted exactly as the matcher
counts them, so the 100M-step budget fails at the same instruction and the
interrupt check is asked at the same step. The code returns to Go to grow
its stack, for a check or the budget, and every `nativePoll` (65,536) steps,
since it is never at a safe point. A pattern the code does not do -- a
lookaround, an instruction that reads leftwards, a backreference under the
`i` flag over UTF-16 -- is refused for that kind of subject, and matched by
the matcher.

**Placing it.** The VM gives the regexp package a host (`regexp.NativeHost`,
`internal/vm/zzjit_regexpnative.go`), so that `regexp` imports nothing of
the JIT's: `Place` puts the code in the runtime's arena and `Room` counts it
against the JIT's budget, making room as a function's compile does. Code the
budget had no room for is remembered by its size, and asked for again every
`nativeRetry` (64) matches before it is made again. A pattern's code is
given back by a cleanup whose argument holds only the code and the host's
count, never the host: the host reaches the runtime, and so the pattern, and
would keep every JIT runtime alive.

**Calling it from native code.** `RegExp.prototype.exec` and `test` are
leaves (see [Built-ins and leaves](#built-ins-and-leaves)): native code
calls them without a frame, for any pattern, global and sticky ones
included, given a string and a writable `lastIndex`; so is a string's
`replace` by a RegExp that is no literal's, of two arguments, which
register code's feedback answers where the call's site holds
`String.prototype.replace` (`Leaf`). A match that would fail -- its
step budget spent, the check's interrupt met -- errs with nothing written
(`errRegExpLeafAgain`), and the call is made again with a frame, which
throws; the match already found past the budget is remembered
(`regexpLeaf.spent`), so it is not made twice. The leaves' checks ask
whether `RegExp.prototype` and `RegExp` are as the realm made them
(`propsIntact`), which a pristine layout answers at once, as V8's protectors
do, and every way of changing a watched property clears.

## Zero cost when off

**Without the tag.**

- `jitBuilt` is false, and every hook in the interpreters -- the register
  interpreter's, the stack interpreter's and the tree tier's -- is either an
  empty stub or inside `if jitBuilt`, so it is compiled out.
- The native handlers hold no feedback dispatcher: the binary has plain's
  table and handlers alone (`zzzyn_native_nofeedback.go`).
- In `internal/regexp`, `nativeBuilt` is false (`native_off.go`), and the
  search's hand-off to native code is a branch on that constant, compiled
  out with it.
- `internal/cmd/hotdiff` checks the stack interpreter's and the tree tier's
  hot functions while they remain: compare an untagged `cmd/qjs` at main
  with one at the change: `executeAt`, `runTree`, `runTreeNested`,
  `callObject`, `callTree`, `runFD` and `backEdgeCheck` must be the same,
  instruction by instruction. The native handlers are pinned by their
  generated files' staleness tests (`TestRegNativeGenerated`).

**With the tag, JIT off.**

- A runtime with the JIT off is given plain's dispatcher, the one a build
  without the JIT runs (`nativeDispatch`), which a build with the JIT makes
  as a build without does (`TestRegNativePlainSameWithJIT`).
- The register interpreter pays a sample hook only where a budget runs out,
  and one test of the template's code in `regEnterW` (`jitRegHas`); no
  handler tests anything for the JIT.
- The JIT's fields are in what a template's closures share (`closureCode`'s
  `jitCodeFields`, empty without the tag): `closure` is 56 bytes in both
  builds (`TestClosureSizes`).
- `Runtime`'s and `Realm`'s JIT fields sit at the end of their structs, so
  no offset the interpreters read moves. `TestJITFieldLayout` checks both
  builds. The native handlers read the fields whose offsets the tag moves
  relative to a base no tag changes (`rtBase`, `ccBase`), so that one
  handlers' block serves every build.
- Measure the remaining flag tests with `placements`.

## Validation

```sh
go test ./internal/jit/... ./internal/vm                      # untagged
QUICKJS_REQUIRE_JIT=1 CGO_ENABLED=0 go test -tags quickjs_jit ./internal/jit/... ./internal/vm ./internal/regdiff .
QJS_JITREG=1 go test -tags quickjs_jit ./internal/vm .        # JIT runtimes on register code
go test -tags quickjs_jit -race -count=1 ./internal/jit/...
go test -tags quickjs_jit -gcflags=all=-d=checkptr=2 ./internal/jit/... ./internal/vm
go test -tags quickjs_jit ./internal/jit/compile -run '^$' -fuzz '^FuzzLower$' -fuzztime=30s
(cd internal/jit/verify && go test -tags quickjs_jit ./...)   # encodings, both arches
TEST262_DIR=/path/to/test262 go test -tags quickjs_jit ./conformance \
  -run TestConformance -count=1 -timeout 60m -args -conformance.jit
```

| Check | What it holds |
|---|---|
| `TestRegLowerEvaluate` (internal/regdiff) | The lowering and the exits' contract without a backend: each call of the register interpreter's differential corpus run by the IR's evaluator with the VM as its runner (`RegLowerCall`), compared with an ordinary call in a second runtime that has run the same calls. Its cases run in parallel, the micro-benchmarks in 16 parts, each function called with 3 sets of arguments; `QUICKJS_REGLOWER_FULL=1` calls each with 8 and runs the micro-benchmarks whole, `QUICKJS_REGLOWER_ARGS=n` sets the sets of arguments, `QUICKJS_REGLOWER_SHARD=k/n` runs a shard. `-short` skips it. |
| `TestRegNativeCall` (internal/regdiff) | Native code made from register code: the corpus compiled at each function's first call and entered by `vm.RegNativeCall` -- the call's frame made as `regEnterW` makes it, run natively from its start, or, by the evaluator with the VM up to the second time it reaches a loop's header, from there -- each answer compared with an ordinary call's. |
| `TestJITRegNative` (internal/vm) | Functions run so, the collector seen idle and marking. |
| `TestJITFeedbackParity` (internal/regdiff) | Each answer of register code's feedback against the stack form's at the same source position, and what each build decides. |
| `TestJITSameWorkWhileMarking` (internal/vm) | The marking rule, for both forms' code over the micro-benchmarks. |
| `TestLowerRegCorpusRefusals` (internal/jit/compile) | Refusals by reason over directories of code (`QUICKJS_JIT_REG_REFUSALS`). |
| `ir.Validate`, `Evaluate` | The atomicity rule, on every program and as it runs. |
| `CheckAllocations` (mir) | Every compile's allocation against liveness computed again; the VM's tests turn it on. |
| `TestJITObjectLayout`, `TestJITFieldLayout`, `TestClosureSizes` | The fields native code reads, and that the tag moves nothing the interpreters read. |
| `verify` | Both targets' encodings against `golang.org/x/arch`'s disassemblers, and the register rules. |

- `QUICKJS_REQUIRE_JIT=1` fails a native test job that would otherwise pass
  by falling back.
- The VM's JIT tests force promotion and assert native entry, so they can't
  pass in the interpreters.
- The JIT's tests run under `-d=checkptr=2` (CI's native-boundary job) and
  `GODEBUG=gccheckmark=1`, for the heap read in place.
- `QJS_JITREG` set to any value has runtimes with the JIT run register code:
  a change to the register pipeline runs the VM's and the public package's
  tests so too.
- `-conformance.jit` keeps the normal thresholds, under which few test262
  tests reach native code. `QJS_JIT_STRESS=threshold,budget=3,deopt=7`
  compiles every function at its first call, polls every three back edges,
  and finishes every seventh poll's invocation in the interpreter; register
  code's samples follow its threshold (`jitRegStressSample`). `background=K`
  added compiles the stack form's code in the background, every JIT runtime,
  and installs each compile exactly K checks after it was handed over: the
  window between build and install, the same in every run. The differential
  tests run so among their settings, and the background tests under `-race`
  (`-run 'Background|Differential'`).
- `compilediff native` compares two builds' compiles, and `jitprof` (in
  `verify`'s module) joins a CPU profile with the code, both from `v8bench
  -jitcode`'s file (AGENTS.md, "Validation"). The file is written by
  `vm.JITCodeHook`, which the stack form's compiles call; register code's
  compiles do not call it yet.
- arm64's backend is checked on CI's Linux and macOS arm64 jobs and under
  qemu (`GOARCH=arm64 go test -exec qemu-aarch64`).

## The stack form's path (removed at the switch-over)

Until the switch-over, a runtime with the JIT runs the stack form by
default: the stack interpreter (`executeAt`) and the tree tier run a
function's stack bytecode, and the JIT compiles that bytecode through a
front end of its own. Everything in this section is that path's alone; it
goes when JIT runtimes run the register tier (docs/register-interpreter.md,
Stage 3's end, and Stage 4, which removes the stack interpreter and the tree
tier).

### Its front end

`compile.LowerSSA` lowers a function's stack bytecode to the slot IR, one
instruction per bytecode PC, with a state map `{PC, Depth}` at each: the
bytecode PC and the live operand depth, the operand stack's slots past the
locals (`StackSize`). `LowerSSAHot` lowers a function of calls and no loop
once its callers have shown it hot (`jitHotEntry`), `LowerSSAInline` a
callee for native callers or inlining, and `LowerSSAForward` a forwarding
function for inlining with its forwards. Its refusals are `LowerReg`'s by
the same reasons; `disasm -jit` prints a function's eligibility and IR
without native support:

```sh
go run ./internal/cmd/disasm -jit -func sum -e \
  'function sum(n) { let s=0; for(let i=0;i<n;i++) s+=i; return s }'
```

### Its feedback

The VM's feedback for the stack form is `jitFeedback` (`zzjit_ssa.go` and
the other `zzjit_*.go` files), whose methods take a stack pc and read the
`bytecode.Instr` there: the property site from the instruction's operand
into `closureCode.ic`, the global's cell, the literal pools
(`jitLiteralAt`), the call targets. What its code learns at its exits it
keeps in its function's `jitEntry`, by stack pc: the functions its calls
called (`nativeCalls`, `inlines`, `jitTargetsOf`), `Generic(pc)`, entries
that failed (`failedEntries`), typed and keyed reads' kinds (`typedReads`,
`keyedReads`). It answers every optional interface the builder has, call
frequencies (`Frequency`) included.

**Compiling in the background.** A compile is a job in three steps
(`jitJob`, `zzjit_background.go`), as V8's concurrent compiles are:

- the runtime's goroutine prepares it -- lowers, makes room, and asks the
  feedback ahead (`jitSnapshotOf`): every question the build may ask, of the
  function and of the callees it may inline, which reads and changes the
  runtime's caches, escape analysis's questions included
  (`ssa.VirtualFeedback`);
- a worker builds the SSA from that snapshot (`jitBuildFrom`), optimizes it
  and runs the backend, touching nothing of the runtime's, a panic its
  refusal (`backendPanics`), nor any variable of the process's: the
  encoding, `mir`'s switches (`mir.Options`) and the hooks are as they were
  when the job was prepared. A question the snapshot was not asked is a miss
  (`jitSnapMiss`): the compile is built on the runtime's goroutine at its
  install instead;
- the runtime's goroutine installs it -- `roomFor`, placing the code, and
  what a compile does once placed -- so placing stays where nothing else of
  the runtime's runs.

A compile on the spot finds its callees' code there, a callee's compile
being made within its caller's, so that it may inline them. In the
background it would find them being compiled, and call them natively, for
good: a call made natively leaves no exit to learn from. So a first compile
that meets a callee small enough to inline, being compiled, is held
(`jitHold`), holding no place and asking nothing, and is prepared once that
callee has its code, its calls decided then. Two functions calling each
other are not held for each other (`jitCompileWaits`), and a compile a frame
waits for at a loop is not held for a call the frame has left behind
(`jitFrameLeft`). The answer of a compile in the background is "not now",
never "no": a call whose target has no code yet waits for it
(`jitCalleeAwaited`), and a closure is not refused for a compile under way.

A runtime made `WithJITBackground` hands the work to the process's workers,
at most a quarter of `GOMAXPROCS`, each ending when none is queued, and up
to `jitJobsMax` (4) compiles of its own at once; otherwise it works each
compile where it is made. Meanwhile its entry is busy and pending: a first
compile's function runs in the interpreter and the tree tier, and code
compiled again runs on unless invalidated. Finished compiles are installed
at the next check -- a function's entry (`tryJITAt`), a loop's check
(`tryJITLoop`) -- and thrown away if the entry went or the code deoptimized
since the build. First compiles, compiles again, callees compiled for native
callers (`jitNativeCallee`, which call them through their links meanwhile)
and functions compiled once hot (`jitHotEntry`) go to the background, a
first compile's lowering too (`jitLowerLater`), from a copy of its hints,
thrown away if they change meanwhile (`jitLowering`'s serial); the compile
it was lowered for is decided (`due`) and made at the check that finds it
lowered. So are the callees a compile wants (`jitWantedCallees`), the caller
compiled again once it has spent a budget, by when their code is likely
installed. Top-level code, compiles asked inside another compile, and
compiles timed or repeated (`mir.Timing`, `JITCompileRepeat`) are compiled
where they are asked (`jitInBackground`).

`QJS_JIT_SNAPCHECK=1` checks every build of the VM's tests against its
snapshot (`jitSnapOracle`): it asks a copy of the feedback ahead, builds
from that as a worker would, and fails the run where the SSA, but for
addresses, or the feedback's lists after the replay differ, or the build
missed. `QJS_JIT_SNAPOPS=file` writes the methods each op's builds ask
(`TestJITAskedInventory`). The snapshot's answers and askers are generated
from `jitFeedback`'s methods in the VM's `zzjit_*.go` files
(`internal/vm/internal/snapgen`, `go generate ./internal/vm`), which
`TestSnapshotGenerated` checks.

### Its tier-up

**Hooks.** Three places in the stack form's tiers call the JIT, each behind
`r.jitOn()` or compiled out without the tag: `executeAt`'s interrupted block
calls `jitBackEdge`; `runFD` and the tree's call entries call `tryJITFrame`,
the call path's hook; and a loop's back-edge check promotes by on-stack
replacement (`tryJITLoop`), from the interpreter or the tree tier, at a
back-edge budget varied from one check to the next (`backEdgeBudget`).

**Budgets.** The budgets are V8's, named after its flags. A function is
given its entry, which holds its budget and its caches' mark as V8's
feedback vector does, after `invocation_count_for_feedback_allocation` (8)
framed calls (`jitInvocationsForFeedback`), and is compiled once the stack
form's tiers have spent `invocation_count_for_maglev` (400) invocations'
worth of it (`jitInvocationsForMaglev`; the one optimizing tier is the
closest to Maglev), each its bytecode's length (`jitTierBudget`): an
invocation spends its length, a full budget of back edges its count of them
times the loop's length (`jitTierWork`), as V8's JumpLoop spends its
distance. A loop still running when the budget is spent is entered by
on-stack replacement, as V8's `MaybeOptimizeFrame` does it. Invalidated code
waits for a fresh budget the same way (`jitTierUp`), and code that learned
at its exits too (`jitLearnedDue`). Every invocation the stack form's tiers
run spends it, those the call path answers without a frame (a pure body's, a
leaf's) included when Go makes them for native code (`jitInvokedFrameless`),
and those Go runs for a native call of a function it does not enter itself
(`jitEnterCallee`): a function only native callers call is compiled again by
their calls. A change of the function's caches' feedback (`jitFeedbackMark`)
raises what is left of the budget to `minimum_invocations_after_ic_update`
(500) invocations' worth (`jitInvocationsAfterICUpdate`), never lowering it,
as V8's `NotifyICChanged` does; the change is found when the budget is
spent. Past `jitMaxDeopts` (64) eager deoptimizations a function is compiled
no more. `QJS_JIT_STRESS`'s threshold compiles at once, as tests do
(`jitCallThreshold` 1).

**Invalidation.** A guard's failure invalidates the function's current code
(`jitInvalidate`): Go enters it no more, native callers no longer call it
(its link is cleared), and the stack form's tiers run the function. Code
that learned at its exits what it is to be compiled again for -- a call's
target at a call of Go, a getter, an element's kind, a callee that inlines
more -- is not invalidated: it runs on, and is compiled again in place once
it has spent a budget as invalidated code does (`jitLearnedDue`,
`jitRecompileInRun`) -- a recursive function's while its outer runs are
still in its code, as V8 installs new code while older frames are in the
old: each such run, once what it called returns, goes on from its frame, as
the code it left describes it, and in the new code at its next entry (only a
native call of the code, which returns into it, waits).

**Call frequency.** As V8's lower tiers count, in a function's feedback
vector, its invocations and each call site's calls, the stack form's tiers
count them where every call they make passes once the JIT is on: the call
path's hook (`tryJITFrame`), for a call made with a frame -- the callee's
invocation, and the call at its caller's site, the frame below, whose pc is
past the call as a stack trace reads it (`jitCallCounts`, `jitCountCallAt`)
-- and a pure body's evaluation, for one the call path answers without a
frame (`jitCountPure`). They count while the function's code is to be
compiled -- its entry warming, its code invalidated -- from its first entry
on, which its later entries and the cache's ghosts keep (`jitCounting`), and
only the calls of frames whose invocations they counted. Native code counts
nothing: its own calls through Go, at an exit and of callees with no code
are left out (`goCallDepth`, `nativeCallDepth`), and so are the calls of a
frame it left to the interpreter. A site's frequency is its calls over its
function's runs (`jitCallFrequency`), known where every callee its decisions
name is one whose calls they count -- not a leaf's other bodies
(`bytecode.LeafKind`), nor one called through `Function.prototype.call`, nor
a built-in -- and not for a call they never made that native code went on to
make, learned at its exits (`jitCountLearned`). Frequencies are known once
the function has run `jitCountsEnough` (19) times, in which a call made at
the minimum is made at least once 19 times in 20. A call made less often
than the minimum is decided a native call (`jitCallCold`); a callee whose
calls are decided after its native callers were compiled has them compiled
again, to inline it with its calls where they now may (`jitCallersReopt`).

**The tree tier and profitability.** Each native stretch's work is
estimated, exits and calls of Go counted against it; code whose stretches do
too little between them is left to the tree tier (`jitSSAProfit`): Go's
entering native code and its leaving cost what V8's calls between its tiers
do not, and a call of Go costs more than the tree tier's own call of the
built-in. A run on-stack replacement began judges its calls of Go as it
makes them (`jitRunGoProfit`), and leaves after the call that finds them not
to pay; its stretches are judged apart from those of entries at the start.
Code judged so (`entrySlow`, `loopSlow`) is entered neither at its start nor
at a loop, until Go retries it (`jitRetrySlow`). The frame it left goes back
to the tree that entered it at a loop, once the interpreter has run it back
to that loop's header, the tree going on with the loop as though it had not
been entered (`errBackToTree`); any other frame goes on in its tree from the
next loop header (`runTreeFrom`). Where Go goes on at a PC with no entry,
the interpreter runs only to the next loop header, and native code from
there (`jitGoOn`). Code the JIT never runs keeps the tree tier's fast nested
calls: only a frame whose loop may still promote keeps `runTree`'s recover
(`jitTreeRecovery`); a nested tree whose function is pending keeps it too,
since the code may arrive at the loop's check.

### Its cache's own parts

**Closure hints.** A closure remembers its stack-form code by a hint, a slot
in the cache's hint table (not a pointer), which holds the entry and its
function's address. The hint finds the entry only for a closure of that
function, so a stale hint misses however often its slot was reused. The hint
lives in what a template's closures share (`closureCode`), so the closure
holds nothing of the JIT's; the table grows to the most entries the cache
has held at once, its free slots reused.

**Refusals.**

- Permanent: lowering or the backend refused it. These are cached, and the
  closure is marked refused.
- Deferred: the budget had no room for the code. These are cached until the
  cache releases code.
- Temporary (no budget at all): retried after another warm-up.

What a compile costs, the VM weighs before it asks for one
(`jitCompileBytes`: a word a slot an instruction, its slots counted as at
least 256). A compile asks for room for what its entry is likely to hold
(bytes per bytecode instruction, from the last compiles), then, compiled,
for exactly what its entry will hold. Top-level code's refusals are not kept
in the cache, which a loop evaluating thousands of scripts would fill.

### What only it compiles

**Calls.** A call that has met `jitCallTargets` functions calls any other
through its link, as V8 makes a megamorphic call through the code the
function points to (`ssa.CallSite`'s `Generic`): a closure of the caller's
environment and realm whose hint names its function's link
(`abi.HintTable`), whose frame is its arguments in its parameters (no rest
or `arguments`), and not a generator's or an async function's. A callee
reached so, or by any native call, that has no code yet has its frame made
and leaves for Go to run it (`abi.ExitEnter`), which compiles it for native
callers (`jitNativeCallee`). A call inside an inlined callee is inlined
however seldom; a callee's call in tail position is made as an ordinary call
inlined, where the stack is shallow: its depth check counts the frames it is
inlined in, and deeper it leaves for Go, which gives the frame up as the
interpreter does (`ssa.OpTailRoom`'s Index). Inlined callees' exits that are
taken count toward the call's not being inlined any more
(`ssa.InlineExitFeedback`, `jitInlineLeft`), and a closure made anew at each
call of its maker fails an inlined call's check and is called by its code
from then on (`jitInlinedCalls`).

**Closures from pools.** A native callee takes its cells from its function's
pool at its entry (`ssa.GoCells`, mir's `cellsTake`); the frame Go entered
the code with has them open on its slots. A closure is taken from its
template's pool (`closureTake`), and a scope's end gives its locals fresh
cells from the function's pool (`cellsClose`). Go makes them where a pool
has run out, refilling it, or a function has more than four cells. A cell
native code made on the frame Go entered is open on its slot again once
native code leaves the frame (`jitReopenRunCells`), as the cells of a native
callee's frame Go makes are (`jitReopenCells`). An arrow a derived
constructor makes shares its this binding: on a native callee's frame Go
makes one once a super() has bound this, and leaves native code for one made
before (`jitCalleeMakes`); a super() is made natively in a constructor that
makes no arrow before one (`jitArrowBeforeSuper`), and Go binds the this of
a frame it entered the code with before it makes an arrow there
(`jitBindThis`). A native call of an arrow that shares such a binding gives
it the binding's value as its this once bound, which each call checks (mir's
`arrowBound`).

**Generators and async functions.** `yield` and `await` are exits at which
Go suspends the frame as the interpreter does, and each resumption enters
native code at the frame's pc -- the entry after the yield or await, or, the
first time, past the parameters -- as the tree tier resumes a tree at the
block there (`jitResume`, `runGeneratorTree`). One of calls and no loop is
compiled once its resumptions have made it hot (`LowerSSAHot`,
`jitHotEntry`), each resumption spending its length of the budget. A call of
one makes its object, so none is called natively or inlined; a step over one
resumes it natively (see [Control flow](#control-flow), `jitGenFrame`,
`jitGenDone`, `jitGenCallable`). `yield*`, an `await using` scope's disposal
steps, `import()` and `import.meta` leave for Go and go on past it.

**Top-level code.** A script's, a module's or an indirect eval's code runs
once, so it is entered only at its loops, as V8 enters top-level code by
on-stack replacement: never at its start. Its declarations, which come
before its loops, are left to the interpreter, but for a `let`, `const` or
`class` given its first value between two loops, which Go gives it and
native code goes on. A module's imports are found where the binding they
name is, from the modules already loaded, and a name the module does not
declare in the global object. A direct eval's code, which reads its caller's
scopes, stays in the interpreter.

**Tail calls.** A call in tail position gives up its frame for the callee's
only deep in the stack, as the interpreter's does (`tailCallDepth`): a
shallow one keeps its frame, as a stack trace shows, and native code makes
it as any call (`ssa.OpTailRoom`, `OpTailSelf`, by `abi.Bounds`'
`TailLevel`). Deeper, a call of the closure the frame runs writes its
arguments to the frame's parameters and enters the code at its start again,
a loop in the frame, as a compiler turns tail recursion into one; in a
native callee's frame, a call of another function the call has puts that
function's frame in this one's place, its context this one's, and goes on in
its code, returning where this frame would have. A frame Go entered the code
with is given up by Go (`errTailCall`), as the interpreter gives it up.
Frames the interpreter keeps for such a call -- a construction's, an arrow's
sharing a derived constructor's this -- native code keeps too (`TailFrame`).

**Parameter lists.** A list with defaults or patterns binds its parameters
one at a time from the call's arguments, in its prologue (`ir`'s
`ParamsDead`), which a frame of the VM's has (`f.args`) and one native code
makes -- a native call's, an inlined callee's -- does not. So, as V8's frame
states keep an arguments adaptor frame's arguments, every state inside the
prologue keeps them past its operands (`ir.Program`'s `PrologueArgs`,
`ssa.FrameState`'s `Args`), and a frame Go makes at an exit there is given
them (`jitPrologueArgs`), the interpreter going on binding the parameters
from them; a frame Go runs from its start (`abi.ExitEnter`) is given its
parameters' slots as written (`jitEnterArgs`). A prologue whose states have
no room for the arguments within the frame's operand slots is Go's to enter
only (`PrologueFrameOnly`). A rest parameter's array, and the arguments
object's operations -- an unmapped object's, or a mapped one's where the
function never lets the object out (`compile`'s `argumentsOwn`, from
`bytecode.Function`'s `MakesArguments`) -- are Go's, from the frame of the
VM's the code was entered with, but `arguments[k]` and `arguments.length`,
which Go reads from native code while the object is not made
(`ssa.GoArgument`). Where the function, or a closure, assigns a parameter a
mapped object aliases (`AssignsParams`, a captured parameter), each read of
`arguments[k]` passes Go the parameters as the code has them (`ir.Program`'s
`ArgumentParams`), and Go answers a key that is one's alias with it, as V8's
sloppy arguments elements read the parameter where it lives. A native call
of such code, or of a constructor's `super(...)` of its call's arguments,
writes all of its arguments and their count to its callee's context
(`ssa.CallSite`'s `FrameArgs`, `abi.Context`'s `Args`), as V8's call pushes
them all: Go makes the array and an unmapped object of them without a frame
(`jitGoHostPlain`), reads `arguments[k]` there, and gives every frame it
makes of the callee's level a copy (`jitCallArgs`). A call in tail position
that gives its frame up does not take a callee whose arguments its context
would hold alone, and one of the function itself is Go's (`ir.Program`'s
`FrameArgs`), never a jump to the code's start.

**Forwarding calls.** As V8's TurboFan makes JSCallForwardVarargs,
`f.apply(x, arguments)` in a function whose arguments object is made for
nothing else (`bytecode.Function`'s `ForwardsArguments`) is a call native
code makes of the arguments its frame was given (`ir.Program`'s `Forwards`):
a native callee's are in its context, where its call wrote them
(`FrameArgs`), and Go copies those of a frame it enters the code with there
at each entry (`jitForwardEntry`), their count past the context's room
(`abi.MaxGoArgs`, 16) leaving the calls to Go, which reads the frame's. The
call checks its apply is the realm's own and calls f natively as any call
does, the functions it has been seen to call (`ssa.CallSite`'s `Forward`,
`jitForwardSeen`), up to `jitCallTargets` of them, the arguments copied to
the callee's parameters, undefined past them, and to its context where it
reads them past its parameters, with plain stores, as a call writes its
arguments, the collector marking or not. Anything else Go calls from native
code (`abi.GoCallForward`) through apply's frame; it makes
`Array.apply(null, arguments)` an array of the arguments without a frame
(`jitForwardPlain`). A call of the frame's arguments is never inlined there,
their count unknown; where a caller knows them, a call of a forwarding
function is inlined with its forwards (`LowerSSAForward`, `ssa.InlineSite`'s
`Forward`, `jitForwardsCallInline`).

**The frame's own.** What reads only its operands and the frame Go entered a
function's own code with -- a super reference's read or write, a rest
parameter's array, the arguments object -- Go makes from native code on that
frame (`ir.Instruction`'s `Frame`, `ssa.GoHost`, `jitGoHost`), native code
going on after it. Code that reads its function object -- a super
reference's home object, `push_callee`'s, a mapped arguments object's callee
-- or makes an arrow that does (`bytecode.Function`'s `FrameCallee`, its
arrows' `ReadsMaker`) has a call entry all the same: a native call of it
writes the object to its callee's context (`ssa.CallSite`'s `Function`,
`abi.Context`'s `Function`), and Go gives it to the frames it makes of the
callee's level (`jitLevelHost`). A class's or an object literal's member's
definition that reads no local is Go's from native code in a native callee's
code too, on the frame Go makes of its level (`jitGoCallNested`), or with no
frame where it runs no script and throws nothing (`jitGoHostPlain`).

**On-stack replacement's own code.** An entry escape analysis dropped at a
loop's header Go has never needed has Go go on in the interpreter there,
until two checks of one frame in a row find no entry there, when code is
compiled with it for that frame (`jitLearnLoopEntry`), kept apart from the
function's, as V8 keeps code for OSR: a `jitEntry` of its own, its
function's `osr`, sharing what the function learned (`jitLearned`), with a
link no native code calls through, entered only at a loop's back edge
(`zzjit_osr.go`). Called again a few times (`jitKeptStarts`), or linked by a
native caller, the function is compiled again without the kept entry. The
OSR code goes when the function's code is compiled again or dropped.

### Its side of the shared mechanisms

The mechanisms of [What the builder makes](#what-the-builder-makes) have
their VM side for the stack form's frames in functions of their own, which
go with it:

- learning at exits, by stack pc: a property site's shapes, megamorphic
  marks and additions (`jitPropertyDeopt`, `jitDeoptSeen`, `jitShapeMiss`,
  `jitPolySeen`, `jitPropertyMade`), private names' shapes
  (`jitPrivateSeen`), the shapes a cache held before (`jitSeenFeedback`),
  the functions a call called (`jitCallSeen`), BigInt typed arrays' elements
  (`jitBigTyped`);
- making what native code left unmade on a frame of the VM's: objects
  (`jitMaterialize`, `jitMaterializeObjects`), the closures inlined frames
  name (`jitMaterializeIn`), a closure the code only called
  (`jitCalledClosure`);
- handlers: a throw from Go unwound to native code's handler (`jitCatch`),
  and a native callee's handlers on the frame Go makes (`jitHandlers`);
- what Go makes at a host exit and native code goes on after (`jitHost`): a
  class's or an object literal's members, a key that is an object, a proxy
  on a chain; a member's definition that reads no local (`compile`'s
  `frameDefinition`); a for...in's cursor made without the levels' frames
  where its making runs no script (`jitForInPlain`); `G.call(recv, ...args)`
  of a leaf as G's call (`jitPlainCall`);
- the calls of Go that need the stack form's frame (`jitGoRun`'s), which a
  register run refuses, leaving them to an exit.

### What remains to remove

The switch-over needs, on register code: the feedback the tables in
[Feedback](#feedback) list as not answered yet, call frequencies first (from
the feedback dispatcher), and a loop's steps of a generator resumed natively
(`GenSteps`).
Then it removes:

- `compile.LowerSSA`, `LowerSSAHot`, `LowerSSAInline`, `LowerSSAForward` and
  `lower.go`, and the IR's operand stack (`Depth`, `StackSize`, the
  prologue's arguments);
- `jitFeedback`, its entries' learning by stack pc, its snapshot and
  generator (`zzjit_snapshot*.go`, `snapgen`, `TestSnapshotGenerated`,
  `QJS_JIT_SNAPCHECK`), or their port to register code's feedback;
- the stack form's hooks -- `executeAt`'s `jitBackEdge`, `tryJITFrame` in
  `runFD` and the tree's call entries, `tryJITLoop`, `tryJITAt` -- and its
  tier-up budgets, call counting and profit model, `jitTreeRecovery`,
  `errBackToTree` and `runTreeFrom`;
- the hint table and generic calls through it, the paths for frames of the
  VM's (`jitApplyRecords`, `jitMaterialize`, `jitReopenRunCells` and their
  kind), and `disasm -jit`;
- register code's half of the budget, `QJS_JITREG` and `vm.SetJITRegTier`,
  and `TestJITFeedbackParity`, whose other side is gone.
