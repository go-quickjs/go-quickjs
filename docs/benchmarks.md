# Benchmarks

On an Apple M5 Max with Go 1.27.0, taking the median of five fresh-process
runs at go-quickjs `04bbeec`:

```
BenchmarkEvalArithmetic-18  1.29µs/op   3906 B/op    20 allocs/op
BenchmarkFibonacci-18        447µs/op     16 B/op     1 allocs/op
BenchmarkPropertyAccess-18  1.57µs/op   4884 B/op    23 allocs/op
BenchmarkCallGoFunction-18  1.80µs/op   5524 B/op    31 allocs/op
```

`fib(20)` costs one allocation because the interpreter loop allocates nothing
per call; the other benchmarks include parsing and compiling their source each
iteration.

`go test -bench . -benchmem` also runs compile-once interpreter workloads,
plus compilation, runtime construction, and a recursive factorial workload.
That complete set is:

```
BenchmarkLoopArithmetic-18         81.9µs/op        0 B/op     0 allocs/op
BenchmarkDateUTC-18                 426µs/op        1 B/op     0 allocs/op
BenchmarkLoopPropertyAccess-18      257µs/op        0 B/op     0 allocs/op
BenchmarkLoopArrayIndex-18         14.0µs/op        0 B/op     0 allocs/op
BenchmarkLoopFunctionCall-18        222µs/op        0 B/op     0 allocs/op
BenchmarkLoopMethodCall-18          392µs/op        0 B/op     0 allocs/op
BenchmarkAllocObjects-18            101µs/op   352 KB/op  2000 allocs/op
BenchmarkAllocArrays-18            75.7µs/op   320 KB/op  2000 allocs/op
BenchmarkStringConcat-18           47.6µs/op   129 KB/op  1999 allocs/op
BenchmarkClosureCreateAndCall-18    384µs/op  1.76 MB/op  5001 allocs/op
BenchmarkArrayCallbacks-18         47.4µs/op  36.7 KB/op    24 allocs/op
BenchmarkMapOperations-18           185µs/op   311 KB/op    25 allocs/op
BenchmarkRegExpExec-18              323µs/op   448 KB/op  4000 allocs/op
BenchmarkThrowCatch-18              303µs/op   768 KB/op  6000 allocs/op
BenchmarkJSONRoundTrip-18           520µs/op   748 KB/op  9500 allocs/op
BenchmarkFactorial-18              21.8ms/op   379 KB/op  1294 allocs/op
BenchmarkCompileScript-18          21.7µs/op  39.7 KB/op   298 allocs/op
BenchmarkNewRuntime-18             61.7µs/op   268 KB/op  1222 allocs/op
BenchmarkNewRuntimeSmallStack-18   62.2µs/op   265 KB/op  1222 allocs/op
```

Each compile-once benchmark runs its workload many times: the main loop
benchmarks use 10,000 iterations, the object and array benchmarks allocate
2,000 values, and the callback, collection, regexp, exception and JSON
benchmarks exercise their operation hundreds or thousands of times.
`CompileScript` compiles one program per operation, while the runtime
benchmarks construct and close one runtime per operation.
`Factorial` constructs one runtime and evaluates 100,000 calls to
`factorial(10)` per operation, totaling one million recursive calls
(21.8 ns/call).

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
| Richards | 1,767 | 497 | go-quickjs 3.56x |
| DeltaBlue | 2,184 | 608 | go-quickjs 3.59x |
| Crypto | 2,871 | 321 | go-quickjs 8.94x |
| RayTrace | 4,318 | 731 | go-quickjs 5.91x |
| EarleyBoyer | 5,321 | 1,364 | go-quickjs 3.90x |
| RegExp | 3,992 | 570 | go-quickjs 7.00x |
| Splay | 7,432 | 2,314 | go-quickjs 3.21x |
| NavierStokes | 5,207 | 536 | go-quickjs 9.71x |
| **Composite score** | **3,748** | **720** | **go-quickjs 5.21x** |

The complete fresh-process run includes runtime construction, parsing,
compilation, the suite's warmups and its measured iterations:

| Metric | go-quickjs | goja | Relative result |
|---|---:|---:|---:|
| Wall time | 22.51 s | 53.37 s | go-quickjs 2.37x faster |
| Total allocation | 12.59 GiB | 31.66 GiB | go-quickjs 60.2% less |

The suite warms every workload for at least one second, then measures for at
least another second and continues until it has 32 measured iterations. Both
engines evaluated the same concatenated benchmark sources and validation code.
The measured revisions were go-quickjs `04bbeec`, goja `793a2a6`, and
AreWeFastYet `0e21608`. These results characterize this older interpreter
workload suite rather than every application; go-quickjs leads all eight of
its workloads in this comparison.

## Compared with C QuickJS

Over the V8 suite as a whole, go-quickjs is ahead of QuickJS: it takes
0.63 times QuickJS's time and has a 16% higher composite score. Single
operations still cost more, by about a twelfth on average, as
[the micro-benchmarks](#micro-benchmarks) below show.

go-quickjs has the same design as QuickJS: a bytecode compiler, a
stack-based interpreter, NaN-boxed values. Running that design in Go costs
speed. The same suite ran on an AMD Ryzen 5 3600 under Windows, with Go 1.27.1,
go-quickjs `1c9925f` and QuickJS 2026-06-04. The first two columns are milliseconds for the same
fixed work (the median of three fresh-process runs, lower is better). The
scores are the suite's own (higher is better). Richards and DeltaBlue, which
take only a few milliseconds a run, ran three hundred times in each process,
scaled to three.

| Workload | go-quickjs | QuickJS | QuickJS faster by | go-quickjs score | QuickJS score |
|---|---:|---:|---:|---:|---:|
| Richards | 14.0 ms | 11.0 ms | 1.27x | 763 | 958 |
| DeltaBlue | 24.0 ms | 23.7 ms | 1.01x | 830 | 840 |
| Crypto | 279 ms | 324 ms | 0.86x | 1,273 | 1,104 |
| RayTrace | 146 ms | 144 ms | 1.01x | 1,611 | 1,585 |
| EarleyBoyer | 372 ms | 401 ms | 0.93x | 2,181 | 2,148 |
| RegExp | 279 ms | 960 ms | 0.29x | 1,452 | 384 |
| Splay | 292 ms | 520 ms | 0.56x | 3,102 | 3,338 |
| NavierStokes | 212 ms | 190 ms | 1.12x | 2,330 | 2,403 |
| **Total / composite** | **1,618 ms** | **2,574 ms** | **0.63x** | **1,528** | **1,322** |

The two kinds of figure time different things, which is why they do not
agree on how far ahead go-quickjs is:

- A fixed run times each workload's setup, its runs and its teardown. The
  scored run times only the runs, after a second of warming up.
- The total adds milliseconds, so the longest workloads, RegExp and Splay,
  make most of go-quickjs's lead. The composite score is the geometric mean
  of the workloads' scores, so each counts the same: Richards' gap weighs as
  much as RegExp's lead. By that mean the fixed figures put go-quickjs
  about 19% ahead, and the scores 16%.
- Splay changes sides. Its fixed time is almost all setup and teardown:
  building a tree of 8,000 nodes and dropping it, which QuickJS frees by
  counting references where Go's collector frees it later. A warm run,
  which is what the score times, is splay-tree operations, calls and
  property reads, and takes about 8% longer here. Giving the
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
go-quickjs `1c9925f`, and the figure is the faster of two runs.
`sort_bench` is left out: it reports a run's time over its fastest sort, not
the time of an operation. Over the other 71 tests go-quickjs takes 1.08 times
QuickJS's time by the geometric mean: it is slower on 42, faster on 25, and
level on the rest. The table has the largest gaps either way.

| Test | go-quickjs | QuickJS | go-quickjs / QuickJS |
|---|---:|---:|---:|
| `weak_map_set` | 318 ns | 92.8 ns | 3.43x |
| `weak_map_delete` | 580 ns | 248 ns | 2.34x |
| `array_prop_create` | 33.1 ns | 15.0 ns | 2.21x |
| `bigint256_arith` | 260 ns | 127 ns | 2.05x |
| `global_read` | 11.1 ns | 5.5 ns | 2.00x |
| `bigint64_arith` | 74.2 ns | 39.6 ns | 1.88x |
| `regexp_utf16` | 485 ns | 271 ns | 1.79x |
| `bigint32_arith` | 40.2 ns | 23.1 ns | 1.74x |
| `float_toFixed` | 264 ns | 162 ns | 1.63x |
| `array_read` | 10.7 ns | 6.6 ns | 1.62x |
| `empty_loop` | 8.5 ns | 5.3 ns | 1.59x |
| `empty_down_loop` | 7.8 ns | 5.1 ns | 1.53x |
| `global_write_strict` | 10.5 ns | 6.9 ns | 1.52x |
| `array_for_of` | 27.8 ns | 46.1 ns | 0.60x |
| `array_pop` | 66.4 ns | 111 ns | 0.60x |
| `prop_clone` | 42.7 ns | 78.2 ns | 0.55x |
| `arguments_read` | 88.7 ns | 166 ns | 0.53x |
| `string_build2c` | 66.4 ns | 148 ns | 0.45x |
| `string_to_int` | 46.3 ns | 112 ns | 0.41x |
| `array_slice` | 9.2 ns | 27.3 ns | 0.33x |

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
    collections do. Called that way, an empty function costs about 38 ns,
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
  operations themselves, which the score times, take about 8% longer
  than QuickJS's.
- **Array elements updated, walked and skipped.** `a[k] += y` and `a[k]++`
  on a number in an array's storage are one step in a tree, the element
  updated where it is: one node where there were three, with the array and
  the key checked once rather than twice (`array_update`, 0.84 times
  QuickJS's time). for-in over an array whose keys are its elements walks
  them by index rather than copying them into a list first
  (`array_for_in`, 0.99), and the array methods pass over a hole
  without looking its index up, where nothing up the prototype chain could
  have it (`array_hole_length_decr`, 0.77). A typed array of a
  fixed length is written through the storage it keeps
  (`typed_array_write`, 1.08).
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
