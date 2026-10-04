# Performance plan

This is the list of optimizations found by a review of the whole engine in
October 2026, after v0.16.12, and the state of each. The review read the
code area by area -- the interpreter and call path, the tree tier, objects and
property access, the built-ins, the compiler, and allocation -- and tied each
item to the code that costs. None of the estimates below was measured when
the list was written; each item is measured as it is done, and its row says
what came of it.

Where go-quickjs stands is in [benchmarks.md](benchmarks.md#compared-with-c-quickjs):
level with C QuickJS over the V8 suite, about 1.4 times its time over
QuickJS's own micro-benchmarks. The figures here are from that page's runs
unless an item says otherwise.

## How an item is measured

- Code placement moves the V8 suite's figures by a few per cent, and Richards
  and DeltaBlue by up to a tenth, with no change in the code that runs. A
  change is compared with its parent over eight builds that place the code
  differently, three rounds each, taking each placement's fastest round, with
  Richards and DeltaBlue run 300 times a process.
- A change to the tree builder (`internal/vm/tree_build.go`) also gets the
  long check: Crypto and NavierStokes, 20 runs on each of the eight
  placements. The tree's closures move with any code added among them, and
  have cost those two benchmarks up to 8% on their own.
- A micro-benchmark is timed on the paths the change touches, against its
  parent, before the suite is.
- Allocation counts are deterministic, and are compared exactly.
- Ideas already measured and rejected are listed at the end, so they are not
  tried again without a new angle.

## A. Largest expected effect

| ID | Item | Targets | Risk | Status |
|---|---|---|---|---|
| A1 | `dup` without a spill for property updates | `prop_update` 2.6x, `array_update` 2.8x | Low | Done |
| A2 | Strict global writes without the separate check | `global_write_strict` 3.4x | Low | Done |
| A3 | A cheaper call path, outside the interpreter's switch | `func_call` 1.35x, DeltaBlue, Richards | Low | Done, in part |
| A4 | Cheaper calls from trees, then no call-density gate | EarleyBoyer, RayTrace, Richards | High | Done |
| A5 | Fewer allocations for objects and strings | Splay, DeltaBlue, RayTrace | Medium | Open |

### A1. `dup` without a spill for property updates

`o.x += j` compiles to `get_local o; dup; get_prop x; bin_local j; set_prop x`,
which is worse than what `o.x = o.x + j` compiles to (`get_local2 o o; get_prop
x; bin_local j; set_prop x`). In the tree tier, `dup` always spills
(`OpDup` in `tree_build.go`): a statement stores the object into its stack
slot and another copies it, and both property nodes then see stack slots
rather than a local, and take their generic variants. That is about seven
indirect calls and two stores where QuickJS dispatches six cheap
instructions. `tab[0] += j` goes through `elemReadForUpdate`, three stack
stores and two reloads, for the same reason. `this.n++` as a statement adds
`to_numeric`, `insert2` and `drop`, and `insert2` keeps such a method out of
the frameless evaluator.

- In the tree builder, `dup` and `dup2` of a local, `this`, a number or a
  literal push copies of the entry, with no statement: the two copies are
  adjacent, so nothing can run between their reads. `insert2; set_prop; drop`
  becomes the existing `setPropStmt`, and `insert3; set_index; drop` the
  existing `setIndexStmt`.
- In the compiler (`compileMemberUpdate`, `compileUpdate`), the object read
  is emitted again instead of `dup` when it was a single `get_local`,
  `get_upvalue` or `push_this` and nothing jumps to where the `dup` would go;
  `fuse` then makes a `get_local2`. Literal keys are read again the same way.
  A prefix `++`/`--`, or one whose value is unused, needs no `to_numeric`:
  `inc` and `dec` convert.
- Expected: `prop_update` about 30 to 15 ns, `array_update` about 25 to 12
  ns; `this.n++` methods frameless; Richards's `holdCount++`, `queueCount++`
  and `count--`.

**Done.** The compiler reads a local, an upvalue or `this` again where an
update copied it, and a number-literal key with it, which needs no
conversion; `++` and `--` convert only the old value a postfix form yields;
a property's update for effect leaves nothing to drop. The tree tier reads
a plain read twice on `dup` and `dup2`, and stores `a[0] op= v` directly.
`insert2; set_prop; drop` was left: the compiler no longer emits it for an
update whose value is unused.

- `prop_update` 30.0 to 16.1 ns (-46%), `array_update` 25.7 to 16.7 ns
  (-35%); `this.n++` and `o.count--` methods are frameless.
- V8 suite over eight placements: NavierStokes -6.4% (and -5.5% over twenty
  rounds, faster at every placement), Richards -2.2%, EarleyBoyer -1.1%,
  the rest within 1%; total -0.4%. Crypto +0.6% in the long check.

### A2. Strict global writes without the separate check

In strict code `x = v` for a global is `check_global_ref; <v>;
assert_resolved; set_global`, four instructions where sloppy code has two;
in the tree tier four indirect calls where sloppy has two, and the check and
the store validate the same slot through different cache sites. `set_global`
already makes the strict existence check itself. When `v` cannot run code --
a local, an initialized upvalue, a literal, `this`, a closure -- checking
before it or after it cannot be told apart, so the compiler can emit
`set_global` alone (`checkGlobalRef` in `internal/compiler/with.go`, and its
caller in `expr.go`).

Separately, `OpSetGlobal` asks `globalLexProp` on every write, where
`OpGetGlobal` asks only when the script declared a global `let`, `const` or
class.

- Expected: `global_write_strict` from 26 ns to about sloppy `global_write`'s
  12.

**Done**, with a correction: dropping the check is not unobservable. A
proxy on the global object's prototype chain is asked `has` when the
reference is resolved and again before the store, so `set_global` alone
would ask it once where the standard asks twice. So a value that runs no
code is followed by a new `set_global_strict`, which resolves the reference
and stores, one straight after the other: the same questions in the same
order, and the cached slot of a plain global answered without any. `set_global`
asks for a script's `let` only where the script has one.

- `global_write_strict` 26.3 to 11.6 ns (-56%), as fast as a sloppy write.
- V8 suite (which has no strict code) level over eight placements, total
  -0.2%, each within 1%; the long check level.

### A3. A cheaper call path, outside the interpreter's switch

An empty call costs 42 ns, against 31 in QuickJS. Outside `executeAt`:

- `pushFrame` is just over Go's inlining budget (85 against 80) and is a real
  call on every frame-making call. Its fast path written out where it is
  called, or a cheaper `frameHigh`, saves its prologue and the spills around
  it.
- `runFD` makes seven conditional resets and five tests of the function's
  data on every call, for fields almost no function uses; one flag set when
  the closure is made can stand for them. Its two loops that fill locals with
  undefined can be one.
- A call is three Go frames -- `callFromLoop`, `runFD`, `executeAt` -- with
  nine argument registers spilled and reloaded between the first two. A
  `runFD` specialized for `callFromLoop`'s direct path drops one.
- Expected: 2 to 5 ns a call; DeltaBlue and Richards 1 to 3%. None of it is
  in `executeAt`, whose layout is the lottery.

**Done, in part.** `func_call` was the wrong target: its callee is pure and
runs without a frame, and never reaches `runFD`.

- Done: `runFD` binds the arguments and fills the other locals with
  undefined in one pass, where `bindParameters` filled the parameters and
  `runFD` the rest; and asks about a derived constructor, an arrow inside
  one, `with` and `eval` only for a function that has an extra or is
  derived. V8 suite over eight placements: total -0.7%, Splay -2.0%,
  EarleyBoyer -1.3%, the rest within 1%.
- Not done: `pushFrame` inlined. Keeping `seekFrameBlock` out of line made
  it cost 104 (a call that is not inlined counts 57), and moving the
  `frameHigh` update to the pops means twelve places that decrement the
  depth, any one of them missed leaving returned frames uncleared, for
  about 2 ns a frame.
- Declined: a specialized `runFD` for `callFromLoop`. It would copy the
  hottest call code, about 150 lines with its tail-call loop, to save a
  Go frame on calls the frameless evaluator does not already answer, which
  are the minority of the suite's hot calls.

### A4. Cheaper calls from trees, then no call-density gate

The tree tier refuses a function with more than one call for every twelve
instructions. Those refusals are 95 to 99% of what the interpreter still runs
in every V8 benchmark. Without the gate the suite runs 3.6 to 4.0% faster
(EarleyBoyer 12%, RayTrace 5 to 7%, Richards 4%), but DeltaBlue, whose hot
code is methods of about eighteen instructions with three calls, 4% slower:
`this.m(a);` costs a tree seven indirect calls where the interpreter
dispatches five.

- Statements get a destination, so `runTree` stores what a statement
  evaluates, and `set_local`, `drop` and the spills before a call need no
  wrapper closure.
- A call node evaluates its receiver, the method read, the callee and each
  argument itself, in order, into the argument slots, instead of separate
  spill statements before it. (An earlier attempt, `exp-callfuse`, still
  spilled everything beneath the call and measured -0.3%.)
- Block ends become data: the back edge's interrupt check and a jump to a
  known block are made by `runTree`, not by a closure that only returns a
  number.
- Then the gate is measured at 6 and at 0, and dropped if DeltaBlue is level.
- Risk: the tree builder's layout (see "How an item is measured").

**Done**, by the last step alone: the gate is gone, and the tier builds every
function it can. After A1 and A3, DeltaBlue no longer loses by it. Measured
over eight placements against main, three rounds:

| | Total | EarleyBoyer | RayTrace | Richards | Crypto | DeltaBlue |
|---|---:|---:|---:|---:|---:|---:|
| Destinations for statements | +0.3% | -0.6% | -1.7% | -1.0% | +3.0% | -0.2% |
| Destinations, block ends as data, no gate | -3.0% | -10.6% | -6.7% | -6.1% | +3.7% | -0.7% |
| Block ends as data, no gate | -2.5% | -10.7% | -4.6% | -3.0% | +3.2% | +2.4% |
| No gate | -4.1% | -11.8% | -6.7% | -5.6% | -2.7% | +0.3% |

Destinations for statements cost more than the closures they saved: the
test of which kind each statement is, at every statement of every block,
showed in Crypto's loop as runTree's own time doubling. Block ends as data
did not pay either. The call node that evaluates its own operands was not
needed and was not built. The long check of the no-gate version: Crypto
-1.9%, NavierStokes -0.5%. It allocates 0.4% more over the suite, for the
trees of more functions; a first run of the suite is 9% faster.

### A5. Fewer allocations for objects and strings

- An object that outgrows the room its constructor or literal gave it pays a
  second allocation and keeps its properties apart from it: Splay's nodes,
  DeltaBlue's constraints made through `superConstructor.call`, RayTrace's
  `IntersectionInfo`. A layout's root shape can record how many properties
  its objects end up with (`leaveInlineProps`), and the four creation sites
  allocate that many.
- `'String for key ' + tag + ' in leaf node'` makes an intermediate string;
  Splay does it 32 times an insert, 19% of its allocations. A chain of `+`
  that starts with a string literal can convert each operand as it goes and
  join them once (`OpConcat`, which the tree tier must then build), and
  `joinValues` can write a short result in one allocation.
- A `String` is 72 bytes of header; holding its UTF-16 copy as a pointer
  rather than a slice makes it 56, and sharing that word with a rope's right
  half 48: 32 bytes off every substring, rope node and capture.
- One-character strings, `"true"`, `"false"`, `"null"`, `"undefined"` are
  made anew at each conversion, and a non-integer number's text takes two
  allocations. Per-runtime tables, as for small integers, end both.

## B. Specific gaps

| ID | Item | Targets | Risk | Status |
|---|---|---|---|---|
| B1 | `.length` fast path in the existing nodes | `array_length_read` 2.9x | Medium | Open |
| B2 | Element reads and writes in trees without a call | `array_read` 1.85x, `array_write` 1.6x | Medium | Open |
| B3 | Typed-array elements without the generic path | `typed_array_read` 2.1x, `typed_array_write` 1.8x | Medium | Open |
| B4 | Appending to an array, and `arr.length = n`, directly | `array_prop_create` 2.8x, `array_hole_length_decr` 1.7x | Low | Open |
| B5 | No new layout on every `delete` | `prop_delete` 2.1x | High | Open |
| B6 | RegExp per-call costs | `regexp_ascii` 2.2x, `regexp_utf16` 2.6x, V8 RegExp | Low | Open |
| B7 | Number text without intermediate strings | `float_toExponential` 2.1x, `float_toPrecision` 1.9x | Low | Open |
| B8 | Strings to numbers, and numbers to cached strings | `string_to_int` 1.28x, `int_to_string` 1.6x | Low | Open |
| B9 | Date strings without `fmt.Sprintf` (go-intl) | `date_parse` 2.1x | Low | Open |
| B10 | Compiler: smaller code for common shapes | interpreted code (RegExp's runBlocks, EarleyBoyer) | Medium | Open |

- **B1.** `get_length` reaches `arrayLength` through `getValueProp`, `getProp`
  and `getExoticNamed`; an array or string answers inline before `c.at`, as
  `pureCallAt` and `leafCall` already do. This is a fast path inside the
  existing node: a separate `.length` node was rejected, for what its code's
  placement cost.
- **B2.** `elemAt` and `setElem` (`vm_tree.go`) are not inlinable, so every
  element read and write in a tree is a direct call; their bodies written out
  in the local-operand closures save it.
- **B3.** A typed array's element goes through `getIndexSlow`,
  `typedElemIndex`, two interface assertions and the element-size table. A
  view of fixed length can keep its buffer, element shift and end, which a
  detach shows by emptying the buffer.
- **B4.** Appending at `length` tries `setElem`, then `setIndexed`, then
  `appendElem`, which walks the prototypes each time; try `appendElem` first.
  `arr.length = n` on a dense, writable array with a Number that is its own
  uint32 can set it directly instead of through `defineArrayLength`.
- **B5.** Every `delete` allocates a new unique layout. A dictionary-mode
  layout that the caches refuse to fill would be changed in place, as V8's
  is after a delete. Every cache fill must check it: the risk.
- **B6.** RegExp:
  - a pattern that is only its required literal answers with `indexUnits`,
    without a matcher; a run of literal characters inside a pattern can be
    one instruction (`exec.go`, then `go generate ./internal/regexp`);
  - `lastIndex` is always the RegExp's first own property, writable or not,
    and can be read and written there;
  - `test()` builds a flags string it does not use; match and split ask
    for flags by string;
  - `replace` checks that the built-ins are unmodified twice;
  - a RegExp literal in a loop looks its compiled pattern up by its text
    every time.
- **B7.** `toExponential` and `toPrecision` make four to six allocations;
  written into one buffer, as `toFixed` is, they make one.
- **B8.** `ToNumber` of `"12345"` trims by Unicode and calls `ParseFloat`;
  plain digits can be read directly. `n + ""` builds a new string even for
  the integers below 1024 a runtime keeps.
- **B9.** go-intl formats every date string with nested `fmt.Sprintf`, and
  `Date.parse` copies its argument to UTF-16; both are changed in go-intl,
  and taken with `go get`.
- **B10.** Compiler:
  - a call whose result is dropped can say so, instead of a `drop` after
    it (1,229 such pairs in the V8 RegExp benchmark's interpreted code);
  - a comparison with a constant can jump without pushing it;
  - a literal statement such as `"use strict";` can emit nothing;
  - calls to a function known to run frameless need not count against
    the tree tier's gate (`func_call` itself is refused today).

## C. Modern code

These do not move the V8 suite, which is ES5 and has no strict code, but
matter to hosts running classes, modules and async code.

| ID | Item | Status |
|---|---|---|
| C1 | Strict `return f()` (a tail call) takes the generic call path, and keeps its function out of the tree tier and the frameless evaluator | Open |
| C2 | An `await` makes about nine allocations: two functions, a promise nothing can reach, a job, an argument list | Open |
| C3 | A generator or async call makes four allocations, and resuming copies its whole frame | Open |
| C4 | A bound function always takes the slow call path, and allocates its arguments when it has bound ones | Open |
| C5 | Every `for-of`, spread and array destructuring allocates an iterator object | Open |
| C6 | An arrow function is 448 to 480 bytes, most of it fields only bound or `with` functions use | Open |
| C7 | A closure reads a `let` or `const` it captured after its initialization with a dead-zone check, which keeps arrows out of the frameless evaluator | Open |

## Rejected

Measured and dropped earlier; each needs a new angle before it is tried
again.

- Running JS-to-JS calls inline in the interpreter's loop (+3.7 to 5%;
  Crypto +10 to 14%).
- Closure-threaded dispatch instead of the switch; a separate hot-loop
  function; register-form instructions (Crypto +19%); renumbering the hot
  opcodes; a wider instruction.
- Small integers tagged as integers in a Value: Go's register classes make
  it a trade between integer and float code, not a win.
- Fusing `push_this` and `get_prop` (twice); operand fusion and a fused
  `o.m()` in the frameless evaluator; constant `objectBits`; `isTag` as one
  compare; equality fast paths at the top of `StrictEquals`.
- A `.length` node and a fused `obj.k op=` read in the tree tier: their
  code's placement cost Crypto 2.5% or NavierStokes 8%.
- Leaner objects (values only, keys in the layout); GOGC tuning; PGO, since
  the engine is a library.
