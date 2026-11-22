# JIT compilation time

## Rejected trial: borrowing cached property descriptions

A fresh TypeScript CPU profile at `8e5fbf65`, with 80 repeated compiles
and no allocation profiling, puts about 16% of the sampled compile work
in SSA planning. Determining whether a property needs a host exit reaches
the builder's cached property and primitive-method descriptions. Returning
these descriptions by value copies the whole structure even when the
caller asks only whether feedback exists or reads one flag. A rejected
primitive-method query also returns an entire zeroed structure.

The trial made the builder borrow its cached descriptions. Planning read
them directly; instruction lowering made a local copy before specializing
a site's shape and index. The external feedback interfaces still returned
values. The production change was rejected after longer measurements
confirmed interval-phase regressions. The builder continues to return
descriptions by value. `TestPropertyLoweringKeepsCachedFeedback` exercises
lowering a write from its first polymorphic case and checks that the cached
answer keeps its original shape and index, with and without a workspace.

Five fresh-process pairs, randomized with seed 741, 500 ms per case,
on darwin/arm64, Apple M5 Max and Go 1.27.3 give:

| Cached query | Base / trial ns | Allocations |
|---|---:|---:|
| Property description | 8.771 / 2.362 | 0 / 0 |
| Primitive-method description | 9.148 / 2.767 | 0 / 0 |
| Absent primitive method | 7.891 / 1.870 | 0 / 0 |

The same process pairs measure the existing build-and-optimize fixtures:

| Fixture | Base / trial us | Base / trial allocations |
|---|---:|---:|
| Sum | 4.026 / 3.987 | 12 / 12 |
| Closures | 127.442 / 124.568 | 420 / 420 |
| Branches | 59.885 / 59.179 | 19 / 19 |
| Calls | 32.328 / 31.718 | 23 / 23 |
| Prototype reads | 20.960 / 19.134 | 13 / 13 |

The trial's focused lookup reductions are about 70-76%; its prototype
fixture improves 8.7%. Allocation volume is unchanged apart from rounding
noise. The retained benchmark measures the original implementation:

```sh
go test ./internal/jit/ssa -run '^$' \
  -bench 'Benchmark(PropertyFeedback|BuildOptimize)$' -benchmem
```

Five fresh-process pairs per threading mode, 60 repetitions per compile,
randomized with seed 742 and with normal collection, give:

| Workload | Default base / trial ms | GOMAXPROCS=1 base / trial ms |
|---|---:|---:|
| PdfJS | 26.27 / 26.29 | 31.91 / 31.94 |
| Mandreel | 45.90 / 45.76 | 51.70 / 52.04 |
| Gameboy | 38.13 / 37.79 | 44.18 / 44.69 |
| zlib | 43.36 / 43.78 | 46.60 / 49.52 |
| Typescript | 165.13 / 161.66 | 192.23 / 190.52 |

TypeScript's build phase falls from 63.10 to 59.44 ms at default threads
and from 75.10 to 71.08 ms at one thread (5.8% and 5.4%). Process CPU,
including collection and the benchmark's execution, falls from 11,393.4
to 11,172.2 ms and from 12,436.7 to 12,311.1 ms respectively. Other
workloads are mixed, and single-thread zlib is initially 6.3% slower.

The initial default-thread interval medians exceed the 10% threshold:
PdfJS 0.14 / 0.18 ms, Mandreel 0.29 / 0.33, Gameboy 0.24 / 0.30,
zlib 0.26 / 0.29, and TypeScript 0.68 / 0.75. At one thread, PdfJS's
intervals (0.15 / 0.17) and encode (0.60 / 0.71), and zlib's optimize
(14.58 / 16.13) and intervals (0.24 / 0.30) also exceed it. These results
prompted isolated measurements. The MIR allocator's address moves by
848 bytes on this arm64 build, but that alone does not establish why a
phase changed.

Five isolated process pairs, randomized with seed 743, 200 repetitions
per compile, give 44.58 / 44.38 ms for single-thread zlib and
26.39 / 26.28 ms for default-thread PdfJS. Every phase is within the
10% threshold in these two checks: zlib's optimize is 14.05 / 13.90 ms
and intervals 0.26 / 0.25, and PdfJS's intervals 0.14 / 0.15. The
larger single-thread zlib slowdown does not persist in isolation.

The remaining isolated checks use five randomized process pairs (seed
744) and 150 repetitions per compile, with no concurrent builds or tests:

| Workload | Threads | Total base / trial ms | Intervals base / trial ms |
|---|---|---:|---:|
| Mandreel | Default | 45.99 / 45.87 | 0.30 / 0.35 |
| Gameboy | Default | 38.29 / 38.21 | 0.24 / 0.31 |
| zlib | Default | 45.82 / 45.85 | 0.27 / 0.26 |
| Typescript | Default | 161.81 / 158.54 | 0.76 / 0.69 |
| PdfJS | One | 30.55 / 30.45 | 0.14 / 0.19 |

TypeScript's build phase remains faster, 62.34 / 59.09 ms (5.2%), and
its total compile time improves 2.0%. However, the interval regressions
persist for Mandreel (16.7%), Gameboy (29.2%) and single-thread PdfJS
(35.7%). Every other phase is within the threshold in these checks.
The lookup gains do not justify landing the trial with these unresolved
regressions. Its production changes are removed without proceeding to
execution-placement or conformance-memory comparisons.

Three actual-background process pairs (`-jitbackground -n 3`, without
`-jitcompile`) give these initial medians:

| Workload | Base / trial ms | Base / trial process CPU ms |
|---|---:|---:|
| PdfJS | 184.5 / 183.8 | 294.4 / 288.4 |
| Mandreel | 699.8 / 689.9 | 734.0 / 728.4 |
| Gameboy | 274.6 / 268.2 | 342.9 / 332.2 |
| zlib | 499.7 / 517.2 | 542.2 / 553.2 |
| Typescript | 1456.7 / 1364.3 | 2028.4 / 1939.0 |

These include execution and background scheduling; they do not isolate
the compiler's work. In particular, the TypeScript improvement here is
not a claim of a general 6.3% speedup.

The trial passed the full plain, JIT and Go-handler suites, background
race and stress tests, encoder verification and all four test262 modes
(100,277 passes and 42 existing skips each). Those checks establish
correctness, not a compile-time improvement. Removing the lowering's local
copy made the new cache-preservation test fail in both heap and workspace
variants. The test and lookup benchmark are retained; the production
change and its generated layout adjustments are removed.
The retained SSA suite passes on Go 1.27.3 and Go 1.24.13 against the
restored production code. Regeneration returns the pads to their original
contents and verifies all eight layout configurations.

## Sparse state feedback for background compiles

The background snapshot profile at `da485c30` shows time in the generated
`Resumed` and `EntryGeneric` askers, including effect capture for their false answers.
Snapshots previously asked both methods at every register instruction,
although their answers come from the learned resume and failed-entry
lists and from a cached list of recursive call sites. Unlisted positions
already return false from the snapshot, within its register-code bound.

Snapshot preparation now visits those sparse lists and asks the existing
feedback methods at the positions they name. Recursive-call discovery is
shared with live feedback and honors an already-filled cache. The worker
still reads captured boolean answers, independent of later runtime changes;
out-of-range state questions still miss. The enumeration helper lives beside
the live state feedback in `jit_feedback.go`: putting it after the JIT leaf
anchor exhausted the native-run pad in the configurations with WebAssembly.
Regeneration preserves all eight configurations' recorded pages and offsets.

Five fresh-process pairs, randomized with seed 736, one second per case,
on darwin/arm64, Apple M5 Max and Go 1.27.3 give:

| Benchmark | Before / after us | Before / after bytes | Before / after allocations |
|---|---:|---:|---:|
| Background snapshot | 3.563 / 3.219 | 6,592 / 6,592 | 61 / 61 |
| Background worker | 22.595 / 22.646 | 7,816 / 7,816 | 63 / 63 |

This is a 9.7% reduction in snapshot preparation in this fixture, with
worker compilation essentially unchanged. Reproduce the focused benchmark
with:

```sh
go test -tags quickjs_jit ./internal/vm -run '^$' \
  -bench BenchmarkJITBackgroundCompile -benchmem
```

Three fresh-process pairs per threading mode, 40 repetitions per compile
and randomized with seed 737, give these initial whole-compiler medians:

| Workload | Default before / after ms | GOMAXPROCS=1 before / after ms |
|---|---:|---:|
| PdfJS | 26.84 / 26.71 | 33.72 / 34.61 |
| Mandreel | 45.85 / 45.82 | 52.82 / 52.95 |
| Gameboy | 38.17 / 38.94 | 44.40 / 45.46 |
| zlib | 43.81 / 45.64 | 47.87 / 50.71 |
| Typescript | 174.93 / 169.46 | 195.14 / 200.59 |

These initial samples are mixed. Mandreel's default-thread interval phase
is 0.28 / 0.32 ms. Single-thread phases over the 10% threshold are PdfJS's
prepare (0.75 / 0.86 ms) and scan (1.87 / 2.07), Gameboy's saves
(0.55 / 0.61), zlib's saves (0.55 / 0.62) and encode (0.15 / 0.22), and
TypeScript's prepare (4.10 / 4.54) and intervals (0.74 / 1.07). These
prompted longer isolated measurements with normal collection.

The longer check fixes its sample size at five randomized process pairs
per workload (seed 738), 150 repetitions per compile. Mandreel uses default
threads; the other four use `GOMAXPROCS=1`. Total compile medians are
46.86 / 46.26 ms for Mandreel, 29.98 / 30.19 for PdfJS, 43.79 / 43.70 for
Gameboy, 46.95 / 47.81 for zlib, and 190.06 / 189.60 for TypeScript.
Every phase is within the 10% threshold. The initially slower phases are:

| Workload and phase | Before / after ms |
|---|---:|
| Mandreel intervals | 0.33 / 0.36 |
| PdfJS prepare | 0.65 / 0.65 |
| PdfJS scan | 1.76 / 1.87 |
| Gameboy saves | 0.53 / 0.52 |
| zlib saves | 0.41 / 0.41 |
| zlib encode | 0.14 / 0.15 |
| Typescript prepare | 4.03 / 4.14 |
| Typescript intervals | 0.70 / 0.76 |

Actual background mode omits `-jitcompile`, which repeats synchronous
compiles even when `-jitbackground` is supplied. Three process pairs with
`-jitbackground -mode fixed -n 3` give:

| Workload | Before / after ms | Before / after process CPU ms |
|---|---:|---:|
| PdfJS | 186.4 / 184.5 | 295.8 / 291.6 |
| Mandreel | 695.3 / 704.9 | 729.0 / 730.7 |
| Gameboy | 271.4 / 265.8 | 337.5 / 375.2 |
| zlib | 494.9 / 491.3 | 541.9 / 535.1 |
| Typescript | 1396.2 / 1380.5 | 1971.2 / 1962.1 |

Background scheduling changes which compiles finish during the measured
run. These small wall-time differences and Gameboy's higher CPU do not
establish a general workload speedup; the focused snapshot measurement
isolates the work removed.

The darwin/arm64 `v8bench` binary grows by 176 bytes, from 46,696,930
to 46,697,106 bytes (`CGO_ENABLED=0`, `-buildvcs=false`). Median peak RSS
in the initial five-workload batches is 852 / 850 MB at default threads,
859 / 832 MB at one thread, and 746 / 771 MB in background mode.
The background samples vary from 740-787 MB before and 727-774 MB after;
this change does not demonstrate a reduction in peak memory.
One isolated background-stress test262 pair also passes all variants on
both builds, with peak RSS of 9.27 / 9.69 GB. That single pair does not
establish a retention trend; the higher sample remains part of the result.

A two-placement comparison, three rounds per placement using
`placements compare -k 2 -rounds 3`, gives the following whole-workload
changes. The background comparison also supplies `-jitbackground`.

| Workload | Synchronous | Background |
|---|---:|---:|
| PdfJS | +0.0% | +0.1% |
| Mandreel | +1.2% | +0.5% |
| Gameboy | +1.6% | +0.8% |
| zlib | -0.2% | -0.4% |
| Typescript | +0.6% | -1.3% |

No workload is 3% slower at both placements in either mode. This is an
incremental two-placement check on arm64, not an eight-placement final
branch comparison or an amd64 execution measurement.

`TestJITBackgroundSnapshotStatePCs` checks empty and absent feedback,
learned and recursive positions together, duplicate entries, non-self
targets, an already-filled self-call cache, valid boundary positions,
invalid positions and isolation from later mutation of every source list.
Full plain and JIT-tagged suites, the uncached `QJS_NONATIVE=1` suite,
background stress and background/snapshot race tests pass. Plain, debugger,
JIT-stress and background-stress test262 each pass 100,277 variants with
42 existing skips and zero failures. Go 1.24.13's JIT package tests and
targeted background tests pass, as do both encoders' verifier tests and
Windows amd64/Linux arm64 cross-builds. The Linux 386 root test binary
builds but is not executed on this macOS host.

Native comparison matches 1,234 of 1,236 non-TypeScript arm64 records,
including the diagnostic cross-emission records. The two differences are
address loads in Mandreel's large constraint solver, and the baseline
self-control differs at those same loads. TypeScript varies against itself:
the complete candidate comparison has 2,434 matching, 80 different and
80 unmatched records, versus 2,334 / 82 / 152 in the baseline control.
Linux amd64 `hotdiff` retains every hot function's instruction count and
the exact native handler blocks, but exits 1 on changed PC-relative data
addresses. These comparisons do not establish byte-for-byte identity.

## Call emission helpers

A fresh TypeScript compilation profile at `bf53680f` identifies native-call
emission as a large allocator. Each backend's call emitter creates small
closures for finding argument words, loading the callee's frame address
and entering the fallback. Deferred cold paths retain these helpers,
so Go allocates their environments even for ordinary calls whose cold
path does not need every helper.

The helpers are now compiler methods with their inputs passed explicitly.
The amd64 flag saying that the frame address is cached in an XMM register
is named `frameInX`, distinct from a setup block's local flag saying that
the frame address has been loaded. Label creation and instruction order
are unchanged; the deferred emission closures remain where they were.

`BenchmarkCompileNativeCalls` compiles a function with 16 native calls
on both backends, checking that all 16 survived SSA building. Five
fresh-process pairs, one second per case, randomized with seed 734,
on darwin/arm64 and Go 1.27.3 give:

| Backend | Before / after us | Before / after B | Before / after allocations | Code bytes |
|---|---:|---:|---:|---:|
| amd64 | 38.354 / 38.399 | 21,207 / 20,183 | 174 / 126 | 10,356 on both |
| arm64 | 29.729 / 29.387 | 19,181 / 18,285 | 168 / 120 | 8,260 on both |

This removes three allocation objects per native call. The timing gain
is small on arm64, and there is no demonstrated amd64 timing gain in
this fixture; both emitters are run by an arm64 host, not native amd64.
Reproduce with:

```sh
go test ./internal/jit/mir -run '^$' \
  -bench BenchmarkCompileNativeCalls -benchmem -count=5
```

Three fresh-process pairs per threading mode, 40 repetitions of each
compile across all five required Octane workloads, give:

| Workload | Default before / after ms | GOMAXPROCS=1 before / after ms |
|---|---:|---:|
| PdfJS | 27.66 / 27.33 | 33.53 / 34.10 |
| Mandreel | 46.88 / 47.05 | 51.11 / 52.17 |
| Gameboy | 38.73 / 38.75 | 44.30 / 43.14 |
| zlib | 44.54 / 45.02 | 46.95 / 47.20 |
| Typescript | 174.77 / 172.62 | 194.08 / 196.12 |

Every default-thread phase is within the 10% threshold. Single-thread
zlib's saves phase is initially 0.40 / 0.60 ms. An isolated sample of
five randomized process pairs (seed 735), 150 repetitions, gives
0.41 / 0.38 ms for saves and 44.62 / 44.74 ms total; no phase exceeds
the 10% threshold. The initial outlier does not persist.

TypeScript's default-thread compiler allocation objects fall from
281,054 to 265,983 per repetition, but bytes are 55.65 / 56.32 MB;
compilation counts and feedback differ among processes. Single-thread
counts are 282,234 / 269,584, and bytes 56.30 / 56.85 MB. Process CPU,
including collection and benchmark execution, is 8,151.9 / 8,187.6 ms
at default threads and 8,616.8 / 8,703.5 ms at one thread. These samples
establish fewer allocation objects, not an overall CPU or memory-volume
gain in TypeScript.


A longer isolated single-thread TypeScript sample uses five randomized
fresh-process pairs (seed 735), 100 repetitions, normal collection and
no concurrent builds or tests. Median total compilation is 193.93 /
190.14 ms (2.0% less), and emission 33.24 / 32.14 ms (3.3% less).
Every phase is within the 10% threshold. Allocation objects fall from
274,817 to 257,471 (6.3%), and volume from 49.84 to 49.40 MB. Process
CPU including collection and benchmark execution falls from 20,717.8
to 20,337.1 ms (1.8%); elapsed time is 20,940.9 / 20,507.1 ms. This
supports a modest TypeScript improvement in the longer sample, while
the first sample's slower results remain recorded above.

Actual background compilation, with `-jitbackground -n 3` and without
`-jitcompile`, has these median elapsed times over three process pairs:

| Workload | Before ms | After ms |
|---|---:|---:|
| PdfJS | 178.7 | 185.8 |
| Mandreel | 681.8 | 685.7 |
| Gameboy | 261.5 | 262.7 |
| zlib | 489.9 | 486.5 |
| Typescript | 1,382.5 | 1,329.2 |

These include execution and scheduling; the small, mixed changes are not
evidence of a general speedup.


Full plain and JIT suites, the uncached JIT suite with `QJS_NONATIVE=1`,
background stress and background race checks pass. Plain, debugger,
standard JIT stress and background-stress test262 each pass 100,277
variants, with zero failures and 42 existing skips. Go 1.24.13's full
internal/jit tests and both encoder verifiers pass. Windows/amd64 and
Linux/arm64 builds pass; the Linux/386 root test binary builds but is not
executed on this host. Regeneration retains all eight layout targets,
with four JIT package pads updated.

Native output is compared for both backends: a temporary Go build
overlay switches the diagnostic cross-compile from arm64 to amd64 and
labels those records accordingly. The engine still runs arm64 code;
this is not an amd64 execution test. Outside TypeScript, all 618 amd64
compiles match, and 616 of 618 arm64 compiles match. Two arm64 compiles
of Mandreel's largest function differ in instruction count; the baseline
control differs in that function's address loads. Across all workloads,
2,446 compiles match, 80 differ and 62 are present in only one run. The
baseline control has 2,507 matches, 45 differences and 36 unmatched
compiles. TypeScript remains variable against itself, so identity is
not established for every compile.

Linux/amd64 `hotdiff` finds the native handler blocks and hot Go function
instruction counts unchanged. It reports differences in PC-relative
data addresses; this is not a clean pass of that tool.

## Background snapshot method scan

Background compilation first captures feedback on the runtime's goroutine.
For every lowered instruction, `jitSnapshot.askReg` used to scan all 59
feedback method slots to find the few selected by the instruction's mask.
It now visits the set bits with `bits.TrailingZeros64`, clearing each bit
after visiting it. Questions still run in increasing method order, and
the register-PC questions, callee traversal and side-effect capture are
unchanged. This reduces preparation before the work is submitted to a
worker; synchronous `-jitcompile` does not exercise this path.

The baseline is `902439ad`, whose production compiler is the same as
`d08557da`. Measurements use the same darwin/arm64 host and Go 1.27.3.
Five fresh-process pairs in randomized order (seed 732), one second per
focused benchmark, give:

| Background phase | Before ns | After ns | Bytes / objects per operation |
|---|---:|---:|---:|
| Snapshot preparation | 4,167 | 3,747 | 6,592 / 61 on both |
| Worker compilation | 23,753 | 23,575 | 7,816 / 63 on both |

Snapshot preparation is 10.1% faster in this fixture. The worker's 0.7%
difference is small, and its implementation is unchanged. The fixture
includes JSON calls that survive inlining. Reproduce it with:

```sh
go test -tags quickjs_jit ./internal/vm -run '^$' \
  -bench BenchmarkJITBackgroundCompile -benchmem -count=5
```

Three fresh-process pairs per threading mode, 40 repeated compiles per
workload, give these synchronous compilation medians:

| Workload | Default before / after ms | GOMAXPROCS=1 before / after ms |
|---|---:|---:|
| PdfJS | 27.59 / 27.80 | 32.90 / 33.42 |
| Mandreel | 48.05 / 47.68 | 52.50 / 53.16 |
| Gameboy | 39.85 / 39.55 | 45.26 / 43.58 |
| zlib | 48.10 / 48.17 | 49.20 / 49.59 |
| Typescript | 176.15 / 177.21 | 206.42 / 197.10 |

Every default-thread phase is within the 10% threshold. Single-thread
zlib's saves phase is initially 0.51 / 0.62 ms. An isolated comparison,
five randomized fresh-process pairs (seed 733), 150 repetitions, gives
0.39 / 0.39 ms for saves and 45.05 / 44.97 ms total. Every phase in that
comparison is within 4% of baseline. The first outlier does not persist;
this synchronous path does not prepare snapshots.

The same five workloads also run with `-jit -jitbackground -mode fixed
-n 3`, without `-jitcompile`, in three fresh-process pairs. Median elapsed
times for the complete benchmark are:

| Workload | Before ms | After ms |
|---|---:|---:|
| PdfJS | 186.6 | 182.5 |
| Mandreel | 680.4 | 690.5 |
| Gameboy | 265.0 | 262.8 |
| zlib | 494.0 | 480.2 |
| Typescript | 1,376.5 | 1,363.3 |

These runs include execution, scheduling and installation, and show small
mixed differences rather than a demonstrated general speedup. The
focused preparation benchmark isolates the benefit this change targets.


Validation passes the full plain and JIT Go suites, an uncached full JIT
suite with `QJS_NONATIVE=1`, background stress and background race tests.
Plain, debugger, standard JIT stress and deterministic background stress
test262 each pass 100,277 variants, with zero failures and 42 existing
skips. Native code runs in 62,110 standard-stress variants and 43,238
background-stress variants. The focused background tests also pass on
Go 1.24.13; encoder verification and Windows/amd64 and Linux/arm64 builds
pass. The Linux/386 root test binary builds, but is not run on this host.

Native comparison uses actual background compilation with
`QJS_JIT_STRESS=background=3`, `-jitbackground -jitheld -jitcode`.
Outside TypeScript, 1,232 compiles match and four Mandreel compiles differ
in instruction count; the baseline control reports the same four
Mandreel differences, with the same counts. Overall there are 2,432
matches, 86 differences and 70 compiles present in one run alone; the
baseline control has 2,430 matches, 100 differences and 70 unmatched
compiles. TypeScript remains variable against itself, so this comparison
does not establish identity for every compile.

Generation preserves all eight layout configurations without changing
pads. Linux/amd64 `hotdiff` finds identical native handler blocks and
instruction counts in the hot Go functions. It reports differences in
Go functions' PC-relative data addresses, all displaced by 16 bytes;
that comparison is not a clean tool pass and is not reported as one.

## Repeated phi inputs

Measured on an Apple M5 Max with Go 1.27.3, against the allocator before
this change. The allocator counts every phi edge's use and weight, and
pairs a phi and an input once for register and spill coalescing. A joined pair is already in one class; a conflicting pair
cannot stop conflicting as its classes grow. The member's pool offset,
unused until group initialization, holds the last phi seen, so there is
no extra table or clearing pass. zlib's largest function has 77,199 pairs
before this filtering and 57,793 after it.

Five runs of `BenchmarkCompileARM64/repeated-phi-inputs`, a 128-edge
switch with 32 live phis, take a median 524.84 us before and 456.23 us
after (13.1% less). Both emit 21,172 bytes and allocate about 21 KB in
29 objects. The sum and region controls take 4.32 / 4.18 us and
15.83 / 15.23 us. `TestRepeatedPhiInputs` checks both backends with the
allocation checker, forces spills, and repeats in a rewound workspace.

Three alternating fresh-process pairs, 40 compilation repetitions per
workload, give these median total compilation times:

| Workload | Default before / after ms | GOMAXPROCS=1 before / after ms |
|---|---:|---:|
| PdfJS | 28.46 / 28.96 | 34.82 / 34.82 |
| Mandreel | 47.98 / 48.67 | 52.97 / 53.30 |
| Gameboy | 39.31 / 39.47 | 46.27 / 45.02 |
| zlib | 47.41 / 51.22 | 48.52 / 49.01 |
| Typescript | 178.54 / 181.66 | 192.95 / 198.07 |

This first sample does not show an overall gain. zlib's default-thread
optimize, prepare, saves and emit phases exceed the 10% threshold, as
does Gameboy's single-thread saves phase. Longer isolated comparisons
use five alternating fresh-process pairs and 150 repetitions, with
normal collection. In zlib, total compilation is 45.62 / 45.35 ms and
hints 1.58 / 1.36 ms (13.9% less); every phase is within 3.8% of the
baseline or faster. The improvement is concentrated in repeated phi
inputs, not a general speedup of the compiler.

The longer single-thread Gameboy comparison gives 44.31 / 44.15 ms
total and 0.58 / 0.53 ms for saves; every phase is within 2.6% of the
baseline or faster. Neither first-sample phase regression persists.

`compilediff native -any` over those five workloads finds 2,470 compiles
the same, 68 different and 66 present in only one run. Outside TypeScript,
1,242 match and two Mandreel address loads differ. A second baseline run
has those same two Mandreel differences; overall it has 2,430 matches,
74 differences and 76 compiles in only one run. TypeScript's compile
sequence varies even against itself, so this is not proof of identity
for every TypeScript compile. The plain and JIT-tagged full Go suites
pass, and the generated layout pads align all eight configurations.

The allocation-checker stress configuration
`threshold,budget=7,deopt=5,check` exposes two NumberFormat
`constructor-unit.js` failures. Both reproduce with the baseline
allocator: 500 of 502 NumberFormat variants pass on both builds. The
full changed run passes 100,275 variants, fails those two and has the
42 existing skips. The two failures are open, not counted as passes.

The standard stress setting, `threshold,budget=3,deopt=7`, passes all
100,277 executed variants with 42 existing skips. The JIT and VM tests
also pass with `QJS_NONATIVE=1`. `hotdiff` finds the interpreter's hot
functions and native handler blocks identical on Linux/amd64.


## Rejected sharing of native-call feedback

Starting at `d08557da`, a trial made `NativeCalls` return read-only
cached descriptions, including answers read from a background snapshot.
The SSA builder copied only descriptions whose receiver coercion or
BigInt leaf flag it could change. This removed a 576-byte allocation
from each cached or snapshot lookup: the focused benchmarks took about
71 to 1.6 ns and 75.5 to 9.9 ns respectively. The production change is not
retained, because the compiler phase comparisons did not meet the
regression threshold.

On the same darwin/arm64 host and Go 1.27.3, three alternating process
pairs, 40 compilation repetitions per workload, gave these medians:

| Workload | Default before / trial ms | GOMAXPROCS=1 before / trial ms |
|---|---:|---:|
| PdfJS | 27.71 / 27.97 | 34.68 / 35.45 |
| Mandreel | 47.67 / 47.80 | 52.80 / 52.06 |
| Gameboy | 39.35 / 39.77 | 44.75 / 44.39 |
| zlib | 47.92 / 47.10 | 48.52 / 48.16 |
| Typescript | 179.69 / 175.13 | 193.74 / 199.17 |

TypeScript's default-thread allocation volume fell from 56.70 to
54.04 MB per repetition. However, intervals were 12-25% slower in four
workloads; single-thread PdfJS's hints were 32.5% slower too. A longer
comparison with randomized pair order (seed 731), 100 repetitions,
confirmed interval regressions. Its first three complete pairs gave
0.14 / 0.16 ms for PdfJS, 0.33 / 0.37 ms for Mandreel and 0.26 / 0.32 ms
for Gameboy. Total compilation was 27.04 / 26.67, 47.20 / 47.32,
39.11 / 38.93 and 181.20 / 177.64 ms for PdfJS, Mandreel, Gameboy and
TypeScript. The longer batch was stopped after the rejection; further
partial runs are not used here. These results do not establish why an
unchanged interval-construction phase slows down, and do not justify
accepting it on the strength of the TypeScript total alone.

The ownership tests caught mutations in the old SSA builder and passed
with the trial; background snapshot isolation and race checks passed.
Native comparison found 2,460 compiles identical, 78 different and 40
present in one run alone. Every differing compile was in TypeScript,
which also varied in the baseline control (2,480 identical, 64 different,
36 present in one run). This does not prove every TypeScript compile is
unchanged.

`BenchmarkJITCallFeedback` and `BenchmarkJITBackgroundCompile` are kept
for subsequent work. The latter separates snapshot preparation on the
runtime's goroutine from compilation using the captured snapshot. Its
fixture contains JSON calls, since the initial Point/norm fixture inlined
away the calls whose feedback this experiment was intended to measure.
`TestJITCallFeedbackSnapshot` checks that a worker and workspace rewind
leave captured answers intact after the runtime changes call targets.
The layout generator fix discovered during this experiment is separate:
it preserves recorded pages when shrinking code makes a pad cross 4 KB.

## Rejected closure-table reuse

The next comparison starts at `cac452a9`. A TypeScript allocation profile
shows closure analysis allocating a value-user index, its fill positions,
user pointers and control flags for every build that contains candidate
closures. A trial borrowed the optimizer's existing phi-user tables and
flags, which are idle during building, cleared the user pointers before
returning, and let the optimizer reuse the storage immediately. It
introduced no new cache or scratch fields; existing workspace accounting
and trimming included these arrays. Neither this trial nor the arena
variant below is retained in production.

An earlier version put the four tables in the workspace's arenas. It
reduced allocation volume, but the focused compile benchmark took
125.56 / 127.54 us, and the three-pair default-thread TypeScript sample
took 179.65 / 184.96 ms. That version was not kept. The scratch version
also avoids clearing the fill positions before copying them, and the
user list before filling every element.

Five focused runs of `BenchmarkBuildOptimize/closures`, a loop making
32 closures, take 123.89 / 122.66 us. Allocation volume falls from about
94,387 to 82,162 bytes per compile (13%), and objects from 420 to 416.
The small timing difference is not evidence of a large speedup.
`TestCalledClosuresWorkspace` checks that builds of different sizes,
reusing one workspace, match the independently built optimized SSA. It
also checks that the fixture contains all its closure calls. The benchmark
and this build-equivalence test are retained for future experiments.

The final whole-compiler comparison uses five fresh-process pairs for
each of default threads and `GOMAXPROCS=1`, 50 compilation repetitions
per workload. Pair order and the order within each pair are randomized
with seed 2026; no builds or tests run concurrently with this batch.

| Workload | Default before / trial ms | GOMAXPROCS=1 before / trial ms | Single-thread before / trial MB |
|---|---:|---:|---:|
| PdfJS | 26.86 / 26.87 | 32.69 / 31.62 | 9.73 / 9.35 |
| Mandreel | 45.91 / 45.78 | 50.24 / 50.50 | 10.41 / 10.40 |
| Gameboy | 38.29 / 38.14 | 43.31 / 44.03 | 13.55 / 12.95 |
| zlib | 43.46 / 43.75 | 44.26 / 44.58 | 5.74 / 5.72 |
| Typescript | 171.10 / 166.68 | 186.52 / 191.56 | 53.91 / 53.82 |

The whole batch's process CPU, including collection, is 19,277.6 /
18,980.1 ms at default threads (-1.5%), but 19,867.1 / 20,118.7 ms at
one thread (+1.3%). Peak Go heap is 731.0 / 747.0 MB at default threads
and 751.5 / 751.7 MB at one thread. Fewer temporary bytes do not establish
lower peak memory or generally faster compilation.

The phase threshold also fails: at one thread, PdfJS hints are 0.74 /
0.84 ms (+13.5%), Gameboy prepare 1.20 / 1.34 ms (+11.7%), and zlib
saves 0.46 / 0.54 ms (+17.4%). At default threads, PdfJS intervals are
0.14 / 0.16 ms (+14.3%) and zlib intervals 0.24 / 0.31 ms (+29.2%).
These are small absolute costs, but the rule applies to each phase.
The experiment is rejected rather than assuming that every difference
is noise or that an allocation reduction must improve compilation time.

The trial's correctness checks pass: the SSA tests, full Go suites plain
and with the JIT, an uncached full suite with `QJS_NONATIVE=1`, the
encoder verifier, and plain, standard JIT-stress and debugger
test262 (each 100,277 passed, zero failures, 42 existing skips). Linux
386's root test binary and the Windows amd64 JIT packages cross-build.
The 386 binary is not executed on this macOS host. All eight generated
layout configurations align.

Native comparison matches 1,240 of the 1,244 non-TypeScript compile
records. The four differing Mandreel records have the same instruction
count differences in a control run of the changed binary against itself;
TypeScript's compile sequence varies as in the earlier comparison.
Linux/amd64 `hotdiff` shows identical native handler blocks and identical
Go instruction counts, with differences only in RIP-relative addresses
of relocated data. These comparisons do not claim byte identity where
the tools still report address or run-dependent differences.

The allocation profile still points to native-call feedback copies as a
larger candidate. Removing those copies needs an ownership change: the
SSA builder and optimizer mutate accepted call descriptions, while
background compilation reads a feedback snapshot. Sharing the returned
descriptions directly would let one build change another build's input.

The original allocation measurements follow.

Measured on November 21, 2026 against `1d08a62d`, using Go 1.27.3 on
darwin/arm64 (Apple M5 Max). Both v8bench binaries use `CGO_ENABLED=0`,
`-buildvcs=false` and `-tags quickjs_jit`.

## Change

The SSA workspace's holder-chain, inline-state and accessor-site helpers
returned the address of their input parameter when there was no workspace.
Go's escape analysis therefore put that parameter on the heap even when
the helper copied it into an arena. The helpers now select their output
storage first and copy the input into it. Only the path without a workspace
allocates an individual object.

The register feedback's `Inline` method had the same problem: taking the
address of its answer allocated an `InlineSite` even when inlining was
refused. It now allocates the cached site only when the answer succeeds.

These changes preserve the optimization passes, inlining decisions,
tier-up policy and emitted code. No additional cache or retained capacity
is introduced. The generated layout pads keep the pinned hot code in place.

## Focused measurements

Five runs of `BenchmarkWorkspaceMetadata`, median ns/op. Each operation
constructs 128 metadata objects and rewinds the workspace. The final
feedback-cache change does not affect these SSA-only benchmarks.

| Metadata | Before ns/op | After ns/op | Before B/op | After B/op | Allocations before / after |
|---|---:|---:|---:|---:|---:|
| Holder chains | 1,822 | 703 | 8,192 | 0 | 128 / 0 |
| Inline states | 2,205 | 888 | 10,240 | 0 | 128 / 0 |
| Accessor sites | 4,139 | 1,130 | 24,576 | 0 | 128 / 0 |

`BenchmarkBuildOptimize/prototype` builds and optimizes a function with
prototype-property feedback. Allocations fall from 45 to 13 and bytes from
3,307 to 1,259 per build. Its median time, 20.86 us before and 20.75 us
after, is essentially unchanged; the helper speedups are not whole-compiler
speedups. The sum, branches and calls controls are within 1% of the baseline.

The rejected-inline benchmark drops from 43.92 ns/op to 16.27 ns/op,
208 B/op to zero and one allocation per query to zero (medians of three
baseline and five changed runs). Each query is the first query of a
compile, with the answer table already allocated; repeated cache hits
were already allocation-free.

Reproduce the focused measurements with:

```sh
go test ./internal/jit/ssa -run '^$' \
  -bench 'Benchmark(BuildOptimize|WorkspaceMetadata)' -benchmem -count=5
go test -tags quickjs_jit ./internal/vm -run '^$' \
  -bench BenchmarkJITRejectedInline -benchmem -count=5
```

## Whole-compiler measurements

The initial comparison runs all five required Octane workloads in one
process, in suite order, with `-jitcompile 20`. Each figure below is the
median of three fresh processes, with base, SSA-only and combined builds
run in alternating order. Times include the compilation phases, not the
JavaScript benchmark's execution. Allocation counts are the objects made
by one repetition of all compiles in that workload.

| Workload | Base ms | SSA-only ms | Combined ms | Base objects | Combined objects |
|---|---:|---:|---:|---:|---:|
| PdfJS | 29.88 | 29.58 | 29.33 | 63,799 | 61,786 |
| Mandreel | 50.02 | 49.87 | 49.25 | 54,320 | 53,298 |
| Gameboy | 39.83 | 39.90 | 40.91 | 90,591 | 88,783 |
| zlib | 50.30 | 51.46 | 50.54 | 7,783 | 7,814 |
| Typescript | 184.38 | 189.07 | 192.16 | 307,465 | 285,855 |

No median phase in that comparison is over 10% slower. However, the
combined TypeScript time is 4.2% slower, so this comparison does not show
a general compile-time win. The number and shape of compiles can also
vary between processes: TypeScript made 662-676 compiles in these runs.

Repeating the comparison with `GOMAXPROCS=1` and `-jitcompile 40` gives:

| Workload | Base ms | SSA-only ms | Combined ms | Base objects | Combined objects |
|---|---:|---:|---:|---:|---:|
| PdfJS | 32.53 | 32.96 | 32.91 | 64,100 | 61,773 |
| Mandreel | 49.51 | 51.19 | 50.95 | 54,455 | 53,429 |
| Gameboy | 41.99 | 42.05 | 42.10 | 91,613 | 89,739 |
| zlib | 44.54 | 44.66 | 45.22 | 8,038 | 7,892 |
| Typescript | 190.25 | 188.63 | 189.52 | 306,371 | 284,482 |

At one thread the collector shares the compiler's CPU. Short phase
measurements are sensitive to which phase incurs collection: PdfJS's
uses, hints and scan medians initially exceed the 10% threshold. A longer
isolated PdfJS comparison, three processes per build with 250 repetitions,
gives 29.42 ms before and 29.26 ms after, with every phase within 10%.
Compiler allocations fall from 63,253 to 60,878 objects and from 9.36 MB
to 9.07 MB. This is a 0.5% time difference, not a substantial speedup.

Three isolated TypeScript runs with 150 repetitions give 182.38 ms before
and 184.09 ms after, with allocations falling from 299,525 to 275,658
objects and from 51.70 MB to 48.80 MB. Interval construction is unusually
variable: 0.68-0.79 ms before and 0.62-1.12 ms after. Its median, 0.69 ms
versus 0.98 ms, exceeds the phase threshold and prompted a larger timing
sample. A diagnostic run with collection disabled (`GOGC=off`, 30
repetitions, three processes per build) gives 0.73 ms versus 0.67 ms for
intervals and 171.58 ms versus 172.09 ms total. Disabling collection is
only a diagnostic, not the acceptance configuration.

The final TypeScript check uses normal collection, `GOMAXPROCS=1`, 50
repetitions and nine fresh-process pairs in randomized order (seed 1729).
The batch size was fixed before running it. Median phase times are:

| Phase | Base ms | Combined ms | Change |
|---|---:|---:|---:|
| Build | 76.56 | 75.03 | -2.0% |
| Optimize | 38.66 | 38.27 | -1.0% |
| Prepare | 4.32 | 4.07 | -5.8% |
| Uses | 12.84 | 12.67 | -1.3% |
| Liveness | 8.16 | 8.20 | +0.5% |
| Intervals | 0.74 | 0.77 | +4.1% |
| Hints | 2.17 | 2.13 | -1.8% |
| Scan | 8.15 | 8.09 | -0.7% |
| Saves | 3.69 | 3.59 | -2.7% |
| Emit | 32.81 | 31.93 | -2.7% |
| Encode | 3.57 | 3.55 | -0.6% |
| Total | 192.00 | 188.75 | -1.7% |

Each entry is a median, so phase medians need not sum to the total median.
Every median phase is within the 10% threshold. Intervals range from
0.65-0.94 ms before and 0.65-1.09 ms after; the earlier 42% increase in
its median does not persist in this larger batch. Objects per repetition
fall from 296,002 to 269,277 (9.0%), and bytes from 54.76 MB to 51.78 MB
(5.4%). The whole TypeScript benchmark, with its repeated compiles, takes
10,653.8 ms before and 10,530.2 ms after; process CPU time including
collection falls from 10,599.7 ms to 10,453.8 ms (1.4%). A paired CPU
profile also shows no new concentrated compiler hotspot.

The reliable gains are removal of the individual allocations and the
focused helper speedups. Whole-compiler gains are small and dependent on
the workload and collection behavior; the earlier slower results remain
part of this report. These measurements do not establish a speedup across
every workload or platform.

Reproduce the whole-compiler measurements with two binaries built from
the respective revisions:

```sh
CGO_ENABLED=0 go build -buildvcs=false -tags quickjs_jit \
  -o /tmp/v8bench ./internal/cmd/v8bench
/usr/bin/time -l /tmp/v8bench -jit -bench octane -dir /path/to/octane \
  -suite Typescript,Mandreel,zlib,PdfJS,Gameboy -mode fixed -n 1 -jitcompile 20
GOMAXPROCS=1 /usr/bin/time -l /tmp/v8bench -jit -bench octane -dir /path/to/octane \
  -suite Typescript,Mandreel,zlib,PdfJS,Gameboy -mode fixed -n 1 -jitcompile 40
GOMAXPROCS=1 /usr/bin/time -l /tmp/v8bench -jit -bench octane -dir /path/to/octane \
  -suite Typescript -mode fixed -n 1 -jitcompile 50
```

Peak resident memory is variable: the default-thread runs range from
633-774 MB for the baseline and 762-779 MB for the combined change. These
measurements support fewer transient compiler objects, not a claim of
lower peak process memory.

## Correctness checks

- `TestWorkspaceMetadata` checks independent results, both allocation
  paths, reuse after rewind and zero heap allocations after warm-up.
- `TestJITRejectedInlineAllocations` checks that an unsuccessful inlining
  query is cached as absent and allocates no site.
- Full `go test ./...` runs pass both plain and with `quickjs_jit`.
  An uncached full JIT-tagged run also passes with `QJS_NONATIVE=1`.
  The amd64 and arm64 encoder verifier tests pass.
- Plain and JIT-stress test262 at the repository's pinned revision each
  pass 100,277 variants, with zero failures and 42 existing skips. Native
  code runs in 62,110 stress-test variants.
- `compilediff native` finds the same code in all 128 compiles from Octane
  Richards, DeltaBlue, RayTrace and EarleyBoyer, allowing for addresses and
  arm64's address-materialization lengths.
- `hotdiff` finds every hot interpreter function and native handler block
  identical on linux/amd64. On arm64 it reports relocation immediates in
  some Go functions; instruction counts and the native handler blocks
  match.
- `go generate ./internal/vm` restores all layout anchors in all eight
  pinned configurations.
- The SSA suite and the rejected-inlining allocation test also pass with
  Go 1.24.13. Windows amd64/arm64 and Linux 386 cross-builds pass. The 386
  root test binary builds; it was not executed on this macOS host.
