# Native JIT tier

This package is the optional native tier's machine-code side; the VM's side
is `internal/vm/jit.go` and `jit_*.go`. The tier compiles a hot function's register
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
interpreter (see [Zero cost when off](#zero-cost-when-off)).

The front end compiles each function to register code alone
(docs/register-interpreter.md, "The compiler"), which is what the JIT
compiles: its lowering, its feedback and the VM's helpers native code calls
read register code and its tables, positions included (`RegCode.Lines`).

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
- [Typed code](#typed-code)
- [Zero cost when off](#zero-cost-when-off)
- [Validation](#validation)

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
| Count | `jit_tier.go` | A function's returns count its runs, and its backward jumps their distance, against its budget (`jitCallThreshold` invocations of its length); the check that spends it compiles the function. |
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
| `compile` | Lowers register code into the IR (`LowerReg`; `LowerRegHot`, `LowerRegInline`), and typed code (`LowerTyped`), or refuses it (`Refusal`). It never imports the VM. |
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
| `internal/vm/jit_tier.go` | Tier-up from the register interpreter: sampling, entry at calls and by on-stack replacement, invalidation, what exits teach. |
| `internal/vm/jit_native.go` | The compile (`jitRegFor`), the register code's cache, running code on a register frame and its exits (`jitRegRun`), the calls of Go made on it. |
| `internal/vm/jit_feedback.go` | `jitRegFeedback`, the builder's feedback by register pc. |
| `internal/vm/jit_call.go` | Native calls between register functions: links, call sites, levels, unwinding. |
| `internal/vm/jit_inline.go` | Finishing inlined register callees at an exit. |
| `internal/vm/jit_frame.go` | What Go makes on a register frame for native code: cells, closures, rest parameters, arguments. |
| `internal/vm/jit_runner.go` | The IR's evaluator over a register frame, the VM its runner (`RegLowerCall`), and running a register instruction whole (`regWhole`). |
| `internal/vm/jit_args.go` | Callees that keep their arguments, inlined with their call's arguments, forwarders' `apply` among them. |
| `internal/vm/jit_bg.go`, `jit_snap.go` | Compiling register code in the background, and its feedback asked ahead (`jitRegSnapshot`). |
| `internal/vm/jit_bigint.go` | BigInts' operators learned at exits and made natively or by calls of Go. |
| `internal/vm/jit_elem.go` | Elements' kinds learned at exits: typed arrays, strings, reads by name. |
| `internal/vm/jit_freq.go` | Call frequencies from the feedback dispatcher's counts (`Frequency`). |
| `internal/vm/jit_gen.go` | A loop's step over a register generator, and calls of its `next`, natively. |
| `internal/vm/jit_loop.go` | Built-ins' callback methods made loops of the code's own (see [Array loops](#array-loops)). |
| `internal/vm/jit_ops.go` | Other operations native code makes from what the feedback answers of them. |
| `internal/vm/jit_typed.go` | Typed code's twins, compiled at their module's link, and their feedback: the heap, Math (see [Typed code](#typed-code)). |
| `internal/vm/jit.go`, `jit_ssa.go` | What the register pipeline's files build on -- the JIT's state, the code cache, budgets, links, contexts, calls of Go, the pools, and the feedback's sites and seen cases (`jitFeedback`'s state, which `jitRegFeedback` reads). |
| `internal/vm/jit_leaffast*`, `jit_bigleaf*` | The VM's routines native code jumps to for leaves and BigInts' operators (`LeafFast`, `BigFast`). |
| `internal/vm/jit_background.go`, `jit_snapshot*.go` | Compiling in the background, and the snapshot of the feedback that `jitRegSnapshot` answers from; `jit_snapshot_gen.go` is generated from `jitRegFeedback`'s methods. |
| `internal/vm/native_feedback*` | The native handlers' feedback dispatcher, which a runtime that runs the JIT is given (generated by `internal/vm/internal/nativegen`). |
| `internal/vm/jit_disabled.go` | The stubs for builds without the tier. |
| `internal/regexp/native_*.go` | Regular expressions' machine code: the generator (`native_gen.go`), its macro assembler for both machines (`native_masm.go`), character sets (`native_sets.go`), and entering and placing it (`native_jit.go`); `native_off.go` without the tag. See [Regular expressions](#regular-expressions). |
| `internal/vm/jit_regexpnative.go` | The VM's host for them: the arena and the budget. |

**Where the VM's files sit.** Package vm's functions are laid out in
file-name order, then all its closures (AGENTS.md), so the JIT's files,
`jit.go` and `jit_*.go`, move every function after them: the native
handlers' Go side (`native_*.go`), the register interpreter's
(`reg_*.go`) and the call path's (`vm_*.go`). The names do not keep that
code in place; the layout lock does: its pad J sits before
`jit_leaffast.go` and anchors the JIT's leaf calls (`jitLeafGo`) in the
configurations with the JIT, pads Q and W hold the register
interpreter's loop (`runReg`) and its calls (`rhCallW`), and B holds
`callObject` (`layoutlock.txt`). Every function comes before every
closure, and the package's assembly -- the native handlers' block among
it -- after the closures, so a function added to any of them moves those
too, which only H's pad holds for the handlers' block. A change to a JIT
file is therefore followed by `go generate ./internal/vm`, as any change
to package vm, and `TestLayoutLock` fails until then.

## Register code and its lowering

A register function's frame is a window of registers in the VM's stack:
register 0 holds the function, 1 its this, and the parameters follow from 2.
Its instructions name registers as their operands
(docs/register-interpreter.md, "Instructions"), and a call's operands are a
run of registers, a window of the callee's, which the interpreter's call
makes the callee's frame. `compile.LowerReg` lowers a function's register
code into the slot IR; `LowerRegHot` lowers one its callers have shown to be
hot, and `LowerRegInline` one for native callers or for inlining; each
has a form with hints (`LowerRegWith`, `Hints` by register pc), what the
function's code learned at its exits (see [Feedback](#feedback)). Code
compiled at a function's first call (`bytecode.RegLater`) keeps the names,
constants and property sites it adds in its `RegCode`, which the lowering
reads in place of the function's (`regView`). The three kinds differ only in
what they refuse -- a callee's refusals are an entry's and more, and an
entry's program of host operations alone is refused where a callee's is not
-- so `LowerRegKindsWith` lowers a function once for all three, each kind's
refusal the one its own lowering gives, at the same check, and the program
the kinds that take it share the one each would have been lowered to alone
(`LowerRegForwardWith`'s, which a forwarding callee is lowered to, differs,
and is lowered apart). The VM lowers a function's program once for the
kinds that share it and keeps it (`jitRegLower`, `jitRegLowerKinds`,
`regLowered`, a `jitLowering` by the function's key, which counts a
program the kinds share once), checked
once as it is lowered (`ir.Program`'s `Validate`, which marks it `Checked`:
the builder checks such a program no more, neither at each build nor at
each ask to inline it, `InlineCalls`): its own compiles and every
caller that inlines it take the same program, never changed once lowered,
until its code learns a hint, whose lengths the lowering compares at each
ask, and it is lowered anew (`regLowering`, `relower`).

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
exit writes only the registers the interpreter may read. Each instruction's
set is a persistent bitset (`ir.Liveness`), a tree of 64-slot leaves under
nodes of 16 that the instructions' sets share: an instruction's set is the
next one's with what it writes taken out and what it reads put in, which
copies only the nodes on their paths. The dataflow runs over blocks, each
block's set at its start found again from its successors' with a worklist
until none changes, then each instruction's from its block's end
(`liveRegs`). A program keeps the dense rows instead where they are the
smaller, as they are for most functions of a few hundred registers or
fewer; a dense row at every instruction would make the liveness of n
registers and m instructions n·m bits. The builder asks a set's slots
(`Liveness.Slots`) and the slots two sets tell apart where their trees
differ (`Liveness.Changed`). Under `WithNodeQuirks`, a sloppy function's
parameters stay live throughout too (`Hints.KeepParams`): legacy
`Function.arguments` can read them at any call, even when its body never
reads the parameter. Native and inlined frames therefore reconstruct the
current parameter values before reflection runs.

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

`get_prop_next` and its following `get_prop` lower as two independent
property reads, each with its own origin, feedback site and frame state.
Their combined dispatch and omitted intermediate store belong to the
native interpreter; an exit from compiled code can resume at either read.

A property's value is read as a number where what uses it asks for one --
arithmetic, a comparison, an element's index -- which the lowering finds
by a scan back over the code (`selectNumericProperties`), and as a
reference, tagged, elsewhere. A store, an element's as a property's, takes
its value as it is: a property read only stored stays tagged, where read
for a number it would leave native code at every other value it met. An
element read only stored keeps its numeric read, which reads the element
by its cell once its guard has failed (ssa's `referenceReads`).

Where a register instruction's operands are a run of registers that is not
where its result goes -- `new_array`'s elements, `concat`'s parts -- they
are copied to scratch slots first, which the operation reads as a run of
operands. A part's conversion where it is -- `to_string`'s, `str_part`'s -- that
only a concat of more parts reads, itself or a copy of it in the concat's
run, and that is not live past the concat, is a `StringPart`
(`regStringParts`): a check that it is a
primitive, which the concat converts the same, not a call of Go of its own.
What the lowering leaves to Go whole -- a call, a cold operation
-- is one `Host`, alone in its origin: Go runs the whole register
instruction. Where Go can run it on the frame and the registers it reads
and writes are known (`compile.RegAccess`: up to four read, the fourth at
`ir.Instruction`'s `Fourth`, one written; a plain call's receiver register
written by the call, not read)
-- a for...in's or a for...of's cursor, an object's or an array's spread, a
spread call, a computed key's definition, `#x in o`, super's reads and
writes by name and by a computed key and super's base (their receiver
register 1), a derived
constructor's result -- it is a whole Host (`ir.Instruction`'s `Whole`), its
operands the registers it reads and its result the one it writes, which
native code calls Go for and goes on after (see [Calls of
Go](#calls-of-go)). So is what the builder leaves to Go of an operation
whose operands are its register instruction's own, the only instruction
of its origin (`ir.Instruction`'s `WholeOperands`): a literal's field
whose definition the feedback does not plan, a destructuring's step or
rest, a private member's read or write its site's cache holds no own field
of, a `get_length` whose guard has failed -- an arguments object's, a
getter's -- read by a call of Go on its frame (`ssa.GoHost`). A private
member's read or write (`get_private`, `set_private`) is a property's read
or write of the key its register holds (`ir.Instruction`'s `Private`), made
natively for the shapes its site's cache holds of the key. A call,
`call_w`'s, `call_method_w`'s or `new_w`'s, and a `typeof` are Hosts of
their own kind (`ir.Instruction`'s `Window`), which the builder makes
natively from what the feedback says (see [Calls of Go](#calls-of-go) and
[Native calls](#native-calls)). So is a call in tail position
(`tail_call_w`, `tail_call_method_w`, `ir.Instruction`'s `Tail`): made as
the ordinary call while the stack is shallow, natively or by Go, as the VM's
`rhCallWT` makes it, native code going on after it. Its built-in target is
read from the same call-site cache as an ordinary call's, so intrinsics
and leaves use the same specialization and target guards in tail position.
Deeper, where the interpreter gives the frame to the callee in place
(`regTailTo`), a call of
the closure the frame runs is a jump to the code's start (`ssa.OpTailSelf`,
`ssa.TailWindow`): the window's function, receiver -- a method call's, an
arrow's the this it captured, its register 1, undefined for a plain call --
and arguments written to the frame's registers from 0, undefined in the
rest, the receiver to the VM's frame too where Go entered the code with it
(`abi.Context`'s `FrameThis`), which `load_this` reads; the jump goes to the
start's poll (`ssa.TailPolled`), so that a recursion through such calls is a
loop in the frame, as many calls deep as it likes. That is strict code of a
plain function, a method, an arrow, a getter or a setter (`ir.Program`'s
`TailOwn`) -- not a sloppy function's, whose frame the interpreter gives up
for one of the callee's own, its `f.arguments` reading its count from its
caller's instruction -- that keeps no arguments and whose registers no
closure captures, in a frame the interpreter would give up: Go's entry says
whether, by the frame's kind (`abi.Context`'s `TailFrame`), and a native
call of such a function sets it at every call (`ssa.CallSite`'s `Tail`), not
for a construction, so that no frame reads a flag an earlier one left in the
context. In a native callee's frame, a call there of another of the
functions the call has (its native call sites, `OpTailSelf`'s `Calls`) is a
jump to that function's code with its window in this one's place (`mir`'s
`tailJump`): its registers the call's function, receiver and arguments up to
its parameters, undefined past them, as a native call writes its callee's
window -- its temporaries, which it writes before it reads them, left as
they are -- the context this one -- its closure, its captured bindings,
`TailFrame` for its own calls -- its room the callee's window's, a constant,
as a native call's (below); the jump goes to the callee's tail entry
(`ssa.Func`'s `TailEntry`, `mir`'s `windowTailJump` and `tailEntry`, its
link's body), a routine that makes what its call entry makes past the
window, of its own code's constants -- its operands' address, the VM's
stack's top and high water -- and its bindings past its parameters
undefined, as its call entry does, then goes on at its start, returning
where this frame would have: two functions calling each other in tail
position run as deep as they like in one frame, natively. A call of the
function itself writes its bindings and leaves its temporaries so too. Not a
callee that keeps its arguments, an arrow reading its this, a sloppy one
given the global object, a generic call's, nor a call in a frame Go entered
(`abi.Context`'s `ReturnTo`), which the interpreter gives up. Any other call
there leaves for the interpreter (`ssa.OpTailRoom`'s, or `OpTailSelf`'s
exit), so that native code keeps no frame a tail call gives up, and the
frame ends after it. A call of Go native code makes (`jitRegGoCallOn`) whose
callee's frame the interpreter gave to the callee's callee in place runs
that frame again there (`regRerun`), as the interpreter's own call does
(`regMissEnd`), never leaving it to a frame further out. A `wrap` -- `x op
y`, the second argument of the `asUintN` or `asIntN` call right after it,
which the interpreter makes with the call at once -- is the operator alone,
the call made after it as any call, which native code makes of the
operator's low bits (see [BigInts](#bigints)). A return, a suspension and a
throw the function does not catch are plain Hosts.

`regexp_lit` that calls -- `/x/.exec(s)`, `/x/.test(s)` -- is the literal's
RegExp from its pool, its method read, both in scratch slots, the argument
copied past them, and the call of the method in that window of scratch
slots, a `Window` Host ending the origin (`compile`'s `regexpLit`); the
window's registers, R[A] and R[A+1], are not written, and where they are
live past the instruction it is Go's whole. One that does not call is the
literal and the read, copied to R[A+1] and R[A] last, for the
`call_method_w` after it, a leaf's call of the realm's `exec` or `test`.
A `call_method_w` that makes its first argument a RegExp literal
(`RMarkLitArg`: `s.replace(/x/g, "")`, `split`, `match`) is lowered the
same way (`compile`'s `litArgCall`): the literal's RegExp from its pool
into a scratch slot, as `new_regexp`'s, the rest of the window copied
around it, and the call in that
window -- a replace's leaf where its template is a string constant, the
call of Go otherwise -- each giving the RegExp back to the pool where the
built-in kept nothing of it (`jitRegLitArgMade`, `jitRegExpLitDone`).

**Loops, one header each.** Register code's loops test at their end: a
jump to the test, the body, the test and its branch back to the body, one
branch a turn for the interpreter, whose back edge -- where on-stack
replacement enters -- is that branch's, to the body's first instruction.
Lowered as it is, such a loop has two ways in, the jump's into the test and
the entry's into the body, and no header every turn passes: what it carries
was merged at the test too, in phis of no loop's header, which no loop pass
treats as the loop's. So the lowering puts a copy of the test in place of
the jump (`compile`'s `loopTest`, `lowerLoopTest`), its branch the other way,
to the loop's end, as compilers rotate a loop: the body's first instruction
is the one header, its ways in the entry's and the copy's. The copy's
instructions keep the test's origins, each origin's together after a `Nop`
of the jump's, and `ir.Program.Starts` maps a register pc to its own
lowering, not the copy (`At`). A test that suspends, steps an iterator or is
in another handler than the jump is not copied.

**Comparisons' booleans.** Register code makes a comparison's value a
compare-and-branch to two loads of the booleans, which the interpreter
runs as one branch; lowered so, the value was a phi of two constants that a
test of it -- an inlined callback's result -- branched on again. Where
nothing else jumps into the loads, the lowering makes the compare-and-branch
the comparison's `Binary` to the register -- or, where the branch's way
says to negate it, to a scratch slot and its `Not` to the register, the
origin's one write last, as the atomicity rule below has it -- and the
loads and the jump past them `Nop`s that native code falls through
(`boolBranch`): a branch on
the value is a branch on the comparison, and the comparison, the moves of
its register and what reads it are one block, where an arithmetic
operator takes the boolean as its number natively (`Instruction`'s
`Bools`, `markBools`) -- a jump past the loads
began a block at their end, and Emscripten's `(a < b) | (c < d)`
deoptimized at the `|`.

**What a call writes.** A register call, and an instruction Go runs whole,
writes its result to a register, not to the operand stack: the choice of
the element reads made as numbers (`selectNumericProperties`), backward
from their uses, takes that register as written there, so that the
result's uses are not the uses of what the register held before -- a
function table's element, called and its result ORed with 0 in the same
register, was read as a number and deoptimized at.

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
eval, an arguments object the frame cannot own (`compile`'s
`argumentsOwn`), an instruction it has no lowering for, a function of host
operations alone at its entry (`HostOnly`) -- a call in tail position
counts as a loop, since native code may make it
a jump to the start -- and a program past the slot budget: registers,
captured bindings and scratch slots together within what a slot's number
names (`abi.MaxSlot`). A function of host operations alone is lowered all
the same for native callers (`LowerRegInline`) or where a call it makes
would be native (`LowerRegHot`, `jitRegCallsNatively`): compiled for that
alone, it is worth its code. A call is native where its callee is the
function itself, has no code and is not warming (its native callers' first
call compiles it), or has code native callers call -- not code they call no
more (`notNative`), whose calls native code Go entered would make through
Go. A register function has no parameter prologue -- its defaults are
ordinary code over its registers -- so none is refused for one. A panic in
the lowering is a refusal (`lowerRegRecovered`). `v8bench -jitreport`
prints, after each benchmark, what the JIT refused or never tried, with why.

**The evaluator on a register frame.** `vm.RegLowerCall`
(`jit_runner.go`) runs a call's register frame by the IR's evaluator
over its program, `EvaluateHost` given a `Runner` backed by the VM
(`regRunner`): what the evaluator would leave at -- a host operation, a
guard that failed, a Generic instruction's slow path -- the VM runs on the
frame's real registers, the whole register instruction, by the interpreter's
own handlers (`regWhole`: its handler from `regTable`, and its slow function
from `regSlowTable` where the handler leaves it that), exceptions to the
frame's handlers by `regCatch`; where the evaluator leaves for good -- a
return it cannot make, the budget -- `runReg` goes on with the frame from
that register pc. Inside an expansion it runs the instruction whole only
where nothing of it has committed; and where the program's lowering of the
register pc it goes on at is a lone `Nop` of an instruction another's
lowering did the work of -- `boolBranch`'s loads and jump, which a
compare-and-branch run whole goes on to without its value written -- the VM
runs those too, as the interpreter does, to where the program does its
instruction's own work. That is what native code's exits do too
(see [Exits and deoptimization](#exits-and-deoptimization)), so the IR's
evaluator with the VM as its runner (`RegLowerCall`) checks the lowering
and the exits' contract without a backend
(`TestJITRegBoolWholeRun`).

## Feedback

The builder asks its feedback (`ssa.Feedback` and the optional interfaces
beside it) at each site, by the program's PC; the VM's answer for register
code is `jitRegFeedback` (`jit_feedback.go`), which reads each
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

**Call caches across turns.** A call site's cache holds its callee
strongly, and so a turn's end empties the call caches of the templates the
turn filled (`releaseRegCalls`; the paths that fill one put its template
on the turn's list, `regCallsHold`), keeping each site's callee as a weak
pointer on its template (`regSeen`), as V8 holds call feedback weakly
(docs/register-interpreter.md, "Inline caches and feedback"). The JIT
reads a template's call sites through `regCallsRestored`, which fills an
empty entry again, once, from a callee still alive, and puts the template
back on the turn's list: a function warmed in one turn and compiled in the
next is compiled for the targets it called.

**What native code learns at its exits.** What a function's code learned is
kept by register pc across its compiles, in an entry of its own
(`jitState`'s `regLearned`, a `jitEntry`, by the function's key): what its
code learned (`jitLearned`), beside its budget, deoptimizations and runs
(see [Tier-up](#tier-up-and-on-stack-replacement)):

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
  the functions of the operation (`jitPropOp`: `jitDeoptSeenOp`,
  `jitPolySeenOp`, `jitPolyWriteSeenOp`): the site's cache learns the shape
  as the interpreter makes the operation, and what the cache cannot hold the
  entry notes;
- the functions a call was seen to call at its calls of Go and at its exits
  (`jitRegCallSeen`), and the constructions: asked at a call of Go while
  its site is unsettled (`jitRegCallUnsettled`) -- after `jitCallsToInline`
  calls of built-ins (`jitRegCallSettle`) a site is settled as one Go makes
  (`jitCallDone`), and its calls ask nothing more; a construction's, or a
  call of a function with code, keeps it unsettled. A construction nothing
  taught yet, of the function a global's or a captured binding's read before
  it holds now, or a data property of an object such reads give -- `new
  NS.A.B(...)`, each read of the register the one before it wrote, followed
  (`regConstructorNow`, `regDefined`) -- is seeded at a compile
  (`jitRegSeedConstruct`), in the code's own frame and in a callee inlined,
  whose own code may never be compiled; the construction checks its
  function. Its site is compiled with its pool whatever the pool holds, the
  code having Go fill it again when it runs out (`ssa.GoRefill`); a
  construction Go makes refills it, run out or made for another prototype
  (`jitRefillPoolsOf`). A forwarding constructor's construction inlined
  (`this.k.apply(this, arguments)`, `bytecode.LeafForward`) reads `this.k`
  and k's `apply` as the construction's object and k find them
  (`forwardProperty`), not as the forwarder's own caches, which met every
  class it constructs, do;
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
  `jitRegBigIntExit`, `jitBigIntNote`), which the next
  compiles make natively or call Go for (see [BigInts](#bigints)); and,
  for the first compile, those the register interpreter made in Go -- its
  slow paths', which make every operator on BigInts the native handlers
  do not: division, remainder, powers, shifts, bitwise operators, those
  past the int64 range, a wrap's operator (`wrap`), whose passed-over
  call's site cache it fills -- marked by register pc (`regBigOf`,
  regCode's `bigSeen`, `regBigSeen`): BigInts in the int64 range, from 0
  to 2^64-1, or others, on the heap, which `Generic` answers. An addition,
  subtraction, multiplication or comparison of two BigInts held in their
  Values, which the native handlers make themselves, is marked so too: in
  a runtime that runs the JIT, the feedback dispatcher's handler of it
  leaves such a site to Go until it has a mark (below);
- the register pcs after calls and element writes Go went on at, which the
  code had been built with no entry at (`Resumed`, `jitRegResumeSeen`);
-  the elements' kinds (`jitRegElemSeen`, jit_elem.go): at a host
  exit, an element read by its cell that met a typed array's, a BigInt typed
  array's or a string's element, or an object's property by its name
  (`TypedRead`, `TypedBigRead`, `StringRead`, `KeyedRead`), and a write to
  an object native code does not store to (`KeyedWrite`), the code to be
  compiled again from there (`jitRegReopt`); at a deoptimization, an element
  read made as a number that met what is none; and at a deoptimization or a
  host exit, of the code's own or of a callee inlined in it, an addition
  that met a string -- these every lowering of the function's register code
  is made with from there (the hints, `jitLearned`'s `regHints`, by register
  pc, `jitRegLower`): the read made by its cell (ir's `Reference`), the
  addition Go's (`StringAdd`), the additions of an element read of strings
  too (`stringConcats`). The interpreter's slow paths also mark string
  results of `add`, `add_imm` and their int32 forms, before the first
  compile (`regCode.stringAdds`). They gate the feedback call with
  `jitOn`, which inlines to false in a build without the JIT. The marks
  are atomic and shared by runtimes running the same code. `jitRegLower`
  takes them into the hints before lowering, including an inlined callee.
  They select general addition without proving either operand's type, so later
  numeric operands still add as numbers. An addition the compiler marked
  an append to an upvalue (`add`'s `RAddAppend`) is Go's with its result
  its own (`Owned`, `abi.GoOpOwned`): Go grows the string the binding alone
  holds in place (`jitStrAppend`), as the interpreter's `add` does, rather
  than making a new string at each `s += x`.

A guard's failure invalidates the code; anything else learned marks it to
be compiled again, valid meanwhile, without a deoptimization (see
[Tier-up](#tier-up-and-on-stack-replacement)).

**The operand lattice.** What the interpreter saw an arithmetic,
comparison or bitwise site's operands be -- the byte the feedback
dispatcher ORs into the function's lattice (below, "The feedback
dispatcher"; jit_lattice.go) -- is `Operands` (`ssa.OperandFeedback`), by
the program's instruction, for an operator but `==` and `!=`, a
relational comparison or branch, an update and a unary operator but `!`;
where the function has no lattice (the Go table runs it) or the site is
none of the lattice's, it knows nothing, and the builder speculates as
before. Where it knows, the builder speculates exactly what was met, as
V8's optimizer does on its BinaryOperation and CompareOperation hints:

- **nothing** -- the site never ran: its block ends at it with a host
  exit (`unseen`, which `unfed` takes in), Go making the instruction whole
  and the interpreter going on to the next loop header, as V8's code
  deoptimizes softly for insufficient feedback, with no invalidation;
  the host exit ORs what Go meets there into the lattice
  (`jitLatticeWiden`) and marks the code to be compiled again
  (`jitRegReopt`), an inlined callee's its owner's. Only the first
  instruction of its register instruction, and none after a commit;
- **numbers** -- numbers, guarded, as before;
- **numbers and oddballs** -- numbers, as before, an oddball learned at
  a guard's failure: the oddballs are booleans as often as not, which the
  code makes numbers of natively where it knows them for booleans, and
  built generic, the sums of native loops' booleans (`some`, `every`) had
  been Go's, the loops with them;
- **BigInts**, alone or beside numbers and oddballs -- what the BigInt
  feedback says, below, which knows the operation's results;
- **a string or anything else** -- an object, a symbol
  (`abi.Operands.Generic`) -- generic (`generic`), Go making what native
  code does not, an addition by `GoAdd`, a comparison by `GoCompare`.

A pure body's runs pureCall answers without a frame, numbers alone,
which the dispatcher never sees: its sites answer numbers besides what
their bytes hold. The BigInt marks (below) stay beside the lattice: they
know the operation's result, as V8's BigInt64 hint does, where the
lattice knows its operands only. A lowering takes the lattice's strings
and objects as hints too (`jitLatticeHints`): an addition that met a
string Go's (`StringAdds`), an operator that met an object run whole
(`WholeOps`), as the interpreter's marks and the exits had taught them.

A compile made before the interpreter has run the function to a return
`abi.OperandRuns` (8) times -- at an early call, entered at a loop of its
first, inside a recursion's first run -- takes a site that met nothing for
one it knows nothing of, numbers, as without a lattice; and from then on
every compile does, since that code runs those sites natively and records
nothing of them (`jitLatticeRan`, the lattice's two bytes past its
instructions': the runs the dispatcher's returns count, and the one such
a compile sets). V8 compiles only a
function whose feedback the interpreter has had a budget to fill.
`TestJITLatticeSpeculates` compiles functions whose operators met
strings and objects, which run with no deoptimization, and one whose
arithmetic met nothing, whose first run is a host exit that
invalidates nothing and teaches the lattice.

**What it answers.** `jitRegFeedback` answers every question the builder
asks; nine of its answers it takes from the shared state's own methods
(`jitFeedback`'s `ArrayProtos`, `GenNext`, `HasCache`, `KeyAtoms`,
`LeafKind`, `MapNexts`, `TypeofCells`, `VirtualRoot` and
`WalkedCloseCell`). Where its answer at a site is nothing, the builder
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
| what an arithmetic, comparison or bitwise site's operands met (the operand lattice) | `Operands` |
| steps, `in`, `typeof`, scopes' ends, typed arrays' kinds | `GenNext`, `MapNexts`, `KeyAtoms`, `HasCache`, `TypeofCells`, `WalkedCloseCell`, `TypedKind` |
| elements' kinds learned at exits, captured typed arrays | `TypedRead`, `TypedBigRead`, `KeyedRead`, `KeyedWrite`, `StringRead`, `UpvalueTyped` |
| a pure body (`bytecode.LeafPure`, the compiler's fact), whose operators take BigInts native code holds as such | `PureBody` |
| closures and captured registers' cells from pools, an arrow's this its maker's register 1 (`ssa.ClosureSite`'s `LexThisReg`) | `Closure`, `CellsPool` |
| `instanceof` of the constructor a global or a captured binding read just before holds, the searches, `parseInt`, BigInts' strings, by the built-in its site's cache holds | `InstanceOf`, `IndexOf`, `ParseInt`, `BigString` |
| generators a loop's step or a call of `next` resumes natively, learned at the calls of Go that stepped them (`jitRegGenSeen`) | `GenSteps` |
| a loop's step's iterator's `next` written in script, called natively, and its results' layout, learned at the calls of Go that stepped it (`jitRegStepNextSeen`) | `NativeCalls` |
| keep cells' limit, after a build that ran out of them (`mir.ErrKeeps`) | `KeepLimit` |

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
nothing of it; every other entry is plain's handler. A fused operation
whose partner is a jump that plain's handler takes itself,
`instanceof_next`, has for its feedback handler plain's handler of the
operation unfused, `instanceof`, through that operation's entry
(`nativegen`'s `feedbackUnfused`): the partner then runs as its own
instruction and counts its back edge (below). Its calls -- `call_w`,
`call_method_w` and those in tail position -- count the calls made at each
call site, and its returns the function's runs, into the counts of the
function's closure's code (`jitRegCounts`: slot 0 the runs, 1+C the calls at
call site C), made with the call sites' caches where the runtime runs the
JIT (`jitRegCountsMake`). A callee that missed the site's cache counts
once: `nativeCallMiss` makes the cache the callee's and has the handler
make the call again, whose instruction dispatched again counts again, and
takes back the first count (`jitRegUncount`,
`TestJITRegFramedMissCountedOnce`). `Frequency` is a site's calls over the
runs, once `jitCountsEnough` runs are counted (jit_freq.go); not a leaf's,
whose calls the call path answers without a frame and so without its
returns, nor a generator's or an async function's, whose runs end at
suspensions too, nor a construction's, which Go makes. Native code counts
nothing, as V8's optimized code counts nothing; a run that on-stack
replacement took into
native code ends at a return of the interpreter's (`jitRegLoop`), and
counts. Nor does Go's dispatcher count, where it runs register frames in
place of the native handlers (`QJS_NONATIVE`, a verifying build): no
frequency is known there. Its additions, subtractions, multiplications and
compare-and-branches of two registers (`nativegen`'s `feedbackBigOps`),
which plain's handlers make of two BigInts held in their Values with no
Go, look at their operands first: two such BigInts at a site with no mark
of any kind in its register code's `bigSeen` leave the instruction to Go,
the exit stub through the table, whose handler makes it and marks the site
by what it met (`regBigOf`); anything else goes on in plain's handler. A
site is left so once, and the JIT's first compile makes it natively
(`regBigSeen`) rather than learning it at a first exit from native code,
deoptimizing. Plain's handlers do none of it. Its arithmetic, comparisons and bitwise
operators (bytecode's `LatticeOps`: of two registers, of a register and an
immediate, of one register, and the compare-and-branches of two registers
and of a register and an immediate) OR the kinds of their operands into
their instruction's byte of the function's operand lattice (the closure
code's `jitRegOps`, jit_lattice.go; nativegen's lattice.go), made at the
template's first run where the runtime runs the JIT on the native
handlers: an int32's number (not -0), another number, an oddball, a
BigInt in the int64 range, another BigInt, a string, anything else
(`abi.Operands`), an immediate an int32's. As V8's feedback update, a
handler tests the byte for what its operands are and branches past the
store where it has it, a kind it lacks learned out of line, operand by
operand: numbers, below the least tag, take one test of the byte for
both operands, and BigInts in the int64 range, out of line, one compare
of the two words ANDed and one test, so that a stable site costs a
load and a compare an operand and a test, never a store (`feedback`'s
`jnlt` on two int32s 26 instructions, plain's 12). An operator that
marks BigInts looks at the words the lattice loaded, and at none where
the lattice found two numbers: its `jnlt` on two BigInts of a marked
site 54 instructions, the marks' alone 42 -- a comparator Go's sort runs
in the interpreter, as `typed_i64_sort`'s, pays it at every comparison.
A number is in the byte where it has any number's bit, so that only a
site's first number is converted to tell an int32; an int32's site that
meets a double later learns it at the deoptimization that follows
(`jitLatticeWiden`, which classifies exactly). It is reached through the
frame's cc, as the counts are, not a pinned register: the native
context's `fv` and the register kept for it (amd64's R14, arm64's R15) stay
for a vector pinned where no plain variant has to switch it, baseline
code's. `TestJITLattice` runs every form on operands of every kind, from
bytes that hold nothing and what the handlers' tests find, and checks
each byte against `jitLatticeKind`; `TestJITLatticeBigMarks` that a site
whose byte has met BigInts is still marked at its first two.
`TestRegNativeFeedbackSelected` checks a runtime picks its table, and
`TestRegNativePlainSameWithJIT` that a build with the JIT makes plain's as a
build without does.

## Tier-up and on-stack replacement

A function is compiled once the interpreter has spent a budget on it:
`jitCallThreshold` invocations (400), each the function's length in
register instructions (`jitRegBudget`), each counted -- V8's interrupt
budget is a function's too (`jit_tier.go`):

- a run that returns spends its function's budget by its length: the
  feedback dispatcher's `ret` and `ret_undef` (nativegen's `feedback.go`;
  the Go table's `rhRet` and `rhRetUndef`, `regTickRet`) count down the
  runs left of it (`closureCode`'s `jitRegTick`, armed by `jitRegArm` to
  what is left over the length), and once none is set the function's
  `jitEnters`, which every call of it reads: its next call goes to the
  JIT (`jitRegEnter`), which spends the runs counted since it was armed
  (`jitRegCallTier`) and compiles it if the budget is spent, or arms the
  count again. A run that ends in native code, or a throw, counts
  nothing;
- a backward jump spends its function's budget by its distance, as V8's
  `JumpLoop` spends its bytecodes' distance: the feedback dispatcher's
  jumps, the step-and-branches' among them, which jump as their partners
  do, and `jbool`, and the partners of the `instanceof_next` it runs
  unfused (nativegen's `feedback.go`, `feedbackEdgeAMD64` and
  `feedbackEdgeARM64`; the Go table's `regJump`, `regJumpSlow` and
  `regStepTaken`, `regEdgeTick`) count down the work left of it for back
  edges (`closureCode`'s `jitRegEdges`, armed by `jitRegArmEdges`), and
  once none is left end the interrupt budget, so that the jump, taken,
  leaves to its Go slow function's check (`regBackEdge`), which spends
  the work counted (`jitRegLoop`, `jitRegEdgeWork`) and arms the count
  again. A check where the interrupt budget ended by itself spends what
  the count has so far, so that where a loop is compiled is the same
  whatever the interrupt budget, varied from one expiry to the next
  (`backEdgeBudget`), did.
  Counted so, not sampled, a loop whose turns call native code with loops
  of their own, as Crypto's `bnModPow` does, is compiled when its own work
  has spent the budget, not as samples of the interrupt budget happen to
  land on it. A template's count starts at 0, so that its first back
  edge's check arms it with V8's interrupt budget for feedback allocation
  (`jitBudgetForFeedback`, 940, or the budget where that is smaller),
  whose end begins the budget, as V8's feedback vector, made then, holds
  it: a function is not compiled after one turn of its loop, on that
  turn's feedback alone. Code that runs counts with no end
  (`jitRegCounted`): the interpreter's work in its loops counts toward
  its code's (`jitRegSpent`'s `osrWork`) at the interrupt budget's
  checks, which alone look at such a frame, as before its compile. A
  function never compiled counts nothing more, nor does any in a runtime
  that runs no JIT (`regEdgesOff`).

Every call the interpreter makes of a function whose `jitEnters` is set
goes to the JIT: `rhCallW`'s hit and `regEnterW`, `regEnterScoped` (a
host's call); and the native call handler's, which calls
Go for it with no exit, as it calls a built-in (`nativeJITCall`, by the
exit's protocol through `callH`). A site's cache holds a callee with code
as any other (`nativeCallMiss`, `regCallSlow`); no site is marked for it.

**Compiling.** A function's first check -- a call's, or its first back
edge's -- begins its budget, notes its caches' feedback
(`jitFeedbackMark`), and spends its own work of it at once: a call's
first, the run its return counted. It is compiled (`jitRegFor`) at the check
that spends the budget, its caches' feedback unchanged since -- a change
raises what is left to `jitICThreshold` invocations' worth (`jitRegWarmUp`),
as V8's `NotifyICChanged` delays a tier-up -- or at its first sample where
the runtime compiles at a first call (`QJS_JIT_STRESS`'s threshold): a
call's, its second call, the first run's return having counted it. What
outlives a function's compiles -- what its code learned, its budget, its
deoptimizations, what its runs did -- is its `regLearned` entry
(`jitRegEntry`'s `life`); its code is kept on its template's `closureCode`
(`jitReg`). A callee native callers call that has no code is compiled at
their first call of it through Go (`jitRegNativeCallee`): they make no
sample of it. But one too long to inline (`ssa.MaxInline`, against its
register code's length scaled, `jitRegInlineSize`, to the unit of the
stack bytecode the front end once made, which the limits were set in) that
is warming up, its budget begun and
not spent or not begun -- run fewer times than its first compile takes --
and not refused for host operations alone (`jitRegLongWarming`), is compiled
once its budget is spent, as any function -- the interpreter's runs of it
that Go makes for its native callers count it down -- unless its caller
calls it often: no longer than a function inlines in all
(`ssa.MaxInlineTotal`) and invoked at least 5 hundredths as often as the
caller, V8's `min_inlining_frequency`, each counted by its runs while it had
no code (`jitRegInvoked`, the budget's ticks) and the caller's since by its
entries at its start, its link's calls and Go's calls of it
(`jitRegCalledOften`). So a set-up function run once an iteration, as
Mandreel's is, is compiled once its runs have spent its budget, not at every
call of it through Go.

**Compiling in the background.** A runtime made `WithJIT` compiles in the
background by default where GOMAXPROCS is more than 1, and on the spot where
it is 1 (`SetJITBackgroundAuto`, `jitBackgroundNow`, decided at each compile):
with one P a worker runs only when the runtime's goroutine is preempted,
which native code never is, and the code came late -- Typescript 27% slower,
the V8 suite's first runs 1.5 to 4.8 times as long -- as the WebAssembly
engine's optimizing tier decides too. `WithJITBackground(true)` compiles in
the background whatever GOMAXPROCS is, `WithJITBackground(false)` always on
the spot, which keeps a run's tiering the same from one run to the next, as
a `vm.Runtime` New makes does unless told otherwise: the VM's tests, and the
tools, `v8bench` without `-jitbackground`, `-jitcompile`, `-jitheld`,
`compilediff native`, compile so. Such a runtime, or `QJS_JIT_STRESS`'s
`background=K`, compiles register code in three steps
(`jit_bg.go`): the runtime's goroutine takes the code's register code, its
program lowered on the worker (below), and asks its feedback ahead (`jitRegSnapshotOf`) -- every question the build may ask,
at each instruction the methods the build asks of its op (`jitSnapOps`),
those asked by a state's PC (`Resumed`, `EntryGeneric`) at the learned
resume and failed-entry positions and after cached recursive calls,
with unlisted positions inside the register code answered false,
of the function and of the callees it may inline, each a snapshot of
its own, to `jitSnapDepth`, nothing of a callee no site inlines whatever its
feedback (`ssa.NeverInlined`: too long, or of what `InlineCalls` refuses
before it reads the callee's feedback), and of each leaf an answer names its
kind and, at its call, its pool and its array made again (`askLeafCalls`) --
into a `jitRegSnapshot`, which has
`jitRegFeedback`'s methods alone, generated from them (`snapgen`,
`jit_snapshot_gen.go`), so that the build finds the same optional
interfaces it would. Each method's answers are kept as its results, unboxed,
in a table of its own by argument (`jitSnapTable`, put in the order of the
PCs and searched by halves, the questions put out of order sorted once the
asking is done), and what a question appended to the feedback's lists, taken
back, is a slice of the snapshot's own lists (`jitSnapshot`'s `gained`),
found by its index in the answer; boxed in a map by question, an answer's
keeping had cost about as much as its asking. A method of no argument whose
answer is the runtime's (`snapgen`'s `compileWide`: the realm's array
prototypes, the generators' `next`, the caches native code reads) is asked
of the compile's own function's snapshot alone, and its callees' answer
from its table, its effect used as its: each had asked them again, about
0.2 µs a question of the asking itself. A method may instead be
derived on the worker (`jitSnapDerived`, `jit_snapderive.go`): the
goroutine does not ask it at the PCs, but gathers once a snapshot -- the
function's and each callee's -- a copy of what its answers are made of that
the interpreter or the goroutine may change meanwhile (the caches, what the
code learned), and the worker, before it builds, derives the answers from
that copy into the same tables, their effects appended to a feedback of no
runtime's (the snapshot's sink) so that they are put back at the install as
the asked ones are. A derivation reads only the copy and what does not
change once made -- the register code, the closure's names, the fields of a
shape a cache remembered or of a shared one -- writes nothing of the
runtime's, and is the feedback's own: the feedback on the spot derives from
the live caches as the worker does from their copy. `Property` is derived so
(`jit_snapprop.go`): its gather copies the closure's caches whole, each
one's earlier entries with it, and what the code learned of its sites,
makes the caches the feedback makes of its own -- a `regexp_lit`'s, a
known read's -- and copies them, and asks the heap the little the answers
need of it: a private key's symbol, whether an addition's next shape wants
its index built, a chain cell's addition; it remembers the prototypes'
shapes a learned addition along a chain names, which may be unique and
changed in place until remembered. Every other shape an answer names a
cache remembered when it filled, or is shared, so the worker reads its
fields as they are (`jitPropSrc`, live on the spot and copied in the
snapshot). `Accessor` and `StringProperty`, whose answers read the heap --
the accessors' pairs, a getter's code and its lowering, the primitives'
prototypes -- are asked on the goroutine still, but by the family's gather,
by register pc, and only at the sites the copy says may answer
(`jitSnapAskedAtGather`): an accessor's site whose cache, its earlier
entries or what the code learned holds an accessor, a method's read that
learned primitives or knows nothing; every other PC's answer is none, as the
asking gave it. They are asked after the gather's takes of the heap -- the
caches the feedback makes of its own, the additions' facts -- since a
getter's case reads prototypes' shapes those takes may change. A gather
reads the register code and the copies, never the program (`jitSnapshot`'s
`rc`, not its `p` or `ops`), which is bound to the snapshot after
(`bindProgram`): what a gather asked of the heap it keeps by register pc,
and the family's binding (`jitSnapBinders`) puts it at each PC lowered of
that pc whose op asks the method, so that a callee's state may be gathered
before anything lowers it. The constants a program reads, the typed arrays
its captured bindings hold and `RawMath` are derived so too
(`jit_snapconst.go`): of the closure's constants, whose slice is never
written, of what each captured binding held when the gather read it, and of
the code alone. The methods that
answer from what the code learned at its exits and from what the interpreter
marks of its code (`jit_snapnative.go`: `Generic`, the BigInt and
element-read methods, `CallShape`, `Frequency`, `Operands`, `InlineExit`,
`BigIntSole`, `PureBody`, `GoCalls`) are derived so, from a gather that
takes each list of the learning by its header, an O(1) copy that is the list
as it was: a list of it only grows, appended past every header taken, or is
made anew -- `ssaHostsLeft` at each install, `regCallsMade` at a pc's first
call -- never written within a header's length
(`TestJITLearnedListsPersist`). The learned property sites (`poly`), whose
sites the exits write in place, are taken so too (`jitPolyTake`): taken, the
list is made anew at the next write of a site, which every writer gets
through one helper (`jitPolySiteAt`), and the one rewrite of a site's cases
in place makes them anew, so that a list taken never changes under its
header (`TestJITLearnedPolyPersists`). The gather copies whole what the
interpreter writes in place, the operators' BigInt marks, the operands' lattice and the calls'
counts. The methods that compare a call's callee, as its site's cache holds
it, with the realm's built-ins (`jit_snapcalls.go`: `CharCode`, `Substring`,
`FromCharCode`, `Intrinsic`, `IndexOf`, `ParseInt`, `BigString`) are derived
from the closure's call caches copied whole once a compile, in its copy
(`jitClosureCopy`'s `calls`, restored first where a site is empty, as
`regCallsRestored` restores them), and the realm's built-ins, a few words
copied once a snapshot; a native function's data, which the answers read of
a callee, is fixed when it is made. `NativeCalls` is derived from those
copies too (`jit_snaptargets.go`), with `ViaLeaf`, `Resumed` and
`EntryGeneric`, which read what it learns, through one implementation with
the spot's (`jitCallSrc`: `jitCallLive`, `jitCallCopy`), at every PC; only
typed code's are asked on the goroutine, at their PCs in the methods'
order (`jitSnapAskedAt`). The goroutine's part is to copy, look up and
make, by register pc, never reading the program the function is lowered
to (a callee's may not be lowered yet). It copies the call caches and the
learning's headers; the latter are taken once the PCs are asked, so that
they hold what the questions still asked on the goroutine appended, cut to
their lengths so that an append makes a list of its own, and the learned
calls' entries are never written in place either -- a target marked called
by its code, an inlined call's exits counted, a step's generators grown --
each such write a list made anew. It looks up the facts of each target the
copies name (`jitCallFacts`: what of `jitRegTarget` no argument count
decides, an arrow, a shared this; a learned construction's
`jitCallConstruct` and its pool's room), each call site's, once a closure
a compile, in one pass forward over its code (`jitCallSiteFacts`,
`jitClosureCopy`'s `sites`, which the inliner's callee walk reads too: the
function the binding before a call the interpreter has not made holds now,
a method call's seen callees), what `super()` and a push read of the heap,
and the realm's built-ins. And it makes what an answer must find made, as
the asking would: the links of the targets native callers may call, kept
in the snapshot's gained links (`gatherLink`), the only links a
derivation's effects may name; register code a binding's function has
none of (`regOf`); a RegExp literal's pool, a step's result objects' pool,
a generator function's call's pool, the copied layouts' cache; and the
pool of a construction no question has learned one at
(`jitRegSeedConstruct`), which the asking's `NativeCalls` did not see where
it looked for an array's pool before learning it, unless `Inline`'s
question there learned it first -- so the derivation does not see it either
(`allocHidden`). The pools and leaves a derived answer may name are kept
(`jitSnapData`'s `pools`, `leaves`), their roots and kinds asked ahead with
the answers'. The worker derives every answer from these, the program in
hand: a call's window, a whole instruction's operands. A derivation appends
to its copy of the learned calls, as the asking appended to the live list,
and the runtime's goroutine writes the appends when the compile is done
with, installed or thrown away (`jitSnapFinishers`, `jitTargetsFinish`),
from a list the worker prepared (`jitLearnedPrepared`). A compile thrown
away before a worker derived it is derived there first. A concurrent
compile writes no feedback until it is done, as V8's does not. The merge
puts a call's appends after what its list had when copied and before what
exits learned meanwhile, as the asking had placed them: a call's targets
are read in the order of its entries (`jitLearnedMerge`). The families'
derivations run by stage (`jitSnapDeriveStage`), so that `Inline`'s and
`NativeCalls`' appends are there for `Resumed`; their finishes too
(`jitSnapFinishStage`), so that a callee's entry is made before what it
learned is written into it.

The inline sites -- `Inline`, `ForEach` and `CalledClosure`
(`jit_snapwalk.go`) -- are derived on the worker through one
implementation with the spot's (`jitInlineSrc`: `jitInlineLive`,
`jitInlineSnap`; `jitInlineOf`, `jitForEachOf`, `jitCalledClosureOf`), as
Warp decides its inlining from what its serializer copied. Which callees
they may take is decided on the goroutine by `Inline`'s own decision as far
as it goes before it lowers (`jitInlinePrefix`: one native target, not a
cold call of the function's own, no megamorphic getter, no parent's
constructor, arguments only read), asked once a snapshot's gathers have run
at each call by its register pc (`discover`; `jitCallCtx` without a
program, its window the call's own registers), of the gathers' copies; the
calls' targets it finds are kept, with what finding them appended to the
keep, for the derivation, which finds them no more (`jitKeptTargets`) but
where a call's window is another (a literal's copy). A loop's callback and
a template of a closure the function makes are taken as they are. Each
callee taken is a node: what an answer reads of it taken there, what its
caches have seen taken into its learning, as an answer inlining it takes it
(`jitRegSeenInlined`), the hints its program is lowered of copied
(`jitRegHintsCopy`), and, of one short enough to inline, a snapshot of its
own, gathered of its register code alone, nothing of it lowered, its calls
asked so in turn as deep as the builder inlines (`ssa.MaxInlineDepth`; a
snapshot that deep is asked no inline site's question). A callee that has
learned nothing is gathered of an entry made for the snapshot
(`jitLearnedScratch`), which the finish adopts as the function's where a
derivation read it (`regLearnedAdopt`, `jitFinishEntries`). The worker
lowers a callee's program of those hints, or takes the runtime's where it
is lowered of them, and the install keeps what it lowered
(`regKeepLoweredAt`); a node's answers are derived where a derivation
first takes it to inline it (`calleeOf`), its program bound then, as the
asking had asked a callee where an answer named it (`nestReg`). The
gather's decision differs from the worker's only where it cannot know the
order the derivations take callees in, a megamorphic getter's answer
before or after the seen feedback, and there it takes the callee; a callee
a derivation takes that the gather did not is a miss (`CalleeMisses`),
which `TestJITSnapshotCalleeWalk` holds to none over Octane's functions. A
compile whose asking meets what a worker cannot decide -- a forwarder
inlined with its call's arguments, which reads its frame's receiver; a
parked callee, which the asking unparks; the function's typed code, or a
call site whose facts do not hold what its derivation reads -- asks its
inline sites on the goroutine as before, its callees lowered there, each
kind counted (`JITGoroutineTimes`' `Eager` counts); a callee's typed code
or such a site has the callee lowered and asked there alone
(`EagerLate`). So does a compile while any method the build asks at a PC
is neither derived nor asked by a gather (`jitSnapNodesBlocked`), which a
node, its program not lowered on the goroutine, could not answer.

A compile's own function may be gathered so too, of its register code
alone (`jitRegSnapshotOf` of no program), for the worker to lower it and
bind its program before it derives (`program`): no question then asks
the program on the goroutine. Its `CellsPool` is derived of the program on
the worker, of the learning's pool copied, a pool made there where it had
none -- allocations of its own, nothing of the runtime's -- which the
finish gives the learning where it still has none. Where the asking asks
the program after all -- the function's typed code, a call site whose
facts do not hold, a forwarder or a parked callee taken -- the snapshot is
none, and the caller lowers the function on the goroutine and asks again
of the program (`ProgramsAgain`); a snapshot of no program is bound
before any finish (`finish` panics otherwise). A worker builds, optimizes and compiles
from the snapshot
(`jitRegJob`), touching nothing of the runtime's; and the runtime's
goroutine installs it at a later check -- a sample (`jitRegHot`), at a call
or a back edge, or any call Go makes of a function with code or a compile
pending (`jitRegEnter`), or a native caller's ask for a callee's code whose
compile is done (`jitRegForAs`, outside `QJS_JIT_STRESS`'s schedule), or a
call of Go the function's own code makes with its compile done
(`jitRegCompileDueNow`, outside that schedule too): a
compile handed to the workers marks its function's closure (`closureCode`'s
`jitEnters`) so that its calls go to the JIT, and a call of code compiled again in the background installs the
replacement as one of its first code does, taking the code its worker
placed into the cache as the compile on the spot does its own
(`jitRegPlaceWith`, below). What the questions the build asked appended to
the feedback's lists, taken back, the worker puts back once it has built
(`replay`): the lists are the compile's own feedback's, its root's, its
keep's and its callees', which the runtime's goroutine reads nothing of
until the install but the links a compile under way holds (`holdsLink`,
`regUnlink`), and those it reads as the keep held them at the hand-over
(`jitRegJob`'s `links`) and as the questions took them back -- a link is
made on the goroutine alone, never by a derivation. Put back at the
install, they had been about 12 of its 40 us on Octane's Typescript. A question the snapshot was not asked
is a miss (`jitSnapMiss`): the compile is built at the install instead
(`regSnapMisses` counts them). A compile done while the code it replaces is
run by a native call under way waits (`jitRegInstallWaits`): kept, no
longer one of the `jitJobsMax` compiles the background has room for, it is
installed at the first check after that call is done, as a compile on the
spot is not made meanwhile (`jitRegForAs`), rather than thrown away and asked
again: a call of Go the replaced code makes from its own run, with no native
call of it below, is due for it (`jitRegCompileDueNow`), the run leaving and
its exit installing it (`jitRegGoOnAt`), as for a compile due on the spot. A function with a compile under way or waiting is not asked for
another (`jitRegCompile`). Meanwhile the function runs in the
interpreter, or in the code the compile replaces, valid and called by its
native callers, and is not compiled again (`regPendingJobs`). A run of the
code an install replaced leaves it at its next call of Go
(`jitRegCompileDueNow`'s `jitRegRunReplaced`), where no native call of that
code below it is under way, and its exit goes on in the new code
(`jitRegGoOnAt`), as it goes on in a compile made on the spot at its leaving
for one due. So a compile in the background makes the code the compile on
the spot makes from the same feedback, and the frames that ran the code it
replaces go on in it where they would have gone on in that. What the code
it replaces learned at its exits meanwhile, after the feedback was asked,
the new code was not built for, and its exits there learn nothing more,
the learning done: the install leaves the function marked to be compiled
again for it (`jitRegJob`'s `asks`, `jitEntry`'s `reoptAsks`), as code
that learned is once it has spent a budget (`jitRegLearnedDue`). A compile
of a function deoptimized since it was prepared is thrown away at the install
(`jitRegJob`'s `deopts`), and so is one whose code in the cache a native
call still runs (`jitRegEntry`'s `levels`), that code asked to be compiled
again. Code for a frame waiting at a loop whose entry escape analysis
dropped (`jitRegLoopCode`) is compiled in the background
too, as V8's OSR compiles are concurrent: the job (`osrAt`) waits beside the
function's code it was asked for (`jitRegEntry`'s `osrJob`), the frame goes
on in the interpreter meanwhile, and the install keeps the code beside that
code (`regKeepOSR`) -- or throws it away where the function deoptimized or
its code was compiled again or let go of since, its loops then the new
code's to learn of. Top-level code is compiled on the spot.

While a function's compile waits, its budget begins anew (`jitRegHot`), as
V8 begins a function's interrupt budget anew while its compile is under way:
its loops' back edges count it down, the interrupt budget's checks and its
calls installing the code meanwhile, and the next ask comes a budget later.
Its budget left spent, each back edge had been a check, a call of Go, the
waiting slower than the interpreter. A function the background had no room
for (`jobsFull`) keeps its budget spent and asks again at the interrupt
budget's checks (`jitRegArmEdges`). The work a function does waiting, its
samples' and its native callers' asks, is its job's (`jitRegJob`'s
`waited`), and the process's workers take the queued compile whose function
did most (`jitWorkerPool`'s `next`), as HotSpot's compile queue serves the
method invoked most since it was queued: one no longer run waits behind
those that are, and once it has done no work from one of the flush's ticks
to the next, an epoch or more of the program's running, its runtime drops
it unbegun (`jitRegDropStale`), as HotSpot drops stale tasks; it asks again
once hot again. A compile no worker has begun by its function's second ask,
two budgets of the interpreter's work after it was handed over, is taken
back (`jitRegTakeBack`, `jitTakeBackAsks`): made on the runtime's goroutine
and installed at once, a worker that comes to it after leaving it as it is.
A compile takes about as long as the interpreter spends on two or three of
its function's budgets, so that a process whose cores are all busy -- a
host running a runtime on each -- waits for its code no longer than about
twice what compiling on the spot costs. A worker places the code it compiled
(`jit.NewSSACodeApart`), apart from the code the runtime's goroutine runs
meanwhile: on pages none of the arena's code was ever in or ran from
(`PlaceRawApart`'s rule), so that the install takes it as placed
(`jitRegPlaceWith`), its pages sealed and the instruction cache flushed off
the program's time; an install that throws the compile away, or has no room
for it, closes it.

A compile in the background lowers nothing on the runtime's goroutine
where it can help it (`jit_bgroot.go`). A lowering is a function of the
register code, which is never rewritten, the function, the kind of program
and the hints the code learned, whose lists only grow; so a worker makes the
program the goroutine would. A compile whose snapshot is gathered of no
program (above) is handed over with the function's hints as they are,
copied, and the programs its lowering had made of them, if any
(`jitRegHintsCopy`, `jitRegRoot`); the worker's first step lowers the
function, the kinds that share its program at once (`jitRegLowerKinds`),
or takes the programs made, chooses the kind as `jitRegCompile` does on
the spot -- an entry's, or, refused as host operations alone, a callee's
where it is compiled for its native callers or a hot one's where a call
it makes is native -- and binds the snapshot to it (`program`), which
derives. Its callees' programs are lowered as `Inline` takes them, on the
worker too (`regKeepLoweredAt` at the finish). The install keeps the
programs in the function's lowering, as one lowered on the goroutine is
kept, counted and trimmed the same; where no worker ran, the drop of a
stale job and the install lower it there first (`lowerRoot` is done
once), and a refused lowering finishes nothing, its compile refused as
one lowered on the spot is. A function whose lowering was refused before
is refused on the spot, not handed over, nothing gathered -- one of host
operations alone too where it is neither compiled for its native callers
nor makes a native call (`jitRegCallsNatively`, asked before its
feedback, as on the spot); typed code is lowered on the goroutine: its
link asks for its code once (`jitTypedLinked`). On Octane's Typescript, the root's lowering and its
callees' had been about 50 of the 254 us a compile in the background
spent on the goroutine.

**Compiling again.** Code is compiled again, in place, when:

- a guard failed: the code is invalidated (`jitRegInvalidate`): its link
  cleared, Go no longer
  enters it, and the interpreter runs the function until it has spent a
  fresh budget, a change of its caches' feedback meanwhile delaying the
  compile (`jitRegWarmUp`), as V8's deoptimizer resets a function's budget.
  The calls Go makes of it for its native callers spend it too, an
  invocation each (`jitRegNativeCalleeN`), a loop of a built-in's call an
  invocation for each element; a leaf's, which the interpreter's calls
  answer without a frame and so never sample, is compiled again at its
  native callers' next ask. However often a function deoptimizes, it is
  compiled again, as V8 limits no function's deoptimizations: each widens
  the site it failed at (below, "Deoptimizations end at their sites");
- its exits learned what it is to be compiled for -- a call's target, a
  site's case, a callee to inline, a loop of a built-in's call, an entry
  after a call: the code is marked (`jitRegReopt`, `reoptPending`) with
  no deoptimization, as V8 never invalidates code for
  feedback. It stays valid -- Go enters it, native callers call it through
  its link -- and is compiled again once it has spent a budget since it
  began to learn, with nothing more learned meanwhile
  (`jitRegLearnedDue`): its invocations, Go's entries and the
  runs it went on in after leaving, and native callers' calls, each its
  length, with the work its native stretches did -- their back edges, each
  its loops' mean length, and the instructions to where they left
  (`jitRegStretchWork`) -- and the
  interpreter's in its loops before Go entered them (`jitRegSpent`). A
  call of Go its native run makes counts the work the run's stretch has
  done so far (`jitRegRunWork`), as V8's optimized code spends its budget
  at each `JumpLoop`, not only where the stretch ends -- at an exit, or at
  a poll the varied interrupt budget makes, which would have a loop that
  learns at its calls of Go compiled again wherever a poll happened to
  come. What it learns after its budget began raises what is
  left to `jitICThreshold` invocations' worth from then, at the next such
  call (`jitEntry`'s `tierLearned`), as V8's `NotifyICChanged` raises a
  budget when feedback changes, not from where the budget was spent. It is
  compiled at a call that enters it (`jitRegEnter`), even judged slow, which
  would never be entered and never compiled for what it learned otherwise;
  at a call of Go it makes, which then leaves for it unless a native call of
  it in an outer run runs it (`jitRegCompileDueNow`) -- a native callee's is
  compiled once the unwinding has let it go (`jitRegUnwind`), code only
  native callers call having been compiled again at none of these otherwise;
  at a sample, at an exit, or at a loop's poll, which leaves for it rather
  than calling Go while it learns (`GoPoll`), and a run goes on in the new
  code (`jitRegGoOnAt`, `jitRegReoptimize`). The compile is asked with the
  register pc the frame Go goes on with waits at, and the runs of the code
  Go is inside after their calls of Go, each learned as resumed first, so
  that the new code has the entries they go on at, as V8's compile for a
  frame carries its bytecode offset: the call the frame waits after is often
  the one just learned, which the new code makes natively, with no entry
  after it. Where Go goes on after a call or an element's write the code
  was built with no entry after (`unresumed`) all the same, the code is
  compiled again at once with the entry and goes on in it
  (`jitRegResumeSeen`). An element's write leaves only where Go refuses it
  -- an append is a call of Go that goes on -- so, as V8's code is entered
  only at a function's start and its loops, it has no entry after it until
  Go has gone on there: an entry after each had cut a loop of element
  updates into a block a write, joined by phis. A recompile refused
  keeps the code it has.

A change of a function's caches' feedback alone has its code compiled
again for nothing: its mark (`jitFeedbackMark`, `jitLearnedMark`) only
delays a compile, so that an exit the interpreter finishes, filling the
caches, does not drop the code and compile it again. Code
compiled before a site's cache knew a shape reads it by a scan, which never
leaves native code to learn one.

**What is entered where.** Of the code compiled (`jitRegTakes`), what
`jitRegCalled` says -- neither a generator's nor an async function's code,
nor top-level code -- is entered at its start by calls, native callers'
among them, and inlined. Top-level code, which runs once, is entered only by
on-stack replacement at its loops, as V8 enters top-level code: its
declarations before them run in the interpreter. A generator's or an async
function's frame, which the interpreter suspends at each `yield` and `await`
and makes again at each resumption (`regGenRun`), is entered at its loops
and where it resumes (`jitRegEnter` from `regGenRun`), a first run past
`gen_entry`, which does nothing there: the lowering gives the code an entry at
each resumption -- past `gen_entry`, past an `await`, past the jump after a
`yield` that a resumption by `next` goes on past, and at the return path
that jump goes over (`lowerReg`'s entries). The suspensions -- `yield`,
`await`, `yield*`, the parameters' end, a delegation's step -- and
`gen_entry`, which reads how the frame resumed, are exits Go does not run
whole (`regWholeRuns`). At a `yield`, an `await`, a `yield*` or the
parameters' end, Go suspends the frame itself (`jitRegSuspend`), as the
interpreter would: the pc past it
kept for the resumption, the run not ended in the interpreter, so that a
`yield` of a generator Go resumed does not end a run there. An async
function's `await` the interpreter would go on from in place -- the frame
resumed by its await's job, no job queued, the value awaited settled, so
that the job the await would queue would be the queue's next, resuming the
frame with what the value settled to at once (the VM's `regSuspend`) --
native code goes on from in place too, with no exit (ir's `Await`, ssa's
`await`): `ssa.OpAwaitReady` reads the frame's kind, which Go gives each
run of an async function's code (`abi.Context`'s `AwaitJob`), and the job
queue's next index and length where the VM keeps them (`JobHead`,
`JobCount`), at the await -- a call before it may queue a job, so no two
are merged -- and exits to the await's state where the frame is to
suspend; a value no object is its own, and an object's is what Go finds it
settled to (`ssa.GoAwait`, the VM's `regSettled`), a fulfilled promise of
the realm's constructor's value, Go refusing anything else, which leaves
at the await. The interpreter's interrupt check there is its budget's;
native code's loops poll at their back edges. An async generator's awaits
never go on in place, the interpreter's rule. Go made each such await at
an exit before (`regAwaitsInPlace`, which an exit at one still asks), an
exit and an entry some 25 times the interpreter's await: `async_await`'s
loop, an await at every turn, was judged slow for its exits and left to
the interpreter, at 20.4 ns a turn with the JIT on a Ryzen 5 3600, and
runs natively now at 4.7; `async_busy`'s, an await every 256 turns, 5.9 to
4.3. A throw a resumption injects -- a
rejected `await`, a generator's `throw` -- that a handler of the frame's
takes Go catches before the frame runs (`regGenCaught`), the value thrown
in the handler's register as `regCatch` gives it, and the code is entered
at the handler, where it has an entry as for a throw Go catches at an
exit: left to `gen_entry`, the interpreter caught it and ran the rest of
the frame, an async function's loop that awaits a promise that may reject
natively in its first run alone. A delegation's step is left to the
interpreter, which goes on with the frame there. A synchronous generator's
code a loop's step or a call of `next` resumed natively suspends it at a
`yield` itself (see [Control flow](#control-flow), "Generators' steps"). A
generator's or an async function's code is compiled at its loops' samples
and by the resumptions Go makes of it, each an invocation that spends its
length of the budget (`jitRegResumed`, `jitRegHot`): an async function whose
loop awaits at every turn is resumed at every turn, and its back edges may
be too few for a sample. Code refused for host operations alone is asked
again once its resumptions have done `jitHotWork` of it. Its entries are
none OSR's -- none escape analysis may drop, for its resumptions' sake -- so
its loops' headers are found from the program (`jitRegShape`).

**Entering.** The code is entered:

- by a call from any site: the template's `closureCode` marks a function
  with code (`jitEnters`, set with it, `setJITReg`), which `rhCallW`'s hit
  and `regEnterW` test, entering the code (`jitRegEnter`), and the native
  call handler too, on both architectures, which calls Go for it with no
  exit (`nativeJITCall`) -- one test a call pays; no site is marked;
- by native callers, through the function's link (see [Native
  calls](#native-calls));
- by on-stack replacement at the loop whose back edge took the sample
  (`jitRegLoop`), from inside the dispatcher that runs the frame;
- at the next back edge of a frame whose valid code left it where it has
  no entry (`jitRegLeaveTo`), the interpreter's check made due there at
  once (`regGoOn`, `regForced`) and spending no budget: the interpreter runs
  the rest of that iteration, as from V8's deoptimizations, not the rest of
  the invocation. Code with a loop: an entry at one, or a header whose entry
  escape analysis dropped (`unentered`), where the frame goes on in the code
  kept for it (below); code with none, whose frame's invocation ends in the
  interpreter, does not spend its caller's check. Not where it left at a
  `ret`, a `yield` or an `await` (`regEndsFrame`): the frame's run ends or
  suspends there, and the next back edge is another frame's, whose budget
  the check would spend for no sample, and a generator's code leaving at
  each `yield` would keep its for...of loop's function from ever being
  sampled.

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

A loop whose entry escape analysis dropped (`ssa.Func`'s `Unentered`, by
register pc, as its header's frame state names it: `jitRegEntry`'s
`unentered`) has a frame found at its header
with no entry at two checks in a row -- each a full budget of back edges,
or the check made due at once after its code left it -- compile the
function apart with that entry kept (`ssa.Func`'s `FrameAt`, the header's
register pc too, its objects made) and go on in that code, kept beside the function's (`osr`, `osrOf`)
for the frames that wait there next and let go of with it
(`jitRegLoopCode`). A run
in it stays in it at its exits while the function's code is the one it
was compiled beside. Each frame that goes on in it from other code is
counted (`regGoOnFrom` says which code's run left it); past
`jitFramesLeaving` of them, three calls in four having left, the header is
learned as one Go goes on at (`jitLearned`'s `resumed`, the feedback's
`Resumed`) and the function compiled again with that entry, its objects
made there, the kept code going with the code it was kept beside
(`jitRegFramesLeave`), so that such a
call does not pay the exit's objects and a run of the kept code for the few
turns before it leaves. An entry at a loop's header the build did not mark `OSR`
-- a header learned so, any header of a generator's code -- is a loop entry
all the same (`jitRegShape`). `FrameAt` is a header's register pc, the unit escape
analysis compares with its headers' PCs. Any compile a frame waits at a
loop for -- the check that spends the budget (`jitRegLoop`), a compile
for what the code learned made in a run of a function hardly called yet,
at the loop around where the frame goes on (`regLoopAround`) -- keeps
that loop's entry the same way (`jitRegFrameAt`), unless native callers call
the function's code (its link's `refs`). Those entries (`ssa.Func`'s
`EntryKept`, `jitRegEntry`'s `kept`) are for that frame alone: called again
`jitKeptStarts` times, the function is compiled again without them, its
objects unmade for every call from then on (`jitRegKeptGone`, judged by all
it did, its stats kept) -- unless three of those calls in four left its code
(`jitRegKeptCallsLeave`; a host exit counts in code with kept entries, which
has an entry after it where the code without them has none), which would ask
for the entry back. A caller compiled to call it natively, whose calls Go
does not see, has it compiled so at once (`jitRegCalleesKeptNoEntries`).

**Profit.** Each native stretch -- an entry, the code entered again after
an exit, a call of Go -- costs what an exit and an entry would
(`jitSSAMinWork`), against the work the stretch did (`jitRegStretchWork`):
its back edges, at the mean length of the function's loops in the
program's instructions (`irLoopMean`), and the distance to where it left.
Every `jitSSAProbe` stretches, code whose work does not make up for
them Go no longer enters (`entrySlow`), native callers calling it still;
stretches begun at a loop are judged apart (`loopSlow`) -- and so are a
run's stretches after its host exits, where native code goes on at once,
a run that left at a
loop judged slow leaving to the interpreter after the instruction Go ran
-- and so are the settled calls of Go -- not a call of a function it may come to call
natively, but an async function's, which native code never calls
(`jitRegGoSettled`) -- of a run begun at a loop (`jitRegRunGoProfit`), unless the code
calls functions natively (`jitRegCodeCallsNatively`): judged slow there,
the run leaves once that call is made (`jitRegRun`'s `leave`). A native
callee's calls of Go are judged too, beyond its native callers' calls in
(`jitRegCalleeProfit`), unless it calls functions natively, as for a run:
code that does not pay is neither entered nor called natively
(`notNative`). Go
enters such code again after `jitNativeRetry` calls of it, doubled each
time (`jitRegRetrySlow`), a loop's checks of code judged slow at loops
counting as such calls. Native
callers call it again after `jitNativeRetry` of their calls, doubled each
time it was found not to pay (`jitRegRetryNative`): each call a native
caller makes of it through Go counts, whether Go enters it or not
(`jitRegKnownCallee`, from `jitRegCallSeenOf`), as do Go's entries
of it at its start. Compiled again, it is judged afresh: entered, and
called natively, until the new code is found not to pay in turn
(`jitRegCached`) -- at its loops too
where it was compiled again for a generator's step or making it learned
(`genLearned`, from `jitRegGenLearned`), the steps that each were a call of
Go now native; its calls of Go as a
native callee are counted afresh with its native callers' calls into it
(its link's `in`) wherever those are -- compiled again, compiled again for
leaving (`jitRegUnwound`), called natively again -- as its stats
(`ssaStats`) begin anew. What Go declines counts toward the work the function
spends for a compile for what it learned (`jitRegSpent`'s `slowCalls`), as
its native callees' work between their calls of Go does (`calleeWork`).
A resumption of a generator's or an async function's code judged slow is
declined the same way, and the next `jitSSAProbe` resumptions run in the
interpreter without a look at the code (`jitResumeSkip`). `JITReport` shows
it as slow, with the calls the interpreter made of it.

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

**States share their slots.** A frame state's slots are a persistent
array, a tree of 64-entry nodes that states share, as V8's frame states
share their StateValues (`ssa/slots.go`): a node holds a mask of the
entries it has and one value or child for each, and a change copies only
the nodes on its path that the edit making the state did not make itself.
The builder makes a state from the last one its block made, or, for a
block's first, from the last of its only predecessor, through blocks that
made none (`stateSlots`, `lastOf`): it reads again only the slots written
since -- every write to a block is listed for its next state -- and those
that the liveness at one PC names and at the other does not; a slot both
name that nothing wrote since has the value it had, which a read would find
again without making anything. A block of several predecessors, sealed,
makes its first state from its first predecessor's last (`mergeFrom`),
reading again the slots written in any predecessor since its last state
and those the predecessors' last states do not agree on, found where their
trees differ (`diffSlots`). A read finds a slot's value in its block's
definitions (`defTable`, by block and slot, of what there is), else in the
block's last state, which names it and which nothing wrote since, and only
then in the blocks before (`readVar`). In a sealed block of several
predecessors it marks the block, reads theirs, and makes a phi only where
they differ or a read came round a loop to the mark (`mergeVar`, Braun et
al.'s marker algorithm), numbered as it was when the phi was made before
the reads. A phi made for each slot a merge's first state names, however
alike its arguments, would take n·m phis for n values live across m ends of
branches, and as many definitions; and states naming every slot would name
n² slots for a function of n values live at once and an exit at each of its
n instructions -- 13,000 slots some seven gigabytes -- where they take what
the instructions change. The passes walk a state's set slots in order
(`FrameState.All`); those that do the same for each value wherever it is
-- the rewrites of replaced values, dead code's marking -- visit each node
once, a rewrite changing a shared node in place, as it replaces the value
in every state that has it; a pass that changes one state's slots alone (a
loop header's, `keep.go`) copies its path. A state's slots count as their
values' uses once for each value and block naming the state: `recount`
weighs each node by the states and nodes that have it, from the roots
down (`slotWeights`), and the builder counts no state's uses before it.
The backend does the same: `findLazy` counts a state's references into its
values through its nodes (`ssa.SlotWalk`), and `allocate` puts a block's
states off to its end and meets each node once, every value at the last
position, the last Go does not refuse and the first instruction's own of
the states naming it. An exit's description is its state's nodes too
(Exits and deoptimization).

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
nothing, `add_prop`'s addition speculates on numbers, its exit running the
instruction again from its start. So that it may, a read its origin goes on
after with an instruction that may leave is never built to call Go at a
megamorphic site's miss, but leaves for Go there (the builder's
`originGoesOn`): built to call, its addition would have been Go's, which may
leave, and the program refused. Copies never leave: `get_method_w`'s read,
which the method's and the receiver's copies into the call's window follow,
calls Go at its miss as a `get_prop`'s does, Go reading the method as the
interpreter's method read does, through the site's cache by its data
entries (the VM's `jitRegPropertyMade`, `cachedProp`) -- a getter's entry
there the interpreter would take for the method itself. Each such miss had
left native code: Octane's Typescript, reading `typeCheck`, `emit` and
`isClass` of AST nodes of more shapes than the megamorphic cache kept,
about 330 times a run.

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
register (`windowLeaf`, `leafMade`). One whose value nothing reads but its
truth is told so (`CallSite`'s `Unused`): the program's liveness
says (`windowResultUnused`) -- the register read by nothing after the call,
or only by a test of its truth or a not just after it, or kept where a
branch jumps where it is falsy, as `&&` keeps it. Exec's leaf then gives
back its dummy rather than making an array, replace's makes no string. A
leaf's call of `exec` at a site the register compiler marked for its array
to be made again in the memory of its last (`RMarkReuse`, the VM's
regexpreuse.go) is told the site by its register pc (`CallSite`'s `Reuse`,
from `ExecReuseFeedback`, `abi.GoCallState`'s `Reuse`, each in a word
another field leaves), with whether the
program's liveness has the pieces' local read by nothing after the call
before it is written: where the call is of the function's own code, not
an inlined callee's, the VM tells exec's leaf the frame's slot of the
result's local as the call's mark, and the leaf makes the array again only
where the same frame made the last -- the slots of a frame native code
runs say nothing of its locals, so none is read -- its pieces only where
the liveness says. The calls are told a site of their own, so that the
arrays native frames made and the interpreter's are kept apart, and one
whose site's last array another frame made forgets it, making a new one. A
fresh invocation in the same stack window may reuse the previous invocation's
array: none of that invocation's locals remain live. Entering native code
partway through a frame, including a loop header at pc zero or a native
call's continuation, still forgets arrays held in that frame's slots,
because native registers may keep references the slots cannot show. A
leaf's call of `exec` or `test` on a RegExp literal's RegExp -- a calling
`regexp_lit`'s, or a `call_method_w` marked `RMarkLitRecv` after one that
does not call -- is told the literal's pool (`CallSite`'s `Give`, from the
feedback's `regLitPool`, `abi.GoCallState`'s `Give`): nothing but the call
reaches the RegExp, and the VM gives it back to the pool once the leaf has
returned (`jitRegExpLeafGive`), lastIndex 0, where reading the method ran
nothing, as the call of Go it stands for gives the literal its RegExp back
-- a loop's `/x/.exec(s)` had a RegExp made at each turn by the pool's
refills. So too a replace's leaf of the RegExp a `call_method_w` marked
`RMarkLitArg` made its first argument (`jitLeafForm`'s `giveArg`,
`jitRegExpPoolGive`).
`BigInt(x)` and
`BigInt.asUintN(n, x)` or `asIntN`'s are made natively too (see
[BigInts](#bigints)), the width the constant the program copies to the
window's register on the way to the call alone (the VM's
`regConstOperand`). A call in tail position is made as any call while the
stack is shallow (`ssa.OpTailRoom` guarding it), and leaves for the
interpreter deeper, which makes it in constant space. A call of more
operands than the context's operands hold (`abi.MaxGoArgs`) is a native
call all the same, and where it does not make its call natively -- a
callee with no code, one past those it calls, a context past the deepest
-- a call of Go whose operands it writes to the callee's window past the
VM's stack's top rather than to the context, the room checked first, the
window's layout the frame Go makes the call of (`mir`'s `windowGoCall`,
the VM's `jitRegWideCall`). Such a call with no native callee is left to an
exit.

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
(`unboxPhis`, `sinkLoopBoxes`). A phi of boxes of float64s, numeric
constants and entry slots a guard unboxes already, each box used by phis
of numbers alone, it among them, is a float64 with no other evidence: its
box is made once where it is, not on each way to it -- a register loop ends
where the copy of its test ends too, and a sum it carries past its end had
been boxed at every turn for that phi. The box the loop header's phi takes
at its back edge is that phi's too, so both are unboxed in one round.

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
has seven general registers for values against thirteen SSE ones. A phi of
no loop's header whose arguments are all int32s -- conversions, constants,
others of its group, no entry's number -- is one whatever its uses: as an
int32 it is converted once after the merge, not in each predecessor --
register code's loop's test, which a loop's int32 read after it reaches the
exit from, at every turn. A box of
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

**Primitive cases.** A method read whose receivers have been strings, BigInts,
numbers or booleans reads the method off the prototype the realm fixed for
the kind, where that has it as its own data (`OpStringProp`,
`ssa.PropertySite`'s `Primitive`; the VM's `jitPrimitiveSeen` learns a kind
at the exit or the deoptimization that met it), as V8 reads a number's off
the HeapNumber map's prototype -- or off a prototype of that one's, a
number's `hasOwnProperty` off `Object.prototype`, the site's holders the
prototypes from the kind's on (the VM's `jitPrimitiveChain`), each checked
by its shape as a read's holders are, as V8 checks the HeapNumber map's
prototype chain. Such a method had made the site megamorphic at its first
deoptimization, and every receiver of the kind left native code there, the
exit teaching nothing. Each kind is one more case of the site, as a
V8 IC keeps the HeapNumber or the String map among an object's maps: a site
that has met several kinds, or objects too -- a shape its cache holds, the
cases, deep reads or a megamorphic cache the code met at its exits -- tells
them apart by the receiver's word (`OpTypeIs`, which exits for none of
these kinds), each kind's read on a branch of its own
(`PropertySite.PrimitiveCases`), the objects' on the last, all going on
where they meet. An object met at a read compiled for primitives alone is learned so too,
its cache filled, the code compiled again with the objects' case. Each
receiver of a case the code had no read for had left native code at every
read, the exit teaching nothing once the site had met objects: a number's
`x.toString()` at a site of a dozen object shapes, 128 exits in every other
call of 5,000.

**Megamorphic reads.** Only at a site whose cache is megamorphic -- it gave
up (`maxCacheFills`), its cases are full, or it met what no case holds --
does a receiver of another shape have its own table searched and look in the
runtime's megamorphic cache (`abi.MegaCache`) before it leaves for Go, as
V8's megamorphic loads probe its stub cache: an entry, by the receiver's
shape and the key, names the receiver's own data property, or a prototype's
along a chain the code checks object by object and shape by shape
(`abi.MegaChain`), or that none of the prototypes has it, the read's value
undefined (`abi.MegaAbsent`). Go fills an entry at a read's exit, and
empties the cache once a collection has ended (`flush`, at the next
back-edge check), as V8 clears its stub caches after every full
collection: an entry holds its shape, and the shape its prototype, whose
methods hold all they capture. A read
whose receiver's prototypes are no longer its case's -- a method added to
one -- looks there too. So does a megamorphic site's write of a property
the receiver has not: an entry may name an addition, as V8's stub cache
holds a store's transition -- none of the receiver's prototypes has the key,
the chain checked as an absent read's is, and the receiver extensible with
room in its table takes the property at its table's end and the entry's
next shape (`abi.MegaEntry`'s `Next`), as the VM's appendTransition adds it
(mir's `megaAdd`), the shape stored through the barrier's routine while the
collector marks. Go fills such an entry when it makes an addition along a
shared transition for native code (the VM's `jitMegaFillAdd`): a loop
adding `o.y` to objects `Object.create` makes of a prototype each call of
its function makes anew, which had had Go make every addition. A
megamorphic site's write that neither the receiver's own table nor the
megamorphic cache has -- an addition it has no entry for, a shape it never
met -- is a call of Go (`ssa.GoCall`) on a branch the builder makes
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
whose frame Go cannot make, Go reads a miss only where that needs no frame
(`abi.GoCallState`'s `Inline`, the VM's `jitPlainRead`): a data property
an ordinary object or its prototypes have, or none; a getter, and a
receiver that is no object, leave there, and a callee whose megamorphic
read has met a getter is not inlined (see [Inlining](#inlining)). Each
such miss had left native code. A megamorphic site's write of
such a receiver is a miss so too: the write's object none, Go writes the
property of the primitive on the call's branch -- a setter on its
prototypes called with it as this, nothing in sloppy code otherwise, a
TypeError in strict code, which leaves at the call -- and native code goes
on after it. A write learns that it met a primitive where the interpreter
makes it, at a deoptimization or at a host exit, and the code is compiled
again for it (the VM's `jitPolySite`'s prim): before, each such write left
native code with no entry after it, and the interpreter ran the rest of the
invocation.

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
show the accessor as the interpreter's do. A register read or write calls
an accessor it does not inline -- one past inlining's bounds, a getter's
loop -- natively through its function's link (`AccessorSite`'s `Call`,
`jitRegFeedback`'s `accessorCall`), as any register call is made: the
read or write has no window of its own registers, so the call makes its
callee's past the VM's stack's top, as every register call's window is
made there -- the function in its register 0 where the callee reads it,
the receiver in 1, a setter's value in 2 (`mir`'s `windowAccessor`), the
rest as a method call's (`windowSetup`, `windowArgs`) -- on both backends.
Go finds a level's function, one that reads none, in the call's state
(`windowCallees`), and finishes the read or the write with the callee's
answer where the callee leaves (`regFinishCall`). While the accessor's
link has no code, the call is Go's from native code, which asks for code
native callers may call (`jitRegAccessorSeen`, `jitRegKnownCallee`), as a
call of Go asks for its callee's. One that is not register code of the
realm -- a built-in's, a bound function's -- is a call of Go on the
register frame (`jitRegGoCallOn`, `jitRegPropertyMade`). A read at a megamorphic site
whose misses Go reads from native code (see Megamorphic reads above) is no
accessor's (`ssa`'s `megamorphicByGo`): Go calls the getter at the miss,
as V8's megamorphic loads call the runtime for an accessor they meet.
Compiled for the one getter, the read had checked every receiver against
the shapes the getter was found for, and the rest -- most, at such a site
-- had left native code there.

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

**Keyed stores.** `o[k] = v` (`ir.ArrayWrite`) and a computed key's define
-- `{ [k]: v }`, a class's field, register code's `define_index`, a whole
host of three operands -- are compiled from their site's keyed store cache
(the VM's reg_table_keyed.go, `ssa.KeyedStoreSite`), which remembers for
each pair of a shape and a key it met the slot written or the transition
taken, as V8's keyed store inline caches remember names and maps. The
builder makes a branch for each key (`keyedStores`, `split`): whether the
key is a string the VM's table of key strings names that key's atom by
(`ssa.OpKeyIs`, looked for as `keyedRead` looks, never exiting), and on its
true edge the key's write, an `OpPropWrite` of the key's own slots and
additions exactly as a named write's site makes it, its map checks, its
transitions along the chain the cache found (none for a define, its
additions marked `Define`), its barrier and its room. A receiver none of the
key's shapes deoptimizes, and so does a string the table names an atom by
that none of the keys is (`OpKeyIs`'s `KeyIsAny`), the interpreter's store
teaching the cache the new pair; once the site has deoptimized (`Generic`)
such a receiver exits to Go, which stores and goes on natively, and such a
key goes the way any other key goes -- a string the table does not know,
made anew for each store, or no string: an element's store, Go's keyed
write (`ssa.GoKeyedWrite`), or the define's call of Go. A site whose cache
gave up (`maxCacheFills`) is compiled as before.

**The in operator.** `k in o` (`ir.HasProperty`) is a call of Go from native
code (`ssa.GoHas`), as V8's code calls its HasProperty builtin: Go searches
the object and its chain as the interpreter does, where that runs nothing
and throws nothing. A proxy on the chain, whose has trap would run, a module
namespace, a key that is an object, whose conversion calls its `toString` or
`Symbol.toPrimitive`, and a right operand that is no object, which throws,
Go refuses: the code leaves for Go to make it there, and goes on after it.
Before it calls Go for a key string, native code (mir's `keyedHas`), like
the interpreter's handler, looks in the realm's cache of the operator's
answers (the VM's hasCache, `abi.HasCacheWays`), which Go fills. The cache
is slotted by the string's and the layout's addresses, as V8's stub caches
are slotted by maps and names, four entries to a set, the newest first: up
to four keys of one layout never displace each other, wherever their
addresses fall, and a fifth takes the place of the oldest.

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
number, an array's `push` of any number of values and its `pop` on their
fast paths -- an array with no room for the values, one or several, grown
by `push`'s leaf (below), native code going on after it (`a7abd0b4`) --
and `new Array()` or `new Array(n)` from a pool. Strings' and arrays' `indexOf`, `lastIndexOf`
and `includes` are a call of Go of their own (`ssa.GoIndexOf`), which
answers where the built-in runs nothing. Anything else such a call meets is
Go's.

**Roundings.** `Math.floor`, `Math.ceil`, `Math.trunc` and `Math.round`
are instructions of their own (`ssa.OpFloorF64` ...) on every CPU: arm64's
FRINT, amd64's SSE4.1 ROUNDSD where the CPU has it (`abi.Encoding`'s
`Round`), and otherwise SSE2's instructions alone (mir's `roundSD`), as
V8's portable code and Go's `math.Trunc` round: a magnitude past 2^52 --
an integer already -- NaN and the infinities as they are; anything else
truncated to an int64 and converted back, which is exact, given the sign
of the operand, so that -0.5 truncates to -0, then a floor one less where
that is above the operand and a ceiling one more where it is below.
`Math.round` is its ceiling, less one where the ceiling less a half is
above the operand. `TestJITRoundings` pins all four against Go's math over
their edges with the instruction and without it.

**Leaves.** A built-in that runs no script where its check passes -- a leaf:
String's methods, Number's, a Map's or a Set's operations, `Date.now`,
`Object.prototype.hasOwnProperty` and `Array.isArray` of what is neither a
proxy nor a module namespace (`hasOwnProperty` of a primitive too, but
undefined or null, which throws), `Object.create` of a prototype alone, an
array's `push` off its fast path,
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
`isSafeInteger` with no call at all (`OpNumberIs`). `Object.create` of
one argument native code makes of a pool the call's site has
(`ssa.CreatePoolFeedback`, mir's `createMake`), as V8's code allocates the
object `CreateObjectWithoutProperties` would: of an object that is the
pool's prototype, the pool's next object, taken as a construction's
receiver is (`poolTake`, with the barrier while the collector marks), the
leaf's call made for any other -- which fills the pool, of another
prototype the pool that prototype's and empty, of the pool's, run out,
filled with objects made as the leaf makes them, but for the first such
call, whose object teaches the prototype's objects their room (the VM's
`jitCreateFill`); a site given other prototypes `maxCacheFills` times in a
row, none of them filled for, gives its pool up and calls the leaf alone
(`jitCreateForm`). A method call of the
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
(`ssa.GoArith`), in the root or in a callee inlined -- a pure body's
operator of an operand native code unboxed among them, once its
speculation has failed -- giving back what the interpreter would of the
root's operands' blocks (`ssa.CallSite`'s `Release`). A sum, a difference, a product, a
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
but the operator itself, whose result replaces it (`regBigFresh`, the
program's `Live`) -- not a register of those that hold their values alone,
whose old values only their own stores give back. Those that do
(`BigIntSole`, `regSoleRegisters`) are the bindings -- the registers past
the parameters and before the temporaries (`RegCode`'s `Inits`) -- that no
closure captures, read only as operators' operands, appends, tests and
branches: a move, a call's window, a return, a global's store, a property's
read or a postfix update of one has it hold its value no more, and an
instruction the analysis does not know has the function none. The
temporaries are fresh: `t = t * x` of a temporary gives the old one's block
back as the product is made.

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
among them (the VM's `jitStoreBigElem`). That call is handed the site's
cell too, and sets the bit of the BigInt type it stored in (the VM's
`jitBigStoreSeen`), marking the code to be compiled again: a site that has
met BigInt64Arrays alone, or BigUint64Arrays alone, stores a BigInt's low
64 bits natively, as V8's keyed store stores a machine word
(`ssa.OpBigElemWrite` of `OpBigLow`, which a BigInt the code made
unboxed folds to its word); any other array or value leaves for Go, whose
exit sets the other type's bit in the cell (`jitTypedWriteSeen`) for the
code compiled again to tell them apart.

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
deoptimizer materializes an object -- register code's, `closure_w` writing
the call's first argument just before it (`jitRegFeedback`'s
`madeCallback`). An exit inside the callback has Go make
its frame past the loop's, finish it and go on with the loop after its
index; one that leaves too often has the callback called natively again.
Where every closure the code makes is such a callback, a native call's frame
gives its cells back to their pool as it returns (`ssa.Func`'s `CellsBack`),
and a scope's end gives it none fresh: nothing else can hold them, as V8's
escape analysis leaves a context only such closures read unmade. Code that
makes other closures gives them back too where the invocation made none of
them (`ssa.Func`'s `CellsKept`), decided as it returns by its context's
`CellsKept`: cleared where native code takes the cells from their pool
(mir's `cellsTake`), set by a closure native code makes of them
(`closureTake`) and by every path of Go's that has them (`jitCellsHeld`) --
the cells Go made, a closure or a scope's end Go makes on the frame, any
operation on the frame, the frame made of the code's level with its cells
opened on its window. Code that suspends, a generator's or an async
function's, gives none back; a function with a direct eval is no native
callee. EarleyBoyer's `deriv_trees` takes ten cells at every entry and
makes its closure on one path: the JIT had allocated a quarter more objects
than the interpreter on Earley, 2% more since.

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

A `sort` of a typed array of a number type, or of an `Array` of numbers,
by a comparator native code may call again at will -- one whose code reads
only its parameters and constants that are numbers, computes, compares,
branches forward and returns (`jitRegSortComparator`) -- is a merge sort
of the code's own, the comparator inlined, over a copy Go makes and writes
back after the last comparison (`ssa`'s `sortCall`). Any exit inside it has
Go sort the array again from the first, which nothing the comparator did
can show; a comparator that cannot be inlined leaves the call to Go.

### Allocation and escape analysis

**Allocation.** Object, array and regular expression literals, closures and
their captured cells (see Closures), and the receivers of constructions, are
taken from pools of objects the VM makes ahead (`abi.ObjectPool`), as V8's
code allocates inline; Go refills a pool that runs out. `new Array(n)`'s
pools hold arrays by size class (`abi.ObjectPool`'s `Room`, the VM's
`jitArrayRoom`): up to `abi.PoolSmallRoom` (16) elements, the sizes an
array's own allocation has room for, its array taken for any n up to the
room; past it a power of two, the elements a slice of their own, its array
taken for an n of more than half the room. An n the pool's room does not
fit -- longer, or at most half a room past 16 -- has Go fill the pool again
with arrays of n's class (`Want`, 1 plus n), and the construction is made
again: a pool's arrays have at most twice the room their lengths need. A
filling past 16 makes as many arrays as 64 KiB of elements holds, at least
one, and a pool whose class changes is filled with one array, more as it
runs out, so that a site whose lengths change class at every call is given
an array a call, not a filling. Up to `abi.PoolRoomLimit` (65,536)
elements, the longest the Array constructor makes with its holes in dense
storage, every `new Array(n)` is taken from a pool; a longer n, whose array
is its length alone, is Go's construction, called from native code.

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
alone. Another phi's use of one is at the end of the predecessor it comes
from (`liveAt`): a register loop's end, which the copy of its test before
the loop reaches too, merges the object the loop began with from that way
alone, which no turn of the loop is on. A register loop's header is its
body's first block, where an object the turn makes is taken anew past the
header's phis: one taken in the merge's own block is used past it only by
another phi there (`liveAt`), every other path making it again first. And a
construction's receiver register, `new_w`'s R[A+1], is the construction's
to write, read by nothing, as a plain call's is (`RegAccess`, `liveRegs`):
were it live, it had carried the last turn's object round the loop to the
function's entry, which loads it, and no such object could be left unmade.
A use that needs an object made -- a call's operand, a store into an
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

Register code's are answered by `closure_w`'s register pc
(`jitRegFeedback`'s `CalledClosure`): the template of the maker's constant
the instruction names, what it captures by the register code's `Caps` -- a
register of the maker's, or one of its own captured bindings -- and its
function lowered for an inlined frame (`madeInline`), called plainly, its
window the call's. One that keeps its arguments only to read them by index
and count them (`jitRegReadsArguments`) reads the call's (`InlineSite`'s
`ReadsArguments`), as an inlined call's callee does; any other that keeps
them is made and called. An exit inside the inlined closure that teaches
nothing counts against inlining it at its call, as at an inlined call's
(`jitInlineLeft`): past `jitInlineExits` the call's shape is no longer given
(`CallShape`'s `notInline`), and the closure is made and called natively --
one whose reads past the call's count are Go's had left native code at every
call. An arrow that reads its this reads the maker's register 1, as
`closure_w` gives it the frame's (`InlineSite`'s `LexThis`), but in a
derived constructor, whose this may not be bound yet. A scope's end that
closes a register it captures (`close_regs`, lowered to `ir.CloseUpvalues`
of its registers) has the build inline no call of the closure past it. A
call is found to call the closure by its window's register 0 in the call's
state (`calledClosures`). That register is a copy of the closure's (`move
r5, r4; call_w r5 = r5(1)`), which an entry after an operation Go ran before
the call -- a call before it whose inlined callee left to Go -- loads: such
a load is the closure too where every state of that operation names one of
its values in the register, which the operation does not write, Go having
made the frame of one of them, the closure of its template (`resumeCopies`),
as an entry's load of the closure's own register is. One of a callee given
it by its window's function, copied just before from a parameter's register
(`givenCall`), so that a register callee calling what it is given is
inlined with the closure in it (`InlineCallsGiven`). At an exit, Go makes
each unmade closure on the maker's frame as `closure_w` would, its captured
registers' cells open on the frame by then, and gives it to every register
and inlined window naming its template (`jitRegMadeIn`); an inlined level
named by a template is the closure its caller's call's window holds -- or
the loop's callback slot, for a callback inlined in a loop (`jitRegInlined`).

### Closures and cells

A closure holds its function, captured bindings, shared template
(`closureCode`) and realm. The template owns the module environment,
bound while its tree of constants is prepared; every closure made from
it uses that same environment. Native calls and generator entries read
the environment through the template and the realm from the closure
when checking their target.

Code whose locals an inner closure captures keeps them in cells of its own
(`ir.Program`'s `Cells`), as V8 allocates a function's context: the
context's `Cells`, each an upvalue holding its value, which native code
reads and assigns through it, and frame states leave out. On a register
frame the k-th captured register's cell is the k-th cell: on the run's own
frame the cells are open on the window's registers, the upvalues its
closures share, which the frame closes as it ends; a native callee's are
closed cells holding the registers' values, which Go opens on its window
where it makes the callee's frame (`jitRegReopenCells`). A native callee's
entry takes its cells natively, a block from the function's pool
(`CellsPool`, mir's `cellsTake`): the block's pointers and its cells in one
allocation (`jitNewCells`), of up to sixteen cells (`jitCellsMax`) --
EarleyBoyer's `deriv_trees` captures ten, which had been Go's at every
entry, 12% of Earley's calls of Go on a native frame; a function capturing
more has Go make them on the frame. A closure is made
of them as the interpreter's `regClosure` makes one, and a scope's end
(`close_regs`, `ir.CloseUpvalues` with `Key` its first register and
`Extra` their count) gives its registers fresh cells holding what the old
ones held (`ssa.GoClose`, its `CallSite`'s `Closes` the count): natively
from the function's pool of cells (mir's `cellsClose`), the old cells
closed, on the run's own frame and a
native callee's alike. A pool of closures or of cells that has run out,
at a closure, an entry or a scope's end, is filled again by a call of Go
that finds no frame (`ssa.GoRefill`, mir's `poolRefill`), as V8's code
calls its runtime when its space runs out, and the take is made again
from its start, every check again, the collector perhaps marking since;
each refill had been an operation on the frame that made the object too,
finding the native callee's level -- 58,000 in three runs of AWFY. Only
where Go cannot fill it, and for what native code does not make, does Go
make them on the frame (`jitRegCellsClosure`), which closes a scope's
registers alone.

An arrow made from its pool takes its maker's register 1 as its this
(`LexThisReg`), an arrow's maker too. A maker that shares a derived
constructor's this binding (`funcExtra`'s `lexThisRef`) runs natively only
once the binding is bound (`jitRegThisBound`), its register 1 made the
value bound: bound, the binding is fixed -- a second `super()` throws --
so capturing its value is exact. Such an arrow's own `this_bound` and
`check_this_init` read its register 1, checked bound, Go throwing the
`ReferenceError` where it is not: wherever its code runs, register 1 is
the value bound -- Go enters it so, and a native call of it, which no
site's cache holds and the call learns from the function it called
(`jitRegSharedArrow`), gives it the value once the binding is bound
(`mir`'s `arrowBound`) -- as V8's code checks the hole of the this its
context holds.

A call of a closure that captures bindings checks the function the callee
runs, in its environment and realm, not the object (`ssa.CallSite`'s
`ByCode`), and gives the callee the closure it finds, as V8 checks a call's
code: a callback made anew at each call of its maker is a new object, the
same function. A call of a closure of one of the caller's own templates --
one its code makes, anew at each invocation -- in a caller that recurses
(a call cache of its own code has met it) is checked by its code from the
first compile (`jitRegMadeBy`), as V8's call feedback holds a closure's
feedback cell and its graph knows a call's target the code made by its
`SharedFunctionInfo`: checked by its object, it met another at every
level but the one learned, a recursion leaving native code at every
level. Outside a recursion an invocation's own run of the code calls the
closure it made, which a check of its object holds for, and which
inlining needs. A closure is inlined where its call checks its object, which
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
an entry of its own, as V8's code goes on at a handler. A throw from a call
of Go the code's own frame makes -- a call Go makes, a property read or
write, an instruction run whole, the frame's or a native callee's -- never
leaves native code where the handler in force at the call takes it: the
call's description has the offset of its continuation there
(`abi.GoCallState`'s `Catch`, mir's `catchStub`), Go puts what was thrown
in the call's keep cell and reports that it did not make the call, with
`GoStatus` `abi.GoCaught` (`jitRegCaughtValue`) -- the status is asked only
of a call Go did not make, so that the calls it makes pay nothing for the
continuation -- and the call goes on at the continuation, which
writes the frame as an exit at the call's state would (`exitThen`, its
records applied natively), gives the handler's register what was thrown,
its pointer word with Go's write barrier while the collector marks, and
jumps to the handler's entry, which reads the frame's registers -- on both
backends, as V8's optimized code goes on at the handler its handler table
names for a call that throws. Code that leaves an object or a closure
unmade (escape analysis, `calledClosures`), code whose captured registers
are in cells of their own (`ir.Program`'s `Cells`), typed code, and a call
inside an inlined callee have no continuation, and leave at the call as before; so
does a call Go would otherwise leave at -- the runtime stopped, the code
invalid, a compile due, a debugger. Such a call, its cost the throw's,
which the interpreter pays alike, counts in no judgment of the code's
profit (`jitSSAStats`' `caught`, `jitRegRunGoProfit`'s calls): counted, a
loop whose every turn's read threw was judged slow and left to the
interpreter. A read of undefined or null where a handler is in force is
Go's from native code, as a primitive's is (`jitPropOp`'s `caught`), its
TypeError caught so. A finally clause's
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

**A for...in's start** over an ordinary object whose keys are its layout's
is native code's too (mir's `forInStart`), as V8's `ForInPrepare` takes the
receiver's map's enum cache: the object of class Object with no elements,
its prototype its layout's, its layout shared and its keys made already --
kept on the layout's side by the first loop that walked an object of it
(the VM's `shapeKeys`, `abi.Encoding`'s `SideForIn`) -- and its prototypes
none, or a chain the realm found no enumerable key on, its validity cell
valid (the realm's `forInChains`), each prototype with no elements, which
the cell does not see. The cursor is the runtime's last (`forInLast`), done,
taken again as the VM's `newForInObject` takes it: not done, at its first
key, walking the object's keys, its object and its layout, written to the
call's keep cell; its pointer words go through Go's write barrier while the
collector marks. Anything else -- another object, a cursor still in use, a
layout no loop has walked -- is the call of Go it had been
(`jitForInPlain`), which makes the keys a layout keeps. The start had been
that call at every loop, a native call's round trip of Go: for_in_break
took 18 ns a loop with the JIT, 15.5 without, 4.6 with it now.

**Iterators written in script.** Register code's step whose cursor has met
an iterator whose `next` is written in script calls that `next` natively
(ssa's `userStep`), as any native call of a function is made: the step's
feedback is a native call site of the `next`, learned at the call of Go
that stepped it (the VM's `jitRegStepNextSeen`, `regStepSites`), and the
`next` is compiled for native callers then. The step checks its cursor
(`ssa.OpCursorCell`: not done, no array's, no for...in's, its `next` that
function), and on a branch of its own, where it is not, Go steps it as
above (`split`); where it is, the call is made of the cursor's iterator,
its receiver, and its `next`, its window no register's (`InlineCache`'s
`Consumed` none), and its result is answered by a call of Go of the
iterator protocol (`ssa.CallSite`'s `StepResult`, `abi.GoCallStepResult`),
which native code makes itself of a result of the layout the step learned
whose `done` is false or true (mir's `stepResult`): its `value`, or, done,
the uninitialized value and the cursor's state marked done. A getter, a
`done` no boolean, a result no object are Go's, which throws as the
interpreter does. Where the `next` leaves native code, Go finishes it and
the step from what it returned (`jitGoPending`'s `step`,
`jitRegStepCalled`), never calling it again: native code where the code
the call returns to is valid, Go's protocol on the result otherwise; a
`next` that throws marks the cursor done, so that the loop's handler does
not close it.

**Generators' steps.** A for...of's or a destructuring's step over a
generator (`ssa.CallSite`'s `Gens`) resumes it by a native call, as V8's
code calls a generator's resumption: the step checks the cursor's generator
-- of a function it has met, suspended, not in a `yield*`, stepped by the
realm's `next` -- and jumps to the generator code's generator entry (mir's
`genEntry`), which makes the frame from the generator and goes on at the
entry for its place (`ssa.Func`'s `GenResumes`). A yield there writes the
frame, saves the generator's place and returns the value to the step
(`ssa.Block`'s `Yield`); a return finishes it and returns the step's done.
Anything else leaves as a native callee does, and Go finishes the
resumption from the generator. Only a synchronous generator without `yield*`
whose locals no closure captures is resumed so: one whose suspension left
captures of its registers (the VM's `genSaved`, `abi.Encoding`'s
`GenSaved`) is Go's.

A generator a loop's step may resume natively keeps its whole register file
in its values, the callee's and this's registers included (`regGenNative`,
the VM's `regCode.genFrom` 0; an async function's and a generator's Go
alone resumes keep them from register 2), and its native code runs on them
as its registers: its code's generator entry points the context's `Locals`
at the generator's values, copying none, makes room past the VM's stack's
top for what the code keeps past its registers -- its scratch slots and its
calls' windows, past them -- as a call entry makes a native callee's, and
writes the value sent in to the yield's register (`ssa.GenResume`'s
`Sent`), its pointer word through `storeRef` while the collector marks --
where the generator's place (its register pc, `Place`) has an entry, the
resumption by `next` going on at the jump's target after the yield, the
first past `end_params`. A yield (`ssa.Block`'s `YieldSlot`, its value's
register) writes the frame as an exit does, into the generator's values,
and saves its place, suspended and begun, as `regGenRun` does; the handlers
are the code's, by register pc, so nothing of them is saved. The room past
the VM's stack's top has a window of the registers' size at its base
(`abi.Context`'s `Base`): where the code leaves native code, Go copies the
values to it the first time it finds the level (`jitRegGenLevel`), points
the context's `Locals` there, and moves an exit's description there
(`jit.RebaseExitLocals`), so that Go reads and writes the frame where it
does any level's, and native code going on runs on the window; a yield then
copies the window back to the values, their pointer words through `storeRef`
while the collector marks (the copy's offset kept in the context's
`RecordAt`), where `Locals` is not the values. A generator resumed so is one
whose locals no closure captures and that keeps no arguments
(`jitRegGenCallable`): nothing is open on its registers. The step learns its
generators at the calls of Go that step them (`jitRegGenSeen`, from the
step's `iter_next` and a `call_method_w` of the realm's `next`). Its loop's
cursor, `for_of_start` Go runs whole, is taken natively from a generator's
room (`ssa.CallSite`'s `GenIter` on the whole call's site, mir's
`genForOf`); Go makes any other. A level of such a resumption that leaves
native code Go makes the frames of as `regGenRun` does -- next's below the
generator's, its this, function and handlers the generator's
(`jitRegPushLevel`) -- and finishes as `regGenRun` finishes a resumption
(`jitRegGenDone`): suspended, the window written back and the place saved,
the step's result the value or, for a call of `next`, its result object;
ended, the generator done and the step's cursor too, the step going on at
its loop's end (`regFinishCall`).

**Destructuring.** Destructuring from an iterator takes a cursor, as a
for...of does: Go makes it, called from native code (`abi.GoCallForOf`).
Each element is a step that goes on with undefined once the iterator is done
(`ir.IterStep`'s `When`, `abi.GoCallFill`), native over an array walked in
place, a native resumption of a generator the step has met (learned at the
step Go made, as a loop's step is, `jitRegGenStepSeen`), and Go's
otherwise. Its end closes the iterator natively where there is nothing to
close -- the cursor done, or an array's -- and by Go where a return method
must be called (`ssa.OpIterClosed`): by a call of Go native code goes on
after (`ssa.GoIterClose`, through `goLive`) where the close runs no script
-- a generator the destructuring left suspended at a yield whose return
path is a return alone, its return method the realm's -- and after an exit
otherwise, which runs its finally clause, the loop it is in, or a return
method of its own. A for...of's break closes it so too. What is left, for a rest element, is
Go's. A for...in loop left early -- a break, a continue of an outer loop, a
return -- marks its cursor done natively (`ir.IterClose`'s `When`,
`ssa.OpForInDone`), as the interpreter's `iter_close` does, so that the
runtime's next loop takes that cursor rather than making one (the VM's
`newForInObject`).

## The backend

`mir` selects instructions, allocates registers by linear scan and encodes
amd64 or arm64 machine code (`CompileWith`, the target's; `CompileAMD64` and
`CompileARM64` make either on any machine). The two backends share one core
(`core.go`) and move together: a change to how the JIT compiles lands in
amd64's and arm64's in the same commit.

**Register allocation.** `mir`'s scan (`allocate`) gives each value an
interval from its definition to its last use, stretched over every block it
is live in or out of. Which blocks those are is a backward dataflow over the
blocks of the values used in another block than their own (`blockLiveness`),
each block's set an `ir.LiveBuilder` persistent set, as the slot liveness's
are, and an interval's ends are read along the layout, each block's sets
against the one before: what the sets change from block to block, not what
they hold. A walk of each value's blocks back from its uses had taken as
many steps as values are live in each block, summed over the blocks -- n
values live across a loop of 2n blocks 2n² steps, 40 s of a compile of
16,000. A frame state's slot is a use, which keeps the value
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
frequency over their length; of those alike, one it splits (below), which
loads nothing back, and then the one that ends last. A
budget loop's register goes first, its loop then counting in memory. A phi's
edge move to a slot its phi or argument already holds, and may share, costs
nothing, and so is taken off the cost of that value in a slot (`inSlot`):
spilled together, the two share the slot and the edge moves nothing.
Beyond that hint, a group's spilled members share one slot (`gslot`): its
first spilled member takes a slot for the group, which every later one
spilled takes too -- the members' intervals meet none of each other's, so
the slot holds each in turn. Freed at a member's end while the group has
members to come, the slot is held for them, until the group's last member
ends. Another value takes it only where no slot of the free list may hold
it, only if its interval ends before the group's next member begins (the
slot's gap) -- so that the group keeps it (`lends`) -- and not if it is a
member of another group, which is to keep the slot it takes. The held
slots are in a heap by gap, the greatest first: where its gap is too
short, every other one's is; found by a walk of every held slot, each
checked against its group's members, they had taken zlib's compiles ten
times as long to allocate. An edge's move between
two spilled members is then none. Given only a register's coalescing and
the hint, a large function's spilled phis had each been a slot of its own
and its edges copied them slot to slot: Octane zlib's largest asm.js
function had 2.4 MB of its 3 MB of code at its edges. Given to any value
where the free list had none, the groups lost their slots where most
values were spilled, and zlib's edges moved nearly as much as without the
sharing. A function takes more slots so where many values are live at
once and few slots are free between a group's members -- 199 to 269 in
one of `TestSSANativeHighPressure`'s random programs -- and those past
the context's own are in its `SpillMore` (below), which those tests give
their contexts as the vm does. Spilled, an interval is in its slot whole:
stored where it is made, loaded at each use.

A slot asks less of its values than a register does: only that no two
are live at once, where an interval spans every position from a value's
first to its last. So a phi and its arguments are also joined into spill
classes (`spillClasses`) of every pair whose values are never live at once,
their intervals meeting or not, as V8's allocator merges the spill ranges
of a phi and its inputs where their use intervals, holes and all, do not
meet. Repeated inputs of one phi contribute every edge's use and cost,
but only their first coalescing pair: a successful pair is already joined,
and an interfering pair cannot become disjoint as its classes grow. The
member's pool offset holds the last phi paired with it until the groups
are filled, so recognizing repeats needs no additional table.
A value of a class of more than one is spilled to the class's own
slot, which no other value takes, and the group's slot and the free list
are for the rest. Two values of strict SSA are live at once only if one's
definition dominates the other's and it is live there (Budimlić et al.):
two classes are joined where, their values merged in the order of the
dominator tree's walk (`ssa.Func`'s `DomOrder`) and walked with a stack of
the values that dominate the one met, no value is live at the definition
of the nearest of the other class's above it (Boissinot et al.'s check) --
one further up would be live at that one's too, which its own class
proved not. A value is live at a position of block b where it is live
into b (the block liveness's sets) or made in b before it, and live out of
b or used in b after it (`xuse`'s last position in b); a phi's position
is its block's start, where the other phis of the block are taken as live.
A large function's values of one variable meet in their intervals -- one
live on a loop's exit, another on the path past it -- where they are never
live at once: Octane zlib's largest function, 47 loops entered by on-stack
replacement and some 240 values live through each, had its phis each in a
slot of its own though the groups shared what they could, and its edges
copied them slot to slot, 70% of its 1.8 MB of code; with the classes, its
code is 440 KB. Nothing of the classes is made until the scan first
spills a value of a pair. Then the pairs' components -- the values one
value's pairs reach -- are found by union-find, and one walk of all their
values in the dominator tree's order, bucketed by block with no sort,
checks every component at once, each against the top of its own stack:
most components (nine in ten, in Octane) have no two values live at once,
and such a component is one class, which every merge would have made. Only
a component with two values live at once has its pairs merged in their
order, as above, so its classes are the ones all the pairs make. Made
for every pair up front, the classes had made the allocator's hints four
to six times as long in Octane's Mandreel and zlib; made so, hints and the
scan take 10 to 23% longer than with no classes, the price of zlib's
code, which runs in 38% less time with them.

**Spill slots.** Nothing bounds how many a function takes: the free slots'
lists grow, doubling, as the slots taken pass them, and the code says how
many it uses (`Code`'s `Spills`). The context holds the first
`abi.SpillSlots` (256) itself (`Spill`), each at a displacement from the
context register; the rest are in its `SpillMore`, a `[]uint64`, slot n at
`SpillMore[n-256]`, which the code reaches through the slice's address,
loaded at each access (`spillLoad`, `spillStore`). arm64 loads it into the
encoder's temporary, X27, and reads or writes the slot through it, adding
the offset in steps first where it is past a scaled load's reach. amd64
has no register to spare: a word loaded into a general register is loaded
through that register; a store, or a double's load or store, goes through a
scratch register other than what it stores, kept meanwhile in the
context's `SpillTemp`, since any register may be live where code reads or
writes a slot. Code of no more than 256 slots has none of this, its code as
it was. Go gives every context room for the most slots any code it placed
uses before that code can run (the VM's `spillsFor`), and a context made
later the same (`jitGrowContexts`), keeping the words a context's slots
hold, which native code suspended in a call of Go may have spilled: no
entry, call or callee checks the room, and native code may call the code at
any level. The room of the first chunk's contexts counts against the JIT's
budget, which nothing takes back while the runtime lives. An exit's description, a call's state and a
call of Go's operands name a slot by its number as before (`ExitSpill`,
`abi.SpillOffset`), and Go reads it where it is (`Context.SpillAt`,
`Context.Word`). Its words are raw, as `Spill`'s are: a reference spilled
is kept where the collector sees it too (see [Calls of Go](#calls-of-go)).
The VM places such code from register programs.

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

**Branch reach.** A function's code may be of any length, and every branch
in it reaches its target. amd64's jumps take a rel32, which reaches 2 GB
either way: the assembler shortens those that fit a byte, and refuses code
past 2 GB, which no budget holds. arm64's reach less: a B 128 MB, a B.cond,
CBZ or CBNZ 1 MB, an ADR 1 MB. Its assembler keeps them in reach as V8's
does, by veneer pools (`internal/jit/asm/arm64`'s `pool`). A branch back to
a bound label past its reach is made of ones that reach: a conditional
branch is its opposite over a B, a B past 128 MB a jump through X27, the
target's address made by an ADR and subtractions. A branch forward to a
label not yet bound joins a queue, its kind's; once the oldest has come
half its reach, a pool follows the next instruction: a B over it, then a
veneer -- a B to the label -- for every branch of the queue that has come a
quarter of its reach, which the branch is pointed at instead, one veneer for
all of the pool's branches to a label. A veneer is a B in the queue of Bs,
taken by a later pool once it has come 32 MB. A pool between two of the
backend's instructions is a jump over it, which changes no register or
flag; it never comes within a run that must stay together (data, the
assembler's own sequences), and the backend never jumps by a count of
instructions nor keeps a value in X27 at a branch's target. A branch whose
label is bound before a pool takes it stays as it is: code under 256 KB has
no pool, and is the same as without them. An ADR back past its reach is
an ADR and subtractions, as the far jump's address is. Forward, an `Adr`'s
address is its result, which a veneer cannot stand for: the backend's are
of return points a few instructions on, which no pool takes, and one past
its reach is an error of `Finish`. Two kinds of ADR the regular
expressions' code generator makes are kept in reach all the same: an
`AdrJump`, the address of a resume point, which the code only jumps to, is
pointed at a veneer as a branch is; and an `Adr` of a `Table` -- data the
assembler places itself, a character class's bitmap -- has the table placed
in the pool that would have taken it, as V8's constant pools are, or after
the code (`Tables`) where no pool came first. So a pattern's code of any
length assembles too.

**Workspaces.** Compiles reuse their workspace's analysis tables and
assembler storage (`jit.Workspace`, the SSA's and `mir`'s). SSA's prototype
holder chains, inlined frame states and accessor sites are copied directly
into their arenas. Their helpers allocate separate storage only for builds
without a workspace; returning the address of an input parameter would
make Go allocate that input even on the arena path. `TestWorkspaceMetadata`
checks that repeated requests allocate no heap objects after warm-up.
The register feedback's inlining cache likewise allocates an `InlineSite`
only for a successful answer; a rejected inline keeps just its asked bit.
`TestJITRejectedInlineAllocations` checks that rejected probes allocate no
site once the answer table has room.

A workspace keeps what the compiles of the collector's current and last
cycle needed, as `sync.Pool` keeps what was put in it for a cycle more.
Each arena keeps chunks for the most any of those compiles reached
(`arena.RewindAt`), and `mir`'s scratch tables -- the assemblers', the
move schedule's, the exits' -- go once they hold more than twice the most
any of them used. So a big compile's tables serve the next as big without
being made again, and go once a whole cycle has passed with none as big.
`jit.PutWorkspace` and `jit.ReturnWorkspace` stamp a workspace with the
pool's count of cycles as it is given back, and its rewind -- there, or on
the worker that takes one given back unrewound -- passes that. A pooled
workspace goes whole after two idle cycles, or at the return after a turn
that left it unused (`DropIdleWorkspaces`). Counted by jobs, the last
sixteen, a big compile's chunks had gone as it ended, and the next as big
made them again: half of the garbage Typescript's compiles made. The
builder makes an instruction's guards with a method of a value on the
stack (`ssa`'s `guarder`), not a closure it gives its helpers: a variadic
call through a func value puts its arguments on the heap. A compile's
lists are the workspace's too: the exits' descriptions, which its code
keeps, are copied out at their length, and the calls of Go whose refusals
branch to a label are scratch on amd64, as on arm64.

On arm64, the workspace also keeps the byte buffer the assembler's
instruction words are encoded into (`FinishInto`). A workspace compile's
bytes hold until its next compile;
placing the code copies them into the executable arena before the workspace
is returned. The buffer counts toward the workspace's memory and is trimmed
with its other scratch storage when recent compiles need less. Compiling
without a workspace returns independent bytes.
The arm64 table used to patch Go calls' failure branches is scratch too;
it drops its pointers to the calls' descriptions when the compile ends.

A compile in the background takes its workspace on its worker, where it
is built (or on the runtime's goroutine, where the compile is taken back),
and gives it back once nothing built in it is used: after its install has
placed the code and made its entry of it, or thrown the compile away. The
install gives it back unrewound (`jit.ReturnWorkspace`), and the worker
that takes it for its next compile rewinds it (`GetWorkspaceOnWorker`,
which takes such a workspace first; `GetWorkspace`, a compile on the spot's,
takes one rewound first): the rewind had been more than half of an
install's time on the runtime's goroutine, and handing it to a worker as a
task of its own -- the queue's lock, often a worker started -- a quarter of
what was left. A workspace given back unrewound is kept, aged and let go of
as one rewound is (the collector's last two cycles, a runtime's return to
its host).
`TestJITRegWorkspaceBackAfterPlace` holds the order, with the code placed by
the worker and by the install from the backend's bytes, arm64's buffer.

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

**Dead registers.** Native code loads the registers live at its entry and
writes the frame again only at its exits, whose records write the
registers live there; the collector scans the VM's stack whole, so what
the interpreter left in a register dead at the entry -- a call's window, a
temporary -- stayed reachable for as long as native code ran: a loop
entered at its header kept the array a method call before it had in its
window, 16 MB, past every growth of the next. Go writes undefined to the
registers dead at the entry's instruction before it enters a loop or goes
on after an exit (the VM's `jitRegClearDead`), as V8's stack slots dead at
a safepoint are no roots: not the function's or the receiver's register,
nor a parameter, which an arguments object may alias and legacy
reflection reads, nor a register a closure captures, nor any of a
suspending function's, whose generator keeps the frame. Native code never
reads such a register -- it loads none it was not entered with -- and the
interpreter writes one before it reads it, as liveness says. A live
register a loop reassigns, whose entry value's pointer word stays in its
slot (a source of the loop's phi), is not dead there; the loop's polls
reach it (below).

**Registers brought up to date at polls.** A live register a loop
reassigns keeps, in its slot, the reference it held when native code was
entered, which nothing the loop reads reaches once the register holds
another: array_push_1m's `a = []` kept the last turn's array there until
the loop ended. At a poll of a loop's back edge that Go answers with a new
budget and native code goes on after (`ssa.GoPoll`), Go writes the
header's state into the frame's registers where nothing native code still
reads is overwritten (`jit.Rehome`, the VM's `jitRegRehome`), as an exit to
the header would, and native code goes on with no exit. It does so at one
such poll in sixteen of the runtime's (`jitRehomeEvery`), so a stale
reference lasts at most sixteen budgets' run, some sixteen thousand back
edges -- tens of microseconds in a tight loop, far short of a collector's
cycle, which allocation paces. At every poll, some 150 ns each, it had
cost forEach's callback loop, a few nanoseconds a turn, 7%. The poll's call of
Go carries the header state's exit description (`abi.GoCallState`'s
`Rehome`) -- of its slots, those whose values may be references or take
one's place, a loop's counters left out, each marked as the compile
found it (mir's `findRehomes`) -- and the registers the call's routine
saved. Go finds every slot's source -- its shadow's word, or its
origin's slot where the reference is taken from there -- before it
writes any, and writes a frame register only where its value is not the
slot's own, no other value's reference is read from the slot, and the
compile did not pin it; it reads a source's words only for a register it
writes, a heap cell's being the poll's dearest read. A value native code holds whose source is a shadow phi is
*rewired*: once its register is written, the shadow's word -- in a saved
register, which the routine puts back, or a spill slot -- is made the
register's address, so that its reference is read from there and the cell
it came from, which may be in an object only the old slot held (a list
walked by `n = n.next`), is read no more; the shadow phis are what
`shadowMerges` made, read only as sources, so the word means the same to
every use. A rewired value whose register cannot be written, with a source
outside the frame's registers, the VM's stack and the context, writes
nothing at all. What the header's state does not name but is live at the
poll -- a value made before the loop, held in a register of the
allocation, an object's address or a cell read from it -- the compile
follows back to what holds its objects: a frame register it reads
through is pinned, never written; a value of the state, a keep cell, a
global's, a closure's or the runtime's cell ends the way; and a phi the
state does not name, any turn's value at run time, leaves the header's
polls with nothing to write. The same polls empty the keep cells no value
live there reads (`abi.Rehome`'s `DeadKeeps`, mir's `rehomeDeadKeeps`):
of the cells the allocation gave (`colorKeeps`), those that no value the
state does not name is followed back to -- the follow above, which
reaches every such value's sources or leaves no plan -- and, at run time
(`jit.Rehome`), that no slot of the state takes its reference from: a
loop-carried result not made yet may name, through its shadow phi, a
cell another keep was given before its own write. A call's result made
before the loop and dropped -- `a = f()`, then `a = null` -- had kept its
object in its cell for as long as the loop ran, once no exit at the
loop's pushes ended the run. A header whose state has no register to
write -- a loop of numbers -- has polls that do this alone, where the code
has keep cells: `consume(big())` before such a loop had kept big's array
in its call's cell to the loop's end. Not where the code makes loops of
built-ins' calls, whose loops keep cells of their own. Not inlined
callees' frames, typed code, a generator's code, nor a native callee's
frame. Go's stores, with Go's barrier: native code reads each reference
through its source after it as before, the same value, and does the same
work whether the collector marks or not.

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
| `Spill`, `SpillMore`, `SpillTemp`, `Keep`, `Regs`, `XRegs` | Spill slots (`abi.SpillSlots`, 256, and those past them, see Spill slots), the references the code keeps where the collector sees them (`abi.MaxKeeps`, 256), and the registers a shared routine saves. |
| `BackEdges` | The interpreter's back-edge counter, which every back edge decrements. |
| `GoOp`, `GoArgs` and the call's words | A call of Go: the operation, its arguments' count and kind (see [Calls of Go](#calls-of-go)). |
| `Next`, `Prev`, `Live`, `Level`, `Base`, `Closure`, `Code`, `Function`, `NewTarget`, `ReturnTo`, `Args` | Native calls: the callee's context, which runs a call's callee or describes an inlined frame (`LiveCall`, `LiveInline`), its window, closure, the code it runs and its function object, where it returns to, the arguments its frame keeps. |
| `StackBase`, `StackEnd`, `StackTop`, `StackHigh`, `HighSeen` | The VM's stack, for the room a callee's frame needs. |
| `Global`, `GlobalLex`, `LexNames`, `GlobalEpoch` | A script's global and lexical bindings, and the count of changes to the global object's table. |
| `AwaitJob`, `JobHead`, `JobCount` | Whether an async function's frame is one its await's job resumed, and the VM's job queue's next index and length, which an await that goes on in place reads (`ssa.OpAwaitReady`). |
| `TypedEpoch`, `Mega`, `Hints`, `Loop`, `Shade`, `Bounds`, `TailFrame` | The typed arrays' epoch, the megamorphic cache, the hint table, an array loop's index, the marking's shading cell, the levels' limits (`abi.Bounds`' `LevelLimit`, `TailLevel`), and a frame a tail call gives up. |

The contexts are the runtime's, a native call's callee in the next, as V8's
optimized frames are on the machine stack: 4 at first, and a chunk of as
many more as there are each time a call finds none past its own, some 17 KB
a context. A chunk is allocated apart and linked to the last (`Next`,
`Prev`), and no context ever moves: native code, its callers' `ReturnTo`
and `Prev`, and the levels Go pins and puts off hold their addresses. They
grow as deep as the VM's frames may go (`Config.MaxCallDepth`, the VM's
`jitContextsMost`), and `Bounds`' `LevelLimit` stops a call at the frames'
limit as well as at the last context: a call past the last context is a
call of Go, which grows them for the calls after it, and one past the
frames' limit is a call of Go too, which throws the interpreter's
RangeError at the interpreter's depth. The first chunk counts against the
JIT's budget; the rest are the native levels' stack, which the depth limit
bounds, as V8's stack is not of its code space. They are given back as Go
shrinks a goroutine's stack: at each of the cache's flush ticks
(`jitShrinkContexts`), the chunks past every context in use -- below the
VM's `ctxTop`, which every run and call of Go moves past the levels it is
under, or `Live` -- and past every one native code has entered since the
last tick, which a frame's `Locals` set at its entry marks and the tick
clears, are dropped. The collector scans them as Go memory: a chunk made
while it marks is allocated marked, and every pointer stored into it after
is stored with the write barrier, Go's or native code's.

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

**Routines of their own.** A call of Go that has a routine of its own
(`abi.Encoding`'s `GoStubs`, `abi.GoStub`) is made as V8's code calls a
builtin at its own entry with its descriptor's registers, rather than through
one entry that decodes the call: native code jumps to the operation's
routine (the VM's `jit_leaffast_*.s`) with what the call passes in that
operation's registers, and the routine calls the operation's own Go
(`jit_gostub.go`) and jumps straight back -- no description read, no
dispatch of every kind of call of Go (`jitGoDirect`, `jitHostCall`), and
none of `jitHostCall`'s frame, made for every case's locals at every call,
which had been the largest part of a small call's way to Go. Each routine
is, as `callGo` is, a function the Go function that entered the code
called: native code moves no SP and makes no call instruction, and what the
routine calls is ordinary Go. It does what `jitHostCall` does before any
operation (`jitStubEnter`): a runtime made `WithGCYield` waits while the
collector marks, a test's work at each call of Go is done. The site saves
what lives across it, as a leaf's does, and stores a loop's budget its
register counts back around it. The Math functions Go computes -- pow and
a remainder of numbers where native code's exact paths do not answer
(`power`, `remainder`), and `ssa.OpMathCall`'s functions -- take their
operands in amd64's X0 and X1 or arm64's F0 and F1, which of `unaryMath`'s
functions in RCX or R17, and give their result in X0 or F0, as V8's code
calls its ieee754 functions: Go refuses none, so the call has no
description and no refusal. A power or a remainder is no call to the
allocation, which keeps what lives across it in registers, so its call --
out of line, where its exact paths fail -- has every allocatable register
saved around the routine (`stubSaving`), as `goSaving`'s are, but goes no
other way; a Math function's call (`OpMathCall`) saves only what lives
across it. The other routines take what `GoFast` takes -- the call's
description, which describes no operand, and where it goes on -- and their
operands in the context's `GoArgs`, where the call's code writes them, so
that a site differs from `GoFast`'s only in where it jumps (`goStub`): a
string's `+` Go makes (`GoAppend`, `GoAdd`, `GoConcat`), and the keyed
operations native code leaves to Go -- a property's delete, a string's or
an array's search, a read and a write by a key, the in operator
(`GoDelete`, `GoDeleteStrict`, `GoIndexOf`, `GoIndex`, `GoKeyedWrite`,
`GoHas`) -- and the calls Go makes without a frame where it can, a routine
for each kind the code knows (`GoCall`'s `GoCallSet`, `GoCallForOf`,
`GoCallRest`, `GoCallStep`, `GoCallForIn`), which makes with the frame
what it cannot without (`jitGoCall`), as `jitGoDirect` does. Go gives the
context what `ExpandGoCall` gives it but the operands (`jitStubCall`) and
makes the operation with the function `jitHostCall`'s case calls; what that
refuses goes on `Fail` bytes past, to the call's exit, as from `GoFast`.
A convention of registers for the operands, as V8's descriptors have them,
would change the sites and the routines' first instructions alone.

**A leaf through call.** A method call of a leaf through the realm's
`Function.prototype.call`, `G.call(x, ...)` -- `hasOwnProperty.call(o, k)`
-- is a leaf's call of G, as V8's call reducer reduces a call of `call`
whose target it knows: the code checks the method is `call` and its
receiver G -- either not, a host exit -- and calls G with x for its
receiver and the rest for its arguments through `LeafFast`, or makes G
itself where native code makes the leaf (ssa's `windowViaLeaf`,
`ViaLeafFeedback`). The call learns G at its plain call of Go, which
applies G without a frame (the VM's `jitPlainCall`, `jitViaLeafSeen`,
`jitLearned`'s `viaLeaves`), at the code's own level, its first G only;
one that also called a function of the script's natively -- through
`call` too, learned at its exits -- stays the general way.

**On the register frame.** A call Go makes for native code -- a call's site
that has no native target, a construction, a getter or setter not inlined --
Go makes on the run's register frame (`jitState`'s `regRuns`,
`jitRegGoCall`, `jitRegGoCallOn`), as the interpreter's slow path does
(`regCallSlow`, `regConstruct`): the function, the receiver and the
arguments put in a window of their own past the VM's stack's top -- never in
the frame's, whose registers the code may still hold values in as it found
them -- and the frame's pc past the call meanwhile. A built-in of the realm
with a native body -- not bound, not `Function.prototype.call`, not one of
Math's, not one the call's marks send to `regMarkedCall` -- is called
directly in a frame of its own (`jitRegBuiltin`, `jitRegNativeMade`):
`regCallSlow`'s search of the site's
cache and the generic call would be most of what a call of `JSON.parse` or
`Object.create` costs. A call that threw, that
the runtime stopped in, or after which the code was found invalid, its exit
finishes (`regPending`, `regFinishCall`): the answer to the call's register,
or the exception to the frame's handlers, never the call made again.

**What native code makes first.** Of an instruction run whole, native code
makes what its site's feedback has it make itself
(`jitRegFeedback`'s `NativeCalls` for a whole Host, `regWholeSite`): an
object's spread by the layouts Go has copied (`ssa.CallSite`'s `Copies`),
an array's spread by a dense array's elements (`ArrayIter`), an element
appended (`Append`), a literal's field along its shape's transition
(`DefineNext`), a computed key that is a string its own (`StringSelf`,
the builder's `wholeStringSelf`), a for...of's cursor from a generator's
room (`ForOf`); what it does not make, Go makes without the frame where
that runs nothing, ahead of a run's judgment of its calls of Go and with
no level's frame made for a native callee's (`jitRegPlainWhole`): the
operations `jitRegFrameless` makes as `jitOpFrameless` makes their stack
instructions, learning the layouts' copies -- an object pattern's rest, a
computed key converted or defined, a literal's `__proto__`; a method's home
object set, a method defined, a literal's or a class's getter or setter
defined, a class's field initializer given and a function named after its
key, of the closure's names -- and a for...in's cursor, a for...of's cursor
of an array -- the interpreter's last, where its loop or pattern closed it
(`arrCursor`), which native code's close gives back as `iter_close` does
(`mir`'s `iterRelease`) -- a loop's step over an array where it reads its
dense storage or is done, and a destructuring's rest of an array all in its
dense storage (`ssa.CallSite`'s `ForIn`, `ForOf`, `Step`, `abi.GoCallRest`:
`jitGoCallPlain`'s `jitForInPlain`, `jitForOfPlain`, `jitStepPlain`,
`jitRestPlain`); and on the frame otherwise. A class linked to its parent (`new_class`) and an object spread
that runs a getter Go makes on the operands with the frame (`jitRegOpMade`),
not run whole: neither writes a register, so nothing is saved and put back;
a throw goes to the frame's handlers as a call's does (`regFinishCall`).

**Loops' steps.** A for...of's or a for...in's step (`iter_next`, an
`ir.IterStep` native code does not make itself) is a call of Go of the
step (`ssa.CallSite`'s `Step`). Of a cursor over an iterator whose `next`
is no generator's -- the script's, a Map's or a Set's -- Go makes the step
itself: the `next` called as register code calls it, its result's `done` and
`value` read (`regStepNext`), by `jitRegStepMade`. Any other it runs as
the register instruction on the frame, as an instruction run whole is
(below, `jitRegWhole`): the cursor's register given the code's value. The
value it steps to is the call's result, or the uninitialized value where
the loop is done; what throws, or the runtime stopped, the exit at the
call finishes (`regPending`, `regFinishCall`).

**Instructions run whole.** A whole Host (`ir.Instruction`'s `Whole`) is
a call of Go (`ssa.CallSite`'s `Whole`, `abi.GoCallWhole`) that runs the
register instruction at its PC on the register frame, by the interpreter's
own handlers (`regWhole`), as V8's code calls a runtime function for what
it has no code of (`jitRegWhole`): the registers it reads are given the
values the code passed, its result, the register it writes, goes to the
call's keep cell, and what it wrote in the frame is put back -- the code
keeps values in the frame's registers as it found them -- but a captured
register of the run's own frame, where its cell holds it. In code whose
captured registers are in cells of their own, those cells are opened on
the window's registers first, each holding what it holds
(`jitRegReopenCells`), as Go has them before it runs the frame at an
exit: a cell native code gave a register at a scope's end (`cellsClose`)
is open on no register, which still holds what it held when native code
entered -- read there, every later turn's let of a loop was the turn's
the code entered at. On a native callee's frame its level's frames are
made first, as for any call of Go that runs script
(`jitRegGoCallNested`). What does not go on at the next instruction -- a
throw, a handler of the frame's that took one, the runtime stopped, the
code found invalid -- the exit at the call finishes
(`regPending`, `regFinishWhole`): the register the instruction wrote, or
the handler's, written over the exit's records, the instruction never run
again. Which instructions are whole Hosts the registers each reads and
writes say (compile's `RegAccess`): a using declaration's
`new_dispose_capability`, `add_disposable` and `dispose` among them,
whose disposal may run script, which would otherwise be host exits at every
turn of a loop with a using declaration. So is an arithmetic operator --
`+`, `-`, `*`, `/`, `%`, `**`, the bitwise ones, the comparisons `bin`
makes -- that met an object at an exit or a deoptimization (the VM's
`jitRegObjectArithSeen`, compile's `Hints`' `WholeOps`): its conversion
(ToPrimitive, a `valueOf`) may run script, so the lowering makes it a whole
Host from then on, as V8's code calls its Add, Subtract or Multiply builtin
for feedback any and goes on after it. Each had left native code at every
turn -- `s += o`, `new Date() - start`, `o * 2` -- 64 exits, the loop then
run in the interpreter.

**On the frame's own state.** What reads only its operands and the frame --
a `new.target`, a rest parameter's array and an unmapped arguments object of
the arguments the frame kept, `arguments[k]` and `arguments.length` of them,
the cells of code whose captured registers are in cells, a closure, a
scope's end -- Go makes from native code on the register frame
(`ssa.GoHost`, `GoArgument`, `GoCells`, `GoClosure`, `GoClose`;
`jit_frame.go`, `jitRegFrameOp`): the run's own frame, or a native
callee's, which has none of the VM's -- a stand-in of its closure, function
object and receiver, its window at its context's `Base`, cleared only of
what an earlier use left in it (`standIn`). Left to an exit: a
mapped arguments object, which aliases the registers; a native callee's rest
parameters and arguments, which its calls keep none of. A closure a
native callee makes that captures by value shares the callee's closed cells
of its registers in place of copies of them: what it takes by value is
never assigned once it is made. Where the code's captured registers are the frame's own slots,
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

**Register functions' calls** (`jit_call.go`). A register function's
link (the runtime's `regLinks`) holds its code's call entry while it has
code native callers may call -- set as the code is compiled, cleared as it
is invalidated or dropped -- and, where it holds none, Go makes
the call from native code (`jitRegGoCall`), as a call of Go. The callee's
window is made at the VM's stack's top, past the caller's window, scratch
slots and frame (`ssa.CallSite`'s `Window`): the call writes a method call's
receiver to its register 1 -- or a plain call's undefined or global object,
an arrow's captured this, for a callee that reads it -- and the arguments
from 2, undefined in its parameters past them; the function to its register
0 only where a callee reads it (`ReadsFunction`, the front end's
`FrameCallee`: its code reads it, its home object or names it), or the call is
generic, through Function.prototype.call or apply, or a super(). A call that
leaves it out describes, in its state, the function object each of its
checks finds (`abi.CallState`'s `Callees`, 0 for a closure checked by its
code, whose object the checks keep in the context's `CallCallee`), and Go
gives a frame it makes of the callee's level that one, written to the
register too (`jitRegLevelCallee`, `jit.CalledFunction`), where its closure
is the level's -- past a call in tail position that gave the frame to
another, whose call wrote it, the register's. It checks
the room for the callee's window, or for its arguments where they are more
than its registers (`CallSite`'s `LocalCount`): one check of a constant,
the VM's stack's top it leaves kept for the window's address where the call
has one function. What the callee's code takes past its window -- its
scratch slots, its loops' slots, its objects' homes -- is its compile's,
no constant of its function, so the
contexts' `StackEnd` is the VM's stack's length less the most any register
code compiled takes past its window (the VM's `regRoom`,
`jitStackRoom`), raised in every context before a code that takes more is
callable: its frame fits past any window the check found room for. The
link's room (`LinkFrame`) is what Go's entries and `enterExit` make. The call's state leaves out the window's
registers the call consumes (`ssa.Value`'s `Consumes`): those the
interpreter's frame does not read past the call, normally or at a handler
(`ir.Program`'s `LiveAt`), and the result's -- a callee that leaves native
code is finished from its own frame and the caller goes on past the call, so
no frame Go makes of the caller reads them, and the call stores no record of
them. The callee's code makes its frame at its call entry: its context the
caller's next, its bindings past the parameters undefined (`Func`'s
`InitEnd`, the register code's `Inits`, as the interpreter's call makes
them) -- not its temporaries, which it writes before it reads them -- its
stack's top past its window, scratch slots and frame (`Func`'s `Scratch`).
The result comes back in registers to the call's keep cell and register.

**Which calls.** The feedback says which functions a call makes natively
(`jitRegFeedback`'s `NativeCalls`, `regCallSites`): the one its site's cache
holds (`regCalls`); for a method call, the methods its `get_method_w` read's
cache saw for the other shapes it met (`jitRegSeenCallees`), learned from
the first compile; and those the code learned it calls at its calls of Go
and at its exits (`jitRegCallSeen`), up to `jitCallTargets` (4) -- a call
that knows none of its functions, not a construction, settled: those met in
its first `jitCallsToInline` calls of Go decided together, the code compiled
again once for them (`jitRegCallSettling`), and a call that leaves at a host
exit for a function an earlier exit there taught the code, which it was not
compiled for (its entry's `targets`), invalidating the code, the interpreter
running the function until it is compiled again with it
(`jitRegCallRepeats`): left valid, it left at each such call until its loop
was judged slow for it -- each checked by its object, or, a closure its
maker makes anew -- another object of a function the call has called -- by
its code (`ssa.CallSite`'s `ByCode`), where the function runs register code
of the form's own layout of the runtime's realm, and its frame is one the
call path makes as a call's (not a class's constructor, nor one that shares
a derived constructor's this). A callee that keeps its arguments -- reads
`arguments`, has a rest parameter -- is given them and their count in its
context (`ssa.CallSite`'s `FrameArgs`, `mir`'s `windowArgs`), where its
reads of them are answered from: by native code itself, `arguments[k]` of
an index below their count and `arguments.length` while the object is not
made, as V8's code reads an arguments object it never made from its frame
(`mir`'s `argRead`, the value to the read's keep cell, its pointer word
through `storeRef` while the collector marks, the context's words being
none the collector sees); by Go otherwise (`GoArgument`: a key that is no
index of them, the object made, the frame Go entered the code with, whose
context holds no call's arguments, and a sloppy function's parameter
its object would alias read as the code has it, `ArgumentParams`, which
`jitArgument` reads: the registers from `ParamBase`, 2, past the function's
and its this, live at each read of the arguments, for the object the
interpreter makes at an exit reads them). EarleyBoyer's `sc_list` and
`sc_append`, native callees that read their arguments in a loop, had called
Go at each read, 86% of Earley's calls of Go on a native frame, each finding
the callee's level. Its rest parameter's array and an unmapped arguments
object are made from them by native code itself, as V8's code allocates
them inline from its frame (`ssa.CallSite`'s `Made`, `mir`'s `frameMade`):
the next of a pool of the site's (`jitRegFeedback`'s `FrameMade`) --
`new Array(n)`'s arrays of its room, or arguments objects of their
allocation's room for 2, 4 or 7 elements, each with the realm's unmapped
arguments objects' properties -- the arguments written into it and their
count its length; a count past the pool's room has Go fill it with objects
of room for it and the take made again, as `new Array(n)`'s is, and the
elements' pointer words go through `storeRef` while the collector marks.
A mapped arguments object, whose callee native code does not have, more
arguments than an arguments object's allocation holds, and the frame Go
entered the code with have Go make them from the arguments
(`jitRegFrameArgs`); and the frame Go makes of its level is given them
(`jitRegPushLevel`). Or the callee is inlined (see
[Inlining](#inlining)). A forwarding call, `f.apply(x, arguments)` of the
realm's `apply` (`apply_args`, `ssa.CallSite`'s `Forward`), is made so too,
of the functions the code learned it forwards to at its calls of Go, settled
as a call's functions are: those met in its first `jitCallsToInline` calls
decided together, up to `jitCallTargets` (`jitRegForwardSeen`,
`jitLearned`'s `callSites` and `settling`), the window's receiver its own,
its function f only where a callee reads it, as a plain call's
(`windowCallees`), its parameters the frame's arguments from its context
(`mir`'s `windowForward`), which a function that forwards has there -- given
at its entry from Go, or by its native caller -- so that prototype.js's
`Class.create` constructors, as Octane's RayTrace makes every object, are
constructed natively, their forwarding call native too. Any other f is Go's
from native code (`jitRegForwardMade`), from the arguments in the context:
`Array.apply(null, arguments)` without a frame (`jitForwardPlain`). A method
call of the realm's `Function.prototype.call`, `G.call(x, ...)` --
TypeScript's `_super.call(this, ...)` -- calls G natively: learned at its
calls of Go (`jitRegViaSeen`), the call checks the method is `call` and its
receiver G (`ssa.CallSite`'s `Via`) and writes G's window from G, x and the
rest, a G that keeps its arguments given them past x in its context
(`FrameArgs`, `windowArgs`); never inlined. A receiver to coerce at a
register call -- a sloppy G given null -- has Go make the call from native
code rather than leave. A call that meets any other function leaves native
code for Go, which learns there what to call next -- a call in tail position
too, which Go makes at the exit as the interpreter's `tail_call` does
(`regWholeTail`): the ordinary call while the stack is shallow, the frame
given to the callee or given up deeper. A call that has met `jitCallTargets`
functions calls any other through its link, as V8 makes a megamorphic call
through the code the function points to (`ssa.CallSite`'s `Generic`, `mir`'s
`genericCheck` and `genWindowSetup`): a closure of the caller's environment
and realm whose template's hint names its function's register link
(`regHint`, `abi.HintTable`), whose link's flags let a generic call make it
(`jitRegLinkFlags`); the window's parameters past the arguments, the link's
`LinkParams` says how many, made undefined, a plain call's receiver
undefined, an arrow's its captured this. Such a callee is
given a link and compiled for native callers at the first call Go makes of
it past the call's targets; one with no code is Go's call. A call that met,
at an exit, a function native callers never call -- a built-in, a bound
function -- past the functions it calls natively is generic so too
(`jitRegGenericSeen`, the VM's `jitLearned`'s `genericCalls`), and is
inlined no more: Go calls such a function from native code, as V8's code
makes a call whose feedback met a built-in through its generic Call
builtin. Learned at no exit, `x.toString()` of objects and numbers left at
every number's call. A construction
(`new_w`) of a base constructor that does not read `new.target` -- a plain
function, or a class's constructor with no field to initialize -- is made so
too, with the constructions its code learned: its receiver taken from a pool
of objects made as `regConstruct` makes one (`jitNewPool`, `ssa.CallSite`'s
`Pool`), in the callee's register 1 and its context's `This`, which the
result is where the callee returns no object. A call of a synchronous
generator function of register code whose window binds its parameters with
nothing run (end_params right after gen_entry), learned at the call Go made
of it (`jitRegGenMakeSeen`), takes its generator from a pool
(`ssa.CallSite`'s `GenMake`): the arguments written to the generator's
registers from its window's register 2 (`GenArgAt`), a method's receiver to
its register 1 too (`GenThisAt`). A construction with the realm's Array,
`new Array()` or `new Array(n)` of a length up to `abi.PoolRoomLimit`,
learned at the call Go made of it (`jitRegAllocSeen`), takes its array from
a pool (`ssa.CallSite`'s `Alloc`). A callee native callers call that has no
code is compiled at their first call of it through Go
(`jitRegNativeCallee`): a function of host operations alone is lowered
for them (`LowerRegInline`).

**Derived constructions.** A derived class's own constructor is
constructed natively too, its instance from the class's pool, in the
callee's register 1 as the interpreter has it, and in its context's
`Instance`, its `This` the uninitialized value, its `Function` the class
(`ssa.CallSite`'s `Derived`); a class without a constructor of its own --
`constructor(...args) { super(...args) }`, register code that starts with
`super_forward` (`jitRegForwardsSuper`) -- with the code of the class it
extends, its prototype checked to be still that class (`Parent`), as V8
skips a default derived constructor. A class's constructor of either kind
that reads `new.target` is constructed so as well: only ever constructed,
its new.target is its context's, which the frames Go makes of its level
and the stand-in of them read (`jitRegReadsNewTarget`). A derived
constructor's register code has a this slot past its captured bindings
(`ir.Program`'s `This`, the context's `This`), the binding: `this_bound`,
`check_this_init` and a `derived_ret` of `undefined` -- what `ret_undef`
makes it, `load_undef` moved to its register, which the lowering finds
(`undefinedAt`) -- read it checked, and leave to Go, which throws the
`ReferenceError`, where it is unbound; `derived_ret` of anything else is
Go's. Its `super_call` (`ssa.CallSite`'s `Super`, the builder's
`superCall`) is a window call of the parent the code learned it constructs
with at its calls of Go and its exits (`jitRegSuperSeen`), checked to be
the running function's prototype (`mir`'s `superGuards`, of the context's
`Function`): the parent's window made with the function
whose code runs in register 0 and the instance in 1, its context given the
instance -- a derived parent's to bind, its this unbound -- new.target and
that function (`mir`'s `windowSuper`), on both backends. The this it binds
-- the result, or the instance where the parent returns no object -- is
`super_call`'s register's, register 1's and the this slot's, stored in the
context's `This` with the barrier while the collector marks. Any other
super() is a call of Go on the frame (`jitRegSuperMade`), which binds the
frame's this and the context's: one of a class that may make an arrow before
a super() (`jitRegArrowBeforeSuper`), whose binding the arrow shares, one of
a parent of fields or that reads nothing native callers construct. Go gives
a derived frame it enters the code with that context at each entry
(`jitRegDerivedContext`), and binds the frame's this from it after
(`jitBindThis`).

**Polls.** A native call is no interpreter's, which spends the interrupt
budget: the callee's code spends the back-edge budget at its start
instead (`ssa.OpPoll`, see [Polls](#polls)), and calls Go when it runs
out, so that a native recursion meets the deadline and the memory limit.

**Levels.** No frame of the VM's is made for a native call. Where Go runs
while native callees are under way, it makes each level's frame as
`regEnterW` would -- its function, its window, past the call it makes --
from the context (`Closure`, `Base`) and the level's code, which the
context names (`Code`, `jitRegLevelAt`), as V8's frame says what runs in
it: every entry of a function's code that makes a native call's frame --
its call entry, its tail entry, where a call in tail position gives the
frame to it, and a generator's resumption -- stores the word its compile
was given for it (`ssa.Func`'s `Self`), so that Go reads a level's code in
one load, the code the level runs whatever its function's code is now. The
word is the VM's id of the code (`jitRegSelf`), an index into the JIT's
table of the entries that have code or are compiled (`regSelves`): an
immediate one store holds on amd64, where an entry's address had been a
load from the code's constants and a store at every call. An id is taken
for a compile and given back once the code is closed (`regClose`) or the
compile made none, and taken again first, so that the table is as long as
the most entries that had code at once. The cache holds the entry, and
once it lets it go, the retired code does (`regRetired`) while any native
call of its function is under way (`regInUse`, `regCalled`); Go reads the
id only while the context's call is (`LiveCall`), and checks that it names
an entry of the context's function (`jitRegCodeNamed`, a panic in the
package's tests). `TestJITLevelCodeLetGo` lets the code go under its
levels with the collector at work, and `TestJITSelfIdsGivenBack` holds the
table to the entries with code. A call that leaves for Go to run its
callee (`ExitEnter`) names none, and its level goes on in its function's
code now (`jitRegCodeNow`):

- a call of Go a native callee makes (`jitRegGoCallNested`) is made on its
  level's frame, the one level Go finds the code of for the call, whatever
  the depth: the run it
  is of is the one whose native code runs now (`regRunAt`, set around
  `SSACode.Run` and `Resume`), not found by a walk down the contexts; the
  levels between the run's and the calling one have no frames until one
  of them is read (`jitLazyLevels`) -- a stack trace, a function's caller,
  through `frameAt` -- room left for them below the calling level's frame,
  so that the depth of calls is what it would be, and are then made in the
  realm the call was made in, as below, and pinned until the call returns
  (`jitRegMakeLazy`, `jitRegLazyDone`, on the state's `regLazyLevels`).
  Levels among which a generator's resumption may be, each with its next's
  frame below its own, are all given frames for the call (`jitRegGensIn`,
  asked only once a link has had a generator entry, `regGens`): the
  levels and frames on stacks of the state's (`regNestLevels`), allocated
  by none. A derived construction's level, and a super()'s parent's, as
  `runFD` makes a construction's: its new.target, and for a derived
  constructor its function, instance and this binding from its context
  (`jitDerivedFrame`); a write a call of Go makes with no frame
  (`jitGoCallPlain`) by the register instruction at the register pc
  (`jitRegPlainSet`);
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
  `errTailCall`) is finished as `regEnterW` finishes one, in the callee's
  native code where it has some (`regRestartRun`). A derived
  construction's result is an object, or what it throws (`jitDerivedResult`);
  a super() whose parent Go finished binds the frame's this to it, or to the
  instance (`jitSuperBound`);
- a closure a native callee makes is made on a stand-in of its level --
  closure, function object, its receiver, or, in a derived constructor, the
  binding its context holds, an arrow made before it is bound left to the
  exit -- its captures by value given the level's cells, which hold the
  values, never assigned once captured, as a copy would; one that reads
  what its maker's frame has of its
  own, new.target or the arguments (`ReadsMaker`), is left to the exit.

A call of Go from the last context, and an unwinding from it, add contexts
(`jitGrowContexts`): a recursion of native calls is
native past the contexts the runtime started with. So does an exit inside
inlined frames whose innermost state is in the last context
(`jitRegInlined`), where an inlined frame's check of the contexts past it
(`ssa.OpFrameRoom`) found too few: left there at every call, it had had
the call no longer inlined, and the closures given to the callee made.

While Go runs beside them, every level's code is held: the cache does not
drop it until the level is done. A level Go found the code of pins it
(`jitRegEntry`'s `levels`); one a call of Go put off holds it by its
context, which names it (`Code`), which the cache counts only when it asks
-- before it lets the code go (`regLevelsOf`) -- so that a call of Go pins
nothing for the levels below it. The function is not compiled again while a
native call of it has not returned -- a context a native call runs in
(`abi.LiveCall`) is its function's (`regCalled`, a look at every context by
the function). A level Go unwinds is no such call: its context is cleared
as Go takes it (`jitRegUnwind`), and it goes on in the code it holds,
natively where its call returns to, kept until it is done. So a recursion's
function is compiled again while Go finishes its levels, as V8 installs new
code while older frames run the old: the new code's calls through the link
enter it, and the levels finish in the old. Counted as levels instead, a
recursion's outer levels would keep it from being compiled again for what
it learned for good. The code stays the level's after the call of Go too,
the level going on in it natively or unwound by Go at its exit: the call
pins it to the run below (`jitRegPinLevel`, `goPinned`) until the run's
native code leaves. While any run's native code runs (`ctxTop`), code
native callers may call (its link) is in use (`regInUse`), the native calls
of it Go knows nothing of until they leave: the cache's flush lets it be.
Code let go of while in use -- for room, compiled again once invalid -- is
kept, not closed (`regRetired`), where a level running it still finds it,
and closed once nothing uses it (`jitRegCloseRetired`). A frame Go makes
of an inlined callee a plain call called is given the receiver the
interpreter's call gives it
(`jitRegPlainReceiver`), which the inlined frame holds only for a callee that
reads it and a stack trace reads. A frame Go makes of a native level keeps
the receiver in its window's register 1 (`jitRegConstructs`): the plain
call's, which the call writes for every callee (`windowSetup`), or the one a
call in tail position gave the frame since, deep in the stack -- a method's,
which the plain call's written over it had lost. The levels and frames of a
call of Go are on stacks of the JIT's state (`regNestLevels`,
`regLazyLevels`), each call's past those of the calls under way below it.
Both backends make such a call natively, the same way (`mir`'s `call`,
`windowSetup`).

A callee that leaves native code on more than one in `jitUnwindShare` of
its native callers' calls is compiled again while it learns, up to
`jitUnwindReopts` times, then no longer called natively (`notNative`, its
link cleared) until Go has made `jitNativeRetry` calls of it, doubled each
time (`jitRegUnwound`, `jitRegRetryNative`): its native callers make a call
of Go there rather than have Go finish their levels at each call. A poll
that unwinds the call is not the callee leaving (see [Polls](#polls)): Go
goes on with the callee in its code as the call it was, and counts it as
leaving at the first exit after the poll that is no poll nor its return
(`jitState`'s `regPaused`). An exit
inside an inlined callee at a property's read or write whose site is not
megamorphic is a map check's failure (`jitRegShapeMiss`): the site learns
the shape as at a guard of the callee's own (`jitRegDeoptSeen`) and the code
that inlined it is invalidated, compiled again with the site's cases, rather
than leave there at every call. A function a call learned by its code
(`ssa.CallSite`'s `ByCode`) is given its link there, as one learned by its
object is: past the functions the call makes natively, a generic call finds
it by its hint (`regHint`).

## Inlining

**What is offered.** A plain call, a method's or a construction of a base
constructor (`call_w`, `call_method_w`, `new_w`), or a call in tail position
(`tail_call_w`, `tail_call_method_w`), offered as the ordinary call
(`jitRegCallOp`), of one function -- the one
its site's cache holds and its code learned it calls, no other, not checked
by its code -- that runs register code native callers may call, not the
caller itself, nor an arrow, is offered with its register code lowered for
an inlined frame (`LowerRegInline`, `jitRegFeedback`'s `Inline`,
`ssa.InlineSite`'s `Window`), asked by its own register feedback, what its
sites' caches have seen added first (`jitRegSeenFeedback`), the root's keep
holding what its sites name. A getter or setter a register read or write
calls (`jitRegFeedback`'s `Accessor`, `accessorOf`) is offered the same way
(`AccessorSite`'s `Inline`), once the receiver and the accessor are checked
(`accessorOp`). A callee whose read at a megamorphic site has met a
getter on its receivers' prototypes, and receivers of more than one
shape, is not offered (`jitMegaGetter`): inlined, the read has no frame
for Go to call the getter on, and native code leaves there, the
interpreter finishing the caller's invocation, at every receiver of the
getter; called natively, its own code makes the read on its frame. A
site of one receiver shape whose cache gave up on a getter it cannot
hold -- a function's legacy arguments -- is compiled for the getter,
which checks that shape, and its callee inlined. A compile asks each call once: the builder asks the root's
calls in each of its builds, and a callee's at each call of it it plans,
and `Inline`'s and `NativeCalls`' answers are kept by the program's
instruction for the rest (`jitRegFeedback`'s `askedOf`); and every call of
one callee a compile asks of has one feedback (`jitRegCalleeOf`), but a
forwarding callee's, whose frame is its call's: its answers are its code's
and its caches', the same at each. So is a closure the function makes and only calls, or only
gives to a callee that calls it, unchecked and never made, an arrow reading
its this among them, its this the maker's (see [Closures the code only
calls](#allocation-and-escape-analysis)). A typed callee, a function of an
asm.js module, is offered so too, and taken into typed code only (see
[Typed code](#typed-code)).

Native call descriptions are read-only, shared by repeated asks and builds,
including from a background snapshot. The builder copies a description
only where it omits an unneeded receiver coercion.

**What the builder takes.** What a function inlines is bounded by size
alone, as V8's JSInliningHeuristic bounds it, never by a count of calls: a
callee's instructions, a budget of all a function inlines, which the calls
made most often get first, and past it only small callees (`ssa`'s
`maxInlineTotal`, `maxInlineSmall`), as deep as `maxInlineDepth`. The
bounds were set in the front end's stack instructions, measured against
V8's bytecode; register code's lowering has 0.85 of them for the same
function, the median of Are We Fast Yet's functions of unique names, so a
register instruction counts 20/17 of one (`ssa`'s `inlineSize`). How often
is V8's call frequency where the feedback knows it (`Frequency`): a call
made less often than `ssa.MinInliningFrequency` (0.05, V8's
`min_inlining_frequency` as V8 runs: the flag's 0.15 is lowered to 0.05
wherever Maglev is on, as it is by default) for each invocation of the
function compiled is not inlined, whatever its callee's size, and is made
a native call -- by the builder (`inlineSiteAt`, `planCallback` for a
loop's callback) and by the feedback before it offers the callee
(`regCallCold`), a call inside a callee inlined excepted -- as Turbofan's
JSInliningHeuristic asks the frequency before it forces a small function in,
Turbofan being the optimizing compiler Node runs (the exemption of a small
callee at a cold call is Turbolev's, which Node does not run); the
frequencies are known once the function has run 59 times
(`jitCountsEnough`); the most frequent come
first, a call whose frequency is not known taken as made once outside the
loops and more often inside one (`ssa`'s `inlineOrder`). Register code's
frequencies are what the feedback dispatcher counted (see
[Feedback](#feedback)). A callee's
throw, or a call its own code has never been seen to make, does not keep it
from being inlined: native code leaves the inlined frame there, as V8's code
deoptimizes at a call with no feedback. An inlined callee's object literal
is made as the root's: its pool (`literal`) and the definitions of its
fields (`define`) are its own feedback's, asked of its frame. Its
definitions asked of the root alone, a callee's literal inlined in a loop
had left native code at its first field at every call.

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
the call's register -- and, where that is a register a closure captures,
to its cell too, as a call's own result is stored (`storeCapturedAt`). A
call in tail position is inlined behind its depth check, the root's
`ssa.OpTailSelf` or `OpTailRoom`: shallow in the stack the callee runs
inlined; deeper, where the interpreter gives the frame up to the callee,
native code leaves for it to. An inlined accessor's window is its function, from a
cell (`InlineSite`'s `FunctionCell`), the receiver and a setter's value, a
getter's result the read's register. A construction's receiver is taken from
the pool of the objects its code learned it makes, kept in a cell, in
register 1, which is the result: the callee returns nothing or it
(`returnsWindowReceiver`). Every state inside an inlined register callee
holds its register 1, live or not, which names the inlined frame's receiver
(`InlineState`'s `ThisSlot`): Go makes a construction's result of it where
it finishes the callee, and the frame it makes of the level has it as its
this, which a stack trace names the frame by -- the function the frame has
is the object the code inlined it for, from its context (`abi.Context`'s
`InlineCallee`). The atomicity rule holds in it as in the root: an exit
inside it runs its register instruction whole again (`atomicInstruction`,
each frame's `readCalls`). Its own calls are asked the same way, of its own
caches and what its code learned, and inlined in it in turn.

A callee that keeps its arguments is inlined too (`forwardInline`). One
that reads them by index
and counts them alone, no object made, no parameter aliased
(`jitRegReadsArguments`, `ssa.InlineSite`'s `ReadsArguments`), reads the
call's: `arguments[k]` of a constant k below their count is the call's
argument k, `arguments.length` their count, as V8 eliminates the arguments
object; a key no constant, of up to eight arguments, is checked to be an
integer below the count and picks its argument by compares of it with
each index (`argumentSelect`), as V8 reads the elements of an inlined
frame's arguments that escape analysis replaced by the call's values; any
other key leaves native code, Go's from the next compile on. One that
hands them on, `f.apply(x, arguments)` (`jitRegForwards`), lowered so that
its `apply_args` is the inliner's (`compile`'s `LowerRegForwardWith`, no
`Forwards`), is inlined with them (`Arguments`), each `apply_args` in it
the call of the function it hands them to, inlined with the same arguments
(`Forward`, `forwardInline`, `windowForwardCall`): for a construction of a
forwarding constructor, k of `this.k.apply(this, arguments)` found on its
prototype; else the one function its own code's forward settled on; else,
for a construction alone, the function the read of f finds now
(`jitRegForwardGuess`) -- a plain call is inlined for no guess: a
forwarder's closures may each hand their arguments
to a function of their own (`jitRegForwardTarget`) -- checked to be f,
apply checked to be the realm's, x the callee's receiver. A forward settled
on one function has the forwarder's native callers (`jitEntry`'s
`regCallers`, by key, joined as their code is cached: `regJoinCallers`)
compiled again to inline it (`jitRegCallersReopt`). No frame is made over
again: the frame Go makes of such a callee's level at an exit inside it is
given the arguments of its caller's call's window, or of the frame that
handed them on (`jitRegInlineArgs`). A callee that would leave native code
at a call it has never made -- one its own code leaves to Go and has not
been seen to make in runs enough to tell, or one whose result it throws --
is inlined all the same, the call an exit (`InlineExit`, of the calls Go
made for its code by register pc, `regCallsMade`).

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
learned something else -- a site's case, a read's value not a number
(`jitNumberMissed`), which the code compiled again reads generic -- has it
compiled again for it (`jitRegReopt`); a change of the callee's caches
alone does not. One that learned nothing counts against inlining at the
call (`jitInlineLeft`, `jitInlineExits`, `notInline`): past it, the code
compiled next makes a native call there. None counts that left at a site
of the callee's -- a property's read or write whose site is not
megamorphic, a construction whose pool ran out (`jitRegLeftAtSite`) --
nor one of code to be compiled again, nor one whose operation threw, which the callee
called natively leaves native code for just the same.

An inlined typed callee's window has its raw slots: the function's slot
kinds (`ssa.Func`'s `Kinds`, the exits' `abi.ExitFrame` `Kinds`) are its
program's and, past them, each inlined typed callee's at its window
(`frameKinds`), so that an exit writes every level's raw slots raw, as the
root's (see [Typed code](#typed-code)). The level's frame is its twin's,
whose code is the typed code, and Go goes on with it by the typed table
(`runRegFor`, `regWholeTyped`), as with the root's.

## Exits and deoptimization

**State maps.** Each reachable instruction has a state map: its register
pc. An exit names one, and its
records say what is written into the frame: the registers live at its
register pc.

**Exits** (`abi`'s exit kinds, in `Context.ExitKind`):

| Kind | What happened | What Go does on a register frame (`jitRegRun`) |
|---|---|---|
| `ExitDeopt` | A guard failed, before its instruction committed anything. | The interpreter runs the rest of the invocation from the exit's register pc; the failure is noted, the code invalidated, and the function compiled again once the interpreter has spent a fresh budget. |
| `ExitHost` | The instruction is Go's. | Go runs the whole register instruction at the exit's pc by the interpreter's own handlers (`regWhole`: its handler from `regTable`, its slow function from `regSlowTable`, as `runReg` and the native handlers' exit stub run them), an exception to the frame's handlers by `regCatch`; native code goes on at the entry of the register pc the instruction went on to, and where the code has none, the interpreter goes on to the frame's next back edge, where native code goes on (`jitRegLeaveTo`). |
| `ExitPoll` | `BackEdges` ran out at a loop's header. | The interpreter's back-edge check; the code compiled again if what it learned is due; native code goes on at the loop's header. |
| `ExitReturn` | The code returned. | `Ret` holds the result's number word, or `RetFrom` where the result is. |
| `ExitEnter` | A native call's callee had no code native callers may call. | Its frame is made, its arguments in it, and Go runs it from its start; its caller's code goes on after the call. |
| `ExitTable` | The exit left its frame to Go to write. | Go writes it from the exit's description (`abi.ExitDescriptor`, `jit.ApplyExit`) and the registers it saved, then reads the exit's own kind. |

**Descriptions share their states' nodes.** An `ExitTable` exit's
description (`abi.ExitDescriptor`) is a node of its code's `ExitFrame.Tree`:
the nodes are those of the frame states' slots (`ssa.SlotNode`), each
described once (mir's `exitNode`) and shared by every description whose
state has it, as the states share it -- a leaf lists the entries of the
slots it writes, each entry, a slot and where its words are, kept once for
the code (`ExitFrame.Slots`), and an inner node its children. `ApplyExit`
walks the tree. Whether a description may be written slot by slot
(`Direct`) is found from what its nodes keep: whether a slot under one has
its reference in a register, and the slots its entries name as their
references' sources, checked against the state. A list of each
description's every slot would make a function of n values live at once and
an exit at each of its n instructions take n² entries, and give up a
description past 65,535 slots for code that writes the frame.

**Rebuilding the register frame.** An exit's records write the registers
live at its register pc, in the window, natively as the code leaves (or Go,
from its description, for an `ExitTable`); then Go makes the objects escape
analysis left unmade into the registers that name them
(`jitRegMaterialize`) -- at every kind of exit, a loop's poll's included,
whose loop carrying them has no entry at its header to go on at -- and goes
on by the exit's kind. A frame native code
called natively that Go finishes, or one where Go goes on at a register pc
with no entry, runs in the interpreter to its next back edge, where native
code goes on at the loop's header (`jitRegLeaveTo`); a frame whose code was
invalidated, to the end of the invocation.

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
leaves to Go is learned too (`jitRegHostSeen`, `jitRegWriteSeen`), and so
is a loop of a built-in's call -- its function, receiver and callback --
that left at its checks or inside it (`jitRegForEachSeen`); where that
teaches the code something it was not compiled for, it is to be compiled
again (`jitRegReopt`). See [Feedback](#feedback) and
[Tier-up](#tier-up-and-on-stack-replacement) for what follows.

**Deoptimizations end at their sites.** A deoptimization widens what it
failed on, as V8's feedback widens at its deoptimizations: a property
site's cases take the receiver's shape, up to one more than they hold
(`jitMissedShapes`), and the site is megamorphic past it or at a shape met
again after a compile; an operator's BigInts are made natively, then the
operator is generic; a call's targets join those it is compiled for, up
to `jitCallTargets`; an entry is entered generic, and any other guard's
instruction built generic (`Generic`), at the first failure. An
operator's operand lattice learns the kinds it met (`jitLatticeWiden`).
Each of those ladders ends, so a site deoptimizes a bounded number of
times; and lest one go on past its ladder -- what a site learned forgotten
while its code was parked (`regForget`), say -- each site's
deoptimizations are counted by register pc, an entry's apart, in what its
function learned (`jitLearned`'s `siteDeopts`, `jitRegSiteDeopted`): one
that invalidates the code counts at the site of the function whose
instruction failed, an inlined callee's in the callee's, and at
`jitSiteDeopts` (8), past every ladder, the site is built at its top from
then on (`jitRegSiteTop`) -- an instruction generic and a property site
megamorphic too, a loop of a built-in's call Go's call, an entry generic,
and a call's targets met past those compiled for no longer invalidating
the code (`jitRegCallRepeats`), as V8's megamorphic calls go through its
generic Call builtin. A function's deoptimizations are so at most
`jitSiteDeopts` times its sites, its inlined callees' among them, which is
what ends a loop of deoptimizations: there is no limit on a function's
own, as V8 has none for JavaScript, and refusal is for bailouts alone --
a compiler's panic, `abi.MaxSlot`, the debugger. A deoptimization at a site
already at its top widens nothing and counts in `topDeopts`, and the tier
trace marks it (`JITTierEvent`'s `Top`); none should be, and the tests
that deoptimize a function over and over check it
(`TestJITDeoptsBoundedBySites`, `TestJITSiteDeoptsForceTheTop`). In the
V8 suite, Octane, Are We Fast Yet and ssr, no site reaches the bound: the
most deoptimizations at one site are 3, of one function 14 (Typescript's).

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
would write more. Its spill slots are as many as its allocation takes
(see Spill slots). Code of 300, 1,000 and 5,000 locals runs natively
(`TestJITManySlots`), as does register code of 1,000 and 5,000 values
live at once, in some 1,400 and 6,700 spill slots, and one of 10,000
compiles for both backends (`TestJITSpillMore`,
`TestJITSpillMoreCompiles`).

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
memory limit, gives a new budget, and lets the code go on -- at a loop's
back edge, once it has brought the frame's registers up to date (see
[Native code on the register frame](#native-code-on-the-register-frame)).
The call is the budget's alone: the collector's marking changes nothing of
it.

A poll is a pause, not an exit, whether it calls Go or exits: how often one
comes is the budget's, not the code's. The code entered again after a
poll's exit goes on with the stretch the poll paused -- judged neither as a
stretch of its own (`jitSSAStats`' `resumed`) nor as one begun at the loop
it polled at -- an inlined callee's poll counts nothing against its
inlining, and a native call a poll unwinds is not its callee leaving
(`jitRegUnwind`, above). Counted as exits, a stress budget of three back
edges had made `forEach`'s code stretches of three turns, judged slow, and
its callbacks callees that left at every call, each called by Go.

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

A realm a host drops is reachable from what the JIT keeps of it -- the pools
of its functions' code, what they learned, the closures their calls met --
however its key holds it, so the flush lets go of it (`regFlush`): a realm
none of whose code was used and none of whose functions ran since the
flush's last look (their `regLifeUses`) has its functions' state let go of,
with their entries with no code and their programs (`regLetGoLife`); its
code goes as any unused code does. One that has only gone quiet warms up
again if it runs again. Its key gone, the rest goes with it (`regAge`).

**Refusals.** A refusal is an entry with no code, and marks the template
refused (`closureCode`'s `jitRegRefused`), so that its samples ask nothing
more -- but a function of host operations alone, which is lowered again once
a budget more is spent (`hostOnly`). Code of any length is placed (see
Allocation). A compile the budget has no room for leaves
no entry, and is not compiled again until the cache has let some code go
(its `life`'s `deferred` and `generation`), so that a function the budget
has no room for is not compiled again at every call Go makes of it.
Deferred, the function warms up again -- its budget begun anew, which its
back edges and calls count down (`jitRegArmEdges`, `jitRegArm`) -- before it
is asked for again, and an ask finds the deferral by a lookup
(`jitRegForAs`): with its budget left spent, every turn of its loop and
every call of it would ask again. A first compile asks for the room its code
is likely to hold (`perInstr`) before it lowers anything; a compile again of
a function whose code the cache holds asks only for the room its last
compile was short of (`reoptRoom`). Placed, the code takes the room of the
code it replaces -- the cache's, with the code kept beside it for a loop, or
that loop's code -- as given back, that code held meanwhile (`busy`,
`regRoomFor`), so that a large function's code is not counted twice and its
compile again refused for the room its own old code holds. No function is
refused for its deoptimizations (see [Exits and
deoptimization](#exits-and-deoptimization), "Deoptimizations end at their
sites").

**What a compile will hold.** The VM weighs it before it asks for a
compile (`jitCompiles`, `jitCompileBytes`): 512 bytes an instruction and 64
a slot, of what the code, its descriptions and its program take -- 260
bytes an instruction at the median of the V8 suite's, Octane's and AWFY's
functions, 421 at the 99th percentile. A function whose estimate is past
the budget is not compiled; like a deferred one, it warms up again and is
asked for again once each budget is spent. The budget bounds what the JIT
holds, not a compile's working memory, which is linear in the function's
size as what it holds is (`TestJITCompileLinear`): some 131,000
instructions are compiled at the default budget, 16,000 at 8 MiB,
whatever their slots. The estimate counts the register code's
instructions, scaled as inlining's bounds are (`jitRegInlineSize`) to the
unit the shares were measured in, and its registers.

**Hints.** A template's hint, a slot of the cache's hint table (not a
pointer), names its function's link for generic calls, which native code
finds a callee's code by (`regHint`, `abi.HintTable`); the slot holds the
function's address too, so a stale hint misses however often its slot was
reused. The hint lives in what a template's closures share
(`closureCode`), so the closure holds nothing of the JIT's. Go finds a
function's link by it too (`regLinkOf`), before the map of links, by
the slot's link and its key: a link is in its slot only while the map
has it. The table grows only in room made for its growth (`hintGrowth`, `makeRoom`); where
there is none, the link has no slot for now and a generic call of the
function is Go's until a later link finds the room. When the OS denies
executable memory, the runtime marks the JIT unavailable for good.

**Debugger.** Runtimes made `WithDebugger` never run native code.

**Allocation.**

- A runtime's code lives in its `Arena`: chunks of 64 KiB, each function
  placed after the last at 64-byte alignment, so that a runtime's functions
  share a few mappings (`TestJITArenaMappings`). A function longer than a
  chunk gets a mapping of its own, a whole number of chunks as long as it,
  which no other code shares, so that releasing it unmaps it at once,
  rather than leaving its megabytes mapped for the few kilobytes of others
  placed after it, which no budget would count. The code after it goes on
  in the shared chunk.
  So code of any length is placed, its branches in reach across all of it
  (see [Branch reach](#the-backend)); its footprint, its length among it, is
  counted against the budget as any code's, and code the budget has no room
  for is deferred as any compile is (`TestLongCodePlaced`, `TestJITLongCode`).
- Placing a function makes the pages it touches writable, copies it, and
  seals them read/execute again. Pages are never writable and executable at
  once, and published code is never patched. Other code in those pages does
  not run meanwhile: an arena is one runtime's, and its native code always
  returns to Go before Go compiles anything.
- `PlaceRawApart` and `NewSSACodeApart` place code while other code of the
  arena runs on other threads, for the WebAssembly JIT's compiles in the
  background and the workers' of register code: from a page boundary past
  everything placed in the last chunk, or in a new chunk, on pages no code
  is in and none was ever executable, the codes of one call together;
  code placed after packs after them as usual. Under `LayoutSeed` a
  batch begins a drawn offset under 4 KiB past its page boundary, as a
  chunk's first code does, so that a benchmark's placements move the code
  compiled in the background too.
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
`WithJITMemoryLimit`'s if one is given; else 64 MiB, or an eighth of the
runtime's memory limit where that is smaller. Every program measured fits
the default -- Octane's Typescript peaks at 21 MiB, Octane run whole in
one runtime at 44 -- and the WebAssembly engine's is the same. It is a
ceiling: the flush gives back what has gone unused, while the runtime runs
and as it returns to its host.

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

**Register code in the budget.** Register code counts in what the JIT holds
(`regHeld`, in `held`), against the one budget, and is aged, reclaimed and
flushed: its uses -- Go's entries, the runs
it went on in, native callers' calls -- date it (`regAge`), and a compile
makes it young (`jitRegEntry`'s `used`, its epoch); `makeRoom` reclaims it
with theirs, refusals first, then code unused this epoch and the one before,
least recently used first (`regVictims`, `regReclaim`); and `flush` drops
code unused between two of its looks (`regFlush`), a flush age of the
program's running apart (`jitFlushAge`, `flushNow`), which it takes at a
back edge's check, before a compile and as the runtime returns to its host
with no frame running (`jitReturned`, from `endTurn`), so that an idle
runtime gives back what it no longer runs -- code used is code that
ran, and the callees of code used, inlined there (its keep's `inlined`, what
it learned of its calls) or called natively (its keep's `links`), whose own
uses do not count those runs.

A function's state (`regLearned`, a `jitEntry`) counts too: the entry and
what it learned, its pools' objects among it (`regAccount`, `regCounted`),
counted as it grows where its code joins a callee's callers
(`regJoinCallers`), as a compile caches its code, and at every look of the
cache's. A compile is placed only in room for its code and for what it had
the state hold besides -- the pools of the literals, closures and
constructions it makes, filled as it was built (`jitRegPlace`); and the
state is held busy meanwhile, no victim of the room its own compile makes
(`regLifeSpare`). It is made in the room for it (`regLearnedOf`); where
there is none it is kept all the same, uncounted, and counted once its
function's compile asks for its room with the code's (`jitRegRoomAhead`) or
at the cache's next look, which then makes the room among it -- it is made
at its function's first sample, after its first run, and refused, a loop
driving a program's phases had had no code once the first phase filled the
budget. The state of a function the cache holds nothing for is reclaimed
with the refusals, least recently made first (its `serial`, which orders its
function's refusal and programs among theirs too, `regVictimSerial`: ties
left to the cache's maps had a full budget's compiles differ from run to
run), and so is a refusal's with it; a state reclaimed when listed leaves
the floor as it was, which a stream of functions each taking another's room
had otherwise made every compile list the cache again. Code let go of so has
its function warm up anew (`regLetGo`), and compiled again it is compiled
with what its function learned that holds no object, which outlives its code
(`regLearned`): the sites whose speculation failed, the reads and the
arithmetic that met other kinds, where Go went on, the lowerings' hints.
The
callees its calls met, the closures it inlined, the pools its constructions
and literals took objects from go (`regForget`): a pool keeps every object
it handed out, and a closure everything it captured, so kept with a
function whose bytecode outlives them they would keep a finished program's
heap alive, for every later collection to mark again. A closed entry
drops what its code read by address and the targets it was compiled for
(`regClose`); a run's end clears its contexts' records up to what any
code's native calls write (`callRefs`, which every compile raises) and,
with no native code running, the stand-in frame of a callee's call of Go
(`calleeFrame`). The
cache knows a function by its key, not its templates, so a template's code
(`jitReg`) is code only while the cache holds it (`jitRegEntry`'s
`cached`): the template's next call, or its native callers' next call of Go,
finds code let go of and has its closures have none (`jitRegLetGoOf`), the
runs it had counted kept (`jitRegInvoked`), and the native callers' call
compiles it for them again (`jitRegNativeCalleeN`). A callee let go of
had warmed up: one too long to inline is not taken for one warming up
(below) and is compiled for its native callers at their next call of
Go.

Code is in use, and never let go of, while it runs as Go knows it -- a run
of it, a level of a native call of it Go found, a compile of its function
in the background (`regRunning`) -- or while a native call of it has not
returned, which the contexts say: native code marks a callee's context
(`abi.LiveCall`, its closure stored) before the call and clears it as the
callee returns, and the cache looks for the function among them
(`regCalled`, `regCalledFns`). So code
under a native frame that is not running -- a program's functions after the
program, which native callers call -- is let go of for a compile asked under
another program's native frame, or flushed there. Code replaced while a run
or a call is in it is closed once the last leaves it (`dropped`,
`regClose`).

A link (`regLinks`) is counted by the code that calls through it
(`jitLink`'s `refs`, `regHoldLinks`: once for each entry's keep, as it is
placed and as it is closed), and is let go of once its function has nothing
in the cache, no code calls through it and no compile under way -- on the
runtime's goroutine (`regBuilds`) or in the background -- asked for it
(`regUnlink`); a call Go makes for a native caller gives a function a link,
for a generic call to find it by its hint (`regHint`), only if the cache
holds something of it (`regLinkIfCached`). A link let go of frees its slot
of the hint table but leaves the slot its function (`regUnhint`): a template
of the function, of the link's realm, may still name the slot, and a generic
call compares the function and its own realm alone, so the slot is given to
no link of the same function after (`regFreeHint`), whose realm's code that
template's closures would call.

A function's programs (`regLowered`, one lowering for the kinds that share
its program, another for a forwarding callee's) are a cache its compiles are made from, which the budget can
always reclaim: each is the cheapest victim (`jitVictimLowering`), whatever
its function's entry, but for the program the function's code was compiled
from, which that code holds and which goes with it (`regLoweringSpare`,
`regTrimLowering`); the flush lets go of those no compile asked for since
its last look (`regLowerLook`); a compile of the function under way keeps
them (`jitLowering`'s `busy`, a pending job). A compile that asks for one
let go of lowers it again (`jitRegLower`). A compile refused the room for
its code drops its function's programs.

A callee too long to inline that is still warming up is not compiled at
its first call from native code unless its caller calls it often, as
told under the tier-up above (`jitRegLongWarming`, `jitRegCalledOften`);
otherwise it warms up as any function does, so that long functions run
once a frame, as Mandreel's are, do not take the budget.

**Parking** (`jitRegPark`, `jitRegUnpark`, internal/vm/jit_idle.go). Code
holds strongly what it reads by address -- its calls' targets, the
prototypes and shapes its checks compare, the closures it inlined, the
cells and pools it reads, and its own function and closure -- and so does
what its function learned (`jitLearned`): held so, code compiled while a
program ran kept that program's objects alive as long as the code lived,
and the cache, which holds code by its function's key, kept the code
whether or not anything else reached the function. A library function's
code held a finished run's closure as its call's target, and through it
the run's heap; zlib's code its 128 MB heap. V8 holds such objects weakly
in its code and feedback and deoptimizes code whose objects die, at the
collection that finds them dead. Go has no such point: a cleanup or a
finalizer runs after the collector found the object dead, on another
goroutine, by which time a sweep may have given the object's memory to a
new object that code comparing the old address would take for it. So code
holds its objects strongly while it may run and weakly only while it
cannot. As the runtime returns to its host with no frame running, once
the program has run, since the last parking ended, 200 times what that
parking, the unparkings since and the compiles again of the code it lost
cost (`jitReturned`, `parkDue`, `jitParkPays`: what parking costs besides
came to some one and a half times that, so parking's share of the running
is some 1 to 2%; and a turn too short to measure parks nothing), the cache parks its code no
run, native call or compile uses, valid or invalidated: out of the cache
(`regParked`), its link cleared, so that native callers call Go, its entry
marked not cached, so that its templates' calls take the path code let go
of takes; and every pointer to a closure, an object, a shape or a
function's bytecode its entry, its keep and its function's learned state
hold becomes a weak pointer, its slot cleared (`jitParked`), the walk going
into the structures the entry owns alone (`jitParkInto`) and no other --
but for the package's own static objects, the caches' sentinels and
`absentHolder` (`jitParkStatic`), which are the binary's data rather than
the heap's, which a weak pointer cannot be made to, and never die: their
slots keep them; its pools are emptied of the objects made for them, filled again from
their sources when the code next runs out, and its function's learned
state, which other functions' compiles read, is a copy that holds no
object meanwhile (`regForgotten`). No compile is under way then: the
parking waits for the background's. The first use after -- Go's call of
the function (`jitRegEnter`, `jitRegHot`), a native caller's call of it,
which leaves for Go (`jitRegLetGoOf`), a compile asked for it
(`jitRegForAs`), a caller's compile that reads what it learned
(`jitRegCalleeOf`) -- unparks it: every weak pointer's `Value`, nil from
before its object's memory can be reused, is read; all alive, the slots
are written back, the learned state too with what the copy learned
meanwhile (`regRemembered`), and the code goes back into the cache with
its link, no compile made; one dead, the code is let go of as the flush
lets code go, and the function warms up anew. No comparison against an
address an object no longer has is made. Code parked a flush age and
unused is let go of at the next return (`regAgeParked`), however many
parkings came between, but the callees of the cache's code (its keeps'
`inlined` and `links`), as the flush keeps them, parked another flush age;
and parked code is the first `makeRoom` takes. The cadence is parking's
cost, not the flush age: parked once a flush age of the program's running,
the code a program's last turn compiled had stayed held when the turn
ended within a flush age of the last parking -- after Octane, Typescript's
code and some 190 MB of its heap, in most runs on one machine and none on
another. Active code pays nothing:
its entries are tested as before, and a parked one takes paths code let go
of took already, once. The parking also drops `makeRoom`'s list of
victims, an epoch's, which had kept the entries it listed past the turn.
After Octane in one runtime the JIT's live heap is the interpreter's and
some 50 MB in every run, against 250 to 280 MB more before.

**Making room** (`makeRoom`). A compile asks for room for what its entry
will hold before its code is placed. The cache reclaims, in this order, each
least recently used first: parked code; lowerings (a register function's, but for the
program its code holds); refusals remembered; code not used in the last two epochs (old, as V8
flushes code by age); the workspaces. Code used within two epochs, code that
runs, and an entry being compiled are never reclaimed. If all it may reclaim
would not make the room, it reclaims nothing and the compile is refused: the
function runs in the interpreter until some code is old. An epoch is a
collector cycle, or 50 ms.

The cache holds no more than its budget whenever it returns to the program.
A compile, refused or not, ends with a settle (`settle`): past the budget --
the programs its callees lowered, the states its callers joined -- it takes
young code too, the least recently used first, none that runs. A look of
the cache's (`age`, the flush) counts what it holds anew -- every state
measured again, grown by what its code learned at its exits, and the states
kept with no room for them -- and outside a compile or a background
compile's install, whose end settles, it settles at once (`keepWithin`):
at a function's first sample, a link's hint, the contexts' growth, a
regular expression's compile, a back edge's flush.

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
arm64: R15, R17, R16 and R0, with the link register). A nil over a nil,
a primitive's over a primitive's, has nothing to write or to shade, and
the routine goes straight back from it, nothing saved. An object's
pointer stored back where it already is goes through the barrier all the
same: native code writes some pointer words with no barrier by design --
a native call's operands into its callee's frame, a loop's step into a
keep cell (below) -- so a slot's holding a pointer says nothing of
whether the collector has seen it, and what native code holds where the
collector does not look, an exit's records among them, is shaded by the
barrier's store of it (below, "Shading first"), even into a slot that
holds it already. Skipping those stores left objects of AWFY's Havlak
reachable only from the VM's stack, already scanned, which the collector
freed. Shading is skipped only
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
is shaded in its pool before the call takes it; what native code writes
into it -- an array literal's elements and a generator's arguments, written
while the pool still holds it, a closure's captured cells, the arguments in
an entry's cells -- an argument native code reads from its context, whose
words the collector does not see, into its read's keep cell (mir's
`argRead`), and a value pushed or popped go through it too, as do
the pointer words of the calls of Go native code makes itself, made alike
whether the collector marks or not (mir's `byMarking`): an object spread by
layout and a literal's field along its shape's transition -- the values,
the shape, the result -- an array spread and an element appended -- the
elements, the result -- an array's length written shorter, whose elements
past it are cleared so, which shades what they held, and a for...in's
cursor taken again -- the keys, their atoms, the object, its layout, the
cursor to its keep cell (mir's `forInStart`). Marking or not, an object a
call takes goes to the call's keep cell only once every operand is read, as
a call's result does: an operand's pointer word may be read from that very
cell, where the call kept the object it made the turn before -- `v = [v]`
in a loop (mir's `arrayLiteral`, `genMakeMarking`).

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

**Waiting for the marking.** A runtime made with `WithGCYield`, in a
process with one P, waits while the collector marks (`gcYieldNow`), so that
Go's idle mark worker marks at full speed rather than a quarter of the P.
It waits only where Go runs already -- after native code has returned to Go
(`jitRegStretch`), at the top of a call of Go from native code
(`jitHostCall`), in a pool's refill and at the interrupt check -- never by
leaving native code: the code does the same work, with or without the
option, marking or not.

`TestJITSameWorkWhileMarking` holds the code to the rule: every test of
the micro-benchmarks is run with native code seeing the collector idle and
marking, and must make as many calls of Go, leave as often, have the
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
`internal/vm/jit_regexpnative.go`), so that `regexp` imports nothing of
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

## Typed code

A function of a validated asm.js module has typed code beside its register
code (docs/register-interpreter.md, "Typed code"): each register of one kind
for the whole function, a Value, a raw int32 or a raw float64, the kinds
proven by the validator. The JIT compiles typed code as V8 compiled
validated asm.js, at the module's link rather than through warm-up and
feedback, and with no speculation on the kinds.

**The twin.** The JIT compiles a closure's register code, keyed by its
function, and a function of a module has two codes. A closure linked to its
typed code in a runtime with the JIT therefore gets a twin (the VM's
`asmTwin`): a closure of the same upvalues, realm and call caches, whose
function is a copy of the closure's whose register code is the typed code
(`bytecode.RegReady`), and whose template is its own -- its native code,
its counts, its key in the cache. Typed call sites cache the twin
(`regCallKind`), the typed interpreter runs its frames where it has no
native code, and the JIT compiles it as any register code. A frame of the
twin is the function's to everything that reads one -- its name, its
script, its positions, which its own register code, the typed code, gives
(`Function.PosAt`). A native call checks its callee's function object, the
closure's, and enters the twin's code (`jitTypedTwinOf`).

**Compiled at the link.** The link of a module's closure
(`jitTypedLinked`) links the module's functions the closure calls --
those its function tables hold among them -- then
asks for its twin's code as any compile asks (`jitRegFor`): in the
foreground, or in the background where the runtime compiles there, the
typed interpreter running the function until its code installs -- on the
spot where the background has no room for another compile (`jitJobsMax`),
since nothing asks a typed function's code again; the
compile asks the cache for room as every compile does, and one with no room
or refused leaves the function to the typed interpreter. Typed code's state
is marked `typed`: no stretch, call of Go or exit judges it slow, as none
of them is a speculation's cost. A call through a function table (a call
whose function `ttab_get` read) has the table's functions as its targets
from the first compile (`regTableTargets`), each checked by its object as
a learned target is: validation fixes the table, and its functions are
linked with the caller. Learned a call of Go at a time, each a compile
again, the micro-benchmarks' calls through a table of four had been Go's
for most of a run. A table of more functions than a call checks
(`jitCallTargets`) is called by index (`ssa.CallSite`'s `Table`,
`regTableEntries`): an `abi.TableEntry` for each of its functions -- the
function object, its twin, the twin's captured bindings and link -- and
the call's masked index, which the lowering names on the table's read
(`ArrayRead`'s Key) and the build finds by the function's value
(`tableRead`), has `OpTableCallee` put the entry's address in the
context's `CallTable` right before the call. Past its first functions'
checks, the call's generic path takes the closure and the link from the
entry (mir's `tableCheck`) rather than from the function's hint, checking
that the entry holds the call's function; an entry with no link is Go's
call. Checked one by one, a table's functions past the fourth had each
been a call of Go.

**The lowering** (`compile.LowerTyped`, beside `LowerReg`). A register is a
slot, as `LowerReg` has it, and the ordinary operations typed code holds
(`bytecode.TypedPlainOps`: moves, constants, an upvalue's read, jumps,
`call_w`, the returns) are lowered by `LowerReg`'s code. The raw registers
are raw slots (`ir.Program`'s `Kinds`: `SlotI32`, `SlotF64`, scratch slots
of each kind as an expansion takes them), and the raw operations `ir.Raw`
instructions on them (`ir`'s raw.go), a compare-and-branch an `ir.Branch`
whose `Raw` names the compare (`RawBrI`, `RawBrU`, `RawBrF`). A
function table's element is the table's read at the masked index; a heap
access a `RawLoad` or `RawStore` of its element type, and a module
variable a `RawCellGet` or `RawCellSet` of its cell. Only what has no
lowering is refused, by the operation's name, and the function runs in the
typed interpreter.

**The build** (`ssa`'s raw.go). A raw slot holds what every slot holds in
the builder, a value: the box of its float64, or of an int32's conversion.
So unboxPhis, intPhis and truncInts take a raw slot's phis as they take a
number's, and a read unboxes the box the instruction that wrote it made;
no unbox of a raw slot is left to check. The operations are the SSA's
own, with no guard: `OpAddI32`, `OpSubI32`, `OpMulI32` (imul), the
bitwise ones and shifts, `OpCmpI32` signed and unsigned (`CmpUnsigned`),
`OpClz32`, `OpToInt32`, `OpI32ToF64`, `OpU32ToF64`, the float arithmetic,
compares, `OpFroundF64`, `OpSqrtF64` and the roundings, `OpMathCall` for
the other Math functions (`RawMathFeedback`), `OpPowF64` and `OpModF64`,
which Go makes where they are not exact. Integer division and remainder
are instructions of their own (`OpDivI32`, `OpDivU32`, `OpRemI32`,
`OpRemU32`), as V8's asm.js operations `I32AsmjsDivS` and its kin: the
operands widened to 64 bits, sign- or zero-extended, and divided there --
IDIV or DIV on amd64, SDIV or UDIV and MSUB on arm64 -- where no int32's
quotient overflows, so MinInt32 / -1 wraps to MinInt32 and MinInt32 % -1
is 0, and a zero divisor gives 0 without dividing: `| 0`'s results of
JavaScript's, never a fault, on any CPU. A double's `%` is Go's
(`OpModF64`), as V8 calls fmod. A conversion of a value (a parameter,
an import's result) unboxes a number, converts an oddball natively, and
leaves anything else to Go at a host exit, which runs the instruction --
valueOf, which may throw -- and resumes after it. The heap's accesses are
`OpHeapLoad` and `OpHeapStore` of the heap the feedback gives
(`HeapFeedback`, an `abi.HeapCell`): the byte address, its low bits cleared
to the element's size and sign-extended, compared unsigned with the heap's
length -- a negative one is past any length -- and the element read or
written at the heap's base; out of the heap a load gives 0 or NaN and a
store does nothing. The VM keeps every heap native code reads current as
its buffer's bytes move or go (`asmHeapKeep`, `bufferChanged`), so native
code reads the cell with no check of an epoch: a detached heap has a
length of 0. The base and the length are values of their own, the
accesses' last operands (`OpHeapBase`, `OpHeapLen`), and variables of the
build's SSA construction as slots are (`heapOf`), of slots below 0: read
where a block has them, and loaded from the cell only where they are stale
-- at an entry, and after a call (`reloadAfterCall`), which may run script
that detaches or moves the buffer; nothing else changes a cell. Where a
join's predecessors have different ones, the join loads them again rather
than merge them by a phi (`heapMerge`): every one of them is the cell's,
and a phi of them had been moved at every edge of a large function, slot
to slot where they spilled. So a loop that calls nothing loads them once,
before it, its header's phi of them trivial. They are only ever the
accesses' operands, so they live in a spill slot from where they are made
and take no register (mir's `memOperand`) -- a phi of them and its
arguments one slot, their spill class's, which no edge moves -- and each
access compares its address with the length and adds the base where they
are, as memory operands (amd64's `spillOperand`; arm64 loads them), where
it had loaded the cell's address and both of its words. Given registers,
they had taken from a small loop the one its back-edge budget needed,
which then counted in memory. Neither op exits, and a store stores no pointer: the marking
rule holds with nothing of the barrier's. A module variable is its cell's
number word (`OpCellNumLoad`, `OpCellNumStore` of the cell
`OpUpvalueCell` gives): validation keeps anything but a number from the
cell, so a read is the word as a float64 and a write the word of the
number's box, its NaN canonical, the pointer word left as it is, none --
no barrier, no check.

**Entries and exits.** Typed code is entered at its start, where no raw
slot is live, at its loops' headers by on-stack replacement and after what
Go ran: an entry loads a raw slot raw (`OpLoadSlot` of type Int32 or
Float64, never lazily) into the box the build keeps. An exit writes a raw
slot raw, an int32's bits in its number word's low half or a float64's
bits, its pointer word none (`abi.ExitFrame`'s `Kinds`, `abi.RawWord`,
`ApplyExit`; a native call's records, `jitRegApplyRecords`), so that every
exit is exact for the typed interpreter, which the VM resumes at the
exit's pc (`runRegFor`), and an instruction Go runs whole is run by the
typed table (`regWholeTyped`).

**Calls.** A call between the module's functions is `call_w` of a window of
Values, made natively: the feedback answers the callee from the binding the
call's register was read from, the callee's twin and typed code
(`jitTypedCallee`). The callee converts its parameters as its code's
prologue says, a number's natively. An import's call is Go's from native
code, and its result, which may be anything, is converted as a value is.
The result of a call of a function of the module -- a function binding's,
which the link checks, or a table's element -- is a number of the
function's declared type, every return of which validation proves: its
conversion is marked so (`ir.RawNumber`, found by the lowering's
`callResult`), and is its number word, unchecked (`ssa.OpNumberF64`), with
no check of a tag and no call of Go for a string; a signed int's, which
the call coerces with | 0, converted by truncation alone, with no check of
a number past the int64s' (`ssa.ToInt32Exact`). A box of an integer's
conversion, which is never NaN, is its bits, with no NaN made canonical
(mir's `exactF64`). Ordinary code's native
code calls a function of the module natively too, its site's cache the
twin's though the interpreter marks it a crossing (`regCallCross`,
`jitTypedCross`): the interpreter enters the other kind of code in a code
entry of its own because a native context holds one table, but native
code reads no table, and the callee's heap is its own cell. The window is
Values in both kinds of code, and an exit of the callee's goes on in the
typed interpreter from the frame Go makes of its level, as an exit of a
typed caller's callee does.

**Inlined.** A typed callee is offered as any register callee is (see
[Inlining](#inlining)), its program its typed lowering and its feedback
its own -- its heap, its Math functions' computations -- and the builder
takes it by the ordinary rules, its frequency, the same budgets and its
calls inlined in it, every one, or it is not, into typed code alone
(`ssa`'s `typedInline`), where what it reads of the VM its feedback
answers: a typed build refuses what its feedback does not answer, which
no callee's may make it do. Its module variables are read and written in
its closure's cells, as an inlined callee's captured bindings are
(`rawCell`); its raw values stay in registers, the boxes of its arguments
and its result folded away; its phis are typed code's, int32 wherever
their arguments are (intPhis). Its slots' kinds are the function's past
the caller's, at its window (`frameKinds`): an exit inside it -- a loop's
poll, a guard's failure -- writes its raw slots raw, and Go makes its
frame of its typed code and goes on with it in the typed interpreter (see
[Its exits](#inlining)), as at the root's exits; and so do the records Go
writes a frame from (`jitRegApplyRecords`, by the code's kinds,
`jitRegEntry`'s `kinds`). A recursion is not inlined in itself, nor a
callee whose call of an import, Go's, nothing inlines -- as an ordinary
callee is not.

**What is left.** A call between typed functions natively boxes its
arguments into the window and the callee unboxes them, and the callee
boxes its result, which the caller unboxes: a raw calling convention would
keep them in registers -- the int32s and float64s passed and returned raw,
the callee entered past its parameters' conversions at an entry of its own
for typed callers, its window's Values still written for the frames Go
makes of its level, and a level Go finishes giving its result back raw
where its native caller resumes. A typed function the interpreter answers
without a frame (a leaf, `regCallKind`) is never linked, and runs as the
ordinary code's leaf.

## Zero cost when off

**Without the tag.**

- `jitBuilt` is false, and every hook in the register interpreter's Go
  code is either an empty stub or inside `if jitBuilt`, so it is compiled
  out. The one exception is the native call handlers' test of `jitEnters`
  (below): one handlers' block serves every build, so the test is in it, and
  never holds without the tag.
- The native handlers hold no feedback dispatcher: the binary has plain's
  table and handlers alone (`native_nofeedback.go`).
- In `internal/regexp`, `nativeBuilt` is false (`native_off.go`), and the
  search's hand-off to native code is a branch on that constant, compiled
  out with it.
- `internal/cmd/hotdiff` checks the register interpreter's hot functions:
  compare an untagged `cmd/qjs` at main with one at the change: `runReg`,
  `rhCallW`, `regEnterW`, `regCallSlow`, `regRunFD`, `callObject`,
  `nativeRun`, `nativeLoop` and the native assembly must be the same,
  instruction by instruction. The native handlers are pinned by their
  generated files' staleness tests too (`TestRegNativeGenerated`).

**With the tag, JIT off.**

- A runtime with the JIT off is given plain's dispatcher, the one a build
  without the JIT runs (`nativeDispatch`), which a build with the JIT makes
  as a build without does (`TestRegNativePlainSameWithJIT`).
- The register interpreter pays a check only where a budget runs out, and
  one test of the template's `jitEnters` byte at each call entry -- the
  native `call_w` and `call_method_w` handlers, `rhCallW`'s hit,
  `regEnterW`, `regEnterScoped` and the tail call's -- behind `jitBuilt` in
  Go; no other handler tests anything for the JIT.
- The JIT's fields are in what a template's closures share (`closureCode`'s
  `jitCodeFields`, empty without the tag): `closure` is 56 bytes in both
  builds (`TestClosureSizes`).
- `Runtime`'s and `Realm`'s JIT fields sit at the end of their structs, so
  no offset the interpreter reads moves. `TestJITFieldLayout` checks both
  builds. The native handlers read the fields whose offsets the tag moves
  relative to a base no tag changes (`rtBase`, `ccBase`), so that one
  handlers' block serves every build.
- Measure the remaining flag tests with `placements`.

## Validation

```sh
go test ./internal/jit/... ./internal/vm                      # untagged
QUICKJS_REQUIRE_JIT=1 CGO_ENABLED=0 go test -tags quickjs_jit ./internal/jit/... ./internal/vm .
go test -tags quickjs_jit -race -count=1 ./internal/jit/...
go test -tags quickjs_jit -gcflags=all=-d=checkptr=2 ./internal/jit/... ./internal/vm
(cd internal/jit/verify && go test -tags quickjs_jit ./...)   # encodings, both arches
TEST262_DIR=/path/to/test262 go test -tags quickjs_jit ./conformance \
  -run TestConformance -count=1 -timeout 60m -args -conformance.jit
```

| Check | What it holds |
|---|---|
| `TestJITRegNative` (internal/vm) | Functions run so, the collector seen idle and marking. |
| `TestJITSameWorkWhileMarking` (internal/vm) | The marking rule, over the micro-benchmarks. |
| `ir.Validate`, `Evaluate` | The atomicity rule, on every program and as it runs. |
| `CheckAllocations` (mir) | Every compile's allocation against liveness computed again; the VM's tests turn it on. |
| `TestJITObjectLayout`, `TestJITFieldLayout`, `TestClosureSizes` | The fields native code reads, and that the tag moves nothing the interpreter reads. |
| `verify` | Both targets' encodings against `golang.org/x/arch`'s disassemblers, and the register rules. |

- `QUICKJS_REQUIRE_JIT=1` fails a native test job that would otherwise pass
  by falling back.
- The VM's JIT tests force promotion and assert native entry, so they can't
  pass in the interpreter.
- The JIT's tests run under `-d=checkptr=2` (CI's native-boundary job) and
  `GODEBUG=gccheckmark=1`, for the heap read in place.
- `-conformance.jit` keeps the normal thresholds, under which few test262
  tests reach native code. `QJS_JIT_STRESS=threshold,budget=3,deopt=7`
  compiles every function at its first call, polls every three back edges,
  and finishes every seventh poll's invocation in the interpreter; register
  code follows its threshold through `jitCallThreshold` (1), a function's
  first check compiling it (`jitRegHot`). `background=K` added compiles
  in the background, in every JIT runtime, and installs each compile exactly
  K checks after it was handed over: the window between build and install,
  the same in every run; `miss` with it has every register compile's worker
  leave its build as one that asked what its snapshot was not asked, built
  on the runtime's goroutine at its install, from feedback asked anew;
  `takeback` with it gives no compile to a worker, in place of the schedule,
  each taken back at its function's second ask, the path every compile takes
  where every worker is busy. The
  differential tests run so among their settings, and the background tests
  under `-race` (`-run 'Background|Differential'`).
- `compilediff native` compares two builds' compiles, and `jitprof` (in
  `verify`'s module) joins a CPU profile with the code, both from `v8bench
  -jitcode`'s file (AGENTS.md, "Validation"). The file is written by
  `vm.JITCodeHook`, which every compile calls.
- `v8bench -jittier FILE` writes the tier trace, V8's `--trace-opt` and
  `--trace-deopt`: a line for each compile placed, with what it was for
  (first, callee, deopt, learned, flushed, again, `+osr`), and each
  deoptimization that invalidates code, with what failed -- a property
  site by its kind and what its cache held (unfed, mono, poly, mega), or
  the instruction -- the code's age in invocations' worth (`jitRegAge`),
  the budget before the next compile, and where it failed, the function's
  own instruction or an inlined callee's by its register pc (`top` where
  the site was at its ladder's top already); and prints each benchmark's
  counts of them, with the most deoptimizations of one function and at
  one site. `vm.JITTierHook` writes it, called at placing and at
  `jitRegInvalidate`, never on a hot path.
- arm64's backend is checked on CI's Linux and macOS arm64 jobs and under
  qemu (`GOARCH=arm64 go test -exec qemu-aarch64`).
