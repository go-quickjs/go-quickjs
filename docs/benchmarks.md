# Benchmarks

Every figure below was measured at `976e5cc1`, the code of v0.45.1, on an
AMD Ryzen 5 3600 under Windows with Go 1.27.3, on 2026-11-25: the bundle
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
go-quickjs `976e5cc1` gives these per-metric medians, measured on
2026-11-25. Compiler figures use five fresh processes, each running both
bundles with 500 ms per benchmark. Load figures use twenty fresh processes
per bundle, one load per process. One process runs at a time, with
30-second pauses between the five compiler rounds and between the twenty
load rounds. Each load round runs both bundles in separate processes,
alternating their order. No builds or tests ran beside them; a per-process
CPU log ran throughout; another process took more than half a CPU in the
ten seconds around the first compiler round and around the first load
round, and the five compiler rounds and the first load round were run
again. All other samples are included, including the first. The previous
figures, at `0c0bac44` on the same machine (on main as `77a851ee`, the
same production code but for a test's switch, `bgDone`, off outside
tests, and the layout lock's pads it moved), were 20.58 and 54.20 ms to
compile, 63.00 and 406.56 ms to load first, 5.03 and 13.27 compiler MB/op,
19.24 and 86.31 load MB/op and 36.59 and 103.88 MiB at the peak; the
compiler test binary is the same bytes at both revisions.

| Bundle | Compile | First load |
|---|---:|---:|
| Three.js | 20.60 ms | 63.65 ms |
| Babel | 54.19 ms | 402.64 ms |

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
| Three.js | 5.04 | 19.24 | 36.91 |
| Babel | 13.27 | 86.31 | 103.76 |

The `qjs` binary built without the JIT, using `CGO_ENABLED=0 go build
-pgo=off -trimpath ./cmd/qjs`, is 44,927,488 bytes for windows/amd64. The
load test binary is 43,833,344 bytes, and the compiler test binary is
10,224,640 bytes. The
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
[goja]. All three go-quickjs columns use revision `976e5cc1`, measured alongside
goja on 2026-11-25. Each column is the median of three fresh-process runs
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
runtime's default, then 8 MiB. This revision uses the register interpreter and
register JIT by default.
Higher suite scores are better; the relative columns divide each
go-quickjs score by goja's.

| Workload | go-quickjs JIT off | go-quickjs JIT on | go-quickjs JIT background | goja | JIT off / goja | JIT on / goja | JIT background / goja |
|---|---:|---:|---:|---:|---:|---:|---:|
| Richards | 1,407 | 5,711 | 5,754 | 178 | 7.90x | 32.08x | 32.33x |
| DeltaBlue | 1,520 | 6,261 | 6,328 | 222 | 6.85x | 28.20x | 28.50x |
| Crypto | 2,388 | 8,322 | 8,379 | 113 | 21.13x | 73.65x | 74.15x |
| RayTrace | 3,872 | 12,517 | 12,260 | 267 | 14.50x | 46.88x | 45.92x |
| EarleyBoyer | 4,487 | 5,868 | 5,896 | 470 | 9.55x | 12.49x | 12.54x |
| RegExp | 2,898 | 3,996 | 4,000 | 224 | 12.94x | 17.84x | 17.86x |
| Splay | 6,773 | 9,950 | 9,934 | 917 | 7.39x | 10.85x | 10.83x |
| NavierStokes | 4,956 | 17,078 | 16,998 | 184 | 26.93x | 92.82x | 92.38x |
| **Composite score** | **3,109** | **7,883** | **7,806** | **260** | **11.96x** | **30.32x** | **30.02x** |

The runner's timed suite interval includes runtime construction, parsing,
compilation, the suite's warmups and its measured iterations, but excludes
reading the source files and the final forced garbage collection. CPU time
includes work on other threads, including the collector. These are the
go-quickjs and goja runs:

| Metric | go-quickjs JIT off | go-quickjs JIT on | go-quickjs JIT background | goja | JIT off relative to goja |
|---|---:|---:|---:|---:|---:|
| Suite wall time | 22.42 s | 20.25 s | 20.25 s | 119.34 s | 5.32x faster |
| Suite CPU time | 27.10 s | 27.23 s | 27.38 s | 160.01 s | 83.1% less |
| Total allocation | 3.72 GiB | 6.30 GiB | 6.23 GiB | 26.41 GiB | 85.9% less |

Score mode does different amounts of work as throughput changes, so the CPU
and allocation totals are not equal-work efficiency measurements. The
composite is the median of the three printed composite scores, not a
composite calculated from the workload medians.

The suite warms every workload for at least one second, then measures for at
least another second and continues until it has 32 measured iterations. All
engines evaluated the same benchmark sources and validation code.
The measured revisions were go-quickjs `976e5cc1`, goja `a177d211bf60`, and
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
A per-process CPU log ran beside the runs, and a sample during which
another process took about half a CPU or more was run again, 30 seconds
after the last: goja's second. Each engine's three binaries were
byte-identical; both JIT modes used the same binary. The engine source
matched the revision above throughout measurement.

The composite samples were:

| Mode | Run 1 | Run 2 | Run 3 |
|---|---:|---:|---:|
| JIT off | 3,086 | 3,114 | 3,109 |
| JIT on | 7,883 | 7,900 | 7,837 |
| JIT background | 7,787 | 7,935 | 7,806 |
| goja | 260 | 261 | 258 |

All samples were included. The table takes the median of each workload and
of the printed composite scores independently. Background compilation's
composite was 0.98% lower than synchronous compilation's; this run does not
establish an overall throughput advantage for either compilation mode. The
previous table, at `0c0bac44` on the same machine, had composites of 3,037,
7,895 and 7,855 against goja's 258 (11.77x, 30.60x and 30.45x).

## Compared with C QuickJS

Over the V8 suite as a whole, go-quickjs is well ahead of QuickJS. Its
interpreter takes 0.33 times QuickJS's time and has 2.26 times its
composite score, ahead on all eight workloads. With its JIT it takes 0.22
times QuickJS's time and scores 5.8 times as much. On the larger programs
of Octane and Are We Fast Yet the interpreter takes 0.43 and 0.64 times
QuickJS's time, and the JIT 0.28 and 0.20 times
([below](#octane-and-are-we-fast-yet)); a page React 18 renders on the
server takes 0.58 times, and 0.47 with the JIT
([below](#server-rendering)). Single operations cost about half as much,
0.49 times QuickJS's time by the geometric mean of its micro-benchmarks,
and 0.24 times with the JIT, as [the micro-benchmarks](#micro-benchmarks)
below show; the slowest of them takes 1.5 times as long.

At `976e5cc1`, the revision measured here, whose code v0.45.1 is,
go-quickjs compiles every function to register code -- three-address
instructions that name the frame's registers as their operands, as V8's
Ignition and Lua's VM do -- and nothing else: the stack bytecode is gone
from the front end too (`d6ac45be`). It runs that code on the register
interpreter (docs/register-interpreter.md): on amd64 and arm64 by native
threaded handlers, machine code that `internal/vm/internal/nativegen`
generates, and elsewhere by a table of Go handlers. A function of a
validated asm.js module -- code compiled from C, as Octane's zlib is -- is
also compiled to typed code, whose registers each hold a raw int32 or
float64 or a Value for the whole function, as the validator proved, and
which runs by a typed table of its own, natively too
(register-interpreter.md, "Typed code"; `d8cbc918`). The JIT, which is
experimental and optional, compiles a hot function's register code to
native code that runs on the interpreter's own frame, and an asm.js
module's typed code at its link, with no speculation (see its [design
notes](../internal/jit/README.md); `5ae2317e`). The page's previous
figures, at `0c0bac44`, on main as `77a851ee`, v0.45.0's code, the same
but for a test's switch, were of the same design
before the changes below, which are mostly the JIT's: what its code had
left to Go at an exit it now does natively or by a call of Go it goes on
after -- a megamorphic method read's miss, a construction past the
functions it constructs natively, an accessor it does not inline, a throw
from a call of Go, a literal's field -- a primitive's method inherited
from `Object.prototype` read as a case at its site, a typed array's
methods noted by the interpreter for the JIT's code, a closure the JIT's
code made planned at its first use as a callback, and the barrier's
routine shading a pointer stored over itself; and in the interpreter, a
reaction's job that no longer moves its reaction to the heap. The same
suites ran on an AMD Ryzen 5 3600 under Windows, with Go 1.27.3 and
QuickJS 2026-06-04, go-quickjs `976e5cc1` for all three columns, in one
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
| Richards | 8.2 ms | 9.9 ms | 11.3 ms | 0.73x | 0.88x |
| DeltaBlue | 15.5 ms | 16.2 ms | 24.0 ms | 0.65x | 0.67x |
| Crypto | 160 ms | 57.1 ms | 331 ms | 0.48x | 0.17x |
| RayTrace | 61.8 ms | 50.1 ms | 141 ms | 0.44x | 0.36x |
| EarleyBoyer | 170 ms | 115 ms | 403 ms | 0.42x | 0.29x |
| RegExp | 150 ms | 130 ms | 857 ms | 0.17x | 0.15x |
| Splay | 122 ms | 96.9 ms | 497 ms | 0.25x | 0.20x |
| NavierStokes | 102 ms | 36.0 ms | 189 ms | 0.54x | 0.19x |
| **Total** | **820 ms** | **546 ms** | **2,513 ms** | **0.33x** | **0.22x** |

The suite's own scores, the median of three (higher is better):

| Workload | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| Richards | 1,395 | 5,674 | 948 | 1.47x | 5.99x |
| DeltaBlue | 1,494 | 6,315 | 839 | 1.78x | 7.53x |
| Crypto | 2,337 | 8,226 | 1,110 | 2.11x | 7.41x |
| RayTrace | 3,795 | 12,837 | 1,561 | 2.43x | 8.22x |
| EarleyBoyer | 4,420 | 5,811 | 2,157 | 2.05x | 2.69x |
| RegExp | 2,756 | 4,031 | 435 | 6.34x | 9.27x |
| Splay | 6,601 | 9,397 | 3,322 | 1.99x | 2.83x |
| NavierStokes | 4,937 | 16,407 | 2,400 | 2.06x | 6.84x |
| **Composite** | **3,033** | **7,758** | **1,340** | **2.26x** | **5.79x** |

At `0c0bac44` the interpreter took 0.32 times QuickJS's time over the
suite and had 2.26 times its composite score, and the JIT 0.21 and 5.82
times. The interpreter's total is 820 ms where it was 812, and its
composite level, 3,033 against 3,032; DeltaBlue took 15.5 ms where it
took 14.3, its three runs 14.2 to 15.8 ms, and over four placements of
each build its warm runs are level, 4.4 ms in both. With the JIT the
total is 546 ms where it was 533, and the composite 0.5% lower, 7,758
against 7,798; Richards took 9.9 ms, as it did, its runs 9.9, 9.9 and
13.6 ms: a fixed run of Richards or DeltaBlue lasts a few milliseconds,
much of it before the JIT's code takes over, and falls in one of two
groups in both builds. QuickJS's total is 2,513 ms where it was 2,544,
its composite 1,340 where it was 1,339.

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
  RegExp. By that mean the interpreter takes 0.42 times QuickJS's time,
  and the JIT 0.30 times, where their scores are 2.26 and 5.79 times as
  high.
- The JIT's lead is much larger in the scores than in fixed time. A fixed
  run of Richards or DeltaBlue lasts a few milliseconds, and a function
  runs in the interpreter until it is hot enough to compile, so much of
  that run is spent before the JIT's code takes over: with the JIT,
  Richards takes 1.21 times the interpreter's fixed time and DeltaBlue
  1.05 (Richards' three runs with it took 9.9, 9.9 and 13.6 ms). The
  scores time warm runs, where Richards, DeltaBlue, Crypto, RayTrace and
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
  Splay, 1.31 and 1.42 times the interpreter's score, and RegExp, 1.46
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
three fresh-process runs (lower is better), in the same run of the
matrix, during which no other process took more than a sixth of a CPU.

| Octane | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| Richards | 3.0 ms | 5.3 ms | 3.9 ms | 0.77x | 1.36x |
| DeltaBlue | 5.3 ms | 8.0 ms | 7.8 ms | 0.68x | 1.03x |
| Crypto | 52.1 ms | 22.9 ms | 110 ms | 0.47x | 0.21x |
| RayTrace | 21.2 ms | 31.6 ms | 50.1 ms | 0.42x | 0.63x |
| EarleyBoyer | 63.0 ms | 58.6 ms | 137 ms | 0.46x | 0.43x |
| RegExp | 116 ms | 117 ms | 462 ms | 0.25x | 0.25x |
| Splay | 124 ms | 88.8 ms | 457 ms | 0.27x | 0.19x |
| NavierStokes | 41.3 ms | 18.1 ms | 66.5 ms | 0.62x | 0.27x |
| PdfJS | 185 ms | 244 ms | 383 ms | 0.48x | 0.64x |
| Mandreel | 2,470 ms | 794 ms | 3,350 ms | 0.74x | 0.24x |
| Gameboy | 394 ms | 266 ms | 467 ms | 0.84x | 0.57x |
| CodeLoad | 13.0 ms | 13.6 ms | 12.7 ms | 1.02x | 1.07x |
| Box2D | 88.7 ms | 96.0 ms | 153 ms | 0.58x | 0.63x |
| zlib | 1,612 ms | 570 ms | 8,300 ms | 0.19x | 0.07x |
| Typescript | 1,489 ms | 1,895 ms | 2,134 ms | 0.70x | 0.89x |
| **Total** | **7,108 ms** | **4,616 ms** | **16,668 ms** | **0.43x** | **0.28x** |

| Are We Fast Yet | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| DeltaBlue | 565 ms | 329 ms | 716 ms | 0.79x | 0.46x |
| Richards | 3,243 ms | 685 ms | 4,808 ms | 0.67x | 0.14x |
| Json | 1,017 ms | 258 ms | 1,338 ms | 0.76x | 0.19x |
| CD | 1,597 ms | 497 ms | 2,834 ms | 0.56x | 0.18x |
| Havlak | 3,386 ms | 1,181 ms | 4,347 ms | 0.78x | 0.27x |
| Bounce | 760 ms | 249 ms | 1,488 ms | 0.51x | 0.17x |
| List | 549 ms | 160 ms | 1,256 ms | 0.44x | 0.13x |
| Mandelbrot | 176 ms | 41.0 ms | 698 ms | 0.25x | 0.06x |
| NBody | 667 ms | 123 ms | 1,022 ms | 0.65x | 0.12x |
| Permute | 866 ms | 230 ms | 1,716 ms | 0.50x | 0.13x |
| Queens | 656 ms | 127 ms | 1,071 ms | 0.61x | 0.12x |
| Sieve | 566 ms | 327 ms | 1,226 ms | 0.46x | 0.27x |
| Storage | 1,506 ms | 690 ms | 1,648 ms | 0.91x | 0.42x |
| Towers | 908 ms | 203 ms | 1,565 ms | 0.58x | 0.13x |
| **Total** | **16,508 ms** | **5,095 ms** | **25,783 ms** | **0.64x** | **0.20x** |

- Octane's zlib takes 0.19 times QuickJS's time, 1,612 ms, and 0.07 times
  with the JIT, 570 ms (1,607 and 567 at `0c0bac44`). zlib
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
  register code: 2,470 ms, 0.74 times QuickJS's time, and 794 with the
  JIT (2,488 and 803 at `0c0bac44`). Its largest function is
  compiled at its first call, within the timed run (`d6ac45be`). Its `| 0`
  arithmetic runs as int32 instructions (`add_i32`, `add_imm_i32`, the
  bitwise operators with an immediate) and its typed arrays' elements are
  read and written by native handlers, but each element read and each `|
  0` still converts through a float: raw registers outside asm.js are
  designed and not built (register-interpreter.md, "Known limits and open
  work").
- Without the JIT, go-quickjs is ahead on every benchmark of both suites
  but Octane's CodeLoad, which is mostly compiling: it takes 1.02 times
  QuickJS's time, 13.0 ms, and 1.07 times with the JIT, where it took
  1.00 and 1.07 at `0c0bac44` and 1.22 and 1.37 at `258b4e96`,
  before the front end compiled to register code alone (`d6ac45be`). On
  Are We Fast Yet the interpreter is 21% ahead on DeltaBlue, 22% on
  Havlak, a third on Richards, 39% on Queens and 42% on Towers. A call's
  window of registers is the callee's frame, so nothing is copied, a frame
  record is written only where a reader needs it, and on amd64 and arm64
  a call, a construction and a return are native handlers that never
  enter Go ([Calls](#where-the-time-goes) below). The programs that
  allocate most are the least ahead: Storage 9%, Havlak 22%. Since
  `0c0bac44` the interpreter's totals are level, Octane's 7,111 to 7,108
  ms and Are We Fast Yet's 16,382 to 16,508. Octane's EarleyBoyer took
  63.0 ms where it took 59.2, its three runs 58.4 to 74.9 ms, and over
  four placements of each build its warm runs are level, 0.3% longer;
  Storage took 1,506 ms where it took 1,477, its runs 1,476 to 1,522.
- Octane's total with the JIT is 2.2% shorter, 4,616 ms where it was
  4,720. Typescript takes 1,895 ms where it took 2,004, its runs 1,885 to
  1,911; its code had left native code some 330 times a run at a method
  read of a megamorphic site whose cache missed, the run finished in the
  interpreter, and Go now makes that read from native code, which goes on
  after it (`080f535e`). RegExp took 117 ms where it took 127, its runs
  111 to 131. EarleyBoyer took 58.6 ms where it took 49.7, its runs 46.4
  to 64.2 ms, and Splay 88.8 where it took 83.9, its runs 82.0 to 91.8;
  over four placements of each build their warm runs are level, 0.2%
  longer and 1.0% shorter. Are
  We Fast Yet's total with the JIT is level, 5,096 to 5,095 ms; its Json
  took 258 ms where it took 275, and Queens 127 where it took 118, its
  runs 125 to 131 ms, level over four placements of each build. Storage
  takes 690 ms with the JIT where it took 697.
- With the JIT, Are We Fast Yet takes 0.06 to 0.46 times QuickJS's time,
  its loops of arithmetic the least, its programs that allocate most --
  DeltaBlue, Storage -- the most. Richards' runs with the JIT fall in two
  groups, of about 650 to 690 ms and of 720 to 940: its three in the
  matrix took 683, 685 and 723 ms, and at `0c0bac44` 668, 673 and 677.
- When the stack JIT was removed, its timing had the register JIT slower
  than the stack JIT just before it on Octane's RayTrace (+7.6%) and
  CodeLoad (+25%) and on Are We Fast Yet's Json (+8.2%), Permute (+7.0%)
  and Queens (+6.3%) (docs/performance-plan.md, "The register JIT's
  remaining gaps"). Against the stack JIT's figures at `acffb1aa` (270,
  227 and 119 ms), Json is 4% shorter, 258 ms, Permute 1% longer, 230, and
  Queens 6% longer, 127 ms, its runs 125 to 131; CodeLoad took 14.2 ms
  then and takes 13.6. RayTrace took 31.6 ms, its runs with the JIT 31.3
  to 31.9 ms, where they took 31.7 to 35.4 at `0c0bac44`.
- On eight of Octane's fifteen the JIT is behind the interpreter:
  Richards (77% behind), DeltaBlue (51%), RayTrace (49%), PdfJS (32%),
  Typescript (27%), Box2D (8%), CodeLoad (5%) and RegExp (1%); RegExp
  was 9% behind at `0c0bac44`, 127 ms against 117. Octane's fixed work is
  one run of each benchmark, so compiling counts, and the code runs in
  the interpreter until it is hot; a runtime with the JIT keeps the
  frames the plain interpreter's calls and constructions do without. The
  JIT's own figures are close to what they were at `0c0bac44` -- Richards
  5.5 ms then, DeltaBlue 8.0, RayTrace 32.1, PdfJS 253 -- but for
  Typescript's, above, and so are the interpreter's.

### Server rendering

The ssr suite renders a page with React 18's own builds, as a host behind
a web server does for each request: Render is one runtime rendering the
page again and again, FirstRender five fresh runtimes, each loading React
and rendering once. The same matrix, the median of three fresh-process
runs (lower is better):

| ssr | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| Render | 1,088 ms | 889 ms | 1,881 ms | 0.58x | 0.47x |
| FirstRender | 154 ms | 276 ms | | | |

Render took 0.57 and 0.47 times QuickJS's time at `0c0bac44`, 1,110 and
910 ms, and FirstRender 153 and 282 ms. Render took 1,088 ms here, its
three runs 1,078 to 1,091 ms, and 889 with the JIT, its runs 884 to 905;
QuickJS took 1,881 ms where it took 1,944. FirstRender is 154 and 276 ms.
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
the runtime's goroutine, which waits for each, as a runtime made without
`WithJITBackground` does; `qjs --jit` compiles in the background. Since
v0.39.0 a compile made in the background is installed once the native
calls of the code it replaces are done, where it was thrown away
(`7812928f`), a native caller's ask installs a callee's finished compile
(`f1a05866`), a compile's snapshot asks nothing of callees no site could
inline (`39bdcec8`), and an asm.js module's link compiles on the spot what
the background has no room for (`5307e6a0`), and a compile in the
background asks ahead all its compile asks, so that it makes the code a
compile on the spot makes, and is run as soon as it is installed, a run of
the code it replaced leaving at its next call of Go (`0cf4b86d`); the
figures here are of compiles made on the spot, which none of these
changes.

### Micro-benchmarks

QuickJS's own micro-benchmarks, [`tests/microbench.js`](https://github.com/bellard/quickjs/blob/master/tests/microbench.js),
time one operation each, in nanoseconds. Each test was given the work QuickJS
takes about 150 ms for, all three ran that same work in fresh processes,
all three at `976e5cc1`, and the figure is the fastest of three runs. The
runners' micro mode does this, from a copy of the file kept in `internal/v8bench`, and
the matrix above runs it. `sort_bench` is left out: it reports a run's time
over its fastest sort, not the time of an operation. Over the other 71 tests
go-quickjs takes 0.49 times QuickJS's time by the geometric mean, as it
did at `0c0bac44`, and v0.38.0, the stack design, 0.79 in `913095f4`'s
session: it is slower on 6, faster on 62, and within 5% on the other
three. With the JIT it takes 0.24 times QuickJS's time, as at `0c0bac44`,
slower on 2, faster on 67 and within 5% on 2.
The table has the interpreter's largest gaps either way:

| Test | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| `weak_map_set` | 144 ns | 95.1 ns | 94.4 ns | 1.53x | 1.01x |
| `weak_map_delete` | 369 ns | 441 ns | 249 ns | 1.48x | 1.77x |
| `bigint256_arith` | 177 ns | 121 ns | 126 ns | 1.41x | 0.96x |
| `int_to_string` | 57.7 ns | 42.0 ns | 48.4 ns | 1.19x | 0.87x |
| `date_parse` | 970 ns | 988 ns | 835 ns | 1.16x | 1.18x |
| `func_closure_call` | 35.8 ns | 2.81 ns | 31.7 ns | 1.13x | 0.09x |
| `regexp_utf16` | 251 ns | 241 ns | 256 ns | 0.98x | 0.94x |
| `prop_create` | 50.5 ns | 32.8 ns | 52.1 ns | 0.97x | 0.63x |
| `array_prop_create` | 14.2 ns | 11.1 ns | 15.0 ns | 0.95x | 0.74x |
| `float_toExponential` | 165 ns | 140 ns | 182 ns | 0.91x | 0.77x |
| `global_func_call` | 8.11 ns | 2.09 ns | 32.1 ns | 0.25x | 0.07x |
| `prop_update` | 2.81 ns | 1.89 ns | 11.8 ns | 0.24x | 0.16x |
| `func_call` | 6.75 ns | 0.96 ns | 31.0 ns | 0.22x | 0.03x |
| `array_length_decr` | 12.7 ns | 9.81 ns | 59.1 ns | 0.22x | 0.17x |
| `array_for_in` | 14.9 ns | 12.2 ns | 76.0 ns | 0.20x | 0.16x |
| `string_build2c` | 26.2 ns | 26.8 ns | 143 ns | 0.18x | 0.19x |
| `array_hole_length_decr` | 12.6 ns | 10.0 ns | 70.3 ns | 0.18x | 0.14x |
| `array_for_of` | 7.48 ns | 4.94 ns | 46.2 ns | 0.16x | 0.11x |
| `array_slice` | 4.04 ns | 4.21 ns | 27.2 ns | 0.15x | 0.15x |
| `prop_clone` | 10.7 ns | 9.39 ns | 72.8 ns | 0.15x | 0.13x |

Of the gaps the stack design had, the largest are gone: in v0.38.0,
measured in `913095f4`'s session, `array_prop_create` took 2.25 times
QuickJS's time and takes 0.95, `global_read` 1.79 and 0.84, `array_read`
1.41 and 0.86, `empty_do_loop` 1.34 and 0.52.

The JIT does the most for a test that is a loop of arithmetic, locals,
property reads and writes, array elements and calls: `prop_read` takes
0.81 ns with it, `func_call` 0.96 ns, a fifth of the interpreter's time
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
71 took 7% or more longer with the JIT, the same four as at `0c0bac44`:
`global_destruct`, `global_destruct_strict`, `weak_map_delete` and
`string_build_large1`. Of the extra tests below that QuickJS runs, 16 did,
where 20 had at `0c0bac44`: two of the async tests and
`generator_delegate`, a spread call, `array_map_tail_args`, a sort and
the reads of the 64-bit typed arrays, a global `match`,
`Object.setPrototypeOf` to a fresh object, a class and a subclass made per
call, and four JSON tests: of duplicate keys, new layouts, getters and a
prototype changed between calls; no longer `async_call`, `object_rest`,
three of the sorts, `typed_u64_reduce`, a case-insensitive search, nor
`Object.create` of a fresh object.

Since `0c0bac44` the gains are mostly the JIT's. No cache of the
interpreter's holds a typed array's method, so a site reading one gave
the JIT's code nothing, and the code left there, learned that site alone
and was compiled again, to leave at the next; the interpreter now notes
such a read for the JIT, whose code reads the method natively from the
first (`ad839225`). With the JIT `typed_some` takes 7.33 ns where it took
10.3, `typed_findIndex` 7.12 where it took 9.71, `typed_i64_forEach` 12.3
where it took 16.5 and `typed_map` 10.6 where it took 13.0. A closure the
JIT's code makes is planned at its first use as a callback, so that a
built-in calls it kept ready (`a6aad051`): with the JIT `typed_u8_sort`
takes 216 ns where it took 243, and `typed_sort` and `typed_f64_sort` 217
and 216 where they took 240 and 237. With the JIT `has_own_call_hit`
takes 51.8 ns where it took 63.6 and `array_reduce_upvalue` 7.58 where it
took 9.37. In the interpreter a reaction's job no longer moves its
reaction to the heap, an allocation of 80 bytes at every reaction queued
that KI-85's fix had added (`80e39f8a`): `async_chain` takes 1,522 ns
where it took 1,597, 6.9% less over four placements of each build, and
`async_generator` 718 where it took 752, 3.2% less; with the JIT
`async_chain` takes 1,571 ns where it took 1,696, 8.2% less over the
placements. Over the 418 tests, by the geometric mean, the interpreter's
figures are 1.000 times theirs at `0c0bac44`, and the JIT's 0.979
times.

Some of these tests depend on where the linker places the code more than
on the code. The layout lock keeps the interpreter's hot code where it
measured best, but the code between its anchors, the Go handlers after the
loop and the closures still move with every change to the package, and a
change of a few per cent in one of these tests is not by itself a change in
the engine. A pad that grows past a page moves everything after it 4 KB,
every offset modulo 4 KB the same (docs/optimizing.md, "The layout
lock"). Each round's changes are followed by the lock's pads written
anew, as this one's were (`77a851ee`, `976e5cc1`).

Each row slower than at `0c0bac44` by 7% or more in the matrix's run was
timed again over four placements of each build, loaded at a fixed base,
and those slower at every placement were timed at commits between the
two, two placements of each. With the JIT `class_factory` takes 1,285 ns
where it took 1,135, 11.1% longer over the placements, at all four: a
class made at each turn of a loop had its method read leave native code
at a megamorphic miss and the loop judged slow and run in the
interpreter; that read is Go's from native code now, and so is the
construction past the functions a site constructs natively (`080f535e`,
`5bce0c43`), which together took it from about 1,120 to 1,210 ns, and
the loop runs natively, constructing by a call of Go at every turn. With
the JIT `array_push_args5` takes 40.0 ns where it took 36.6 and
`array_push_args5_single` 54.3 where it took 50.1, 8.7% and 8.6% longer,
at all four: while the collector marks, native code's store of a pointer
over the same pointer goes through the barrier's routine again, as it
must, since that a slot holds a pointer does not say the collector has
shaded it (`b54ed004`, which fixed objects freed while reachable); its
build against its parent's, 15% and 7% longer. `json_parse_duplicates`
takes 487 ns with the JIT where it took 454, 4.8% longer at all four; it
calls no function of the script's, and its time rose between `937b260f`
and `976e5cc1`, whose changes it does not run -- a callback's planning
and the layout lock's pads -- so most likely where the linker put the
code. The rest of the rows 7% or more slower in the matrix --
`weak_map_set`, `dataview_read` and `dataview_write` in the interpreter,
the V8 suite's DeltaBlue and Octane's EarleyBoyer, and with the JIT
`object_spread_large`, Octane's EarleyBoyer and Are We Fast Yet's Queens
-- are level over the placements, within 2.7%, none slower at every
placement; `object_spread_large` with the JIT is 2.7% slower, at three
of the four.

The rows `0c0bac44`'s page left slower or open, with its figures and
`2c758a33`'s in brackets:

- `async_chain` takes 1,522 ns (1,597, 1,474) and `async_generator` 718
  (752, 697), and with the JIT `async_chain` 1,571 (1,696, 1,608), above;
- `int_to_string` takes 57.7 ns (57.7, 61.2) and `bigint256_arith` 177
  (177, 192); with the JIT `append_pair_read` 33.2 (32.2, 49.6) and
  `html_escape_plain` 107 (112, 113);
- with the JIT `string_indexOf` and `string_concat` take 82.8 and 119 ns
  (80.2 and 114, 77.4 and 102), `array_some` 9.64 (9.53, 8.69) and
  `has_own_call_for_in` 59.3 (61.5, 90.9); `string_trim` 56.6 (54.5,
  56.4), and 75.0 without the JIT (75.6, 83.3);
- `json_stringify_decimals` takes 35.2 ns (34.9, 34.7),
  `json_stringify_ints` 25.3 (25.1, 24.7), and with the JIT
  `json_stringify_unique` 124 (125, 126);
- `string_indexOf` takes 88.4 ns (88.4, 80.4), `forward_construct` 73.7
  (80.0, 74.0), `array_indexOf` 109 (115, 107), `json_stringify_strings`
  29.8 (30.5, 31.3), and with the JIT `array_indexOf` 95.6 (95.2, 103);
- `json_stringify_dict` takes 79.8 ns (79.1, 80.2) and
  `string_starts_with` 68.3 (68.9, 71.6); with the JIT `string_template`
  155 (157, 176), `string_code_point_at` 5.32 (5.21, 5.34),
  `has_own_call_miss` 52.7 (52.3, 91.7) and `string_from_code_point`
  79.2 (84.5, 88.4);
- `generator_next` takes 102 ns (102, 102), `dataview_write` 67.2 (62.4,
  61.7), level over the placements (above), and `array_literal_empty`
  70.0 (70.2, 72.6);
- Are We Fast Yet's Storage takes 1,506 ms (1,477, 1,486), and 690 with
  the JIT (697, 683).

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
times its time, and 0.21 times with the JIT (0.501 and 0.214, where it
took 0.499 and 0.219 at `0c0bac44`), and v0.38.0 0.70 and
0.30 over its 313; leaving out the JSON tests, 0.54 and 0.20. The asm.js
kernels' loops (`asm_adler`, `asm_crc32`, `asm_copy`, `asm_fnv`,
`asm_dot`, `asm_switch`) take 0.12 to 0.29 times QuickJS's time as typed
code, and 0.03 to 0.07 with the JIT, which compiles them at the module's
link; their recursion and calls (`asm_fib`, `asm_call_leaf`) 0.80 and
0.55, 0.22 and 0.09 with the JIT, which inlines a typed callee
(`4ae95d8b`) and calls typed code natively from the ordinary code that
drives it (`d14b7cea`), and a call through a function table 1.03, 0.16
with the JIT. A switch on strings past its first few cases
takes 0.05 to 0.16 times QuickJS's time, its literal cases one lookup
(`100eb7f`). An await costs a fiftieth of QuickJS's time (`async_await`
15.5 against 743 ns): an async function resumed by its await's job goes
on in place at an await of a value that needs no job of its own. A
generator resumed from for-of is level (`generator_for_of` 1.03), where
v0.38.0 took 1.44 times, and a for-in over a dictionary takes 0.22 times,
where it took 1.41: its keys are made once for the dictionary's layout and
handed out by the native handlers. Calls in tail position take 0.55 to
0.60 times QuickJS's time, and a `map` callback that returns a call in
tail position 0.48, about level with one that returns an ordinary call,
0.47: such a
callback is entered directly, as any other is (`400fc19c`). A call of
`charCodeAt` whose method is read before its argument, as in
`s.charCodeAt(++i)`, is answered by the call handler from its target, as
V8's call feedback does (`b90b70c4`): `string_charcode_update` takes 0.34
times QuickJS's time. A site that calls fresh closures of two templates in
turn misses its cache at each call and retries the leaves the native call
handler answers (`ab16fd73`), and since v0.43.0 a closure's capture list
is filled directly (`648d7d30`); `closure_leaf_miss` takes 0.93 times,
where it took 1.14 in v0.42.0. A global `exec` still takes 1.33 times, as
it makes a result and a String for each match, and a sticky one 1.04;
their times are those at `0c0bac44`, QuickJS's 6 to 7% shorter. The largest
gaps either way:

| Test | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| `scope_chain` | 647 ns | 674 ns | 371 ns | 1.75x | 1.82x |
| `array_literal_empty` | 70.0 ns | 61.9 ns | 40.2 ns | 1.74x | 1.54x |
| `typed_u8_find` | 46.6 ns | 7.73 ns | 27.1 ns | 1.72x | 0.29x |
| `set_proto_fresh` | 362 ns | 396 ns | 212 ns | 1.71x | 1.87x |
| `subclass_fresh` | 1,132 ns | 1,264 ns | 693 ns | 1.63x | 1.82x |
| `array_fill_down` | 136 ns | 143 ns | 85.5 ns | 1.59x | 1.67x |
| `typed_find` | 48.5 ns | 7.78 ns | 30.9 ns | 1.57x | 0.25x |
| `typed_findIndex` | 47.3 ns | 7.12 ns | 30.9 ns | 1.53x | 0.23x |
| `object_create_fresh` | 336 ns | 350 ns | 230 ns | 1.46x | 1.52x |
| `typed_u64_reduce` | 110 ns | 110 ns | 75.3 ns | 1.46x | 1.46x |
| `asm_fnv` | 6.65 ns | 1.47 ns | 54.2 ns | 0.12x | 0.03x |
| `json_stringify_decimals` | 35.2 ns | 36.8 ns | 333 ns | 0.11x | 0.11x |
| `string_switch_last` | 27.4 ns | 12.9 ns | 265 ns | 0.10x | 0.05x |
| `string_switch_large` | 29.0 ns | 22.1 ns | 404 ns | 0.07x | 0.05x |
| `string_switch_miss` | 17.5 ns | 12.6 ns | 267 ns | 0.07x | 0.05x |
| `regexp_search_utf16` | 684 ns | 720 ns | 13,375 ns | 0.05x | 0.05x |
| `string_switch_nonstring` | 12.2 ns | 7.71 ns | 265 ns | 0.05x | 0.03x |
| `regexp_search_i` | 682 ns | 716 ns | 21,016 ns | 0.03x | 0.03x |
| `json_parse_long` | 1,833 ns | 1,858 ns | 86,564 ns | 0.02x | 0.02x |
| `async_await` | 15.5 ns | 17.7 ns | 743 ns | 0.02x | 0.02x |

A BigInt in the int64 range is held in the value itself (`9746906`), as
QuickJS holds one that fits in a word, and so is one below 2^64 in
magnitude (`f8c3229`); a larger one is a block of no pointers, made by the
engine's own arithmetic (`f1bf681`). Short BigInts' `+`, `-`, `*` and
comparisons are native handlers: `bigint64_arith` takes 0.37 times
QuickJS's time. A larger one is made by the engine's arithmetic in Go:
`bigint256_arith` takes 1.41 times, and the extra BigInt tests 0.43 to
1.22 times, `bigint_toString`, `bigint_pidigits`, `bigint_modpow` and
`bigint64_xorshift` the slowest. The JIT makes BigInts of 64 bits natively: `bigint64_arith`
takes 0.10 times QuickJS's time with it, `bigint64_xorshift` 0.05.

The largest gaps now are:

- objects made on a prototype that is itself new -- `Object.create` of a
  fresh object, `Object.setPrototypeOf` to one, a scope chain built as
  objects, a subclass made per call and its instance -- 1.46 to 1.75
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
  0.21 to 0.70 times QuickJS's time, 0.43 by their geometric mean, where
  v0.38.0, its stack interpreter and its tree tier, took 0.45 to 1.15 and
  0.75 by the mean. `int_arith` takes 0.47 times QuickJS's time and
  `float_arith` 0.36.
- **Calls.** A call's window of registers is the callee's frame: the
  caller writes the callee, the receiver and the arguments there, and
  nothing is copied. A callee the call site's cache knows is entered with
  no question asked again, and only the fields of the frame record a
  reader needs are written. Natively, a call, a construction and a return
  are handlers of their own that keep the caller's state on a native
  frame stack and never enter Go (register-interpreter.md, "Frames and
  calls"). A method call whose body needs a frame takes 34.0 ns against
  69.5 in QuickJS (`method_call`), one that only returns its argument
  12.5 against 42.6 (`method_call_leaf`), an empty function 6.75 against
  31.0 (`func_call`); with the JIT they take 11.7, 2.67 and 0.96 ns. A
  recursive call is level: `call_fib` and `call_mutual` take 0.93 and 0.94
  times QuickJS's time. A body that only returns a parameter or a
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
  takes 0.48 times QuickJS's time, `prop_write` 0.34, `global_read` 0.84,
  `global_write` 0.40, `instanceof_class` 0.25, and `mega_read` and
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
  rather than making a rope (`00ce03b5`): `string_build2c` takes 0.18 times
  QuickJS's time, `local_append_keep` 0.29 and `template_list` 0.35, and
  0.19 times with the JIT on the first of these, where it took 0.29 in
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
  (`typed_array_write`, 0.42, and 0.11 with the JIT).
- **`apply(this, arguments)`.** In a function whose only use of
  `arguments` is passing it to `apply` (a common way to write a class
  constructor), the arguments object is never made: the call hands on the
  frame's arguments as they were passed (`apply_args`). A constructor
  whose whole body is `this.initialize.apply(this, arguments)`, as
  RayTrace's are, is constructed by its call site as a forwarder, and
  natively, where the target is an initializer, without entering either
  frame (`746d9438`, `bdab5ba4`): `forward_construct` takes 0.20 times
  QuickJS's time, and 0.02 with the JIT. It took 0.54 at `258b4e96`, 0.23
  at `913095f4`, 0.32 at `2f05d3fc`, 0.25 at `c2dae008`, 0.24 at
  `b8c2c45f` and `b5df63ea`, 0.20 at `7f8205ec`, 0.19 at `2c758a33` and
  0.21 at `0c0bac44`, level over four placements of each build since.

What remains in the interpreter is mostly in Go: the operations with no
native handler and the fast paths that miss, which exit to their Go
handlers, the built-ins and the object model -- the gaps above are in
them -- and the collector. The JIT is the way past the rest: a hot
function compiled to native code dispatches nothing, which is why, with
it, the loops and calls above take from a third of the interpreter's
time down to a tenth.

[goja]: https://github.com/dop251/goja
[v8-v7]: https://github.com/mozilla/arewefastyet/tree/master/benchmarks/v8-v7
