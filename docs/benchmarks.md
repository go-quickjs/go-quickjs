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

Over the V8 suite as a whole, go-quickjs is ahead of QuickJS. Its
interpreter takes 0.58 times QuickJS's time and has a 24% higher composite
score: it is behind on Richards, level on DeltaBlue and ahead on the other
six. With its JIT it takes 0.33 times QuickJS's time, scores 3.5 times as
much, and is ahead on all eight. On the larger programs of Octane and Are
We Fast Yet the interpreter is behind, 1.10 and 1.08 times QuickJS's
time, and the JIT ahead, 0.60 and 0.30 times
([below](#octane-and-are-we-fast-yet)). Single operations cost less on
average, 0.90 times QuickJS's time, and 0.40 times with the JIT, as
[the micro-benchmarks](#micro-benchmarks) below show, though a few still
take about twice as long.

go-quickjs has the same design as QuickJS: a bytecode compiler, a
stack-based interpreter, NaN-boxed values. Running that design in Go costs
speed. The JIT, which is experimental and optional, compiles hot functions
to native code (see its [design notes](../internal/jit/README.md)). The
same suite ran on an AMD Ryzen 5 3600 under Windows, with Go 1.27.1,
go-quickjs `3141a88` (after v0.28.0) and QuickJS 2026-06-04. Two runs
went at once, each on the six CPUs of a level 3 cache of its own. The
JIT's column is a build with `-tags quickjs_jit` run with `-jit`. The
figures are milliseconds for the same fixed work, the median of three
fresh-process runs (lower is better):

| Workload | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| Richards | 12.7 ms | 8.1 ms | 11.4 ms | 1.11x | 0.71x |
| DeltaBlue | 24.8 ms | 21.2 ms | 24.2 ms | 1.02x | 0.88x |
| Crypto | 297 ms | 80.7 ms | 331 ms | 0.90x | 0.24x |
| RayTrace | 125 ms | 79.9 ms | 143 ms | 0.87x | 0.56x |
| EarleyBoyer | 369 ms | 174 ms | 407 ms | 0.91x | 0.43x |
| RegExp | 244 ms | 245 ms | 938 ms | 0.26x | 0.26x |
| Splay | 267 ms | 208 ms | 512 ms | 0.52x | 0.41x |
| NavierStokes | 175 ms | 41.8 ms | 189 ms | 0.92x | 0.22x |
| **Total** | **1,535 ms** | **881 ms** | **2,664 ms** | **0.58x** | **0.33x** |

The suite's own scores, the median of three (higher is better):

| Workload | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| Richards | 853 | 4,056 | 934 | 0.91x | 4.34x |
| DeltaBlue | 836 | 3,418 | 831 | 1.01x | 4.11x |
| Crypto | 1,210 | 6,336 | 1,093 | 1.11x | 5.80x |
| RayTrace | 1,841 | 6,586 | 1,567 | 1.17x | 4.20x |
| EarleyBoyer | 2,380 | 3,369 | 2,113 | 1.13x | 1.59x |
| RegExp | 1,621 | 1,529 | 425 | 3.81x | 3.60x |
| Splay | 3,366 | 5,036 | 3,325 | 1.01x | 1.51x |
| NavierStokes | 2,675 | 13,839 | 2,414 | 1.11x | 5.73x |
| **Composite** | **1,648** | **4,656** | **1,326** | **1.24x** | **3.51x** |

The two kinds of figure time different things, which is why they do not
agree on how far ahead go-quickjs is:

- A fixed run times each workload's setup, its runs and its teardown. The
  scored run times only the runs, after a second of warming up.
- The total adds milliseconds, so the longest workloads, RegExp and Splay,
  make most of the interpreter's lead. The composite score is the
  geometric mean of the workloads' scores, so each counts the same:
  Richards weighs as much as RegExp. By that mean the interpreter takes
  0.75 times QuickJS's time, and the JIT 0.41 times, where their scores
  are 24% higher and 3.5 times as high.
- The JIT's lead is much larger in the scores than in fixed time. A fixed
  run of Richards or DeltaBlue lasts a few milliseconds, and a function
  runs in the interpreter until it is hot enough to compile, so much of
  that run is spent before the JIT's code takes over. The scores time
  warm runs, where Richards, DeltaBlue, Crypto and NavierStokes score
  4.1 to 5.2 times what the interpreter does.
- Richards is behind by both measures, 11% in fixed time and 9% in its
  score. This page had it level at v0.27.0, but that came from the
  clocks. The runners then timed go-quickjs with the clock Windows gives
  Go, which moves only every 0.5 ms, or every 15.6 ms while no program
  asks for more, and QuickJS with `Date.now`, which counts whole
  milliseconds. Both now read the performance counter (`ab0ef0b`), and
  timed that way v0.27.0 takes 13.0 ms on Richards, as main does.
- Splay's lead is in its fixed time. That is almost all setup and
  teardown: building a tree of 8,000 nodes and dropping it, which QuickJS
  frees by counting references where Go's collector frees it later. A
  warm run, which is what the score times, is splay-tree operations, calls
  and property reads, and is level with QuickJS's. Giving the collector
  four times the room (`GOGC=400`) leaves Splay's score as it was, so
  collection is not what the score measures.
- The JIT changes little where the time is spent in Go code it calls
  rather than in the script's own: RegExp's matching, whose score is the
  interpreter's, and the allocation-heavy EarleyBoyer and Splay, about
  1.5 times.

Figures move by a few percent between builds from code placement alone,
and Richards and DeltaBlue by more, up to a tenth. To reproduce them, the
suites below and the micro-benchmarks, run `go run
./internal/cmd/v8bench/matrix -engines gq,gqjit,qjs -qjs /path/to/qjs -v8
/tmp/v8-v7 -octane /tmp/octane -awfy /tmp/awfy -fetch`.

### Octane and Are We Fast Yet

Octane 2.0 is the V8 suite's benchmarks revised, with larger programs
added: TypeScript's compiler, pdf.js, a Game Boy emulator, Box2D, zlib
and Mandreel, the last two compiled from C and C++. Are We Fast Yet's
fourteen benchmarks are written to compare language implementations
alike, each in the same style in every language. Both reach far more of
the language and of the JIT than the V8 suite does. They ran in the same
matrix, the same way: milliseconds for the fixed work, the median of
three fresh-process runs (lower is better).

| Octane | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| Richards | 4.6 ms | 4.4 ms | 3.9 ms | 1.18x | 1.13x |
| DeltaBlue | 8.3 ms | 9.3 ms | 7.8 ms | 1.06x | 1.19x |
| Crypto | 99.9 ms | 28.6 ms | 111 ms | 0.90x | 0.26x |
| RayTrace | 40.2 ms | 46.1 ms | 50.0 ms | 0.80x | 0.92x |
| EarleyBoyer | 124 ms | 59.2 ms | 142 ms | 0.87x | 0.42x |
| RegExp | 183 ms | 208 ms | 470 ms | 0.39x | 0.44x |
| Splay | 236 ms | 186 ms | 465 ms | 0.51x | 0.40x |
| NavierStokes | 64.9 ms | 19.6 ms | 67.1 ms | 0.97x | 0.29x |
| PdfJS | 299 ms | 300 ms | 380 ms | 0.79x | 0.79x |
| Mandreel | 4,243 ms | 1,955 ms | 3,429 ms | 1.24x | 0.57x |
| Gameboy | 503 ms | 338 ms | 472 ms | 1.06x | 0.71x |
| CodeLoad | 15.7 ms | 16.9 ms | 13.1 ms | 1.20x | 1.29x |
| Box2D | 147 ms | 146 ms | 156 ms | 0.94x | 0.93x |
| zlib | 9,508 ms | 3,422 ms | 8,452 ms | 1.12x | 0.40x |
| Typescript | 2,582 ms | 2,880 ms | 2,157 ms | 1.20x | 1.34x |
| **Total** | **18,713 ms** | **10,151 ms** | **16,938 ms** | **1.10x** | **0.60x** |

| Are We Fast Yet | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| DeltaBlue | 966 ms | 418 ms | 752 ms | 1.29x | 0.56x |
| Richards | 5,563 ms | 1,024 ms | 4,869 ms | 1.14x | 0.21x |
| Json | 1,437 ms | 398 ms | 1,358 ms | 1.06x | 0.29x |
| CD | 2,781 ms | 804 ms | 2,867 ms | 0.97x | 0.28x |
| Havlak | 5,572 ms | 2,447 ms | 4,519 ms | 1.23x | 0.54x |
| Bounce | 1,376 ms | 302 ms | 1,516 ms | 0.91x | 0.20x |
| List | 1,397 ms | 275 ms | 1,274 ms | 1.10x | 0.22x |
| Mandelbrot | 394 ms | 45.2 ms | 690 ms | 0.57x | 0.07x |
| NBody | 1,029 ms | 196 ms | 1,018 ms | 1.01x | 0.19x |
| Permute | 1,774 ms | 257 ms | 1,685 ms | 1.05x | 0.15x |
| Queens | 1,174 ms | 148 ms | 1,064 ms | 1.10x | 0.14x |
| Sieve | 1,116 ms | 321 ms | 1,221 ms | 0.91x | 0.26x |
| Storage | 1,821 ms | 805 ms | 1,642 ms | 1.11x | 0.49x |
| Towers | 1,727 ms | 408 ms | 1,564 ms | 1.10x | 0.26x |
| **Total** | **28,162 ms** | **7,887 ms** | **26,094 ms** | **1.08x** | **0.30x** |

- Without the JIT, go-quickjs leads where the V8 suite has it lead --
  RegExp, Splay, the arithmetic of Crypto and NavierStokes, Mandelbrot's
  loops -- and is behind on code that is mostly calls and allocation, by
  as much as 29% (Are We Fast Yet's DeltaBlue) and 23% (Havlak). Of
  Octane's long programs, Mandreel's and zlib's code compiled from C and
  C++, Typescript's compiler and CodeLoad's parsing are 12 to 24% behind,
  and pdf.js is 21% ahead.
- With the JIT, Are We Fast Yet takes 0.07 to 0.56 times QuickJS's time,
  its loops of arithmetic the least, its programs that allocate most --
  DeltaBlue, Havlak, Storage -- the most.
- On seven of Octane's fifteen the JIT is level with the interpreter or
  behind it: level on PdfJS and Box2D, behind on RayTrace (15%), RegExp
  (14%), DeltaBlue and Typescript (12%) and CodeLoad (8%). Octane's fixed
  work is one run of each benchmark, so compiling counts, and Typescript
  compiles its code about four times a function, a third of them code
  dropped for room or flushed and compiled again. Why RayTrace and RegExp
  take longer is not yet explained.

### Micro-benchmarks

QuickJS's own micro-benchmarks, [`tests/microbench.js`](https://github.com/bellard/quickjs/blob/master/tests/microbench.js),
time one operation each, in nanoseconds. Each test was given the work QuickJS
takes about 150 ms for, all three ran that same work in fresh processes at
`3141a88`, and the figure is the fastest of three runs. The runners'
micro mode does this, from a copy of the file kept in `internal/v8bench`, and
the matrix above runs it. `sort_bench` is left out: it reports a run's time
over its fastest sort, not the time of an operation. Over the other 71 tests
go-quickjs takes 0.90 times QuickJS's time by the geometric mean: it is
slower on 19, faster on 35, and within 5% on the rest. With the JIT it
takes 0.40 times QuickJS's time, slower on 6 and faster on 61. The table
has the interpreter's largest gaps either way:

| Test | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| `array_prop_create` | 38.3 ns | 16.5 ns | 15.2 ns | 2.52x | 1.08x |
| `weak_map_delete` | 523 ns | 474 ns | 252 ns | 2.07x | 1.88x |
| `global_read` | 10.1 ns | 1.74 ns | 5.67 ns | 1.78x | 0.31x |
| `weak_map_set` | 143 ns | 111 ns | 94.3 ns | 1.51x | 1.18x |
| `array_read` | 9.47 ns | 1.54 ns | 6.67 ns | 1.42x | 0.23x |
| `typed_array_read` | 13.7 ns | 2.37 ns | 9.88 ns | 1.38x | 0.24x |
| `global_write` | 10.5 ns | 2.79 ns | 7.74 ns | 1.36x | 0.36x |
| `empty_do_loop` | 7.18 ns | 1.83 ns | 5.48 ns | 1.31x | 0.33x |
| `int_to_string` | 63.4 ns | 54.3 ns | 49.8 ns | 1.27x | 1.09x |
| `date_parse` | 999 ns | 986 ns | 845 ns | 1.18x | 1.17x |
| `string_length` | 8.08 ns | 1.06 ns | 14.4 ns | 0.56x | 0.07x |
| `array_for_of` | 20.9 ns | 7.61 ns | 46.6 ns | 0.45x | 0.16x |
| `string_to_int` | 49.3 ns | 41.7 ns | 114 ns | 0.43x | 0.37x |
| `prop_clone` | 31.0 ns | 32.2 ns | 72.1 ns | 0.43x | 0.45x |
| `array_pop` | 47.9 ns | 21.6 ns | 112 ns | 0.43x | 0.19x |
| `array_slice` | 11.5 ns | 12.2 ns | 27.5 ns | 0.42x | 0.44x |
| `string_build2c` | 58.9 ns | 45.4 ns | 150 ns | 0.39x | 0.30x |

The JIT does the most for a test that is a loop of arithmetic, locals,
property reads and writes, array elements and calls: `prop_read` takes
0.84 ns with it, `func_call` 1.79 ns, a tenth of either interpreter's
time or less. A test whose time is a built-in function's work runs much
as it does in the interpreter: the weak collections, regular expressions,
`prop_clone`, `array_slice`, and the conversions between numbers and
strings. Only `float_to_string` takes longer with the JIT than without
it by 7% or more, and, of the extra tests below, `async_await`,
`regexp_unicode` and `regexp_search_utf16`.

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
calls in tail position, method calls with a frame and without, loops of a
few simple instructions, private class members, `Math.random`, the `in`
operator, `delete`, the array methods that take a callback, `indexOf`,
String's methods that run no script, Math's imul, clz32, pow, atan2 and
hypot, a template and a long chain of calls joined, BigInts' division, shifts,
hashing, powers and decimal strings, and regular expressions as programs use
them: validating, tokenizing, replacing, splitting, named groups and the u
flag. Over the
81 QuickJS can run (it has no using declarations) go-quickjs takes 0.81
times its time, and 0.40 times with the JIT. An await costs much less,
resumed without allocating (`3af4f32`), as does an async generator, and
regular expressions that search a text or split it much less again. A
generator costs more to make and to resume from for-of: two frames a
step, and its state set up at each resumption. So does a call between two
functions in tail position, which goes through the pending tail call and
its checks, and a global or sticky exec, which makes a result and a
String for each match. The largest gaps either way:

| Test | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| `generator_for_of` | 105 ns | 25.6 ns | 57.3 ns | 1.83x | 0.45x |
| `tail_mutual` | 65.5 ns | 9.22 ns | 36.7 ns | 1.78x | 0.25x |
| `regexp_sticky` | 372 ns | 385 ns | 253 ns | 1.47x | 1.52x |
| `generator_create` | 590 ns | 587 ns | 403 ns | 1.46x | 1.46x |
| `regexp_exec_g` | 384 ns | 404 ns | 268 ns | 1.43x | 1.51x |
| `typed_forEach` | 54.8 ns | 31.5 ns | 39.3 ns | 1.39x | 0.80x |
| `string_starts_with` | 97.4 ns | 84.8 ns | 70.6 ns | 1.38x | 1.20x |
| `regexp_replace_dollar` | 1,306 ns | 1,310 ns | 970 ns | 1.35x | 1.35x |
| `try_catch` | 17.2 ns | 4.44 ns | 13.3 ns | 1.29x | 0.33x |
| `tail_self` | 44.1 ns | 11.0 ns | 36.3 ns | 1.21x | 0.30x |
| `prop_in` | 17.5 ns | 5.51 ns | 36.0 ns | 0.49x | 0.15x |
| `array_indexOf` | 133 ns | 116 ns | 275 ns | 0.48x | 0.42x |
| `loop_sum` | 8.76 ns | 1.35 ns | 19.1 ns | 0.46x | 0.07x |
| `loop_arith` | 24.9 ns | 3.27 ns | 56.2 ns | 0.44x | 0.06x |
| `async_generator` | 638 ns | 655 ns | 1,947 ns | 0.33x | 0.34x |
| `regexp_split` | 1,161 ns | 1,200 ns | 5,737 ns | 0.20x | 0.21x |
| `async_await` | 99.6 ns | 120 ns | 754 ns | 0.13x | 0.16x |
| `regexp_search_i` | 1,839 ns | 1,866 ns | 20,877 ns | 0.09x | 0.09x |
| `regexp_search_utf16` | 533 ns | 592 ns | 13,098 ns | 0.04x | 0.05x |

The BigInt tests were the largest gaps, 9.65 and 5.56 times QuickJS's time
at `608415b`, where every BigInt was an allocation. A BigInt in the int64
range is now held in the value itself (`9746906`), as QuickJS holds one
that fits in a word, and so is one below 2^64 in magnitude (`f8c3229`);
a larger one is a block of no pointers, made by the engine's own
arithmetic (`f1bf681`). `bigint64_arith` takes 0.76 times QuickJS's time
and `bigint256_arith` 1.18 times, and the extra BigInt tests 0.73 to 1.11
times. The JIT makes BigInts of 64 bits natively: `bigint64_arith` takes
0.12 times QuickJS's time with it.

The largest gaps now are:

- `array_prop_create`, an array made by assigning past its end, whose
  storage grows as it goes and leaves what it outgrew to the collector;
- `generator_for_of` and `generator_create`, for the frames of each step
  and the state a generator sets up at each resumption, and the two
  allocations a generator is made of;
- `tail_mutual`, each call going through the pending tail call;
- `global_read` and `global_write`, a global variable's cache checked in a
  call;
- the weak collections. A WeakMap holds each value on its key rather than
  in a table of its own (`2bc1620`), so that a value which refers to its
  key does not keep the key alive, as the specification requires; adding
  an entry costs more for it.

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
  geometric mean), `empty_loop` and the extra tests `loop_sum`,
  `loop_arith`, `loop_locals`, `loop_elements` and `loop_props` run with
  the tree tier off, `QJS_NOTREE=1`. Fusing common instruction sequences
  into single instructions (`a[i]`, `a[++i]`, `x & 0xff`,
  `local * local`) reduces how many are dispatched. Nearly
  every function runs instead as trees of Go closures, a node for every few
  instructions with no operand stack between them, and with locals and
  constants read in place (see the [design notes](design.md)). That is what
  closed the gap on Crypto and most of it on NavierStokes, which do little but
  arithmetic, local variables and array elements. A tree's loops run as Go
  loops, rebuilt from its blocks, with the comparison that ends a loop and
  the `i++` that steps it made in place: the tests that are mostly a loop --
  `empty_loop` (1.00), `int_arith` (0.97), `float_arith` (0.75),
  `array_update` -- are level with QuickJS or ahead of it, and the six
  loops above take 0.44 to 1.16 times QuickJS's time as trees. A loop whose body
  would need its branches chosen by a structure of their own, an `&&` or an
  else-if chain, still runs from block to block, which costs less.
- **Calls.** A call from JavaScript to JavaScript fills in a frame record of
  some fifteen fields, and each pointer it stores pays a write barrier's
  check. Even so a call costs about what QuickJS's does: a method call
  whose body needs a frame -- one with a loop, even an empty one -- about
  70 ns, as in QuickJS (`method_call`), and a method that only returns
  its argument about 37 ns, against 43 (`method_call_leaf`). With the JIT
  they take 13.2 and 3.7 ns. Richards, DeltaBlue and
  EarleyBoyer are mostly small calls. Two kinds of function are answered
  without a frame, where nothing they do could call code or throw:
  - one whose whole body is `return this.x`, `return this.x.length`,
    `return this.x[i]`, or stores of its parameters in `this`, which is much
    of EarleyBoyer's constructions;
  - one that reads and computes, and may store to a property once as the
    last thing it can fail at, as `isHeldOrSuspended`, DeltaBlue's `input`,
    `output` and `execute` and RayTrace's `dot` do. It may call another such
    function, `new` a constructor of the first kind, as EarleyBoyer's
    `sc_cons` does, and push onto or pop from an array, as DeltaBlue's
    collections do. Called that way, an empty function costs about 31 ns,
    as in QuickJS (`func_call`). One of seven
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
  the key checked once rather than twice (`array_update`, 0.65 times
  QuickJS's time). for-in over an array whose keys are its elements walks
  them by index rather than copying them into a list first
  (`array_for_in`, 0.99), and the array methods pass over a hole
  without looking its index up, where nothing up the prototype chain could
  have it (`array_hole_length_decr`, 0.89). A typed array of a
  fixed length is written through the storage it keeps
  (`typed_array_write`, 0.93).
- **`apply(this, arguments)`.** In a function whose only use of `arguments` is
  passing it to `apply` (a common way to write a class constructor), the
  arguments object is never made. A constructor whose whole body is
  `this.initialize.apply(this, arguments)`, as RayTrace's are, calls the
  method directly, with the frames a stack trace would show standing in for
  its body.

What remains in the interpreter is mostly the cost of a Go indirect call,
which every instruction or tree node pays, and of the operations the gaps
above are in. The ways around the first which an interpreter written in C
has are not open to one written in Go, as measured here:
- Running calls in the same loop, rather than a Go call each, made the loop
  slower than the calls it saved.
- Turning each instruction into a Go closure, instead of a case of the switch,
  came to little more than 10% on the loop best suited to it.
- Building each expression into a tree of closures that reads its locals
  and constants in place nearly halved the time Crypto and NavierStokes
  take, but did not make a call cheaper: entering a tree costs what entering
  the interpreter does.

The JIT is the way past it: a hot function compiled to native code
dispatches nothing, which is why, with it, the loops and calls above take
from about a third of the interpreter's time down to a twentieth.

[goja]: https://github.com/dop251/goja
[v8-v7]: https://github.com/mozilla/arewefastyet/tree/master/benchmarks/v8-v7
