# Benchmarks

Every figure below was measured at `2c758a33`, the code of v0.44.0, on an
AMD Ryzen 5 3600 under Windows with Go 1.27.3, on 2026-11-24: the bundle
timings, the V8 comparison with goja, and the comparison with C QuickJS,
under [Compared with C QuickJS](#compared-with-c-quickjs), on the register
interpreter and the JIT that compiles its register code. Where a section
quotes an earlier figure, it names the revision it was measured at.

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
Windows has neither: there the peak is the process's peak working set,
`PeakWorkingSetSize` of `GetProcessMemoryInfo`, read from the process's
handle once it has exited.

On an AMD Ryzen 5 3600, windows/amd64, Go 1.27.3, `GOMAXPROCS=1`,
`TZ=UTC`, with `CGO_ENABLED=0 -pgo=off -trimpath` and JIT disabled,
go-quickjs `2c758a33` gives these per-metric medians, measured on
2026-11-24. Compiler figures use five fresh processes, each running both
bundles with 500 ms per benchmark. Load figures use twenty fresh processes
per bundle, one load per process. One process runs at a time, with
30-second pauses between the five compiler rounds and between the twenty
load rounds. Each load round runs both bundles in separate processes,
alternating their order. No builds or tests ran beside them; a per-process
CPU log ran throughout, and the one load round during which another
process took a CPU was run again. All other samples are included,
including the first. The previous figures, at `b37daa23`, were an Apple M5
Max's under macOS, and do not compare with these.

| Bundle | Compile | First load |
|---|---:|---:|
| Three.js | 20.75 ms | 63.38 ms |
| Babel | 52.96 ms | 403.57 ms |

The first-load interval includes parsing, compilation and execution. It
excludes runtime construction, source reads, host setup and the functional
checks. The compiler-only interval starts after parsing and measures
compilation of the existing AST; it does not include work deferred to
executing the bundle.

Memory figures for the same runs are below. Allocations are total bytes
allocated per operation, in decimal MB. Peak load RSS is the whole test
process's peak working set, including its untimed setup and functional
checks, in MiB. Each metric is the median of its own samples:

| Bundle | Compiler MB/op | Load MB/op | Peak load RSS MiB |
|---|---:|---:|---:|
| Three.js | 5.04 | 19.24 | 36.50 |
| Babel | 13.28 | 86.31 | 103.86 |

The `qjs` binary built without the JIT, using `CGO_ENABLED=0 go build
-pgo=off -trimpath ./cmd/qjs`, is 44,797,952 bytes for windows/amd64. The
load test binary is 43,624,448 bytes, and the compiler test binary is
10,183,680 bytes. The
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
[goja]. All three go-quickjs columns use revision `2c758a33`, measured alongside
goja on 2026-11-24. Each column is the median of three fresh-process runs
on an AMD Ryzen 5 3600 under Windows with Go 1.27.3, with 30 seconds
between runs. One engine ran at a time, on every CPU, with the order
rotated each round. Each runner was built with DYNAMICBASE cleared, so
that Windows loads it at its image base, and each process ran from a
fresh copy of it, as under [Compared with C
QuickJS](#compared-with-c-quickjs). JIT off was built without the
`quickjs_jit` tag. JIT on compiles
synchronously; JIT background enables the JIT and compiles in the background
with `-jit -jitbackground`. Both use the same binary built with
`-tags quickjs_jit` and the runner's 32 MiB JIT budget rather than the
runtime's default 8 MiB. This revision uses the register interpreter and
register JIT by default.
Higher suite scores are better; the relative columns divide each
go-quickjs score by goja's.

| Workload | go-quickjs JIT off | go-quickjs JIT on | go-quickjs JIT background | goja | JIT off / goja | JIT on / goja | JIT background / goja |
|---|---:|---:|---:|---:|---:|---:|---:|
| Richards | 1,419 | 5,771 | 5,841 | 178 | 7.97x | 32.42x | 32.81x |
| DeltaBlue | 1,519 | 6,380 | 6,341 | 221 | 6.87x | 28.87x | 28.69x |
| Crypto | 2,372 | 8,278 | 8,395 | 114 | 20.81x | 72.61x | 73.64x |
| RayTrace | 3,929 | 12,937 | 12,492 | 267 | 14.72x | 48.45x | 46.79x |
| EarleyBoyer | 4,466 | 5,819 | 5,879 | 464 | 9.62x | 12.54x | 12.67x |
| RegExp | 2,872 | 4,075 | 4,047 | 220 | 13.05x | 18.52x | 18.40x |
| Splay | 6,715 | 10,252 | 10,186 | 976 | 6.88x | 10.50x | 10.44x |
| NavierStokes | 4,927 | 17,078 | 16,981 | 183 | 26.92x | 93.32x | 92.79x |
| **Composite score** | **3,105** | **7,951** | **7,919** | **261** | **11.90x** | **30.46x** | **30.34x** |

The runner's timed suite interval includes runtime construction, parsing,
compilation, the suite's warmups and its measured iterations, but excludes
reading the source files and the final forced garbage collection. CPU time
includes work on other threads, including the collector. These are the
go-quickjs and goja runs:

| Metric | go-quickjs JIT off | go-quickjs JIT on | go-quickjs JIT background | goja | JIT off relative to goja |
|---|---:|---:|---:|---:|---:|
| Suite wall time | 22.40 s | 20.27 s | 20.23 s | 120.59 s | 5.38x faster |
| Suite CPU time | 27.26 s | 27.30 s | 27.53 s | 162.34 s | 83.2% less |
| Total allocation | 3.69 GiB | 6.36 GiB | 6.33 GiB | 26.57 GiB | 86.1% less |

Score mode does different amounts of work as throughput changes, so the CPU
and allocation totals are not equal-work efficiency measurements. The
composite is the median of the three printed composite scores, not a
composite calculated from the workload medians.

The suite warms every workload for at least one second, then measures for at
least another second and continues until it has 32 measured iterations. All
engines evaluated the same benchmark sources and validation code.
The measured revisions were go-quickjs `2c758a33`, goja `a177d211bf60`, and
AreWeFastYet `0e21608`. The isolated goja runner pins
`v0.0.0-20261102223426-a177d211bf60` for this comparison. These results
characterize this older workload suite rather than every application;
go-quickjs leads all eight of its workloads in this comparison.

The comparison used `internal/cmd/v8bench/matrix`, selecting `gq`, `gqjit`,
`gqjitbg` and `goja` in separate invocations with `-suites score -runs 1
-parallel=false -v8 SUITE_DIR` and a separate `-out` directory for each
sample, with 30-second pauses. The engine orders were `gq,gqjit,gqjitbg,goja`,
`gqjit,gqjitbg,goja,gq` and `gqjitbg,goja,gq,gqjit`. The tables are the
medians of the saved outputs. A per-process CPU log ran beside the runs,
and a sample during which another process took more than half a CPU was
run again, 30 seconds after the last: JIT off's second and goja's first
and third. Each engine's three binaries were byte-identical; both JIT
modes used the same binary. The engine source matched the revision above
throughout measurement.

The composite samples were:

| Mode | Run 1 | Run 2 | Run 3 |
|---|---:|---:|---:|
| JIT off | 3,106 | 3,001 | 3,105 |
| JIT on | 7,978 | 7,951 | 7,914 |
| JIT background | 8,001 | 7,910 | 7,919 |
| goja | 261 | 261 | 261 |

All samples were included. The table takes the median of each workload and
of the printed composite scores independently. Background compilation's
composite was 0.40% lower than synchronous compilation's; this run does not
establish an overall throughput advantage for either compilation mode. The
previous table, at `1d08a62d` on an Apple M5 Max, had composites of 7,951,
20,130 and 20,218 against goja's 776; the ratios here are larger because
goja is slower on this machine, relative to go-quickjs, than on that one.

## Compared with C QuickJS

Over the V8 suite as a whole, go-quickjs is well ahead of QuickJS. Its
interpreter takes 0.31 times QuickJS's time and has 2.32 times its
composite score, ahead on all eight workloads. With its JIT it takes 0.20
times QuickJS's time and scores 5.9 times as much. On the larger programs
of Octane and Are We Fast Yet the interpreter takes 0.42 and 0.63 times
QuickJS's time, and the JIT 0.28 and 0.20 times
([below](#octane-and-are-we-fast-yet)); a page React 18 renders on the
server takes 0.60 times, and 0.51 with the JIT
([below](#server-rendering)). Single operations cost about half as much,
0.49 times QuickJS's time by the geometric mean of its micro-benchmarks,
and 0.24 times with the JIT, as [the micro-benchmarks](#micro-benchmarks)
below show; the slowest of them takes 1.5 times as long.

At `2c758a33`, the revision measured here, whose code v0.44.0 is,
go-quickjs compiles every
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
`5ae2317e`). The page's previous figures, at `7f8205ec` and, for the
JIT's column, `329ee166`, whose code v0.43.0 is, were of the same design
before the changes below, which are mostly the JIT's: native code's
conversions of short decimal strings, its element writes without entries
and its constant keys, its pushes grown and its empty literals' room
learned natively, the interpreter's string additions and writes of a
primitive's property learned before the first compile, reads past four
shapes compiled megamorphic, and a compile path that does less work. The
same suites ran on an AMD Ryzen 5 3600 under Windows, with Go 1.27.3 and
QuickJS 2026-06-04, go-quickjs `2c758a33` for all three columns, in one
run of the matrix. Two runs went at once, each on the six CPUs of a level
3 cache of its own, the JIT's beside the plain build's. The JIT's column
is a build with `-tags quickjs_jit` run with `-jit`. Each runner was built
with DYNAMICBASE cleared, so that Windows loads it at its image base, and
each process ran from a fresh copy of it (`-fixedbase`, `-freshcopy`,
`a9c5e457`), as for the previous figures (see where a program is loaded,
below). The figures are milliseconds for the same fixed work, the median
of three fresh-process runs (lower is better):

| Workload | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| Richards | 8.5 ms | 8.4 ms | 11.6 ms | 0.73x | 0.72x |
| DeltaBlue | 13.9 ms | 17.5 ms | 24.3 ms | 0.57x | 0.72x |
| Crypto | 156 ms | 54.9 ms | 329 ms | 0.47x | 0.17x |
| RayTrace | 66.8 ms | 51.2 ms | 142 ms | 0.47x | 0.36x |
| EarleyBoyer | 166 ms | 113 ms | 399 ms | 0.42x | 0.28x |
| RegExp | 149 ms | 129 ms | 946 ms | 0.16x | 0.14x |
| Splay | 121 ms | 98.0 ms | 499 ms | 0.24x | 0.20x |
| NavierStokes | 102 ms | 35.7 ms | 191 ms | 0.53x | 0.19x |
| **Total** | **798 ms** | **523 ms** | **2,613 ms** | **0.31x** | **0.20x** |

The suite's own scores, the median of three (higher is better);
go-quickjs's are from a second run of the scored suite alone, its builds
the same bytes, after another process took most of a CPU during one of the
first's:

| Workload | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| Richards | 1,405 | 5,745 | 952 | 1.48x | 6.03x |
| DeltaBlue | 1,507 | 6,387 | 843 | 1.79x | 7.58x |
| Crypto | 2,344 | 8,255 | 1,110 | 2.11x | 7.44x |
| RayTrace | 3,829 | 12,580 | 1,579 | 2.42x | 7.97x |
| EarleyBoyer | 4,450 | 5,671 | 2,184 | 2.04x | 2.60x |
| RegExp | 2,831 | 3,852 | 393 | 7.20x | 9.80x |
| Splay | 6,625 | 9,724 | 3,317 | 2.00x | 2.93x |
| NavierStokes | 4,991 | 16,918 | 2,412 | 2.07x | 7.01x |
| **Composite** | **3,072** | **7,802** | **1,325** | **2.32x** | **5.89x** |

At `7f8205ec` and `329ee166` the interpreter took 0.31 times QuickJS's
time over the suite and had 2.31 times its composite score, and the JIT
0.20 and 5.84 times. Both totals are level, 802 to 798 ms and 526 to 523
with the JIT, and so are the composites, 0.6% and 0.7% higher. With the
JIT, DeltaBlue took 17.5 ms where it took 16.2, its three runs 16.9 to
20.9 ms, and Splay 98.0 where it took 93.5, its runs 87.7 to 104: a
fixed run of DeltaBlue lasts a few milliseconds, much of it before the
JIT's code takes over, and Splay's is mostly the collector's, and over
four placements of each build their warm runs take 6.8% less time and
1.2% more than at `329ee166`. QuickJS's total is 2,613 ms where it was 2,581, its composite
1,325 as it was.

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
  and the JIT 0.29 times, where their scores are 2.32 and 5.89 times as
  high.
- The JIT's lead is much larger in the scores than in fixed time. A fixed
  run of Richards or DeltaBlue lasts a few milliseconds, and a function
  runs in the interpreter until it is hot enough to compile, so much of
  that run is spent before the JIT's code takes over: with the JIT,
  Richards takes 0.99 times the interpreter's fixed time and DeltaBlue
  1.26 (DeltaBlue's three runs with it took 16.9 to 20.9 ms). The scores
  time warm runs, where Richards, DeltaBlue, Crypto, RayTrace and
  NavierStokes score 3.3 to 4.2 times what the interpreter does.
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
  Splay, 1.27 and 1.47 times the interpreter's score, and RegExp, 1.36
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
docs/optimizing.md, "Builds and the machine"), as the previous figures
were. To reproduce these figures, the suites below and
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
three fresh-process runs (lower is better), from the same run of the
matrix as the V8 suite's.

| Octane | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| Richards | 3.0 ms | 5.0 ms | 4.0 ms | 0.75x | 1.25x |
| DeltaBlue | 5.2 ms | 7.7 ms | 7.7 ms | 0.68x | 1.00x |
| Crypto | 52.1 ms | 23.1 ms | 109 ms | 0.48x | 0.21x |
| RayTrace | 20.9 ms | 32.9 ms | 50.2 ms | 0.42x | 0.66x |
| EarleyBoyer | 71.2 ms | 64.5 ms | 135 ms | 0.53x | 0.48x |
| RegExp | 116 ms | 115 ms | 506 ms | 0.23x | 0.23x |
| Splay | 114 ms | 81.8 ms | 465 ms | 0.24x | 0.18x |
| NavierStokes | 41.9 ms | 18.0 ms | 67.1 ms | 0.62x | 0.27x |
| PdfJS | 199 ms | 267 ms | 380 ms | 0.52x | 0.70x |
| Mandreel | 2,499 ms | 785 ms | 3,347 ms | 0.75x | 0.23x |
| Gameboy | 389 ms | 287 ms | 459 ms | 0.85x | 0.62x |
| CodeLoad | 12.9 ms | 13.7 ms | 12.9 ms | 1.00x | 1.06x |
| Box2D | 88.1 ms | 97.4 ms | 154 ms | 0.57x | 0.63x |
| zlib | 1,588 ms | 567 ms | 8,314 ms | 0.19x | 0.07x |
| Typescript | 1,487 ms | 1,950 ms | 2,134 ms | 0.70x | 0.91x |
| **Total** | **7,059 ms** | **4,687 ms** | **16,779 ms** | **0.42x** | **0.28x** |

| Are We Fast Yet | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| DeltaBlue | 558 ms | 338 ms | 724 ms | 0.77x | 0.47x |
| Richards | 3,216 ms | 661 ms | 4,800 ms | 0.67x | 0.14x |
| Json | 1,006 ms | 257 ms | 1,346 ms | 0.75x | 0.19x |
| CD | 1,613 ms | 492 ms | 2,848 ms | 0.57x | 0.17x |
| Havlak | 3,387 ms | 1,175 ms | 4,384 ms | 0.77x | 0.27x |
| Bounce | 755 ms | 244 ms | 1,492 ms | 0.51x | 0.16x |
| List | 549 ms | 158 ms | 1,264 ms | 0.43x | 0.13x |
| Mandelbrot | 178 ms | 40.4 ms | 689 ms | 0.26x | 0.06x |
| NBody | 663 ms | 120 ms | 1,027 ms | 0.65x | 0.12x |
| Permute | 872 ms | 228 ms | 1,715 ms | 0.51x | 0.13x |
| Queens | 662 ms | 125 ms | 1,058 ms | 0.63x | 0.12x |
| Sieve | 561 ms | 315 ms | 1,221 ms | 0.46x | 0.26x |
| Storage | 1,486 ms | 683 ms | 1,698 ms | 0.88x | 0.40x |
| Towers | 899 ms | 207 ms | 1,560 ms | 0.58x | 0.13x |
| **Total** | **16,413 ms** | **5,081 ms** | **25,922 ms** | **0.63x** | **0.20x** |

- Octane's zlib takes 0.19 times QuickJS's time, 1,588 ms, and 0.07 times
  with the JIT, 567 ms (1,617 and 582 at `7f8205ec` and `329ee166`). zlib
  is an asm.js module, compiled from C by Emscripten, and its 51 functions
  pass the validator (`b920ca25`): each runs as typed code (`047ee156`,
  `d8cbc918`), whose int32 and float64 registers hold raw words, so its `|
  0` arithmetic, its compares and its heap's loads and stores convert
  nothing and check no kind; as ordinary register code, before v0.39.0, it
  took 5,675 ms, converting each element read and each `| 0` through a
  float. With the JIT the typed code is compiled at the module's link, as
  V8 compiled validated asm.js at its instantiation, with no guard on any
  kind (`bc00f894`, `5ae2317e`). A phi and its arguments share a spill
  slot wherever no two of them are live at once, not only where their
  intervals do not meet, as V8's allocator merges spill ranges
  (`ebe73f44`): zlib's largest function, 47 loops entered by on-stack
  replacement with some 240 values live through each, had copied its phis
  slot to slot at every edge, and its code went from 1.80 MB to 439 KB.
  Made for every pair before the allocator's scan, the classes had made
  its phase of hints four to six times as long; they are made only for the
  pairs of a component one of whose values spills, when the scan first
  spills it (`4eff9cf3`). Since `329ee166` a phi and an input it takes on
  many edges are paired once, at the first (`4b780683`: zlib's largest
  function had 77,199 pairs, of which 57,793 are distinct), and a
  component no two of whose values are live at once is one class, found by
  one walk of the dominator tree for all of them (`7c5dbfd7`). A run of
  all of a suite's compiles, timed with `-jitcompile`, now spends 11 to
  18% longer on the hints and the scan together than with no classes,
  where `329ee166`'s code spent 15 to 36% longer (`7c5dbfd7`'s message has
  the figures). The heap's base and length are loaded once outside a loop
  that calls nothing, kept round a loop entered from an entry, and live in
  their spill slot, read in place by each access, taking no register
  (`47f6294a`, `6956a938`, `948a2cb1`, `6fb4805f`); a call through a
  function table of more than four functions is made by index
  (`6fbcf73b`); and a branch too far for 16 bits makes only itself far,
  where it made the whole function's jumps far, or left the function
  without typed code (`5d69103e`). The JIT also inlines a typed callee
  into typed code, an exit inside it writing its raw slots raw
  (`4ae95d8b`, `de072cfa`), and takes a module function's result as the
  number its type says, unchecked (`a4bfc824`): `asm_call_leaf`, a call of
  a small function a turn, takes 5.67 ns with the JIT, where it took 13.3
  in v0.39.0. The stack JIT of v0.38.0 took 2,027 ms, measured in
  `3ecb8200`'s session.
- Mandreel, compiled from C++ too, is not asm.js and runs as ordinary
  register code: 2,499 ms, 0.75 times QuickJS's time, and 785 with the
  JIT, where it took 889 at `329ee166` (below). Its largest function is
  compiled at its first call, within the timed run (`d6ac45be`). Its `| 0`
  arithmetic runs as int32 instructions (`add_i32`, `add_imm_i32`, the
  bitwise operators with an immediate) and its typed arrays' elements are
  read and written by native handlers, but each element read and each `|
  0` still converts through a float: raw registers outside asm.js are
  designed and not built (register-interpreter.md, "Known limits and open
  work").
- Without the JIT, go-quickjs is ahead on every benchmark of both suites
  but Octane's CodeLoad, which is mostly compiling: it takes as long as
  in QuickJS, 12.9 ms, and 1.06 times QuickJS's time with the JIT, where
  it took 0.98 and 1.14 at `7f8205ec` and `329ee166` and 1.22 and 1.37
  at `258b4e96`, before the front end compiled to register code alone
  (`d6ac45be`). On Are We Fast Yet the interpreter is 23% ahead on
  DeltaBlue and on Havlak, a third on Richards, 37% on Queens and 42% on
  Towers. A call's window of registers is the callee's frame, so nothing
  is copied, a frame record is written only where a reader needs it, and
  on amd64 and arm64 a call, a construction and a return are native
  handlers that never enter Go ([Calls](#where-the-time-goes) below). The
  programs that allocate most are the least ahead: Storage 12%, Havlak
  23%. Since `7f8205ec` the interpreter's totals are level, Octane's
  7,105 to 7,059 ms and Are We Fast Yet's 16,416 to 16,413. Octane's
  EarleyBoyer took 71.2 ms in the matrix where it took 59.0, its three
  runs 57.9 to 76.0 ms, and 64.5 with the JIT where it took 45.8, its
  runs 48.9 to 67.1; over four placements of each build its warm runs
  are level, 55.1 against 55.5 ms and with the JIT 32.8 against 32.8: a
  fixed run of it is mostly what the collector does, and moves with
  where a collection falls.
- Octane's total with the JIT is 4.6% lower, 4,915 to 4,687 ms: Mandreel
  889 to 785, Gameboy 327 to 287, RegExp 135 to 115. Over four placements
  of each build their warm runs take 6.9%, 6.1% and 3.2% less time, PdfJS
  6.4% less and Typescript 3.1% less, each at every placement. A string
  addition the interpreter has made of a string is marked in the register
  code, so that the first compile makes it Go's rather than speculating on
  numbers and learning the string at a guard's failure (`2e86ab34`,
  `3205fa7d`), and a built-in called in tail position, as `return
  String.fromCharCode(c)`, is specialized as any call of it is
  (`89480e50`); where they landed the two took 3.0 and 3.5% off PdfJS's
  warm runs. A fresh call that native code enters at its start keeps the
  arrays of `exec` that the call before made (`39b5e734`), 3.3% off
  RegExp's where it landed. A read whose cache has met a fifth shape is
  compiled megamorphic, as V8's is, where it was compiled for four and
  deoptimized at each fifth (`0d6e1eaa`): 78 of Typescript's 265 shape
  deoptimizations in three iterations were at such sites. Mandreel's and
  Gameboy's loops write arrays and typed arrays, which an element write's
  entry made only where Go goes on after it (`e56d306f`) and a constant
  key taken as the index (`18752332`) make shorter; those two are the
  likeliest cause, though no build between them was timed. Are We Fast
  Yet's total with the JIT is level, 5,046 to 5,081 ms; its DeltaBlue took
  338 ms in the matrix where it took 315, and over four placements of each
  build 0.8% longer. Storage takes 683 ms with the JIT where it took
  698.
- With the JIT, Are We Fast Yet takes 0.06 to 0.47 times QuickJS's time,
  its loops of arithmetic the least, its programs that allocate most --
  DeltaBlue, Storage -- the most. Richards' runs with the JIT fall in two
  groups, of about 650 to 680 ms and of 745 to 940: its three in the
  matrix took 659, 661 and 670 ms, and at `329ee166` 657, 658 and 664.
- When the stack JIT was removed, its timing had the register JIT slower
  than the stack JIT just before it on Octane's RayTrace (+7.6%) and
  CodeLoad (+25%) and on Are We Fast Yet's Json (+8.2%), Permute (+7.0%)
  and Queens (+6.3%) (docs/performance-plan.md, "The register JIT's
  remaining gaps"). Against the stack JIT's figures at `acffb1aa` (270,
  227 and 119 ms), Json is 5% shorter, 257 ms, Permute level, 228, and
  Queens 5% longer, 125 ms; CodeLoad took 14.2 ms then and takes 13.7.
  RayTrace took 32.9 ms, its runs with the JIT 31.9 to 33.6 ms, where
  they took 31.2 to 33.9 at `329ee166`.
- On seven of Octane's fifteen the JIT is behind the interpreter:
  Richards (67% behind), RayTrace (57%), DeltaBlue (48%), PdfJS (34%),
  Typescript (31%), Box2D (11%) and CodeLoad (6%); RegExp, 17% behind at
  `329ee166`, is level, 115 ms against 116. Octane's fixed work is one run
  of each benchmark, so compiling counts, and the code runs in the
  interpreter until it is hot; a runtime with the JIT keeps the frames
  the plain interpreter's calls and constructions do without. The JIT's
  own figures are close to what they were at `329ee166` -- Richards 5.2
  ms then, DeltaBlue 7.7, RayTrace 32.2, PdfJS 266, Typescript 1,988 --
  but for Typescript's, 1,950 ms, and the interpreter's are as they
  were.

### Server rendering

The ssr suite renders a page with React 18's own builds, as a host behind
a web server does for each request: Render is one runtime rendering the
page again and again, FirstRender five fresh runtimes, each loading React
and rendering once. The same matrix, the median of three fresh-process
runs (lower is better):

| ssr | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| Render | 1,126 ms | 961 ms | 1,886 ms | 0.60x | 0.51x |
| FirstRender | 150 ms | 276 ms | | | |

Render took 0.58 and 0.49 times QuickJS's time at `7f8205ec` and
`329ee166`, 1,086 and 929 ms, and FirstRender 150 and 281 ms. Render took
1,126 ms here, its three runs 1,085 to 1,153 ms, and 961 with the JIT,
its runs 932 to 992; over four placements of each build its warm runs
are level, 215.0 against 215.6 ms, and with the JIT 177.7 against 177.7.
FirstRender is 150 and 276 ms.
A props object's key that some of a site's objects lack is cached beside
the shapes that have it, and so is a function's `prototype` once made
(`6521aac5`), which took the render's reads that exit to Go from 106,527
to 35,402, its first render's from 10,596 to 3,724. Most of the rest
were `defaultProps`, which a render reads of every element's type: a
tag's name is a string, whose prototypes have no such key, and the read
walked String.prototype's table and Object.prototype's each time. A key
a primitive's prototypes lack is remembered with the chain's cell, which
a change to any of them invalidates (`18acbcb9`).
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
takes about 150 ms for, all three ran that same work in fresh processes,
all three at `2c758a33`, and the figure is the fastest of three runs. The
runners' micro mode does this, from a copy of the file kept in `internal/v8bench`, and
the matrix above runs it. `sort_bench` is left out: it reports a run's time
over its fastest sort, not the time of an operation. Over the other 71 tests
go-quickjs takes 0.49 times QuickJS's time by the geometric mean, where
it took 0.48 at `7f8205ec`, and v0.38.0, the stack design, 0.79 in
`3ecb8200`'s session: it is slower on 6, faster on 62, and within 5% on
the other three. With the JIT it takes 0.24 times QuickJS's time, where it
took 0.27 at `329ee166`, slower on 2, faster on 65 and within 5% on 4.
The table has the interpreter's largest gaps either way:

| Test | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| `bigint256_arith` | 192 ns | 126 ns | 127 ns | 1.51x | 0.99x |
| `weak_map_delete` | 373 ns | 437 ns | 248 ns | 1.50x | 1.76x |
| `weak_map_set` | 126 ns | 92.9 ns | 92.8 ns | 1.36x | 1.00x |
| `int_to_string` | 61.2 ns | 49.3 ns | 49.6 ns | 1.23x | 0.99x |
| `date_parse` | 970 ns | 986 ns | 828 ns | 1.17x | 1.19x |
| `func_closure_call` | 35.8 ns | 2.81 ns | 31.8 ns | 1.13x | 0.09x |
| `regexp_utf16` | 248 ns | 247 ns | 254 ns | 0.98x | 0.97x |
| `prop_create` | 50.1 ns | 37.8 ns | 51.3 ns | 0.98x | 0.74x |
| `array_prop_create` | 14.2 ns | 11.1 ns | 14.9 ns | 0.95x | 0.74x |
| `float_toExponential` | 163 ns | 139 ns | 183 ns | 0.89x | 0.76x |
| `global_func_call` | 8.18 ns | 2.13 ns | 32.8 ns | 0.25x | 0.06x |
| `prop_update` | 2.79 ns | 1.90 ns | 11.6 ns | 0.24x | 0.16x |
| `func_call` | 6.84 ns | 0.95 ns | 31.3 ns | 0.22x | 0.03x |
| `array_length_decr` | 12.8 ns | 9.68 ns | 59.1 ns | 0.22x | 0.16x |
| `array_for_in` | 15.2 ns | 12.2 ns | 76.9 ns | 0.20x | 0.16x |
| `array_hole_length_decr` | 12.9 ns | 10.1 ns | 69.9 ns | 0.18x | 0.14x |
| `string_build2c` | 26.0 ns | 27.9 ns | 142 ns | 0.18x | 0.20x |
| `array_slice` | 4.29 ns | 4.44 ns | 27.3 ns | 0.16x | 0.16x |
| `prop_clone` | 11.3 ns | 9.32 ns | 71.9 ns | 0.16x | 0.13x |
| `array_for_of` | 7.24 ns | 4.94 ns | 46.2 ns | 0.16x | 0.11x |

Of the gaps the stack design had, the largest are gone: in v0.38.0,
measured in `3ecb8200`'s session, `array_prop_create` took 2.25 times
QuickJS's time and takes 0.95, `global_read` 1.79 and 0.86, `array_read`
1.41 and 0.86, `empty_do_loop` 1.34 and 0.53.

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
and 0.49 and 0.47 without it. Code that keeps its objects gains nothing
from this, and the suites above hardly move for it.

The interpreter has gained more than the JIT, so some tests run slower
with the JIT than without it. In the run made for this page, 6 of these
71 took 7% or more longer with the JIT: `prop_delete`,
`global_destruct`, `global_destruct_strict`, `weak_map_delete`,
`string_build2c` and `string_build_large1`; no longer `array_update`,
`array_prop_create`, `string_build1` and `string_to_int` (below). Of the
extra tests below that QuickJS runs, 22 did, where 27 had at
`329ee166`: three of the async tests, `hasOwnProperty` called through a
borrowed reference three ways, a spread call, `prop_delete_last`,
`array_map_tail_args`, `template_numbers`, four of the sorts, a global
`match`, a case-insensitive search, three JSON tests of additions, new
layouts and a prototype changed between calls, and, since the
interpreter's are faster, `Object.create` of a fresh object,
`Object.setPrototypeOf` to one and a subclass made per call; no longer
`generator_delegate`, `async_call`, a push of five values, the spread of
a large object, `object_rest`, a for-in over a dictionary,
`carried_exit_loops`, `array_index_append`, nor the JSON tests of
instances and getters.

Since `7f8205ec` and `329ee166` the gains are the JIT's. A string of one
to nine ASCII digits, with at most one point among them, is converted to
its number natively, where each conversion was a call of Go
(`ad5bd2dd`): with the JIT `string_to_int` takes 7.22 ns where it took
45.5, and `string_to_float` 8.87 where it took 63.7. An element write has
an entry after it only once Go has gone on there, so that a loop of
writes is one block with one check of its array, and a constant integer
key is the element's index as it is, converted and checked at no access
(`e56d306f`, `18752332`): with the JIT `array_update` takes 1.92 ns where
it took 3.54, `array_write` 1.15 where it took 2.19, `array_read` 0.92
where it took 1.42, and the typed arrays' element writes 1.42 to 2.14 ns
where they took 2.24 to 3.35. A push of one value with no room left is
grown by push's leaf natively, and an empty literal native code makes
learns the room its arrays grow to from them, as the interpreter's site
does (`a7abd0b4`): `array_push` takes 10.4 ns with the JIT where it took
18.2, `array_push_args3` 24.1 where it took 56.0, `array_push_args5` 36.4
where it took 80.1, and `array_index_append` 13.0 where it took 22.4,
0.76 times QuickJS's time where it took 1.29. A write site remembers the
interpreter's writes of a primitive's property, so that the code compiled
for it makes them from the first compile (`817ef6eb`):
`carried_exit_loops`, whose 24 loops each write a number's property and
which failed a guard at each loop's write and was compiled again 23
times, takes 69.8 ns where it took 188. Native code's calls of a
built-in through Go are made by the direct call's routine itself
(`4e161661`): `has_own_call_for_in` takes 90.9 ns with the JIT where it
took 101. The interpreter's figures are level: over the 418 tests, by the
geometric mean, 1.001 times their figures at `7f8205ec`, and the JIT's
0.954 times theirs at `329ee166`.

Some of these tests depend on where the linker places the code more than
on the code. The layout lock keeps the interpreter's hot code where it
measured best, but the code between its anchors, the Go handlers after the
loop and the closures still move with every change to the package, and a
change of a few per cent in one of these tests is not by itself a change in
the engine. A pad that grows past a page moves everything after it 4 KB,
every offset modulo 4 KB the same (docs/optimizing.md, "The layout
lock"). Each round's changes are followed by the lock's pads written
anew, as this one's were (`c4d52f54`, `2c758a33`).

Each row slower than at `7f8205ec` or `329ee166` by 7% or more in the
matrix's run was timed again over four placements of each build, loaded at
a fixed base. In the interpreter `int_to_string` takes 61.2 ns where it
took 56.8, and 5.1% longer over the placements, at three of the four;
`bigint256_arith` 192 where it took 184, 3.6% longer, at two. Both make
additions that leave add's fast path, of a number and a string and of two
BigInts, and add's slow path now marks a string addition for the JIT's
first compile through calls of its own, out of line, so that the Go
handlers keep their sizes (`2e86ab34`). In a build without the JIT the
mark is never made, but the slow path's call is there: over four
placements of that commit's build and its parent's, `int_to_string` is
2.9% slower and `bigint256_arith` 4.2%, each at two of the four. With the
JIT `append_pair_read`, two appends to a captured string and a read of its
length a turn, takes 49.6 ns in the matrix where it took 34.5, and 6.3%
longer over the placements, at two of the four; `html_escape_plain` 3.6%
longer, at three; their code was not compared, and a cause is not
established. The rest of the rows 7% or more slower in the matrix with the
JIT -- `bigint256_arith`, `upvalue_short_append`, `forward_array`,
`string_from_code_point`, `upvalue_two_appends`, `append_read_each`,
`map_set_int`, `typed_u8_map` and `array_filter` -- are within 3.2% over
the placements, none slower at every placement, and `forward_array` 2.5%
faster; `string_trim` without the JIT, 7.2% slower in the matrix, is level
over them.

The rows `7f8205ec`'s page left slower or open:

- with the JIT `string_indexOf` and `string_concat` take 77.4 and 102 ns
  (86.7 and 120 at `329ee166`, 77.7 and 104 at `b5df63ea`), and
  `has_own_call_for_in` 90.9 (101), above; `string_trim` 56.4 (61.7);
- `json_stringify_decimals` takes 34.7 ns (34.1 at `7f8205ec`, 32.9 at
  `b8c2c45f`), `json_stringify_ints` 24.7 (25.0, 24.8), and with the JIT
  `json_stringify_unique` 126 (123, 114);
- `string_indexOf` takes 80.4 ns (91.4, 82.6), `json_stringify_strings`
  31.3 (30.9, 30.3), and with the JIT `array_indexOf` 103 (105, 102);
- `json_stringify_dict` takes 80.2 ns (83.3, 80.0) and
  `string_starts_with` 71.6 (75.4, 69.0); with the JIT `string_template`
  176 (181, 172), `string_code_point_at` 5.34 (5.17, 5.41),
  `has_own_call_miss` 91.7 (90.4, 78.3) and `string_from_code_point`
  88.4 (80.8, 75.1), the last 1.0% slower over four placements of each
  build;
- `generator_next` takes 102 ns (102), `dataview_write` 61.7 (61.4) and
  `array_literal_empty` 72.6 (71.1);
- Are We Fast Yet's Storage takes 1,486 ms (1,492), and 683 with the JIT
  (698).

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
times its time, and 0.22 times with the JIT (0.502 and 0.221, where it
took 0.497 and 0.227 at `7f8205ec` and `329ee166`), and v0.38.0 0.70 and
0.30 over its 313; leaving out the JSON tests, 0.54 and 0.21. The asm.js
kernels' loops (`asm_adler`, `asm_crc32`, `asm_copy`, `asm_fnv`,
`asm_dot`, `asm_switch`) take 0.12 to 0.28 times QuickJS's time as typed
code, and 0.03 to 0.07 with the JIT, which compiles them at the module's
link; their recursion and calls (`asm_fib`, `asm_call_leaf`) 0.80 and
0.54, 0.22 and 0.09 with the JIT, which inlines a typed callee
(`4ae95d8b`) and calls typed code natively from the ordinary code that
drives it (`d14b7cea`), and a call through a function table 1.01, 0.16
with the JIT. A switch on strings past its first few cases
takes 0.05 to 0.16 times QuickJS's time, its literal cases one lookup
(`100eb7f`). An await costs a fiftieth of QuickJS's time (`async_await`
15.4 against 751 ns): an async function resumed by its await's job goes
on in place at an await of a value that needs no job of its own. A
generator resumed from for-of is level (`generator_for_of` 1.02), where
v0.38.0 took 1.44 times, and a for-in over a dictionary takes 0.22 times,
where it took 1.41: its keys are made once for the dictionary's layout and
handed out by the native handlers. Calls in tail position take 0.56 to
0.60 times QuickJS's time, and a `map` callback that returns a call in
tail position 0.46, level with one that returns an ordinary call: such a
callback is entered directly, as any other is (`400fc19c`). A call of
`charCodeAt` whose method is read before its argument, as in
`s.charCodeAt(++i)`, is answered by the call handler from its target, as
V8's call feedback does (`b90b70c4`): `string_charcode_update` takes 0.34
times QuickJS's time. A site that calls fresh closures of two templates in
turn misses its cache at each call and retries the leaves the native call
handler answers (`ab16fd73`), and since v0.43.0 a closure's capture list
is filled directly (`648d7d30`); `closure_leaf_miss` takes 0.98 times,
where it took 1.14 in v0.42.0. A global `exec` still takes 1.33 times, as
it makes a result and a String for each match; a sticky one 1.05. The largest
gaps either way:

| Test | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| `array_literal_empty` | 72.6 ns | 63.5 ns | 41.2 ns | 1.76x | 1.54x |
| `scope_chain` | 653 ns | 667 ns | 371 ns | 1.76x | 1.80x |
| `typed_u8_find` | 45.5 ns | 9.21 ns | 27.2 ns | 1.67x | 0.34x |
| `set_proto_fresh` | 352 ns | 378 ns | 212 ns | 1.66x | 1.78x |
| `subclass_fresh` | 1,119 ns | 1,207 ns | 684 ns | 1.64x | 1.76x |
| `array_fill_down` | 138 ns | 141 ns | 86.2 ns | 1.60x | 1.63x |
| `typed_findIndex` | 47.3 ns | 9.00 ns | 30.5 ns | 1.55x | 0.30x |
| `typed_u64_reduce` | 115 ns | 114 ns | 75.2 ns | 1.53x | 1.51x |
| `typed_find` | 46.8 ns | 9.47 ns | 30.7 ns | 1.52x | 0.31x |
| `typed_u64_forEach` | 117 ns | 97.4 ns | 79.0 ns | 1.48x | 1.23x |
| `asm_copy` | 5.90 ns | 1.50 ns | 48.9 ns | 0.12x | 0.03x |
| `json_stringify_decimals` | 34.7 ns | 35.3 ns | 333 ns | 0.10x | 0.11x |
| `string_switch_last` | 27.4 ns | 12.9 ns | 265 ns | 0.10x | 0.05x |
| `string_switch_large` | 29.2 ns | 21.4 ns | 407 ns | 0.07x | 0.05x |
| `string_switch_miss` | 17.5 ns | 12.5 ns | 267 ns | 0.07x | 0.05x |
| `regexp_search_utf16` | 680 ns | 725 ns | 13,328 ns | 0.05x | 0.05x |
| `string_switch_nonstring` | 12.2 ns | 7.42 ns | 263 ns | 0.05x | 0.03x |
| `regexp_search_i` | 662 ns | 725 ns | 20,877 ns | 0.03x | 0.03x |
| `json_parse_long` | 1,848 ns | 1,870 ns | 85,832 ns | 0.02x | 0.02x |
| `async_await` | 15.4 ns | 17.2 ns | 751 ns | 0.02x | 0.02x |

A BigInt in the int64 range is held in the value itself (`9746906`), as
QuickJS holds one that fits in a word, and so is one below 2^64 in
magnitude (`f8c3229`); a larger one is a block of no pointers, made by the
engine's own arithmetic (`f1bf681`). Short BigInts' `+`, `-`, `*` and
comparisons are native handlers: `bigint64_arith` takes 0.36 times
QuickJS's time. A larger one is made by the engine's arithmetic in Go:
`bigint256_arith` takes 1.51 times, and the extra BigInt tests 0.42 to
1.23 times, `bigint_pidigits`, `bigint_toString`, `bigint_modpow` and
`bigint64_xorshift` the slowest. The JIT makes BigInts of 64 bits natively: `bigint64_arith`
takes 0.10 times QuickJS's time with it, `bigint64_xorshift` 0.05.

The largest gaps now are:

- objects made on a prototype that is itself new -- `Object.create` of a
  fresh object, `Object.setPrototypeOf` to one, a scope chain built as
  objects, a subclass made per call and its instance -- 1.44 to 1.76
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
  a `replace` by a template with `$` (1.33 and 1.34 times).

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
  0.75 by the mean. `int_arith` takes 0.46 times QuickJS's time and
  `float_arith` 0.35.
- **Calls.** A call's window of registers is the callee's frame: the
  caller writes the callee, the receiver and the arguments there, and
  nothing is copied. A callee the call site's cache knows is entered with
  no question asked again, and only the fields of the frame record a
  reader needs are written. Natively, a call, a construction and a return
  are handlers of their own that keep the caller's state on a native
  frame stack and never enter Go (register-interpreter.md, "Frames and
  calls"). A method call whose body needs a frame takes 33.8 ns against
  69.2 in QuickJS (`method_call`), one that only returns its argument
  12.4 against 42.5 (`method_call_leaf`), an empty function 6.84 against
  31.3 (`func_call`); with the JIT they take 11.4, 2.66 and 0.95 ns. A
  recursive call is level: `call_fib` and `call_mutual` take 0.93 times
  QuickJS's time. A body that only returns a parameter or a
  constant, or a short pure one of numbers and upvalues, is answered at
  the call site without a frame; natively, so are a getter of `this.k`,
  `this.k.length` or `this.k[p]`, a `typeof` test, an initializer that
  stores its parameters in `this`, and a constructor that only forwards
  to one (`d54099e3`, `09409332`, `bdab5ba4`); the initializer writes its
  values at the slots, and gives the final layout, that it remembers from
  the last construction of the same layout (`8505335a`): `class_new`
  takes 0.42 times QuickJS's time. `Object.create` of a prototype alone
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
  beside the shapes that have it, and a key a chain lacks is remembered by
  the chain's cell (`6521aac5`, `18acbcb9`); a write site that keeps
  several shapes adds to each of them natively (`d44da58c`); an
  `instanceof` site keeps the constructor's prototype slot too
  (`32036a73`), and a branch on its result is the same instruction
  (`1dcf124c`), with the read of a global constructor before it too
  (`03dadba8`). A keyed access converts its key through the runtime's
  table of key strings, also in sets of four ways (`6deb2e51`), and a
  keyed store remembers the shapes and keys its site met (`f04364dd`). Two
  adjacent reads of properties share one native dispatch (`4d5d636d`). The
  native handlers check the cache where the instruction runs: `prop_read`
  takes 0.49 times QuickJS's time, `prop_write` 0.33, `global_read` 0.85,
  `global_write` 0.39, `instanceof_class` 0.25, and `mega_read` and
  `mega_add`, at sites that meet a new layout at every call, 0.74 and
  0.71. With the JIT, a read whose cache has met a fifth shape is compiled
  megamorphic, reading through the shared cache natively (`0d6e1eaa`). An
  object holds only its values, its keys in its shape, as a QuickJS object
  does (`944e2e8`).
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
  QuickJS's time, 0.07 with the JIT, and the asm.js kernels' loops 0.12 to
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
  QuickJS's time, `local_append_keep` 0.27 and `template_list` 0.35, and
  0.20 times with the JIT on the first of these, where it took 0.29 in
  v0.42.0.
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
  (`typed_array_write`, 0.41, and 0.11 with the JIT).
- **`apply(this, arguments)`.** In a function whose only use of
  `arguments` is passing it to `apply` (a common way to write a class
  constructor), the arguments object is never made: the call hands on the
  frame's arguments as they were passed (`apply_args`). A constructor
  whose whole body is `this.initialize.apply(this, arguments)`, as
  RayTrace's are, is constructed by its call site as a forwarder, and
  natively, where the target is an initializer, without entering either
  frame (`746d9438`, `bdab5ba4`): `forward_construct` takes 0.19 times
  QuickJS's time, and 0.02 with the JIT. It took 0.54 at `258b4e96`, 0.23
  at `3ecb8200`, 0.32 at `2f05d3fc`, 0.25 at `c2dae008`, 0.24 at
  `b8c2c45f` and `b5df63ea` and 0.20 at `7f8205ec`.

What remains in the interpreter is mostly in Go: the operations with no
native handler and the fast paths that miss, which exit to their Go
handlers, the built-ins and the object model -- the gaps above are in
them -- and the collector. The JIT is the way past the rest: a hot
function compiled to native code dispatches nothing, which is why, with
it, the loops and calls above take from a third of the interpreter's
time down to a tenth.

[goja]: https://github.com/dop251/goja
[v8-v7]: https://github.com/mozilla/arewefastyet/tree/master/benchmarks/v8-v7
