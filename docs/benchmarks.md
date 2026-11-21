# Benchmarks

The Go benchmarks and bundle timings below were measured at `b37daa23`.
The V8 comparison with goja was measured at `a2126155` on the register
interpreter and the JIT that compiles its register code. The comparison
with C QuickJS, under [Compared with C QuickJS](#compared-with-c-quickjs),
was measured at `b5df63ea`. Other comparisons retain the revisions and
measurement dates their sections name.

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
[goja]. All three go-quickjs columns use revision `a2126155`, measured alongside
goja on 2026-11-19. Each column is the median of three fresh-process runs
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
| Richards | 3,265 | 15,236 | 15,303 | 521 | 6.27x | 29.24x | 29.37x |
| DeltaBlue | 3,590 | 13,825 | 13,792 | 624 | 5.75x | 22.16x | 22.10x |
| Crypto | 5,417 | 21,937 | 21,835 | 330 | 16.42x | 66.48x | 66.17x |
| RayTrace | 9,480 | 35,741 | 33,669 | 786 | 12.06x | 45.47x | 42.84x |
| EarleyBoyer | 10,071 | 13,836 | 13,004 | 1,423 | 7.08x | 9.72x | 9.14x |
| RegExp | 9,164 | 11,262 | 10,616 | 589 | 15.56x | 19.12x | 18.02x |
| Splay | 17,496 | 25,458 | 22,687 | 2,719 | 6.43x | 9.36x | 8.34x |
| NavierStokes | 10,779 | 39,880 | 38,655 | 568 | 18.98x | 70.21x | 68.05x |
| **Composite score** | **7,440** | **20,070** | **19,269** | **757** | **9.83x** | **26.51x** | **25.45x** |

The runner's timed suite interval includes runtime construction, parsing,
compilation, the suite's warmups and its measured iterations, but excludes
reading the source files and the final forced garbage collection. CPU time
includes work on other threads, including the collector. These are the
go-quickjs and goja runs:

| Metric | go-quickjs JIT off | go-quickjs JIT on | go-quickjs JIT background | goja | JIT off relative to goja |
|---|---:|---:|---:|---:|---:|
| Suite wall time | 20.15 s | 20.09 s | 20.09 s | 50.96 s | 2.53x faster |
| Suite CPU time | 24.16 s | 26.76 s | 26.83 s | 73.16 s | 67.0% less |
| Total allocation | 8.65 GiB | 16.20 GiB | 15.47 GiB | 33.83 GiB | 74.4% less |

Score mode does different amounts of work as throughput changes, so the CPU
and allocation totals are not equal-work efficiency measurements. The
composite is the median of the three printed composite scores, not a
composite calculated from the workload medians.

The suite warms every workload for at least one second, then measures for at
least another second and continues until it has 32 measured iterations. All
engines evaluated the same benchmark sources and validation code.
The measured revisions were go-quickjs `a2126155`, goja `a177d211bf60`, and
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
| JIT off | 7,546 | 7,440 | 7,186 |
| JIT on | 20,428 | 20,070 | 19,034 |
| JIT background | 19,269 | 18,978 | 19,333 |
| goja | 786 | 757 | 739 |

All samples were included. The table takes the median of each workload and
of the printed composite scores independently. Background compilation's
composite was 3.99% lower than synchronous compilation's; this run does not
establish an overall throughput advantage for either compilation mode.

## Compared with C QuickJS

Over the V8 suite as a whole, go-quickjs is well ahead of QuickJS. Its
interpreter takes 0.32 times QuickJS's time and has 2.20 times its
composite score, ahead on all eight workloads. With its JIT it takes 0.21
times QuickJS's time and scores 5.6 times as much. On the larger programs
of Octane and Are We Fast Yet the interpreter takes 0.43 and 0.64 times
QuickJS's time, and the JIT 0.31 and 0.20 times
([below](#octane-and-are-we-fast-yet)); a page React 18 renders on the
server takes 0.61 times, and 0.53 with the JIT
([below](#server-rendering)). Single operations cost about half as much,
0.49 times QuickJS's time by the geometric mean of its micro-benchmarks,
and 0.28 times with the JIT, as [the micro-benchmarks](#micro-benchmarks)
below show; the slowest of them takes 1.5 times as long.

At `b5df63ea`, the revision measured here, go-quickjs compiles every
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
`5ae2317e`). The page's previous figures, at `b8c2c45f`, whose code
v0.41.0 is, were of the same design before the megamorphic caches' native
probes, `Object.create`'s native leaf and the changes below. The same
suites ran on an AMD Ryzen 5 3600 under Windows, with Go 1.27.3,
go-quickjs `b5df63ea` and QuickJS 2026-06-04. Two runs went at
once, each on the six CPUs of a level 3 cache of its own. The JIT's
column is a build with `-tags quickjs_jit` run with `-jit`. The figures
are milliseconds for the same fixed work, the median of three
fresh-process runs (lower is better):

| Workload | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| Richards | 8.2 ms | 8.1 ms | 11.2 ms | 0.73x | 0.72x |
| DeltaBlue | 14.0 ms | 17.3 ms | 23.8 ms | 0.59x | 0.73x |
| Crypto | 154 ms | 54.9 ms | 326 ms | 0.47x | 0.17x |
| RayTrace | 69.8 ms | 52.2 ms | 144 ms | 0.48x | 0.36x |
| EarleyBoyer | 172 ms | 117 ms | 398 ms | 0.43x | 0.30x |
| RegExp | 149 ms | 128 ms | 856 ms | 0.17x | 0.15x |
| Splay | 117 ms | 88.2 ms | 494 ms | 0.24x | 0.18x |
| NavierStokes | 100 ms | 36.5 ms | 190 ms | 0.53x | 0.19x |
| **Total** | **807 ms** | **525 ms** | **2,506 ms** | **0.32x** | **0.21x** |

The suite's own scores, the median of three (higher is better):

| Workload | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| Richards | 1,397 | 5,643 | 960 | 1.46x | 5.88x |
| DeltaBlue | 1,493 | 5,865 | 844 | 1.77x | 6.95x |
| Crypto | 2,357 | 8,184 | 1,112 | 2.12x | 7.36x |
| RayTrace | 3,370 | 12,480 | 1,568 | 2.15x | 7.96x |
| EarleyBoyer | 3,871 | 4,940 | 2,181 | 1.77x | 2.27x |
| RegExp | 2,841 | 4,099 | 436 | 6.52x | 9.40x |
| Splay | 6,605 | 9,499 | 3,362 | 1.96x | 2.83x |
| NavierStokes | 4,996 | 16,571 | 2,391 | 2.09x | 6.93x |
| **Composite** | **2,960** | **7,574** | **1,348** | **2.20x** | **5.62x** |

At `b8c2c45f` the interpreter took 0.33 times QuickJS's time over the
suite and had 2.19 times its composite score, and the JIT 0.22 and 5.61
times. The interpreter's total is level, 812 to 807 ms, its composite
0.8% higher, Splay 117 ms where it took 124. The JIT's total is 3.3%
lower, 543 to 525 ms, with Splay 88.2 where it took 97.8 and RegExp 128
where it took 133; its composite is level. QuickJS's total moved as much
between sessions: 2,491 ms at `b8c2c45f`'s, 2,506 here, and 2,596 in a
run of `91f885a1` earlier the same day, its RegExp 947 ms in every run of
that session and 856 in this one, the binary the same (see where a
program is loaded, below).

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
  and the JIT 0.29 times, where their scores are 2.20 and 5.62 times as
  high.
- The JIT's lead is much larger in the scores than in fixed time. A fixed
  run of Richards or DeltaBlue lasts a few milliseconds, and a function
  runs in the interpreter until it is hot enough to compile, so much of
  that run is spent before the JIT's code takes over: with the JIT,
  Richards takes 0.99 times the interpreter's fixed time and DeltaBlue
  1.24 (DeltaBlue's three runs with it took 15.7 to 17.4 ms). The scores
  time warm runs, where Richards, DeltaBlue, Crypto, RayTrace and
  NavierStokes score 3.3 to 4.0 times what the interpreter does.
- Splay's lead is largest in its fixed time. That is almost all setup and
  teardown: building a tree of 8,000 nodes and dropping it, which QuickJS
  frees by counting references where Go's collector frees it later. A warm
  run, which is what the score times, is splay-tree operations, calls and
  property reads, and a new node for each insertion. Each node's payload
  holds array literals of constants, which share their elements with the
  literal until one is written, as V8's copy-on-write arrays do
  (`a2d9f12e`); its score is 96% ahead of QuickJS's.
- The JIT changes least where the time is spent in Go code it calls
  rather than in the script's own: the allocation-heavy EarleyBoyer and
  Splay, 1.28 and 1.44 times the interpreter's score, and RegExp, 1.44
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
that the lock does not hold; the figures here were made before the
matrix and the placements tool loaded their builds at a fixed base, as a
Linux build loads at its link address (`-fixedbase`), and a copy at a
fixed base can still sit apart, for where its file lies in memory, which
the tools now take out by running each process from a fresh copy
(`-freshcopy`; docs/optimizing.md, "Builds and the machine"). The
figures here are of one copy of each build: a micro-benchmark that moved
by a tenth or more between two of them may have moved for that alone. To
reproduce
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
| Richards | 2.9 ms | 4.6 ms | 3.9 ms | 0.74x | 1.18x |
| DeltaBlue | 5.2 ms | 7.5 ms | 7.9 ms | 0.66x | 0.95x |
| Crypto | 51.8 ms | 22.8 ms | 109 ms | 0.47x | 0.21x |
| RayTrace | 26.2 ms | 33.2 ms | 49.8 ms | 0.53x | 0.67x |
| EarleyBoyer | 59.5 ms | 48.2 ms | 136 ms | 0.44x | 0.36x |
| RegExp | 117 ms | 132 ms | 461 ms | 0.25x | 0.29x |
| Splay | 119 ms | 85.2 ms | 461 ms | 0.26x | 0.18x |
| NavierStokes | 41.5 ms | 18.8 ms | 65.9 ms | 0.63x | 0.29x |
| PdfJS | 199 ms | 261 ms | 375 ms | 0.53x | 0.70x |
| Mandreel | 2,484 ms | 868 ms | 3,366 ms | 0.74x | 0.26x |
| Gameboy | 391 ms | 322 ms | 461 ms | 0.85x | 0.70x |
| CodeLoad | 12.7 ms | 13.8 ms | 12.9 ms | 0.98x | 1.07x |
| Box2D | 89.0 ms | 98.1 ms | 154 ms | 0.58x | 0.64x |
| zlib | 1,593 ms | 849 ms | 8,317 ms | 0.19x | 0.10x |
| Typescript | 1,609 ms | 1,990 ms | 2,109 ms | 0.76x | 0.94x |
| **Total** | **7,191 ms** | **5,133 ms** | **16,691 ms** | **0.43x** | **0.31x** |

| Are We Fast Yet | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| DeltaBlue | 561 ms | 313 ms | 713 ms | 0.79x | 0.44x |
| Richards | 3,279 ms | 658 ms | 4,809 ms | 0.68x | 0.14x |
| Json | 1,010 ms | 259 ms | 1,339 ms | 0.75x | 0.19x |
| CD | 1,576 ms | 485 ms | 2,814 ms | 0.56x | 0.17x |
| Havlak | 3,411 ms | 1,186 ms | 4,309 ms | 0.79x | 0.28x |
| Bounce | 761 ms | 246 ms | 1,487 ms | 0.51x | 0.17x |
| List | 544 ms | 159 ms | 1,247 ms | 0.44x | 0.13x |
| Mandelbrot | 178 ms | 40.9 ms | 687 ms | 0.26x | 0.06x |
| NBody | 665 ms | 122 ms | 1,026 ms | 0.65x | 0.12x |
| Permute | 866 ms | 229 ms | 1,675 ms | 0.52x | 0.14x |
| Queens | 658 ms | 122 ms | 1,056 ms | 0.62x | 0.12x |
| Sieve | 560 ms | 301 ms | 1,221 ms | 0.46x | 0.25x |
| Storage | 1,492 ms | 726 ms | 1,654 ms | 0.90x | 0.44x |
| Towers | 889 ms | 202 ms | 1,550 ms | 0.57x | 0.13x |
| **Total** | **16,447 ms** | **5,061 ms** | **25,609 ms** | **0.64x** | **0.20x** |

- Octane's zlib takes 0.19 times QuickJS's time, 1,593 ms, and 0.10 times
  with the JIT, 849 ms (1,609 and 854 at `b8c2c45f`). zlib is an asm.js
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
  register code: 2,484 ms, 0.74 times QuickJS's time, and 868 with the
  JIT. Its largest function is compiled at its first call, within the
  timed run (`d6ac45be`). Its `| 0` arithmetic runs as int32 instructions
  (`add_i32`, `add_imm_i32`, the bitwise operators with an immediate) and
  its typed arrays' elements are read and written by native handlers, but
  each element read and each `| 0` still converts through a float: raw
  registers outside asm.js are designed and not built
  (register-interpreter.md, "Known limits and open work").
- Without the JIT, go-quickjs is ahead on every benchmark of both suites.
  Octane's CodeLoad, which is mostly compiling, takes 0.98 times QuickJS's
  time, and 1.07 with the JIT, where it took 0.95 and 1.06 at `b8c2c45f`
  and 1.22 and 1.37 at `258b4e96`, before the front end compiled to
  register code alone (`d6ac45be`). On Are We Fast Yet the interpreter is
  21% ahead on DeltaBlue and on Havlak, a third on Richards, 38% on
  Queens and 43% on Towers. A call's window of registers is the
  callee's frame, so nothing is copied, a frame record is written only
  where a reader needs it, and on amd64 and arm64 a call, a construction
  and a return are native handlers that never enter Go
  ([Calls](#where-the-time-goes) below). The programs that allocate most
  are the least ahead: Storage 10%, Havlak 21%. Since `b8c2c45f` the
  interpreter's Are We Fast Yet total is 2.2% lower, Havlak 5.5% (3,610
  to 3,411 ms) and Storage 3.7% (1,549 to 1,492): a site that has given up
  its cache of shapes reads and writes through the realm's megamorphic
  caches in the native handlers, as V8's interpreter probes its stub
  cache, where each such access had exited to Go (`5916704b`), and a
  write site that keeps several shapes adds a property to any of them
  natively (`d44da58c`), which took Havlak's writes that exited to Go from
  5.39 million to 5.3 thousand. Octane's interpreter total is level,
  7,236 to 7,191 ms. With the JIT both totals are 1.4% and 0.7% lower.
  Octane's EarleyBoyer and Splay with the JIT move between runs more than
  between the revisions: 47.4 to 65.2 ms and 81.7 to 109 ms in this
  session, 48.1 to 60.7 and 87.7 to 111 in `b8c2c45f`'s.
- With the JIT, Are We Fast Yet takes 0.06 to 0.43 times QuickJS's time,
  its loops of arithmetic the least, its programs that allocate most --
  DeltaBlue, Storage -- the most. Richards' runs with the JIT fall in two
  groups, of about 650 to 680 ms and of 745 to 940: its three in the
  matrix took 661, 657 and 658 ms, and at `b8c2c45f` 663, 659 and 938.
- When the stack JIT was removed, its timing had the register JIT slower
  than the stack JIT just before it on Octane's RayTrace (+7.6%) and
  CodeLoad (+25%) and on Are We Fast Yet's Json (+8.2%), Permute (+7.0%)
  and Queens (+6.3%) (docs/performance-plan.md, "The register JIT's
  remaining gaps"). Against the stack JIT's figures at `acffb1aa` (270,
  227 and 119 ms), Json and Permute are level, 259 and 229 ms, and Queens
  2% longer, 122 ms; CodeLoad is faster, 14.2 then and 13.8 ms now.
  RayTrace took 33.2 ms, its runs with the JIT in this session 33.0 to
  34.0 ms, where they took 34.1 to 34.7 at `b8c2c45f`.
- On eight of Octane's fifteen the JIT is behind the interpreter:
  Richards (59% behind), DeltaBlue (44%), PdfJS (31%), RayTrace (27%),
  Typescript (24%), RegExp (13%), Box2D (10%) and CodeLoad (9%).
  Octane's fixed work is one run of each benchmark, so compiling counts,
  and the code runs in the interpreter until it is hot; a runtime with
  the JIT keeps the frames the plain interpreter's calls and
  constructions do without. The JIT's own figures are close to what they
  were at `b8c2c45f` -- Richards 4.7 ms then, DeltaBlue 7.7, RayTrace
  34.6, PdfJS 246, Typescript 2,049 -- and the interpreter's are as fast
  or faster, which leaves less for the JIT's code to win back.

### Server rendering

The ssr suite renders a page with React 18's own builds, as a host behind
a web server does for each request: Render is one runtime rendering the
page again and again, FirstRender five fresh runtimes, each loading React
and rendering once. The same matrix, the median of three fresh-process
runs (lower is better):

| ssr | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| Render | 1,145 ms | 987 ms | 1,875 ms | 0.61x | 0.53x |
| FirstRender | 154 ms | 288 ms | | | |

Render took 0.62 and 0.52 times QuickJS's time at `b8c2c45f`, 1,166 and
968 ms, and FirstRender 158 and 286 ms. The interpreter's Render is 1.8%
shorter, its three runs 1,141 to 1,166 ms where they took 1,161 to
1,171, and with the JIT it is 2% longer, 974 to 990 ms where they took
963 to 974: a run of `91f885a1`, the parent of the revision measured,
had the interpreter's at 1,202 to 1,220 ms, and six copies of each of
`b8c2c45f`'s and `91f885a1`'s builds, each loaded where Windows chose,
took 1,128 to 1,173 and 1,133 to 1,178 ms, 1% apart by their medians.
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
takes about 150 ms for, all three ran that same work in fresh processes at
`b5df63ea`, and the figure is the fastest of three runs. The runners'
micro mode does this, from a copy of the file kept in `internal/v8bench`, and
the matrix above runs it. `sort_bench` is left out: it reports a run's time
over its fastest sort, not the time of an operation. Over the other 71 tests
go-quickjs takes 0.49 times QuickJS's time by the geometric mean, as it
did at `b8c2c45f`, and v0.38.0, the stack design, 0.79 in
`3ecb8200`'s session: it is slower on 6, faster on 63, and within 5% on
the other two. With the JIT it takes 0.28 times QuickJS's time, as it did,
slower on 3, faster on 65 and within 5% on 3. The table has the interpreter's largest gaps
either way:

| Test | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| `weak_map_delete` | 372 ns | 439 ns | 250 ns | 1.49x | 1.76x |
| `bigint256_arith` | 186 ns | 111 ns | 126 ns | 1.48x | 0.88x |
| `weak_map_set` | 127 ns | 92.0 ns | 92.5 ns | 1.37x | 0.99x |
| `date_parse` | 960 ns | 1,003 ns | 829 ns | 1.16x | 1.21x |
| `int_to_string` | 56.1 ns | 49.8 ns | 48.9 ns | 1.15x | 1.02x |
| `func_closure_call` | 35.7 ns | 2.80 ns | 31.7 ns | 1.13x | 0.09x |
| `prop_create` | 51.5 ns | 43.4 ns | 52.0 ns | 0.99x | 0.84x |
| `regexp_utf16` | 247 ns | 249 ns | 256 ns | 0.97x | 0.97x |
| `array_prop_create` | 14.0 ns | 15.9 ns | 14.9 ns | 0.94x | 1.06x |
| `float_toExponential` | 162 ns | 138 ns | 180 ns | 0.90x | 0.77x |
| `global_func_call` | 8.17 ns | 2.13 ns | 33.0 ns | 0.25x | 0.06x |
| `prop_update` | 2.80 ns | 1.88 ns | 11.5 ns | 0.24x | 0.16x |
| `func_call` | 6.76 ns | 0.95 ns | 30.8 ns | 0.22x | 0.03x |
| `array_length_decr` | 12.6 ns | 10.5 ns | 58.5 ns | 0.22x | 0.18x |
| `array_for_in` | 14.9 ns | 12.2 ns | 76.0 ns | 0.20x | 0.16x |
| `string_build2c` | 26.6 ns | 40.9 ns | 143 ns | 0.19x | 0.29x |
| `array_hole_length_decr` | 12.5 ns | 9.70 ns | 69.8 ns | 0.18x | 0.14x |
| `array_for_of` | 7.32 ns | 4.93 ns | 46.2 ns | 0.16x | 0.11x |
| `array_slice` | 4.09 ns | 4.38 ns | 27.3 ns | 0.15x | 0.16x |
| `prop_clone` | 10.6 ns | 9.33 ns | 72.8 ns | 0.15x | 0.13x |

Of the gaps the stack design had, the largest are gone: in v0.38.0,
measured in `3ecb8200`'s session, `array_prop_create` took 2.25 times
QuickJS's time and takes 0.94, `global_read` 1.79 and 0.85, `array_read`
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
and `carried_short` take 0.06 and 0.04 times QuickJS's time with the JIT,
and 0.50 and 0.48 without it. Code that keeps its objects gains nothing
from this, and the suites above hardly move for it.

The interpreter has gained more than the JIT, so some tests run slower
with the JIT than without it. In the run made for this page, 11 of these
71 took 7% or more longer with the JIT: `prop_delete`, `array_update`,
`array_prop_create`, `local_destruct`, `global_destruct`,
`global_destruct_strict`, `weak_map_delete`, `string_build1`,
`string_build2c`, `string_build_large1` and `string_to_int`. Of the extra
tests below that QuickJS runs, 30 did, where 25 had at `b8c2c45f`:
`generator_delegate` and four of the async tests, `hasOwnProperty`
called through a borrowed reference three ways, pushes and spread calls
of several arguments, `object_rest`, four of the sorts, `indexOf` of an
array, a sticky regular expression, a global `match` and a search of
UTF-16 text, `carried_exit_loops`, five JSON tests of instances, getters,
exponents, additions and new layouts, `array_index_append`,
`template_numbers`, `prop_delete_last` and `array_map_tail_args`; no
longer `object_create_shared` (below).

Since `b8c2c45f` the gains are in objects made by `Object.create` and in
sites that meet many shapes. `Object.create` of a prototype alone is a
native leaf, called without a frame (`ffb23541`), and in the JIT's code
an object of a site's prototype is taken from a pool the site keeps, as
V8's code allocates what its builtin would (`fd52997d`):
`object_create_shared` takes 76.8 ns where it took 89.5, and 37.9 with
the JIT where it took 112, 0.26 times QuickJS's time. A read or write
site that has given up its cache of shapes probes the realm's
megamorphic caches in the native handlers, as V8's interpreter probes
its stub cache, where it had exited to Go (`5916704b`); a write site
that keeps several shapes adds a property to any of them natively
(`d44da58c`); and with the JIT a megamorphic write adds a property
natively by the entry the cache holds (`ffb23541`). The two tests added
for them (`8c1396bf`), `mega_read` and `mega_add`, take 0.74 and 0.68
times QuickJS's time, 0.28 and 0.49 with the JIT; those commits took
`mega_add` from 174 to 112 ns and `mega_read` from 47.6 to 28.3. JSON's
keys for a replacer, a reviver and `toJSON` are no longer remembered in
the runtime's table of key strings, whose four-way sets made each one a
probe (`ea38774a`): `json_stringify_replacer` takes 708 ns where it took
808, 722 with the JIT where it took 851, and `json_stringify_tojson` 83.2
where it took 91.5. The JSON encoder is at `c2dae008`'s offsets again,
`encodeObject` as long as it was (`638b9a62`), and the lock holds the
builds with the WebAssembly engine too (`24d565ad`):
`json_stringify_escapes` takes 87.4 ns where it took 102, and
`json_stringify_unicode` with the JIT 37.7 where it took 50.6. The rest
since `b8c2c45f` is meant to move no figure here: the built-ins' and
Function.prototype's own keys in V8's order (`8b5d6339`, `c7c7830a`;
a runtime is made as quickly), a sloppy function's own `caller` in
standards mode, made when first asked for (`f18011fa`), source-phase
re-exports (`8f927215`), `Atomics.wait`'s blocking policy
(`a327994e`), and native code's exit counts and the tests' switches
made each runtime's (`831e57c4`, `6b858ef8`, `d8feacc5`), which leave
`runReg` one load of its runtime's flag where it read the process's.

Some of these tests depend on where the linker places the code more than
on the code. The layout lock keeps the interpreter's hot code where it
measured best, but the code between its anchors, the Go handlers after the
loop and the closures still move with every change to the package, and a
change of a few per cent in one of these tests is not by itself a change in
the engine. Since `b8c2c45f` the lock has a position before JSON.parse's
parser (JP, `27b5b5a9`) and configurations for the builds with the
WebAssembly engine (`24d565ad`). A pad that grows past a page moves
everything after it 4 KB, every offset modulo 4 KB the same: the lock's
E did when Function.prototype's and the built-ins' keys were put in
V8's order (`8b5d6339`, `c7c7830a`), and `json_stringify_decimals` was
46% slower at every placement until the reservation moved out of E's
range (`b5df63ea`; docs/optimizing.md, "The layout lock"). Rows the
matrix has slower than at `b8c2c45f`, each timed again over four
placements of `b5df63ea` against `b8c2c45f` with both loaded at a fixed
base, and for some over copies loaded where Windows chose:

- slower at every placement: `json_stringify_decimals` 4.8% (34.2 ns in
  the matrix, 32.9 at `b8c2c45f`) and `json_stringify_ints` 4.6% (25.8
  against 24.8), and with the JIT `json_stringify_unique` 9% (122 against
  114): their code lies in the encoder's stretch between
  `encodeElements` and `appendJSONString`, which is at `c2dae008`'s
  offsets for `json_stringify_escapes` and `json_stringify_unicode`,
  where these took what they take now (`27b5b5a9`);
- slower at most placements: `string_indexOf` 6% at three of four
  (89.9 ns in the matrix against 82.6), `json_stringify_strings` 2.6% at
  three (31.7 against 30.3), and with the JIT `array_indexOf` 3% at three
  (118 against 102); `string_indexOf` and `array_indexOf` were slower in
  every copy of `91f885a1`'s build too;
- level over the placements, slower in the matrix's copy:
  `json_stringify_dict` (93.3 against 80.0), `string_starts_with` (73.5
  against 69.0), and with the JIT `string_template` (188 against 172),
  `string_code_point_at` (6.00 against 5.41), `has_own_call_miss` (97.2
  against 78.3, 78 to 81 over copies of `91f885a1`'s build where
  `b8c2c45f`'s took 83 to 85) and `string_from_code_point` (87.2 against
  75.1, 90 to 133 over those copies, 88 to 96).

The matrix's `generator_next` takes 103 ns where it took 112,
`dataview_write` 67.1 where it took 78.8 and `array_literal_empty` 70.9
where it took 79.7, which `b8c2c45f`'s page had among the rows that move
both ways with where the code lies.

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
times its time, and 0.23 times with the JIT; over the 344 of `b8c2c45f`
0.50 and 0.23, where it took 0.51 and 0.23 then, and v0.38.0 0.70 and
0.30 over its 313; leaving out the JSON tests, 0.55 and 0.22. The asm.js
kernels' loops (`asm_adler`, `asm_crc32`, `asm_copy`, `asm_fnv`,
`asm_dot`, `asm_switch`) take 0.12 to 0.28 times QuickJS's time as typed
code, and 0.03 to 0.07 with the JIT, which compiles them at the module's
link; their recursion and calls (`asm_fib`, `asm_call_leaf`) 0.80 and
0.55, 0.30 and 0.09 with the JIT, which inlines a typed callee
(`4ae95d8b`), and a call through a function table 1.06, 0.17 with the
JIT. A switch on strings past its first few cases
takes 0.05 to 0.16 times QuickJS's time, its literal cases one lookup
(`100eb7f`). An await costs a fiftieth of QuickJS's time (`async_await`
15.2 against 747 ns): an async function resumed by its await's job goes
on in place at an await of a value that needs no job of its own. A
generator resumed from for-of is level (`generator_for_of` 1.02), where
v0.38.0 took 1.44 times, and a for-in over a dictionary takes 0.22 times,
where it took 1.41: its keys are made once for the dictionary's layout and
handed out by the native handlers. Calls in tail position take 0.55 to
0.61 times QuickJS's time, and a `map` callback that returns a call in
tail position 0.48, level with one that returns an ordinary call: such a
callback is entered directly, as any other is (`400fc19c`). A call of
`charCodeAt` whose method is read before its argument, as in
`s.charCodeAt(++i)`, is answered by the call handler from its target, as
V8's call feedback does (`b90b70c4`): `string_charcode_update` takes 0.33
times QuickJS's time. A site that calls fresh closures of two templates in
turn misses its cache at each call and retries the leaves the native call
handler answers (`ab16fd73`); `closure_leaf_miss` takes 1.14 times. A
global `exec` still takes 1.31 times, as it makes a result and a String
for each match; a sticky one 1.01. The largest gaps either way:

| Test | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| `scope_chain` | 665 ns | 665 ns | 363 ns | 1.83x | 1.83x |
| `set_proto_fresh` | 375 ns | 391 ns | 210 ns | 1.78x | 1.86x |
| `array_literal_empty` | 70.9 ns | 61.5 ns | 40.8 ns | 1.74x | 1.51x |
| `subclass_fresh` | 1,160 ns | 1,230 ns | 682 ns | 1.70x | 1.80x |
| `typed_u8_find` | 44.6 ns | 9.61 ns | 27.2 ns | 1.64x | 0.35x |
| `array_fill_down` | 140 ns | 140 ns | 86.6 ns | 1.61x | 1.62x |
| `object_create_fresh` | 344 ns | 341 ns | 228 ns | 1.51x | 1.50x |
| `typed_findIndex` | 46.1 ns | 9.27 ns | 31.0 ns | 1.49x | 0.30x |
| `typed_find` | 46.0 ns | 9.64 ns | 31.0 ns | 1.49x | 0.31x |
| `typed_u64_reduce` | 108 ns | 108 ns | 74.8 ns | 1.44x | 1.45x |
| `asm_copy` | 5.76 ns | 2.01 ns | 48.3 ns | 0.12x | 0.04x |
| `string_switch_last` | 27.4 ns | 12.8 ns | 264 ns | 0.10x | 0.05x |
| `json_stringify_decimals` | 34.2 ns | 34.9 ns | 331 ns | 0.10x | 0.11x |
| `string_switch_large` | 29.0 ns | 21.4 ns | 406 ns | 0.07x | 0.05x |
| `string_switch_miss` | 17.6 ns | 12.5 ns | 266 ns | 0.07x | 0.05x |
| `regexp_search_utf16` | 679 ns | 737 ns | 13,142 ns | 0.05x | 0.06x |
| `string_switch_nonstring` | 12.2 ns | 7.34 ns | 265 ns | 0.05x | 0.03x |
| `regexp_search_i` | 672 ns | 708 ns | 20,977 ns | 0.03x | 0.03x |
| `json_parse_long` | 1,836 ns | 1,864 ns | 83,810 ns | 0.02x | 0.02x |
| `async_await` | 15.2 ns | 19.4 ns | 747 ns | 0.02x | 0.03x |

A BigInt in the int64 range is held in the value itself (`9746906`), as
QuickJS holds one that fits in a word, and so is one below 2^64 in
magnitude (`f8c3229`); a larger one is a block of no pointers, made by the
engine's own arithmetic (`f1bf681`). Short BigInts' `+`, `-`, `*` and
comparisons are native handlers: `bigint64_arith` takes 0.37 times
QuickJS's time. A larger one is made by the engine's arithmetic in Go:
`bigint256_arith` takes 1.48 times, and the extra BigInt tests 0.43 to
1.27 times, `bigint_pidigits`, `bigint_toString`, `bigint_modpow` and
`bigint64_xorshift` the slowest. The JIT makes BigInts of 64 bits natively: `bigint64_arith`
takes 0.10 times QuickJS's time with it, `bigint64_xorshift` 0.05.

The largest gaps now are:

- objects made on a prototype that is itself new -- `Object.create` of a
  fresh object, `Object.setPrototypeOf` to one, a scope chain built as
  objects, a subclass made per call and its instance -- 1.51 to 1.83
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
  a `replace` by a template with `$` (1.31 and 1.30 times).

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
  calls"). A method call whose body needs a frame takes 33.8 ns against
  69.3 in QuickJS (`method_call`), one that only returns its argument
  12.5 against 42.7 (`method_call_leaf`), an empty function 6.76 against
  30.8 (`func_call`); with the JIT they take 11.6, 2.64 and 0.95 ns. A
  recursive call is level: `call_fib` and `call_mutual` take 0.93 times
  QuickJS's time. A body that only returns a parameter or a
  constant, or a short pure one of numbers and upvalues, is answered at
  the call site without a frame; natively, so are a getter of `this.k`,
  `this.k.length` or `this.k[p]`, a `typeof` test, an initializer that
  stores its parameters in `this`, and a constructor that only forwards
  to one (`d54099e3`, `09409332`, `bdab5ba4`); the initializer writes its
  values at the slots, and gives the final layout, that it remembers from
  the last construction of the same layout (`8505335a`): `class_new`
  takes 0.43 times QuickJS's time. `Object.create` of a prototype alone
  is a leaf too, which the interpreter and native code call without a
  frame (`ffb23541`).
- **Property access.** Objects built the same way share a shape, as in
  QuickJS and V8, and each property read and write, global variable read
  and write, call and `instanceof` remembers where it last found what it
  looked for; a site that meets a second shape keeps up to four, natively
  for a write too (`defbf92d`), and one that keeps meeting new shapes
  reads through the realm's shared cache, as V8's megamorphic stub cache
  does, in sets of four ways (`46370076`), and the native handlers probe
  that cache and the realm's cache of additions themselves (`5916704b`);
  a write site that keeps several shapes adds to each of them natively
  (`d44da58c`); an `instanceof` site keeps the
  constructor's prototype slot too (`32036a73`), and a branch on its
  result is the same instruction (`1dcf124c`). A keyed access converts
  its key through the runtime's table of key strings, also in sets of
  four ways (`6deb2e51`), and a keyed store remembers the shapes and keys
  its site met (`f04364dd`). Two adjacent reads of properties share one
  native dispatch (`4d5d636d`). The native handlers check the cache where
  the instruction runs: `prop_read` takes 0.49 times QuickJS's time,
  `prop_write` 0.34, `global_read` 0.85, `global_write` 0.39,
  `instanceof_class` 0.26, and `mega_read` and `mega_add`, at sites that
  meet a new layout at every call, 0.74 and 0.68. An object holds only its values, its
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
  `crypto_am3` takes 0.70 times QuickJS's time, 0.20 with the JIT, and
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
  rather than making a rope (`00ce03b5`): `string_build2c` takes 0.19 times
  QuickJS's time, `local_append_keep` 0.27 and `template_list` 0.35. The
  interpreter is ahead of the JIT on the first of these, 0.29 times with it.
- **JSON.** The 49 `json_` tests take 0.31 times QuickJS's time, and 0.31
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
  frame (`746d9438`, `bdab5ba4`): `forward_construct` takes 0.24 times
  QuickJS's time, and 0.02 with the JIT. It took 0.54 at `258b4e96`, 0.23
  at `3ecb8200`, 0.32 at `2f05d3fc`, 0.25 at `c2dae008` and 0.24 at
  `b8c2c45f`.

What remains in the interpreter is mostly in Go: the operations with no
native handler and the fast paths that miss, which exit to their Go
handlers, the built-ins and the object model -- the gaps above are in
them -- and the collector. The JIT is the way past the rest: a hot
function compiled to native code dispatches nothing, which is why, with
it, the loops and calls above take from a third of the interpreter's
time down to a tenth.

[goja]: https://github.com/dop251/goja
[v8-v7]: https://github.com/mozilla/arewefastyet/tree/master/benchmarks/v8-v7
