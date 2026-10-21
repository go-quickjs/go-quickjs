# Performance plan

This is the list of optimizations found by a review of the whole engine in
October 2026, after v0.16.12, and the state of each. The review read the
code area by area -- the interpreter and call path, the tree tier, objects and
property access, the built-ins, the compiler, and allocation -- and tied each
item to the code that costs. None of the estimates below was measured when
the list was written; each item is measured as it is done, and its row says
what came of it. Every item has been taken up. The rounds after the review,
and what is still open, are in [After the review](#after-the-review).

When the review was written, go-quickjs was level with C QuickJS over the V8
suite and took about 1.4 times its time over QuickJS's own micro-benchmarks.
At v0.27.0 it takes 0.60 times QuickJS's time over the V8 suite, 0.93 times
by the geometric mean of the micro-benchmarks and 0.97 times over
`microbench_extra.js`; see
[benchmarks.md](benchmarks.md#compared-with-c-quickjs). The figures in an
item are from the runs of its time unless it says otherwise.

## How an item is measured

- Code placement moves the V8 suite's figures by a few per cent, and single
  benchmarks by up to a tenth, with no change in the code that runs. The
  cause is AMD Zen 2's op cache, which holds the decoded code at one offset
  in every 4 KB in one set of eight: hot code that lands on the same sets is
  decoded again on every pass. A change is compared with its parent by
  `internal/cmd/v8bench/placements` (see AGENTS.md): eight builds of each
  side, placing `executeAt`, the call path, `runTree` and the tree's
  closures differently in a balanced design, each placement's fastest round
  taken, and the short benchmarks run 300 times a process. `-parallel -k 4`
  screens a change in minutes; a batch is confirmed with all eight, over
  the V8 suite, Octane and Are We Fast Yet.
- Before measuring, the interpreter's hot functions are checked to be
  unchanged (`internal/cmd/hotdiff`) and the sizes of package vm's closures
  compared (`go tool nm -size`): a closure that grows moves every closure
  after it, and has cost Crypto and NavierStokes up to 11% on its own. Four
  placements keep the closures' phase fixed, so a change to their sizes
  needs all eight.
- A micro-benchmark is timed on the paths the change touches, against its
  parent, before the suite is.
- Allocation counts are deterministic, and are compared exactly. A change
  that moves what is allocated is also compared by CPU time, which the fixed
  mode prints beside each benchmark (`cpu`): the collector runs on other
  threads, which hides its cost from the wall time of an idle machine but
  not from an embedder running a runtime on each core.
- JIT code has the same op-cache limit: one AWFY benchmark, all of it
  native, runs 495 or 600 ms depending only on where its code lands (81%
  against 43% of its operations from the op cache). `placements -jit`
  varies where the JIT places its first code (`V8BENCH_JITPAD`), but only
  its first 64 KB chunk: code compiled later, a benchmark's late in a
  suite, lands where the code before it ends, at the same place in every
  placement, and neither is the JIT's Go side moved. A change that resizes
  earlier code can then look like an eight-placement regression of a later
  benchmark. Before calling one real, compare its native code's bytes per
  operation, its exits and compiles, run it alone (`-suite`) over the
  placements and at several first offsets, and count its retired
  instructions (AMDuProfPcm): Towers' +7.5% and the V8 suite's +1.7% at
  `d4e1855` were the same code, byte for byte.
- Ideas already measured and rejected are listed at the end, so they are not
  tried again without a new angle.

## A. Largest expected effect

| ID | Item | Targets | Risk | Status |
|---|---|---|---|---|
| A1 | `dup` without a spill for property updates | `prop_update` 2.6x, `array_update` 2.8x | Low | Done |
| A2 | Strict global writes without the separate check | `global_write_strict` 3.4x | Low | Done |
| A3 | A cheaper call path, outside the interpreter's switch | `func_call` 1.35x, DeltaBlue, Richards | Low | Done, in part |
| A4 | Cheaper calls from trees, then no call-density gate | EarleyBoyer, RayTrace, Richards | High | Done |
| A5 | Fewer allocations for objects and strings | Splay, DeltaBlue, RayTrace | Medium | Done, in part |

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
- A call is three Go frames -- `callDirect`, `runFD`, `executeAt` -- with
  nine argument registers spilled and reloaded between the first two. A
  `runFD` specialized for `callDirect`'s direct path drops one.
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
- Declined: a specialized `runFD` for `callDirect`. It would copy the
  hottest call code, about 150 lines with its tail-call loop, to save a
  Go frame on calls the frameless evaluator does not already answer, which
  are the minority of the suite's hot calls.
- Done later (2026-10-05, `0820228`), in a smaller form: `callTree`, about
  60 lines without the tail-call loop, for a function whose call needs
  nothing but the frame, which `planTreeCall` decides once and keeps in
  `funcData.treeCall`; `callDirect` asks that first. V8 suite over eight
  placements: total -1.4%, Crypto -2.8%, EarleyBoyer -2.9%, Splay -2.1%,
  Richards and DeltaBlue -1.7%. Testing it in the tree's call nodes as
  well cost DeltaBlue 4 to 6%: its unplanned, mostly pure callees then
  found their funcData twice.

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

**Done, in part.** Four of the five:

- A constructor's root layout records the room its objects came to need
  (`noteGrowth`, up to eight properties), and `new` gives the next ones
  that room. Splay -5%; RayTrace's objects grow by 1.1 MB over a run.
- Strings of one ASCII character and of `undefined`, `null`, `false` and
  `true` are the runtime's own, made once; a number's text is written into
  its String's own allocation.
- A join of at most 72 bytes is one allocation (`joinShort`): template
  literals about 8% faster.
- A String holds its code units as a pointer: 72 bytes to 56, and every
  string a size class smaller.

Allocated bytes over the suite -4.0%, allocations -1.8%. V8 suite over eight
placements: total -1.1%, Splay -5.1%, RegExp -2.2%; DeltaBlue +2% at most
placements, in every variant measured, with or without the constructor's
room -- it does almost no string work, and is the benchmark placement moves
most. Not done then: compiling `"lit" + x + "lit"` to one `OpConcat`.

**Then (2026-10-12, `152bea0`):** an addition chain in which a string literal
has made every addition from there on a concatenation compiles to one
`concat` of its parts, each operand after the literal converted as it is
evaluated, by adding it to the empty string, so the order of conversions is
unchanged. It needs no new instruction: both tiers already ran `concat` for
template literals, and the JIT makes it by a call of Go. Splay 1.34M to
1.08M allocations a run (-19%).

## B. Specific gaps

| ID | Item | Targets | Risk | Status |
|---|---|---|---|---|
| B1 | `.length` fast path in the existing nodes | `array_length_read` 2.9x | Medium | Done |
| B2 | Element reads and writes in trees without a call | `array_read` 1.85x, `array_write` 1.6x | Medium | Declined |
| B3 | Typed-array elements without the generic path | `typed_array_read` 2.1x, `typed_array_write` 1.8x | Medium | Done, in part |
| B4 | Appending to an array, and `arr.length = n`, directly | `array_prop_create` 2.8x, `array_hole_length_decr` 1.7x | Low | Done |
| B5 | No new layout on every `delete` | `prop_delete` 2.1x | High | Done |
| B6 | RegExp per-call costs | `regexp_ascii` 2.2x, `regexp_utf16` 2.6x, V8 RegExp | Low | Done |
| B7 | Number text without intermediate strings | `float_toExponential` 2.1x, `float_toPrecision` 1.9x | Low | Done |
| B8 | Strings to numbers, and numbers to cached strings | `string_to_int` 1.28x, `int_to_string` 1.6x | Low | Done |
| B9 | Date strings without `fmt.Sprintf` (go-intl) | `date_parse` 2.1x | Low | Done |
| B10 | Compiler: smaller code for common shapes | interpreted code (RegExp's runBlocks, EarleyBoyer) | Medium | Done, in part |

- **B1.** `get_length` reaches `arrayLength` through `getValueProp`, `getProp`
  and `getExoticNamed`; an array or string answers inline before `c.at`, as
  `pureCallAt` and `leafCall` already do. This is a fast path inside the
  existing node: a separate `.length` node was rejected, for what its code's
  placement cost. **Done** in the tree tier, with a variant for a local
  operand: `array_length_read` 22.7 to 11.0 ns (-52%), `string_length` 17.3
  to 10.8 ns (-38%); the V8 suite level over eight placements, and the long
  Crypto and NavierStokes check level. The interpreter's case is left, its
  code being the placement lottery and the hot code now in trees.
- **B2.** `elemAt` and `setElem` (`vm_tree.go`) are not inlinable, so every
  element read and write in a tree is a direct call; their bodies written out
  in the local-operand closures save it. **Declined.** They cost 94 and 93
  against Go's budget of 80, and written with early returns no less: the
  two type tests alone are 22. Writing them out in the tree builder's
  closures adds code where its placement has cost Crypto and NavierStokes
  before, to save one direct call, a nanosecond or two. `isHole` is now one
  comparison of the value's bits with the hole's, which every hole test
  gains by. They stay calls; later rounds made them leaner (`5586641`: no
  number test, early returns) and gave them typed arrays' elements inline
  (`3fe6d9f`: Octane -4.4%), keeping them frameless leaves: a call added to
  either gives it a frame and a stack check that every element access pays.
- **B3.** A typed array's element goes through `getIndexSlow`,
  `typedElemIndex`, two interface assertions and the element-size table. A
  view of fixed length can keep its buffer, element shift and end, which a
  detach shows by emptying the buffer. **Done, in part:** the tree tier's
  slow paths read a typed array's element straight from it (reading one
  can neither throw nor run code) and write one without `setIndexed`:
  `typed_array_read` -13 to -17%, `typed_array_write` -9 to -12%. Keeping
  the buffer and bounds in the view was left: every place that makes a
  view would have to keep them. **Then** done: a view of a fixed length
  keeps its buffer's storage and end from when it is made (`9442ec8`),
  which a detached buffer's empty bytes fail, and the tree tier reads and
  writes elements from them (`a12fd25`). A view that tracks a resizable
  buffer's length still has none.
- **B4.** Appending at `length` tries `setElem`, then `setIndexed`, then
  `appendElem`, which walks the prototypes each time; try `appendElem` first.
  `arr.length = n` on a dense, writable array with a Number that is its own
  uint32 can set it directly instead of through `defineArrayLength`.
  **Done:** `array_length_decr` -38%, `array_hole_length_decr` -22%,
  `array_push` -8%; `array_prop_create` is level, its time the array's
  growth.
- **B5.** Every `delete` allocates a new unique layout. A dictionary-mode
  layout that the caches refuse to fill would be changed in place, as V8's
  is after a delete. Every cache fill must check it: the risk.

  **Done**, the other way round: a unique layout is marked when a cache
  keeps it, which the five places that keep one do, and one no cache has
  kept changes in place; a kept one is replaced as before. An object
  leaving a shared layout copies its index rather than building one.
  `prop_delete` -21%; the V8 suite -0.6%, nothing slower than noise.
  test262 built with the caches' verification: 99,595 passed, 0 failed.
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

  **Done, in part:** a pattern that is nothing but characters (no group,
  class, folding or unicode flag) is answered by the literal search, before
  a matcher is borrowed; a RegExp literal finds its pattern by its constant,
  in a small table of the runtime's, and the tree tier builds it, so a
  function with one is no longer kept in the interpreter; `test()` asks for
  the flag bits. `regexp_ascii` -27%, `regexp_utf16` -23%, `regexp_replace`
  -16%; the V8 suite over eight placements, with B3 and B4: RegExp -8.0%,
  NavierStokes -3.8%, EarleyBoyer +2.0%, total -1.7%. Building RegExp
  literals in trees showed a compiler bug, fixed on its own: four
  instructions that push a value were counted as pushing none, so a frame
  could be a slot short.

  **Then:** `lastIndex` is read and written at its slot; a global match of
  a RegExp whose exec is the built-in collects the matches as replace does,
  without an array for each; split searches with the RegExp's own pattern
  where making the sticky copy would run nothing a script wrote; replace is
  given the flags `replaceDirect` read. A global `match` -64%, `split` -24%
  to -39%, `replace` -6% to -10%, `exec` -10%; the V8 suite over eight
  placements: RegExp -7.8%, EarleyBoyer -2.0%, Richards -1.8%, total -2.1%.
  A run of literal characters inside a pattern as one instruction is left.
- **B7.** `toExponential` and `toPrecision` make four to six allocations;
  written into one buffer, as `toFixed` is, they make one.
  **Done:** the digits are appended to a buffer on the stack, the point,
  the zeros and the exponent put in place around them, and the string made
  once. `toExponential(2)` -42%, `toExponential()` -33%, `toPrecision(4)`
  -34%, `toPrecision(12)` -37%; 207,785 answers the same as before and as
  Node's.
- **B8.** `ToNumber` of `"12345"` trims by Unicode and calls `ParseFloat`;
  plain digits can be read directly. `n + ""` builds a new string even for
  the integers below 1024 a runtime keeps.

  **Done:** up to 15 digits, signed or not, are read before anything is
  trimmed; `n + ""` is the number's string, the runtime's below 1024.
  `string_to_int` -68%, `int_to_string` -29%.
- **B9.** go-intl formats every date string with nested `fmt.Sprintf`, and
  `Date.parse` copies its argument to UTF-16; both are changed in go-intl,
  and taken with `go get`.

  **Done:** go-intl v0.3.2 writes Date's five strings into one buffer
  (`toString` -60%, `toISOString` -57%), and `Date.parse` widens a short
  ASCII string into the runtime's buffer rather than a copy the string
  keeps (-7%). `date_parse` -43%.
- **B10.** Compiler:
  - a call whose result is dropped can say so, instead of a `drop` after
    it (1,229 such pairs in the V8 RegExp benchmark's interpreted code);
  - a comparison with a constant can jump without pushing it;
  - a literal statement such as `"use strict";` can emit nothing;
  - calls to a function known to run frameless need not count against
    the tree tier's gate (`func_call` itself is refused today).

  **Done, in part:** a literal statement emits nothing. The gate went
  with A4, and with it the interpreter's share of the suite: a call and
  its drop are already one statement in a tree, and a comparison with a
  constant one node, so the first two would serve the little code trees
  refuse. Left.

## C. Modern code

These do not move the V8 suite, which is ES5 and has no strict code, but
matter to hosts running classes, modules and async code.

| ID | Item | Status |
|---|---|---|
| C1 | Strict `return f()` (a tail call) takes the generic call path, and keeps its function out of the tree tier and the frameless evaluator | Done |
| C2 | An `await` makes about nine allocations: two functions, a promise nothing can reach, a job, an argument list | Done |
| C3 | A generator or async call makes four allocations, and resuming copies its whole frame | Done |
| C4 | A bound function always takes the slow call path, and allocates its arguments when it has bound ones | Done |
| C5 | Every `for-of`, spread and array destructuring allocates an iterator object | Done |
| C6 | An arrow function is 448 to 480 bytes, most of it fields only bound or `with` functions use | Done |
| C7 | A closure reads a `let` or `const` it captured after its initialization with a dead-zone check, which keeps arrows out of the frameless evaluator | Done |

**C1, done:** the tree tier builds `tail_call`, giving the frame up as the
interpreter does once the stack is 32 deep; a plain tail call is a call in
the frameless evaluator. A method's tail call stays out of the frameless
evaluator, and the interpreter's own `tail_call` is as it was: either change
moved Crypto by 2 to 4%, which runs neither. The builder is in the package's
last file, as inside `op` it moved NavierStokes by 6 to 10%. Strict
`return f()` -45%, a method's -8%, a deep chain of them -9%. Later, class
methods and arrows calling in tail position were given `callTree` too
(`05aedb9`); a tail call's arguments go into a buffer the runtime keeps
rather than a slice of their own (`3291414`: `tail_self` 129 to 83 ns); and
a function calling itself in tail position starts again in its own frame
(`6099e8c`). Two functions calling each other in tail position still make a
frame for each call (`tail_mutual` 2.7 times QuickJS's time).

**C2, done:** an await's reaction names the generator it resumes and how,
and its job resumes it: no functions, no promise for nothing to read. A
plain `await` -47% to -52%, `for await` over an async generator -40%.

C1 and C2 together, against main over eight placements: total +0.7%,
Crypto +1.6% to +1.8%. Neither runs in the suite: C2 alone, whose code the
suite never reaches, moved Crypto +2.1%, and C1's variants moved it between
+1.6% and +6.1% with no relation to what they run. Taken as placement, as
A5's DeltaBlue was.

**C3, done in part:** a generator and its object are one allocation, its
arguments and locals another, and its window is no longer cleared at each
suspension -- a returning call leaves its own for `endTurn`. A generator
made and run -10%, an async call -7%, a yield -3%, a spread of one -4%.
Taking the suspension's signal from the runtime instead of allocating it
made a yield 14% faster, but is a change to `executeAt`, which cost Crypto
4%: left. **Then** done for trees, without touching `executeAt` (`54e2794`):
a tree's yield or await writes the runtime's one signal, and a resumption
builds its frame in place and returns through fewer functions; a for-of over
a generator went from about 2,300 instructions a value to a fraction of
that. `27b720c` kept a resumed loop a Go loop and stepped `yield*` over a
generator directly, and made a small generator one allocation of 184 bytes.

**C4, done:** `callObject` hands a bound function's call to `callBound`,
which merges its arguments on the runtime's stack and calls the target the
way `callDirect` does. Bound calls -16% to -40%, `map` with a bound
callback -32%. It was first declined: single builds and the first
eight-placement harness put Crypto 7 to 11% slower. That harness moved the
whole package, so the distances between `executeAt`, the call path,
`runTree` and the tree's closures were the change's in every placement. A
harness that also sets each of those four at 0 and 32 mod 64 in a balanced
design, at distances varying by up to 2 KB, measured it at +0.1% (Crypto
+0.5%, +0.8% over twelve rounds). Main's own Crypto spans 284 to 325 over
those placements.

**C5, done:** a cursor and its object are one allocation. A for-of over a
small array -15%, array destructuring -14%, a for-in -8%.

**C6, done (2026-10-15):** an arrow is one allocation -- object, data,
closure, captures and its `funcExtra` -- and what was left was its size.
Moving the bound-function fields alone out of `funcExtra` changed
`callObject` for a size class, 480 to 448 bytes, with time level, and was
dropped. Instead the rarely used fields went into one pointer
(`funcExtra.rare`) and a function's length became an `int32` (`a37ea45`:
480 to 384 bytes), and a template's closures share one `closureCode` of
names, constants and caches (`196118f`: 320 or 352 bytes).

**C7, done:** a function is compiled where it is made, so a `let` or
`const` its maker has initialized by then is read without a dead-zone
check, as the maker's own reads are; a hoisted declaration is made at its
block's start, before the block's bindings, and keeps the check. An arrow
reading such a binding may then be pure: `a.map(x => x * k)` -27%, a sort
comparator -28%, a call -26%.

## D. V8's model for objects and tiering

Two of our designs differ from V8's where real programs meet them all the
time, and each heuristic built on them has been a patch on a symptom. Found
on Octane's Gameboy (2026-10-17) with a CPU profile whose samples are mapped
to the JIT code holding them (every placement logged, freed code never
reused):

- Its emulator object has 215 properties. A layout shared by the objects
  built alike holds at most 64 (`maxShapeDepth`), so each run's emulator has
  a shape of its own, and every cache meets a new shape at every run: some
  twenty reads of the instruction dispatch leave native code at each run,
  and the code is compiled again for each.
- Go going on in the interpreter after an exit, at a PC with no entry, had
  the code compiled with entries in the middle of the loop's body, first
  after every guard, then at each PC Go went on at (learned entries). Each
  entry is a join where what the loop proved before holds no longer: the
  dispatch's native code ran 40% longer with 46 of them.
- What a read met settles into a compile only after exits that meet
  nothing new; a read that met a shape is served by the megamorphic cache
  natively after its one exit, so the code may make no exit to settle on.

What V8 does instead, from its source (`src/objects`, `src/execution`):

- **Maps.** Objects that add the same named properties in the same order
  share a map, up to 1,020 descriptors (`kMaxNumberOfDescriptors`), the
  maps along a transition chain sharing one descriptor array. Only keyed
  stores (`StoreOrigin::kMaybeKeyed`) beyond `fast_properties_soft_limit`
  out-of-object fields, and never a prototype's, send an object to
  dictionary mode (`Map::TooManyFastProperties`). Dictionary-mode objects
  share their maps through `NormalizedMapCache` -- "a typical program needs
  very limited number of distinct normalized maps" -- so caches stay
  monomorphic on them.
- **Prototypes.** A prototype map's validity cell guards every prototype
  chain through it: a load found on a prototype checks one cell, which any
  change to a prototype on the chain invalidates, however long the chain.
- **Entries.** Optimized code is entered at a function's start or by
  on-stack replacement at a loop's back edge, from a lower-tier frame
  running a long loop. A deopt hands the frame to the interpreter for the
  rest of the invocation; nothing enters optimized code in a loop's body.
- **Feedback.** Feedback lives in the lower tiers' inline caches. A change
  of one delays tier-up until a fresh interrupt budget -- at least
  `minimum_invocations_after_ic_update` (500) invocations' worth, the
  budget being invocations times bytecode length -- is spent in the lower
  tiers (`TieringManager::NotifyICChanged`): feedback settles where it is
  gathered, not at exits.

| ID | Item | Status |
|---|---|---|
| D1 | Transition chains past 64 properties, up to V8's 1,020, the shapes along a chain sharing one keys array and one index (`findOwn` already checks an index entry against the object's own table) | Done (`ca95576`) |
| D2 | Dictionary mode as V8's: keyed additions past a soft limit and deletes turn an object's table into its own, under a shape shared by the dictionary objects of its prototype, so caches stay monomorphic on them | Done (`23356b3`): the last key's delete rolls back, attribute changes are shared transitions, keyed adds past 12 make a dictionary, and dictionary objects share one cache identity (`dictShape`) |
| D3 | Prototype validity cells: a read found on a prototype checks one cell, not each holder's shape (today two, then the deep-read cases) | Done (`82dc853`) for reads past two prototypes and of absent properties; reads one or two up keep their holder checks, which native code is compiled for |
| D4 | Slack from the constructor's `this.x =` count, as V8's expected properties, refined over its first objects | Done (`a88e64e`): counted up to 252, tracked over the first seven objects |
| D5 | JIT entries at a function's start and its loops' headers only; a deopt leaves the rest of the invocation to the interpreter and the tree tier, which go on natively by OSR at a loop. Replaces `allEntries` and the learned entries | Done (`2919ad8`); a frame Go finishes for a native call goes on at a loop's header too |
| D6 | A shape native code was not compiled for deopts, as V8's map checks do; a read is megamorphic in native code only where its cache is | Done (`ac60a85`); a read whose cache is empty scans the receiver's own small table (`Scan`), and a read remembered by a chain's cell or as dictionaries is megamorphic (`0430bf0`) |
| D7 | Compiled again on the lower tiers' budget, reset when a cache's feedback changes, as V8's tier-up; replaces the settling on exits, the ticks of Go's work and the profit check's stretches | Done (`09301d0`, `dd6bb7b`, `e2d731e`, `3979bad`): only an eager deopt invalidates; first compile and compile again on V8's budgets, 400 invocations times the bytecode's length, 500 after a cache changes; the profit check kept, code it judges slow still entered by OSR at a long loop (`dddc1d4`) |

The order is D1 and D2 first: they remove the churn of feedback at its
source and help the interpreter and the tree tier as much as the JIT. Then
D3, then D5 to D7 together, which depend on one another. Each is measured
as above, without and with the JIT, against the baseline: `79c9eee`,
which has the learned entries and the settling at invocations that D5
and D7 replace. With them, against the commit
before them, Octane with the JIT took 4.2% less time (Typescript -10.2%,
Box2D -6.0%) and Gameboy alone 1.9% more.

Measured, all seven with what followed them (`87da7ab`), against the
baseline (`4aa3fa7`), `placements -parallel -k 8`: without the JIT,
Octane -0.4% (Typescript -3.6%, Box2D +2.0%), AWFY -0.2%, V8 0.0%; with
the JIT, Octane -2.2% (Gameboy -37.6%, PdfJS -35.0%, CodeLoad -22.5%,
RegExp -17.7%, Splay -7.4%, Typescript -2.7%; Box2D +14.2%, RayTrace
+10.4%, Mandreel +2.5%), AWFY -1.8% (CD -5.5%, Havlak -3.7%; Json
+5.3%, Sieve +5.3%, Storage +2.2%), V8 +0.1% (DeltaBlue +21.7%, 1.8 ms
to 2.1). Two runs a benchmark, warm-up included, had had Box2D 22%
faster: its loss is its steady state's.

### Found while doing D5 to D7, left for later

- **The interpreter's caches hold one shape.** Each refill of a site's
  cache counts as a fill, so a site that the lower tiers run with two
  shapes in turn gives up on its cache (`maxCacheFills`, 16) after 16
  alternations and reads through the runtime's shared cache, where V8's
  inline caches hold four maps. Done since: a site keeps the entries of up
  to three shapes besides its own (`propCache.poly`), which every tier's
  reads and writes answer from, the frameless calls' and the tree's own
  table scans included (`ownScan`). Left: the JIT's record of the shapes
  a site met (`propSeen`) still keeps entries of its own beside them,
  where one record would do.
  Measured, one build of each against `573dae7` (a quick check, not
  placements): without the JIT DeltaBlue 63% faster on Octane and 54% on
  the V8 suite, Box2D, RayTrace and Splay about 10%; with it Octane 3.4%
  faster in all, Crypto and Richards faster. What the JIT decides changed
  where a site its code had taken megamorphic is polymorphic now:
  DeltaBlue's `Plan.prototype.execute`, whose `c.execute()` has several
  targets, leaves native code at each call (a call without a native
  target) and is judged slow, `ScaleConstraint`'s `execute` never
  entered; Box2D leaves native code 11 times rather than 68, but its
  small `set` methods are judged slow; RayTrace deoptimizes 4 times
  rather than none. A polymorphic call, V8's check of each target, is
  what the first needs.
- **`get_prop` in `executeAt` never fills a small table's cache**: it reads
  a plain own property of up to `linearScanLimit` keys directly
  (`plainOwn`), which is as quick as a hit. The JIT therefore learns such
  a site at its first deoptimization (`jitDeoptSeen`), and a site whose
  cache is empty, though it may have run, scans the receiver's own small
  table natively and deoptimizes only where that fails
  (`ssa.PropertySite.Scan`) -- a deviation from D6's rule that a site
  without feedback deoptimizes, which would otherwise cost each such site
  a deoptimization and a budget.
- **The profit check stays** (`jitSSAProfit`, `entrySlow`): it prices Go
  entering and leaving native code, a cost V8's tiers do not have.
- **Host instructions left in hot code**, each ending its block with an
  entry after it where Go resumes the code, which V8 would make a call of
  its runtime:
  - calls without a native target: generic or polymorphic ones, ones of
    more than 16 operands, calls inside an inlined callee, `apply` not
    forwarded;
  - megamorphic property reads and writes;
  - global sites with no feedback;
  - generic `==`, `%` and arithmetic, unary operators and updates;
  - `typeof` not compared with a constant;
  - `instanceof` with a constructor the code does not know;
  - literals without a pool.
- **Making them calls of Go (`ssa.GoCall`)**, as V8's code calls its
  runtime, by what each would take:
  - small: generic calls (a GoCall site offered in `nativeCalls` and the
    VM's `NativeCalls`), megamorphic reads and writes (`jitPropertyMade`
    already makes the generic access), and `typeof`;
  - moderate: `instanceof`, a new GoCall kind, since it may run script;
    generic `==` and `%`;
  - hard: calls inside an inlined callee: a GoCall needs the root frame
    (`b.cur == b.root`), and an inlined level has no frame state to give
    Go.
- **Done since, of the above**: generic calls
  of arrows (`ac6c73e`); code the profit check judges slow entered by OSR
  at a long loop (`dddc1d4`); a megamorphic write's miss a call of Go on
  a branch of its own in the SSA (`87da7ab`), RayTrace's exits at
  `best.hitCount`; the generic operations, each a call of Go from native
  code where it had left: an arithmetic operand that is a string
  (`GoNumber`, `4e1cf4e`, a remainder's included), == and === native
  code does not compare (`GoEqual`, `84e1734`), typeof and an unknown
  instanceof (`1806772`).
- **Still open, of the above:** callees whose lowering for native
  callers refuses a captured local, every call of them made by Go
  (DeltaBlue's `inputsKnown` 64K, `satisfy` 35K, `constraintsConsuming`
  10K a run); calls, and writes, inside inlined callees (below). New:
  RayTrace's small methods (`add`, `multiplyScalar`, `initialize`)
  judged slow early in a short run; Box2D's steady state 14% slower with
  the JIT (above). A megamorphic site's read of an object is a call of Go
  on a branch of its own since (`bbcd42e`); one of a primitive's method,
  whose receiver is no object (`OpObjectOf`), still leaves:
  Typescript's `this.value.toString()` of a number (typescript-compiler.js
  1607), 3,754 exits in five runs, PdfJS's `command.charAt` of a string
  (pdfjs.js 17374), 327 -- a branch on the receiver's kind before the
  cell's search would make it Go's too.
- **Exits inside inlined callees** (2026-10-18, every suite, five runs of
  each, after `bbcd42e`): Typescript's array literals 14,067 -- native
  since (`c06f622`: an inlined callee's literals from pools its inliner
  keeps) -- and its keyed reads, StringHashTable's `this.table[key]`
  (typescript-compiler.js 519, 569), 11,466, which are Go's keyed read
  refusing, as it does in the root, not the inlining; then the calls and
  writes inside them: Typescript's megamorphic write at 754, 3,978, and
  Gameboy's `this.memoryHighWriter[address](...)` (gbemu-part2.js 7916),
  869 -- some 5,000 in all, everything else two or fewer each. Making
  them calls of Go needs two pieces, held for that: the backends to write
  each inlined level's context at a call of Go, as an exit's code writes
  them from its description's inlined frames (`ExitInline`); and Go to
  make those levels' frames for the call, as `jitGoCallNested` makes a
  native call's levels', so that the call's stack trace has them and a
  property's call reads the callee's caches at the callee's PC.
- **Host exits counted by instruction** (2026-10-18, one run of each
  suite with the JIT, after `d3ec102`): of the generic operations above,
  `==` left native code 179 times in all three suites, `typeof` 28, `%`
  and `instanceof` not at all -- not worth their calls of Go yet. The
  most frequent were a for...of loop's step and its `iter_close`, 75,000
  each, all AWFY's Bounce, twice at each loop: the step that finds the
  cursor done compared its answer with the uninitialized value, which
  native `eqtagged` left for Go, and `iter_close` was always Go's. Both
  native since (`1c92b0b`): Bounce leaves native code 1,499 times a run,
  not 146,695. Making `iter_close` native found a bug of the
  interpreter's, fixed since (`3365aa0`): an array walked in place,
  left before its end, closed nothing, where a return method a script
  gave the array iterator's prototypes is to be called; native code
  checks the chain's validity cell for it. Still the most frequent,
  each by its sites' totals: element reads (`get_index`), Typescript's
  29,000 and Gameboy's 12,000; method calls (`call_method`, `call`),
  Typescript's 7,500, Havlak's 5,800, CD's 1,500; constructions
  (`new`), Sieve's 2,100 and Bounce's 1,500.
  Of the element reads since: Typescript's was its token table read by
  names its keyword table answers, `staticTokens["44"]`, which Go's keyed
  read for native code refused -- read as by the index since (`27e49c4`,
  Typescript 66,268 exits to 49,862); Gameboy's were `memoryReadNormal`'s
  typed-array read inlined, compiled as an array's because an inlined
  callee's element feedback was the root's -- its own since (`bffbc73`,
  Gameboy 28,591 to 12,736).
- **Placements of the round since `573dae7`** (to `0c82eb3`: the
  polymorphic caches, for...of, the walked array's close, element reads
  by name and an inlined callee's, arrow callees, the compiler's backend
  steps; eight placements, means): with the JIT Octane -1.3% (Gameboy
  -18.9%, Splay -3.7%, Mandreel -1.8%; Box2D +6.7%, every placement
  slower), AWFY -1.0% (Bounce -9.9%, Sieve -7.5%, CD -3.7%, DeltaBlue
  -3.4%), the V8 suite -0.5% (DeltaBlue -6.9%, Splay -6.0%); without it
  all three level (+0.6%, 0.0%, +1.1%, within the placements' ranges).
  Box2D's loss is the polymorphic caches' (`d3ec102`): its guard failures
  went from 46 to 103 a run of ten, entries from 107,135 to 48,713. Sites
  the interpreter's one-shape caches had given up on, megamorphic in
  native code and never deoptimizing, are polymorphic now, and each
  deoptimizes, once or twice, as its objects take the fields Box2D adds
  after construction, waiting a budget in the tree tier and compiled
  again each time -- V8's model, whose recompiles cost less and run in
  the background. No site met a fifth shape (V8's megamorphic rule,
  tried: no change). What makes it up is the compiler's speed (D's
  follow-up, under way) and the element feedback below.
- **The lower tiers gather no element feedback.** V8's keyed loads and
  stores record the maps and elements kinds they meet in the interpreter
  (KeyedLoadIC), so optimized code is compiled for a typed array from its
  first compile. Ours learn an element's kind only at the code's own
  exits (`jitIndexSeen`): a site the first compile took for an array's
  meets typed arrays at host exits, and the code, learning, runs on
  until a budget restarted by every cache change is spent (`e2d731e`) --
  Gameboy's `executeIteration` read `TICKTable[op]` as an array's for
  3,076 exits a run. The fix is V8's: the interpreter's and the tree
  tier's element reads note the kinds they meet per site, kept where only
  sites that meet more than an array's pay, and the first compile takes
  them. Left for later, with the interpreter's hot paths to measure.
  Tried (2026-10-18, not kept): the tree tier noting, per site, a typed
  array's read, a string's and a read by name, kept on the function's
  warming entry -- which a compile must carry over, `jitFor` making a new
  entry -- and its first compile taking them. A string's and a name's
  are noted in the nodes' slow paths, cheaply; a typed array's element
  is read in `elemAt`, the fast path every array read's node inlines,
  which knows no site, so noting it means a check after `elemAt` in every
  node, the tree tier's hottest code, in builds with the JIT. Before a
  function is warming, too, it has no entry to note in: `r.jit` is made
  at the first compile. Do it with the nodes' layout measured, or give
  `elemAt` the site.
- **Code is not compiled again while a native call of it goes on**
  (`jitReoptimize`'s `otherwiseInUse`). Runs Go entered no longer wait
  (2026-10-19): a recursive function due to be compiled again is, where
  its innermost run learned it, as V8 installs new code while older
  frames are in the old -- a run suspended at a native call whose callee
  Go finishes goes on from its frame, as the code that left describes it
  (`jitGoRun`), and in the new code at its next entry; one suspended in a
  call of Go leaves when the call returns, its code kept till then.
  Waiting for them, Typescript's `AstWalker.walk`, its outer runs in its
  code all the way down, had kept code calling one of its visitors
  natively and left at its call of each other, 145,000 times a run. A
  native call of the code, from another function's, still waits: it
  returns into the code, whose frames Go reads by the entry's code. It
  costs most in code compiled for native callers alone: such a
  function runs only as a native callee, so a compile again is always
  asked for while a native call of it is live, and never made. AWFY
  DeltaBlue's EqualityConstraint and ScaleConstraint learn their super()'s
  parent (jitSuperSeen), are due long past their budget -- spent 190,000
  of 4,000 -- and keep their first code for good, their super() a call of
  Go, BinaryConstraint and AbstractConstraint run by the tree tier under
  them, Strength.of declined 36,000 times a run (2026-10-19, after
  e0e21ca; the marking that had seemed the cause is not: the declines
  happen with the collector idle). Making it means each level's frame
  read by the code it is in, which the level would record.
- **Native code while the collector marks**, as V8's code calls its
  write barrier and goes on. Native code tests Go's write-barrier flag
  before it stores a pointer; while it is set, what stores one is made by
  Go, called from native code (a call of Go), or leaves for Go (an exit).
  The plan, in five stages, each a commit of its own with a regression
  test and a sabotage that fails it:
  1. *Go resumes native callers while marking* (`e0e21ca`): the only
     pointer they store before testing the flag again is the call's
     result, which Go wrote with its barrier.
  2. *Stores through Go's barrier* (`a511e30`): a property's value, an
     element, a keep, an array literal's elements and a property added
     along a transition, its shape too, jump to `storeRef`
     (`entry_*.s`), which calls a nosplit Go function doing `*slot = p`;
     a routine each code shares saves its registers (refStoringCall).
     `TestStoreRefShades` moves objects during a real collection, again
     under `GODEBUG=gccheckmark=1`; `jit.StoreRefClobber` gives every
     register junk across a store.
  3. *Native calls while marking* (2026-10-19): native calls are made
     natively whether the collector marks or not. A call's setup needs
     no barrier -- the records, the callee's context and frame outlive
     none of their sources and overwrite only an earlier call's -- and
     the result, which outlives the callee's frame it comes from, is
     stored through `storeRef` while marking (README, The write barrier).
     Go still makes, while marking, a construction whose receiver comes
     from a pool, a `push` and a `pop`, which
     move a pointer into what outlives the call; they join stage 4.
  4. *Pools while marking* (2026-10-19): native code takes from its
     pools while the collector marks too. A literal's or `new Array(n)`'s
     object has its pool's cell cleared through `storeRef`
     (poolTakeMarking), which shades it; a construction's receiver is
     shaded in its pool before the call takes it (poolShade); a closure's
     captured cells and an arrow's this, an entry's cells' arguments, a
     pushed value and a popped one go through `storeRef` while marking;
     a loop's step needs no barrier -- the element it copies stays in its
     array. Left for stage 5: ssa.GoTake, markingCall, markingTake's
     Go path and abi.GoOpMarking, which code with no `storeRef` (the
     harness) still uses.
  5. *The rest, and cleanup* (2026-10-19): a call in tail position
     shades its operands, read into GoArgs before the frame is written
     over, through `storeRef` into the context's Shade while marking,
     where it had left native code; `storeRef` is required (mir refuses
     an encoding without it), and what code without it needed goes:
     ssa.GoStore, GoTake and GoAddAlong, markingCall, markingExit,
     abi.GoOpMarking and ExitMarking, the VM's jitMarkingCallee and
     goCallMarking, and its jitMarking() exceptions in jitCalleeProfit,
     jitInlineLeft and jitUnwound, which marking no longer gives cause
     for. An exit's records are still Go's to apply while marking
     (recordsTail): staged in GoArgs as tail calls are, at an exit, where
     Go is called anyway.
  Stages 3 to 5 had waited (counted 2026-10-19 over one AWFY run, the
  calls of Go made only because the collector marked were 12,000 in
  DeltaBlue, 21,000 in CD, 50,000 in Storage and 65,000 in Havlak, a
  tenth or less of the calls of Go that were not the collector's, below,
  which were done first); they were done the same day.
  `jitMarkingForTest`, a set byte native code reads as the flag, is
  enough to choose the paths in tests; real marking needs a collection
  running (TestStoreRefShades's way).
- **Calls of Go that are not the collector's.** Counted by operation
  over one AWFY run (2026-10-19, native stores while marking), the
  largest are element stores native code leaves to Go (ssa.GoElemStore):
  355,000 in DeltaBlue, 3.6 million in Havlak, 5.5 million in Storage,
  which fills `new Array(n)`'s holes; then cells Go makes for captured
  locals (ssa.GoCells), 260,000 in DeltaBlue and 2.0 million in Havlak,
  and Havlak's closures (GoClosure 446,000, GoClose 559,000). V8 stores
  into a holey array's hole and allocates its contexts in its code. The
  calls Go makes only because the collector marks are a tenth of these
  or less: DeltaBlue 12,000, CD 21,000, Havlak 65,000, Storage 50,000
  (native code while the collector marks, stages 3 and 4, above). The element stores were all into holes inside
  the length; native code makes them now (mir's storeCell, Storage -20%,
  Havlak -5%), and native callees take their cells from their function's
  pool (mir's cellsTake, Havlak -5%, NBody -4%, DeltaBlue -2.5%).
  Havlak's closures since: findSet's arrow, which captures only its
  maker's this, is taken from its pool in code with no cells of its own
  too (a095f71: GoClosure 439,404 -> 19,693 a run, Havlak -3.2% in a
  fast check), and a scope's end gives its locals fresh cells natively,
  on the frame Go entered too (mir's cellsClose, 88fe84d: findLoops'
  GoClose 565,844 -> 4,356, level in time). Found on the way: a plain call
  of a function that reads its this was never made natively, its code
  compiled for native callers never run (763e2d4 makes it, with the
  global object or undefined as its this). Of AWFY's calls of Go, the
  largest left are Json's 2.7 million GoCalls, NBody's 250,000 and
  Bounce's 150,000, to be broken down by what they call.
- **The `in` operator** (found 2026-10-19 by a test): a function with
  `k in o` anywhere was refused whole. Now compiled, each `in` a call of
  Go (ssa.GoHas) that answers an object's own or inherited property, no
  getter called, and leaves for the interpreter a proxy's trap, an object
  key's conversion and a TypeError. Of the 46 functions it had refused in
  Octane, the V8 suite and AWFY (pdf.js 34, Mandreel 7, EarleyBoyer 5),
  28 compile now, 14 are left to native callers and inlining (no loop),
  and 4 stopped at `delete` (below, compiled since). Next, natively, as V8's keyed has: an
  array's dense
  element that is no hole, needing no feedback (a split, true on one side
  and the call of Go on the other, as megamorphic reads split); then an
  object's constant key by shapes learned at the call of Go itself, the
  interpreter keeping no cache for `in` (which would change executeAt).
- **The `delete` operator** (`delete_prop`): a function with `delete o.k`
  or `delete o[k]` was refused whole -- 4 of the functions `in` had
  refused stopped at it. Now compiled, as V8 first does it, each `delete`
  a call of Go (ssa.GoDelete, GoDeleteStrict) that deletes what deletes
  without running code -- an ordinary object's own property, configurable
  or not (false in sloppy code), its shape changed as the interpreter
  changes it, a dense array's element, an absent property -- and leaves
  for the interpreter, having changed nothing, a proxy's deleteProperty
  trap, a module namespace, typed arrays, string wrappers, mapped
  arguments objects, an object key's conversion, a base that is no object
  and, in strict code, a property that cannot be deleted (the TypeError).
  The call clears the deleted property's cell, so it is a store of any key
  to the analyses (ssa's allocOnly): a value read from that property and
  used after the delete is kept first, which a test that read the values
  and deleted their properties caught, the references read back from the
  cleared cells. Of the 25 functions it had refused in Octane, the V8
  suite and AWFY (pdf.js 10, EarleyBoyer 6 in each suite, TypeScript 2,
  Box2D 1), 16 compile now and 9 are left to native callers and inlining
  (no loop); the whole corpus 3507 -> 3523 functions eligible. `delete x`
  of a global (`delete_var`, only zlib's teardown) and `delete super.x`
  are still refused. Next, natively, the deletion of an object's
  last-added property back along its shape's transition, as V8's code
  deletes its map's last property (the VM's deleteOwn does so for a shared
  layout already): it needs feedback the opcode has no site for -- the
  receivers' shapes and the key, learned at the call of Go itself, keyed
  by pc as keyed reads are -- a constant key (`delete o.k`'s push_const),
  and in mir for both architectures the shape guard, the cell cleared and
  the parent shape stored, with the write barrier while the collector
  marks, as a transition's store makes them the other way. Worth it only
  where deletes are hot, to be counted first (calls of Go by operation, as
  above). Found on the way, not the JIT's, and fixed since: the
  interpreter and the tree tier answered true for any primitive base,
  where `delete 'ab'.length` and `delete 'ab'[0]` are false in sloppy code
  and throw in strict (ToObject's String exotic object), as Node answers.
- **Array.prototype.forEach as a loop in the caller's code** (2026-10-19,
  the user's choice, as TurboFan's JSCallReducer reduces it). AWFY
  NBody's 247,000 forEach calls a run are calls of Go, whose callbacks Go
  runs in the tree tier: by a profile, Go's forEach is 35% of NBody's
  time, the callbacks' property updates in the tree tier 10%. The plan:
  - The VM learns a call of the realm's forEach on a plain array, and its
    callback's closure, at the call of Go (as push's), and gives the
    builder a ForEach site: the function's address and a native call of
    the callback (by its code, a closure made anew at each call of its
    maker, as NBody's arrows are).
  - The builder makes the call a loop of its own blocks: the function and
    the array checked, the length read once, then for each index below
    it, the element read where the array has it -- a hole, or one past
    the array's length now, skipped, the prototypes holding no index
    (arrayGuards) -- and the callback called natively with it, its index
    and the array, this the second argument or undefined.
  - Its state is a frame of its own, as an inlined callee's is
    (InlineState): no closure, the function object its callee, five
    locals -- the array, the callback, this, the index, the length -- and
    a PC saying where in it an exit is: before the callback of the index,
    or in its call. An exit inside -- a check of the loop failing, or the
    callback leaving native code where its caller cannot go on -- writes
    it as it writes an inlined callee's, and Go finishes it as V8's
    builtin continuation frames finish the builtin: its forEach run from
    the index, or the one after, then the caller from after the call. A
    stack trace shows forEach's native frame there, as Go's forEach
    shows it.
  - Then the callback inlined into the loop, which needs inlining a
    closure that captures bindings, refused now; then map, filter, some,
    every, find, findIndex and reduce, each a loop of the same kind.
  Done in 3192b5a, but for a frame of its own: the loop's slots are the
  caller's frame's, past its operands, and a word of its context
  (abi.Context's Loop) says one is under way, which Go reads wherever it
  finishes that frame -- a frame kind of its own would have had every
  native call inside it write its context and every walk of the levels
  learn it. NBody 769 -> 406 ms in a fast check, its forEach's calls of
  Go 247,075 -> 29 a run; array_forEach (micro) 36.8 -> 26.1 ns an
  element, the callback's call most of what is left (array_for_of, 7.6).
  Typed arrays' forEach, %TypedArray%.prototype's, is Go's still.
  reduce done in 5218aa7, the same loop with the accumulator in this's
  slot, its callback's result the next one: array_reduce (micro) 42.5 ->
  28.0 ns an element. map, filter, some, every, find and findIndex done
  in 7e02bd5, loops of the same kind: map's and filter's array made by Go
  once where the array's species is Array's (GoNewArray), the results
  appended natively -- any value's store at an array's length appended
  where it has room, as a number's was (19dfd1b) -- and some's and the
  others' loops left at the first answer; a loop's slots hold the
  element and the callback's result too, which Go goes on from after an
  exit past the call. array_map (micro) 39.5 -> 34.2 ns an element
  (-18% at each of four JIT code offsets), array_filter 60.6 -> 39.2,
  array_some 48.6 -> 25.8. map's and filter's stores into their own
  array then appended with no guard but its room (e0abf9c,
  ssa.StoreFresh), defined whatever Array.prototype holds: array_map
  33.2 -> 28.2, array_filter 44.1 -> 38.7. A typed array's forEach,
  reduce, some, every, find and findIndex loops too (9236a08), its type's
  prototype checked, its elements read as numbers: typed_forEach 115 ->
  30 ns an element; map and filter, whose result their species makes, are
  Go's still.
  - Found on the way: a typed array's method read, t.forEach, is a call of
    Go at every call: typed arrays have no shapes (shapeClass), so no
    cache remembers what a name finds on them, in the interpreter, the
    tree tier or native code. Their names but the canonical numeric
    strings are an ordinary object's; a shape of their own, whose cache
    leaves those strings out, would read the method natively.
  - Still to do: the callback inlined into the loop. An inlined callee's
    captured bindings are Go's now (an exit); NBody's arrows read and
    write their maker's locals, which the loop's code has, as V8
    specializes an inlined closure to its known context. An exit inside
    the inlined callback needs its closure, made anew at each call, in a
    slot of its frame.
- **Octane's PdfJS +2.4% since 5a2bc9d** (2026-10-19, eight placements
  with the JIT, all eight higher: 149..153 -> 153..157 ms a run), Octane
  +1.4% in all while AWFY took 23.8% less time and the V8 suite was level.
  Not the in operator (PdfJS alone over four JIT layouts: 573.6 ms with
  it, 570.4 without). Bisected over four layouts, every commit from the
  slow-judgment fixes on is about 1 to 2% over 5a2bc9d (567.6 ms; the tip
  578.6), no one step its cause at that noise (+-3% a build): to be
  profiled, its JIT report diffed between the two.
- **Compiling in the background, last.** V8 compiles on a thread of its
  own while the function runs on in its lower tier; ours compiles on the
  runtime's goroutine, so that each compile's whole cost is paid where it
  is asked for -- with D6 and D7, Typescript's compiles again took 3.2 s
  of a run against 1.2 s before them. It is to be built at a very late
  stage, after compiles are cheap: done now, it would hide how slow they
  are, the time spent off the measured thread. It needs a snapshot of the
  feedback a compile reads (V8's JSHeapBroker), a `Runtime` being unsafe
  for concurrent use, and a safe point to install the code at.

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
  compare; equality fast paths at the top of `StrictEquals`. (The tree tier
  later called `o.m()` on a local or `this` in one node, `a5813af`.)
- A `.length` node in the tree tier: its code's placement cost Crypto 2.5%.
  A fused `obj.k op=` read, which cost NavierStokes 8% as a node of its own,
  was done later as one node for every update of a place (`055a53f`).
- GOGC tuning; PGO, since the engine is a library. (Leaner objects, values
  only with the keys in the layout, were once on this list and were done in
  `944e2e8`.)
- A nursery, a young generation for the interpreter's objects: 40% of
  EarleyBoyer's small objects die within 4K allocations, but nearly all of
  Splay's survive, and the tree tier keeps temporaries in Go locals, which a
  collector of its own cannot see. Smaller objects did what it was for.
- Structuring every tree's control flow eagerly: build cost made the V8
  suite 1.5% slower. Structuring only loops, done (`8d54f7e`), pays.
- Steady reads and calls taking their own operands in the tree builder
  (410 lines, V8 level), before the invariant that made spills unneeded was
  found; `elemAt` over the value's raw bits; pure functions as trees, or a
  closure evaluator of pure bodies, slower than `pureCallAt`'s switch (91
  against 82 ns); a 1.5x growth of `appendElem`'s own.
- A self tail call as a loop inside the tree's call node: every version
  moved the closures after it, by up to 11%. A function calling itself
  starts again in its frame instead, from a closure in a last-sorting file
  (`6099e8c`).
- A derived constructor's `this` binding in the frame, and `super(a, b)`
  without copying its arguments: 12.6% fewer allocations in DeltaBlue, but
  slower over Are We Fast Yet at most placements, for 24 bytes more a frame.
- Holding an outgrown creation site's objects in a table sized to its
  largest object, up to 32 properties: one large outlier makes every small
  object take 1.6 times the memory.

## After the review

The rounds that followed, each measured as above. The commit says more.

| When | Commit | What | Effect |
|---|---|---|---|
| 2026-10-05 | `0820228` | `callTree`, a call needing only the frame | V8 -1.4% |
| 2026-10-07 | `055a53f` | Every update of a place -- local, upvalue, global, property, element -- as one tree node | V8 -1.2%; updates -19 to -36% |
| 2026-10-07 | v0.24.1 | Tree builders out of the big function's inlining budget, tree calls through `callTree`, nested trees without `recover`, global sites keeping their environment | V8 -4.5% |
| 2026-10-08 | `8d54f7e` | A tree's loops as Go loops, structured from its block graph | micro-benchmarks -9.4% |
| 2026-10-12 | `b03617f`..`152bea0` | Property reads, tree calls, deletes, spreads, generators, RegExp, Date, string chains | V8 -0.8%, micro-benchmarks -6.1% |
| 2026-10-13 | `6e5ce1f`, `944e2e8` | An object 96 to 56 bytes, what only some have kept apart; keys in the shape, values in the object | bytes -18%, a two-property object 144 to 96 bytes |
| 2026-10-14 | `e019ec8`..`96eab49` | Deep prototype chains' additions cached, typed arrays in `elemAt`, stacks of throws resolved lazily | Octane -9% without the JIT; `try_throw` 297 to 34 ns |
| 2026-10-15 | `a5813af`, `7f06603` | A call evaluates its callee and arguments in its node, without spills | AWFY -2.7% |
| 2026-10-15 | `3ed2f6f`, `60888ee`, `d1805f2` | `new Array(n)` filled in place; `&&`, `\|\|` and `?:` as one node | AWFY -3.9%, -1.1%, -1.4% |
| 2026-10-16 | `6193edc`..`cb7f7b8` | JIT: the callee makes its own frame; a native call's state and calls of Go described statically, as V8's deopt data; constants compared in one instruction; smaller equality and truthiness | with the JIT AWFY -3.2%, Octane -1.3%, V8 -1.0%; hot code -16% |
| 2026-10-17 | `7668123`..`d27ce77` | `Math.random()` from a per-runtime cache of 64 xorshift128+ numbers, as V8's: a load in native code, a frameless call in the interpreter | with the JIT 49 to 3.7 ns (Node 6.3), without -13.6% |
| 2026-10-17 | `897895c`..`d4e1855` | JIT: getters and setters called natively, prototype reads at megamorphic sites, loose `==` of booleans, recompiles that wait for room | Octane with the JIT -2.0% (Box2D -33%, PdfJS -7%, Typescript -5%) |
| 2026-10-18 | `e2a4f0c`..`310d0e9` | JIT: callees' contexts and closures from pools, as V8 allocates contexts; native code that goes on while the collector marks | with the JIT AWFY -2% (pooled closures), -14% (code judged slow less often), -23.8% (the marking stages) |
| 2026-10-19 | `310d0e9`..`4dc3a3d` | JIT: forEach, reduce, map, filter, some, every, find and findIndex as loops of the caller's code, a typed array's too; one-character substrings, parseInt and indexOf natively; constructors that only forward their arguments inlined | Box2D -11% and -9.5%; `string_indexOf` 110.8 to 72.1 ns |
| 2026-10-20 | `18e6850`..`713fcc4` | BigInts: those in the int64 range natively in the JIT; arithmetic of our own (`internal/bignum`, its inner loops in assembly) in place of math/big; values below 2^64 held in their Values; results made in the blocks of operands no one else holds | the interpreter at or below C QuickJS on every BigInt micro-benchmark: `bigint256_arith` 290 to 136 ns (QuickJS 144), `bigint64_xorshift` 1789 to 154 (189) |
| 2026-10-20 | `ddc9831`..`50be20c` | JIT: BigInts as 64-bit words and as Uint64s, `asUintN` and `asIntN` of words, `**`, truth and `toString`; heap BigInts' arithmetic by a leaf of the VM, in blocks given back | with the JIT faster than the interpreter on every BigInt micro-benchmark: `bigint64_xorshift` 624 to 9.5 ns, `bigint256_arith` 247 to 117 (136) |
| 2026-10-20 | `bc35a9f` | JIT: code unused for a second flushed, with what it was compiled for | live heap after the suites with the JIT: Octane 355 to 230 MB, AWFY 54 to 10 MB; AWFY's CPU time -19% |
| 2026-10-20 | `262fb78`..`b5ee46c` | JIT: calls of Go that run nothing made without a frame; keyed writes, a Map's or a Set's iterator's step and an array's length natively | `prop_create` 201 to 65 ns, `map_for_of` 66 to 14, `array_length_decr` 85 to 18, `map_set_bigint` 405 to 171 (the interpreter's 224) |
| 2026-10-20 | `e49c1d9`, `26eb754` | JIT: loops whose calls of Go or exits do not pay go back to the tree tier, at the loop that entered native code | `date_now` 69 to 50 ns (the interpreter's 49), `generator_create` 979 to 690 (687) |

### What is open

- **Allocation, judged by CPU.** Over Are We Fast Yet go-quickjs is 1.07
  times QuickJS's wall time but 1.21 times its CPU time (2026-10-15):
  Havlak, DeltaBlue and Storage pay most for the collector. DeltaBlue, the
  furthest behind, spends about a quarter of its other time in property
  access through the caches.
- **Micro-benchmarks still about twice QuickJS's time** (v0.27.0): an
  array's growth by index (`array_prop_create` 2.39x), `WeakMap` set and
  delete (2.1x, which `weak.Make` sets a floor under), two functions in
  mutual tail calls (`tail_mutual` 2.74x), and generators made and run by
  for-of (1.7 to 1.9x). Global reads (1.70x, a top-level `let` or `const`
  about 10 ns more than a `var`) and element reads (1.40x) are the common
  ones. BigInts past 64 bits, 2.19x then (`bigint256_arith`), are below
  QuickJS's time since 2026-10-20, with the JIT or without.
- **RegExp:** a run of literal characters inside a pattern as one
  instruction (B6) is still left; `regexp_ascii` and `regexp_utf16` take
  1.4 times QuickJS's time.
- **The JIT** (`-tags quickjs_jit`):
  - Compiling costs too much for what it saves in large programs. Octane's
    Typescript, which with the JIT had taken twice the interpreter's time,
    takes 0.86 times it (7.2 s against 8.4 over three runs, 2026-10-20, at
    v8bench's budget of 32 MiB), among the least the JIT gains of the
    suite's programs; with code unused flushed since (`bc35a9f`) it evicts
    no code for room, where it evicted 1,075 entries a run, and leaves
    native code 34,399 times a run where 74,847. Most of its compiling is compiling again for what a function
    learned, 1.5 to 4.6 ms each as they inline more. Compiled code whose
    exits cost more than it saves is not always given up: a stretch is
    judged by 20 instructions of work against an exit (`jitSSAMinWork`),
    far less than an exit costs. More room for code (`WithJITMemoryLimit`)
    must never be slower, and was, before the flush: with 16 MB Typescript
    took 18% longer than with 8. The runtime's default stays 8 MB until
    that is measured again.
  - **What code in use was compiled for stays alive.** Code unused is
    flushed (`bc35a9f`), and what it was compiled for with it; code still
    in use holds its call targets' closures (`nativeCalls`), its holders
    and the closures it inlined, strongly, where V8 holds them weakly and
    deoptimizes code whose objects die. Go gives no hook to invalidate
    code before a dead object's address is reused, so a weak target needs
    code that checks it is still alive before it is entered. Octane's
    Typescript keeps about 110 MB more than the interpreter while it runs.
  - Box2D's exits, once comparisons of two objects, `instanceof` of a
    parameter and additions across many shapes, are 13 a run now (a
    count of exits and calls of Go by site, 2026-10-19). Its cost was
    calls of Go: parseInt of numbers, 120,000 of its 150,000 calls of
    functions from native code in three runs, made natively in 3ca9eb9
    (Box2D -11%, every run below every base run). Then its constructors,
    function V() { V.b2Vec2.apply(this, arguments); this.constructor ===
    V && this.b2Vec2.apply(this, arguments) }, each construction made by
    Go and the tree tier: inlined in 9b840fc (bytecode's
    ForwardsArguments), each forward the call of its target, the frame
    keeping the construction's arguments for an exit (InlineState's
    Args), a known closure's captured bindings read natively, a shared
    initializer's deep additions compiled for: Box2D alone -9.5% and
    -6.5% at two code offsets, Octane -1.8%. What is left of its calls of
    Go is GoMath, Math.sin and Math.cos, 115,000 in three runs, about 1%
    of a run at 18 ns a call.
    Gameboy's alphabet.indexOf(data.charAt(i)), 85,000 calls of the
    built-in, and every string's and array's indexOf, lastIndexOf and
    includes, are calls of Go of their own (0e7ed76, GoIndexOf), the
    built-in's Go function called with no frame: string_indexOf (micro)
    110.8 -> 72.1 ns, array_indexOf 429.1 -> 354.9.
  - **Leaf calls of built-ins, one mechanism for all** (2026-10-20, not
    begun). A built-in that runs no script is called from native code by
    one of three paths, each with a list of its own: `Intrinsic` for
    Math's functions (an instruction of their own, or `GoMath`), the
    frameless calls of Go for Map's and Set's operations and `String(x)`
    (`jitGoCallPlain`), and for the rest, `Date.now` among them,
    `jitGoCall`, which gives each call a frame, pins the code, copies and
    clears the operands and asks what the JIT may learn -- more than the
    tree tier spends on the whole call: `date_now` took 67 to 79 ns with
    the JIT, 51 without. Its loop now goes back to the tree tier, whose
    calls pay (`e49c1d9`): level, 50 ns, but a loop of such calls can only
    match the tree tier while each call of Go costs native code more than
    the tree's call of the built-in. V8 calls such functions from
    optimized code as fast API calls, with no frame of JavaScript's. The
    plan:
    1. A built-in declares itself where it is made: a leaf entry in its
       `funcData`, the Go function that makes the call -- a result, or
       "not mine", for anything it cannot answer without running a
       script or throwing, which the ordinary call then makes -- and its
       effects: none (`Math.sin`, `Math.max`), reads of state (`Date.now`,
       `Math.random`, `Map.prototype.get`), or writes (`Map.prototype.set`,
       an array's `push`). No tier keeps a list of functions.
    2. In the SSA, a call whose feedback names such a built-in is a leaf
       call, guarded on the callee as `Intrinsic`'s calls are: one without
       effects may be shared and hoisted out of a loop, and its number
       result kept unboxed.
    3. Native code calls it through a trampoline of its own, which calls
       the entry's function with the operands' words, past `goCall`'s
       expansion and `jitHostCall`'s switch: about 25 ns a call of Go
       today, perhaps 5 to 10.
    4. A native form, where there is one, stays first: `Math.sqrt`,
       `Math.random`'s cache, a Map iterator's step.
    5. The tree tier answers from the same entries, where `mathCall` and
       `elemOp` each keep cases of their own.
    First `Date.now`, Math's functions and what `jitGoCallPlain` lists,
    measured by `date_now`, the `math_` and `map_` micro-benchmarks and
    the suites' placements; then Date's getters, `String.fromCharCode`,
    `charCodeAt`, `Number.isInteger`, `Object.is`, `Array.isArray`, a
    typed array's `subarray`. It complements judging native code that
    does not pay (its loops sent back to the tree tier): that decides
    when not to run native code; this makes a loop of such calls pay.
  - A native call site still spends about 300 bytes on its guards and on
    writing its arguments, which a callee checking its own frame's room
    and the context's level could shed.
  - `placements -jit` to vary the offsets of later code chunks and the
    JIT's Go-side code (see "How an item is measured").
