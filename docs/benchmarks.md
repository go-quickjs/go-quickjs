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
[goja]. Both go-quickjs columns use revision `769cf3d8`, measured alongside
goja on 2026-10-28. Each column is the median of three fresh-process runs
on an Apple M5 Max with Go 1.27.3, with 30 seconds between runs and AC
power throughout. One engine ran at a time, with the order rotated each
round so every engine ran first, second and third once. JIT off was built
without the `quickjs_jit` tag.
Higher suite scores are better; the relative columns divide each
go-quickjs score by goja's.

| Workload | go-quickjs JIT off | go-quickjs JIT on | goja | JIT off / goja | JIT on / goja |
|---|---:|---:|---:|---:|---:|
| Richards | 2,111 | 12,003 | 495 | 4.26x | 24.25x |
| DeltaBlue | 2,579 | 11,782 | 586 | 4.40x | 20.11x |
| Crypto | 3,141 | 16,475 | 320 | 9.82x | 51.48x |
| RayTrace | 5,438 | 20,383 | 727 | 7.48x | 28.04x |
| EarleyBoyer | 6,497 | 9,008 | 1,333 | 4.87x | 6.76x |
| RegExp | 4,795 | 4,505 | 559 | 8.58x | 8.06x |
| Splay | 8,255 | 12,460 | 2,245 | 3.68x | 5.55x |
| NavierStokes | 5,572 | 36,730 | 534 | 10.43x | 68.78x |
| **Composite score** | **4,335** | **13,012** | **705** | **6.15x** | **18.46x** |

The runner's timed suite interval includes runtime construction, parsing,
compilation, the suite's warmups and its measured iterations, but excludes
reading the source files and the final forced garbage collection. CPU time
includes work on other threads, including the collector. These are the
JIT-off go-quickjs and goja runs:

| Metric | go-quickjs JIT off | goja | Relative result |
|---|---:|---:|---:|
| Suite wall time | 22.33 s | 53.33 s | go-quickjs 2.39x faster |
| Suite CPU time | 27.26 s | 75.07 s | go-quickjs 63.7% less |
| Total allocation | 10.94 GiB | 31.38 GiB | go-quickjs 65.2% less |

Score mode does different amounts of work as throughput changes, so the CPU
and allocation totals are not equal-work efficiency measurements. The
composite is the median of the three printed composite scores, not a
composite calculated from the workload medians.

The suite warms every workload for at least one second, then measures for at
least another second and continues until it has 32 measured iterations. Both
engines evaluated the same benchmark sources and validation code.
The measured revisions were go-quickjs `769cf3d8`, goja `793a2a6`, and
AreWeFastYet `0e21608`. These results characterize this older workload
suite rather than every application; go-quickjs leads all eight of its
workloads in this comparison.

The comparison used `internal/cmd/v8bench/matrix`, selecting `gq`, `gqjit`
and `goja` in separate invocations with `-suites score -runs 1
-parallel=false -v8 /tmp/v8-v7` and a separate `-out` directory for each
sample, with 30-second pauses. The engine orders were `gq,gqjit,goja`,
`gqjit,goja,gq` and `goja,gq,gqjit`. `-report` combined the saved outputs
into the median score table. Each engine's three binaries were
byte-identical. The JIT-off composite samples were 4,404, 4,332 and 4,335;
goja's were 694, 705 and 731. All samples were included.

For the JIT-on column, the runner was built with `-tags quickjs_jit` and
run with `-jit`, using the runner's 32 MiB JIT budget rather than the
runtime's default 8 MiB. Its composite samples were 14,081, 12,842 and
13,012, all included. The table takes the median of each workload and of
the printed composite scores independently.

## Compared with C QuickJS

Over the V8 suite as a whole, go-quickjs is ahead of QuickJS. Its
interpreter takes 0.60 times QuickJS's time and has a 25% higher composite
score: it is behind on Richards, level on DeltaBlue and ahead on the other
six. With its JIT it takes 0.33 times QuickJS's time, scores 3.8 times as
much, and is ahead on all eight. On the larger programs of Octane the
interpreter is level with QuickJS, 1.00 times its time, and on Are We
Fast Yet behind, 1.07 times; the JIT takes 0.53 and 0.26 times
([below](#octane-and-are-we-fast-yet)). Single operations cost less on
average, 0.90 times QuickJS's time, and 0.35 times with the JIT, as
[the micro-benchmarks](#micro-benchmarks) below show, though a few still
take about twice as long.

go-quickjs has the same design as QuickJS: a bytecode compiler, a
stack-based interpreter, NaN-boxed values. Running that design in Go costs
speed. The JIT, which is experimental and optional, compiles hot functions
to native code (see its [design notes](../internal/jit/README.md)). The
same suite ran on an AMD Ryzen 5 3600 under Windows, with Go 1.27.3,
go-quickjs `882b9d0` (after v0.29.0) and QuickJS 2026-06-04. Two runs
went at once, each on the six CPUs of a level 3 cache of its own. The
JIT's column is a build with `-tags quickjs_jit` run with `-jit`. The
figures are milliseconds for the same fixed work, the median of three
fresh-process runs (lower is better):

| Workload | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| Richards | 12.7 ms | 8.1 ms | 11.3 ms | 1.12x | 0.72x |
| DeltaBlue | 23.7 ms | 23.1 ms | 23.9 ms | 0.99x | 0.97x |
| Crypto | 296 ms | 71.1 ms | 328 ms | 0.90x | 0.22x |
| RayTrace | 124 ms | 68.3 ms | 143 ms | 0.87x | 0.48x |
| EarleyBoyer | 355 ms | 151 ms | 399 ms | 0.89x | 0.38x |
| RegExp | 240 ms | 245 ms | 879 ms | 0.27x | 0.28x |
| Splay | 260 ms | 202 ms | 492 ms | 0.53x | 0.41x |
| NavierStokes | 173 ms | 38.0 ms | 190 ms | 0.91x | 0.20x |
| **Total** | **1,509 ms** | **833 ms** | **2,521 ms** | **0.60x** | **0.33x** |

The suite's own scores, the median of three (higher is better):

| Workload | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| Richards | 860 | 4,445 | 947 | 0.91x | 4.69x |
| DeltaBlue | 853 | 4,792 | 842 | 1.01x | 5.69x |
| Crypto | 1,219 | 6,823 | 1,109 | 1.10x | 6.15x |
| RayTrace | 1,848 | 8,788 | 1,591 | 1.16x | 5.52x |
| EarleyBoyer | 2,428 | 3,537 | 2,183 | 1.11x | 1.62x |
| RegExp | 1,682 | 1,602 | 435 | 3.87x | 3.68x |
| Splay | 3,496 | 5,042 | 3,362 | 1.04x | 1.50x |
| NavierStokes | 2,701 | 15,566 | 2,405 | 1.12x | 6.47x |
| **Composite** | **1,680** | **5,176** | **1,346** | **1.25x** | **3.85x** |

The two kinds of figure time different things, which is why they do not
agree on how far ahead go-quickjs is:

- A fixed run times each workload's setup, its runs and its teardown. The
  scored run times only the runs, after a second of warming up.
- The total adds milliseconds, so the longest workloads, RegExp and Splay,
  make most of the interpreter's lead. The composite score is the
  geometric mean of the workloads' scores, so each counts the same:
  Richards weighs as much as RegExp. By that mean the interpreter takes
  0.75 times QuickJS's time, and the JIT 0.40 times, where their scores
  are 25% higher and 3.8 times as high.
- The JIT's lead is much larger in the scores than in fixed time. A fixed
  run of Richards or DeltaBlue lasts a few milliseconds, and a function
  runs in the interpreter until it is hot enough to compile, so much of
  that run is spent before the JIT's code takes over. The scores time
  warm runs, where Richards, DeltaBlue, Crypto, RayTrace and
  NavierStokes score 4.8 to 5.8 times what the interpreter does.
- Richards is behind by both measures, 12% in fixed time and 9% in its
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
| Richards | 4.5 ms | 4.5 ms | 3.9 ms | 1.15x | 1.15x |
| DeltaBlue | 8.1 ms | 8.6 ms | 7.7 ms | 1.05x | 1.12x |
| Crypto | 99.6 ms | 26.2 ms | 109 ms | 0.91x | 0.24x |
| RayTrace | 39.7 ms | 41.6 ms | 50.2 ms | 0.79x | 0.83x |
| EarleyBoyer | 111 ms | 62.2 ms | 139 ms | 0.80x | 0.45x |
| RegExp | 173 ms | 170 ms | 463 ms | 0.37x | 0.37x |
| Splay | 229 ms | 165 ms | 457 ms | 0.50x | 0.36x |
| NavierStokes | 64.8 ms | 18.3 ms | 64.5 ms | 1.00x | 0.28x |
| PdfJS | 296 ms | 272 ms | 373 ms | 0.79x | 0.73x |
| Mandreel | 4,026 ms | 1,626 ms | 3,377 ms | 1.19x | 0.48x |
| Gameboy | 492 ms | 336 ms | 462 ms | 1.06x | 0.73x |
| CodeLoad | 15.9 ms | 16.3 ms | 13.0 ms | 1.22x | 1.25x |
| Box2D | 146 ms | 135 ms | 154 ms | 0.94x | 0.87x |
| zlib | 7,880 ms | 3,094 ms | 8,327 ms | 0.95x | 0.37x |
| Typescript | 2,548 ms | 2,394 ms | 2,145 ms | 1.19x | 1.12x |
| **Total** | **16,697 ms** | **8,899 ms** | **16,736 ms** | **1.00x** | **0.53x** |

| Are We Fast Yet | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| DeltaBlue | 894 ms | 364 ms | 719 ms | 1.24x | 0.51x |
| Richards | 5,444 ms | 867 ms | 4,771 ms | 1.14x | 0.18x |
| Json | 1,405 ms | 338 ms | 1,347 ms | 1.04x | 0.25x |
| CD | 2,776 ms | 622 ms | 2,844 ms | 0.98x | 0.22x |
| Havlak | 5,277 ms | 1,976 ms | 4,300 ms | 1.23x | 0.46x |
| Bounce | 1,373 ms | 259 ms | 1,477 ms | 0.93x | 0.18x |
| List | 1,373 ms | 207 ms | 1,245 ms | 1.10x | 0.17x |
| Mandelbrot | 421 ms | 42.5 ms | 690 ms | 0.61x | 0.06x |
| NBody | 1,026 ms | 125 ms | 1,017 ms | 1.01x | 0.12x |
| Permute | 1,669 ms | 240 ms | 1,680 ms | 0.99x | 0.14x |
| Queens | 1,147 ms | 132 ms | 1,062 ms | 1.08x | 0.12x |
| Sieve | 1,136 ms | 285 ms | 1,213 ms | 0.94x | 0.24x |
| Storage | 1,670 ms | 798 ms | 1,647 ms | 1.01x | 0.48x |
| Towers | 1,702 ms | 240 ms | 1,549 ms | 1.10x | 0.15x |
| **Total** | **27,303 ms** | **6,531 ms** | **25,609 ms** | **1.07x** | **0.26x** |

- Without the JIT, go-quickjs leads where the V8 suite has it lead --
  RegExp, Splay, the arithmetic of Crypto and NavierStokes, Mandelbrot's
  loops -- and is behind on code that is mostly calls and allocation, by
  as much as 24% (Are We Fast Yet's DeltaBlue) and 23% (Havlak). Of
  Octane's long programs, Mandreel's code compiled from C++, Typescript's
  compiler and CodeLoad's parsing are 19 to 22% behind, and pdf.js is 21%
  ahead. zlib's, compiled from C, is 5% ahead, where it was 20% behind at
  `3714c86`: its bitwise arithmetic and its elements at int32 keys run as
  int32 trees since `cf27162`, `5e22bb2` and `f838b8c`.
- With the JIT, Are We Fast Yet takes 0.06 to 0.51 times QuickJS's time,
  its loops of arithmetic the least, its programs that allocate most --
  DeltaBlue, Havlak, Storage -- the most.
- On five of Octane's fifteen the JIT is level with the interpreter or
  behind it: level on Richards, RegExp and CodeLoad, behind on DeltaBlue
  (6%) and RayTrace (5%). Octane's fixed work is one run of each
  benchmark, so compiling counts: DeltaBlue's 16 compiles took about
  2.5 ms of its 8.5 at `3714c86`. Typescript, level at `3714c86`, is 6%
  faster with the JIT, and RegExp, 17% slower then, is level since its
  run blocks are compiled once (`f877a6e`).

### Micro-benchmarks

QuickJS's own micro-benchmarks, [`tests/microbench.js`](https://github.com/bellard/quickjs/blob/master/tests/microbench.js),
time one operation each, in nanoseconds. Each test was given the work QuickJS
takes about 150 ms for, all three ran that same work in fresh processes at
`882b9d0`, and the figure is the fastest of three runs. The runners'
micro mode does this, from a copy of the file kept in `internal/v8bench`, and
the matrix above runs it. `sort_bench` is left out: it reports a run's time
over its fastest sort, not the time of an operation. Over the other 71 tests
go-quickjs takes 0.90 times QuickJS's time by the geometric mean: it is
slower on 20, faster on 38, and within 5% on the rest. With the JIT it
takes 0.35 times QuickJS's time, slower on 4 and faster on 62. The table
has the interpreter's largest gaps either way:

| Test | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| `array_prop_create` | 43.7 ns | 15.6 ns | 14.9 ns | 2.94x | 1.05x |
| `weak_map_delete` | 528 ns | 472 ns | 248 ns | 2.13x | 1.91x |
| `global_read` | 10.0 ns | 1.69 ns | 5.57 ns | 1.80x | 0.30x |
| `weak_map_set` | 146 ns | 96.4 ns | 92.3 ns | 1.58x | 1.05x |
| `array_read` | 9.52 ns | 1.47 ns | 6.57 ns | 1.45x | 0.22x |
| `typed_array_read` | 13.7 ns | 2.31 ns | 9.77 ns | 1.40x | 0.24x |
| `global_write` | 10.6 ns | 2.59 ns | 7.60 ns | 1.39x | 0.34x |
| `global_write_strict` | 10.5 ns | 2.61 ns | 7.63 ns | 1.38x | 0.34x |
| `empty_do_loop` | 7.13 ns | 0.83 ns | 5.38 ns | 1.33x | 0.15x |
| `date_parse` | 1,083 ns | 1,019 ns | 829 ns | 1.31x | 1.23x |
| `string_build2c` | 64.8 ns | 39.5 ns | 147 ns | 0.44x | 0.27x |
| `array_for_of` | 19.9 ns | 6.32 ns | 46.0 ns | 0.43x | 0.14x |
| `array_pop` | 47.7 ns | 19.7 ns | 110 ns | 0.43x | 0.18x |
| `prop_clone` | 30.8 ns | 10.4 ns | 71.9 ns | 0.43x | 0.14x |
| `array_slice` | 11.6 ns | 12.1 ns | 27.0 ns | 0.43x | 0.45x |
| `string_to_int` | 44.7 ns | 39.1 ns | 112 ns | 0.40x | 0.35x |

The JIT does the most for a test that is a loop of arithmetic, locals,
property reads and writes, array elements and calls: `prop_read` takes
0.80 ns with it, `func_call` 1.15 ns, a tenth of either interpreter's
time or less. A test whose time is a built-in function's work runs much
as it does in the interpreter: regular expressions, `array_slice`, and
the conversions between numbers and strings. In the run made for this
page, only `string_to_float` of these tests took 7% or more longer with
the JIT than without it, and, of the extra tests below,
`generator_delegate`, `string_trim`, `string_chain`, `async_await`,
`regexp_match_g` and `regexp_search_utf16`. A single run's list moves:
of the three regular expressions slower in both runs at `3714c86`, one
is in it.

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
88 QuickJS can run (it has no using declarations) go-quickjs takes 0.79
times its time, and 0.35 times with the JIT. An await costs much less,
resumed without allocating (`3af4f32`), as does an async generator, and
regular expressions that search a text or split it much less again. A
generator still costs more to resume from for-of, 1.30 times QuickJS's
time, though less than before: it is 96 bytes, made by its kind
(`d8fad84`, `7422742`). A call between two functions in tail position
costs more, as it goes through the pending tail call and its checks, and
so does a global or sticky exec, which makes a result and a String for
each match. The largest gaps either way:

| Test | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| `tail_mutual` | 54.7 ns | 8.81 ns | 35.2 ns | 1.55x | 0.25x |
| `regexp_sticky` | 385 ns | 382 ns | 259 ns | 1.49x | 1.47x |
| `regexp_exec_g` | 390 ns | 401 ns | 272 ns | 1.43x | 1.47x |
| `regexp_replace_dollar` | 1,303 ns | 1,337 ns | 955 ns | 1.36x | 1.40x |
| `generator_for_of` | 73.0 ns | 21.0 ns | 56.1 ns | 1.30x | 0.37x |
| `object_rest` | 638 ns | 211 ns | 493 ns | 1.29x | 0.43x |
| `object_computed` | 208 ns | 166 ns | 161 ns | 1.29x | 1.03x |
| `closure_callback` | 95.8 ns | 6.62 ns | 77.1 ns | 1.24x | 0.09x |
| `string_starts_with` | 83.2 ns | 75.6 ns | 69.1 ns | 1.20x | 1.09x |
| `try_catch` | 15.9 ns | 4.29 ns | 13.3 ns | 1.20x | 0.32x |
| `loop_sum` | 8.58 ns | 0.93 ns | 18.9 ns | 0.45x | 0.05x |
| `array_indexOf` | 120 ns | 110 ns | 270 ns | 0.45x | 0.41x |
| `loop_arith` | 24.2 ns | 3.23 ns | 55.1 ns | 0.44x | 0.06x |
| `array_spread` | 223 ns | 122 ns | 513 ns | 0.44x | 0.24x |
| `async_generator` | 620 ns | 642 ns | 1,946 ns | 0.32x | 0.33x |
| `regexp_split` | 1,194 ns | 1,198 ns | 5,736 ns | 0.21x | 0.21x |
| `async_await` | 110 ns | 122 ns | 754 ns | 0.15x | 0.16x |
| `regexp_search_i` | 1,743 ns | 1,848 ns | 20,837 ns | 0.08x | 0.09x |
| `regexp_search_utf16` | 535 ns | 583 ns | 13,103 ns | 0.04x | 0.04x |

The BigInt tests were the largest gaps, 9.65 and 5.56 times QuickJS's time
at `608415b`, where every BigInt was an allocation. A BigInt in the int64
range is now held in the value itself (`9746906`), as QuickJS holds one
that fits in a word, and so is one below 2^64 in magnitude (`f8c3229`);
a larger one is a block of no pointers, made by the engine's own
arithmetic (`f1bf681`). `bigint64_arith` takes 0.77 times QuickJS's time
and `bigint256_arith` 1.17 times, and the extra BigInt tests 0.68 to 1.16
times. The JIT makes BigInts of 64 bits natively: `bigint64_arith` takes
0.12 times QuickJS's time with it.

The largest gaps now are:

- `array_prop_create`, an array made by assigning past its end, whose
  storage grows as it goes and leaves what it outgrew to the collector;
- `tail_mutual`, each call going through the pending tail call;
- `regexp_sticky` and `regexp_exec_g`, a global or sticky `exec`, which
  makes a result and a String for each match;
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
  1.2 to 3.1 times over six loops of simple instructions (1.9 by their
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
  `empty_loop` (0.96), `int_arith` (0.97), `float_arith` (0.77),
  `array_update` -- are level with QuickJS or ahead of it, and the six
  loops above take 0.44 to 1.15 times QuickJS's time as trees. A loop whose body
  would need its branches chosen by a structure of their own, an `&&` or an
  else-if chain, still runs from block to block, which costs less.
- **Calls.** A call from JavaScript to JavaScript fills in a frame record of
  some fifteen fields, and each pointer it stores pays a write barrier's
  check. Even so a call costs about what QuickJS's does: a method call
  whose body needs a frame -- one with a loop, even an empty one -- about
  73 ns, against 69 in QuickJS (`method_call`), and a method that only
  returns its argument about 32 ns, against 42 (`method_call_leaf`). With
  the JIT they take 11.7 and 2.9 ns. Richards, DeltaBlue and
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
    collections do. Called that way, an empty function costs about 28 ns,
    against 31 in QuickJS (`func_call`). One of seven
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
  the key checked once rather than twice (`array_update`, 0.66 times
  QuickJS's time). for-in over an array whose keys are its elements walks
  them by index rather than copying them into a list first
  (`array_for_in`, 1.03), and the array methods pass over a hole
  without looking its index up, where nothing up the prototype chain could
  have it (`array_hole_length_decr`, 0.91). A typed array of a
  fixed length is written through the storage it keeps
  (`typed_array_write`, 0.92).
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
