# Benchmarks

On an Apple M5 Max with Go 1.27.0, taking the median of five fresh-process
runs at go-quickjs `6c3dd16`, with 30 seconds between runs:

```
BenchmarkEvalArithmetic-18  1.17µs/op   3938 B/op    20 allocs/op
BenchmarkFibonacci-18        425µs/op     16 B/op     1 allocs/op
BenchmarkPropertyAccess-18  1.45µs/op   4916 B/op    23 allocs/op
BenchmarkCallGoFunction-18  1.49µs/op   5556 B/op    31 allocs/op
```

`fib(20)` costs one allocation because the interpreter loop allocates nothing
per call; the other benchmarks include parsing and compiling their source each
iteration.

`go test -bench . -benchmem` also runs compile-once interpreter workloads,
plus compilation, runtime construction, and a recursive factorial workload.
That complete set is:

```
BenchmarkLoopArithmetic-18         57.8µs/op        1 B/op     0 allocs/op
BenchmarkDateUTC-18                 370µs/op        1 B/op     0 allocs/op
BenchmarkLoopPropertyAccess-18      206µs/op        0 B/op     0 allocs/op
BenchmarkLoopArrayIndex-18         11.1µs/op        0 B/op     0 allocs/op
BenchmarkLoopFunctionCall-18        181µs/op        0 B/op     0 allocs/op
BenchmarkLoopMethodCall-18          270µs/op        0 B/op     0 allocs/op
BenchmarkAllocObjects-18           93.2µs/op   352 KB/op  2000 allocs/op
BenchmarkAllocArrays-18            67.9µs/op   320 KB/op  2000 allocs/op
BenchmarkStringConcat-18           42.0µs/op   129 KB/op  1999 allocs/op
BenchmarkClosureCreateAndCall-18    366µs/op  1.76 MB/op  5001 allocs/op
BenchmarkArrayCallbacks-18         45.8µs/op  36.7 KB/op    24 allocs/op
BenchmarkMapOperations-18           163µs/op   311 KB/op    25 allocs/op
BenchmarkRegExpExec-18              299µs/op   448 KB/op  4000 allocs/op
BenchmarkThrowCatch-18              285µs/op   768 KB/op  6000 allocs/op
BenchmarkJSONRoundTrip-18           490µs/op   748 KB/op  9500 allocs/op
BenchmarkFactorial-18              21.0ms/op   377 KB/op  1294 allocs/op
BenchmarkCompileScript-18          20.6µs/op  40.2 KB/op   301 allocs/op
BenchmarkNewRuntime-18             58.7µs/op   267 KB/op  1222 allocs/op
BenchmarkNewRuntimeSmallStack-18   61.1µs/op   265 KB/op  1222 allocs/op
```

Each compile-once benchmark runs its workload many times: the main loop
benchmarks use 10,000 iterations, the object and array benchmarks allocate
2,000 values, and the callback, collection, regexp, exception and JSON
benchmarks exercise their operation hundreds or thousands of times.
`CompileScript` compiles one program per operation, while the runtime
benchmarks construct and close one runtime per operation.
`Factorial` constructs one runtime and evaluates 100,000 calls to
`factorial(10)` per operation, totaling one million recursive calls
(21.0 ns/call).

## V8 version 7 benchmark suite

`go run ./internal/cmd/v8bench -dir /tmp/v8-v7 -fetch -mode score` runs the
suite on go-quickjs; see that command for its other modes. Run from
`internal/cmd/v8bench/goja`, `go run . -dir /tmp/v8-v7 -mode score` runs it
the same way on goja. That runner is a Go module of its own, so go-quickjs
does not depend on goja. `go run ./internal/cmd/v8bench/external -engine qjs
-cmd /path/to/qjs -dir /tmp/v8-v7 -mode score` runs it on QuickJS itself, or
with `-engine node` on Node. `-arg` passes the program an argument, once for
each: `-engine node -arg --jitless` runs Node without its JIT compilers.

For a comparison with another pure-Go JavaScript engine, the Mozilla
[AreWeFastYet V8 version 7 suite][v8-v7] was run against go-quickjs and
[goja]. The figures below are the median of three alternating-order,
fresh-process runs on an Apple M5 Max with Go 1.27.0, with 30 seconds between
runs. Higher suite scores are better.

| Workload | go-quickjs | goja | Relative result |
|---|---:|---:|---:|
| Richards | 2,003 | 527 | go-quickjs 3.80x |
| DeltaBlue | 2,420 | 639 | go-quickjs 3.79x |
| Crypto | 3,002 | 332 | go-quickjs 9.04x |
| RayTrace | 4,726 | 793 | go-quickjs 5.96x |
| EarleyBoyer | 5,830 | 1,437 | go-quickjs 4.06x |
| RegExp | 4,083 | 584 | go-quickjs 6.99x |
| Splay | 7,717 | 2,556 | go-quickjs 3.02x |
| NavierStokes | 6,183 | 556 | go-quickjs 11.12x |
| **Composite score** | **4,094** | **752** | **go-quickjs 5.44x** |

The complete fresh-process run includes runtime construction, parsing,
compilation, the suite's warmups and its measured iterations:

| Metric | go-quickjs | goja | Relative result |
|---|---:|---:|---:|
| Wall time | 22.41 s | 51.65 s | go-quickjs 2.30x faster |
| Total allocation | 13.60 GiB | 32.00 GiB | go-quickjs 57.5% less |

The suite warms every workload for at least one second, then measures for at
least another second and continues until it has 32 measured iterations. Both
engines evaluated the same concatenated benchmark sources and validation code.
The measured revisions were go-quickjs `6c3dd16`, goja `793a2a6`, and
AreWeFastYet `0e21608`. These results characterize this older interpreter
workload suite rather than every application; go-quickjs leads all eight of
its workloads in this comparison.

## Compared with C QuickJS

Over the V8 suite as a whole, go-quickjs is ahead of QuickJS: it takes
0.62 times QuickJS's time and has a 19% higher composite score. Single
operations cost about the same on average, 0.99 times QuickJS's time, as
[the micro-benchmarks](#micro-benchmarks) below show, though a few still
take two to over three times as long.

go-quickjs has the same design as QuickJS: a bytecode compiler, a
stack-based interpreter, NaN-boxed values. Running that design in Go costs
speed. The same suite ran on an AMD Ryzen 5 3600 under Windows, with Go 1.27.1,
go-quickjs v0.26.0 (`09518f1`) and QuickJS 2026-06-04. The first two columns are milliseconds for the same
fixed work (the median of three fresh-process runs, lower is better). The
scores are the suite's own (higher is better). Richards and DeltaBlue, which
take only a few milliseconds a run, ran three hundred times in each process,
scaled to three.

| Workload | go-quickjs | QuickJS | QuickJS faster by | go-quickjs score | QuickJS score |
|---|---:|---:|---:|---:|---:|
| Richards | 13.8 ms | 11.1 ms | 1.24x | 767 | 953 |
| DeltaBlue | 24.5 ms | 23.7 ms | 1.03x | 814 | 831 |
| Crypto | 263 ms | 323 ms | 0.81x | 1,372 | 1,104 |
| RayTrace | 134 ms | 143 ms | 0.94x | 1,647 | 1,561 |
| EarleyBoyer | 370 ms | 404 ms | 0.92x | 2,217 | 2,151 |
| RegExp | 281 ms | 863 ms | 0.33x | 1,467 | 434 |
| Splay | 268 ms | 508 ms | 0.53x | 3,321 | 3,317 |
| NavierStokes | 186 ms | 192 ms | 0.97x | 2,731 | 2,398 |
| **Total / composite** | **1,539 ms** | **2,468 ms** | **0.62x** | **1,594** | **1,336** |

The two kinds of figure time different things, which is why they do not
agree on how far ahead go-quickjs is:

- A fixed run times each workload's setup, its runs and its teardown. The
  scored run times only the runs, after a second of warming up.
- The total adds milliseconds, so the longest workloads, RegExp and Splay,
  make most of go-quickjs's lead. The composite score is the geometric mean
  of the workloads' scores, so each counts the same: Richards' gap weighs as
  much as RegExp's lead. By that mean the fixed figures put go-quickjs
  about 27% ahead, and the scores 19%.
- Splay's lead is in its fixed time. That is almost all setup and
  teardown: building a tree of 8,000 nodes and dropping it, which QuickJS
  frees by counting references where Go's collector frees it later. A
  warm run, which is what the score times, is splay-tree operations, calls
  and property reads, and is level with QuickJS's. Giving the collector
  four times the room (`GOGC=400`) leaves Splay's score as it was, so
  collection is not what the score measures.

Figures move by a few percent between builds from code placement alone,
and Richards and DeltaBlue by more, up to a tenth. To reproduce them, run
`go run ./internal/cmd/v8bench -dir /tmp/v8-v7 -mode fixed -n 3` and the
`external` runner with `-engine qjs`.

### Micro-benchmarks

QuickJS's own micro-benchmarks, [`tests/microbench.js`](https://github.com/bellard/quickjs/blob/master/tests/microbench.js),
time one operation each, in nanoseconds. Each test was given the work QuickJS
takes about 150 ms for, both engines ran that same work in fresh processes at
go-quickjs v0.26.0, and the figure is the faster of two runs. The runners'
micro mode does this, from a copy of the file kept in
`internal/v8bench`: run `go run ./internal/cmd/v8bench -mode micro -n 1`
and the `external` runner with `-engine qjs -mode micro -n 1`, twice each.
`sort_bench` is left out: it reports a run's time over its fastest sort, not
the time of an operation. Over the other 71 tests go-quickjs takes 0.99 times
QuickJS's time by the geometric mean: it is slower on 33, faster on 36, and
level on the rest. The table has the largest gaps either way.

| Test | go-quickjs | QuickJS | go-quickjs / QuickJS |
|---|---:|---:|---:|
| `weak_map_set` | 304 ns | 92.4 ns | 3.29x |
| `weak_map_delete` | 595 ns | 248 ns | 2.40x |
| `bigint256_arith` | 291 ns | 126 ns | 2.31x |
| `regexp_utf16` | 545 ns | 256 ns | 2.13x |
| `array_prop_create` | 30.1 ns | 14.9 ns | 2.01x |
| `regexp_ascii` | 473 ns | 254 ns | 1.86x |
| `bigint64_arith` | 67.2 ns | 39.5 ns | 1.70x |
| `global_read` | 9.3 ns | 5.5 ns | 1.68x |
| `float_toFixed` | 269 ns | 163 ns | 1.65x |
| `array_push` | 84.2 ns | 55.0 ns | 1.53x |
| `string_length` | 8.3 ns | 14.1 ns | 0.59x |
| `arguments_read` | 99.0 ns | 171 ns | 0.58x |
| `prop_update` | 6.6 ns | 11.5 ns | 0.58x |
| `string_build2` | 54.8 ns | 95.3 ns | 0.58x |
| `array_update` | 4.8 ns | 8.7 ns | 0.55x |
| `array_for_of` | 19.9 ns | 46.6 ns | 0.43x |
| `string_to_int` | 44.2 ns | 112 ns | 0.39x |
| `string_build2c` | 64.3 ns | 165 ns | 0.39x |
| `array_slice` | 10.0 ns | 27.3 ns | 0.37x |

`regexp_ascii`, `regexp_utf16` and `bigint256_arith` are 9 to 23% slower
than at `8d54f7e`. Their code has changed since only to charge what they
make to the memory limit (`7906852`, `d98a96e`) and, for BigInts, in the
trees' slow path (`fde2d78`). Alternated with a build of `775a65d`, one
test at a time, `4753462` (their code as this release has it) came out 0
to 10% slower: most of the difference is the placement effect the next
paragraph describes.

Some of these tests depend on where the linker places the code more than on
the code, by more than eight placements can average out: those vary where
the code starts, not how far apart its pieces are. Grouping the hot files
(`250e8f6`) left the V8 suite level but made `array_for`, `array_update` and
`float_arith` 33–43% slower and `func_call` 26% slower, at every placement.
`6b12267` changed none of their code, only the size of two of the tree
tier's nodes; averaged over eight placements of
`6b12267`, `array_update` (29.8 ns) and `func_call` (48.5 ns) had come back
only part of the way, and `array_push` and `map_delete` were 7–10% slower. A
change of that size in one of these tests is not by itself a change in the
engine.

The runners also run `microbench_extra.js`, tests of what microbench.js has
none of, generators and async functions, written as its tests are.
go-quickjs runs their loops as trees, suspensions included. Making,
resuming and delegating to a generator cost up to twice what they cost
QuickJS, and a loop in a generator or an async function is a fifth to two
fifths slower (the `_busy` tests); an await costs less, being resumed
without allocating (`3af4f32`):

| Test | go-quickjs | QuickJS | go-quickjs / QuickJS |
|---|---:|---:|---:|
| `generator_delegate` | 452 ns | 226 ns | 2.00x |
| `generator_create` | 766 ns | 396 ns | 1.93x |
| `generator_for_of` | 105 ns | 55.9 ns | 1.89x |
| `generator_busy` | 26.8 ns | 18.9 ns | 1.42x |
| `async_busy` | 26.3 ns | 22.1 ns | 1.19x |
| `generator_next` | 223 ns | 198 ns | 1.12x |
| `async_chain` | 1,887 ns | 2,536 ns | 0.74x |
| `async_call` | 540 ns | 858 ns | 0.63x |
| `async_generator` | 675 ns | 1,983 ns | 0.34x |
| `async_await` | 126 ns | 751 ns | 0.17x |

The BigInt tests were the largest gaps, 9.65 and 5.56 times QuickJS's time
at `608415b`, where every BigInt was an allocation. A BigInt in the int64
range is now held in the value itself (`9746906`), as QuickJS holds one that
fits in a word, and their loops run as trees since the tree tier stopped
leaving a function that makes calls to the interpreter (`3df6232`).
`bigint64_arith`'s sum also outgrows 64 bits halfway through each run, after
which both engines allocate.

The weak collections are the largest gaps now. A WeakMap holds each value on
its key rather than in a table of its own (`2bc1620`), so that a value which
refers to its key does not keep the key alive, as the specification
requires; adding an entry costs about a quarter more for it. Most of the others are the cost of
each step, which the list below explains. The suite as a whole does better than the operations it is
made of because of the places go-quickjs leads: strings and regular
expressions over whole texts, which Go's runtime and the engine's matcher do
well, and Splay's allocation-heavy setup.

What the gap is made of:

- **Dispatching instructions.** QuickJS's interpreter jumps straight from one
  instruction's code to the next, and the C compiler keeps the interpreter's
  state in registers. Go has neither computed jumps nor a way to keep state in
  registers across a switch that large.
  - Every instruction goes through one `switch`. Around it, the interpreter's
    state is stored to memory and loaded back.
  - Go inlines only the smallest functions into a function of that size, so
    the common cases are written out by hand where they are used.

  The interpreter takes about twice QuickJS's time for the same loop: 1.2
  to 3.0 times over six loops of simple instructions (1.9 by their
  geometric mean; v0.26.0 with the tree tier off, `QJS_NOTREE=1`), each
  of its instructions 3.6 to 5.1 ns. Fusing common instruction sequences
  into single instructions (`a[i]`, `a[++i]`, `x & 0xff`,
  `local * local`) reduces how many are dispatched. Nearly
  every function runs instead as trees of Go closures, a node for every few
  instructions with no operand stack between them, and with locals and
  constants read in place (see the [design notes](design.md)). That is what
  closed the gap on Crypto and most of it on NavierStokes, which do little but
  arithmetic, local variables and array elements. A tree's loops run as Go
  loops, rebuilt from its blocks, with the comparison that ends a loop and
  the `i++` that steps it made in place: the tests that are mostly a loop --
  `empty_loop` (0.96), `int_arith` (0.94), `float_arith` (0.76),
  `array_update` -- are level with QuickJS or ahead of it, and the six
  loops above take 0.39 to 1.15 times QuickJS's time as trees. A loop whose body
  would need its branches chosen by a structure of their own, an `&&` or an
  else-if chain, still runs from block to block, which costs less.
- **Calls.** A call from JavaScript to JavaScript fills in a frame record of
  some fifteen fields, and each pointer it stores pays a write barrier's
  check. A method call whose body needs a frame -- one with a loop, even
  an empty one -- costs about 76 ns here, against about 69 ns in QuickJS;
  a method that only returns its argument costs about 55 ns, against 42. Richards, DeltaBlue and EarleyBoyer are mostly
  small calls. Two kinds of function are answered without a frame, where
  nothing they do could call code or throw:
  - one whose whole body is `return this.x`, `return this.x.length`,
    `return this.x[i]`, or stores of its parameters in `this`, which is much
    of EarleyBoyer's constructions;
  - one that reads and computes, and may store to a property once as the
    last thing it can fail at, as `isHeldOrSuspended`, DeltaBlue's `input`,
    `output` and `execute` and RayTrace's `dot` do. It may call another such
    function, `new` a constructor of the first kind, as EarleyBoyer's
    `sc_cons` does, and push onto or pop from an array, as DeltaBlue's
    collections do. Called that way, an empty function costs about 45 ns,
    against about 31 ns in QuickJS (`func_call`).
- **Property access.** Objects built the same way share a shape, as in
  QuickJS and V8, and each property read and write, each getter and
  setter, each global variable read and each `instanceof` remembers where it last found
  what it looked for. The cache is checked in a call rather than where the
  instruction runs, since the interpreter's loop grows slower with every line
  added to it. In this release a go-quickjs object keeps its keys beside
  its values, where a QuickJS object holds only its values, but that
  matters little: making every object 24 bytes larger moved Splay and
  EarleyBoyer by about 1%. Since `944e2e8` the keys are in the shape, as
  QuickJS keeps them.
- **Memory management.** QuickJS counts references: an object is freed the
  moment the last reference to it goes, and a cycle collector handles the
  rest. go-quickjs relies on Go's garbage collector. It traces the live heap
  concurrently and needs a write barrier on every pointer stored, including
  every value written to the interpreter's stack. The collector does most of
  its work on other cores, so with cores to spare it costs less wall-clock
  time than CPU time. A program that allocates heavily still pays for it in
  CPU time.
- **Bounds checks.** Go checks every slice index, and the interpreter's stack,
  locals and array elements are all slices.

Where go-quickjs is level or ahead:

- **Crypto.** Its arithmetic runs as trees, whose nodes read their locals
  and constants in place, and each bitwise operator has a node of its own
  that skips the conversion where its operand is already an integer.
- **RegExp.** A pattern that contains a run of plain text every match must
  have, near where the match begins, is searched for that text first, and
  tried only where it could have been reached from -- what V8's regexp
  compiler gets from its lookahead analysis, and most of the lead here. With
  an unmodified RegExp, `replace` and `split` find their matches without
  building the arrays `exec` would return, a `replace` template without a
  `$` makes no strings of what matched, and `split` searches for each
  separator instead of trying every position. The matcher passes over
  positions where no match can begin in a loop of their own, reads a code
  unit that is a whole character without a call, and takes a greedy run of
  one character in a single step.
- **Splay, in fixed work.** That is mostly building the tree and dropping
  it. Go's allocator makes many small objects cheaply, a short string or an
  array of up to sixteen elements is one allocation with its contents, and
  dropping the tree costs nothing until the collector runs. The splay
  operations themselves, which the score times, are level with
  QuickJS's.
- **Array elements updated, walked and skipped.** `a[k] += y` and `a[k]++`
  on a number in an array's storage are one step in a tree, the element
  updated where it is: one node where there were three, with the array and
  the key checked once rather than twice (`array_update`, 0.55 times
  QuickJS's time). for-in over an array whose keys are its elements walks
  them by index rather than copying them into a list first
  (`array_for_in`, 0.87), and the array methods pass over a hole
  without looking its index up, where nothing up the prototype chain could
  have it (`array_hole_length_decr`, 0.81). A typed array of a
  fixed length is written through the storage it keeps
  (`typed_array_write`, 0.88).
- **`apply(this, arguments)`.** In a function whose only use of `arguments` is
  passing it to `apply` (a common way to write a class constructor), the
  arguments object is never made. A constructor whose whole body is
  `this.initialize.apply(this, arguments)`, as RayTrace's are, calls the
  method directly, with the frames a stack trace would show standing in for
  its body.

Most of what remains is the cost of a call, which is most of the gap in
Richards and what is left of it in DeltaBlue, and the cost of a Go indirect call,
which every instruction or tree node pays. The ways around that which an
interpreter written in C has are not open to one written in Go, as
measured here:
- Running calls in the same loop, rather than a Go call each, made the loop
  slower than the calls it saved.
- Turning each instruction into a Go closure, instead of a case of the switch,
  came to little more than 10% on the loop best suited to it.
- Building each expression into a tree of closures that reads its locals
  and constants in place nearly halved the time Crypto and NavierStokes
  take, but did not make a call cheaper: entering a tree costs what entering
  the interpreter does.

[goja]: https://github.com/dop251/goja
[v8-v7]: https://github.com/mozilla/arewefastyet/tree/master/benchmarks/v8-v7
