# Benchmarks

On an Apple M5 Max with Go 1.27.0, taking the median of five fresh-process
runs at go-quickjs `67de024`:

```
BenchmarkEvalArithmetic-18  1.18µs/op   3890 B/op    20 allocs/op
BenchmarkFibonacci-18        720µs/op     17 B/op     1 allocs/op
BenchmarkPropertyAccess-18  1.50µs/op   4867 B/op    23 allocs/op
BenchmarkCallGoFunction-18  1.59µs/op   5508 B/op    31 allocs/op
```

`fib(20)` costs one allocation because the interpreter loop allocates nothing
per call; the other benchmarks include parsing and compiling their source each
iteration.

`go test -bench . -benchmem` also runs compile-once interpreter workloads,
plus compilation and runtime-construction benchmarks. That complete set is:

```
BenchmarkLoopArithmetic-18         88.3µs/op        0 B/op     0 allocs/op
BenchmarkDateUTC-18                 421µs/op        1 B/op     0 allocs/op
BenchmarkLoopPropertyAccess-18      261µs/op        0 B/op     0 allocs/op
BenchmarkLoopArrayIndex-18         14.3µs/op        0 B/op     0 allocs/op
BenchmarkLoopFunctionCall-18        216µs/op        0 B/op     0 allocs/op
BenchmarkLoopMethodCall-18          394µs/op        0 B/op     0 allocs/op
BenchmarkAllocObjects-18            100µs/op   352 KB/op  2000 allocs/op
BenchmarkAllocArrays-18            73.8µs/op   320 KB/op  2000 allocs/op
BenchmarkStringConcat-18           46.9µs/op   129 KB/op  1999 allocs/op
BenchmarkClosureCreateAndCall-18    385µs/op  1.76 MB/op  5001 allocs/op
BenchmarkArrayCallbacks-18         47.3µs/op  36.6 KB/op    24 allocs/op
BenchmarkMapOperations-18           178µs/op   311 KB/op    25 allocs/op
BenchmarkRegExpExec-18              315µs/op   448 KB/op  4000 allocs/op
BenchmarkThrowCatch-18              291µs/op   768 KB/op  6000 allocs/op
BenchmarkJSONRoundTrip-18           506µs/op   748 KB/op  9500 allocs/op
BenchmarkCompileScript-18          20.5µs/op  38.7 KB/op   295 allocs/op
BenchmarkNewRuntime-18             59.6µs/op   268 KB/op  1222 allocs/op
BenchmarkNewRuntimeSmallStack-18   62.4µs/op   265 KB/op  1222 allocs/op
```

Each compile-once benchmark runs its workload many times: the main loop
benchmarks use 10,000 iterations, the object and array benchmarks allocate
2,000 values, and the callback, collection, regexp, exception and JSON
benchmarks exercise their operation hundreds or thousands of times.
`CompileScript` compiles one program per operation, while the runtime
benchmarks construct and close one runtime per operation.

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
fresh-process runs on an Apple M5 Max with Go 1.27.0. Higher suite scores are
better.

| Workload | go-quickjs | goja | Relative result |
|---|---:|---:|---:|
| Richards | 1,779 | 508 | go-quickjs 3.50x |
| DeltaBlue | 2,204 | 617 | go-quickjs 3.57x |
| Crypto | 2,767 | 327 | go-quickjs 8.46x |
| RayTrace | 4,379 | 793 | go-quickjs 5.52x |
| EarleyBoyer | 5,535 | 1,447 | go-quickjs 3.83x |
| RegExp | 3,909 | 585 | go-quickjs 6.68x |
| Splay | 7,253 | 2,440 | go-quickjs 2.97x |
| NavierStokes | 5,057 | 544 | go-quickjs 9.30x |
| **Composite score** | **3,725** | **734** | **go-quickjs 5.07x** |

The complete fresh-process run includes runtime construction, parsing,
compilation, the suite's warmups and its measured iterations:

| Metric | go-quickjs | goja | Relative result |
|---|---:|---:|---:|
| Wall time | 22.37 s | 51.27 s | go-quickjs 2.29x faster |
| Total allocation | 12.77 GiB | 31.40 GiB | go-quickjs 59.3% less |

The suite warms every workload for at least one second, then measures for at
least another second and continues until it has 32 measured iterations. Both
engines evaluated the same concatenated benchmark sources and validation code.
The measured revisions were go-quickjs `67de024`, goja `793a2a6`, and
AreWeFastYet `0e21608`. These results characterize this older interpreter
workload suite rather than every application; go-quickjs leads all eight of
its workloads in this comparison.

## Compared with C QuickJS

Over the V8 suite as a whole, go-quickjs is ahead of QuickJS: it takes
0.68 times QuickJS's time and has a 9% higher composite score. Single
operations still cost more, by about an eighth on average, as
[the micro-benchmarks](#micro-benchmarks) below show.

go-quickjs has the same design as QuickJS: a bytecode compiler, a
stack-based interpreter, NaN-boxed values. Running that design in Go costs
speed. The same suite ran on an AMD Ryzen 5 3600 under Windows, with Go 1.27.1,
go-quickjs `69f51f7` and QuickJS 2026-06-04. The first two columns are milliseconds for the same
fixed work (the median of three fresh-process runs, lower is better). The
scores are the suite's own (higher is better). Richards and DeltaBlue, which
take only a few milliseconds a run, ran three hundred times in each process,
scaled to three.

| Workload | go-quickjs | QuickJS | QuickJS faster by | go-quickjs score | QuickJS score |
|---|---:|---:|---:|---:|---:|
| Richards | 15.4 ms | 11.1 ms | 1.39x | 681 | 959 |
| DeltaBlue | 25.0 ms | 23.8 ms | 1.05x | 798 | 841 |
| Crypto | 282 ms | 325 ms | 0.87x | 1,288 | 1,102 |
| RayTrace | 146 ms | 143 ms | 1.02x | 1,570 | 1,581 |
| EarleyBoyer | 411 ms | 397 ms | 1.03x | 2,060 | 2,168 |
| RegExp | 286 ms | 865 ms | 0.33x | 1,421 | 432 |
| Splay | 300 ms | 512 ms | 0.59x | 2,993 | 3,286 |
| NavierStokes | 221 ms | 188 ms | 1.18x | 2,255 | 2,419 |
| **Total / composite** | **1,686 ms** | **2,465 ms** | **0.68x** | **1,466** | **1,344** |

The two kinds of figure time different things, which is why they do not
agree on how far ahead go-quickjs is:

- A fixed run times each workload's setup, its runs and its teardown. The
  scored run times only the runs, after a second of warming up.
- The total adds milliseconds, so the longest workloads, RegExp and Splay,
  make most of go-quickjs's lead. The composite score is the geometric mean
  of the workloads' scores, so each counts the same: Richards' gap weighs as
  much as RegExp's lead. By that mean the fixed figures put go-quickjs
  about 14% ahead, and the scores 9%.
- Splay changes sides. Its fixed time is almost all setup and teardown:
  building a tree of 8,000 nodes and dropping it, which QuickJS frees by
  counting references where Go's collector frees it later. A warm run,
  which is what the score times, is splay-tree operations, calls and
  property reads, and takes about a tenth longer here. Giving the
  collector four times the room (`GOGC=400`) leaves Splay's score as it
  was, so collection is not what the score measures.

Figures move by a few percent between builds from code placement alone,
and Richards and DeltaBlue by more, up to a tenth. To reproduce them, run
`go run ./internal/cmd/v8bench -dir /tmp/v8-v7 -mode fixed -n 3` and the
`external` runner with `-engine qjs`.

### Micro-benchmarks

QuickJS's own micro-benchmarks, [`tests/microbench.js`](https://github.com/bellard/quickjs/blob/master/tests/microbench.js),
time one operation each, in nanoseconds. Each test was given the work QuickJS
takes about 150 ms for, both engines ran that same work in fresh processes at
go-quickjs `69f51f7`, and the figure is the faster of two runs. Over the 72
tests go-quickjs takes 1.13 times QuickJS's time by the geometric mean: it is
slower on 46, faster on 22, and level on the rest. The table has the largest
gaps either way.

| Test | go-quickjs | QuickJS | go-quickjs / QuickJS |
|---|---:|---:|---:|
| `weak_map_set` | 284 ns | 91.8 ns | 3.10x |
| `array_prop_create` | 41.2 ns | 15.0 ns | 2.76x |
| `weak_map_delete` | 585 ns | 251 ns | 2.33x |
| `bigint256_arith` | 264 ns | 126 ns | 2.09x |
| `global_read` | 11.5 ns | 5.6 ns | 2.06x |
| `array_update` | 15.8 ns | 8.6 ns | 1.83x |
| `bigint64_arith` | 71.8 ns | 39.6 ns | 1.81x |
| `regexp_utf16` | 480 ns | 269 ns | 1.79x |
| `typed_array_read` | 16.8 ns | 9.9 ns | 1.69x |
| `float_toFixed` | 275 ns | 164 ns | 1.67x |
| `bigint32_arith` | 39.3 ns | 23.6 ns | 1.67x |
| `array_read` | 10.7 ns | 6.6 ns | 1.63x |
| `typed_array_write` | 23.1 ns | 14.2 ns | 1.62x |
| `string_length` | 9.2 ns | 14.1 ns | 0.65x |
| `array_pop` | 71.3 ns | 111 ns | 0.64x |
| `array_for_of` | 28.4 ns | 46.4 ns | 0.61x |
| `prop_clone` | 42.7 ns | 77.4 ns | 0.55x |
| `string_build2c` | 64.0 ns | 147 ns | 0.44x |
| `string_to_int` | 46.7 ns | 112 ns | 0.42x |
| `array_slice` | 8.8 ns | 27.2 ns | 0.32x |

Some of these tests depend on where the linker places the code more than on
the code, by more than eight placements can average out: those vary where
the code starts, not how far apart its pieces are. Grouping the hot files
(`250e8f6`) left the V8 suite level but made `array_for`, `array_update` and
`float_arith` 33–43% slower and `func_call` 26% slower, at every placement.
`6b12267` changed none of their code, only the size of two of the tree
tier's nodes. In the build measured above all four are about as fast as before
the grouping again, `func_call` within 4%; averaged over eight placements of
`6b12267`, `array_update` (29.8 ns) and `func_call` (48.5 ns) had come back
only part of the way, and `array_push` and `map_delete` were 7–10% slower. A
change of that size in one of these tests is not by itself a change in the
engine.

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

  An instruction costs about twice what it costs QuickJS. Fusing common
  instruction sequences into single instructions (`a[i]`, `a[++i]`,
  `x & 0xff`, `local * local`) reduces how many are dispatched. Nearly
  every function runs instead as trees of Go closures, a node for every few
  instructions with no operand stack between them, and with locals and
  constants read in place (see the [design notes](design.md)). That is what
  closed the gap on Crypto and most of it on NavierStokes, which do little but
  arithmetic, local variables and array elements.
- **Calls.** A call from JavaScript to JavaScript fills in a frame record of
  some fifteen fields, and each pointer it stores pays a write barrier's
  check. A method call that makes a frame costs about 70 ns here, against
  about 40 ns in QuickJS. Richards, DeltaBlue and EarleyBoyer are mostly
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
    collections do. Called that way, an empty function costs about 42 ns,
    against about 31 ns in QuickJS (`func_call`).
- **Property access.** Objects built the same way share a shape, as in
  QuickJS and V8, and each property read and write, each getter and
  setter, each global variable read and each `instanceof` remembers where it last found
  what it looked for. The cache is checked in a call rather than where the
  instruction runs, since the interpreter's loop grows slower with every line
  added to it. A go-quickjs object keeps its keys beside its values, where a
  QuickJS object holds only its values, but that matters little: making every
  object 24 bytes larger moved Splay and EarleyBoyer by about 1%.
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
  operations themselves, which the score times, take about a tenth longer
  than QuickJS's.
- **`apply(this, arguments)`.** In a function whose only use of `arguments` is
  passing it to `apply` (a common way to write a class constructor), the
  arguments object is never made. A constructor whose whole body is
  `this.initialize.apply(this, arguments)`, as RayTrace's are, calls the
  method directly, with the frames a stack trace would show standing in for
  its body.

Most of what remains is the cost of a call, which is most of the gap in
Richards, DeltaBlue and EarleyBoyer, and the cost of a Go indirect call,
which every instruction or tree node pays. The ways around that which an
engine in C or with a JIT has are not open to one in Go, as measured here:
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
