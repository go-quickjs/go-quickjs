# Benchmarks

On an Apple M5 Max with Go 1.27.0 and the native JIT disabled, taking
per-metric medians of five fresh-process runs at go-quickjs `90b4e60f` on
2026-10-21, with 30 seconds between runs and AC power throughout:

```
BenchmarkEvalArithmetic-18  1.26µs/op   4131 B/op    22 allocs/op
BenchmarkFibonacci-18        441µs/op     17 B/op     1 allocs/op
BenchmarkPropertyAccess-18  1.56µs/op   5268 B/op    26 allocs/op
BenchmarkCallGoFunction-18  1.60µs/op   5876 B/op    33 allocs/op
```

`fib(20)` costs one allocation because the interpreter loop allocates nothing
per call; the other benchmarks include parsing and compiling their source each
iteration.

`go test -run '^$' -bench . -benchmem .` also runs compile-once interpreter
workloads, plus compilation, runtime construction, and a recursive factorial
workload. The remaining root-package benchmarks are:

```
BenchmarkLoopArithmetic-18         59.7µs/op        1 B/op     0 allocs/op
BenchmarkDateUTC-18                 390µs/op        1 B/op     0 allocs/op
BenchmarkLoopPropertyAccess-18      210µs/op        0 B/op     0 allocs/op
BenchmarkLoopArrayIndex-18         12.0µs/op        0 B/op     0 allocs/op
BenchmarkLoopFunctionCall-18        188µs/op        0 B/op     0 allocs/op
BenchmarkLoopMethodCall-18          257µs/op        0 B/op     0 allocs/op
BenchmarkAllocObjects-18           77.8µs/op   224 KB/op  2000 allocs/op
BenchmarkAllocArrays-18            72.7µs/op   320 KB/op  2000 allocs/op
BenchmarkStringConcat-18           36.6µs/op  97.2 KB/op  1999 allocs/op
BenchmarkClosureCreateAndCall-18    332µs/op  1.44 MB/op  5001 allocs/op
BenchmarkArrayCallbacks-18         46.5µs/op  36.4 KB/op    18 allocs/op
BenchmarkMapOperations-18           133µs/op   311 KB/op    25 allocs/op
BenchmarkRegExpExec-18              358µs/op   384 KB/op  3000 allocs/op
BenchmarkThrowCatch-18              240µs/op   704 KB/op  6000 allocs/op
BenchmarkJSONRoundTrip-18           514µs/op   612 KB/op  8500 allocs/op
BenchmarkFactorial-18              20.5ms/op   379 KB/op  1694 allocs/op
BenchmarkCompileScript-18          27.4µs/op  52.1 KB/op   452 allocs/op
BenchmarkNewRuntime-18             64.5µs/op   267 KB/op  1574 allocs/op
BenchmarkNewRuntimeSmallStack-18   65.5µs/op   265 KB/op  1574 allocs/op
```

Each compile-once benchmark runs its workload many times: the main loop
benchmarks use 10,000 iterations, the object and array benchmarks allocate
2,000 values, and the callback, collection, regexp, exception and JSON
benchmarks exercise their operation hundreds or thousands of times.
`CompileScript` parses, compiles and evaluates one program per operation,
while the runtime benchmarks construct and close one runtime per operation.
`Factorial` constructs one runtime and evaluates 100,000 calls to
`factorial(10)` per operation, totaling one million recursive calls
(20.5 ns/call). Each of the five samples came from a separate invocation,
with one second per benchmark and `-benchmem`, not repeated measurements
inside a single test process.

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
[goja]. Both go-quickjs columns use revision `90b4e60f`: JIT on was measured
on 2026-10-20, JIT off and goja on 2026-10-21. Each column is the median of
three fresh-process runs on an Apple M5 Max with Go 1.27.0, with 30 seconds
between runs and AC power throughout. The engines ran in separate batches,
not alternating order. JIT off was built without the `quickjs_jit` tag.
Higher suite scores are better; the relative columns divide each
go-quickjs score by goja's.

| Workload | go-quickjs JIT off | go-quickjs JIT on | goja | JIT off / goja | JIT on / goja |
|---|---:|---:|---:|---:|---:|
| Richards | 2,058 | 11,014 | 512 | 4.02x | 21.51x |
| DeltaBlue | 2,514 | 10,440 | 620 | 4.05x | 16.84x |
| Crypto | 2,897 | 16,068 | 326 | 8.89x | 49.29x |
| RayTrace | 5,397 | 18,518 | 769 | 7.02x | 24.08x |
| EarleyBoyer | 6,366 | 9,587 | 1,384 | 4.60x | 6.93x |
| RegExp | 3,996 | 4,355 | 582 | 6.87x | 7.48x |
| Splay | 8,777 | 12,224 | 2,589 | 3.39x | 4.72x |
| NavierStokes | 5,776 | 34,429 | 534 | 10.82x | 64.47x |
| **Composite score** | **4,238** | **12,479** | **738** | **5.74x** | **16.91x** |

The runner's timed suite interval includes runtime construction, parsing,
compilation, the suite's warmups and its measured iterations, but excludes
reading the source files and the final forced garbage collection. CPU time
includes work on other threads, including the collector. These are the
JIT-off go-quickjs and goja runs:

| Metric | go-quickjs JIT off | goja | Relative result |
|---|---:|---:|---:|
| Suite wall time | 22.37 s | 52.04 s | go-quickjs 2.33x faster |
| Suite CPU time | 27.40 s | 73.07 s | go-quickjs 62.5% less |
| Total allocation | 11.09 GiB | 31.73 GiB | go-quickjs 65.1% less |

Score mode does different amounts of work as throughput changes, so the CPU
and allocation totals are not equal-work efficiency measurements. The
composite is the median of the three printed composite scores, not a
composite calculated from the workload medians.

The suite warms every workload for at least one second, then measures for at
least another second and continues until it has 32 measured iterations. Both
engines evaluated the same benchmark sources and validation code.
The measured revisions were go-quickjs `90b4e60f`, goja `793a2a6`, and
AreWeFastYet `0e21608`. These results characterize this older workload
suite rather than every application; go-quickjs leads all eight of its
workloads in this comparison.

The comparison used `internal/cmd/v8bench/matrix`, selecting `gq`, `gqjit`
and `goja` in separate invocations with `-suites score -runs 1
-parallel=false -v8 /tmp/v8-v7` and a separate `-out` directory for each
sample, with 30-second pauses. `-report` combined the saved outputs into
the median score table. Within each engine's batch, the three binaries
were byte-identical. The JIT-off composite samples were 4,305, 4,221 and
4,238; goja's were 748, 733 and 738. All samples were included.

For the JIT-on column, the runner was built with `-tags quickjs_jit` and
run with `-jit`. Its composite samples were 12,729, 12,479 and 12,266, all
included. The table takes the median of each workload and of the printed
composite scores independently.

## Compared with C QuickJS

Over the V8 suite as a whole, go-quickjs is ahead of QuickJS: it takes
0.60 times QuickJS's time, is level on Richards and ahead on the other
seven, and has a 24% higher composite score. Single operations cost a
little less on average, 0.93 times QuickJS's time, as
[the micro-benchmarks](#micro-benchmarks) below show, though a few still
take about twice as long.

go-quickjs has the same design as QuickJS: a bytecode compiler, a
stack-based interpreter, NaN-boxed values. Running that design in Go costs
speed. The same suite ran on an AMD Ryzen 5 3600 under Windows, with Go
1.27.1, go-quickjs v0.27.0 (`6099e8c`) and QuickJS 2026-06-04, the two
engines at once, each on the six CPUs of a level 3 cache of its own. The
first two columns are milliseconds for the same fixed work (the median of
three fresh-process runs, lower is better). The scores are the suite's own
(higher is better), the median of three.

| Workload | go-quickjs | QuickJS | QuickJS faster by | go-quickjs score | QuickJS score |
|---|---:|---:|---:|---:|---:|
| Richards | 11.2 ms | 11.0 ms | 1.02x | 833 | 936 |
| DeltaBlue | 23.2 ms | 25.0 ms | 0.93x | 826 | 829 |
| Crypto | 277 ms | 331 ms | 0.84x | 1,294 | 1,087 |
| RayTrace | 122 ms | 142 ms | 0.86x | 1,857 | 1,537 |
| EarleyBoyer | 358 ms | 402 ms | 0.89x | 2,426 | 2,131 |
| RegExp | 259 ms | 867 ms | 0.30x | 1,553 | 428 |
| Splay | 269 ms | 511 ms | 0.53x | 3,256 | 3,265 |
| NavierStokes | 183 ms | 191 ms | 0.96x | 2,555 | 2,398 |
| **Total / composite** | **1,527 ms** | **2,526 ms** | **0.60x** | **1,638** | **1,321** |

The two kinds of figure time different things, which is why they do not
agree on how far ahead go-quickjs is:

- A fixed run times each workload's setup, its runs and its teardown. The
  scored run times only the runs, after a second of warming up.
- The total adds milliseconds, so the longest workloads, RegExp and Splay,
  make most of go-quickjs's lead. The composite score is the geometric mean
  of the workloads' scores, so each counts the same: Richards weighs as
  much as RegExp. By that mean the fixed figures put go-quickjs about 35%
  ahead, and the scores 24%. The scores have Richards 11% behind, where
  its fixed time, its setup included, is level.
- Splay's lead is in its fixed time. That is almost all setup and
  teardown: building a tree of 8,000 nodes and dropping it, which QuickJS
  frees by counting references where Go's collector frees it later. A
  warm run, which is what the score times, is splay-tree operations, calls
  and property reads, and is level with QuickJS's. Giving the collector
  four times the room (`GOGC=400`) leaves Splay's score as it was, so
  collection is not what the score measures.

Figures move by a few percent between builds from code placement alone,
and Richards and DeltaBlue by more, up to a tenth. To reproduce them and the
micro-benchmarks below, run `go run ./internal/cmd/v8bench/matrix -engines
gq,qjs -suites v8,micro,score -qjs /path/to/qjs -v8 /tmp/v8-v7`.

### Micro-benchmarks

QuickJS's own micro-benchmarks, [`tests/microbench.js`](https://github.com/bellard/quickjs/blob/master/tests/microbench.js),
time one operation each, in nanoseconds. Each test was given the work QuickJS
takes about 150 ms for, both engines ran that same work in fresh processes at
go-quickjs v0.27.0, and the figure is the fastest of three runs. The runners'
micro mode does this, from a copy of the file kept in `internal/v8bench`, and
the matrix above runs it. `sort_bench` is left out: it reports a run's time
over its fastest sort, not the time of an operation. Over the other 71 tests
go-quickjs takes 0.93 times QuickJS's time by the geometric mean: it is
slower on 26, faster on 38, and level on the rest. The table has the largest
gaps either way.

| Test | go-quickjs | QuickJS | go-quickjs / QuickJS |
|---|---:|---:|---:|
| `array_prop_create` | 36.1 ns | 15.1 ns | 2.39x |
| `bigint256_arith` | 282 ns | 129 ns | 2.19x |
| `weak_map_delete` | 549 ns | 254 ns | 2.16x |
| `weak_map_set` | 194 ns | 94.0 ns | 2.06x |
| `bigint64_arith` | 68.6 ns | 39.7 ns | 1.73x |
| `global_read` | 9.6 ns | 5.7 ns | 1.70x |
| `prop_create` | 82.4 ns | 50.8 ns | 1.62x |
| `regexp_utf16` | 390 ns | 273 ns | 1.43x |
| `array_read` | 9.3 ns | 6.6 ns | 1.40x |
| `regexp_ascii` | 363 ns | 263 ns | 1.38x |
| `string_length` | 7.8 ns | 14.4 ns | 0.55x |
| `string_build2` | 52.7 ns | 96.8 ns | 0.55x |
| `string_build_large1` | 55.4 ns | 102 ns | 0.54x |
| `array_pop` | 60.4 ns | 113 ns | 0.54x |
| `prop_update` | 6.2 ns | 11.8 ns | 0.52x |
| `array_slice` | 11.8 ns | 27.6 ns | 0.43x |
| `prop_clone` | 31.1 ns | 72.8 ns | 0.43x |
| `string_build2c` | 54.6 ns | 150 ns | 0.36x |
| `string_to_int` | 36.1 ns | 114 ns | 0.32x |

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
none of, written as its tests are: generators and async functions, try
statements, iterators, using declarations, closures made and called anew,
calls in tail position and private class members. Over the 22 QuickJS can
run (it has no using declarations) go-quickjs takes 0.97 times its time.
An await costs much less, resumed without allocating (`3af4f32`), as does
an async generator; a generator costs more to make and to resume from
for-of, as do two functions calling each other in tail position, which
make a frame for each call where a function calling itself does not:

| Test | go-quickjs | QuickJS | go-quickjs / QuickJS |
|---|---:|---:|---:|
| `tail_mutual` | 97.7 ns | 35.7 ns | 2.74x |
| `generator_for_of` | 107 ns | 56.5 ns | 1.90x |
| `generator_create` | 699 ns | 411 ns | 1.70x |
| `try_finally` | 36.2 ns | 23.0 ns | 1.58x |
| `tail_self` | 54.6 ns | 36.1 ns | 1.51x |
| `private_field` | 39.3 ns | 26.4 ns | 1.49x |
| `private_method` | 83.3 ns | 65.5 ns | 1.27x |
| `generator_busy` | 24.3 ns | 19.4 ns | 1.26x |
| `try_catch` | 16.5 ns | 13.6 ns | 1.22x |
| `closure_callback` | 90.3 ns | 79.9 ns | 1.13x |
| `try_throw` | 33.2 ns | 30.3 ns | 1.10x |
| `async_busy` | 23.8 ns | 22.0 ns | 1.08x |
| `generator_delegate` | 244 ns | 231 ns | 1.05x |
| `map_for_of` | 36.0 ns | 36.8 ns | 0.98x |
| `generator_next` | 200 ns | 205 ns | 0.97x |
| `closure_counter` | 38.8 ns | 43.3 ns | 0.89x |
| `iterator_for_of` | 231 ns | 315 ns | 0.73x |
| `closure_make` | 209 ns | 308 ns | 0.68x |
| `async_chain` | 1,511 ns | 2,541 ns | 0.59x |
| `async_call` | 459 ns | 857 ns | 0.53x |
| `async_generator` | 569 ns | 1,936 ns | 0.29x |
| `async_await` | 114 ns | 752 ns | 0.15x |

The BigInt tests were the largest gaps, 9.65 and 5.56 times QuickJS's time
at `608415b`, where every BigInt was an allocation. A BigInt in the int64
range is now held in the value itself (`9746906`), as QuickJS holds one that
fits in a word, and their loops run as trees since the tree tier stopped
leaving a function that makes calls to the interpreter (`3df6232`).
`bigint64_arith`'s sum also outgrows 64 bits halfway through each run, after
which both engines allocate.

The largest gaps now are `array_prop_create`, an array made by assigning
past its end, whose storage grows as it goes and leaves what it outgrew to
the collector; the BigInts past 64 bits; and the weak collections. A
WeakMap holds each value on its key rather than in a table of its own
(`2bc1620`), so that a value which refers to its key does not keep the key
alive, as the specification requires; adding an entry costs more for it.
Most of the others are the cost of each step, which the list below
explains. The suite as a whole does better than the operations it is
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

  The interpreter takes nearly twice QuickJS's time for the same loop:
  1.2 to 3.0 times over six loops of simple instructions (1.8 by their
  geometric mean; v0.27.0 with the tree tier off, `QJS_NOTREE=1`), each
  of its instructions 3.0 to 4.9 ns. Fusing common instruction sequences
  into single instructions (`a[i]`, `a[++i]`, `x & 0xff`,
  `local * local`) reduces how many are dispatched. Nearly
  every function runs instead as trees of Go closures, a node for every few
  instructions with no operand stack between them, and with locals and
  constants read in place (see the [design notes](design.md)). That is what
  closed the gap on Crypto and most of it on NavierStokes, which do little but
  arithmetic, local variables and array elements. A tree's loops run as Go
  loops, rebuilt from its blocks, with the comparison that ends a loop and
  the `i++` that steps it made in place: the tests that are mostly a loop --
  `empty_loop` (0.94), `int_arith` (0.94), `float_arith` (0.73),
  `array_update` -- are level with QuickJS or ahead of it, and the six
  loops above take 0.42 to 1.15 times QuickJS's time as trees. A loop whose body
  would need its branches chosen by a structure of their own, an `&&` or an
  else-if chain, still runs from block to block, which costs less.
- **Calls.** A call from JavaScript to JavaScript fills in a frame record of
  some fifteen fields, and each pointer it stores pays a write barrier's
  check. Even so a call costs less than QuickJS's: a method call whose
  body needs a frame -- one with a loop, even an empty one -- about 64 ns,
  against about 69 ns in QuickJS, and a method that only returns its
  argument about 33 ns, against 42. Richards, DeltaBlue and EarleyBoyer
  are mostly small calls. Two kinds of function are answered without a
  frame, where nothing they do could call code or throw:
  - one whose whole body is `return this.x`, `return this.x.length`,
    `return this.x[i]`, or stores of its parameters in `this`, which is much
    of EarleyBoyer's constructions;
  - one that reads and computes, and may store to a property once as the
    last thing it can fail at, as `isHeldOrSuspended`, DeltaBlue's `input`,
    `output` and `execute` and RayTrace's `dot` do. It may call another such
    function, `new` a constructor of the first kind, as EarleyBoyer's
    `sc_cons` does, and push onto or pop from an array, as DeltaBlue's
    collections do. Called that way, an empty function costs about 29 ns,
    against about 31 ns in QuickJS (`func_call`). One of seven
    instructions or more that calls nothing runs as its tree instead,
    whose frame costs less than evaluating it a step at a time.
- **Property access.** Objects built the same way share a shape, as in
  QuickJS and V8, and each property read and write, each getter and
  setter, each global variable read and each `instanceof` remembers where it last found
  what it looked for. The cache is checked in a call rather than where the
  instruction runs, since the interpreter's loop grows slower with every line
  added to it. An object holds only its values, its keys in its shape, as
  a QuickJS object does (`944e2e8`).
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
  the key checked once rather than twice (`array_update`, 0.62 times
  QuickJS's time). for-in over an array whose keys are its elements walks
  them by index rather than copying them into a list first
  (`array_for_in`, 1.03), and the array methods pass over a hole
  without looking its index up, where nothing up the prototype chain could
  have it (`array_hole_length_decr`, 0.86). A typed array of a
  fixed length is written through the storage it keeps
  (`typed_array_write`, 0.91).
- **`apply(this, arguments)`.** In a function whose only use of `arguments` is
  passing it to `apply` (a common way to write a class constructor), the
  arguments object is never made. A constructor whose whole body is
  `this.initialize.apply(this, arguments)`, as RayTrace's are, calls the
  method directly, with the frames a stack trace would show standing in for
  its body.

What remains is mostly the cost of a Go indirect call, which every
instruction or tree node pays, and of the operations the gaps above are
in. The ways around the first which an interpreter written in C has are
not open to one written in Go, as measured here:
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
