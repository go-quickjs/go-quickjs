# A register interpreter

This is the design of the interpreter that is to replace the stack
interpreter (`executeAt`) and, where it measures faster, the tree tier: a
register machine in the manner of V8's Ignition and of Lua's VM, written in
Go, compiled straight from the AST by a backend of its own. It says what
the instruction set is and why, how the compiler allocates registers, how
the loop is written under Go's constraints, how frames, calls, exceptions,
generators, closures and inline caches work, what becomes of the tree tier
and the JIT, and the stages the migration goes through, each shippable and
measured, the old pipeline the fallback for whatever the new one does not
cover yet.

It starts from what was measured, because two earlier attempts at register
forms went badly and the reasons decide most of what follows.

## The measure

The interpreter's core operations run about twice as slow as a pure-Go
register interpreter we compared against: calls (`call_fib` 2.09x,
`call_mutual` 2.38x), a loop of local moves (`loop_locals` 1.85x), stores
(`prop_write` 1.75x, `array_write` 1.95x), globals (1.66 to 1.85x) and
callbacks from built-ins (`array_reduce_upvalue` 1.74x). Our runtime --
JSON, BigInt, number formatting, regular expressions, generators -- is far
faster than its, so a core that matches makes the engine faster overall.
The project's measure is the V8 suite's composite without the JIT on this
machine, 1,901 today: at least the comparison engine's, and jitless Node's
2,342. EarleyBoyer, Splay and RayTrace, the call- and allocation-heavy
benchmarks, are where the gap is.

Where our time goes was profiled test by test. The costs the design has to
remove are:

- **Operands not read in place.** In the tree tier `a = b` is two closures,
  a store calling a tree that reads the local: about 47 instructions and two
  indirect calls where a register move is about 25.
- **A Go call per node.** Go has no callee-saved registers, so each node a
  block calls reloads what it had live: about 14 instructions a statement,
  5 of them reloads, and 8 more for the callee's prologue and epilogue.
- **Stores that do more than store.** `prop_write` loads `this` it does not
  use and round-trips a float through memory to test a tag; `array_write`
  makes three Go calls a store and converts a constant key every time.
- **Globals.** Two Go calls, about eight dependent loads and three bounds
  checks a read: eval variables, the script's lexical bindings, the slot's
  attributes.
- **Calls.** Nine Go calls, seven of them indirect, for each JavaScript call
  of `call_mutual`, and about 41 ns of machinery: six barrier-checked
  pointer stores into a heap frame, the arguments copied, seven checks that
  reset what the frame's last user left, six stores into the tree context. A
  pure body runs in a stack evaluator whose `switch` is a quarter of the
  samples.
- **Callbacks from built-ins.** A chain five calls deep from `reduce` to the
  callback's tree, a `defer` and `recover` for each call, two interrupt
  ticks.

The profiling agent estimated that fixing the tree tier's nodes would bring
a simple node to about 2.4 ns, against the comparison engine's 1.5 ns a
register instruction. That is the bar this design has to clear, not a reason
to stop: the format has to beat what better nodes would give.

## What the experiments showed

**Fewer dispatches is not the measure.** On 2026-09-29 register forms were
added to the stack interpreter: one generic `bin_r` operation whose operands
were each a local or a stack slot, chosen by their sign. It removed 35% of
the dispatches in Crypto's `am3` and made Crypto 19% slower. Widening
`bytecode.Instr` for the extra operands cost 3% everywhere by itself. What
an instruction costs is the work it does beyond its operation: decoding
operand kinds, branching on them, the instruction's width, and where the
loop's code lands. An instruction that decides at run time what its operands
are, or which operator it is, has a second dispatch inside it.

**A translation is only as good as the stack form.** The first prototype
(`6ae14dca`) made register code from the stack code, over a symbolic stack
whose temporaries were homed by depth. It works -- the vm package's tests
pass with it but for five that pin tree-tier mechanisms -- and it is kept,
frozen, as a reference. It inherits the stack form's granularity (one stack
operation a step, fusions bolted on), its slot layout, its 16-bit sites and
its generic operators, and it cannot do better than what the stack form
says.

**The kernels, by hand.** The design's instruction set was then written by
hand for `am3`'s inner loop, `call_fib` and `loop_locals`, run in a loop of
its own, and timed against the stack interpreter, the tree tier and the
translation, at two placements of the code (`cff0c8ab`, micro-benchmarks,
best of 5, ns an operation):

| Test | Stack interpreter | Tree tier | Translated | Hand-written | Comparison engine |
|---|---:|---:|---:|---:|---:|
| `crypto_am3` | 208.6 / 216.1 | 103.8 / 104.7 | 105.6 / 107.0 | 101.6 / 101.6 | 81.6 |
| `loop_locals` | 32.8 / 32.9 | 20.9 / 20.9 | 12.3 / 12.7 | 11.6 / 11.6 | 11.3 |
| `call_fib` | 60.4 / 60.1 | 59.4 / 59.9 | 51.5 / 52.0 | 44.1 / 44.3 | 28.6 |
| `empty_loop` | 14.9 / 14.1 | 5.3 / 5.0 | 5.6 / 5.6 | 5.6 / 5.6 | 5.0 |

`am3` runs 25 register instructions a turn where the stack form runs 34:
twice as fast as the stack interpreter, where `bin_r` was slower than it,
because no instruction decodes anything. It is level with the tree tier,
whose int32 nodes were built for this loop. The comparison engine runs the
same 25 instructions -- its disassembly matches the hand-written code
nearly one for one -- in 81.6 ns: 3.3 ns an instruction against our 4.1.
`loop_locals` is level with it. Calls are the largest gap left, 44 against
29 ns: the frame and the entry, not the instructions, which are the same
nine for each recursive call in both.

**Go 1.27's loop-invariant pass hurts an interpreter.** The NaN that
`Float` canonicalizes to is a constant; the pass took it out of the loop,
once for each of seven uses, into seven XMM registers that every case
calling anything then saved and restored. Reading it from a variable instead
took `loop_locals` from 15.0 to 11.5 ns and `empty_loop` from 6.6 to 5.6.
Any constant the loop's cases use is read from memory or folded into an
instruction for this reason.

## Instructions

### Format

An instruction is one 64-bit word with fixed fields:

```
bits  0- 7  Op  the operation
bits  8-15  X   a small operand: an argument count, a flag
bits 16-31  A   a register, most often the destination
bits 32-47  B   a register, an index, a 16-bit immediate
bits 48-63  C   a register, an index, a 16-bit immediate or jump offset
```

B and C together are BC, a 32-bit index or offset. Every operation decodes
each field it uses with one shift and one zero or sign extension, the same
for every operation: no operand is ever of a kind decided at run time. A
register is 16 bits, so a function has 65,536 registers before anything
special is needed, where an 8-bit design has 256.

A 16-bit register costs nothing an 8-bit one does not: `regs[uint16(w>>16)]`
is a shift and a zero extension, as `regs[uint8(w>>8)]` is, and indexing a
pointer to an array of 65,536 values with a `uint16` needs no bounds check,
as indexing one of 256 with a `uint8` does not. What it costs is width:
eight bytes an instruction where a 32-bit design has four. Our stack
instructions are twelve bytes today. The common case is therefore as fast
as an 8-bit design's, which is what the hand-written `loop_locals` shows,
level with the comparison engine's 8-bit code.

### No limit

Nothing in a function is capped:

- **Registers past 65,535.** A function that needs more has them as far
  registers, reached only by two moves: `mov_far A, BC` copies far register
  BC (32 bits) to register A, and `mov_to_far BC, A` the other way. The
  allocator gives near registers to parameters, temporaries and the bindings
  used most, and far ones to the rest; an operation on a far binding is a
  move in, the operation, a move out. A function of that size is generated
  code, run once.
- **Sites, constants and names past 65,535.** An operation whose index would
  not fit has a wide form: the same operation followed by an extension word
  holding a 32-bit index, read by the wide form's case alone, so the narrow
  form's case never tests for it. Global reads, loads of constants and
  closures already take a 32-bit BC and need none.
- **Jumps past 32,767 instructions.** A compare-and-branch's offset is 16
  bits; one that would not fit is the opposite branch over a `jmp`, whose
  offset is 32 bits.
- **Calls of more than 255 arguments.** The count is X; a call of more is
  its wide form, with the count in the extension word.

Ignition does this differently: 8-bit operands by default, and the `Wide`
and `ExtraWide` prefixes that make the next instruction's operands 16 or 32
bits. V8 generates each handler three times, once per operand scale, and
dispatches the prefix as an instruction of its own. In Go the prefix is a
second dispatch, and three copies of each case would triple the loop past
the size at which Go stops inlining into it (below). Ignition saves bytes,
which V8 counts carefully; at eight bytes an instruction we spend some, and
in exchange the 16-bit case is the common one and the far forms are rare
and separate.

### No accumulator

Ignition keeps one operand of most operations in an accumulator, which
makes its bytecode small: `Add r1` adds r1 to the accumulator. In V8 the
accumulator is a machine register the generated handlers keep across the
dispatch. In Go it would be a `Value`, two machine words, carried around a
loop in which every case that calls anything clobbers every register: Go
has no callee-saved registers, and its allocator spills a value live across
the loop at its definition. The kernels show what that costs: the loop's pc
and instruction word, which have to be live across the dispatch, are
already stored to the stack at every instruction. An accumulator would add
two stores and two loads to every instruction, and two or three dispatches
to every expression -- `l = xl * l` is `Ldar xl; Mul l; Star l` where three
address code has one `mul`. At 3 to 4 ns a dispatch in Go, against about
1 ns in V8's handlers, the dispatch count dominates. The set is three
address: an operation reads its operands from registers or immediates and
writes its result to a register.

### The operations

Each hot operation is an opcode of its own, as Ignition's `AddSmi`,
`BitwiseOrSmi` and `ShiftRightSmi` are, so that no hot case switches on an
operator held in the instruction. A generic form that names its operator is
kept only for cold operators (`**`, `in`, `instanceof`, `typeof` beyond its
fused tests). The families:

- **Moves and loads:** `mov`, `load_int` (a 32-bit immediate), `load_const`,
  `load_undef`, `load_null`, `load_true`, `load_false`, `load_empty`,
  `load_hole`.
- **Arithmetic:** `add`, `sub`, `mul`, `div`, `mod` of two registers;
  `add_imm`, `sub_imm`, `mul_imm` with a 16-bit immediate; `inc`, `dec`,
  `neg`, `to_numeric`. Each has a number fast path in its case and calls out
  for anything else.
- **Bitwise:** `bit_and`, `bit_or`, `bit_xor`, `shl`, `shr`, `ushr`, and
  each with an immediate, and `bit_not`, with an int32 fast path; the
  immediate forms are what Crypto's masks and shifts are.
- **Comparisons that branch:** `jlt`, `jle`, `jgt`, `jge` and their
  negations `jnlt`, `jnle`, `jngt`, `jnge` -- different from each other where
  a NaN is compared -- with a register or an immediate on the right, and
  `jseq`, `jsne`, `jeq`, `jne`, `jnullish`, `jundef`, `jtypeof` (a typeof
  compared with a literal): a comparison that decides a jump never makes a
  boolean.
- **Comparisons that produce a value:** `lt`, `le`, `gt`, `ge`, `seq`,
  `sne`, `eq`, `ne`, cold enough to share one generic case.
- **Jumps:** `jmp`, `jtrue`, `jfalse` (ToBoolean, a boolean's without a
  call). A jump with a negative offset counts the interrupt budget.
- **Bindings:** `get_upval`, `set_upval`, `check_hole` (the dead zone's
  ReferenceError, naming the binding), `close_upvals`.
- **Globals:** `get_global` and `set_global` carry their site, whose entry
  remembers the slot; a strict store is its own operation.
- **Properties and elements:** `get_prop`, `set_prop`, `get_method` (which
  writes the method and the receiver into a call's window in one
  instruction), `get_index`, `set_index`, `get_index_imm`, `set_index_imm`
  (an immediate index: `tab[3] = j` with no load of the 3), `get_length`,
  `define_field`, `new_object` (from the literal's site and its first
  values, as one instruction), `new_array`.
- **Calls:** `call`, `call_undef_this`, `new`, `ret`, `ret_undef`,
  `call_spread`, `apply_args`, `tail_call`, `closure`.
- **Iteration, exceptions, generators** in later stages: `for_in_next`,
  `iter_next`, `throw`, `rethrow`, `yield`, `await`, `resume`.

The opcode is masked to the switch's size and every value of the mask has a
case, so the jump table needs no range check: today 7 bits, with the unused
values padded, growing to 8 bits when the set passes 128.

### Instructions a turn

What the core tests execute in the inner loop, in the stack form today, in
this set, and in the comparison engine's code (from its disassembler):

| Test | Stack form | This set | Comparison engine |
|---|---:|---:|---:|
| `empty_loop` | 4 | 2 | 2 |
| `loop_locals` | 9 | 6 | 6 |
| `call_fib` (loop) | 12 | 7 | 7 |
| `call_fib_rec` (recursive / base) | 11 / 6 | 9 / 2 | 9 / 2 |
| `call_mutual` (each call) | 8 | 6 | 6 |
| `prop_write` (4 stores) | 12 | 6 | 6 |
| `array_write` (10 stores) | 44 | 12 | 12 |
| `global_read` (4 reads) | 20 | 10 | 10 |
| `global_write` (4 writes) | 12 | 6 | 6 |
| `method_call` (loop) | 10 | 6 | 7 |
| `array_reduce_upvalue` (callback) | 6 | 5 | 5 |
| Crypto `am3` | 34 | 25 | 25 |

The counts are the same as the comparison engine's but where `get_method`
saves its move into the call window, and where immediates save a load of a
constant it keeps in a register. The difference between the engines is not
the count: it is what each instruction and each call costs.

## The compiler

### A backend of its own

The register code is compiled from the AST by a backend beside the stack
compiler, as V8's `BytecodeGenerator` compiles its AST. It shares with the
stack compiler what is not code generation: parsing, early errors, and
scope resolution -- which name is a binding of this function, an upvalue
and of which descriptor, a global; which bindings a closure captures; what
a direct eval or a debugger can see. The stack compiler records its
resolution of each identifier and declaration as it compiles a function,
and the register backend reads that record instead of resolving names
again, so the two forms of a function agree on every binding by
construction.

The backend runs when the stack compiler finishes a function, while its AST
is at hand, and only where the register tier is on. A function holding
anything the backend does not cover yet is left without register code and
runs as it does today.

### The frame

A frame is a window of the runtime's value stack:

```
r0            the function being called
r1            this
r2 ...        the parameters, in order
              the function's bindings that live in registers
              the temporaries
```

The function and its `this` are registers because a call's window is the
callee's frame: the caller writes the callee to the window's first
register, the receiver to the second and the arguments after them, and the
callee's registers start there. Nothing is copied. A parameter no argument
was given for is made undefined by the call; arguments past the parameters,
which only `arguments` and a rest parameter read, are moved aside first by
the functions that have them.

Bindings take registers in the order the backend meets them. A binding a
closure captures is a register too: the closure's upvalue points into the
window while the frame lives and is closed when it returns, as now, which
works because the value stack never moves. The debugger and stack traces
find a binding through a table of the function's that maps each name to its
register and the range of pcs it is in scope for.

### Register allocation

Temporaries are allocated in scopes, as V8's `RegisterAllocationScope`
does: an expression's operands take registers above those live when it
began, and its scope frees them when it ends, so temporaries are a stack
and the frame's size is the deepest it gets. A call allocates its window
last, at the top of what is live, so that the callee's frame, which starts
at the window and runs past it, overlaps only registers the caller has
finished with.

Code generation is destination-driven: an expression is compiled into the
register its consumer names -- `s = s + x` is one `add` into `s`'s
register, `return a + b` one `add` and a `ret` -- or, where the consumer
only reads it, into whatever register already holds it, which for a binding
is the binding's own. An operand is read where it lies.

Reading a binding in place has one hazard: a call or anything that runs
code between the read and the operation could change it. `x + f()` reads
`x` before `f` runs. The backend copies such an operand to a temporary
first when the rest of the expression could assign it -- an assignment to
the same name in it, or a call where the binding is captured by a closure
that writes it. Everything else is read in place.

### compilediff

`compilediff` compares what the front end makes at two revisions. While the
stack compiler is the source of truth for the stack form, a register backend
change must leave the stack code exactly as it was: compilediff's verdict
of no difference over the test262 tree and the suites is the proof, and the
register code is compared the same way between two revisions of the
backend, by its own listing. Once the stack form is retired, compilediff
compares register code alone.

## The loop

The interpreter is a loop of its own, `runReg`, in files that sort after
the package's other files (`zzreg_*.go`): `executeAt` and the other hot
functions keep their code and their addresses, and the closures move by
the size of what is added, which the placements measure. Its techniques are
the ones Go allows:

- **The register window is unchecked.** The loop holds a pointer to an
  array of 65,536 values at the frame's first register, so `regs[w.A()]`
  has no bounds check. A verifier proves, for every function, that every
  register an instruction names is below the frame's count, and the frame
  is given that many slots before it runs.
- **The code is read unchecked,** at a pc the verifier proves is in the
  code: every jump lands on an instruction, and the last instruction does
  not run on past the end.
- **The switch is a jump table with no range check,** the opcode masked and
  every value of the mask a case: Go makes a jump table only of a switch
  whose cases are dense, and checks its range unless the value is provably
  inside it.
- **Nothing of an instruction is live across a call.** A case that calls out
  keeps the pc in a memory record and reads it, and its instruction, back
  afterwards, so that Go has no reason to save them at every dispatch.
  Slow paths are out of line and re-read their instruction from the code.
- **The loop stays below Go's big-function size.** Go's inliner calls a
  function of 5,000 or more IR nodes big and inlines only callees of cost
  20 or less into it; past that, `regInt32` (cost 41, 18 calls),
  `regTruth`, `Value.IsString`, `propCache.holder` and a dozen more become
  calls in every case. The count is of the loop's own source, before
  anything is inlined into it: a helper inlined costs the loop the few
  nodes of its call, not its body. `TestRegLoopIsNotBig` pins it, as
  `TestIntendedInlining` pins the helpers, and `internal/cmd/bigfunc`
  measures it: `go run ./internal/cmd/bigfunc -func runReg` prints the
  count against 5,000, `-cases 40` each case clause of 40 nodes or more,
  and the callees above cost 20 the loop would call if it were big. The
  budget is spent with care, a hot case at a time:
  - **The compare-and-branches share their taken tail.** A case is a
    fast path and a `goto`: to `slow` (the slow path, `regSlow`),
    `cmpSlow`, `typed`, `jump`, or `taken`, a compare-and-branch taken,
    whose offset `SC` the tail reads. The shared `taken` took 167 nodes out
    of the 29 compare-and-branches' `off = w.SC()` and 49 instructions out
    of the loop.
  - **The slow calls are not shared.** `set_global`, `set_index`'s miss
    and the operations with no fast path each keep their own call of
    `regSlow`. Sent to the one `slow` tail, they saved 45 nodes and made
    `int_arith`, `float_arith`, `loop_sum` and `loop_props` 7 to 11% slower
    at every placement, the inner ones (`regInner`, `regTail`) included,
    with every instruction on those loops' paths the same: the loop's
    blocks laid out otherwise. A tail that stores the pc (`slow`'s
    `kept.pc = pc`) is reached after a call only with the pc read back
    from `kept`: set_index's `goto slow` after `regSetMiss` without it made
    the pc live across the call, Go saved it at every dispatch, and
    `crypto_am3` and a dozen loops were 8 to 11% slower at every
    placement. `TestRegDispatchKeepsNoPC` disassembles the loop and fails
    if its dispatch stores to the stack.
  - **A case costs what its source says.** The compare-and-branches are
    about 53 nodes each, the bitwise operators 73 to 77, a fused element
    read or update 100 to 128, `append` (a call of `regAppend` with the pc
    kept) about 20.
  - **A helper that returns a bool with values does not fold.** A
    `regNums(a, b) (x, y float64, ok bool)` in the compares saved 96 nodes
    and added 40 instructions: its `ok` is materialised and tested where
    the written-out `&&` branches. A saving is checked with `hotdiff
    -funcs '(*Runtime).runReg'` against the build before it.
  - At d931057d the loop was at 4,981, 19 below the limit; with the
    shared taken tail and `append` and `str_freeze` as cases of their own
    it is at 4,875, 125 below.
- **The hot code is one block.** `zzreg_run.go` holds, declared first
  and in this order, `runReg`; the call and its return, `regCallW` and
  `regEnterW`; what a call runs out of line, `regDirect` (a built-in's
  callback), `regNative`, `regLeafRun`, `regCloseUpvalues`; and what an
  iteration runs out of line, `regSlow` and its operators, `regElemMiss`,
  `regSetMiss`, `regBackEdge`, `regAppend`, `regCharCodeAt`,
  `regIterNext`, `regLit`, `regMarkedCall`. Go lays a package's functions
  out in file order, so the distances between them -- which decide what
  shares the op cache's sets -- depend on their own sizes alone, and the
  layout lock's lever Q (`zzreg_ru_layoutpad.go`, the anchor `runReg`)
  puts the block at a fixed address modulo 4 KB whatever changes before
  it; the placements' pad before it (`zzreg_ru_pad.go`) moves it whole.
  It stays where the register files were, after the JIT's, not first in
  the package: there it moves none of the old tiers' code, whose anchors
  all come before it, and only the A pad would change to take the
  built-ins back behind 80 KB of it -- and the files after it (`zzz_*`)
  are pinned by nothing either way.
  Before, the block was spread over `zzreg_call.go`, `zzreg_run.go` and
  `zzreg_switch.go`, with the cold `zzreg_report.go` between: a change to
  a miss path moved `regSlow`, `regIn` and `regNative` against `runReg`,
  and the tail calls' 4.8 KB in `zzreg_call.go` made `forward_loop` 11%
  slower at every placement with no instruction of its path changed. New
  hot code -- what a call, a return or a loop's turn runs -- goes into
  the block, after what is there; cold code -- miss paths, `regCold`,
  set-up, the report, the tail calls (`zzzy_regtail.go`) -- never does.
  The set-up and the entries from a tree are in `zzreg_entry.go`. An
  unrelated function added before the block, with the pads regenerated,
  leaves every function of the block where it was.
- **No constants in registers.** What Go's loop-invariant pass would take
  out of the loop is read from a variable (above).
- **The loop holds only the hot operations.** Every case added moves the
  code of every case after it, and the placements, which move the loop as
  a whole, cannot vary that: each case is a lottery for unrelated tests.
  An operation that does not run per turn of a hot loop -- a switch's
  table, `typeof` as a value, an empty literal, what sets a frame up --
  is a label of the one case that calls `regCold`, which switches on the
  operation again. Measured: the switch operations as cases of their own
  made `prop_update` 8.9%, `call_fib` 8.7% and six other tests 5 to 7%
  slower at every placement; behind `regCold`, all are level and the
  switches keep their gain.
- **Opcode values are for per-iteration hot steps.** The 7-bit space holds
  128 values, each a case. An operation that runs per turn of a hot loop
  takes a direct value of its own, out of the free list (`runReg`'s
  `goto invalid` case, which lists them; a value taken out of it and left
  there does not compile). Every other operation -- stage 2's handlers
  and iterator steps but the hot ones, a literal's set-up, a rare
  conversion -- is `cold`, one value, its sub-operation (`RColdOp`) in X,
  dispatched a second time (`regColdSub`). `cold` is a value of the slow
  path's case, whose call of `regSlow` already returns an error: with an
  error check added to `regCold`'s case instead, `empty_loop` was 7.5%
  and `string_build1` 6.4% slower at every placement, and without it
  level (the loop's blocks laid out otherwise, every instruction of those
  loops the same). A value freed goes to the end of the free
  list, and no hot operation is renumbered: its case's code would move.
  At the removal of the translation reference the free values were 24,
  29, 36, 37, 40, 41, 42 and 43, and 127 is `invalid`'s; `iter_next`
  took 24, `regexp_lit` 29, and the tail calls 36 and 37.

What remains is Go's: a register store of a `Value` is a write-barrier
check, the loop's instruction word is still saved at the dispatch where a
case's register pressure needs it, and every case ends in a jump back to
the one indirect branch. A case that would call with the pc live -- a
jump's truthiness or `===` of strings, a throw's `pc-1` -- answers what
needs no call inline and leaves the rest to the compare's slow path, or
passes the loop's pc rather than `pc-1`, which Go would keep from before
the increment: with none left, the dispatch stores no pc. `am3` runs at
3.6 ns an instruction against the comparison engine's 3.3.

## Frames and calls

A call between two register functions is made in the loop's case and a
function it calls, `regEnter`, without the generic call path:

- the callee checked to be a closure of this realm with register code and
  nothing of its surroundings to establish -- not bound, not a class
  constructor called, not an arrow sharing a derived constructor's `this`;
- the window is the callee's frame: its parameters are the arguments;
- the frame record is a few words: the function, the window's base, the pc,
  the caller's record. It lives on the Go stack in `runReg`'s variables,
  and in a table of the runtime's for stack traces, as the comparison
  engine's `frameInfo` is: one pointer store a call where the heap frame
  takes six barrier-checked stores and seven checks;
- the interrupt and depth checks are one decrement and one compare.

The register interpreter recurses on the Go stack, a call of `runReg` for
each JavaScript call, as the stack interpreter does. Running calls inline
in the loop was measured in the stack interpreter and lost 3.7 to 5% (it
spills more); it is not retried until the frame is small enough to change
that answer.

A built-in calling a callback enters the register code directly: the
arguments, which are a Go slice, are copied into a window at the top of
the stack, and `regEnter` runs the callee, with no tree, no `defer` and no
`recover`. A register function calling a built-in passes it the window's
arguments as its slice, without copying.

Stage 1 keeps the heap frame for what reads it today -- stack traces, the
debugger, closures' `this`, errors' positions -- and writes only the fields
those read. Moving every reader to the lean record is stage 2.

## Exceptions

An exception is an error value returned, never a panic: a Go `panic` and
`recover` cost microseconds where a returned error costs a compare. A
function with handlers has a table of them (`RegCode.Handlers`, innermost
first), each a list of ranges of pcs, the handler's pc and the register
the exception's value goes to. Nothing runs as a try block is entered or
left: no push and no pop, as the stack form's `push_catch` and `pop_catch`
are.

- **Every way out of the loop with an error goes through one tail,**
  `fail`, after the switch. A case that can throw stores `kept.pc` before
  it calls out, as every case with a call already did, and on an error
  sets `failed` and jumps to the tail; the tail calls `regCatch`, which
  looks the instruction at `kept.pc-1` up in the table and returns the
  handler's pc, its value written to the handler's register, or -1, and
  the loop returns the error to the caller's case, which does the same.
  `TestRegThrowsKeepPC` reads `runReg`'s source and fails where a `goto
  fail` has no `kept.pc = pc` before it in its case or tail, or where the
  loop returns an error anywhere but from the tail. The tail and the
  stores cost the loop 44 nodes (4,726 to 4,770); the pc is still never
  live across a call. Each case calling `regCatch` itself instead, with
  no tail, cost 177 nodes more (53 below the limit) and was level over
  the placements: nothing 3% or more slower at every placement either
  way, the geometric mean 0.8% faster, `crypto_am3` -4.3% and
  `empty_loop` -5.7% (each slower at one placement of four),
  `global_write` -1.2%. The shared tail stays, for the budget. Against
  s2, before the tail, `global_write` and `global_write_strict` are 4.4%
  slower at every placement with either form: not the tail's.
- **What a handler catches is what the interpreter's catches**
  (`unwindToHandler`): a JavaScript exception (`*Thrown`), whatever threw
  it -- a throw statement, an operator, a getter or a setter, a built-in,
  a call of either tier, a stack overflow's RangeError. An interrupt or a
  memory limit goes on past every handler. A call that threw has left
  the stack and the frame records as they were before it (`popFrameOf`),
  so the handler needs no unwinding of its own.
- **A finally clause is compiled once for each way out of what it
  protects** (regcomp's `try.go`): in place after the block's normal
  completion; before each break, continue or return that leaves it, the
  return's value taken first into a register of its own; and as the
  handler for a throw, which throws the value again at its end. There is
  no completion record and no dispatch on one: each copy goes on as its
  exit does. A copy before an exit is outside the ranges of the handlers
  the exit leaves and inside those of the handlers around them, so that a
  throw from a finally clause run by a break is caught by the try around
  the loop and not by its own catch. Copies nest at most four deep (a
  clause inside a clause is compiled for each copy of the outer); deeper,
  the function stays the stack form's.
- **The catch binding** is its slot's register, which the stack compiler
  records for the clause (`Record.Catches`), or a temporary where nothing
  reads it. A catch clause's pattern, a binding a closure captures in a
  loop (each turn's is a new one) and a using declaration's scope stay
  the stack form's.
- **A throw statement with a handler around it in its function** is a
  move of its value into the handler's register and a jump to it, with
  no `Thrown` made, as the tree's `throwToHandler` does: through the
  table, `try_throw` was 33 to 128 ns (the frames captured for nothing).
- **for-of and for-in loops** keep their cursor in a register of their
  own: `for_of_start` or `for_in_start` makes it (`startForOf`,
  `startForIn`, with their fast paths for an array and a generator) and
  `iter_next` steps it at the head of each turn, jumping out where it has
  no value. The two starts are cold operations; `iter_next` is the one
  direct value stage 2 has taken (24): through `cold`, `regSlow` and
  `regColdSub` were 16% of `generator_for_of`, and as a case (a call
  with the pc kept, 30 nodes) `array_for_of` went from 17.3 to 13.2 ns
  and `generator_for_of` from 93 to 83 in one build. `regIterNext`
  reads an array's element in its dense storage as the tree's
  `iterNextTree` does, and steps a generator's cursor as the tree does
  (`stepGenTree`, resumed in place) under a recover of its own
  (`regIterNextTree`), since a generator's body throws as the tree
  throws, with a panic. Any other iterator's `next`, where it is a
  function of script, is called as register code calls (`regStepNext`,
  through `regCall`): through `iterNext`'s `stepIter` it was called from
  Go, `callObject`, `runFD` and `runTree` making the frame and the
  register function's one tree node entering it, and `iterator_for_of`
  was 257 ns against the tree's 196 (208 so). A Map's or a Set's
  iterator is stepped as the tree's `iterNextTree` steps it
  (`mapIterStep`). A call of a generator function from register code is
  made at its site (`regCallGen`, `regGenCall`: `newGenerator`, the
  receiver coerced as `callClosure` coerces it), not through `regCall`
  and `callDirect`: `generator_create` 480 to 404 ns, the tree 430. The
  step's recover goes when a generator's body is register code (stage
  2, item 3): without it `generator_create` was 383. A key string a for-in handed out is read by
  `get_index`'s slow path as the tree reads it (`stringKeyRead`), and
  `f.call(t, ...)` goes to `callThrough` without the generic call's
  search (`has_own_call_for_in`). A for-of loop's body is in the
  ranges of a handler of its own, the cursor's, which closes the iterator
  as a throw does (`iter_close_throw`, its return method's error dropped)
  and throws again; a break, a continue of an outer loop or a return that
  leaves the loop closes it on the way out as it runs a finally clause
  (`iter_close`, its error thrown), and `iter_next` is outside the
  ranges, so that an iterator whose `next` threw is not closed. A for-in
  cursor needs no closing. A pattern or a member as the target, a
  captured lexical binding (each turn's is a new one) and `for await`
  stay the stack form's.

## Generators and async functions

A generator's frame is its window. Suspension copies the window and the pc
into the generator object, and resumption copies them back to the top of
the stack and continues at the pc, as the stack form does with its operand
stack -- but there is no operand stack, so the copy is the frame's
registers alone. An `await` whose operand is already settled, or not a
thenable, with no job waiting, continues without suspending at all: the
profiling found every await fully suspending and resuming, which is most of
`async_await`'s 3.65x and is a defect to fix in the existing interpreter
first.

## Closures and upvalues

A closure captures a binding by pointing an upvalue at its register while
the frame lives; the frame's return closes it, copying the value into the
upvalue. A binding a closure captures from inside a loop with `let`, which
needs a fresh binding each turn, is closed at the end of each turn by
`close_upvals`. A binding captured by value -- a constant, a strict
parameter no one assigns -- is copied into the closure as now. The `closure`
instruction carries, for each of the new function's upvalues taken from its
maker, the register of the binding, so that the function's template needs
nothing of the maker's layout.

## Inline caches

Every property, element, global and call site has an entry in the
function's caches, numbered by the backend with no 16-bit limit (the wide
forms above). The loop checks a site's entry in the case, with one test
for a hit, and calls out for anything else:

- **A property read or write** compares the object's shape with the entry's
  own shape and reads or writes the slot; a prototype's property, an
  accessor, a polymorphic or megamorphic site is out of line. This is the
  existing `propCache` and its `ownShape`, which both forms share.
- **A global** reads or writes the slot its site remembers while the global
  epoch is the one the site was filled at: the shape tree's `globalEpoch`,
  which the JIT's global reads trust too, advances when a binding of the
  global object moves, goes or changes its attributes and when a
  script-level lexical binding is declared, which may shadow a global. A
  site is filled at the epoch only for a plain data property -- a writable
  one for a write -- of a global object of a layout of its own, which no
  lexical binding shadows (`regNoteGlobal`); the epoch, kept per closure
  template beside its sites (`regEpochs`), is one compare where the read
  compared the layout and looked the name up in the lexical table.
- **An element** of a dense array is read or written inline for an index
  that is an int32 in range; holes, typed arrays and the rest are out of
  line.
- **A call** compares the callee with the function the site last called and
  enters its register code without asking again what kind of function it
  is.

## The tree tier and the JIT

During the migration the tree tier and the JIT keep consuming the stack
form, which the stack compiler still emits for every function: a function
with register code is run by the register interpreter, and a function
without it by the tiers that run it today.

Whether the tree tier is still needed is a measurement, not a choice. The
kernels show the register interpreter level with it on `am3`, the loop its
int32 nodes were written for, and 1.8x faster on moves and 1.35x on calls.
When the register interpreter is at least level with the tree tier on every
suite and micro-benchmark, plain and at `GOMAXPROCS=1`, the tree tier is
retired; until then each function runs on whichever the measurements
choose, and the int32 and bitwise paths that make Crypto fast stay where
they are.

The JIT builds its SSA from the stack form today. It moves to reading
register code, which maps more directly onto SSA -- a register is a
variable, an instruction one value -- and its deoptimization then rebuilds
a register frame, which is simpler than an operand stack because every
value has a name. That is a stage of its own, after the interpreter covers
the language, and the JIT's README is updated with it.

## The debugger, eval and with

- **The debugger.** Code compiled for a debugger has a `debug` instruction
  at each statement, where a pause or a breakpoint can happen, as the stack
  form's `debug_stmt`; the binding table above tells it which register
  holds each name in scope. A debugger's assignment to a binding writes the
  register.
- **A direct eval** reads and writes its caller's bindings through
  descriptors that name their registers, as they name slots today.
- **A `with` body** resolves its names dynamically; the function keeps the
  stack form's dynamic lookups for names inside one, as operations of the
  register form's own.

All three are the last stage's: until then a function with a debugger, a
direct eval or a `with` runs as now.

## Stages

Each stage lands on its own, is measured against the one before by the
placements, plain and with the JIT, and keeps test262 at 100,247 passed and
none failed in every mode -- plain, with the JIT, with every runtime made
`WithDebugger`, and in the register tier. What a stage does not cover runs
on the old pipeline -- until the end state, in which the stack interpreter
and the tree tier are removed. So no new feature is left to them, not for
a form, not for a while, not because the tree is faster: what is added
runs entirely as register code. The fallbacks there are -- regcomp's
refusals (the inventory below) -- are a list to burn down, not a
design. The tree tier is
the floor to beat and the place to port mechanisms from, never a place to
send code.

### Stage 0: the evidence (done)

The translation reference (`6ae14dca`) and the hand-written kernels
(`cff0c8ab`) above: the operations are specialised, the loop is its own,
the register window and the code are unchecked, the switch has no range
check, and nothing a case keeps is live across a call.

Both were removed once the backend (regcomp) covered what they measured:
the translation (`internal/compiler/regform`, `QJS_REGFORM`), the kernels
(`QJS_REGHAND`) and the eight operations only the translation emitted --
`closure`, `bin_imm`, `jmp_cmp`, `jmp_cmp_imm`, `call`, `call_method`,
`new` and `close_upvals` -- with their cases, slow paths and tests. Their
figures stay recorded above; the commits named there keep the code.

### Stage 1: plain functions

- The AST backend for plain functions and arrows: simple parameters,
  `var`, `let` and `const` of identifiers, expressions, `if`, the loops
  without `for-in` and `for-of`, `break` and `continue`, `return`, calls,
  `new`, object and array literals, closures. Anything else leaves the
  function without register code.
- The loop at the comparison engine's cost an instruction on the kernels:
  the dispatch's saves removed, the big-function size pinned by a test.
- `regEnter` with the lean frame record for calls between register
  functions, and the direct entry from built-ins' callbacks.
- Globals with the lexical epoch; `get_method` into the call window;
  immediate element indices.
- The register tier opt-in (`QJS_REG`), then the default for the functions
  it covers once its measurements show a net gain.

To ship, it needs: test262 in all four modes with the register tier on, its
own tests for each mechanism (a count of register functions built, a call
made without the generic path, a site filled once), the micro-benchmarks
and the V8 suite over the placements, and the hot functions unchanged by
`hotdiff`.

#### Where stage 1 stands

The backend (`internal/compiler/regcomp`) compiles from the AST and the
record of the stack compiler's resolution (`regrecord.go`), and covers
nearly every function of Richards, DeltaBlue, NavierStokes, Splay and
RayTrace; `disasm -reg` lists its code beside the stack form's, 20 to 35%
fewer instructions in their methods. The stack form is unchanged:
compilediff against `ce98c3cf` writes the same output. test262 in register
mode passes what the default build passes on this checkout.

What the measurements found, against the design:

- **The loop's size is a cliff, not a slope.** With the specialised
  compare-and-branches each carrying its own slow path, `runReg` passed
  Go's big-function size, and every helper of cost above 20 became a call:
  `regInt32`, `lexShadows`, `regTruthy`. `crypto_am3` went from 102 to 137
  ns. The cases now share three tails after the switch -- the slow path, the
  compare's slow path, the taken jump -- reached by `goto`, and a case is a
  fast path and a jump.
- **Store forwarding hides the loads Go keeps.** A `struct` the loop stores
  its pc to before a call is forwarded to the load after it unless the
  callee is given the struct's address; given it, the pc is no longer live
  across the call. The dispatch still saves the pc and the instruction
  where a case's fast path needs more registers than there are; the
  comparison engine's dispatch saves three values too, so this is not the
  difference between the two.
- **Numbers as float64 cost index-heavy code.** NavierStokes runs as
  register code throughout and is 27% slower than the tree tier, which
  keeps an int32 it has proven in a Go variable; every `get_index` here
  converts its key from a float. The tree tier's int32 paths stay where
  they win: a function the tree builds with int32 nodes keeps its tree.
- **A per-function fallback multiplies the boundaries.** EarleyBoyer and
  Splay are 39% and 30% slower: a tree-tier function calling a register
  function goes through `callTree`, `runTreeNested` and a copy of the
  window, and the reverse through `callDirect`; the gate's "fast boundary"
  needs the tree's call nodes to enter register code as `regDirect` does,
  and constructors (`new`) to run as register frames.
- **Fusions the stack form has are owed.** `prop_update` (`o.a += j`) is
  three instructions a statement here against one tree node, 16.6 against
  6.4 ns; the backend needs the update of a place as one operation, as the
  tree has it. (Done since: `add_prop`, 5.8 ns.)
- **A call's cost was its general path's.** `regCallW` was 30% of
  `call_fib`, most of it spills of the paths a cache hit never takes. The
  hit is now the whole of `regCallW`: the callee's tag and pointer read as
  words (read as a `Value`, the tag went through a float register and the
  stack to be compared), the cache's kind saying what to write or check of
  the this, the frame's facts read from the register code (new.target
  read, registers captured), and the call's value written to `R[B]` by the
  call, so the loop keeps nothing of the instruction across it. A first
  call, a leaf, a frame keeping its arguments and a this to coerce are out
  of line. `call_fib` 39.2 to 32.3 ns, `method_call` 64.2 to 48.7,
  Richards -11%, DeltaBlue -20% over the placements.
- **Index-heavy loops need the tree's fusions.** `x[++i]`
  (`get_index_pre`), a compare-and-branch with an upvalue (`jle_upval`, a
  loop's bound a closure captures) and an element's store made in the
  binding its value goes on to take `lin_solve`'s inner loop from 19
  instructions a turn to 13; NavierStokes -10% over the placements.
- **The pc saved at every dispatch came from three cases.** jfalse and
  jtrue called `Truthy`, `===` called `StrictEquals`, and the dead-zone
  check passed `pc-1`. With those answered inline or on the slow path
  (`regTruth`; `===` of two numbers or of a value compared by its words),
  Go stores only the instruction word at the dispatch: `crypto_am3` 100 to
  91 ns, NavierStokes -5%, Crypto -6%, the V8 total -3.9% over two
  placements.
- **A call's window is filled by the read of its method.** `get_method_w`
  reads `o.m` of a receiver in a binding and puts the receiver in the
  window beside it, the move gone (`method_call` 49 to 45 ns). A leaf
  callee is answered in `regCallW` before anything of the frame is
  written, and a pure body the frameless evaluator has given up on leaves
  the site's leaf kind. `method_call_leaf` stays near 38 ns against the
  tree's 31: the tree's one node is six register instructions here.
- **Trivial bodies and built-ins at the call site.** A body whose first
  instruction returns a parameter, or nothing, is answered by `regCallW`
  from the window (`regLeafArg`, `regLeafUndef`), not by the frameless
  evaluator: `method_call_leaf` 40 to 25 ns against the tree's 31. A
  built-in of this realm is a kind of the site's cache (`regCallNative`),
  called by `regNative` as `callDirect` calls one -- Math's functions,
  push and pop and the native leaves without a frame -- rather than
  through `regCallSlow`, `regCall` and `callDirect`'s search:
  `array_push` 70 to 52 ns, `array_pop` 64 to 45.
- **Strict global stores and lazy arguments.** A strict store of a value
  that runs no code asks at the store whether the name resolves
  (`set_global` with X=1), as the stack form's folded `set_global_strict`
  does; `arguments.length` and `arguments[i]` of a function whose object
  is made on demand are `args_len` and `args_index`. The V8 suite has no
  refusal left that costs a crossing.
- **x[i++] is one operation** (`get_index_post`). First measured slower
  (`crypto_am3` 91 to 101 ns at two placements) against a base whose pads
  were off their targets; with both builds' pads regenerated, at four
  placements, `crypto_am3` is 2% faster and Crypto 1 to 4% at every one.
  NavierStokes, which has no `x[i++]`, is 23% slower at one placement and
  level at the others: the case moves the loop's code, which that
  placement puts on a bad alignment (the second of `-k 4`'s placements,
  placement 3: 54.7 to 67.5 ms, reproduced in two runs). The cliff is in
  the layout of the cases inside `runReg`, which the lock does not hold;
  it is left to the step that lays out the loop, the call routine and the
  entry as one block the lock anchors. `get_method_w`, re-measured the same
  way, stays: without it `method_call` is 7% slower at every placement and
  the V8 total slower at every one. The
  loop's own alignment moves the small loops: `loop_locals` is 10.4 to 11.3
  ns with `runReg` at 32 mod 64 and 13.1 at 0, which the placements do not
  vary.
- **What the tree's mechanisms gave, register code gives too.** The
  package's tests in register mode found wrong answers and costs the
  differential test had not: a trace read the heap frame's this and
  new.target, which the lean call does not write -- a frame's `regKind`
  now says they are its window's -- and placed a call at its expression's
  start; a block's `let` hid a function expression's own name; an apply
  other than the built-in was given a new arguments object each call,
  now one kept in the binding's register (`apply_args`'s C); a computed
  key both read and written ran an object's toString twice, now
  converted once (`key_of_base`, the stack form's
  `to_property_key_of_base`), which a key an operator made, a
primitive, does without. The tree's
  mechanisms the register code lacked are its own now: `concat` and
  `str_part`, the stack form's string chain, one string made where an
  addition made one each; closures that take a value by value
  (`newClosureByValue` reading registers); a loop's function expression
  made once (the record's `Reused`); `in` through the realm's cache; and
  a leaf's property reads and stores on the stack form's sites
  (`propSite`), which the frameless evaluators read, so that a run of its
  register code fills them. With the tree tier off, the stack
  interpreter runs everything, register code included.
- **What a call answers without a frame, every entry answers.** A body
  that returns a constant is answered at the call site as one returning a
  parameter is (`regLeafConst`): `func_call` 34 to 19 ns against the
  tree's 28. A built-in's callback enters through `regDirect`, which now
  answers the same bodies -- a constant, a parameter, a short pure body
  through `pureCall`, a this-leaf through `leafCall` -- as `callDirect`
  does a function without register code; a register closure is planned at
  `closure_w`, so its callbacks had skipped that and made a frame each:
  `map_forEach` and `set_forEach` -19%, `typed_forEach` -22%. A pure body
  `pureByTree` gives a tree is entered as code: `forward_call6`'s
  six-operand sum was 11% slower through the evaluator.
- **An empty literal learns its room** (`new_array_empty`, the tree's
  `arrayEmptyNode` and its site): `array_push_args3` grew every array
  from nothing, -29%.
- **An element's update is one operation.** `o[k] += v` and `o[k]++` as
  statements are `add_index` (a key and a value in registers),
  `add_index_k` (a 16-bit index) and `add_index_imm` (an immediate value),
  on `add_prop`'s conditions: the value a constant or a binding no one
  else writes. The key is converted once; the instruction is placed at the
  key, where the stack form reads, and Aux names the stack form's store,
  whose position a store's error takes. `array_update` 18 to 6 ns.
- **A negative literal is an immediate** wherever a literal is
  (`numberLit`): `!== -1`, `x + -1`, `x > -2`, a switch's cases.
- **A switch's literal cases are one lookup** (`switch`, the table in
  `Switches`): each run of cases whose selectors are literals -- numbers,
  strings, booleans, null -- between cases of other selectors, made as the
  tree's `switchLookup` makes its table: integers of a small range by an
  index, strings by length, then the constant's pointer, then their units.
  `switch (typeof x)` of string cases asks x's type (`switch_typeof`), and
  `typeof x === "name"` is one compare-and-branch (`jtypeof`), an object's
  type asked inline but a proxy's and document.all's. `typeof` is an
  operation of its own, out of the loop. `string_switch_*` were 5 to 11
  times the tree's time, a slow-path `===` a case; `typeof_test` 3 times.
  An empty literal's array is made by a call of its own, not the slow
  path's.
- **A typed array's element is out of line, not the slow path's.**
  `get_index`, `set_index` and their immediate forms answer a dense
  element inline and send what they miss to `regElemMiss`, which reads or
  writes a typed array's element in its buffer as `elemAt` and `setElem`
  do, before the slow path: `typed_*_read` 24 to 15.5 ns and
  `typed_*_write` 24 to 14 against the tree's 13 and 14. get_index and the
  immediate forms reach it through a label they share, a jump each in the
  loop; set_index calls it before its own slow-path call, since with the
  case made shorter the cases after it moved and `crypto_am3` was 5.5%
  slower at every placement. An update of a number in place (`add_prop`,
  `add_index`) writes the number's word alone, as the tree's updates do,
  with no write barrier's check: `array_update` 6.3 to 5.9 ns,
  `prop_update` 5.5 to 5.1.
- **A short body of numbers and upvalues runs as its register code
  without a frame** (`regLeafCode`, `regLeafRun`): a pure body that is
  straight code to its first return, of at most 12 loads, moves, upvalue
  reads and stores and arithmetic operations, reading neither the
  callee's register nor its this's and writing no parameter's register --
  a counter, an accumulator, a callback that maps or reduces numbers and
  counts its calls. It is translated once into operations numbered
  densely (`regLeafOp`), whose switch is a jump table, and run in the
  call's window by `regCallW` and `regDirect` (which writes only the
  parameters' arguments there). Its stores to upvalues, at most four, are
  held until it returns, and none is read after it is made, so what is no
  number gives up having changed nothing, and the call is made with a
  frame, as the frameless evaluator's misses are, counted by the same
  `pureMiss`. `pureCallAt` dispatched each stack instruction through a
  switch of scattered cases, and gave up on a store made before an
  operation: `func_closure_call` 40 to 30 ns over the placements (the
  tree 33), `closure_counter` -22%, `array_reduce` -15%,
  `array_reduce_upvalue` 59 to 48 (the tree 62).
- **An element appended or a hole filled is out of line too**
  (`regElemMiss`, the tree's `setIndexMiss`: `appendElem`, `fillHole`),
  not the slow path's: `array_prop_create` 54 to 44 ns and
  `array_index_append` 56 to 48 in one build (the tree 33 and 36).
  set_index's miss asks an array for them first, in a function of its own
  (`regSetMiss`), as the tree's `setIndexMiss` does, rather than after
  `regElemMiss`'s typed array, `setElem` and `typedStore`, which an array
  the fast path missed never answers: 41 to 31 ns and 45 to 34 in one
  build.
- **A string's length is the loop's,** as an array's is: `get_length`
  answered only arrays inline and sent a string to the slow path, where the
  tree's node answers both. `string_length` 15.6 to 7.0 ns in one build
  (the tree 8.1, the comparison engine 5.5).
- **A local's appends grow its string in place,** as the tree's do
  (`strAppend`): the stack compiler, which decides which additions are
  appends (`localAppend`) and which reads are followed by a `str_freeze`,
  records both for the backend (the Record's `Appends` and `Freezes`), so
  that the two forms agree on every binding. An append is `append`; a
  recorded read the register form makes a value of -- a move, an
  argument, a store, a return -- is preceded by `str_freeze` of the
  binding's register. A read the register form makes no value of, a
  compare's operand or a step, keeps nothing and is not frozen, and
  `x.length` is no read for either form. A short circuit around an append
  whose value nothing uses (`r !== l && (t += x)`) is an append too.
  Both are cases of their own since the loop's budget was measured and
  its tails shared ("The loop"): `append` a call of `regAppend` with the
  pc kept, `str_freeze` its test inline, writing only a string an append
  owns. Before that, with `append` the slow path's and `str_freeze`
  `regCold`'s (d931057d), the loop had 19 nodes to spare, and over the
  placements `string_build1` was 52.6 to 34.7 ns, `template_numbers` 130
  to 101, `local_append_read` 60 to 43, `template_list` 356 to 312, but
  `local_append_keep`, `local_append_once`, `append_keep_each` and
  `append_keep_pair` 5 to 9% slower than before, a read's freeze and an
  append each a second dispatch and a call. As cases: `string_build1`
  30.5 (the tree 32.4, the comparison engine 49.2), `string_build_large1`
  29.8, `local_append_keep` 62.8 (the tree 65.6, the comparison engine
  62.4), `local_append_read` 41.0 (43.6, 58.0), `append_keep_each` 130
  (135, 120), `local_append_once` 144 (167, 100).
- **An object literal of plain keys is made at once**
  (`new_object_lit`): its values go into registers in order and one
  operation, behind `regCold`, makes the object with its site's room and
  takes each key's transition, where `new_object` and a `define_field`
  each went through the slow path. The values evaluated before the object
  is made is a difference nothing can see: the object is no one's until
  the literal is done. A computed key, an accessor, a method, a spread,
  `__proto__`, a key twice or more than 255 keys keep the form of one
  instruction each. `object_literal_fixed` 171 to 127 ns in one build (the
  tree 101), 197 to 141 over the placements. Where every key was added
  along a transition from the site's root, the site keeps the shape they
  led to (`propCache`'s `next`, its root in `s1`), as the tree's literal
  node does, and the next object from that root is given its values in
  place and that shape: 141 to 105 ns in one build, the tree 109.
- **s.charCodeAt(i) is one operation** (`char_code_at`), as the tree's
  `charCodeAtCall` node is: a receiver in a binding, the argument a local
  no one else writes in one of the first 256 registers (X). `regCharCodeAt`
  reads the code unit where the receiver is a string, the method the
  built-in found in the cache of a primitive prototype's data properties
  that `primProp` fills, and the argument a number; anything else is the
  read and the call as `get_method_w` and `call_method_w` make them, the
  read placed at its own position (Aux) and the call at the
  instruction's. The case is a call with the pc kept, 27 nodes. The slow
  path of a method's read on a string or a number reads the same cache
  before `getValueProp`. Over the placements, `html_escape_entities` 2,538
  to 1,813 ns (the tree 1,924, the comparison engine 1,878),
  `html_escape_mixed` 800 to 636, `int_toString` 81.5 to 77.1 (the tree
  70, the comparison engine 82); the V8 suite level.
- **A RegExp literal is register code** (`QJS_REGEXPLIT` and
  `QJS_REGCALLS` removed). As a `new_regexp` through the slow path it made
  `template_list` 304 to 228 ns, but every function with a literal lost
  the tree's mechanisms around it: the literal's RegExp given back after
  `/x/.exec(s)` and `test` (`regexpLitMethod`, the spares), a literal
  handed to `replace`, `split` or `match` searched without its object
  (`regexpArgCall`), and an exec's array made again in the memory of the
  last (`reuseExecCall`) or not at all where nothing reads it
  (`regexp_sticky` 255 to 377 ns, eleven regexp tests 6 to 48% slower).
  As cold operations (`lit_exec`, `call_lit`, `call_arg_lit`,
  `call_dead`, `call_reuse`), two second dispatches a call, `regexp_replace`
  was still 25% and `html_escape_plain` 8.3% slower.

  Now the call is always the hot `call_method_w`, and what the tree's
  nodes do around it is a **mark** of the call (`RegCode.Marks`:
  `RMarkDead`, `RMarkReuse`, `RMarkLitRecv`, `RMarkLitArg`, their operand
  in `Aux`, all 32 bits), which changes nothing of the dispatch: the site's
  discovery (`regCallSlow`) caches a callee as `regCallMarked` only where
  the mark is its -- the realm's exec, test or replace, any callee for a
  literal argument, a reuse only where `execReuseOf` has a site -- and
  `regCallW` sends those to `regMarkedCall`; every other callee keeps its
  kind. Marked where no reuse site existed, the call went through
  `callRest` with a frame where `regNative` had none, 10% slower: a mark
  applies only where it does something. A literal argument is made by the
  call itself (`RMarkLitArg`): nothing can see it made after the
  arguments that follow. The literal's method is `regexp_lit`, a direct
  operation (value 29): the literal made or given back and its method
  chosen; with `RLitCall`, for one argument that is a constant or a local
  nothing captures in a function with no arguments object -- which the
  method's read, a getter, could neither write nor see uninitialized --
  the call made at once, the built-in called without the window. Any
  other argument is evaluated between `regexp_lit` and the marked call:
  the method must be read before it. A constant past 16 bits keeps the
  general form (`new_regexp` and the call), never a refusal.

- **The opcode space was full but for two values.** The dispatch masks the
  operation to 7 bits so that its jump table needs no range check, and
  with `char_code_at` 126 of the 128 values were taken. Widening the mask
  costs a case value for each of the 128 new values, more than the loop's
  budget. The translation reference's operations, which the backend never
  emits, were removed with it (see stage 0): their eight values are free,
  and `cold`, the one value every cold operation shares, took 126 (the
  opcode allocation rule, under "The loop").

#### Raw int32 registers (step 5, designed, deferred)

Index-heavy code pays a float conversion at every element access: a key
held as a float64 is converted to an index, and an int32 result back to a
float. The tree tier avoids it by keeping an int32 it has proven in a Go
variable. The register form does the same by static typing in the backend,
not by a new tag in `Value` -- an int32 tag cost floats 58% when it was
tried (2026-10-02).

- **What a register slot holds.** A register is a `Value`: a float64 word
  and a pointer word. A register the backend has proven to hold an int32
  for the whole of its live range -- a loop counter, an index, a bitwise
  result -- may hold it raw: the int32 in the number word, the pointer word
  nil. Which registers are raw is a fact of the code, not of the value: no
  instruction asks.
- **Why the collector is safe.** The collector scans only the pointer
  word, which in a raw register is nil, as in a number's. The number word
  is never read as a pointer.
- **Who may read one.** Only operations specialised for raw operands --
  `add_i32`, `inc_i32`, compare-and-branch on raw int32s, `get_index` and
  `set_index` with a raw key -- read a raw register. Every generic reader
  needs the conversion to a `Value` first (`box_i32`): a call's window, a
  return, a store to a property or an element, a move into a `Value`
  register, any slow path's operand. A binding a closure captures, a
  generator's window, a debugger's scope, a direct eval's binding and a
  stack trace's frame are never raw: only registers private to the frame's
  straight code qualify.
- **The verifier enforces it.** It tracks each register's kind, `Value` or
  raw int32, along every path: kinds propagate over jumps, both sides of a
  join at a label or a handler's entry must agree (a mixed join is
  rejected), and code where a generic reader can see a raw register is
  rejected. An instruction on raw registers that can call out -- a back
  edge's interrupt check, a stack overflow, a throw -- may have no raw
  register live where the callee or the unwinder reads the frame
  generically. The interpreter's correctness rests on the verifier, not on
  the backend's care.
- **Overflow.** An int32 operation whose result does not fit -- past 2^31,
  or -0 from a multiplication -- leaves the raw form through a boxed
  `Value` register the backend reserves for it, and continues on the
  generic path. Until that path exists the backend proves int32-ness only
  for what cannot overflow: masks, shifts, `| 0`, counters bounded by an
  int32 limit.
- **Tested by difference.** A differential test runs the micro-benchmark
  files, the V8 suite's programs and generated int32 arithmetic -- overflow
  past int32, -0, NaN, shifts by 32 or more, `>>> 0`, mixes with doubles --
  with the register tier on and off, and compares every result.
  (Built: `internal/v8bench/regdiff_test.go`, run after every step.)

Deferred, 2026-11-09, on these measurements:

- **NavierStokes' keys are not provable.** In `lin_solve` and
  `lin_solve2`, which are its time, every element key is `j * rowSize`,
  and `rowSize` is an upvalue made from `setResolution`'s argument: no
  static proof reaches it. What the backend could prove are the loop
  counters, compared with the upvalues `width` and `height`. The tree tier
  keeps those locals as Values and converts the same keys, and is still
  21% faster: its lead is fused nodes such as `x[++currentRow]`, not
  int32s, and the register form closes it by fusion and instruction count.
- **am3's operands are Values.** Its parameters `i`, `j`, `c` and `n` and
  every element read are Values; what is provable -- the masks, shifts and
  their products -- is about 13 of the loop's 26 instructions, 10 to 15% at
  best, for a verifier of kinds and a doubled set of operations.
- **Speculation is not taken.** Unboxing a parameter or an element read
  behind a check that exits to the generic code would type both loops, but
  brings exits, mapped generic code, twice the operations and a threat to
  `runReg`'s size into the interpreter, to remove a cost the faster tier
  pays too.

If this is ever built, the rule for calls out is the narrower one: no raw
register may be live where a callee or the unwinder reads the frame
generically -- a register a closure captures, a parameter an arguments
object maps, a call's window and the callee frame above it, a handler's
entry. An interrupt check, a stack check, a throw with no handler in the
frame and a ToPrimitive read nothing of the frame's registers, so a raw
loop counter may be live across a back edge; the literal rule, no raw
register live across anything that can call out, would forbid every raw
counter.

### Stage 2: zero refusals

The register tier is the baseline interpreter: it refuses nothing V8's
Ignition compiles, which in practice is everything. No form stays in the
old tiers, not generators, not eval, not the debugger's code; each is
compiled as register code, and the old tiers go when the last refusal
does. The refusals are counted by `internal/cmd/regrefusals`, which
compiles every file of the directories it is given with the register
form on and counts the functions regcomp compiled and those it refused,
by the first reason each met (a function refused for one reason may meet
another once it is gone):

```sh
go run ./internal/cmd/regrefusals D:/Data/v8-v7 D:/Data/octane D:/Data/bundles /path/to/test262/test
```

At 8474d6d0 (RegExp literals compiled; test262's negative tests, 4,728
files, do not compile and are not counted):

| | files | functions | register code | refused |
|---|---|---|---|---|
| V8 suite | 10 | 820 | 808 (98.5%) | 12 |
| Octane | 21 | 6,976 | 6,801 (97.5%) | 175 |
| bundles (Babel, Three.js) | 2 | 9,829 | 6,234 (63.4%) | 3,595 |
| test262 | 54,084 | 162,893 | 63,090 (38.7%) | 99,803 |
| all | 54,117 | 180,518 | 76,933 (42.6%) | 103,585 |

Every reason, all four together (V8, Octane and bundles where they have
it):

| functions | reason | V8 / Octane / bundles |
|---|---|---|
| 49,389 | top-level code | 10 / 21 / 2 |
| 12,737 | generator or async | 0 / 0 / 11 |
| 9,844 | class constructor | 0 / 0 / 128 |
| 7,154 | getter, setter, field initializer or static block | 0 / 39 / 141 |
| 5,382 | private members | |
| 3,899 | parameter patterns, defaults or rest | 0 / 0 / 176 |
| 2,981 | a field's initializer (`ast.FieldInit`) | |
| 2,819 | tail call | 0 / 83 / 2,566 |
| 1,912 | a BigInt literal | |
| 1,449 | private methods' installation (`ast.InstallPrivateMethods`) | |
| 1,426 | an object literal's property (accessors, methods, spreads, computed) | 0 / 12 / 5 |
| 757 | direct eval | 1 / 1 / 0 |
| 655 | a template literal | 0 / 0 / 7 |
| 527 | a with, an eval's variables or an Annex B function | 0 / 3 / 1 |
| 434 | a name the backend does not read | 0 / 0 / 335 |
| 206 | a destructuring declaration | 0 / 0 / 4 |
| 201 | an arguments binding the record does not name | 0 / 1 / 0 |
| 197 | a for-of or for-in loop's pattern or member | |
| 189 | a spread element | |
| 153 | arguments | 0 / 1 / 52 |
| 146 | super | 0 / 0 / 119 |
| 98 | an assignment target (patterns) | |
| 96 | a class declaration inside | 0 / 0 / 1 |
| 96 | a strict global store of a value that runs code | 0 / 2 / 0 |
| 92 | a name the record does not resolve | |
| 83 | typeof of a global | 1 / 9 / 12 |
| 71 | a class expression inside | |
| 69 | a spread argument | 0 / 0 / 1 |
| 67 | `super` as an expression | |
| 59 | a logical assignment | |
| 47 | a store the backend does not compile | 0 / 0 / 19 |
| 45 | `new.target` | |
| 41 | a using declaration | |
| 39 | an object literal's key | 0 / 1 / 0 |
| 35 | an optional chain | |
| 27 | a catch clause's pattern | |
| 19 | a hole | |
| 17 | an object literal's `__proto__` or a key twice | 0 / 0 / 4 |
| 15 | a function declaration in a block | |
| 13 | delete of a super property | |
| 12 | `super()` | |
| 11 | a labelled statement that is not a loop | 0 / 0 / 1 |
| 9 | an optional call or too many arguments (a cap: 255) | 0 / 0 / 9 |
| 8 | a private name | |
| 8 | delete of a name or an optional chain | 0 / 1 / 0 |
| 7 | a captured lexical binding in a loop | |
| 6 | a declaration in a switch | |
| 6 | a with statement | |
| 6 | a tagged template | |
| 5 | a call of eval | |
| 5 | a for-of or for-in head's declaration | |
| 4 | `??` | |
| 4 | an update target | |
| 3 | `import.meta` | |
| 2 | a compound assignment target | |
| 2 | a lexical declaration in a switch | 0 / 0 / 1 |
| 1 | a site past 16 bits (a cap) | 0 / 1 / 0 |

Not in the corpora, and refused as well: code compiled for a debugger
(`WithDebugger`), `for await`, a finally clause nested more than four
copies deep, and the caps -- more than 65,536 registers, a jump past 16
bits, a name past 16 bits, more than 255 arguments, properties or
elements in one instruction. A cap goes as a wide form, never a refusal.

The plan to zero, in the order of what each costs real code first -- the
V8 suite, Octane and the bundles -- and then test262, each form with its
regdiff cases and test262 in register mode on its directories:

1. **Tail calls** (2,819; 2,566 of the bundles', strict code's `return
   f()`) -- done. The stack compiler records the calls it makes tail calls
   (`Record.Tails`), and regcomp makes them `tail_call_w` and
   `tail_call_method_w`, call_w and call_method_w in tail position with
   direct values of their own (36 and 37), so that `regCallW` checks
   nothing of them: a mark read in `regCallW`'s depth check, its frame's
   end checked for a restart and a tail call after every call, made
   `call_fib` 12%, `call_mutual` 14% and the typed arrays' callbacks 6 to
   16% slower at every placement. `regTailCallW` makes the call
   `regCallW`'s until the stack is `tailCallDepth` deep; what only a tail
   call runs is in `zzzy_regtail.go`, after every other file, since in
   `zzreg_call.go` it moved the call path's functions against `applyCall`
   and `nativeFrame` and made `forward_loop` 11% and
   `string_switch_miss` 8% slower at every placement. There, a
   callee of register code of the form's own layout that keeps no
   arguments takes the frame over in place (`regTailInPlace`): the
   frame's captured bindings closed, the arguments moved to the
   parameters, the frame run again from the callee's first instruction
   by its caller (`errRegRestart`, checked only once the call has ended
   in an error: `regCallWEnd`, `regRerun`), as the tree's `selfRestart` and
   `tailSwap` do -- unwinding to the caller each time made `tail_self`
   3.7 and `tail_mutual` 2.5 times the tree's. Any other frame that is no
   construct's and shares no derived constructor's this is given up as
   the stack form's `tail_call` gives it up: the call pending
   (`r.pendingTail`) and the loop leaving with `errTailCall`, which its
   caller makes: `regCallW` and `regCallMiss` through `regCall`, whose loop makes every later call of a chain in place (a
   bound function resolved to its target there, as `tailTarget` does), and
   a frame the tree or run made through `treeTail`, as after
   `tailCallNode`. A function with a tail call is not given a `treeCall`
   (`regPlan`), whose callers expect none: `planTreeCall`'s `treeRest`.
   After it: Octane 98.7% register code, the bundles 89.0% (63.4%), all
   44.1%.
2. **Top-level code** (49,389, most of test262; every script and module):
   a script's and a module's body as a function of registers, its
   bindings the global object's and the global lexical environment's.
3. **Generators, async functions, `for await`** (12,737): the frame's
   window copied into the generator on a suspension ("Generators and async functions"
   above), the settled-await fast path; the step's recover goes
   with them.
4. **Classes** (about 27,000 together: constructors, accessors, fields,
   static blocks, private members and methods, `super`, `new.target`,
   class expressions inside functions).
5. **Parameter patterns, defaults and rest; destructuring everywhere**
   (declarations, assignment targets, catch clauses, for-of and for-in
   heads; about 4,500).
6. **BigInt literals** (1,912), **templates and tagged templates** (661),
   **object literals of every property kind** (1,482).
7. **The arguments object in every form** (354), **spreads** (258),
   **names the backend does not read** (434, 335 of the bundles'), names
   the record does not resolve (92), `typeof` of a global (83), strict
   global stores (96), logical assignment (59), optional chains (44),
   `??` in the forms left (4).
8. **eval and `with`** (757 direct evals, 527 functions beside an eval's
   variables, a with or an Annex B function, 11 more): binding
   descriptors for what an eval can reach.
9. **`using`** (41), labelled blocks (11), declarations in switches (8),
   function declarations in blocks (15), captured loop bindings (7), the
   finally depth (completion dispatch, no cap), the debugger's code
   (statement markers in register code), and the caps.

### Stage 3: the JIT and the tree tier

The JIT's front end reads register code, or the AST, and its
deoptimization rebuilds register frames: with the stack interpreter and
the tree tier removed, the stack form is no input it can keep. The tree
tier is measured against the register interpreter suite by suite, its
mechanisms ported, and removed.

### Stage 4: one pipeline

The stack compiler stops emitting code; scope resolution stays as the
front end both backends shared. `executeAt` and the stack form's
instruction set go.
