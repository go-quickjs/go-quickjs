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
size has not been measured again on the register tiers. Figures against
C QuickJS marked v0.39.0 are from the comparison at `3ecb8200` on a Ryzen
5 3600 (benchmarks.md).

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

- **Slower than the old tiers.** At `388324de`, over four placements
  against the base before it, two micro-benchmarks are more than 3% slower
  at every placement: `regexp_split` +8.4% and `forward_construct` +16.9%,
  a constructor that hands its arguments to `this.initialize.apply(this,
  arguments)`. `regexp_split` is not profiled yet; the span measured holds
  several changes, so the first step is to find which.
  `forward_construct`'s forwarder runs without a frame in native handlers
  since v0.39.0, 0.23 times QuickJS's time where it took 0.54, and 0.32 at
  `2f05d3fc`: builds of the revisions between put most of the step at the
  renaming of package vm's files (`c985f5b0`, `be71fe74`), which changed
  no code it runs, and the layout lock's positions at `c2dae008` put
  that code back at `3ecb8200`'s offsets, where it takes 0.25. The rest,
  with 4 to 8% of the `carried_` and `escape_` tests', came with the
  native initializer's remembered layouts (`8505335a`), not profiled on
  Zen 2 yet. At `7f8205ec` it takes 0.20, 75.3 ns against 84.8 at
  `3ecb8200` and 87.9 at `b5df63ea`, and the `carried_` and `escape_`
  tests are faster than they were there (`carried_short` 112 ns against
  134), after the allocation round (`af68d128`..`dc7ddd31`). It has not
  been compared with the old tiers again. The JIT's slower rows are under
  [The register JIT's remaining gaps](#the-register-jits-remaining-gaps).
- **Raw int32 and float64 values outside asm.js.** A validated asm.js
  module runs as typed code, its registers raw int32 and float64 values
  for the whole function (register-interpreter.md, "Typed code"); zlib
  took 1,556 ms at v0.39.0 where it took 7,605. Other index-heavy code
  still pays a float conversion at every element access and every `| 0`
  (register-interpreter.md, "Known limits and open work"). A local that
  only ever holds an int32 needs mapped `arguments`, direct eval and reads
  before its first store accounted for.
- **asm.js modules' limits.** Only the first closure of a module's
  template is linked to typed code, and a module instantiated a second
  time in a runtime runs as ordinary code; a call through a function table
  of more than four functions is made by index since `6fbcf73b`. The JIT
  inlines a typed callee nothing inside of which exits (`asm_call_leaf` 9.4
  ns at v0.39.0, 4.5 now, against 4.3 for the same code as ordinary
  JavaScript); `asm_fib` took 23% longer with the JIT than as ordinary
  JavaScript until ordinary native code called a module's typed code
  natively (`d14b7cea`: 4.69 ns against 4.64 for the same code untyped, in
  that commit's measure).
- **The leaf evaluators** (`pureCall`, `leafCall`), which evaluate register
  code (regcomp's pure and leaf bodies) for the Go table and wherever no
  native handler takes the call. A recursion of pure calls deeper than 32
  (`pureCallDepth`) is given up on whole and, after a few such, made with
  frames, where switching at the limit alone needs the pure path to pass
  on an error such as a stack overflow; and `pureCallAt` could switch on a
  case byte for each function. Queens, 1.31 times QuickJS's time on the
  old tiers (2026-10-15), spent 47% of it in two tiny methods that read and
  write elements, which pure bodies with element reads and several stores
  would serve (*unverified*).
- **Growing an array by index.** `array_prop_create` took 2.51 times
  QuickJS's time at `6a53e072`, the prototypes checked at each append and
  Go's slice growth (*unverified*). Arrays could start at a capacity of 8
  at their first growth, and `obj[i] = v` on a plain object wants a keyed
  store cache of several entries by site (`setIndexed` was 32% of
  `prop_create`'s profile, 2026-10-12).
- **Typed arrays have no shapes** (`shapeClass`), so no cache remembers
  what a name finds on one: a method read such as `t.forEach` takes the
  slow path in every tier. Their names but the canonical numeric strings
  are an ordinary object's; a shape of their own, whose cache leaves those
  strings out, would serve the method reads (2026-10-19, *unverified*).
- **A view that tracks a resizable buffer's length** keeps no storage and
  end, as a view of fixed length does (`9442ec8`), so its elements take the
  slow path.
- **The `in` operator** caches neither symbol keys nor canonical index
  strings such as `"0"` (2026-10-21, *unverified*).
- **Object helpers past the inliner.** `noneHas` and the allocation
  helpers no longer inline, each calling `rootFor` or `findOwn`, since the
  32-byte objects (`5c26cde5`).
- **A for-of cursor without an Object header.** The cursor is 88 bytes
  (`d9173a40`, `323f9031`), in the size class of 96; as a kind of Value of
  its own it would shed the header's 32.
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
- **A closure's realm in its template.** With the realm shared through
  `closureCode` as the module environment is, a closure was 40 bytes, not
  48, and Earley allocated 4.6% fewer bytes. But `callObject`'s and
  `callDirect`'s realm checks took a load more: each grew 32 bytes, which
  moved `pureElemOp` off its 64-byte alignment -- `array_pop` 5.5% slower
  at every placement, level with the code after `callObject` padded back
  to its offsets -- and wrapped O's pad, at its minimum in the builds
  with the JIT, a page on (2026-11-21, Ryzen 5 3600). The realm stays the
  closure's, read in one load as before.
- **EarleyBoyer's Earley** takes 1.61 ms against Node 26.8.1 jitless's
  1.06 ms on M5 Max arm64, where Boyer takes 19.8 ms against 20.9
  (2026-11-21, Go 1.27.3, JIT off, both placements): the suite's score is
  16% below Node's for Earley alone. 74% of Earley's bytes are closures
  and their cells. Its `sc_list` reads `arguments` through a local alias,
  which no longer makes the object; V8's interpreter still makes it
  (`CreateMappedArguments`), so what is left is not a V8 interpreter
  optimization missing here. These figures are of the series with the
  realm in the template too (above). `closure_many_locals` was 5.9% slower
  on arm64 at both placements with that 40-byte closure, a cause not
  found: restoring the native handlers' addresses, or the one-capture
  object's size, did not remove it.
- **Implicit derived constructors** took 1.21 times QuickJS's time on the
  old tiers (356 against 295 ns, 2026-10-26); their closures are 224 to 288
  bytes since `59382e63`, not timed again.
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
  - A global `exec` takes 1.36 times QuickJS's time at `6a53e072` (a sticky
    one level): the match's array and its strings are one allocation
    (`45a402e2`), and the matcher is about 30% of the rest.
    `regexp_replace_dollar`, 1.35 times, is not profiled.
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
  - Go's memory profiler at its default rate unwinds the interpreter's
    stack at each sample, 3.6% of `regexp_ascii`'s samples on the old
    tiers; v8bench leaves `runtime.MemProfileRate` as it is.
- **String's methods.** padStart and padEnd with a filler past ASCII, and a
  repeat of a string that ends in a high surrogate and begins with a low
  one, take the old builder; the annex B HTML methods build Go strings and
  scan them again. Behind QuickJS on the old tiers (`3141a88`):
  `String.fromCharCode` of two arguments (113 to 119 ns against 83),
  startsWith and endsWith past ASCII (110 to 127 against 79 to 84), substr
  (124 against 105).
- **JSON** takes 0.41 of its former time and 1.10 times Node's (2026-11-06).
  Left: a batch of objects, as strings have, in which a dead object would
  keep what the others hold alive; a String's flag for text known to need
  no escape, for which it has no byte free; short strings' values shared by
  their text, as V8 internalizes them, which a document of different ones
  pays for; 17-digit doubles written, which are strconv's.
- **WeakMap.** `weak_map_delete` took 2.09 times QuickJS's time at
  `6a53e072`: Go's `weak.Make` searches a span's list of specials twice
  for each new key and takes the heap's lock the sweeper takes. A key's
  weak reference is to a holder, taken from a batch made ahead
  (`5589f873`, `1274da7d`); a record for each key could go only with
  ephemerons from Go's collector.
- **Typed arrays' `indexOf` and `includes`** poll for no interrupt
  (2026-10-21, *unverified*).
- **Date.** `date_parse` makes 12 allocations, which want go-intl's
  formatting to append (2026-10-12).
- **BigInt.** The FFT's scratch memory is not counted against the memory
  limit, and linear operations (addition, shifts, bitwise operations) are
  not interruptible. ADX and MULX kernels on amd64 are parked: math/big
  multiplies 64 words in 1,756 ns against our 3,048 (2026-10-20).
  `pidigits` spent 13% in the free list and 9% in `s += y` (2026-10-20,
  *unverified*).
- **`mayTie`** counts digits by a loop rather than `FormatUint`
  (2026-10-04, *unverified*).

### Start-up, compiling and programs run again

- **The compiler's speed** (Octane's CodeLoad, 129.8 -> 102.8 ms once
  the stack bytecode went, 2026-11-17). `Token.IsPunct`'s chains
  and the name maps are each about 3% of a compile, the final tables are
  allocated one by one, and a string `Op` in a binary node is 16 bytes
  where a kind would be one, at most of the compiler's sites
  (2026-11-06). A RegExp literal is validated by building and dropping its
  parse tree.
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
the stack JIT lowered; their counts are from then.

- **Shapes known across a loop's header.** A loop header starts knowing no
  shapes -- its entry for on-stack replacement knows nothing -- so a loop's
  first read of an object checks its shape at each turn. Moving the check
  before the loop, its exit the loop's entry, would check it once
  (2026-10-23, *unverified*).
- **A property read is three dependent loads**: a value with a reference is
  carried as the address of the cell it came from, its reference read from
  it, then the object's values, then the value, where V8 loads an in-object
  field once (`3141a88`). An object's values sit behind a pointer since the
  32-byte objects (`fece0478`..`6c58742a`), so in-object fields would be a
  change of the object model.
- **`push` in a loop.** Native `push` checks Array.prototype's and
  Object.prototype's layouts on every call, counts the call and stores to
  its keep cell, about 100 instructions an iteration, and growing the array
  leaves for Go (`pureElemOp`, its time in clearing large blocks, the heap
  profile's sampling and copy barriers): `array_push_large`, 8,192 pushes
  onto `[]`, took 20.5 ns a push with the JIT against Node's 4.1 at
  `fb40baf`. V8 hoists the prototype checks out of the loop
  (*unverified*).
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
  vectors); and `arguments` objects, never tried. A closure whose body
  calls another closure it captures needs its calls' feedback, which a
  template's function has only once it runs natively on its own; a closure
  made in a loop's turn over a per-iteration binding still takes the
  binding's cells each turn (`closure_make`'s remaining allocations).
  A loop whose calls leave it early makes its objects at each exit:
  `carried_exit_100`, `carried_exit_20` and `carried_every_2` are 3.1 to
  3.3% slower than without the partial analysis (against v0.35.0).
- **Inlining.** Callees with loops are inlined only when small
  (`maxInlineLoop`, 24 instructions) or when the loop calls a closure the
  caller gives: inlining Crypto's `am3` made its callers' code 60% larger
  and Crypto 25% slower. A rule that inlines a loop only where the call's
  cost is not spread over its turns is wanted, and a loop of few turns
  could be inlined given turns-a-call feedback, which a function does not
  have. Callbacks with default or destructured parameters are not inlined,
  nor two closures passed in one call analysed, for root-level closures
  too; a callee with `define_index` or a spread compiles for native callers
  but is never inlined (2026-10-23, *unverified*).
- **Strict equality with a literal** is the operand's word, length and
  bytes against immediates for an ASCII literal of up to 64 bytes
  (`100eb7f3`). A longer or non-ASCII literal, and loose equality, take the
  generic `eqtagged`, about 450 bytes a site, whose string comparison is
  the main path on string-heavy code (85 to 90% of executions on a render
  and on PdfJS). What the literal path saved in code size was not
  measured.
- **Calls of Go.**
  - A leaf's Go side is about a hundred instructions besides the leaf's
    own: the routine and the ABI0 wrapper (27), `jitLeafGo`'s frame and its
    spills around the check's and the function's closures, the operands'
    slice's checks, the keep cell's barrier test. V8's fast API calls call
    a function of a typed signature; a native leaf's check and function
    fused, or forms of typed operands, would take most of it.
  - Calls that save every register (`goSaving`) -- a back edge's poll, a
    pool's refill, a string's absent index -- go through `callGo`.
  - More leaves native code could make itself by the kinds it has
    (`ssa.LeafKind`): `charAt` and `at` past Latin-1 (a string made),
    `String.fromCodePoint`, a Map's `get` of a key whose kind the code
    knows; and `Math.sin` and `Math.cos` on amd64, about 1% of Box2D,
    115,000 calls of Go in three runs at 18 ns (2026-10-20).
  - `call`'s Go-made path is about 180 bytes a site, nearly all `goArg` for
    each operand: Go reading the operands from the call's records rather
    than `OffGoArgs` would save about 30 bytes a site, a reordering of
    `call` and of what `jitGoCall` reads across mir and vm, to be sized
    against `call`'s inline bytes first.
  - With a memory limit set, every operation Go makes for native code
    takes the framed path, the frameless paths not charging the meter
    (2026-10-23, *unverified*).
  - Frames that the debugger and stack traces see from `jitGoCallNested`
    show captured locals as they were while cells hold their values;
    entering code with cells from Go in mid-function wants a test of its
    own (2026-10-18, *unverified*).
- **Register allocation and the code**, each *unverified* since it was
  found:
  - A native call's state could be found from its return address, as V8's
    safepoint tables are, rather than stored by two instructions; each
    argument makes an xmm15 round trip (2026-10-22).
  - Operands only the call reads are kept (`x.next` in `tail`), unless
    their source may be a keep cell the same run writes (2026-10-22).
  - `this` carried as a phi takes the compare-number-words path when its
    pointer word is read (2026-10-22).
  - Intervals touching at one instruction leave loop-carried moves
    (try_catch's sum, array_for_of's, a counter after its `addsd`):
    lifetime holes, or a result reusing a dying operand's register, which
    every emitter would have to allow (2026-10-23).
  - Exit stubs are 10 bytes, where an index would do in place of the
    descriptor's address; consecutive guards sharing one frame state could
    share one stub; a call-free loop's exit has 3 extra instructions, which
    Go could replace by reading the budget register from the saved
    registers (2026-10-27).
  - The context's fields past its first 128 bytes take a 4-byte
    displacement each: spills at offset 10,816, 749,000 accesses, all
    have one, and `[reg+0]` carries a displacement byte, about 220,000
    operands (2026-10-23). Displacements were 22% of the bytes of
    Typescript's code, and exit stubs about 5%.
  - Compare-and-branch fusion requires that nothing be emitted after the
    compare, which leaves about 80 of Octane's sites unfused where only
    captured bindings' loads follow; a truthiness that redundancy
    elimination shares between branches stays a boolean (2026-10-22).
  - Unboxed loops still box a value that feeds the loop's exit edge at
    every turn; forEach's and a typed forEach's captured `sum` is stored to
    its cell, and `some`'s `===` boxes (2026-10-27).
  - Constants are stored into spilled phis (25,000 in Box2D), and cold code
    is about 23% of all code (2026-10-14).
  - For the op cache: entry blocks and slow paths out of the hot region,
    `MovAddr`'s 10-byte addresses (45,000) pooled, stubs' conditional jumps
    that are never short (2026-10-16).
  - Clearing a keep cell from its highest used (KeepHigh) took Typescript
    1.4% faster and Towers 11 to 15% slower by layout; parked (2026-10-20).
  - `bigint64_arith`'s loop counters as unboxed float64 phis run about 7%
    slower than tagged, unexplained (2026-10-20).
  - `aliases.may` (`ssa/origin.go`) can store a "false" computed while a
    phi round around a loop is in progress; never seen to change an answer
    (2026-10-11).
  - The review of the first pipeline asked for: no stale `*byte` pointers to
    unmapped code (store `uintptr`), no hand-kept assembly offsets
    (`go_asm.h`), `runtime.AddCleanup`, `Validate` refusing Opaque, String
    and Uninitialized literals, and layout asserts at compile time.

### The JIT's compile cost and memory

- **Compiles cost more than a short program gains.** A fresh runtime's
  first server render took 38.2 ms with the stack JIT compiling in the
  background (35.5 with its snapshot) against the interpreter's 21.9
  (`86cb59be`..`fd6c4f88`); the register JIT took 25.4% off the first
  render (`388324de`), which is still to be compared with the interpreter.
  Measured on the stack JIT (2026-10-30), compile time follows the code
  emitted, about 120 bytes of machine code a stack instruction (156 in
  Typescript, 168 in PdfJS; 352 KB for one
  2,982-byte switch whose few arms run), at about 5 us an instruction --
  1 to build, 1.5 to optimize, 1 to emit -- and grows as size^0.82; no
  phase is superlinear. Three ops were about half of the code: on the
  render `call` 28.7%, `eqtagged` 20.4% and `keep` 10.5%, on Typescript
  22.8%, 12.5% and 13.5%; on Box2D the property ops lead. A call's inline
  bytes are per site by nature (see Rejected); emitting less is what is
  left.
- **The JIT behind the interpreter in a cold run.** In Octane's fixed work,
  one run of each benchmark, the JIT took longer than the interpreter at
  v0.39.0 on Richards (62%), DeltaBlue (44%), PdfJS (34%), RayTrace (27%),
  RegExp (14%), Typescript (13%) and CodeLoad (8%), Box2D level. Most of
  it is the time compiles take on the runtime's own goroutine (2026-11-17,
  `31c7f1b9`, a Ryzen 9 9950X3D, one run): PdfJS ran 60 ms behind and
  compiled for 66, Typescript 200 behind and compiled for 373, Box2D 10
  and 23, Richards 1.5 and 3.2; without it each is faster than the
  interpreter, RegExp level, and by five runs the JIT is ahead on all but
  DeltaBlue. CodeLoad compiles nothing. A compile costs 0.25 to 0.5 ms,
  its SSA build 36% of it, the encoding 20% and the optimizer 18%.
  Compiling a callee on its own budget rather than at its native caller's
  call (Are We Fast Yet +15%) was dropped; the tier-up budgets wait for a
  baseline compiler's experiment. Compiled in the background (as `qjs
  --jit` compiles), the compiles leave the runtime's goroutine: Octane's
  fixed work takes 0.92 times what it takes compiled in the foreground
  (1,223 against 1,326 ms over four placements, 2026-11-18), where it had
  taken 1.49 times, zlib 3.3 times; Are We Fast Yet's takes 1.004 times,
  DeltaBlue 1.03 (1.13 before). RegExp's share was its exec arrays: the
  interpreter reuses them (`regexpreuse.go`), which a runtime with the
  JIT did not until native code's calls of `exec` made them again too
  (`e9bf2365`; Octane's RegExp with the JIT allocated 131 MB, now 77).
- **Where a compile's time and memory go.** `allocate` 14% of a compile,
  the build's `plan` 15% (most of it the feedback's answers), the
  assemblers' `Finish` 9%; the
  closures of `compiler.call` and the 65 kinds of cold code are a sixth of
  the objects left (2026-11-06), a change of the code generator's shape.
  The register-group allocator added about 25% to allocation on Typescript
  and 7 to 20% to `mir.CompileIn` (2026-10-23); encoding is 3 to 10% above
  its earlier cost from loop alignment (*unverified*). Optimize's last round
  finds nothing in most compiles (3 to 4% of their time), and skipping it
  soundly needs care with `unboxPhis`; worklists would replace the rounds.
- **A compile's working memory** is outside every runtime's budget, in a
  pool the process shares (`d4d57347`). The budget weighs only what a
  compile will hold, 512 bytes an instruction and 64 a slot
  (`jitCompileBytes`, internal/jit/README.md); the working memory is
  linear in the function's size (`TestJITCompileLinear`), and was about
  7.5 KB an instruction on the stack JIT (17 KB for Box2D's
  `initPhysics`), not measured on the register JIT.
- **The slot IR's size.** An `ir.Instruction` is 136 bytes, 83% of a
  program's bytes, with a 16-byte StateMap for each instruction: packing the
  operands' slots to `int32` would save about 18%, and literals as indexes
  into a constant table about half (*unverified*).
- **The code arena's free space** between live functions is neither counted
  nor reused until its 64 KB chunk empties: 460 to 520 KB mapped for 323 KB
  live under a 1 MB limit (2026-10-12, *unverified*). A placement's code is
  limited to 1 MiB by arm64's conditional branches' range.
- **What code in use was compiled for stays alive.** Code unused is flushed
  with what it was compiled for (`bc35a9f`), but code in use holds its call
  targets' closures (`nativeCalls`), its holders and the closures it
  inlined strongly, where V8 holds them weakly and deoptimizes code whose
  objects die. Go gives no hook to invalidate code before a dead object's
  address is reused, so a weak target needs code that checks it is alive
  before it is entered. Octane's Typescript kept about 110 MB more than the
  interpreter while it ran (stack JIT).
- **The default room for code.** More room (`WithJITMemoryLimit`) must
  never be slower, and before the flush was: with 16 MB Typescript took 18%
  longer than with 8. The runtime's default stays 8 MiB until that is
  measured again; v8bench runs with 32 MiB. A stretch of native code is
  judged by 20 instructions of work against an exit (`jitSSAMinWork`), far
  less than an exit costs.
- **The profit model is parked.** `a21df702`, never merged, has a
  calibration (`TestJITCalibration`, `QUICKJS_JIT_CALIBRATE=1`) and charges
  a callee's exits to the callee; Splay, Json and RegExp were slower with
  it. Its kernels no longer measure the endings they were written for
  (most calls of Go and exits they timed are native now), so a return to
  it starts by redesigning them.
- **A flush's back-off**, the wait doubled for code flushed before, took
  Typescript's compiles from 1,807 to 995 in five runs but its live heap
  from 262 to 356 MB (2026-10-23); since the flush's clock leaves the
  compiles' time out (`3822dbce`), what it would gain is not measured.

### Measurement and tooling

- **What the layout lock leaves free.** The lock as first made pinned
  `db306c0e`'s layout; over four placements the locked `ce98c3cf` against
  `db306c0e` measured 0.990 on the micro-benchmarks' geometric mean, 0.999
  with the JIT, none 3% slower at every placement. Not pinned: the generic
  groups no lever reaches (`setRoot`..`firstRoot`, `newPolyObject`,
  `appendGrown`, the constructors' `chargeChild`), closures against each
  other, read-only data, and code between two of its positions.
- **What the placements do not vary.** Read-only data: two builds of the
  same code, their type descriptors 32 bytes apart, took 211 and 192 ms on
  `string_template` (2026-11-04). Where package vm sits against the
  packages linked before it: a change in `internal/jit/mir` alone moved vm
  10,464 bytes and made RegExp 2.4% slower, the parent padded the same as
  slow (the lock's regexp pad now holds the matcher). ASLR. The JIT's Go
  side, and distances inside a JIT function: `bigint64_search` was 25%
  slower at six of eight offsets for one byte more inside its function
  (2026-11-08).
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
  is never native.
- **Flaky tests.** `TestAsyncWorkTimer` under the full suite's load;
  `TestJITNoHeapWalks` once in four runs under `QJS_JIT_STRESS`, the meter
  counting the process's allocations, which another runtime's cleanups may
  make meanwhile; `TestWorkspaceCompileAllocations` under `-race`, 64.2
  against its limit of 62 (2026-10-27). Each *unverified* since.
- **The marking rule's checks.** A test scanning the source for a raw store
  to a pointer field outside the `storeRef` helpers; `storeRef`'s bulk
  forms, clearing n values or copying n from GoArgs, where a call's setup
  makes two or three calls of the routine (2026-10-19).
- **arm64's timing.** The JIT runs on darwin/arm64 and linux/arm64; broad
  timing of native execution there is open.

### Semantics found on the way

- Stack traces unlike Node's: a receiver's own `constructor` property,
  which V8 passes over; an arrow called as a method, which V8 names by its
  receiver; an object made by `new P` after `P.prototype` was replaced.
- `new.target.prototype` is read at `new` (`constructWithTarget`), where
  the specification and V8 read it in `super()`, as a Proxy given to
  `Reflect.construct` as its new.target shows.
- `a[k] op= y` with an object key converts the key once, as C QuickJS
  does, where Node converts it twice; no test pins it.
- RegExp's legacy statics are updated in the current realm, where the
  standard has them updated only when the RegExp's realm is it (test262
  passes).
- A breakpoint resolves to one statement, so of the copies of a finally
  clause the compiler makes at each break or continue, only one stops at
  it (code for a debugger keeps one copy on the normal path).
- An optional chain's call and a spread call in tail position are not tail
  calls, as the standard has them.
- A private field added to an object that is not extensible is refused, as
  the proposal has it; Node v26 adds it.

## The register JIT's remaining gaps

The JIT compiles register code alone since `388324de`. The stack JIT's
tests were ported to it, and some of them hold only for what the stack JIT
did and the register JIT does not do yet: each is skipped by name with a
`t.Skip` that says what falls short and points here (`grep -rn "register
JIT gap:" internal/vm`). Each gap is closed by giving the register path
the mechanism the test describes -- the stack JIT had it, and its test's
comment says what it was -- never by loosening the test, and the skip goes
with the fix. None of them is a wrong answer: every skipped test's answers
are the interpreter's; what differs is how much runs natively, how often
code is compiled, and the shape of the code.

**Native calls and entries.** Code that should run natively runs in Go or
in the interpreter.

- `TestJITAccessorsNative`: a small getter and setter read in a loop are to
  be inlined, and a built-in's getter called by Go from native code, which
  goes on after it. `selves(ps, 4000)` does not run natively, and `sums`
  makes some 2,000 calls of Go.
- `TestJITPropertyReleaseAndGC`: after a getter's `a.push`, the run finishes
  in the interpreter rather than going on natively after the call of Go, so
  that the later callbacks run with no native run under way.
- `TestJITArgumentsInRecursion`: a recursion that reads its arguments, as
  EarleyBoyer's `sc_list` does, is to read `arguments[i]` of an arguments
  object that aliases nothing by a call of Go from native code, and to
  leave code that is due to be compiled again only where the compile can
  be made then. It finishes in the interpreter 30 times and leaves for Go
  85 times.
- `TestJITArrowSharedBindingCalled`: arrows a derived constructor made
  share its this binding, and a native call of one is to give it the
  binding's value, checking at each call that it is bound (mir's
  `arrowBound`). `callAll` makes 548 calls of Go a round, not none.
- `TestJITDerivedNativeConstructArrows`: a derived constructor whose arrows
  all come after its `super()` is to be constructed natively, Go binding the
  frame's this as that `super()` bound it (`jitBindThis`). `B`'s and `C`'s
  code is never called natively.
- `TestJITCatchCallees`: a callee with exception handlers is to be called
  natively, a frame Go makes of it having the handlers in force there.
  `parse` never is.
- `TestJITSuperReferences`: a super reference's read or write is to be
  Go's from native code on the frame Go entered the code with, a native
  call writing its function object to its callee's context for the home
  object. `B.prototype.w` leaves native code 145 times.
- `TestJITSpreadFrameless`: spreads and rests of plain objects and dense
  arrays, and computed keys that are strings, are to be made without a
  frame. A spread call of a frameless callee calls Go with a frame 3
  times.
- `TestJITSSAStringAddCallsGo`: a `+` with a string is to be Go's from
  native code (`ssa.GoAdd`), which stays native, as V8's code calls its
  StringAdd. `tree`, a recursion like Splay's `GeneratePayloadTree`, is
  never called natively.

**Compiling again and learning.** The code is compiled again where it
should not be, or not where it should, or does not learn what it met.

- `TestJITKeyedWrites`: an element's write to an object native code does not
  store to is to be Go's from native code (`ssa.GoKeyedWrite`). `fill` leaves
  its code 4 times.

**Exits.**

- `TestJITBigTypedArrays`: loops over BigInt typed arrays are to run natively
  and leave nowhere. Over arrays past 2 GiB they leave native code once and
  make 3 calls of Go.
- `TestJITExceptionHandlers`: a function with try statements, compiled with
  its handlers known at each instruction, is to be entered again once
  compiled again. `operation({}, 100)` runs in the interpreter.

**The code's shape.**

- `TestJITGivenClosures`: a closure a function gives to a method calling it
  in a loop is to be inlined with the method within the size rule.
  `scaledBy` inlines its call past the rule and makes the closure.
- `TestJITSSAContextsGrow`: native calls that run out of contexts are to
  have more of them at once, up to as many as the VM's frames may take.
  A deep native recursion gets 256 contexts, not 8,192.

**Tests ported, not skipped.** Of the tests that read the stack JIT's
internals, four were ported to the register JIT's in the removal and run:
`TestJITClosureCacheRelease`, `TestJITPropertyCallbackRelease`,
`TestJITProgramRerunsKeepLayouts` and `TestJITStringBoundaries`. The fifth,
`TestJITPropertyReleaseAndGC`, is a gap (above). `TestJITLoweringsOnlyInRoom`
runs again since settling takes young code once the JIT is past its budget.

**Carried from the stack JIT, unverified on the register JIT.** These were
open against the stack JIT and concern mechanisms the register JIT took
over from it; each is *unverified on the register JIT* until the next
round measures it there.

- **Element kinds are learned only at exits.** V8's keyed loads and stores
  record the maps and elements kinds they meet in the interpreter, so
  optimized code is compiled for a typed array from its first compile. Here
  an element read's kind is learned at native code's own exits
  (`jit_elem.go`): a site the first compile took for an array's meets
  typed arrays at exits, and the code runs on, learning, until a budget
  every cache change restarts is spent -- Gameboy's `executeIteration` read
  `TICKTable[op]` as an array's for 3,076 exits a run. The fix is V8's: the
  interpreter's element reads note the kinds they meet per site.
- **Compiles in flight are outside the budget.** What compiles in the
  background hold -- up to four workspaces, their code and snapshots a
  runtime -- is the process's, outside `WithJITMemoryLimit`, and a runtime
  that does not run keeps its finished ones until it runs or closes.
- **A native call's cost.** Inlining's cut of rarely-called callees made
  Richards 1.2% slower at every placement: `runTask`'s `packetPending`,
  called 0.129 times a call over a run, becomes a native call 2.5 million
  times a run, which costs more here than a call in V8's code does.
- **Frequencies are the warm-up's.** Code compiled again for what it
  learned uses the counts its function had before it ran natively, and a
  call never made while it warmed is unknown, not cold.
- **Calls inside an inlined callee** that need Go need the root frame, and
  an inlined level has no frame state to give Go.
- **Branches never run before the compile deoptimize one at a time**
  (Gameboy's `memoryReadJumpCompile` 10 times, Typescript's `innerScan`
  11); borrowing the shape from the function's other reads of `this` cut
  Gameboy's code 33% and grew Typescript's 6 to 13%, and learning more at
  each deoptimization would depart from V8.
- **Native generators.** `return()` and `throw()` are Go's; `yield*`, a
  spread or `Array.from` of a generator, and generators with captured
  locals are not native steps; a `next()` result is an object from a pool.
- **A number's method sites** are learned at a deoptimization each; code
  compiled before `Number.prototype` or `String.prototype` changed leaves
  native code at the read until compiled again; a sloppy method on a
  primitive exits at each call, Go alone wrapping the receiver.
- **Native `push`** leaves native code where the array must grow (8 times
  in 1,000 pushes), where `a[i] = v` grows it by a call of Go and goes on;
  typed arrays' `map` and `filter` stay Go's.
- **`String.fromCharCode`** past Latin-1 is a call of Go only in a
  function's own code, and exits in inlined code (PDF.js's `Lexer_getObj`
  inlining `Stream_lookChar`).
- **`async_await`** took 5 to 10% longer with the JIT than without it: two
  million resumptions not entered, each cheap, and the await's queueing
  dearer with the JIT on, not explained.
- **Octane's Typescript compiles about four times a function** (2,400
  compiles of 607 functions in three runs), a third of them a function
  whose code was dropped for room or flushed, compiled again into code of
  the same size.
- **`global_func_call`** took 1.11 to 1.15 times its earlier time with the
  JIT on the same native code but for displacements and constants, the
  extra samples on the dependent loads of the global's shape, its keys and
  the lexical names; no pad, seed or collector setting moved it.
- **test262 under JIT stress** ran 10 to 50% slower since `45d15dc`.

**Left by the removal's timing.** Against the base before the removal, over
four placements, every suite's total is level or faster; these were slower
by more than 3% at every placement:

- The register JIT against the stack JIT: Octane's RayTrace +7.6%; Are We
  Fast Yet's Json +8.2%, Permute +7.0% and Queens +6.3% (Havlak +3.2% at two
  placements of four); 27 micro-benchmarks, among them generators and async
  functions (+11 to 29%), `array_fill_down`
  (+137%), `math_clz32` (+17%), classes and prototypes (+10 to 22%),
  `forward_poly` and `forward_loop` (+9%) and `json_parse_new_layout`
  (+16%). The interpreter's two are listed above, under the register
  interpreter.

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
