# Benchmarks

On an Apple M5 Max with Go 1.27.0, taking the median of five fresh-process
runs at go-quickjs `d0122e4`:

```
BenchmarkEvalArithmetic-18  1.25µs/op   4184 B/op    24 allocs/op
BenchmarkFibonacci-18        709µs/op     16 B/op     1 allocs/op
BenchmarkPropertyAccess-18  1.63µs/op   5448 B/op    31 allocs/op
BenchmarkCallGoFunction-18  1.59µs/op   6032 B/op    34 allocs/op
```

`fib(20)` costs one allocation because the interpreter loop allocates nothing
per call; the other benchmarks include parsing and compiling their source each
iteration.

`go test -bench . -benchmem` also runs compile-once interpreter workloads,
plus compilation and runtime-construction benchmarks. That complete set is:

```
BenchmarkLoopArithmetic-18         84.4µs/op        0 B/op     0 allocs/op
BenchmarkDateUTC-18                 446µs/op        1 B/op     0 allocs/op
BenchmarkLoopPropertyAccess-18      307µs/op        0 B/op     0 allocs/op
BenchmarkLoopArrayIndex-18         18.8µs/op        0 B/op     0 allocs/op
BenchmarkLoopFunctionCall-18        251µs/op        0 B/op     0 allocs/op
BenchmarkLoopMethodCall-18          517µs/op        1 B/op     0 allocs/op
BenchmarkAllocObjects-18           94.8µs/op   320 KB/op  2000 allocs/op
BenchmarkAllocArrays-18            72.9µs/op   320 KB/op  2000 allocs/op
BenchmarkStringConcat-18           53.9µs/op   161 KB/op  1999 allocs/op
BenchmarkClosureCreateAndCall-18    507µs/op  2.40 MB/op  5001 allocs/op
BenchmarkArrayCallbacks-18          108µs/op  37.0 KB/op    24 allocs/op
BenchmarkMapOperations-18           268µs/op   394 KB/op    46 allocs/op
BenchmarkRegExpExec-18              342µs/op   496 KB/op  4000 allocs/op
BenchmarkThrowCatch-18              286µs/op   768 KB/op  6000 allocs/op
BenchmarkJSONRoundTrip-18           478µs/op   772 KB/op  9500 allocs/op
BenchmarkCompileScript-18          21.6µs/op  43.0 KB/op   325 allocs/op
BenchmarkNewRuntime-18             94.9µs/op   432 KB/op  1426 allocs/op
BenchmarkNewRuntimeSmallStack-18    104µs/op   425 KB/op  1426 allocs/op
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
| Richards | 1,546 | 525 | go-quickjs 2.94x |
| DeltaBlue | 1,950 | 637 | go-quickjs 3.06x |
| Crypto | 2,699 | 329 | go-quickjs 8.20x |
| RayTrace | 4,254 | 758 | go-quickjs 5.61x |
| EarleyBoyer | 4,865 | 1,379 | go-quickjs 3.53x |
| RegExp | 3,108 | 572 | go-quickjs 5.43x |
| Splay | 6,895 | 2,459 | go-quickjs 2.80x |
| NavierStokes | 4,966 | 536 | go-quickjs 9.26x |
| **Composite score** | **3,392** | **736** | **go-quickjs 4.61x** |

The complete fresh-process run includes runtime construction, parsing,
compilation, the suite's warmups and its measured iterations:

| Metric | go-quickjs | goja | Relative result |
|---|---:|---:|---:|
| Wall time | 22.43 s | 52.56 s | go-quickjs 2.34x faster |
| Total allocation | 11.66 GiB | 31.37 GiB | go-quickjs 62.8% less |

The suite warms every workload for at least one second, then measures for at
least another second and continues until it has 32 measured iterations. Both
engines evaluated the same concatenated benchmark sources and validation code.
The measured revisions were go-quickjs `c13db5a`, goja `793a2a6`, and
AreWeFastYet `0e21608`. These results characterize this older interpreter
workload suite rather than every application; go-quickjs leads all eight of
its workloads in this comparison.

## Compared with C QuickJS

go-quickjs has the same design as QuickJS: a bytecode compiler, a
stack-based interpreter, NaN-boxed values. Running that design in Go costs
speed. The same suite ran on an AMD Ryzen 5 3600 under Windows, with Go 1.27.1,
go-quickjs `c13db5a` and QuickJS 2026-06-04. The first two columns are milliseconds for the same
fixed work (the median of three fresh-process runs, lower is better). The
scores are the suite's own (higher is better). Richards and DeltaBlue, which
take only a few milliseconds a run, ran thirty times in each process, scaled
to three.

| Workload | go-quickjs | QuickJS | QuickJS faster by | go-quickjs score | QuickJS score |
|---|---:|---:|---:|---:|---:|
| Richards | 20.8 ms | 11.1 ms | 1.9x | 508 | 952 |
| DeltaBlue | 30.5 ms | 24.1 ms | 1.27x | 653 | 842 |
| Crypto | 312 ms | 325 ms | 0.96x | 1,138 | 1,093 |
| RayTrace | 152 ms | 149 ms | 1.02x | 1,442 | 1,504 |
| EarleyBoyer | 481 ms | 404 ms | 1.19x | 1,703 | 2,169 |
| RegExp | 386 ms | 866 ms | 0.45x | 1,051 | 433 |
| Splay | 322 ms | 522 ms | 0.62x | 2,700 | 3,309 |
| NavierStokes | 244 ms | 191 ms | 1.28x | 2,007 | 2,367 |
| **Total / composite** | **1,973 ms** | **2,523 ms** | **0.78x** | **1,231** | **1,328** |

The two kinds of figure time different things, which is why go-quickjs
takes less time in total and still scores lower:

- A fixed run times each workload's setup, its runs and its teardown. The
  scored run times only the runs, after a second of warming up.
- The total adds milliseconds, so the longest workloads, RegExp and Splay,
  make most of go-quickjs's lead. The composite score is the geometric mean
  of the workloads' scores, so each counts the same: Richards' gap weighs as
  much as RegExp's lead. By that mean the fixed figures put the two
  level.
- Splay changes sides. Its fixed time is almost all setup and teardown:
  building a tree of 8,000 nodes and dropping it. That takes about 305 ms
  and 5 ms here, against 360 ms and 100 ms in QuickJS, which frees the tree
  by counting references where Go's collector frees it later. A warm run,
  which is what the score times, is splay-tree operations, calls and
  property reads. A run takes about 2.9 ms here against 2.55 ms. Giving the
  collector four times the room (`GOGC=400`) leaves Splay's score as it
  was, so collection is not what the score measures.

Figures move by a few percent between builds from code placement alone,
and Richards and DeltaBlue by more, up to a tenth. To reproduce them, run
`go run ./internal/cmd/v8bench -dir /tmp/v8-v7 -mode fixed -n 3` and the
`external` runner with `-engine qjs`.

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
  `x & 0xff`, `local * local`) reduces how many are dispatched. A function
  with few calls runs instead as trees of Go closures, a node for every few
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
    collections do. Called that way, an empty function costs about 40 ns,
    against about 33 ns in QuickJS.
- **Property access.** Objects built the same way share a shape, as in
  QuickJS and V8, and each property read and write, each getter, each
  global variable read and each `instanceof` remembers where it last found
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
  operations themselves, which the score times, take about a sixth longer
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
