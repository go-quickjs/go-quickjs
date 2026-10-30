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
  10K a run; done since by context allocation, `074e65c`, `5a2bc9d`);
  calls, and writes, inside inlined callees (below). New:
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
     for. An exit's records were still Go's to apply while marking
     (recordsTail), staged in GoArgs as tail calls are, until `6f3509b`
     applied them natively (the second round of 2026-10-28, in E).
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

## E. The JIT against Node

Measured 2026-10-22 at `3141a88` (`internal/cmd/v8bench/matrix`, Node
v26.10.0, the fixed work's median of three fresh processes; profiles of
each benchmark with `-cpuprofile`, their flat time sorted into native
code, the JIT's Go side, the VM, allocation, the collector and the rest).
The JIT is ahead of V8's interpreter and 3 to 7.5 times its optimizing
tiers' time:

| | go-quickjs JIT | Node | Node `--jitless` | JIT / Node |
|---|---:|---:|---:|---:|
| V8 suite | 881 ms | 263 ms | 1,286 ms | 3.3x |
| Octane | 10,151 ms | 1,576 ms | 16,050 ms | 6.4x |
| Are We Fast Yet | 7,887 ms | 1,045 ms | 17,470 ms | 7.5x |
| micro-benchmarks (153, geometric mean) | | | | 4.5x |

What the gap is made of:

- **Native code itself.** Richards, Towers, List and Bounce spend 78 to
  99% of their CPU time in native code and still take 5 to 8.5 times
  Node's time; the benchmarks native code dominates are about 40% of Are
  We Fast Yet's gap. The micro-benchmarks say where V8's code is ahead:
  inlining (`method_call` 13.2 ns against 0.62, `func_call` 1.79 against
  0.19, `func_closure_call` 15x), checks taken out of loops
  (`global_write` 2.79 against 0.14), the array methods that take a
  callback (`some`, `reduce`, `map`, `filter`, 9 to 28 times: V8 inlines
  the built-in's loop and the callback into it).
- **Allocation and the collector.** The collector is 22 to 43% of the CPU
  time of Storage, Havlak, DeltaBlue, CD, EarleyBoyer and Splay, about 60%
  of Are We Fast Yet's gap; Storage's arrays are made by `jitFillPool`
  (31%), Havlak makes the closures it passes to forEach (`newClosureIn`,
  9%). Go's collector is neither generational nor moving, so what is left
  is to allocate less: objects V8's escape analysis never makes take 11
  to 40 times its time here (`prop_clone`, `closure_make`,
  `iterator_for_of`, `local_destruct`, `generator_next`).
- **Coverage and tiering in large programs.** zlib's two largest
  functions, 13,660 and 5,039 instructions, are past the 4,096 the JIT
  compiles, and leave a fifth of its time in the interpreter (since
  `300e8a5` such functions compile where their compile fits the budget:
  the 5,039 does, the 13,660 is refused for its 514 slots);
  Typescript leaves native code 125,000 times a run, compiles about four
  times a function and drops 107 functions for room; Box2D and PdfJS
  spend 12 to 20% of their CPU parsing and compiling source, much of it
  never run (V8 compiles a function when it is first called).
- **Calls of Go for built-ins.** String's methods take 10 to 19 times
  Node's time with the JIT (`string_code_point_at` 52 ns against 2.7):
  the call of Go's own cost, about 27 ns for a leaf that does nothing.

Where the JIT is ahead of Node: BigInts of 64 bits (`bigint64_arith` 4.9
ns against 19.4), a throw caught in the same function (`try_throw` 4.5
against 97), `loop_sum`, `prop_delete_last`, `string_from_code_point`.

What a loop's turn is spent on (step 1 below, 2026-10-22, with
`v8bench -jitcode` and `jitprof`, about 1,000 samples a benchmark):

- **Calls** are 22 to 34% of native time in Towers, Richards, Bounce and
  List (`call` and `call entry`). A native call is 60 to 70 instructions
  before its jump: the context's depth, the callee checked through its
  cell, the frame's room, each argument's two words with a test of each
  source, every live value spilled to keep cells and loaded back, and the
  collector's marking flag at the return. V8 inlines Towers' callees,
  where ours are not inlined at all: `ssa.InlineCalls` refuses a callee
  that reads a global, makes a call that is not inlined itself, or has a
  loop, and `pushDisk` and `popDiskFrom` each read `Error` on a path
  that throws, which keeps them and `moveTopDisk` out of `moveDisks`.
- **A property read's chain of loads**: a value with a reference is
  carried as the address of the cell it came from, its reference read
  from it (`[cell+8]`), then the object's values (`[obj+8]`) and the
  value: three dependent loads where V8 loads an in-object field once.
- **A spill on the critical path**: List's `isShorterThan` spends 45% of
  its samples on one property read's cell address stored to a spill slot
  and loaded straight back, every turn -- the allocator holding the
  loop's phis live to its back edge's poll, not anything the collector
  needs (fixed, `3d997a4`); its turn is about 70 instructions where V8's
  is about 12 (unrolled four times).
- **Conditions as booleans**: `x !== null` in a loop's test is `cmp`,
  `setz`, `movzx`, two moves, `xor 1`, a move, `test` and the jump, where
  V8 has `cmp` and the jump: 12% of `isShorterThan`.
- Constants such as the object tag re-read from the context at each
  check, off the critical path.

Done since (2026-10-22, `3d997a4`..`92d71f7`, JIT on, against the build
before; three alternated runs of each, then four JIT code placements,
`V8BENCH_JITSEED` 1 to 4):

- The backend (`3d997a4`..`7db8ed6`): a back edge's poll no longer keeps
  its header's phis alive, which spilled List's loop; keeps test the
  barrier flag once a run; a native call writes, saves and reads back
  only what it must (Towers' self-call 85 to 49 instructions, a native
  return 23 to 14); a branch compares rather than testing a boolean made
  of a compare (21,365 of the suites' 21,819 such branches; List's two
  tests 19 instructions to 4). V8 suite -2.9%, Are We Fast Yet -1.9%
  (List -23%), Octane -3.9%.
- The inliner (`86da85e`..`92d71f7`): callees with throws and calls
  their code was never seen to make, those made exits from the inlined
  frame; callees that read globals and what they captured; a function's
  constructions given the inlining's bounds first, since a forwarding
  constructor not inlined is a call of Go (RayTrace); and three bugs
  found on the way, a construction Go enters given the wrong function,
  a closure lost to a build limited to 256 keep cells (Typescript, now
  and then, "undefined is not a function"), a super()'s parent inlined by
  a compile again. Over the four placements: V8 suite -1.7%, Are We Fast
  Yet -1.9% (Towers -28.7%), Octane +1.1% (geometric mean -0.3%; Box2D
  -6.9%, Typescript +4.0%).

- Array methods' callbacks (`2cee3f7`, `ca4e24e`): the one closure a
  loop of forEach, map, filter, reduce, some, every, find or findIndex is
  given, inlined in the loop, and a closure literal made at the call and
  inlined there not made at all, made by Go at an exit that needs it:
  `array_reduce` 27.4 to 8.4 ns, `array_forEach` 28.0 to 10.0,
  `array_some` 28.2 to 12.2, `array_map` 36.8 to 22.5, Are We Fast Yet's
  NBody -28% at every placement.
- Typescript's sites (`73af3a0`..`eceffb6`): an ordinary object's index
  read by Go from native code; a megamorphic write inside an inlined
  callee Go's from native code, not an exit; a polymorphic method call
  learning every method its read has met at once; a store with checks
  appending natively; a generic call compiling the callee it meets with
  no code; `_super.call(this)` of a captured parent native. Typescript's
  runs finished in the interpreter 17,617 to 3,327, exits 72,927 to
  36,923; Typescript -3.9% at every placement, RayTrace (Octane) -9.4%.
- Two bugs on arm64 found by CI's macOS jobs, their fixes folded into the
  commits they were in: the listing `jitprof` reads, which arm64's code
  had not (`9f88c77`), and a call's state finding a saved box by its register,
  which another value saved across the call had after the box's end, so
  that an exit rebuilt a loop's counter from an address (`55db91f`;
  amd64's code had the same flaw, not met).

The round as a whole (`3d997a4`..`eceffb6` against `0c96ef6`, four
placements): V8 suite -3.2%, Are We Fast Yet -10.0% (geometric mean
-13.5%: Towers -45%, NBody -30%, List -23%, Richards -12.5%), Octane -2.9%;
56 micro-benchmarks -10.5% by their geometric mean. `generator_next`
(+19%) and `string_build_large2` (+19%), slower at the one placement
measured, were Go's code placement: over four placements built in one
checkout -0.6% and +12.5% bimodal (+23, -9, +14, +25), its time in Go's
string code; a micro-benchmark compared across binaries built in other
checkouts measures where Go placed the code, not the change.

The backend's second round (`91f6829`..`dfe1be4`, against `7717386`;
four Go code placements, `placements compare -jit` for the suites, the
regressed benchmarks alone in fresh processes): phis grouped with their
arguments, the groups the one scan's preferences (Octane's edge moves
107,309 -> 59,528); a loop's back-edge budget counted in a register
where its turn would wait on the counter in memory, by the turn's
loop-carried chain (loop_sum had been bound by the counter's
store-to-load forward, 1.31 or 1.68 ns a turn by where it landed); loop
headers at 64-byte lines, the padding jumped over; and the backend's
compile made cheaper than main's -- liveness found from each value's
uses rather than by block sets to a fixpoint, the groups in sorted
arrays, exits' slots and code constants found by hash (Typescript's
backend 425 -> 404 ms of compiles a run; 480 with the groups before).
V8 suite -0.5%, Are We Fast Yet -0.5%, Octane -1.8% (Typescript -6.5%,
Gameboy -3.3%; alone, Typescript, Box2D, Mandreel, zlib and Gameboy
-0.5 to -1.1%); micro-benchmarks -5.7% by their geometric mean
(empty_loop -55%, loop_locals -46%, loop_sum -18%, method_call_leaf -9%,
try_catch -4%). Slower at every placement: `prop_in` +7.4% and
`array_for` +5.9%. On the way: a register-group design that compiled
every function two and three times (the backend 11 to 20% dearer), and a
limit of 500 values a function on its second scan, rejected as an
arbitrary threshold; a budget read at every entry of a loop that turns
seldom (`method_call` +5.9%); exit edges laid out in a loop's code (a
taken branch a turn); and a budget wrongly stored as a float after a
poll, which stopped every later compile in the runtime.

The coverage round (2026-10-23, `675cc0b`..`b90a77d`, against `ed8cf09`;
four Go code placements, `placements compare -jit` for the suites, the
micro-benchmarks run on each placement's binary, the least of three, by
their geometric mean over the placements): the operations Go makes, the
frames, and callees' frames and resumptions (the coverage below), step
3's escape analysis (above), and step 4's first part -- functions past
4,096 instructions compiled where their compile fits the budget
(`300e8a5`), a callee too long to inline warmed up before it is compiled
for native callers (`ab7c5e5`) and compiled for a caller that calls it
0.15 times a call or more, V8's rule for inlining (`f2805ae`), a loop's
back edges spending the budget at that loop's own length (`f401f56`),
and what is live across a store found by walks from its uses in large
functions (`be45131`). V8 suite +0.8% (RayTrace -8.5%; Crypto +2.2%,
RegExp +2.0%), Are We Fast Yet +0.1% (CD -4.7%, NBody -4.2%; Sieve
+5.6%, Havlak +2.5% and Bounce +2.3%, which each commit's build run
alone puts within its runs' spread), Octane -2.6% (Mandreel -6.8%,
RayTrace -7.5%, Typescript -4.0%, PdfJS -3.5%, Gameboy -1.6%; Splay
+4.2%); micro-benchmarks -2.6% by their geometric mean, of 153
(`closure_make` -69%, `prop_clone` -65%, `func_closure_call` -38%,
`func_call` -28%, `array_reduce` -14%). Slower: `private_method` +12%,
its functions judged slow and run by the tree tier, the JIT's compiles,
entries and exits the same in every build of the round (Go's placement
of the tree tier's private members); `global_destruct` +7.7% and
`async_call` +5%, level in each commit's build alone. `placements
compare` picks the runs a benchmark shorter than a second is given from
each binary's own first run: Gameboy, whose first run takes 330 to 350
ms, was run 10 times on one side and 3 on the other, its warm-up
weighing three times as much on one (+19% in the suite, +48% alone);
given 10 runs on both, -1.6%. The runs are picked once now, for both
labels, by the first run that times a benchmark (`repsTable`). The VM's
own tests pass under `QJS_JIT_STRESS` since `9bda4ec`, which CI runs.

The round of 2026-10-26 (`36cd021`..`7b3754a`, against `fb48e4f`; four Go
code placements, `placements compare -parallel` for the suites, the
micro-benchmarks on each placement's binary, the least of three):

- **Why the JIT trailed the interpreter on six of Octane's benchmarks**:
  in one run it spent its gain compiling. Richards makes 12 compiles,
  2.4 to 3.3 ms of its 5; Typescript 766 of 464 functions, about 60% of
  them again, 38% of its CPU; CodeLoad compiles nothing, its time is
  parsing. Recompiles that need not have been: a shape another came from
  dropped from a read's cases while there was room (`8f32baa`;
  Typescript's `bindSymbol` deoptimized at 9 sites in turn), a call of a
  captured closure and a function's calls of itself not decided at the
  caller's compile (`f877a6e`, `10879b1`; RegExp's recompiles 2 -> 0,
  zlib's 13 -> 5, EarleyBoyer's 17 -> 8), and a long callee warming up
  called through Go rather than its link (`1e66601`). Deciding more calls
  made more of PdfJS native, which met three exits at every call: a
  string's read past its end, a call target learned after the compile
  (code that leaves for it twice is invalidated now, as V8 deoptimizes
  on a wrong target) and `String.fromCharCode` past Latin-1 (`51c53e7`,
  `bc75633`, `c2591b3`): PdfJS's exits 5,006 -> 3,057, Octane's 34,952 ->
  31,865, Are We Fast Yet's 8,306 -> 5,748. Octane's recompiles 586 ->
  470.
- **The interpreter's calls and allocation** (`8174354`..`6549b9f`): a
  class's construction had been refused by the call planner, so every
  `new` of a class went through `runFD` (Havlak 3,308,370 a run -> 481);
  additions past a long prototype chain are cached by the chain's cell
  (writes through `deepStore`: DeltaBlue 155,868 -> 0, Havlak 1,304,840
  -> 0); `super(a, b)` passes its arguments in place; calls of up to
  three arguments as nodes of their own count (a two-argument method
  call 215 -> 156 instructions to `callTree`), which call a tail-calling
  function on its tree themselves; `Array(n)` a native leaf. Are We Fast
  Yet allocates 4.9% less. What the round added to named functions is in
  closures (`6549b9f`), every named function as long as the base's, so
  that the hot ones keep their places; the closures sit 672 bytes later,
  the new `init` code's. A second `super()` constructs the parent's own
  object, its fields and body run on it, then throws (`09789cf`): the
  instance made before the first had been given them again (`this.x` 2
  where Node and QuickJS have 1, a private field's TypeError where they
  throw the ReferenceError). A method read off a string or a number
  in the method nodes is the primitives' table's (`7b3754a`):
  String.prototype is a wrapper, which the shape caches leave out, so
  every such read had gone the long way, in the base too
  (`string_indexOf`'s node to `callRest` about 250 -> 192 instructions).

Over four placements, against the base, in one session: without the
JIT, V8 suite level, Are We Fast Yet -1.0% (Storage -6.5%, Towers
-4.7%), Octane -1.2%; micro-benchmarks -0.2% by their geometric mean of
159 (`math_imul` -15%, `math_min` -15%, `map_set_bigint` -13%,
`string_indexOf` -9%). With the JIT, V8 suite +0.2%, Are We Fast Yet
-0.7% (DeltaBlue -4.8%), Octane -3.2% (Box2D -30%, Typescript -4.8%,
RayTrace -3.9%, RegExp -3.5%, PdfJS -1.8%); micro-benchmarks level. The
same binaries of the base measured 1.5% faster on Are We Fast Yet in
this session than in an earlier one, where the round's interpreter was
-3.7% (Sieve -16.6%, Queens -7.8%); the last two commits, timed against
the round without them, are +0.1% (`09789cf`) and +0.6% (`7b3754a`, at
two placements slower and two faster). On the
way: the nodes by count had sent every call not on a tree through a
closure that asked again what the node had (`func_call` +12.9%,
`method_call_leaf` +17.9% at every placement), and the chain cell's case
had grown `pureCallAt`, moving every case after it; both are folded into
the commits that made them.

Open from it:

- Storage with the JIT +1.7%, slower at three placements of four, is
  placement: the same native code (its bytes differ only in 55 heap
  pointers), the same Go instructions and allocations; the round's growth
  of the JIT's packages moved `jitFillPool`, a third of its profile,
  against `jit.goCall` and `newArrayObject`, and a pad file that moves
  nothing but addresses brings it within 1% of the base at five shifts
  (+10.5% unpadded, timed alone).
- Without the JIT, `array_forEach` 37 -> 53 ns at two placements of four
  and level at two, `math_clz32` 42-50 -> 50-54: the 64 bytes the last
  two commits add before the closures, their paths unchanged.
- With the JIT, `array_forEach` +30% and `array_some` -40% in a whole
  run, level each run alone: where the code earlier tests compiled puts
  theirs.
- Compiling is still what a single run of Typescript or Richards spends
  most on: 5 to 10 microseconds an instruction, the backend 45%,
  optimization 25%, the build 23%; calls are 23% of Typescript's code,
  keep cells 15%. V8's answer, compiling in the background, meets a
  runtime's one goroutine. (The round of 2026-10-27, below.)
- `toInt32` is 20.7% of zlib's interpreted time, 870 million conversions
  a run: QuickJS keeps small integers as integers; nested bitwise results
  kept as int32 in the tree tier would need a second kind of value.
  (Done without one, below.)

The round of 2026-10-27 (`6ffbb54`..`f838b8c`, against `b07caea`; four Go
code placements, `placements compare -parallel`, the micro-benchmarks on
each placement's binary, the least of three):

- **The JIT's compiles** (`6ffbb54`..`6d28f33`): what made them dear was
  how much they made, 613 SSA values a compile in Typescript, 22% of them
  phis, about eleven entries a compile of which Go went on at 27 of 6,146
  after calls in a run. An entry after a call of a function's code is
  made once Go has gone on there, the code compiled again at once and
  the frame going on in it, as V8 compiles code to enter where a frame
  needs it; an inlined call keeps its entry, its exits the code's own.
  One call of Go out of line for each native call that does not make
  it; the kept values' definitions found by block; plan's table of
  inlining answers from the workspace; a keep reads the pointer word it
  copies; a word compared with a kind's once (amd64). Typescript's
  compiles 611 -> 495 ms a run (-19%), 872 -> 683 us a compile, its code
  31.2 -> 24.5 KB a compile; PdfJS -11%, Box2D -7%, AWFY -6.5% of their
  compile CPU.
- **int32 trees in the interpreter** (`cf27162`..`f838b8c`): a bitwise
  operator one of whose operands is a number is an int32 (a BigInt would
  throw), so its entry builds a tree returning int32, without a second
  kind of Value; an element read or write at an int32 key, an element
  read under a bitwise operator, and a sum under one are made in place
  in such trees, everything else the slow paths' in their order.
  `F = c[g >> 2] | 0` 219 -> 179 instructions and 4 -> 1 conversions,
  `(g >>> 7) & 255` 72 -> 43.

Over four placements, against the base: without the JIT, V8 suite +0.2%,
Are We Fast Yet +1.0%, Octane -10.5% (zlib -17.9%, Mandreel -3.5%),
micro-benchmarks -0.1% by their geometric mean; with it, V8 suite -1.7%
(EarleyBoyer -4.8%, NavierStokes -4.8%), Are We Fast Yet -2.1% (CD -6.0%,
Towers -4.9%, Json -4.6%), Octane -3.5% (Typescript -6.1%, PdfJS -4.2%,
Gameboy -3.3%), micro-benchmarks -0.2%. On the way: entries made lazily
left Go arriving where there was none, each turn going on in the
interpreter until a compile a budget later, and the turns so cut short
had code judged slow and never entered (`private_method` +70%, Are We
Fast Yet's finished in the interpreter 594 -> 1,691), and entries after
inlined calls made lazily kept Queens' column boxed through its loop
(+2.8%); both are folded into the commit that made them. On Windows,
checked out with CRLFs, the int32 tests' expected output had them.

Open from it:

- Mandelbrot without the JIT +4.8%, `generator_for_of` +6%: neither
  builds an int32 tree nor runs another instruction; a control build, the
  round's layout with every tree the base's, is as slow (+3.8% and
  95-98 ns): everything after the int32 files moved 1,216 bytes.
- Sieve with the JIT +4.5% timed alone (faster at one placement of four,
  about 10% slower at three): the same compiles, entries and exits, its
  code smaller (5,292 -> 5,208); it allocates 235 MB in 8,400 objects,
  as Storage, whose like was the Go helpers' placement.
- Locals that only ever hold int32 still convert at each use (`g >> 2`
  with `g` a local, 8.6% of zlib's samples): proving it needs mapped
  `arguments`, eval and reads before the first store accounted for.
- Mandreel's time is its heap's global read (13%), not conversions.
- Compiling: still spread thinly; `===`'s cold paths 9% of code, exit
  stubs 15 bytes each, 31% of Typescript's compiles recompiles.

Step 5 below, the calls of Go for built-ins (2026-10-27,
`375a3bd`..`54c1dbb`, against `131390c`; counted in thread cycles a call
-- QueryThreadCycleTime, the least of 25 runs of 2^18 turns, the binaries
alternated -- and by the micro-benchmarks, the least of two runs of each
binary, on a machine other jobs shared; no placements):

- **A leaf's call through a routine of its own** (`375a3bd`, `bf51174`):
  native code jumps to the VM's `jitLeafFast` (`abi.Encoding`'s
  `LeafFast`), the description naming the leaf (`abi.GoCallState`'s
  `Leaf`), no operand written for the function, the result's number word
  back in a register; Go applies the leaf from a form it makes at the
  call's first (`jitLeafForm`), past `callGo`, `goCall`, `ExpandGoCall`,
  `jit.HostCall`'s function value, `jitHostCall`'s dispatch and the code's
  own routine of calls of Go. `Number.isNaN` through a local 93 -> 48
  cycles, a Map's get 158 -> 111, `Date.now` 125 -> 80, `startsWith` 247
  -> 193.
- **What only a call's refusal reads, restored on the refusal's way
  alone** (`bd72f8c`): the receiver, the function and the cells they were
  read through, beneath a call in its state, saved for an exit and dead
  after the call; a loop of `str.codePointAt(j & 15)` read back 3 values
  where 8.
- **Leaves native code makes itself** (`0af16eb`, `54c1dbb`), by kind
  (`ssa.LeafKind`, the VM's elemOp): `codePointAt` and `at` of a flat
  string's units, the leaf's call on a branch of its own otherwise (146 ->
  26 and 160 -> 26 cycles; charCodeAt's 19), and Number's four predicates
  with no call (`Number.isNaN` 71-87 -> 10-14, an empty turn's 10-11).
- **Every other call of Go that saves its live registers through a routine
  of the VM's** (`e80e014`, `GoFast`, `jitGoDirect`): `jit.goCall` with
  `jitHostCall` called directly and no routine of the code's: a string
  comparison 202 -> 170 cycles, indexOf 300 -> 275; and Math's functions
  Go computes save what lives across them, not every register (`9abbe36`:
  trig's `Math.sin` 34 register moves to 14; fdlibm's work is most of a
  call).

Micro-benchmarks with the JIT, base -> the round: `string_code_point_at`
40.7 -> 7.4 ns (Node 2.7, 15 times its time before), `string_at` 47.9 ->
7.9, `date_now` -35%, `map_set_int` -18%, `map_set_bigint` -23%,
`map_delete` -15%, `string_repeat` -24%, `string_trim` and `string_slice`
-18%, `string_pad_start` -17%, `string_starts_with` -16%, `string_chain`
-16%, `string_concat` -16%, `float_toPrecision` -13%, `float_toFixed`
-10%; `string_template` +3.1% and `float_to_string` +3.5%, single
binaries, which measure their placement as much. Are We Fast Yet and
Octane run as before (one run each, alternated, a shared machine: level).
The hot functions are the base's in builds with and without the JIT
(hotdiff).

Found on the way: a routine in Go's text that jumps straight back to the
native code that jumped to it -- `MOVQ x, R11; JMP AX` -- cost some 60
cycles a round trip on this Zen 2, where a direct jump in between, or the
same two instructions in the code's own memory, cost a cycle or two: the
distance between the JIT's memory and Go's text did not matter (an arena
placed within the same 2 GB as Go's text measured the same). None of the
routines jumps back so; each calls Go first.

Open from it:

- A leaf's Go side is still about a hundred instructions besides the
  leaf's own: the routine and the ABI0 wrapper (27), `jitLeafGo`'s frame
  and its spills around the check's and the function's closures, the
  operands' slice's checks, the keep cell's barrier test. V8's fast API
  calls a function of a typed signature; a native leaf's check and
  function fused, or forms of typed operands, would take most of it.
- Calls that save every register (goSaving) -- a back edge's poll, a
  pool's refill, a string's absent index -- go through callGo still.
- `string_indexOf` and `array_indexOf` are the built-ins' own work now
  (`strAndSearch`, `clampedPosition`, `stringslite.Index`).
- More leaves native code could make itself by the same kinds: `charAt`
  and `at` past Latin-1 (a string made), `String.fromCodePoint`, Map's get
  of a key the code knows the kind of.

The round of 2026-10-28 (`2460953`..`608137b`, against `131390c`; four Go
code placements, `placements compare -parallel -k 4`, the micro-benchmarks
on each placement's binary, the least of three), step 5 above among it:

- **Stack traces named as V8 names them** (`2460953`, `d2b6d52`): a
  frame's type is V8's GetConstructorName's -- a toStringTag, a
  generator's function (`g.next`, where it was `Object.next`) -- and an
  anonymous function's name V8's FuncNameInferrer's, the compiler handing
  `obj.method`, `<computed>` and an enclosing constructor's name through
  conditionals, logical operators, commas and literals; 48 cases Node
  gives.
- **Callees with loops, calls in tail position, unboxed loops**
  (`66ae2b9`..`d20628f`): a loop's numbers are unboxed where that
  speculates nothing, their boxes made where they are used (`array_reduce`
  and `array_map` 2 and 1 boxes a turn to none); a callee with a loop is
  inlined where the loop calls a closure the caller gives it, the closure
  not made (`closure_callback` 278,015 -> 17,337 allocations); a callee
  whose last act is a call is inlined, its frames counted toward the
  depth (Havlak 9.1 -> 7.2 million allocations).
- **Generators in both tiers** (`d9173a4`..`6124758`): a generator of 96
  bytes without an ext, made by kind (an async function's with its
  promise, a loop's with its cursor): `generator_create`'s work allocates
  288 bytes in one allocation where it allocated 448 in two; the
  tree tier's loop steps it in one place with no recover, yield* too; the
  JIT calls next, makes a generator and takes a for-of's cursor natively,
  from pools.
- **The marking rule** (`dfbf26d`..`72a5580`): native code does the same
  work whether the collector marks (the JIT README's write barrier);
  `TestJITSameWorkWhileMarking` runs every micro-benchmark both ways and
  compares the calls of Go, exits and interpreter finishes. It found the
  round's generators leaving for Go while marking (`generator_next` 513
  -> 26,422 calls of Go a run) and eight older paths on main -- array
  length writes, spreads, computed keys, a literal's clone, delete,
  spread calls -- all brought to it; its list of known breaches is empty.
- **What the first placements found** (`9920b27`..`39834f8`): a string
  builder's append, and a + no builder takes (a prepend, `i + ''`), past
  `jitHostCall`'s dispatch (`string_build1` 148 -> 128 cycles, main's
  150; `string_build3` 221 -> 202, main's 213); a call that met more
  than one function made natively to each, not inlined; an exit inside an
  inlined callee teaches
  the callee's feedback and its inliner is compiled again (Game Boy 3,641
  -> 2,009 runs finished in the interpreter, main's 3,238); and the tree
  tier's step over a Map, a for-in or any iterator doing `iterNext`'s work
  itself (`map_for_of` 289 -> 224 instructions a step, main's 243).
  `bytecode.Function`'s new `TraceName` had gone after `Name`, among the
  fields a call reads, which changed the displacements in `executeAt`,
  `runTree`, `runTreeNested`, `callTree` and `runFD`; it is the struct's
  last field, and hotdiff finds the hot functions main's.
- **Numbers' equalities** (`608137b`): `===` and `!==` of two values
  known to be numbers compare float64s, as V8 lowers them, not the boxes
  through the generic path; Mandelbrot's code 4,819 -> 4,221 bytes,
  Octane's 2.0% smaller, every compile, exit and interpreter finish the
  same.

Over four placements, against the base: without the JIT, V8 suite -0.2%,
Are We Fast Yet -0.5%, Octane -0.1%, micro-benchmarks -1.2% by their
geometric mean (`generator_create` -25%, `generator_for_of` -23%,
`async_busy` -14%, `private_method` -10%, `map_for_of` -8%); with it, V8
suite -0.3%, Are We Fast Yet -4.5% (Havlak -11.5%, Sieve -7.8%, Bounce
-3.6%), Octane -0.5%, micro-benchmarks -5.5% (`closure_callback` -80%,
`private_method` -67%, `generator_create` -65%, `generator_next` -57%,
`array_forEach` -36%).

What the slower ones were, each run alone and counted (thread cycles,
Delve's instructions a turn, `-jitreport`), at a first placement run:

- Layout, the same work: `array_length_read` (+4.9% with the JIT) and
  Mandelbrot (+2.5%) run fewer instructions, the box `66ae2b9` sinks out
  of their loops, and their loops sit elsewhere in their 64-byte lines:
  NOPs run once before the loop make the round 5 to 24% faster than main
  at most offsets, and moving the code by whole lines changes nothing.
  `bigint64_search` (+6.7%) the same within its function. RegExp (+2.4%)
  was package vm moved 10,464 bytes against regexp and the runtime by a
  change in `internal/jit/mir` alone: the parent padded to that layout is
  as slow, the same source; placements do not vary where vm sits against
  the packages before it. `array_spread`, `bigint64_pow`,
  `method_call_leaf` and `string_build2` without the JIT run main's
  instructions, function by function (1,738, 736, 526, 507).
- Warm-up, the same work done earlier: PdfJS (+1.3%) and Game Boy
  (+1.6%), measured with few runs, compile what main compiles a few runs
  later, `195667b` moving where back-edge budgets fall (PdfJS's
  `getOperatorList`, 135 KB of code, compiled in the measured window);
  at 15 to 30 runs both are level or faster.
- Noise: Sieve (memory-bound, the same native code, the same allocation
  and marking), `array_slice` (never native, its functions main's),
  CodeLoad, EarleyBoyer. Without the JIT, Crypto +5.2% and Sieve +4.1%
  are one placement each (17% and 24% there, level at the others).

Open from it:

- `prop_in_keyed` without the JIT +20% at every placement: it runs the
  same 439 instructions a turn, its closures instruction for instruction
  the base's; the generators' closures before the tree tier's other init
  closures moved them 192 bytes, which placements, moving the closures as
  a group, do not vary. A pad that won it back made `generator_delegate`
  28% slower at one placement, and is not kept.
- CD with the JIT: `195667b` inlines `delta()` twice in
  `findIntersection`, three frames each, which takes 6 of the 8 inlines a
  function may have (`maxInlines`), so its later `minus()` calls are made
  natively and their objects allocated (+1.7%). V8 bounds inlining by
  size alone; without the count CD allocates 9.4% less, but Octane's code
  is 1.6% larger and Typescript allocates 4.5% more: a round of its own.
  Done in the next round (`e79cb47`, below).
- Huge functions compiled again for each call target learned
  (`getOperatorList` six times), and callees inlined before their own
  calls have feedback (`memoryHighRead`): main does both.
- `string_concat` +6.7% and `regexp_test` +5.3% with the JIT at three
  placements, not yet looked at.
- A yield whose operand is an object beneath it still leaves for Go while
  the collector marks, through the exit records' tail, the one exception
  the rule names. Done in the next round (`6f3509b`, below).

A second round of 2026-10-28 (`2bf51d4`..`cdfe563`, against `93336ae`;
four Go code placements with the JIT, `placements compare -parallel -k
4`, the micro-benchmarks on each placement's binary, the least of
three), the two items above and what measuring them found:

- **Inlining bounded by size alone** (`e79cb47`, `3b178e7`), as V8's
  inliner is: the count of inlines a function may have (`maxInlines`)
  is gone, a cumulative budget of 192 instructions and 48 a callee
  left; a callee of 10 instructions or fewer is inlined past the
  budget, up to five times it, as V8 inlines small functions whatever
  the budget; and the calls loops hold take the budget first, deepest
  loop first, standing in for V8's order by call frequency. CD's
  `findIntersection` inlines its `minus()` calls (program allocations
  9.73 -> 8.78 million).
- **An exit's records applied natively while the collector marks**
  (`6f3509b`): the exit records' tail (recordsTail) stores through the
  barrier's routine, as the rest of native code does, so a yield whose
  operand is an object beneath it no longer leaves for Go; the marking
  rule has no exception left. `TestJITGenYieldRecordsWhileMarking`, and
  `TestJITSameWorkWhileMarking` compares the records applied too; a new
  micro-benchmark, `generator_objects` (`cdfe563`), yields the nodes a
  generator walks.
- **The JIT cache's code ages by the program's time** (`3822dbc`): its
  clocks -- the epochs makeRoom ages code by, the flush's ticks -- left
  the compiles' time in, which runs on the program's thread, so the
  more the JIT compiled, the sooner code went unused for a second, was
  flushed, and was compiled again. They now run on the runtime's time
  less its compiles' (`jitState.running`), and are the runtime's own,
  not a base every runtime shared. Octane's Typescript alone, three
  runs of three: compiles 652, 659, 883 -> 580, 578, 580; flushed 70,
  77, 278 -> 6, 4, 6; every other suite's counts and live heaps the
  same or less. `-jitreport` counts the code flushed and the compiles it
  cost (`2bf51d4`).

Over four placements, against the base, with the JIT: V8 suite -0.1%,
Are We Fast Yet -0.7% (CD -4.3%, Havlak -1.4%), Octane -0.5% (Mandreel
-7.0%), micro-benchmarks -0.2% by their geometric mean, none slower at
every placement (`array_reduce` -11.1%). The aging alone is level
(Octane -0.4%, Typescript -0.3%): the placements take each build's
least run, which the flushes' bad runs never were. The interpreter's
code is main's: hotdiff finds the hot functions the same.

What the slower ones were, counted (`-jitreport`, SSA and code by
compile, thread cycles):

- Typescript +2.6% at every placement is the compiler's time: it
  inlines 50% more IR (20.9K -> 31.3K instructions at `-n 3`), its
  compiles 6 to 10% longer, compiles, exits and interpreter finishes
  level. Matched against every site's calls counted in an interpreted
  run, the extra inlined instructions are 6,078 at sites called less
  than 0.15 times a caller's invocation -- where V8 would not inline --
  3,150 from 0.15 to 1, 296 at 1 or more, and 2,277 at sites not
  counted (tail calls). Those counts given to the inliner, with V8's
  0.15, bring Typescript back to the base's compile time.
- PdfJS +1.2% (two placements of four) did not reproduce run alone
  (`-n 10`, fewer compiles than the base, 94 against 115); Splay's,
  `array_forEach`'s, `array_map`'s and the other micro-benchmarks' SSA
  and code are the base's compile for compile: layout.

Open from it:

- Call frequency by site, which Typescript's cost needs. No tier counts
  a site's calls: the property caches count fills, `jitCallSeen` only
  undecided calls, and a callee's invocations mix every caller's. A
  count in native code (a saturating counter at each call site outside
  loops, the frequency known after 400 returns) took the extra inlined
  IR down 14% and half the extra compile time, but cost as much as it
  saved, and inside loops `func_call` +11%; it is not kept. V8 takes
  the counts from its lower tiers: here the tree tier's call nodes,
  which are hot closures and need placements of their own. Done in the
  next round (`324bd1a`..`0b79a06`, below), from the frames the tiers
  below native code make.
- The cache's clock is still time, the program's, so one run of three
  still flushed 124 functions in Typescript. A clock of the program's
  work -- the back-edge budget's units, in every tier -- runs from 34 a
  millisecond (Towers) to 200,000 (Mandelbrot): at the slower rates
  zlib's code and heap outlived it (Octane's live heap 229 -> 350 MB),
  at 64K units a tick Octane flushed 968 for 566. It needs native
  code's calls counted into one counter of the runtime's, on both
  architectures. A compile's lowering before `compileSSA` (3.5% of its
  time) still counts toward the clock.

A third round of 2026-10-28 (`9df3b5e`..`0b79a06`, against `9a4375c`;
four Go code placements with the JIT, the micro-benchmarks on each
placement's binary, the least of three), the first item above:

- **Call frequency by site** (`324bd1a`, `b148f70`), as V8's lower
  tiers count it in a function's feedback: while a function waits to
  be compiled -- warming, or its code invalidated -- every call its
  frames make is counted at its site, and its invocations with it.
  `tryJITFrame`, which every framed call of the interpreter and the
  tree tier reaches (constructions, accessors and built-ins' callbacks
  among them), counts it; `pureCallAt` and `leafForward` count the
  calls they answer without a frame, and a call through
  `Function.prototype.call` or `apply` is counted at the site that
  calls them. Native code counts nothing: a counter in it (the round
  before) cost what it saved. The counting is under `jitBuilt`, so the
  build without the JIT has main's text, every symbol's name, size and
  address; a caller that does not count pays an inlined check of its
  template's flag, about 24 instructions a call.
- **Inlining ordered and cut by frequency** (`b1dfaf5`, `0b79a06`), as
  JSInliningHeuristic takes its candidates: a call made less than 0.15
  times an invocation (`min_inlining_frequency`) is not inlined, small
  or not, and is made as a native call; the most frequent are inlined
  first. A frequency is known after 19 counted runs, the fewest in which
  a call at 0.15 is seen at least once 19 times in 20; a call whose
  frequency is not known is ordered as before the counts, once an
  invocation outside loops and more than any count inside one. A call
  inside an inlined callee is inlined however seldom it is made: V8
  leaves such a call a call, which an inlined frame here does not make.
  `-jitreport` counts the callees inlined and their instructions
  (`9df3b5e`).

Typescript inlines 18,602 instructions where it inlined 30,802 (the
count rule before the size rule, 20.9K); Octane's code is 55.6 MB, not
58.1, and its allocations 16.4 million, not 17.0. The first placements
found RayTrace 21% slower: its constructions, all of Class.create's
wrapper, were answered by `leafForward` without a frame, so not counted,
and ordered after every counted call; the wrapper, inlined no more, was
refused on its own ("arguments object"). Counting them, and ordering
the unknown as before, made it level.

Over four placements, against the base, with the JIT: V8 suite -0.6%,
Are We Fast Yet +0.8%, Octane -0.2% (Typescript -2.4%),
micro-benchmarks +0.3% by their geometric mean.

What the slower ones were, compile by compile (`-jitreport`, `-jitcode`)
and with the cut compiled out of the same commit:

- Richards (+1.2% at every placement) is the cut: `runTask`'s
  `packetPending`, made 0.129 times a call over a whole run, is a
  native call, 2.5 million a run; without the cut Richards is -0.2%.
  V8's rule cuts it too, but a native call costs more here than a call
  in V8's code: a cheaper native call (open above) would win it back.
  Never cutting small callees instead took Typescript's inlined
  instructions back to 28.2K.
- Havlak (+2.0%), zlib (+1.9%): two and one calls below the cut, which
  cost nothing measured, the cut compiled out or not; rerun, Havlak
  +1.0% from one placement, the session's noise about 2%. Json and
  `array_reduce` (+12.2%) compile as the base does, compile for
  compile: layout. Main with package vm moved to the branch's addresses
  (pads in ssa and after `vm_leaf_pure.go`) is not slower, so it is not
  vm's offset against the packages before it either.
- Octane's live heap after a run is bimodal on both builds, about 230
  or 360 MB, two runs of six each.

Open from it:

- Frequencies are the warm-up's: code compiled again for what it
  learned uses the counts its function had before it was native, and a
  call never made while it warmed is not known, not cold.
- Leaf bodies other than pure ones, answered without a frame, are not
  counted; a tail call past depth 32 is counted at its caller's caller.
- `jitCalledOftenBy`, which compiles a long callee early for its caller,
  still judges by the callee's invocations, not the site's count.

A fourth round of 2026-10-28 (`d0acb93`..`599e74d`, against `cf4e6f2`),
from the JIT's differential fuzzer: a program squaring a BigInt 24
times, then writing it out, never finished. `String(x)` of its 19.5
million bits took 54 s, every algorithm in `internal/bignum` past
Karatsuba being quadratic, and no deadline could stop it: one Go call
did all of it. V8's `src/bigint` is the model throughout:

- **Interruption** (`d0acb93`, `241ed67`), as V8's `ProcessorImpl`
  checks `should_terminate`: an operation of 256 words or more checks,
  about every 4 million word operations, the runtime's context and its
  memory limit, and returns the error the VM throws for them. A
  deadline stops `String(x)` of 20 million bits within 15 ms. Shorter
  operations take the paths they took (`0393d3c`).
- **Multiplication:** Toom-Cook 3 above 200 words (`4000577`), its
  scratch from one stack, and Schönhage–Strassen above 2,500
  (`7c7cf11`).
- **Division:** Burnikel–Ziegler above 57 words (`15f4eb9`), and
  Barrett division with a Newton inverse for divisors of 13,310 words
  and more (`7819c6d`).
- **Conversions:** `toString` (`ced0de0`) and parsing (`52b7020`) by
  halves on the fast division and multiplication, the reciprocals of
  the powers kept for the conversion; a power-of-two base is linear.
  Intl's format of a long BigInt converts by the checked path
  (`23d3268`).
- **The fuzzer** (`83fbb08`): a target that warms programs as programs
  warm -- the default thresholds, then 24, then 24 under stress, each
  run 40 times -- and every run bounded in time, the interpreter's
  outcome skipped when it runs out and any other tier's a failure. The
  program above is in both targets' corpora; it passes in 0.16 s.
- **A BigInt written out past 2^30 bits** is refused as V8 refuses it
  (`8fd6ba0`): a string, by V8's StringToBigInt, past 2^24 parts of as
  many whole digits as fit in 64 bits, leading zeros not counted, is no
  BigInt (a SyntaxError, a comparison false); a literal, by V8's
  scanner, of more than 2^28 characters after its prefix, separators
  and leading zeros counted, is an early SyntaxError. BigInt's
  SyntaxError quotes at most 1,000 code units of the string
  (`599e74d`): refusing one of 2^28 digits took 2 s writing them out.

The program above, ours against Node (24 squarings, 19.5 million bits):
multiplying 159 ms against 158 (1,694 before), `toString` 1,674 against
1,827 (54,244 before), parsing its output 743 against 735.

Over four placements, against the base: without the JIT, V8 suite
+0.5%, Are We Fast Yet -1.0%; with it, -0.5% and +0.2%;
micro-benchmarks +0.1% and -0.2% by their geometric mean. The BigInt
micro-benchmarks and `string_slice` and `string_upper`, up to 13% slower
on one build, are level with main padded to the branch's addresses;
`async_await` (+18%) and `async_call` (+10%) are two thirds the tree
tier's closures moved, about 4 KB later, and the rest the same code,
every function in their profile the same size.

Open from it:

- The FFT's scratch memory is not counted against the memory limit.
- Linear operations (addition, shifts, bitwise operations) are not
  interruptible.
- `async_await`'s remaining 6%, of layout.

Open from it:

- Typescript, +4.0% at every placement, before its sites were fixed
  (above): about 10% more runs finished in
  the interpreter, at sites that inlining reaches more often -- a
  polymorphic call (`visible`'s `isMember`) learned one target per
  recompile, an inlined constructor's writes that add properties
  exiting at each new receiver's shape (`ASTSpan`), a keyed read
  (`comparisonCache[comboId]`) and a length write in inlined code.
  Learning all of a call's targets at once, and inlined writes checking
  several shapes, would serve every program.
- Calls an inlined callee does not inline are refused, not made from
  its frame: making them (native calls, calls of Go, tail calls) took
  Towers' and Richards' calls further down but made DeltaBlue, Havlak,
  Typescript and the V8 suite's short runs slower, an inlined copy
  having no feedback of its own and a call from it that leaves native
  code sending its caller to the interpreter.
- Callees with loops are not inlined: inlining Crypto's `am3` made its
  callers' code 60% larger and Crypto 25% slower. A rule that inlines a
  loop only where the call's cost is not spread over the loop's turns is
  wanted first. (Where the loop calls a closure the caller gives, the
  round of 2026-10-28, above.)
- A call is still an indirect jump each way: native code runs on the
  goroutine's stack and never moves SP, so that Go's unwinder, stack scan
  and growth meet no return address of its, and a return is not
  predicted by the return stack. Most of a call's remaining samples are
  just after one of the jumps, but that is the sequence's latency landing
  there: measured apart (four call sites of one leaf, Go assembly, a
  `CALL` to a stub that pops the return address into a register and
  jumps, the leaf returning by `JMP R11`, against `CALL` and `RET`), an
  unpredicted return costs 0.9 ns a call where the sites follow one
  another and 1.5 ns where they are picked at random -- at most a tenth
  of a native method call's 13 ns. A stack of native code's own, which
  `CALL` and `RET` need, is not worth its risk for that. 7 registers
  spill `moveDisks`' parameters at entry.

Step 2 below, done since (2026-10-22): the array methods' callbacks
inlined in the loops native code makes of their calls. A callback that
is the one closure the loop has been given is inlined, the loop checking
it once; one the caller makes just before the call -- `a.forEach((x) =>
...)`, the usual form -- is inlined and never made: it reads and assigns
what it captures as the caller's own slots, and an exit's frame state
holds its template where the closure would be, which Go makes the closure
from (the VM's `jitMaterialize`), as V8's deoptimizer materializes what
escape analysis removed. Counted with `-jitreport` and the runner's
allocations, a run of each micro-benchmark (`-n 1`): the callbacks of
`array_some`, `reduce`, `map`, `filter`, `forEach` and `typed_forEach`,
called natively 1.27 to 2.91 million times, are called no more; the runs'
allocations fall from 54,000 to 24,500 (`some`, `forEach`), 50,200 to
20,600 (`reduce`), 70,100 to 57,100 (`map`), 119,300 to 106,300
(`filter`) and 52,400 to 25,000 (typed); `jitprof` over the six (`-n
10`), 390 native samples to 111, the calls' and call entries' 121 to
none. Are We Fast Yet's NBody: `advance`'s callback, called natively
1,249,505 times a run, is inlined, the run's allocations 504,000 to
252,000 (85 MB to 8.6 MB). Havlak, Json, CD and Octane's Box2D, PdfJS
and Typescript compile, enter and leave as before: Havlak's and
DeltaBlue's forEach are SOM's `Vector.forEach`, a script method with a
loop, whose callbacks' closures (`newClosureIn`, 9% of Havlak) are made
still.

Open from it:

- The context: a function whose locals the callback captures takes their
  cells at each entry still (`ssa.GoCells`), one allocation a call where
  the closure had been a second. Not taking them needs them made at an
  exit too, where a closure made there that escapes would not share them
  with the native code that goes on: step 3.
- The loop's index and the callback's result are boxed at each turn
  (`boxf`, a fifth of the six's native samples): the loop's phis are not
  unboxed.
- `closure_callback` and Havlak give closures to script functions with
  loops, which callees with loops not being inlined keeps out.
- A callback made at the call whose this argument, or reduce's initial
  value, is more than a local's copy (`a.forEach(f, this.x)`) is made,
  called natively by its code, not inlined.

Step 3 below, begun since (2026-10-23; `5dc9733`..`6cd7c96`, counted by
the runner's allocations with `-jit`, a run each): escape analysis as an
SSA pass (`ssa/escape.go`), TurboFan's. An object literal's object, or
the receiver of a construction the code inlines (a class's constructor
returning its this included, `5dc9733`), that nothing but the code sees
is not made: its properties are SSA values, its field reads those values,
and the checks its making made of the heap stay (the constructor's
prototype, deoptimizing once replaced; an addition's prototypes; a
method's holders). An exit's frame state names it by a word no value has,
its shape, prototype and properties in a home past the frame's slots, and
Go makes it there, one object for every slot naming it (`f3eaa79`).
Octane's RayTrace 86,456 -> 74,915 allocations (16.6 -> 10.9 MB), Box2D
91,273 -> 82,886 (21.3 -> 16.8 MB); the V8 suite's RayTrace 85,101 ->
79,108; Are We Fast Yet's CD 3,260,567 -> 3,211,100. The handler in force
is now an exceptional successor of what it guards in the compiler's
liveness, so a try statement's frame keeps only what its handler reads
(`457e0e6`). A native call whose closures are all callbacks it never
makes gives its context's cells back to their pool as it returns
(`9b86e46`): NBody 252,301 -> 5,558 allocations (8.4 -> 0.9 MB). A
closure the code makes and only calls -- `const sq = (x) => x * x + n` in
a loop, a function expression made and called in a loop's turn, a nested
function declaration -- is found in a first build's SSA, inlined at its
calls with no check of the callee in a second, and not made, Go making
it at an exit (`6cd7c96`): `closure_make`, `closure_counter`,
`closure_callback` and `func_closure_call` 3,973,442 -> 2,382,168
allocations (705.6 -> 272.4 MB). test262 under `-conformance.jit`, plain
and with `threshold,budget=3,deopt=7`: 99,637 passed, none failed.

Open from it:

- Havlak's and DeltaBlue's closures (915 MB of Are We Fast Yet's
  allocation) and their contexts' cells: given to script methods with
  loops (SOM's `Vector.forEach`), which callees with loops not being
  inlined keeps out.
- An iterator's `{value, done}` and a destructuring's array: native code
  makes no iteration protocol of its own, the step a call of Go
  (`ir.IterStep`), which sees the result.
- An object the code reads an absent property of, tests for truth, or
  compares with `===` or `typeof` escapes; so does one live across a
  loop's header, and one given to a call not inlined -- CD's vectors.
- A closure whose body calls another closure it captures needs its
  calls' feedback, which a template's function has only once it runs
  natively on its own; and a closure only called, made in a loop's turn
  over a per-iteration binding, still takes the binding's cells at each
  turn (`closure_make`'s remaining allocations): a scope's end gives fresh
  ones unless the frame is a native call's, as Go may have made closures
  on the old ones at an exit.

The plan, in order -- the compiler first, since micro-optimizations do not
close a gap of three to seven times:

1. **What native code spends a loop's turn on.** Count the instructions
   of a turn of Towers', Richards' and List's hot loops against V8's
   optimized code for them (`--print-opt-code`), and sort ours by what
   they do: tag checks, shape checks, loads of the context, spills and
   reloads, write barriers, polls, calls. Then fix what is largest, in the
   general mechanism: shape checks taken out of loops (a loop header knows
   no shapes today), int32 and float64 values unboxed across a loop, the
   native call's cost, inlining deeper and of polymorphic sites.
2. **The array methods that take a callback, with the callback inlined**
   into their loop, which also stops the callback's closure being made
   (Havlak).
3. **Escape analysis**: an object, a closure, an iterator's result, a
   destructuring's array or a small value object that does not outlive
   the code that makes it held in registers and slots, not allocated.
4. **Large programs**: functions past 4,096 instructions compiled, or at
   least their loops; fewer recompiles and smaller code (52 KB a
   function in Typescript); function bodies compiled when first called.
5. **Calls of Go for built-ins** through a trampoline of their own (step
   3 of the leaf calls, under the JIT's items below). Done 2026-10-27
   (`375a3bd`..`54c1dbb`, above).

The interpreter, which every build without the JIT runs, is behind C
QuickJS on Octane (1.10 times its time) and Are We Fast Yet (1.08): Are
We Fast Yet's DeltaBlue (1.29) and Havlak (1.23), Mandreel (1.24),
Typescript and CodeLoad (1.20) -- calls, allocation and the compiler's
own speed. It is a track of its own beside the JIT's.

## F. Server-side rendering and the collector

A React 18 server-rendering bundle of 117 KB, rendering one page per call,
is the workload public comparisons of Go engines lead with, and the one a
host embedding this engine behind a web server runs. Measured 2026-10-30
on the Ryzen, fresh processes, the steady-state time of one render (the
time of 2n renders less the time of n, so neither the process's start nor
the bundle's load counts), median of five rounds:

| Engine | wall a render | CPU a render |
|---|---|---|
| go-quickjs, interpreter | 14.2 ms | 18.8 ms |
| go-quickjs, JIT | 17.0 ms | 15.6 ms |
| C QuickJS 2026-06-04 | 12.0 ms | 12.0 ms |

So the interpreter's own work is close to C's -- 1.18 times its wall time
-- and its CPU is 1.57 times, all of the difference the collector's
threads. A host that gives each core a runtime pays that CPU; one with
cores to spare does not. It is the figure the comparisons report (they
run with `GOMAXPROCS=1`, where the collector's work lands in wall time
too), and it is the gap to close.

A render allocates 1.8 MiB in 16,500 Go objects. Go's collector costs
time in proportion to the allocation rate, so the CPU over wall follows
that figure and nothing else; the live heap after a render is 9 MB.
Where the 1.8 MiB goes, from an allocation profile of 400 renders:

| Site | Share | What it is |
|---|---|---|
| `pureElemOp` | 25% | `arr.push(v)` growing an array's elements |
| `String.Concat` | 21% | a rope node for each of ~8,200 concatenations |
| `newLiteralObject`, `newLargeLiteralObject` | 22% | the objects the program makes |
| `newArrayStorage` | 10% | the arrays the program makes |
| `newForInObject` | 8.5% | a cursor for each of ~780 `for...in` loops |

Three quarters of it is the program's own objects, strings and arrays,
which C QuickJS allocates too and frees by reference count. What is ours
to remove:

| ID | Item | Targets | Risk | Status |
|---|---|---|---|---|
| F1 | A string builder in the tree tier | 21% of a render's allocation, string building 1.13x C | High | Open |
| F2 | A `for...in` cursor from a free list | 8.5% of a render's allocation | Medium | Open |
| F3 | An array's old elements reused when it grows | up to 14% of a render's allocation | High | Open |
| F4 | The JIT slower than the interpreter here | 1.20x the interpreter | Medium | Done |
| F5 | A fresh runtime's first render and its memory, with the JIT | first render 66 ms against 22, live heap 26.2 MiB against 7.3 | Medium | Memory done; first render measured, documented, not fixed |

- **F1.** `s += x` makes a rope node an iteration, 48 bytes, and the JIT
  already avoids it: a loop whose appends are the only readers of the
  string grows one buffer in place (`zzjit_builder.go`, `ssa.GoAppend`),
  frozen at every exit to Go. The tree tier has no such analysis. It
  needs the same property proved over the bytecode -- every read of the
  local is the left side of an addition stored back to it, any other read
  freezing the builder first -- and nothing may hold the string meanwhile.
  It is the largest single item here and the one that can go wrong
  silently, since a string that is mutated after something else has seen
  it is a wrong answer, not a crash.
- **F2.** A cursor is one allocation of the object, the cursor and the
  snapshot together (`newForInObject`), and nothing outside the VM can
  reach one: a cursor the step reports done could go on a free list of
  the runtime's and be taken by the next loop. The reasoning to get right
  is that the step reporting done is the last read of it -- a loop left by
  `break`, an exception or a suspended generator must not return a cursor
  to the list.
- **F3.** Go's `append` allocates a new block and copies, so about half
  the bytes a growing array allocates are the blocks it has outgrown,
  dead the moment they are copied. A free list of element blocks by size
  class would take them back. The risk is that anything holding the
  slice across a growth -- `o.elemsOf()` passed to a helper that appends
  -- would see reused values rather than stale ones, which is a wrong
  answer rather than a crash. A capacity floor on the first growths was
  tried first and is not the answer: the growth here is in large arrays,
  and floors of 4, 8 and 16 elements left the render's allocation count
  unchanged to the object (2026-10-30).
- **F4.** The JIT is 1.20 times the interpreter's wall time on this
  workload, which is mostly short polymorphic calls and string building
  rather than loops, and a JIT runtime holds 18.7 MiB against the
  interpreter's 2.0. Profiled 2026-10-30, after 300 warm renders so that
  it is the steady state and not the warm-up (14.3 ms against 17.1): the
  glue between the tiers costs more than native code saves. In seconds of
  samples over 600 renders -- interpreter build first, JIT build second --
  the interpreter and tree tier take 5.00 against 1.36 and other VM code
  2.43 against 1.76, so native code takes over about 3.6 s of work and
  does it in 2.2; but the glue around it takes 3.71, of which
  `jitGoCallNested` is 1.48 flat, about 190 ns a call rebuilding the level
  frames. A render crosses the boundary about 27,500 times into native
  code and 13,200 out of it. What those calls out are: 3,652 calls of
  compiled closures the profit model marked slow, because it judges each
  function alone and so splits the renderer's recursion into a native half
  and a slow half with every call between them going through Go; 1,431
  `Function.prototype.call`; 1,404 `RegExp.prototype.exec`; 802 calls of
  native closures called through Go anyway. On top of those, a `for...in`
  loop costs one host operation at its start and one Go call for each key:
  native code has no `for...in` of its own (`compile/lower.go` makes
  `OpForInStart` a host operation, which "sets where native code goes
  on"), and each step is an `ir.IterStep`. It does not abandon the call to
  the interpreter -- native code resumes after the start, and the 571 a
  render counted at one function's `for_in_start` are the native stretches
  that end there, not calls given up.

  So, by what each is worth: `jitGoCallNested` is 2.5 ms of the 3.0 ms a
  render the JIT is behind, so a cheaper native-to-Go call with native
  levels on the stack is first. `for...in` over ordinary objects compiled
  natively comes next -- the start a Go call rather than a host exit, and
  the step native while the cursor walks a key snapshot of the object's
  own layout and the object still has it, which is V8's ForInNext map
  check against the enum cache, the snapshot being the one `shapeKeys`
  already keeps with the shape. It is worth a few tenths of a millisecond
  here, and more wherever a program walks objects, since every key costs a
  call of Go today. Last, the profit model judged across calls, so that a
  callee judged slow counts the Go calls it imposes on its native callers
  and a recursion is wholly in one tier: that starts from `a21df702`, held
  off main, which has the calibration (`TestJITCalibration`,
  `QUICKJS_JIT_CALIBRATE=1`) and charges a callee's exits to the callee,
  and whose open problem -- Splay, Json and RegExp slower -- is the
  acceptance bar.

  **Done**, the first of those: a call of Go from a native callee makes
  only the calling level's frame and leaves room for the levels below it,
  which `frameAt` fills from their contexts if a frame among them is read
  -- a stack trace, a function's caller, a generator's search for its run,
  the memory meter's walk -- as V8 walks its optimized frames only when
  asked (`ae764285`). The render's JIT time went 17.2 ms to 15.5 against
  the interpreter's 13.9 and C QuickJS's 12.0, about half the gap; four
  placements with the JIT put the V8 suite at -0.4%, Are We Fast Yet at
  -0.3% and Octane at +0.2%, every benchmark within its spread but
  EarleyBoyer (-2.3% and -2.7%) and Json (-2.3%). Instrumented runs of the
  three suites put frames off and built none of them, so nothing there
  pays for the check. The frequency counting that reads two and three
  frames down never forces the build: it skips a call made through Go, and
  the deeper read lands exactly on the calling level's own frame.

  **Done**, the second: a `for...in` over an object of a shared layout
  steps natively (`036a5248`). Its start is a call of Go rather than a host
  exit, and the step is native while the cursor walks the layout's keys and
  the object still has that layout, which is V8's ForInNext against the
  enum cache, the keys being the snapshot `shapeKeys` keeps with the shape.
  A delete or an attribute change takes the object off a shared layout, so
  the check fails and Go steps it, which is what makes it safe to skip the
  Go path's per-key lookup; a dictionary stays Go's. `object_for_in` went
  43.5 ns a key to 26.3, where the interpreter takes 36.3 -- the JIT build
  was slower than the interpreter at this and is now faster -- and
  `dict_for_in` is unchanged at 166. Four placements with the JIT: Octane
  -0.4% (PdfJS -2.7% at every placement, it uses `for...in`), the V8 suite
  +0.5% and Are We Fast Yet +0.4%, both inside their placements' spread.

  The render's JIT time went 15.8 ms to 14.2, against the interpreter's
  13.8 and C QuickJS's 12.0, which is far more than the per-key calls
  promised: the exits a render fell from about 2,430 to about 570, and an
  exit and resume at a loop's start costs about 0.8 us here. So the two
  items together took the JIT from 1.23 times the interpreter on this
  render to 1.02. What is left for the profit model is to make it faster
  than the interpreter rather than level with it, and the JIT's own
  published costs -- a first render 3.8 times the interpreter's and 18.7
  MiB a runtime against 2.0 -- which compiling less should serve.
- **F5.** Measured on this machine, one fresh runtime loading the bundle
  and rendering, and 20 of them for the memory:

  | | interpreter | JIT |
  |---|---|---|
  | first render | 22-24 ms | 66-71 ms |
  | second render | 15 ms | 80-82 ms |
  | steady state | 14.1 ms | 13.6 ms |
  | allocated a render | 5.1 MiB | 26.5 first, 44.0 second |
  | live heap a runtime | 7.3 MiB | 26.2 MiB |

  The second render costs more than the first, and the first two together
  spend about 100 ms compiling to win 0.5 ms a render: on the order of 100
  renders to pay back, and a single render can never pay back at all, since
  the compiling alone (36 ms) costs more than the interpreter's whole first
  render. So a host that makes a runtime for each request is paying for the
  JIT and getting nothing, which is what our documentation has to say
  plainly rather than leave to the reader's arithmetic.

  Where the first render's 32.6 ms of compiles go: the SSA and the backend
  (build 8.6, optimize 8.4, the calls' saves 0.5, emit 5.8, encode 2.0) and
  4.3 in lowering, over 77 functions, 58 compiles and 5 again. Two functions
  are 46% of it: the renderer's element and node functions, 1,734 and 729
  bytes of bytecode, 8.3 and 7.0 ms. The second render's 55 ms is almost one
  function: a tag-name switch of 2,982 bytes, each arm with a `for...in`
  over its props, compiled twice at about 22 ms -- once, then again after a
  single deoptimization -- for 352 KB of code, and it runs about 440 times a
  render. Compile cost grows faster than bytecode size, 4.3 us a byte for
  the first of those functions and 7.2 for this one. (Figures measured in a
  test read high at first: `internal/vm`'s `TestMain` sets
  `mir.CheckAllocations`, which charged the calls' saves 4.5 ms rather than
  0.5. Time the compiles outside that, or with it off.)
  Native code's collector share is visible too: 20 fresh first renders take
  1.81 s of CPU with the JIT against 0.63 without, of which the mark
  workers are 0.36 s, about 18 ms a render on other threads, where in the
  interpreter's profile they do not reach the top 200 lines.

  Where the 19 MiB a runtime goes: 12.58 MiB is compile workspaces, which
  grow to fit the largest compile and stay, and which the 8 MiB budget does
  not count at all -- so `WithJITMemoryLimit` bounds 6.5 MiB while the
  runtime holds 12.6 it never sees, which makes this as much a fix for that
  option as for memory. The counted 6.51 MiB is lowerings 3.29 (kept after
  the compile; the switch function's alone is 1.05 MB), entries 2.07 (code
  0.95, footprint 1.50, learned 0.38) and 1.14 for 64 shared contexts.
  After the first render alone the figures are 2.36 and 4.28 MiB.

  **Done**, the workspaces: a compile takes the memory it works in from a
  pool the process shares between its runtimes and gives it back once its
  code is placed, so a runtime holds none between compiles, and the pool
  lets one go after two of the collector's cycles unused (`d4d57347`). Over
  20 runtimes each loading the bundle and rendering twice, the live heap a
  runtime went 25.5 MiB to 12.2, against the interpreter's 5.7; after one
  render, 12.3 to 9.7. The steady render is unchanged and the placements
  flat (V8 -0.1%, AWFY -0.3%, Octane -0.4%, its Typescript -0.1%, so
  pooling costs no compile time). The pool is reported as the process's
  figure (`JITStats.Pooled`) rather than counted against a runtime's
  budget, which is deliberate: counted there, it had code dropped to make
  room for it. `WithJITMemoryLimit`'s documentation says what the pool is
  and what it does not count, where it had described the old per-runtime
  workspace.

  **Declined**, the lowerings, measured rather than assumed: of the 3.29
  MiB, only 1.8 is freeable at all -- the own-code programs are held by
  their entries' code anyway -- and dropping them costs about 10 ms a
  render. Re-lowering every inline request, which is what dropping means,
  took lowering from 3.3 ms to 25.0 over a load and two renders, the first
  render from 61-64 ms to 73.6, the second from 75-80 to 88-91, and added
  9.8 MiB to the first render's allocation. They are already inside the
  budget, and `makeRoom` drops the ones of functions with no entry before
  it drops any code, so they go first under pressure and earn their keep
  without it. If this memory matters later, the lever is `ir.Instruction`
  at 136 bytes -- 83% of a program's bytes -- where packing the operands'
  slots to `int32` is about 18% and literals as indexes into a constant
  table would roughly halve it; an ir-wide change, not worth it before the
  two policy mechanisms.

  **The tier-up's units do not measure time**, which decides what is worth
  doing here. The budget charges `len(fn.Code)` an invocation and a loop's
  length a back edge, and the interpreter's time for one of those units is
  2.1 ns on this render, 4.5 on Richards, 15 on DeltaBlue and 10 to 18 on
  RayTrace: a sevenfold spread. So a break-even rule in those units -- spend
  no more compiling a function than the interpreter has spent running it --
  is the call-count threshold of [Rejected](#rejected) with a size factor,
  and would delay Richards-like code for nothing while under-protecting
  this one. The overcount falls hardest on the functions this item is
  about: a 2,982-byte switch is charged its whole length for a call that
  runs one arm, and V8's charge (a Return's bytecode offset) would charge it
  the same, since its common tags break to the generic path at the end. What
  V8 can afford there and we cannot is the compiler behind the count: we
  tier up at V8's Maglev invocation count (400) with a compiler costing 4 to
  7 us a bytecode byte, which is TurboFan-class.

  **And policy cannot fix it.** With the tier-up warmed later --
  `SetJITWarmUp` at 400, 1,500, 3,000 and 10,000 -- the first ten renders
  sum to 315, 268, 250 and 197 ms against the interpreter's 152, and the
  steady state is 13.8 ms either way. Later compiles spread the cost and
  never remove it, because the functions are hot across renders and compile
  in the end. A measured per-function benefit would let the same 100 ms of
  compiling be scheduled more cleverly; the problem is that the 100 ms
  exists. A sampling profiler to measure it (a ticker poking the check
  counters, V8's old runtime profiler) was therefore declined as well, on
  that ground and three others: a 1 kHz ticker a runtime is a poor bargain
  for a host running one a core or a request, tiering would stop being
  reproducible where the differential fuzzer and every placement comparison
  need two runs to compile the same functions, and it would be off under the
  race detector, so that the detector ran a different policy from
  production.

  So, next: the deoptimization half of the policy, which is where the profit model comes back in scoped form --
  which does not need a benefit measure: a recompilation after a
  deoptimization waits in proportion to the measured compile time of the
  code that deoptimized, so that a 22 ms compile waits far longer than a
  0.3 ms one. That is what makes the second render dearer than the first,
  and the cost side of it is measured exactly rather than counted.

  **There is no superlinear phase.** Over 1,057 compiles from this render,
  Typescript, Box2D, PdfJS and RayTrace, every phase is flat or falling per
  bytecode byte, and compile time grows as size^0.82 in bytecode and
  size^0.87 in emitted code. The 4.3-against-7.2 us a byte that suggested
  one was noise between compiles: the same function varies twofold, 15.7 to
  23.9 ms for that switch and 10.0 to 16.5 for one of Typescript's. What
  there is instead is a large constant an instruction -- about 1 us to build
  and 1 us to emit, 1.5 to optimize, 5 in all -- and, behind it, the figure
  to attack: about **120 bytes of machine code a bytecode instruction**,
  156 in Typescript, 168 in PdfJS, and 352 KB for a 2,982-byte switch whose
  few arms run. Compile time follows the code emitted, so cheapening the
  compile means emitting less:
  1. An arm its feedback says never runs compiled as an exit rather than as
     code, as V8 compiles a cold branch. Measured by code bytes an
     instruction and compile time, on that switch and across the five
     workloads.
  2. A first compile without the optimizer: **measured, and the answer is
     no.** Every level ran in one binary switched by an environment
     variable, so no layout difference stood between them. Without the
     optimizer's rounds the code grows 15 to 30% and compile time falls 0 to
     13% -- on Typescript it rises 9% -- because the optimizer is not a cost
     on top of the backend but what makes the backend's input small. The
     same r=0.96 from the other side. Never re-tiered, that code keeps 59 to
     85% of the speed (Octane +41%, Crypto and NavierStokes twice as slow);
     re-tiered to full once hot it keeps the speed (Octane +0.6%, AWFY
     +4.3%) but makes the first render worse, 80-85 ms against 63-66, since
     hot functions then compile twice inside it. One optimizer round without
     escape analysis is a side finding -- about the same speed for 4 to 19%
     less compile time -- which is optimizer tuning, not a tier. If a tier
     is ever built, two things found here wait for it: a function only ever
     called natively never passes `jitLearnedDue`'s checks and would stay
     cheap for ever, and thirteen tests assert compile counts, deopts and
     escape analysis rather than answers.
  3. What a compile reads that the runtime mutates, which is what compiling
     off the runtime's goroutine would need, as V8 compiles concurrently:
     **the remaining structural option, not started.** It would put the
     first render at about the interpreter's 22 ms, since the render would
     run interpreted while compiles happened elsewhere. The mapping is about
     a day -- feedback and caches, shapes, the code cache's room accounting,
     the lowerings and links, the workspace (already pooled) -- and the
     build is far more, against the engine's rule that a `Runtime` is not
     safe for concurrent use. A deliberate decision for the project, not
     something to drift into.
  **What those 120 bytes are**, by mir's annotation marks: the ops' inline
  code is 52 to 59% of a compile's text, cold paths 23 to 36%, and edges,
  exit stubs and shared routines the rest. Three ops are about half of it --
  on this render, `call` 28.7%, `eqtagged` 20.4% and `keep` 10.5%, and on
  Typescript 22.8%, 12.5% and 13.5% -- where a call is about 520 bytes a
  value (320 inline, 185 cold), a tagged equality 410 to 600, and a keep
  only 110 but numerous: 2,768 of them on this render, 18,541 in Typescript.
  The 2,982-byte switch has the same profile, its tag-string comparisons
  about 1,300 equalities at some 450 bytes each. Box2D is the exception, its
  property ops leading (`propwrite` 12.9%, `propcell` 8.3%, `objectof`
  6.9%, `propread` 6.6%), so whatever is done must be judged on it too.

  So the item is those ops' expansion rather than cold arms in general --
  and which part of each, the taken rates settle. Counted in native code
  over 40 renders and the suites' runs:

  | slow path | taken |
  |---|---|
  | `eqtagged`'s string compare | 85.4% here, 89.5% PdfJS, 0.7% Typescript, 0 Box2D |
  | `call`'s Go-made path | 0.4 to 4.1% |
  | `keep`'s marking copy | 0.4 to 1.3% |

  So `eqtagged`'s cold path **must not** be moved out of line: on the
  string-heavy workloads it is the main path, and a call on 85 to 90% of
  executions would make the very function this is about slower while making
  it smaller. Its lever is the string comparison itself, which needs a look
  at what interning guarantees first; a constant operand's known length
  would settle many, but a literal is only 1.5% of Typescript's comparisons
  and 10% of this render's. `call`'s Go path, 180 bytes a site and truly
  cold, is one shared routine per function taking the site's index -- about
  9% of this render's code and 7% of Typescript's, for one jump on a few per
  cent of calls. `keep`'s marking copy is the same shape and much smaller.

  `keep` itself is a count question, not an out-lining one, and the count
  is the finding: **27 to 41% of all keeps are redundant** -- 762 of 2,768
  here, 18,592 of 52,497 in Typescript, 1,459 of 3,554 in PdfJS -- each a
  keep of a value already kept into the same cell, writing a word identical
  to the one the cell holds. `keepAcrossStores` gives a value one cell for
  every store that keeps it, and `reconstruct` rewrites a later keep to take
  the dominating kept copy, so the later keep copies the cell's own pointer
  back into it. Dropping it changes no word anything reads -- Go's barrier
  is needed only where a pointer slot changes, so it is safe mid-cycle too
  -- which makes it a pass after `reconstruct` with no cost per execution,
  about a tenth of a compile's code. What the pass must assert rather than
  infer: that the kept copy dominates the re-keep, and that nothing but
  keeps of that value writes its cell.

  **Done** (`05690650`), with both of those checked rather than inferred --
  the dominator tree and the order within a block, and one predicate
  (`ssa.Value.WritesKeepCell`) that the pass and `colorKeeps` share, so a
  writer added to either is seen by both; a re-keep that fails either check
  is kept, and the `quickjs_verify` build panics. Re-keeps went to zero
  everywhere, and the code shrank 3.3% on this render, 5.9% on PdfJS, 1.0%
  on Box2D and 1.5% on RayTrace. Compile time and the renders are level,
  and the placements flat (V8 +0.4%, AWFY -0.1%, Octane +0.0%): keeps were
  about a tenth of the code, so this is code size and memory rather than
  time. An earlier "2 MB of Typescript" was a six-iteration run's count
  multiplied; Typescript cannot be compared this way at all, its compiles
  varying 648 to 700 between runs of either side.

  `call`'s Go path turned out **not** to be a shared-stub item and is
  parked: the call into Go is already shared per function (about 20 bytes a
  site), and the remaining 180 is `goArg` per operand -- the number word, a
  reference test, the pointer read from the operand's own origin -- which no
  routine keyed by site index can share. Removing it means having Go read
  the operands from the call's records instead of `OffGoArgs`, and the jumps
  to `goCold` happen before those records are written, so it is a reordering
  of `call` and a change to what `jitGoCall` reads, across mir and vm: a
  protocol change worth about 30 bytes a site, to be sized against `call`'s
  335 to 356 inline bytes before anyone starts it.

  **Recompiles are 40 to 65% of all compile time**, which qualifies "policy
  cannot fix it" above: that holds for first compiles, not for these. By
  reason, total compile time: this render 131.8 ms, of which first compiles
  are 52.1 and recompiles about 80 (resume 23.8, learned and deopt 20.8,
  learned with inline and unwind 17.4, deopt 6.4, inline 5.7); Typescript
  520.6 ms, of which deoptimization alone is 228.9 over 127 recompiles;
  Box2D 101.8 with 38 in recompiles; PdfJS 112.2 with 65. So the
  deoptimization half of the policy is worth more than it looked.

  The policy is nonetheless **dropped**, on measurement, both ways round.
  Waiting in proportion to the *cost* of what deoptimized was not enough: it put the second render below the first (74-78
  ms to 46-49, the first ten from 307 to 253) and took the steady render to
  12.9, but cost Box2D 11.0%, zlib 5.4%, Typescript 4.1% and DeltaBlue
  5.7%, because cost alone cannot tell a switch where recompiling buys
  nothing from a big hot function where recompiling soon is what makes it
  fast. The benefit side has to be in it, and it need not be a clock: the
  deoptimized code has counted the native work its runs did
  (`ssaStats.work`, `calleeWork`), which is the measure `jitSSAProfit`
  already judges `entrySlow` by. Weighing the wait by cost over realized
  work keeps both sides counters, so a program recompiles the same
  functions on every run, and separates the two cases. Its bar is those
  four benchmarks flat again with the second render still below the first.
  It does not clear it: logged at every deoptimization, the native use a
  code had made of its budget before deoptimizing is 0.028 for that switch
  and 0.027 for Box2D's hot function, with 73 of Typescript's 161
  deoptimizations below 0.1 and 6 of this render's 9 -- so the early
  feedback deoptimizations whose quick recompile is what makes Box2D,
  DeltaBlue and Typescript fast look exactly like the switch where
  recompiling buys nothing. There is no separation to be had from these
  counters, and no policy at all is better than one that taxes the
  workloads where recompiling is what makes the JIT worth having. The
  recompile pot stays real; C attacks it at its cost instead.

  The counters' resets were suspected of a live bug and are clear: the entry
  judge reads only totals that grow against bases reset with them, the
  callee judge resets `calleeGoCalls`, `calleeWork` and `link.in` together
  at every reset, and the learned budget's one window -- `jitUnwound`
  zeroing `link.in` while a learned recompile is pending -- was instrumented
  across seven workloads with no occurrence, since the same call starts the
  learning afresh. The negative deltas were a probe's own artifact. What
  leaves functions slow on this render is early recompiles cutting native
  stretches short, not the counters.

  And a cheap first tier is sound on correctness: with the optimizer's
  rounds off, the handler-liveness test still matches the interpreter under
  every stress configuration, and the three tests that fail assert the
  optimizer's output -- an object not made, a sum unboxed, an allocation
  count -- so they need tier-aware forms whenever a first tier arrives, and
  nothing depends on an optimizer pass for its answers.

  The profit model as a whole is **parked**, not dropped -- the two
  mechanisms above are the part of it F5's data pays for, and they need
  none of its machinery. The rest is the indirect route to the same
  complaint, and its calibration has to be rebuilt before any of it.
  `a21df702`'s harness was ported onto main and most of its kernels no
  longer measure the ending they were written for, by its own checks:
  `gocall` makes no Go call, `host` makes one instead of exiting, `cgo`,
  `cgonat`, `cnewgo` and `cngo` make none, `chost`, `cmidh` and `inl` make
  no exit, `tret` and `tdeo1` are inlined, `tdeoq` does not deoptimize, and
  six of its twelve built-in sweeps no longer measure their ending:
  `indexOf`, `substring`, a Map's `get`, a Set's `has` and `String` are
  native, and `typeof` is a call of Go where the kernel expected an exit. A nested script Go call even
  measures cheaper than the tree tier's own call since the lazy frames
  landed. So the JIT has changed under its own measurements in the 461
  commits since, and whoever returns to the model starts by redesigning the
  kernels for today's endings, not by porting the constants.

Landed here: a number part of a string concatenation is written into the
join rather than through a string of its own (`ba508d37`), which took a
third of the allocations off a string-building benchmark and 20% of its
time, and left the render's allocation unchanged -- it concatenates
strings, not numbers. Fusing `a + b + c` into one join needed nothing:
the compiler already does it (`compileStringChain`).

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
| 2026-10-21 | `a02e9d1`..`3141a88` | Built-ins that run no script called as leaves by every tier; RegExp's literal runs, byte searches and a literal's RegExp given back; String's methods written in place; generators, tail calls and finally clauses in the tree tier; in the JIT a longer substring, concatenation, shapes known, destructuring and parameter prologues | without the JIT V8 -1.6% (RegExp -6.1%), AWFY -1.8%; with it V8 -2.0% |

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
- **RegExp** (after `2bce839`..`348a3d5` and `cbcd058`..`76a80a2`,
  2026-10-22: the literal runs of B6, the BMP tables of large classes,
  `spanRoom`, the legacy statics, the 144-byte RegExp, two first units
  and code units four at a time, a literal's RegExp given back):
  - `regexp_exec_g` and `regexp_sticky` are still 1.5 times QuickJS's
    time: each exec makes a 256-byte result and a 48-byte String for the
    group, and the matcher is about 30% of it; `regexp_replace_dollar`,
    1.35 times, is not profiled.
  - `regexp_replace_fn` is 3.4 to 3.9% slower interpreted at four
    placements after `cbcd058`..`76a80a2` (4,799 to 4,984 ns; 5.5% faster
    with the JIT), with no new work in replace's path in a profile.
  - A literal's RegExp is not given back after a replace by a function
    (1,496 bytes and 33 allocations an op), matchAll, search, a strict
    `return /x/.test(s)` (a tail call), or in a generator's or an async
    function's tree; the JIT's reuse stops for a realm whose
    RegExp.prototype changes its layout, and a pool loses its object at
    each deoptimization.
  - Lazy repetitions and counted loops still push the choice points the
    quick check could pass; a named group's groups object is a 112-byte
    allocation of its own.
  - The legacy statics update the current realm's, where the standard has
    them updated only when the RegExp's realm is it (test262 passes).
  - Go's memory profiler at its default rate unwinds the interpreter's
    deep stack at each sample, 3.6% of `regexp_ascii`'s samples; v8bench
    leaves `runtime.MemProfileRate` as it is.
- **Found by the round of 2026-10-21** (`04e358b`..`e1f5608`), not done:
  - `in`: a top-level `in` runs the interpreter's own case, which only a
    change to `executeAt` would give the tree's answer; symbol keys and
    canonical index strings ("0") are not cached; a new empty object, of
    no shape yet, costs native code one call of Go before it is.
  - Typed arrays' `indexOf` and `includes` poll for no interrupt at all.
  - Generators: the loop cursor is 160 bytes (`9d029f1`); a step in the
    tree tier is still about 100 ns against QuickJS's 57 -- `resumeTree`
    28% of `generator_for_of`'s samples, setting the frame up and writing
    the generator's state back, and the recover each resumption sets, 14
    ns, which an exception must still complete the generator through --
    and a generator made and run to its end 552 ns against 407: two
    allocations of 448 bytes, a 160-byte generator holding three slices
    that are mostly empty, two of which native code reads, and the
    collector 45% of it. With the JIT `generator_create` takes longer than
    without it (589 ns), a call of Go making the generator.
  - A stack trace names a built-in's frame "Object.next" where Node says
    "Iterator.next" or "g.next".
  - `WeakMap`: `weak_map_delete` is mostly Go's `weak.Make` -- a span's
    list of specials searched twice for each new key, and the heap's lock
    the sweeper takes too; a record for each key could go only with
    ephemerons from Go's collector.
  - Native code's `Array.prototype.push` leaves native code where the
    array must grow (8 times in 1,000 pushes), where `a[i] = v` grows it
    by a call of Go and goes on.
  - `array_prop_create` in the interpreter: tree dispatch, the prototype
    check at each append and Go's slice growth are what is left.
  - A number's method's sites are learned at a deoptimization each, where
    the tree tier could mark them as it marks BigInts' in
    `getPropThisNode` (zzmethod.go's `methodRead` and `methodCall` do
    not); code compiled before Number.prototype or String.prototype
    changed leaves native code at the read until it is compiled again; a
    sloppy method of the script's on a primitive exits at each call, Go
    alone wrapping the receiver.
- **Native code's generators** (after `fc452f0`, `9eeaa8b`: a for-of's
  or a destructuring's step resumes a generator by a native call of its
  code), not done, by what each would gain (the first two done in the
  round of 2026-10-28, `bf64954`, `e562a0e`, `94cf520`):
  - A call of `next()` -- a site calling the realm's next on a
    generator -- through the same entry, its `{value, done}` from a pool
    of objects of the result's shape, as object literals are; `return()`
    and `throw()` Go's (`generator_next`).
  - A generator and its cursor made natively, from pools, the generator's
    values inline, and its parameters' prologue run natively: each is a
    call of Go still, which judges the caller's loop slow
    (`generator_create`).
  - `yield*`: resume points after it, the delegating generator's code
    stepping the inner one by the same call; an iterator's raw result
    still Go's.
  - Spread and `Array.from` of a generator, host operations, as a native
    loop of steps.
  - Generators with captured locals, which want their cells kept in the
    generator.
  - A yield with pointers to save while the collector marks exits, where
    it could store them through the barrier's routine; a generator's level
    takes one context and two frames, so the frames' depth may reach twice
    the limit before Go throws its RangeError; the frames made for calls
    of Go from a generator's level are not shared between calls.
- **QJS_JIT_STRESS and the VM's Go tests:** done (`9bda4ec`): the tests
  that count native calls, exits and compiles take the JIT's own
  thresholds, the rest run stressed, and CI's stressed test262 job runs
  them. `TestJITNoHeapWalks` failed once in four full runs under it (a
  heap walk with the JIT, none without): the meter counts the process's
  allocations, which another runtime's cleanups may make meanwhile.
- **Calls, private members and tail calls in the tree tier** (after
  `d4b1ed5`..`d99aa74`), not done:
  - A recursive call of the tree tier's with a frame costs about 80 ns,
    QuickJS's 40: the cost is in `callTree` and `runTreeNested`, which
    must stay as they are; most of what is left of `tail_mutual`'s gap is
    the loop that gives the frame up (`tailFromTree`, `takeTailCall`,
    `tailTarget`), whose editing moves the hot code.
  - A call in tail position of another function past the depth is made
    in the frame it gives up (`e5feded`..`9d029f1`): `tail_mutual` 82 ->
    53 ns, QuickJS's 37; each call still goes through the pending tail
    call and its checks, an interrupt's poll included, and `tail_self`'s
    44 ns (QuickJS's 36) is the callee's global read (5 ns) and the hand
    through the pending call (8 ns). A finally clause's inline copy on the
    normal path made `try_finally` 34 -> 21 ns, under QuickJS's 23;
    `try_catch` is 16.8 against 13.4, `try_throw` 33.8 against 29.8.
  - A breakpoint resolves to one statement, so the copies of a finally
    clause the compiler makes at each break or continue are each a
    breakpoint's only in one of them (code for a debugger keeps one copy
    on the normal path).
  - An optional chain's call and a spread call in tail position are not
    tail calls yet, as the standard has them.
  - The interpreter's own case for private members (`executeAt`'s) and
    `#x in o` are not cached.
  - A private field added to an object that is not extensible is refused,
    as the stage-3 proposal has it; Node v26 adds it.
- **String's methods** (after `9f801a2`..`3141a88`: every one the
  micro-benchmarks have at most 1.2 times QuickJS's time, several under
  it):
  - What is left in the interpreter is the call: in
    `string_code_point_at` the method is 20% of the time, the tree tier's
    call node and `callRest` 35%; `string_starts_with` is 92.9 ns against
    69 for that. Each method takes its receiver through a closure
    (`thisStr`), an indirect call a plain function would save, 2 to 4 ns.
  - `toString` cannot take `toInteger`'s inlined path: `executeAt` calls
    it.
  - padStart and padEnd of a filler past ASCII, and a repeat of a string
    that ends in a high surrogate and begins with a low one, take the old
    builder; the annex B HTML methods build Go strings and scan them
    again.
  - Still behind QuickJS: fromCharCode of two arguments (113 to 119 ns,
    83), startsWith and endsWith past ASCII (110 to 127, 79 to 84), substr
    (124, 105).
- **The JIT's coverage of modern code** (after `8a0ecca`..`e8ab528`: a
  longer substring a leaf's call, + of strings concatenated at once,
  shapes an earlier operation checked not checked again, destructuring
  and parameter lists with defaults and patterns native). Of the 13,986
  functions of npm's own code the JIT's lowering takes 5,618 (5,101
  before), most of the rest doing no loop's or array's work (51%);
  refused:
  - Done since: object spread and rest, array spread and spread calls, a
    literal's computed keys, `__proto__` and the fields after them, tagged
    templates and a named function expression's own name, which had
    refused 1,062 of npm's functions, compile, each an operation Go makes
    from native code (`compile.GoOperation`, `abi.GoCallOp`): without a
    frame where that runs no script, with the code's in the same call
    where it may (a getter, a proxy, an iterator, a key's `toString`, a
    callee), never an exit. Native code makes the common ones itself: an
    object spread by the runtime's table of spreads by layout (V8's
    CloneObjectIC), the fields after it along their shapes' first
    transitions, an array spread under the iteration's protector into the
    room its literal's arrays learned to need (V8's allocation sites), an
    element appended; Go an object rest by its source's layout, and a
    leaf's spread call. TestLowerCorpusRefusals measures it: of npm's
    14,033 functions 1,968 are refused (3,030 before), Are We Fast Yet's
    13 of 477 (20). Micro-benchmarks against the tree tier: object_spread
    0.45 times its time, array_spread 0.5, object_rest 0.3, object_computed
    0.85; spread_call and object_getter_spread, framed, judged not to pay
    and left to it. Open: a spread call of a script function is Go's call
    with the frame, where a native call of the array's elements would
    serve; a callee's `push_callee` (11).
  - Frames (2026-10-23, `a5bc68f`..`f3a2c34`): parameter prologues,
    try statements, cursors and class and literal member definitions in
    native callees' code, rest parameters, the arguments object and
    super references in a function's own code. npm's functions their own
    code takes 5,622 -> 5,858, and those native callers and inlining take
    5,048 -> 5,911; Octane's own 3,151 -> 3,181, callers' 3,552 -> 3,612;
    AWFY's callers' 295 -> 311. Left of the frames' refusals: a mapped
    arguments object that aliases (npm 31, Octane 149, mostly forwarding
    constructors the inliner takes), private members whose key is a
    local (48), the implicit derived constructor's `super(...args)` for
    native callers (144, which constructions skip), a derived constructor
    making an arrow (20), `for await` (13), and async functions as native
    callers' callees (420, nearly all of which `LowerSSAHot` takes once
    hot). A super reference, and the arguments, in code native callers
    call need the function object and the call's arguments in the
    callee's context, which native calls do not write. With the operations
    Go makes (above), TestLowerCorpusRefusals refuses 896 of npm's 14,033
    functions (1,968 with those alone), 178 of Octane's 6,955 (148 a
    mapped arguments object), 13 of Are We Fast Yet's 477 (the implicit
    derived constructors).
  - Rest parameters and, in a function's own code, the arguments object
    (2026-10-23): the array and the object are Go's at host exits, from
    the frame Go entered the code with, `arguments[k]` and
    `arguments.length` Go's reads from native code (`ssa.GoArgument`) --
    a call of Go each, where V8 reads its frame; such code has no call
    entry, a native call writing the parameters alone. Left: a mapped
    object aliasing a parameter the function assigns or an object it
    lets out (npm 31, Octane 149, most of them forwarding constructors
    the inliner takes), and native callers' calls of these functions.
    Super references (`get_super_prop` 117 functions) likewise, Go's
    from native code on the frame (`ssa.GoHost`), such code having no call
    entry, a native callee's frame no function object to read the home
    object of.
  - A parameter list's prologue is native for native callers, inlining
    and generic calls too (2026-10-23): its states keep the call's
    arguments past their operands, which a frame Go makes there is given
    (npm's callee refusals for non-simple parameters 404 -> 2). An object
    pattern's read of a property its source lacks -- `{ x, y = 1 } = o` --
    had deoptimized at every read, nothing learned, until the function was
    compiled no more; it looks in the megamorphic cache, and Go reads a
    miss from native code.
  - Callees' frames and resumptions (2026-10-23, `ba55f1a`..`714177b`):
    async functions and generators of calls and no loop compiled once
    their resumptions make them hot (LowerSSAHot, which Go had never
    asked: npm's 521); a native call writes what a frame of the VM's has
    that its callee's code reads -- all its arguments past the parameters
    (`abi.Context`'s `Args`: rest parameters, arguments objects, a
    default derived constructor's super() of them) and its function
    object (`Function`: super references, push_callee, a mapped
    object's callee) -- so such callees have call entries; a derived
    constructor that makes arrows is constructed natively, its super()
    native where no arrow precedes it, and arrows sharing a bound
    derived constructor's binding are called natively. TestLowerCorpusRefusals,
    refused: npm 896 -> 66, Octane 170 -> 163 (all for Go's entry),
    AWFY 13 -> 0. At run time, a native call past the deepest context is
    a call of Go, not an exit (AWFY Havlak's exits 7,816 -> 2,344, its
    invocations finished in the interpreter 5,714 -> 524), and `new
    Array(n)` past a pool's room Go's construction from native code
    (Sieve's exits 2,101 -> 64, Bounce's 1,499 -> 0, Json's 269 -> 13).
    Left: a private method's key in a local (npm 14), which a native
    callee's frame does not hold; `for await` (13) and a mapped object
    that aliases (31) in a function's own code; a super reference's or a
    mapped object's read is a call of Go each, so small methods of them
    are judged not to pay as native callees; CD's and DeltaBlue's
    polymorphic calls inlined for one target, which leave at the other's
    calls until they are called natively (done since, `7e5b3f9`: a call
    that met more than one function is made natively to each).
  - Shapes known: a loop header starts knowing nothing -- its entry for
    on-stack replacement knows nothing -- so a loop's first read of an
    object checks its shape at each turn; moving the check before the
    loop, its exit the loop's entry, would check it once.
  - `async_await` takes 5 to 10% longer with the JIT than without it: 2
    million resumptions not entered, each cheap, but `enqueueAwait`'s
    append costs more with the JIT on (130 against 70 ms of samples), not
    yet explained.
  - Octane's Typescript compiles about four times a function (2,400
    compiles of 607 functions in three runs), a third of them a function
    whose code was dropped for room or flushed, compiled again into code
    of the same size; its code averages 52 KB a function. Of all the code
    made, exit stubs (a 10-byte pointer and a jump each) are about 5% of
    the bytes and displacements 22%: the context's fields past its first
    128 bytes take four bytes each.
- **The JIT** (`-tags quickjs_jit`):
  - Compiling costs too much for what it saves in large programs. Octane's
    Typescript, which with the JIT had taken twice the interpreter's time,
    takes 0.86 times it (7.2 s against 8.4 over three runs, 2026-10-20, at
    v8bench's budget of 32 MiB), among the least the JIT gains of the
    suite's programs; with code unused flushed since (`bc35a9f`) it evicts
    16 entries in three runs for room, at 31.9 of the 32 MiB (2026-10-28),
    where it evicted 1,075 a run, and leaves
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
  - **Leaf calls of built-ins, one mechanism for all** (2026-10-20; step 1
    begun 2026-10-21). Done so far: a built-in declares its frameless
    application where it is made (`setElemOp`), which every tier applies
    before it would make a frame -- the collections' deletions, the weak
    collections' operations, `Date.now` and `String(x)` (`a02e9d1`,
    `date_now` -14.7% interpreted, -10.3% with the JIT), and String's 27
    methods that run no script as native leaves: the built-in's own
    function behind an exact check of its receiver and arguments
    (`leafChecks`, `8d33d24`; with the JIT `string_at` -25.5%,
    `string_repeat` -17.9%, interpreted -1 to -9%); and Math's imul,
    clz32, pow, atan2 and hypot, applied by `mathCall` in every tier but
    the JIT, imul an instruction of native code's (`OpMulI32`, `fd75674`;
    `math_imul` 63.2 -> 4.6 ns with the JIT, 66.3 -> 50.2 interpreted),
    clz32 too (`OpClz32`, BSR of 2x+1 on amd64, CLZ on arm64, `0d88274`;
    `math_clz32` 22.0 -> 5.3 ns); Number's functions and methods, Number(x)
    and Boolean(x) (`c2c717b`). Step 2, a leaf's call native code has seen
    is a call of Go of its own (`ssa.GoLeaf`, `febf9d0`), checked to call
    that function, which Go applies with nothing to find out: a leaf's call
    38 -> 27 ns, `date_now` -18%, `map_set_string` -16%. A method read off
    a number or a boolean is native, off its prototype, as a string's is
    (`8dad26c`; `float_toFixed` -16%). Templates and concats of any number
    of parts are native, a part's conversion a check that it is a
    primitive, which the concat's one call converts (`ir.StringPart`,
    `46f1552`; `string_chain` 1069 -> 540 ns, faster than the tree tier's
    598; Are We Fast Yet's Json -19%).
    Next:
    - A call of Go's own cost, 27 ns for a leaf that does nothing (a loop
      of `Number.isNaN`, 1.5 ns empty): native code writes each operand's
      two words with a check of each, saves the live registers and reads
      the result back from its keep cell (about 9 ns); `callGo`, `goCall`,
      `ExpandGoCall` and `HostCall`'s indirect call (about 10). A routine
      for leaves' calls of their own, as `jitBigFast` is BigInts', takes
      only the last; the native side wants the operands passed where they
      are and fewer registers saved -- step 3. Done 2026-10-27 (plan E's
      step 5): the leaves' routine, their forms, cold saves, native forms
      of codePointAt, at and Number's predicates, the other calls' routine.
    - The objects' and arrays', Date's getters and the collections' `size`
      (an accessor's read, not a call).
    - A sloppy method of the script's on a primitive exits at each call
      (only Go wraps the receiver); a number's method's sites are learned
      at a deoptimization each, where the tree tier could mark them, as it
      marks BigInts' in `getPropThisNode` (zzmethod.go's `methodRead`,
      `methodCall` do not).

    Before: a built-in that runs no script is called from native code by
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

### Open items collected on 2026-10-28

Open items found by the rounds and their agents since the review, which no
section above records, collected from their reports so that none is lost.
Each gives what it was found with and when. Some were not checked again
since (marked *unchecked*). None is judged here; each is to be measured
first, as the rest of the plan is.

**The JIT's coverage and exits**

- A callee with `define_index` or a spread compiles for native callers
  but is never inlined (2026-10-23).
- With a memory limit set, every operation Go makes for native code takes
  the framed path: the frameless paths do not charge the meter
  (2026-10-23).
- `template_object` is not native (5 npm functions, 2026-10-23).
- Reads off a number that only Object.prototype answers
  (`x.hasOwnProperty`), and non-method reads (`n.foo`), are Go's
  (2026-10-21, *unchecked*).
- A megamorphic read where a few receivers have a getter exits for those
  receivers (2026-10-17, *unchecked*).
- Callbacks with default or destructured parameters are not inlined; two
  closures passed in one call are not analysed, a limit for root-level
  closures too (2026-10-22, 2026-10-27).
- DeltaBlue: `Plan.execute` never compiled (host-only) and about 681,000
  callback calls a run not entering native code (2026-10-19); an arrow in
  `removePropagateFrom` invalidated and never compiled again, closures
  made anew never building up the recompile budget, about 1,100 Go entries
  a run (2026-10-22). Both *unchecked* since the callback inlining.
- RayTrace: a read in `getPixelColor`'s copy inlined in `renderScene` had
  no feedback, the callee's own code never filling its caches; its miss
  invalidated `renderScene` (Go entries into `getRay` 3,271 -> 7,127,
  2026-10-22). Likely served by an exit inside an inlined callee teaching
  the callee's feedback (`aad0d8c`), *unchecked*.
- Richards' warm-up: 400 Go entries into `IdleTask.run` in short runs;
  `WorkerTask.run` warming at 225-436 calls a run, 361 ns against 60
  native, called 0.08 times a caller's invocation, under V8's inlining
  rule (2026-10-22). AWFY Richards' 33 exits: a fourth target of
  `runTask`'s site found after the compile, each call exiting until
  `jitLearnedDue` (2026-10-23).
- A polymorphic miss with room for more targets made by a call of Go that
  stays native (`ssa.CallSite.MissGo`): AWFY DeltaBlue's exits 1,699 ->
  699, Gameboy's 9,222 -> 7,311, but it broke a case like AstWalker's; a
  generic fallback through the callee's link at every site with targets
  took V8 DeltaBlue's exits 67 -> 4 and made AWFY Richards' worse (947 ->
  1,384). Tried, not committed (2026-10-23).
- Towers' exits 1 -> 290 once `buildTowerAt` was inlined: it refills its
  object pool by an exit, not a call of Go, about one a 700 constructions
  (2026-10-22, *unchecked*).
- `jitEnterCallee` gives no `f.args` to a callee whose parameters are
  lexical, and no test reaches it; the tail-call depth's offset cannot be
  seen by a test (2026-10-22).
- The cap of 96 native call contexts (about 18 KB each): EarleyBoyer's
  `deriv_trees` recursion has calls at the last context made by Go and
  finished in the interpreter (2026-10-23). Raising it is a question of
  memory.
- `jitLearnedMark` does not count `bigSites`; `inlinePending` is still
  turned into `inlineReopt` at an entry to code already invalidated,
  harmless since `jitReoptimize` changed (2026-10-21).
- Derived construction: an empty base parent (stage 2a), inlining (stage
  3), field initializers lowered as method callees so that `jitConstructs`
  drops its `fieldInit == nil` condition; derived roots are never
  slack-tracked (`noteBuilt` skipped, 2026-10-19).
- PdfJS's sites of 2026-10-14: deopts at `atob`, `transportObj` and
  `fontLoaderBind`'s additions; `decodeCharString`'s `get_index` and
  compare, about 8,500 exits (*unchecked*).
- `String.fromCharCode` past Latin-1 is a call of Go only in a function's
  own code with a keep cell for it, and still exits in inlined code:
  PDF.js's `Lexer_getObj` inlining `Stream_lookChar` (`c2591b3`).
- Native generators, besides the list above: a `next()` result is still
  a 96-byte object from the pool's fillings, which escape analysis could
  make virtual; native creation excludes generators that read
  `arguments`, have more than eight locals, are in a with or eval scope,
  or are sloppy and read `this`; the pools are filled by one
  `newGenerator` call an object, most of `generator_create`'s cost with
  the JIT (2026-10-27).

**The JIT's compile cost, tiering and memory**

- Resume points only where Go resumed: reloading every live slot at each
  resume point is 110K of `initPhysics`'s 128K values. Points only where
  Go resumed cut compile CPU 25-40%, but doubled the interpreter's
  continuations and broke 7 tests (2026-10-23, kept as a patch, not
  committed).
- A flush's back-off, the wait doubled once for code flushed before:
  Typescript's five runs compiled 1,807 -> 995 times, its compiles 3,361
  -> 2,575 ms, exits 48.0K -> 34.5K; the live heap after a collection 262
  -> 356 MB, and 164 drops for room (2026-10-23, kept as a patch, not
  committed).
- A compile's working memory is about 7.5 KB an instruction (17 KB for
  `initPhysics`), which the JIT's budget does not count; its fit check
  assumes 2 KB, and the measured figure would refuse functions that
  compile today (2026-10-23).
- The cache-change raise: any cache change since an entry was made adds
  500 invocations, where V8's NotifyICChanged raises only a function
  already due. Dropping it: EarleyBoyer's exits 1,320 -> 132, Typescript
  829 compiles and 1,936 interpreter finishes; the "due" variants made
  every large program worse; `TestJITV8Budgets` pins the rule
  (2026-10-23).
- Branches never run before the compile deoptimize one at a time
  (Gameboy's `memoryReadJumpCompile` 10 times, Typescript's `innerScan`
  11): borrowing the shape from the function's other reads of `this` cut
  Gameboy's code 33% and grew Typescript's 6-13%, dropped; learning more
  at each deopt would depart from V8 (2026-10-26).
- Optimize's last round finds nothing in most compiles (3-4%), and
  skipping it soundly needs care with `unboxPhis`; worklists in place of
  rounds; `removeDead` and the two liveness passes behind the store
  checks scan the same frame states. The build and optimization are more
  than half of all compile time (2026-10-27).
- `reconstruct`'s stubs merged: consecutive guards that share one rebuilt
  state sharing one exit stub (2026-10-18).
- The register-group allocator's own cost: allocation about +25% on
  Typescript, `mir.CompileIn` +7-20%; "a different way of building the
  groups" would be needed (2026-10-23).
- Encoding 3-10% above its earlier cost on every program, from loop
  alignment; zlib's and Mandreel's compile allocations 1-7% above, from
  reading every phi argument and frame-state slot (2026-10-23,
  *unchecked*).
- The compile budget of the JIT's second phase -- under 50 us and 64 KB
  transient for a function of 200 instructions -- was never confirmed;
  about four times over when it was last tracked (2026-10-12).
- The code arena's free space between live functions is neither counted
  nor reused until its 64 KB chunk empties (460-520 KB mapped for 323 KB
  live under a 1 MB limit); a lowered program made outside a compile is
  not checked against the budget (2026-10-12, *unchecked*).
- The 1 MiB limit a placement's code has, from arm64's conditional
  branches' range (2026-10-23).
- Decisions made once for good, from the inventory of 2026-10-14, not
  checked again since the budgets of D7: `hostOnly` cleared and then
  `cl.jitRefused` set after a budget's passing failure; refusals that
  depend on feedback setting `cl.jitRefused`; `notInline` after 16
  inlined exits over an entry's whole life; a compile refused for its
  code's size or its spills not tried again with less inlining; `tryJITAt`
  testing `< 4` where the others test `< 8` (*unchecked*).
- An `ir.Instruction` is 128 bytes, with a 16-byte StateMap for each
  bytecode instruction, which packing would halve; descriptors' headers
  about 64 bytes. It was what made Box2D and Typescript unstable at 8 MiB
  (2026-10-14, *unchecked*).

**The JIT's code**

- A native call's state found from its return address, as V8's safepoint
  tables are, not stored by two instructions; an xmm15 round trip for
  each argument (2026-10-22).
- Keeps of operands only the call reads (`x.next` in `tail`), unless
  their source may be a keep cell the same run writes (2026-10-22).
- `this` carried as a phi takes the compare-number-words path when its
  pointer word is read (2026-10-22).
- Lifetime holes, or a result reusing a dying operand's register:
  intervals touching at one instruction leave loop-carried moves
  (try_catch's sum, array_for_of's, a counter after its `addsd`); every
  emitter would have to allow it (2026-10-23).
- Exit stubs of 10 bytes, an index in place of the descriptor's address;
  call-free loops' extra 3-instruction stub at each exit, which Go could
  replace by reading the budget register from the saved registers
  (2026-10-27).
- Spills at offset 10,816 of the context, 749K accesses all with 4-byte
  displacements; `[reg+0]` carrying a displacement byte, about 220K
  operands (2026-10-23).
- Keep cells are never reused: past the 256th of a function, each call of
  Go is an exit (RegExp's `runBlock11`, 2026-10-12); reusing them by
  liveness. KeepHigh, a keep cell's clearing from its highest used:
  Typescript -1.4%, Towers +11-15% from layout (2026-10-20, parked).
- Compare-and-branch fusion's rule that nothing is emitted after the
  compare leaves about 80 Octane sites unfused where only captured
  bindings' loads follow; a truthiness that redundancy elimination shares
  between branches stays a boolean (2026-10-22).
- Unboxed loops' boxes left: a box feeding a value on the loop's exit
  edge is made every turn; forEach's and typed forEach's captured `sum`
  stored to its cell; `some`'s `===` (2026-10-27).
- A loop of few turns could be inlined without a closure passed, given
  turns-a-call feedback a function does not have (2026-10-27).
- Calls inside inlined callees take the bounds in their PCs' order, not
  by frequency, as V8's do, which needs counts by site (2026-10-22);
  loops' calls first since (`3b178e7`), by depth, and by the counts by
  site since the third round of 2026-10-28 (`b1dfaf5`), in E.
- Constants stored into spilled phis (25K in Box2D); cold code about 23%
  of all (2026-10-14, *unchecked*).
- The plan for the op cache's sake (2026-10-16), judged by hot bytes and
  the op cache's share, besides the flag tested once a run and loop
  headers at 64-byte lines, which are done: entry blocks and slow paths
  out of the hot region; constants from a RIP-relative pool (10-byte
  `movabs` were 10-15% of hot bytes); `MovAddr`'s 45K of 10 bytes
  pooled; stubs' conditional jumps never short (*unchecked*).
- `Math.sin` and `Math.cos` natively on amd64: about 1% of Box2D,
  115,000 calls of Go in three runs at 18 ns (2026-10-20).
- `jitGoCall` runs `jitGenCallSeen` and `jitGenMakeSeen` at every call Go
  makes for native code, about 0.2 s of a 27 s profile (2026-10-27).
- `bigint64_arith`'s loop counters as unboxed f64 phis, about 7% slower
  than tagged, unexplained (2026-10-20, *unchecked*).
- ADX and MULX kernels for bignums on amd64: math/big still beats our
  64-word multiply, 1,756 against 3,048 ns (2026-10-20, parked).
- `aliases.may` can store a "false" computed while a phi round around a
  loop is in progress; never seen to change an answer, a fix kept
  uncommitted (2026-10-11, *unchecked*).
- Frames the debugger and stack traces see from `jitGoCallNested` show
  captured locals as they were while cells hold their values; entering
  code with cells from Go mid-function wants a test of its own
  (2026-10-18, *unchecked*).
- What the review of the old pipeline (2026-10-08) asked of it, for the
  SSA pipeline: stale `*byte` pointers to unmapped code (store
  `uintptr`), hand-kept assembly offsets (`go_asm.h`),
  `runtime.AddCleanup`, `Validate` accepting Opaque, String and
  Uninitialized literals, layout asserts at compile time, `qjs --jit`
  falling back silently in builds without the tag (*unchecked*).

**The JIT, the collector and allocation**

- Escape analysis of `arguments` objects, never tried.
- Context allocation's step B: callees with cells inlined (cells in keep
  slots, `InlineState.Cells`, `ExitInline.Cells`), and native recells,
  left until a profile asks (2026-10-19).
- A test scanning the source for a raw store to a pointer field outside
  the `storeRef` helpers; `storeRef`'s bulk forms, clearing n values or
  copying n from GoArgs, a call's setup two or three calls of the routine
  (2026-10-19).
- Under `-race`, `TestWorkspaceCompileAllocations` fails: 64.2 against
  its limit of 62, main's 63.9 (2026-10-27).

**The interpreter and the tree tier**

- A per-funcData flag letting the call node skip `callRest`'s tests, 20
  to 30 instructions a call; Go's recursion replaced by a frame stack of
  the tree tier's own, a redesign.
- Tail calls with the callee recorded: `tail_mutual` -16.7%, the suites
  +0.8 to +2.5% mostly at one placement, kept on a branch, not merged;
  Richards' 12 million shallow tail calls a run on the slow path, which a
  fix grows a function before `callTree` to serve.
- The tree tier's generators: a `next` frame made lazily; the stack's
  save and a try's state allocated only when needed.
- A "global" operand kind in treegen, a global read in place (the file
  grows).
- int32 trees do not cover bitwise operators made by `op()` on two plain
  trees (`c[i>>2] << c[j>>2]`, about 6M), comparisons pushed as values
  (about 3M), an int32 written into an integer typed array (15.7M, boxed
  and converted again); the fused read applies its operator through a
  switch (2026-10-27). zlib's upvalues' heap reads.
- In a switch, `dup; case; strict_eq; jump_if_true` as
  `jump_if_cmp_false strict_ne`, a dispatch fewer a case, a local
  discriminant read again with no `drop` (2026-10-04).
- The interpreter's caches (2026-10-04, *unchecked*, `executeAt` being
  kept as it is): `OpSetProp` calling `setPropCached` always, where an
  inline `storeOwn` would save a call of six arguments; `leafCall` trying
  `plainOwn` before `cl.ic[...].own`; propCache's hot fields first, or
  64 bytes.
- The for-of cursor (160 bytes) as a kind of Value with no Object
  header: 96 bytes less for every for-of, spread and destructuring
  (2026-10-12).
- Queens, 1.31 times QuickJS: 47% of its time is the calls of two tiny
  methods reading and writing elements; pure bodies with element reads
  and several stores (2026-10-15).
- Implicit derived constructors, 1.21 times QuickJS (356 against 295 ns);
  closures of 264-328 bytes, their layout fixed by the frozen functions'
  reads (2026-10-26).
- The compiler's speed (CodeLoad): its four scans of a function (this,
  arguments, direct eval, this's properties) merged, about 4% of compile
  time; the code buffers sized, 3-5%; a RegExp literal validated by
  building and dropping its parse tree; the collector about a quarter of
  compile time (2026-10-26).
- NewRuntime: about 600 built-in functions made eagerly, its largest cost;
  made lazily they would change what scripts see of the global's
  descriptors, a decision for the API; a lazy property index for large
  built-in objects, about 12 KB a runtime (2026-10-02).
- Goroutine stack growth, about 3% with the interpreter's frame of 1.7 KB
  (2026-09-30, *unchecked*).
- Pure calls deeper than 32 fall back to frames entirely; switching at the
  limit alone needs the pure path to pass on an error such as a stack
  overflow (2026-10-05).
- Structured control flow: structuring lazily, a loop once its back edges
  prove it hot (eagerly cost V8 +1.5%); `&&` and else-if arms as
  statements (Splay +2%, dropped); a loop whose branches need a structure
  of their own runs block to block (2026-10-08).
- The hot functions made insensitive to their alignment; the hot files'
  grouping cost `array_for` +43%, `array_update` +36%, `func_call` +26%,
  and a layout keeping both was not tried; `elemAt` sits outside what
  placements vary (Crypto +4 to +11% at 3 of 8 placements, 2026-10-25).
- `a?.b` as one logical node, `a` held in a slot (2026-10-15).
- BigInts in the interpreter: `bigint32_arith` level with QuickJS, not
  below (a closure, two failed number checks, `arithSlow`'s frame, the
  short BigInt's write barrier); `pidigits` 13% in the free list, 9% in
  `s += y` (2026-10-20).
- The place updates' fallbacks: `arguments[k]`, TDZ upvalues, operands
  with calls, `super`, private names and `with`, `G.x += 1` reading the
  global twice (2026-10-08, *unchecked*).
- `getElem`'s fast part merged into the tree's `getIndexSlow`, a call and
  about 15 instructions a typed element (2026-10-07, *unchecked*).
- A tree callee's locals over its caller's argument slots, which breaks
  `arguments` unless restricted; a case byte for each function in
  `pureCallAt` (2026-10-07).
- Arrays starting at a capacity of 8 at their first growth; a cache by
  site for `obj[i] = v` on plain objects (`setIndexed` 32% of
  `prop_create`'s profile), which wants a keyed store cache of several
  entries (2026-10-12).
- An Object of 48 bytes, its values a pointer and two 32-bit lengths
  (stage 3 of 2026-10-13); a closure's blocks inside its function object,
  about 4.9K allocations fewer in EarleyBoyer, at the cost of many size
  combinations (2026-10-12).

**Built-ins**

- RegExp: lending and returning the matcher's state costs 3-4% on many
  short ASCII matches; `recordLegacyMatch` copying the positions at every
  exec, about 3% (2026-10-21, *unchecked*). Its allocations: captures'
  substrings, match results, `wtf8.ToUTF16`, split's appends, a long
  replace's builder; a layout made in advance for `lastIndex` (2026-10-12,
  *unchecked*). No match array where an exec's result is dropped
  (2026-10-01, *unchecked*). DeltaBlue with the JIT +13% in placements,
  not seen in fixed mode, to check again (2026-10-22).
- `date_parse`'s 12 allocations left, which want go-intl's formatting to
  append (2026-10-12).
- JSON.parse defines each key by the generic `defineOwnProp`; no gap
  points at it (2026-10-04).
- Strings appended in place, as C QuickJS reallocates; Splay's
  `String(key)`, 24K allocations (2026-10-12).
- `mayTie` counting digits by a loop, not `FormatUint` (2026-10-04,
  *unchecked*).

**The JIT slower than the interpreter, unexplained**

- `string_template` +17%, `array_forEach` +8% and `local_destruct` +5% at
  every seed and placement (2026-10-23); after a fix, `string_template`
  about 4% and `local_destruct` 3-4% "from something else" (*unchecked*).
- RegExp (1.02 the interpreter's time, its time in the matcher), CodeLoad
  (1.03, parsing), `regexp_fresh`, `regexp_search_i` and
  `regexp_search_utf16`, 7% or more (2026-10-27).
- `async_call`, `async_chain` and `async_generator` 2-6% (2026-10-20).

**Measurement and tooling**

- Placements varying where package vm sits against the packages before
  it, as a pad in mir would.
- Data's layout: a build of the same bytes 14% slower at one placement,
  `type:*` and `go:func.*` 192 bytes further on (a longer path); ASLR's
  effect on the interpreter's comparisons (2026-10-16, 2026-10-20).
- test262 with `-conformance.debugger` and `-conformance.jit` together
  fails "no test ran native code", the debugger's code never native
  (2026-10-22).
- A cold matrix, single runs, after the compile work: the release's had
  the JIT slower on Octane's DeltaBlue (+24%), RegExp (+17%) and RayTrace
  (+5%) (2026-10-27).
- The 24-hour differential fuzz, never run; the longest stopped at 2 h 39
  min (run at milestones).
- `TestAsyncWorkTimer` flaking under the full suite's load.
- A test that tree closures call no tiny helpers (`IsNumber`, `Float`,
  `IsObject`, `at`), after the inlining budget's finding of 2026-10-07.
- arm64: the JIT runs only on darwin/arm64, not linux/arm64, and was
  never timed beyond small kernels.
- test262 under JIT stress 10-50% slower since `45d15dc` (*unchecked*).

**Semantics**

- Stack traces still unlike Node's: a receiver's own `constructor`
  property, which V8 passes over; an arrow called as a method, which V8
  names by its receiver; an object made by `new P` after `P.prototype`
  was replaced.
- `new.target.prototype` is read at `new`, where the specification and V8
  read it in `super()`, which a Proxy given to `Reflect.construct` as its
  new.target shows (2026-10-19, *unchecked*).
- `a[k] op= y` with an object key converts the key once, as C QuickJS
  does, where Node converts it twice; not pinned by a test (2026-10-08).

**Dropped, not to be tried again without a new angle** (besides
[Rejected](#rejected)): fusing `get_prop` with `get_prop`; an inline rope
check in `String.Go()`; fusing method calls on expression receivers (25
test262 failures); a self tail call as a loop; one free block a size for
BigInts; new, instanceof, new_object and set_global as host operations
(DeltaBlue +10%); no RegExp object for `/lit/.exec(s)`, which scripts can
observe; a generator's frame reused, unsafe where `super()` in an async
arrow writes the frame's this; first compiles at higher call thresholds.
Judged when little of the suites ran natively, and not judged since
(2026-10-09): OpGlobalCell shared across stores and calls, entry blocks
after the hot blocks, V8's constant globals watched for writes (39 write
sites).
