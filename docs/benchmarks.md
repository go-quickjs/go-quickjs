# Benchmarks

On an Apple M5 Max, taking the median of five runs:

```
BenchmarkEvalArithmetic-18  1.22µs/op   4040 B/op    23 allocs/op
BenchmarkFibonacci-18        664µs/op     16 B/op     1 allocs/op
BenchmarkPropertyAccess-18  1.51µs/op   4952 B/op    30 allocs/op
BenchmarkCallGoFunction-18  1.51µs/op   5840 B/op    33 allocs/op
```

`fib(20)` costs one allocation because the interpreter loop allocates nothing
per call; the other benchmarks include parsing and compiling their source each
iteration.

`go test -bench . -benchmem` also runs compile-once interpreter workloads,
plus compilation and runtime-construction benchmarks. That complete set is:

```
BenchmarkLoopArithmetic-18          150µs/op        0 B/op     0 allocs/op
BenchmarkDateUTC-18                 490µs/op        0 B/op     0 allocs/op
BenchmarkLoopPropertyAccess-18      422µs/op        0 B/op     0 allocs/op
BenchmarkLoopArrayIndex-18         26.2µs/op        0 B/op     0 allocs/op
BenchmarkLoopFunctionCall-18        333µs/op        0 B/op     0 allocs/op
BenchmarkLoopMethodCall-18          530µs/op        0 B/op     0 allocs/op
BenchmarkAllocObjects-18            129µs/op   320 KB/op  2000 allocs/op
BenchmarkAllocArrays-18            84.0µs/op   320 KB/op  2000 allocs/op
BenchmarkStringConcat-18           63.5µs/op   161 KB/op  2029 allocs/op
BenchmarkClosureCreateAndCall-18    493µs/op  2.08 MB/op  5001 allocs/op
BenchmarkArrayCallbacks-18          106µs/op  36.9 KB/op    24 allocs/op
BenchmarkMapOperations-18           403µs/op   633 KB/op    46 allocs/op
BenchmarkRegExpExec-18              551µs/op   496 KB/op  4000 allocs/op
BenchmarkThrowCatch-18              281µs/op   768 KB/op  6000 allocs/op
BenchmarkJSONRoundTrip-18           485µs/op   764 KB/op 10000 allocs/op
BenchmarkCompileScript-18          20.2µs/op  38.5 KB/op   279 allocs/op
BenchmarkNewRuntime-18             99.2µs/op   407 KB/op  1243 allocs/op
BenchmarkNewRuntimeSmallStack-18   89.9µs/op   400 KB/op  1243 allocs/op
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
| Richards | 1,234 | 512 | go-quickjs 2.41x |
| DeltaBlue | 1,650 | 611 | go-quickjs 2.70x |
| Crypto | 1,882 | 329 | go-quickjs 5.72x |
| RayTrace | 4,197 | 786 | go-quickjs 5.34x |
| EarleyBoyer | 4,263 | 1,441 | go-quickjs 2.96x |
| RegExp | 3,157 | 582 | go-quickjs 5.42x |
| Splay | 5,827 | 2,540 | go-quickjs 2.29x |
| NavierStokes | 3,407 | 529 | go-quickjs 6.44x |
| **Composite score** | **2,866** | **743** | **go-quickjs 3.86x** |

The complete fresh-process run includes runtime construction, parsing,
compilation, the suite's warmups and its measured iterations:

| Metric | go-quickjs | goja | Relative result |
|---|---:|---:|---:|
| Wall time | 24.42 s | 51.80 s | go-quickjs 2.12x faster |
| Total allocation | 11.28 GiB | 31.49 GiB | go-quickjs 64.2% less |

The suite warms every workload for at least one second, then measures for at
least another second and continues until it has 32 measured iterations. Both
engines evaluated the same concatenated benchmark sources and validation code.
The measured revisions were go-quickjs `1893261`, goja `793a2a6`, and
AreWeFastYet `0e21608`. These results characterize this older interpreter
workload suite rather than every application; go-quickjs leads all eight of
its workloads in this comparison.

## Compared with C QuickJS

go-quickjs has the same design as QuickJS: a bytecode compiler, a
stack-based interpreter, NaN-boxed values. Running that design in Go costs
speed. The same suite ran on an AMD Ryzen 5 3600 under Windows, with Go 1.27.1
and QuickJS 2026-06-04. The first two columns are milliseconds for the same
fixed work (the median of three fresh-process runs, lower is better). The
scores are the suite's own (higher is better).

| Workload | go-quickjs | QuickJS | QuickJS faster by | go-quickjs score | QuickJS score |
|---|---:|---:|---:|---:|---:|
| Richards | 25.5 ms | 11.0 ms | 2.3x | 410 | 960 |
| DeltaBlue | 33.4 ms | 24.0 ms | 1.4x | 586 | 838 |
| Crypto | 575 ms | 327 ms | 1.8x | 622 | 1,103 |
| RayTrace | 171 ms | 149 ms | 1.1x | 1,309 | 1,570 |
| EarleyBoyer | 576 ms | 417 ms | 1.4x | 1,479 | 2,092 |
| RegExp | 395 ms | 864 ms | 0.46x | 1,048 | 433 |
| Splay | 375 ms | 522 ms | 0.72x | 2,271 | 3,121 |
| NavierStokes | 435 ms | 194 ms | 2.2x | 1,128 | 2,374 |
| **Total / composite** | **2,629 ms** | **2,530 ms** | **1.04x** | **969** | **1,322** |

The scored run weighs more heavily than the fixed one what a long-running
program pays for its heap. Each workload is warmed for a second and then
measured for another. That is why Splay and EarleyBoyer, which allocate the
most, fare worse there. Figures move by a few percent between builds from
code placement alone. To reproduce them, run
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

  An instruction costs about twice what it costs QuickJS. That is most of the
  gap in Crypto and NavierStokes, which do little but arithmetic, local
  variables and array elements. Fusing common instruction sequences into
  single instructions (`a[i]`, `a[++i]`, `x & 0xff`, `local * local`) reduces
  how many are dispatched.
- **Calls.** A call from JavaScript to JavaScript fills in a frame record of
  some twenty fields, and each pointer it stores pays a write barrier's
  check. An empty call costs about 47 ns here, against about 25 ns in
  QuickJS. Richards and DeltaBlue are mostly small method calls. A function
  whose whole body is `return this.x`, `return this.x.length`,
  `return this.x[i]`, or stores of its parameters in `this` is answered
  without a frame, where nothing it does could call code or throw; that is
  much of DeltaBlue's calls and EarleyBoyer's constructions.
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
  time than CPU time. A long-running program that allocates heavily pays for
  it all the same. The scored Splay and EarleyBoyer show this.
- **Bounds checks.** Go checks every slice index, and the interpreter's stack,
  locals and array elements are all slices.

Where go-quickjs is level or ahead:

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
- **Splay, in fixed work.** Go's allocator makes many short-lived objects
  cheaply.
- **`apply(this, arguments)`.** In a function whose only use of `arguments` is
  passing it to `apply` (a common way to write a class constructor), the
  arguments object is never made. A constructor whose whole body is
  `this.initialize.apply(this, arguments)`, as RayTrace's are, calls the
  method directly, with the frames a stack trace would show standing in for
  its body.

Most of what remains is the interpreter itself: the cost of dispatching an
instruction and of a call, which is most of the gap in Crypto, NavierStokes
and Richards. The ways around that which an engine in C or with a JIT has are
not open to one in Go, as measured here: running calls in the same loop
rather than a Go call each made the loop slower than the calls it saved, and
turning each instruction into a Go closure instead of a case of the switch
came to little more than 10% on the loop best suited to it.

[goja]: https://github.com/dop251/goja
[v8-v7]: https://github.com/mozilla/arewefastyet/tree/master/benchmarks/v8-v7
