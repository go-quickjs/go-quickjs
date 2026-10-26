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
interpreter takes 0.59 times QuickJS's time and has a 24% higher composite
score: it is behind on Richards, level on DeltaBlue and ahead on the other
six. With its JIT it takes 0.33 times QuickJS's time, scores 3.8 times as
much, and is ahead on all eight. On the larger programs of Octane and Are
We Fast Yet the interpreter is behind, 1.14 and 1.09 times QuickJS's
time, and the JIT ahead, 0.56 and 0.27 times
([below](#octane-and-are-we-fast-yet)). Single operations cost less on
average, 0.90 times QuickJS's time, and 0.36 times with the JIT, as
[the micro-benchmarks](#micro-benchmarks) below show, though a few still
take about twice as long.

go-quickjs has the same design as QuickJS: a bytecode compiler, a
stack-based interpreter, NaN-boxed values. Running that design in Go costs
speed. The JIT, which is experimental and optional, compiles hot functions
to native code (see its [design notes](../internal/jit/README.md)). The
same suite ran on an AMD Ryzen 5 3600 under Windows, with Go 1.27.1,
go-quickjs `3714c86` (after v0.28.0) and QuickJS 2026-06-04. Two runs
went at once, each on the six CPUs of a level 3 cache of its own. The
JIT's column is a build with `-tags quickjs_jit` run with `-jit`. The
figures are milliseconds for the same fixed work, the median of three
fresh-process runs (lower is better):

| Workload | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| Richards | 13.6 ms | 7.5 ms | 11.2 ms | 1.21x | 0.67x |
| DeltaBlue | 24.5 ms | 22.6 ms | 23.7 ms | 1.03x | 0.95x |
| Crypto | 297 ms | 77.3 ms | 331 ms | 0.90x | 0.23x |
| RayTrace | 123 ms | 76.6 ms | 142 ms | 0.86x | 0.54x |
| EarleyBoyer | 360 ms | 161 ms | 400 ms | 0.90x | 0.40x |
| RegExp | 242 ms | 239 ms | 955 ms | 0.25x | 0.25x |
| Splay | 266 ms | 197 ms | 503 ms | 0.53x | 0.39x |
| NavierStokes | 175 ms | 40.6 ms | 190 ms | 0.92x | 0.21x |
| **Total** | **1,535 ms** | **849 ms** | **2,580 ms** | **0.59x** | **0.33x** |

The suite's own scores, the median of three (higher is better):

| Workload | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| Richards | 838 | 4,250 | 950 | 0.88x | 4.47x |
| DeltaBlue | 825 | 4,522 | 836 | 0.99x | 5.41x |
| Crypto | 1,209 | 6,717 | 1,091 | 1.11x | 6.16x |
| RayTrace | 1,826 | 9,231 | 1,589 | 1.15x | 5.81x |
| EarleyBoyer | 2,398 | 3,471 | 2,159 | 1.11x | 1.61x |
| RegExp | 1,628 | 1,637 | 387 | 4.21x | 4.23x |
| Splay | 3,383 | 5,006 | 3,309 | 1.02x | 1.51x |
| NavierStokes | 2,672 | 14,589 | 2,415 | 1.11x | 6.04x |
| **Composite** | **1,636** | **5,019** | **1,318** | **1.24x** | **3.81x** |

The two kinds of figure time different things, which is why they do not
agree on how far ahead go-quickjs is:

- A fixed run times each workload's setup, its runs and its teardown. The
  scored run times only the runs, after a second of warming up.
- The total adds milliseconds, so the longest workloads, RegExp and Splay,
  make most of the interpreter's lead. The composite score is the
  geometric mean of the workloads' scores, so each counts the same:
  Richards weighs as much as RegExp. By that mean the interpreter takes
  0.76 times QuickJS's time, and the JIT 0.40 times, where their scores
  are 24% higher and 3.8 times as high.
- The JIT's lead is much larger in the scores than in fixed time. A fixed
  run of Richards or DeltaBlue lasts a few milliseconds, and a function
  runs in the interpreter until it is hot enough to compile, so much of
  that run is spent before the JIT's code takes over. The scores time
  warm runs, where Richards, DeltaBlue, Crypto, RayTrace and
  NavierStokes score 5.1 to 5.6 times what the interpreter does.
- Richards is behind by both measures, 21% in fixed time and 12% in its
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
and Richards, DeltaBlue and Crypto by more, up to a tenth. A build of
v0.28.0 and one of `3714c86`, made in two checkouts, had the
interpreter's Crypto 20% slower on main while another process ran; over
eight placements of each, made in one checkout, main took 2.6% longer
(96.1 against 93.7 ms a run), level at five of the eight. The extra time
is in `elemAt`, whose instructions are the same in both builds; only its
place in the op cache differs, and running beside another process makes
a bad place worse. To reproduce them, the
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
| Richards | 4.8 ms | 4.6 ms | 4.0 ms | 1.20x | 1.15x |
| DeltaBlue | 8.5 ms | 10.5 ms | 7.9 ms | 1.08x | 1.33x |
| Crypto | 100 ms | 28.4 ms | 110 ms | 0.91x | 0.26x |
| RayTrace | 43.5 ms | 45.6 ms | 49.8 ms | 0.87x | 0.92x |
| EarleyBoyer | 133 ms | 58.7 ms | 141 ms | 0.94x | 0.42x |
| RegExp | 174 ms | 203 ms | 517 ms | 0.34x | 0.39x |
| Splay | 227 ms | 172 ms | 466 ms | 0.49x | 0.37x |
| NavierStokes | 64.5 ms | 19.0 ms | 65.4 ms | 0.99x | 0.29x |
| PdfJS | 313 ms | 288 ms | 381 ms | 0.82x | 0.76x |
| Mandreel | 4,319 ms | 1,819 ms | 3,450 ms | 1.25x | 0.53x |
| Gameboy | 497 ms | 326 ms | 473 ms | 1.05x | 0.69x |
| CodeLoad | 15.8 ms | 15.8 ms | 13.1 ms | 1.21x | 1.21x |
| Box2D | 146 ms | 137 ms | 155 ms | 0.94x | 0.88x |
| zlib | 10,231 ms | 3,308 ms | 8,506 ms | 1.20x | 0.39x |
| Typescript | 2,655 ms | 2,688 ms | 2,179 ms | 1.22x | 1.23x |
| **Total** | **19,532 ms** | **9,635 ms** | **17,173 ms** | **1.14x** | **0.56x** |

| Are We Fast Yet | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| DeltaBlue | 955 ms | 406 ms | 710 ms | 1.35x | 0.57x |
| Richards | 5,485 ms | 878 ms | 4,767 ms | 1.15x | 0.18x |
| Json | 1,449 ms | 355 ms | 1,352 ms | 1.07x | 0.26x |
| CD | 2,816 ms | 688 ms | 2,826 ms | 1.00x | 0.24x |
| Havlak | 5,472 ms | 2,324 ms | 4,378 ms | 1.25x | 0.53x |
| Bounce | 1,360 ms | 274 ms | 1,487 ms | 0.91x | 0.18x |
| List | 1,405 ms | 206 ms | 1,259 ms | 1.12x | 0.16x |
| Mandelbrot | 403 ms | 42.1 ms | 685 ms | 0.59x | 0.06x |
| NBody | 1,032 ms | 128 ms | 1,030 ms | 1.00x | 0.12x |
| Permute | 1,799 ms | 241 ms | 1,696 ms | 1.06x | 0.14x |
| Queens | 1,179 ms | 126 ms | 1,057 ms | 1.12x | 0.12x |
| Sieve | 1,137 ms | 273 ms | 1,210 ms | 0.94x | 0.23x |
| Storage | 1,833 ms | 794 ms | 1,654 ms | 1.11x | 0.48x |
| Towers | 1,758 ms | 257 ms | 1,576 ms | 1.12x | 0.16x |
| **Total** | **28,114 ms** | **7,031 ms** | **25,694 ms** | **1.09x** | **0.27x** |

- Without the JIT, go-quickjs leads where the V8 suite has it lead --
  RegExp, Splay, the arithmetic of Crypto and NavierStokes, Mandelbrot's
  loops -- and is behind on code that is mostly calls and allocation, by
  as much as 35% (Are We Fast Yet's DeltaBlue) and 25% (Havlak). Of
  Octane's long programs, Mandreel's and zlib's code compiled from C and
  C++, Typescript's compiler and CodeLoad's parsing are 20 to 25% behind,
  and pdf.js is 18% ahead.
- With the JIT, Are We Fast Yet takes 0.06 to 0.57 times QuickJS's time,
  its loops of arithmetic the least, its programs that allocate most --
  DeltaBlue, Havlak, Storage -- the most.
- On six of Octane's fifteen the JIT is level with the interpreter or
  behind it: level on Richards, CodeLoad and Typescript, behind on
  DeltaBlue (24%), RegExp (17%) and RayTrace (5%). Octane's fixed work is
  one run of each benchmark, so compiling counts: DeltaBlue's 16 compiles
  take about 2.5 ms of its 8.5, and RegExp's run blocks were compiled
  two and three times, their calls of a captured closure learned at
  exits (since `f877a6e`, once).

### Micro-benchmarks

QuickJS's own micro-benchmarks, [`tests/microbench.js`](https://github.com/bellard/quickjs/blob/master/tests/microbench.js),
time one operation each, in nanoseconds. Each test was given the work QuickJS
takes about 150 ms for, all three ran that same work in fresh processes at
`3714c86`, and the figure is the fastest of three runs. The runners'
micro mode does this, from a copy of the file kept in `internal/v8bench`, and
the matrix above runs it. `sort_bench` is left out: it reports a run's time
over its fastest sort, not the time of an operation. Over the other 71 tests
go-quickjs takes 0.90 times QuickJS's time by the geometric mean: it is
slower on 19, faster on 36, and within 5% on the rest. With the JIT it
takes 0.36 times QuickJS's time, slower on 5 and faster on 61. The table
has the interpreter's largest gaps either way:

| Test | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| `array_prop_create` | 38.2 ns | 16.4 ns | 14.9 ns | 2.57x | 1.11x |
| `weak_map_delete` | 525 ns | 472 ns | 251 ns | 2.09x | 1.88x |
| `global_read` | 10.0 ns | 1.67 ns | 5.59 ns | 1.79x | 0.30x |
| `weak_map_set` | 143 ns | 111 ns | 93.9 ns | 1.52x | 1.18x |
| `array_read` | 9.48 ns | 1.53 ns | 6.62 ns | 1.43x | 0.23x |
| `typed_array_read` | 13.8 ns | 2.39 ns | 9.87 ns | 1.39x | 0.24x |
| `global_write_strict` | 10.5 ns | 2.60 ns | 7.60 ns | 1.38x | 0.34x |
| `global_write` | 10.5 ns | 2.57 ns | 7.66 ns | 1.37x | 0.34x |
| `empty_do_loop` | 7.19 ns | 0.81 ns | 5.38 ns | 1.33x | 0.15x |
| `int_to_string` | 60.7 ns | 51.0 ns | 48.8 ns | 1.24x | 1.05x |
| `prop_update` | 6.57 ns | 1.91 ns | 11.7 ns | 0.56x | 0.16x |
| `array_for_of` | 20.8 ns | 7.17 ns | 46.0 ns | 0.45x | 0.16x |
| `array_slice` | 11.9 ns | 11.9 ns | 27.1 ns | 0.44x | 0.44x |
| `string_to_int` | 46.9 ns | 43.1 ns | 112 ns | 0.42x | 0.38x |
| `array_pop` | 46.4 ns | 19.7 ns | 111 ns | 0.42x | 0.18x |
| `prop_clone` | 31.3 ns | 10.6 ns | 76.7 ns | 0.41x | 0.14x |
| `string_build2c` | 56.2 ns | 52.4 ns | 147 ns | 0.38x | 0.36x |

The JIT does the most for a test that is a loop of arithmetic, locals,
property reads and writes, array elements and calls: `prop_read` takes
0.79 ns with it, `func_call` 1.20 ns, a tenth of either interpreter's
time or less. A test whose time is a built-in function's work runs much
as it does in the interpreter: regular expressions, `array_slice`, and
the conversions between numbers and strings. None of these tests takes
7% or more longer with the JIT than without it in both of the two matrix
runs made for this page; of the extra tests below, `regexp_fresh`,
`regexp_search_i` and `regexp_search_utf16` do.

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
87 QuickJS can run (it has no using declarations) go-quickjs takes 0.81
times its time, and 0.37 times with the JIT. An await costs much less,
resumed without allocating (`3af4f32`), as does an async generator, and
regular expressions that search a text or split it much less again. A
generator costs more to make and to resume from for-of: two frames a
step, and its state set up at each resumption. So does a call between two
functions in tail position, which goes through the pending tail call and
its checks, and a global or sticky exec, which makes a result and a
String for each match. The largest gaps either way:

| Test | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| `tail_mutual` | 65.2 ns | 9.06 ns | 35.2 ns | 1.85x | 0.26x |
| `generator_for_of` | 95.4 ns | 26.0 ns | 56.5 ns | 1.69x | 0.46x |
| `regexp_sticky` | 378 ns | 378 ns | 250 ns | 1.51x | 1.51x |
| `generator_create` | 593 ns | 596 ns | 400 ns | 1.48x | 1.49x |
| `regexp_exec_g` | 391 ns | 398 ns | 265 ns | 1.48x | 1.50x |
| `typed_forEach` | 55.2 ns | 11.8 ns | 39.4 ns | 1.40x | 0.30x |
| `regexp_replace_dollar` | 1,325 ns | 1,328 ns | 952 ns | 1.39x | 1.39x |
| `string_starts_with` | 96.5 ns | 79.4 ns | 69.5 ns | 1.39x | 1.14x |
| `try_catch` | 17.0 ns | 4.38 ns | 13.3 ns | 1.28x | 0.33x |
| `tail_self` | 45.4 ns | 11.0 ns | 35.8 ns | 1.27x | 0.31x |
| `loop_sum` | 8.64 ns | 1.08 ns | 18.9 ns | 0.46x | 0.06x |
| `loop_arith` | 25.0 ns | 3.16 ns | 54.9 ns | 0.46x | 0.06x |
| `object_spread` | 192 ns | 89.5 ns | 428 ns | 0.45x | 0.21x |
| `array_spread` | 218 ns | 123 ns | 557 ns | 0.39x | 0.22x |
| `async_generator` | 607 ns | 658 ns | 1,949 ns | 0.31x | 0.34x |
| `regexp_split` | 1,159 ns | 1,192 ns | 5,710 ns | 0.20x | 0.21x |
| `async_await` | 99.1 ns | 116 ns | 774 ns | 0.13x | 0.15x |
| `regexp_search_i` | 1,719 ns | 1,983 ns | 21,327 ns | 0.08x | 0.09x |
| `regexp_search_utf16` | 524 ns | 566 ns | 13,418 ns | 0.04x | 0.04x |

The BigInt tests were the largest gaps, 9.65 and 5.56 times QuickJS's time
at `608415b`, where every BigInt was an allocation. A BigInt in the int64
range is now held in the value itself (`9746906`), as QuickJS holds one
that fits in a word, and so is one below 2^64 in magnitude (`f8c3229`);
a larger one is a block of no pointers, made by the engine's own
arithmetic (`f1bf681`). `bigint64_arith` takes 0.78 times QuickJS's time
and `bigint256_arith` 1.06 times, and the extra BigInt tests 0.72 to 1.14
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
  1.1 to 3.0 times over six loops of simple instructions (1.8 by their
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
  `empty_loop` (1.01), `int_arith` (0.97), `float_arith` (0.76),
  `array_update` -- are level with QuickJS or ahead of it, and the six
  loops above take 0.46 to 1.16 times QuickJS's time as trees. A loop whose body
  would need its branches chosen by a structure of their own, an `&&` or an
  else-if chain, still runs from block to block, which costs less.
- **Calls.** A call from JavaScript to JavaScript fills in a frame record of
  some fifteen fields, and each pointer it stores pays a write barrier's
  check. Even so a call costs about what QuickJS's does: a method call
  whose body needs a frame -- one with a loop, even an empty one -- about
  71 ns, against 69 in QuickJS (`method_call`), and a method that only
  returns its argument about 38 ns, against 42 (`method_call_leaf`). With
  the JIT they take 11.8 and 3.2 ns. Richards, DeltaBlue and
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
    collections do. Called that way, an empty function costs about 30 ns,
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
  the key checked once rather than twice (`array_update`, 0.67 times
  QuickJS's time). for-in over an array whose keys are its elements walks
  them by index rather than copying them into a list first
  (`array_for_in`, 1.03), and the array methods pass over a hole
  without looking its index up, where nothing up the prototype chain could
  have it (`array_hole_length_decr`, 0.89). A typed array of a
  fixed length is written through the storage it keeps
  (`typed_array_write`, 0.94).
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
from a quarter of the interpreter's time down to a twenty-fifth.

[goja]: https://github.com/dop251/goja
[v8-v7]: https://github.com/mozilla/arewefastyet/tree/master/benchmarks/v8-v7
