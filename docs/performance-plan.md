# Performance plan

This file is the optimization work still open in go-quickjs and how each
item is measured. An item says what costs, where it is in the code, what
would fix it, and the evidence, with the revision it was measured at. The
engine runs JavaScript on two tiers, the register interpreter
([register-interpreter.md](register-interpreter.md)) and the JIT that
compiles its register code
([internal/jit/README.md](../internal/jit/README.md)); where they stand
against C QuickJS and Node is in
[benchmarks.md](benchmarks.md). Completed items are not kept here: their
commits and the release notes say what each did and what it measured.

Many figures below predate `388324de`, which removed the stack
interpreter, the closure-tree tier and the stack JIT, and `d6ac45be`,
after which the front end compiles a function to register code alone, at
its first call. An item measured on those tiers is kept only where its
cost lies outside them -- a built-in, the object model, the collector, the
JIT's SSA optimizer and backends -- and is marked *unverified* where its
size has not been measured again on the register tiers. Every item was
checked against the code at v0.45.1: where the code still does what an
item says, its figure may still be the old one, and *needs timing* marks
an item whose verdict waits on a measurement. Figures against C QuickJS
marked v0.39.0 are from the comparison at `913095f4` on a Ryzen 5 3600
(benchmarks.md).

Items marked *(JIT project)* belong to the JIT's next project, whose
plan is [jit-design.md](jit-design.md); each says which of its stages
takes it up, and is not to be taken up apart from it.

## How an item is measured

- **Placements.** Code placement moves the V8 suite's figures by a few per
  cent, and single benchmarks by up to a tenth, with no change in the code
  that runs. The cause is AMD Zen 2's op cache, which holds the decoded
  code at one offset in every 4 KB in one set of eight: hot code that
  lands on the same sets is decoded again on every pass. A change is
  compared with its parent by `internal/cmd/v8bench/placements` (see
  AGENTS.md): eight builds of each side, placing `callObject`, the JIT's
  leaf calls (`jitLeafGo`), the Go table's loop (`runReg`), the register
  calls (`rhCallW`), the native dispatcher (`nativeDrive`) and the native
  handlers' block (`regNativeHandlers`) differently in a balanced design,
  and the package data at a phase of 0 or 32 bytes (`c3917728`); each
  placement's fastest round is taken, and a short benchmark is run again
  in processes of its own, up to 300 times. `-k 2` times a change while it
  is iterated, `-parallel -k 4` screens it in minutes, and a batch is
  confirmed with all eight over the V8 suite, Octane and Are We Fast Yet.
  `-micro` compares the micro-benchmarks over the placements the same way
  (`42acae44`).
- **The layout lock.** A change to package vm is timed with its pads
  regenerated (`go generate ./internal/vm`), so that both sides have the
  hot code at the base's offsets modulo 4 KB and 64 bytes:
  `callObject`, the JIT's leaf calls, `runReg`, `rhCallW`, the regular
  expression matcher, `regNativeHandlers` and the Go runtime's `mheap_`,
  `writeBarrier` and `gcController` (`internal/vm/layoutlock.txt`). What
  the lock cannot hold is in docs/optimizing.md ("The layout lock").
- **The hot code unchanged.** `internal/cmd/hotdiff` compares the register
  interpreter's hot functions in two builds instruction by instruction --
  `runReg`, `rhCallW`, `regEnterW`, `regCallSlow`, `regRunFD`,
  `callObject`, the native dispatcher's Go side and the native handlers'
  block -- which a benchmark at 5 to 10% of layout noise cannot show.
  `TestIntendedInlining` fails when a function the hot paths rely on is no
  longer inlined. Package vm's assembly, the native handlers' block among
  it, is laid out after every closure, so the closures' sizes are compared
  too (`go tool nm -size`): closures one change added moved the block 672
  bytes, which made Octane's RegExp and the generator tests slower, before
  the lock's assembly pad anchored it.
- **Touched paths first.** A micro-benchmark of the paths a change touches
  is timed against its parent before the suites are. A change to the JIT's
  compile path runs `internal/cmd/compilediff native` first: a benchmark
  whose compiles are the same needs no timing.
- **Allocation and CPU.** Allocation counts are deterministic and are
  compared exactly. A change that moves what is allocated is also compared
  by CPU time, which the fixed mode prints beside each benchmark (`cpu`),
  and at `GOMAXPROCS=1`: the collector runs on other threads, which hides
  its cost from the wall time of an idle machine but not from an embedder
  running a runtime on each core.
- **JIT code placement.** Native code has the same op-cache limit: one Are
  We Fast Yet benchmark, all of it native, runs 495 or 600 ms depending
  only on where its code lands (81% against 43% of its operations from the
  op cache). `placements -jit` moves where the JIT places its first code
  (`V8BENCH_JITPAD`), lays out its later chunks and the gaps between its
  functions by each placement's seed (`V8BENCH_JITSEED`, `07c5890d`), and
  starts each function's code 0 to 56 bytes into its line
  (`V8BENCH_JITLINE`). The JIT's Go side is not moved, nor distances inside
  a function, so a change that resizes earlier code can still look like an
  eight-placement regression of a later benchmark. Before calling one
  real, compare its native code's bytes per operation, its exits and
  compiles, run it alone (`-suite`) over the placements and several first
  offsets, and count its retired instructions (AMDuProfPcm): Towers' +7.5%
  and the V8 suite's +1.7% at `d4e1855` were the same code, byte for byte.
- Ideas measured and rejected are listed at the end, so that they are not
  tried again without a new angle.

## Open items

### The register interpreter and the object model

- **Raw int32 and float64 values outside asm.js.** A validated asm.js
  module runs as typed code, its registers raw int32 and float64 values
  for the whole function (register-interpreter.md, "Typed code"); zlib
  took 1,556 ms at v0.39.0 where it took 7,605. Other index-heavy code
  still pays a float conversion at every element access and every `| 0`
  (register-interpreter.md, "Known limits and open work"). A local that
  only ever holds an int32 needs mapped `arguments`, direct eval and reads
  before its first store accounted for.
- **asm.js modules' limits.** Only the first closure of a module's
  template is linked to typed code (`regTypedOf`), and a module
  instantiated a second time in a runtime runs as ordinary code. Calls
  between typed functions pass their arguments and result as Values:
  `asm_fib`'s typed code boxes its argument before each `call_w`,
  converts the result after it and boxes what it returns, though with the
  JIT it runs natively throughout (4.2 million native entries a run, no
  exit and no call of Go, 2026-11-25). A raw convention -- int32 and
  float64 arguments in registers, a typed entry past the parameters'
  conversions, the result raw, the resume after a level Go finishes
  converting it -- would save the callee's load and check and the result's
  box, about 15 to 20% of a recursive call (internal/jit/README.md, "Typed
  code"). At `976e5cc1` `asm_fib` takes 0.80 times QuickJS's time and 0.22
  with the JIT, `asm_call_leaf` 0.55 and 0.09, a call through a function
  table 1.03 and 0.16.
- **The leaf evaluators** (`pureCall`, `leafCall`), which evaluate register
  code (regcomp's pure and leaf bodies) for the Go table and wherever no
  native handler takes the call. A recursion of pure calls deeper than 32
  (`pureCallDepth`) is given up on whole and, after a few such, made with
  frames, where switching at the limit alone needs the pure path to pass
  on an error such as a stack overflow; and `pureCallAt` could switch on a
  case byte for each function.
- **Fresh prototypes.** Objects made on a prototype that is itself new --
  `Object.create` of a fresh object, `Object.setPrototypeOf` to one, a
  scope chain built of objects, a subclass made per call -- take 1.46 to
  1.75 times QuickJS's time at `976e5cc1` (`object_create_fresh`,
  `set_proto_fresh`, `scope_chain`, `subclass_fresh`), and as long or
  longer with the JIT: each turn's prototype is a new layout, so every
  read off it misses, and native code's loop is judged slow and left to
  the interpreter, as V8's stub cache misses too. Profiled with the JIT
  (2026-11-23), the time is the object model's: the native handlers 21%,
  `Object.create` 19%, the stores' slow paths (`rsSetPropLength`,
  `regSetProp`, `setPropCached`) and the fresh object's layouts
  (`newCreated`, `newShapedLiteral`, `ensureShape`, `fillPropCache`), and
  the collector.
- **Typed arrays have no shapes** (`shapeClass`), so no cache of the
  interpreter's remembers what a name finds on one: a method read such as
  `t.forEach` takes the slow path there. Their names but the canonical
  numeric strings are an ordinary object's; a shape of their own, whose
  cache leaves those strings out, would serve the method reads. The JIT
  reads them natively: the interpreter notes such a read for it
  (`ad839225`; `typed_some` 7.33 ns with the JIT, where it took 10.3).
  `typed_find` and `typed_findIndex` take 1.53 to 1.72 times QuickJS's
  time in the interpreter at `976e5cc1`.
- **A view that tracks a resizable buffer's length** keeps no storage and
  end, as a view of fixed length does (`9442ec8`; `fixedStorage` gives
  nil for one), so its elements take the slow path.
- **The `in` operator with a symbol key** takes the slow path: `hasQuick`
  answers indexes and key strings, and the realm's `hasCache` remembers
  only strings (`reg_in.go`).
- **Object helpers past the inliner.** `noneHas`, `rootFor` and the
  allocation helpers that call it, `newFuncObject` among them, are not
  inlinable at v0.45.1 (`-gcflags=-m`), each calling `rootFor` or
  `findOwn`, since the 32-byte objects (`5c26cde5`, which left them so:
  inlining them would save one call, not the call `rootFor` is).
- **A for-of cursor without an Object header.** The cursor is 88 bytes
  (`iterObject`, `d9173a40`, `323f9031`), in the size class of 96, and an
  array's closed cursor is taken by the next loop over an array
  (`arrCursor`); as a kind of Value of its own it would shed the header's
  32.
- **A closure's blocks inside its function object.** The capture list is
  in the object's allocation for up to 17 captures; the cells a closure is
  the first to open are not. Allocated with the object (three cells beside
  four to six captures, four beside twelve to fifteen, the size classes
  unchanged), they took 2,882 allocations, 17%, from an Earley run, 2.7%
  of its time on M5 Max arm64 and 7.2% on Windows amd64 (2026-11-21,
  Go 1.27.3, JIT off, seven rounds at two placements). It was dropped: a
  cell another closure shares then keeps the first closure's whole
  function object alive, with its properties and everything it captures
  (`TestCaptureCellsKeepNoFunction`). It needs the compiler to prove that
  no other closure, nor a second closure of the template, shares the cells.
  Its larger `regClosure` had also moved `regRunFD` and the cell-closing
  helpers 96 bytes, an 11% JIT `closure_make` gap.
- **EarleyBoyer's Earley** takes 1.61 ms against Node 26.8.1 jitless's
  1.06 ms on M5 Max arm64, where Boyer takes 19.8 ms against 20.9
  (2026-11-21, Go 1.27.3, JIT off, both placements, the series with a
  closure's realm in its template, Rejected): the suite's score is 16%
  below Node's for Earley alone. 74% of Earley's bytes are closures and
  their cells. Its `sc_list` reads `arguments` through a local alias,
  which no longer makes the object; V8's interpreter still makes it
  (`CreateMappedArguments`), so what is left is not a V8 interpreter
  optimization missing here (*needs timing*).
- **Implicit derived constructors** took 1.21 times QuickJS's time on the
  old tiers (356 against 295 ns, 2026-10-26); their closures are 224 to 288
  bytes since `59382e63`. No micro-benchmark has one: the extra tests'
  subclasses have constructors of their own (*needs timing*).
- **`NewRuntime`** makes about 600 built-in functions eagerly, its largest
  cost. Made lazily, they would change what scripts see of the global's
  descriptors, a decision for the API. A lazy property index for large
  built-in objects would save about 12 KB a runtime (2026-10-02).

### Allocation and the collector

Go's collector is neither generational nor moving, and costs CPU in
proportion to the allocation rate, mostly on other threads. Over Are We
Fast Yet the engine took 1.07 times QuickJS's wall time and 1.21 times its
CPU (2026-10-15): Havlak, DeltaBlue and Storage pay most. On a React
server render the allocation was, before the appends in place, the
`for...in` cursor kept and the room learned at a literal (2026-10-30):
growing arrays (`pureElemOp`) 25%, rope nodes (`String.Concat`) 21%, the
program's objects 22%, its arrays (`newArrayStorage`) 10%, `for...in`
cursors 8.5%. Those three took 200 pages from 1,455 MB in 13.5 million
allocations to 575 MB in 4.9 million (2026-11-06). What is left is mostly
the program's own objects, which only allocating less removes.

- **An array's outgrown elements reused** (F3). Go's `append` allocates a
  new block and copies, so about half the bytes a growing array allocates
  are blocks it has outgrown, dead once copied. A free list of element
  blocks by size class would take them back. The risk is a wrong answer
  rather than a crash: anything holding the slice across a growth --
  `o.elemsOf()` passed to a helper that appends -- would see reused values
  rather than stale ones. A literal's learned room (`arrayroom.go`) and
  growth by half from 16,384 elements (`growelems.go`) took most of
  the bytes; a capacity floor on the first growths does not help (below).
- **Strings.** Splay's `String(key)` makes 24,000 allocations a run
  (2026-10-12, *unverified*). Appends a binding owns grow in place
  (`compiler/strappend.go`, `vm/strappend.go`); forms not taken are
  `out = out + x`, as Pug writes it, a template's `with`, as lodash's, and
  a closure's append whose right side runs code, which could append to the
  binding between its read and the addition.

### Built-ins

- **RegExp.**
  - A global `exec` takes 1.33 times QuickJS's time (a sticky one 1.04)
    and `replace` by a template with `$` 1.34 (benchmarks.md, at
    `976e5cc1`): the match's array and its strings are one allocation
    (`45a402e2`), and the matcher was about 30% of the rest at `6a53e072`.
  - An exec's cost before its matcher runs: `/(a)?/.test` took 203 ns
    against Node `--jitless`'s 136 (2026-11-03, *unverified*), and every
    call of exec as a leaf pays it however little the match does.
  - A literal's RegExp is not given back after a replace by a function
    (1,496 bytes and 33 allocations an op), `matchAll` or `search`; the
    JIT's reuse stops for a realm whose `RegExp.prototype` changes its
    layout, and a pool loses its object at each deoptimization.
  - Lazy repetitions and counted loops still push the choice points the
    quick check could pass; a named group's groups object is a 112-byte
    allocation of its own.
  - Lending and returning the matcher's state cost 3 to 4% on many short
    ASCII matches (2026-10-21, *unverified*); `wtf8.ToUTF16` and split's
    appends allocate; `lastIndex` could have a layout made in advance.
- **String's methods.** padStart and padEnd with a filler past ASCII, and a
  repeat of a string that ends in a high surrogate and begins with a low
  one, take the old builder; the annex B HTML methods build Go strings and
  scan them again. Behind QuickJS on the old tiers (`3141a88`):
  `String.fromCharCode` of two arguments (113 to 119 ns against 83),
  startsWith and endsWith past ASCII (110 to 127 against 79 to 84), substr
  (124 against 105) (*needs timing*).
- **JSON** takes 0.41 of its former time and 1.10 times Node's (2026-11-06).
  Left: a batch of objects, as strings have, in which a dead object would
  keep what the others hold alive; a String's flag for text known to need
  no escape, for which it has no byte free; short strings' values shared by
  their text, as V8 internalizes them, which a document of different ones
  pays for; 17-digit doubles written, which are strconv's. The encoder's
  layout is a trade between its rows: `encodeElements`, `writeKeys` and
  `appendJSONString` sit at the same offsets modulo 4 KB at v0.42.0 and at
  `bed71e54`, 96 bytes before `b8c2c45f`'s, and at `976e5cc1`
  `json_stringify_ints` and `json_stringify_decimals` take 25.3 and 35.2
  ns, and `json_stringify_unique` with the JIT 124, against 24.8, 32.9 and
  114 at `b8c2c45f`; a sweep of the encoder's offsets by 32 bytes found
  none that holds every row (2026-11-21). Whether they are still slower
  over the placements, `-micro -k 4` against `b8c2c45f`, plain and with
  the JIT, would say (*needs timing*).
- **WeakMap.** `weak_map_delete` takes 1.48 times QuickJS's time, 1.77 with
  the JIT, and `weak_map_set` 1.53, at `976e5cc1`: Go's `weak.Make`
  searches a span's list of specials twice for each new key and takes the
  heap's lock the sweeper takes. A key's weak reference is to a holder,
  taken from a batch made ahead (`5589f873`, `1274da7d`); a record for each
  key could go only with ephemerons from Go's collector.
- **Typed arrays' `indexOf` and `includes`** poll for no interrupt: their
  loops over the elements call nothing that checks one
  (`builtins_typedarray.go`).
- **Date.** `date_parse` makes 12 allocations, which want go-intl's
  formatting to append (2026-10-12); it takes 1.16 times QuickJS's time at
  `976e5cc1`.
- **BigInt.** The FFT's scratch memory is not counted against the memory
  limit, and linear operations (addition, shifts, bitwise operations) are
  not interruptible: products, quotients and conversions are, at checked
  entry points (`d0acb93c`). ADX and MULX kernels on amd64 are parked:
  math/big multiplies 64 words in 1,756 ns against our 3,048 (2026-10-20).
  `pidigits` spent 13% in the free list and 9% in `s += y` (2026-10-20,
  *unverified*).

### Start-up, compiling and programs run again

- **The compiler's speed** (Octane's CodeLoad, 129.8 -> 102.8 ms once
  the stack bytecode went, 2026-11-17; 1.02 times QuickJS's time at
  `976e5cc1`). `Token.IsPunct` compares the token's text, and the name
  maps are each about 3% of a compile, the final tables are allocated one
  by one, and a binary node's `Op` is a 16-byte string where a kind would
  be one byte, at most of the compiler's sites (2026-11-06). A RegExp
  literal is validated by building and dropping its parse tree
  (`regexp.Validate`).
- **Resolving a function at its first call.** A function is compiled to
  register code at its first call, from its AST and the record of its
  scopes the walk made when the program was compiled, both kept until then
  (register-interpreter.md, "Compiled at the first call"). The AST is
  7.7 MB of Three.js's 14.0 MB after its load and 24.6 MB of Babel's 44.5
  (2026-11-15, the walk's stack bytecode, since removed, included). V8's
  way keeps neither: a preparse at load, which finds a function's extent
  and early errors and resolves no more of its inner scopes than it must,
  and a full parse of the function's source at its first call. Between
  them, a full parse and walk at load with the AST dropped and the
  function parsed again at its first call would release the same memory
  at today's load time. Both need a resolver that can start inside a
  function, in the scopes the load recorded around it -- eval, with,
  arguments and class fields included -- which the walk is not.
- **A `Program` run again** keeps its literals' roots (KI-66,
  `rerun.go`) and nothing else: each run interns its names, makes its
  constants and fills every cache anew, most of what a run of a large
  program pays before it does anything. Keeping a function's caches from
  run to run would not serve as it stands: a class a run defines is a new
  prototype, so a new layout, at each run, and the sites reading its
  objects would go megamorphic, where each run's own caches are
  monomorphic. To be measured as a host's load, a `Program` on a kept
  runtime, before deciding.
- **`Eval` of one source for each request** compiles anew. A literal that
  names its keys shares a layout with every other of them in the realm
  (`literalkeys.go`), and what reads it keeps what it learned; `{}`
  filled in after, a spread and the rest make their shapes anew. A cache of
  compiled source would serve such hosts; none has asked for one.

### The JIT's optimizer and backends

These are in `ssa` and `mir`, which register code is compiled through as
the stack JIT's was, and in the VM's calls of Go. Most were found on code
the stack JIT lowered; their counts are from then, and each was checked
in the register JIT's code at v0.45.1.

- **Shapes known across a loop's header.** A loop header starts knowing no
  shapes -- a block inherits what is known only from a single predecessor
  (`ssa/shapes.go`), and its entry for on-stack replacement knows nothing
  -- so a loop's first read of an object checks its shape at each turn.
  Moving the check before the loop, its exit the loop's entry, would check
  it once.
- **A property read is three dependent loads**: a value with a reference is
  carried as the address of the cell it came from, its reference read from
  it, then the object's values, then the value, where V8 loads an in-object
  field once (`3141a88`). An object's values sit behind a pointer since the
  32-byte objects (`fece0478`..`6c58742a`), so in-object fields would be a
  change of the object model.
- **`push` in a loop.** Native `push` appends in place and grows a full
  array by push's leaf, going on natively after it, for any number of
  values (`aad6f0b1`, `a7abd0b4`); it still checks Array.prototype's and
  Object.prototype's layouts at every call and stores its result to its
  keep cell, where V8 hoists the prototype checks out of the loop.
  `array_push_large`, 8,192 pushes onto `[]`, took 20.5 ns a push with the
  JIT against Node's 4.1 at `fb40baf`, before the growth was native
  (*needs timing*).
- **Escape analysis** (`ssa/escape.go`, partial since `41575a42`..
  `557ad428`). Left made: array and template literals, the largest group
  of Octane's candidates; a phi merging an unmade object with a made one,
  which Graal makes at the edge; a callee past the inlining budget whose
  result is a temporary, RayTrace's (a budget of 400 cut its allocations 6%
  at no gain in time); an object a rare path keeps in a local, which the
  frame state names and so makes; an iterator's `{value, done}` and a
  destructuring's array, the step being a call of Go (`ir.IterStep`); an
  object whose absent property is read, that is tested for truth or
  compared by `===` or `typeof`, or that is given to a call not inlined (CD's
  vectors); and an `arguments` object once made: an inlined callee's
  `arguments[k]` and `arguments.length` read the call's arguments, and a
  function's own code reads them by a call of Go while the object is
  unmade (`af716ef6`, `dab1d554`). A closure whose body calls another
  closure it captures needs its calls' feedback, which a template's
  function has only once it runs natively on its own; a closure made in a
  loop's turn over a per-iteration binding still takes the binding's cells
  each turn (`closure_make`'s remaining allocations). A loop whose calls
  leave it early makes its objects at each exit: `carried_exit_100`,
  `carried_exit_20` and `carried_every_2` were 3.1 to 3.3% slower than
  without the partial analysis (against v0.35.0, *needs timing*).
- **Inlining.** Callees with loops are inlined only when small
  (`maxInlineLoop`, 24 instructions) or when the loop calls a closure the
  caller gives: inlining Crypto's `am3` made its callers' code 60% larger
  and Crypto 25% slower. A rule that inlines a loop only where the call's
  cost is not spread over its turns is wanted, and a loop of few turns
  could be inlined given turns-a-call feedback, which a function does not
  have. A callback's default parameters are ordinary register code and
  refuse nothing; one with an array pattern among its parameters is not
  inlined (its iterator steps), nor two closures passed in one call
  analysed; a callee with `define_index` or a spread is a whole host, which
  Go makes only in the root frame, so it compiles for native callers but
  is not inlined unless the site's exit is rare (`ssa/build.go`).
- **Inlining sizes calibrated to V8's bytecode** *(JIT project)*.
  `inlineSize` counts a register instruction as 20/17 of a unit, a median
  over Are We Fast Yet's functions, which reads a loop about 20% small
  against V8's bytecode: a fused step-and-branch is one instruction where
  V8 has four. A per-operation count fitted to 3,790 functions has a
  median of 1.00 of V8's bytecode bytes, half of them within 7%, where the
  flat count's is 0.90 (2026-11-25). Alone it made CD 4.3% and Box2D 12%
  slower, the bounds being tighter than V8's; with V8 14.6's bounds too,
  Are We Fast Yet took 1.3% less, Octane 0.4% and the V8 suite 0.6%,
  Bounce 13% and RayTrace 3%, but Box2D 2.4 to 4.9% more, its allocation
  18% more (escape analysis on larger graphs), and compiles 3 to 8% more.
  Box2D's allocations come first, then both together with the tests that
  rely on the sizes rebuilt and the compiles timed per phase. Taken up by
  [stage 4](jit-design.md#stage-4-inlining-loops-and-redundancy).
- **Strict equality with a literal** is the operand's word, length and
  bytes against immediates for an ASCII literal of up to 64 bytes
  (`100eb7f3`). A longer or non-ASCII literal, and loose equality, take the
  generic `eqtagged`, about 450 bytes a site (not counted again), whose
  string comparison is the main path on string-heavy code (85 to 90% of
  executions on a render and on PdfJS). What the literal path saved in code
  size was not measured.
- **A construction Go makes from native code** *(JIT project)*. `new C(j)`
  of a class made anew at each turn is a call of Go whose constructor runs
  in a nested entry of the interpreter (`regConstruct`, `regEnterW`): the
  native construction's stores are refused, their caches missing every
  new root layout (`5bce0c43`). With the JIT `class_factory` took 11.1%
  longer at `976e5cc1` than at v0.45.0's measurement, 1,285 ns against
  1,135, at all four placements, the price of its loop running natively
  (benchmarks.md). The way is V8's generic construction: the object made
  natively from the constructor's initial layout, its root dynamic, and
  the body called by its code. Taken up by
  [stage 6](jit-design.md#stage-6-the-call-path).
- **Calls of Go.**
  - A leaf's Go side is about a hundred instructions besides the leaf's
    own: the routine and the ABI0 wrapper (27), `jitLeafGo`'s frame and its
    spills around the check's and the function's closures, the operands'
    slice's checks, the keep cell's barrier test. V8's fast API calls call
    a function of a typed signature; a native leaf's check and function
    fused, or forms of typed operands, would take most of it. `G.call(x,
    ...)` of a known leaf is the leaf's call (`be4de18a`).
  - Calls that save every register (`goSaving`) share one routine a
    function; pow's and `%`'s go to routines of their own (`ef7b8feb`), and
    the rest -- a back edge's poll, a pool's refill, a string's absent
    index, element stores, equality of ropes -- through `callGo`.
  - More leaves native code could make itself by the kinds it has
    (`ssa.LeafKind`): `charAt` and `at` past Latin-1 (a string made),
    `String.fromCodePoint`, a Map's `get` of a key whose kind the code
    knows. `Math.sin` and `Math.cos` are calls of Go through a routine of
    their own (`ef7b8feb`), not native; they were about 1% of Box2D,
    115,000 calls of Go in three runs at 18 ns (2026-10-20), and their cost
    a call has not been measured since.
  - `call`'s Go-made path is about 180 bytes a site, nearly all `goArg` for
    each operand: Go reading the operands from the call's records rather
    than `OffGoArgs` would save about 30 bytes a site, a reordering of
    `call` and of what `jitGoCall` reads across mir and vm, to be sized
    against `call`'s inline bytes first.
  - With a memory limit set, every operation Go makes for native code
    takes the framed path: each frameless path refuses while the runtime
    meters its memory (`jit_goops.go`, `jit_leaffast.go`, `jit_gostub.go`).
  - *(JIT project)* The Value operands of the calls of Go with routines of
    their own (`abi.GoStub`) are passed in the context's GoArgs. In fixed
    registers, as V8's call descriptors give a builtin's parameters, they
    would save a store and a store-forwarded load a word; the stub still
    stores them into the Go function's ABI0 frame. mir's scan gives each
    value one location for its whole interval, with no splitting at a use,
    and a Value's pointer word is read at the site from its source, so the
    constraint is not one the allocator takes as it is: either the scan
    splits intervals at each stub's use, which the spill classes, the
    frame states and the exit descriptions would have to follow, or the
    scan prefers the fixed registers by a hint and the moves left are
    resolved at the site (2026-11-24, deferred). Taken up by
    [stage 6](jit-design.md#stage-6-the-call-path).
- **Register allocation and the code**, each found on the stack JIT's
  code, its count from then:
  - A native call's state could be found from its return address, as V8's
    safepoint tables are, rather than stored by two instructions; each
    operand's pointer word makes an xmm15 round trip (2026-10-22).
  - Operands only the call reads are kept (`x.next` in `tail`), unless
    their source may be a keep cell the same run writes (2026-10-22); it
    may no longer hold, register calls writing their operands straight into
    the callee's window and the records leaving out what a call consumes
    (`30236e90`), which a listing of `tail` would show.
  - `this` carried as a phi takes the compare-number-words path when its
    pointer word is read (2026-10-22).
  - Intervals touching at one instruction leave loop-carried moves
    (try_catch's sum, array_for_of's, a counter after its `addsd`):
    mir's intervals have no holes, so lifetime holes, or a result reusing
    a dying operand's register, which every emitter would have to allow
    (2026-10-23). internal/jit/lsra, which has holes, is the WebAssembly
    JIT's.
  - Exit stubs load their descriptor's address in 10 bytes, where an index
    would do; guards of one frame state and kind share one stub (`stubAt`);
    a loop counting its budget in a register stores it back on the way to
    each exit, 3 instructions Go could replace by reading the budget
    register from the saved registers (2026-10-27).
  - The context's fields past its first 128 bytes take a 4-byte
    displacement each: spills at offset 10,816, 749,000 accesses, all
    have one, and `[reg+0]` carries a displacement byte, about 220,000
    operands (2026-10-23). Displacements were 22% of the bytes of
    Typescript's code, and exit stubs about 5%.
  - Compare-and-branch fusion requires that nothing be emitted after the
    compare, which leaves about 80 of Octane's sites unfused where only
    captured bindings' loads follow; a truthiness that redundancy
    elimination shares between branches stays a boolean (2026-10-22).
  - A loop's number carried past its end is boxed once where it is used
    past the loop (`30236e90`); whether forEach's and a typed forEach's
    captured `sum` is still stored to its cell at each turn, and `some`'s
    `===` boxes, a listing would show (2026-10-27).
  - Constants are stored into spilled phis (25,000 in Box2D), and cold code
    is about 23% of all code (2026-10-14).
  - For the op cache: 64-bit constants come from a pool after the code
    (`30e4e15e`), and slow paths lie past the hot code; entry blocks out of
    the hot region, and the exit stubs' 10-byte addresses, are left
    (2026-10-16).
  - Clearing a keep cell from its highest used (KeepHigh) took Typescript
    1.4% faster and Towers 11 to 15% slower by layout; parked (2026-10-20).
  - `bigint64_arith`'s loop counters as unboxed float64 phis ran about 7%
    slower than tagged, unexplained (2026-10-20, *needs timing*).
  - The review of the first pipeline: `Validate` refuses Opaque and String
    literals but accepts Uninitialized ones (`ir/validate.go`); native
    code is held as a slice of mapped memory, not a `uintptr`, and
    released by finalizers rather than `runtime.AddCleanup`. The offsets
    abi hard-codes are asserted at compile time against the VM's structs,
    and the hand-written assembly reads no struct's field.

### What the register JIT took over from the stack JIT

These were open against the stack JIT and concern mechanisms the register
JIT took over from it; each was checked in the register JIT's code at
v0.45.1, its figures being the stack JIT's unless dated later.

- **Element kinds are learned only at exits.** V8's keyed loads and stores
  record the maps and elements kinds they meet in the interpreter, so
  optimized code is compiled for a typed array from its first compile. Here
  an element read's kind is learned at native code's own exits
  (`jitRegElemSeen`, `jit_elem.go`): a site the first compile took for an
  array's meets typed arrays at exits, and the code runs on, learning,
  until a budget every cache change restarts is spent -- Gameboy's
  `executeIteration` read `TICKTable[op]` as an array's for 3,076 exits a
  run. The fix is V8's: the interpreter's element reads note the kinds
  they meet per site, as its string additions (`2e86ab34`) and its method
  reads of typed arrays (`ad839225`) are noted.
- **Compiles in flight are outside the budget.** What compiles in the
  background hold -- up to four workspaces (`jitJobsMax`), their code and
  snapshots a runtime -- is the process's, outside `WithJITMemoryLimit`,
  whose count (`held`) has no job in it, and a runtime that does not run
  keeps its finished ones until it runs or closes.
- **A native call's cost.** Inlining's cut of rarely-called callees made
  Richards 1.2% slower at every placement: `runTask`'s `packetPending`,
  called 0.129 times a call over a run, becomes a native call 2.5 million
  times a run, which costs more here than a call in V8's code does
  (*needs timing*).
- **Frequencies are the warm-up's.** The feedback dispatcher counts each
  call site's calls for each run of its function (`jitRegCounts`,
  `jit_freq.go`); a call never counted is cold after 59 runs
  (`jitCountsEnough`), unknown before. Native code counts nothing, as
  V8's optimized code counts nothing, so code compiled again for what it
  learned uses the counts its function had before it ran natively.
- **Calls inside an inlined callee.** A call of Go inside an inlined
  callee is made from native code where it needs no VM frame -- a
  megamorphic read's miss or write (`142f73dc`, `a6652098`); one that
  needs a frame -- a getter, a receiver to coerce, a throw to a handler --
  leaves, the exit making the frames.
- **Branches never run before the compile deoptimize one at a time**
  (Gameboy's `memoryReadJumpCompile` 10 times, Typescript's `innerScan`
  11); borrowing the shape from the function's other reads of `this` cut
  Gameboy's code 33% and grew Typescript's 6 to 13%, and learning more at
  each deoptimization would depart from V8.
- **Native generators.** Only a synchronous generator without `yield*`
  whose locals no closure captures is resumed natively, by a call, a
  for-of or a destructuring's step (`326ac7c7`); `return()` and `throw()`
  are Go's, nor are a spread or `Array.from` of a generator native steps;
  a `next()` result is an object from a pool.
- **A number's method sites.** A primitive's method read is a case of its
  site beside the objects' (`a7d30aea`), found along the kind's prototype
  chain (`1f775fc2`), each kind learned once, at the exit that met it. A
  sloppy method on a primitive is called by Go from native code, which
  wraps the receiver, but exits where it is inlined.
- **`String.fromCharCode`** past Latin-1 is a call of Go only in a
  function's own code, and exits in inlined code (PDF.js's `Lexer_getObj`
  inlining `Stream_lookChar`).
- **Async functions with the JIT.** `async_await` took 5 to 10% longer
  with the JIT than without it on the stack JIT. On the register JIT the
  async rows and `generator_delegate` took 1.04 to 1.17 times the
  interpreter's time with the JIT (2026-11-22), their native code a small
  share: an await's resumption is a stretch of 5 to 10 instructions, less
  than an entry costs, so native suspension at await was judged not to
  pay, and the difference is Go code the JIT build places differently --
  `resumeAwait`, `job.call`, `regGenRun`, `findOwn` -- which the layout
  lock does not anchor (*needs timing*).
- **Octane's Typescript compiles about four times a function** (2,400
  compiles of 607 functions in three runs, stack JIT), a third of them a
  function whose code was dropped for room or flushed, compiled again
  into code of the same size. Its method reads at megamorphic sites left
  native code some 330 times a run until Go made them from native code
  (`080f535e`); its compiles per function a run on the register JIT
  are to be counted.

### The JIT's compile cost and memory

- **Compiles cost more than a short program gains.** A fresh runtime's
  first server render took 38.2 ms with the stack JIT compiling in the
  background (35.5 with its snapshot) against the interpreter's 21.9
  (`86cb59be`..`fd6c4f88`); the register JIT took 25.4% off the first
  render (`388324de`), which is still to be compared with the interpreter.
  Measured on the stack JIT (2026-10-30), compile time followed the code
  emitted, about 120 bytes of machine code a stack instruction (156 in
  Typescript, 168 in PdfJS; 352 KB for one
  2,982-byte switch whose few arms run), at about 5 us an instruction --
  1 to build, 1.5 to optimize, 1 to emit -- and grew as size^0.82; no
  phase was superlinear. Three ops were about half of the code: on the
  render `call` 28.7%, `eqtagged` 20.4% and `keep` 10.5%, on Typescript
  22.8%, 12.5% and 13.5%; on Box2D the property ops lead. A call's inline
  bytes are per site by nature (see Rejected); emitting less is what is
  left. The register JIT's cost an instruction has not been measured.
- **The JIT behind the interpreter in a cold run.** In Octane's fixed work,
  one run of each benchmark, the JIT takes longer than the interpreter at
  `976e5cc1` on Richards (77%), DeltaBlue (51%), RayTrace (49%), PdfJS
  (32%), Typescript (27%), Box2D (8%), CodeLoad (5%) and RegExp (1%)
  (benchmarks.md). Most of it is the time compiles take on the runtime's
  own goroutine (2026-11-17, `31c7f1b9`, a Ryzen 9 9950X3D, one run): PdfJS
  ran 60 ms behind and compiled for 66, Typescript 200 behind and compiled
  for 373, Box2D 10 and 23, Richards 1.5 and 3.2; without it each was
  faster than the interpreter, RegExp level, and by five runs the JIT was
  ahead on all but DeltaBlue. CodeLoad compiles nothing. A compile costs
  0.25 to 0.5 ms, its SSA build 36% of it, the encoding 20% and the
  optimizer 18%. Compiling a callee on its own budget rather than at its
  native caller's call (Are We Fast Yet +15%) was dropped. Compiled in the
  background (as `qjs --jit` compiles), the compiles leave the runtime's
  goroutine: Octane's fixed work takes 0.92 times what it takes compiled
  in the foreground (1,223 against 1,326 ms over four placements,
  2026-11-18), where it had taken 1.49 times, zlib 3.3 times; Are We Fast
  Yet's takes 1.004 times, DeltaBlue 1.03 (1.13 before). A compile in the
  background makes the code a compile on the spot makes and is run as
  soon as it is installed (`0cf4b86d`, `842fda18`). RegExp's share was its
  exec arrays: the interpreter reuses them (`regexpreuse.go`), which a
  runtime with the JIT did not until native code's calls of `exec` made
  them again too (`e9bf2365`; Octane's RegExp with the JIT allocated 131
  MB, now 77).
- **The tier-up's budgets** *(JIT project)*. A function compiles at 400
  invocations' worth of work, V8's count for its first optimizing tier, and
  a deoptimization starts the same budget again: on a Ryzen 9 9950X3D2,
  Octane -n 1, recompiles after a deoptimization were 57% of Typescript's
  compile time, 47% of PdfJS's and 45% of Gameboy's, most of Typescript's
  guard failures shape guards of reads that each meet one new shape
  (2026-11-17). The tier trace (`vm.JITTierHook`, v8bench `-jittier`,
  `442c9617`) shows each decision. V8 14.6's budget, begun after eight
  invocations' worth, and its delay after a deoptimization were built and
  measured on a Ryzen 5 3600 over two placements (2026-11-24): Octane -n 1
  0.993, -n 5 1.006, -n 20 0.986, the V8 suite 1.013, RayTrace 1.066 --
  its `testIntersection` compiled at its 166th invocation rather than its
  500th and deoptimized, its feedback still changing, which V8's raise of
  the budget at a cache's change would have waited for. They are parked
  until a baseline tier and fast calls of Go from native code exist; a
  later first compile like V8's top tier's was dropped (Rejected, "A later
  tier-up"). Taken up by stages
  [0](jit-design.md#stage-0-correctness-and-hygiene) and
  [1](jit-design.md#stage-1-tiering-and-background-compiles).
- **Where a compile's time and memory go.** `-jitcompile N` times each
  phase (`mir.Times`); the shares measured before the phases were split --
  `allocate` 14% of a compile, the build's `plan` 15% (most of it the
  feedback's answers), the assemblers' `Finish` 9% -- are not measured
  again. A call's cold helpers are compiler methods since `1c8d57fe`; the
  other cold paths are closures, 82 sites on amd64 and 78 on arm64. The
  register-group allocator added about 25% to allocation on Typescript
  and 7 to 20% to `mir.CompileIn` (2026-10-23); encoding is 3 to 10% above
  its earlier cost from loop alignment (*unverified*). Optimize runs its
  rounds until one finds nothing (`ssa/opt.go`), and the last finds
  nothing in most compiles (3 to 4% of their time); skipping it soundly
  needs care with `unboxPhis`; worklists would replace the rounds.
- **A compile's working memory** is outside every runtime's budget, in a
  pool the process shares (`f1842cd8`), a workspace let go after two of the
  collector's cycles unused. The budget weighs only what a compile will
  hold, 512 bytes an instruction and 64 a slot (`jitCompileBytes`,
  internal/jit/README.md); the working memory is linear in the function's
  size (`TestJITCompileLinear`), and was about 7.5 KB an instruction on the
  stack JIT (17 KB for Box2D's `initPhysics`), not measured on the register
  JIT. Its time a value grows past the caches: the SSA is linked by
  pointers (`ssa.Value`'s `Args`), and over eight times the values of
  `TestJITCompileLinear`'s blocks shape every phase takes 7.1 to 8.0 times
  the steps, but loops' emit 16.9 times (2026-11-25); a compact SSA, the
  allocator's data laid out by value index, is a design change of weeks.
- **The slot IR's size.** An `ir.Instruction` is 144 bytes, three 24-byte
  operands among them, with a 16-byte StateMap for each instruction
  (counted at v0.45.1): packing the operands' slots to `int32`, and
  literals as indexes into a constant table, would take a large part of
  it. Its share of a program's bytes, 83% on the stack JIT, is not
  measured again.
- **The code arena's free space** between live functions is neither counted
  nor reused until its 64 KB chunk empties: 460 to 520 KB mapped for 323 KB
  live under a 1 MB limit (2026-10-12, *unverified*). An arena that places
  code in released space (`jit.NewReusingArena`) exists and only the
  WebAssembly engine uses it. Code of any length can be placed: arm64's
  assembler keeps its branches in reach by veneer pools.
- **What code in use was compiled for stays alive.** Code unused is flushed
  with what it was compiled for (`bc35a9f`), but code in use holds its call
  targets' closures (`nativeCalls`), its holders and the closures it
  inlined strongly, where V8 holds them weakly and deoptimizes code whose
  objects die. Go gives no hook to invalidate code before a dead object's
  address is reused, so a weak target needs code that checks it is alive
  before it is entered. Octane's Typescript kept about 110 MB more than the
  interpreter while it ran (stack JIT).
- **The default room for code** *(JIT project)*. The runtime's default is 8
  MiB (`jitCacheBytes`), and v8bench runs with 32 MiB: on the stack JIT
  Octane's Typescript took 3,750 ms under the default against 2,850 under
  32 MiB (`WithJITMemoryLimit`'s documentation). More room must never be
  slower, and before the flush was: with 16 MB Typescript took 18% longer
  than with 8. The JIT project makes the default 64 MiB. A stretch
  of native code is judged by 20 instructions of work against an exit
  (`jitSSAMinWork`), far less than an exit costs. Taken up by
  [stage 0](jit-design.md#stage-0-correctness-and-hygiene).
- **The profit model is parked.** `a21df702`, never merged, has a
  calibration (`TestJITCalibration`, `QUICKJS_JIT_CALIBRATE=1`) and charges
  a callee's exits to the callee; Splay, Json and RegExp were slower with
  it. Its kernels no longer measure the endings they were written for
  (most calls of Go and exits they timed are native now), so a return to
  it starts by redesigning them.
- **A flush's back-off**, the wait doubled for code flushed before, took
  Typescript's compiles from 1,807 to 995 in five runs but its live heap
  from 262 to 356 MB (2026-10-23); since the flush's clock leaves the
  compiles' time out (`3822dbce`), what it would gain is not measured. The
  flush waits a fixed age (`jitFlushAge`).

### Measurement and tooling

- **What the layout lock leaves free.** The lock as first made pinned
  `6022049a`'s layout; over four placements the locked `ce98c3cf` against
  `6022049a` measured 0.990 on the micro-benchmarks' geometric mean, 0.999
  with the JIT, none 3% slower at every placement. Pinned since: the
  generics after the native handlers' block (CZ), the string appends and
  the RegExp leaves (GK, MVT), positions for hot code far past its anchor
  (`9852d3f2`, `8f56e006`), and the typed arrays' callbacks, named
  functions (`21f3e0f8`). Not pinned: `setRoot`..`firstRoot`,
  `newPolyObject`, the constructors' `chargeChild`, generics against each
  other, closures against each other, read-only data, and code between two
  of its positions.
- **The JIT's Go side past JN** *(JIT project)*. Against `bed71e54`, the
  register JIT's last gaps closed left four micro-benchmarks slower with
  the JIT at both placements: `array_indexOf` and `forward_array` 5 to 8%,
  `array_pop` 5%, `json_stringify_tojson` 3%. Their native code and JIT
  reports are the base's and the Go they run is unchanged; what moved is
  where it sits, by the Go the change added to `jit_call.go`,
  `jit_feedback.go`, `jit_frame.go` and `jit_native.go` after JN
  (`jitGoCallPlain` and `jitMaybePlain` 1,952 bytes modulo 4 KB,
  `jitForwardPlain` 2,976). Two positions at the base's offsets recovered
  part of it (`array_indexOf` 4.2%, `forward_array` 2.6%, `array_pop`
  3.1%, `array_slice` 4.5% slower at one placement) and were not taken
  (2026-11-25). Accepted as layout. Taken up by
  [stage 0](jit-design.md#stage-0-correctness-and-hygiene).
- **What the placements do not vary.** Read-only data and Go's type
  descriptors: two builds of the same code, their type descriptors 32
  bytes apart, took 211 and 192 ms on `string_template` (2026-11-04), and
  a type added in a file of package vm, every anchor at its target, moved
  `runtime.types` 192 bytes and the descriptors `newObject` and
  `newArrayStorage` load 248 to 512 (2026-11-25, windows/amd64), which
  neither the lock nor `TestLayoutLock` sees; a new equality function is
  caught (`layouteq.txt`). Where package vm sits against the packages
  linked before it: a change in `internal/jit/mir` alone moved vm 10,464
  bytes and made RegExp 2.4% slower, the parent padded the same as slow
  (the lock's regexp pad now holds the matcher). ASLR, but on Windows,
  whose builds are timed at their image base (`-fixedbase`). Distances
  inside a JIT function, whose start alone moves within its line
  (`V8BENCH_JITLINE`): `bigint64_search` was 25% slower at six of eight
  offsets for one byte more inside its function (2026-11-08).
- **A copy of a build at one speed of its own.** On Windows two copies of
  one build can time apart, each the same every run: two of `91f885a1`'s
  took 26 and 46 ns on `json_stringify_ints`, and ten of twelve 49 ns on
  `json_stringify_decimals` where two took 35 (2026-11-20). Most of it is
  where Windows loads each file, in steps of 64 KB: the placements tool
  and the matrix load their windows/amd64 builds at their image base
  (`-fixedbase`), and of ten such copies of `91f885a1`'s all took 49. But
  of eight copies of `b5df63ea`'s at a fixed base one took 48 where seven
  took 35, every run, and the same bytes copied again to its path took 35
  (2026-11-21): the file's own copy, where its pages lie in memory most
  likely, which the caches past the first index by. So the tools also
  run each process from a fresh copy of its build there (`-freshcopy`).
  Open: how much of a row's spread between placements was its copies'.
- **test262 with `-conformance.debugger` and `-conformance.jit`
  together** fails "no test ran native code": code compiled for a debugger
  is never native, and the runner's check does not allow for it.
- **Flaky tests.** `TestJITNoHeapWalks` once in four runs under
  `QJS_JIT_STRESS`, the meter counting the process's allocations, which
  another runtime's cleanups may make meanwhile;
  `TestWorkspaceCompileAllocations` under `-race`, 64.2 against its limit
  of 62 (2026-10-27), which no CI job runs under the race detector. Each
  *unverified* since.
- **The marking rule's checks.** A test scanning the source for a raw store
  to a pointer field outside the `storeRef` helpers; `storeRef`'s bulk
  forms, clearing n values or copying n from GoArgs, where a call's setup
  makes two or three calls of the routine (2026-10-19).
- **arm64's timing.** The JIT runs on darwin/arm64 and linux/arm64; broad
  timing of native execution there is open.

### Semantics found on the way

Checked at v0.45.1 against Node v26 where marked.

- Stack traces unlike Node's (checked): a receiver's own `constructor`
  property, which V8 passes over; an arrow called as a method, which V8
  names by its receiver; an object made by `new P` after `P.prototype` was
  replaced.
- `new.target.prototype` is read at `new` (`constructWithTarget`), where
  the specification and V8 read it in `super()`, as a Proxy given to
  `Reflect.construct` as its new.target shows (checked).
- `a[k] op= y` with an object key converts the key once, as C QuickJS
  does, where Node converts it twice; no test pins it (checked).
- RegExp's legacy statics are updated in the current realm, where the
  standard has them updated only when the RegExp's realm is it (test262
  passes).
- A breakpoint resolves to one statement, so of the copies of a finally
  clause the compiler makes at each break or continue, only one stops at
  it (code for a debugger keeps one copy on the normal path).
- An optional chain's call and a spread call in tail position are not tail
  calls, as the standard has them.
- A private field added to an object that is not extensible is refused, as
  the proposal has it; Node v26 adds it (checked).

## The register JIT's remaining gaps

The JIT compiles register code alone since `388324de`. The stack JIT's
tests were ported to it, and some of them held only for what the stack
JIT did and the register JIT did not yet: each was skipped by name, with a
`t.Skip` that said what fell short. Each gap was closed by giving the
register path the mechanism its test describes, never by loosening the
test, the skip going with the fix; none of them had been a wrong answer.
No skip remains (`grep -rn "register JIT gap:" internal/vm` finds none),
and none has since v0.45.1. What closed them:

- `TestJITKeyedWrites`, `TestJITSSAStringAddCallsGo`,
  `TestJITSpreadFrameless`, `TestJITBigTypedArrays`,
  `TestJITPropertyReleaseAndGC`: Go's calls from native code on the
  register frame -- a literal's field, a generic BigInt operator in an
  inlined callee, a receiver's own accessor -- and their tests' setups.
- `TestJITSuperReferences`: `super_set_index` run whole, a whole Host's
  fourth read (`ir.Instruction`'s `Fourth`).
- `TestJITArrowSharedBindingCalled`, `TestJITDerivedNativeConstructArrows`:
  an arrow sharing a bound derived constructor's this learned from the
  function a call called, its `this_bound` its register 1; a closure that
  captures none of a native callee's registers made on its level.
- `TestJITArgumentsInRecursion`: an inlined call's arguments read by a key
  no constant picked natively (ssa's `argumentSelect`); a closure the
  caller's own code makes called by its code from the first compile.
- `TestJITAccessorsNative`: register code's reference reads find their
  accessors; an accessor not inlined called natively, its window made
  past the VM's stack's top (mir's `windowAccessor`).
- `TestJITExceptionHandlers`, `TestJITCatchCallees`: a throw from a call of
  Go goes on at its handler in native code (`abi.GoCallState`'s `Catch`,
  mir's `catchStub`).

**Tests ported, not skipped.** Of the tests that read the stack JIT's
internals, four were ported to the register JIT's in the removal and run:
`TestJITClosureCacheRelease`, `TestJITPropertyCallbackRelease`,
`TestJITProgramRerunsKeepLayouts` and `TestJITStringBoundaries`. The fifth,
`TestJITPropertyReleaseAndGC`, was a gap, closed (above).
`TestJITLoweringsOnlyInRoom` runs again since settling takes young code
once the JIT is past its budget.

What the register JIT took over from the stack JIT and still leaves open
is under [Open items](#what-the-register-jit-took-over-from-the-stack-jit).

**Left by the removal's timing.** Against the base before the removal,
over four placements, every suite's total was level or faster, and the
register JIT was slower than the stack JIT by more than 3% at every
placement on Octane's RayTrace (+7.6%), Are We Fast Yet's Json (+8.2%),
Permute (+7.0%) and Queens (+6.3%), and on 27 micro-benchmarks. The base
is gone; against the stack JIT's figures at `acffb1aa`, benchmarks.md has
Json 4% shorter at `976e5cc1`, Permute 1% longer and Queens 6% longer, and
the rows slower with the JIT than without it.

## Rejected

Measured and dropped; each needs a new angle before it is tried again.

- **Small integers tagged as integers in a Value.** Go's register ABI
  picks a value's register class from its field's type, so one layout
  cannot make both kinds cheap. In a kernel of Crypto's `am3` and
  NavierStokes's `lin_solve` (2026-10-02, not committed), an int32 in a
  NaN payload of a `uint64` field took ints 40% faster and floats 58%
  slower; of a `float64` field, 3% and 1% either way.
- **GOGC tuning, and PGO**, the engine being a library.
- **A nursery** for the engine's objects: 40% of EarleyBoyer's small
  objects die within 4,000 allocations, but nearly all of Splay's survive.
  Smaller objects did what it was for (`6e5ce1f`, `944e2e8`,
  `fece0478`..`6c58742a`).
- **A realm's root for each count of a literal's keys**, V8's
  `ObjectLiteralMapFromCache`: the engine's fast paths follow a layout's
  first transition, which a shared root has for one literal's next key
  alone, and native spreads and clones are compiled along it. With the
  JIT, over four placements, `object_spread` +174%, `prop_clone` +192%,
  `prop_delete` +29%, `object_getter_spread` +36%, `object_computed` +13%.
  A root for each set of keys is kept (`literalkeys.go`).
- **An outgrown creation site's objects in a table sized to its largest**,
  up to 32 properties: one large outlier makes every small object take 1.6
  times the memory.
- **An array's room by other rules.** A capacity floor of 4, 8 or 16
  elements on the first growths left a render's allocation count
  unchanged (the growth is in large arrays). Sampling a literal's first
  execution kept what code loaded once had made, and Octane's CodeLoad took
  16% longer; clearing the sample at a collection rather than making it
  weak lost most of what is learned; inheriting the room within a turn
  left several callbacks 7 to 18% slower; learning large lengths in native
  pools alone left `array_reduce_upvalue` 47.6% slower. Growing by half
  from 1,024 elements made `array_push_large` 3 to 16% slower, and
  doubling cost `array_push_28k` 7.4% more bytes.
- **Appends.** An append followed by a read of its binding made a plain
  addition, to save what the mark costs: every read micro-benchmark was
  level without it, and `local_append_keep` 17.4% slower with it. Doubling
  a tail from 1,024 bytes gained 1.9% of a render and left `template_list`
  and `append_read_each` 6.1% and 16.6% slower.
- **Equal strings as one pointer.** Nothing promises interning: a literal
  is a new `*String` for each closure prepared, a key handed to a script a
  new one each time, and the atom table interns names, not string values.
  Equal strings behind different pointers are 7.9% of a render's string
  comparisons, so native code may take pointer equality as equality only,
  never inequality, short of a representation change across the VM.
- **JSON.parse after simdjson and sonic**, one thread, three alternating
  runs: a fraction's digits eight at a time made floats 6% faster but
  decimals 8% and exponents 5% slower, most numbers being short; spaces a
  word at a time made pretty-printed text 4% slower. A structural index was
  not built: scanning is 10 to 25% of a parse, against the collector's 24
  to 50%.
- **One free block a size for BigInts; an inline rope check in
  `String.Go()`; no RegExp object for `/lit/.exec(s)`**, which scripts can
  observe.
- **A first compile without the optimizer.** Without its rounds the code
  grows 15 to 30% and compile time falls 0 to 13% -- on Typescript it rises
  9% -- because the optimizer is what makes the backend's input small.
  Never compiled again, such code keeps 59 to 85% of the speed (Octane
  +41%); compiled again in full once hot, it keeps the speed but makes a
  first render worse, 80 to 85 ms against 63 to 66, hot functions
  compiling twice in it (stack JIT, 2026-10-30).
- **A later tier-up.** `SetJITWarmUp` at 400, 1,500, 3,000 and 10,000 put
  the first ten renders at 315, 268, 250 and 197 ms against the
  interpreter's 152, and the steady state at 13.8 ms either way: later
  compiles spread the cost and never remove it, the functions being hot
  across renders. A sampling profiler to measure each function's benefit
  was declined too: a ticker a runtime is a poor bargain for a host running
  one a core or a request, tiering would stop being reproducible, which the
  differential fuzzer and the placements need, and it would be off under
  the race detector.
- **A recompile delayed after a deoptimization.** Waiting in proportion to
  the compile time of the code that deoptimized put a render's second run
  below its first but cost Box2D 11.0%, zlib 5.4%, Typescript 4.1% and
  DeltaBlue 5.7%. Weighing the wait by the native work the code did does
  not separate the cases: the share of its budget a code had used before
  deoptimizing was 0.028 for a switch where recompiling buys nothing and
  0.027 for Box2D's hot function, where recompiling soon is what makes it
  fast (stack JIT, 2026-10-30).
- **Counting call frequencies in native code**, a saturating counter at
  each call site: it took the extra inlined IR down 14% and half the extra
  compile time but cost as much as it saved, and `func_call` 11% inside
  loops. The tiers below native code count them.
- **A call's code shared between its sites.** A native call site is 430 to
  470 bytes inline and 268 to 334 cold; the largest parts, the per-target
  checks and the arguments, are per site by nature, and the constants are
  already RIP-relative reads from the context's table. One stack-room check
  for a polymorphic site's largest target would save 5 to 10% of a call's
  bytes, 1 to 3% of the code. A convention that pushes arguments and jumps
  to one entry, as V8's does, would be a backend redesign.
- **A stack of native code's own**, so that calls are `CALL` and `RET`:
  native code runs on the goroutine's stack and never moves SP, so that
  Go's unwinder, stack scan and growth meet no return address of its, and
  each return is an indirect jump the return stack does not predict.
  Measured apart, an unpredicted return costs 0.9 ns a call where sites
  follow one another and 1.5 ns where they are picked at random, at most a
  tenth of a native method call.
- **Dropping the JIT's lowered programs** to save memory (stack JIT,
  2026-10-30): only 1.8 of their 3.29 MiB on a render could be freed, and
  lowering them again cost about 10 ms a render. Under pressure the cache
  drops the lowerings of functions with no entry before any code.
- **A closure's realm in its template.** With the realm shared through
  `closureCode` as the module environment is, a closure was 40 bytes, not
  48, and Earley allocated 4.6% fewer bytes. But `callObject`'s and
  `callDirect`'s realm checks took a load more: each grew 32 bytes, which
  moved `pureElemOp` off its 64-byte alignment -- `array_pop` 5.5% slower
  at every placement, level with the code after `callObject` padded back
  to its offsets -- and wrapped O's pad, at its minimum in the builds
  with the JIT, a page on (2026-11-21, Ryzen 5 3600). `closure_many_locals`
  was 5.9% slower on arm64 at both placements with that 40-byte closure, a
  cause not found: restoring the native handlers' addresses, or the
  one-capture object's size, did not remove it. The realm stays the
  closure's, read in one load as before.
- **Trimming entry clears by backwards liveness** (2026-11-17,
  darwin/arm64, JIT off, two placements) saved only 1.5% on EarleyBoyer.
  The compiler pass and its interaction with the JIT's use of `Inits` to
  identify sole BigInt bindings did not justify that gain; it was removed.
- **The native interpreter probing the shared constructor cache for
  instanceof** (same comparison) cost 1%. Collections clear that cache,
  and a native handler that subsequently succeeds through its ordinary
  path never fills it again. The retained optimization keeps the prototype
  slot in the property's site instead. Its probe runs after the site
  address is computed: probing before it repeated that work on misses
  and made a primitive-only loop 9% slower; sharing the address brought
  that cost down to 2%.
- **Branching through comma expressions and strict boolean wrappers**
  (2026-11-17, darwin/arm64, JIT off, two placements) removed five
  instructions from Boyer's equality function: `(a === b) !== false`
  branched on the comparison without materializing its boolean. EarleyBoyer
  stayed at 26.15 ms, within 0.1%, against the version with parameter
  stores completed in the allocation call. The compiler change was removed.
- **Prepared objects for constructions** (2026-11-19, Go 1.27.3, JIT
  off, three rounds at two placements): a site whose base constructor's
  initializer and shape had settled kept 32 objects made ahead, which
  `new_w`'s native handler took without calling Go, Go refilling the
  pool when it ran out. Far fewer allocation calls, but the target
  suites were 1.6% slower, 2.1% in a repeat, RayTrace 3.3% slower at
  both placements. The pool was removed.
- **Smaller native interpreter changes against `dc7ddd31`** (2026-11-19,
  Go 1.27.3, JIT off, three rounds at two placements unless said, on an
  M5 Max (arm64) and an i9-9900K (Windows amd64)), each removed for a
  gain of under a per cent or one that did not hold on both machines.
  The figures are RayTrace, EarleyBoyer, Splay:
  - The barrier flag's padding moved past an unconditional jump, every
    handler's offset kept: arm64 -0.5%, -0.1%, +0.1%; amd64, five
    rounds, +1.5%, +0.2%, +1.3%.
  - arm64's property slot address as one shifted add: -0.2%, -0.1%,
    +0.5%.
  - An unbound function's kind remembered in an object flag, so that
    instanceof skips its kind and bound checks after the site's shape
    check: probed out of line, level, -0.2%, +1.8%; inline, +0.1%,
    -0.7%, -0.6%, EarleyBoyer faster at one placement alone. Neither
    pays for an object flag.
  - The old pointer's equality check skipped while the collector is
    idle, on arm64: -1.0%, +0.1%, -0.4%.
  - Adjacent property reads fused three or four at a time rather than
    two, a count checked in each pair, on arm64: RayTrace +3.8%, slower
    at both placements, EarleyBoyer level, Splay -0.4%.
  - A settled two-property constructor's object allocated inline: arm64
    +0.6%, -0.7%, -1.5%, nine construction micro-benchmarks 0.997;
    amd64 +0.3%, +1.2%, -1.0%.
  - The next instruction loaded before its pointer is advanced, every
    offset and count kept: arm64 +0.3%, +0.9%, +0.4%; amd64 -1.1%,
    +0.1%, -0.1%.
  - Native globals' slot bounds check dropped, the epoch proving the
    slot: arm64 +0.3%, -0.4%, -0.2%; amd64 -1.4%, -0.9%, +0.7%. With
    the epoch also kept at each return from Go, which saves its load:
    arm64 -0.0%, -0.2%, -0.8%; amd64 +0.6%, +0.2%, +0.3%.
  - Constant array literals made by the native literal helper, as
    object literals and empty arrays are, measured on amd64 alone
    against the boolean-literal branch: +0.6%, +0.3%, +1.5%.
- **EarleyBoyer's calls and closures** (2026-11-20 and 21, Go 1.27.3,
  JIT off, Earley alone unless said, two placements, M5 Max arm64 and
  i9-9900K Windows amd64), each removed for a gain under a per cent, at
  one placement, or not on both machines:
  - Frameless `sc_isPair` and `sc_isVector`, global instanceof
    wrappers, a leaf kind of their own: EarleyBoyer 0.5% faster at one
    placement; the wrappers are 10,000 calls a Boyer run.
  - The frame's open-cell count tested directly at a return:
    EarleyBoyer +1.8%, then -0.7% in a confirmation; Windows +0.6%.
  - The pinned window's and caches' bases saved in the native record,
    four loads less a framed call: EarleyBoyer -1.8% on arm64, +0.8% on
    Windows.
  - Fixed-size allocations for batches of up to 16 cells: 0.6% on arm64
    at one placement.
  - Two adjacent captured-variable reads fused: +0.1%; with the capture
    list's pointer shared by both, +0.7%.
  - Entry stores omitted for locals assigned before a closure captures
    them: +0.2% on arm64.
  - Immutable captures counted once per template rather than at each
    closure: 0.4% on arm64, 0.5% on Windows, at one placement.
  - Immutable captures read straight from their closed cells, an
    operation of their own: 1.9% slower at both placements.
