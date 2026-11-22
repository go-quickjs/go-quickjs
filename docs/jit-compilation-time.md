# JIT compilation allocations

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
