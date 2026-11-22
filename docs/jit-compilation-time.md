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
