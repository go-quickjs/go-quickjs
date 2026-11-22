# Benchmarks

The Go benchmarks and bundle timings below were measured at `b37daa23`.
The V8 comparison with goja was measured at `1d08a62d` on the register
interpreter and the JIT that compiles its register code. The comparison
with C QuickJS, under [Compared with C QuickJS](#compared-with-c-quickjs),
was measured at `7f8205ec`, its JIT's column at `329ee166`. Other
comparisons retain the revisions and measurement dates their sections
name.

On an Apple M5 Max with Go 1.27.3 and the native JIT disabled, taking
per-metric medians of five fresh-process runs at go-quickjs `b37daa23` on
2026-11-16, with 30 seconds between runs and AC power throughout:

```
BenchmarkEvalArithmetic-18  1.23µs/op  4170 B/op  28 allocs/op
BenchmarkFibonacci-18        333µs/op    20 B/op   1 allocs/op
BenchmarkPropertyAccess-18  2.14µs/op  6442 B/op  36 allocs/op
BenchmarkCallGoFunction-18  1.91µs/op  6226 B/op  43 allocs/op
```

`fib(20)` costs one allocation because the interpreter loop allocates nothing
per call; the other benchmarks include parsing and compiling their source each
iteration.

`go test -run '^$' -bench . -benchmem .` also runs compile-once interpreter
workloads, plus JSON parsing and serialization, compilation, runtime
construction, and a recursive factorial workload. The remaining root-package
benchmarks are:

```
BenchmarkLoopArithmetic-18        31.3µs/op      0 B/op     0 allocs/op
BenchmarkDateUTC-18                262µs/op      0 B/op     0 allocs/op
BenchmarkLoopPropertyAccess-18     121µs/op      0 B/op     0 allocs/op
BenchmarkLoopArrayIndex-18        6.72µs/op      0 B/op     0 allocs/op
BenchmarkLoopFunctionCall-18       156µs/op      0 B/op     0 allocs/op
BenchmarkLoopMethodCall-18         174µs/op      0 B/op     0 allocs/op
BenchmarkAllocObjects-18          42.3µs/op   160 KB/op  2000 allocs/op
BenchmarkAllocArrays-18           56.3µs/op   256 KB/op  2000 allocs/op
BenchmarkStringConcat-18          19.6µs/op  12.7 KB/op    13 allocs/op
BenchmarkClosureCreateAndCall-18   402µs/op  1.12 MB/op  5001 allocs/op
BenchmarkArrayCallbacks-18        28.9µs/op  36.1 KB/op    18 allocs/op
BenchmarkMapOperations-18          108µs/op   311 KB/op    25 allocs/op
BenchmarkRegExpExec-18             283µs/op   320 KB/op  1000 allocs/op
BenchmarkThrowCatch-18             211µs/op   672 KB/op  6000 allocs/op
BenchmarkJSONRoundTrip-18          161µs/op   232 KB/op  3000 allocs/op
BenchmarkFactorial-18             16.3ms/op   417 KB/op  2284 allocs/op
BenchmarkCompileScript-18         22.9µs/op  42.6 KB/op   343 allocs/op
BenchmarkNewRuntime-18            71.9µs/op   317 KB/op  2146 allocs/op
BenchmarkNewRuntimeSmallStack-18  82.9µs/op   314 KB/op  2146 allocs/op
BenchmarkJSONParse-18              278µs/op   652 KB/op  4751 allocs/op
BenchmarkJSONStringify-18          221µs/op   826 KB/op    21 allocs/op
```

Each compile-once benchmark runs its workload many times: the main loop
benchmarks use 10,000 iterations, the object and array benchmarks allocate
2,000 values, and the callback, collection, regexp, exception and JSON
benchmarks exercise their operation hundreds or thousands of times.
`CompileScript` parses, compiles and evaluates one program per operation,
while the runtime benchmarks construct and close one runtime per operation.
`Factorial` constructs one runtime and evaluates 100,000 calls to
`factorial(10)` per operation, totaling one million recursive calls
(16.3 ns/call). Each of the five samples came from a separate invocation,
with one second per benchmark and `-benchmem`, not repeated measurements
inside a single test process. `JSONParse` and `JSONStringify` process a
1,000-record document, with input construction and compilation outside the
timed interval.

## Loading large bundles

The optional open-source corpus uses two unmodified MIT-licensed upstream
builds: [Three.js r160][three-bundle] (1,315,993 bytes, [license][three-license])
and [Babel standalone 7.26.10][babel-bundle] (3,014,882 bytes,
[license][babel-license]). Three.js exercises a large unminified library;
Babel adds a larger minified bundle with many nested functions. The
tests pin each file's SHA-256 and run without a network connection. Download
the corpus once, outside the checkout, as with the V8 suites:

```sh
go run ./internal/cmd/bundlefetch -dir /tmp/quickjs-bundles
export QUICKJS_BUNDLE_DIR=/tmp/quickjs-bundles

go test . -run '^TestOpenSourceBundles$' -count=1
GOMAXPROCS=1 go test ./internal/compiler -run '^$' \
  -bench '^BenchmarkCompileBundle$' -benchmem
```

`BenchmarkCompileBundle` parses once outside the measured interval and
compiles the AST anew each iteration, reporting compiler allocations.
`BenchmarkLoadBundle` measures parsing, compilation and execution in a new
runtime each iteration. Runtime construction, file reads, host setup and
checks are outside its interval. Both bundles run as classic scripts, as
`Runtime.EvalFile` does; the host supplies a console stub. The checks exercise
Three.js vector operations and use Babel to transform an arrow function,
with exact expected results. Without `QUICKJS_BUNDLE_DIR` the optional tests
and benchmarks skip; with it, missing files or checksum mismatches fail.

For first-use latency, build once and run one operation in each fresh
process, alternating the two builds being compared:

```sh
CGO_ENABLED=0 go test -c -pgo=off -trimpath -o /tmp/quickjs-load.test .
TZ=UTC GOMAXPROCS=1 /usr/bin/time -l /tmp/quickjs-load.test \
  -test.run '^$' -test.bench '^BenchmarkLoadBundle/Three$' \
  -test.benchtime=1x -test.benchmem
```

Choose `Babel` separately so that each fresh process loads just one bundle.
On macOS, `time -l` reports peak process RSS; Linux's equivalent is `time -v`.

On an Apple M5 Max, macOS/arm64, Go 1.27.3, `GOMAXPROCS=1`, `TZ=UTC`,
with `CGO_ENABLED=0 -pgo=off -trimpath` and JIT disabled, go-quickjs
`b37daa23` gives these per-metric medians, measured on 2026-11-16. Compiler
figures use five fresh processes, each running both bundles with 500 ms per
benchmark. Load figures use twenty fresh processes per bundle, one load per
process. One process runs at a time, with 30-second pauses between the five
compiler rounds and between the twenty load rounds. Each load round runs
both bundles in separate processes, alternating their order. AC power was
used throughout, with no concurrent builds or tests. All samples are
included, including the first.

| Bundle | Compile | First load |
|---|---:|---:|
| Three.js | 13.23 ms | 40.63 ms |
| Babel | 36.32 ms | 212.88 ms |

The first-load interval includes parsing, compilation and execution. It
excludes runtime construction, source reads, host setup and the functional
checks. The compiler-only interval starts after parsing and measures
compilation of the existing AST; it does not include work deferred to
executing the bundle.

Memory figures for the same runs are below. Allocations are total bytes
allocated per operation, in decimal MB. Peak load RSS is the whole test
process, including its untimed setup and functional checks, in MiB. Each
metric is the median of its own samples:

| Bundle | Compiler MB/op | Load MB/op | Peak load RSS MiB |
|---|---:|---:|---:|
| Three.js | 7.85 | 26.74 | 50.20 |
| Babel | 25.62 | 118.02 | 136.52 |

The `qjs` binary built without the JIT, using `CGO_ENABLED=0 go build
-pgo=off -trimpath ./cmd/qjs`, is 42,792,210 bytes. The load test binary is
41,065,618 bytes, and the compiler test binary is 9,343,618 bytes. The
bundle sizes and SHA-256 checks are the pinned upstream files above.

[three-bundle]: https://unpkg.com/three@0.160.0/build/three.js
[three-license]: https://github.com/mrdoob/three.js/blob/r160/LICENSE
[babel-bundle]: https://unpkg.com/@babel/standalone@7.26.10/babel.min.js
[babel-license]: https://github.com/babel/babel/blob/v7.26.10/LICENSE

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
[goja]. All three go-quickjs columns use revision `1d08a62d`, measured alongside
goja on 2026-11-22. Each column is the median of three fresh-process runs
on an Apple M5 Max with Go 1.27.3, with 30 seconds between runs and AC
power throughout. One engine ran at a time, with the order rotated each
round. JIT off was built without the `quickjs_jit` tag. JIT on compiles
synchronously; JIT background enables the JIT and compiles in the background
with `-jit -jitbackground`. Both use the same binary built with
`-tags quickjs_jit` and the runner's 32 MiB JIT budget rather than the
runtime's default 8 MiB. This revision uses the register interpreter and
register JIT by default.
Higher suite scores are better; the relative columns divide each
go-quickjs score by goja's.

| Workload | go-quickjs JIT off | go-quickjs JIT on | go-quickjs JIT background | goja | JIT off / goja | JIT on / goja | JIT background / goja |
|---|---:|---:|---:|---:|---:|---:|---:|
| Richards | 3,364 | 15,505 | 15,356 | 538 | 6.25x | 28.82x | 28.54x |
| DeltaBlue | 3,716 | 13,944 | 14,387 | 653 | 5.69x | 21.35x | 22.03x |
| Crypto | 5,677 | 21,788 | 21,766 | 344 | 16.50x | 63.34x | 63.27x |
| RayTrace | 11,026 | 36,075 | 34,005 | 787 | 14.01x | 45.84x | 43.21x |
| EarleyBoyer | 11,799 | 15,749 | 16,039 | 1,525 | 7.74x | 10.33x | 10.52x |
| RegExp | 9,047 | 10,819 | 11,160 | 589 | 15.36x | 18.37x | 18.95x |
| Splay | 17,789 | 25,164 | 25,075 | 2,627 | 6.77x | 9.58x | 9.55x |
| NavierStokes | 10,515 | 39,326 | 39,623 | 578 | 18.19x | 68.04x | 68.55x |
| **Composite score** | **7,951** | **20,130** | **20,218** | **776** | **10.25x** | **25.94x** | **26.05x** |

The runner's timed suite interval includes runtime construction, parsing,
compilation, the suite's warmups and its measured iterations, but excludes
reading the source files and the final forced garbage collection. CPU time
includes work on other threads, including the collector. These are the
go-quickjs and goja runs:

| Metric | go-quickjs JIT off | go-quickjs JIT on | go-quickjs JIT background | goja | JIT off relative to goja |
|---|---:|---:|---:|---:|---:|
| Suite wall time | 20.15 s | 20.09 s | 20.10 s | 51.33 s | 2.55x faster |
| Suite CPU time | 24.28 s | 26.85 s | 26.98 s | 73.46 s | 66.9% less |
| Total allocation | 8.89 GiB | 16.73 GiB | 16.65 GiB | 34.99 GiB | 74.6% less |

Score mode does different amounts of work as throughput changes, so the CPU
and allocation totals are not equal-work efficiency measurements. The
composite is the median of the three printed composite scores, not a
composite calculated from the workload medians.

The suite warms every workload for at least one second, then measures for at
least another second and continues until it has 32 measured iterations. All
engines evaluated the same benchmark sources and validation code.
The measured revisions were go-quickjs `1d08a62d`, goja `a177d211bf60`, and
AreWeFastYet `0e21608`. The isolated goja runner pins
`v0.0.0-20261102223426-a177d211bf60` for this comparison. These results
characterize this older workload suite rather than every application;
go-quickjs leads all eight of its workloads in this comparison.

The comparison used `internal/cmd/v8bench/matrix`, selecting `gq`, `gqjit`,
`gqjitbg` and `goja` in separate invocations with `-suites score -runs 1
-parallel=false -v8 SUITE_DIR` and a separate `-out` directory for each
sample, with 30-second pauses. The engine orders were `gq,gqjit,gqjitbg,goja`,
`gqjit,gqjitbg,goja,gq` and `gqjitbg,goja,gq,gqjit`. `-report` combined the saved outputs
into the median score table. Each engine's three binaries were
byte-identical; both JIT modes used the same binary. The engine source
matched the revision above throughout measurement.

The composite samples were:

| Mode | Run 1 | Run 2 | Run 3 |
|---|---:|---:|---:|
| JIT off | 7,629 | 8,005 | 7,951 |
| JIT on | 20,130 | 20,010 | 20,813 |
| JIT background | 20,545 | 20,085 | 20,218 |
| goja | 757 | 779 | 776 |

All samples were included. The table takes the median of each workload and
of the printed composite scores independently. Background compilation's
composite was 0.44% higher than synchronous compilation's; this run does not
establish an overall throughput advantage for either compilation mode.

## Compared with C QuickJS

Over the V8 suite as a whole, go-quickjs is well ahead of QuickJS. Its
interpreter takes 0.31 times QuickJS's time and has 2.31 times its
composite score, ahead on all eight workloads. With its JIT it takes 0.20
times QuickJS's time and scores 5.8 times as much. On the larger programs
of Octane and Are We Fast Yet the interpreter takes 0.42 and 0.64 times
QuickJS's time, and the JIT 0.29 and 0.20 times
([below](#octane-and-are-we-fast-yet)); a page React 18 renders on the
server takes 0.58 times, and 0.49 with the JIT
([below](#server-rendering)). Single operations cost about half as much,
0.48 times QuickJS's time by the geometric mean of its micro-benchmarks,
and 0.27 times with the JIT, as [the micro-benchmarks](#micro-benchmarks)
below show; the slowest of them takes 1.5 times as long.

At `7f8205ec`, the revision measured here, go-quickjs compiles every
function to register code -- three-address instructions that name the
frame's registers as their operands, as V8's Ignition and Lua's VM do --
and nothing else: the stack bytecode is gone from the front end too
(`d6ac45be`). It runs that code on the register interpreter
(docs/register-interpreter.md): on amd64 and arm64 by native threaded
handlers, machine code that `internal/vm/internal/nativegen` generates,
and elsewhere by a table of Go handlers. A function of a validated asm.js
module -- code compiled from C, as Octane's zlib is -- is also compiled to
typed code, whose registers each hold a raw int32 or float64 or a Value for
the whole function, as the validator proved, and which runs by a typed
table of its own, natively too (register-interpreter.md, "Typed code";
`d8cbc918`). The JIT, which is experimental and optional, compiles a hot
function's register code to native code that runs on the interpreter's
own frame, and an asm.js module's typed code at its link, with no
speculation (see its [design notes](../internal/jit/README.md);
`5ae2317e`). The page's previous figures, at `b5df63ea`, whose code
v0.42.0 is, were of the same design before the changes below: the
megamorphic probes' index, the caches of absent keys, the closures' and
the arguments' changes, and typed code's calls, heap and spill slots in
the JIT. The same suites ran on an AMD Ryzen 5 3600 under Windows, with
Go 1.27.3 and QuickJS 2026-06-04: go-quickjs `7f8205ec` for the plain
columns and QuickJS's, and `329ee166` for the JIT's, whose code is
`4eff9cf3`'s, which changed only the JIT's register allocator; its plain
build is the same bytes as `7f8205ec`'s, but for the revision Go stamps
in it. Two runs went at once, each on the six CPUs of a level 3 cache of
its own, the JIT's beside the plain build's. The JIT's column is a build
with `-tags quickjs_jit` run with `-jit`. Each runner
was built with DYNAMICBASE cleared, so that Windows loads it at its image
base, and each process ran from a fresh copy of it (`-fixedbase`,
`-freshcopy`, `a9c5e457`); the previous figures were of one copy of each
build, loaded where Windows chose (see where a program is loaded,
below). The figures are milliseconds for the same fixed work, the median
of three fresh-process runs (lower is better):

| Workload | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| Richards | 8.0 ms | 8.4 ms | 11.2 ms | 0.71x | 0.75x |
| DeltaBlue | 14.1 ms | 16.2 ms | 24.0 ms | 0.59x | 0.67x |
| Crypto | 156 ms | 55.2 ms | 325 ms | 0.48x | 0.17x |
| RayTrace | 63.6 ms | 51.3 ms | 141 ms | 0.45x | 0.36x |
| EarleyBoyer | 164 ms | 114 ms | 394 ms | 0.42x | 0.29x |
| RegExp | 150 ms | 129 ms | 956 ms | 0.16x | 0.13x |
| Splay | 121 ms | 93.5 ms | 508 ms | 0.24x | 0.18x |
| NavierStokes | 101 ms | 37.1 ms | 190 ms | 0.53x | 0.20x |
| **Total** | **802 ms** | **526 ms** | **2,581 ms** | **0.31x** | **0.20x** |

The suite's own scores, the median of three (higher is better):

| Workload | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| Richards | 1,385 | 5,704 | 942 | 1.47x | 6.06x |
| DeltaBlue | 1,503 | 5,860 | 840 | 1.79x | 6.98x |
| Crypto | 2,329 | 8,198 | 1,105 | 2.11x | 7.42x |
| RayTrace | 3,848 | 12,506 | 1,572 | 2.45x | 7.96x |
| EarleyBoyer | 4,451 | 5,760 | 2,180 | 2.04x | 2.64x |
| RegExp | 2,824 | 4,039 | 390 | 7.24x | 10.36x |
| Splay | 6,719 | 9,771 | 3,366 | 2.00x | 2.90x |
| NavierStokes | 4,913 | 16,374 | 2,404 | 2.04x | 6.81x |
| **Composite** | **3,055** | **7,744** | **1,325** | **2.31x** | **5.84x** |

At `b5df63ea` the interpreter took 0.32 times QuickJS's time over the
suite and had 2.20 times its composite score, and the JIT 0.21 and 5.62
times. The interpreter's total is level, 807 to 802 ms, RayTrace 63.6 ms
where it took 69.8 and EarleyBoyer 164 where it took 172; its composite
is 3.2% higher, RayTrace's score 14% and EarleyBoyer's 15%. A read of a
function's `prototype` is now answered by its site's cache once the
function has made it, and a key some of a site's objects lack is kept
beside the shapes that have it (`6521aac5`), which took the reads that
exit to Go of Octane's RayTrace, the same program, from 32,067 to 351:
the `prototype` of each class `Class.create` makes. EarleyBoyer's
closures fill their capture lists in the function object (`648d7d30`),
and its `arguments` read through local aliases and its sloppy parameters
captured by value need no arguments object and no cell (`9c2c76bf`,
`a6da8b05`). The JIT's total is level,
525 to 526 ms, and its composite 2.2% higher, EarleyBoyer's score 17%.
Its Splay took 93.5 ms where it took 88.2, but in twelve fresh-process
runs of each build, alternated over four placements, 91.9 ms by the
median against 93.3, and EarleyBoyer 115 against 121; Richards, which
took 9.6 ms in a run of `7f8205ec`, took 8.4 in both, and its warm runs,
which the placements tool times, 0.65 ms each. A fixed run of Richards
lasts a few milliseconds, most of it before the JIT's code takes over,
and Splay's is mostly what the collector does. QuickJS's figures moved
more between sessions: its RegExp took 956 ms where it took 856, as it
took 947 in every run of another session the day `b5df63ea`'s was made,
the binary the same, and its RegExp score is 390 where it was 436, which
lowers its composite to 1,325 from 1,348.

On amd64 and arm64 a call of a body that only stores its parameters in
`this` (an initializer), of a constructor that only forwards to one with
`this.initialize.apply(this, arguments)`, as RayTrace's do, of a getter of
`this.k`, `this.k.length` or `this.k[p]`, as DeltaBlue's methods are, of a
sum, difference or product of its parameters or captured values, or of a
`typeof` test, is answered by the native call and construction handlers
without a frame (`cf1bbfda`, `d54099e3`, `09409332`, `746d9438`,
`bdab5ba4`). Since v0.39.0 the initializer gives the object its final
layout once, after every value is in place (`4799c889`), and remembers the
layouts its plan proved -- the object's before and after its stores, each
store's slot, the prototype chain as it checked it -- so that a
construction of an object of the same layout tests that layout and the
chain once and writes its values, as V8's initial map lets it
(`8505335a`); a base class's constructor has such a plan too (`de0a728b`).
`instanceof` keeps the constructor's prototype slot at its site, where the
native handler reads it rather than looking the property up (`32036a73`).
Those are the plain interpreter's handlers: a runtime with the JIT calls
through its own, which keep the frames.
v0.38.0 (`440db6bd`), the last release of the stack interpreter, measured
in `3ecb8200`'s session, took 0.51 and 0.23 times QuickJS's time.

The two kinds of figure time different things, which is why they do not
agree on how far ahead go-quickjs is:

- A fixed run times each workload's setup, its runs and its teardown. The
  scored run times only the runs, after a second of warming up.
- The total adds milliseconds, so the longest workloads, RegExp and Splay,
  make much of the lead. The composite score is the geometric mean of the
  workloads' scores, so each counts the same: Richards weighs as much as
  RegExp. By that mean the interpreter takes 0.41 times QuickJS's time,
  and the JIT 0.29 times, where their scores are 2.31 and 5.84 times as
  high.
- The JIT's lead is much larger in the scores than in fixed time. A fixed
  run of Richards or DeltaBlue lasts a few milliseconds, and a function
  runs in the interpreter until it is hot enough to compile, so much of
  that run is spent before the JIT's code takes over: with the JIT,
  Richards takes 1.05 times the interpreter's fixed time and DeltaBlue
  1.15 (DeltaBlue's three runs with it took 16.1 to 17.6 ms). The scores
  time warm runs, where Richards, DeltaBlue, Crypto, RayTrace and
  NavierStokes score 3.2 to 4.1 times what the interpreter does.
- Splay's lead is largest in its fixed time. That is almost all setup and
  teardown: building a tree of 8,000 nodes and dropping it, which QuickJS
  frees by counting references where Go's collector frees it later. A warm
  run, which is what the score times, is splay-tree operations, calls and
  property reads, and a new node for each insertion. Each node's payload
  holds array literals of constants, which share their elements with the
  literal until one is written, as V8's copy-on-write arrays do
  (`a2d9f12e`); its score is twice QuickJS's.
- The JIT changes least where the time is spent in Go code it calls
  rather than in the script's own: the allocation-heavy EarleyBoyer and
  Splay, 1.29 and 1.45 times the interpreter's score, and RegExp, 1.43
  times, though a runtime with the JIT compiles its patterns to machine
  code as V8's Irregexp does (`2be0068`).

Figures move by a few percent between builds from where the linker places
the code, and a single benchmark by up to a tenth. The layout lock holds
the interpreter's hot code -- the Go table's loop, the call path and the
native handlers' block -- at the offsets it measured best
(register-interpreter.md, "Code layout"), so that a change elsewhere does
not move it; what lies between its anchors still moves, and so does
everything after a pad that grows past a page (`b5df63ea`; see
docs/optimizing.md, "The layout lock"). On Windows one copy of a build
can also time apart from another copy of the same bytes, each at one speed
every time it runs: two copies of one build of `91f885a1` took 26 and 46
ns on `json_stringify_ints`, and of twelve copies ten took 49 ns on
`json_stringify_decimals` and two 35, where every copy of `b8c2c45f`'s
took 33. Most of that is where Windows loads each file, in steps of 64 KB
that the lock does not hold, and a copy at a fixed base can still sit
apart, for where its file lies in memory. The figures here are of builds
loaded at their image base, as a Linux build loads at its link address,
each process from a fresh copy (`-fixedbase`, `-freshcopy`;
docs/optimizing.md, "Builds and the machine"). The previous figures, at
`b5df63ea`, were of one copy of each build loaded where Windows chose, so
a micro-benchmark that moved by a tenth or more between the two may have
moved for that alone. To reproduce these figures, the suites below and
the micro-benchmarks, run `go run ./internal/cmd/v8bench/matrix -engines
gq,gqjit,qjs -qjs /path/to/qjs -v8 /tmp/v8-v7 -octane /tmp/octane -awfy
/tmp/awfy -ssr /tmp/ssr -fetch`.

### Octane and Are We Fast Yet

Octane 2.0 is the V8 suite's benchmarks revised, with larger programs
added: TypeScript's compiler, pdf.js, a Game Boy emulator, Box2D, zlib
and Mandreel, the last two compiled from C and C++. Are We Fast Yet's
fourteen benchmarks are written to compare language implementations
alike, each in the same style in every language. Both reach far more of
the language and of the JIT than the V8 suite does. They ran in the same
matrix, the same way: milliseconds for the fixed work, the median of
three fresh-process runs (lower is better). The plain build's figures
for the V8 suite above and for these are from a second run of the matrix
for those three suites alone, its builds the same bytes, after other
processes had taken a CPU for some seconds during the first run's Octane
and Are We Fast Yet; the JIT's are from runs of their own at `329ee166`,
for the same reason.

| Octane | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| Richards | 3.1 ms | 5.2 ms | 3.9 ms | 0.79x | 1.33x |
| DeltaBlue | 5.2 ms | 7.7 ms | 7.7 ms | 0.68x | 1.00x |
| Crypto | 52.2 ms | 23.2 ms | 109 ms | 0.48x | 0.21x |
| RayTrace | 21.1 ms | 32.2 ms | 49.3 ms | 0.43x | 0.65x |
| EarleyBoyer | 59.0 ms | 45.8 ms | 136 ms | 0.43x | 0.34x |
| RegExp | 115 ms | 135 ms | 508 ms | 0.23x | 0.26x |
| Splay | 114 ms | 79.6 ms | 461 ms | 0.25x | 0.17x |
| NavierStokes | 42.6 ms | 18.7 ms | 65.9 ms | 0.65x | 0.28x |
| PdfJS | 189 ms | 266 ms | 374 ms | 0.50x | 0.71x |
| Mandreel | 2,514 ms | 889 ms | 3,331 ms | 0.75x | 0.27x |
| Gameboy | 388 ms | 327 ms | 464 ms | 0.83x | 0.70x |
| CodeLoad | 12.5 ms | 14.6 ms | 12.8 ms | 0.98x | 1.14x |
| Box2D | 88.4 ms | 101 ms | 154 ms | 0.57x | 0.65x |
| zlib | 1,617 ms | 582 ms | 8,312 ms | 0.19x | 0.07x |
| Typescript | 1,482 ms | 1,988 ms | 2,149 ms | 0.69x | 0.93x |
| **Total** | **7,105 ms** | **4,915 ms** | **16,753 ms** | **0.42x** | **0.29x** |

| Are We Fast Yet | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| DeltaBlue | 554 ms | 315 ms | 712 ms | 0.78x | 0.44x |
| Richards | 3,227 ms | 658 ms | 4,782 ms | 0.67x | 0.14x |
| Json | 1,008 ms | 257 ms | 1,352 ms | 0.75x | 0.19x |
| CD | 1,587 ms | 482 ms | 2,818 ms | 0.56x | 0.17x |
| Havlak | 3,383 ms | 1,162 ms | 4,322 ms | 0.78x | 0.27x |
| Bounce | 764 ms | 247 ms | 1,496 ms | 0.51x | 0.17x |
| List | 550 ms | 160 ms | 1,261 ms | 0.44x | 0.13x |
| Mandelbrot | 176 ms | 41.0 ms | 695 ms | 0.25x | 0.06x |
| NBody | 659 ms | 121 ms | 1,027 ms | 0.64x | 0.12x |
| Permute | 871 ms | 227 ms | 1,675 ms | 0.52x | 0.14x |
| Queens | 657 ms | 123 ms | 1,060 ms | 0.62x | 0.12x |
| Sieve | 562 ms | 306 ms | 1,213 ms | 0.46x | 0.25x |
| Storage | 1,492 ms | 698 ms | 1,637 ms | 0.91x | 0.43x |
| Towers | 907 ms | 203 ms | 1,564 ms | 0.58x | 0.13x |
| **Total** | **16,416 ms** | **5,046 ms** | **25,626 ms** | **0.64x** | **0.20x** |

- Octane's zlib takes 0.19 times QuickJS's time, 1,617 ms, and 0.07 times
  with the JIT, 582 ms (1,593 and 849 at `b5df63ea`). zlib is an asm.js
  module, compiled from C by Emscripten, and its 51 functions pass the
  validator (`b920ca25`): each runs as typed code (`047ee156`,
  `d8cbc918`), whose int32 and float64 registers hold raw words, so its
  `| 0` arithmetic, its compares and its heap's loads and stores convert
  nothing and check no kind; as ordinary register code, before v0.39.0, it
  took 5,675 ms, converting each element read and each `| 0` through a
  float. With the JIT the typed code is compiled at the module's link, as
  V8 compiled validated asm.js at its instantiation, with no guard on any
  kind (`bc00f894`, `5ae2317e`). Since `b5df63ea` a phi and its arguments
  share a spill slot wherever no two of them are live at once, not only
  where their intervals do not meet, as V8's allocator merges spill ranges
  (`ebe73f44`): zlib's largest function, 47 loops entered by on-stack
  replacement with some 240 values live through each, had copied its phis
  slot to slot at every edge, and its code went from 1.80 MB to 439 KB.
  Made for every pair before the allocator's scan, the classes had made
  its phase of hints four to six times as long; they are now made only
  for the pairs of a component one of whose values spills, when the scan
  first spills it (`4eff9cf3`). A run of all of a suite's compiles,
  timed with `-jitcompile`, spends 1.2 to 1.4 times as long on the hints
  and the scan together as at `b5df63ea` (PdfJS 7.0 to 9.7 ms, Mandreel
  10.5 to 14.5, zlib 16.6 to 20.7, Typescript 24.1 to 34.1), and takes
  as long in all for PdfJS, less for zlib, 3% longer for Gameboy, 6% for
  Mandreel and 8% for Typescript, which makes 3% more compiles.
  The heap's base and length are loaded once outside a loop that calls
  nothing, kept round a loop entered from an entry, and live in their
  spill slot, read in place by each access, taking no register
  (`47f6294a`, `6956a938`, `948a2cb1`, `6fb4805f`); a call through a
  function table of more than four functions is made by index
  (`6fbcf73b`); and a branch too far for 16 bits makes only itself far,
  where it made the whole function's jumps far, or left the function
  without typed code (`5d69103e`). The JIT also inlines a typed callee
  into typed code, an exit inside it writing its raw slots raw
  (`4ae95d8b`, `de072cfa`), and takes a module function's result as the
  number its type says, unchecked (`a4bfc824`): `asm_call_leaf`, a call
  of a small function a turn, takes 5.67 ns with the JIT, where it took
  13.3 in v0.39.0. The stack JIT of v0.38.0 took 2,027 ms, measured in
  `3ecb8200`'s session.
- Mandreel, compiled from C++ too, is not asm.js and runs as ordinary
  register code: 2,514 ms, 0.75 times QuickJS's time, and 878 with the
  JIT. Its largest function is compiled at its first call, within the
  timed run (`d6ac45be`). Its `| 0` arithmetic runs as int32 instructions
  (`add_i32`, `add_imm_i32`, the bitwise operators with an immediate) and
  its typed arrays' elements are read and written by native handlers, but
  each element read and each `| 0` still converts through a float: raw
  registers outside asm.js are designed and not built
  (register-interpreter.md, "Known limits and open work").
- Without the JIT, go-quickjs is ahead on every benchmark of both suites.
  Octane's CodeLoad, which is mostly compiling, takes 0.98 times QuickJS's
  time, and 1.14 with the JIT, where it took 0.98 and 1.07 at `b5df63ea`
  and 1.22 and 1.37 at `258b4e96`, before the front end compiled to
  register code alone (`d6ac45be`). On Are We Fast Yet the interpreter is
  22% ahead on DeltaBlue and on Havlak, a third on Richards, 38% on
  Queens and 42% on Towers. A call's window of registers is the
  callee's frame, so nothing is copied, a frame record is written only
  where a reader needs it, and on amd64 and arm64 a call, a construction
  and a return are native handlers that never enter Go
  ([Calls](#where-the-time-goes) below). The programs that allocate most
  are the least ahead: Storage 9%, Havlak 22%. Since `b5df63ea` the
  interpreter's Octane total is 1.2% lower, 7,191 to 7,105 ms: RayTrace
  26.2 to 21.1 ms, from the functions' `prototype` its classes read, now
  cached (`6521aac5`), and Typescript 7.8%, 1,609 to 1,482 ms. A site that
  has given up its cache of shapes had read and written natively only an
  object with no index of its keys, and exited to Go for a larger one --
  Typescript's syntax tree nodes, 3.07 million of its 3.11 million such
  exits, and 261 thousand writes; the native handlers now find such a
  property by the object's index, as Go's lookup does (`a8c984b4`), and
  the caches' absent keys took its other reads that exit from 73,586 to
  11,624. The Are We Fast Yet totals are level, 16,447 to 16,416 ms and
  5,061 to 5,046 with the JIT, Storage with the JIT 726 to 698. Octane's
  total with the JIT is 4.2% lower, 5,133 to 4,915 ms, almost all of it
  zlib's. Octane's EarleyBoyer and Splay with the JIT move between runs
  more than between the revisions: 45.3 to 58.8 ms and 76.9 to 99.6 ms in
  the JIT's three runs, 47.4 to 65.2 and 81.7 to 109 in `b5df63ea`'s.
- With the JIT, Are We Fast Yet takes 0.06 to 0.44 times QuickJS's time,
  its loops of arithmetic the least, its programs that allocate most --
  DeltaBlue, Storage -- the most. Richards' runs with the JIT fall in two
  groups, of about 650 to 680 ms and of 745 to 940: its three in the
  matrix took 657, 658 and 664 ms, and at `b5df63ea` 661, 657 and 658.
- When the stack JIT was removed, its timing had the register JIT slower
  than the stack JIT just before it on Octane's RayTrace (+7.6%) and
  CodeLoad (+25%) and on Are We Fast Yet's Json (+8.2%), Permute (+7.0%)
  and Queens (+6.3%) (docs/performance-plan.md, "The register JIT's
  remaining gaps"). Against the stack JIT's figures at `acffb1aa` (270,
  227 and 119 ms), Json is 5% shorter, 257 ms, Permute level, 227, and
  Queens 3% longer, 123 ms; CodeLoad took 14.2 ms then and takes 14.6.
  RayTrace took 32.2 ms, its runs with the JIT 31.2 to 33.9 ms, where
  they took 33.0 to 34.0 at `b5df63ea`.
- On eight of Octane's fifteen the JIT is behind the interpreter:
  Richards (68% behind), RayTrace (53%), DeltaBlue (48%), PdfJS (41%),
  Typescript (34%), RegExp (17%), CodeLoad (17%) and Box2D (14%).
  Octane's fixed work is one run of each benchmark, so compiling counts,
  and the code runs in the interpreter until it is hot; a runtime with
  the JIT keeps the frames the plain interpreter's calls and
  constructions do without. The JIT's own figures are close to what they
  were at `b5df63ea` -- Richards 4.6 ms then, DeltaBlue 7.5, RayTrace
  33.2, PdfJS 261, Typescript 1,990 -- and the interpreter's are as fast
  or faster, RayTrace's by a fifth and Typescript's by 8%, which leaves
  less for the JIT's code to win back. In eight fresh-process runs of
  each build, alternated over four placements, Richards with the JIT took
  4.9 ms by the median against 4.8, DeltaBlue 7.8 against 7.9, Mandreel
  856 against 862 and Typescript 1,985 against 1,988; PdfJS took 2.6%
  longer, 257 against 251, its compiles above. RegExp with the JIT took
  135 ms in the matrix where it took 132, its three runs 127 to 139, and
  131 against 128 in those alternated runs: a fixed run of it takes about
  5 ms more where a collection falls within it, and whether one does
  moves from run to run (runs of it alone took 125 to 135 ms in either
  build, with 125 to 129 ms of CPU where none fell and about 200 where
  one did). Native code's calls of `exec` now make their result arrays
  again in the memory of the site's last (`e9bf2365`): it allocates 43.4
  MB a run where it allocated 54.8, and over four placements of
  `7f8205ec`'s build and `b5df63ea`'s its warm runs take 4.7% less time,
  49.0 ms against 51.5.

### Server rendering

The ssr suite renders a page with React 18's own builds, as a host behind
a web server does for each request: Render is one runtime rendering the
page again and again, FirstRender five fresh runtimes, each loading React
and rendering once. The same matrix, the median of three fresh-process
runs (lower is better):

| ssr | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| Render | 1,086 ms | 929 ms | 1,887 ms | 0.58x | 0.49x |
| FirstRender | 150 ms | 281 ms | | | |

Render took 0.61 and 0.53 times QuickJS's time at `b5df63ea`, 1,145 and
987 ms, and FirstRender 154 and 288 ms. The interpreter's Render is 5.2%
shorter, its three runs 1,075 to 1,090 ms where they took 1,141 to
1,166, and with the JIT 5.9%, 910 to 940 ms where they took 974 to 990;
FirstRender is 150 and 281 ms.
A props object's key that some of a site's objects lack is now cached
beside the shapes that have it, and so is a function's `prototype` once
made (`6521aac5`), which took the render's reads that exit to Go from
106,527 to 35,402, its first render's from 10,596 to 3,724. Most of the
rest were `defaultProps`, which a render reads of every element's type:
a tag's name is a string, whose prototypes have no such key, and the read
walked String.prototype's table and Object.prototype's each time. A key
a primitive's prototypes lack is now remembered with the chain's cell,
which a change to any of them invalidates (`18acbcb9`).
A render helper's `return element(...)` is a callback that
returns a call in tail position, which a built-in enters directly
(`400fc19c`). v0.38.0, the stack design, took 0.85 and 0.58 times, and
FirstRender 197 and 366 ms, in `3ecb8200`'s session. React writes a page
as thousands of short
chunks appended to a string a closure holds, and an append grows that
string where it is, when nothing else can see it (`00ce03b5`). An empty
literal learns the room its arrays grow to (`c62a9bb0`), and a for-in
takes the last loop's cursor once that loop has finished (`5326a3f8`).

QuickJS's runner loads a script once, so it has no FirstRender. A fresh
runtime's render is where the JIT costs most: these compiles are made on
the runtime's goroutine, which waits for each, as a runtime made without
`WithJITBackground` does; `qjs --jit` compiles in the background. Since
v0.39.0 a compile made in the background is installed once the native
calls of the code it replaces are done, where it was thrown away
(`7812928f`), a native caller's ask installs a callee's finished compile
(`f1a05866`), a compile's snapshot asks nothing of callees no site could
inline (`39bdcec8`), and an asm.js module's link compiles on the spot what
the background has no room for (`5307e6a0`); the figures here are of
compiles made on the spot, which none of these changes.

### Micro-benchmarks

QuickJS's own micro-benchmarks, [`tests/microbench.js`](https://github.com/bellard/quickjs/blob/master/tests/microbench.js),
time one operation each, in nanoseconds. Each test was given the work QuickJS
takes about 150 ms for, all three ran that same work in fresh
processes -- the plain build and QuickJS at `7f8205ec`, the JIT at
`329ee166` -- and the figure is the fastest of three runs. The runners'
micro mode does this, from a copy of the file kept in `internal/v8bench`, and
the matrix above runs it. `sort_bench` is left out: it reports a run's time
over its fastest sort, not the time of an operation. Over the other 71 tests
go-quickjs takes 0.48 times QuickJS's time by the geometric mean, where
it took 0.49 at `b5df63ea`, and v0.38.0, the stack design, 0.79 in
`3ecb8200`'s session: it is slower on 6, faster on 63, and within 5% on
the other two. With the JIT it takes 0.27 times QuickJS's time, where it
took 0.28, slower on 3, faster on 65 and within 5% on 3. The table has
the interpreter's largest gaps either way:

| Test | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| `weak_map_delete` | 365 ns | 442 ns | 249 ns | 1.46x | 1.77x |
| `bigint256_arith` | 184 ns | 108 ns | 127 ns | 1.45x | 0.85x |
| `weak_map_set` | 127 ns | 92.7 ns | 93.5 ns | 1.36x | 0.99x |
| `int_to_string` | 56.8 ns | 47.2 ns | 49.1 ns | 1.16x | 0.96x |
| `date_parse` | 960 ns | 982 ns | 835 ns | 1.15x | 1.18x |
| `func_closure_call` | 35.8 ns | 2.81 ns | 31.9 ns | 1.12x | 0.09x |
| `prop_create` | 50.0 ns | 42.0 ns | 51.3 ns | 0.98x | 0.82x |
| `regexp_utf16` | 246 ns | 257 ns | 259 ns | 0.95x | 0.99x |
| `array_prop_create` | 13.9 ns | 16.0 ns | 14.9 ns | 0.93x | 1.07x |
| `float_toExponential` | 162 ns | 142 ns | 182 ns | 0.89x | 0.78x |
| `global_func_call` | 8.11 ns | 2.11 ns | 33.0 ns | 0.25x | 0.06x |
| `prop_update` | 2.81 ns | 1.88 ns | 11.6 ns | 0.24x | 0.16x |
| `func_call` | 6.75 ns | 0.95 ns | 31.0 ns | 0.22x | 0.03x |
| `array_length_decr` | 12.7 ns | 11.3 ns | 59.5 ns | 0.21x | 0.19x |
| `array_for_in` | 15.1 ns | 12.4 ns | 76.4 ns | 0.20x | 0.16x |
| `string_build2c` | 26.5 ns | 27.6 ns | 142 ns | 0.19x | 0.19x |
| `array_hole_length_decr` | 12.4 ns | 10.4 ns | 70.0 ns | 0.18x | 0.15x |
| `array_for_of` | 7.33 ns | 5.02 ns | 46.3 ns | 0.16x | 0.11x |
| `array_slice` | 4.28 ns | 4.33 ns | 27.1 ns | 0.16x | 0.16x |
| `prop_clone` | 11.0 ns | 9.45 ns | 72.5 ns | 0.15x | 0.13x |

Of the gaps the stack design had, the largest are gone: in v0.38.0,
measured in `3ecb8200`'s session, `array_prop_create` took 2.25 times
QuickJS's time and takes 0.93, `global_read` 1.79 and 0.85, `array_read`
1.41 and 0.86, `empty_do_loop` 1.34 and 0.53.

The JIT does the most for a test that is a loop of arithmetic, locals,
property reads and writes, array elements and calls: `prop_read` takes
0.82 ns with it, `func_call` 0.95 ns, a fifth of the interpreter's time
or less. A test whose time is a built-in function's work runs much as it
does in the interpreter: regular expressions, `array_slice`, and the
conversions between numbers and strings. An object a function makes and
drops need not be made at all, even where it crosses a merge or goes round
a loop, as in `sum = sum.add(v)`: it is made only where it escapes, by
partial escape analysis, as in Graal (`41575a42`). `escape_accumulate`
and `carried_short` take 0.06 and 0.04 times QuickJS's time with the JIT,
and 0.50 and 0.47 without it. Code that keeps its objects gains nothing
from this, and the suites above hardly move for it.

The interpreter has gained more than the JIT, so some tests run slower
with the JIT than without it. In the run made for this page, 9 of these
71 took 7% or more longer with the JIT: `prop_delete`, `array_update`,
`array_prop_create`, `global_destruct`, `global_destruct_strict`,
`weak_map_delete`, `string_build1`, `string_build_large1` and
`string_to_int`; no longer `local_destruct` and `string_build2c`
(below). Of the extra tests below that QuickJS runs, 27 did, where 30
had at `b5df63ea`: `generator_delegate` and three of the async tests,
`hasOwnProperty` called through a borrowed reference three ways, a push
of five values, a spread call and a spread of a large object,
`object_rest`, four of the sorts, a for-in over a dictionary, a global
`match`, a case-insensitive search, `carried_exit_loops`, four JSON
tests of instances, getters, new layouts and a prototype changed
between calls, `array_index_append`, `template_numbers`,
`prop_delete_last` and `array_map_tail_args`; no longer `async_chain`, a
push of three values, `indexOf` of an array, a sticky regular
expression, a search of UTF-16 text, nor the JSON tests of exponents and
additions.

Since `b5df63ea` the gains are mostly the JIT's. A builder's append of
two strings goes straight to the JIT's append, which checks the length
limit itself, where it first asked of each operand whether it was a
BigInt, then its kind and length (`64bb7164`): with the JIT
`string_build1` takes 29.6 ns where it took 40.8, `string_build2c` 27.6
where it took 40.9 and `string_build_large1` 32.8 where it took 45.6,
though the interpreter's 26.5 ns is still ahead on the first two. A
destructuring in native code takes the interpreter's last cursor once its
pattern has closed it, and copies a rest of dense elements without a
frame (`1d50f73e`): `global_destruct` takes 27.8 ns with the JIT where
it took 34.6, and `local_destruct` 15.7 where it took 20.7, level with
the interpreter's 16.0. A push of several values is native, grown by
push's leaf where the array has no room (`aad6f0b1`):
`array_push_args3` takes 56.0 ns with the JIT where it took 76.0, and
`array_push_args5` 80.1 where it took 96.3. Ordinary native code calls
an asm.js module's typed code natively, where each such call had been a
call of Go (`d14b7cea`), and a typed call's int result is truncated with
no check (`56e02a31`): `asm_fib` takes 9.93 ns with the JIT where it
took 13.4; with the heap's base and length kept out of the loops
(above), `asm_dot` takes 2.67 ns where it took 3.22, `asm_copy` 1.50
where it took 2.01, `asm_adler` 1.67 and `asm_switch` 2.57 where they
took 1.88 and 2.96. In the interpreter `closure_leaf_miss`, whose site
calls fresh closures of two templates in turn, takes 297 ns where it took
350, 0.96 times QuickJS's time where it took 1.14: a closure's capture
list is filled directly, a list of five to seventeen captures embedded
in the function object (`648d7d30`), the module's
environment is shared through the template (`341d1e7b`), and a fresh
closure is made without clearing the fields already empty (`7c15bfa9`).
A global read whose only use is an `instanceof` is one native handler
with its branch (`03dadba8`). The rest since `b5df63ea` is meant to move
no figure here: a direct eval's vars in the body's own scope where the
parameter list has closures (`b9ece15f`, `b86d91f9`; KI-79), and a
scope's lexical bindings collected and checked in linear time
(`9954214d`).

Some of these tests depend on where the linker places the code more than
on the code. The layout lock keeps the interpreter's hot code where it
measured best, but the code between its anchors, the Go handlers after the
loop and the closures still move with every change to the package, and a
change of a few per cent in one of these tests is not by itself a change in
the engine. A pad that grows past a page moves everything after it 4 KB,
every offset modulo 4 KB the same (docs/optimizing.md, "The layout
lock"); since `b5df63ea` the anchors after the native handlers' block,
after package regcomp and, with the JIT, after package mir are pages
further on, each offset modulo 4 KB its target, as the lock means them to
move (`16172f2e`, `ba411043`, `7c739713`, `c35e6dd2`).

With the JIT, `string_indexOf` and `string_concat` take 86.7 and 120 ns
in the matrix where they took 77.7 and 104, and over four placements of
each build, loaded at a fixed base, 6.3% and 7.5% longer:
`string_indexOf` at three of the four, `string_concat` 2.9 to 11.5% at
each. Their code is the same. They call String's methods from the JIT's
Go side, in the range between the lock's anchors J and N, which the lock
does not hold and this round's JIT changes grew (`c35e6dd2`,
`e9bf2365`): builds of the commits between `b5df63ea` and `7f8205ec`, at
one placement, take 78 to 92 ns on `string_indexOf` with the JIT, as
what lies before its code there grows and shrinks, and `string_trim`,
4.8% slower over the placements at `7f8205ec`, is level at `329ee166`
(+0.5%). `has_own_call_for_in`, which calls a borrowed `hasOwnProperty`
from native code through the same Go side, is 7.4% slower over the
placements, 4 to 15% at three and 4% faster at the fourth. Ten more rows
with the JIT are 7 to 19% slower in the matrix's run and within 2.8%
over the placements: `string_slice`, `string_starts_with`,
`dict_for_in`, `array_length_decr`, `array_hole_length_decr`,
`map_set_bigint`, `math_random`, `bigint64_search`,
`object_spread_large` and `mega_add`. The interpreter's rows are within
2.5% over the placements.

The rows `b5df63ea`'s page left slower or open:

- `json_stringify_decimals` takes 34.1 ns (34.2 at `b5df63ea`, 32.9 at
  `b8c2c45f`), `json_stringify_ints` 25.0 (25.8, 24.8), and with the JIT
  `json_stringify_unique` 123 (122, 114): the encoder's stretch is where
  it was;
- `string_indexOf` takes 91.4 ns (89.9, 82.6), level with `b5df63ea`'s
  over the placements, `json_stringify_strings` 30.9 (31.7, 30.3), and
  with the JIT `array_indexOf` 105 (118, 102);
- `json_stringify_dict` takes 83.3 ns (93.3, 80.0) and
  `string_starts_with` 75.4 (73.5, 69.0), level over the placements; with
  the JIT `string_template` 181 (188, 172), `string_code_point_at` 5.17
  (6.00, 5.41), `has_own_call_miss` 90.4 (97.2, 78.3) and
  `string_from_code_point` 80.8 (87.2, 75.1);
- `generator_next` takes 102 ns (103), `dataview_write` 61.4 (67.1) and
  `array_literal_empty` 71.1 (70.9);
- Are We Fast Yet's Storage takes 1,492 ms (1,492), and 698 with the JIT
  (726).

The runners also run `microbench_extra.js`, tests of what microbench.js has
none of, written as its tests are: generators and async functions, try
statements, iterators, using declarations, closures made and called anew,
calls in tail position, method calls with a frame and without, loops of a
few simple instructions, private class members, `Math.random`, the `in`
operator, `delete`, the array methods that take a callback, `indexOf`,
String's methods that run no script, Math's imul, clz32, pow, atan2 and
hypot, a template and a long chain of calls joined, for-in over an object
and over a dictionary, what classes do -- a list walked, small methods
moving objects between arrays, a method called for each element, callbacks
made in a method -- BigInts' division, shifts, hashing, powers and decimal
strings, and regular expressions as programs use them: validating,
tokenizing, replacing, splitting, named groups and the u flag -- and
since v0.31.1 switch statements of strings and numbers and `typeof`'s
tests, typed arrays of every element type and DataViews, calls of a
borrowed `hasOwnProperty`, pushes of several values, object literals and
strings appended to a captured binding -- and since v0.32.0 objects made
on prototypes made and dropped, classes made per call, and objects a
function's own code alone sees -- and since v0.35.0 strings grown in
place, arrays past 16,384 elements, objects a loop carries, and 49 tests
of `JSON.parse` and `JSON.stringify` -- and since v0.39.0 Crypto's `am3`,
classes constructed and tested with `instanceof`, the kernels of an asm.js
module -- its loops over its heap of bytes, words and doubles, its calls,
a switch and a function table -- and a call from plain code into typed
code -- and since then the calls React's render helpers make: fresh
closures of two templates called in turn by one site, a `map` callback
that returns a call in tail position, and `charCodeAt` called where its
method is read before its argument -- and since v0.40.0 `codePointAt`,
`at` and `charAt` called as methods -- and since v0.41.0 reads and
additions at sites that meet a new layout at every call. Over the 346
QuickJS can run (it has no using declarations) go-quickjs takes 0.50
times its time, and 0.23 times with the JIT, as it did at `b5df63ea`
(0.497 and 0.227 where it took 0.503 and 0.229), and v0.38.0 0.70 and
0.30 over its 313; leaving out the JSON tests, 0.54 and 0.22. The asm.js
kernels' loops (`asm_adler`, `asm_crc32`, `asm_copy`, `asm_fnv`,
`asm_dot`, `asm_switch`) take 0.12 to 0.29 times QuickJS's time as typed
code, and 0.03 to 0.07 with the JIT, which compiles them at the module's
link; their recursion and calls (`asm_fib`, `asm_call_leaf`) 0.81 and
0.55, 0.22 and 0.09 with the JIT, which inlines a typed callee
(`4ae95d8b`) and calls typed code natively from the ordinary code that
drives it (`d14b7cea`), and a call through a function table 1.04, 0.16
with the JIT. A switch on strings past its first few cases
takes 0.05 to 0.15 times QuickJS's time, its literal cases one lookup
(`100eb7f`). An await costs a fiftieth of QuickJS's time (`async_await`
15.4 against 746 ns): an async function resumed by its await's job goes
on in place at an await of a value that needs no job of its own. A
generator resumed from for-of is level (`generator_for_of` 1.03), where
v0.38.0 took 1.44 times, and a for-in over a dictionary takes 0.22 times,
where it took 1.41: its keys are made once for the dictionary's layout and
handed out by the native handlers. Calls in tail position take 0.55 to
0.60 times QuickJS's time, and a `map` callback that returns a call in
tail position 0.46, level with one that returns an ordinary call: such a
callback is entered directly, as any other is (`400fc19c`). A call of
`charCodeAt` whose method is read before its argument, as in
`s.charCodeAt(++i)`, is answered by the call handler from its target, as
V8's call feedback does (`b90b70c4`): `string_charcode_update` takes 0.31
times QuickJS's time. A site that calls fresh closures of two templates in
turn misses its cache at each call and retries the leaves the native call
handler answers (`ab16fd73`); `closure_leaf_miss` takes 0.96 times, where
it took 1.14 (above). A global `exec` still takes 1.33 times, as it makes
a result and a String for each match; a sticky one 1.03. The largest
gaps either way:

| Test | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| `scope_chain` | 661 ns | 674 ns | 351 ns | 1.88x | 1.92x |
| `set_proto_fresh` | 373 ns | 375 ns | 211 ns | 1.77x | 1.78x |
| `array_literal_empty` | 71.1 ns | 63.0 ns | 41.9 ns | 1.70x | 1.50x |
| `subclass_fresh` | 1,144 ns | 1,193 ns | 681 ns | 1.68x | 1.75x |
| `typed_u8_find` | 45.0 ns | 9.31 ns | 27.4 ns | 1.64x | 0.34x |
| `array_fill_down` | 139 ns | 141 ns | 86.4 ns | 1.61x | 1.63x |
| `object_create_fresh` | 344 ns | 347 ns | 226 ns | 1.52x | 1.53x |
| `typed_find` | 46.5 ns | 9.54 ns | 30.9 ns | 1.51x | 0.31x |
| `typed_findIndex` | 46.5 ns | 9.05 ns | 30.9 ns | 1.50x | 0.29x |
| `typed_u64_reduce` | 108 ns | 109 ns | 75.9 ns | 1.42x | 1.44x |
| `asm_copy` | 5.73 ns | 1.50 ns | 48.3 ns | 0.12x | 0.03x |
| `string_switch_last` | 27.6 ns | 13.0 ns | 266 ns | 0.10x | 0.05x |
| `json_stringify_decimals` | 34.1 ns | 35.4 ns | 333 ns | 0.10x | 0.11x |
| `string_switch_large` | 29.2 ns | 21.6 ns | 411 ns | 0.07x | 0.05x |
| `string_switch_miss` | 17.7 ns | 12.5 ns | 269 ns | 0.07x | 0.05x |
| `regexp_search_utf16` | 679 ns | 722 ns | 13,184 ns | 0.05x | 0.05x |
| `string_switch_nonstring` | 12.3 ns | 7.42 ns | 265 ns | 0.05x | 0.03x |
| `regexp_search_i` | 675 ns | 728 ns | 20,972 ns | 0.03x | 0.03x |
| `json_parse_long` | 1,822 ns | 1,870 ns | 85,582 ns | 0.02x | 0.02x |
| `async_await` | 15.4 ns | 17.8 ns | 746 ns | 0.02x | 0.02x |

A BigInt in the int64 range is held in the value itself (`9746906`), as
QuickJS holds one that fits in a word, and so is one below 2^64 in
magnitude (`f8c3229`); a larger one is a block of no pointers, made by the
engine's own arithmetic (`f1bf681`). Short BigInts' `+`, `-`, `*` and
comparisons are native handlers: `bigint64_arith` takes 0.36 times
QuickJS's time. A larger one is made by the engine's arithmetic in Go:
`bigint256_arith` takes 1.45 times, and the extra BigInt tests 0.42 to
1.24 times, `bigint_pidigits`, `bigint_toString`, `bigint_modpow` and
`bigint64_xorshift` the slowest. The JIT makes BigInts of 64 bits natively: `bigint64_arith`
takes 0.10 times QuickJS's time with it, `bigint64_xorshift` 0.05.

The largest gaps now are:

- objects made on a prototype that is itself new -- `Object.create` of a
  fresh object, `Object.setPrototypeOf` to one, a scope chain built as
  objects, a subclass made per call and its instance -- 1.52 to 1.88
  times QuickJS's time;
- arrays made empty or grown downward from past their end
  (`array_literal_empty`, `array_fill_down`);
- the typed arrays' `find` and `findIndex` in the interpreter, and the
  64-bit typed arrays' reads and callbacks, whose elements are BigInts;
- the weak collections. A WeakMap holds each value on its key rather than
  in a table of its own (`2bc1620`), so that a value which refers to its
  key does not keep the key alive, as the specification requires; adding
  an entry costs more for it;
- a global `exec`, which makes a result and a String for each match, and
  a `replace` by a template with `$` (1.33 and 1.32 times).

The suite as a whole does better than the operations it is made of
because of the places go-quickjs leads by most: strings and regular
expressions over whole texts, which Go's runtime and the engine's matcher
do well, and Splay's allocation-heavy setup.

#### Where the time goes

- **Dispatching instructions.** Register code names its operands in the
  instruction, so an expression is one instruction rather than the pushes
  and pops of a stack machine: Crypto's `am3` runs 25 register
  instructions a turn where the stack form ran 34. On amd64 and arm64
  each instruction is run by a native handler that ends in a dispatch of
  its own -- the next instruction read, its handler's address loaded from
  the table, an indirect jump: four instructions -- with the interpreter's
  state pinned in machine registers across it, which Go cannot do: in Go
  every handler returns to a loop and is called again, its state stored
  and reloaded around the call. 134 of the 245 operations have a native
  handler; the rest, and every fast path that misses, go to their Go
  handler through an exit that calls it in Go's convention
  (register-interpreter.md, "Native handlers"). The six loops of simple
  instructions -- `empty_loop` and the extra tests `loop_sum`,
  `loop_arith`, `loop_locals`, `loop_elements` and `loop_props` -- take
  0.21 to 0.67 times QuickJS's time, 0.42 by their geometric mean, where
  v0.38.0, its stack interpreter and its tree tier, took 0.45 to 1.15 and
  0.75 by the mean. `int_arith` takes 0.47 times QuickJS's time and
  `float_arith` 0.35.
- **Calls.** A call's window of registers is the callee's frame: the
  caller writes the callee, the receiver and the arguments there, and
  nothing is copied. A callee the call site's cache knows is entered with
  no question asked again, and only the fields of the frame record a
  reader needs are written. Natively, a call, a construction and a return
  are handlers of their own that keep the caller's state on a native
  frame stack and never enter Go (register-interpreter.md, "Frames and
  calls"). A method call whose body needs a frame takes 33.8 ns against
  70.1 in QuickJS (`method_call`), one that only returns its argument
  12.4 against 43.0 (`method_call_leaf`), an empty function 6.75 against
  31.0 (`func_call`); with the JIT they take 11.8, 2.66 and 0.95 ns. A
  recursive call is level: `call_fib` and `call_mutual` take 0.93 and
  0.92 times QuickJS's time. A body that only returns a parameter or a
  constant, or a short pure one of numbers and upvalues, is answered at
  the call site without a frame; natively, so are a getter of `this.k`,
  `this.k.length` or `this.k[p]`, a `typeof` test, an initializer that
  stores its parameters in `this`, and a constructor that only forwards
  to one (`d54099e3`, `09409332`, `bdab5ba4`); the initializer writes its
  values at the slots, and gives the final layout, that it remembers from
  the last construction of the same layout (`8505335a`): `class_new`
  takes 0.44 times QuickJS's time. `Object.create` of a prototype alone
  is a leaf too, which the interpreter and native code call without a
  frame (`ffb23541`).
- **Property access.** Objects built the same way share a shape, as in
  QuickJS and V8, and each property read and write, global variable read
  and write, call and `instanceof` remembers where it last found what it
  looked for; a site that meets a second shape keeps up to four, natively
  for a write too (`defbf92d`), and one that keeps meeting new shapes
  reads through the realm's shared cache, as V8's megamorphic stub cache
  does, in sets of four ways (`46370076`), and the native handlers probe
  that cache and the realm's cache of additions themselves (`5916704b`),
  finding an own property of an object whose keys have an index as Go's
  lookup does (`a8c984b4`); a key some of a site's objects lack is kept
  beside the shapes that have it, and a key a chain lacks is remembered
  by the chain's cell (`6521aac5`, `18acbcb9`); a write site that keeps
  several shapes adds to each of them natively (`d44da58c`); an
  `instanceof` site keeps the constructor's prototype slot too
  (`32036a73`), and a branch on its result is the same instruction
  (`1dcf124c`), with the read of a global constructor before it too
  (`03dadba8`). A keyed access converts
  its key through the runtime's table of key strings, also in sets of
  four ways (`6deb2e51`), and a keyed store remembers the shapes and keys
  its site met (`f04364dd`). Two adjacent reads of properties share one
  native dispatch (`4d5d636d`). The native handlers check the cache where
  the instruction runs: `prop_read` takes 0.49 times QuickJS's time,
  `prop_write` 0.33, `global_read` 0.85, `global_write` 0.39,
  `instanceof_class` 0.24, and `mega_read` and `mega_add`, at sites that
  meet a new layout at every call, 0.74 and 0.67. An object holds only its values, its
  keys in its shape, as a QuickJS object does (`944e2e8`).
- **Memory management.** QuickJS counts references: an object is freed the
  moment the last reference to it goes, and a cycle collector handles the
  rest. go-quickjs relies on Go's garbage collector. It traces the live heap
  concurrently and needs a write barrier on every pointer stored. The
  native handlers store a number without one, and a pointer through the
  barrier's routine only while the collector marks; an initializer's short
  run of stores checks once whether it marks (`45057224`), and native code
  keeps that flag in a register between its calls of Go (`dc7ddd31`). A
  small object's header is written in its allocation (`7890878f`), and a
  literal of two properties writes its values in place (`f10dbb36`). The
  collector does most of its work on other cores, so with cores to spare
  it costs less wall-clock time than CPU time. A program that allocates
  heavily still pays for it in CPU time: Storage and Havlak, which
  allocate most, are the least ahead. In a process of one P, where a mark
  phase lasts four times its work and the script runs under the barrier
  all that while, a runtime made with `WithGCYield` waits while the
  collector marks (`ec1bdc6e`); the figures here, with cores to spare, are
  of runtimes made without it.
- **Numbers as Values.** Outside asm.js's typed code every register holds
  a Value, so an element read and a `| 0` convert through a float where an
  int32 would do; raw registers for other code are designed and not
  built.

Where go-quickjs is ahead by most:

- **Crypto.** Its arithmetic is register code whose bitwise operators have
  forms with an immediate, and `(x + y) | 0` is one instruction, all
  native handlers:
  `crypto_am3` takes 0.69 times QuickJS's time, 0.20 with the JIT, and
  Crypto 0.48.
- **Code compiled from C to asm.js.** A validated asm.js module runs as
  typed code, its int32s and float64s raw in their registers, its heap's
  loads and stores natively, and with the JIT is compiled at its link with
  no speculation (`d8cbc918`, `5ae2317e`): Octane's zlib takes 0.19 times
  QuickJS's time, 0.07 with the JIT, and the asm.js kernels' loops 0.12 to
  0.29, 0.03 to 0.07 with it.
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
  one character in a single step. A match whose result nothing reads makes
  no array, and a `replace` whose string is only measured makes its length
  alone. In a runtime with the JIT, a pattern is compiled to machine code
  at its second match, on amd64 and arm64, as V8's Irregexp compiles it
  (`2be0068`), and `exec`, `test` and `replace` are called from native
  code without a frame.
- **Strings built by appending.** A closure's `s += x` and a local's
  `out += esc(x)` grow the string in place, in storage with room to spare,
  rather than making a rope (`00ce03b5`): `string_build2c` takes 0.19 times
  QuickJS's time, `local_append_keep` 0.28 and `template_list` 0.34, and
  0.19 times with the JIT on the first of these, where it took 0.29.
- **JSON.** The 49 `json_` tests take 0.30 times QuickJS's time, and 0.31
  with the JIT, as the work is a built-in's. `JSON.parse` walks the shape
  tree as it reads, expecting the keys of the last object parsed in the same
  place, and makes each object once, at its last key. `JSON.stringify` keeps
  a layout's keys and slots from the second time it meets it (`097d71ec`).
  Numbers are read and written without strconv's general path (`baafd8e3`),
  and long strings are scanned with AVX2 or NEON where the machine has them:
  `json_parse_records` takes 0.32 times QuickJS's time,
  `json_stringify_records` 0.15 and `json_parse_long` 0.02.
- **Splay.** Its fixed work is mostly building the tree and dropping
  it. Go's allocator makes many small objects cheaply, a short string or an
  array of up to sixteen elements is one allocation with its contents, and
  dropping the tree costs nothing until the collector runs. An array
  literal of constants is not copied at all until it is written: its
  arrays share the literal's elements (`a2d9f12e`, and natively
  `15a29721`).
- **Array elements updated, walked and skipped.** `a[k] += y` and `a[k]++`
  are one instruction, `add_index`, the element updated where it is, with
  the array and the key checked once (`array_update`, 0.28 times QuickJS's
  time). A for-in over an array whose keys are its elements walks them by
  index rather than copying them into a list first (`array_for_in`, 0.20),
  and the array methods pass over a hole without looking its index up,
  where nothing up the prototype chain could have it; a write of a
  shorter length is a native handler's (`88feca00`;
  `array_hole_length_decr`, 0.18). A typed array's elements are read and
  written by native handlers for every element type but Float16
  (`typed_array_write`, 0.41).
- **`apply(this, arguments)`.** In a function whose only use of
  `arguments` is passing it to `apply` (a common way to write a class
  constructor), the arguments object is never made: the call hands on the
  frame's arguments as they were passed (`apply_args`). A constructor
  whose whole body is `this.initialize.apply(this, arguments)`, as
  RayTrace's are, is constructed by its call site as a forwarder, and
  natively, where the target is an initializer, without entering either
  frame (`746d9438`, `bdab5ba4`): `forward_construct` takes 0.20 times
  QuickJS's time, and 0.02 with the JIT. It took 0.54 at `258b4e96`, 0.23
  at `3ecb8200`, 0.32 at `2f05d3fc`, 0.25 at `c2dae008` and 0.24 at
  `b8c2c45f` and `b5df63ea`.

What remains in the interpreter is mostly in Go: the operations with no
native handler and the fast paths that miss, which exit to their Go
handlers, the built-ins and the object model -- the gaps above are in
them -- and the collector. The JIT is the way past the rest: a hot
function compiled to native code dispatches nothing, which is why, with
it, the loops and calls above take from a third of the interpreter's
time down to a tenth.

[goja]: https://github.com/dop251/goja
[v8-v7]: https://github.com/mozilla/arewefastyet/tree/master/benchmarks/v8-v7
