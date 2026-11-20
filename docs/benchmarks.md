# Benchmarks

The Go benchmarks, bundle timings, and V8 comparison with goja below were
measured at `b37daa23` on the register interpreter and the JIT that compiles
its register code. The comparison with C QuickJS, under [Compared with C
QuickJS](#compared-with-c-quickjs), was measured at `b8c2c45f`. Other
comparisons retain the revisions and measurement dates their sections name.

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
[goja]. All three go-quickjs columns use revision `b37daa23`, measured alongside
goja on 2026-11-16. Each column is the median of three fresh-process runs
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
| Richards | 3,160 | 16,182 | 16,189 | 538 | 5.87x | 30.08x | 30.09x |
| DeltaBlue | 2,860 | 14,877 | 14,956 | 644 | 4.44x | 23.10x | 23.22x |
| Crypto | 5,311 | 22,291 | 22,399 | 350 | 15.17x | 63.69x | 64.00x |
| RayTrace | 6,499 | 35,522 | 36,777 | 849 | 7.65x | 41.84x | 43.32x |
| EarleyBoyer | 8,782 | 14,163 | 14,221 | 1,503 | 5.84x | 9.42x | 9.46x |
| RegExp | 9,110 | 11,410 | 11,376 | 593 | 15.36x | 19.24x | 19.18x |
| Splay | 15,402 | 24,130 | 24,187 | 2,727 | 5.65x | 8.85x | 8.87x |
| NavierStokes | 10,737 | 40,176 | 40,662 | 582 | 18.45x | 69.03x | 69.87x |
| **Composite score** | **6,707** | **20,434** | **20,544** | **782** | **8.58x** | **26.13x** | **26.27x** |

The runner's timed suite interval includes runtime construction, parsing,
compilation, the suite's warmups and its measured iterations, but excludes
reading the source files and the final forced garbage collection. CPU time
includes work on other threads, including the collector. These are the
go-quickjs and goja runs:

| Metric | go-quickjs JIT off | go-quickjs JIT on | go-quickjs JIT background | goja | JIT off relative to goja |
|---|---:|---:|---:|---:|---:|
| Suite wall time | 20.18 s | 20.10 s | 20.10 s | 50.80 s | 2.52x faster |
| Suite CPU time | 23.33 s | 26.21 s | 26.49 s | 71.99 s | 67.6% less |
| Total allocation | 7.39 GiB | 16.40 GiB | 16.70 GiB | 34.91 GiB | 78.8% less |

Score mode does different amounts of work as throughput changes, so the CPU
and allocation totals are not equal-work efficiency measurements. The
composite is the median of the three printed composite scores, not a
composite calculated from the workload medians.

The suite warms every workload for at least one second, then measures for at
least another second and continues until it has 32 measured iterations. All
engines evaluated the same benchmark sources and validation code.
The measured revisions were go-quickjs `b37daa23`, goja `a177d211bf60`, and
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
| JIT off | 6,797 | 6,481 | 6,707 |
| JIT on | 20,434 | 20,508 | 19,322 |
| JIT background | 20,544 | 20,671 | 18,695 |
| goja | 782 | 792 | 764 |

All samples were included. The table takes the median of each workload and
of the printed composite scores independently. Background compilation's
composite was 0.54% higher than synchronous compilation's; this run does not
establish an overall throughput advantage for either compilation mode.

## Compared with C QuickJS

Over the V8 suite as a whole, go-quickjs is well ahead of QuickJS. Its
interpreter takes 0.33 times QuickJS's time and has 2.19 times its
composite score, ahead on all eight workloads. With its JIT it takes 0.22
times QuickJS's time and scores 5.6 times as much. On the larger programs
of Octane and Are We Fast Yet the interpreter takes 0.43 and 0.65 times
QuickJS's time, and the JIT 0.31 and 0.20 times
([below](#octane-and-are-we-fast-yet)); a page React 18 renders on the
server takes 0.62 times, and 0.52 with the JIT
([below](#server-rendering)). Single operations cost about half as much,
0.49 times QuickJS's time by the geometric mean of its micro-benchmarks,
and 0.28 times with the JIT, as [the micro-benchmarks](#micro-benchmarks)
below show; the slowest of them takes 1.5 times as long.

At `b8c2c45f`, the revision measured here, go-quickjs compiles every
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
`5ae2317e`). The page's previous figures, at `2f05d3fc` and, for the
micro-benchmarks, `c2dae008`, whose code v0.40.0 is, were of the same
design before the allocation and dispatch changes and the keyed caches
below. The same suites ran on an AMD Ryzen 5 3600 under Windows, with Go
1.27.3, go-quickjs `b8c2c45f` and QuickJS 2026-06-04. Two runs went at
once, each on the six CPUs of a level 3 cache of its own. The JIT's
column is a build with `-tags quickjs_jit` run with `-jit`. The figures
are milliseconds for the same fixed work, the median of three
fresh-process runs (lower is better):

| Workload | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| Richards | 8.0 ms | 8.6 ms | 11.3 ms | 0.71x | 0.76x |
| DeltaBlue | 14.4 ms | 16.2 ms | 24.1 ms | 0.60x | 0.67x |
| Crypto | 155 ms | 54.4 ms | 331 ms | 0.47x | 0.16x |
| RayTrace | 71.1 ms | 53.2 ms | 144 ms | 0.50x | 0.37x |
| EarleyBoyer | 172 ms | 123 ms | 411 ms | 0.42x | 0.30x |
| RegExp | 148 ms | 133 ms | 868 ms | 0.17x | 0.15x |
| Splay | 124 ms | 97.8 ms | 492 ms | 0.25x | 0.20x |
| NavierStokes | 102 ms | 37.0 ms | 188 ms | 0.54x | 0.20x |
| **Total** | **812 ms** | **543 ms** | **2,491 ms** | **0.33x** | **0.22x** |

The suite's own scores, the median of three (higher is better):

| Workload | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| Richards | 1,397 | 5,658 | 956 | 1.46x | 5.92x |
| DeltaBlue | 1,471 | 5,812 | 844 | 1.74x | 6.89x |
| Crypto | 2,347 | 8,211 | 1,103 | 2.13x | 7.44x |
| RayTrace | 3,274 | 12,492 | 1,540 | 2.13x | 8.11x |
| EarleyBoyer | 3,816 | 4,959 | 2,160 | 1.77x | 2.30x |
| RegExp | 2,878 | 4,035 | 435 | 6.62x | 9.28x |
| Splay | 6,485 | 9,429 | 3,322 | 1.95x | 2.84x |
| NavierStokes | 4,918 | 16,342 | 2,388 | 2.06x | 6.84x |
| **Composite** | **2,937** | **7,516** | **1,339** | **2.19x** | **5.61x** |

At `2f05d3fc` the interpreter took 0.34 times QuickJS's time over the suite
and had 2.08 times its composite score, and the JIT 0.21 and 5.53 times.
The interpreter's total is 6.6% lower, 870 to 812 ms, and its composite
4.7% higher: EarleyBoyer takes 172 ms where it took 209, Splay 124 where
it took 137 and RayTrace 71.1 where it took 77.1, and their scores are
13%, 18% and 7% higher. That is the interpreter's round of allocation
changes. Builds of the revisions between, one after the other on one
level 3 cache, put Splay's step where a repeated short join checks its
pieces before copying them (`af68d128`), a short initializer copies its
arguments into the object directly (`b5c360c3`) and a local the body
writes before it reads it needs no store at the function's entry
(`4ce68337`), and EarleyBoyer's where a small object's header is written
in its allocation (`7890878f`), a literal of two properties writes its
values in place (`f10dbb36`), and an initializer's short run of stores
checks once that the collector is idle (`45057224`) and native code
keeps that flag in a register between Go calls (`dc7ddd31`). A cached
literal is also allocated with its final shape now (`5a96b7cd`). The
JIT's total is level, 539 to 543 ms, its composite 0.7% higher and
Splay's score 11% higher.

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
  RegExp. By that mean the interpreter takes 0.42 times QuickJS's time,
  and the JIT 0.29 times, where their scores are 2.19 and 5.61 times as
  high.
- The JIT's lead is much larger in the scores than in fixed time. A fixed
  run of Richards or DeltaBlue lasts a few milliseconds, and a function
  runs in the interpreter until it is hot enough to compile, so much of
  that run is spent before the JIT's code takes over: with the JIT,
  Richards takes 1.08 times the interpreter's fixed time and DeltaBlue
  1.13 (DeltaBlue's three runs with it took 16.0 to 17.8 ms). The scores
  time warm runs, where Richards, DeltaBlue, Crypto, RayTrace and
  NavierStokes score 3.3 to 4.1 times what the interpreter does.
- Splay's lead is largest in its fixed time. That is almost all setup and
  teardown: building a tree of 8,000 nodes and dropping it, which QuickJS
  frees by counting references where Go's collector frees it later. A warm
  run, which is what the score times, is splay-tree operations, calls and
  property reads, and a new node for each insertion. Each node's payload
  holds array literals of constants, which share their elements with the
  literal until one is written, as V8's copy-on-write arrays do
  (`a2d9f12e`); its score is 95% ahead of QuickJS's.
- The JIT changes least where the time is spent in Go code it calls
  rather than in the script's own: the allocation-heavy EarleyBoyer and
  Splay, 1.30 and 1.45 times the interpreter's score, and RegExp, 1.40
  times, though a runtime with the JIT compiles its patterns to machine
  code as V8's Irregexp does (`2be0068`).

Figures move by a few percent between builds from where the linker places
the code, and a single benchmark by up to a tenth. The layout lock holds
the interpreter's hot code -- the Go table's loop, the call path and the
native handlers' block -- at the offsets it measured best
(register-interpreter.md, "Code layout"), so that a change elsewhere does
not move it; what lies between its anchors still moves. To reproduce
these figures, the suites below and the micro-benchmarks, run `go run
./internal/cmd/v8bench/matrix -engines gq,gqjit,qjs -qjs /path/to/qjs -v8
/tmp/v8-v7 -octane /tmp/octane -awfy /tmp/awfy -ssr /tmp/ssr -fetch`.

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
| Richards | 3.0 ms | 4.7 ms | 3.8 ms | 0.79x | 1.24x |
| DeltaBlue | 5.3 ms | 7.7 ms | 7.6 ms | 0.70x | 1.01x |
| Crypto | 52.5 ms | 22.9 ms | 110 ms | 0.48x | 0.21x |
| RayTrace | 25.7 ms | 34.6 ms | 50.4 ms | 0.51x | 0.69x |
| EarleyBoyer | 61.1 ms | 58.0 ms | 137 ms | 0.44x | 0.42x |
| RegExp | 116 ms | 127 ms | 470 ms | 0.25x | 0.27x |
| Splay | 121 ms | 109 ms | 453 ms | 0.27x | 0.24x |
| NavierStokes | 40.6 ms | 17.9 ms | 65.8 ms | 0.62x | 0.27x |
| PdfJS | 187 ms | 246 ms | 372 ms | 0.50x | 0.66x |
| Mandreel | 2,495 ms | 871 ms | 3,335 ms | 0.75x | 0.26x |
| Gameboy | 392 ms | 305 ms | 458 ms | 0.86x | 0.66x |
| CodeLoad | 12.3 ms | 13.7 ms | 12.9 ms | 0.95x | 1.06x |
| Box2D | 90.1 ms | 101 ms | 152 ms | 0.59x | 0.66x |
| zlib | 1,609 ms | 854 ms | 8,348 ms | 0.19x | 0.10x |
| Typescript | 1,641 ms | 2,049 ms | 2,131 ms | 0.77x | 0.96x |
| **Total** | **7,236 ms** | **5,206 ms** | **16,727 ms** | **0.43x** | **0.31x** |

| Are We Fast Yet | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| DeltaBlue | 569 ms | 313 ms | 721 ms | 0.79x | 0.43x |
| Richards | 3,282 ms | 663 ms | 4,794 ms | 0.68x | 0.14x |
| Json | 1,022 ms | 259 ms | 1,350 ms | 0.76x | 0.19x |
| CD | 1,598 ms | 492 ms | 2,853 ms | 0.56x | 0.17x |
| Havlak | 3,610 ms | 1,170 ms | 4,425 ms | 0.82x | 0.26x |
| Bounce | 765 ms | 249 ms | 1,493 ms | 0.51x | 0.17x |
| List | 551 ms | 160 ms | 1,265 ms | 0.44x | 0.13x |
| Mandelbrot | 177 ms | 40.9 ms | 691 ms | 0.26x | 0.06x |
| NBody | 658 ms | 124 ms | 1,027 ms | 0.64x | 0.12x |
| Permute | 873 ms | 229 ms | 1,686 ms | 0.52x | 0.14x |
| Queens | 659 ms | 123 ms | 1,067 ms | 0.62x | 0.11x |
| Sieve | 563 ms | 318 ms | 1,221 ms | 0.46x | 0.26x |
| Storage | 1,549 ms | 706 ms | 1,676 ms | 0.92x | 0.42x |
| Towers | 885 ms | 202 ms | 1,566 ms | 0.57x | 0.13x |
| **Total** | **16,816 ms** | **5,095 ms** | **25,942 ms** | **0.65x** | **0.20x** |

- Octane's zlib takes 0.19 times QuickJS's time, 1,609 ms, and 0.10 times
  with the JIT, 854 ms (1,591 and 843 at `2f05d3fc`; its runs in the
  interpreter in this session took 1,598 to 1,614 ms). zlib is an asm.js
  module, compiled from C by Emscripten, and its 51 functions pass the
  validator (`b920ca25`): each runs as typed code (`047ee156`,
  `d8cbc918`), whose int32 and float64 registers hold raw words, so its
  `| 0` arithmetic, its compares and its heap's loads and stores convert
  nothing and check no kind; as ordinary register code, before v0.39.0, it
  took 5,675 ms, converting each element read and each `| 0` through a
  float. With the JIT the typed code is compiled at the module's link, as
  V8 compiled validated asm.js at its instantiation, with no guard on any
  kind (`bc00f894`, `5ae2317e`), and the members of a phi group that spill
  share one slot, which took zlib's largest function from 2.13 to 1.81 MB
  of code, its moves at the edges from 1.80 to 1.31 MB (`83a58f53`). The
  JIT also inlines a typed callee into typed code, an exit inside it
  writing its raw slots raw (`4ae95d8b`, `de072cfa`), and takes a module
  function's result as the number its type says, unchecked (`a4bfc824`):
  `asm_call_leaf`, a call of a small function a turn, takes 5.95 ns with
  the JIT, where it took 13.3 in v0.39.0. The stack JIT of v0.38.0 took
  2,027 ms, measured in `3ecb8200`'s session.
- Mandreel, compiled from C++ too, is not asm.js and runs as ordinary
  register code: 2,495 ms, 0.75 times QuickJS's time, and 871 with the
  JIT. Its largest function is compiled at its first call, within the
  timed run (`d6ac45be`). Its `| 0` arithmetic runs as int32 instructions
  (`add_i32`, `add_imm_i32`, the bitwise operators with an immediate) and
  its typed arrays' elements are read and written by native handlers, but
  each element read and each `| 0` still converts through a float: raw
  registers outside asm.js are designed and not built
  (register-interpreter.md, "Known limits and open work").
- Without the JIT, go-quickjs is ahead on every benchmark of both suites.
  Octane's CodeLoad, which is mostly compiling, takes 0.95 times QuickJS's
  time, and 1.06 with the JIT, where it took 0.98 and 1.06 at `2f05d3fc`
  and 1.22 and 1.37 at `258b4e96`, before the front end compiled to
  register code alone (`d6ac45be`). On Are We Fast Yet the interpreter is
  21% ahead on DeltaBlue, 18% on Havlak, a third on Richards, 38% on
  Queens and 43% on Towers. A call's window of registers is the
  callee's frame, so nothing is copied, a frame record is written only
  where a reader needs it, and on amd64 and arm64 a call, a construction
  and a return are native handlers that never enter Go
  ([Calls](#where-the-time-goes) below). The programs that allocate most
  are the least ahead: Storage 8%, Havlak 18%. Since `2f05d3fc` the
  interpreter's totals are 1.2% and 1.4% lower, with Octane's Splay 16%
  shorter (143 to 121 ms), its EarleyBoyer 9.5% and Box2D 7.5%, and Are We
  Fast Yet's CD and Permute 7%, List 6% and Towers 5%; Storage is 4%
  longer, 1,488 to 1,549 ms, in every run. With the JIT both totals are
  level: Octane's is 1.5% longer, most of it Typescript, whose runs took
  2,020 to 2,104 ms where they took 1,996 to 2,052. Octane's EarleyBoyer
  and Splay with the JIT move between runs more than between the
  revisions: 48.1 to 60.7 ms and 87.7 to 111 ms in this session, 47.7 to
  59.5 and 90.5 to 126 in `2f05d3fc`'s.
- With the JIT, Are We Fast Yet takes 0.06 to 0.43 times QuickJS's time,
  its loops of arithmetic the least, its programs that allocate most --
  DeltaBlue, Storage -- the most. Richards' runs with the JIT fall in two
  groups, of about 650 to 680 ms and of 745 to 940: its three in the
  matrix took 663, 659 and 938 ms, and at `2f05d3fc` 838, 664 and 790.
- When the stack JIT was removed, its timing had the register JIT slower
  than the stack JIT just before it on Octane's RayTrace (+7.6%) and
  CodeLoad (+25%) and on Are We Fast Yet's Json (+8.2%), Permute (+7.0%)
  and Queens (+6.3%) (docs/performance-plan.md, "The register JIT's
  remaining gaps"). Against the stack JIT's figures at `acffb1aa` (270,
  227 and 119 ms), Json and Permute are level, 259 and 229 ms, and Queens
  3% longer, 123 ms; CodeLoad is faster, 14.2 then and 13.7 ms now.
  RayTrace took 34.6 ms, its runs with the JIT in this session 34.1 to
  34.7 ms, where they took 32.3 to 33.4 at `2f05d3fc`.
- On nine of Octane's fifteen the JIT is behind the interpreter or level
  with it: Richards (57% behind), DeltaBlue (45%), RayTrace (35%), PdfJS
  (32%), Typescript (25%), Box2D (12%), CodeLoad (11%) and RegExp (9%)
  behind, EarleyBoyer level. Octane's fixed work is one run of each
  benchmark, so compiling counts, and the code runs in the interpreter
  until it is hot; a runtime
  with the JIT keeps the frames the plain interpreter's calls and
  constructions do without. The JIT's own figures are close to what they
  were at `2f05d3fc` -- Richards 4.8 ms then, DeltaBlue 7.8, RayTrace
  33.3, PdfJS 259, Typescript 2,006 -- and the interpreter's are as fast
  or faster, which leaves less for the JIT's code to win back.

### Server rendering

The ssr suite renders a page with React 18's own builds, as a host behind
a web server does for each request: Render is one runtime rendering the
page again and again, FirstRender five fresh runtimes, each loading React
and rendering once. The same matrix, the median of three fresh-process
runs (lower is better):

| ssr | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| Render | 1,166 ms | 968 ms | 1,866 ms | 0.62x | 0.52x |
| FirstRender | 158 ms | 286 ms | | | |

Render took 0.65 and 0.53 times QuickJS's time at `2f05d3fc`, 1,199 and
972 ms, and FirstRender 157 and 284 ms. The interpreter's Render is 2.7%
shorter, its three runs 1,161 to 1,171 ms where they took 1,189 to
1,200; builds of the revisions between, run one after the other, put the
step at the keyed stores, which remember the shapes and keys their sites
met and store natively by them (`f04364dd`, `52027c0c`), with the key's
atom found in the runtime's table (`a4f9a2e3`). With the JIT Render is
level. A render helper's `return element(...)` is a callback that
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
takes about 150 ms for, all three ran that same work in fresh processes at
`b8c2c45f`, and the figure is the fastest of three runs. The runners'
micro mode does this, from a copy of the file kept in `internal/v8bench`, and
the matrix above runs it. `sort_bench` is left out: it reports a run's time
over its fastest sort, not the time of an operation. Over the other 71 tests
go-quickjs takes 0.49 times QuickJS's time by the geometric mean, where it
took 0.50 at `c2dae008` and v0.38.0, the stack design, 0.79 in
`3ecb8200`'s session: it is slower on 6, faster on 63, and within 5% on
the other two. With the JIT it takes 0.28 times QuickJS's time, as it did,
slower on 3 and faster on 66. The table has the interpreter's largest gaps
either way:

| Test | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| `weak_map_delete` | 384 ns | 437 ns | 250 ns | 1.54x | 1.75x |
| `bigint256_arith` | 186 ns | 109 ns | 127 ns | 1.47x | 0.86x |
| `weak_map_set` | 127 ns | 94.3 ns | 92.9 ns | 1.36x | 1.01x |
| `int_to_string` | 56.6 ns | 48.6 ns | 48.7 ns | 1.16x | 1.00x |
| `date_parse` | 972 ns | 972 ns | 844 ns | 1.15x | 1.15x |
| `func_closure_call` | 36.0 ns | 2.80 ns | 32.2 ns | 1.12x | 0.09x |
| `prop_create` | 52.3 ns | 43.1 ns | 51.1 ns | 1.02x | 0.84x |
| `regexp_utf16` | 257 ns | 250 ns | 266 ns | 0.97x | 0.94x |
| `array_prop_create` | 14.0 ns | 15.9 ns | 15.0 ns | 0.93x | 1.06x |
| `float_toExponential` | 165 ns | 136 ns | 180 ns | 0.91x | 0.76x |
| `global_func_call` | 8.11 ns | 2.12 ns | 32.6 ns | 0.25x | 0.06x |
| `prop_update` | 2.80 ns | 1.89 ns | 11.6 ns | 0.24x | 0.16x |
| `func_call` | 6.91 ns | 0.95 ns | 31.1 ns | 0.22x | 0.03x |
| `array_length_decr` | 12.4 ns | 10.4 ns | 58.6 ns | 0.21x | 0.18x |
| `array_for_in` | 15.0 ns | 12.5 ns | 75.6 ns | 0.20x | 0.17x |
| `string_build2c` | 26.4 ns | 43.8 ns | 145 ns | 0.18x | 0.30x |
| `array_hole_length_decr` | 12.7 ns | 10.1 ns | 70.3 ns | 0.18x | 0.14x |
| `array_for_of` | 7.38 ns | 4.93 ns | 46.1 ns | 0.16x | 0.11x |
| `array_slice` | 4.20 ns | 4.57 ns | 27.3 ns | 0.15x | 0.17x |
| `prop_clone` | 10.7 ns | 9.60 ns | 71.6 ns | 0.15x | 0.13x |

Of the gaps the stack design had, the largest are gone: in v0.38.0,
measured in `3ecb8200`'s session, `array_prop_create` took 2.25 times
QuickJS's time and takes 0.93, `global_read` 1.79 and 0.85, `array_read`
1.41 and 0.87, `empty_do_loop` 1.34 and 0.53.

The JIT does the most for a test that is a loop of arithmetic, locals,
property reads and writes, array elements and calls: `prop_read` takes
0.80 ns with it, `func_call` 0.95 ns, a fifth of the interpreter's time
or less. A test whose time is a built-in function's work runs much as it
does in the interpreter: regular expressions, `array_slice`, and the
conversions between numbers and strings. An object a function makes and
drops need not be made at all, even where it crosses a merge or goes round
a loop, as in `sum = sum.add(v)`: it is made only where it escapes, by
partial escape analysis, as in Graal (`41575a42`). `escape_accumulate`
and `carried_short` take 0.06 and 0.03 times QuickJS's time with the JIT,
and 0.52 and 0.50 without it. Code that keeps its objects gains nothing
from this, and the suites above hardly move for it.

The interpreter has gained more than the JIT, so some tests run slower
with the JIT than without it. In the run made for this page, 12 of these
71 took 7% or more longer with the JIT: `prop_delete`, `array_update`,
`array_prop_create`, `array_slice`, `local_destruct`, `global_destruct`,
`global_destruct_strict`, `weak_map_delete`, `string_build1`,
`string_build2c`, `string_build_large1` and `string_to_int`. Of the extra
tests below that QuickJS runs, 25 did, where 30 had at `c2dae008`:
`generator_delegate`, `async_await`, `async_call`, `hasOwnProperty`
called through a borrowed reference, pushes and spread calls of several
arguments, `object_rest`, four of the sorts, a sticky regular expression
and a global `match`, `object_create_shared`, `carried_exit_loops`, four
JSON tests of decimals, non-ASCII strings, getters and new layouts,
`array_index_append`, `template_numbers`, `prop_delete_last` and
`array_map_tail_args`.

Since `c2dae008` the plain interpreter has gained most where objects are
made and written. A keyed store remembers the shapes and keys its site
met and stores by them, natively and in the JIT's code, its key's atom
found in the runtime's table as a read's is (`a4f9a2e3`, `f04364dd`,
`85075750`, `52027c0c`): `object_computed`, whose literals have computed
keys, takes 81.4 ns where it took 185, 0.50 times QuickJS's time where it
was 1.17, and 45.4 ns with the JIT where it took 151. The allocation
round above takes `escape_rare` from 75.8 to 58.7 ns, `escape_accumulate`
from 142 to 126 and `carried_short` from 133 to 119, and in the same
commits `prop_write` from 2.85 to 2.61 ns and `global_write` from 3.24 to
2.98.
Native code writes an array's length where it shortens the array
(`88feca00`): `array_length_decr` and `array_hole_length_decr` take 12.4
and 12.7 ns, where they took 24.0 and 23.9. A branch on an `instanceof`
result is one instruction (`1dcf124c`): `instanceof_class` takes 39.1 ns
where it took 42.2. A method call of `codePointAt`, `at` or `charAt` is
answered by a native kernel (`45cc9313`): `string_at` takes 23.0 ns where
it took 54.8, `string_code_point_at` 19.9 where it took 46.4, and the
seven tests added for them 0.25 to 0.33 times QuickJS's time. With the
JIT a megamorphic read that meets a getter calls it in Go at its miss,
and a callee whose read met one is not inlined (`93ba1d1d`, `27a30110`,
`a6652098`), which keeps such a getter's answers right; and a look of the
JIT's cache outside a compile settles what the cache holds, so that it
never holds more than its budget (`55bb6a86`).

Some of these tests depend on where the linker places the code more than
on the code. The layout lock keeps the interpreter's hot code where it
measured best, but the code between its anchors, the Go handlers after the
loop and the closures still move with every change to the package, and a
change of a few per cent in one of these tests is not by itself a change in
the engine. Since `c2dae008` the lock has positions before `vm_object.go`,
the generators and the cold handlers (`90d40a1f`), for the generics after
the native handlers' block and the named functions' closures
(`8097c77b`), and before package jit, where native code's calls of Go
lie (`de942afb`). Builds of the revisions since `c2dae008`, run one after the
other on one level 3 cache, time some tests 10 to 25% apart, both ways,
where the code they run is the same: `json_parse_unicode` 62 or 74 ns,
`generator_next` 104 to 133, `bigint256_arith` 161 to 185,
`dataview_write` 58 to 71, `json_stringify_unicode` 36 or 49, and
`string_concat` with the JIT 97 to 121. Each of these is slower at
`b8c2c45f` than at `c2dae008` -- `generator_next` 112 ns against 104,
`bigint256_arith` 186 against 173, `dataview_write` 78.8 against 64.4,
`string_concat` with the JIT 113 against 96.6 -- and each is as fast at
some revision between. `array_literal_empty` took 79.7 ns in the matrix,
where it took 70.8, and 70.1 alone, where `c2dae008` takes 69.0. Four
steps stay in every later build: `json_stringify_replacer` takes 14%
longer, 808 ns against 710, since the table keyed reads convert their
keys through was made four-way sets (`6deb2e51`); `json_stringify_escapes`
15%, 102 ns against 89.0, and `json_stringify_unicode` with the JIT 38%,
50.6 ns against 36.8, since the WebAssembly engine's merge (between
`5ef0d50b` and `686480fd`), which added a host hook's check to the JSON
encoder and moved it; and `object_create_shared` with the JIT 16%, 112 ns
against 97.1, most of it from the JIT's keyed stores (`85075750`).

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
`at` and `charAt` called as methods. Over the 344 QuickJS can run (it has
no using declarations) go-quickjs takes 0.51 times its time, and 0.23
times with the JIT; over the 337 of `c2dae008` 0.51 and 0.24, where it
took 0.52 and 0.24 then, and v0.38.0 0.70 and 0.30 over its 313; leaving
out the JSON tests, 0.55 and 0.22. The asm.js
kernels' loops (`asm_adler`, `asm_crc32`, `asm_copy`, `asm_fnv`,
`asm_dot`, `asm_switch`) take 0.12 to 0.28 times QuickJS's time as typed
code, and 0.03 to 0.07 with the JIT, which compiles them at the module's
link; their recursion and calls (`asm_fib`, `asm_call_leaf`) 0.81 and
0.55, 0.30 and 0.09 with the JIT, which inlines a typed callee
(`4ae95d8b`), and a call through a function table 1.01, 0.17 with the
JIT. A switch on strings past its first few cases
takes 0.05 to 0.16 times QuickJS's time, its literal cases one lookup
(`100eb7f`). An await costs a fiftieth of QuickJS's time (`async_await`
15.4 against 740 ns): an async function resumed by its await's job goes
on in place at an await of a value that needs no job of its own. A
generator resumed from for-of is level (`generator_for_of` 1.04), where
v0.38.0 took 1.44 times, and a for-in over a dictionary takes 0.22 times,
where it took 1.41: its keys are made once for the dictionary's layout and
handed out by the native handlers. Calls in tail position take 0.52 to
0.60 times QuickJS's time, and a `map` callback that returns a call in
tail position 0.48, level with one that returns an ordinary call: such a
callback is entered directly, as any other is (`400fc19c`). A call of
`charCodeAt` whose method is read before its argument, as in
`s.charCodeAt(++i)`, is answered by the call handler from its target, as
V8's call feedback does (`b90b70c4`): `string_charcode_update` takes 0.34
times QuickJS's time. A site that calls fresh closures of two templates in
turn misses its cache at each call and retries the leaves the native call
handler answers (`ab16fd73`); `closure_leaf_miss` takes 1.17 times. A
global `exec` still takes 1.33 times, as it makes a result and a String
for each match; a sticky one 1.03. The largest gaps either way:

| Test | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| `array_literal_empty` | 79.7 ns | 62.0 ns | 39.0 ns | 2.04x | 1.59x |
| `scope_chain` | 670 ns | 675 ns | 375 ns | 1.79x | 1.80x |
| `subclass_fresh` | 1,144 ns | 1,201 ns | 679 ns | 1.69x | 1.77x |
| `set_proto_fresh` | 353 ns | 370 ns | 211 ns | 1.67x | 1.75x |
| `typed_u8_find` | 45.3 ns | 9.28 ns | 27.3 ns | 1.66x | 0.34x |
| `array_fill_down` | 140 ns | 144 ns | 86.3 ns | 1.63x | 1.67x |
| `object_create_fresh` | 354 ns | 360 ns | 224 ns | 1.58x | 1.61x |
| `dataview_write` | 78.8 ns | 52.9 ns | 51.0 ns | 1.55x | 1.04x |
| `typed_findIndex` | 47.4 ns | 8.97 ns | 30.9 ns | 1.53x | 0.29x |
| `typed_find` | 47.0 ns | 9.54 ns | 31.2 ns | 1.51x | 0.31x |
| `asm_fnv` | 6.65 ns | 1.47 ns | 54.7 ns | 0.12x | 0.03x |
| `string_switch_last` | 27.4 ns | 12.8 ns | 265 ns | 0.10x | 0.05x |
| `json_stringify_decimals` | 32.9 ns | 37.1 ns | 333 ns | 0.10x | 0.11x |
| `string_switch_large` | 29.1 ns | 21.5 ns | 405 ns | 0.07x | 0.05x |
| `string_switch_miss` | 17.6 ns | 12.5 ns | 267 ns | 0.07x | 0.05x |
| `regexp_search_utf16` | 687 ns | 716 ns | 13,178 ns | 0.05x | 0.05x |
| `string_switch_nonstring` | 12.2 ns | 7.47 ns | 264 ns | 0.05x | 0.03x |
| `regexp_search_i` | 684 ns | 704 ns | 20,916 ns | 0.03x | 0.03x |
| `json_parse_long` | 1,835 ns | 1,864 ns | 84,373 ns | 0.02x | 0.02x |
| `async_await` | 15.4 ns | 17.5 ns | 740 ns | 0.02x | 0.02x |

A BigInt in the int64 range is held in the value itself (`9746906`), as
QuickJS holds one that fits in a word, and so is one below 2^64 in
magnitude (`f8c3229`); a larger one is a block of no pointers, made by the
engine's own arithmetic (`f1bf681`). Short BigInts' `+`, `-`, `*` and
comparisons are native handlers: `bigint64_arith` takes 0.37 times
QuickJS's time. A larger one is made by the engine's arithmetic in Go:
`bigint256_arith` takes 1.47 times, and the extra BigInt tests 0.42 to
1.22 times, `bigint_toString`, `bigint64_xorshift` and `bigint_modpow`
the slowest. The JIT makes BigInts of 64 bits natively: `bigint64_arith`
takes 0.10 times QuickJS's time with it, `bigint64_xorshift` 0.05.

The largest gaps now are:

- objects made on a prototype that is itself new -- `Object.create` of a
  fresh object, `Object.setPrototypeOf` to one, a scope chain built as
  objects, a subclass made per call and its instance -- 1.58 to 1.79
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
  and reloaded around the call. 132 of the 244 operations have a native
  handler; the rest, and every fast path that misses, go to their Go
  handler through an exit that calls it in Go's convention
  (register-interpreter.md, "Native handlers"). The six loops of simple
  instructions -- `empty_loop` and the extra tests `loop_sum`,
  `loop_arith`, `loop_locals`, `loop_elements` and `loop_props` -- take
  0.21 to 0.68 times QuickJS's time, 0.42 by their geometric mean, where
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
  calls"). A method call whose body needs a frame takes 33.9 ns against
  68.6 in QuickJS (`method_call`), one that only returns its argument
  12.4 against 42.4 (`method_call_leaf`), an empty function 6.91 against
  31.1 (`func_call`); with the JIT they take 11.5, 2.66 and 0.95 ns. A
  recursive call is level: `call_fib` and `call_mutual` take 0.93 times
  QuickJS's time. A body that only returns a parameter or a
  constant, or a short pure one of numbers and upvalues, is answered at
  the call site without a frame; natively, so are a getter of `this.k`,
  `this.k.length` or `this.k[p]`, a `typeof` test, an initializer that
  stores its parameters in `this`, and a constructor that only forwards
  to one (`d54099e3`, `09409332`, `bdab5ba4`); the initializer writes its
  values at the slots, and gives the final layout, that it remembers from
  the last construction of the same layout (`8505335a`): `class_new`
  takes 0.45 times QuickJS's time.
- **Property access.** Objects built the same way share a shape, as in
  QuickJS and V8, and each property read and write, global variable read
  and write, call and `instanceof` remembers where it last found what it
  looked for; a site that meets a second shape keeps up to four, natively
  for a write too (`defbf92d`), and one that keeps meeting new shapes
  reads through the realm's shared cache, as V8's megamorphic stub cache
  does, in sets of four ways (`46370076`); an `instanceof` site keeps the
  constructor's prototype slot too (`32036a73`), and a branch on its
  result is the same instruction (`1dcf124c`). A keyed access converts
  its key through the runtime's table of key strings, also in sets of
  four ways (`6deb2e51`), and a keyed store remembers the shapes and keys
  its site met (`f04364dd`). Two adjacent reads of properties share one
  native dispatch (`4d5d636d`). The native handlers check the cache where
  the instruction runs: `prop_read` takes 0.49 times QuickJS's time,
  `prop_write` 0.33, `global_read` 0.85, `global_write` 0.39 and
  `instanceof_class` 0.27. An object holds only its values, its
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
  `crypto_am3` takes 0.71 times QuickJS's time, 0.20 with the JIT, and
  Crypto 0.47.
- **Code compiled from C to asm.js.** A validated asm.js module runs as
  typed code, its int32s and float64s raw in their registers, its heap's
  loads and stores natively, and with the JIT is compiled at its link with
  no speculation (`d8cbc918`, `5ae2317e`): Octane's zlib takes 0.19 times
  QuickJS's time, 0.10 with the JIT, and the asm.js kernels' loops 0.12 to
  0.28, 0.03 to 0.07 with it.
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
  rather than making a rope (`00ce03b5`): `string_build2c` takes 0.18 times
  QuickJS's time, `local_append_keep` 0.27 and `template_list` 0.35. The
  interpreter is ahead of the JIT on the first of these, 0.30 times with it.
- **JSON.** The 49 `json_` tests take 0.31 times QuickJS's time, and 0.32
  with the JIT, as the work is a built-in's. `JSON.parse` walks the shape
  tree as it reads, expecting the keys of the last object parsed in the same
  place, and makes each object once, at its last key. `JSON.stringify` keeps
  a layout's keys and slots from the second time it meets it (`097d71ec`).
  Numbers are read and written without strconv's general path (`baafd8e3`),
  and long strings are scanned with AVX2 or NEON where the machine has them:
  `json_parse_records` takes 0.33 times QuickJS's time,
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
  (`typed_array_write`, 0.42).
- **`apply(this, arguments)`.** In a function whose only use of
  `arguments` is passing it to `apply` (a common way to write a class
  constructor), the arguments object is never made: the call hands on the
  frame's arguments as they were passed (`apply_args`). A constructor
  whose whole body is `this.initialize.apply(this, arguments)`, as
  RayTrace's are, is constructed by its call site as a forwarder, and
  natively, where the target is an initializer, without entering either
  frame (`746d9438`, `bdab5ba4`): `forward_construct` takes 0.24 times
  QuickJS's time, and 0.02 with the JIT. It took 0.54 at `258b4e96`, 0.23
  at `3ecb8200`, 0.32 at `2f05d3fc` and 0.25 at `c2dae008`.

What remains in the interpreter is mostly in Go: the operations with no
native handler and the fast paths that miss, which exit to their Go
handlers, the built-ins and the object model -- the gaps above are in
them -- and the collector. The JIT is the way past the rest: a hot
function compiled to native code dispatches nothing, which is why, with
it, the loops and calls above take from a third of the interpreter's
time down to a tenth.

[goja]: https://github.com/dop251/goja
[v8-v7]: https://github.com/mozilla/arewefastyet/tree/master/benchmarks/v8-v7
