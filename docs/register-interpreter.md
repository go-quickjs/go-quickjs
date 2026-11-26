# The register interpreter

go-quickjs runs JavaScript as register code: each function is compiled from
its syntax tree to instructions that name registers as their operands, as
V8's Ignition and Lua's VM do, and run by an interpreter whose handlers are
machine code generated for amd64 and arm64, and Go functions elsewhere. The
same instructions are what the JIT compiles, on the same frames. This
document is the tier's design: how it works, why it was built this way, and
the evidence for each choice where it was measured.

The interpreter replaced two tiers, a stack-bytecode interpreter after
QuickJS's and a tier that ran most functions as trees of Go closures built
from that bytecode, which [Why a register machine](#why-a-register-machine)
measures it against. Nothing runs them now.

## Contents

- [Where it runs](#where-it-runs)
- [Why a register machine](#why-a-register-machine)
- [Instructions](#instructions)
- [The compiler](#the-compiler)
- [The loop](#the-loop)
- [Native handlers](#native-handlers)
- [Frames and calls](#frames-and-calls)
- [Exceptions](#exceptions)
- [Iteration](#iteration)
- [Generators and async functions](#generators-and-async-functions)
- [Closures and upvalues](#closures-and-upvalues)
- [Inline caches and feedback](#inline-caches-and-feedback)
- [Loop-kernel instructions](#loop-kernel-instructions)
- [Typed code](#typed-code)
- [Tiering to the JIT](#tiering-to-the-jit)
- [The debugger, eval and with](#the-debugger-eval-and-with)
- [Code layout](#code-layout)
- [Correctness](#correctness)
- [Tools and switches](#tools-and-switches)
- [Known limits and open work](#known-limits-and-open-work)

## Where it runs

Every function of every runtime is compiled to register code and run by
the register interpreter; there is no other tier to fall back to, and a
function with no register code is an internal error.

- **amd64 and arm64.** The native handlers run it
  (`internal/vm/native_*`).
- **Other architectures.** The Go handler table runs it
  (`reg_table*.go`), the portable form of the same interpreter, slower
  than the native handlers; an architecture that needs the speed gets
  native handlers of its own.
- **A runtime with the JIT.** The same, and the JIT compiles the hot
  functions' register code (internal/jit/README.md).

`QJS_NONATIVE=1` runs register code on the Go table on amd64 and arm64
too, to compare the two and as an escape hatch. [Tools and
switches](#tools-and-switches) lists the rest.

Register code is the one form a function is compiled to: a walk of the
AST resolves its names and records them, and the register backend compiles
the AST with that record (see [The compiler](#the-compiler)). The
evaluators of leaf functions (`pureCall`, `leafCall`) read register code
too, and `Program` keeps one compiled form for standard mode and one for
`WithNodeQuirks`.

## Why a register machine

### What the old tiers cost

The interpreter's core operations ran about twice as slow as another pure-Go
register interpreter measured as a reference: calls (`call_fib` 2.09x,
`call_mutual` 2.38x), local moves (`loop_locals` 1.85x), stores
(`prop_write` 1.75x, `array_write` 1.95x), globals (1.66 to 1.85x), and
callbacks from built-ins (`array_reduce_upvalue` 1.74x). The runtime around
the core -- JSON, BigInt, number formatting, regular expressions, generators
-- was already far faster than the reference's, so a core that matched it
would make the engine faster overall. Profiled test by test, the costs were:

- **Operands not read in place.** In the tree tier `a = b` is two closures,
  a store calling a tree that reads the local: about 47 instructions and two
  indirect calls where a register move is about 25.
- **A Go call per node.** Go has no callee-saved registers, so each node a
  block calls reloads what it had live: about 14 instructions a statement,
  5 of them reloads, and 8 more for the callee's prologue and epilogue.
- **Stores that do more than store.** `prop_write` loaded a `this` it did
  not use and round-tripped a float through memory to test a tag;
  `array_write` made three Go calls a store and converted a constant key
  each time.
- **Globals.** Two Go calls, about eight dependent loads and three bounds
  checks a read.
- **Calls.** Nine Go calls, seven of them indirect, for each JavaScript call
  of `call_mutual`, and about 41 ns of machinery: six barrier-checked
  pointer stores into a heap frame, the arguments copied, seven checks that
  reset what the frame's last user left.
- **Callbacks from built-ins.** A chain five calls deep from `reduce` to the
  callback's tree, with a `defer` and a `recover` for each call.

A stack interpreter adds to these its operand traffic: every operand is
pushed and popped through memory, and every expression is several
dispatches. In Go a dispatch costs 3 to 4 ns -- a load of the handler, an
indirect call, and the state the caller had live stored and reloaded around
it -- against about 1 ns in V8's generated handlers, so the dispatch count
dominates what an interpreter costs.

### What the experiments showed

**Fewer dispatches alone is not the measure.** Register operands added to
the stack interpreter as one generic `bin_r` whose operands were each a
local or a stack slot, chosen by their sign, removed 35% of the dispatches
in Crypto's `am3` and made Crypto 19% slower; widening the stack
instruction for the extra operands cost 3% everywhere by itself. An
instruction that decides at run time what its operands are, or which
operator it is, has a second dispatch inside it. So every operand here is
of a kind fixed by the operation, and every hot operator is an operation of
its own.

**A translation is only as good as the stack form.** Register code
translated from the stack code, over a symbolic stack whose temporaries
were homed by depth, worked, but inherited the stack form's granularity,
its slot layout, its 16-bit sites and its generic operators. So the
register code is compiled from the AST by a backend of its own.

**The kernels, by hand.** The instruction set was first written by hand for
`am3`'s inner loop, `call_fib` and `loop_locals`, and timed against the
other forms at two placements (best of 5, ns an operation; the prototype
and the translation were removed once the backend covered them):

| Test | Stack interpreter | Tree tier | Translated | Hand-written | Reference |
|---|---:|---:|---:|---:|---:|
| `crypto_am3` | 208.6 / 216.1 | 103.8 / 104.7 | 105.6 / 107.0 | 101.6 / 101.6 | 81.6 |
| `loop_locals` | 32.8 / 32.9 | 20.9 / 20.9 | 12.3 / 12.7 | 11.6 / 11.6 | 11.3 |
| `call_fib` | 60.4 / 60.1 | 59.4 / 59.9 | 51.5 / 52.0 | 44.1 / 44.3 | 28.6 |
| `empty_loop` | 14.9 / 14.1 | 5.3 / 5.0 | 5.6 / 5.6 | 5.6 / 5.6 | 5.0 |

`am3` runs 25 register instructions a turn where the stack form runs 34,
twice as fast as the stack interpreter and level with the tree tier, whose
int32 nodes were built for that loop; the reference runs the same 25,
nearly one for one. What a turn executes, in each form:

| Test | Stack form | Register code | Reference |
|---|---:|---:|---:|
| `empty_loop` | 4 | 2 | 2 |
| `loop_locals` | 9 | 6 | 6 |
| `call_fib` (loop) | 12 | 7 | 7 |
| `call_fib_rec` (recursive / base) | 11 / 6 | 9 / 2 | 9 / 2 |
| `call_mutual` (each call) | 8 | 6 | 6 |
| `prop_write` (4 stores) | 12 | 6 | 6 |
| `array_write` (10 stores) | 44 | 12 | 12 |
| `global_read` (4 reads) | 20 | 10 | 10 |
| `method_call` (loop) | 10 | 6 | 7 |
| Crypto `am3` | 34 | 25 | 25 |

With the counts equal, what is left between two register interpreters is
what each instruction and each call costs, which is what the rest of this
design spends its effort on: native handlers, and calls that make no more
of a frame than a reader needs.

**What it gives the JIT.** The JIT compiles the instructions the
interpreter runs, on the interpreter's frames:

- One input. Register code is already three-address code, a register a
  variable, which maps onto the JIT's slot IR with no operand stack: 20 to
  35% fewer instructions than the stack form in the V8 suite's methods,
  and a liveness over registers that is more precise than one over an
  operand stack, so that exits write fewer records.
- One set of feedback. The interpreter's caches are keyed by register pc
  and so is what the JIT asks; a call site's cache names its callee from
  the first compile.
- One frame. Native code runs on the register window in place, since the
  value stack never moves: entering it at a call or at a loop's header
  (on-stack replacement) converts nothing, and a deoptimization writes the
  live registers and resumes the interpreter at a register pc. An inlined
  callee's frame is the window at the caller's call instruction's A, as the
  interpreter's call makes it; with no operand stack to rebuild, a frame
  state is just the live registers.

### What it measured as the default

At `a151c7c9`, against main's old tiers at `440db6bd`, over four
placements: the micro-benchmarks' geometric mean was 0.82 with the native
handlers (44 tests still slower at every placement) and 0.995 on the Go
table (99 slower), and the V8 suite, Octane and Are We Fast Yet level or
faster. The native handlers made the tier pay, and the Go table alone did
not.

## Instructions

### Format

An instruction is one 64-bit word with fixed fields (`bytecode.RInstr`):

```
bits  0- 7  Op  the operation
bits  8-15  X   a small operand: an argument count, a comparison, a flag
bits 16-31  A   a register, most often the destination
bits 32-47  B   a register, an index, a 16-bit immediate (SB)
bits 48-63  C   a register, an index, a 16-bit immediate or offset (SC)
```

B and C read together are BC, a 32-bit index or a signed jump offset. Every
operation decodes each field it uses with one shift and one zero or sign
extension, the same for every operation: no operand is of a kind decided at
run time.

A 16-bit register costs nothing an 8-bit one does not: `regs[uint16(w>>16)]`
is a shift and a zero extension, as `regs[uint8(w>>8)]` is, and indexing a
pointer to an array of 65,536 values with a `uint16` needs no bounds check.
What it costs is width -- eight bytes an instruction where a 32-bit design
has four -- which buys a common case as fast as an 8-bit design's and makes
the cases past 16 bits rare.

### Nothing is capped

No limit of the format is a limit of the language: a function past one is
compiled differently, never refused.

- **Registers past 65,535.** A function whose bindings do not fit has the
  rest as far registers, past every near one, reached only by `mov_far`
  (far register BC to register A), `mov_to_far` and `far_init`; an
  operation on a far binding is a move in, the operation, a move out. The
  allocator gives near registers to parameters, temporaries and the
  bindings used most (`nearCap`, `farSlots`), compiling the function again
  with fewer bindings near where the first try runs out (`errRegs`). Such
  functions are generated code, run once.
- **Jumps past 32,767 instructions.** A compare-and-branch's offset is 16
  bits. A function where one does not reach is compiled again with that
  jump far (`farJump`, by its order among the function's jumps of 16 bits),
  as an assembler relaxes its branches, until every jump reaches: a far
  branch jumps over a `jmp` to a `jmp`, whose offset is 32 bits, and the
  others stay one instruction. Typed code's branches are made far the same
  way (`CompileTyped`), where a function with one that did not reach had
  had no typed code.
- **Sites past 65,535.** A function with more property, global or literal
  sites than 16 bits number is compiled again with its sites shared by key
  (`site16`, `siteKey`): two instructions of one kind and one name share an
  entry, which serves both as a polymorphic site does.
- **Names past 65,535.** Only cold operations and miss paths read a name,
  so a name past 16 bits is the operand `RNameFar`, and `RegCode.FarNames`,
  by the instruction's pc, holds its index.
- **Constants, closures, global reads** take a 32-bit BC and need nothing.
- **Calls of more than 255 arguments** are compiled as spread calls: the
  arguments made into an array, then `call_spread` (`wideArgs`). So are
  array literals past one instruction's elements.

Ignition makes operands 8 bits by default, with `Wide` and `ExtraWide`
prefixes for 16 and 32, and generates each handler three times. In Go a
prefix is a second dispatch, and three copies of every handler triple the
code to lay out; a wider word keeps one handler an operation.

### No accumulator

Ignition keeps one operand of most operations in an accumulator, a machine
register its generated handlers keep across the dispatch, which makes its
bytecode small. In Go it would be a `Value`, two words, carried around a
loop in which every handler that calls anything clobbers every register: Go
has no callee-saved registers. It would add two stores and two loads to
every instruction, and two or three dispatches to every expression --
`l = xl * l` is `Ldar xl; Mul l; Star l` where three-address code has one
`mul`. The set is three-address: an operation reads its operands from
registers or immediates and writes its result to a register.

### The operations

Each hot operation is an opcode of its own, as Ignition's `AddSmi` and
`BitwiseOrSmi` are, so that no hot handler switches on an operator held in
the instruction. The families:

- **Moves and loads:** `move`, `load_int` (a 32-bit immediate),
  `load_const`, `load_undef`, `load_null`, `load_true`, `load_false`,
  `load_empty`, `load_uninit`, `load_this`.
- **Arithmetic:** `add`, `sub`, `mul` of two registers; `add_imm`,
  `sub_imm`, `mul_imm` with a 16-bit immediate; `inc`, `dec`, `post_inc`,
  `post_dec`, `unary`. `bin` names its operator for the cold ones (`/`,
  `%`, `**`). Each has a number fast path and calls out for anything else.
- **int32 results:** `add_i32`, `sub_i32`, `add_imm_i32`: `(x ± y) | 0` in
  one instruction ([Loop-kernel instructions](#loop-kernel-instructions)).
- **Bitwise:** `bit_and`, `bit_or`, `bit_xor`, `shl`, `shr`, `ushr`, and
  each with an immediate (`and_imm` ...), the masks and shifts of Crypto
  and of asm.js code.
- **Compare-and-branch:** `jlt`, `jle`, `jgt`, `jge` and their negations
  `jnlt` ... -- different where a NaN is compared -- with a register, an
  immediate (`_imm`) or an upvalue (`_upval`, a loop bound a closure
  captures) on the right; `jseq`, `jsne`, `jeq`, `jne`, `jnull`,
  `jnotnull`, `jtypeof` (a `typeof` compared with a literal). A comparison
  that decides a jump never makes a boolean. `jnull` and `jnotnull` with
  `RNullStrict` in X compare with null alone (`=== null`, `!== null`),
  avoiding the temporary value and load of a two-register comparison.
  `jbool` compares directly with a boolean literal, X's low bit, under
  strict equality; `RCmpTrue` says to branch on equality rather than
  inequality. Other types never equal the literal and need no conversion.
- **Step-and-branch:** `step_jlt` and its kin, a counter's step and the
  compare-and-branch after it in one dispatch.
- **Jumps:** `jmp`, `jmp_true`, `jmp_false` (ToBoolean, a boolean's without
  a call).
- **Bindings:** `get_upval`, `set_upval`, their checked forms for the dead
  zone, `check_ref`, `close_regs`.
- **Globals:** `get_global`, `set_global` (a strict store is X=1).
- **Properties and elements:** `get_prop`, `set_prop`, `get_method` and
  `get_method_w` (the method and its receiver written into a call's window
  in one instruction), `get_index`, `set_index` and their immediate forms,
  `get_index_pre` and `get_index_post` (`x[++i]`, `x[i++]`), `get_length`,
  `add_prop`, `add_index` and their forms (`o.n += v`, `o[k]++`),
  `get_private`, `set_private`, `in`, `instanceof`, `delete`.
- **Literals:** `new_object`, `new_object_lit` (an object of plain keys
  made at once from its values), `new_array`, `new_array_k` (a literal of
  constants), `new_array_empty`, `closure_w`, `regexp_lit`.
- **Strings:** `concat` and `str_part` (a template's or a chain's parts,
  one string made where each addition would make one), `append` and
  `str_freeze` (a local's appends grown in place), `char_code_at`.
- **Calls:** `call_w`, `call_method_w`, `new_w`, `tail_call_w`,
  `tail_call_method_w`, `ret`, `ret_undef`, `call_spread`, `apply_args`,
  `args_len`, `args_index`.
- **Control:** `switch` (a table of a switch's literal cases),
  `switch_typeof`, `throw`, `iter_next`, `yield`.
- **Cold operations** (`ROp.Cold`, 106 of them): declarations, classes,
  private members' installation, destructuring, super, iteration's starts
  and closes, generators' and async functions' entries and suspensions,
  `with`, direct eval, using declarations, the debugger's `debug`, the far
  moves.

### Opcode values

`bytecode.ROp` is 8 bits, and the Go table, its slow functions and the
native table each have 256 entries, every value with one: a value no
operation has is `rhInvalid`'s, which panics and which the verifier proves
no code holds, so the dispatch needs no range check. 245 values are taken
(`ROpCount`), 11 are free.

How a value is allotted:

- **Appended, never inserted.** A new operation takes the value before
  `ROpCount`, so no existing value changes, and its handlers are appended
  after the others in their files and in the native block, so that none
  before them moves ([Code layout](#code-layout)).
- **Only for what the dispatch must tell apart.** What changes nothing of
  how an instruction is dispatched is not an operation: a call's
  regular-expression work is a mark of the call (`RegCode.Marks`:
  `RMarkDead`, `RMarkReuse`, `RMarkLitRecv`, `RMarkLitArg`), read by the
  site's call of the built-in; a BigInt operand that may be given back is a
  bit of X (`RArithFreshB`, `RArithOwnA`); an append to an upvalue is
  `add`'s X.
- **By frequency.** A fused form earns a value only where a static count
  over the corpora shows its pattern often enough (`internal/cmd/regcount`,
  [Loop-kernel instructions](#loop-kernel-instructions)).
- **No sub-operations.** Every cold operation is a handler of its own:
  folding several into one value would bring back the second dispatch the
  table removed, and an instruction that interprets operations of its own
  is a frameless evaluator inside the loop, which the design rules out.

Typed code's raw operations need more values than are free; they take a
second table instead ([Typed code](#typed-code)), whose values are free
where the verifier proves the generic operations absent, rather than a
wider opcode, which would cost every dispatch an instruction and break X's
8-bit users.

## The compiler

### From the AST

The register code is compiled from the AST by its own backend,
`internal/compiler/regcomp`, as V8's `BytecodeGenerator` compiles its AST.
What is not code generation is the walk's, `internal/compiler`: early
errors and scope resolution -- which name is a binding of this function,
an upvalue and of which descriptor, a global; which bindings a closure
captures; what a direct eval or a debugger can see -- and the function's
tables and facts. The walk emits no code. It records its resolution of each
identifier and declaration as it goes (`regcomp.Record`,
`internal/compiler/regrecord.go`), and the backend reads that record
rather than resolving names again. The record also carries the decisions
made with the scopes: which additions are appends to a local's string
(`Appends`, `Freezes`), which function expressions a loop makes once
(`Reused`), each catch clause's slot, which calls are in tail position,
top-level code's declarations.

**Positions.** `RegCode.Lines` is each instruction's source position, an
entry where it changes, -1 for an instruction that has none, and
`RegCode.PosAt` looks one up. A frame's pc is the register pc past the
instruction where it can throw or call out, and a stack trace, an error's
position and the debugger read `Lines` through it. An instruction that
places an error apart from its own position -- a fused store's, a `| 0`'s,
a read's before its call -- names in `Aux` a pc past the code's last, whose
entry in `Lines` is that error's position, and the frame's pc holds it
while it throws. Each position is the one V8 gives the error
(`TestErrorPlacedAsV8`).

Top-level code is compiled with the script or module (`CompileTop`); a
function, at its first call ([below](#compiled-at-the-first-call)).

### The frame

A frame is a window of the runtime's value stack:

```
r0            the function being called
r1            this
r2 ...        the parameters, in order
              the function's bindings that live in registers
              the temporaries
              (far registers, where the bindings do not fit 16 bits)
```

The function and its `this` are registers because a call's window is the
callee's frame: the caller writes the callee to the window's first
register, the receiver to the second and the arguments after them, and the
callee's registers start there. Nothing is copied. A parameter no argument
was given for, and the bindings after the parameters (`RegCode.Inits`),
start as undefined. The register compiler can prove that a trailing part
of those bindings is written before every read (`RegCode.SkipInit`), so
the interpreter omits their entry stores. The proof excludes captured
bindings, direct eval, debugger code, arguments objects, suspending
functions and exception handlers; an unknown instruction or a bounded
liveness analysis that does not converge retains all entry stores. The
full binding count and conceptual initial state remain available to the
JIT. Arguments past the parameters, which only
`arguments` and a rest parameter read, are kept aside by the functions that
have them (`KeepsArgs`, `UsesArguments`).

A binding a closure captures is a register too: the closure's upvalue
points into the window while the frame lives and is closed when it ends,
which works because the value stack never moves. Where a reader needs a
local by its slot in the function's `Locals` -- a direct eval, the
debugger, an exec's reused array -- `RegCode.LocalReg` maps each local to
its register.

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
register, `return a + b` an `add` and a `ret` -- or, where the consumer
only reads it, into whatever register already holds it, which for a
binding is the binding's own. An operand is read where it lies.

Reading a binding in place has one hazard: code run between the read and
the operation could change it. `x + f()` reads `x` before `f` runs. The
backend copies such an operand to a temporary first when the rest of the
expression could assign it -- an assignment to the same name in it, or a
call where the binding is captured by a closure that writes it. Everything
else is read in place.

For each backward jump the backend records the registers live at the loop's
head (`RegCode.BackLive`): the temporaries past them are dead there, and
the collector's sweep of stale slots clears them so that a dead temporary
keeps nothing alive.

### Folds and fusions

The backend folds what V8's parser and bytecode generator fold: unary
operators and arithmetic on literals, string-literal concatenation,
constant conditions, dead branches, code after a jump. It fuses where a
fused instruction saves a dispatch and its parts cannot be told apart:
`o.a += j` is `add_prop`, `x[i++]` `get_index_post`, `s.charCodeAt(i)`
`char_code_at`, a receiver's method read straight into a call window
`get_method_w`, a literal's keys `new_object_lit`, a counted loop's step
and test `step_jlt` (by a peephole over the finished code, `fuseSteps`). A
fused instruction keeps, in `RegCode.Aux` where it needs one, the pc past
the code's whose position its second part's errors take
([Positions](#from-the-ast)).

Two adjacent named reads, `o.a.b`, keep both instructions and positions:
the first is `get_prop_next`, followed by an ordinary `get_prop` reading
its temporary. The native handler probes both data-property caches and,
where both answer, stores only the final result and skips the partner.
A miss leaves the first read to its Go handler, then runs the partner at
its own position, so accessors, proxies and errors retain their order.
Longer chains pair without overlap; optional chains keep their ordinary
reads. The JIT lowers each instruction as an independent property read,
with its original frame state and feedback site.

A negative literal is an immediate wherever a literal is, and a literal
left operand of `*` is moved to the right (`3 * x` is `mul_imm`), which
`+` cannot do, since it may join strings.

### Compiled at the first call

A function's register code is compiled when it is first called, as V8
compiles lazily: one never called is never compiled. The walk keeps the
record and the AST in a `bytecode.RegLater` until then, and
`Function.RegCode` compiles it once, under a lock, since a program's
functions are shared by the runtimes running it. Script and module code,
which runs at once, is compiled with them.

By the first call the function's tables are shared: every runtime that
loaded the program has made a closure's tables from them, and another
goroutine may be reading them. So code compiled late adds nothing to the
Function: it compiles over a view whose slices are full to their capacity,
and keeps the tables with its additions in the `RegCode` (`Names`,
`Constants`, `PropSites`), its sites numbered past the Function's. A
runtime adds what is past a closure template's tables where the template
is first given its register code (`regExtend`), its names and strings
copied out of the source under a memory limit, as an evaluator's are.

Whether a function is a leaf (`RegCode.Leaf`), the backend decides on its
register code, and so at its first call, which is made with a frame: the
calls after it are answered without one where the leaf allows ([Frames and
calls](#frames-and-calls)).

### The verifier

The interpreter reads registers and code without bounds checks, so what
it takes on trust is proved for every function as it is compiled
(`regcomp.Verify`): every register an instruction names is below
`NumRegs`, a call's window too; every constant, name, site, binding and
upvalue it names exists; every jump lands on an instruction; the last
instruction does not run past the end; a step-and-branch's partner is the
instruction after it; and every operation an instruction holds has a
handler. Code that fails is a compiler bug: it is refused
(`regcomp.Refuse`), never run.

## The loop

The interpreter's portable form is a small fixed loop, `runReg`
(`reg_run.go`), over a table of handlers, a Go function an operation
(`reg_table.go`, `reg_table_cold.go`, and the later additions in
`reg_table_<name>.go`):

```go
st := r.regEnter(f, c)
regs, code, pc := st.regs, c.code, start
for {
	w = regAt(code, pc)
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
one indirect call. What is hot is decided by the program, so no operation
is privileged with a place in the loop and none pays a second dispatch.
The loop is about 30 instructions, `runReg` about 800 bytes, and an
operation added does not change it; `TestRegLoopIsSmall` holds it below
864 bytes and fails where it stores to the stack between one dispatch and
the next. On amd64 and arm64 `runReg` hands its frame to the native
handlers before its loop, and the loop runs only with `QJS_NONATIVE`.

**The protocol.** A handler is given the frame's registers, the
instruction, its pc and the frame's state (`regState`), in the registers
Go's ABI passes them in, and returns the pc to go on at:

- `pc+1`, or a jump's target, where it has done the instruction;
- `^pc` where its fast path has not -- an operand of another kind, a cache
  that missed, a back edge that spent the interrupt budget: the loop then
  runs the operation's slow function from `regSlowTable`, which does the
  whole instruction (`rsGetProp`, `rsAdd` ...) and never returns `^pc`;
- `regDone` once the frame has returned, or has thrown what none of its
  handlers catches (`st.err`). What `ret` returns the loop reads from the
  instruction it ended at, so `ret`'s handler stores nothing and is a
  leaf.

**Leaf handlers.** A fast path that misses gives back `^pc` rather than
calling its slow function, and a number is stored by its number word alone
where the register's pointer word is nil -- a store of no pointer, which
needs no write barrier. So arithmetic, the bitwise operators, `inc` and
`dec`, the loads of a constant that is no pointer, the compare-and-branches
and the jumps are leaves: no frame, no stack check, no call. A handler that
stores a pointer has a frame and a write-barrier check. A temporary that
held an object and is given a number goes to the slow function, which
stores with the barrier: register allocation reuses temporaries across
kinds.

**An operation of no fast path** -- a call, a closure, a cold operation --
has its whole self as its handler. A call's handler is the call path itself
(`rhCallW`, the first function after the loop): as a handler that called a
call function it was a frame more a call, and `call_mutual` and
`method_call` were 17% slower.

**Errors and positions.** A handler that can throw or run code first keeps
the pc past its instruction (`st.at(pc)`, `st.kept`), which is also the
frame's pc, what a stack trace and an error's position read
([Positions](#from-the-ast)). The fail tail, `st.fail`, finds the handler of the
instruction at `kept.pc-1` (`regCatch`) and goes on there, or ends the
frame. `TestRegThrowsKeepPC` reads the handlers' source and fails where
one reaches the tail, or hands a call the kept pc, before it has kept it.

**The interrupt.** A jump back counts down the runtime's budget in its
handler; where it is spent the handler gives back `^pc`, and the slow
function makes the check (`regJumpSlow`, `regBackEdge`), the condition
evaluated again, which has no side effect.

**State per runtime.** A frame's state comes from the runtime's free list
as the frame starts and goes back, its pointers cleared, as it ends
(`regEnter`, `regLeave`): separate runtimes on separate goroutines share
none, and a frame a Go panic leaves keeps its own for the collector.

**The window and the code are unchecked.** A handler indexes the frame's
registers through a pointer to an array of 65,536 values at its first
register, so `regs[w.A()]` has no bounds check; the verifier has proved
every register below the frame's count, and the frame is given that many
slots before it runs. The code is read at a pc the verifier has proved is
in it.

**Why a table, not a switch.** A measured bake-off chose it. `runReg` as a
switch varied 10 to 27% across code placements against 1 to 5% for the
table; ten unrelated operations added moved kernels by up to 27%, against
3% for the table; an operation of no hot loop paid 5 to 8 ns for a second
dispatch; and the switch grew with every case past Go's big-function size,
past which Go inlines only callees of cost 20 into it, and needed a layout
lock of its own -- a golden file of its cases' offsets, levers inside it,
and an IR node budget. The table's handlers measured 4 to 7% slower than
the switch on the kernels' hot operations and 10 to 15% faster on what the
switch dispatched twice. The best Go form measured is this one: the
arguments in registers, the next pc returned, the leaf number stores. A
state read from a struct, the next instruction or handler returned, and
handlers that called the next one themselves were all slower. The table is
also the shape native code takes on amd64 and arm64, which leaves to its Go
handlers what it does not do itself.

What is left is Go's: every operation ends in a return to the loop and its
indirect call, a handler that stores a pointer has a frame and a barrier
check, and a Go call passes at most four arguments in registers, so the
state's fields are loads. The native handlers remove all three.

## Native handlers

On amd64 and arm64 a register frame runs by native threaded handlers
(`native_run.go`): machine code for the hot operations, over the same
instructions and the same frames, which leaves to the Go table what it does
not do itself. There is no second interpreter: what the native code does
not do, an exit to the Go handler does. arm64's handlers are amd64's,
handler for handler, in its instructions; below, amd64's registers are
named, and arm64's where they differ.

**Why native.** A Go handler's cost is its dispatch: a return to the loop,
an indirect call, the loop's state stored and reloaded around it, a frame
for any handler that stores a pointer. Each native handler ends in its own
dispatch, as Ignition's do -- the next instruction read, its handler's
address loaded from the table, an indirect jump: four instructions, nothing
spilled -- with the state every handler needs pinned in machine registers
across it. That made the tier pay: the native handlers' 0.82 against the Go
table's 0.995, both against the old tiers ([Why a register
machine](#what-it-measured-as-the-default)).

On arm64 the generated blocks are aligned to 32 bytes, keeping each
handler's address modulo 32 fixed when the Go code before the assembly
grows. The placement comparisons can still vary their alignment modulo 64.

**Pinned registers.** `RBX` the instruction's address, `R12` the frame's
registers, `R13` the dispatch table, `R15` the closure's caches, `R11` the
back-edge budget, `R10` the least tag, `RDI` the context; `RAX` holds the
instruction word, and `RCX`, `RDX`, `RSI`, `R8`, `R9` and three SSE
registers are a handler's scratch. `R10` also holds the write-barrier
flag in its low bit (see the marking rule below). No Value's word is
`tagBase` itself -- every tag is at least `tagBase+1`, every number
below `tagBase` -- so a comparison with the threshold answers the same
with the bit set, and the exact tag checks compare the bits from 16 up
alone. No handler uses `R14` (`R15` on arm64), kept for a feedback vector
to be pinned in should one be needed, so that a handler's note would be
one OR to memory. On arm64 the pinned registers are `R19` to `R25` in
that order, with `R7` the write-barrier flag as Go last left it, `R0` the
instruction word, `R1` to `R6`, `R16`, `R17` and `D0` to `D3` the scratch;
the code uses `R26` only for an exit's func value and `R27` only as the
encoder's temporary, and never touches `R18` (the platform's), `R28` (the
goroutine), `R29` (the frame pointer) or `R30`, which holds `nativeEnter`'s
return to `nativeLoop` throughout, so that a profile taken in the code
unwinds to it.

**What is native.** 134 of the 245 operations have a native handler
(nativegen's `handlerOps`); the rest go straight to the exit stub. Native
are the numbers' arithmetic and bitwise operators, the int32 forms,
increments, the compare-and-branches and step-and-branches, truthiness,
`==`, `!=`, `===`, `!==` (two flat strings compared eight bytes at a time up
to 1,024 bytes; a longer text is Go's, so that a loop comparing it each turn
still exits to see an interrupt), moves, constants, upvalues, globals by
their site, property reads and writes by the site's entry or its
polymorphic entries (`propPoly`), a property added by the transition the
site remembers or one of its polymorphic entries does, a key string's
store by its keyed site's entries
(`set_index`, `define_index`), an array's length written as long or
shorter, as `setArrayLength` truncates it (nativegen's `arrayLength`: a longer one, a
value that is no length and storage a literal shares are Go's), `o.n +=`, `in` by the realm's cache and by an array's
elements, `typeof` and `jtypeof`, a template's parts, `length`, dense
arrays' and typed arrays' elements (every kind but Float16), short BigInts'
`+`, `-`, `*`, their compare-and-branches of two registers and of a register
and an upvalue, and their `===` and `!==` (equal only if both their words
are, a BigInt the Value holds being held one way), a string's `charCodeAt`
(fused, or the built-in a method call's site has cached) and the built-ins
`codePointAt`, `at` and `charAt` a method call's site has cached,
an element written at an array's length where it has room,
`Array.prototype.push` of one argument, a `switch`'s table, a for-in's step
over an array's indices or a snapshot of keys, a for-of's step over a dense
array or a generator of register code, a destructuring's step over an array
by the intrinsic iterator (`iter_step`), a cursor's close where nothing is
to be called (`iter_close`), `yield` in a frame that step entered, closing
upvalues, `instanceof` by OrdinaryHasInstance (the realm's
`Symbol.hasInstance` as the site's cache finds it, the constructor's own
data prototype, the value's chain walked), a script-level `let`, `const` or
`class` read and written by its global site (`globalLex`, at the global
epoch as a global object's slot is), and calls, constructions and returns
([Frames and calls](#frames-and-calls)). A fast path that misses
jumps to the exit before it has written anything. arm64 answers one case
more: NaN made an integer, which `FCVTZS` gives as 0 where `CVTTSD2SI` has
no integer.

An `instanceof` site's property cache also keeps the constructor's own
prototype slot, plus one, in two bytes of its padding. It is filled only
when the constructor's remembered shape proves its inherited
`Symbol.hasInstance` is the realm's immutable intrinsic and its prototype
property holds data. A matching shape and an ordinary, unbound function
then need no property lookup: the handler reads the current prototype
value and walks the instance's chain, leaving proxies and errors to Go.
Refilling the property cache clears the slot; collecting the runtime's
shared constructor cache leaves it intact. The probe uses the site's
already computed address, so a miss does not repeat that lookup. This
probe follows the existing out-of-line native helpers, keeping their
positions as well as the handlers' unchanged.

When a condition alone consumes an `instanceof` result, regcomp emits
`instanceof_next` followed by its ordinary `jmp_true` or `jmp_false`
partner. The native handler shares `instanceof`'s checks and prototype
walk, then takes the partner's branch without storing a boolean or
dispatching the partner. A backward branch spends the same interrupt
budget. Any miss, including a spent budget, leaves both operands intact
for Go, which stores the boolean and continues at the partner. The Go
table and JIT execute the pair as the two original instructions, and so
does the feedback dispatcher, whose handler of `instanceof_next` is
plain's `instanceof` (nativegen's `feedbackUnfused`): the partner then
counts its back edge toward the function's budget, which a branch taken
by `instanceof_next` itself would not. The handler and its cache probe
follow every handler and helper before them, so that none of those
moves.

Where the constructor is an immediately preceding global read into a
temporary, regcomp emits `get_global_instanceof` before that pair. The
native handler probes the global site's epoch and slot, reads the current
constructor there, and shares the ordinary `instanceof` checks and walk.
It skips the temporary store and its partner's dispatch. A miss returns
to the global read, before any visible work or register store; the Go
table and JIT execute all three instructions normally. The fused form is
not emitted for debugger code. The feedback dispatcher runs the ordinary
global read and leaves the branch to its own handler, so the JIT sees each
back edge.

**The exit stub.** An operation with no native handler has the exit stub's
address in the table, and every miss jumps to it. It writes the
instruction, its index and the budget to the context, makes the top frame's
Go state where it is not made (`nativeTop`), and calls the instruction's Go
handler from `regTable` itself through `nativeCallH` -- a function that the
Go function which entered the code called, as the JIT's `callGo` is, which
gives the handler its arguments in Go's registers and its frame the room
they spill to. Where the handler gives back `^pc`, the stub calls the slow
function the same way; the code goes on at the pc they give, the pinned
registers put back from the context. Where the frame has ended, where Go
has no state for the depth yet, where the native frame stack is full, and
at the base frame's return, the stub returns to `nativeRun`, which does the
rest and enters the code again. A handler whose miss the Go handler would
only send to its slow function calls the slow function at once
(`nativeSlowOnMiss`).

**Calling Go's convention.** The stub and the trampoline write Go's internal
register convention (ABIInternal) by hand: on amd64 the four arguments in
`AX`, `BX`, `CX` and `DI`, the func value in `DX`, the result in `AX`, the
goroutine in `R14`, `X15` zero, and the arguments' spill room in the
caller's frame; on arm64 the arguments in `R0` to `R3`, the func value in
`R26`, the result in `R0`, the goroutine in `R28`, the spill room at
`8(RSP)` to `40(RSP)`, and the word below the stack pointer left alone (it
holds `nativeLoop`'s caller's frame pointer). Go does not promise that
convention across releases: `TestRegNativeCallHConvention` calls a Go
handler through `nativeCallH` and fails, saying which part differs, where
the toolchain's convention is not this one, and CI runs it under Go 1.24 and
the newest Go.

**Calls of Go with no exit.** What Go does that throws nothing and runs no
code is called through `nativeCallH` with none of an exit's work: a
closure (`nativeClosure`), a literal (`nativeLit`), an iterator result
(`nativeIterRes`), an object for a construction (`nativeNew`), a call of
the JIT's code (`nativeJITCall`). A built-in a call site has cached is
called the same way (`nativeBuiltin`): one that needs no frame -- Math's
functions, push and pop, a native leaf whose check passes -- with none of
an exit's state, since it throws nothing and runs no code (`string_at` 61.0
to 52.0 ns on a Ryzen 5 3600); anything else as `rhCallW` makes it
(`regNative`), returning to the exit stub's tail, which goes on or ends the
frame, so that a built-in that throws or calls back is the exit's case.

**String methods by their target.** Some of String.prototype's methods
the handlers answer themselves where `call_method_w`'s site has cached
the built-in, as V8's call feedback, not its bytecode, decides what a
call does: the method is known by its `elemOp`, the mark a built-in has
of a frameless application, and answered on a string, with a number for
its argument, by a kernel of its own (nativegen's `strIntrinsic`):
`charCodeAt` by `char_code_at`'s (`charCodeUnit`), `codePointAt`, `at`
and `charAt` by theirs (`strUnitCall`), whose four `elemOp`s are in a
row, so that another built-in is told by one compare. So
`s.charCodeAt(++i)` and `s.charCodeAt(i + 1)`, whose method is read
before the argument is evaluated and which `char_code_at` therefore
cannot take, and `s.codePointAt(i)`, `s.at(i)` and `s.charAt(i)`, which
have no fused form, are an ordinary method call: a script function in
the built-in's place, or another built-in, is the site's callee like any
other, and the JIT reads the same feedback. `codePointAt` joins a
surrogate pair; `at` counts a position below zero from the end; `at`'s
and `charAt`'s string is the runtime's own of a character below 256, the
one `unitString` hands out, read from the runtime's tables
(`charStrings`, `latin1Strings`), so that the kernel allocates nothing,
and undefined or `charAt`'s empty string past the string. A receiver
that is not a string, an argument that is not a number, a rope, a code
unit of 256 or more for a string, a character whose string the runtime
has not made yet, or anything else the kernel does not answer is
`nativeBuiltin`'s. The Go table calls the built-in as a native leaf. The
code is laid after the call handler's numbered exits (nativegen's
`end`): push's check of its `elemOp`, which goes there, is the only jump
of an earlier handler it changed, and that by its displacement.

**Call sites of several callees.** A call site's cache holds one callee.
Where `call_w` or `call_method_w` meets another, the handler exits with the
context's `miss` set and the stub calls `nativeCallMiss`, which makes the
cache the new callee's and calls it as the cache says, so that the code
then enters it itself rather than Go entering it in a code entry of its own.
The feedback dispatcher counts the call again as it dispatches it again, and
`nativeCallMiss` takes back its first count (`jitRegUncount`).

**The context.** What the code reads beyond the pinned registers is the
context (`nativeCtx`), kept by a frame's `regState` and made at its first
native run: the top frame's code, caches and closure, the runtime's fields
it reads, and the native frame stack, one record a call the code made
itself, which starts at 8 records and doubles up to 128 (a call past that
is Go's). A realm's fields -- its prototypes, its caches of the in
operator's answers and of the primitives' methods -- are read through the
realm running now, the Runtime's first word, at offsets the context gives,
never by an address saved in the context, since the collector does not see
it and the realm may be gone (`TestRegNativeInDroppedRealm`). Each
runtime's contexts are its own.

**Generated, position-independent, one for every build.** The handlers are
written with the JIT's assemblers by a generator,
`internal/vm/internal/nativegen`, and assembled into the binary as one TEXT
symbol of machine code -- BYTE directives on amd64, WORD directives on
arm64, whose assembler takes nothing else -- with the table as GLOBL/DATA
(`native_handlers_<arch>.s`, `go generate ./internal/vm`). The
generator does not import package vm: it reads vm's private layout -- the
context's, the frame records', the Runtime's offsets, the constants the
handlers test -- as plain numbers from `NativeLayout`, which only a build
with the `quickjs_nativelayout` tag has. The block is never patched and has
no runtime's constant in it: every address is the context's, and the
Runtime's and a closure template's fields, whose offsets the JIT's tag
changes, are read relative to bases the context gives (`rtBase`,
`ccBase`). So package vm needs no assembler and no JIT at run time, and one
block serves every runtime. `TestRegNativeGenerated` fails while a file is
not what the generator makes; `TestRegNativeLinked` that a binary links
what the generator makes for its build.

**Variants.** The generator makes a dispatcher per architecture and, within
one, per variant: a dispatch table a runtime picks through its contexts'
table pointer (`R13`, `R21` on arm64). `plain` is the only one in builds
without the JIT, and the one a runtime that does not run the JIT uses in
builds with it. `feedback`, in JIT builds alone, is a runtime's that runs
the JIT ([Inline caches and feedback](#inline-caches-and-feedback)): its own
handlers record what the JIT needs and then go on in plain's handler, so
that plain's code has no part of it. A variant's block holds only the
handlers it overrides (`native_handlers_feedback_<arch>.s`, sorted after
plain's block so that the stubs before plain's lie where they do without
the JIT), its table
pointing at plain's for every other operation. `TestRegNativePlainSameWithJIT`
checks that plain's block is the same with and without the JIT, and
`TestRegNativeFeedbackSelected` that a runtime picks its table. The
debugger's mode is meant to be a variant too.

**The marking rule.** A handler stores a number's word alone where the
register held no pointer; a pointer word it stores plainly while the
collector does not mark, and by the barrier's routine (`nativeStoreRef`,
a store with Go's write barrier) while it does, reading the runtime's
write-barrier flag. The flag is read once and kept between calls of Go,
in `R10`'s low bit on amd64 and in `R7` on arm64: Go changes it only
with the world stopped, which native code, never a safe point, does not
let happen, so it can change only while Go runs. Go runs only before
native code is entered (`nativeEnter`) or inside `nativeCallH` and
`nativeStoreRef`, and the flag is read again at each before native code
goes on: on amd64 into the low byte of the context's tag threshold,
from which every reload of the pinned registers takes `R10`; on arm64
into `R7`, by the two trampolines and by the code's entry. That is the
only difference: the code does the same work whether the collector
marks or not, and never leaves native code or calls Go for the
marking's sake. `TestRegNativeKernels`,
`TestRegNativeFastPaths` and `TestRegNativeCalls` run each case with the
flag read as set and as clear and fail on a different answer or a
different count of exits; `TestRegNativeCallExits` checks the call handler
against `rhCallW` case by case, and `TestRegNativeConcurrent` two runtimes
at once.

**Preemption.** The code is not preemptible: Go reaches it at the exits. A
loop that never exits still spends the back-edge budget and a recursion the
interrupt counter, which the handlers count down as the Go handlers do,
exiting at the end, so that the interrupt check -- and with it the
scheduler and the collector -- runs as often as in the Go table's loop.

## Frames and calls

**The call path.** `call_w` and `call_method_w` (`R[B] = R[A](R[A+2] ...
R[A+1+X])`, this `R[A+1]`) are one handler, `rhCallW`, natively and in Go.
The site's cache (`regCallCache`, by the instruction's C) holds the callee
it called last, its closure, its register code and a kind saying what the
call does with the this. A callee the cache knows, of the register form's
frame layout and with nothing of its surroundings to establish -- not
bound, not a class constructor called, not an arrow sharing a derived
constructor's `this` -- is entered with no question asked again: the window
is its frame, its parameters the arguments where they lie, the bindings
past them made undefined. Anything else is `regCallMiss`'s.

**The frame record** is the runtime's `frame`, from a block of them, and
written only where a reader needs it: the function, the window's base and
the pc, which stack traces read, and the few fields its last user may have
left set, cleared where they are (`regEnterW`). The this, new.target and
arguments are the window's: a frame's `regKind` says so to the readers that
find them through the record. One decrement and one compare make the depth
and interrupt checks. Natively, `call_w` enters a callee as `rhCallW` does
and pushes the caller's state on the native frame stack; `ret` pops it.

The interpreter recurses on the Go stack in Go, a call of `runReg` for each
JavaScript call; natively it does not, and Go is entered only at an exit.
Calls inline in Go's loop were measured in the stack interpreter and lost
3.7 to 5%, since the loop then spills more.

**Calls answered without a frame.** A body whose first instruction returns
a parameter or a constant is answered at the call site from the window
(`regLeafArg`, `regLeafConst`); a short pure body of numbers and upvalues,
straight code to its first return of at most 12 operations, runs as its
register code with no frame (`regLeafCode`, `regLeafRun`), its stores to
upvalues held until it returns so that what it cannot answer gives up
having changed nothing; a getter of a captured binding is answered from the
callee's closure (`leafUp`). A frameless run gives up while an interrupt is
due, which its frame then counts. These are the cases a frame costs more
than the body, and each is answered alike by every entry -- a call site, a
built-in's callback, natively -- so that no path is slower for having
skipped another's check.

The plain native call handler also answers a leaf returning one numeric
addition, subtraction or multiplication of parameters or captured bindings.
Its register code records the operation and its two sources in `leafNative`;
the handler reads them directly from the call window or closure, without
making a frame. Missing arguments and non-numbers take the ordinary call
path before a value is stored. Its result uses the ordinary native store,
including the write barrier while the collector marks. These helpers follow
the existing native handlers so that adding them moves none of those
handlers. The JIT's feedback dispatcher retains its ordinary call path and
counts the callee for compilation.

A pure helper comparing `typeof` of a parameter or captured binding with
any type name also has a `leafNative` descriptor, for equality or inequality.
The plain native handler uses the ordinary type classifier and returns the
boolean without a frame; a missing argument is undefined. Proxies and
`[[IsHTMLDDA]]` objects take the ordinary call path before a value is stored.
The descriptor is recognized from the complete register-code pattern,
including the boolean return, so a numeric result or additional work still
runs as the ordinary body. The Go table evaluates the pure body, and the
JIT's feedback dispatcher keeps its call and compilation counts.

The same descriptor also records getters returning `this.k`, `this.k.length`
or `this.k[p]`. The native helper probes the callee's own-property cache
before reading the field. It answers lengths of dense arrays and numeric
indices naming present dense elements; other cases enter the ordinary
frame, with the call window untouched. A cached replacement for the call's
`this`, an accessor, an inherited property, a hole or a non-numeric index
also takes that path. Pointer results use the native write barrier. The
helpers sit after the handlers and preserve every handler's offset.

**Built-ins.** A built-in of the call's realm is a kind of the site's cache
(`regCallNative`), called with the window's arguments as its slice, without
a copy. A call of the realm's `Function.prototype.call` whose callee the
site's cache holds (`regCallThrough`) enters the callee natively in the
window one register on, and `apply_args` its target in a window made at
the stack's top (`nativeApply`), neither with the built-in's frame, which
stack traces and `f.caller` pass over, as V8 has none. A built-in calling a
callback enters the register code directly (`regDirect`): its arguments
copied into a window at the top of the stack, no `defer`, no `recover`.

A callback a built-in calls many times -- a sort's comparator, the
callbacks of forEach, map, filter, some, every, find, reduce and the typed
arrays', a replacement function of `replace`, `JSON.parse`'s reviver -- is
kept ready from one call to the next (`regCallback`): its window reserved
at the stack's top once, and natively its code entry's state and context
kept too. Each call writes the window's registers and the frame record and
enters the code (`nativeCallbackRun`): where the last call returned with no
exit, the state and the context's top frame are still the frame's, and
only the first instruction and the budget are given before the entry.
Anything else -- an exit, a throw, a frame given to a callee in place --
goes on as the native loop does (`nativeDrive`), and the next call makes
the state and the top frame again. The frame record is pushed for the call
alone, so that what Go runs between two calls finds no frame of it. A body
the frameless evaluators answer (`pureCallAt`) is cheaper entered so with a
frame (`array_some` 50.9 to 42.5 ns); a short straight body of numbers and
upvalues still runs frameless in the window (`regLeafRun`), and one that
returns a parameter or a constant is answered from the arguments
(`callDirect`). A runtime that runs the JIT keeps its leaves on
`callDirect`, which counts them for tier-up, and so does the Go table,
whose loop entered for each call is slower than the frameless evaluators.
A function with calls in tail position is entered so too, directly
(`regDirectTail`) or kept ready: its frame may end given to a callee in
place or in a call given up, which the entry finishes once the frame is
closed (`regTailEntryEnd`) -- the frame run again with the callee's code
(`regRerun`), or the call made by `regTailLoop`, but where that loop owns
the chain at this depth -- and a kept callback's window is reserved again
as it was, however many registers a callee given its frame used.
A reviver that cannot see a third argument -- fewer than three parameters,
none a rest parameter, no arguments object or direct eval, no debugger and
not Node's quirks -- is given no context object, and the parse keeps no
records of the source text for it (`seesThirdArg`: `json_parse_reviver`
4,667 to 1,524 ns).

**Constructions.** `new_w` has a call site of its own, whose cache holds
a constructor the handler enters as `call_w` enters a callee
(`regCallConstruct`): a function's or a base class's once its objects'
room is settled, a derived class's, and a forwarder's (`LeafForward`), none
with fields of its own. Its object is made by Go with no exit (`nativeNew`)
of the constructor's prototype and root, as `constructWithTarget` makes it
-- a body that only stores its parameters answered by `leafCall` there --
and the frame is `regFrameConstruct`, its new.target the constructor, a
derived class's with its this binding (`regThisBinding`); `ret` gives the
frame's this where the body returns no object. `super_call` and
`super_forward` in a derived class's constructor enter the parent
constructor natively in a window at the stack's top (`nativeSuper`,
`regFrameSuper`), whose `ret` binds the caller's this as `superBind` does,
and `derived_ret` is answered in place. A derived constructor's this is the
binding `super()` binds, read through the frame (`this_bound`) by it and
the arrows inside it. Anything else -- a bound function, a proxy, a
built-in, a class with fields -- is constructed by the generic path, the
site's miss (`regConstruct`).

The construction cache's unused `this` words remember where the
constructor's shape keeps `prototype`: the shape and the slot
(`regCtorThis`). A shape check replaces the property lookup; the slot's
value is read each time, so that a `prototype` assigned takes effect, and
a remembered shape is never changed in place. Another shape is looked up
and remembered again; an accessor, a missing property or a primitive takes
the ordinary construction path.

**Initializers.** The register compiler gives a body that only stores up
to eight of its parameters in `this` -- some made 0 where falsy, written
`if (!p) p = 0` or `this.k = p ? p : 0` -- and returns undefined a plan of
its stores (`RegCode.Stores`: each store's cache site, its parameter, and
whether a falsy one is 0); a base class's constructor that does the same
and returns `this` gets one too, which only a construction makes
(`regCode.initNew`: a forwarder whose method is the class, and an
`apply_args` of it, run it in its frame, which throws the TypeError of a
class called without `new`); and a constructor that is
`this.k.apply(this, arguments)` or `this.k.call(this, ...)` -- the call's
arguments parameters or constants -- a plan of its forwarding
(`RegCode.Forward`). The native interpreter makes those stores itself,
without the bodies' frames (`initRun` in nativegen's `*_init.go`): at a
construction of a constructor with a plan, at one through a forwarder whose
method `this.k` -- read at the forwarder's site, or in its table of
`this.k` reads (`fwdSite`) where the site has given up -- is a function of
a closure, not bound, with a plan, and whose `apply` or `call` is the
realm's, and at an `apply_args` of a target with a plan. Every store is
checked before any is made, as `set_prop`'s handler checks it: a plain
writable property of the shape the stores before it leave, or an addition
`addProp` would make, with room for it; and a parameter made 0 is decided,
a string's, a BigInt's or `document.all`'s by the body. Then each is
written, in the plan's order, at its slot -- a property's own, or the one
its addition's layout gives it, in the room the check found -- its
pointers through the write barrier's routine while the collector marks;
and the layout the last store leaves, with its count of values, is given
the object once, as `addProp` gives an addition's, no code having run that
could see the layouts between. A name stored twice is written twice, in
order. Anything else -- a setter, a read-only or missing property, a
proxy, an accessor or a replacement of the method or of `apply`, a bound
or proxy target, a body that is not a plan -- runs the bodies in their
frames, nothing written. `nativeNew` and `nativeApply` say where the
handler may try: the JIT not running, whose frames have feedback to give,
no interrupt due, and the frames the bodies would have had within the
limit, so that past it they throw the frames' RangeError. The Go table
runs these bodies in their frames, as it runs any other.

The native initializer keeps, on the template's `closureCode`, what the
last plan it checked in full proved (`initCache`), as V8 keeps a
constructor's initial map and the map its stores lead to: the layout the
object had before the stores and the one they left, each store's slot, and
the prototype chain as the additions' caches checked it -- the first two
prototypes and their layouts, or the chain's validity cell. The layouts
are remembered ones, never changed in place, so an object of the first
layout whose chain is as it was takes the same stores to the same slots:
`initRun` then checks the layout, the room for the second, the object's
extensibility and the chain once, decides the parameters made 0, and
writes each value at its slot and the final layout, with no check of each
store. For a plan of up to four stores with no parameter made 0, the cache
also keeps each store's parameter index and the argument count it needs.
A hit with enough arguments makes the stores directly, in order, without
walking the plan; missing arguments take the ordinary cached path, which
supplies undefined. Both paths use the same pointer barriers and final
layout update.
Anything else is checked store by store as before and refills the
entry -- emptied first, its slots recorded as the values are written, its
pointers written through the write barrier's routine while the collector
marks, and the first layout last -- where every addition's cache checked
the same chain; past `maxCacheFills` fills, as a site's cache gives up, it
is not refilled. A prototype replaced roots the objects elsewhere, and a
prototype's layout changed -- a property added or made an accessor,
frozen, sealed, given another prototype, made a dictionary -- fails the
chain's check, so the entry needs no invalidation of its own. It is one
runtime's, as the template's caches are, and holds what they hold. One
entry serves a constructor wherever it is constructed, since its objects
start from one root; a site's call cache, which the call handlers index by
its size, does not grow for it.

**Tail calls.** `tail_call_w` and `tail_call_method_w` are operations of
their own, so that the ordinary call checks nothing of tail position. While
the stack is shallower than `tailCallDepth` they are ordinary calls. Deeper,
a strict callee that keeps no arguments and does not suspend is given the
frame in place (`regTailTo`): the arguments moved to the parameters, the
bindings past them undefined, the frame record the callee's; natively the
caller's state stays on the native frame stack, so that the callee returns
to the frame's caller. A chain of such calls runs in one frame however long
it is.

**Arguments.** `arguments.length` and `arguments[i]` of a function whose
object is made only on demand are `args_len` and `args_index`, which reads
an index past the parameters from the frame's kept arguments. `args_len`
reads the frame's kept count natively until the object is materialized;
then its length is read as an ordinary property. A function
may read them through a local initialized with `arguments` directly in its
body too, if neither binding is captured or reassigned and every read of
the alias follows its initialization and reads a length or element. The
existing compiler walk records those reads; the register backend checks
their resolved bindings, leaving eval, debugger and with code alone. All
aliases use the arguments binding's register, so a slow index read that
materializes the object gives every later read the same object. A function
whose only use of `arguments` is to hand it to `apply` has no object made:
each such call is `apply_args`, which hands on the frame's arguments as
passed. Where an object is made, the frame keeps its arguments on the
runtime's argument stack until it returns, before its bindings take the
window past the parameters.
The native `apply_args` makes the stores of a target's initializer plan
itself, as a construction does (see Initializers); the caller keeps its own
frame and arguments.

## Exceptions

An exception is an error value returned, never a panic: a Go `panic` and
`recover` cost microseconds where a returned error costs a compare. A
function with handlers has a table of them (`RegCode.Handlers`, innermost
first), each a list of ranges of pcs, the handler's pc and the register the
exception's value goes to. Nothing runs as a try block is entered or left:
no push and no pop.

- **One way out.** Every exit with an error goes through `st.fail`, which
  looks the instruction at `kept.pc-1` up in the table (`regCatch`) and
  goes on at its handler, the value in the handler's register, or ends the
  frame with the error for its caller to do the same.
- **What a handler catches** is a JavaScript exception, whatever threw it
  -- a throw statement, an operator, a getter or a setter, a built-in, a
  call, a stack overflow's RangeError. An interrupt or a memory limit goes
  past every handler. A call that threw has left the stack and the frame
  records as they were before it, so a handler needs no unwinding of its
  own.
- **A throw statement inside a try of its own function** is a move of its
  value into the handler's register and a jump to it, with no exception
  object made or stack captured.
- **A finally clause is compiled once for each way out of what it
  protects** (regcomp's `try.go`): in place after the block's normal
  completion, before each break, continue or return that leaves it (the
  return's value taken first into a register of its own), and as the
  handler for a throw, which throws the value again at its end. There is no
  completion record and no dispatch on one: each copy goes on as its exit
  does. A copy before an exit is outside the ranges of the handlers the
  exit leaves and inside those of the ones around them, so that a throw
  from a finally clause run by a break is caught by the try around the
  loop, not by its own catch. Copies nest up to four deep
  (`finallyDepth`); a clause deeper is compiled once with a completion
  dispatch (`finShared`), and so is every clause inside it.
- **The catch binding** is its slot's register, which the record names, or
  a temporary where nothing reads it.

## Iteration

for-of and for-in keep their cursor in a register of their own:
`for_of_start` or `for_in_start` makes it, with fast paths for an array, a
generator and a Map's or a Set's iterator, and `iter_next` steps it at the
head of each turn, jumping out where it has no value. `iter_next` reads an
array's element in its dense storage, steps a generator of register code
directly (below), and calls any other iterator's `next` as register code
calls; a step of an iterator of script reads its result's `done` and
`value` where they are its own data properties, without the generic read's
walk (`regStepOf`). A for-in walks an array's indices by index while its
element is in its dense storage, and an object's snapshot of keys while it
keeps the shape they came from. Whether nothing up an object's prototype
chain has an enumerable key, which every for-in asks, is kept in the realm
by the chain's first object while the chain's validity cell holds
(`protosNoEnum`). A dictionary's keys -- a plain object whose layout is its
own -- are made once for its layout and kept (`dictForIn`), the layout
remembered as a cache remembers one, so that the loop's steps hand them
out natively as a shared layout's (`dict_for_in` 71 to 11 ns).

Cursors are reused rather than made at each loop. A for-in cursor needs no
closing, but a loop left early marks it done all the same, so that the next
loop takes it (`newForInObject`); an array's cursor that `iter_close` or
`iter_close_throw` closes -- a pattern's end, a for-of left by a break --
is released (`iterState.released`) and the next cursor over an array takes
it (`arrCursor`). A pattern's rest element over an array whose rest is all
in its dense storage is copied at once (`regIterRest`).

A for-of loop's body is in the ranges of the cursor's own handler, which
closes the iterator as a throw does and throws again; a break, a continue
of an outer loop or a return that leaves the loop closes it on the way out
as it runs a finally clause (`iter_close`). `iter_next` is outside the
ranges, so an iterator whose `next` threw is not closed. A for-await's step
is `async_iter_next`, `await` and `iter_result`, its closing
AsyncIteratorClose's, the result awaited.

## Generators and async functions

A generator's frame is its window (regcomp's `gen.go`, the VM's
`reg_generator.go`). Suspension copies the window and the pc into the
generator object, and resumption copies them back to the top of the stack
and goes on at the pc. There is no operand stack, so the copy is the
registers alone, and only from `genFrom`: where no closure captures the
callee's register or this's, a resumption makes those two again from the
generator. The captures open on the window's registers move with it,
pointed at the generator's copy on a suspension and back at the stack on a
resumption (`regMoveUps`), so that two closures made in different
resumptions share a binding.

The generator and its saved values share one allocation for windows of
up to eight values (`genAlloc`), with room for two, four, six or eight.
The six-value case fits short generators with two parameters and their
suspension registers without reserving eight. A function whose generators
are stepped by loops also reserves the first loop's cursor in that same
allocation; an async function reserves its promise pointer instead.

- **Entry.** The code's first instruction is `gen_entry`, which a
  resumption that goes on skips. A generator's creation runs its
  parameters' binding up to `end_params`, which suspends it, so that a
  default that throws throws at the call; a generator whose parameters need
  no code is not run at its creation. An async function's first step runs
  at once, its errors rejecting its promise.
- **Delivery.** What a resumption delivers goes to the suspension's own
  register, never a binding's: `yield` (the value sent in), `await` (what
  it settled to), `yield_star` (the value and how). A return injected at a
  yield is compiled after it, as a return statement there, through the
  finally clauses and the loops' iterators it leaves; a throw injected is
  thrown at the suspension, inside its handlers' ranges.
- **`yield*`** is a loop of the code's own: `iter_resume` sends what the
  generator was resumed with to the delegate, `iter_unpack_delegate` reads
  its result, `yield_star` suspends with it. A delegate its realm's next
  steps is stepped directly, with no result object at any level of a chain
  of delegations.
- **The drivers** -- next, return and throw, an async function's steps, an
  async generator's queue -- hand a generator's resumption to
  `regGenResume`. A call site of a
  generator function or an async function makes its object without the
  generic call's search (`regGenCall`).
- **Natively**, a for-of step over a generator of the realm suspended at a
  yield, and a call of its `next`, are the code's own (nativegen's
  `genStep`, `genNext`, `genYield`, `genReturn`): the step pushes next's
  frame and the generator's, copies the window to the stack's top and goes
  on where the resumption goes on; the yield copies it back, pops both and
  goes on past the step with the value. A throw, a yield with captures open
  on the window, `yield*` and the rest are Go's.
- **An async function's first run is a call.** A call site of an async
  function that keeps no arguments, has no direct eval and nothing of its
  surroundings runs its body in the call's window as a call runs a callee
  (`regAsyncRun`), with no generator: one that returns or throws before it
  awaits settles its promise and is done, and one that awaits makes its
  generator then, from the window as it is, and queues the await's job as
  `runAsync`'s first step would. What runs, and in what order, is the same
  (`TestAsyncFirstRunOrder`, Node's order); what is saved is the generator
  and the window's copies (`async_call` 318 to 192 ns).
- **An await's job continues in place.** An async function resumed by its
  await's job goes on at once at an await of a value that is not an object
  or of a fulfilled promise of the realm's constructor, where no job is
  waiting: the job the await would queue would be the next to run, with
  that value. The first run of an async function, which its caller's code
  follows, never does.

## Closures and upvalues

A closure captures a binding by pointing an upvalue at its register while
the frame lives; the frame's end closes it, copying the value into the
upvalue. A binding that needs a fresh copy each turn of a loop is closed at
the end of each turn by `close_regs`. A binding captured by value -- a
constant, a parameter no one assigns and no mapped arguments object
aliases -- is copied into the closure.
`closure_w` carries, through `RegCode.Caps`, the register of each of the
new function's upvalues taken from its maker, so that the function's
template needs nothing of the maker's layout. A loop's function expression
that captures nothing a turn changes is made once (the record's `Reused`).

The closure's capture list is filled directly from those registers and
the maker's inherited upvalues. Only bindings not already open receive
new cells, in one allocation sized to those bindings. A function with up
to 17 captures keeps the capture list in its function object's allocation;
the larger variants fill successive allocation size classes.

Natively, `close_regs` and a return close the upvalues open on the frame
with no exit, and `closure_w`'s closure is made by a call of Go with no
exit. The allocation is Go's.

## Inline caches and feedback

Every property, element, global and call site has an entry in the
function's caches, kept per closure template and per runtime
(`closureCode`): `ic` for property, global and literal sites, `regEpochs`
beside it for the globals, `regCalls` for calls.

- **A property read or write** compares the object's shape with the
  entry's (`propCache`; `ownShape` for an own data property, one compare)
  and reads or writes the slot. A site that meets a second shape keeps the
  entries it met before (`propPoly`, up to three more, as V8's polymorphic
  caches keep four maps); a site that keeps meeting new shapes stops
  filling (`maxCacheFills`) and reads through the realm's shared cache,
  found by the shape and the key together, what V8 calls the megamorphic
  stub cache; its additions through the realm's cache of them
  (`deepAdds`), each with the prototype chain it was made along. The
  native handlers probe both themselves at such a site, as V8's
  interpreter's handlers probe its stub cache, and an object's own table
  for a read and a write of a property it has -- a small one scanned, a
  larger one's index probed as `findOwn` probes it, its recent entries and
  then its slots (`indexFind`)
  (`internal/vm/internal/nativegen`'s `*_mega.go`), laid out after every
  other code of the block; Go fills them. A prototype's property is cached
  with the holder and the prototype chain's validity cell; an accessor with
  its getter or setter. An addition remembers its transition.
- **A global** reads or writes the slot its site remembers while the global
  epoch is the one the site was filled at. The epoch advances when a
  binding of the global object moves, goes or changes its attributes and
  when a script-level lexical binding that may shadow a global is
  declared, so the check is one compare. A site is filled only for a plain
  data property -- a writable one for a write -- of a global object no
  lexical binding shadows.
- **A keyed store** -- `o[k] = v`, `{ [k]: v }`, a class's field --
  has a site of its own (its Aux, regcomp's `keyedSite`), whose entries
  remember, for each pair of a shape and a key it met, the slot it wrote
  or the transition it took, its own entry and up to three more, as V8's
  keyed store inline caches remember names and maps
  (reg_table_keyed.go). A key string's atom is the one keyAtoms
  remembers with it, as for a keyed read; an array index is the
  element paths' and no entry's. A site that keeps meeting new pairs
  stops filling (`maxCacheFills`).
- **An element** of a dense array is read or written in place for an index
  that is an int32 in range; a typed array's element, an append, a hole
  filled are out of line but not the slow path's (`regElemMiss`,
  `regSetMiss`).
- **A call** compares the callee with the function the site called last
  and enters it as the cache's kind says.

**Call caches and the turn's end.** A call site's entry holds its callee
strongly, and with it the closure's captures: an entry in a template a
whole program shares -- a library's forEach -- had kept a finished
program's graph alive through every collection after it. V8 holds call
feedback weakly; here the handlers read the entries as they are, so the
turn's end lets them go instead. The paths that fill a call cache -- the
misses, all of them -- put the template on the turn's list once
(`regCallsHold`); a turn's end, with no frame running, empties those
templates' caches and keeps each site's callee as a weak pointer on the
template (`releaseRegCalls`, `regSeen`). The next call at a site fills its
entry again. The JIT, which reads the entries as call feedback, refills
them from the weak pointers where the callee is still alive
(`regCallsRestored`), so that a function warmed in one turn and compiled in
the next is compiled for its targets. The calls that read a cache pay
nothing for any of it.

**Feedback for the JIT.** The JIT reads the caches above as its feedback,
by register pc, and what its own code learns at its exits. What no cache
holds is recorded only by the feedback dispatcher's own handlers, which only
a runtime that runs the JIT is given: the calls each call site makes and
each function's runs, for call frequencies; the work of each backward jump
by its distance, for the tier-up budget; the operators that met BigInts.
A runtime without the JIT runs handlers that record nothing, and a build
without it has no such handlers. internal/jit/README.md ("Feedback") is the
detail.

## Loop-kernel instructions

Value-in, Value-out instructions for counted loops and asm.js-style `| 0`
arithmetic, which save dispatches without a second representation of
values. They follow the rules every operation does: appended to the
operation values, each with its regcomp pattern, its Go handler and slow
function, its amd64 and arm64 native handlers and its JIT lowering, so that
none opens a refusal; and their native handlers appended after every other.

- **Step-and-branch**: `step_jlt`, `step_jle` (a register bound),
  `step_jlt_imm`, `step_jgt_imm`, `step_jge_imm` (the immediate SB) and
  `step_jle_upval` (an upvalue bound). A is the counter, SC the offset, X
  the step: its low 7 bits a signed k (-64 to 63, never 0,
  `bytecode.StepK`), its high bit `RStepAdd` for `+=`/`-=` of a literal,
  else an update (`++`/`--`, k = ±1). The instruction is the step and then
  the compare-and-branch at pc+1, its partner, in one dispatch: R[A] += k,
  then to pc+1+SC where the partner would jump, else to pc+2. The partner
  stays in the code, which is what makes every miss simple: a step of what
  is not a number is made by the slow path, which goes on at pc+1; a bound
  that is not a number, or a backward jump with the interrupt budget spent,
  commits the step and goes on at the partner, whose slow path compares and
  checks the interrupt. Nothing is ever done twice or skipped. The step's
  throws are placed at its position, the comparison's at the partner's. The
  JIT lowers it as its step alone, falling to the partner, so the IR is the
  code before fusion. Its backward jump counts toward the JIT's budget as
  the partner's does. regcomp's `fuseSteps` makes them of each adjacent
  pair, but in a function whose jumps are far; for a debugger, a for loop's
  test has a `debug` of its own between the two, so nothing is fused.
- **`mul_imm`**: R[A] = R[B] * SC, of `x * k` and `k * x` (a literal's
  ToNumeric runs nothing and the product commutes).
- **`add_i32`, `sub_i32`, `add_imm_i32`**: R[A] = ToInt32(R[B] ± R[C]) or
  ToInt32(R[B] + SC), `(x ± y) | 0` with no intermediate stored. The fast
  path is two numbers whose result is below 2^63 in magnitude, its
  truncation's low 32 bits; the slow path makes the operator as `add` or
  `sub` make it, then the `| 0` of its result, and a BigInt result throws
  the `|`'s TypeError at the position of the pc `Aux[pc]`. X `RAddII32Sub`
  says the source was `x - K`, since `"5" - 1` is not `"5" + -1`. The JIT
  lowers each as the pair it stands for. With them regcomp leaves out a
  `| 0` whose left is a bitwise operator with a number literal or a `>>>`
  for an operand: its result is an int32 Number, or it threw.

The kinds were chosen by `internal/cmd/regcount`'s static count of
adjacent step and compare-and-branch pairs over Octane, Are We Fast Yet,
the V8 suite and the micro-benchmarks: `jlt` 1,006, `jlt_imm` 234,
`jge_imm` 80, `jle_upval` 36, `jle` 26, `jlt_upval` 18, `jgt_imm` 15, the
rest 7 or fewer each. A compare of two `| 0`s, which `jlt_i32` would take,
counted 48 in zlib and none in Mandreel, against 853 `sub_i32` sites, the
rarest of the int32 forms: not worth a value. `loop_int32`'s turn is 12
dispatches with them, 17 without.

The verifier checks each: the step ±1, or k ≠ 0 with `RStepAdd`; at pc+1
the partner's operation of the same A, B and target, and an instruction
after it (`bytecode.StepPaired`); `mul_imm`'s X 0; the int32 forms' Aux a
pc past the code's that `Lines` has.

## Typed code

A function of a validated asm.js module is given *typed code* (T), a twin
of its ordinary register code (G) in which every register has one kind for
the whole function (`RegCode.Kinds`): a Value, a raw int32 or a raw
float64. T needs operations the ordinary set has no values for, so it is
decoded through a second table, in which the cold range's values -- which
typed code never holds -- are the raw operations. G stays the function's
code: every generic entry runs it, and a module that fails validation, or a
closure that fails its link, runs G alone, so nothing is refused.

### Validation and compilation

- **Validation.** `internal/compiler/asmjs` validates a function that says
  `"use asm"` as the compiler meets it (`compileFunctionLiteral`), before
  its body is compiled. The subset is what fastcomp-era Emscripten writes,
  floats and `Math.fround` included. It records each expression's type. A
  literal's type comes from its source text: the parser folds an operator
  of literals into one literal, which keeps its first operand's position,
  so a folded literal is typed by its value and its first token. Code
  compiled for a debugger is never validated. `QJS_ASMTRACE` prints why a
  module fails.
- **Compilation.** `regcomp.CompileTyped` compiles a function of the
  module from those types at its first typed call (`regcomp.LaterTyped`,
  kept in the function's `RegLater`, so that `Function` keeps its size),
  after its ordinary code, whose call sites, names and constants it
  extends: T numbers its call sites past G's, since the two codes share
  the template's caches. The frame is registers 0 and 1, then the
  parameters as the call passed them -- Values that nothing writes, so
  that `f.arguments` reads them as passed -- then a raw register for each
  local, raw temporaries of one kind each, and last every call's window,
  Values: a call's arguments are computed into raw temporaries first, so
  every window is at the same place, past everything raw.
- **Positions.** T has its own line table (`Lines`), each instruction at
  the position the ordinary code gives the same expression. A frame
  running T holds a pc of T; a stack trace marks it `bytecode.TypedPC` as
  it captures the frame (`framePC`), and `Function.PosAt` finds a marked
  pc in T's table. Which frames run T it reads from the runtime's typed
  code entries (`typedRuns`): a typed entry runs typed frames alone, and
  a call out of it is an entry of its own, so its frames are its own
  depth and those its native context entered past it.
  `TestAsmStackPositions` pins every frame's position against G's.
- **The kinds verifier.** `regcomp.VerifyKinds` checks the rules K1 to K8
  before the code is used: each operation's operand kinds, the frame's
  head Value and never written, every call's window Value with nothing raw
  live above it, nothing that reads a frame generically (no handlers,
  closures, suspension, `arguments`, far registers or method calls), raw
  words leaving only through a box, and the heap's shifts, module
  variables and tables in range. All 51 functions of Octane's zlib compile
  and pass.

### The operations

`bytecode/regtypedops.go` names them at the cold range's values: moves
and constants; the conversions into raw (`tconv_i`, `tconv_f`, which may
run `valueOf` and throw) and the boxes out of it (`ibox`, `ubox`, `fbox`,
the last making a NaN canonical); int32 arithmetic that wraps, division and
remainder that give `| 0`'s results (a zero divisor gives 0, `MinInt32 /
-1` gives `MinInt32`); compares and compare-and-branches, signed and
unsigned; float arithmetic, compares and branches, whose negations differ
on NaN; `fround`; heap loads and stores of every view, bounds-checked (out
of bounds a load is 0 or NaN and a store does nothing); module variables,
read and written in their upvalue cells; and `tab_get`, a function
table's entry. An I register's int32 is its number word's low 32 bits. A
`switch` compiles to compare-and-branches, and the Math functions T does
not inline (`floor`, `pow`, ...) run in Go, by the engine's own Math.

### The table and the call path

- **Where T lives.** On the template, as `closureCode.regTyped`. In a
  runtime with the JIT the linked closure also has a twin whose register
  code is T, which typed call sites enter and the JIT compiles at the
  module's link (internal/jit/README.md, "Typed code").
- **Tables switch only at a code entry.** A call site caches as enterable
  only a callee whose code is of its own kind. `regCallKind` gives a typed
  caller the callee's T, and an ordinary caller T where the callee is no
  leaf. `regCacheCallee` marks a callee of the other kind `regCallCross`,
  which none of the hit paths' compares of the kind, Go's (`rhCallW`) or
  native, lets through; the site's miss (`regCallSlow`) then enters T by
  `regEnterWTyped`, in a code entry of its own. The JIT's native code,
  which reads no table, calls the twin natively from either kind of code
  (internal/jit/README.md, "Typed code"). A typed site caches no
  built-in, since the call handler calls one with no exit, after which the
  code would read its heap as it was. `f.call` and `f.apply` of a typed
  function enter its ordinary code.
- **Go.** `runRegTyped` is `runReg`'s loop over `regTypedTable` and
  `regTypedSlowTable` (`reg_typed.go`, after the ordinary table's files),
  with states from a list of their own (`regFreeTyped`). The ordinary
  operations typed code may hold (`bytecode.TypedPlainOps`) keep their
  handlers; `call_w`'s is `rhTCallW`, `rhCallW`'s copy, whose entered
  callee runs by `runRegTyped`. Every other value is `rhInvalid`.
- **Native.** The typed dispatcher is a variant of nativegen's
  (`nativegen/typed.go`, `amd64_typed.go`, `arm64_typed.go`), in every
  amd64 and arm64 build but the layout's. Its table has its own handlers
  of the raw operations, plain's of the ordinary operations typed code
  holds, and the exit stub for every other value. A typed code entry's
  context holds it throughout, so a call between two typed functions is
  plain's call handler, its callee's frame decoded by the same table and
  its return going back to typed code. `nativeRunTyped`, `nativeLoopTyped`
  and `nativeCallMissTyped` are the ordinary ones' copies over the typed
  table, since those name the ordinary one. Division, `Math.clz32`, `abs`,
  `min`, `max`, `%` of doubles, the other Math functions and `tab_get`
  exit to Go.
- **What ordinary code pays.** Nothing on the hit paths: `runReg`,
  `rhCallW`, `nativeLoop` and plain's handlers are the same code. A miss
  pays one compare (`rc.typed`), and a cache fill one load of `regTyped`.
- **The prototype.** `QJS_TYPEDPROTO=name,...` (test-only, read once)
  gives the named functions a twin that is their ordinary code
  (`bytecode.TypedTwin`), the ordinary arithmetic and branches of
  `call_fib_rec`, `call_even`, `call_odd` and `cross_add` in the typed
  table too (`bytecode.TypedProtoOps`): the call path's tests and
  `call_cross`'s crossing run on it. When the call path was built
  (2026-11-15, two placements), ordinary calls' geometric mean was 1.002
  natively and 1.001 in the Go loop, typed calls to typed code within 2%
  of untyped, and a crossing 11.6 ns natively, 3.4 times `func_call`.

### The link and the heap

- **The link** is per closure, at its first call from a call site
  (`regTypedOf`, from `regCallKind`'s cache fill). T reaches the module's
  bindings through its own closure's upvalues, and its code names what
  each upvalue it reads must be (`RegCode.Asm`): a view of the heap, the
  realm's own Math function, a number, a module variable, an import, a
  function of the module or a table. Validation forbids assigning the
  bindings the link checks, so what a closure's upvalues hold at its first
  call is what they held at the module's return. Only a template's first
  closure is linked (`regTypedCode.asmCl`), since its typed code and heap
  are the template's.
- **The heap** is the views' buffer, read as of the runtime's
  `typedEpoch` (`asmHeap`). A buffer detached later, by an import or a
  host, has no bytes: every load gives 0 or NaN and every store does
  nothing, as JavaScript's views of a detached buffer do. Native code
  reads the base and length from its context, which Go makes current at
  the entry and after every exit (`nativeTypedExit`, `nativeLoopTyped`).
  A typed site enters a typed callee in its own code entry only where the
  callee's heap is its own or none (`regTypedCross`).
- **Switches.** `QJS_NOTYPED`, read once, leaves every module to its
  ordinary code, as `QJS_NONATIVE` does the native handlers.

## Tiering to the JIT

With the JIT on, the JIT compiles register code; internal/jit/README.md is
its design. What the interpreter
gives it:

- **Input.** `compile.LowerReg` (internal/jit/compile/lowerreg.go) lowers a
  function's register code to the JIT's slot IR: a register is a slot,
  most instructions one IR instruction with their operands named, a fused
  one several of the same origin under the atomicity rule -- nothing exits
  once something of it has committed, so that an exit can always run the
  register instruction again from its start.
- **Feedback.** The interpreter's caches, by register pc, and what native
  code learns at its exits; call counts and back-edge work from the
  feedback dispatcher's handlers.
- **Tier-up.** A function's returns count its runs, and its backward jumps
  their distance, against a budget; the check that spends it compiles the
  function, in the foreground or the background. A template whose function
  has code is marked (`jitEnters`), which every call entry -- `rhCallW`,
  the native call handler, `regEnterW`, the callbacks' entry -- reads, so
  that no handler does any other work for the JIT. A loop is entered by
  on-stack replacement from the back edge's check (`regBackEdge`,
  `jitRegLoop`), whichever dispatcher runs the frame, which goes on where
  the native code leaves the frame.
- **Exits and deoptimization.** Native code runs on the register window in
  place. An exit writes the live registers and either runs the register
  instruction whole by the interpreter's own handlers and goes on natively,
  or resumes the interpreter at the exit's register pc. An inlined callee
  is materialized as a register frame at its caller's window.

The JIT refuses direct eval, `with`, the debugger's code and far registers
([Known limits](#known-limits-and-open-work)). Typed code it compiles from
its kinds, at its module's link and with no speculation, and an exit from
it resumes the typed table (internal/jit/README.md, "Typed code").

## The debugger, eval and with

- **The debugger.** Code compiled for a debugger has a `debug` instruction
  at each statement, where a pause or a breakpoint can happen; such code is
  compiled for it alone and runs only in the interpreter. `LocalReg` tells
  the debugger which register holds each local, and its assignment to a
  binding writes the register. Natively, `debug` is an exit.
- **A direct eval** (`direct_eval`) reads and writes its caller's bindings
  through descriptors that name their registers (`LocalReg`).
- **A `with` body** resolves its names dynamically: `with_get`, `with_set`,
  `with_resolve` and the rest look the objects of the frame's with scopes
  up first, then the binding the record resolved.

## Code layout

Package vm's code layout decides much of the interpreter's speed: AMD Zen
2's op cache holds the code at one offset of every 4 KB in one of 64 sets,
and hot code a change moves onto the same sets is decoded again on every
pass (AGENTS.md, docs/optimizing.md). The layout lock holds the hot code
in place whatever a change moves around it, and the register tier is laid
out so that what changes moves as little of the rest as it can.

- **The lock, not the names.** The register interpreter's Go code is in
  `reg_*.go`, the native handlers' Go side in `native_*.go`, the JIT's in
  `jit.go` and `jit_*.go`. Functions are laid out in file-name order, so
  code added in one file moves every function after it; the lock's pads
  put the anchors back (`runReg`, `rhCallW`, `regNativeHandlers` below),
  and file names are chosen by topic, not by where they sort. What still
  depends on names is listed in AGENTS.md: the pads' files, the
  assembly pad before the handlers' files, and the inits that read the
  handler tables (`reg_tables.go`, `reg_typed.go`) after the files that
  write them.
- **No case lock.** `runReg` is a fixed loop with no cases, so there is no
  golden file of case offsets and no lever inside it, as the switch it
  replaced needed; an operation added changes no instruction of it.
- **Levers Q, W and H.** The lock anchors `runReg` (Q) and the call path
  after it, `rhCallW` (W), at fixed offsets modulo 4 KB
  (`reg_ru_layoutpad.go`, `reg_run_a_layoutpad.go`); the Go handlers
  follow them in their files. The native handlers' block is assembly, laid
  out after every closure of the package, so that a closure added anywhere
  moved all of it -- 672 bytes in one round, which made Octane's RegExp and
  the generator tests slower; H anchors it (`regNativeHandlers`) by a pad
  of whole 32-byte slots of `int3` in an assembly file before it
  (`native_a_layoutpad_*.s`).
- **New handlers go in files of their own.** A Go handler added to
  `reg_table.go` moves every handler after it; a branch added to
  `rhCallW` once cost the Go table 7 to 10%. So later handlers are in files
  of their own, `reg_table_<name>.go`, which sort after it
  (`reg_table_fast.go`, `reg_table_i32.go`, `reg_table_step.go`), whose
  `init` replaces their table entries; `reg_tables.go`, which sorts after
  every such file, then makes the native exit table from the whole
  tables.
- **The native block is laid out in `handlerOps`' order,** and what came
  later goes last, so that it moves no handler before it. A path an early
  handler takes rarely -- `get_index` by a key string, a tail call past
  `tailCallDepth`, a generator's step, a for-in's step -- is laid after the
  last handler, reached by a jump (amd64's `nativeGen.late`): laid in
  place, four such paths moved `set_index_i`'s neighbours and made two
  typed-array tests 8 to 11% slower at every placement.
- **The regular expression lever X.** A package is laid out where the
  packages linked before it end, so `internal/bytecode`'s new operations
  moved `internal/regexp` 3,520 bytes and made the V8 suite's RegExp 3.5%
  slower at every placement, a change no pad of package vm, which comes
  after it, could reach. The lock pins the matcher (`(*matcherASCII).run`)
  by a pad at the start of package regexp (`internal/regexp/aa_layoutpad.go`).
- **What is not tuned.** arm64's native block and the Go table on other
  architectures follow the rules above but are not layout-tuned: the lock
  and its pads are amd64's. A platform that needs a fast interpreter gets
  native handlers, not a tuned Go table.

After any change to package vm, `go generate ./internal/vm` regenerates the
native handlers and then the pads; `TestLayoutLock` fails until it has run.
`hotdiff` compares the interpreter's hot functions instruction by
instruction between two builds: `runReg`, the calls (`rhCallW`,
`regEnterW`, `regCallSlow`, `regRunFD`, `callObject`), the native
dispatcher's Go side (`nativeRun`, `nativeLoop`) and its assembly, the
handler blocks included; the native handlers are also pinned by their
generated files' staleness tests.

## Correctness

- **The verifier** proves what the handlers take on trust, for every
  function as it is compiled ([The verifier](#the-verifier)).
- **The forms test262 never reaches.** `TestRegForms*` (regforms_test.go)
  run what an instruction's operands cannot number -- names, registers,
  jumps, call arguments and cache sites past 16 bits or 8 -- a stack trace
  through register frames and f.arguments with Node's quirks, and check
  each answer against the one the stack interpreter and the tree tier gave
  before they were removed. `TestRegNativeCaches` (internal/vm) runs the
  micro-benchmarks natively and on the Go table and compares the caches
  the JIT reads (`RegCacheDigest`): the native fast paths and the Go
  handlers must fill them alike. The register interpreter's semantics are
  test262's, in each of its modes, and the vm package's tests'.
- **Native against Go.** The native kernel, fast-path and call tests run
  each case natively and by the Go table, with the write-barrier flag set
  and clear, and compare answers and exit counts.
- **The debugger's code** is register code with `debug` instructions; it
  must do exactly what other code does, which test262 checks with every
  runtime made `WithDebugger` (`-conformance.debugger`).
- **test262** passes what the default build passes in every mode: plain,
  with the JIT (`-conformance.jit`, and under `QJS_JIT_STRESS`), with the
  debugger, and on the Go table (`QJS_NONATIVE=1`).
- **compilediff** compares register code between two revisions of the
  backend, every field of each function's `RegCode`, or the refusal or
  panic that left it none, compiled as its first call would.
- **arm64** runs the same tests on CI's arm64 runners and under qemu.

## Tools and switches

| | |
|---|---|
| `QJS_NONATIVE=1` | register code on the Go table on amd64 and arm64 |
| `QJS_REGREPORT` | at a runtime's close, the calls into and out of register code, by kind and by callee |
| `QJS_REGTRACE` | print why a function has no register code |
| `QJS_REGDUMP` | print each function's register code as it is made |
| `QJS_NOTYPED=1` | asm.js modules run their ordinary code, never typed code |
| `QJS_ASMTRACE` | print why a `"use asm"` module fails validation, or a function has no typed code |
| `QJS_TYPEDPROTO=name,...` | test-only: the named functions run typed twins of their ordinary code |
| `disasm` | each function's register code; `-jit` whether `LowerReg` lowers it, and its exit maps |
| `regrefusals` | compile directories of code and count the backend's refusals by reason |
| `regcount` | static counts of the patterns fused and int32 instructions take, and of those they leave |
| `compilediff` | the register code two revisions make, compared |
| `hotdiff` | two builds' hot functions compared instruction by instruction (`-funcs` names others) |
| `bigfunc` (`BIGFUNC_ALL`) | Go's inlining size of the loop and its handlers |

## Known limits and open work

- **The walk and the AST.** A function never called keeps its AST and
  the walk's record until it is, and the walk resolves every function's
  scopes as the program is compiled. A preparse that resolves a function
  once it is first called, as V8 reparses a lazily compiled function, needs
  a resolver that can start inside a function
  ([performance-plan.md](performance-plan.md)).
- **Other architectures** run the Go table, slower than the native
  handlers. The portable fallback's dispatch -- a generated switch over the
  Go handlers against the table -- is to be measured, and is never
  layout-tuned.
- **The JIT's refusals of register code:** direct eval, `with`, the
  debugger's code, an arguments object the frame cannot own, and far
  registers past what an exit's records name (`abi.MaxSlot`).
- **Raw registers outside asm.js.** Index-heavy code that is not a
  validated asm.js module pays a float conversion at every element access
  and every `| 0`, where the tree tier had kept an int32 in a Go variable.
  Designed and not built: raw float64 temporaries within an expression
  behind one guard, and raw int32 regions with statically bounded
  exactness, each with a verifier of kinds, whose operations would take
  values of the typed table ([Typed code](#typed-code)).
- **A baseline compiler or an assembly loop.** Neither is planned: the
  native handlers are the interpreter's machine code, and a baseline
  compiler would pay only if it almost never fell back to the interpreter.
