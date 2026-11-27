# Benchmarks

Every figure below was measured at `f596215b`, after v0.45.2, on an
AMD Ryzen 5 3600 under Windows with Go 1.27.3, on 2026-11-27: the bundle
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
go-quickjs `f596215b` gives these per-metric medians, measured on
2026-11-27. Compiler figures use five fresh processes, each running both
bundles with 500 ms per benchmark. Load figures use twenty fresh processes
per bundle, one load per process. One process runs at a time, with
30-second pauses between the five compiler rounds and between the twenty
load rounds. Each load round runs both bundles in separate processes,
alternating their order. No builds or tests ran beside them; a per-process
CPU log ran throughout; another process took nearly half a CPU in the ten
seconds around the second load round, which was run again. All other
samples are included, including the first. The previous figures, at
`976e5cc1` on the same machine, were 20.60 and 54.19 ms to compile, 63.65
and 402.64 ms to load first, 5.04 and 13.27 compiler MB/op, 19.24 and
86.31 load MB/op and 36.91 and 103.76 MiB at the peak.

| Bundle | Compile | First load |
|---|---:|---:|
| Three.js | 20.81 ms | 64.47 ms |
| Babel | 54.90 ms | 405.68 ms |

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
| Babel | 13.28 | 86.31 | 103.90 |

The `qjs` binary built without the JIT, using `CGO_ENABLED=0 go build
-pgo=off -trimpath ./cmd/qjs`, is 44,936,704 bytes for windows/amd64. The
load test binary is 43,848,192 bytes, and the compiler test binary is
10,225,152 bytes. The
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
[goja]. All three go-quickjs columns use revision `f596215b`, measured alongside
goja on 2026-11-27. Each column is the median of three fresh-process runs
on an AMD Ryzen 5 3600 under Windows with Go 1.27.3, with 30 seconds
between runs. One engine ran at a time, on every CPU, with the order
rotated each round. Each runner was built with DYNAMICBASE cleared, so
that Windows loads it at its image base, and each process ran from a
fresh copy of it, as under [Compared with C
QuickJS](#compared-with-c-quickjs). JIT off was built without the
`quickjs_jit` tag. JIT on compiles
synchronously; JIT background enables the JIT and compiles in the background
with `-jit -jitbackground`. The runners keep these meanings now that `WithJIT`
and `qjs --jit` compile in the background by default where GOMAXPROCS is more
than 1: `-jit` alone compiles on the spot (`WithJITBackground(false)`), and
`-jitbackground` in the background whatever GOMAXPROCS is. Both use the same binary built with
`-tags quickjs_jit` and the runner's 32 MiB JIT budget rather than the
runtime's default, 64 MiB. This revision uses the register interpreter and
register JIT by default.
Higher suite scores are better; the relative columns divide each
go-quickjs score by goja's.

| Workload | go-quickjs JIT off | go-quickjs JIT on | go-quickjs JIT background | goja | JIT off / goja | JIT on / goja | JIT background / goja |
|---|---:|---:|---:|---:|---:|---:|---:|
| Richards | 1,432 | 5,772 | 5,730 | 177 | 8.09x | 32.61x | 32.37x |
| DeltaBlue | 1,514 | 6,329 | 6,276 | 222 | 6.82x | 28.51x | 28.27x |
| Crypto | 2,355 | 8,360 | 8,407 | 114 | 20.66x | 73.33x | 73.75x |
| RayTrace | 3,956 | 12,432 | 12,789 | 266 | 14.87x | 46.74x | 48.08x |
| EarleyBoyer | 4,442 | 9,098 | 9,081 | 468 | 9.49x | 19.44x | 19.40x |
| RegExp | 2,818 | 4,027 | 4,087 | 223 | 12.64x | 18.06x | 18.33x |
| Splay | 6,651 | 9,477 | 9,225 | 931 | 7.14x | 10.18x | 9.91x |
| NavierStokes | 4,908 | 16,621 | 16,769 | 183 | 26.82x | 90.83x | 91.63x |
| **Composite score** | **3,070** | **8,248** | **8,259** | **260** | **11.81x** | **31.72x** | **31.77x** |

The runner's timed suite interval includes runtime construction, parsing,
compilation, the suite's warmups and its measured iterations, but excludes
reading the source files and the final forced garbage collection. CPU time
includes work on other threads, including the collector. These are the
go-quickjs and goja runs:

| Metric | go-quickjs JIT off | go-quickjs JIT on | go-quickjs JIT background | goja | JIT off relative to goja |
|---|---:|---:|---:|---:|---:|
| Suite wall time | 23.47 s | 20.26 s | 20.23 s | 119.25 s | 5.08x faster |
| Suite CPU time | 28.04 s | 28.04 s | 28.45 s | 159.66 s | 82.4% less |
| Total allocation | 3.70 GiB | 6.93 GiB | 7.08 GiB | 26.40 GiB | 86.0% less |

Score mode does different amounts of work as throughput changes, so the CPU
and allocation totals are not equal-work efficiency measurements. The
composite is the median of the three printed composite scores, not a
composite calculated from the workload medians.

The suite warms every workload for at least one second, then measures for at
least another second and continues until it has 32 measured iterations. All
engines evaluated the same benchmark sources and validation code.
The measured revisions were go-quickjs `f596215b`, goja `a177d211bf60`, and
AreWeFastYet `0e21608`. The isolated goja runner pins
`v0.0.0-20261102223426-a177d211bf60` for this comparison. These results
characterize this older workload suite rather than every application;
go-quickjs leads all eight of its workloads in this comparison.

The comparison used `internal/cmd/v8bench/matrix`, selecting `gq`,
`gqjit`, `gqjitbg` and `goja` in separate invocations with `-suites score
-runs 1 -parallel=false -v8 SUITE_DIR` and a separate `-out` directory for
each sample, with 30-second pauses. The engine orders were
`gq,gqjit,gqjitbg,goja`, `gqjit,gqjitbg,goja,gq` and
`gqjitbg,goja,gq,gqjit`. The tables are the medians of the saved outputs.
A per-process CPU log ran beside the runs; no other process took more
than a quarter of a CPU during a sample. JIT off's first sample was run
again, 30 seconds after the last, from a runner the same bytes as its
second's and third's: the first's had been built before the checkout's
documentation changed, which its build information records. Each
engine's three binaries were then byte-identical; both JIT modes used
the same binary. The engine source
matched the revision above throughout measurement.

The composite samples were:

| Mode | Run 1 | Run 2 | Run 3 |
|---|---:|---:|---:|
| JIT off | 3,129 | 3,023 | 3,070 |
| JIT on | 8,177 | 8,248 | 8,276 |
| JIT background | 8,259 | 8,323 | 8,238 |
| goja | 260 | 262 | 259 |

All samples were included. The table takes the median of each workload and
of the printed composite scores independently. Background compilation's
composite was 0.13% higher than synchronous compilation's; this run does
not establish an overall throughput advantage for either compilation mode.
The previous table, at `976e5cc1` on the same machine, had composites of
3,109, 7,883 and 7,806 against goja's 260 (11.96x, 30.32x and 30.02x).
EarleyBoyer's scores with the JIT are 9,098 and 9,081 where they were
5,868 and 5,896: native code now reads its native callees' arguments and
takes their cells and closures itself (`a3bc83cd`, `359c571f`).

## Compared with C QuickJS

Over the V8 suite as a whole, go-quickjs is well ahead of QuickJS. Its
interpreter takes 0.31 times QuickJS's time and has 2.27 times its
composite score, ahead on all eight workloads. With its JIT it takes 0.20
times QuickJS's time and scores 6.1 times as much. On the larger programs
of Octane and Are We Fast Yet the interpreter takes 0.43 and 0.64 times
QuickJS's time, and the JIT 0.28 and 0.20 times
([below](#octane-and-are-we-fast-yet)); a page React 18 renders on the
server takes 0.59 times, and 0.48 with the JIT
([below](#server-rendering)). Single operations cost about half as much,
0.49 times QuickJS's time by the geometric mean of its micro-benchmarks,
and 0.24 times with the JIT, as [the micro-benchmarks](#micro-benchmarks)
below show; the slowest of them takes 1.5 times as long.

At `f596215b`, the revision measured here, after v0.45.2, go-quickjs
compiles every function to register code -- three-address instructions
that name the frame's registers as their operands, as V8's Ignition and
Lua's VM do -- and nothing else: the stack bytecode is gone from the front
end too (`d6ac45be`). It runs that code on the register interpreter
(docs/register-interpreter.md): on amd64 and arm64 by native threaded
handlers, machine code that `internal/vm/internal/nativegen` generates,
and elsewhere by a table of Go handlers. A function of a validated asm.js
module -- code compiled from C, as Octane's zlib is -- is also compiled to
typed code, whose registers each hold a raw int32 or float64 or a Value
for the whole function, as the validator proved, and which runs by a typed
table of its own, natively too (register-interpreter.md, "Typed code";
`d8cbc918`). The JIT, which is experimental and optional, compiles a hot
function's register code to native code that runs on the interpreter's own
frame, and an asm.js module's typed code at its link, with no speculation
(see its [design notes](../internal/jit/README.md); `5ae2317e`). The
page's previous figures, at `976e5cc1`, v0.45.1's code, were of the same
design before the changes below, which are the JIT's: in a runtime with
the JIT the interpreter's feedback handlers record what kinds of operand
each arithmetic, comparison and bitwise site met, and the JIT's first
compile speculates on that (`eaf6ea31`, `8d4c550e`); a function
deoptimizes as often as its sites need, each site's ladder bounding it,
where 64 deoptimizations refused it for good (`2a6ccd9a`); an await goes
on in place natively (`f2dac6e0`); code no run uses is parked as the
runtime returns to its host, and the JIT's default budget is 64 MiB
(`407d76d8`, `5f5ce472`); a compile in the background does nearly all its
work on its worker (`98ec0fae`, `2423d93e`), and `WithJIT` compiles in the
background by default where GOMAXPROCS is more than 1 (`73a62aa7`); and
native code reads a native callee's arguments itself, takes the cells of a
function capturing up to sixteen locals and the closures made of them from
pools it fills again without a frame, gives the cells back where no
closure took them, and makes rest parameters' arrays and unmapped
arguments objects (`a3bc83cd`, `359c571f`, `e7d42818`, `691ca9a7`,
`75459d89`). The same suites ran on an AMD Ryzen 5 3600 under Windows,
with Go 1.27.3 and QuickJS 2026-06-04, go-quickjs `f596215b` for all three
columns, in one run of the matrix. Two runs went at once, each on the six
CPUs of a level 3 cache of its own, the JIT's beside the plain build's.
The JIT's column is a build with `-tags quickjs_jit` run with `-jit`,
which compiles on the spot, as `WithJITBackground(false)` does. Each
runner was built with DYNAMICBASE cleared, so that Windows loads it at its
image base, and each process ran from a fresh copy of it (`-fixedbase`,
`-freshcopy`, `a9c5e457`), as for the previous figures (see where a
program is loaded, below). The figures are milliseconds for the same fixed
work, the median of three fresh-process runs (lower is better):

| Workload | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| Richards | 8.5 ms | 10.8 ms | 11.3 ms | 0.75x | 0.96x |
| DeltaBlue | 14.1 ms | 17.1 ms | 24.0 ms | 0.59x | 0.71x |
| Crypto | 155 ms | 56.1 ms | 328 ms | 0.47x | 0.17x |
| RayTrace | 61.6 ms | 49.3 ms | 146 ms | 0.42x | 0.34x |
| EarleyBoyer | 167 ms | 107 ms | 408 ms | 0.41x | 0.26x |
| RegExp | 151 ms | 127 ms | 864 ms | 0.17x | 0.15x |
| Splay | 118 ms | 97.3 ms | 498 ms | 0.24x | 0.20x |
| NavierStokes | 105 ms | 36.1 ms | 197 ms | 0.53x | 0.18x |
| **Total** | **815 ms** | **522 ms** | **2,589 ms** | **0.31x** | **0.20x** |

The suite's own scores, the median of three (higher is better):

| Workload | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| Richards | 1,422 | 5,641 | 938 | 1.52x | 6.01x |
| DeltaBlue | 1,507 | 6,368 | 841 | 1.79x | 7.57x |
| Crypto | 2,352 | 8,243 | 1,106 | 2.13x | 7.45x |
| RayTrace | 3,810 | 12,764 | 1,548 | 2.46x | 8.25x |
| EarleyBoyer | 4,428 | 8,977 | 2,173 | 2.04x | 4.13x |
| RegExp | 2,730 | 4,047 | 433 | 6.30x | 9.35x |
| Splay | 6,511 | 9,244 | 3,357 | 1.94x | 2.75x |
| NavierStokes | 4,878 | 16,522 | 2,407 | 2.03x | 6.86x |
| **Composite** | **3,040** | **8,195** | **1,340** | **2.27x** | **6.12x** |

At `976e5cc1` the interpreter took 0.33 times QuickJS's time over the
suite and had 2.26 times its composite score, and the JIT 0.22 and 5.79
times. The interpreter's total is 815 ms where it was 820, and its
composite level, 3,040 against 3,033; QuickJS's total is 2,589 ms where
it was 2,513, its composite 1,340 as it was, so the lower ratio is
mostly QuickJS's longer time. With the JIT
the total is 522 ms where it was 546, and the composite 5.6% higher,
8,195 against 7,758, nearly all of it EarleyBoyer's: it scores 8,977
where it scored 5,811, and its fixed run takes 107 ms where it took 115.
Its list functions read their arguments in a loop as native callees,
each read a call of Go that had to find the callee's frame, and a
function capturing ten of its locals took its cells on a frame at each
call; native code now reads the arguments and takes the cells itself
(`a3bc83cd`, `359c571f`). Richards took 10.8 ms with the JIT where it
took 9.9, its runs 10.4 to 11.7 ms, and DeltaBlue 17.1 where it took
16.2: a fixed run of either lasts a few milliseconds, much of it before
the JIT's code takes over, and falls in one of two groups in both
builds; over four placements of each build their warm runs are level,
1.8% and 1.0% longer, and in six more fresh runs of each build Richards
took 8.5 to 11.2 ms at `976e5cc1` and 8.5 to 10.5 here.

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
in `913095f4`'s session, took 0.51 and 0.23 times QuickJS's time.

The two kinds of figure time different things, which is why they do not
agree on how far ahead go-quickjs is:

- A fixed run times each workload's setup, its runs and its teardown. The
  scored run times only the runs, after a second of warming up.
- The total adds milliseconds, so the longest workloads, RegExp and Splay,
  make much of the lead. The composite score is the geometric mean of the
  workloads' scores, so each counts the same: Richards weighs as much as
  RegExp. By that mean the interpreter takes 0.41 times QuickJS's time,
  and the JIT 0.29 times, where their scores are 2.27 and 6.12 times as
  high.
- The JIT's lead is much larger in the scores than in fixed time. A fixed
  run of Richards or DeltaBlue lasts a few milliseconds, and a function
  runs in the interpreter until it is hot enough to compile, so much of
  that run is spent before the JIT's code takes over: with the JIT,
  Richards takes 1.27 times the interpreter's fixed time and DeltaBlue
  1.21 (Richards' three runs with it took 10.4, 10.8 and 11.7 ms). The
  scores time warm runs, where Richards, DeltaBlue, Crypto, RayTrace and
  NavierStokes score 3.4 to 4.2 times what the interpreter does.
- Splay's lead is largest in its fixed time. That is almost all setup and
  teardown: building a tree of 8,000 nodes and dropping it, which QuickJS
  frees by counting references where Go's collector frees it later. A warm
  run, which is what the score times, is splay-tree operations, calls and
  property reads, and a new node for each insertion. Each node's payload
  holds array literals of constants, which share their elements with the
  literal until one is written, as V8's copy-on-write arrays do
  (`a2d9f12e`); its score is 1.94 times QuickJS's.
- The JIT changes least where the time is spent in Go code it calls
  rather than in the script's own: the allocation-heavy Splay, 1.42 times
  the interpreter's score, and RegExp, 1.48 times, though a runtime with
  the JIT compiles its patterns to machine code as V8's Irregexp does
  (`2be0068`). EarleyBoyer, which allocates as heavily, was 1.31 times
  the interpreter's score at `976e5cc1` and is 2.03 times, now that the
  closures, cells and argument reads of its native callees are native
  code's own.

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
three fresh-process runs (lower is better), in the same run of the
matrix, during which no other process took more than a sixth of a CPU.

| Octane | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| Richards | 3.0 ms | 5.4 ms | 3.9 ms | 0.77x | 1.38x |
| DeltaBlue | 5.2 ms | 8.6 ms | 7.8 ms | 0.67x | 1.10x |
| Crypto | 52.6 ms | 23.9 ms | 109 ms | 0.48x | 0.22x |
| RayTrace | 21.0 ms | 34.0 ms | 50.1 ms | 0.42x | 0.68x |
| EarleyBoyer | 66.8 ms | 55.5 ms | 137 ms | 0.49x | 0.41x |
| RegExp | 119 ms | 111 ms | 460 ms | 0.26x | 0.24x |
| Splay | 112 ms | 90.5 ms | 463 ms | 0.24x | 0.20x |
| NavierStokes | 41.7 ms | 18.1 ms | 68.1 ms | 0.61x | 0.27x |
| PdfJS | 202 ms | 237 ms | 372 ms | 0.54x | 0.64x |
| Mandreel | 2,473 ms | 771 ms | 3,358 ms | 0.74x | 0.23x |
| Gameboy | 408 ms | 254 ms | 460 ms | 0.89x | 0.55x |
| CodeLoad | 12.8 ms | 13.2 ms | 12.8 ms | 1.00x | 1.03x |
| Box2D | 89.3 ms | 110 ms | 158 ms | 0.57x | 0.70x |
| zlib | 1,603 ms | 590 ms | 8,288 ms | 0.19x | 0.07x |
| Typescript | 1,510 ms | 1,879 ms | 2,141 ms | 0.71x | 0.88x |
| **Total** | **7,104 ms** | **4,584 ms** | **16,619 ms** | **0.43x** | **0.28x** |

| Are We Fast Yet | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| DeltaBlue | 565 ms | 323 ms | 716 ms | 0.79x | 0.45x |
| Richards | 3,219 ms | 687 ms | 4,769 ms | 0.68x | 0.14x |
| Json | 1,002 ms | 262 ms | 1,345 ms | 0.74x | 0.19x |
| CD | 1,597 ms | 472 ms | 2,845 ms | 0.56x | 0.17x |
| Havlak | 3,375 ms | 1,158 ms | 4,288 ms | 0.79x | 0.27x |
| Bounce | 764 ms | 245 ms | 1,494 ms | 0.51x | 0.16x |
| List | 550 ms | 155 ms | 1,237 ms | 0.44x | 0.13x |
| Mandelbrot | 178 ms | 40.8 ms | 697 ms | 0.26x | 0.06x |
| NBody | 665 ms | 123 ms | 1,030 ms | 0.65x | 0.12x |
| Permute | 859 ms | 230 ms | 1,691 ms | 0.51x | 0.14x |
| Queens | 659 ms | 124 ms | 1,054 ms | 0.63x | 0.12x |
| Sieve | 556 ms | 337 ms | 1,214 ms | 0.46x | 0.28x |
| Storage | 1,473 ms | 702 ms | 1,643 ms | 0.90x | 0.43x |
| Towers | 891 ms | 205 ms | 1,562 ms | 0.57x | 0.13x |
| **Total** | **16,340 ms** | **5,077 ms** | **25,608 ms** | **0.64x** | **0.20x** |

- Octane's zlib takes 0.19 times QuickJS's time, 1,603 ms, and 0.07 times
  with the JIT, 590 ms (1,612 and 570 at `976e5cc1`). zlib
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
  `913095f4`'s session.
- Mandreel, compiled from C++ too, is not asm.js and runs as ordinary
  register code: 2,473 ms, 0.74 times QuickJS's time, and 771 with the
  JIT (2,470 and 794 at `976e5cc1`). Its largest function is
  compiled at its first call, within the timed run (`d6ac45be`). Its `| 0`
  arithmetic runs as int32 instructions (`add_i32`, `add_imm_i32`, the
  bitwise operators with an immediate) and its typed arrays' elements are
  read and written by native handlers, but each element read and each `|
  0` still converts through a float: raw registers outside asm.js are
  designed and not built (register-interpreter.md, "Known limits and open
  work").
- Without the JIT, go-quickjs is ahead on every benchmark of both suites
  but Octane's CodeLoad, which is mostly compiling: it takes 1.00 times
  QuickJS's time, 12.8 ms, and 1.03 times with the JIT, where it took
  1.02 and 1.07 at `976e5cc1` and 1.22 and 1.37 at `258b4e96`,
  before the front end compiled to register code alone (`d6ac45be`). On
  Are We Fast Yet the interpreter is 21% ahead on DeltaBlue and on
  Havlak, a third on Richards, 37% on Queens and 43% on Towers. A call's
  window of registers is the callee's frame, so nothing is copied, a frame
  record is written only where a reader needs it, and on amd64 and arm64
  a call, a construction and a return are native handlers that never
  enter Go ([Calls](#where-the-time-goes) below). The programs that
  allocate most are the least ahead: Storage 10%, Havlak 21%. Since
  `976e5cc1` the interpreter's totals are level, Octane's 7,108 to 7,104
  ms and Are We Fast Yet's 16,508 to 16,340. Octane's PdfJS took 202 ms
  where it took 185, its three runs 200 to 212 ms, and EarleyBoyer 66.8
  where it took 63.0, its runs 58.0 to 73.8: over four placements of each
  build their warm runs are level, 0.0% and 0.1% shorter, and in six more
  fresh runs of each build PdfJS took 182 to 192 ms at `976e5cc1` and 179
  to 201 here. Storage took 1,473 ms where it took 1,506.
- Octane's total with the JIT is 0.7% shorter, 4,584 ms where it was
  4,616. EarleyBoyer took 55.5 ms where it took 58.6, its runs 44.9 to
  59.5, and over four placements of each build its warm runs are 8.4%
  shorter: its list functions' argument reads and its entry cells are
  native code's now (`a3bc83cd`, `359c571f`; above). Typescript takes
  1,879 ms where it took 1,895, and Mandreel 771 where it took 794, its
  warm runs 2.7% shorter over the placements. Box2D took 110 ms where it
  took 96.0, its runs 96.7 to 116 ms, RayTrace 34.0 where it took 31.6,
  and DeltaBlue 8.6 where it took 8.0. Over four placements of each build
  their warm runs are level, 1.0% shorter, 1.4% and 2.0% longer, and in
  six more fresh runs of each build Box2D took 93.7 to 101 ms at
  `976e5cc1` and 95.2 to 103 here, RayTrace 31.5 to 32.9 and 31.8 to 36.2;
  DeltaBlue took 7.7 to 8.1 ms and 8.3 to 8.8, slower in every run. A
  fixed run of DeltaBlue lasts eight milliseconds, much of it before the
  JIT's code takes over, and the 0.6 ms is in that run, which the
  placements' warm runs do not show; it is not yet traced to a commit.
  Are We Fast Yet's total with the JIT is level, 5,095 to 5,077 ms; its
  CD took 472 ms where it took 497, and Storage 702 where it took 690.
- With the JIT, Are We Fast Yet takes 0.06 to 0.45 times QuickJS's time,
  its loops of arithmetic the least, its programs that allocate most --
  DeltaBlue, Storage -- the most. Richards' runs with the JIT fall in two
  groups, of about 650 to 690 ms and of 720 to 940: its three in the
  matrix took 676, 687 and 734 ms, and at `976e5cc1` 683, 685 and 723.
- When the stack JIT was removed, its timing had the register JIT slower
  than the stack JIT just before it on Octane's RayTrace (+7.6%) and
  CodeLoad (+25%) and on Are We Fast Yet's Json (+8.2%), Permute (+7.0%)
  and Queens (+6.3%) (docs/performance-plan.md, "The register JIT's
  remaining gaps"). Against the stack JIT's figures at `acffb1aa` (270,
  227 and 119 ms), Json is 3% shorter, 262 ms, Permute 1% longer, 230, and
  Queens 4% longer, 124 ms, its runs 124 to 127; CodeLoad took 14.2 ms
  then and takes 13.2. RayTrace took 34.0 ms, its runs with the JIT 33.7
  to 34.3 ms, where they took 31.3 to 31.9 at `976e5cc1` (above).
- On seven of Octane's fifteen the JIT is behind the interpreter:
  Richards (80% behind), DeltaBlue (65%), RayTrace (62%), Typescript
  (24%), Box2D (23%), PdfJS (17%) and CodeLoad (3%); RegExp, 1% behind at
  `976e5cc1`, is 6% ahead, 111 ms against 119. Octane's fixed work is
  one run of each benchmark, so compiling counts, and the code runs in
  the interpreter until it is hot; a runtime with the JIT keeps the
  frames the plain interpreter's calls and constructions do without. The
  JIT's own figures are close to what they were at `976e5cc1` -- Richards
  5.3 ms then, DeltaBlue 8.0, RayTrace 31.6, PdfJS 244 -- and PdfJS's gap
  is smaller for the interpreter's longer run, above.

### Server rendering

The ssr suite renders a page with React 18's own builds, as a host behind
a web server does for each request: Render is one runtime rendering the
page again and again, FirstRender five fresh runtimes, each loading React
and rendering once. The same matrix, the median of three fresh-process
runs (lower is better):

| ssr | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| Render | 1,090 ms | 884 ms | 1,852 ms | 0.59x | 0.48x |
| FirstRender | 152 ms | 270 ms | | | |

Render took 0.58 and 0.47 times QuickJS's time at `976e5cc1`, 1,088 and
889 ms, and FirstRender 154 and 276 ms. Render took 1,090 ms here, its
three runs 1,088 to 1,124 ms, and 884 with the JIT, its runs 880 to 885;
QuickJS took 1,852 ms where it took 1,881. FirstRender is 152 and 270 ms.
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
FirstRender 197 and 366 ms, in `913095f4`'s session. React writes a page
as thousands of short
chunks appended to a string a closure holds, and an append grows that
string where it is, when nothing else can see it (`00ce03b5`). An empty
literal learns the room its arrays grow to (`c62a9bb0`), and a for-in
takes the last loop's cursor once that loop has finished (`5326a3f8`).

QuickJS's runner loads a script once, so it has no FirstRender. A fresh
runtime's render is where the JIT costs most: these compiles are made on
the runtime's goroutine, which waits for each, as a runtime made
`WithJITBackground(false)` does; `qjs --jit`, as `WithJIT` by default,
compiles in the background where GOMAXPROCS is more than 1. Since
v0.39.0 a compile made in the background is installed once the native
calls of the code it replaces are done, where it was thrown away
(`7812928f`), a native caller's ask installs a callee's finished compile
(`f1a05866`), a compile's snapshot asks nothing of callees no site could
inline (`39bdcec8`), and an asm.js module's link compiles on the spot what
the background has no room for (`5307e6a0`), and a compile in the
background asks ahead all its compile asks, so that it makes the code a
compile on the spot makes, and is run as soon as it is installed, a run of
the code it replaced leaving at its next call of Go (`0cf4b86d`); since
`976e5cc1` such a compile's snapshot is gathered on the runtime's
goroutine and answered, lowered and placed on its worker (`98ec0fae`,
`2423d93e`, `04f96131`). The figures here are of compiles made on the
spot, which none of these changes.

### Micro-benchmarks

QuickJS's own micro-benchmarks, [`tests/microbench.js`](https://github.com/bellard/quickjs/blob/master/tests/microbench.js),
time one operation each, in nanoseconds. Each test was given the work QuickJS
takes about 150 ms for, all three ran that same work in fresh processes,
all three at `f596215b`, and the figure is the fastest of three runs. The
runners' micro mode does this, from a copy of the file kept in `internal/v8bench`, and
the matrix above runs it. `sort_bench` is left out: it reports a run's time
over its fastest sort, not the time of an operation. Over the other 71 tests
go-quickjs takes 0.49 times QuickJS's time by the geometric mean, as it
did at `976e5cc1`, and v0.38.0, the stack design, 0.79 in `913095f4`'s
session: it is slower on 6, faster on 63, and within 5% on the other
two. With the JIT it takes 0.24 times QuickJS's time, as at `976e5cc1`,
slower on 2, faster on 66 and within 5% on 3.
The table has the interpreter's largest gaps either way:

| Test | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| `weak_map_delete` | 376 ns | 436 ns | 252 ns | 1.49x | 1.73x |
| `bigint256_arith` | 180 ns | 127 ns | 126 ns | 1.43x | 1.01x |
| `weak_map_set` | 130 ns | 93.7 ns | 92.6 ns | 1.41x | 1.01x |
| `date_parse` | 964 ns | 980 ns | 823 ns | 1.17x | 1.19x |
| `int_to_string` | 56.8 ns | 42.9 ns | 48.8 ns | 1.16x | 0.88x |
| `func_closure_call` | 35.8 ns | 2.81 ns | 31.7 ns | 1.13x | 0.09x |
| `regexp_utf16` | 253 ns | 255 ns | 255 ns | 0.99x | 1.00x |
| `array_prop_create` | 14.3 ns | 11.7 ns | 14.9 ns | 0.96x | 0.78x |
| `float_toExponential` | 165 ns | 138 ns | 181 ns | 0.92x | 0.77x |
| `float_toFixed` | 145 ns | 122 ns | 162 ns | 0.89x | 0.75x |
| `global_func_call` | 8.15 ns | 2.10 ns | 32.5 ns | 0.25x | 0.06x |
| `prop_update` | 2.86 ns | 1.91 ns | 11.5 ns | 0.25x | 0.17x |
| `array_length_decr` | 12.9 ns | 9.80 ns | 57.9 ns | 0.22x | 0.17x |
| `func_call` | 6.79 ns | 0.96 ns | 31.1 ns | 0.22x | 0.03x |
| `array_for_in` | 15.3 ns | 11.9 ns | 75.5 ns | 0.20x | 0.16x |
| `string_build2c` | 26.5 ns | 25.9 ns | 143 ns | 0.19x | 0.18x |
| `array_hole_length_decr` | 12.4 ns | 9.56 ns | 70.3 ns | 0.18x | 0.14x |
| `array_slice` | 4.28 ns | 4.33 ns | 27.0 ns | 0.16x | 0.16x |
| `array_for_of` | 7.28 ns | 4.94 ns | 46.0 ns | 0.16x | 0.11x |
| `prop_clone` | 10.9 ns | 9.58 ns | 77.2 ns | 0.14x | 0.12x |

Of the gaps the stack design had, the largest are gone: in v0.38.0,
measured in `913095f4`'s session, `array_prop_create` took 2.25 times
QuickJS's time and takes 0.96, `global_read` 1.79 and 0.84, `array_read`
1.41 and 0.87, `empty_do_loop` 1.34 and 0.52.

The JIT does the most for a test that is a loop of arithmetic, locals,
property reads and writes, array elements and calls: `prop_read` takes
0.79 ns with it, `func_call` 0.96 ns, a fifth of the interpreter's time
or less. A test whose time is a built-in function's work runs much as it
does in the interpreter: regular expressions, `array_slice`, and the
conversions between numbers and strings. An object a function makes and
drops need not be made at all, even where it crosses a merge or goes round
a loop, as in `sum = sum.add(v)`: it is made only where it escapes, by
partial escape analysis, as in Graal (`41575a42`). `escape_accumulate`
and `carried_short` take 0.06 and 0.04 times QuickJS's time with the JIT,
and 0.49 and 0.48 without it. Code that keeps its objects gains nothing
from this, and the suites above hardly move for it.

The interpreter has gained more than the JIT, so some tests run slower
with the JIT than without it. In the run made for this page, 4 of these
71 took 7% or more longer with the JIT, three of the four at `976e5cc1`:
`global_destruct_strict`, `weak_map_delete` and `string_build_large1`,
no longer `global_destruct`. Of the extra tests below that QuickJS runs,
20 did, where 16 had at `976e5cc1`: three of the async tests and
`generator_delegate`, the two tests of an arguments object read and made
(`args_read`, `arguments_object`, new since), a spread call, an object
spread and an object rest, a sort of 64-bit elements, a case-insensitive
search, `Object.setPrototypeOf` to a fresh object, a class and a subclass
made per call, a construction that reads `new.target`, and five JSON
tests: of duplicate keys, new layouts, dates, a replacer's list of keys
and a prototype changed between calls; no longer `async_await`,
`array_map_tail_args`, the reads of the 64-bit typed arrays, a global
`match`, nor JSON's getters.

Since `976e5cc1` the gains are the JIT's. An await the interpreter goes
on with in place -- of a value or a fulfilled promise, no job queued --
native code goes on with in place too, where each had been an exit to Go
(`f2dac6e0`): with the JIT `async_await` takes 4.63 ns where it took
17.7, and `async_busy` 4.41 where it took 5.95. A native callee reads its
arguments itself (`a3bc83cd`): with the JIT `array_map_tail_args` takes
246 ns where it took 320, 22% less over two placements of each build,
from that commit on. Native code takes a function's cells and the
closures made of them from pools it fills again without a frame, of up
to sixteen captured locals, and a pool's closures are made without
looking up what they capture (`359c571f`, `691ca9a7`): `closure_make`
takes 30.2 ns with the JIT where it took 66.0, and `closure_many_locals`
34.3 where it took 75.6. Of the tests new since, `closure_cells_ten`
takes 42.9 ns with the JIT against the interpreter's 149, `rest_param`
98.2 against 177 and `closure_kept_cells` 260 against 473. Two run slower
with the JIT: `arguments_object` 277 ns against 211, and `args_read` 532
against 196, whose loop the JIT judges slow at its property read and
leaves to the interpreter, which then runs with the JIT's feedback
handlers. With the JIT `string_concat` takes 98.5 ns where it took 119
and `string_indexOf` 67.0 where it took 82.8, though over two placements
of each build both are level or slower, so most of it is where the code
lies. Over the 418 tests both builds ran, by the geometric mean, the
interpreter's figures are 0.999 times theirs at `976e5cc1`, and the
JIT's 0.993 times.

Some of these tests depend on where the linker places the code more than
on the code. The layout lock keeps the interpreter's hot code where it
measured best, but the code between its anchors, the Go handlers after the
loop and the closures still move with every change to the package, and a
change of a few per cent in one of these tests is not by itself a change in
the engine. A pad that grows past a page moves everything after it 4 KB,
every offset modulo 4 KB the same (docs/optimizing.md, "The layout
lock"). Each round's changes are followed by the lock's pads written
anew, as this one's were, each commit of its last stage with its own
(`73308ddf`, `74b4bfe6`, `c96ce1c1`).

Each row slower than at `976e5cc1` by 7% or more in the matrix's run was
timed again over four placements of each build, loaded at a fixed base,
and those slower at every placement were timed at commits between the
two, two placements of each. With the JIT `typed_i64_sort` takes 424 ns
where it took 371, 5.6% longer over the placements, at all four: it is
level through `9d14b87b`, and 7% longer from `8d4c550e` on, so it is the
operand lattice's cost (`eaf6ea31`, `8d4c550e`), most likely at the sort's
comparator, whose `<` and `>` compare BigInts: the first compile now
builds such a site for what the lattice met, where it was compiled for
numbers and learned at a guard's failure; the two commits were not timed
apart. With the JIT `string_repeat` takes 65.6 ns where it took 59.6,
13% longer at all four, but over two placements of each commit between it
moved from 10% shorter to 13.5% longer and back between commits that do
not touch its path -- 13.5% longer at `8d4c550e`, 1.3% shorter at
`74b4bfe6`, 2.8% shorter at `75459d89` and 8.3% longer at `c96ce1c1` --
so most likely where the linker put the code. The rest of the rows 7% or
more slower in the matrix -- `for_in_break`, `global_destruct_strict` and
`dict_for_in` in the interpreter and Octane's PdfJS (above), and with the
JIT `array_reduce_upvalue`, `typed_u64_reduce`, `bigint1024_arith`,
`object_spread_large`, `object_spread_plain`, `string_pad_start`,
`string_template`, `subclass_new`, `forward_array`, `construct_new_target`,
`carried_exit_7`, `generator_next`, `upvalue_two_appends` and
`json_parse_duplicates`, the V8 suite's Richards and Octane's DeltaBlue,
RayTrace and Box2D (above) -- are within 4.7% over the placements and
none slower at every placement, but for `array_reduce_upvalue` with the
JIT, 7.2% longer, at three of the four.

The rows `976e5cc1`'s page left slower or open, with its figures and
`0c0bac44`'s in brackets:

- with the JIT `class_factory` takes 1,283 ns (1,285, 1,135),
  `array_push_args5` 41.8 (40.0, 36.6), `array_push_args5_single` 53.7
  (54.3, 50.1) and `json_parse_duplicates` 529 (487, 454), the last level
  over the placements (above);
- `weak_map_set` takes 130 ns (144, 127), `dataview_read` 65.7 (63.8,
  57.0) and `dataview_write` 66.5 (67.2, 62.4), and with the JIT
  `object_spread_large` 218 (195, 173), level over the placements
  (above);
- `async_chain` takes 1,463 ns (1,522, 1,597) and `async_generator` 699
  (718, 752), and with the JIT `async_chain` 1,633 (1,571, 1,696);
- `int_to_string` takes 56.8 ns (57.7, 57.7) and `bigint256_arith` 180
  (177, 177); with the JIT `append_pair_read` 32.4 (33.2, 32.2) and
  `html_escape_plain` 109 (107, 112);
- with the JIT `string_indexOf` and `string_concat` take 67.0 and 98.5 ns
  (82.8 and 119, 80.2 and 114), above, `array_some` 9.14 (9.64, 9.53) and
  `has_own_call_for_in` 59.5 (59.3, 61.5); `string_trim` 60.5 (56.6,
  54.5), and 74.6 without the JIT (75.0, 75.6);
- `json_stringify_decimals` takes 34.7 ns (35.2, 34.9),
  `json_stringify_ints` 25.1 (25.3, 25.1), and with the JIT
  `json_stringify_unique` 116 (124, 125);
- `string_indexOf` takes 92.8 ns (88.4, 88.4), `forward_construct` 71.8
  (73.7, 80.0), `array_indexOf` 105 (109, 115), `json_stringify_strings`
  30.5 (29.8, 30.5), and with the JIT `array_indexOf` 98.4 (95.6, 95.2);
- `json_stringify_dict` takes 80.2 ns (79.8, 79.1) and
  `string_starts_with` 67.9 (68.3, 68.9); with the JIT `string_template`
  170 (155, 157), level over the placements (above),
  `string_code_point_at` 5.22 (5.32, 5.21), `has_own_call_miss` 54.0
  (52.7, 52.3) and `string_from_code_point` 73.2 (79.2, 84.5);
- `generator_next` takes 101 ns (102, 102), and 62.6 with the JIT (57.8,
  59.5), level over the placements (above), and `array_literal_empty`
  71.4 (70.0, 70.2);
- Are We Fast Yet's Storage takes 1,473 ms (1,506, 1,477), and 702 with
  the JIT (690, 697).

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
additions at sites that meet a new layout at every call -- and since
v0.45.1 a callee's arguments read in a loop and an arguments object made,
a rest parameter's array, a function capturing ten of its locals, and
closures that keep their cells. Over the 351 QuickJS can run (it has no
using declarations) go-quickjs takes 0.50 times its time, and 0.22 times
with the JIT (0.503 and 0.216); over the 346 of `976e5cc1`'s page 0.499
and 0.212, where it took 0.501 and 0.214 then, and v0.38.0 0.70 and 0.30
over its 313; leaving out the JSON tests, 0.55 and 0.20. The asm.js
kernels' loops (`asm_adler`, `asm_crc32`, `asm_copy`, `asm_fnv`,
`asm_dot`, `asm_switch`) take 0.12 to 0.28 times QuickJS's time as typed
code, and 0.03 to 0.07 with the JIT, which compiles them at the module's
link; their recursion and calls (`asm_fib`, `asm_call_leaf`) 0.81 and
0.54, 0.23 and 0.09 with the JIT, which inlines a typed callee
(`4ae95d8b`) and calls typed code natively from the ordinary code that
drives it (`d14b7cea`), and a call through a function table 1.03, 0.16
with the JIT. A switch on strings past its first few cases
takes 0.05 to 0.16 times QuickJS's time, its literal cases one lookup
(`100eb7f`). An await costs a fiftieth of QuickJS's time (`async_await`
15.4 against 752 ns), and a hundred-and-sixtieth with the JIT (4.63 ns):
an async function resumed by its await's job goes on in place at an
await of a value that needs no job of its own, and so does native code
since `f2dac6e0`. A generator resumed from for-of is level
(`generator_for_of` 1.02), where v0.38.0 took 1.44 times, and a for-in
over a dictionary takes 0.23 times,
where it took 1.41: its keys are made once for the dictionary's layout and
handed out by the native handlers. Calls in tail position take 0.55 to
0.59 times QuickJS's time, and a `map` callback that returns a call in
tail position 0.45, about level with one that returns an ordinary call,
0.44: such a
callback is entered directly, as any other is (`400fc19c`). A call of
`charCodeAt` whose method is read before its argument, as in
`s.charCodeAt(++i)`, is answered by the call handler from its target, as
V8's call feedback does (`b90b70c4`): `string_charcode_update` takes 0.35
times QuickJS's time. A site that calls fresh closures of two templates in
turn misses its cache at each call and retries the leaves the native call
handler answers (`ab16fd73`), and since v0.43.0 a closure's capture list
is filled directly (`648d7d30`); `closure_leaf_miss` takes 0.96 times,
where it took 1.14 in v0.42.0. A global `exec` still takes 1.35 times, as
it makes a result and a String for each match, and a sticky one 1.04;
their times and QuickJS's are within 2% of those at `976e5cc1`. The largest
gaps either way:

| Test | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| `scope_chain` | 657 ns | 683 ns | 367 ns | 1.79x | 1.86x |
| `array_literal_empty` | 71.4 ns | 63.6 ns | 40.0 ns | 1.79x | 1.59x |
| `arguments_object` | 211 ns | 277 ns | 122 ns | 1.72x | 2.27x |
| `set_proto_fresh` | 361 ns | 393 ns | 210 ns | 1.72x | 1.87x |
| `subclass_fresh` | 1,137 ns | 1,272 ns | 685 ns | 1.66x | 1.86x |
| `typed_u8_find` | 44.7 ns | 7.22 ns | 27.1 ns | 1.65x | 0.27x |
| `array_fill_down` | 138 ns | 145 ns | 85.2 ns | 1.62x | 1.71x |
| `typed_findIndex` | 46.9 ns | 7.12 ns | 30.9 ns | 1.52x | 0.23x |
| `typed_u64_reduce` | 115 ns | 118 ns | 76.1 ns | 1.52x | 1.55x |
| `typed_find` | 46.8 ns | 7.59 ns | 30.9 ns | 1.51x | 0.25x |
| `asm_fnv` | 6.69 ns | 1.49 ns | 54.3 ns | 0.12x | 0.03x |
| `string_switch_last` | 27.6 ns | 13.3 ns | 263 ns | 0.11x | 0.05x |
| `json_stringify_decimals` | 34.7 ns | 35.8 ns | 332 ns | 0.10x | 0.11x |
| `string_switch_large` | 29.1 ns | 21.9 ns | 404 ns | 0.07x | 0.05x |
| `string_switch_miss` | 17.7 ns | 13.0 ns | 267 ns | 0.07x | 0.05x |
| `regexp_search_utf16` | 693 ns | 706 ns | 13,132 ns | 0.05x | 0.05x |
| `string_switch_nonstring` | 12.3 ns | 8.15 ns | 264 ns | 0.05x | 0.03x |
| `regexp_search_i` | 677 ns | 727 ns | 20,922 ns | 0.03x | 0.03x |
| `json_parse_long` | 1,844 ns | 1,852 ns | 84,568 ns | 0.02x | 0.02x |
| `async_await` | 15.4 ns | 4.63 ns | 752 ns | 0.02x | 0.01x |

A BigInt in the int64 range is held in the value itself (`9746906`), as
QuickJS holds one that fits in a word, and so is one below 2^64 in
magnitude (`f8c3229`); a larger one is a block of no pointers, made by the
engine's own arithmetic (`f1bf681`). Short BigInts' `+`, `-`, `*` and
comparisons are native handlers: `bigint64_arith` takes 0.37 times
QuickJS's time. A larger one is made by the engine's arithmetic in Go:
`bigint256_arith` takes 1.43 times, and the extra BigInt tests 0.42 to
1.21 times, `bigint_toString`, `bigint_pidigits`, `bigint_modpow` and
`bigint64_xorshift` the slowest. The JIT makes BigInts of 64 bits natively: `bigint64_arith`
takes 0.10 times QuickJS's time with it, `bigint64_xorshift` 0.05.

The largest gaps now are:

- objects made on a prototype that is itself new -- `Object.create` of a
  fresh object, `Object.setPrototypeOf` to one, a scope chain built as
  objects, a subclass made per call and its instance -- 1.50 to 1.79
  times QuickJS's time;
- an arguments object made and returned at each call
  (`arguments_object`), 1.72 times, and 2.27 with the JIT, though native
  code makes it since `75459d89`;
- arrays made empty or grown downward from past their end
  (`array_literal_empty`, `array_fill_down`);
- the typed arrays' `find` and `findIndex` in the interpreter, and the
  64-bit typed arrays' reads and callbacks, whose elements are BigInts;
- the weak collections. A WeakMap holds each value on its key rather than
  in a table of its own (`2bc1620`), so that a value which refers to its
  key does not keep the key alive, as the specification requires; adding
  an entry costs more for it;
- a global `exec`, which makes a result and a String for each match, and
  a `replace` by a template with `$` (1.35 and 1.34 times).

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
  0.22 to 0.68 times QuickJS's time, 0.43 by their geometric mean, where
  v0.38.0, its stack interpreter and its tree tier, took 0.45 to 1.15 and
  0.75 by the mean. `int_arith` takes 0.48 times QuickJS's time and
  `float_arith` 0.36.
- **Calls.** A call's window of registers is the callee's frame: the
  caller writes the callee, the receiver and the arguments there, and
  nothing is copied. A callee the call site's cache knows is entered with
  no question asked again, and only the fields of the frame record a
  reader needs are written. Natively, a call, a construction and a return
  are handlers of their own that keep the caller's state on a native
  frame stack and never enter Go (register-interpreter.md, "Frames and
  calls"). A method call whose body needs a frame takes 33.7 ns against
  68.4 in QuickJS (`method_call`), one that only returns its argument
  12.4 against 42.5 (`method_call_leaf`), an empty function 6.79 against
  31.1 (`func_call`); with the JIT they take 11.2, 2.67 and 0.96 ns. A
  recursive call is level: `call_fib` and `call_mutual` take 0.93 times
  QuickJS's time. A body that only returns a parameter or a
  constant, or a short pure one of numbers and upvalues, is answered at
  the call site without a frame; natively, so are a getter of `this.k`,
  `this.k.length` or `this.k[p]`, a `typeof` test, an initializer that
  stores its parameters in `this`, and a constructor that only forwards
  to one (`d54099e3`, `09409332`, `bdab5ba4`); the initializer writes its
  values at the slots, and gives the final layout, that it remembers from
  the last construction of the same layout (`8505335a`): `class_new`
  takes 0.41 times QuickJS's time. `Object.create` of a prototype alone
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
  takes 0.49 times QuickJS's time, `prop_write` 0.35, `global_read` 0.84,
  `global_write` 0.39, `instanceof_class` 0.25, and `mega_read` and
  `mega_add`, at sites that meet a new layout at every call, 0.75 and
  0.70. With the JIT, a read whose cache has met a fifth shape is compiled
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
  rather than making a rope (`00ce03b5`): `string_build2c` takes 0.19 times
  QuickJS's time, `local_append_keep` 0.31 and `template_list` 0.35, and
  0.18 times with the JIT on the first of these, where it took 0.29 in
  v0.42.0.
- **JSON.** The 49 `json_` tests take 0.30 times QuickJS's time, and 0.30
  with the JIT, as the work is a built-in's. `JSON.parse` walks the shape
  tree as it reads, expecting the keys of the last object parsed in the same
  place, and makes each object once, at its last key. `JSON.stringify` keeps
  a layout's keys and slots from the second time it meets it (`097d71ec`).
  Numbers are read and written without strconv's general path (`baafd8e3`),
  and long strings are scanned with AVX2 or NEON where the machine has them:
  `json_parse_records` takes 0.31 times QuickJS's time,
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
  at `913095f4`, 0.32 at `2f05d3fc`, 0.25 at `c2dae008`, 0.24 at
  `b8c2c45f` and `b5df63ea`, 0.20 at `7f8205ec`, 0.19 at `2c758a33`, 0.21
  at `0c0bac44` and 0.20 at `976e5cc1`, level over four placements of each
  build since.

What remains in the interpreter is mostly in Go: the operations with no
native handler and the fast paths that miss, which exit to their Go
handlers, the built-ins and the object model -- the gaps above are in
them -- and the collector. The JIT is the way past the rest: a hot
function compiled to native code dispatches nothing, which is why, with
it, the loops and calls above take from a third of the interpreter's
time down to a tenth.

[goja]: https://github.com/dop251/goja
[v8-v7]: https://github.com/mozilla/arewefastyet/tree/master/benchmarks/v8-v7
