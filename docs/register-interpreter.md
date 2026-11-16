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
  20 or less into it; past that, `IsNumber` and its kind become calls in
  every case. A test pins it, as `TestIntendedInlining` pins the helpers.
- **No constants in registers.** What Go's loop-invariant pass would take
  out of the loop is read from a variable (above).

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
function with handlers has a table of them, each a range of pcs, the
handler's pc and the register its completion goes to. When a case returns an
error, the loop looks the pc up in the table: a handler in this frame
resumes there; none returns the error to the caller's case, which does the
same. `finally` is a handler that runs with the completion -- normal,
return, break, throw -- in two registers and dispatches on it at its end,
as the stack form's does. Iterators a loop holds open are closed by the
unwinding as now.

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
on the old pipeline.

### Stage 0: the evidence (done)

The translation reference (`6ae14dca`) and the hand-written kernels
(`cff0c8ab`) above: the operations are specialised, the loop is its own,
the register window and the code are unchecked, the switch has no range
check, and nothing a case keeps is live across a call.

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
- **Strict global stores and lazy arguments.** A strict store of a value
  that runs no code asks at the store whether the name resolves
  (`set_global` with X=1), as the stack form's folded `set_global_strict`
  does; `arguments.length` and `arguments[i]` of a function whose object
  is made on demand are `args_len` and `args_index`. The V8 suite has no
  refusal left that costs a crossing.
- **x[i++] as one operation measured slower** (`crypto_am3` 91 to 101 ns at
  two placements, 4 ns of it the fusion itself) and was dropped. The
  loop's own alignment moves the small loops: `loop_locals` is 10.4 to 11.3
  ns with `runReg` at 32 mod 64 and 13.1 at 0, which the placements do not
  vary.

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

### Stage 2: frames

The lean frame record everywhere a frame is read: stack traces, the
debugger's frames, errors' positions, generators. Exceptions with handler
tables, `try`, `catch` and `finally`; `arguments` and rest parameters;
destructuring; classes and `super`; `for-in` and `for-of`; templates;
spreads; optional chains; tail calls. Built-ins' callbacks all enter
directly.

### Stage 3: generators, async, eval, with, the debugger

Generators and async functions as copied windows, with the settled-await
fast path; direct eval through binding descriptors; `with`; the debugger's
statement markers and binding tables. At its end every function has
register code and the stack interpreter runs nothing.

### Stage 4: the JIT and the tree tier

The JIT's front end reads register code and its deoptimization rebuilds
register frames. The tree tier is measured against the register interpreter
suite by suite and retired where it no longer wins.

### Stage 5: one pipeline

The stack compiler stops emitting code; scope resolution stays as the
front end both backends shared. `executeAt`, the translation reference and
the stack form's instruction set go.
