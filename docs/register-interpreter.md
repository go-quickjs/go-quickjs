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

## Status

The register interpreter is the default on amd64, where its handlers are
native (`internal/vm/zzzyn_native_*`): every function is compiled to
register code and run as it, none falling back to the old tiers. Other
architectures, arm64 included, keep the stack interpreter and the tree
tier by default, and move over as their native handlers land; their
register code runs on the Go handler table (`zzreg_table*.go`), which is
slower than the tiers it would replace. The default is one constant an
architecture, `regArch` in `internal/bytecode/regdefault_*.go`, read
through `bytecode.RegDefault`.

A runtime with the JIT runs the old tiers on every architecture, since the
JIT reads the stack form alone: the host compiles its code with no
register form (`compiler.Options.NoRegister`, `vm.Runtime.RegTier`), and a
`Program` keeps a form for each kind of runtime, so that no function's tree
is shared between the two. This stays until the JIT reads register code
(Stage 3), when the old tiers are removed.

`QJS_REG` in the environment overrides the default: `QJS_REG=0` runs the
old tiers everywhere, `QJS_REG=1` the register interpreter everywhere. The
first is temporary, for comparing with the tiers being replaced, and goes
with them; the second is how arm64 is tested until it is the default there.
`QJS_NONATIVE=1` runs register code on the Go handlers on amd64 too.

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
  code, run once (item 15).
- **Sites, constants and names past 65,535.** An operation whose index would
  not fit has a wide form: the same operation followed by an extension word
  holding a 32-bit index, read by the wide form's case alone, so the narrow
  form's case never tests for it. Global reads, loads of constants and
  closures already take a 32-bit BC and need none. A name past 16 bits
  needs no wide form either: only cold operations and miss paths read a
  name, so its operand is `RNameFar` and `RegCode.FarNames`, by the
  instruction's pc, holds the index (item 14).
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

### Compiled at the first call

A function's register code is compiled when it is first called, as V8
compiles lazily: one never called is never compiled. The stack compiler
records what the backend needs as it compiles (the `Record`), and keeps
it, with the function's AST, in a `bytecode.RegLater` until then;
`Function.RegCode` compiles it once, under a lock, since a program's
functions are shared by the runtimes running it. A script's or a module's
code, which runs as soon as it is compiled, is compiled with it
(`Function.Reg`). What reads register code before a call -- the program
sites a re-run seeds (`zzz_rerun.go`) -- reads it only where it has been
compiled (`RegCompiled`); a closure made before its function's first call
is planned at that call (`regStmt`, `planTreeCall`), not as it is made.
A leaf's code (`Function.Leaf`) is compiled as its closure is made, as
all were: the stack form's leaf evaluators (`pureCall`, `leafCall`)
answer its calls and build nothing, so it would never reach a first call
of its own, and a sort's comparator ran in `pureCallAt` rather than
`regLeafRun` (`array_sort` +15%, the callbacks of `forEach`, `map` and
a replacer 6 to 18% slower at every placement, until it was).

The backend added names, constants and property sites to the Function as
it compiled. By the first call those are shared: every runtime that loaded
the program has made a closure's tables from them (`prepare`), and another
goroutine may be reading them. So code compiled lazily adds nothing to the
Function: it compiles over a view of it whose slices are full to their
capacity, and keeps the tables with its additions in the `RegCode`
(`Names`, `Constants`, `PropSites`), its sites numbered past the private
members' so that the stack form's numbering stays as it is. A runtime adds
what is past a closure template's tables where the template is first
given its register code (`regExtend`): where `cl.reg` is set (`regOf`,
`regStmt`), which every entry into the code reads, and where a plan lets
a call enter the code without it (`regBindPlan`, for `regDirect`). A table
missed would be an index out of range, not a wrong read: the loop's reads
of names, constants and caches are bounds-checked.

## The loop

The interpreter is a small fixed loop, `runReg`, over a table of
handlers, a Go function an operation (`zzreg_table.go`,
`zzreg_table_cold.go`), in files that sort after the package's other
files (`zzreg_*.go`): `executeAt` and the other hot functions keep their
code and their addresses.

```go
st := r.regEnter(f, c)
regs, code, pc := st.regs, c.code, start
for {
	w := regAt(code, pc)
	if pc = regTable[w.Op()](regs, w, pc, st); pc < 0 {
		if pc == regDone {
			break
		}
		pc = ^pc
		w = regAt(code, pc)
		if pc = regSlowTable[w.Op()](regs, w, pc, st); pc < 0 {
			break
		}
	}
}
```

Every operation costs the same to reach: one load of its table entry and
one indirect call, whatever it is and however often a program runs it.
What is hot is decided by the program, not the interpreter, so no
operation is privileged with a place in the loop and none pays a second
dispatch. The loop is 30 instructions, `runReg` with the state it takes
and gives back 800 bytes and 143 IR nodes, and an operation added does
not change it; `TestRegLoopIsSmall` holds it below 864 bytes and fails
where the loop stores to the stack between one dispatch and the next.

**The handlers' protocol.** A handler is given the frame's registers, the
instruction, its pc and the frame's state (`regState`), in the registers
Go's ABI passes them in, and returns the pc to go on at:

- `pc+1`, or a jump's target, where it has done the instruction;
- `^pc` where its fast path has not -- an operand of another kind, a
  cache that missed, a number to store into a register that holds a
  pointer, a back edge that spent the interrupt budget: the loop then runs
  the operation's slow function from `regSlowTable`, which does the whole
  instruction as the stack form does (`rsGetProp`, `rsAdd`, `rsCompare`
  ...), and never returns `^pc` itself;
- `regDone` (`math.MinInt`) once the frame has returned, or has thrown
  what none of its handlers catches (`st.err`), a generator's suspension
  and a tail call's restart among them, which no handler of the frame's
  takes. What `ret` returns the loop reads from the instruction it ended
  at, so that `ret`'s handler stores nothing and is a leaf; that made
  `call_fib` and `call_mutual` 3 to 4% faster.

An operation of no fast path -- a call, a closure, an operation of no hot
loop -- has its whole self as its handler. A call's is the call path
itself, `rhCallW`, the first function after the loop: as a handler that
called `regCallW` it was one frame more a call, and `call_mutual` and
`method_call` were 17% slower. The 106 operations of no hot
loop -- declarations, classes, destructuring, generators, with, eval
(`ROp.Cold`) -- are handlers of their own in the same table, each calling
its operation's function (`regNewClass`, `regIterStep`, `regSuspend`,
...): `cold` and its sub-operations in X are gone, and with them the
switches that dispatched them a second time (`regColdSub` and the group
functions it called).

**Leaf handlers.** A fast path that misses gives back `^pc` rather than
calling its slow function, and a number is stored by its `num` word alone
where the register's `ref` is nil -- a store of no pointer, which needs no
write barrier. So the arithmetic, the bitwise operators, `inc` and `dec`,
the loads of a constant that is no pointer, the compare-and-branches and
the jumps are leaves: no frame, no stack check, no call. A store of a
pointer is a write-barrier check, and makes a handler that stores one --
`move`, `get_prop`, `load_const` -- a function with a frame as before.
A temporary register that held an object and is given a number is the
slow function's, which stores the number with the barrier before it
tries the operation's conversions: register allocation reuses
temporaries across kinds, and with the slow functions converting every
such store Box2D was 20%, zlib 13% and `bigint64_search` 33% slower.

**Errors and positions.** A handler or slow function that can throw or
run code keeps the pc past its instruction in `st.kept` first, by
`st.at(pc)`, which also sets the frame's pc to the stack form's
instruction, what a stack trace and an error's position read; the calls
that read the kept pc (`regCallW`, `regElemMiss`, an iterator's step) are
given it the same way. The fail tail, `st.fail`, finds the handler of the
instruction at `st.kept.pc-1` (`regCatch`) and goes on there, or ends the
frame with the error. `TestRegThrowsKeepPC` reads the handlers' source
and fails where one goes to the tail, or hands a call the kept pc, before
it has kept it. The loop keeps no pc of its own across a handler: the pc
is an argument and the result, and what it keeps -- the registers, the
code, the state -- is saved once before the loop and read back after each
call.

**The interrupt.** A jump back counts down the runtime's budget in its
handler (`regJump`); where the budget is spent it gives back `^pc`, and
its slow function makes the check (`regJumpSlow`, `regBackEdge`), the
condition evaluated again, which has no side effect.

**State per runtime.** A frame's state is a `regState` from the runtime's
free list, taken as the frame starts and given back, its pointers
cleared, as it ends (`regEnter`, `regLeave`): separate runtimes on
separate goroutines share none, and a frame that a Go panic leaves keeps
its own, which the collector takes.

**The window and the code are unchecked.** A handler indexes the frame's
registers through a pointer to an array of 65,536 values at its first
register, so `regs[w.A()]` has no bounds check: the verifier proves, for
every function, that every register an instruction names is below the
frame's count, and the frame is given that many slots before it runs. The
code is read at a pc the verifier proves is in it: every jump lands on an
instruction, the last does not run on past the end, and every operation
an instruction holds has a handler (`ROpCount` bounds them; the table's
other entries panic).

**Why a table.** A measured bake-off of dispatchers chose it. `runReg` as a
switch failed every criterion:

- it varied 10 to 27% across code placements, against 1 to 5% for a
  table of handlers;
- rebuilding it with ten unrelated operations added moved kernels by up
  to 27% (`loop_locals`, `prop_in`), against 3% at most for the table;
- an operation of no hot loop paid 5 to 8 ns for its second dispatch;
- it was 19.9 KB and about 4,630 IR nodes, grew with every case, and
  needed a layout lock of its own, a golden file of its cases' offsets,
  levers and an IR node budget to be kept where it measured best.

The table's handlers measured 4 to 7% slower than the switch on the hot
operations of kernels (`crypto_am3` 3.59 against 3.28 ns an instruction,
`loop_locals` 2.48 against 1.84, `call_fib` 8.3 against 5.9) and 10 to
15% faster on what the switch dispatched twice (`typeof_value` 4.6
against 5.3, `prop_in` 4.6 against 5.4, `bigint64_arith` 8.8 against
10.1). The best Go form measured was this one: the arguments in
registers, the next pc returned, and the leaf number stores, worth 3% on
`crypto_am3` (3.48). A state in a struct read by the handler, the next
instruction or handler returned, and handlers that called the next
instruction's handler themselves were all slower; `//go:nosplit` was worth
about 1%. The table is also the portable form of what machine code does
on amd64 and arm64: generated native threaded handlers over it, which
leave to its Go handlers what they do not do themselves. The suites over
eight placements against the switch, register mode: the
micro-benchmarks 1.029 by the geometric mean (149 of 384 3% or more
slower, 57 3% or more faster), the V8 suite 9.0% slower, Octane 20.6%,
AWFY 14.9%. What the switch held in its own cases lost most: the
property and global reads and writes 22 to 41% (`prop_write` 4.66
against 6.34 ns, `global_write` 4.52 against 6.39), `loop_locals` 34%,
calls 9 to 24%, `crypto_am3` 18%, Box2D and zlib 25%; what it dispatched
twice gained: `async_await` 33%, the switches 9 to 17%, `private_field`
12%, `loop_arith` 12%. `crypto_am3` was 4.5% slower before `call_w`'s
handler became the call path and 18% after, at every placement, with no
instruction of its loop's handlers changed. The likely cause is their
layout: the handlers' order in their file, which no placement varies,
puts `add`, `shr_imm` and `jlt_imm` on the same op-cache sets. The Go table is the portable fallback, not the end: it lands with
the native handlers.

**Removed with the switch.** The golden file of the hot cases' offsets
(`layoutcases.txt`) and `layoutpad`'s reader of the dispatch table; the
levers P and K inside the old loop's block (`regBinImm`, `regDeadCall`),
which held its slow path and cold's dispatch where they were; the free
opcode values and the 7-bit mask the switch's jump table needed; and the
IR node budget (`TestRegLoopCallsNoHelper`), which kept the switch below
Go's big-function size, past which it inlines only callees of cost 20.
The layout lock keeps the levers Q, which anchors `runReg`, and W, which
anchors the call path after it (`rhCallW`, where `regCallW` was); the
handlers follow. The
placements' filler in the loop's cases (`padRegInner`) applies only to a
loop that has them.

What remains is Go's: every operation ends in a return to the loop and its
one indirect call, a handler that stores a pointer has a frame and a
write-barrier check, and a Go call passes no more than the four arguments
in registers, so the state's fields are loads.

## Native handlers

On amd64 a register frame runs by native threaded handlers
(`zzzyn_native_amd64.go`), machine code for the hot operations over the
same instructions and the same frames; the Go table is what they leave
work to, and every other architecture's interpreter. `runReg` hands its
frame to `nativeRun` before its loop; `QJS_NONATIVE` set in the
environment (read once, `regNativeOn`) keeps the frames in the Go table's
loop, to compare the two and as an escape hatch.

**The dispatch.** Each handler ends in its own dispatch, as V8's
Ignition's do: the next instruction read, its operation's handler's
address loaded from the native table, an indirect jump -- four
instructions, nothing spilled. The pinned registers hold what every
handler needs: `RBX` the instruction's address, `R12` the frame's
registers, `R13` the native table, `R15` the closure's caches, `R11` the
back-edge budget, `R10` the least tag, `RDI` the context; `RAX` holds the
instruction word, and `RCX`, `RDX`, `RSI`, `R8`, `R9` and three SSE
registers are a handler's scratch.

**What is native.** The numbers' arithmetic, increments, `-x`, `+x`,
the bitwise operators of any number (by ToInt32) and the
compare-and-branches, of registers, immediates and upvalues; truthiness
of a boolean, a number, an object, a string, undefined and null; `==`
and `!=` of numbers, of one word kind and of undefined or null against
anything; `===` and `!==` (two strings by identity and length, and two flat ones
of one length by their text, eight bytes at a time, up to 1,024 bytes:
a longer text is Go's, so that an exit reads the counters a host's stop
zeroes, which a loop comparing it each turn would not otherwise see until
its budget ran out); moves, constants, upvalues read and written,
and globals read and written by their site; property reads and writes by
the site's cache, its own entry or the entries of the shapes it met
before (`propPoly`), and a property added by the transition it
remembers; `o.n +=` of an own number; `in` by the
realm's in cache, `typeof`, the length of an array or a string, the
elements of dense arrays (read, `x[++i]`, `x[i++]`, written over any
element) and typed arrays (every kind but Float16 and BigInt64); short
BigInts' add, sub and mul; and calls and returns:
`call_w` and `call_method_w` enter a callee their site's cache knows as
`rhCallW` does -- its this, the frame record, the stack top and depth --
and run it, the caller's state pushed on the native frame stack; a
callee that keeps its arguments (one that reads `arguments`, which
`rhCallW` leaves to `regCallMiss`) is entered as `regEnterW` enters it,
its arguments appended to the runtime's argument stack; and a leaf that
returns a parameter or a constant is answered with no frame, while one
Go answers frameless otherwise (a pure body, one that reads or writes
its this) is entered with a frame, cheaper here than an exit. `ret` pops
the frame, and gives an entered frame's arguments back. Later additions:
`x--` as `x++`; a string's or a number's method (`get_method_w`), from
the realm's cache of their prototypes' data properties (`primProps`), as
`regGetMethod` reads it; for-of's step over a dense array by the
intrinsic iterator, its end included; `s.charCodeAt(i)`
(`char_code_at`) of a flat string, the built-in read from the same cache;
BigInt64Array's and BigUint64Array's elements read and written, a BigInt
held in its Value; Uint8ClampedArray's writes, clamped and rounded to
even; the compare-and-branches of two such BigInts, as 128-bit integers;
an element written at the length of an array with room for it, as
`appendElem` appends it, the array's and its prototype chain's index
keys and elements checked;
and a call in tail position while the stack is shallower than
`tailCallDepth`, which is `call_w`'s. Deeper, Go gives the frame to the
callee in place (`regTailTo`); where it is the base frame, `nativeRun`
goes on with the callee's code in the same code entry
(`nativeRestart`), rather than ending it for `regRestartRun` to run each
callee in an entry of its own. A fast path that misses jumps to
the exit; nothing is written before it does.

**The exit stub.** An operation of no native handler has the exit stub's
address in the table, and every miss jumps to it. It writes the
instruction, its index and the budget to the context, makes the top
frame's state where it is not made (`topOK`) as `nativeTop` makes it --
the state Go made for its depth before given the frame, its code,
closure and window, the callers' pcs written -- and calls the
instruction's Go handler
from `regTable` itself, through `nativeCallH`: a function that the Go
function that entered the code called, as the JIT's `callGo` is, which
gives the handler its arguments in Go's registers and its frame the room
they spill to. Where the handler gives back `^pc`, the stub calls the
slow function from `regSlowTable` the same way; the code goes on at the
pc they give, the pinned registers put back from the context, the budget
the runtime's `backEdges` across the calls. Where the frame has ended
(`regDone`), and where Go has made no state for the depth yet, the stub
returns to `nativeRun`, which ends the frame, or makes the state and runs
the instruction, and enters the code again; so does a call the native frame
stack has no room for, and the base frame's return. The call depends on
Go's internal register convention (ABIInternal) on amd64, which the
exit stub and the trampoline write by hand: the four arguments in `AX`,
`BX`, `CX` and `DI`, the func value in `DX`, the result in `AX`, the
goroutine in `R14`, `X15` zero, and the arguments' spill room in the
caller's frame. Go does not promise it across releases;
`TestRegNativeCallHConvention` calls a Go handler through `nativeCallH`
and fails, saying which part differs, where the toolchain's convention
is not this one, and CI runs it under Go 1.24 and Go 1.27. The call made
`array_push_args3_single`, whose pushes exit, 19% faster than the return
and the entry (199 to 162 ns an operation), and `string_indexOf` 13%. There is no second interpreter: what the Go table does, an exit does. A frame the code entered itself that ends in Go
-- a throw, a return that closes captured upvalues -- ends there as
`rhCallW` ends a callee after `runReg` (`nativeEnd`), and its caller goes
on or meets the throw. Its state is made for it from its frame record
when Go first needs it (`nativeTop`), and the callers' pcs, which only Go
reads -- a stack trace, the debugger -- are written then
(`nativeFillPCs`).

**The context.** What the code reads beyond the pinned registers is the
context, `nativeCtx`, which a frame's `regState` keeps, made at its first
native run: the top frame's code, caches and closure, the runtime's
fields it reads (by addresses set once), and the native frame stack, one
record a call the code made itself. The stack starts at 8 records and Go
doubles it when a call finds it full, up to 128; a call past that is
Go's, whose frame runs in a code entry of its own. Each runtime's states
are its own, so separate runtimes on separate goroutines share no
context. A realm's fields -- its prototypes, its caches of the in
operator's answers and of the primitives' methods -- are read through
the realm running now, the Runtime's first word, at offsets the context
gives: a context lives on from realm to realm with its state, and an
address of the realm it was made in, which the collector does not see,
was read once a host had dropped that realm and it was collected
(`TestRegNativeInDroppedRealm`).

**Generation and the shared-code rules.** The handlers are written with
the JIT's assembler by a generator, `internal/vm/internal/nativegen`, and
assembled into the binary as one TEXT symbol of bytes and the table as
GLOBL/DATA (`zzzyn_native_handlers_amd64.s`, `go generate
./internal/vm`); `TestRegNativeGenerated` fails while the file is not
what the generator makes. The generator does not import package vm: it
reads vm's private layout -- the context's, the frame records', the
Runtime's and the other types' offsets, the constants the handlers test
against -- as plain numbers, which vm's `NativeLayout` gives in a build
with the `quickjs_nativelayout` tag, which a helper of the generator alone
builds and runs. Each architecture's emitter is files of its own beside
the shared layout, the shared list of the operations the handlers answer
and the writer of the block. The package's code needs no assembler and
no JIT. The block is one
for every runtime and every build tag: position-independent, never
patched, with no runtime's constant in it -- every address it needs is
the context's, and the Runtime's and a closureCode's fields, whose
offsets the JIT's tag changes, are read relative to a base the context
gives (`rtBase`, `ccBase`), which the test checks under each tag.

**Dispatchers: architectures and variants.** The generator makes several
native dispatchers, on two axes. The first is the architecture: amd64
now, arm64 next, others later, each with its emitter and its files. The
second is the variant, within an architecture: a dispatch table over the
handlers, which a runtime picks through its contexts' table pointer
(`ctx.table`, `R13` in the code; `nativeDispatch`). `plain` is the only
one in builds without the JIT, and the one a runtime that does not run
the JIT uses in builds with it. `feedback` is a runtime's that runs the
JIT, in builds with it alone: its handlers that record the JIT's
feedback OR the kinds of value they met into the function's feedback
slot (below), every other handler plain's; none records yet. Mode
tables, such as the debugger's, are to be variants too. A handler is
generated once: plain's block holds the entry, the exit stub and every
handler, and a variant only the handlers it overrides, in a block of its
own beside its table (`zzzyn_native_feedback_amd64.s`, built with the
JIT alone), whose entries point at those and at plain's handlers for
every other operation. Its own handlers reach plain's code through the
table: the exits through two slots after the operations', another
handler through its operation's entry. `TestRegNativeGenerated` checks
every file, each from the layout of its variant's build;
`TestRegNativePlainSameWithJIT` that plain's is the same in a build with
the JIT and without; `TestRegNativeLinked` that a binary links what the
generator makes for its build; `TestRegNativeFeedbackSelected` that a
runtime picks its table.

**The marking rule.** A handler stores a number's word alone where the
register held no pointer; a pointer word it stores plainly while the
collector does not mark, and by the barrier's routine
(`nativeStoreRef`, a store with Go's write barrier) while it does,
reading the runtime's write-barrier flag. That is the only difference:
the code does the same work whether the collector marks or not, and
never leaves native code or calls Go for the marking's sake.
`TestRegNativeKernels`, `TestRegNativeFastPaths` and `TestRegNativeCalls`
run each case with the flag read as set and as clear and fail on a
different answer or a different count of exits;
`TestRegNativeCallExits` checks the call handler against `rhCallW` case
by case, and `TestRegNativeConcurrent` two runtimes at once.

**Room for feedback.** The JIT's next step gives each function a
feedback vector, as Ignition does: a slot a site, into which every
handler, native and Go, ORs the kinds of value it met (a small integer,
a double, a string, anything else; elements' kinds; a site of several
shapes), the hotness of calls and back edges coming from the budget the
code already keeps. The context has its pointer (`fv`, none yet), which
the frame record keeps and a call switches as it switches the closure's
caches; and no handler uses `R14`, which is kept for it to be pinned in,
so that a handler's note is one OR to memory and spills nothing.
`TestRegNativeCaches` (internal/regdiff) runs the register interpreter's
corpus natively and by the Go table and checks that the caches the JIT
reads -- the property sites, the global sites' epochs, the call sites'
kinds and callees (`RegCacheDigest`) -- end up the same: the native fast
paths and the Go handlers they leave to fill them alike.

**Preemption.** The code is not preemptible: Go reaches it at the exits,
and a loop that never exits still spends the back-edge budget, and a
recursion the interrupt counter, which the handlers count down as the Go
handlers do and exit at its end, so that the interrupt check, and with
it the scheduler and the collector, run as often as in the Go table's
loop.

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

A generator's frame is its window (regcomp's gen.go, the VM's
zzreg_zgen.go). Suspension copies the window and the pc into the generator
object, and resumption copies them back to the top of the stack and
continues at the pc, as the stack form does with its operand stack -- but
there is no operand stack, so the copy is the frame's registers alone. The
captures open on the window's registers move with it, pointed at the
generator's copy on a suspension and back at the stack on a resumption
(`regMoveUps`), so that two closures made in different resumptions share a
binding. A generator of register code keeps its window in its `vals`,
its registers from `genFrom` and then, where the code keeps them, its
arguments; its `pc` is a register pc, and `generator.reg` marks it.
`genFrom` is 2 where no closure the code makes captures the callee's
register or this's and nothing reads a local by its register: a
resumption makes those two again from the generator, whose `this` is
this's register (an async arrow's lexical this). Each generator and
async function keeps two values fewer: `extra_pair_gen` keeps 3, in
the allocation of 4 rather than 8 (the tree's keeps its 2 locals), and
`async_chain` is 7.8% faster.

- **The code's first instruction is `gen_entry`** (cold), which a
  resumption that goes on skips: `runReg` starts at the pc it is given,
  0 for any other frame. `gen_entry` runs at the frame's first run, and
  at a throw injected, which it throws at the suspension, a negated pc
  in the frame's `savedSP`. `yield` is a case of the loop's own
  (`RYield`), the step of a loop over a generator: through `cold`, the
  three switches its suspension and `gen_entry` went through
  (`regSlow`, `regColdSub`, `regColdGen`) were a third of a for-of's
  step.
- **A generator's creation runs its parameters' binding** up to
  `end_params`, which suspends it, as the stack form's prologue does: a
  default or a pattern that throws, throws at the call. An async
  function's are bound by its first resumption, which runs at once
  (`runAsync`), an error there rejecting its promise as the specification
  has it.
- **What a resumption delivers goes to the suspension's own register,**
  never a binding's, written before the code runs (`regGenResume`):
  `yield` (R[A] the value sent in), `await` (what it settled to),
  `yield_star` (the value and how, a pair). A return injected at a yield
  is compiled after it, a return statement there, through the finally
  clauses and the loops' iterators it leaves (`retFrom`); the yield's next
  instruction jumps past it, and a resumption by next starts past that
  jump. A throw injected is thrown at the suspension itself, inside its
  handlers' ranges.
- **`yield*` is a loop of the code's own,** as the stack form's is:
  `iter_resume` sends what the generator was resumed with to the
  delegate's next, throw or return, `iter_unpack_delegate` reads its
  result (an async generator awaits it first), `yield_star` suspends with
  it, and a return the delegation carried out returns from after the
  loop. An operation whose next instruction is a jump (`iter_resume`,
  `iter_unpack_delegate`, `iter_result`, `async_iter_close`) takes it to
  leave and skips it to go on: the verifier checks the jump is there.
  A synchronous yield* to a generator its realm's next steps is stepped
  directly, as the tree's `delegateNext` steps one (`regDelegateNext`):
  what the delegate yields goes on as a value, the frame suspended as at
  the `yield_star` four instructions on (the verifier checks it is
  there), with no result object at any level of a chain of delegations.
  The delegation's steps are placed at the operand's position, as the
  stack form's are.
- **A for await's step** is `async_iter_next`, `await` and `iter_result`
  into a register of its own; the body is in the ranges of the cursor's
  handler, which closes it as AsyncIteratorClose does -- the return
  method's result awaited, what closing throws dropped -- and a break, a
  continue of an outer loop or a return that leaves it closes it the same
  way, the result awaited and checked to be an object.
- **The drivers are the stack form's:** next, return and throw, an async
  function's steps and an async generator's queue call `resumeFull`, and
  `resumeFrame` hands a register generator's resumption to
  `regGenResume`, which answers with the same `resumeResult`; next
  resumes a suspended register generator of its realm at once. A for-of
  over a register generator steps it with no recover (`regGenStep`),
  its body throwing as register code throws: next's frame pushed as
  `stepIter` pushes it, no result object, no search for a tree. A
  generator whose parameters need no code (`end_params` the second
  instruction) is not run at its creation; its first resumption starts
  past `end_params`. A call site of a generator function or an async
  function makes its object without the generic call's search
  (`regGenCall`), an async function's first step at once.
- **An await's job continues in place.** An async function its await's
  job resumes from the job queue (`generator.awaitJob`, a frame of kind
  `regFrameAwaitJob`) goes on in place at an await of a value that is not
  an object or of a fulfilled promise of the realm's own constructor,
  where no job is waiting: the job the await would queue would be the
  queue's next, and the frame would be resumed with that value. The first
  run of an async function, which its caller's code follows, never does.

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
without it by the tiers that run it today. A runtime with the JIT runs
neither register code nor the register form's compiles (see Status), so
that its functions reach the JIT as before.

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

A direct eval and a `with` are compiled (Stage 2, item 8), and so is the
debugger's code (item 10).

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
  it covers once its measurements show a net gain: the default on amd64
  now (see Status).

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
  (Built: `internal/regdiff/regdiff_test.go`, run after every step.)

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

Reopened, 2026-11-11, for asm.js-style code, where the deferral's reasons
do not hold. Octane's zlib, Mandreel and Game Boy were 38.5%, 26.9% and
17.9% slower than the tree. Profiled (`-cpuprofile`, two runs each):

| | zlib | Mandreel | Game Boy |
|---|---:|---:|---:|
| time, tree → register (ms, one profile run) | 16,263 → 21,490 | 5,187 → 6,383 | 869 → 983 |
| `regInt32` and its `IsNumber`, register | 8.1 s of 22.1 | 1.5 s of 7.1 | small |
| the tree's `toInt32` and `i32Exact` | 1.05 s of 16.6 | | |
| `regElemMiss` (a typed array's element) | 3.8 s, 17% | 1.5 s, 21% | 0.16 s, 8.5% |

zlib's gap is the conversions: the bitwise operators' round trip of each
operand through a float (`regInt32`) and of each result back (`Int32`),
`or_imm`'s `x|0` alone 3.5 s of it, where the tree's int32 trees keep an
int32 in a Go variable through an expression. Mandreel's is the typed
heap as much: every `heap32[(r1+2)]` is a dense-array check missed and a
call of `regElemMiss`, which decodes the instruction again. Its locals are
declared without initialisers (`var r0;`) and its keys are sums, nothing
of which is provable; zlib's are initialised (`var b=0,c=0`) and coerced
(`a=a|0`), and 700 to 1,300 of its 4,319 `|0`s are of a bare local.

What stands in the way is the loop, not the proof:

- **Opcodes.** The design needs a raw form of each bitwise operator, its
  immediate form, the wrapping add and subtract, the compares-and-branches,
  `get_index` and `set_index` with a raw key, and the conversions in and
  out: about twenty values. Two are free (42 and 43); the 7-bit space is
  spent.
- **The loop's budget.** `runReg` is at 4,865 of Go's 5,000 nodes; twenty
  cases of 25 to 40 nodes each put it past the limit, where Go stops
  inlining `regInt32`, `regTruth` and a dozen more into every case.
- **A cheaper check is not one.** `regInt32` without its tag test (a NaN
  fails the round trip's compare too) made `loop_int32` 8.1% slower at
  every placement and zlib 1.6%: dropped. Nor is ToInt32 by truncation:
  `regInt32` as one compare of the bits without the sign against 2^63's
  (a tagged value, a NaN and the infinities fail it) and `int64(f)`'s
  low 32 bits, which takes fractions and numbers past 2^32 too, made
  Mandreel 2.5% and zlib 2.2% slower over four placements (35131ca9 ->
  it): dropped. `TestRegBitwiseToInt32` pins what either answers.

So the raw registers need room first: a wider opcode field and a loop
whose rarely hot cases leave it for `regCold`, or a second loop for code
the backend has typed int32 throughout (an asm.js function), entered at
the call and holding no Value register at all, which keeps `runReg` as it
is. Either is a change to the loop's layout as large as any made, and is
measured over the placements as one.

#### Raw float64 temporaries (designed, 2026-11-11, not built)

What NavierStokes measured: in register mode it was 10% slower than the
tree tier (two placements' worth of profile, three runs a mode, 6 s of
samples). Its time is `lin_solve`'s loop, 14 instructions an element --
`x[currentRow] = (x0[currentRow] + a*(x[currentRow-1] + x[++currentRow] +
x[lastRow++] + x[nextRow++]))*invC` -- and every intermediate of the
right side is a `Value` written to its register: the operands' tag tests
(`IsNumber`, 13.9% of the run), the canonical NaN of each result
(`regNum`, 3.8%), and the store of both words. The tree evaluates the same
expression as nested nodes that return float64s, and tests and boxes only
at the leaves and the root. The index cases' checks, the other 17%, were
trimmed by 2d91b42b (NavierStokes -7.7%).

The design keeps an expression's float64 intermediates raw, as the tree
does, without speculation and without a second representation of `Value`:

- **A raw region.** The backend marks a region: a maximal straight run of
  the arithmetic operators `+ - * /` and unary `-` (and, with the int32
  design above, the bitwise ones) over at least two operations, whose
  leaves are registers the region does not write -- locals, and
  temporaries computed before it, an element or a property read among
  them -- and whose interior results are read only inside it. Nothing in
  a region calls, stores, throws or jumps: loads stay outside it, as
  Values. Its root's value is the one result it writes as a `Value`.
- **One guard at the head.** `guard_num` tests up to three leaf registers
  are numbers (more leaves, more guards), and on any that is not, jumps to
  the region's generic code: the same expression compiled as today. Once
  every leaf is a number, `+ - * /` of numbers is a number and nothing in
  the region can leave it, so its operations test nothing.
- **Raw operations.** `fadd`, `fsub`, `fmul`, `fdiv` and `fneg` write only
  the number word of their result register -- a float64 whose NaN, if it
  is one, has whatever bits the FPU gave it -- and read only number words:
  a leaf's, which is the number itself, or an interior's. The root's
  operation is the boxing form (`fadd_v` ...): it writes the whole `Value`,
  its NaN canonical (`regNum`'s test, once a region, not once an
  operation) and its pointer word nil. `IsNumber` is the guard's alone.
- **No rebox, no exit in the middle.** A failed guard is before anything
  was written but the region's own interiors, which nothing outside reads,
  so the generic code runs the expression from its start with the leaves
  as they were: the same answer, the same side effects (a string `+`'s, a
  `valueOf`'s), the same throw (a BigInt mixed with a Number), the same
  stack trace -- the generic instructions carry the source positions.
  Nothing raw is ever seen by a reader of `Value`s: the interiors are dead
  when the region ends, and the root is boxed.
- **The generic code.** Out of line, after the function's last
  instruction, ending in a jump back to the instruction after the region:
  the region costs no dispatch where the guard holds. Code size: each
  region twice, plus a guard and a jump.
- **The collector.** An interior's pointer word is not written: it keeps
  whatever the register held before, a pointer the collector already
  knew, so nothing it sees is invalid; `sweepStaleSlots` clears it at the
  next back edge, as it clears any dead temporary today. No raw word is
  ever read as a pointer.
- **The verifier.** It checks each region: its interiors are written by
  raw operations and read only by raw ones of the same region; every leaf
  is covered by its guard and not written in the region; no instruction in
  it calls, stores or jumps, and no label is inside it (a jump or a
  handler cannot enter it); the guard's target is the region's generic
  code, which ends in a jump to the region's end. A register is raw only
  between its raw write and its last raw read, so no join, no handler and
  no call ever meets one. A generator's suspension, a debugger's stop and
  a direct eval are statements, never inside an expression's region.
- **Opcodes.** Eleven values: `guard_num`, five raw operations and five
  boxing ones. The 7-bit space has one free value (42). The 18 cases of no
  hot operation (layoutcases.txt) become cold sub-operations, which frees
  them, and the raw operations are cases after the hot ones until timing
  puts them among them (a hot-case edit, with the golden file).
- **What it saves, estimated.** In `lin_solve` the region is the right
  side above: 7 operations, 5 leaves in 2 guards. Per element: 7 tag tests
  of operand pairs become 5 in two instructions, 6 canonical NaNs become 1,
  6 pointer-word stores become 1, one dispatch more (the second guard)
  for none of the boxing. Of the 14% and 3.8% above, about two thirds go:
  10% of NavierStokes, which would put it level with the tree. Crypto's
  `am3` gains with the int32 form, not this one.
- **Tested by difference.** regdiff's arithmetic over numbers, strings,
  BigInts, objects with `valueOf`, NaNs of every payload, -0 and the
  infinities, each leaf of a region in turn not a number.

What it does not do: speculate on a load's type, or keep a value raw
across a statement, a call or a loop's back edge -- a loop variable stays a
`Value`. That is where the int32 design's verifier of kinds would be
needed; this one needs none beyond the region's.

Scheduled, 2026-11-11, after the measured gaps against the tree. At
2d91b42b NavierStokes is level with the tree in register mode (1,653 ms
against 1,680, three runs each): the indexing trim closed the gap this
design was first written for. What it still removes is 17 to 19% of a
float loop (`IsNumber` 12.5%, `regNum` 5.0% of NavierStokes), and level
with the tree is not the goal: the tree goes once register mode is level
or better everywhere, and the aim is the comparison engine. The order:
generator resumption in place and the forwarder construction path first,
both measured gaps against the tree, then this, in two stages, each
timed alone -- the 18 cases of no hot operation made cold sub-operations,
which frees their opcode values (a compare-and-branch that slows a test
3% at every placement stays a case), then the region pass, its opcodes,
the verifier's rules and the differential tests behind a compiler switch,
made the default only if it earns it over its cost in register-code
bytes and compile time. The region machinery is shaped for int32 regions
(`guard_int32`, raw integer and bitwise operations, Crypto's `am3`)
without building them.

#### Raw int32 regions (designed, 2026-11-12, not built)

The float64 regions' machinery, for the bitwise operators: what zlib's
and am3's conversions cost (`regInt32` 8.1 s of zlib's 22.1) is each
operand's round trip through a float and each result's boxing, where the
tree keeps an int32 in a Go variable through an expression. An int32
region is a float64 region whose interiors are held as int64s and whose
leaves are guarded as int32s.

- **Exactness.** JavaScript computes `(s + i*3) | 0` in float64 and
  applies ToInt32 to the root. An int64 interior equals the float64 one
  exactly while every interior is an integer of magnitude at most 2^53:
  a float64 operation on exact integers whose exact result is within 2^53
  is exact, and ToInt32 of an exact integer is its low 32 bits. So the
  backend bounds every interior's magnitude statically: a guarded leaf
  by 2^31, a constant by itself, `&` `|` `^` `<<` `>>` by 2^31, `>>>` by
  2^32, `+` and `-` by the sum of their operands' bounds, `*` by the
  product, `%` by its dividend's, unary `-` by its operand's. An
  operation whose bound is at most 2^53 checks nothing. Past it, its
  checking form (`_c`) compares the result's magnitude with 2^53 and
  bails past it (below), and its bound is 2^53 after. An operation whose
  operands' bounds could overflow an int64 -- a product of bounds past
  2^63 -- ends the region: today's generic instruction computes it as a
  `Value`, a leaf of the region above it.
- **Multiplication.** `i*3` of a guarded leaf `i` is bounded by 3 * 2^31
  and checks nothing. A product of two guarded leaves, am3's `xh*l`, is
  bounded by 2^62, fits an int64, and is `imul_c`, one compare of its
  magnitude. A product of a checked interior (2^53) and an operand
  bounded past 2^9 ends the region. No product is computed in float64
  inside an int32 region.
- **A bail, not an exit.** A failed check jumps to the region's generic
  code, as a failed guard does. Nothing has been written but the
  region's interiors, which nothing outside reads, so the generic code
  runs the expression from its start with the leaves as they were: the
  same value, the same rounding of a product past 2^53, the same effects.
  A region's guard and checks have the one target.
- **Division and modulo.** `/` is not an int32 operation: a region with
  one is a float64 region. Such a region may end at a ToInt32 consumer
  -- asm.js's `(a / b) | 0` -- through `f2i32_v`, the root's whole
  ToInt32 of a float64: int64 truncation below 2^63, 0 for NaN and the
  infinities, the generic conversion (which calls no script) for the
  rest. `%` is `irem_c`: Go's remainder truncates and takes the
  dividend's sign, as JavaScript's does for a divisor that is not zero.
  A zero divisor, whose result is NaN, bails.
- **-0.** An int64 has no -0, and the integer operations make one:
  `0 * -1`, `-0`, `-4 % 2`, `-0 - 0`, a leaf holding -0 (the guard admits
  it: the round trip takes it for 0). Where the root is a ToInt32 or
  ToUint32 consumer -- `|`, `&`, `^`, the shifts, `>>> 0` -- the
  conversion loses a zero's sign, and inside the region none is
  observable, since nothing in it divides. Where the root is not, the
  backend tracks whether it may be -0: a bitwise result never; a leaf
  may; `a + b` only if both may; `a - b` if `a` may; `a * b` if one may be
  zero and the other negative; unary `-` if its operand may be zero; `%`
  if its dividend may be negative or -0. A root that may be -0 is boxed
  by `ibox_z`, which bails on a zero result so that the generic code
  makes the zero with its sign. A root whose single reader cannot tell
  -0 from 0 -- an element key (ToPropertyKey(-0) is "0"), a compare, a
  branch's test, a bitwise operator -- is boxed by `ibox`.
- **A root past int32.** Where the root is not a ToInt32 consumer --
  `(a & 0xff) + b`, `(x >> 2) * 3` -- its value is the int64 interior,
  at most 2^53 by the bounds, boxed as its float64, which is exact.
  ToUint32's root (`ibox_u`) boxes the `uint32` of its interior.
- **The guard.** `guard_i32` tests up to three leaf registers: each a
  number whose float64 is an int32 -- `IsNumber` and `regIsInt32`'s round
  trip. A `Value` holds a float64 and there is no int32 tag, so an int32
  leaf is always a float64 that holds one. Inside the region an interior
  is an int64 and needs no round trip: a bitwise operator's ToInt32 of
  one is its low 32 bits. `regIsInt32` thus moves to the guard, once for
  each leaf in a region. The generic bitwise cases outside regions keep
  it as it is.
- **Which kind.** A region over `+ - * %` and the bitwise operators,
  with at least one bitwise operator and no `/`, is an int32 region; any
  other is a float64 one. A leaf that is a number but not an int32 fails
  the guard each time and runs the generic code, at the guard's cost
  over today. The counting build measures each site's guard failures
  over the corpora before the pass is the default.
- **Raw operations.** `iadd`, `isub`, `imul_c`, `irem_c`, the five
  bitwise ones, unary `ineg` and `inot`, `iadd_c` and `isub_c` for the
  checked bounds, and the immediate forms of the bitwise operators and of
  `iadd` (an asm.js expression's constants). Each writes the number word
  of its register as an int64's bits. The root is boxed by a separate
  `ibox`, `ibox_z` or `ibox_u` instruction rather than a boxing form of
  each operation: one dispatch more for each region, against a dozen
  opcode values. About twenty values in all, from those stage 1 frees.
- **The verifier.** The float64 region's rules, and: an int32 interior
  is read only by int32 operations of its region. It recomputes every
  bound and rejects an unchecked operation past 2^53, a product whose
  operands' bounds pass 2^63, an unchecked `%`, and an `ibox` where the
  root may be -0 and its reader can tell.
- **Tested by difference.** regdiff's int32 arithmetic: products past
  2^53 (`(a*b)|0` of two int32s near 2^31, where Math.imul gives another
  answer), sums past 2^32 and 2^53, -0 from each source with and without
  a ToInt32 root, a zero divisor, `>>> 0` of negatives, shifts by 32 and
  more, and in turn a leaf of 1.5, -0, NaN, 2^31 and a string.

Where a region is not formed, it costs nothing; where its guard fails, it
costs the guard. In zlib's `c = (c + 1) | 0`, it saves the round trips
of `c` and of `c + 1` and one boxing. A bare `x | 0` is one operation,
not a region, and stays `or_imm`.

**Opcode values for the regions.** Stage 1 frees 20 values, and one more
is free: 21, against 11 for the float64 regions and about 20 for the
int32 ones. A region instruction that interprets its own operations is
not the answer: that is a frameless evaluator inside the loop, which this
design rules out. A region operation gets a value of its own, as every
other operation does, only by its measured frequency: once the region
pass exists behind its switch, the dispatch counts over the corpora decide
which get values. The rest are cold sub-operations, which a region may
hold like any other instruction. The expected direct ones are
`guard_num`, `guard_i32`, `fadd`, `fsub`, `fmul`, the boxing roots,
`iadd`, `isub`, `imul_c`, the bitwise operations and shifts, and `ibox`.
The expected cold ones are `fdiv`, `fneg`, `irem_c`, `ineg`, `inot`,
`ibox_u`, `ibox_z`, and the immediate forms where a register operand
does as well.

Open, 2026-11-12: `string_concat` is 5.5% slower at every placement since
f25d1df4 put a computed receiver in the call's result register and copied
it into the window with `get_method_w`. Both forms take the slow path for
a primitive string receiver (`regGetMethod`'s `primProps`), so the new
form costs one store more. The code it runs is identical, as are its
offsets modulo 4 KB, its data, its allocations and its collections; the
time is in data loads, and the old shape is level at only two
placements of four. The fix is a fast path in `get_method` and
`get_method_w` for a primitive string's method, read inline from
`primProps`. It is general -- every `"str".method()` -- and is made with
stage 1's hot-case edit.

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
(`WithDebugger`; done, item 10), a finally clause nested more than four
copies deep (done, item 11), and the caps -- more than 65,536 registers (done, item 15), a jump past 16
bits, a name past 16 bits (done, item 14), more than 255 arguments, properties or
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
2. **Top-level code** (49,389, most of test262; every script and module)
   -- done for scripts and modules (`regcomp.CompileTop`, top.go): the
   body a function of registers, its bindings the global object's, the
   global lexical environment's or the module's. What it declares before
   its first statement the stack compiler records as it emits it
   (`Record.Top`): `check_global_lex`, `check_global_var`,
   `define_global_var`, `declare_global_lex`, `declare_module_lex` and
   `define_global_func` of each hoisted function's closure, made in that
   order as cold operations that do what executeAt's cases do
   (zzreg_top.go); a lexical declaration's store is `init_global_lex` or
   `init_module_lex` (`ResGlobalLexInit`, `ResModuleLexInit`), and a
   var's `set_global` whatever the strictness (`ResGlobalVar`). A
   module's declarations function (`<module bindings>`) is register code
   of no statements. The completion value is a register of its own
   (`Record.Completion`): an expression statement sets it once its value
   is made, the statements that may produce none reset it, and each copy
   of a finally clause keeps the try block's around the clause, as the
   stack form does -- whose copy before a break or a continue did not,
   and gave the clause's value where Node gives the block's (fixed with
   it). `typeof` of a name no binding resolves is `get_global_opt`
   (cold), which the fused `typeof x === "..."` takes too. A loop's
   backward jump records how many registers its head has live
   (`RegCode.BackLive`), and the sweep of stale slots clears the rest:
   a call's window kept what it was given until written over. A
   Program's register literals are seeded like its stack ones
   (`listSites`). Eval code, a module with top-level await and code
   compiled for a debugger are items 3 and 8.
3. **Generators, async functions, async generators, `for await` and a
   module's top-level await** (12,737) -- done ("Generators and async
   functions" above): the frame's window copied into the generator on a
   suspension, a return injected at a yield compiled as a return there,
   yield* and for await as loops of the code's own, an await's job going
   on in place at a settled value, and a for-of's step of a register
   generator with no recover. The JIT learns no register generator
   (`jitGenSeen`, `jitGenMakeable`).
3b. **Captured lexical bindings in loops** (878, with catch bindings and
   a head expression's closure of the head's binding) -- done: each
   turn's binding is a new one by closing the captures open on its
   register (`close_regs`) where the binding is made again -- a block's
   entry, a for-of or for-in turn's head, a catch clause's entry, its
   value waiting in a temporary meanwhile -- so that the closures of the
   turns before keep theirs, closed. A for head's let is copied from turn
   to turn (CreatePerIterationEnvironment): closed after the head's
   initialization and at each turn's end, before the update. A head
   expression's closure of the head's binding reads it in its dead zone,
   the bindings the stack compiler records for the head (`Record.Heads`).
3c. **Names in their dead zone, and for-of and for-in stores** (626 and
   721) -- done: the stack compiler records a dead-zone upvalue's read and
   write as `ResUpvalCheck` (`get_upval_check`, `set_upval_check`, cold,
   the dead zone outranking constness), a binding of its own function
   written before its declaration as `ResLocalCheck` (`check_ref` and a
   move), and a function expression's own name written as the constant's
   store or nothing, by strictness. A for-of or for-in loop stores each
   turn's value to any binding a store reaches: a global var, a global
   or a module's let, a checked binding, a constant (which throws). Their arguments object is made by
   the creation's run, from the arguments the generator keeps; a mapped
   one's aliases are captures of the parameters' registers, which move
   with the window.
4. **Classes** (about 27,000 together: constructors, accessors, fields,
   static blocks, private members and methods, `super`, `new.target`,
   class expressions inside functions, and a module's `export default
   class {}`) -- done (regcomp's class.go, the VM's zzreg_class.go). The
   stack compiler records what it made of each class (`Record.Classes`,
   a `ClassRec`): the slots of the class scope's bindings -- its inner
   name, its private names' keys, its list of private methods, its
   computed field keys -- and the constants of the constructor, the
   instances' field initializer, each member's function and each static
   field's or block's, in the order they run; and where each private
   name's key is (`Record.Privates`, a register or an upvalue). The
   register form makes the class in the stack form's order with cold
   operations of the stack form's instructions' names (`new_class`,
   `define_method`, `class_getter` and the rest, `private_name`,
   `add_private_method`, `define_private`, `set_field_init`), the static
   elements called as methods of the class, and closes the class scope's
   captured bindings at its end (`close_regs`), so that a class made in
   a loop gives each evaluation's methods their own. A field initializer's
   `FieldInit` is `define_index` or `define_private` on this, and its
   `InstallPrivateMethods` `install_private_methods`. A private member is
   read, written, updated, called and tested (`#x in o`) through its
   key's register (`get_private`, `set_private`, `private_in`, the name
   for the errors in the instruction's Aux); `super.x`, `super[k]` and
   `super.m()` through `super_get`, `super_set`, `super_base` and the
   index forms, the frame's this written to the record first. A derived
   constructor's this, and an arrow's written inside one, is the binding
   super() binds (`RegCode.ThisRef`): `this_bound` reads it, checked,
   `super_call` (`super_forward` for the constructor a derived class is
   given, `super_spread`) binds it, and every return goes through
   `derived_ret` (`derivedResult`); `regStmt` runs such a frame. A class
   constructor has no direct call path (`regPlan`, `regCallSlow`): a call
   throws, and a construction goes through construct and runFD. The stack
   form's this before super(), a private method's read for a call and a
   read in the dead zone had no position of their own (fixed with it).
   After it, with the strict global store (item 7): all corpora 89.6%
   register code (70.0%), Octane 99.9%, the bundles 94.1%.
5. **Parameter patterns, defaults and rest; destructuring everywhere**
   (declarations, assignment targets, catch clauses, for-of and for-in
   heads; about 4,500) -- done (regcomp's pattern.go). A pattern unpacks
   a register in the stack form's order: an array pattern's cursor is
   for_of_start's, stepped by `iter_step` and `iter_rest` (cold) and
   closed by `iter_close` at the end, its steps in the ranges of a
   handler of the cursor's that closes it for a throw (`iter_close_throw`,
   which an iterator whose `next` threw has marked done), as a for-of
   body's; an object pattern checks its source (`check_coercible`), reads
   each key -- a computed one converted first (`to_property_key`) -- after
   its leaf's object and key are evaluated, and makes its rest
   (`object_rest`) leaving out the keys it read, kept in a window, each
   evaluated once (the stack form evaluated a computed one again for the
   rest: fixed with it, as Node has it). A default replaces undefined,
   the source copied first where it is a binding a store may assign. A
   list of parameters with a default, a pattern or a rest binds them in
   order: those a checked read or a closure may see wait in a register of
   their own while the parameter is in its dead zone (`paramsAside`), and
   a rest is `rest_param` over the arguments the frame keeps
   (`RegCode.KeepsArgs`). A for-of or for-in head's pattern or property is
   unpacked at the head of each turn, inside the iterator's handler.
6. **BigInt literals** (1,912), **templates and tagged templates** (661),
   **object literals of every property kind** (1,482) -- done, with
   spreads, holes, optional chains, `??`, logical assignment and
   assignment to a constant (regcomp's literal.go). A BigInt is a
   constant. `X.asUintN(w, x op y)` and `asIntN`'s -- a 64-bit hash's or
   generator's step -- make their operator `wrap`, cold, right before
   the call: where the callee is BigInt's and x and y BigInts held in
   their Values, it makes the call's value on machine words
   (`wrapShort`, the tree's `wrapCall`) and passes the call over, with
   no BigInt past 64 bits on the heap. A template's parts go to a window that one `concat` joins,
   converting each with ToString; a substitution is converted at once
   (`to_string`, cold) only where a later part may run code, which could
   see the order. A tagged template is a call, a method's with its
   receiver, given its site's object (`template_object`, the site the
   stack compiler records, `Record.Templates`). An object literal's other
   properties are cold operations doing what the stack form's do:
   `copy_data_props` (a spread), `define_getter`, `define_setter` and
   their computed forms, `define_index` after `to_property_key`,
   `set_func_name`, `set_home_object` and `set_proto_of` (`__proto__:
   v`); a number key is a computed one. An array's hole is the
   uninitialized marker in its window; with a spread, the elements
   before it are made at once and the rest pushed or spread
   (`array_push`, `array_spread`), and a call or a `new` with a spread is
   `call_spread` of such an array. An optional link checks its object
   with `jnot_null`, and where that falls through, === undefined and
   === null, so that document.all's stand-in goes on; a nullish link
   jumps to the chain's end, where it is undefined, and `(o?.m)()` keeps
   its receiver. `??` is the same check; `a ||= b`, `&&=` and `??=` read
   the target once, a computed key converted once (`key_of_base`), and
   store only where the read does not decide. An assignment to a constant
   throws (`assign_const`). The stack form's reads in an optional chain
   had no position of their own (fixed with it).
7. **The arguments object in every form** (354), **names the backend does
   not read** (434, 335 of the bundles'), names the record does not
   resolve (92), strict global stores (96). The strict global store of a
   value that runs code is done: whether the name resolves is settled
   before the value is made and reported after it, `check_global_ref`
   and `assert_resolved`, as the stack form's. A strict function that
   ends in a tail call makes its arguments object as any other: an
   unmapped one is a copy, which a frame taken over in place leaves
   alone (69). A function that names eval without calling it has an
   arguments binding the stack form fills for an eval that never comes;
   nothing reads it, it has no register, and none is made (217). What
   the record did not resolve was one form, 642 functions of test262's:
   dynamic import, `import(x)`, `import.source(x)` and
   `import.defer(x)`, whose callee the parser makes a name -- done: a
   cold `import` of the specifier and the options in a window, at the
   call's position, as the stack form's; `import.meta` is `import_meta`
   (cold).
8. **eval and `with`** (757 direct evals, 527 functions beside an eval's
   variables, a with or an Annex B function, 11 more) -- done. A call the
   stack form made a direct eval (`Record.Evals`, with its scope's index)
   is `direct_eval` (cold): the callee, its receiver and the arguments, or
   an array of them, in a window. Where the callee is the realm's eval and
   the first argument a string, the code is compiled and linked to the
   frame as evalIn links it to a heap frame's, each binding it names
   captured from its register (`RegCode.LocalReg`, the slot's register)
   as a closure's capture is, its this from register 1; otherwise it is
   an ordinary call, a tail call where marked. A function with a direct
   eval gives every binding a register and makes its arguments object as
   it starts, and is called through runFD, which makes the scope of its
   eval's variables. A name a with object or an eval's variable may
   answer is compiled as the stack form compiles it (`Record.Withs`, the
   probe and its limit): a cold probe of the innermost limit of the
   frame's with objects and eval's variables -- `with_get`,
   `with_get_this`, `with_delete`, `with_set` -- that sets a found flag
   in a register of its own, and a jump around the static access; a
   reference that is read and written, or resolved before its value is
   made, is `with_resolve` and then `with_read` and `with_write` through
   the object it found. A with statement's object is put on the frame's
   with objects by `with_push` and taken off by `with_pop`, compiled for
   each way out as a finally clause is, a throw's in a handler of its
   own. A frame runs as register code whatever with objects and eval's
   variables runFD gives it, and a generator keeps them across a
   suspension. The stack form probed a binding typeof reads twice
   (fixed with it). Eval code itself is register code too (item 9e).
9. **`using` and `await using`** (177) -- done (regcomp's try.go,
   `usingScope`): a statement list, a for head or a for-of head that
   declares with using is a scope whose resources are in a register of
   its own (`new_dispose_capability`), each value added as it is
   declared, before its binding is initialized (`add_disposable`), and a
   finally clause that disposes of them, compiled for each way out as
   any other: the normal end and each exit with no exception to fold in
   (`dispose`), a throw with the one it folds in. An await using scope's
   disposal is a loop of `dispose_step`s, each running the disposers
   until one leaves a value to await, which the loop awaits in a handler
   whose throw the next step folds in, as the stack form's
   compileDispose. Then the finally depth (completion dispatch, no cap),
   the debugger's code (statement markers in register code), and the
   caps.
9b. **Labelled statements that are no loop, declarations in switches,
   delete of a name** (69, 36 and 41) -- done. A labelled statement of any
   other kind is a target only a break naming it reaches (`loop.named`),
   and a statement's labels are a set (`labelSet`): `a: b: while` is the
   target of a break or a continue naming either, which the parser and
   the stack compiler took as b's alone (fixed with it). A switch's case
   block is one scope, entered once the discriminant is made
   (`enterBlock` of every case's statements). delete of a name is false
   for a binding and `delete_var` (cold) for a global, as the stack form
   resolved it (the record's read of the operand); delete of an optional
   chain's property deletes it, true where a link is nullish -- the stack
   form deleted nothing (fixed with it).
9c. **Function declarations in blocks, switches and behind labels**
   (128, and the Annex B functions of item 8's count) -- done. A block's
   functions are made as it is entered, after its lexical bindings, a
   closure's capture closed first in a loop (`enterBlock`); a labelled
   declaration is its list's, as an unlabelled one is (the stack form
   never made one: fixed with it). Annex B's var is assigned as the
   declaration is evaluated, with what the block's binding holds then
   (B.3.3.1), a store the stack compiler records as the declaration's
   name's write -- it assigned it as the block was entered, which Node
   does not (fixed with it).
9d. **The last refusals of the corpora** (40) -- done; the debugger
   statement outside a debugger's code does nothing, as the stack form's.
   - *A finally clause's bindings* (9, "a name the record resolves two
     ways"): the stack form compiles a clause once for each way out --
     inline at the normal end, as the handler's, and at each break or
     continue that leaves it (unwindTo) -- each copy's bindings in slots
     of their own (slots are never reused), so
     a name in it resolved to one slot in one copy and another in the
     next. The record keeps the first copy's notes and drops the later
     ones' (`Record.BeginFinally`); regcomp compiles all its copies from
     the first's slots, which no other binding has, and finds the first
     copy's function constants (`closure` by start).
   - *Sloppy code's assignment to a call* (12: `f() = v`, `f() += v`,
     `f()++`, `for (f() in o)`): the call made, then
     `throw_reference_error` (cold) at the target, the value never made,
     as the stack form's compileCallTarget. A logical assignment to a call
     is a syntax error, and the other targets are panics, not refusals.
   - *Annex B's `for (var a = init in o)`* (4): the initializer assigned
     once, before the object is made. *A let's own initializer storing
     it in its dead zone* (`let y = [y] = []`, 2): the pattern's store
     checks the dead zone as an assignment's does.
   - *The caps as wide forms.* A call of more than 255 arguments -- plain,
     a method's, an optional chain's, new, super(), a tagged template's,
     a direct eval's -- passes an array of them as a spread call does
     (`wideArgs`, `call_spread`); a spread call through an optional chain
     is chainCall's. An array literal of more than `arrayChunk` (1,024)
     elements makes that many at once and pushes the rest; a literal of
     constants past 16 bits names its count by its table's (new_array_k's
     B 0); an object literal's room is a hint, at most 65,535. A function
     with more cache sites, or call sites, than 16 bits number is compiled
     again with its sites shared by key (`site16`, `siteKey`): the kind of
     cache and the name, a literal's keys or constants, a call's
     operation -- entries that answer alike for every instruction of the
     key, a call's checked against its callee. A function whose stack
     form alone has nearly as many (Mandreel's global_init, 155,109;
     test262's string-upper-lower-mapping.js, 65,561) is compiled a
     third time with the register form's sites numbered first, from 1,
     shared by key, and the stack form's moved past them
     (`moveStackSites`: the B of every instruction the stack compiler
     gave a site, and the object literals' `LiteralKeys`): the two forms
     index one closure's caches, and only the register form's operands
     are 16 bits.
   - A jump of a 16-bit offset that does not reach (`errJumps`): the
     function is compiled again with every such jump far
     (`farJumps`), the branch taken to a `jmp` of 32 bits, past a
     `jmp` over it where it is not: three instructions where there was
     one, only in a function past 32,767 of them.
   - `with (o) f(...a)` passed no receiver in the stack form (fixed with
     it): a spread call probes with `with_get_this` as any call does.
9e. **Eval code** -- done. The top level a direct or an indirect eval
   evaluates, strict or sloppy, and the Function constructor's code, are
   compiled as any top-level code (`regScript` refuses only a debugger's),
   and so is every function written inside them, which the record skipped
   where the code had an eval scope. The caller's bindings are upvalues by
   name (`evalIn` captures them from the frame, its register for register
   code); a with object's names are probed as in the caller. A var or a
   function the code declares goes where the stack form's
   define_global_var and define_global_func put it: the global object, or
   inside a function the frame's eval variables (`f.evalVars`, which
   runFD gives the frame), which the cold `define_global_var` and
   `define_global_func` now do too. A function the code declares where
   the caller binds the name as a var is stored into that binding, as
   the stack form's storeVar does: a declaration of its own in the record
   (`TopOp` set_upvalue, of an identifier of its own, whose write the
   record resolves, a with object's probe included). The completion value
   of a with statement whose body produces none is undefined (fixed with
   it).
10. **The debugger's code** (`WithDebugger`) -- done. The stack compiler
   records each debug_stmt it makes by its position (`Record.Debugs`,
   the first copy's in a finally clause compiled more than once), and
   regcomp makes a cold `debug` at each statement, loop test and update
   and for-in or for-of head of that position, the statement's index in
   Aux: it does nothing unless a debugger has asked to stop, and then sets
   the frame's pc to the stack form's debug_stmt, by which the debugger
   finds the statement, its scope and its position, and asks the debugger
   as executeAt's case does. Every binding has a register, as beside a
   direct eval, and `RegCode.LocalReg` names it: a `DebugFrame` of a frame
   that runs as register code (`regDebugFrame`) reads and sets its
   bindings there (`Scopes`, `SetVariable`), its this in register 1, and
   evaluates code there as a direct eval of the register form's
   (`regEvalCode`). Breakpoints and steps are the statements' indexes, as
   in the stack form.
11. **Finally clauses nested past four copies** -- done (`finShared`). A
   try statement compiled inside `finallyDepth` copies of clauses has its
   clause compiled once, at its end, with a completion dispatch: each way
   in loads its kind in a register of the statement's -- 0 the block's
   normal end, 1 a throw from the handler, k > 1 an exit -- and jumps to
   the clause, after which compares of the kind (`RJSEqI`) go on: past the
   statement, to the throw again, or to the rest of exit k, the clauses
   outside and its jump or return, compiled there with the registers the
   exits keep live left alone. A clause inside one so compiled is
   compiled once too, so that the code grows with the nesting, not as
   its power. The stack form's copies of a clause with a try statement
   of its own lost the clauses outside it (fixed with it).
12. **Entries as register code's own** (`regRunFD`). What runFD would
   run -- a call through callObject or callDirect, a script's or an
   eval's code, a tree's call (callTreeWith) -- is entered as register
   code directly where the function has any (`fn.Reg`, which only
   register mode's compiler makes, so that the default tiers' paths test
   a field they hold already): its frame given what runFD gives one of
   its surroundings -- the with objects and the eval's variables of the
   functions around it, a scope for its own direct eval's -- and a chain
   of tail calls it ends in made in its place, in constant space
   (regTailLoop). A direct eval's function and one inside a with are so
   no longer entered through runFD, nor is a construction
   (constructRun, and callTreeWith whatever the new.target; planTreeNew
   gives register code no tree's) or a frame with a this binding: a
   derived constructor's construction makes the binding super() binds,
   an arrow written inside one shares it, and the frame record's this
   is register 1 at entry, which superCall reads and rebinds. Eval
   code's this is the call site's, which may be such a binding: top-level
   code records it (`Record.ThisRef`, the stack compiler's thisIsValue)
   and reads this through the binding, as such an arrow does. Strict
   eval code's function declarations, bindings of its own rather than
   top-level declarations, are made (they were not: test262's name-unicode.js,
   strict).
13. **The fallback counter** (`vm.RegFallbacks`, QJS_REGFALLTRACE to
   name each). Register mode counts every time it hands code to the
   other tiers: a function firstForm builds a tree for, having no
   register code; a frame regStmt runs in the stack interpreter; and
   register code entered through runTree, a frame runFD or callTree
   made rather than its own -- the node that runs register code in a
   tree is reached no other way. With constructions entered as
   register code (item 12) the count is zero, and the regdiff tests (the
   micro list, the V8 suite and every corpus of theirs) and test262 in
   register mode, every directory, fail on any.
14. **Names past 16 bits** -- done. A name operand (a global's read or
   write, a cold operation's name, a private member's in its Aux) whose
   index in the function's names is 65,535 or more is `RNameFar`, and
   `RegCode.FarNames`, by the instruction's pc, has the index
   (`regNameIdx`, `regName`): the hot cases never read a name, so none
   tests for it. A global's cache site is keyed by the full index, and an
   object literal made at once keeps its keys as 32-bit indexes
   (`ObjKeys`). The verifier checks either form (`nameOK`).
   `TestRegDiffFarNames` starts each function, and scripts, with 65,600
   other names.
15. **Registers past 65,535** -- done. A function whose bindings alone
   would not fit 16 bits of registers, or that ran out of them compiling
   (compiled again with a quarter of them for its bindings, `nearCap`),
   gives the bindings it uses least far registers (`farSlots`): those
   only plain reads and writes reach -- no closure captures them, no read
   finds them in their dead zone, no direct eval or debugger reaches the
   function, and none is the arguments object, a catch clause's, a
   class's, a function declaration's or the completion value. A far
   binding's register is past every near one and every temporary, the
   window NumRegs long; `mov_far` and `mov_to_far` (cold) are all that
   reach it, and `far_init` makes the far bindings undefined as the
   function starts. A read is a move in, a store a move out, a compound
   assignment or an update the generic read and store, an append or a
   for-of head's register the long way. `r16` panics on a far register,
   which `generate` makes a refusal: a form that would name one as an
   operand is refused, never compiled to read another register. A
   check_ref's slot past 16 bits is FarNames', as a name is.
   `TestRegDiffFarRegisters` declares 65,600 bindings before the ones its
   cases use, in a function, a generator and a named function
   expression, and has a template of 40,000 parts beside 30,000
   bindings, which compiles the second time.

### Stage 3: the JIT

This stage moves the native tiers from the stack form to register code.
Once the stack interpreter and the tree tier are gone, the stack form is no
input the JIT can keep. It also decides whether a baseline compiler or an
assembly interpreter loop belongs under the optimizing JIT.

Citations are to branch `reg` at `4be8c5dc`, which another agent was editing
on 2026-11-10, so line numbers are approximate. Function names are exact.
Where this section infers rather than reads, it says so.

#### Where things stand

- **The JIT reads the stack form.** `compile.LowerSSA` lowers a function's
  stack bytecode to the slot IR (`internal/jit/compile/lower.go`, 2,716
  lines, 219 cases over `bytecode.Op`). The IR has one instruction per
  bytecode pc and a `StateMap{PC, Depth}` at each
  (`internal/jit/ir/program.go`, `StateMap` and `Program`). `ssa.Build`
  reads only the IR and an `ssa.Feedback` keyed by that pc
  (`internal/jit/ssa/build.go:27`). The `ssa` and `mir` packages do not
  import `internal/bytecode`.
- **The feedback is the VM's `jitFeedback`** (`internal/vm/zzjit_ssa.go`,
  `type jitFeedback`, about 90 methods across the `zzjit_*.go` files). Each
  method takes a stack pc and reads the `bytecode.Instr` there. It reads:
  - the property site, from `in.B` into `closureCode.ic`;
  - the global's cell;
  - literal pools (`jitLiteralAt`);
  - call targets. These are learned mostly from native code's own exits
    (`jitEntry.nativeCalls` and `inlines`, `jitTargetsOf`), and from call
    frequencies counted at callee entry while the caller warms
    (`jitCountCallAt`, `zzjit_frequency.go`);
  - what native code met at an exit: `Generic(pc)`, `typedReads`,
    `keyedReads`.
- **Tier-up hooks.** There are three:
  - executeAt's `interrupted` block calls `jitBackEdge` (`vm.go:2946`);
  - `runFD` and the tree's call entries call `tryJITFrame` behind
    `r.jitOn()` (`vm_call_run.go:159`, `zcall_tree.go:167` and `:447`);
  - `tryJITFrame` counts framed calls up to `jitInvocationsForFeedback` (8,
    `zzjit_native.go:49`). Loops promote on a randomized back-edge budget
    (`backEdgeBudget`, `tryJITLoop`, `zzjit_native.go` about 2781 and 2807).
- **runReg has no JIT hook** (no `jitOn` in any `zzreg_*.go`). Its back edge
  decrements the runtime-wide `r.backEdges` in the shared `jump` tail and
  calls `regBackEdge` out of line (`zzreg_run.go`, the `jump:` tail and
  `regBackEdge`). Its calls decrement `r.interruptCounter` in `regEnterW`
  (`zzreg_call.go`, `regEnterW`).
- **runReg's caches** are in `closureCode` (`runtime.go:723`), which is the
  template's per runtime:
  - `ic []propCache`: property, global and literal sites. Register sites are
    numbered after the stack form's (`regcomp`'s `site`, `propSite`,
    `globalSite`), except a leaf's, which shares the stack form's sites.
  - `regCalls []regCallCache`, indexed by a call's C (`RegCode.CallSites`,
    `reg.go:520`): callee, closure, regCode, this and kind.
  - `regEpochs []uint64`, parallel to `ic`, for global sites.
  - `arrays`, for empty array literals.
  - `instanceof`'s site, in `Aux`.

#### 1. The feedback vector

**Principle: the fast path is the feedback.** In V8 each Ignition handler
writes its feedback slot (`BinaryOperationHint`, `CompareOperationHint`,
CallIC counts). runReg cannot afford that:
- a word has no field left for a slot index;
- each write is a load of the vector and a read-modify-write in a case;
- each case added to the loop costs nodes against Go's 5,000-node limit
  (4,875 used, "The loop").

The rule instead: what a case's fast path answers is the expected kind, and
anything else is recorded where the case already left the loop, on its slow
path, keyed by the pc the slow path already has (`kept.pc - 1`). The
optimizing JIT then speculates the fast path's kind wherever the site has no
"seen other" mark. This is the model the stack form's JIT already follows
for what native code meets: `Generic(pc)`, `jitIndexSeen`. It only moves the
first lesson earlier, from the JIT's first exit to the interpreter's first
miss.

**What each kind of site holds:**

| Site (register op) | Held today | Added | Where it is recorded |
|---|---|---|---|
| property read/method/store (`get_prop`, `get_method[_w]`, `set_prop`, `add_prop*`, `define_field`) | `propCache` by C: own shape and index, holder (`p1`/`s1`, `cell`), getter, up to four shapes polymorphic (`poly`), seen shapes with the JIT on (`refill`, `jitNoteSeen`), fills | none | the slow path, as today (`regGetProp`, `regSetProp`) |
| global (`get_global`, `set_global`) | `propCache` slot and env, plus `regEpochs[C]` | none | `regNoteGlobal` |
| element (`get_index*`, `set_index*`, `add_index*`, `get_length`) | nothing | a seen byte by pc: hole, out of range or append, typed array kind (reusing `TypedKind`'s codes), string receiver, string or symbol key, mapped arguments, other | `regElemMiss`, `regSetMiss`, the `typed:` and `slow:` tails' callees |
| arithmetic and bitwise (`add`, `sub`, `mul`, `bin`, `*_imm`, `inc`, `dec`, `unary`) | nothing | a seen byte by pc: string operand, BigInt, other non-number | `regBinary`, `regBinImm`, `regStep`, `regUnary` |
| compare-and-branch (`jlt`... `jseq`, `jeq`, `*_upval`) | nothing | a seen byte by pc: strings, mixed, objects | `regCompare` on `cmpSlow` |
| call (`call_w`, `call_method_w`, `new_w`, tail forms) | `regCallCache` by C: monomorphic target, closure, regCode and kind (enter, this, native, generator, marked, leaf) | a call count; a polymorphic mark when a miss replaces a different target | the count in `regCallW` (outside runReg); the mark in `regCallSlow` |
| literal (`new_object_lit`, `new_array_empty`, `new_array_k`, `regexp_lit`) | `propCache.next`/`s1` (the shape the keys led to), `arrays` (room) | none in the interpreter. The JIT's pools stay the JIT's (`jitLiteralAt`), keyed by register pc | as today |
| `instanceof`, `in` | site in Aux, cache by layout (`362e755e`); `in` through the realm's cache | none | as today |
| iteration (`iter_next`, `for_in_start`, `for_of_start`) | nothing | a seen byte by pc: array, generator, Map or Set, other | `regIterNext`, the starts in `regColdSub` |

Arithmetic kinds are not split into int32 and float in the interpreter. The
JIT already proves int32 by truncation (`ssa/ints.go`, `truncInts`) and by
entry checks (`OpCheckI32`). It does not speculate a Smi add with an
overflow check, as V8's `SpeculativeSafeIntegerAdd` does. If it ever wants
to, the speculation learns from its own exit, as `Generic(pc)` does today.
Recording int32 on the fast path would cost a branch and a store in every
arithmetic case, which the loop's budget and its layout cannot pay.

**Layout.** The vector lives where the caches already live, in
`closureCode`, per template per runtime, the analogue of V8's feedback
vector per closure template per native context:

```
closureCode
  ic       []propCache      by site (an op's C, or Aux): property, global, literal, instanceof
  regEpochs []uint64        parallel to ic, global sites only
  regCalls []regCallCache   by call site (an op's C), + count uint32, + poly bit in kind
  arrays   []*arrayRoom     empty literals
  regSeen  []uint8          by register pc, made at the first record, nil until then
  reg      *regCode         + hot int32: the tier-up counter (section 2)
```

- **Indexing by pc.** The JIT reaches a site by register pc: pc to
  instruction word, then C (or `Aux[pc]`) to the entry. That is today's
  `in.B` with another decoder.
- **`regSeen`** costs one byte per instruction, about an eighth of the code,
  and only for a function that ever missed.
- **Numbering in Stage 4.** Once the stack form goes, regcomp numbers sites
  from 1 with no stack-form offset. The leaf sharing in `propSite` ends with
  the frameless stack evaluators. `RegCode` then gains a table of sites
  (kind and pc) so that the verifier can check that each site is named
  once.

**Cost on the hot path:**
- runReg's cases are unchanged, and so is its node count.
- Every new record is in a function the case already calls.
- The one addition outside slow paths is the call count, an increment on
  the `regCallCache` line that `regCallW` has already loaded. It goes behind
  `if jitBuilt`, so an untagged build compiles it out and `hotdiff` sees no
  change.

#### 2. Tier-up from runReg

**Back edges: no new counter.** The stack form's tier-up for loops is
already sampling: the runtime-wide budget runs out (`r.backEdges`, randomized
by `backEdgeBudget` so that it does not alias with a loop's period), and the
frame where it ran out is credited (`fullBudget`, `jitTierWork`). runReg
spends the same budget at the same place, its `jump` tail, which already
stores `kept.pc` and calls `regBackEdge`. Changes:

- `regBackEdge` (out of line) gains a JIT branch behind `jitBuilt`:
  - it calls `r.jit.backEdgeBudget()` instead of resetting to
    `backEdgeCheckInterval`;
  - it computes the loop header from the jump at `kept.pc-1` (`kept.pc +
    regJumpOff(w)`);
  - it calls a register form of `tryJITLoop`. Nothing in runReg changes.
- **Coming back from native code.** Native code that finishes the frame or
  exits at another pc has to bring runReg there.
  - `regCatch` (out of line, `zzreg_switch.go`) learns a sentinel error.
    `regBackEdge` returns `errRegGoOn` with the pc to resume at in `kept.pc`,
    and `regCatch` returns that pc before it looks at handlers. runReg's
    `fail:` tail already continues at whatever `regCatch` returns.
  - A frame native code finished resumes at a `ret r0` that regcomp appends
    to every function, with the result written to r0. The function is r0's
    value only until the frame ends.
  - So OSR in either direction adds no instruction to runReg (inferred
    design, not built).

**Calls: one counter outside the loop.** The counter goes in `regEnterW`,
which every framed register call goes through, on the `regCode` whose
`numRegs`, `args`, `params` and `inits` it reads anyway:

```
if jitBuilt {
    if rc.hot--; rc.hot < 0 { return r.regEnterHot(o, cl, rc, base, argc, args, newTarget) }
}
```

- **One branch, two uses.** `regEnterHot` counts invocations toward the
  stack form's `jitInvocationsForFeedback` and toward `jitHotWork` (adding
  the function's length, as `jitHotEntry` adds `len(Code)`). It resets the
  counter, and it decides.
  - Once a function has native code, `hot` is held negative. Every call
    takes the same branch into the native entry, the way V8 swaps a
    function's code pointer.
  - No call site has to be found or patched, and no case of `regCallW`
    tests a new kind.
- **`regDirect` takes the same check.** That is the callbacks' entry.
- **Leaves need no count.** Bodies answered without a frame (`regLeafArg`,
  `regLeafConst`, `regLeafCode`, `pureCallAt`) are not compile candidates.
  The optimizing JIT inlines them from the call site's cache.
- **Cost by build.** Untagged: nothing, since `jitBuilt` is a constant
  false. Tagged, JIT on or off: a decrement on a loaded line and a branch
  never taken. That is `tryJITFrame`'s `r.jitOn()` test today, now off the
  tree's path, and it must be shown level with `placements -micro`.
- **The fallback.** If the placements show the counter, credit the callee
  when `r.interruptCounter` runs out instead. That is the same decrement
  regEnterW makes today, sampled one call in 4,096. It costs nothing, but a
  function is chosen statistically rather than counted.

**Against the stack form's counters:**

| | Stack form | Register form |
|---|---|---|
| Loop | the runtime budget runs out in `executeAt`'s `interrupted` block, then `jitBackEdge` and `tryJITLoop` at the stack pc | the same budget runs out in runReg's `jump` tail, then `regBackEdge` and `tryJITLoop` at the register header; no change to runReg |
| Call | `tryJITFrame` on every framed call behind `jitOn()`: `jitCalls` up to 8, then entries and `coldWork` | `rc.hot` in `regEnterW` and `regDirect`, behind `jitBuilt`, out of line once it runs out |
| Call-site frequency | counted at the callee's entry against the caller's top frame while the caller warms (`jitCountCallAt`) | `regCallCache.count` at the site; frequency is the count over the function's invocations |
| Native entry from interpreted callers | `tryJITFrame` on every call | `rc.hot` held negative: the one counter branch |
| pc live across the dispatch | no | no: every hook is in a function runReg already calls with `kept` |

#### 3. The optimizing JIT's front end from register code

**Input.** Read register code, not the AST. The feedback, the frames and
the exits are all keyed by register pc. The AST has no pc, and regcomp has
already made its expressions three-address code and decided its fusions and
its scopes.

**What changes:**

1. **A new lowering, `compile.LowerReg(rc *bytecode.RegCode, fn)`**, into
   the same `ir.Program`.
   - A register is a slot. `Locals` is `NumRegs`, every `StateMap.Depth` is
     0, and there is no operand stack.
   - Most register instructions are one IR instruction with explicit
     operand slots, and the `Copy` traffic of push and pop goes away. The
     doc measured 20 to 35% fewer instructions than the stack form in the
     V8 suite's methods.
   - `liveLocals` becomes a backward liveness pass over all registers. It is
     cheaper, and more precise than the stack form's, whose operands are
     always live. So exits write fewer records.
   - `RegCode.Handlers` maps onto `ir.Handler`. Each handler's ranges become
     `HandlerAt`, and `Depth` is 0.
2. **Fused instructions.** `add_prop`, `add_index*`, `get_index_pre` and
   `get_index_post`, `get_method_w`, `char_code_at`, `new_object_lit` and
   `regexp_lit` with `RLitCall` expand to several IR instructions. That
   breaks the IR's "one instruction per bytecode pc" (`program.go`,
   `Program`'s comment).
   - `Program` gains `Origin []uint32`, the register pc of each IR
     instruction. The builder keeps keying feedback by IR index, and the
     VM's feedback translates through `Origin`.
   - **The atomicity rule.** An IR instruction "commits atomically with
     respect to guards" (`program.go`, `Op`). Inside a fused expansion, a
     guard may exit to the fused instruction's own pc only while nothing has
     committed. After a component with an effect (a setter or getter by Go,
     a host exit), the remaining components are built generic, so that what
     they did not expect goes to Go, as `Generic(pc)` builds do. They never
     deoptimize to a pc that would run the effect twice.
3. **The VM's feedback, re-keyed.** Every `jitFeedback` method that reads a
   `bytecode.Instr` reads an `RInstr`:
   - `siteFrom` and `property` read C;
   - `Global` reads C and `regEpochs`;
   - `NativeCalls`, `Inline` and `CallShape` read `regCalls[C]`. That is a
     monomorphic target from the first compile, where the stack form's JIT
     first learns targets from its own exits;
   - `Frequency` reads `regCalls[C].count`;
   - element, compare and arithmetic hints read `regSeen[pc]`;
   - `TypedKind`, `KeyedRead` and the like keep the JIT's own learned
     tables on `jitEntry` (`typedReads`, `keyedReads`, `callSites`,
     literals), now keyed by register pc.

   `zzjit_snapshot_gen.go` is regenerated (`go generate ./internal/vm`).
4. **Frames, exits and deoptimization.**
   - A register frame is a window of `r.stack` at `wb = f.base - locals`
     (runReg's `regWindow`). The value stack never moves, so native code
     reads and writes slots in place, exactly as it does the stack form's
     (README, "Values and the garbage collector"). `Context.Locals` is the
     window's base. An exit's records write registers, and its pc is a
     register pc.
   - Go resumes the frame with runReg at that pc. That needs runReg to take
     a start pc (`runReg(f, c, pc)`, callers passing 0), which is one more
     argument to the hot function and is measured. Go also sets `f.pc =
     c.src[pc] + 1` while positions still come from the stack form's tables,
     as `regSlow` does.
   - **Inlined frames are simpler than the stack form's.** A call's window
     is the callee's frame: register A of the caller's call instruction is
     the callee's r0.
     - Materializing an inlined callee writes its registers at `wb + A`,
       writes the frame record as `regEnterW` does without running it, and
       runs runReg at the callee's pc.
     - The caller then resumes at the call's pc + 1, with the callee's
       result written to the call's B.
     - With nothing to rebuild on an operand stack, the frame state is
       just the live registers.
   - **Generators.** Their suspended windows (copied into the generator
     object) replace the operand-stack copies that `GenSteps` and the
     resumable entries handle now.
5. **OSR from runReg.** Section 2 covers the trigger. The entry needs the
   loop header's state (all live registers, read in place) and an
   `ssa.Func` entry at that register pc (`HasEntry`). The register form has
   one entry per loop header, as the stack form does.

**What carries over unchanged:**
- `ir` (the program, the evaluator oracle, `Validate`), apart from
  `Origin`;
- `ssa`: the whole builder (8,184 lines), its optimizations, escape
  analysis, ints, keep cells;
- `mir`, the encoders, `abi`, the arena, the code cache and its hints;
- the write barrier and `storeRef`, records and `recordsTail`, `callGo`;
- the profit model and the marking rule.

**What is rewritten:** `compile/lower.go`'s per-op lowering and its
analyses, which today read `fn.Code`:
- `selectGlobalSlots`, `selectPropertyLoops`, `selectNumericProperties`;
- `stringParts` and `stringConcats` (the register form has `concat` and
  `str_part` already);
- `typeTests`, `liveLocals`, the prologue and arguments handling;
- the VM side of exits, entries, inlined-frame materialization, `jitCalled`
  and the generator steps.

This is a port of roughly 3,000 lines of lowering plus the 90 feedback
methods. It is the largest and riskiest piece of the stage.

**Expected effect (inferred):**
- Steady-state code should be about level. SSA already removes the stack
  form's copies, and the shapes, targets and pools it compiles for are the
  same.
- Compiles get cheaper: 20 to 35% fewer IR instructions, and call targets
  known at the first compile, which avoids the recompile that learns them.
- The cost is the risk of losing the many stack-pc heuristics the JIT
  gathered over its rounds: forEach loops, pools, forwarders, leaf kinds.

#### 4. A baseline compiler

**What it would be.** Register code becomes machine code one instruction at
a time, from templates, with no IR, as V8's Sparkplug does.
- **runReg's frame.** The window is in `r.stack` and the frame record is
  `regEnterW`'s. The feedback vector is the same `closureCode` caches.
- **runReg's slow paths.** They are the functions runReg's cases call:
  `regSlow`, `regCallW`, `regElemMiss`, `regSetMiss`, `regAppend`,
  `regIterNext`, `regColdSub`.
- **No deoptimization metadata.** Every register is in the window at every
  instruction boundary. The baseline hands a frame to runReg at any pc, and
  runReg hands one back at any loop header, with nothing written.
- **Every instruction is covered from the first version.**
  - A hot op's template inlines its fast path.
  - Any other op is a call of a Go routine that runs that one instruction
    through its runReg helper. The routine returns the next pc, or the
    handler's pc from `regCatch`, and native code goes on through a
    pc-to-code table.

This has been weighed twice before, in other forms. Both were declined, and
the assessment starts from them.

##### What was declined, and how templates differ

**1. Closure-threaded dispatch** (docs/performance-plan.md, "Rejected").
On the stack interpreter, each op was a Go closure. On a pure arithmetic
loop, its best case, it was 1.09 to 1.17 times as fast as the switch, 3.7
against 4.2 ns an instruction. It was declined as not worth rewriting about
250 opcodes. The tree tier is closure compilation taken to its limit, and
the register interpreter now beats it ("The tree tier and the JIT").

A closure per op removes only the switch's jump-table load. It keeps the
rest of a transfer:
- an indirect call through the closure's function word;
- Go's prologue, its stack-bound check;
- the operands loaded from the closure's captured fields: offsets held as
  data, not encoded in the instruction;
- a return;
- the caller's loop;
- the spills Go makes around every call, having no callee-saved registers
  ("A Go call per node" in "The measure").

The 12% it gained is the switch's share alone. It is not what dispatch
costs.

A native template keeps none of these:
- execution falls through from one instruction's code to the next, with no
  branch at all;
- register A is a displacement, `[base + 16*A]`, and a constant is an
  immediate;
- each site has its own compare and branch, so prediction is per site,
  where runReg's `add` shares one number test across every `add`;
- nothing is spilled, because nothing is kept in machine registers between
  instructions except the window's base and the context.

**2. The optimizing JIT with its optimizer off, as a cheap first tier**
(docs/performance-plan.md, around line 2573, item 2). This is "the
baseline JIT we declined". All levels ran in one binary, switched by an
environment variable. It found:
- **Code size.** The code grew 15 to 30%.
- **Compile time** fell only 0 to 13%, and rose 9% on Typescript. The
  optimizer is what makes the backend's input small, and compile time
  follows emitted code (r = 0.96, about 120 bytes of machine code a
  bytecode instruction).
- **Speed, never re-tiered.** The code kept 59 to 85% of full speed: Octane
  +41%, Crypto and NavierStokes twice as slow.
- **Speed, re-tiered once hot.** It kept full speed (Octane +0.6%, AWFY
  +4.3%), but the first render took 80 to 85 ms against 63 to 66, because
  hot functions compiled twice.
- **Two open needs.** A re-tier trigger that natively called functions
  pass (`jitLearnedDue` runs only on Go's entries); and tier-aware forms of
  the thirteen tests that assert the optimizer's output.

That experiment answers whether a cheap level of the *same* pipeline pays,
and it does not. The pipeline's cost an instruction is fixed by work a
template compiler does not do. The same section of the plan measures about
5 µs a bytecode instruction:
- about 1 µs to build the SSA;
- 1.5 µs to optimize;
- 1 µs to emit;
- the rest in lowering, liveness, intervals, the scan and the calls' saves.

With the optimizer off, the build, liveness, allocation and selection still
run over a larger input. A template compiler makes one linear pass over the
register words: decode, switch on the op, append a fixed sequence through
the assembler, record jump fixups and a pc table entry. There is no IR, no
value numbering, no liveness, no intervals and no allocation.

**What a template compile would cost (inferred, not measured for
JavaScript here):**
- **Bytes an instruction.**
  - `move` is about 30 bytes: two loads, a flag test, two stores.
  - `add` is about 60 bytes: two tag tests, `addsd`, NaN canonicalization,
    and the store with its flag test.
  - `get_prop`'s hit is about 70 to 80 bytes: object tag, shape against
    `ic[C].ownShape`, slot load, store. Its miss is a shared stub of about
    20 bytes.
  - A compare-and-branch is about 40 bytes, and a call-out about 20.

  That averages about 40 to 70 bytes a *register* instruction. A register
  instruction stands for about 1.3 to 1.5 stack instructions (20 to 35%
  fewer, "Where stage 1 stands"), so this is about 30 to 50 bytes a stack
  instruction: **2.5 to 4 times less code than the SSA backend's 120**.
  The cost is that nothing stays in a machine register across
  instructions.
- **Time an instruction.** About 0.2 to 0.5 µs, from a few encoder calls
  of tens of ns each for 10 to 20 machine instructions, against 5 µs: **10
  to 25 times less**. In the SSA path, compile time tracks emitted code
  because each emitted instruction has an SSA value, an interval and an
  allocation behind it. A template's bytes have only the encoder behind
  them.
- **The in-repo precedent.** The WebAssembly engine's baseline (below)
  compiles every function at its first call. It holds a 3,028-function
  program's code in 23 MB, against 34 MB for its optimizing tier.

The first measurement any prototype must make is this per-instruction
cost, and with it the premise behind the whole tier.

##### What naive native code buys over the interpreter

**The best evidence is the declined experiment.** Its never-re-tiered
code, at 59 to 85% of full JIT speed, was naive only in its optimizations.
It still had SSA, registers across instructions, speculation from the
caches and native calls. Against the interpreters of the time:
- **Crypto.** The full JIT takes 63 ms against 301 for the interpreter
  (docs/benchmarks.md). Twice as slow is about 126 ms: **2.4 times as fast
  as the interpreter**.
- **Octane.** The full JIT is 0.39 of QuickJS's time against the
  interpreter's 0.90. +41% is about 0.55, so **1.6 times as fast as the
  interpreter**.

Templates are below that code. Values stay in the window as 16-byte
NaN-boxed words, and every store to a register tests the barrier flag. The
register interpreter is also faster than the stack interpreter and tree
tier those figures were against: 1.8x on moves and 1.35x on calls in the
kernels, level on `am3`.

**The share of runReg's time that is dispatch and decode.** No measurement
in the docs isolates it. No figure in them is "JIT minus unboxing and
inlining": every JIT figure includes both. What there is:

- **Kernel accounting** (this document).
  - `loop_locals` runs 6 instructions, mostly moves, in 10.4 to 11.6 ns:
    1.8 ns an instruction.
  - A move's own work is two 16-byte loads, two stores and a flag test,
    about 0.3 to 0.5 ns. That leaves about 1.3 ns, 5 to 6 cycles at about
    4.2 GHz, of fetch, mask, table jump, decode, jump back, the word saved
    at the dispatch, and the shared branch's mispredictions: **about 70%**.
  - `am3` runs 25 instructions in 91 ns, at 3.6 ns each, so the same 1.2
    to 1.5 ns is **33 to 41%**.
  - The comparison engine's switch takes 3.3 ns an instruction on `am3`,
    and the doc's "3 to 4 ns a dispatch in Go, against about 1 ns in V8's
    handlers" is the same picture.
- **The closure prototype.** It is a lower bound: removing only the table
  jump gave 12%.
- **Regular expressions' native code** (`2be0068b`). It is templates per
  matcher instruction, Irregexp's way, against a Go interpreter of the same
  program.
  - A native attempt was **3.6 times** as fast as the interpreter's;
  - a log parser over long text took 43% less time;
  - **but** V8's RegExp benchmark gained only 8%, and Octane's 2%, where
    the time per exec is outside the matcher.
- **The WebAssembly engine's baseline** (the WebAssembly branch's docs/wasm.md,
  "Measured", Ryzen 9 9950X3D2).
  - Against its closure-tree interpreter: `loop` 223 to 14.1 ms, `fib` 206
    to 11.3, sha256 1,921 to 65.4, json 884 to 35.8.
  - That is **15 to 30 times**, and on `loop` the baseline equals the
    optimizing tier.
  - This is an upper bound that does not carry over. Wasm's values are
    untyped words in machine registers, with no tag tests, no 16-byte
    values and no barrier on locals. JavaScript templates keep all three.

Taken together (inferred):

| Workload | Time in the loop (est.) | Dispatch and decode in it | Templates, time against runReg |
|---|---|---|---|
| Arithmetic and local loops (`am3`, `loop_locals`, NavierStokes' `lin_solve`) | 85-95% | 35-70% | 0.5-0.7x |
| Property and method code (Richards, DeltaBlue) | 60-75% | 20-30% | 0.8-0.95x with native calls; worse than 1.0x with calls through Go (below) |
| Allocation-heavy code (RayTrace, EarleyBoyer, Splay) | 40-60% | 20-30% | 0.85-0.95x |
| Built-in-bound code (RegExp, JSON, strings) | <15% | -- | about 1.0x |

**Calls of Go from native code are dear.**
- In runReg a slow path is a direct Go call, about 1 to 2 ns.
- From native code the plan measures **about 27 ns** for a call of Go to a
  leaf that does nothing (docs/performance-plan.md, around line 1169).
  About 9 ns of that is native code's own; about 10 is `callGo`,
  `goCall`, `ExpandGoCall` and the indirect call. The leaves' own
  trampoline was planned at 5 to 10.

So:
- **Megamorphic code is slower than in runReg.** Code that lives in its
  slow paths runs them at 5 to 25 ns where runReg pays 1 to 2: megamorphic
  sites, primitive receivers, a generic `+`.
- **Calls through Go lose.** A register call made through Go, native code
  then `regCallW`, `regEnterW` and `enterSSA`, adds two crossings. That is
  more than `call_fib`'s nine-instruction body saves (about 12 ns), so call
  code is slower than runReg.
- **So templates need native calls from their first version.** These are
  baseline-to-baseline calls by the native call protocol (`abi.Context`
  levels, `ReturnTo`), as the WebAssembly baseline makes its calls through
  its link table and return stack.

**What it would mean for the suites (inferred):**
- **As a tier alone, without the optimizer:** about 0.75 to 0.85 times
  runReg's time on the V8 suite.
- **Under the optimizing JIT,** it would serve only:
  - warm code below the optimizer's thresholds;
  - code the optimizer refuses or judges slow (`entrySlow`);
  - the time between a deopt and the next compile.

  That is about 2 to 10%, Octane at the top.
- **Not for the jitless measure.** A baseline is a JIT. It does nothing for
  the measure this project is judged on: the V8 composite without the JIT,
  1,901 against the comparison engine's and jitless Node's 2,342.

##### Code size and the op cache

- **The trade.** runReg's loop is one function, about the size of Zen 2's
  op cache's reach, shared by every script. Template code is per function
  and grows with the program's warm code, at 3 to 6 times the bytecode.
- **What it costs.** The JIT has already met this. Towers' and Box2D's
  times were bimodal from where their native code landed (memory note
  "JIT op-cache layout"):
  - Towers' four small functions came to about 61 KB of code over 12
    compiles;
  - it ran with 43% of its ops from the op cache at one offset and 81% at
    another, at IPC 2.84 against 3.48;
  - it took about 600 against 495 ms a run.
- **What follows.**
  - Template code for a large program's warm set, Typescript's or PdfJS's,
    will not fit, and will run from the legacy decoders. Its timing will
    move with placement as the optimizer's does.
  - Any measurement must vary `V8BENCH_JITPAD`, `JITSEED` and `JITLINE`.
  - The templates must put every slow path out of line, take constants
    RIP-relative and read the barrier flag's address from the context.
    These are the plan's own lessons from that bloat.
- **Budget.** The baseline's code competes with the optimizer's for the
  8 MiB budget, where Typescript already drops 107 functions for room.

##### The first render and background compiling

- **Where the first render stands.** It runs in 63 to 66 ms with the JIT
  against the interpreter's 22. Most of the difference is compiles on the
  runtime's goroutine: 32.6 ms, in D8's breakdown.
- **What sank the cheap tier.** Re-tiering from it added 15 to 20 ms more,
  because the cheap compile cost nearly what the full one did.
- **What templates change, if the 10 to 25x figure holds.** Compiling
  twice then costs 5 to 10% of one optimizing compile, not 90 to 100%.
  Templates also read nothing the runtime mutates, beyond the addresses of
  the caches, which do not move. So a template compile can run off the
  goroutine more easily than the SSA path's.
- **But D8 attacks the same window from the other side.** It is phases 1
  to 5 done, and planned to bring the first render to about the
  interpreter's 22 ms: runReg runs while the optimizer compiles elsewhere.
- **So the baseline's case is that window under D8.** It is the code that
  runs between "warm" and "optimized code installed". Its gain there is
  bounded by runReg's time over that window, which D8 already shortens.

##### What it reuses

**From internal/jit:**

| Piece | Reuse |
|---|---|
| `arena.go`, `memory_*`, `cache_*`, `policy_darwin.go`, `PlaceRaw`/`RawCode` | as is: W^X, icache, MAP_JIT, code another compiler made |
| `entry_*.s`: `enterSSA`/`enterRaw`, `callGo`, `storeRef` | as is |
| `abi.Context`, `abi.Encoding` | as is: `Locals`, `BackEdges` (the poll), `GoOp`, `GlobalEpoch`, `Keep`, object offsets and tags (`TestJITObjectLayout`) |
| `asm/amd64`, `asm/arm64`, `verify` | as is |
| code cache, hints, realms, flush, budget, D8's workers | as is, with an entry kind of its own |
| the write barrier and the marking rule | as is: a template's store tests the flag and goes through `storeRef` while marking. `TestJITSameWorkWhileMarking` covers it |
| exits, records, state maps | not needed |
| `ir`, `ssa`, `mir` | not used |

**From the WebAssembly engine's baseline.** It lives in `wasm/internal/jit`
on the WebAssembly branch (`78932ede`, `35a8ead9`); its README and
docs/wasm.md there were read.
- **What it shares with the JavaScript JIT:** only the lowest layer:
  - the encoders (`internal/jit/asm`, checked by `internal/jit/verify`);
  - the executable memory (`Arena`, `PlaceRaw`, `RawCode.EnterAt`);
  - the execution boundary's rules.

  Its front end, tiers, context, exits and entry points are its own.
- **What it offers here: a working design of exactly this kind.**
  - A one-pass compiler over the interpreter's own frames. Its
    platform-independent front end (`compile.go`, `stack.go`) serves
    per-machine backends (`compile_GOARCH.go`, `stack_GOARCH.go`,
    `ops_GOARCH.go`).
  - OSR both ways at a loop's head with no translation.
  - A register cache of the operand stack, and homes for hot locals.
  - Native calls through a link table and a return stack, with no Go
    between, and tier-up by counting calls at the code's entry.
  - Trap and slow-path stubs emitted after the body; a pooled compile
    workspace; a JIT memory limit that drops code no frame is in.
  - A reference-slot barrier routine; background compiles for its
    optimizing tier.
- **What does not carry over:** its numeric and memory templates, since a
  wasm value has no tag. Its front end is over the operand stack. A
  JavaScript baseline would take its structure, frames and calls, not its
  code.

##### Go's runtime

The baseline adds no new kind of interaction (README, "The execution
boundary").
- **Async preemption.** Native code is never a safe point. Loops poll
  through `Context.BackEdges`, straight code is bounded by its length, and
  every call of Go is a cooperative preemption point.
- **Stack maps.** Native code holds no Go pointer that only it holds:
  - every `Value` stays in the window;
  - `r.stack` never moves;
  - the cache bases are reloaded after each call of Go.
- **Stack growth.** No address into the goroutine stack is held.
- **Faults kill the process.** The tier is opt-in, as the optimizer is.

##### Verdict on option 4

**The evidence does not show that the tier pays, and it should not be
built as a general compiler now.**

- **Speed.** Naive native code with SSA and registers kept 59 to 85% of the
  optimizer's speed. Templates would keep less. Their gain over runReg is
  large only on dispatch-bound loops, about 1.5 to 2 times, and those are
  what the optimizer takes anyway. Elsewhere it is 5 to 15%.
- **Calls of Go.** At 5 to 25 ns against runReg's 1 to 2, they make
  slow-path and call code slower unless native calls come first.
- **Compile cost.** The premise that templates compile 10 to 25 times more
  cheaply than the SSA path is an inference. Nothing in this repository
  has measured it for JavaScript, and the declined experiment shows how a
  "cheap tier" can turn out not to be cheap.
- **Op cache and budget.** Its code is the kind of large warm code whose
  placement made Towers and Box2D bimodal, and it competes for a budget
  Typescript already runs out of.
- **Overlap with D8.** D8 shrinks the window it would serve.
- **The jitless measure.** It does nothing for it.

**The kill gate**, if it is ever tried, comes before any general compiler:
1. A hand-made template version of two kernels, made with the assembler,
   placed as `RawCode` and entered from `regBackEdge` at the loop's header
   on runReg's frame:
   - `crypto_am3`'s inner loop;
   - `loop_locals`.
2. A hand-made `call_fib` with native baseline-to-baseline calls.
3. A prototype emitter for ten ops, timed per instruction over the V8
   suite's functions.

Continue only if all of these hold, at every placement with the JIT pads
varied:
- `am3` at most 0.6 times runReg's time, and `loop_locals` at most 0.5;
- `call_fib` faster than runReg;
- a template compile at most 0.5 µs a register instruction, and at most 70
  bytes;
- the baseline code never, or almost never, deopts or falls back to the
  interpreter. It speculates nothing, so there is nothing to deopt: a case
  its template does not handle calls the same helper runReg calls and goes
  on in native code, as V8's Sparkplug does. It leaves for runReg only when
  the optimizing JIT asks it to. The prototype counts its exits to runReg
  per thousand instructions over the V8 suite, Octane and the bundles, and
  that count must be near zero. This makes native-to-Go calls the cost to
  get right first: about 27 ns each today, against 1-2 ns for runReg's
  direct calls.

Stop at the first that fails.

#### 5. A hand-written assembly interpreter loop

**What it would buy over the Go switch (inferred):**
- **Pinned registers.** pc, the window base, the code base, the constants
  and the closure stay in registers across handlers. That removes the
  spills and reloads Go makes because it has no callee-saved registers,
  and the instruction word saved at the dispatch.
- **Threaded dispatch.** Each handler ends with its own fetch and jump.
  That removes the jump back and gives each handler its own predictor
  history. On a modern predictor a shared switch jump is already predicted
  well (Rohou et al., 2015), and the closure prototype's 12% is about what
  the dispatch jump alone is worth.
- **Together,** about 0.5 to 1.0 ns of the 3.6 ns an instruction on
  arithmetic loops, 15 to 25% there. On the suites, 5 to 10%.
- **Its one real advantage:** it is no JIT, so it counts toward the
  jitless measure.

**What it costs:**
- **Every call into Go.** ABI0 through the ABI wrapper, with the pinned
  registers spilled and reloaded. Every slow path and every call is dearer
  than runReg's direct calls with Go's register ABI.
- **Pointer maps.**
  - An asm frame's locals are not scanned (`NO_LOCAL_POINTERS`).
  - Spilled pointers must be ones the collector reaches otherwise (the
    window, the code, `r.cur`'s frames).
  - `checkptr` and `gccheckmark` see nothing of the loop.
  - Write barriers are written by hand, or made through a Go helper.
- **Async preemption.** The assembler marks assembly functions unsafe for
  async preemption, so the loop must poll at back edges and calls, as
  native code does.
- **The stack.** runReg recurses once a JavaScript call. The asm loop
  needs a real frame with a stack check, and must hold no address into its
  frame across a call of Go.
- **Three copies of every hot handler:** amd64, arm64, and runReg as the
  pure-Go fallback that AGENTS.md's portability rule requires. Every
  semantic fix lands three times. arm64 is tested only on darwin/arm64 CI.
- **Default-on risk.** It would be the default interpreter: an asm bug
  faults every embedder, not only those who opted into the JIT.
- **Layout.** The loop moves outside the layout lock's model of Go
  functions and closures.

**Against option 4.**
- **What it keeps.** The fetch, the indirect jump and the decode stay on
  every instruction, and each case's branches stay shared across sites.
- **What it reaches.** It reaches every runtime, not only JIT ones.
- **This document's evidence against it.** The remaining jitless gap is in
  calls and frames, not dispatch:
  - "The difference between the engines is not the count: it is what each
    instruction and each call costs";
  - `call_fib` takes 44 against 29 ns with the same nine instructions;
  - a pure-Go switch reaches 3.3 ns an instruction where runReg is at 3.6.
- **Verdict.** It is not worth three copies of the hottest code now.

#### 6. Recommendation and plan

**Recommendation: the front-end port (3a), and neither option 4 nor option
5 as a commitment.**

- **3a is required.** The stack form goes. It brings sections 1 to 3: the
  feedback vector, recorded off runReg's fast paths; tier-up from
  `regBackEdge` and `regEnterW` with nothing added to the loop; and the
  optimizing JIT's lowering from register code, with exits and OSR into
  register frames.
- **Option 4, the baseline: not now.** The evidence above does not show it
  paying in a JIT build that has the optimizer and D8.
  - It may be reopened only through the kill gate in section 4, after 3a
    and D8.
  - It must be measured against runReg plus the optimizer compiling in the
    background, not against runReg alone.
  - The WebAssembly baseline is the design to follow if it is reopened.
- **Option 5, the assembly loop: not now.** It may be reopened only if the
  jitless composite stalls after Stage 2 and the tree tier's removal, and a
  profile of the register tier then shows dispatch above 40% of the V8
  suite's time. Even then it is an amd64-only hybrid for about 25 hot ops
  that falls back to runReg at the same pc, behind a build tag first.
  - **Its kill gate:** an amd64 prototype of `crypto_am3`'s and
    `loop_locals`' ops, at least 20% faster than runReg on them at every
    placement, before anything else of it is written.

**Stages of 3a and their gates.** Every gate includes:
- test262 at 100,247 passed, none failed, in every mode: plain,
  `-conformance.jit`, `-conformance.debugger`, register mode;
- `TestJITSameWorkWhileMarking` with its breach list unchanged, and every
  native path also tested under `jitMarkingForTest`;
- the full suite with and without the tag;
- `TestRegLoopCallsNoHelper` and `TestRegDispatchKeepsNoPC` passing;
- untagged builds unchanged by `hotdiff`.

1. **3a-1, feedback.**
   - Work: `regSeen`, `regCallCache.count`, the polymorphic call bit,
     `rc.hot`, all out of runReg.
   - Gate: `placements compare -parallel -k 4 -micro`, untagged and tagged
     with the JIT off, with no test 3% slower at every placement; the V8
     suite level at `-k 8`. If `rc.hot` shows, use the fallback of section
     2, sampling at `interruptCounter`'s expiry.
2. **3a-2, the register lowering.**
   - Work: `LowerReg` into `ir.Program` with `Origin` and the atomicity
     rule for fused operations, checked by `ir.Validate`; `ir.Evaluate`
     against runReg on the corpus tests; the feedback methods re-keyed;
     `compilediff native` reporting register pcs.
   - Gate: refusals no more than the stack lowering's on the V8 suite,
     Octane and the bundles.
3. **3a-3, exits, OSR and tier-up from runReg.**
   - Work: `runReg(f, c, pc)`, the `regCatch` sentinel, the appended `ret`,
     inlined frames at `wb + A`, generators.
   - Gate:
     - the JIT's placements (`-jit -k 4`, with JITPAD, JITSEED and JITLINE
       varied) on V8, Octane, AWFY, `ssr`, `fresh` and the micro-benchmarks
       against the stack-form JIT, both foreground and with
       `-jitbackground`;
     - totals level or better, no benchmark slower at every placement, and
       `-jitcompile` phase times no worse;
     - `QJS_JIT_STRESS` clean;
     - the JIT's README updated in the same commit.
   - Only then is the stack form's JIT path removed.

**Biggest risks.**
- **The port of the optimizer's front end.** About 3,000 lines of lowering
  and 90 feedback methods hold heuristics gathered against stack pcs. A
  suite that loses one regresses quietly, which is why the stack path
  stays until 3a-3's gate passes.
- **Mid-instruction exits in fused register operations.** The atomicity
  rule must be enforced by the validator, not left to care.
- **Feedback earlier than before.** Call targets are known from
  `regCalls` at the first compile, where the stack-form JIT learned them
  at exits. More is inlined earlier, which moves compile cost and code size
  in the first render.
- **The op cache, for any native tier.** No timing is believed without the
  JIT's placement knobs.

#### 3a-3's working plan (2026-11-12)

**Where 3a-2 left it.** On reg-cov, behind tests only:
- `compile.LowerReg` lowers every register operation into the slot IR,
  with `ir.Program.Origin` and the atomicity rule, which `ir.Validate`
  enforces.
- `ssa.Build` takes register programs and keeps the rule as it builds
  (`atomicInstruction`), and mir compiles them. Every program of the V8
  suite, Octane and the bundles builds and compiles, both forms alike.
- `jitRegFeedback` answers by register pc from the caches runReg fills:
  property sites, global sites, call shapes and targets (`regCalls`),
  constants.
- `vm.RegLowerCall`, with `regRunner`, is an exit's VM side today. The
  evaluator stands where native code will: a host exit runs the whole
  register instruction on the frame's real registers by the
  interpreter's slow paths, and a deoptimization resumes `runReg(f, c,
  pc)` at the exit's register pc. `runReg` already takes a start pc,
  since generators use it, so no edit of it is needed for entry at an
  arbitrary pc.

**Step A: built entirely in `zzjit_*` files, no register-tier edit.**
- **A1, an exit's frame rebuilt as a register frame** (`jitRegExit`).
  - An exit's records write the window's registers `[wb, wb+Regs)`, by
    `Program.Live`, and materialize escape analysis's virtual objects
    into registers.
  - It sets `f.pc = c.src[pc] + 1`, as `regSlow` does.
  - A host exit then runs the register instruction whole by
    `regRunner`'s dispatch (`regSlow`, `regCallW`, `regCold` and the
    rest, all called out of the loop as they are today), and native code
    goes on at the next entry.
  - A deoptimization runs `runReg(f, c, pc)`.
  - The context's `Locals` is the window's base: `r.stack` never moves,
    so native code reads and writes registers in place.
- **A2, inlined frames.** An inlined callee is materialized as a
  register frame at `wb + A` of the caller's call instruction:
  - its frame is pushed as `regLowerEnter` pushes one, without running
    it, and `runReg` runs it from its exit's pc;
  - the caller then resumes at the call's pc + 1, with the value in B.
- **A3, the OSR entry map.** Each loop header's register pc goes to an
  `ssa.Func` entry at `p.At(pc)`, its live registers from
  `Program.Live`: the register form has one entry per loop header, as
  the stack form.
- **A4, the feedback snapshot for background compiles.**
  `jitRegFeedback` is asked ahead, as `jitFeedback` is
  (`zzjit_snapshot.go`): snapgen extended to the second type, its
  generated file regenerated. Until then, register code compiles in the
  foreground only.
- **A5, calls between register functions.**
  - `NativeCalls` and `Inline` come from `regCalls` targets, with the
    callees' programs from `LowerRegInline`.
  - A native callee's link is its register entry.
  - Frequency waits for 3a-1's call counts; until then a call is
    weighted by the caller's own hotness.
- **A6, the compile pipeline** (`jitRegFor`): LowerReg, then ssa.Build
  with jitRegFeedback, Optimize, mir and SSACode, in the JIT's existing
  code cache, keyed by function and form.
- **Testing step A.**
  - A test-only native entry, `RegLowerCall` with native code in the
    evaluator's place, runs the regdiff corpus with every function
    compiled at its first call. It is compared with ordinary calls, as
    TestRegLowerEvaluate does now.
  - TestJITSameWorkWhileMarking gets register-code cases, and every
    native path is tested under `jitMarkingForTest`.

**Step B: the smallest hook set in the register tier**, which the
coordinator sequences after a merge of reg-cov into reg. No hook is in
runReg's loop, and runReg itself is not edited.
- **B1, calls** (`zzreg_call.go`, outside the loop): `regEnterW` and
  `regDirect`/`regDirectOnce`, the callbacks' entry.
  - One branch behind `jitBuilt`: `if jitBuilt { if rc.hot--; rc.hot < 0
    { return r.regEnterHot(...) } }`.
  - `regEnterHot` (zzjit, out of line) counts, compiles and enters
    native code, and holds `hot` negative once the function has code.
  - `regCode` gains `hot int32` (`zzreg_run.go`, a field, not the loop).
  - Untagged builds compile it out; tagged builds pay a decrement on a
    line regEnterW already loads. That is measured with
    `placements -micro`, tagged and untagged. If it shows, the fallback
    credits the callee when `interruptCounter` runs out, the decrement
    regEnterW already makes.
- **B2, loops** (`regBackEdge` in `zzreg_run.go`): it is called out of
  line from the loop's jump tail when `r.backEdges` runs out, and the
  tail does not change.
  - Behind `jitBuilt`, it takes the budget from
    `r.jit.backEdgeBudget()` and calls `r.jitRegLoop(f, c, k)` (zzjit).
  - That function finds the header from the jump at `k.pc - 1`, enters
    native code at A3's entry, and on its return sets `k.pc` and returns
    the sentinel `errRegGoOn`.
- **B3, going on after native code** (`regCatch` in `zzreg_switch.go`,
  outside the loop): its first test is `if err == errRegGoOn { return
  k.pc }`. The fail tail already continues at whatever regCatch returns,
  so OSR out of native code adds no instruction to runReg.
  - A frame native code finished resumes at a `ret r0` that regcomp
    appends to every function (a regcomp change, not the tier's), its
    value written to r0.
- **No hook needed** in `regSlow`, `regColdSub` or `regCallW`: an exit
  calls them from zzjit as `regRunner` does now.
  - 3a-1's feedback, `regSeen` and call counts, records in their slow
    branches: that is 3a-1's hook set, apart from 3a-3's.
- **Generators** are a step after B: `regGenRun` (`zzreg_zgen.go`)
  entering a resumption's native code, the suspended window replacing
  the stack form's `GenSteps`.

**Order and gates.**
1. A6, A1 and A3 with the test-only entry.
   - Gate: the regdiff corpus native at first call, compared with
     ordinary calls, 0 disagreements; the marking test's register cases.
2. A5 and A2, with the same gates; inlined frames' rebuild under
   `QJS_JIT_STRESS`'s deopt settings through the test entry.
3. B1 to B3, scheduled by the coordinator, and an env switch
   (`QJS_JITREG`) routing the JIT to register code.
   - Gate: test262 `-conformance.jit` in register mode, 0 failures and
     0 fallbacks; `QJS_JIT_STRESS` clean; hotdiff of runReg the same,
     and of `regEnterW` in untagged builds.
4. A4, then 3a-3's timing gate: the JIT's placements (`-jit -k 4`,
   JITPAD, JITSEED and JITLINE varied) on V8, Octane, AWFY, `ssr`,
   `fresh` and the micro-benchmarks against the stack-form JIT,
   foreground and with `-jitbackground`. Only then does the stack path
   go.

### Stage 4: one pipeline

The stack compiler stops emitting code; scope resolution stays as the
front end both backends shared. `executeAt` and the stack form's
instruction set go.
